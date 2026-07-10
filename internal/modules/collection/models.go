package collection

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
)

// CreateRateChartRequest creates a new pricing chart for an org unit and
// deactivates the org's older active charts. ObjectID fields unmarshal from
// plain hex JSON strings.
type CreateRateChartRequest struct {
	OrgUnitID        primitive.ObjectID `json:"org_unit_id"`
	Name             string             `json:"name"`
	Version          string             `json:"version,omitempty"` // §6.3 pricing version; auto-derived if empty
	BaseRatePerLitre float64            `json:"base_rate_per_litre"`
	FatRatePerPoint  float64            `json:"fat_rate_per_point"`
	SNFRatePerPoint  float64            `json:"snf_rate_per_point"`
	EffectiveFrom    *time.Time         `json:"effective_from,omitempty"` // default: now
}

// CreateReadingRequest records one analyzer measurement with its anti-tamper
// envelope (blueprint §8.2). Optional geo/device fields are pointers so
// "absent" is distinguishable from zero.
type CreateReadingRequest struct {
	DCSID           primitive.ObjectID `json:"dcs_id"`
	Mode            string             `json:"mode"` // ANALYZER_DIRECT | PHOTO_OCR | MANUAL
	FatPct          float64            `json:"fat_pct"`
	SNFPct          float64            `json:"snf_pct"`
	CLR             float64            `json:"clr,omitempty"`
	WaterPct        float64            `json:"water_pct,omitempty"`
	QuantityLitres  float64            `json:"quantity_litres,omitempty"`
	DeviceID        string             `json:"device_id,omitempty"`
	PhotoObjectKey  string             `json:"photo_object_key,omitempty"`
	OCRConfidence   float64            `json:"ocr_confidence,omitempty"` // 0..1
	GeoLat          *float64           `json:"geo_lat,omitempty"`
	GeoLng          *float64           `json:"geo_lng,omitempty"`
	DeviceTimestamp *time.Time         `json:"device_timestamp,omitempty"`
}

// CreatePourRequest records one farmer's pour. ClientEventID is the
// device-generated offline-first idempotency key (§3.1) — a STRING business
// key, never an ObjectID. Optional references are *primitive.ObjectID.
type CreatePourRequest struct {
	ClientEventID     string              `json:"client_event_id"`
	FarmerPartyID     primitive.ObjectID  `json:"farmer_party_id"`
	DCSID             primitive.ObjectID  `json:"dcs_id"`
	Shift             string              `json:"shift"` // MORNING | EVENING
	QuantityLitres    float64             `json:"quantity_litres"`
	FatPct            float64             `json:"fat_pct"`
	SNFPct            float64             `json:"snf_pct"`
	CLR               float64             `json:"clr,omitempty"`
	TemperatureC      *float64            `json:"temperature_c,omitempty"` // cold-chain reading at pour time (°C)
	AnimalID          *primitive.ObjectID `json:"animal_id,omitempty"`
	AnalyzerReadingID *primitive.ObjectID `json:"analyzer_reading_id,omitempty"`
	Source            string              `json:"source"` // reading mode that produced the values
	PouredAt          *time.Time          `json:"poured_at,omitempty"`
	DeviceID          string              `json:"device_id,omitempty"`
	GeoLat            *float64            `json:"geo_lat,omitempty"`
	GeoLng            *float64            `json:"geo_lng,omitempty"`
}

// PourResponse wraps a pour together with the idempotent-replay marker so
// offline sync clients can tell a fresh insert from a harmless replay.
type PourResponse struct {
	Pour             domain.MilkPour `json:"pour"`
	IdempotentReplay bool            `json:"idempotent_replay,omitempty"`
}

// BatchSyncRequest replays up to 500 offline-captured pours in one call —
// the device reconnect path (§3.1).
type BatchSyncRequest struct {
	Pours []CreatePourRequest `json:"pours"`
}

// Batch-sync per-item outcomes.
const (
	BatchItemCreated   = "created"
	BatchItemDuplicate = "duplicate"
	BatchItemError     = "error"
)

// BatchSyncItemResult reports the outcome of one pour in a batch sync.
// PourID carries the stored pour's ObjectID as a hex string.
type BatchSyncItemResult struct {
	ClientEventID string `json:"client_event_id"`
	Status        string `json:"status"` // created | duplicate | error
	PourID        string `json:"pour_id,omitempty"`
	Error         string `json:"error,omitempty"`
}

// CorrectedValues carries the fields a supersede correction may change; nil
// means "keep the original value".
type CorrectedValues struct {
	QuantityLitres *float64 `json:"quantity_litres,omitempty"`
	FatPct         *float64 `json:"fat_pct,omitempty"`
	SNFPct         *float64 `json:"snf_pct,omitempty"`
}

// SupersedePourRequest is an append-only correction (§3.4): the old pour is
// marked SUPERSEDED and a new repriced pour references it.
type SupersedePourRequest struct {
	Reason    string          `json:"reason"`
	Corrected CorrectedValues `json:"corrected"`
}

// GenerateInvoicesRequest issues the day's per-farmer invoices for a DCS.
type GenerateInvoicesRequest struct {
	DCSID primitive.ObjectID `json:"dcs_id"`
	Date  string             `json:"date,omitempty"` // YYYY-MM-DD IST; default: today
}

// GenerateInvoicesResponse summarises an invoice generation run. Updated
// counts existing invoices that had late pours merged in; SkippedPourIDs
// surfaces pours that could NOT be invoiced because the farmer's invoice for
// the day is already frozen by settlement — staff must resolve them manually.
type GenerateInvoicesResponse struct {
	Created        int                  `json:"created"`
	Updated        int                  `json:"updated"`
	Existing       int                  `json:"existing"`
	SkippedPourIDs []primitive.ObjectID `json:"skipped_pour_ids,omitempty"`
	Invoices       []domain.Invoice     `json:"invoices"`
}

// listMeta is the platform-wide pagination meta shape ({limit, offset, total}).
type listMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}
