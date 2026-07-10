package collection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
)

// Anti-tamper thresholds (blueprint §8.2).
const (
	// lowOCRConfidenceThreshold — OCR extractions below this confidence are
	// flagged for human re-verification.
	lowOCRConfidenceThreshold = 0.75
	// maxClockSkew — device timestamps farther than this from server time
	// get the DEVICE_CLOCK_SKEW flag.
	maxClockSkew = 10 * time.Minute
	// maxBatchSyncItems caps one offline reconnect batch.
	maxBatchSyncItems = 500
	// defaultLanguage for farmer SMS when the party has no preference set.
	defaultLanguage = "hi"
)

// service holds all business logic of the collection module.
type service struct {
	d    *deps.Deps
	repo *repo
	log  *slog.Logger
}

// newService wires the service onto the platform dependencies. log is the
// module-scoped logger derived in Register.
func newService(d *deps.Deps, r *repo, log *slog.Logger) *service {
	return &service{d: d, repo: r, log: log}
}

// actorID parses the actor's party id (carried as an ObjectID hex string in
// the JWT) into a primitive.ObjectID — call once per service method that
// stores or compares the actor's identity.
func actorID(actor auth.Actor) (primitive.ObjectID, error) {
	return httpx.ParseID(actor.PartyID, "actor")
}

// requireInScope enforces the actor's org scope over a target org unit and
// WARN-logs the denial (business rejection, never silent).
func (s *service) requireInScope(ctx context.Context, actor auth.Actor, targetOrgID primitive.ObjectID, op string) error {
	err := s.d.Orgs.RequireInScope(ctx, actor, targetOrgID)
	if err != nil {
		var appErr *httpx.AppError
		if errors.As(err, &appErr) && appErr.Status == http.StatusForbidden {
			s.log.WarnContext(ctx, "org scope denied",
				slog.String("op", op),
				slog.String("target_org_id", targetOrgID.Hex()),
				slog.String("actor_party_id", actor.PartyID),
				slog.String("actor_role", actor.RoleCode))
		}
		return err
	}
	return nil
}

// --- rate charts ---

// CreateRateChart stores a new pricing chart for an org unit and deactivates
// the org's previously active charts (the new chart supersedes them).
func (s *service) CreateRateChart(ctx context.Context, actor auth.Actor, req CreateRateChartRequest) (*domain.RateChart, error) {
	if req.OrgUnitID.IsZero() || req.Name == "" {
		return nil, httpx.BadRequest("VALIDATION", "org_unit_id and name are required")
	}
	if req.BaseRatePerLitre < 0 || req.FatRatePerPoint < 0 || req.SNFRatePerPoint < 0 {
		return nil, httpx.BadRequest("VALIDATION", "rates must not be negative")
	}
	if req.BaseRatePerLitre+req.FatRatePerPoint+req.SNFRatePerPoint == 0 {
		return nil, httpx.BadRequest("VALIDATION", "at least one rate component must be positive")
	}
	if _, err := s.d.Orgs.Get(ctx, req.OrgUnitID); err != nil {
		return nil, err
	}
	if err := s.requireInScope(ctx, actor, req.OrgUnitID, "create rate chart"); err != nil {
		return nil, err
	}
	createdBy, err := actorID(actor)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	effectiveFrom := now
	if req.EffectiveFrom != nil {
		effectiveFrom = req.EffectiveFrom.UTC()
	}
	if err := s.repo.deactivateActiveCharts(ctx, req.OrgUnitID); err != nil {
		return nil, err
	}
	version := req.Version
	if version == "" {
		// Auto-derive a stable, human-readable version so pricing is always
		// pinnable even when the caller omits one (§6.3): YYYY-MM + short id.
		version = effectiveFrom.Format("2006-01") + "-" + primitive.NewObjectID().Hex()[:6]
	}
	chart := &domain.RateChart{
		ID:               primitive.NewObjectID(),
		OrgUnitID:        req.OrgUnitID,
		Name:             req.Name,
		Version:          version,
		BaseRatePerLitre: req.BaseRatePerLitre,
		FatRatePerPoint:  req.FatRatePerPoint,
		SNFRatePerPoint:  req.SNFRatePerPoint,
		EffectiveFrom:    effectiveFrom,
		Active:           true,
		CreatedBy:        createdBy,
		CreatedAt:        now,
	}
	if err := s.repo.insertRateChart(ctx, chart); err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, "rate chart created",
		slog.String("rate_chart_id", chart.ID.Hex()),
		slog.String("org_unit_id", chart.OrgUnitID.Hex()),
		slog.String("name", chart.Name),
		slog.Float64("base_rate_per_litre", chart.BaseRatePerLitre),
		slog.String("actor_party_id", actor.PartyID))
	return chart, nil
}

// ResolveActiveChart is the HTTP-facing chart resolution: it enforces the
// caller's org scope over the DCS (pricing policy is confidential — any
// authenticated user must not be able to enumerate other unions' rates) and
// then delegates to the unchecked resolver.
func (s *service) ResolveActiveChart(ctx context.Context, actor auth.Actor, dcsID primitive.ObjectID) (*domain.RateChart, error) {
	dcs, err := s.d.Orgs.Get(ctx, dcsID)
	if err != nil {
		return nil, err
	}
	if dcs.Type != domain.OrgTypeDCS {
		return nil, httpx.BadRequest("NOT_A_DCS", "org unit "+dcsID.Hex()+" is not a DCS")
	}
	if err := s.requireInScope(ctx, actor, dcsID, "resolve rate chart"); err != nil {
		return nil, err
	}
	return s.resolveActiveChartForDCS(ctx, dcsID)
}

// resolveActiveChartForDCS finds the rate chart pricing a DCS's pours: among
// active charts already effective on the DCS itself or any of its ancestors
// (org_unit_id ∈ dcs.Path ∪ {dcs.ID}), the nearest org wins; ties break to
// the latest effective_from. Returns httpx.NotFound when no chart covers the
// DCS. Scope enforcement is the caller's job — the pour pricing path has
// already checked the actor's scope.
func (s *service) resolveActiveChartForDCS(ctx context.Context, dcsID primitive.ObjectID) (*domain.RateChart, error) {
	dcs, err := s.d.Orgs.Get(ctx, dcsID)
	if err != nil {
		return nil, err
	}
	if dcs.Type != domain.OrgTypeDCS {
		return nil, httpx.BadRequest("NOT_A_DCS", "org unit "+dcsID.Hex()+" is not a DCS")
	}
	return s.resolveActiveChartForOrgUnit(ctx, dcs)
}

