package logistics

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"math"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/orgscope"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
)

const (
	// canCapacityLitres is the standard milk-can volume used to size a
	// consignment: can_count = ceil(total litres / 40).
	canCapacityLitres = 40.0

	// maxPourRefs caps how many pour references a consignment.created
	// provenance event carries; beyond it the pour count lives in the
	// payload only, keeping ledger events bounded.
	maxPourRefs = 300

	// maxTripStops bounds a planned route so trip documents stay small.
	maxTripStops = 100

	// Physical sanity bounds for a hand-held transit thermometer reading.
	minPlausibleTempC = -30.0
	maxPlausibleTempC = 70.0
)

// service holds all logistics business logic: consignment aggregation,
// the consignment/trip state machines, and provenance emission.
type service struct {
	repo   *repo
	orgs   *orgscope.Resolver
	ledger *provenance.Ledger
	log    *slog.Logger
}

// newService wires the service to its collaborators.
func newService(repo *repo, orgs *orgscope.Resolver, ledger *provenance.Ledger, log *slog.Logger) *service {
	return &service{repo: repo, orgs: orgs, ledger: ledger, log: log}
}

// actorID parses the actor's party ObjectID out of its JWT hex string.
func (s *service) actorID(actor auth.Actor) (primitive.ObjectID, error) {
	return httpx.ParseID(actor.PartyID, "actor")
}

// internal logs an unexpected failure with its operation context and maps it
// to a 500.
func (s *service) internal(ctx context.Context, op string, err error) *httpx.AppError {
	s.log.ErrorContext(ctx, op+" failed", slog.Any("err", err))
	return httpx.Internal(err)
}

// ---------------------------------------------------------------------------
// Consignments
// ---------------------------------------------------------------------------

// createConsignment pools one DCS date+shift's RECORDED pours into an OPEN
// consignment: pour IDs, total litres, quantity-weighted fat/SNF averages and
// the can count. The unique dcs+date+shift index makes creation idempotent-ish
// (a replay returns CONSIGNMENT_EXISTS).
func (s *service) createConsignment(ctx context.Context, actor auth.Actor, req createConsignmentRequest) (*domain.DCSConsignment, error) {
	if req.DCSID.IsZero() {
		return nil, httpx.BadRequest("MISSING_DCS_ID", "dcs_id is required")
	}
	if err := validateShift(req.Shift); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	date, err := resolveDate(req.Date, now)
	if err != nil {
		return nil, err
	}
	actorID, err := s.actorID(actor)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, req.DCSID); err != nil {
		s.log.WarnContext(ctx, "consignment creation denied: DCS out of scope",
			slog.String("dcs_id", req.DCSID.Hex()), slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}
	org, err := s.orgs.Get(ctx, req.DCSID)
	if err != nil {
		return nil, err
	}
	if org.Type != domain.OrgTypeDCS {
		return nil, httpx.BadRequest("NOT_A_DCS", "org unit "+req.DCSID.Hex()+" is not a DCS")
	}

	rows, err := s.repo.findRecordedPours(ctx, req.DCSID, date, req.Shift)
	if err != nil {
		return nil, s.internal(ctx, "create consignment: find recorded pours", err)
	}
	if len(rows) == 0 {
		s.log.WarnContext(ctx, "consignment creation rejected: no RECORDED pours",
			slog.String("dcs_id", req.DCSID.Hex()), slog.String("date", date),
			slog.String("shift", req.Shift), slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable("NO_POURS_TO_CONSIGN",
			fmt.Sprintf("no RECORDED pours for DCS %s on %s %s", req.DCSID.Hex(), date, req.Shift))
	}
	pourIDs, totalQty, avgFat, avgSNF, assurance := aggregatePours(rows)

	consignment := &domain.DCSConsignment{
		ID:                  primitive.NewObjectID(), // pre-generated: insert + ledger refs stay consistent
		DCSID:               req.DCSID,
		Date:                date,
		Shift:               req.Shift,
		PourIDs:             pourIDs,
		TotalQuantityLitres: totalQty,
		CanCount:            int(math.Ceil(totalQty / canCapacityLitres)),
		AvgFatPct:           avgFat,
		AvgSNFPct:           avgSNF,
		Assurance:           assurance,
		Status:              domain.ConsignmentStatusOpen,
		CreatedBy:           actorID,
		CreatedAt:           now,
	}
	if err := s.repo.insertConsignment(ctx, consignment); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			s.log.WarnContext(ctx, "consignment creation rejected: duplicate for dcs+date+shift",
				slog.String("dcs_id", req.DCSID.Hex()), slog.String("date", date),
				slog.String("shift", req.Shift), slog.String("actor_party_id", actor.PartyID))
			return nil, httpx.Conflict("CONSIGNMENT_EXISTS",
				fmt.Sprintf("a consignment for DCS %s on %s %s already exists", req.DCSID.Hex(), date, req.Shift))
		}
		return nil, s.internal(ctx, "create consignment: insert", err)
	}

	// Provenance: the pooling-boundary event. Pour refs are capped so the
	// event stays bounded — past the cap the payload's pour_count is the
	// authoritative tally (the doc's pour_ids keeps the full set).
	refs := make([]provenance.Ref, 0, min(len(pourIDs), maxPourRefs))
	for i, id := range pourIDs {
		if i >= maxPourRefs {
			break
		}
		refs = append(refs, provenance.Ref{EntityType: domain.EntityMilkPour, EntityID: id.Hex(), Relation: "aggregates"})
	}
	event, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventConsignmentCreated,
		EntityType: domain.EntityConsignment,
		EntityID:   consignment.ID.Hex(),
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  consignment.DCSID.Hex(),
		Payload: map[string]any{
			"pour_count":            len(pourIDs),
			"total_quantity_litres": totalQty,
			"can_count":             consignment.CanCount,
			"date":                  date,
			"shift":                 req.Shift,
		},
	})
	if err != nil {
		return nil, s.internal(ctx, "create consignment: append provenance", err)
	}
	if err := s.repo.setConsignmentProvenanceSeq(ctx, consignment.ID, event.Seq); err != nil {
		return nil, s.internal(ctx, "create consignment: set provenance seq", err)
	}
	consignment.ProvenanceSeq = event.Seq
	s.log.InfoContext(ctx, "consignment created",
		slog.String("consignment_id", consignment.ID.Hex()),
		slog.String("dcs_id", consignment.DCSID.Hex()),
		slog.String("date", date), slog.String("shift", req.Shift),
		slog.Int("pour_count", len(pourIDs)),
		slog.Float64("total_quantity_litres", totalQty),
		slog.String("actor_party_id", actor.PartyID))
	return consignment, nil
}

