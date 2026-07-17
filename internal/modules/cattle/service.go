package cattle

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
)

// service holds all cattle business logic: ownership rules, scope checks,
// status transitions, provenance emission and the collar capability gate.
type service struct {
	d    *deps.Deps
	repo *repository
	log  *slog.Logger
}

// newService wires the service to the platform container, the repository and
// the module-scoped logger.
func newService(d *deps.Deps, repo *repository, log *slog.Logger) *service {
	return &service{d: d, repo: repo, log: log}
}

// actorID parses the actor's party identity once per service call — JWT
// claims carry ObjectIDs as hex strings (JWTs are JSON).
func actorID(actor auth.Actor) (primitive.ObjectID, error) {
	return httpx.ParseID(actor.PartyID, "actor")
}

// logFailure routes a failed operation to the right log level — WARN for
// business rejections (4xx AppErrors: scope denials, conflicts, gate blocks),
// ERROR for unexpected failures — and returns err unchanged.
func (s *service) logFailure(ctx context.Context, op string, err error, attrs ...any) error {
	args := append(attrs, slog.Any("err", err))
	var appErr *httpx.AppError
	if errors.As(err, &appErr) && appErr.Status < http.StatusInternalServerError {
		s.log.WarnContext(ctx, op+" rejected", args...)
	} else {
		s.log.ErrorContext(ctx, op+" failed", args...)
	}
	return err
}

// validHealthEventTypes is the closed set of loggable event types — the
// domain constants aligned with Bharat Pashudhan transaction types (§9).
var validHealthEventTypes = map[string]struct{}{
	domain.HealthEventVaccination:   {},
	domain.HealthEventTreatment:     {},
	domain.HealthEventAI:            {},
	domain.HealthEventDiseaseReport: {},
	domain.HealthEventCalving:       {},
	domain.HealthEventMilkRecording: {},
	domain.HealthEventEPrescription: {},
}

// validMVUStatuses is the closed set of MVU case statuses (filter validation).
var validMVUStatuses = map[string]struct{}{
	domain.MVUCaseRequested:  {},
	domain.MVUCaseDispatched: {},
	domain.MVUCaseArrived:    {},
	domain.MVUCaseClosed:     {},
	domain.MVUCaseCancelled:  {},
}

// validatePashuAadhaar enforces the national ear-tag format: exactly 12
// ASCII digits (§9). Pure function — unit tested.
func validatePashuAadhaar(id string) *httpx.AppError {
	if len(id) != 12 {
		return httpx.BadRequest("INVALID_PASHU_AADHAAR", "pashu_aadhaar must be exactly 12 digits")
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return httpx.BadRequest("INVALID_PASHU_AADHAAR", "pashu_aadhaar must contain digits only")
		}
	}
	return nil
}

// resolveAnimalOwner decides who the registered animal belongs to. A FARMER
// may only register their own animals (any explicit owner_party_id must match
// the caller); every other registrar role must name the owner explicitly.
// actorID is the caller's parsed party ObjectID. Pure function — unit tested.
func resolveAnimalOwner(actor auth.Actor, actorID primitive.ObjectID, requestedOwner *primitive.ObjectID) (primitive.ObjectID, *httpx.AppError) {
	if actor.RoleCode == domain.RoleFarmer {
		if requestedOwner != nil && !requestedOwner.IsZero() && *requestedOwner != actorID {
			return primitive.NilObjectID, httpx.Forbidden("farmers may only register their own animals")
		}
		return actorID, nil
	}
	if requestedOwner == nil || requestedOwner.IsZero() {
		return primitive.NilObjectID, httpx.BadRequest("OWNER_REQUIRED", "owner_party_id is required when registering on a farmer's behalf")
	}
	return *requestedOwner, nil
}

// canViewAnimal enforces v1 read access: the owner FARMER, or one of the
// care-circle roles already gated at the route (VETERINARIAN, AI_TECH,
// MISSION_OFFICIAL). actorID is the caller's parsed party ObjectID.
//
// TODO(consent): v2 must additionally verify a DPDP consent artefact from the
// owner (consents collection, blueprint §18-A) before a non-owner role reads
// an animal or its health history. v1 deliberately ships the role check only.
func canViewAnimal(actor auth.Actor, actorID primitive.ObjectID, animal *domain.Animal) *httpx.AppError {
	if actor.RoleCode == domain.RoleFarmer && animal.OwnerPartyID != actorID {
		return httpx.Forbidden("farmers may only view their own animals")
	}
	return nil
}