// ResolveActiveChartForOrg returns the active chart covering an org unit
// directly — the admin masters rate view is ORG-scoped (a union/federation
// admin edits the chart they set), not DCS-scoped like the pour path. Scope is
// enforced against the actor.
func (s *service) ResolveActiveChartForOrg(ctx context.Context, actor auth.Actor, orgID primitive.ObjectID) (*domain.RateChart, error) {
	org, err := s.d.Orgs.Get(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if err := s.requireInScope(ctx, actor, orgID, "resolve rate chart"); err != nil {
		return nil, err
	}
	return s.resolveActiveChartForOrgUnit(ctx, org)
}

// resolveActiveChartForOrgUnit finds the active rate chart covering an org:
// among active charts already effective on the org itself or any of its
// ancestors (org_unit_id ∈ org.Path ∪ {org.ID}), the nearest org wins; ties
// break to the latest effective_from. Works at any level — a DCS pour resolves
// upward, a union/federation admin sees the chart they set. NotFound when no
// chart covers the org. Scope enforcement is the caller's job.
func (s *service) resolveActiveChartForOrgUnit(ctx context.Context, org *domain.OrgUnit) (*domain.RateChart, error) {
	// Distance 0 = the org itself; Path is root→parent, so walk it backwards.
	distance := map[primitive.ObjectID]int{org.ID: 0}
	orgIDs := []primitive.ObjectID{org.ID}
	for i := len(org.Path) - 1; i >= 0; i-- {
		distance[org.Path[i]] = len(org.Path) - i
		orgIDs = append(orgIDs, org.Path[i])
	}

	charts, err := s.repo.activeChartsForOrgs(ctx, orgIDs, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	var best *domain.RateChart
	for i := range charts {
		c := &charts[i]
		if best == nil {
			best = c
			continue
		}
		dc, db := distance[c.OrgUnitID], distance[best.OrgUnitID]
		if dc < db || (dc == db && c.EffectiveFrom.After(best.EffectiveFrom)) {
			best = c
		}
	}
	if best == nil {
		return nil, httpx.NotFound("active rate chart for org " + org.ID.Hex())
	}
	return best, nil
}

// --- analyzer readings ---

// deriveIntegrityFlags computes the anti-tamper flags of a reading from its
// capture envelope (blueprint §8.2). Plausibility flags are appended
// separately by the caller.
func deriveIntegrityFlags(mode string, ocrConfidence float64, hasGeotag bool, deviceTimestamp, serverNow time.Time) []string {
	var out []string
	if mode == domain.ReadingModeManual {
		out = append(out, domain.IntegrityFlagManualEntry)
	}
	if mode == domain.ReadingModePhotoOCR && ocrConfidence < lowOCRConfidenceThreshold {
		out = append(out, domain.IntegrityFlagLowOCRConfidence)
	}
	if !hasGeotag {
		out = append(out, domain.IntegrityFlagMissingGeotag)
	}
	if !deviceTimestamp.IsZero() {
		skew := serverNow.Sub(deviceTimestamp)
		if skew < 0 {
			skew = -skew
		}
		if skew > maxClockSkew {
			out = append(out, domain.IntegrityFlagClockSkew)
		}
	}
	return out
}

// CreateReading stores one analyzer measurement. Suspicious readings are
// flagged, never rejected — the anomaly trail is the deterrent (§8.2).
func (s *service) CreateReading(ctx context.Context, actor auth.Actor, req CreateReadingRequest) (*domain.AnalyzerReading, error) {
	if req.DCSID.IsZero() {
		return nil, httpx.BadRequest("VALIDATION", "dcs_id is required")
	}
	switch req.Mode {
	case domain.ReadingModeDirect, domain.ReadingModePhotoOCR, domain.ReadingModeManual:
	default:
		return nil, httpx.BadRequest("VALIDATION", "mode must be ANALYZER_DIRECT, PHOTO_OCR or MANUAL")
	}
	if req.FatPct <= 0 || req.SNFPct <= 0 {
		return nil, httpx.BadRequest("VALIDATION", "fat_pct and snf_pct are required")
	}
	if err := s.requireInScope(ctx, actor, req.DCSID, "create reading"); err != nil {
		return nil, err
	}
	recordedBy, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	if req.Mode == domain.ReadingModePhotoOCR {
		if !s.d.Flags.Enabled(ctx, flags.FlagPhotoOCR) {
			s.log.WarnContext(ctx, "reading rejected: photo-OCR feature disabled",
				slog.String("dcs_id", req.DCSID.Hex()),
				slog.String("actor_party_id", actor.PartyID))
			featureErr := httpx.Forbidden("photo-OCR ingestion is disabled on this deployment")
			featureErr.Code = "FEATURE_DISABLED"
			return nil, featureErr
		}
		if req.PhotoObjectKey == "" {
			return nil, httpx.BadRequest("VALIDATION", "photo_object_key is required for PHOTO_OCR (evidence retention, §8.2)")
		}
	}

	now := time.Now().UTC()
	var deviceTS time.Time
	if req.DeviceTimestamp != nil {
		deviceTS = req.DeviceTimestamp.UTC()
	}
	hasGeo := req.GeoLat != nil && req.GeoLng != nil
	integrityFlags := deriveIntegrityFlags(req.Mode, req.OCRConfidence, hasGeo, deviceTS, now)
	plausibilityFlags := domain.CheckPlausibility(req.FatPct, req.SNFPct, req.QuantityLitres)
	integrityFlags = append(integrityFlags, plausibilityFlags...)

	reading := &domain.AnalyzerReading{
		ID:               primitive.NewObjectID(),
		DCSID:            req.DCSID,
		DeviceID:         req.DeviceID,
		Mode:             req.Mode,
		FatPct:           req.FatPct,
		SNFPct:           req.SNFPct,
		CLR:              req.CLR,
		WaterPct:         req.WaterPct,
		QuantityLitres:   req.QuantityLitres,
		PhotoObjectKey:   req.PhotoObjectKey,
		OCRConfidence:    req.OCRConfidence,
		DeviceTimestamp:  deviceTS,
		ServerReceivedAt: now,
		IntegrityFlags:   integrityFlags,
		PlausibilityOK:   len(plausibilityFlags) == 0,
		RecordedBy:       recordedBy,
		CreatedAt:        now,
	}
	if hasGeo {
		reading.GeoLat, reading.GeoLng = *req.GeoLat, *req.GeoLng
	}
	if err := s.repo.insertReading(ctx, reading); err != nil {
		return nil, err
	}

	if _, err := s.d.Ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventReadingRecorded,
		EntityType: domain.EntityAnalyzerReading,
		EntityID:   reading.ID.Hex(),
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  reading.DCSID.Hex(),
		Payload: map[string]any{
			"mode":            reading.Mode,
			"integrity_flags": reading.IntegrityFlags,
		},
	}); err != nil {
		err = fmt.Errorf("append reading provenance: %w", err)
		s.log.ErrorContext(ctx, "reading provenance append failed",
			slog.String("reading_id", reading.ID.Hex()), slog.Any("err", err))
		return nil, httpx.Internal(err)
	}
	if len(integrityFlags) > 0 {
		s.log.WarnContext(ctx, "reading recorded with integrity flags",
			slog.String("reading_id", reading.ID.Hex()),
			slog.String("dcs_id", reading.DCSID.Hex()),
			slog.String("mode", reading.Mode),
			slog.Any("integrity_flags", integrityFlags),
			slog.String("actor_party_id", actor.PartyID))
	}
	s.log.InfoContext(ctx, "reading recorded",
		slog.String("reading_id", reading.ID.Hex()),
		slog.String("dcs_id", reading.DCSID.Hex()),
		slog.String("mode", reading.Mode),
		slog.Float64("fat_pct", reading.FatPct),
		slog.Float64("snf_pct", reading.SNFPct),
		slog.Bool("plausibility_ok", reading.PlausibilityOK),
		slog.String("actor_party_id", actor.PartyID))
	return reading, nil
}

