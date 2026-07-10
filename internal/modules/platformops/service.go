package platformops

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// exportMaxRows caps the immutable audit export (§12) — bounded by contract.
const exportMaxRows = 5000

// workerClaimCap bounds one mock-SMS worker run to 100 claims.
const workerClaimCap = 100

// settableFlagKeys is the closed set of PUT-able feature flags. Capability
// gating is a deliberate act, not a free-text store (blueprint principle #6):
// an unknown key is NotFound, never an implicit insert.
var settableFlagKeys = map[string]struct{}{
	flags.FlagCollarTelemetry:  {},
	flags.FlagPhotoOCR:         {},
	flags.FlagONDC:             {},
	flags.FlagConsumerCommerce: {},
}

// service holds all platformops business logic.
type service struct {
	deps *deps.Deps
	repo *repository
	log  *slog.Logger
}

// newService wires the service with its module-scoped logger.
func newService(d *deps.Deps, repo *repository, log *slog.Logger) *service {
	return &service{deps: d, repo: repo, log: log}
}

// ---- Admin: feature flags (§12) ----

// listFlags returns every stored feature flag for the admin console.
func (s *service) listFlags(ctx context.Context) ([]flags.Flag, error) {
	all, err := s.deps.Flags.All(ctx)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list feature flags: %w", err))
	}
	if all == nil {
		all = []flags.Flag{}
	}
	return all, nil
}

// setFlag flips one well-known feature flag and writes the explicit
// "admin.flag_set" audit entry.
func (s *service) setFlag(ctx context.Context, actor auth.Actor, key string, enabled bool) (*SetFlagResponse, error) {
	if _, ok := settableFlagKeys[key]; !ok {
		s.log.WarnContext(ctx, "feature flag set rejected: unknown key",
			slog.String("key", key), slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.NotFound("feature flag " + key)
	}
	if err := s.deps.Flags.Set(ctx, key, enabled, actor.PartyID); err != nil {
		s.log.ErrorContext(ctx, "set feature flag failed",
			slog.String("key", key), slog.Any("err", err))
		return nil, httpx.Internal(fmt.Errorf("set feature flag: %w", err))
	}
	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "admin.flag_set",
		TargetType: "FEATURE_FLAG",
		TargetID:   key,
		Meta:       map[string]any{"key": key, "enabled": enabled},
	})
	s.log.InfoContext(ctx, "feature flag set",
		slog.String("key", key), slog.Bool("enabled", enabled),
		slog.String("actor_party_id", actor.PartyID))
	return &SetFlagResponse{Key: key, Enabled: enabled}, nil
}

// ---- Admin: control-tower stats (§12) ----

// adminStats returns the read-only control-tower snapshot. Role gating
// (SUPER_ADMIN / PCDF_ADMIN / MISSION_OFFICIAL) is enforced at the route; the
// figures are platform-global, so there is no org scope to require here.
func (s *service) adminStats(ctx context.Context) (*AdminStats, error) {
	now := time.Now().UTC()
	stats, err := s.repo.adminStats(ctx, domain.DateKeyIST(now))
	if err != nil {
		return nil, err
	}
	stats.GeneratedAt = now
	return stats, nil
}

// ---- Admin: product master (§12) ----

// listProducts returns the full admin product master (active + inactive).
func (s *service) listProducts(ctx context.Context) ([]domain.Product, error) {
	return s.repo.listProducts(ctx, false)
}

// listActiveProducts returns only ACTIVE products — the session-readable
// catalogue served on GET /products (e.g. a plant operator picking product
// options for a lot; a store screen). Inactive/retired SKUs are hidden.
func (s *service) listActiveProducts(ctx context.Context) ([]domain.Product, error) {
	return s.repo.listProducts(ctx, true)
}

