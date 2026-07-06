package logistics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
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
}

// newService wires the service to its collaborators.
func newService(repo *repo, orgs *orgscope.Resolver, ledger *provenance.Ledger) *service {
	return &service{repo: repo, orgs: orgs, ledger: ledger}
}

// ---------------------------------------------------------------------------
// Consignments
// ---------------------------------------------------------------------------

// createConsignment pools one DCS date+shift's RECORDED pours into an OPEN
// consignment: pour IDs, total litres, quantity-weighted fat/SNF averages and
// the can count. The unique dcs+date+shift index makes creation idempotent-ish
// (a replay returns CONSIGNMENT_EXISTS).
func (s *service) createConsignment(ctx context.Context, actor auth.Actor, req createConsignmentRequest) (*domain.DCSConsignment, error) {
	if req.DCSID == "" {
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
	if err := s.orgs.RequireInScope(ctx, actor, req.DCSID); err != nil {
		return nil, err
	}
	org, err := s.orgs.Get(ctx, req.DCSID)
	if err != nil {
		return nil, err
	}
	if org.Type != domain.OrgTypeDCS {
		return nil, httpx.BadRequest("NOT_A_DCS", "org unit "+req.DCSID+" is not a DCS")
	}

	rows, err := s.repo.findRecordedPours(ctx, req.DCSID, date, req.Shift)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if len(rows) == 0 {
		return nil, httpx.Unprocessable("NO_POURS_TO_CONSIGN",
			fmt.Sprintf("no RECORDED pours for DCS %s on %s %s", req.DCSID, date, req.Shift))
	}
	pourIDs, totalQty, avgFat, avgSNF := aggregatePours(rows)

	consignment := &domain.DCSConsignment{
		ID:                  uuid.NewString(),
		DCSID:               req.DCSID,
		Date:                date,
		Shift:               req.Shift,
		PourIDs:             pourIDs,
		TotalQuantityLitres: totalQty,
		CanCount:            int(math.Ceil(totalQty / canCapacityLitres)),
		AvgFatPct:           avgFat,
		AvgSNFPct:           avgSNF,
		Status:              domain.ConsignmentStatusOpen,
		CreatedBy:           actor.PartyID,
		CreatedAt:           now,
	}
	if err := s.repo.insertConsignment(ctx, consignment); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, httpx.Conflict("CONSIGNMENT_EXISTS",
				fmt.Sprintf("a consignment for DCS %s on %s %s already exists", req.DCSID, date, req.Shift))
		}
		return nil, httpx.Internal(err)
	}

	// Provenance: the pooling-boundary event. Pour refs are capped so the
	// event stays bounded — past the cap the payload's pour_count is the
	// authoritative tally (the doc's pour_ids keeps the full set).
	refs := make([]provenance.Ref, 0, min(len(pourIDs), maxPourRefs))
	for i, id := range pourIDs {
		if i >= maxPourRefs {
			break
		}
		refs = append(refs, provenance.Ref{EntityType: domain.EntityMilkPour, EntityID: id, Relation: "aggregates"})
	}
	event, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventConsignmentCreated,
		EntityType: domain.EntityConsignment,
		EntityID:   consignment.ID,
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  consignment.DCSID,
		Payload: map[string]any{
			"pour_count":            len(pourIDs),
			"total_quantity_litres": totalQty,
			"can_count":             consignment.CanCount,
			"date":                  date,
			"shift":                 req.Shift,
		},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.setConsignmentProvenanceSeq(ctx, consignment.ID, event.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	consignment.ProvenanceSeq = event.Seq
	return consignment, nil
}