// ListReadings pages a DCS's readings for anomaly review, optionally
// restricted to one IST day.
func (s *service) ListReadings(ctx context.Context, actor auth.Actor, dcsID primitive.ObjectID, date string, page httpx.Page) ([]domain.AnalyzerReading, int64, error) {
	if dcsID.IsZero() {
		return nil, 0, httpx.BadRequest("VALIDATION", "dcs_id is required")
	}
	if err := s.requireInScope(ctx, actor, dcsID, "list readings"); err != nil {
		return nil, 0, err
	}
	var dayStart, dayEnd *time.Time
	if date != "" {
		start, end, err := istDayRange(date)
		if err != nil {
			return nil, 0, err
		}
		dayStart, dayEnd = &start, &end
	}
	return s.repo.listReadings(ctx, dcsID, dayStart, dayEnd, page)
}

// --- pours ---

// CreatePour records one farmer's pour with same-day pricing. Replays of the
// same client_event_id return the stored pour with idempotentReplay=true —
// offline sync retries are harmless (§3.1).
func (s *service) CreatePour(ctx context.Context, actor auth.Actor, req CreatePourRequest) (pour *domain.MilkPour, idempotentReplay bool, err error) {
	if req.ClientEventID == "" || req.FarmerPartyID.IsZero() || req.DCSID.IsZero() {
		return nil, false, httpx.BadRequest("VALIDATION", "client_event_id, farmer_party_id and dcs_id are required")
	}
	if req.Shift != domain.ShiftMorning && req.Shift != domain.ShiftEvening {
		return nil, false, httpx.BadRequest("VALIDATION", "shift must be MORNING or EVENING")
	}
	if req.QuantityLitres <= 0 {
		return nil, false, httpx.BadRequest("VALIDATION", "quantity_litres must be positive")
	}
	switch req.Source {
	case domain.ReadingModeDirect, domain.ReadingModePhotoOCR, domain.ReadingModeManual:
	default:
		return nil, false, httpx.BadRequest("VALIDATION", "source must be ANALYZER_DIRECT, PHOTO_OCR or MANUAL")
	}
	if err := s.requireInScope(ctx, actor, req.DCSID, "create pour"); err != nil {
		return nil, false, err
	}
	recordedBy, err := actorID(actor)
	if err != nil {
		return nil, false, err
	}

	// Idempotency short-circuit BEFORE any state-dependent business check:
	// a replay of an already-stored pour must return the stored pour even if
	// membership lapsed or the rate chart changed since first sync — otherwise
	// offline devices keep the record dirty and retry forever.
	if existing, err := s.repo.pourByClientEventID(ctx, req.ClientEventID); err == nil {
		return s.replayStoredPour(ctx, actor, existing)
	} else if !isNotFoundErr(err) {
		return nil, false, err
	}

	now := time.Now().UTC()
	member, err := s.repo.farmerIsActiveMember(ctx, req.FarmerPartyID, req.DCSID, now)
	if err != nil {
		return nil, false, err
	}
	if !member {
		s.log.WarnContext(ctx, "pour rejected: farmer not an active member",
			slog.String("farmer_party_id", req.FarmerPartyID.Hex()),
			slog.String("dcs_id", req.DCSID.Hex()),
			slog.String("client_event_id", req.ClientEventID),
			slog.String("actor_party_id", actor.PartyID))
		return nil, false, httpx.Unprocessable("FARMER_NOT_MEMBER",
			"farmer does not hold an active FARMER assignment at this DCS")
	}

	// HARD plausibility gate: receipts must be trustworthy, so implausible
	// values are rejected here (unlike readings, which are merely flagged).
	if violations := domain.CheckPlausibility(req.FatPct, req.SNFPct, req.QuantityLitres); len(violations) > 0 {
		s.log.WarnContext(ctx, "pour rejected: implausible values",
			slog.String("farmer_party_id", req.FarmerPartyID.Hex()),
			slog.String("dcs_id", req.DCSID.Hex()),
			slog.String("client_event_id", req.ClientEventID),
			slog.Float64("fat_pct", req.FatPct),
			slog.Float64("snf_pct", req.SNFPct),
			slog.Float64("quantity_litres", req.QuantityLitres),
			slog.String("actor_party_id", actor.PartyID))
		return nil, false, httpx.Unprocessable("IMPLAUSIBLE_VALUES",
			"fat/SNF/quantity outside physically plausible bounds")
	}

	chart, err := s.resolveChartForPricing(ctx, req.DCSID)
	if err != nil {
		return nil, false, err
	}
	rate, amount := chart.PricePour(req.FatPct, req.SNFPct, req.QuantityLitres)

	pouredAt := now
	if req.PouredAt != nil {
		pouredAt = req.PouredAt.UTC()
	}

	// A shift whose consignment already exists is sealed: milk recorded now
	// would be invoiced but never board the van — quantity provenance and the
	// consignment's totals would silently diverge (§7 pooling boundary).
	pourDate := domain.DateKeyIST(pouredAt)
	consigned, err := s.repo.shiftConsigned(ctx, req.DCSID, pourDate, req.Shift)
	if err != nil {
		return nil, false, err
	}
	if consigned {
		s.log.WarnContext(ctx, "pour rejected: shift already consigned",
			slog.String("dcs_id", req.DCSID.Hex()),
			slog.String("pour_date", pourDate),
			slog.String("shift", req.Shift),
			slog.String("client_event_id", req.ClientEventID),
			slog.String("actor_party_id", actor.PartyID))
		return nil, false, httpx.Conflict("SHIFT_CONSIGNED",
			"a consignment already pools this DCS shift — the pour set is sealed for "+pourDate+" "+req.Shift)
	}
	// Pre-generated _id keeps the insert and the ledger refs consistent in
	// one flow.
	p := &domain.MilkPour{
		ID:                primitive.NewObjectID(),
		ClientEventID:     req.ClientEventID,
		FarmerPartyID:     req.FarmerPartyID,
		AnimalID:          req.AnimalID,
		DCSID:             req.DCSID,
		Shift:             req.Shift,
		PourDate:          pourDate,
		QuantityLitres:    req.QuantityLitres,
		FatPct:            req.FatPct,
		SNFPct:            req.SNFPct,
		CLR:               req.CLR,
		TemperatureC:      req.TemperatureC,
		RatePerLitre:      rate,
		Amount:            amount,
		RateChartID:       chart.ID,
		RateChartVersion:  chart.Version, // §6.3 pin the pricing version on the pour
		AnalyzerReadingID: req.AnalyzerReadingID,
		Source:            req.Source,
		Assurance:         domain.AssuranceForSource(req.Source), // §6.2 capture assurance
		Status:            domain.PourStatusRecorded,
		PouredAt:          pouredAt,
		RecordedBy:        recordedBy,
		DeviceID:          req.DeviceID,
		CreatedAt:         now,
	}
	if req.GeoLat != nil && req.GeoLng != nil {
		p.GeoLat, p.GeoLng = *req.GeoLat, *req.GeoLng
	}

	duplicate, err := s.repo.insertPour(ctx, p)
	if err != nil {
		return nil, false, err
	}
	if duplicate {
		// Race backstop: two concurrent requests both missed the lookup —
		// the unique client_event_id index caught it at insert time.
		existing, err := s.repo.pourByClientEventID(ctx, req.ClientEventID)
		if err != nil {
			return nil, false, err
		}
		return s.replayStoredPour(ctx, actor, existing)
	}

	if err := s.recordPourProvenance(ctx, actor, p); err != nil {
		return nil, false, err
	}
	s.log.InfoContext(ctx, "pour recorded",
		slog.String("pour_id", p.ID.Hex()),
		slog.String("farmer_party_id", p.FarmerPartyID.Hex()),
		slog.String("dcs_id", p.DCSID.Hex()),
		slog.String("pour_date", p.PourDate),
		slog.String("shift", p.Shift),
		slog.Float64("quantity_litres", p.QuantityLitres),
		slog.Float64("rate_per_litre", p.RatePerLitre),
		slog.Float64("amount", p.Amount),
		slog.String("actor_party_id", actor.PartyID))
	s.announcePour(ctx, p)
	return p, false, nil
}

