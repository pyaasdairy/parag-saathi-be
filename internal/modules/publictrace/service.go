package publictrace

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/orgscope"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
)

const (
	// traceDepth bounds the upstream graph walk (QR → lot → batch → BMC lots
	// → consignments comfortably fits; the ledger also hard-caps event count).
	traceDepth = 10
	// scanVerifySpanCap bounds how many ledger events one consumer scan
	// re-hashes; wider traced ranges are verified partially (most recent tail).
	scanVerifySpanCap = 5000
	// publicVerifySpanCap bounds the public /ledger/verify endpoint span.
	publicVerifySpanCap = 10000
	// downstreamEventCap keeps the official trace response bounded even for
	// heavily referenced entities.
	downstreamEventCap = 2000
)

// validEntityTypes is the closed set of traceable entity types (domain.Entity*).
var validEntityTypes = map[string]struct{}{
	domain.EntityMilkPour:        {},
	domain.EntityAnalyzerReading: {},
	domain.EntityInvoice:         {},
	domain.EntityConsignment:     {},
	domain.EntityRouteTrip:       {},
	domain.EntityBMCLot:          {},
	domain.EntityBatch:           {},
	domain.EntityProductLot:      {},
	domain.EntityBatchQR:         {},
	domain.EntityQCResult:        {},
	domain.EntitySettlement:      {},
	domain.EntityAnimal:          {},
	domain.EntityParty:           {},
}

// Service resolves consumer QR scans and official trace queries. All reads,
// no writes except the fire-and-forget scan counter — nothing here ever
// mutates provenance.
type Service struct {
	log      *slog.Logger
	repo     *repo
	ledger   *provenance.Ledger
	orgs     *orgscope.Resolver
	qrSecret string

	// Incremental verification watermark: the chain is verified cumulatively
	// across scans, so a repeat scan whose traced range is already covered
	// answers O(1) instead of re-hashing thousands of interleaved events on
	// the hottest public endpoint. Per-instance; a restart re-verifies.
	verifyMu     sync.Mutex
	verifiedUpTo int64
	chainBroken  bool
	brokenAtSeq  int64
}

// NewService wires the service from the shared dependency container.
func NewService(d *deps.Deps) *Service {
	return &Service{
		log:      d.Log,
		repo:     newRepo(d.DB),
		ledger:   d.Ledger,
		orgs:     d.Orgs,
		qrSecret: d.Cfg.QRSigningSecret,
	}
}

