package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Animal statuses.
const (
	AnimalStatusActive   = "ACTIVE"
	AnimalStatusSold     = "SOLD"
	AnimalStatusDeceased = "DECEASED"
)

// Animal: `_id` is a generated ObjectID; the 12-digit Pashu Aadhaar ear-tag
// is the unique national business key (blueprint §9) so every Saathi record
// stays interoperable with Bharat Pashudhan / NDLM. CollarEnabled is the
// dormant capability flag — OFF until a government collar scheme lands.
type Animal struct {
	ID              primitive.ObjectID `bson:"_id,omitempty"  json:"id"`
	PashuAadhaar    string             `bson:"pashu_aadhaar"  json:"pashu_aadhaar"` // unique 12-digit national ear-tag ID
	OwnerPartyID    primitive.ObjectID `bson:"owner_party_id" json:"owner_party_id"`
	DCSID           primitive.ObjectID `bson:"dcs_id"         json:"dcs_id"`
	Species         string             `bson:"species"        json:"species"` // COW, BUFFALO
	Name            string             `bson:"name,omitempty" json:"name,omitempty"` // the farmer's name for the animal (notice प्रारूप)
	Breed           string             `bson:"breed,omitempty"     json:"breed,omitempty"`
	Sex             string             `bson:"sex,omitempty"       json:"sex,omitempty"`
	BirthDate       *time.Time         `bson:"birth_date,omitempty" json:"birth_date,omitempty"`
	LactationStatus string             `bson:"lactation_status,omitempty" json:"lactation_status,omitempty"` // DRY, LACTATING
	// LactationNo is the animal's current lactation cycle number (1 = first
	// calving); AvgDailyYieldLitres is its recent average daily milk yield —
	// both surface on the farmer's cattle card.
	LactationNo         int     `bson:"lactation_no,omitempty"          json:"lactation_no,omitempty"`
	AvgDailyYieldLitres float64 `bson:"avg_daily_yield_litres,omitempty" json:"avg_daily_yield_litres,omitempty"`
	CollarEnabled   bool               `bson:"collar_enabled" json:"collar_enabled"`
	Status          string             `bson:"status"         json:"status"`
	CreatedAt       time.Time          `bson:"created_at"     json:"created_at"`
	UpdatedAt       time.Time          `bson:"updated_at"     json:"updated_at"`
}

// Health event types — aligned with Bharat Pashudhan transaction types so
// push-back to the national DB is a mapping, not a migration.
const (
	HealthEventVaccination   = "VACCINATION"
	HealthEventTreatment     = "TREATMENT"
	HealthEventAI            = "ARTIFICIAL_INSEMINATION"
	HealthEventDiseaseReport = "DISEASE_REPORT"
	HealthEventCalving       = "CALVING"
	HealthEventMilkRecording = "MILK_RECORDING"
	HealthEventEPrescription = "E_PRESCRIPTION"
)

// Bharat Pashudhan sync states for a health event.
const (
	BPSyncPending = "PENDING"
	BPSyncSynced  = "SYNCED"
	BPSyncFailed  = "FAILED"
)

// HealthEvent is one entry in an animal's health history. Recorded by vets,
// AI techs, or derived from telemetry when the collar flag flips on.
type HealthEvent struct {
	ID              primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	AnimalID        primitive.ObjectID `bson:"animal_id"     json:"animal_id"`
	PashuAadhaar    string             `bson:"pashu_aadhaar" json:"pashu_aadhaar"`
	Type            string             `bson:"type"          json:"type"`
	Details         map[string]any     `bson:"details,omitempty"    json:"details,omitempty"` // vaccine name, diagnosis, semen batch...
	RecordedByParty primitive.ObjectID `bson:"recorded_by_party"    json:"recorded_by_party"`
	RecordedByRole  string             `bson:"recorded_by_role"     json:"recorded_by_role"`
	OccurredAt      time.Time          `bson:"occurred_at"          json:"occurred_at"`
	BharatPashudhan string             `bson:"bp_sync_status"       json:"bp_sync_status"` // PENDING | SYNCED | FAILED
	BPSyncRef       string             `bson:"bp_sync_ref,omitempty" json:"bp_sync_ref,omitempty"`
	CreatedAt       time.Time          `bson:"created_at"    json:"created_at"`
}

// MVU (1962 Mobile Veterinary Unit) dispatch statuses (blueprint §10).
const (
	MVUCaseRequested  = "REQUESTED"
	MVUCaseDispatched = "DISPATCHED"
	MVUCaseArrived    = "ARRIVED"
	MVUCaseClosed     = "CLOSED"
	MVUCaseCancelled  = "CANCELLED"
)

// MVUCase tracks an in-app "call 1962 MVU" request through to the visit log.
type MVUCase struct {
	ID             primitive.ObjectID   `bson:"_id,omitempty" json:"id"`
	AnimalID       *primitive.ObjectID  `bson:"animal_id,omitempty" json:"animal_id,omitempty"`
	FarmerPartyID  primitive.ObjectID   `bson:"farmer_party_id"     json:"farmer_party_id"`
	DCSID          primitive.ObjectID   `bson:"dcs_id"        json:"dcs_id"`
	Symptoms       string               `bson:"symptoms,omitempty"  json:"symptoms,omitempty"`
	Status         string               `bson:"status"        json:"status"`
	VetPartyID     *primitive.ObjectID  `bson:"vet_party_id,omitempty"    json:"vet_party_id,omitempty"`
	DriverPartyID  *primitive.ObjectID  `bson:"driver_party_id,omitempty" json:"driver_party_id,omitempty"`
	VisitNotes     string               `bson:"visit_notes,omitempty"     json:"visit_notes,omitempty"`
	HealthEventIDs []primitive.ObjectID `bson:"health_event_ids,omitempty" json:"health_event_ids,omitempty"`
	RequestedAt    time.Time            `bson:"requested_at"  json:"requested_at"`
	ClosedAt       *time.Time           `bson:"closed_at,omitempty" json:"closed_at,omitempty"`
}

// EducationContent is a vernacular audio/video/infographic item for the
// education hub (blueprint §10) — icon-navigable, pre-cacheable offline.
type EducationContent struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Topic       string             `bson:"topic"      json:"topic"` // CLEAN_MILKING, FEED_RATION, BREED_CARE...
	Title       string             `bson:"title"      json:"title"`
	Language    string             `bson:"language"   json:"language"`
	MediaType   string             `bson:"media_type" json:"media_type"` // AUDIO, VIDEO, INFOGRAPHIC
	MediaURL    string             `bson:"media_url"  json:"media_url"`
	DurationSec int                `bson:"duration_sec,omitempty" json:"duration_sec,omitempty"`
	Published   bool               `bson:"published"  json:"published"`
	CreatedAt   time.Time          `bson:"created_at" json:"created_at"`
}