// replayStoredPour returns an already-persisted pour as an idempotent replay.
// If a previous attempt persisted the pour but died before the ledger append
// (provenance_seq still 0), the missing pour.recorded event, bus publish and
// receipt SMS are healed first — a failed heal keeps returning the error so
// the client retries until the saga completes, instead of blessing an orphan
// with no origin link on the hash chain.
func (s *service) replayStoredPour(ctx context.Context, actor auth.Actor, existing *domain.MilkPour) (*domain.MilkPour, bool, error) {
	if existing.Status == domain.PourStatusRecorded && existing.ProvenanceSeq == 0 {
		// The append itself must stay idempotent: a pour.recorded event may
		// already be chained if only the seq back-stamp failed last time.
		evs, err := s.d.Ledger.EventsForEntity(ctx, domain.EntityMilkPour, existing.ID.Hex())
		if err != nil {
			err = fmt.Errorf("check pour provenance: %w", err)
			s.log.ErrorContext(ctx, "pour provenance check failed",
				slog.String("pour_id", existing.ID.Hex()), slog.Any("err", err))
			return nil, false, httpx.Internal(err)
		}
		var chained *provenance.Event
		for i := range evs {
			if evs[i].Type == domain.EventPourRecorded {
				chained = &evs[i]
				break
			}
		}
		if chained != nil {
			if err := s.repo.setPourProvenanceSeq(ctx, existing.ID, chained.Seq); err != nil {
				return nil, false, err
			}
			existing.ProvenanceSeq = chained.Seq
		} else {
			if err := s.recordPourProvenance(ctx, actor, existing); err != nil {
				return nil, false, err
			}
			s.announcePour(ctx, existing)
		}
	}
	s.log.WarnContext(ctx, "pour replayed idempotently",
		slog.String("pour_id", existing.ID.Hex()),
		slog.String("client_event_id", existing.ClientEventID),
		slog.String("farmer_party_id", existing.FarmerPartyID.Hex()),
		slog.String("dcs_id", existing.DCSID.Hex()),
		slog.String("actor_party_id", actor.PartyID))
	return existing, true, nil
}

// announcePour publishes the pour on the bus and queues the farmer's receipt
// SMS — the anti-cheating receipt (§8.1).
func (s *service) announcePour(ctx context.Context, p *domain.MilkPour) {
	s.d.Bus.Publish(eventbus.TopicPourRecorded, *p)
	s.queueFarmerSMS(ctx, p.FarmerPartyID, domain.TemplatePourReceipt, map[string]string{
		"quantity": fmt.Sprintf("%.2f", p.QuantityLitres), // key must match renderSMS's p("quantity")
		"rate":     fmt.Sprintf("%.2f", p.RatePerLitre),
		"amount":   fmt.Sprintf("%.2f", p.Amount),
	})
}

// isNotFoundErr reports whether err is a 404 AppError.
func isNotFoundErr(err error) bool {
	var appErr *httpx.AppError
	return errors.As(err, &appErr) && appErr.Status == http.StatusNotFound
}

// recordPourProvenance appends EventPourRecorded and stamps the ledger seq
// onto the pour.
func (s *service) recordPourProvenance(ctx context.Context, actor auth.Actor, p *domain.MilkPour) error {
	refs := []provenance.Ref{
		{EntityType: domain.EntityParty, EntityID: p.FarmerPartyID.Hex(), Relation: "produced_by"},
	}
	if p.AnimalID != nil {
		refs = append(refs, provenance.Ref{EntityType: domain.EntityAnimal, EntityID: p.AnimalID.Hex(), Relation: "from_animal"})
	}
	if p.AnalyzerReadingID != nil {
		refs = append(refs, provenance.Ref{EntityType: domain.EntityAnalyzerReading, EntityID: p.AnalyzerReadingID.Hex(), Relation: "measured_by"})
	}
	ev, err := s.d.Ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventPourRecorded,
		EntityType: domain.EntityMilkPour,
		EntityID:   p.ID.Hex(),
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  p.DCSID.Hex(),
		Payload: map[string]any{
			"quantity_litres": p.QuantityLitres,
			"rate_per_litre":  p.RatePerLitre,
			"amount":          p.Amount,
		},
	})
	if err != nil {
		err = fmt.Errorf("append pour provenance: %w", err)
		s.log.ErrorContext(ctx, "pour provenance append failed",
			slog.String("pour_id", p.ID.Hex()), slog.Any("err", err))
		return httpx.Internal(err)
	}
	if err := s.repo.setPourProvenanceSeq(ctx, p.ID, ev.Seq); err != nil {
		return err
	}
	p.ProvenanceSeq = ev.Seq
	return nil
}