// upsertProduct creates or updates a product master row keyed by SKU and
// writes the "admin.product_upsert" audit entry. An absent `active` defaults
// to true on the (upserted) row.
func (s *service) upsertProduct(ctx context.Context, actor auth.Actor, req UpsertProductRequest) (*domain.Product, error) {
	sku := strings.TrimSpace(req.SKU)
	if sku == "" {
		s.log.WarnContext(ctx, "product upsert rejected: missing sku",
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.BadRequest("MISSING_SKU", "sku is required")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		s.log.WarnContext(ctx, "product upsert rejected: missing name",
			slog.String("sku", sku), slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.BadRequest("MISSING_NAME", "name is required")
	}
	if req.MRP < 0 {
		s.log.WarnContext(ctx, "product upsert rejected: negative mrp",
			slog.String("sku", sku), slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.BadRequest("INVALID_MRP", "mrp must not be negative")
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}

	if req.ShelfLifeDays < 0 {
		return nil, httpx.BadRequest("INVALID_SHELF_LIFE", "shelf_life_days must not be negative")
	}
	now := time.Now().UTC()
	set := bson.D{
		{Key: "sku", Value: sku},
		{Key: "name", Value: name},
		{Key: "name_hi", Value: strings.TrimSpace(req.NameHi)},
		{Key: "category", Value: domain.NormalizeProductCategory(strings.TrimSpace(req.Category))},
		{Key: "mrp", Value: req.MRP},
		{Key: "unit_size", Value: strings.TrimSpace(req.UnitSize)},
		{Key: "shelf_life_days", Value: req.ShelfLifeDays},
		{Key: "active", Value: active},
		{Key: "updated_at", Value: now},
	}
	product, err := s.repo.upsertProduct(ctx, sku, set, now)
	if err != nil {
		s.log.ErrorContext(ctx, "product upsert failed",
			slog.String("sku", sku), slog.Any("err", err))
		return nil, err
	}

	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "admin.product_upsert",
		TargetType: "PRODUCT",
		TargetID:   product.ID.Hex(),
		Meta:       map[string]any{"sku": sku, "name": name, "mrp": req.MRP, "active": active},
	})
	s.log.InfoContext(ctx, "product upserted",
		slog.String("product_id", product.ID.Hex()), slog.String("sku", sku),
		slog.Bool("active", active), slog.String("actor_party_id", actor.PartyID))
	return product, nil
}

// ---- Admin: Sachiv-cap governance knob ----

// sachivCapKey is the app_settings key for the max-Sachivs-per-DCS ceiling.
const sachivCapKey = "sachiv_cap"

// defaultSachivCap is the ceiling applied until an admin sets one explicitly.
const defaultSachivCap = 2

// getSachivCap returns the current PER-DCS cap and the tightest per-DCS
// occupancy — the largest number of appointed (ACTIVE) SAMITI_SACHEEV at any
// single DCS. The cap is a per-DCS ceiling (§5.2), so this occupancy (not the
// federation-wide total) is the figure the cap is measured against.
func (s *service) getSachivCap(ctx context.Context) (*SachivCapResponse, error) {
	capValue, err := s.repo.getIntSetting(ctx, sachivCapKey, defaultSachivCap)
	if err != nil {
		return nil, err
	}
	appointed, err := s.repo.maxActiveRoleHoldersPerOrg(ctx, domain.RoleSamitiSacheev)
	if err != nil {
		return nil, err
	}
	return &SachivCapResponse{Cap: capValue, Appointed: appointed}, nil
}

// setSachivCap updates the PER-DCS cap. A cap below the busiest DCS's current
// occupancy is a 409 (the knob can be raised freely but never dropped below
// live per-DCS reality), and the change is audited.
func (s *service) setSachivCap(ctx context.Context, actor auth.Actor, capValue int) (*SachivCapResponse, error) {
	if capValue < 0 {
		return nil, httpx.BadRequest("INVALID_CAP", "cap must not be negative")
	}
	appointed, err := s.repo.maxActiveRoleHoldersPerOrg(ctx, domain.RoleSamitiSacheev)
	if err != nil {
		return nil, err
	}
	if capValue < appointed {
		s.log.WarnContext(ctx, "sachiv cap set rejected: below busiest-DCS occupancy",
			slog.Int("cap", capValue), slog.Int("appointed", appointed),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("CAP_BELOW_APPOINTED",
			fmt.Sprintf("cap %d is below the %d Sachivs already appointed at the busiest DCS", capValue, appointed))
	}
	if err := s.repo.setIntSetting(ctx, sachivCapKey, capValue); err != nil {
		return nil, err
	}
	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "admin.sachiv_cap_set",
		TargetType: "SETTING",
		TargetID:   sachivCapKey,
		Meta:       map[string]any{"cap": capValue, "appointed": appointed},
	})
	s.log.InfoContext(ctx, "sachiv cap set",
		slog.Int("cap", capValue), slog.Int("appointed", appointed),
		slog.String("actor_party_id", actor.PartyID))
	return &SachivCapResponse{Cap: capValue, Appointed: appointed}, nil
}