// dispatchConsignment seals an OPEN consignment for pickup (OPEN→DISPATCHED).
// The creation-time pour snapshot is provisional: the seal re-aggregates the
// shift's RECORDED pours so milk recorded between creation and dispatch (the
// collection module also rejects such pours, belt-and-braces) is reflected in
// pour_ids, litres, can count and weighted fat/SNF before the load leaves.
func (s *service) dispatchConsignment(ctx context.Context, actor auth.Actor, id primitive.ObjectID) (*domain.DCSConsignment, error) {
	consignment, err := s.loadConsignment(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, consignment.DCSID); err != nil {
		s.log.WarnContext(ctx, "consignment dispatch denied: DCS out of scope",
			slog.String("consignment_id", id.Hex()), slog.String("dcs_id", consignment.DCSID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}
	if consignment.Status != domain.ConsignmentStatusOpen {
		s.log.WarnContext(ctx, "consignment dispatch rejected: not OPEN",
			slog.String("consignment_id", id.Hex()), slog.String("status", consignment.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("INVALID_CONSIGNMENT_STATE",
			"consignment is "+consignment.Status+"; only OPEN consignments can be dispatched")
	}

	// Re-aggregate at the seal boundary.
	rows, err := s.repo.findRecordedPours(ctx, consignment.DCSID, consignment.Date, consignment.Shift)
	if err != nil {
		return nil, s.internal(ctx, "dispatch consignment: find recorded pours", err)
	}
	extraSet := bson.D{}
	if len(rows) > 0 {
		pourIDs, totalQty, avgFat, avgSNF, assurance := aggregatePours(rows)
		consignment.PourIDs = pourIDs
		consignment.TotalQuantityLitres = totalQty
		consignment.CanCount = int(math.Ceil(totalQty / canCapacityLitres))
		consignment.AvgFatPct = avgFat
		consignment.AvgSNFPct = avgSNF
		consignment.Assurance = assurance
		extraSet = bson.D{
			{Key: "pour_ids", Value: pourIDs},
			{Key: "total_quantity_litres", Value: totalQty},
			{Key: "can_count", Value: consignment.CanCount},
			{Key: "avg_fat_pct", Value: avgFat},
			{Key: "avg_snf_pct", Value: avgSNF},
			{Key: "assurance", Value: assurance},
		}
	}

	now := time.Now().UTC()
	// §6.4: sealing freezes the pour set to the physical cans. The seal_code is
	// an operational human-readable tamper marker (SEAL-<consignment>-<checksum>);
	// the append-only hash chain is the cryptographic integrity anchor.
	sealCode := mintSealCode(consignment.ID, consignment.PourIDs)
	consignment.SealCode = sealCode
	consignment.SealedAt = &now
	extraSet = append(extraSet,
		bson.E{Key: "seal_code", Value: sealCode},
		bson.E{Key: "sealed_at", Value: now},
	)
	matched, err := s.repo.markConsignmentDispatched(ctx, id, now, extraSet)
	if err != nil {
		return nil, s.internal(ctx, "dispatch consignment: mark dispatched", err)
	}
	if !matched { // lost a state race since the read above
		s.log.WarnContext(ctx, "consignment dispatch rejected: lost state race",
			slog.String("consignment_id", id.Hex()), slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("INVALID_CONSIGNMENT_STATE",
			"consignment was modified concurrently; reload and retry")
	}
	event, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventConsignmentDispatched,
		EntityType: domain.EntityConsignment,
		EntityID:   consignment.ID.Hex(),
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  consignment.DCSID.Hex(),
		Payload: map[string]any{
			"pour_count":            len(consignment.PourIDs),
			"total_quantity_litres": consignment.TotalQuantityLitres,
			"can_count":             consignment.CanCount,
			"date":                  consignment.Date,
			"shift":                 consignment.Shift,
		},
	})
	if err != nil {
		return nil, s.internal(ctx, "dispatch consignment: append provenance", err)
	}
	if err := s.repo.setConsignmentProvenanceSeq(ctx, consignment.ID, event.Seq); err != nil {
		return nil, s.internal(ctx, "dispatch consignment: set provenance seq", err)
	}
	consignment.Status = domain.ConsignmentStatusDispatch
	consignment.DispatchedAt = &now
	consignment.ProvenanceSeq = event.Seq
	s.log.InfoContext(ctx, "consignment dispatched",
		slog.String("consignment_id", consignment.ID.Hex()),
		slog.String("dcs_id", consignment.DCSID.Hex()),
		slog.Int("pour_count", len(consignment.PourIDs)),
		slog.Float64("total_quantity_litres", consignment.TotalQuantityLitres),
		slog.String("actor_party_id", actor.PartyID))
	return consignment, nil
}

// listConsignments returns a scoped page of consignments. DCS-scoped actors
// default to their own society; wide read roles (SUPER_ADMIN, STATE_AUDITOR)
// may list unfiltered; everyone else must name a dcs_id inside their scope.
func (s *service) listConsignments(ctx context.Context, actor auth.Actor, q consignmentListQuery, page httpx.Page) ([]domain.DCSConsignment, int64, error) {
	dcsID := q.DCSID
	if dcsID.IsZero() {
		switch {
		case actor.OrgType == domain.OrgTypeDCS && actor.OrgUnitID != "":
			id, err := httpx.ParseID(actor.OrgUnitID, "actor org unit")
			if err != nil {
				return nil, 0, err
			}
			dcsID = id
		case actor.RoleCode == domain.RoleSuperAdmin || actor.RoleCode == domain.RoleStateAuditor:
			// federation-wide read roles may scan unfiltered (still paged)
		default:
			return nil, 0, httpx.BadRequest("DCS_ID_REQUIRED", "dcs_id query parameter is required for your role")
		}
	}
	if !dcsID.IsZero() {
		if err := s.orgs.RequireInScope(ctx, actor, dcsID); err != nil {
			s.log.WarnContext(ctx, "consignment list denied: DCS out of scope",
				slog.String("dcs_id", dcsID.Hex()), slog.String("actor_party_id", actor.PartyID))
			return nil, 0, err
		}
	}
	if q.Date != "" {
		if _, err := resolveDate(q.Date, time.Time{}); err != nil {
			return nil, 0, err
		}
	}
	if q.Status != "" && !isConsignmentStatus(q.Status) {
		return nil, 0, httpx.BadRequest("INVALID_STATUS", "unknown consignment status "+q.Status)
	}
	items, total, err := s.repo.listConsignments(ctx, dcsID, q.Date, q.Status, page)
	if err != nil {
		return nil, 0, s.internal(ctx, "list consignments", err)
	}
	return items, total, nil
}

// getConsignment returns one consignment, enforcing the caller's org scope over
// its DCS — the single-item read backing the FE getLot call.
func (s *service) getConsignment(ctx context.Context, actor auth.Actor, id primitive.ObjectID) (*domain.DCSConsignment, error) {
	consignment, err := s.loadConsignment(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, consignment.DCSID); err != nil {
		s.log.WarnContext(ctx, "consignment read denied: DCS out of scope",
			slog.String("consignment_id", id.Hex()), slog.String("dcs_id", consignment.DCSID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}
	return consignment, nil
}

// approveForUnion generates the DCS→Union B2B invoice for a consignment: it
// values the pooled milk (Σ pour amounts, GST-exempt HSN 0401), mints the
// invoice number and stamps the union-approval fields. Idempotent — a second
// call returns the already-generated invoice. The caller must be in the DCS's
// org scope; the consignment must have left OPEN (be sealed/onward).
func (s *service) approveForUnion(ctx context.Context, actor auth.Actor, id primitive.ObjectID) (*consignmentInvoice, error) {
	consignment, err := s.loadConsignment(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, consignment.DCSID); err != nil {
		return nil, err
	}
	if consignment.Status == domain.ConsignmentStatusOpen {
		return nil, httpx.Conflict("CONSIGNMENT_NOT_SEALED",
			"only a dispatched (sealed) consignment can be invoiced to the union")
	}
	if consignment.UnionApproved {
		return s.buildConsignmentInvoice(ctx, consignment) // idempotent
	}
	actorID, err := s.actorID(actor)
	if err != nil {
		return nil, err
	}
	amount, _, err := s.repo.consignmentBilling(ctx, consignment.PourIDs)
	if err != nil {
		return nil, s.internal(ctx, "approve for union: billing", err)
	}
	now := time.Now().UTC()
	invoiceNo := s.mintConsignmentInvoiceNo(ctx, consignment)
	consignment.UnionApproved = true
	consignment.UnionApprovedAt = &now
	consignment.UnionApprovedByID = &actorID
	consignment.UnionInvoiceNo = invoiceNo
	consignment.UnionInvoiceAmount = round2(amount)
	if err := s.repo.markConsignmentUnionApproved(ctx, id, bson.D{
		{Key: "union_approved", Value: true},
		{Key: "union_approved_at", Value: now},
		{Key: "union_approved_by_id", Value: actorID},
		{Key: "union_invoice_no", Value: invoiceNo},
		{Key: "union_invoice_amount", Value: round2(amount)},
	}); err != nil {
		return nil, s.internal(ctx, "approve for union: stamp", err)
	}
	s.log.InfoContext(ctx, "consignment approved for union invoice",
		slog.String("consignment_id", id.Hex()),
		slog.String("invoice_no", invoiceNo),
		slog.Float64("amount", round2(amount)),
		slog.String("actor_party_id", actor.PartyID))
	return s.buildConsignmentInvoice(ctx, consignment)
}

// getConsignmentInvoice returns the B2B invoice for an already-approved
// consignment (404 until it is approved for the union).
func (s *service) getConsignmentInvoice(ctx context.Context, actor auth.Actor, id primitive.ObjectID) (*consignmentInvoice, error) {
	consignment, err := s.loadConsignment(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, consignment.DCSID); err != nil {
		return nil, err
	}
	if !consignment.UnionApproved {
		return nil, httpx.NotFound("consignment invoice (not yet approved for union)")
	}
	return s.buildConsignmentInvoice(ctx, consignment)
}

// buildConsignmentInvoice assembles the invoice view from a consignment,
// resolving the seller DCS code and the buyer union (its parent). Milk is
// GST-exempt so tax is zero and total equals the taxable milk value.
func (s *service) buildConsignmentInvoice(ctx context.Context, c *domain.DCSConsignment) (*consignmentInvoice, error) {
	inv := &consignmentInvoice{
		InvoiceNo:       c.UnionInvoiceNo,
		ConsignmentID:   c.ID,
		ConsignmentCode: consignmentCode("", c.Date, c.Shift, c.ID),
		FromDCSID:       c.DCSID,
		Date:            c.Date,
		Shift:           c.Shift,
		HSNCode:         "0401",
		GSTNote:         "Fresh milk is GST exempt (HSN 0401)",
		TotalLitres:     c.TotalQuantityLitres,
		AvgFatPct:       c.AvgFatPct,
		AvgSNFPct:       c.AvgSNFPct,
		TaxableAmount:   c.UnionInvoiceAmount,
		TaxAmount:       0,
		TotalAmount:     c.UnionInvoiceAmount,
		GeneratedByID:   c.UnionApprovedByID,
		GeneratedAt:     c.UnionApprovedAt,
	}
	// Farmer count from the pour set (best-effort; never fails the invoice).
	if _, farmers, err := s.repo.consignmentBilling(ctx, c.PourIDs); err == nil {
		inv.FarmerCount = farmers
	}
	// Enrich seller code + buyer union from the org directory (best-effort).
	if org, err := s.orgs.Get(ctx, c.DCSID); err == nil && org != nil {
		inv.ConsignmentCode = consignmentCode(org.Code, c.Date, c.Shift, c.ID)
		inv.ToUnionID = org.ParentID
	}
	return inv, nil
}

// mintConsignmentInvoiceNo builds the B2B invoice number
// "<dcsCode>/<FY>/<idSuffix>" — the id suffix keeps it unique without a shared
// counter. dcsCode falls back to a short id when the org lookup fails.
func (s *service) mintConsignmentInvoiceNo(ctx context.Context, c *domain.DCSConsignment) string {
	dcsCode := c.DCSID.Hex()[18:24]
	if org, err := s.orgs.Get(ctx, c.DCSID); err == nil && org != nil && org.Code != "" {
		dcsCode = org.Code
	}
	return fmt.Sprintf("%s/%s/%s", dcsCode, fiscalYear(c.Date), c.ID.Hex()[18:24])
}

// consignmentCode builds the Developer-Note Appendix-A style consignment code
// CON-<societyCode>-<YYYYMMDD>-<AM|PM>, falling back to an id suffix for the
// society segment when the code is unknown.
func consignmentCode(societyCode, date, shift string, id primitive.ObjectID) string {
	seg := societyCode
	if seg == "" {
		seg = id.Hex()[18:24]
	}
	ap := "AM"
	if shift == domain.ShiftEvening {
		ap = "PM"
	}
	return "CON-" + seg + "-" + strings.ReplaceAll(date, "-", "") + "-" + ap
}

// fiscalYear returns the Indian financial year "YYYY-YY" for a YYYY-MM-DD date
// (April–March). A malformed date falls back to the calendar year.
func fiscalYear(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		if len(date) >= 4 {
			return date[:4]
		}
		return "0000"
	}
	y := t.Year()
	if int(t.Month()) < 4 { // Jan-Mar belongs to the FY that started the previous April
		y--
	}
	return fmt.Sprintf("%d-%02d", y, (y+1)%100)
}

// ---------------------------------------------------------------------------
// Route trips
// ---------------------------------------------------------------------------

// createTrip plans a van run: every stop's consignment must already be
// DISPATCHED and must belong to that stop's DCS. The rider defaults to the
// actor when a VAN_RIDER plans their own route.
func (s *service) createTrip(ctx context.Context, actor auth.Actor, req createTripRequest) (*domain.RouteTrip, error) {
	if req.RouteName == "" {
		return nil, httpx.BadRequest("MISSING_ROUTE_NAME", "route_name is required")
	}
	if req.UnionID.IsZero() {
		return nil, httpx.BadRequest("MISSING_UNION_ID", "union_id is required")
	}
	if err := validateShift(req.Shift); err != nil {
		return nil, err
	}
	if len(req.Stops) == 0 {
		return nil, httpx.BadRequest("NO_STOPS", "at least one stop is required")
	}
	if len(req.Stops) > maxTripStops {
		return nil, httpx.BadRequest("TOO_MANY_STOPS", fmt.Sprintf("a trip may have at most %d stops", maxTripStops))
	}
	now := time.Now().UTC()
	date, err := resolveDate(req.Date, now)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, req.UnionID); err != nil {
		s.log.WarnContext(ctx, "trip creation denied: union out of scope",
			slog.String("union_id", req.UnionID.Hex()), slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}
	union, err := s.orgs.Get(ctx, req.UnionID)
	if err != nil {
		return nil, err
	}
	if union.Type != domain.OrgTypeMilkUnion {
		return nil, httpx.BadRequest("NOT_A_UNION", "org unit "+req.UnionID.Hex()+" is not a MILK_UNION")
	}

	var riderPartyID primitive.ObjectID
	if req.VanRiderPartyID != nil && !req.VanRiderPartyID.IsZero() {
		riderPartyID = *req.VanRiderPartyID
	} else {
		if actor.RoleCode != domain.RoleVanRider {
			return nil, httpx.BadRequest("RIDER_REQUIRED", "van_rider_party_id is required unless a VAN_RIDER plans their own trip")
		}
		actorID, err := s.actorID(actor)
		if err != nil {
			return nil, err
		}
		riderPartyID = actorID
	}

	seen := make(map[primitive.ObjectID]struct{}, len(req.Stops))
	stops := make([]domain.RouteStop, 0, len(req.Stops))
	for _, stop := range req.Stops {
		if stop.DCSID.IsZero() || stop.ConsignmentID.IsZero() {
			return nil, httpx.BadRequest("INVALID_STOP", "every stop needs dcs_id and consignment_id")
		}
		if _, dup := seen[stop.ConsignmentID]; dup {
			return nil, httpx.BadRequest("DUPLICATE_STOP", "consignment "+stop.ConsignmentID.Hex()+" appears in more than one stop")
		}
		seen[stop.ConsignmentID] = struct{}{}

		// Scope guard (IDOR): the stop's DCS must be within the actor's org
		// scope, so one union's rider/supervisor cannot pull another union's
		// consignment onto their trip (cross-union pooling / foreign mutation).
		if err := s.orgs.RequireInScope(ctx, actor, stop.DCSID); err != nil {
			s.log.WarnContext(ctx, "trip creation rejected: stop DCS out of scope",
				slog.String("stop_dcs_id", stop.DCSID.Hex()), slog.String("actor_party_id", actor.PartyID))
			return nil, err
		}
		consignment, err := s.loadConsignment(ctx, stop.ConsignmentID)
		if err != nil {
			return nil, err
		}
		if aerr := validateStopConsignment(consignment, stop.DCSID); aerr != nil {
			s.log.WarnContext(ctx, "trip creation rejected: invalid stop consignment",
				slog.String("consignment_id", stop.ConsignmentID.Hex()),
				slog.String("stop_dcs_id", stop.DCSID.Hex()),
				slog.String("consignment_status", consignment.Status),
				slog.String("code", aerr.Code),
				slog.String("actor_party_id", actor.PartyID))
			return nil, aerr
		}
		stops = append(stops, domain.RouteStop{DCSID: stop.DCSID, ConsignmentID: stop.ConsignmentID})
	}

	trip := &domain.RouteTrip{
		ID:              primitive.NewObjectID(), // pre-generated so provenance can reference it later
		RouteName:       req.RouteName,
		UnionID:         req.UnionID,
		VanRiderPartyID: riderPartyID,
		Date:            date,
		Shift:           req.Shift,
		Stops:           stops,
		Status:          domain.TripStatusPlanned,
		CreatedAt:       now,
	}
	if err := s.repo.insertTrip(ctx, trip); err != nil {
		return nil, s.internal(ctx, "create trip: insert", err)
	}
	s.log.InfoContext(ctx, "trip created",
		slog.String("trip_id", trip.ID.Hex()),
		slog.String("union_id", trip.UnionID.Hex()),
		slog.String("van_rider_party_id", trip.VanRiderPartyID.Hex()),
		slog.String("route_name", trip.RouteName),
		slog.String("date", date), slog.String("shift", req.Shift),
		slog.Int("stop_count", len(stops)),
		slog.String("actor_party_id", actor.PartyID))
	return trip, nil
}

// pickupStop records the trip's rider collecting one consignment: the stop is
// stamped with time+temperature, the consignment moves DISPATCHED→PICKED_UP,
// and the trip moves to IN_PROGRESS on its first pickup.
func (s *service) pickupStop(ctx context.Context, actor auth.Actor, tripID, consignmentID primitive.ObjectID, req pickupRequest) (*domain.RouteTrip, error) {
	if err := validateTemp(req.TempC); err != nil {
		return nil, err
	}
	actorID, err := s.actorID(actor)
	if err != nil {
		return nil, err
	}
	trip, err := s.loadTrip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if aerr := validatePickup(trip, actorID, consignmentID); aerr != nil {
		s.log.WarnContext(ctx, "stop pickup rejected",
			slog.String("trip_id", tripID.Hex()), slog.String("consignment_id", consignmentID.Hex()),
			slog.String("code", aerr.Code), slog.String("actor_party_id", actor.PartyID))
		return nil, aerr
	}

	// Guard the consignment transition atomically; only a DISPATCHED
	// consignment can board the van.
	matched, err := s.repo.markConsignmentPickedUp(ctx, consignmentID, trip.ID)
	if err != nil {
		return nil, s.internal(ctx, "pickup stop: mark consignment picked up", err)
	}
	if !matched {
		consignment, err := s.loadConsignment(ctx, consignmentID)
		if err != nil {
			return nil, err
		}
		s.log.WarnContext(ctx, "stop pickup rejected: consignment not DISPATCHED",
			slog.String("trip_id", tripID.Hex()), slog.String("consignment_id", consignmentID.Hex()),
			slog.String("consignment_status", consignment.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("CONSIGNMENT_NOT_DISPATCHED",
			"consignment is "+consignment.Status+"; only DISPATCHED consignments can be picked up")
	}

	now := time.Now().UTC()
	if err := s.repo.markStopPickedUp(ctx, trip.ID, consignmentID, now, *req.TempC, req.Notes); err != nil {
		return nil, s.internal(ctx, "pickup stop: mark stop picked up", err)
	}

	event, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventConsignmentPickedUp,
		EntityType: domain.EntityConsignment,
		EntityID:   consignmentID.Hex(),
		Refs: []provenance.Ref{
			{EntityType: domain.EntityRouteTrip, EntityID: trip.ID.Hex(), Relation: "carried_by"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: trip.UnionID.Hex(),
		Payload:   map[string]any{"temp_c": *req.TempC},
	})
	if err != nil {
		return nil, s.internal(ctx, "pickup stop: append provenance", err)
	}
	if err := s.repo.setConsignmentProvenanceSeq(ctx, consignmentID, event.Seq); err != nil {
		return nil, s.internal(ctx, "pickup stop: set provenance seq", err)
	}

	trip.Status = domain.TripStatusInProgress
	if i := findStop(trip, consignmentID); i >= 0 {
		trip.Stops[i].PickedUpAt = &now
		trip.Stops[i].TempC = *req.TempC
		trip.Stops[i].Notes = req.Notes
	}
	s.log.InfoContext(ctx, "stop picked up",
		slog.String("trip_id", trip.ID.Hex()),
		slog.String("consignment_id", consignmentID.Hex()),
		slog.Float64("temp_c", *req.TempC),
		slog.String("actor_party_id", actor.PartyID))
	return trip, nil
}

// logColdChain appends one in-transit temperature sample to the trip —
// tamper-evidence for the perishable load between village and BMC.
func (s *service) logColdChain(ctx context.Context, actor auth.Actor, tripID primitive.ObjectID, req coldChainRequest) (*domain.RouteTrip, error) {
	if err := validateTemp(req.TempC); err != nil {
		return nil, err
	}
	actorID, err := s.actorID(actor)
	if err != nil {
		return nil, err
	}
	trip, err := s.loadTrip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if aerr := validateColdChain(trip, actorID); aerr != nil {
		s.log.WarnContext(ctx, "cold-chain log rejected",
			slog.String("trip_id", tripID.Hex()), slog.String("code", aerr.Code),
			slog.String("actor_party_id", actor.PartyID))
		return nil, aerr
	}
	entry := domain.ColdChainEntry{
		TS:     time.Now().UTC(),
		TempC:  *req.TempC,
		GeoLat: req.GeoLat,
		GeoLng: req.GeoLng,
	}
	if err := s.repo.pushColdChain(ctx, trip.ID, entry); err != nil {
		return nil, s.internal(ctx, "log cold chain: push entry", err)
	}
	event, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventColdChainLogged,
		EntityType: domain.EntityRouteTrip,
		EntityID:   trip.ID.Hex(),
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  trip.UnionID.Hex(),
		Payload:    map[string]any{"temp_c": *req.TempC},
	})
	if err != nil {
		return nil, s.internal(ctx, "log cold chain: append provenance", err)
	}
	if err := s.repo.setTripProvenanceSeq(ctx, trip.ID, event.Seq); err != nil {
		return nil, s.internal(ctx, "log cold chain: set provenance seq", err)
	}
	trip.ColdChain = append(trip.ColdChain, entry)
	trip.ProvenanceSeq = event.Seq
	s.log.InfoContext(ctx, "cold chain logged",
		slog.String("trip_id", trip.ID.Hex()),
		slog.Float64("temp_c", *req.TempC),
		slog.String("actor_party_id", actor.PartyID))
	return trip, nil
}

// deliverTrip hands the whole load to a BMC: every stop must be picked up,
// the trip moves IN_PROGRESS→DELIVERED and its consignments PICKED_UP→DELIVERED.
func (s *service) deliverTrip(ctx context.Context, actor auth.Actor, tripID primitive.ObjectID, req deliverRequest) (*domain.RouteTrip, error) {
	if req.BMCID.IsZero() {
		return nil, httpx.BadRequest("MISSING_BMC_ID", "bmc_id is required")
	}
	actorID, err := s.actorID(actor)
	if err != nil {
		return nil, err
	}
	trip, err := s.loadTrip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if aerr := validateDeliver(trip, actorID); aerr != nil {
		s.log.WarnContext(ctx, "trip delivery rejected",
			slog.String("trip_id", tripID.Hex()), slog.String("code", aerr.Code),
			slog.String("actor_party_id", actor.PartyID))
		return nil, aerr
	}
	bmc, err := s.orgs.Get(ctx, req.BMCID)
	if err != nil {
		return nil, err
	}
	if bmc.Type != domain.OrgTypeBMC {
		return nil, httpx.BadRequest("NOT_A_BMC", "org unit "+req.BMCID.Hex()+" is not a BMC")
	}
	// Scope guard: the destination BMC must be within the actor's org scope so
	// a trip cannot be delivered into an unrelated union's chilling centre.
	if err := s.orgs.RequireInScope(ctx, actor, req.BMCID); err != nil {
		s.log.WarnContext(ctx, "trip delivery rejected: BMC out of scope",
			slog.String("bmc_id", req.BMCID.Hex()), slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}

	now := time.Now().UTC()
	matched, err := s.repo.markTripDelivered(ctx, trip.ID, req.BMCID, now)
	if err != nil {
		return nil, s.internal(ctx, "deliver trip: mark delivered", err)
	}
	if !matched { // lost a state race since the read above
		s.log.WarnContext(ctx, "trip delivery rejected: lost state race",
			slog.String("trip_id", tripID.Hex()), slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("INVALID_TRIP_STATE", "trip was modified concurrently; reload and retry")
	}

	consignmentIDs := make([]primitive.ObjectID, 0, len(trip.Stops))
	refs := make([]provenance.Ref, 0, len(trip.Stops))
	for _, stop := range trip.Stops {
		consignmentIDs = append(consignmentIDs, stop.ConsignmentID)
		refs = append(refs, provenance.Ref{
			EntityType: domain.EntityConsignment, EntityID: stop.ConsignmentID.Hex(), Relation: "delivered",
		})
	}
	if err := s.repo.markConsignmentsDelivered(ctx, consignmentIDs); err != nil {
		return nil, s.internal(ctx, "deliver trip: mark consignments delivered", err)
	}

	event, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventTripDelivered,
		EntityType: domain.EntityRouteTrip,
		EntityID:   trip.ID.Hex(),
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  trip.UnionID.Hex(),
		Payload:    map[string]any{"bmc_id": req.BMCID.Hex()},
	})
	if err != nil {
		return nil, s.internal(ctx, "deliver trip: append provenance", err)
	}
	if err := s.repo.setTripProvenanceSeq(ctx, trip.ID, event.Seq); err != nil {
		return nil, s.internal(ctx, "deliver trip: set provenance seq", err)
	}

	trip.Status = domain.TripStatusDelivered
	trip.DeliveredToBMCID = &req.BMCID
	trip.DeliveredAt = &now
	trip.ProvenanceSeq = event.Seq
	s.log.InfoContext(ctx, "trip delivered",
		slog.String("trip_id", trip.ID.Hex()),
		slog.String("bmc_id", req.BMCID.Hex()),
		slog.Int("consignment_count", len(consignmentIDs)),
		slog.String("actor_party_id", actor.PartyID))
	return trip, nil
}

// listTrips returns a scoped page of trips: riders are forced onto their own
// trips; supervisors/presidents default to (and are checked against) their
// union scope.
func (s *service) listTrips(ctx context.Context, actor auth.Actor, q tripListQuery, page httpx.Page) ([]domain.RouteTrip, int64, error) {
	unionID := q.UnionID
	riderPartyID := q.VanRiderPartyID
	if actor.RoleCode == domain.RoleVanRider {
		actorID, err := s.actorID(actor)
		if err != nil {
			return nil, 0, err
		}
		riderPartyID = actorID // riders only ever see their own trips
	} else {
		if unionID.IsZero() &&
			actor.RoleCode != domain.RoleSuperAdmin && actor.RoleCode != domain.RoleStateAuditor &&
			actor.OrgUnitID != "" {
			id, err := httpx.ParseID(actor.OrgUnitID, "actor org unit")
			if err != nil {
				return nil, 0, err
			}
			unionID = id // supervisors/presidents default to their union
		}
		if !unionID.IsZero() {
			if err := s.orgs.RequireInScope(ctx, actor, unionID); err != nil {
				s.log.WarnContext(ctx, "trip list denied: union out of scope",
					slog.String("union_id", unionID.Hex()), slog.String("actor_party_id", actor.PartyID))
				return nil, 0, err
			}
		}
	}
	if q.Date != "" {
		if _, err := resolveDate(q.Date, time.Time{}); err != nil {
			return nil, 0, err
		}
	}
	items, total, err := s.repo.listTrips(ctx, unionID, q.Date, riderPartyID, page)
	if err != nil {
		return nil, 0, s.internal(ctx, "list trips", err)
	}
	return items, total, nil
}

// getTrip returns one trip: a rider only their own, everyone else within
// union scope.
func (s *service) getTrip(ctx context.Context, actor auth.Actor, tripID primitive.ObjectID) (*domain.RouteTrip, error) {
	trip, err := s.loadTrip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if actor.RoleCode == domain.RoleVanRider {
		actorID, err := s.actorID(actor)
		if err != nil {
			return nil, err
		}
		if trip.VanRiderPartyID != actorID {
			s.log.WarnContext(ctx, "trip read denied: assigned to another rider",
				slog.String("trip_id", tripID.Hex()), slog.String("actor_party_id", actor.PartyID))
			return nil, httpx.Forbidden("this trip is assigned to another van rider")
		}
		return trip, nil
	}
	if err := s.orgs.RequireInScope(ctx, actor, trip.UnionID); err != nil {
		s.log.WarnContext(ctx, "trip read denied: union out of scope",
			slog.String("trip_id", tripID.Hex()), slog.String("union_id", trip.UnionID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}
	return trip, nil
}

// ---------------------------------------------------------------------------
// Loaders (mongo.ErrNoDocuments → 404)
// ---------------------------------------------------------------------------

// loadConsignment fetches a consignment, mapping a miss to 404.
func (s *service) loadConsignment(ctx context.Context, id primitive.ObjectID) (*domain.DCSConsignment, error) {
	c, err := s.repo.findConsignmentByID(ctx, id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("consignment " + id.Hex())
	}
	if err != nil {
		return nil, s.internal(ctx, "load consignment", err)
	}
	return c, nil
}

// loadTrip fetches a trip, mapping a miss to 404.
func (s *service) loadTrip(ctx context.Context, id primitive.ObjectID) (*domain.RouteTrip, error) {
	t, err := s.repo.findTripByID(ctx, id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("trip " + id.Hex())
	}
	if err != nil {
		return nil, s.internal(ctx, "load trip", err)
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// Pure guards & helpers (unit-tested in service_test.go)
// ---------------------------------------------------------------------------

// aggregatePours reduces pour rows to (ids, total litres, quantity-weighted
// average fat %, quantity-weighted average SNF %), rounded to 2 decimals.
func aggregatePours(rows []pourRow) (pourIDs []primitive.ObjectID, totalQty, avgFat, avgSNF float64, assurance string) {
	var fatWeighted, snfWeighted float64
	levels := make([]string, 0, len(rows))
	pourIDs = make([]primitive.ObjectID, 0, len(rows))
	for _, row := range rows {
		pourIDs = append(pourIDs, row.ID)
		totalQty += row.QuantityLitres
		fatWeighted += row.FatPct * row.QuantityLitres
		snfWeighted += row.SNFPct * row.QuantityLitres
		levels = append(levels, row.Assurance)
	}
	if totalQty > 0 {
		avgFat = round2(fatWeighted / totalQty)
		avgSNF = round2(snfWeighted / totalQty)
	}
	// §6.2: the consignment inherits the WEAKEST assurance of its pours.
	return pourIDs, round2(totalQty), avgFat, avgSNF, domain.WeakestAssurance(levels...)
}

// round2 rounds to 2 decimal places (litres / percentage precision).
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// mintSealCode builds the §6.4 / Appendix-A seal code SEAL-<consignment>-<checksum>,
// where the checksum is a CRC32 over the frozen pour set so a later silent
// change to the pour list produces a different code.
func mintSealCode(consignmentID primitive.ObjectID, pourIDs []primitive.ObjectID) string {
	h := crc32.NewIEEE()
	h.Write(consignmentID[:])
	for _, id := range pourIDs {
		h.Write(id[:])
	}
	return fmt.Sprintf("SEAL-%s-%04X", consignmentID.Hex()[18:24], h.Sum32()&0xFFFF)
}

// validateShift accepts only the two collection shifts.
func validateShift(shift string) *httpx.AppError {
	if shift != domain.ShiftMorning && shift != domain.ShiftEvening {
		return httpx.BadRequest("INVALID_SHIFT", "shift must be MORNING or EVENING")
	}
	return nil
}

// resolveDate defaults an empty date to the IST day key of now, otherwise
// validates the YYYY-MM-DD form.
func resolveDate(date string, now time.Time) (string, error) {
	if date == "" {
		return domain.DateKeyIST(now), nil
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", httpx.BadRequest("INVALID_DATE", "date must be YYYY-MM-DD")
	}
	return date, nil
}

// validateTemp requires a temperature and rejects physically implausible ones.
func validateTemp(tempC *float64) *httpx.AppError {
	if tempC == nil {
		return httpx.BadRequest("MISSING_TEMP", "temp_c is required")
	}
	if *tempC < minPlausibleTempC || *tempC > maxPlausibleTempC {
		return httpx.BadRequest("IMPLAUSIBLE_TEMP",
			fmt.Sprintf("temp_c must be between %.0f and %.0f °C", minPlausibleTempC, maxPlausibleTempC))
	}
	return nil
}

// isConsignmentStatus reports whether s is a known consignment status.
func isConsignmentStatus(s string) bool {
	switch s {
	case domain.ConsignmentStatusOpen, domain.ConsignmentStatusDispatch,
		domain.ConsignmentStatusPickedUp, domain.ConsignmentStatusDelivered,
		domain.ConsignmentStatusAccepted, domain.ConsignmentStatusRejected:
		return true
	}
	return false
}

// findStop returns the index of the stop carrying consignmentID, or -1.
func findStop(trip *domain.RouteTrip, consignmentID primitive.ObjectID) int {
	for i := range trip.Stops {
		if trip.Stops[i].ConsignmentID == consignmentID {
			return i
		}
	}
	return -1
}

// requireTripRider enforces that only the trip's assigned van rider acts on it.
func requireTripRider(trip *domain.RouteTrip, riderPartyID primitive.ObjectID) *httpx.AppError {
	if trip.VanRiderPartyID != riderPartyID {
		return httpx.Forbidden("this trip is assigned to another van rider")
	}
	return nil
}

// validateStopConsignment guards trip planning: the stop's consignment must
// be DISPATCHED (state machine) and must belong to the stop's DCS.
func validateStopConsignment(consignment *domain.DCSConsignment, stopDCSID primitive.ObjectID) *httpx.AppError {
	if consignment.Status != domain.ConsignmentStatusDispatch {
		return httpx.Conflict("CONSIGNMENT_NOT_DISPATCHED",
			"consignment "+consignment.ID.Hex()+" is "+consignment.Status+"; only DISPATCHED consignments can be planned onto a trip")
	}
	if consignment.DCSID != stopDCSID {
		return httpx.Unprocessable("CONSIGNMENT_DCS_MISMATCH",
			"consignment "+consignment.ID.Hex()+" belongs to DCS "+consignment.DCSID.Hex()+", not the stop's DCS "+stopDCSID.Hex())
	}
	return nil
}

// validatePickup guards the pickup transition: right rider, trip not yet
// delivered, stop exists, stop not already picked.
func validatePickup(trip *domain.RouteTrip, riderPartyID, consignmentID primitive.ObjectID) *httpx.AppError {
	if err := requireTripRider(trip, riderPartyID); err != nil {
		return err
	}
	if trip.Status == domain.TripStatusDelivered {
		return httpx.Conflict("TRIP_ALREADY_DELIVERED", "trip is DELIVERED; no further pickups are possible")
	}
	i := findStop(trip, consignmentID)
	if i < 0 {
		return httpx.NotFound("stop for consignment " + consignmentID.Hex())
	}
	if trip.Stops[i].PickedUpAt != nil {
		return httpx.Conflict("STOP_ALREADY_PICKED", "consignment "+consignmentID.Hex()+" was already picked up on this trip")
	}
	return nil
}

// validateColdChain guards cold-chain logging: right rider, trip still moving.
func validateColdChain(trip *domain.RouteTrip, riderPartyID primitive.ObjectID) *httpx.AppError {
	if err := requireTripRider(trip, riderPartyID); err != nil {
		return err
	}
	if trip.Status == domain.TripStatusDelivered {
		return httpx.Conflict("TRIP_ALREADY_DELIVERED", "trip is DELIVERED; cold-chain logging is closed")
	}
	return nil
}

// validateDeliver guards delivery: right rider, not already delivered, and
// every stop picked up (partial loads cannot be handed to a BMC).
func validateDeliver(trip *domain.RouteTrip, riderPartyID primitive.ObjectID) *httpx.AppError {
	if err := requireTripRider(trip, riderPartyID); err != nil {
		return err
	}
	if trip.Status == domain.TripStatusDelivered {
		return httpx.Conflict("TRIP_ALREADY_DELIVERED", "trip is already DELIVERED")
	}
	var unpicked []string
	for _, stop := range trip.Stops {
		if stop.PickedUpAt == nil {
			unpicked = append(unpicked, stop.ConsignmentID.Hex())
		}
	}
	if len(unpicked) > 0 {
		return httpx.Unprocessable("STOPS_NOT_PICKED", "every stop must be picked up before delivery").
			WithDetails(map[string]any{"unpicked_consignment_ids": unpicked})
	}
	return nil
}