// dispatchConsignment seals an OPEN consignment for pickup (OPEN→DISPATCHED).
// The creation-time pour snapshot is provisional: the seal re-aggregates the
// shift's RECORDED pours so milk recorded between creation and dispatch (the
// collection module also rejects such pours, belt-and-braces) is reflected in
// pour_ids, litres, can count and weighted fat/SNF before the load leaves.
func (s *service) dispatchConsignment(ctx context.Context, actor auth.Actor, id string) (*domain.DCSConsignment, error) {
	consignment, err := s.loadConsignment(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, consignment.DCSID); err != nil {
		return nil, err
	}
	if consignment.Status != domain.ConsignmentStatusOpen {
		return nil, httpx.Conflict("INVALID_CONSIGNMENT_STATE",
			"consignment is "+consignment.Status+"; only OPEN consignments can be dispatched")
	}

	// Re-aggregate at the seal boundary.
	rows, err := s.repo.findRecordedPours(ctx, consignment.DCSID, consignment.Date, consignment.Shift)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	extraSet := bson.D{}
	if len(rows) > 0 {
		pourIDs, totalQty, avgFat, avgSNF := aggregatePours(rows)
		consignment.PourIDs = pourIDs
		consignment.TotalQuantityLitres = totalQty
		consignment.CanCount = int(math.Ceil(totalQty / canCapacityLitres))
		consignment.AvgFatPct = avgFat
		consignment.AvgSNFPct = avgSNF
		extraSet = bson.D{
			{Key: "pour_ids", Value: pourIDs},
			{Key: "total_quantity_litres", Value: totalQty},
			{Key: "can_count", Value: consignment.CanCount},
			{Key: "avg_fat_pct", Value: avgFat},
			{Key: "avg_snf_pct", Value: avgSNF},
		}
	}

	now := time.Now().UTC()
	matched, err := s.repo.markConsignmentDispatched(ctx, id, now, extraSet)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !matched { // lost a state race since the read above
		return nil, httpx.Conflict("INVALID_CONSIGNMENT_STATE",
			"consignment was modified concurrently; reload and retry")
	}
	event, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventConsignmentDispatched,
		EntityType: domain.EntityConsignment,
		EntityID:   consignment.ID,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  consignment.DCSID,
		Payload: map[string]any{
			"pour_count":            len(consignment.PourIDs),
			"total_quantity_litres": consignment.TotalQuantityLitres,
			"can_count":             consignment.CanCount,
			"date":                  consignment.Date,
			"shift":                 consignment.Shift,
		},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.setConsignmentProvenanceSeq(ctx, consignment.ID, event.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	consignment.Status = domain.ConsignmentStatusDispatch
	consignment.DispatchedAt = &now
	consignment.ProvenanceSeq = event.Seq
	return consignment, nil
}

// listConsignments returns a scoped page of consignments. DCS-scoped actors
// default to their own society; wide read roles (SUPER_ADMIN, STATE_AUDITOR)
// may list unfiltered; everyone else must name a dcs_id inside their scope.
func (s *service) listConsignments(ctx context.Context, actor auth.Actor, q consignmentListQuery, page httpx.Page) ([]domain.DCSConsignment, int64, error) {
	dcsID := q.DCSID
	if dcsID == "" {
		switch {
		case actor.OrgType == domain.OrgTypeDCS && actor.OrgUnitID != "":
			dcsID = actor.OrgUnitID
		case actor.RoleCode == domain.RoleSuperAdmin || actor.RoleCode == domain.RoleStateAuditor:
			// federation-wide read roles may scan unfiltered (still paged)
		default:
			return nil, 0, httpx.BadRequest("DCS_ID_REQUIRED", "dcs_id query parameter is required for your role")
		}
	}
	if dcsID != "" {
		if err := s.orgs.RequireInScope(ctx, actor, dcsID); err != nil {
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
		return nil, 0, httpx.Internal(err)
	}
	return items, total, nil
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
	if req.UnionID == "" {
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
		return nil, err
	}
	union, err := s.orgs.Get(ctx, req.UnionID)
	if err != nil {
		return nil, err
	}
	if union.Type != domain.OrgTypeMilkUnion {
		return nil, httpx.BadRequest("NOT_A_UNION", "org unit "+req.UnionID+" is not a MILK_UNION")
	}

	riderPartyID := req.VanRiderPartyID
	if riderPartyID == "" {
		if actor.RoleCode != domain.RoleVanRider {
			return nil, httpx.BadRequest("RIDER_REQUIRED", "van_rider_party_id is required unless a VAN_RIDER plans their own trip")
		}
		riderPartyID = actor.PartyID
	}

	seen := make(map[string]struct{}, len(req.Stops))
	stops := make([]domain.RouteStop, 0, len(req.Stops))
	for _, stop := range req.Stops {
		if stop.DCSID == "" || stop.ConsignmentID == "" {
			return nil, httpx.BadRequest("INVALID_STOP", "every stop needs dcs_id and consignment_id")
		}
		if _, dup := seen[stop.ConsignmentID]; dup {
			return nil, httpx.BadRequest("DUPLICATE_STOP", "consignment "+stop.ConsignmentID+" appears in more than one stop")
		}
		seen[stop.ConsignmentID] = struct{}{}

		consignment, err := s.loadConsignment(ctx, stop.ConsignmentID)
		if err != nil {
			return nil, err
		}
		if aerr := validateStopConsignment(consignment, stop.DCSID); aerr != nil {
			return nil, aerr
		}
		stops = append(stops, domain.RouteStop{DCSID: stop.DCSID, ConsignmentID: stop.ConsignmentID})
	}

	trip := &domain.RouteTrip{
		ID:              uuid.NewString(),
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
		return nil, httpx.Internal(err)
	}
	return trip, nil
}

// pickupStop records the trip's rider collecting one consignment: the stop is
// stamped with time+temperature, the consignment moves DISPATCHED→PICKED_UP,
// and the trip moves to IN_PROGRESS on its first pickup.
func (s *service) pickupStop(ctx context.Context, actor auth.Actor, tripID, consignmentID string, req pickupRequest) (*domain.RouteTrip, error) {
	if err := validateTemp(req.TempC); err != nil {
		return nil, err
	}
	trip, err := s.loadTrip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if aerr := validatePickup(trip, actor.PartyID, consignmentID); aerr != nil {
		return nil, aerr
	}

	// Guard the consignment transition atomically; only a DISPATCHED
	// consignment can board the van.
	matched, err := s.repo.markConsignmentPickedUp(ctx, consignmentID, trip.ID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !matched {
		consignment, err := s.loadConsignment(ctx, consignmentID)
		if err != nil {
			return nil, err
		}
		return nil, httpx.Conflict("CONSIGNMENT_NOT_DISPATCHED",
			"consignment is "+consignment.Status+"; only DISPATCHED consignments can be picked up")
	}

	now := time.Now().UTC()
	if err := s.repo.markStopPickedUp(ctx, trip.ID, consignmentID, now, *req.TempC, req.Notes); err != nil {
		return nil, httpx.Internal(err)
	}

	event, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventConsignmentPickedUp,
		EntityType: domain.EntityConsignment,
		EntityID:   consignmentID,
		Refs: []provenance.Ref{
			{EntityType: domain.EntityRouteTrip, EntityID: trip.ID, Relation: "carried_by"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: trip.UnionID,
		Payload:   map[string]any{"temp_c": *req.TempC},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.setConsignmentProvenanceSeq(ctx, consignmentID, event.Seq); err != nil {
		return nil, httpx.Internal(err)
	}

	trip.Status = domain.TripStatusInProgress
	if i := findStop(trip, consignmentID); i >= 0 {
		trip.Stops[i].PickedUpAt = &now
		trip.Stops[i].TempC = *req.TempC
		trip.Stops[i].Notes = req.Notes
	}
	return trip, nil
}

// logColdChain appends one in-transit temperature sample to the trip —
// tamper-evidence for the perishable load between village and BMC.
func (s *service) logColdChain(ctx context.Context, actor auth.Actor, tripID string, req coldChainRequest) (*domain.RouteTrip, error) {
	if err := validateTemp(req.TempC); err != nil {
		return nil, err
	}
	trip, err := s.loadTrip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if aerr := validateColdChain(trip, actor.PartyID); aerr != nil {
		return nil, aerr
	}
	entry := domain.ColdChainEntry{
		TS:     time.Now().UTC(),
		TempC:  *req.TempC,
		GeoLat: req.GeoLat,
		GeoLng: req.GeoLng,
	}
	if err := s.repo.pushColdChain(ctx, trip.ID, entry); err != nil {
		return nil, httpx.Internal(err)
	}
	event, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventColdChainLogged,
		EntityType: domain.EntityRouteTrip,
		EntityID:   trip.ID,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  trip.UnionID,
		Payload:    map[string]any{"temp_c": *req.TempC},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.setTripProvenanceSeq(ctx, trip.ID, event.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	trip.ColdChain = append(trip.ColdChain, entry)
	trip.ProvenanceSeq = event.Seq
	return trip, nil
}

// deliverTrip hands the whole load to a BMC: every stop must be picked up,
// the trip moves IN_PROGRESS→DELIVERED and its consignments PICKED_UP→DELIVERED.
func (s *service) deliverTrip(ctx context.Context, actor auth.Actor, tripID string, req deliverRequest) (*domain.RouteTrip, error) {
	if req.BMCID == "" {
		return nil, httpx.BadRequest("MISSING_BMC_ID", "bmc_id is required")
	}
	trip, err := s.loadTrip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if aerr := validateDeliver(trip, actor.PartyID); aerr != nil {
		return nil, aerr
	}
	bmc, err := s.orgs.Get(ctx, req.BMCID)
	if err != nil {
		return nil, err
	}
	if bmc.Type != domain.OrgTypeBMC {
		return nil, httpx.BadRequest("NOT_A_BMC", "org unit "+req.BMCID+" is not a BMC")
	}

	now := time.Now().UTC()
	matched, err := s.repo.markTripDelivered(ctx, trip.ID, req.BMCID, now)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !matched { // lost a state race since the read above
		return nil, httpx.Conflict("INVALID_TRIP_STATE", "trip was modified concurrently; reload and retry")
	}

	consignmentIDs := make([]string, 0, len(trip.Stops))
	refs := make([]provenance.Ref, 0, len(trip.Stops))
	for _, stop := range trip.Stops {
		consignmentIDs = append(consignmentIDs, stop.ConsignmentID)
		refs = append(refs, provenance.Ref{
			EntityType: domain.EntityConsignment, EntityID: stop.ConsignmentID, Relation: "delivered",
		})
	}
	if err := s.repo.markConsignmentsDelivered(ctx, consignmentIDs); err != nil {
		return nil, httpx.Internal(err)
	}

	event, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventTripDelivered,
		EntityType: domain.EntityRouteTrip,
		EntityID:   trip.ID,
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  trip.UnionID,
		Payload:    map[string]any{"bmc_id": req.BMCID},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.setTripProvenanceSeq(ctx, trip.ID, event.Seq); err != nil {
		return nil, httpx.Internal(err)
	}

	trip.Status = domain.TripStatusDelivered
	trip.DeliveredToBMCID = req.BMCID
	trip.DeliveredAt = &now
	trip.ProvenanceSeq = event.Seq
	return trip, nil
}

// listTrips returns a scoped page of trips: riders are forced onto their own
// trips; supervisors/presidents default to (and are checked against) their
// union scope.
func (s *service) listTrips(ctx context.Context, actor auth.Actor, q tripListQuery, page httpx.Page) ([]domain.RouteTrip, int64, error) {
	unionID := q.UnionID
	riderPartyID := q.VanRiderPartyID
	if actor.RoleCode == domain.RoleVanRider {
		riderPartyID = actor.PartyID // riders only ever see their own trips
	} else {
		if unionID == "" &&
			actor.RoleCode != domain.RoleSuperAdmin && actor.RoleCode != domain.RoleStateAuditor {
			unionID = actor.OrgUnitID // supervisors/presidents default to their union
		}
		if unionID != "" {
			if err := s.orgs.RequireInScope(ctx, actor, unionID); err != nil {
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
		return nil, 0, httpx.Internal(err)
	}
	return items, total, nil
}

// getTrip returns one trip: a rider only their own, everyone else within
// union scope.
func (s *service) getTrip(ctx context.Context, actor auth.Actor, tripID string) (*domain.RouteTrip, error) {
	trip, err := s.loadTrip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if actor.RoleCode == domain.RoleVanRider {
		if trip.VanRiderPartyID != actor.PartyID {
			return nil, httpx.Forbidden("this trip is assigned to another van rider")
		}
		return trip, nil
	}
	if err := s.orgs.RequireInScope(ctx, actor, trip.UnionID); err != nil {
		return nil, err
	}
	return trip, nil
}

// ---------------------------------------------------------------------------
// Loaders (mongo.ErrNoDocuments → 404)
// ---------------------------------------------------------------------------

// loadConsignment fetches a consignment, mapping a miss to 404.
func (s *service) loadConsignment(ctx context.Context, id string) (*domain.DCSConsignment, error) {
	c, err := s.repo.findConsignmentByID(ctx, id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("consignment " + id)
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return c, nil
}

// loadTrip fetches a trip, mapping a miss to 404.
func (s *service) loadTrip(ctx context.Context, id string) (*domain.RouteTrip, error) {
	t, err := s.repo.findTripByID(ctx, id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("trip " + id)
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// Pure guards & helpers (unit-tested in service_test.go)
// ---------------------------------------------------------------------------

// aggregatePours reduces pour rows to (ids, total litres, quantity-weighted
// average fat %, quantity-weighted average SNF %), rounded to 2 decimals.
func aggregatePours(rows []pourRow) (pourIDs []string, totalQty, avgFat, avgSNF float64) {
	var fatWeighted, snfWeighted float64
	pourIDs = make([]string, 0, len(rows))
	for _, row := range rows {
		pourIDs = append(pourIDs, row.ID)
		totalQty += row.QuantityLitres
		fatWeighted += row.FatPct * row.QuantityLitres
		snfWeighted += row.SNFPct * row.QuantityLitres
	}
	if totalQty > 0 {
		avgFat = round2(fatWeighted / totalQty)
		avgSNF = round2(snfWeighted / totalQty)
	}
	return pourIDs, round2(totalQty), avgFat, avgSNF
}

// round2 rounds to 2 decimal places (litres / percentage precision).
func round2(v float64) float64 { return math.Round(v*100) / 100 }

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
func findStop(trip *domain.RouteTrip, consignmentID string) int {
	for i := range trip.Stops {
		if trip.Stops[i].ConsignmentID == consignmentID {
			return i
		}
	}
	return -1
}

// requireTripRider enforces that only the trip's assigned van rider acts on it.
func requireTripRider(trip *domain.RouteTrip, riderPartyID string) *httpx.AppError {
	if trip.VanRiderPartyID != riderPartyID {
		return httpx.Forbidden("this trip is assigned to another van rider")
	}
	return nil
}

// validateStopConsignment guards trip planning: the stop's consignment must
// be DISPATCHED (state machine) and must belong to the stop's DCS.
func validateStopConsignment(consignment *domain.DCSConsignment, stopDCSID string) *httpx.AppError {
	if consignment.Status != domain.ConsignmentStatusDispatch {
		return httpx.Conflict("CONSIGNMENT_NOT_DISPATCHED",
			"consignment "+consignment.ID+" is "+consignment.Status+"; only DISPATCHED consignments can be planned onto a trip")
	}
	if consignment.DCSID != stopDCSID {
		return httpx.Unprocessable("CONSIGNMENT_DCS_MISMATCH",
			"consignment "+consignment.ID+" belongs to DCS "+consignment.DCSID+", not the stop's DCS "+stopDCSID)
	}
	return nil
}

// validatePickup guards the pickup transition: right rider, trip not yet
// delivered, stop exists, stop not already picked.
func validatePickup(trip *domain.RouteTrip, riderPartyID, consignmentID string) *httpx.AppError {
	if err := requireTripRider(trip, riderPartyID); err != nil {
		return err
	}
	if trip.Status == domain.TripStatusDelivered {
		return httpx.Conflict("TRIP_ALREADY_DELIVERED", "trip is DELIVERED; no further pickups are possible")
	}
	i := findStop(trip, consignmentID)
	if i < 0 {
		return httpx.NotFound("stop for consignment " + consignmentID)
	}
	if trip.Stops[i].PickedUpAt != nil {
		return httpx.Conflict("STOP_ALREADY_PICKED", "consignment "+consignmentID+" was already picked up on this trip")
	}
	return nil
}

// validateColdChain guards cold-chain logging: right rider, trip still moving.
func validateColdChain(trip *domain.RouteTrip, riderPartyID string) *httpx.AppError {
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
func validateDeliver(trip *domain.RouteTrip, riderPartyID string) *httpx.AppError {
	if err := requireTripRider(trip, riderPartyID); err != nil {
		return err
	}
	if trip.Status == domain.TripStatusDelivered {
		return httpx.Conflict("TRIP_ALREADY_DELIVERED", "trip is already DELIVERED")
	}
	var unpicked []string
	for _, stop := range trip.Stops {
		if stop.PickedUpAt == nil {
			unpicked = append(unpicked, stop.ConsignmentID)
		}
	}
	if len(unpicked) > 0 {
		return httpx.Unprocessable("STOPS_NOT_PICKED", "every stop must be picked up before delivery").
			WithDetails(map[string]any{"unpicked_consignment_ids": unpicked})
	}
	return nil
}