// ---- Auditor: read-only audit-log surface (§12) ----

// listAuditLogs returns a page of audit entries, newest first.
func (s *service) listAuditLogs(ctx context.Context, f auditLogFilter, page httpx.Page) ([]audit.Entry, int64, error) {
	return s.repo.listAuditLogs(ctx, f, page)
}

// exportAuditLogs assembles the capped attachment body for the GIGW/DPDP
// immutable audit export.
func (s *service) exportAuditLogs(ctx context.Context, from, to *time.Time) (*AuditExport, error) {
	entries, err := s.repo.exportAuditLogs(ctx, auditLogFilter{From: from, To: to}, exportMaxRows)
	if err != nil {
		return nil, err
	}
	return &AuditExport{
		ExportedAt: time.Now().UTC(),
		From:       from,
		To:         to,
		Count:      len(entries),
		Entries:    entries,
	}, nil
}

// ---- Notifications outbox + mock SMS worker (§12, §13) ----

// listNotifications returns a page of outbox documents with credential
// material redacted: the login OTP must never be readable through the
// outbox surface, or anyone with this route could mint any user's session
// (request OTP for the victim's phone, read it here, verify).
func (s *service) listNotifications(ctx context.Context, phone, status string, page httpx.Page) ([]StoredNotification, int64, error) {
	items, total, err := s.repo.listNotifications(ctx, phone, status, page)
	if err != nil {
		return nil, 0, err
	}
	for i := range items {
		redactNotificationSecrets(&items[i])
	}
	// Bulk-PII read (every party's phone + template params): audited for DPDP
	// forensics, mirroring support.pii_lookup and kyc.pending_list.
	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "admin.notifications_read",
		TargetType: "NOTIFICATION",
		Meta: map[string]any{
			"phone_filter":  phone,
			"status_filter": status,
			"count":         len(items),
			"total":         total,
		},
	})
	return items, total, nil
}

// redactedValue replaces credential material in read surfaces.
const redactedValue = "[REDACTED]"

// redactNotificationSecrets strips the plaintext OTP (params + rendered meta)
// from an outbox document before it leaves the API.
func redactNotificationSecrets(n *StoredNotification) {
	if n.TemplateKey != domain.TemplateOTP {
		return
	}
	if n.Params != nil {
		params := make(map[string]string, len(n.Params))
		for k, v := range n.Params {
			if k == "otp" {
				v = redactedValue
			}
			params[k] = v
		}
		n.Params = params
	}
	if n.Meta != nil {
		meta := make(map[string]any, len(n.Meta))
		for k, v := range n.Meta {
			if k == "rendered_en" || k == "rendered_hi" {
				v = redactedValue
			}
			meta[k] = v
		}
		n.Meta = meta
	}
}