// resolveChartForPricing wraps resolveActiveChartForDCS, converting "no
// chart" into a 422 — the pour payload itself is fine, the DCS just cannot
// price it yet.
func (s *service) resolveChartForPricing(ctx context.Context, dcsID primitive.ObjectID) (*domain.RateChart, error) {
	chart, err := s.resolveActiveChartForDCS(ctx, dcsID)
	if err != nil {
		var appErr *httpx.AppError
		if errors.As(err, &appErr) && appErr.Status == http.StatusNotFound {
			s.log.WarnContext(ctx, "pour rejected: no active rate chart",
				slog.String("dcs_id", dcsID.Hex()))
			return nil, httpx.Unprocessable("NO_ACTIVE_RATE_CHART",
				"no active rate chart covers this DCS — pours cannot be priced")
		}
		return nil, err
	}
	return chart, nil
}

// BatchSyncPours replays up to 500 offline-captured pours. Items succeed or
// fail independently — the batch never aborts (offline reconnect path, §3.1).
func (s *service) BatchSyncPours(ctx context.Context, actor auth.Actor, req BatchSyncRequest) ([]BatchSyncItemResult, error) {
	if len(req.Pours) == 0 {
		return nil, httpx.BadRequest("VALIDATION", "pours must not be empty")
	}
	if len(req.Pours) > maxBatchSyncItems {
		return nil, httpx.BadRequest("VALIDATION",
			fmt.Sprintf("pours must not exceed %d items per batch", maxBatchSyncItems))
	}
	results := make([]BatchSyncItemResult, 0, len(req.Pours))
	for _, item := range req.Pours {
		res := BatchSyncItemResult{ClientEventID: item.ClientEventID}
		pour, replay, err := s.CreatePour(ctx, actor, item)
		switch {
		case err != nil:
			res.Status = BatchItemError
			var appErr *httpx.AppError
			if errors.As(err, &appErr) {
				res.Error = appErr.Code + ": " + appErr.Message
			} else {
				res.Error = "INTERNAL: internal server error"
			}
		case replay:
			res.Status = BatchItemDuplicate
			res.PourID = pour.ID.Hex()
		default:
			res.Status = BatchItemCreated
			res.PourID = pour.ID.Hex()
		}
		results = append(results, res)
	}
	return results, nil
}

