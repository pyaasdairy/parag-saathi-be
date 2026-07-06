package cattle

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

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
}

// newService wires the service to the platform container and the repository.
func newService(d *deps.Deps, repo *repository) *service {
	return &service{d: d, repo: repo}
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
// Pure function — unit tested.
func resolveAnimalOwner(actor auth.Actor, requestedOwner string) (string, *httpx.AppError) {
	if actor.RoleCode == domain.RoleFarmer {
		if requestedOwner != "" && requestedOwner != actor.PartyID {
			return "", httpx.Forbidden("farmers may only register their own animals")
		}
		return actor.PartyID, nil
	}
	if requestedOwner == "" {
		return "", httpx.BadRequest("OWNER_REQUIRED", "owner_party_id is required when registering on a farmer's behalf")
	}
	return requestedOwner, nil
}

// canViewAnimal enforces v1 read access: the owner FARMER, or one of the
// care-circle roles already gated at the route (VETERINARIAN, AI_TECH,
// MISSION_OFFICIAL).
//
// TODO(consent): v2 must additionally verify a DPDP consent artefact from the
// owner (consents collection, blueprint §18-A) before a non-owner role reads
// an animal or its health history. v1 deliberately ships the role check only.
func canViewAnimal(actor auth.Actor, animal *domain.Animal) *httpx.AppError {
	if actor.RoleCode == domain.RoleFarmer && animal.OwnerPartyID != actor.PartyID {
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
		s.d.Log.Error("cattle: provenance append failed",
			slog.String("type", in.Type),
			slog.String("entity_id", in.EntityID),
			slog.Any("err", err),
		)
	}
}

// RegisterAnimal creates an animal record keyed on Pashu Aadhaar (§9) and
// appends the animal.registered provenance event.
func (s *service) RegisterAnimal(ctx context.Context, actor auth.Actor, req RegisterAnimalRequest) (*domain.Animal, error) {
	if err := validatePashuAadhaar(req.PashuAadhaar); err != nil {
		return nil, err
	}
	if req.DCSID == "" {
		return nil, httpx.BadRequest("DCS_REQUIRED", "dcs_id is required")
	}
	if req.Species == "" {
		return nil, httpx.BadRequest("SPECIES_REQUIRED", "species is required")
	}
	owner, aerr := resolveAnimalOwner(actor, req.OwnerPartyID)
	if aerr != nil {
		return nil, aerr
	}
	// Farmers register into their own village context; every other registrar
	// must hold scope over the target DCS.
	if actor.RoleCode != domain.RoleFarmer {
		if err := s.d.Orgs.RequireInScope(ctx, actor, req.DCSID); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	animal := &domain.Animal{
		ID:              uuid.NewString(),
		PashuAadhaar:    req.PashuAadhaar,
		OwnerPartyID:    owner,
		DCSID:           req.DCSID,
		Species:         req.Species,
		Breed:           req.Breed,
		Sex:             req.Sex,
		LactationStatus: req.LactationStatus,
		CollarEnabled:   false, // dormant until a collar scheme flips flags.FlagCollarTelemetry (§9)
		Status:          domain.AnimalStatusActive,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.repo.InsertAnimal(ctx, animal); err != nil {
		return nil, err
	}

	s.appendLedger(ctx, provenance.AppendInput{
		Type:       domain.EventAnimalRegistered,
		EntityType: domain.EntityAnimal,
		EntityID:   animal.ID,
		Refs: []provenance.Ref{
			{EntityType: domain.EntityParty, EntityID: owner, Relation: "owned_by"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: animal.DCSID,
		Payload: map[string]any{
			"pashu_aadhaar": animal.PashuAadhaar,
			"species":       animal.Species,
		},
	})
	return animal, nil
}

// ListAnimals returns a page of animals. Farmers — and plain session tokens,
// which carry no role scope at all — are pinned to their own herd. Every
// other role (except the federation-wide read roles) must name a DCS inside
// its organisational scope, so a narrowly-scoped or consumer role can never
// enumerate the national animal registry.
func (s *service) ListAnimals(ctx context.Context, actor auth.Actor, ownerPartyID, dcsID string, page httpx.Page) ([]domain.Animal, int64, error) {
	switch {
	case actor.RoleCode == domain.RoleFarmer || actor.Kind == auth.TokenKindSession:
		ownerPartyID = actor.PartyID
	case actor.RoleCode == domain.RoleSuperAdmin || actor.RoleCode == domain.RoleStateAuditor:
		// federation-wide read roles may filter freely
	default:
		if dcsID == "" {
			return nil, 0, httpx.BadRequest("DCS_REQUIRED", "dcs_id is required for your role")
		}
		if err := s.d.Orgs.RequireInScope(ctx, actor, dcsID); err != nil {
			return nil, 0, err
		}
	}
	return s.repo.ListAnimals(ctx, ownerPartyID, dcsID, page)
}

// requireAnimalReadAccess applies the animal read rule: the owner FARMER, or
// a care-circle role whose org scope covers the animal's DCS — a vet from one
// union must not read animal health PII across the federation.
func (s *service) requireAnimalReadAccess(ctx context.Context, actor auth.Actor, animal *domain.Animal) error {
	if aerr := canViewAnimal(actor, animal); aerr != nil {
		return aerr
	}
	if actor.RoleCode == domain.RoleFarmer {
		return nil // owner (canViewAnimal already pinned ownership)
	}
	return s.d.Orgs.RequireInScope(ctx, actor, animal.DCSID)
}

// GetAnimal returns one animal after the v1 access check (owner farmer, or a
// care-circle role within org scope; DPDP consent gating remains a v2 TODO).
func (s *service) GetAnimal(ctx context.Context, actor auth.Actor, animalID string) (*domain.Animal, error) {
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
func (s *service) ListHealthEvents(ctx context.Context, actor auth.Actor, animalID string, page httpx.Page) ([]domain.HealthEvent, int64, error) {
	animal, err := s.repo.FindAnimalByID(ctx, animalID)
	if err != nil {
		return nil, 0, err
	}
	if err := s.requireAnimalReadAccess(ctx, actor, animal); err != nil {
		return nil, 0, err
	}
	return s.repo.ListHealthEventsByAnimal(ctx, animal.ID, page)
}

// LogHealthEvent records one health-history entry with bp_sync_status PENDING
// and appends the health.event_logged provenance event.
func (s *service) LogHealthEvent(ctx context.Context, actor auth.Actor, animalID string, req LogHealthEventRequest) (*domain.HealthEvent, error) {
	if _, ok := validHealthEventTypes[req.Type]; !ok {
		return nil, httpx.BadRequest("INVALID_HEALTH_EVENT_TYPE", "type must be one of the catalog health event types")
	}
	animal, err := s.repo.FindAnimalByID(ctx, animalID)
	if err != nil {
		return nil, err
	}
	if err := s.d.Orgs.RequireInScope(ctx, actor, animal.DCSID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	occurredAt := now
	if req.OccurredAt != nil {
		occurredAt = req.OccurredAt.UTC()
	}
	event := &domain.HealthEvent{
		ID:              uuid.NewString(),
		AnimalID:        animal.ID,
		PashuAadhaar:    animal.PashuAadhaar,
		Type:            req.Type,
		Details:         req.Details,
		RecordedByParty: actor.PartyID,
		RecordedByRole:  actor.RoleCode,
		OccurredAt:      occurredAt,
		BharatPashudhan: domain.BPSyncPending, // pushed later via /bp-sync
		CreatedAt:       now,
	}
	if err := s.repo.InsertHealthEvent(ctx, event); err != nil {
		return nil, err
	}

	// The ledger event lands on the animal's timeline (there is no dedicated
	// health-event entity type); the ref pins the animal it was recorded for.
	s.appendLedger(ctx, provenance.AppendInput{
		Type:       domain.EventHealthEventLogged,
		EntityType: domain.EntityAnimal,
		EntityID:   animal.ID,
		Refs: []provenance.Ref{
			{EntityType: domain.EntityAnimal, EntityID: animal.ID, Relation: "recorded_for"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: animal.DCSID,
		Payload: map[string]any{
			"health_event_id": event.ID,
			"type":            event.Type,
		},
	})
	return event, nil
}

// SyncBharatPashudhan is the MOCK national-DB push (§9): it marks every
// PENDING health event of the animal SYNCED under a mock reference. The real
// integration replaces this method body — API and data shapes stay put.
func (s *service) SyncBharatPashudhan(ctx context.Context, actor auth.Actor, animalID string) (*BPSyncResponse, error) {
	animal, err := s.repo.FindAnimalByID(ctx, animalID)
	if err != nil {
		return nil, err
	}
	if err := s.d.Orgs.RequireInScope(ctx, actor, animal.DCSID); err != nil {
		return nil, err
	}
	ref := "BP-MOCK-" + uuid.NewString()[:8]
	count, err := s.repo.MarkPendingHealthEventsSynced(ctx, animal.ID, ref)
	if err != nil {
		return nil, err
	}
	return &BPSyncResponse{SyncedCount: count, BPSyncRef: ref}, nil
}

// CreateMVUCase opens a 1962 MVU request (§10). The DCS comes from the
// farmer's own animal when animal_id is given, else from the actor's org.
func (s *service) CreateMVUCase(ctx context.Context, actor auth.Actor, req CreateMVUCaseRequest) (*domain.MVUCase, error) {
	if strings.TrimSpace(req.Symptoms) == "" {
		return nil, httpx.BadRequest("SYMPTOMS_REQUIRED", "symptoms description is required")
	}

	dcsID := actor.OrgUnitID
	animalID := ""
	if req.AnimalID != "" {
		animal, err := s.repo.FindAnimalByID(ctx, req.AnimalID)
		if err != nil {
			return nil, err
		}
		if animal.OwnerPartyID != actor.PartyID {
			return nil, httpx.Forbidden("you may only raise an MVU case for your own animal")
		}
		animalID = animal.ID
		dcsID = animal.DCSID
	}
	if dcsID == "" {
		return nil, httpx.BadRequest("DCS_UNRESOLVED", "cannot resolve a DCS for this case — provide animal_id or use a DCS-scoped role token")
	}

	mvuCase := &domain.MVUCase{
		ID:            uuid.NewString(),
		AnimalID:      animalID,
		FarmerPartyID: actor.PartyID,
		DCSID:         dcsID,
		Symptoms:      strings.TrimSpace(req.Symptoms),
		Status:        domain.MVUCaseRequested,
		RequestedAt:   time.Now().UTC(),
	}
	if err := s.repo.InsertMVUCase(ctx, mvuCase); err != nil {
		return nil, err
	}
	return mvuCase, nil
}

// DispatchMVUCase moves a case REQUESTED → DISPATCHED, records the assignee
// from the caller's role, and publishes eventbus.TopicMVUDispatched with an
// explicit payload contract — keys: case_id, farmer_party_id, animal_id,
// dcs_id (the notifications module reacts, §10).
func (s *service) DispatchMVUCase(ctx context.Context, actor auth.Actor, caseID string) (*domain.MVUCase, error) {
	mvuCase, err := s.repo.FindMVUCaseByID(ctx, caseID)
	if err != nil {
		return nil, err
	}
	if err := s.d.Orgs.RequireInScope(ctx, actor, mvuCase.DCSID); err != nil {
		return nil, err
	}

	var vetPartyID, driverPartyID string
	switch actor.RoleCode {
	case domain.RoleVeterinarian:
		vetPartyID = actor.PartyID
	case domain.RoleMVUDriver:
		driverPartyID = actor.PartyID
		// Any other role reaching here is break-glass SUPER_ADMIN: the case
		// is dispatched without pinning an assignee.
	}

	transitioned, err := s.repo.DispatchMVUCase(ctx, mvuCase.ID, vetPartyID, driverPartyID)
	if err != nil {
		return nil, err
	}
	if !transitioned {
		return nil, httpx.Conflict("MVU_NOT_REQUESTED", "MVU case is not in REQUESTED state")
	}

	updated, err := s.repo.FindMVUCaseByID(ctx, mvuCase.ID)
	if err != nil {
		return nil, err
	}
	// Explicit event shape: the subscriber decodes structurally by JSON key,
	// so the case ID must travel under "case_id" — publishing the raw domain
	// document (whose ID marshals as "id") left the farmer SMS caseless.
	s.d.Bus.Publish(eventbus.TopicMVUDispatched, map[string]any{
		"case_id":         updated.ID,
		"farmer_party_id": updated.FarmerPartyID,
		"animal_id":       updated.AnimalID,
		"dcs_id":          updated.DCSID,
	})
	return updated, nil
}

// CloseMVUCase moves a DISPATCHED/ARRIVED case to CLOSED with the visit log.
func (s *service) CloseMVUCase(ctx context.Context, actor auth.Actor, caseID string, req CloseMVUCaseRequest) (*domain.MVUCase, error) {
	if strings.TrimSpace(req.VisitNotes) == "" {
		return nil, httpx.BadRequest("VISIT_NOTES_REQUIRED", "visit_notes is required to close a case")
	}
	mvuCase, err := s.repo.FindMVUCaseByID(ctx, caseID)
	if err != nil {
		return nil, err
	}
	if err := s.d.Orgs.RequireInScope(ctx, actor, mvuCase.DCSID); err != nil {
		return nil, err
	}

	transitioned, err := s.repo.CloseMVUCase(ctx, mvuCase.ID, strings.TrimSpace(req.VisitNotes), req.HealthEventIDs, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !transitioned {
		return nil, httpx.Conflict("MVU_NOT_OPEN", "MVU case must be DISPATCHED or ARRIVED to close")
	}
	return s.repo.FindMVUCaseByID(ctx, mvuCase.ID)
}

// ListMVUCases returns a page of cases for the field roles. A dcs_id filter
// is scope-checked against the caller's org.
func (s *service) ListMVUCases(ctx context.Context, actor auth.Actor, dcsID, status string, page httpx.Page) ([]domain.MVUCase, int64, error) {
	if status != "" {
		if _, ok := validMVUStatuses[status]; !ok {
			return nil, 0, httpx.BadRequest("INVALID_MVU_STATUS", "status must be one of REQUESTED, DISPATCHED, ARRIVED, CLOSED, CANCELLED")
		}
	}
	if dcsID != "" {
		if err := s.d.Orgs.RequireInScope(ctx, actor, dcsID); err != nil {
			return nil, 0, err
		}
	}
	return s.repo.ListMVUCases(ctx, dcsID, status, page)
}

// ListEducation returns a page of PUBLISHED education-hub content (§10).
func (s *service) ListEducation(ctx context.Context, topic, language string, page httpx.Page) ([]domain.EducationContent, int64, error) {
	return s.repo.ListPublishedEducation(ctx, topic, language, page)
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
		ID:          uuid.NewString(),
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
		return nil, err
	}
	return content, nil
}

// IngestTelemetry is the dormant collar path (§9). The capability gate runs
// FIRST: while flags.FlagCollarTelemetry is off the endpoint refuses with
// FEATURE_DISABLED, proving the backdoor is provisioned but inert. When the
// scheme lands and the flag flips, frames are acknowledged with 202 — the
// storage pipeline ships with the scheme, not before.
func (s *service) IngestTelemetry(ctx context.Context, req TelemetryRequest) (*TelemetryAck, error) {
	if !s.d.Flags.Enabled(ctx, flags.FlagCollarTelemetry) {
		aerr := httpx.Forbidden("collar telemetry is provisioned but dormant (§9)")
		aerr.Code = "FEATURE_DISABLED"
		return nil, aerr
	}
	if err := validatePashuAadhaar(req.PashuAadhaar); err != nil {
		return nil, err
	}
	return &TelemetryAck{
		Accepted:     true,
		PashuAadhaar: req.PashuAadhaar,
		Note:         "telemetry accepted; persistent storage lands with the collar scheme",
	}, nil
}