// runWorker is the MOCK SMS provider dispatch (real deployment: MSG91 /
// telecom DLT gateway, §13). It claims up to workerClaimCap QUEUED documents
// via a race-safe FindOneAndUpdate loop, stamps each SENT with a mock
// provider reference, and renders the English/Hindi lines into meta for
// visibility. The manual trigger keeps the demo deterministic; production
// runs this as a cron/queue consumer.
func (s *service) runWorker(ctx context.Context) (*WorkerRunResponse, error) {
	sent := 0
	for i := 0; i < workerClaimCap; i++ {
		providerRef := "SMS-MOCK-" + uuid.NewString()[0:8]
		claimed, err := s.repo.claimQueuedNotification(ctx, providerRef, time.Now().UTC())
		if err != nil {
			s.log.ErrorContext(ctx, "claim queued notification failed", slog.Any("err", err))
			return nil, err
		}
		if claimed == nil {
			break // queue drained
		}
		sent++

		english, hindi := renderSMS(claimed.TemplateKey, claimed.Params)
		if claimed.TemplateKey == domain.TemplateOTP {
			// Never persist the rendered OTP text — the provider gets the
			// message transiently; the readable outbox keeps no plaintext code.
			english, hindi = redactedValue, redactedValue
		}
		meta := map[string]any{
			"provider":    "SMS-MOCK",
			"rendered_en": english,
			"rendered_hi": hindi,
		}
		if err := s.repo.setNotificationMeta(ctx, claimed.ID, meta); err != nil {
			// The claim already succeeded — meta is cosmetic, log and move on.
			s.log.ErrorContext(ctx, "stamp notification meta failed",
				slog.String("notification_id", claimed.ID.Hex()), slog.Any("err", err))
		}
		s.log.InfoContext(ctx, "notification dispatched",
			slog.String("notification_id", claimed.ID.Hex()),
			slog.String("template_key", claimed.TemplateKey),
			slog.String("provider_ref", providerRef),
			slog.String("status", domain.NotificationSent))
	}
	s.log.InfoContext(ctx, "notification worker run complete", slog.Int("sent", sent))
	return &WorkerRunResponse{Sent: sent}, nil
}

// renderSMS produces one simple English and one Hindi line per template key —
// mock rendering for visibility; real templates live with the DLT-registered
// provider (§13). Missing params render as empty strings, never a panic.
func renderSMS(templateKey string, params map[string]string) (english, hindi string) {
	p := func(key string) string { return params[key] } // nil-map reads are safe

	switch templateKey {
	case domain.TemplatePayoutCredited:
		return fmt.Sprintf("Rs %s credited to your bank account. UTR %s. - Saathi", p("amount"), p("utr")),
			fmt.Sprintf("₹%s आपके बैंक खाते में जमा हुए। UTR %s। - साथी", p("amount"), p("utr"))
	case domain.TemplateSafetyBlock:
		return fmt.Sprintf("SAFETY ALERT: %s %s blocked at %s. Reasons: %s. Quarantine and inspect immediately. - Saathi",
				p("subject_type"), p("subject_id"), p("stage"), p("reasons")),
			fmt.Sprintf("सुरक्षा चेतावनी: %s %s को %s पर रोका गया। कारण: %s। तुरंत जांच करें। - साथी",
				p("subject_type"), p("subject_id"), p("stage"), p("reasons"))
	case domain.TemplateMVUDispatched:
		return fmt.Sprintf("MVU 1962 van dispatched for your animal (case %s). Please stay reachable. - Saathi", p("case_id")),
			fmt.Sprintf("आपके पशु के लिए 1962 एमवीयू वैन रवाना (केस %s)। कृपया संपर्क में रहें। - साथी", p("case_id"))
	case domain.TemplatePourReceipt:
		return fmt.Sprintf("%sL @ Rs %s = Rs %s credited to today's bill. - Saathi", p("quantity"), p("rate"), p("amount")),
			fmt.Sprintf("%s लीटर @ ₹%s = ₹%s आज के बिल में जुड़े। - साथी", p("quantity"), p("rate"), p("amount"))
	case domain.TemplateInvoiceIssued:
		return fmt.Sprintf("Invoice %s for Rs %s issued. - Saathi", p("invoice_number"), p("amount")),
			fmt.Sprintf("₹%s का बिल %s जारी हुआ। - साथी", p("amount"), p("invoice_number"))
	case domain.TemplateKYCApproved:
		return fmt.Sprintf("KYC approved — tier %s unlocked. - Saathi", p("tier")),
			fmt.Sprintf("KYC स्वीकृत — टियर %s अनलॉक हुआ। - साथी", p("tier"))
	case domain.TemplateKYCRejected:
		return fmt.Sprintf("KYC rejected: %s. - Saathi", p("reason")),
			fmt.Sprintf("KYC अस्वीकृत: %s। - साथी", p("reason"))
	case domain.TemplateKYCPending:
		return fmt.Sprintf("KYC verification pending for %s (tier %s) — review in Saathi.", p("subject"), p("tier")),
			fmt.Sprintf("%s (टियर %s) का KYC सत्यापन लंबित — साथी में समीक्षा करें।", p("subject"), p("tier"))
	case domain.TemplateOTP:
		return fmt.Sprintf("Your Saathi OTP is %s. Do not share it with anyone.", p("otp")),
			fmt.Sprintf("आपका साथी OTP %s है। इसे किसी से साझा न करें।", p("otp"))
	default:
		return "Saathi update: " + templateKey, "साथी सूचना: " + templateKey
	}
}