// appendLedger records a provenance event best-effort. The primary document
// is already committed when this runs, so a ledger hiccup is logged for
// reconciliation rather than turned into a request failure the client would
// retry into a duplicate.
func (s *service) appendLedger(ctx context.Context, in provenance.AppendInput) {
	if _, err := s.d.Ledger.Append(ctx, in); err != nil {
		s.log.ErrorContext(ctx, "provenance append failed",
			slog.String("type", in.Type),
			slog.String("entity_id", in.EntityID),
			slog.Any("err", err),
		)
	}
}

// RegisterAnimal creates an animal record keyed on Pashu Aadhaar (§9) and
// appends the animal.registered provenance event.
func (s *service) RegisterAnimal(ctx context.Context, actor auth.Actor, req RegisterAnimalRequest) (*domain.Animal, error) {
	callerID, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	if aerr := validatePashuAadhaar(req.PashuAadhaar); aerr != nil {
		return nil, aerr
	}
	if req.DCSID.IsZero() {
		return nil, httpx.BadRequest("DCS_REQUIRED", "dcs_id is required")
	}
	if req.Species == "" {
		return nil, httpx.BadRequest("SPECIES_REQUIRED", "species is required")
	}
	owner, aerr := resolveAnimalOwner(actor, callerID, req.OwnerPartyID)
	if aerr != nil {
		s.log.WarnContext(ctx, "animal registration rejected",
			slog.String("reason", aerr.Code),
			slog.String("actor_party_id", actor.PartyID),
			slog.String("actor_role", actor.RoleCode))
		return nil, aerr
	}
	// Farmers register into their own village context; every other registrar
	// must hold scope over the target DCS.
	if actor.RoleCode != domain.RoleFarmer {
		if err := s.d.Orgs.RequireInScope(ctx, actor, req.DCSID); err != nil {
			return nil, s.logFailure(ctx, "register animal scope check", err,
				slog.String("actor_party_id", actor.PartyID),
				slog.String("dcs_id", req.DCSID.Hex()))
		}
	}

	now := time.Now().UTC()
	animal := &domain.Animal{
		// Pre-generated so the insert and the ledger ref share one ID.
		ID:              primitive.NewObjectID(),
		PashuAadhaar:    req.PashuAadhaar,
		OwnerPartyID:    owner,
		DCSID:           req.DCSID,
		Species:         req.Species,
		Name:            strings.TrimSpace(req.Name),
		Breed:           req.Breed,
		Sex:             req.Sex,
		LactationStatus: req.LactationStatus,
		CollarEnabled:   false, // dormant until a collar scheme flips flags.FlagCollarTelemetry (§9)
		Status:          domain.AnimalStatusActive,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.repo.InsertAnimal(ctx, animal); err != nil {
		return nil, s.logFailure(ctx, "insert animal", err,
			slog.String("pashu_aadhaar", req.PashuAadhaar),
			slog.String("actor_party_id", actor.PartyID))
	}

	s.appendLedger(ctx, provenance.AppendInput{
		Type:       domain.EventAnimalRegistered,
		EntityType: domain.EntityAnimal,
		EntityID:   animal.ID.Hex(),
		Refs: []provenance.Ref{
			{EntityType: domain.EntityParty, EntityID: owner.Hex(), Relation: "owned_by"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: animal.DCSID.Hex(),
		Payload: map[string]any{
			"pashu_aadhaar": animal.PashuAadhaar,
			"species":       animal.Species,
		},
	})
	s.log.InfoContext(ctx, "animal registered",
		slog.String("animal_id", animal.ID.Hex()),
		slog.String("pashu_aadhaar", animal.PashuAadhaar),
		slog.String("owner_party_id", owner.Hex()),
		slog.String("dcs_id", animal.DCSID.Hex()),
		slog.String("actor_party_id", actor.PartyID))
	return animal, nil
}

// ListAnimals returns a page of animals. Farmers — and plain session tokens,
// which carry no role scope at all — are pinned to their own herd. Every
// other role (except the federation-wide read roles) must name a DCS inside
// its organisational scope, so a narrowly-scoped or consumer role can never
// enumerate the national animal registry.
func (s *service) ListAnimals(ctx context.Context, actor auth.Actor, ownerPartyID, dcsID primitive.ObjectID, page httpx.Page) ([]domain.Animal, int64, error) {
	switch {
	case actor.RoleCode == domain.RoleFarmer || actor.Kind == auth.TokenKindSession:
		callerID, err := actorID(actor)
		if err != nil {
			return nil, 0, err
		}
		ownerPartyID = callerID
	case actor.RoleCode == domain.RoleSuperAdmin || actor.RoleCode == domain.RoleStateAuditor:
		// federation-wide read roles may filter freely
	default:
		if dcsID.IsZero() {
			return nil, 0, httpx.BadRequest("DCS_REQUIRED", "dcs_id is required for your role")
		}
		if err := s.d.Orgs.RequireInScope(ctx, actor, dcsID); err != nil {
			return nil, 0, s.logFailure(ctx, "list animals scope check", err,
				slog.String("actor_party_id", actor.PartyID),
				slog.String("dcs_id", dcsID.Hex()))
		}
	}
	animals, total, err := s.repo.ListAnimals(ctx, ownerPartyID, dcsID, page)
	if err != nil {
		return nil, 0, s.logFailure(ctx, "list animals", err)
	}
	return animals, total, nil
}

// requireAnimalReadAccess applies the animal read rule: the owner FARMER, or
// a care-circle role whose org scope covers the animal's DCS — a vet from one
// union must not read animal health PII across the federation.
func (s *service) requireAnimalReadAccess(ctx context.Context, actor auth.Actor, animal *domain.Animal) error {
	callerID, err := actorID(actor)
	if err != nil {
		return err
	}
	if aerr := canViewAnimal(actor, callerID, animal); aerr != nil {
		s.log.WarnContext(ctx, "animal read denied",
			slog.String("animal_id", animal.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return aerr
	}
	if actor.RoleCode == domain.RoleFarmer {
		return nil // owner (canViewAnimal already pinned ownership)
	}
	if err := s.d.Orgs.RequireInScope(ctx, actor, animal.DCSID); err != nil {
		return s.logFailure(ctx, "animal read scope check", err,
			slog.String("animal_id", animal.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
	}
	return nil
}

// GetAnimal returns one animal after the v1 access check (owner farmer, or a
// care-circle role within org scope; DPDP consent gating remains a v2 TODO).
func (s *service) GetAnimal(ctx context.Context, actor auth.Actor, animalID primitive.ObjectID) (*domain.Animal, error) {
	animal, err := s.repo.FindAnimalByID(ctx, animalID)
	if err != nil {
		return nil, err
	}
	if err := s.requireAnimalReadAccess(ctx, actor, animal); err != nil {
		return nil, err
	}
	return animal, nil
}

// ListHealthEvents returns a page of an animal's health history under the
// same access rule as GetAnimal.
func (s *service) ListHealthEvents(ctx context.Context, actor auth.Actor, animalID primitive.ObjectID, page httpx.Page) ([]domain.HealthEvent, int64, error) {
	animal, err := s.repo.FindAnimalByID(ctx, animalID)
	if err != nil {
		return nil, 0, err
	}
	if err := s.requireAnimalReadAccess(ctx, actor, animal); err != nil {
		return nil, 0, err
	}
	events, total, err := s.repo.ListHealthEventsByAnimal(ctx, animal.ID, page)
	if err != nil {
		return nil, 0, s.logFailure(ctx, "list health events", err,
			slog.String("animal_id", animal.ID.Hex()))
	}
	return events, total, nil
}

// LogHealthEvent records one health-history entry with bp_sync_status PENDING
// and appends the health.event_logged provenance event.
func (s *service) LogHealthEvent(ctx context.Context, actor auth.Actor, animalID primitive.ObjectID, req LogHealthEventRequest) (*domain.HealthEvent, error) {
	callerID, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	if _, ok := validHealthEventTypes[req.Type]; !ok {
		return nil, httpx.BadRequest("INVALID_HEALTH_EVENT_TYPE", "type must be one of the catalog health event types")
	}
	animal, err := s.repo.FindAnimalByID(ctx, animalID)
	if err != nil {
		return nil, err
	}
	if err := s.d.Orgs.RequireInScope(ctx, actor, animal.DCSID); err != nil {
		return nil, s.logFailure(ctx, "log health event scope check", err,
			slog.String("animal_id", animal.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
	}

	now := time.Now().UTC()
	occurredAt := now
	if req.OccurredAt != nil {
		occurredAt = req.OccurredAt.UTC()
	}
	event := &domain.HealthEvent{
		// Pre-generated so the insert and the ledger payload share one ID.
		ID:              primitive.NewObjectID(),
		AnimalID:        animal.ID,
		PashuAadhaar:    animal.PashuAadhaar,
		Type:            req.Type,
		Details:         req.Details,
		RecordedByParty: callerID,
		RecordedByRole:  actor.RoleCode,
		OccurredAt:      occurredAt,
		BharatPashudhan: domain.BPSyncPending, // pushed later via /bp-sync
		CreatedAt:       now,
	}
	if err := s.repo.InsertHealthEvent(ctx, event); err != nil {
		return nil, s.logFailure(ctx, "insert health event", err,
			slog.String("animal_id", animal.ID.Hex()))
	}

	// The ledger event lands on the animal's timeline (there is no dedicated
	// health-event entity type); the ref pins the animal it was recorded for.
	s.appendLedger(ctx, provenance.AppendInput{
		Type:       domain.EventHealthEventLogged,
		EntityType: domain.EntityAnimal,
		EntityID:   animal.ID.Hex(),
		Refs: []provenance.Ref{
			{EntityType: domain.EntityAnimal, EntityID: animal.ID.Hex(), Relation: "recorded_for"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: animal.DCSID.Hex(),
		Payload: map[string]any{
			"health_event_id": event.ID.Hex(),
			"type":            event.Type,
		},
	})
	s.log.InfoContext(ctx, "health event logged",
		slog.String("health_event_id", event.ID.Hex()),
		slog.String("animal_id", animal.ID.Hex()),
		slog.String("type", event.Type),
		slog.String("actor_party_id", actor.PartyID))
	return event, nil
}

// SyncBharatPashudhan is the MOCK national-DB push (§9): it marks every
// PENDING health event of the animal SYNCED under a mock reference. The real
// integration replaces this method body — API and data shapes stay put.
func (s *service) SyncBharatPashudhan(ctx context.Context, actor auth.Actor, animalID primitive.ObjectID) (*BPSyncResponse, error) {
	animal, err := s.repo.FindAnimalByID(ctx, animalID)
	if err != nil {
		return nil, err
	}
	if err := s.d.Orgs.RequireInScope(ctx, actor, animal.DCSID); err != nil {
		return nil, s.logFailure(ctx, "bp sync scope check", err,
			slog.String("animal_id", animal.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
	}
	ref := "BP-MOCK-" + uuid.NewString()[:8]
	count, err := s.repo.MarkPendingHealthEventsSynced(ctx, animal.ID, ref)
	if err != nil {
		return nil, s.logFailure(ctx, "mark health events synced", err,
			slog.String("animal_id", animal.ID.Hex()))
	}
	s.log.InfoContext(ctx, "bharat pashudhan sync completed",
		slog.String("animal_id", animal.ID.Hex()),
		slog.Int64("synced_count", count),
		slog.String("bp_sync_ref", ref),
		slog.String("actor_party_id", actor.PartyID))
	return &BPSyncResponse{SyncedCount: count, BPSyncRef: ref}, nil
}

// CreateMVUCase opens a 1962 MVU request (§10). The DCS comes from the
// farmer's own animal when animal_id is given, else from the actor's org.
func (s *service) CreateMVUCase(ctx context.Context, actor auth.Actor, req CreateMVUCaseRequest) (*domain.MVUCase, error) {
	farmerID, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Symptoms) == "" {
		return nil, httpx.BadRequest("SYMPTOMS_REQUIRED", "symptoms description is required")
	}

	var dcsID primitive.ObjectID
	if actor.OrgUnitID != "" {
		dcsID, err = httpx.ParseID(actor.OrgUnitID, "org_unit")
		if err != nil {
			return nil, err
		}
	}
	var caseAnimalID *primitive.ObjectID
	if req.AnimalID != nil && !req.AnimalID.IsZero() {
		animal, err := s.repo.FindAnimalByID(ctx, *req.AnimalID)
		if err != nil {
			return nil, err
		}
		if animal.OwnerPartyID != farmerID {
			s.log.WarnContext(ctx, "mvu case rejected: animal not owned by caller",
				slog.String("animal_id", animal.ID.Hex()),
				slog.String("actor_party_id", actor.PartyID))
			return nil, httpx.Forbidden("you may only raise an MVU case for your own animal")
		}
		id := animal.ID
		caseAnimalID = &id
		dcsID = animal.DCSID
	}
	if dcsID.IsZero() {
		return nil, httpx.BadRequest("DCS_UNRESOLVED", "cannot resolve a DCS for this case — provide animal_id or use a DCS-scoped role token")
	}

	mvuCase := &domain.MVUCase{
		// Pre-generated so downstream logging/refs share one ID.
		ID:            primitive.NewObjectID(),
		AnimalID:      caseAnimalID,
		FarmerPartyID: farmerID,
		DCSID:         dcsID,
		Symptoms:      strings.TrimSpace(req.Symptoms),
		Status:        domain.MVUCaseRequested,
		RequestedAt:   time.Now().UTC(),
	}
	if err := s.repo.InsertMVUCase(ctx, mvuCase); err != nil {
		return nil, s.logFailure(ctx, "insert mvu case", err,
			slog.String("actor_party_id", actor.PartyID))
	}
	animalHex := ""
	if mvuCase.AnimalID != nil {
		animalHex = mvuCase.AnimalID.Hex()
	}
	s.log.InfoContext(ctx, "mvu case created",
		slog.String("case_id", mvuCase.ID.Hex()),
		slog.String("farmer_party_id", mvuCase.FarmerPartyID.Hex()),
		slog.String("dcs_id", mvuCase.DCSID.Hex()),
		slog.String("animal_id", animalHex),
		slog.String("actor_party_id", actor.PartyID))
	return mvuCase, nil
}

// DispatchMVUCase moves a case REQUESTED → DISPATCHED, records the assignee
// from the caller's role, and publishes eventbus.TopicMVUDispatched with an
// explicit payload contract — keys: case_id, farmer_party_id, animal_id,
// dcs_id, values ObjectID hex strings (the notifications module reacts, §10).
func (s *service) DispatchMVUCase(ctx context.Context, actor auth.Actor, caseID primitive.ObjectID) (*domain.MVUCase, error) {
	callerID, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	mvuCase, err := s.repo.FindMVUCaseByID(ctx, caseID)
	if err != nil {
		return nil, err
	}
	if err := s.d.Orgs.RequireInScope(ctx, actor, mvuCase.DCSID); err != nil {
		return nil, s.logFailure(ctx, "dispatch mvu case scope check", err,
			slog.String("case_id", mvuCase.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
	}

	var vetPartyID, driverPartyID *primitive.ObjectID
	switch actor.RoleCode {
	case domain.RoleVeterinarian:
		vetPartyID = &callerID
	case domain.RoleMVUDriver:
		driverPartyID = &callerID
		// Any other role reaching here is break-glass SUPER_ADMIN: the case
		// is dispatched without pinning an assignee.
	}

	transitioned, err := s.repo.DispatchMVUCase(ctx, mvuCase.ID, vetPartyID, driverPartyID)
	if err != nil {
		return nil, s.logFailure(ctx, "dispatch mvu case", err,
			slog.String("case_id", mvuCase.ID.Hex()))
	}
	if !transitioned {
		s.log.WarnContext(ctx, "mvu dispatch rejected: case not in REQUESTED state",
			slog.String("case_id", mvuCase.ID.Hex()),
			slog.String("status", mvuCase.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("MVU_NOT_REQUESTED", "MVU case is not in REQUESTED state")
	}

	updated, err := s.repo.FindMVUCaseByID(ctx, mvuCase.ID)
	if err != nil {
		return nil, s.logFailure(ctx, "reload mvu case", err,
			slog.String("case_id", mvuCase.ID.Hex()))
	}
	// Explicit event shape: the subscriber decodes structurally by JSON key,
	// so the case ID must travel under "case_id" — publishing the raw domain
	// document (whose ID marshals as "id") left the farmer SMS caseless.
	// Values are ObjectID hex strings; a caseless animal travels as "".
	animalHex := ""
	if updated.AnimalID != nil {
		animalHex = updated.AnimalID.Hex()
	}
	s.d.Bus.Publish(eventbus.TopicMVUDispatched, map[string]any{
		"case_id":         updated.ID.Hex(),
		"farmer_party_id": updated.FarmerPartyID.Hex(),
		"animal_id":       animalHex,
		"dcs_id":          updated.DCSID.Hex(),
	})
	s.log.InfoContext(ctx, "mvu case dispatched",
		slog.String("case_id", updated.ID.Hex()),
		slog.String("farmer_party_id", updated.FarmerPartyID.Hex()),
		slog.String("dcs_id", updated.DCSID.Hex()),
		slog.String("actor_party_id", actor.PartyID),
		slog.String("actor_role", actor.RoleCode))
	return updated, nil
}

// CloseMVUCase moves a DISPATCHED/ARRIVED case to CLOSED with the visit log.
func (s *service) CloseMVUCase(ctx context.Context, actor auth.Actor, caseID primitive.ObjectID, req CloseMVUCaseRequest) (*domain.MVUCase, error) {
	if strings.TrimSpace(req.VisitNotes) == "" {
		return nil, httpx.BadRequest("VISIT_NOTES_REQUIRED", "visit_notes is required to close a case")
	}
	mvuCase, err := s.repo.FindMVUCaseByID(ctx, caseID)
	if err != nil {
		return nil, err
	}
	if err := s.d.Orgs.RequireInScope(ctx, actor, mvuCase.DCSID); err != nil {
		return nil, s.logFailure(ctx, "close mvu case scope check", err,
			slog.String("case_id", mvuCase.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
	}

	transitioned, err := s.repo.CloseMVUCase(ctx, mvuCase.ID, strings.TrimSpace(req.VisitNotes), req.HealthEventIDs, time.Now().UTC())
	if err != nil {
		return nil, s.logFailure(ctx, "close mvu case", err,
			slog.String("case_id", mvuCase.ID.Hex()))
	}
	if !transitioned {
		s.log.WarnContext(ctx, "mvu close rejected: case not DISPATCHED or ARRIVED",
			slog.String("case_id", mvuCase.ID.Hex()),
			slog.String("status", mvuCase.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("MVU_NOT_OPEN", "MVU case must be DISPATCHED or ARRIVED to close")
	}
	closed, err := s.repo.FindMVUCaseByID(ctx, mvuCase.ID)
	if err != nil {
		return nil, s.logFailure(ctx, "reload mvu case", err,
			slog.String("case_id", mvuCase.ID.Hex()))
	}
	// Notify the requesting farmer that the visit is complete (mvu.closed →
	// platformops). Same explicit key discipline as the dispatch event: the
	// subscriber decodes structurally, so the case id travels under "case_id".
	closedAnimalHex := ""
	if closed.AnimalID != nil {
		closedAnimalHex = closed.AnimalID.Hex()
	}
	s.d.Bus.Publish(eventbus.TopicMVUClosed, map[string]any{
		"case_id":         closed.ID.Hex(),
		"farmer_party_id": closed.FarmerPartyID.Hex(),
		"animal_id":       closedAnimalHex,
		"dcs_id":          closed.DCSID.Hex(),
	})
	s.log.InfoContext(ctx, "mvu case closed",
		slog.String("case_id", closed.ID.Hex()),
		slog.String("farmer_party_id", closed.FarmerPartyID.Hex()),
		slog.Int("health_event_count", len(req.HealthEventIDs)),
		slog.String("actor_party_id", actor.PartyID))
	return closed, nil
}

// ListMVUCases returns a page of cases for the field roles. A dcs_id filter
// is scope-checked against the caller's org.
func (s *service) ListMVUCases(ctx context.Context, actor auth.Actor, dcsID, farmerPartyID primitive.ObjectID, status string, page httpx.Page) ([]domain.MVUCase, int64, error) {
	if status != "" {
		if _, ok := validMVUStatuses[status]; !ok {
			return nil, 0, httpx.BadRequest("INVALID_MVU_STATUS", "status must be one of REQUESTED, DISPATCHED, ARRIVED, CLOSED, CANCELLED")
		}
	}

	// A FARMER may only ever list their OWN cases — force the filter to self,
	// ignoring any dcs_id/farmer_party_id they pass.
	if actor.RoleCode == domain.RoleFarmer {
		self, err := httpx.ParseID(actor.PartyID, "actor")
		if err != nil {
			return nil, 0, err
		}
		farmerPartyID = self
		dcsID = primitive.NilObjectID
	} else if actor.RoleCode != domain.RoleSuperAdmin &&
		actor.RoleCode != domain.RoleStateAuditor &&
		actor.RoleCode != domain.RoleMissionOfficial &&
		actor.RoleCode != domain.RolePCDFAdmin {
		// Scoped field roles (vet/driver/sacheev/adhyaksh) MUST name a DCS so
		// they can never scan every farmer's cases (symptom PII) federation-wide.
		if dcsID.IsZero() {
			return nil, 0, httpx.BadRequest("DCS_REQUIRED", "dcs_id is required for this role")
		}
	}

	if !dcsID.IsZero() {
		if err := s.d.Orgs.RequireInScope(ctx, actor, dcsID); err != nil {
			return nil, 0, s.logFailure(ctx, "list mvu cases scope check", err,
				slog.String("actor_party_id", actor.PartyID),
				slog.String("dcs_id", dcsID.Hex()))
		}
	}
	cases, total, err := s.repo.ListMVUCases(ctx, dcsID, farmerPartyID, status, page)
	if err != nil {
		return nil, 0, s.logFailure(ctx, "list mvu cases", err)
	}
	return cases, total, nil
}

// ListEducation returns a page of PUBLISHED education-hub content (§10).
func (s *service) ListEducation(ctx context.Context, topic, language string, page httpx.Page) ([]domain.EducationContent, int64, error) {
	items, total, err := s.repo.ListPublishedEducation(ctx, topic, language, page)
	if err != nil {
		return nil, 0, s.logFailure(ctx, "list education content", err)
	}
	return items, total, nil
}

// CreateEducation adds an item to the education hub. Published defaults to
// true: v1 has no separate publish step, so unpublished items would be
// unreachable.
func (s *service) CreateEducation(ctx context.Context, req CreateEducationRequest) (*domain.EducationContent, error) {
	if req.Topic == "" || req.Title == "" || req.Language == "" || req.MediaType == "" || req.MediaURL == "" {
		return nil, httpx.BadRequest("EDUCATION_FIELDS_REQUIRED", "topic, title, language, media_type and media_url are required")
	}
	published := true
	if req.Published != nil {
		published = *req.Published
	}
	content := &domain.EducationContent{
		ID:          primitive.NewObjectID(),
		Topic:       req.Topic,
		Title:       req.Title,
		Language:    req.Language,
		MediaType:   req.MediaType,
		MediaURL:    req.MediaURL,
		DurationSec: req.DurationSec,
		Published:   published,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.repo.InsertEducation(ctx, content); err != nil {
		return nil, s.logFailure(ctx, "insert education content", err,
			slog.String("topic", req.Topic))
	}
	s.log.InfoContext(ctx, "education content created",
		slog.String("content_id", content.ID.Hex()),
		slog.String("topic", content.Topic),
		slog.String("language", content.Language),
		slog.Bool("published", content.Published))
	return content, nil
}

// IngestTelemetry is the dormant collar path (§9). The capability gate runs
// FIRST: while flags.FlagCollarTelemetry is off the endpoint refuses with
// FEATURE_DISABLED, proving the backdoor is provisioned but inert. When the
// scheme lands and the flag flips, frames are acknowledged with 202 — the
// storage pipeline ships with the scheme, not before.
func (s *service) IngestTelemetry(ctx context.Context, req TelemetryRequest) (*TelemetryAck, error) {
	if !s.d.Flags.Enabled(ctx, flags.FlagCollarTelemetry) {
		s.log.WarnContext(ctx, "collar telemetry rejected: feature gate disabled",
			slog.String("flag", flags.FlagCollarTelemetry),
			slog.String("pashu_aadhaar", req.PashuAadhaar))
		aerr := httpx.Forbidden("collar telemetry is provisioned but dormant (§9)")
		aerr.Code = "FEATURE_DISABLED"
		return nil, aerr
	}
	if err := validatePashuAadhaar(req.PashuAadhaar); err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, "telemetry frame accepted",
		slog.String("pashu_aadhaar", req.PashuAadhaar))
	return &TelemetryAck{
		Accepted:     true,
		PashuAadhaar: req.PashuAadhaar,
		Note:         "telemetry accepted; persistent storage lands with the collar scheme",
	}, nil
}