// SupersedePour applies an append-only correction (§3.4): the old pour is
// marked SUPERSEDED (the document stays), and a new repriced pour referencing
// it is recorded.
func (s *service) SupersedePour(ctx context.Context, actor auth.Actor, pourID primitive.ObjectID, req SupersedePourRequest) (*domain.MilkPour, error) {
	if req.Reason == "" {
		return nil, httpx.BadRequest("VALIDATION", "reason is required")
	}
	old, err := s.repo.pourByID(ctx, pourID)
	if err != nil {
		return nil, err
	}
	if err := s.requireInScope(ctx, actor, old.DCSID, "supersede pour"); err != nil {
		return nil, err
	}
	recordedBy, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	if old.Status != domain.PourStatusRecorded {
		s.log.WarnContext(ctx, "supersede rejected: pour not correctable",
			slog.String("pour_id", old.ID.Hex()),
			slog.String("status", old.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("POUR_NOT_CORRECTABLE",
			"pour is already "+old.Status+" and cannot be superseded")
	}
	invoiced, err := s.repo.pourIsInvoiced(ctx, pourID)
	if err != nil {
		return nil, err
	}
	if invoiced {
		s.log.WarnContext(ctx, "supersede rejected: pour already invoiced",
			slog.String("pour_id", old.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("POUR_INVOICED",
			"pour is aggregated by an invoice — correct it through settlement instead")
	}
	// The consigned state freezes the pour exactly like the invoiced state:
	// a consignment's pour set, litres and weighted fat/SNF are snapshotted,
	// so correcting after pooling would silently desync payment from the
	// shipped-quantity provenance.
	consigned, err := s.repo.shiftConsigned(ctx, old.DCSID, old.PourDate, old.Shift)
	if err != nil {
		return nil, err
	}
	if consigned {
		s.log.WarnContext(ctx, "supersede rejected: shift already consigned",
			slog.String("pour_id", old.ID.Hex()),
			slog.String("dcs_id", old.DCSID.Hex()),
			slog.String("pour_date", old.PourDate),
			slog.String("shift", old.Shift),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("POUR_CONSIGNED",
			"pour is aggregated by a consignment — correct it through logistics/settlement instead")
	}

	qty, fat, snf := old.QuantityLitres, old.FatPct, old.SNFPct
	if req.Corrected.QuantityLitres != nil {
		qty = *req.Corrected.QuantityLitres
	}
	if req.Corrected.FatPct != nil {
		fat = *req.Corrected.FatPct
	}
	if req.Corrected.SNFPct != nil {
		snf = *req.Corrected.SNFPct
	}
	if qty <= 0 {
		return nil, httpx.BadRequest("VALIDATION", "corrected quantity_litres must be positive")
	}
	if violations := domain.CheckPlausibility(fat, snf, qty); len(violations) > 0 {
		s.log.WarnContext(ctx, "supersede rejected: implausible corrected values",
			slog.String("pour_id", old.ID.Hex()),
			slog.Float64("fat_pct", fat),
			slog.Float64("snf_pct", snf),
			slog.Float64("quantity_litres", qty),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable("IMPLAUSIBLE_VALUES",
			"corrected fat/SNF/quantity outside physically plausible bounds")
	}

	chart, err := s.resolveChartForPricing(ctx, old.DCSID)
	if err != nil {
		return nil, err
	}
	rate, amount := chart.PricePour(fat, snf, qty)

	// Optimistic guard: only one correction can win the RECORDED→SUPERSEDED flip.
	flipped, err := s.repo.markPourSuperseded(ctx, old.ID)
	if err != nil {
		return nil, err
	}
	if !flipped {
		s.log.WarnContext(ctx, "supersede rejected: pour superseded concurrently",
			slog.String("pour_id", old.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("POUR_NOT_CORRECTABLE", "pour was superseded concurrently")
	}

	now := time.Now().UTC()
	corrected := &domain.MilkPour{
		ID:                primitive.NewObjectID(),
		FarmerPartyID:     old.FarmerPartyID,
		AnimalID:          old.AnimalID,
		DCSID:             old.DCSID,
		Shift:             old.Shift,
		PourDate:          old.PourDate, // correction stays on the original settlement day
		QuantityLitres:    qty,
		FatPct:            fat,
		SNFPct:            snf,
		CLR:               old.CLR,
		TemperatureC:      old.TemperatureC, // a correction keeps the original cold-chain reading
		RatePerLitre:      rate,
		Amount:            amount,
		RateChartID:       chart.ID,
		RateChartVersion:  chart.Version,
		AnalyzerReadingID: old.AnalyzerReadingID,
		Source:            old.Source,
		Assurance:         old.Assurance, // a correction keeps the original capture assurance
		Status:            domain.PourStatusRecorded,
		SupersedesPourID:  &old.ID,
		PouredAt:          old.PouredAt,
		RecordedBy:        recordedBy,
		DeviceID:          old.DeviceID,
		GeoLat:            old.GeoLat,
		GeoLng:            old.GeoLng,
		CreatedAt:         now,
	}
	corrected.ClientEventID = old.ClientEventID + "::corr::" + corrected.ID.Hex()[0:8]
	if duplicate, err := s.repo.insertPour(ctx, corrected); err != nil {
		return nil, err
	} else if duplicate {
		s.log.WarnContext(ctx, "supersede rejected: correction collided",
			slog.String("pour_id", old.ID.Hex()),
			slog.String("client_event_id", corrected.ClientEventID),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("POUR_NOT_CORRECTABLE", "correction collided with a concurrent one")
	}

	ev, err := s.d.Ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventPourSuperseded,
		EntityType: domain.EntityMilkPour,
		EntityID:   corrected.ID.Hex(),
		Refs: []provenance.Ref{
			{EntityType: domain.EntityMilkPour, EntityID: old.ID.Hex(), Relation: "supersedes"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: corrected.DCSID.Hex(),
		Payload:   map[string]any{"reason": req.Reason},
	})
	if err != nil {
		err = fmt.Errorf("append supersede provenance: %w", err)
		s.log.ErrorContext(ctx, "supersede provenance append failed",
			slog.String("pour_id", corrected.ID.Hex()), slog.Any("err", err))
		return nil, httpx.Internal(err)
	}
	if err := s.repo.setPourProvenanceSeq(ctx, corrected.ID, ev.Seq); err != nil {
		return nil, err
	}
	corrected.ProvenanceSeq = ev.Seq
	s.log.InfoContext(ctx, "pour superseded",
		slog.String("pour_id", corrected.ID.Hex()),
		slog.String("supersedes_pour_id", old.ID.Hex()),
		slog.String("farmer_party_id", corrected.FarmerPartyID.Hex()),
		slog.String("dcs_id", corrected.DCSID.Hex()),
		slog.Float64("quantity_litres", corrected.QuantityLitres),
		slog.Float64("amount", corrected.Amount),
		slog.String("reason", req.Reason),
		slog.String("actor_party_id", actor.PartyID))
	return corrected, nil
}

// ListPours pages pours. FARMER callers are always pinned to their own
// pours; staff roles must name a DCS inside their scope.
func (s *service) ListPours(ctx context.Context, actor auth.Actor, filter pourListFilter, page httpx.Page) ([]domain.MilkPour, int64, error) {
	if actor.RoleCode == domain.RoleFarmer {
		self, err := actorID(actor)
		if err != nil {
			return nil, 0, err
		}
		filter.FarmerPartyID = self // farmers only ever see their own pours
	} else {
		if filter.DCSID.IsZero() {
			return nil, 0, httpx.BadRequest("VALIDATION", "dcs_id is required")
		}
		if err := s.requireInScope(ctx, actor, filter.DCSID, "list pours"); err != nil {
			return nil, 0, err
		}
	}
	if filter.Date != "" {
		if err := validateDateKey(filter.Date); err != nil {
			return nil, 0, err
		}
	}
	if filter.Shift != "" && filter.Shift != domain.ShiftMorning && filter.Shift != domain.ShiftEvening {
		return nil, 0, httpx.BadRequest("VALIDATION", "shift must be MORNING or EVENING")
	}
	return s.repo.listPours(ctx, filter, page)
}

// --- invoices ---

// GenerateInvoices groups the day's un-invoiced RECORDED pours by farmer and
// issues one invoice each — the same-day payment artefact (§8.1). The unique
// farmer+DCS+day index (on ObjectIDs) is the true duplicate guard; racing
// generators simply count the loser as "existing".
func (s *service) GenerateInvoices(ctx context.Context, actor auth.Actor, req GenerateInvoicesRequest) (*GenerateInvoicesResponse, error) {
	if req.DCSID.IsZero() {
		return nil, httpx.BadRequest("VALIDATION", "dcs_id is required")
	}
	now := time.Now().UTC()
	dateKey := req.Date
	if dateKey == "" {
		dateKey = domain.DateKeyIST(now)
	} else if err := validateDateKey(dateKey); err != nil {
		return nil, err
	}
	dcs, err := s.d.Orgs.Get(ctx, req.DCSID)
	if err != nil {
		return nil, err
	}
	if dcs.Type != domain.OrgTypeDCS {
		return nil, httpx.BadRequest("NOT_A_DCS", "org unit "+req.DCSID.Hex()+" is not a DCS")
	}
	if err := s.requireInScope(ctx, actor, req.DCSID, "generate invoices"); err != nil {
		return nil, err
	}

	pours, err := s.repo.recordedPoursForDay(ctx, req.DCSID, dateKey)
	if err != nil {
		return nil, err
	}
	alreadyInvoiced, err := s.repo.invoicedPourIDs(ctx, req.DCSID, dateKey)
	if err != nil {
		return nil, err
	}

	byFarmer := make(map[primitive.ObjectID][]domain.MilkPour)
	for _, p := range pours {
		if alreadyInvoiced[p.ID] {
			continue
		}
		byFarmer[p.FarmerPartyID] = append(byFarmer[p.FarmerPartyID], p)
	}
	farmerIDs := make([]primitive.ObjectID, 0, len(byFarmer))
	for id := range byFarmer {
		farmerIDs = append(farmerIDs, id)
	}
	sort.Slice(farmerIDs, func(i, j int) bool { return farmerIDs[i].Hex() < farmerIDs[j].Hex() })

	resp := &GenerateInvoicesResponse{Invoices: []domain.Invoice{}}
	// Counter keys stay strings and keep using the org CODE — internal only.
	counterKey := "invoice:" + dcs.Code + ":" + dateKey
	for _, farmerID := range farmerIDs {
		group := byFarmer[farmerID]
		seq, err := s.repo.nextInvoiceSeq(ctx, counterKey)
		if err != nil {
			return nil, err
		}
		inv := &domain.Invoice{
			ID:            primitive.NewObjectID(),
			InvoiceNumber: domain.InvoiceNumberFor(dcs.Code, dateKey, seq),
			FarmerPartyID: farmerID,
			DCSID:         req.DCSID,
			InvoiceDate:   dateKey,
			Status:        domain.InvoiceStatusIssued,
			IssuedAt:      now,
		}
		for _, p := range group {
			inv.PourIDs = append(inv.PourIDs, p.ID)
			inv.TotalQuantityLitres += p.QuantityLitres
			inv.TotalAmount += p.Amount
		}
		inv.TotalQuantityLitres = math.Round(inv.TotalQuantityLitres*100) / 100
		inv.TotalAmount = math.Round(inv.TotalAmount*100) / 100

		duplicate, err := s.repo.insertInvoice(ctx, inv)
		if err != nil {
			return nil, err
		}
		if duplicate {
			// The farmer already has today's invoice (e.g. the MORNING run) —
			// merge the not-yet-invoiced pours into it instead of silently
			// dropping them, or surface them when the invoice is frozen.
			if err := s.mergeIntoExistingInvoice(ctx, actor, req.DCSID, dateKey, farmerID, group, resp); err != nil {
				return nil, err
			}
			continue
		}

		refs := make([]provenance.Ref, 0, len(inv.PourIDs))
		for _, pid := range inv.PourIDs {
			refs = append(refs, provenance.Ref{EntityType: domain.EntityMilkPour, EntityID: pid.Hex(), Relation: "aggregates"})
		}
		ev, err := s.d.Ledger.Append(ctx, provenance.AppendInput{
			Type:       domain.EventInvoiceIssued,
			EntityType: domain.EntityInvoice,
			EntityID:   inv.ID.Hex(),
			Refs:       refs,
			Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
			OrgUnitID:  inv.DCSID.Hex(),
			Payload: map[string]any{
				"invoice_number":        inv.InvoiceNumber,
				"total_quantity_litres": inv.TotalQuantityLitres,
				"total_amount":          inv.TotalAmount,
			},
		})
		if err != nil {
			err = fmt.Errorf("append invoice provenance: %w", err)
			s.log.ErrorContext(ctx, "invoice provenance append failed",
				slog.String("invoice_id", inv.ID.Hex()), slog.Any("err", err))
			return nil, httpx.Internal(err)
		}
		if err := s.repo.setInvoiceProvenanceSeq(ctx, inv.ID, ev.Seq); err != nil {
			return nil, err
		}
		inv.ProvenanceSeq = ev.Seq

		s.d.Bus.Publish(eventbus.TopicInvoiceIssued, *inv)
		s.queueInvoiceSMS(ctx, inv)
		s.log.InfoContext(ctx, "invoice issued",
			slog.String("invoice_id", inv.ID.Hex()),
			slog.String("invoice_number", inv.InvoiceNumber),
			slog.String("farmer_party_id", inv.FarmerPartyID.Hex()),
			slog.String("dcs_id", inv.DCSID.Hex()),
			slog.String("invoice_date", inv.InvoiceDate),
			slog.Int("pour_count", len(inv.PourIDs)),
			slog.Float64("total_amount", inv.TotalAmount),
			slog.String("actor_party_id", actor.PartyID))
		resp.Created++
		resp.Invoices = append(resp.Invoices, *inv)
	}
	return resp, nil
}

// mergeIntoExistingInvoice folds a farmer's not-yet-invoiced pours into their
// already-issued invoice for the day. Only a still-ISSUED invoice may be
// amended (the status guard on the update is atomic against a concurrent
// settlement claim); a PAID/settlement-claimed invoice is never mutated —
// its pours are surfaced in the response so staff can act, never dropped.
func (s *service) mergeIntoExistingInvoice(ctx context.Context, actor auth.Actor, dcsID primitive.ObjectID, dateKey string, farmerID primitive.ObjectID, group []domain.MilkPour, resp *GenerateInvoicesResponse) error {
	existing, err := s.repo.invoiceByFarmerDay(ctx, farmerID, dcsID, dateKey)
	if err != nil {
		return err
	}
	// Only pours genuinely missing from the invoice are merged — a racing
	// generator that lost the insert must not double-add the winner's pours.
	already := make(map[primitive.ObjectID]bool, len(existing.PourIDs))
	for _, id := range existing.PourIDs {
		already[id] = true
	}
	var newPours []domain.MilkPour
	for _, p := range group {
		if !already[p.ID] {
			newPours = append(newPours, p)
		}
	}
	if len(newPours) == 0 {
		resp.Existing++
		return nil
	}

	skip := func(reason string) {
		resp.Existing++
		skipped := make([]string, 0, len(newPours))
		for _, p := range newPours {
			resp.SkippedPourIDs = append(resp.SkippedPourIDs, p.ID)
			skipped = append(skipped, p.ID.Hex())
		}
		s.log.WarnContext(ctx, "invoice merge skipped: "+reason,
			slog.String("invoice_id", existing.ID.Hex()),
			slog.String("invoice_number", existing.InvoiceNumber),
			slog.String("farmer_party_id", farmerID.Hex()),
			slog.String("dcs_id", dcsID.Hex()),
			slog.Any("skipped_pour_ids", skipped),
			slog.String("actor_party_id", actor.PartyID))
	}
	if existing.Status != domain.InvoiceStatusIssued {
		skip("invoice frozen by settlement") // un-invoiceable today, surface loudly
		return nil
	}
	ids := make([]primitive.ObjectID, 0, len(newPours))
	var addQty, addAmount float64
	for _, p := range newPours {
		ids = append(ids, p.ID)
		addQty += p.QuantityLitres
		addAmount += p.Amount
	}
	merged, err := s.repo.appendPoursToInvoice(ctx, existing.ID, ids, addQty, addAmount)
	if err != nil {
		return err
	}
	if !merged {
		skip("invoice left ISSUED concurrently") // between the read and the guarded update
		return nil
	}
	updated, err := s.repo.invoiceByID(ctx, existing.ID)
	if err != nil {
		return err
	}
	updated.TotalQuantityLitres = math.Round(updated.TotalQuantityLitres*100) / 100
	updated.TotalAmount = math.Round(updated.TotalAmount*100) / 100
	if err := s.repo.setInvoiceTotals(ctx, updated.ID, updated.TotalQuantityLitres, updated.TotalAmount); err != nil {
		return err
	}

	refs := make([]provenance.Ref, 0, len(ids))
	for _, pid := range ids {
		refs = append(refs, provenance.Ref{EntityType: domain.EntityMilkPour, EntityID: pid.Hex(), Relation: "aggregates"})
	}
	ev, err := s.d.Ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventInvoiceAmended,
		EntityType: domain.EntityInvoice,
		EntityID:   updated.ID.Hex(),
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  updated.DCSID.Hex(),
		Payload: map[string]any{
			"invoice_number":        updated.InvoiceNumber,
			"added_pour_count":      len(ids),
			"total_quantity_litres": updated.TotalQuantityLitres,
			"total_amount":          updated.TotalAmount,
		},
	})
	if err != nil {
		err = fmt.Errorf("append invoice amendment provenance: %w", err)
		s.log.ErrorContext(ctx, "invoice amendment provenance append failed",
			slog.String("invoice_id", updated.ID.Hex()), slog.Any("err", err))
		return httpx.Internal(err)
	}
	if err := s.repo.setInvoiceProvenanceSeq(ctx, updated.ID, ev.Seq); err != nil {
		return err
	}
	updated.ProvenanceSeq = ev.Seq

	s.d.Bus.Publish(eventbus.TopicInvoiceIssued, *updated)
	s.queueInvoiceSMS(ctx, updated)
	s.log.InfoContext(ctx, "invoice amended",
		slog.String("invoice_id", updated.ID.Hex()),
		slog.String("invoice_number", updated.InvoiceNumber),
		slog.String("farmer_party_id", updated.FarmerPartyID.Hex()),
		slog.String("dcs_id", updated.DCSID.Hex()),
		slog.Int("added_pour_count", len(ids)),
		slog.Float64("total_amount", updated.TotalAmount),
		slog.String("actor_party_id", actor.PartyID))
	resp.Updated++
	resp.Invoices = append(resp.Invoices, *updated)
	return nil
}

// queueInvoiceSMS notifies the farmer of an issued or amended invoice.
func (s *service) queueInvoiceSMS(ctx context.Context, inv *domain.Invoice) {
	s.queueFarmerSMS(ctx, inv.FarmerPartyID, domain.TemplateInvoiceIssued, map[string]string{
		"invoice_number": inv.InvoiceNumber,
		"amount":         fmt.Sprintf("%.2f", inv.TotalAmount),
		"date":           inv.InvoiceDate,
	})
}

// ListInvoices pages invoices. FARMER callers are always pinned to their own
// invoices; staff roles must name a DCS inside their scope.
func (s *service) ListInvoices(ctx context.Context, actor auth.Actor, filter invoiceListFilter, page httpx.Page) ([]domain.Invoice, int64, error) {
	if actor.RoleCode == domain.RoleFarmer {
		self, err := actorID(actor)
		if err != nil {
			return nil, 0, err
		}
		filter.FarmerPartyID = self // farmers only ever see their own invoices
	} else {
		if filter.DCSID.IsZero() {
			return nil, 0, httpx.BadRequest("VALIDATION", "dcs_id is required")
		}
		if err := s.requireInScope(ctx, actor, filter.DCSID, "list invoices"); err != nil {
			return nil, 0, err
		}
	}
	if filter.Date != "" {
		if err := validateDateKey(filter.Date); err != nil {
			return nil, 0, err
		}
	}
	if filter.Status != "" {
		switch filter.Status {
		case domain.InvoiceStatusIssued, domain.InvoiceStatusSettlementPending,
			domain.InvoiceStatusPaid, domain.InvoiceStatusHold:
		default:
			return nil, 0, httpx.BadRequest("VALIDATION", "unknown invoice status "+filter.Status)
		}
	}
	return s.repo.listInvoices(ctx, filter, page)
}

// GetInvoice returns one invoice: a farmer may fetch their own, staff roles
// need the invoice's DCS inside their scope.
func (s *service) GetInvoice(ctx context.Context, actor auth.Actor, id primitive.ObjectID) (*domain.Invoice, error) {
	inv, err := s.repo.invoiceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if actor.RoleCode == domain.RoleFarmer {
		self, err := actorID(actor)
		if err != nil {
			return nil, err
		}
		if inv.FarmerPartyID != self {
			s.log.WarnContext(ctx, "invoice access denied: not the farmer's own",
				slog.String("invoice_id", inv.ID.Hex()),
				slog.String("actor_party_id", actor.PartyID))
			return nil, httpx.Forbidden("farmers may only view their own invoices")
		}
		return inv, nil
	}
	if err := s.requireInScope(ctx, actor, inv.DCSID, "get invoice"); err != nil {
		return nil, err
	}
	return inv, nil
}

// --- helpers ---

// queueFarmerSMS writes an outbox notification for the farmer. Failures are
// logged, never fatal — the receipt must not block the pour (§8.1).
func (s *service) queueFarmerSMS(ctx context.Context, farmerPartyID primitive.ObjectID, templateKey string, params map[string]string) {
	farmer, err := s.repo.getParty(ctx, farmerPartyID)
	if err != nil {
		s.log.WarnContext(ctx, "farmer lookup for SMS failed",
			slog.String("farmer_party_id", farmerPartyID.Hex()), slog.Any("err", err))
		return
	}
	language := farmer.PreferredLanguage
	if language == "" {
		language = defaultLanguage
	}
	n := &domain.Notification{
		ID:          primitive.NewObjectID(),
		PartyID:     &farmer.ID,
		Phone:       farmer.Phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: templateKey,
		Language:    language,
		Params:      params,
		Status:      domain.NotificationQueued,
		QueuedAt:    time.Now().UTC(),
	}
	if err := s.repo.queueNotification(ctx, n); err != nil {
		s.log.WarnContext(ctx, "notification queue failed",
			slog.String("template", templateKey),
			slog.String("farmer_party_id", farmerPartyID.Hex()), slog.Any("err", err))
	}
}

// validateDateKey checks a YYYY-MM-DD day key.
func validateDateKey(date string) error {
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return httpx.BadRequest("VALIDATION", "date must be YYYY-MM-DD")
	}
	return nil
}

// istDayRange converts a YYYY-MM-DD key into the [start, end) UTC window of
// that IST calendar day.
func istDayRange(date string) (start, end time.Time, err error) {
	ist := time.FixedZone("IST", 5*3600+1800)
	day, parseErr := time.ParseInLocation("2006-01-02", date, ist)
	if parseErr != nil {
		return time.Time{}, time.Time{}, httpx.BadRequest("VALIDATION", "date must be YYYY-MM-DD")
	}
	return day.UTC(), day.Add(24 * time.Hour).UTC(), nil
}