// ---- Support: audited limited-PII lookup (§5.2 role 20) ----

// lookupParty resolves a phone to the limited support view. EVERY call —
// hit or miss — is recorded as "support.pii_lookup": PII access is audited,
// full stop.
func (s *service) lookupParty(ctx context.Context, phone string) (*PartyLookupResponse, error) {
	party, findErr := s.repo.findPartyByPhone(ctx, phone)

	entry := audit.Entry{
		Action:     "support.pii_lookup",
		TargetType: "PARTY",
		Meta:       map[string]any{"phone": phone, "found": findErr == nil},
	}
	if findErr == nil {
		entry.TargetID = party.ID.Hex()
	}
	s.deps.Audit.Record(ctx, entry)

	if findErr != nil {
		return nil, findErr
	}

	assignments, err := s.repo.listActiveAssignmentsForParty(ctx, party.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	roles := []PartyRoleView{}
	for _, assignment := range assignments {
		if !assignment.UsableAt(now) {
			continue
		}
		orgName := ""
		if org, orgErr := s.deps.Orgs.Get(ctx, assignment.OrgUnitID); orgErr == nil {
			orgName = org.Name
		}
		roles = append(roles, PartyRoleView{
			RoleCode:  assignment.RoleCode,
			OrgUnitID: assignment.OrgUnitID.Hex(),
			OrgName:   orgName,
		})
	}

	s.log.InfoContext(ctx, "support party lookup",
		slog.String("party_id", party.ID.Hex()), slog.Int("roles", len(roles)))

	// Limited PII by construction: no KYC document numbers, no bank details.
	return &PartyLookupResponse{
		PartyID:  party.ID.Hex(),
		FullName: party.FullName,
		KYCTier:  party.KYCTier,
		Status:   party.Status,
		Roles:    roles,
	}, nil
}

// ---- Event-bus reactions: cross-module SMS queuing ----

// decodeBusPayload structurally re-decodes a bus payload into this module's
// mirror struct — modules never import each other's types (module contract).
func decodeBusPayload(payload any, dst any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal bus payload: %w", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("unmarshal bus payload: %w", err)
	}
	return nil
}