// VerifyQRToken checks a stored signed token in the format QR issuance
// (plant module signQRToken) mints: "base64url(qr_code|product_lot_id|
// issued_unix)" + "." + hex(HMAC-SHA256(secret, decoded payload)). The HMAC
// is computed over the DECODED payload bytes — exactly what issuance signed.
// The comparison is constant-time so a forger learns nothing from response
// timing.
func VerifyQRToken(secret, token string) bool {
	payloadB64, signature, found := strings.Cut(token, ".")
	if !found || payloadB64 == "" || signature == "" {
		return false // malformed: no payload/signature split
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil || len(payload) == 0 {
		return false // malformed: payload is not valid base64url
	}
	expected := auth.HMACHash(secret, string(payload))
	return auth.ConstantTimeEqual(expected, signature)
}

// ScanQR resolves one consumer QR scan into the honest provenance view
// (§7.4): product + plant + lab certificate + the SET of contributing
// samitis — never per-farmer data — plus a live tamper-evidence check.
func (s *Service) ScanQR(ctx context.Context, qrCode string) (*QRScanResponse, error) {
	qr, err := s.repo.qrByCode(ctx, qrCode)
	if err != nil {
		return nil, err
	}

	// Integrity gate: the stored token must carry a valid HMAC. A mismatch
	// means the QR record was tampered with or forged — surface it loudly.
	if !VerifyQRToken(s.qrSecret, qr.SignedToken) {
		s.log.Warn("QR signed-token integrity check failed — possible forgery",
			slog.String("qr_code", qrCode), slog.String("qr_id", qr.ID))
		return nil, httpx.Conflict("QR_INTEGRITY_FAILED",
			"this QR code failed integrity verification and may be counterfeit")
	}

	// Count the scan without ever blocking it: fire-and-forget increment.
	go func(qrID string) {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.repo.incrementScanCount(bg, qrID); err != nil {
			s.log.Warn("scan_count increment failed", slog.String("qr_id", qrID), slog.Any("err", err))
		}
	}(qr.ID)

	lot, err := s.repo.productLotByID(ctx, qr.ProductLotID)
	if err != nil {
		return nil, err
	}
	batch, err := s.repo.batchByID(ctx, lot.BatchID)
	if err != nil {
		return nil, err
	}
	plant, err := s.orgs.Get(ctx, batch.PlantID)
	if err != nil {
		return nil, err
	}

	// Walk the provenance graph upstream from the product lot and STOP at the
	// pooling boundary: batch → BMC lots → consignments (§7.4). Consignment
	// refs (hundreds of per-pour nodes, cross-route trip fan-out) are never
	// expanded — the consumer view only needs the samiti set.
	events, err := s.ledger.TraceStopAt(ctx, domain.EntityProductLot, lot.ID, traceDepth,
		map[string]bool{domain.EntityConsignment: true})
	if err != nil {
		return nil, httpx.Internal(err)
	}

	sourcing, err := s.buildSourcing(ctx, events)
	if err != nil {
		return nil, err
	}

	ledgerInfo, err := s.verifyTracedRange(ctx, events)
	if err != nil {
		return nil, err
	}

	resp := &QRScanResponse{
		Product: ProductInfo{
			Name:       lot.ProductName,
			SKU:        lot.SKU,
			UnitSize:   lot.UnitSize,
			MfgDate:    lot.MfgDate,
			ExpiryDate: lot.ExpiryDate,
		},
		BatchNumber: batch.BatchNumber,
		Plant:       PlantInfo{Name: plant.Name, District: plant.District},
		Sourcing:    *sourcing,
		Ledger:      *ledgerInfo,
		ScanCount:   qr.ScanCount + 1, // reflect this scan; the $inc lands async
	}

	// PLANT_LAB pass certificate — omitted (not an error) when absent.
	qc, err := s.repo.latestPlantLabPass(ctx, batch.ID)
	if err != nil {
		return nil, err
	}
	if qc != nil {
		resp.Quality = &QualityInfo{
			CertificateNumber: qc.CertificateNumber,
			Stage:             qc.Stage,
			Tests:             qc.Tests,
			RecordedAt:        qc.RecordedAt,
		}
	}

	// A recalled lot still resolves with 200 — consumers must SEE the recall
	// notice, not hit a dead link.
	if lot.Status == domain.ProductLotStatusRecalled {
		resp.Recalled = true
		notice := "This product lot has been RECALLED. Do not consume — return it to the point of purchase."
		if lot.RecallReason != "" {
			notice += " Reason: " + lot.RecallReason
		}
		resp.RecallNotice = notice
	}
	return resp, nil
}

// buildSourcing derives the samiti-set view from traced consignment events:
// contributing DCS org ids from each event's org_unit_id/payload, collection
// dates from payloads, with the consignment documents as fallback when the
// events did not carry those fields.
func (s *Service) buildSourcing(ctx context.Context, events []provenance.Event) (*SourcingInfo, error) {
	dcsIDs := map[string]struct{}{}
	dates := map[string]struct{}{}
	consignmentIDs := map[string]struct{}{}

	for _, ev := range events {
		if ev.EntityType != domain.EntityConsignment {
			continue
		}
		consignmentIDs[ev.EntityID] = struct{}{}
		// Only created/dispatched events carry the DCS in org_unit_id —
		// consignment.picked_up is appended by logistics under the UNION's
		// org id, which must never be counted as a contributing samiti.
		switch ev.Type {
		case domain.EventConsignmentCreated, domain.EventConsignmentDispatched:
			if ev.OrgUnitID != "" {
				dcsIDs[ev.OrgUnitID] = struct{}{}
			} else if v, ok := ev.Payload["dcs_id"].(string); ok && v != "" {
				dcsIDs[v] = struct{}{}
			}
		}
		if v, ok := ev.Payload["date"].(string); ok && v != "" {
			dates[v] = struct{}{}
		}
	}

	// Fallback: if the events lacked DCS ids or dates, read the consignment
	// docs themselves (still samiti-level — pour ids are never surfaced).
	if len(consignmentIDs) > 0 && (len(dcsIDs) == 0 || len(dates) == 0) {
		docs, err := s.repo.consignmentsByIDs(ctx, sortedKeys(consignmentIDs))
		if err != nil {
			return nil, err
		}
		for _, c := range docs {
			if c.DCSID != "" {
				dcsIDs[c.DCSID] = struct{}{}
			}
			if c.Date != "" {
				dates[c.Date] = struct{}{}
			}
		}
	}

	samitis := make([]SamitiInfo, 0, len(dcsIDs))
	districts := map[string]struct{}{}
	for _, id := range sortedKeys(dcsIDs) {
		org, err := s.orgs.Get(ctx, id)
		if err != nil {
			// A missing org unit must not break a consumer scan — log and skip.
			s.log.Warn("QR scan: contributing DCS org not resolvable",
				slog.String("dcs_org_id", id), slog.Any("err", err))
			continue
		}
		// Defence in depth: the §7.4 samiti set may only contain DCS units.
		if org.Type != domain.OrgTypeDCS {
			s.log.Warn("QR scan: non-DCS org unit skipped from samiti set",
				slog.String("org_id", id), slog.String("org_type", org.Type))
			continue
		}
		samitis = append(samitis, SamitiInfo{Name: org.Name, Code: org.Code, District: org.District})
		if org.District != "" {
			districts[org.District] = struct{}{}
		}
	}
	sort.Slice(samitis, func(i, j int) bool { return samitis[i].Name < samitis[j].Name })

	collectionDates := sortedKeys(dates)
	return &SourcingInfo{
		Message:         sourcingMessage(collectionDates, len(samitis), sortedKeys(districts)),
		Samitis:         samitis,
		CollectionDates: collectionDates,
	}, nil
}

// sourcingMessage renders the consumer-facing pooled-provenance sentence,
// e.g. "Made from milk collected on 2026-07-05 from 3 samitis in Lucknow".
func sourcingMessage(dates []string, samitiCount int, districts []string) string {
	var b strings.Builder
	b.WriteString("Made from milk collected")
	if len(dates) > 0 {
		b.WriteString(" on " + strings.Join(dates, ", "))
	}
	noun := "samitis"
	if samitiCount == 1 {
		noun = "samiti"
	}
	fmt.Fprintf(&b, " from %d %s", samitiCount, noun)
	if len(districts) > 0 {
		b.WriteString(" in " + strings.Join(districts, ", "))
	}
	return b.String()
}

// verifyTracedRange answers the per-scan tamper-evidence check from a
// cumulative verification watermark: the chain is verified incrementally up
// to the traced range's max seq, and once a prefix is verified, later scans
// inside it are O(1) — no per-request re-hash of thousands of interleaved
// events. Spans over scanVerifySpanCap are verified partially (most recent
// tail) and flagged, matching the previous per-scan behaviour.
func (s *Service) verifyTracedRange(ctx context.Context, events []provenance.Event) (*LedgerInfo, error) {
	if len(events) == 0 {
		return &LedgerInfo{Events: 0, Intact: true, VerifiedRange: [2]int64{0, 0}}, nil
	}
	maxSeq := events[0].Seq
	for _, ev := range events[1:] {
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
	}

	s.verifyMu.Lock()
	defer s.verifyMu.Unlock()

	if s.chainBroken {
		return &LedgerInfo{
			Events:        len(events),
			Intact:        false,
			VerifiedRange: [2]int64{s.brokenAtSeq, s.brokenAtSeq},
		}, nil
	}
	if maxSeq <= s.verifiedUpTo {
		return &LedgerInfo{
			Events:        len(events),
			Intact:        true,
			VerifiedRange: [2]int64{1, s.verifiedUpTo},
		}, nil
	}

	// Extend the watermark: start at the last verified event (re-hashing it
	// covers the boundary linkage), capped to the per-request span budget.
	from, partial := max(int64(1), s.verifiedUpTo), false
	if maxSeq-from+1 > scanVerifySpanCap {
		from, partial = maxSeq-scanVerifySpanCap+1, true
	}
	intact, brokenAt, err := s.ledger.VerifyChain(ctx, from, maxSeq)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if intact {
		s.verifiedUpTo = maxSeq
	} else {
		s.chainBroken, s.brokenAtSeq = true, brokenAt
	}
	return &LedgerInfo{
		Events:        len(events),
		Intact:        intact,
		VerifiedRange: [2]int64{from, maxSeq},
		Partial:       partial,
	}, nil
}

// VerifyLedger runs the public tamper-evidence check over [from,to]. Both
// bounds are optional: `to` defaults to the chain head, `from` to the start
// of the last publicVerifySpanCap events. Spans over the cap are rejected.
func (s *Service) VerifyLedger(ctx context.Context, fromRaw, toRaw string) (*LedgerVerifyResponse, error) {
	latest, err := s.ledger.LatestSeq(ctx)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if latest == 0 {
		return &LedgerVerifyResponse{Intact: true, From: 0, To: 0}, nil // empty chain is trivially intact
	}

	to := latest
	if toRaw != "" {
		n, err := strconv.ParseInt(toRaw, 10, 64)
		if err != nil || n < 1 {
			return nil, httpx.BadRequest("INVALID_RANGE", "'to' must be a positive sequence number")
		}
		to = min(n, latest)
	}
	from := max(int64(1), to-publicVerifySpanCap+1)
	if fromRaw != "" {
		n, err := strconv.ParseInt(fromRaw, 10, 64)
		if err != nil || n < 1 {
			return nil, httpx.BadRequest("INVALID_RANGE", "'from' must be a positive sequence number")
		}
		from = n
	}
	if from > to {
		return nil, httpx.BadRequest("INVALID_RANGE", "'from' must not exceed 'to'")
	}
	if to-from+1 > publicVerifySpanCap {
		return nil, httpx.BadRequest("SPAN_TOO_WIDE",
			fmt.Sprintf("verification span is capped at %d events per request", publicVerifySpanCap))
	}

	intact, brokenAt, err := s.ledger.VerifyChain(ctx, from, to)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	resp := &LedgerVerifyResponse{Intact: intact, From: from, To: to}
	if !intact {
		resp.BrokenAtSeq = &brokenAt
	}
	return resp, nil
}

// TraceGraph returns the full event graph around one entity: upstream via
// the recursive ref walk, downstream via reverse-ref lookup — the recall /
// root-cause tool (§8.3).
func (s *Service) TraceGraph(ctx context.Context, entityType, entityID string) (*TraceGraphResponse, error) {
	if err := validateEntityType(entityType); err != nil {
		return nil, err
	}
	upstream, err := s.ledger.Trace(ctx, entityType, entityID, traceDepth)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	// The cap is pushed into the query: only the LAST downstreamEventCap
	// referencing events are fetched, instead of decoding an unbounded set
	// into memory and truncating afterwards.
	downstream, err := s.ledger.DownstreamRefs(ctx, entityID, downstreamEventCap)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if len(upstream) == 0 && len(downstream) == 0 {
		return nil, httpx.NotFound("provenance for entity")
	}
	if upstream == nil {
		upstream = []provenance.Event{}
	}
	if downstream == nil {
		downstream = []provenance.Event{}
	}
	return &TraceGraphResponse{Upstream: upstream, Downstream: downstream}, nil
}

// Timeline returns one entity's own events in chain order, paginated. The
// skip/limit is pushed into the Mongo query so a page read never loads the
// entity's full history. The second return value is the total event count.
func (s *Service) Timeline(ctx context.Context, entityType, entityID string, page httpx.Page) ([]provenance.Event, int, error) {
	if err := validateEntityType(entityType); err != nil {
		return nil, 0, err
	}
	total, err := s.ledger.CountEventsForEntity(ctx, entityType, entityID)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	if total == 0 {
		return nil, 0, httpx.NotFound("provenance for entity")
	}
	events, err := s.ledger.EventsForEntityPage(ctx, entityType, entityID, page.Offset, page.Limit)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return events, int(total), nil
}

// validateEntityType rejects entity types outside the domain.Entity* catalog.
func validateEntityType(entityType string) error {
	if _, ok := validEntityTypes[entityType]; !ok {
		return httpx.BadRequest("INVALID_ENTITY_TYPE",
			"unknown entity type "+entityType+" — use one of the provenance entity type constants")
	}
	return nil
}

// sortedKeys returns a set's keys in deterministic (lexicographic) order.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
