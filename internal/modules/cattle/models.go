package cattle

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// RegisterAnimalRequest is the payload for POST /cattle/animals.
// ObjectID fields unmarshal from plain hex JSON strings.
type RegisterAnimalRequest struct {
	PashuAadhaar    string              `json:"pashu_aadhaar"`  // 12-digit national ear-tag ID
	OwnerPartyID    *primitive.ObjectID `json:"owner_party_id"` // required for non-FARMER registrars; farmers always own their registration
	DCSID           primitive.ObjectID  `json:"dcs_id"`
	Species         string              `json:"species"` // COW, BUFFALO
	Name            string              `json:"name,omitempty"`
	Breed           string              `json:"breed"`
	Sex             string              `json:"sex"`
	LactationStatus string              `json:"lactation_status"` // DRY, LACTATING
}

// LogHealthEventRequest is the payload for POST /cattle/animals/{id}/health-events.
type LogHealthEventRequest struct {
	Type       string         `json:"type"` // one of the domain.HealthEvent* constants
	Details    map[string]any `json:"details"`
	OccurredAt *time.Time     `json:"occurred_at"` // defaults to now (UTC) when omitted
}

// BPSyncResponse reports the outcome of a mock Bharat Pashudhan push.
type BPSyncResponse struct {
	SyncedCount int64  `json:"synced_count"`
	BPSyncRef   string `json:"bp_sync_ref"`
}

// CreateMVUCaseRequest is the payload for POST /cattle/mvu-cases.
type CreateMVUCaseRequest struct {
	AnimalID *primitive.ObjectID `json:"animal_id"` // optional — resolves the DCS from the animal when given
	Symptoms string              `json:"symptoms"`
}

// CloseMVUCaseRequest is the payload for POST /cattle/mvu-cases/{id}/close.
type CloseMVUCaseRequest struct {
	VisitNotes     string               `json:"visit_notes"`
	HealthEventIDs []primitive.ObjectID `json:"health_event_ids"`
}

// CreateEducationRequest is the payload for POST /cattle/education.
type CreateEducationRequest struct {
	Topic       string `json:"topic"`
	Title       string `json:"title"`
	Language    string `json:"language"`
	MediaType   string `json:"media_type"` // AUDIO, VIDEO, INFOGRAPHIC
	MediaURL    string `json:"media_url"`
	DurationSec int    `json:"duration_sec"`
	Published   *bool  `json:"published"` // defaults to true — v1 has no separate publish endpoint
}

// TelemetryRequest is the payload for the dormant collar path,
// POST /cattle/telemetry (§9).
type TelemetryRequest struct {
	PashuAadhaar string         `json:"pashu_aadhaar"`
	Metrics      map[string]any `json:"metrics"`
}

// TelemetryAck acknowledges an accepted (not yet persisted) telemetry frame.
type TelemetryAck struct {
	Accepted     bool   `json:"accepted"`
	PashuAadhaar string `json:"pashu_aadhaar"`
	Note         string `json:"note"`
}

// ListMeta is the pagination envelope metadata for list endpoints.
type ListMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}