// onPayoutCredited queues the "₹ credited, UTR …" SMS to the farmer
// (blueprint §8.1 last step).
func (s *service) onPayoutCredited(ctx context.Context, payload any) {
	var event payoutCreditedEvent
	if err := decodeBusPayload(payload, &event); err != nil {
		s.log.ErrorContext(ctx, "bad payout.credited payload", slog.Any("err", err))
		return
	}
	if event.FarmerPartyID == "" && event.Phone == "" {
		s.log.WarnContext(ctx, "payout.credited payload has no addressee")
		return
	}
	partyID := s.parseAddresseeID(ctx, event.FarmerPartyID, "payout.credited")
	s.queueNotification(ctx, partyID, event.Phone, domain.TemplatePayoutCredited, map[string]string{
		"amount": strconv.FormatFloat(event.Amount, 'f', 2, 64),
		"utr":    event.UTR,
	})
}

// onGateBlocked alerts the blocked subject's UNION_FIELD_SUPERVISOR(s): the
// subject's org unit is resolved from its document, walked up to the union
// ancestor, and every usable supervisor assignment there gets the safety SMS.
func (s *service) onGateBlocked(ctx context.Context, payload any) {
	var event gateBlockedEvent
	if err := decodeBusPayload(payload, &event); err != nil {
		s.log.ErrorContext(ctx, "bad qc.gate_blocked payload", slog.Any("err", err))
		return
	}

	subjectID, err := primitive.ObjectIDFromHex(event.SubjectID)
	if err != nil {
		s.log.WarnContext(ctx, "qc.gate_blocked subject id is not an object id",
			slog.String("subject_type", event.SubjectType), slog.String("subject_id", event.SubjectID))
		return
	}
	orgUnitID, err := s.repo.subjectOrgUnit(ctx, event.SubjectType, subjectID)
	if err != nil {
		s.log.ErrorContext(ctx, "gate_blocked org resolution failed",
			slog.String("subject_type", event.SubjectType),
			slog.String("subject_id", subjectID.Hex()), slog.Any("err", err))
		return
	}
	union, err := s.unionAncestor(ctx, orgUnitID)
	if err != nil {
		s.log.ErrorContext(ctx, "gate_blocked union walk failed",
			slog.String("org_unit_id", orgUnitID.Hex()), slog.Any("err", err))
		return
	}
	if union == nil {
		s.log.WarnContext(ctx, "gate_blocked org has no union ancestor",
			slog.String("org_unit_id", orgUnitID.Hex()))
		return
	}

	supervisors, err := s.repo.listActiveRoleHolders(ctx, union.ID, domain.RoleUnionFieldSupervisor)
	if err != nil {
		s.log.ErrorContext(ctx, "gate_blocked supervisor lookup failed",
			slog.String("union_id", union.ID.Hex()), slog.Any("err", err))
		return
	}

	params := map[string]string{
		"subject_type": event.SubjectType,
		"subject_id":   event.SubjectID,
		"stage":        event.Stage,
		"reasons":      strings.Join(event.FailureReasons, ", "),
	}
	now := time.Now().UTC()
	notified := map[primitive.ObjectID]bool{}
	for _, assignment := range supervisors {
		if !assignment.UsableAt(now) || notified[assignment.PartyID] {
			continue
		}
		notified[assignment.PartyID] = true
		partyID := assignment.PartyID
		s.queueNotification(ctx, &partyID, "", domain.TemplateSafetyBlock, params)
	}
}

// onMVUDispatched queues the "MVU van on its way" SMS to the farmer (§10).
func (s *service) onMVUDispatched(ctx context.Context, payload any) {
	var event mvuDispatchedEvent
	if err := decodeBusPayload(payload, &event); err != nil {
		s.log.ErrorContext(ctx, "bad mvu.dispatched payload", slog.Any("err", err))
		return
	}
	if event.FarmerPartyID == "" && event.Phone == "" {
		s.log.WarnContext(ctx, "mvu.dispatched payload has no addressee")
		return
	}
	caseID := event.CaseID
	if caseID == "" {
		caseID = event.MVUCaseID
	}
	if caseID == "" {
		caseID = event.ID
	}
	params := map[string]string{"case_id": caseID}
	if event.ETA != "" {
		params["eta"] = event.ETA
	}
	partyID := s.parseAddresseeID(ctx, event.FarmerPartyID, "mvu.dispatched")
	s.queueNotification(ctx, partyID, event.Phone, domain.TemplateMVUDispatched, params)
}

// parseAddresseeID turns an optional addressee party ObjectID hex string
// (carried as a hex string on bus payloads) into a *primitive.ObjectID.
// An empty value is a legitimate phone-only addressing (returns nil); a
// non-empty but malformed value is a business/contract violation (WARN + nil).
func (s *service) parseAddresseeID(ctx context.Context, hex, event string) *primitive.ObjectID {
	if hex == "" {
		return nil
	}
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		s.log.WarnContext(ctx, "bus payload party id is not an object id",
			slog.String("event", event), slog.String("party_id", hex))
		return nil
	}
	return &id
}

// hexOrEmpty renders an optional ObjectID for logs.
func hexOrEmpty(id *primitive.ObjectID) string {
	if id == nil {
		return ""
	}
	return id.Hex()
}

// queueNotification inserts one QUEUED outbox document, resolving phone and
// preferred language from the party record when available. The _id is
// pre-generated so the queued entry can be named in the structured log.
// Failures are logged, never propagated — a lost SMS must not fail the
// publishing flow.
func (s *service) queueNotification(ctx context.Context, partyID *primitive.ObjectID, phone, templateKey string, params map[string]string) {
	language := "hi" // vernacular default (blueprint §13)
	if partyID != nil {
		if party, err := s.repo.findPartyByID(ctx, *partyID); err == nil {
			if phone == "" {
				phone = party.Phone
			}
			if party.PreferredLanguage != "" {
				language = party.PreferredLanguage
			}
		}
	}
	if phone == "" {
		s.log.WarnContext(ctx, "dropping notification with no phone",
			slog.String("template_key", templateKey), slog.String("party_id", hexOrEmpty(partyID)))
		return
	}

	id := primitive.NewObjectID()
	notification := &StoredNotification{Notification: domain.Notification{
		ID:          id,
		PartyID:     partyID,
		Phone:       phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: templateKey,
		Language:    language,
		Params:      params,
		Status:      domain.NotificationQueued,
		QueuedAt:    time.Now().UTC(),
	}}
	if err := s.repo.insertNotification(ctx, notification); err != nil {
		s.log.ErrorContext(ctx, "queue notification failed",
			slog.String("template_key", templateKey), slog.Any("err", err))
		return
	}
	s.log.InfoContext(ctx, "notification queued",
		slog.String("notification_id", id.Hex()),
		slog.String("template_key", templateKey),
		slog.String("party_id", hexOrEmpty(partyID)),
		slog.String("status", domain.NotificationQueued))
}

// unionAncestor returns the MILK_UNION org unit at or above orgUnitID, walking
// the denormalised ancestor Path nearest-first via d.Orgs.Get. Returns nil
// when no union sits on the chain (e.g. a plant directly under the federation).
func (s *service) unionAncestor(ctx context.Context, orgUnitID primitive.ObjectID) (*domain.OrgUnit, error) {
	org, err := s.deps.Orgs.Get(ctx, orgUnitID)
	if err != nil {
		return nil, err
	}
	if org.Type == domain.OrgTypeMilkUnion {
		return org, nil
	}
	// Path holds ancestors root→down; walk nearest ancestor first.
	for i := len(org.Path) - 1; i >= 0; i-- {
		ancestor, err := s.deps.Orgs.Get(ctx, org.Path[i])
		if err != nil {
			return nil, err
		}
		if ancestor.Type == domain.OrgTypeMilkUnion {
			return ancestor, nil
		}
	}
	return nil, nil
}
