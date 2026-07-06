package domain

import (
	"fmt"
	"math"
	"time"
)

// Collection shifts.
const (
	ShiftMorning = "MORNING"
	ShiftEvening = "EVENING"
)

// Plausibility bounds for analyzer values (blueprint §8.2 integrity checks).
// Values outside these bounds are physically implausible for raw milk and are
// flagged instead of silently accepted.
const (
	PlausibleFatMin = 2.0
	PlausibleFatMax = 12.0
	PlausibleSNFMin = 7.0
	PlausibleSNFMax = 11.5
	PlausibleQtyMin = 0.1   // litres per pour
	PlausibleQtyMax = 200.0 // litres per pour
)

// RateChart prices milk from fat/SNF, scoped to an org unit (union or
// federation). rate/L = BaseRatePerLitre + Fat%*FatRatePerPoint + SNF%*SNFRatePerPoint.
type RateChart struct {
	ID               string    `bson:"_id"         json:"id"`
	OrgUnitID        string    `bson:"org_unit_id" json:"org_unit_id"`
	Name             string    `bson:"name"        json:"name"`
	BaseRatePerLitre float64   `bson:"base_rate_per_litre" json:"base_rate_per_litre"`
	FatRatePerPoint  float64   `bson:"fat_rate_per_point"  json:"fat_rate_per_point"`
	SNFRatePerPoint  float64   `bson:"snf_rate_per_point"  json:"snf_rate_per_point"`
	EffectiveFrom    time.Time `bson:"effective_from"      json:"effective_from"`
	Active           bool      `bson:"active"      json:"active"`
	CreatedBy        string    `bson:"created_by"  json:"created_by"`
	CreatedAt        time.Time `bson:"created_at"  json:"created_at"`
}

// PricePour computes (ratePerLitre, amount) for a pour, rounded to paise.
func (rc RateChart) PricePour(fatPct, snfPct, qtyLitres float64) (rate, amount float64) {
	rate = rc.BaseRatePerLitre + fatPct*rc.FatRatePerPoint + snfPct*rc.SNFRatePerPoint
	rate = math.Round(rate*100) / 100
	amount = math.Round(rate*qtyLitres*100) / 100
	return rate, amount
}

// Analyzer reading ingestion modes (blueprint §8.2): direct device integration
// is the tamper-resistant end-state; photo-OCR is the legacy bridge.
const (
	ReadingModeDirect   = "ANALYZER_DIRECT"
	ReadingModePhotoOCR = "PHOTO_OCR"
	ReadingModeManual   = "MANUAL"
)

// Integrity flags attached to readings by the anti-tamper checks.
const (
	IntegrityFlagLowOCRConfidence  = "LOW_OCR_CONFIDENCE"
	IntegrityFlagImplausibleValue  = "IMPLAUSIBLE_VALUE"
	IntegrityFlagMissingGeotag     = "MISSING_GEOTAG"
	IntegrityFlagClockSkew         = "DEVICE_CLOCK_SKEW"
	IntegrityFlagManualEntry       = "MANUAL_ENTRY"
)

// AnalyzerReading is one fat/SNF/CLR measurement with its anti-tamper
// envelope: device identity, geotag, server-vs-device time, evidence photo,
// OCR confidence, plausibility verdict.
type AnalyzerReading struct {
	ID               string    `bson:"_id"       json:"id"`
	DCSID            string    `bson:"dcs_id"    json:"dcs_id"`
	DeviceID         string    `bson:"device_id,omitempty" json:"device_id,omitempty"`
	Mode             string    `bson:"mode"      json:"mode"`
	FatPct           float64   `bson:"fat_pct"   json:"fat_pct"`
	SNFPct           float64   `bson:"snf_pct"   json:"snf_pct"`
	CLR              float64   `bson:"clr,omitempty"       json:"clr,omitempty"`
	WaterPct         float64   `bson:"water_pct,omitempty" json:"water_pct,omitempty"`
	QuantityLitres   float64   `bson:"quantity_litres,omitempty" json:"quantity_litres,omitempty"`
	PhotoObjectKey   string    `bson:"photo_object_key,omitempty" json:"photo_object_key,omitempty"` // immutable evidence in object store
	OCRConfidence    float64   `bson:"ocr_confidence,omitempty"   json:"ocr_confidence,omitempty"`   // 0..1
	GeoLat           float64   `bson:"geo_lat,omitempty" json:"geo_lat,omitempty"`
	GeoLng           float64   `bson:"geo_lng,omitempty" json:"geo_lng,omitempty"`
	DeviceTimestamp  time.Time `bson:"device_timestamp,omitempty" json:"device_timestamp,omitempty"`
	ServerReceivedAt time.Time `bson:"server_received_at"         json:"server_received_at"`
	IntegrityFlags   []string  `bson:"integrity_flags,omitempty"  json:"integrity_flags,omitempty"`
	PlausibilityOK   bool      `bson:"plausibility_ok" json:"plausibility_ok"`
	RecordedBy       string    `bson:"recorded_by"     json:"recorded_by"`
	CreatedAt        time.Time `bson:"created_at"      json:"created_at"`
}

// CheckPlausibility validates fat/SNF/qty against physical bounds and returns
// the violated flags (empty slice = plausible).
func CheckPlausibility(fatPct, snfPct, qtyLitres float64) []string {
	var flags []string
	if fatPct < PlausibleFatMin || fatPct > PlausibleFatMax ||
		snfPct < PlausibleSNFMin || snfPct > PlausibleSNFMax {
		flags = append(flags, IntegrityFlagImplausibleValue)
	}
	if qtyLitres != 0 && (qtyLitres < PlausibleQtyMin || qtyLitres > PlausibleQtyMax) {
		flags = append(flags, IntegrityFlagImplausibleValue)
	}
	return flags
}

// Milk pour statuses. Corrections never edit in place: a correcting pour
// references the superseded one (append-only provenance, blueprint §3.4).
const (
	PourStatusRecorded   = "RECORDED"
	PourStatusSuperseded = "SUPERSEDED"
	PourStatusCancelled  = "CANCELLED"
)

// MilkPour is the atomic unit of procurement: one farmer, one can, one shift.
// ClientEventID is the offline-first idempotency key — devices generate it
// locally and sync later; a unique index makes replays harmless.
type MilkPour struct {
	ID                string    `bson:"_id"       json:"id"`
	ClientEventID     string    `bson:"client_event_id"  json:"client_event_id"`
	FarmerPartyID     string    `bson:"farmer_party_id"  json:"farmer_party_id"`
	AnimalID          string    `bson:"animal_id,omitempty" json:"animal_id,omitempty"`
	DCSID             string    `bson:"dcs_id"    json:"dcs_id"`
	Shift             string    `bson:"shift"     json:"shift"`
	PourDate          string    `bson:"pour_date" json:"pour_date"` // YYYY-MM-DD in IST — the settlement day key
	QuantityLitres    float64   `bson:"quantity_litres" json:"quantity_litres"`
	FatPct            float64   `bson:"fat_pct"   json:"fat_pct"`
	SNFPct            float64   `bson:"snf_pct"   json:"snf_pct"`
	CLR               float64   `bson:"clr,omitempty" json:"clr,omitempty"`
	RatePerLitre      float64   `bson:"rate_per_litre" json:"rate_per_litre"`
	Amount            float64   `bson:"amount"    json:"amount"`
	RateChartID       string    `bson:"rate_chart_id"  json:"rate_chart_id"`
	AnalyzerReadingID string    `bson:"analyzer_reading_id,omitempty" json:"analyzer_reading_id,omitempty"`
	Source            string    `bson:"source"    json:"source"` // reading mode that produced the values
	Status            string    `bson:"status"    json:"status"`
	SupersedesPourID  string    `bson:"supersedes_pour_id,omitempty" json:"supersedes_pour_id,omitempty"`
	PouredAt          time.Time `bson:"poured_at"   json:"poured_at"`
	RecordedBy        string    `bson:"recorded_by" json:"recorded_by"`
	DeviceID          string    `bson:"device_id,omitempty" json:"device_id,omitempty"`
	GeoLat            float64   `bson:"geo_lat,omitempty"   json:"geo_lat,omitempty"`
	GeoLng            float64   `bson:"geo_lng,omitempty"   json:"geo_lng,omitempty"`
	ProvenanceSeq     int64     `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
	CreatedAt         time.Time `bson:"created_at"  json:"created_at"`
}

// Invoice statuses through the same-day payment loop (blueprint §8.1).
const (
	InvoiceStatusIssued            = "ISSUED"
	InvoiceStatusSettlementPending = "SETTLEMENT_PENDING"
	InvoiceStatusPaid              = "PAID"
	InvoiceStatusHold              = "HOLD" // e.g. safety-gate payment hold
)

// Invoice aggregates one farmer's pours at one DCS for one day — the artefact
// behind "same-day farmer payment".
type Invoice struct {
	ID                  string    `bson:"_id"     json:"id"`
	InvoiceNumber       string    `bson:"invoice_number" json:"invoice_number"` // human-readable, e.g. INV-DCS01842-20260706-0001
	FarmerPartyID       string    `bson:"farmer_party_id" json:"farmer_party_id"`
	DCSID               string    `bson:"dcs_id"  json:"dcs_id"`
	InvoiceDate         string    `bson:"invoice_date" json:"invoice_date"` // YYYY-MM-DD
	PourIDs             []string  `bson:"pour_ids"     json:"pour_ids"`
	TotalQuantityLitres float64   `bson:"total_quantity_litres" json:"total_quantity_litres"`
	TotalAmount         float64   `bson:"total_amount" json:"total_amount"`
	Status              string    `bson:"status"       json:"status"`
	SettlementBatchID   string    `bson:"settlement_batch_id,omitempty" json:"settlement_batch_id,omitempty"`
	IssuedAt            time.Time `bson:"issued_at"    json:"issued_at"`
	ProvenanceSeq       int64     `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
}

// DateKeyIST formats t as the YYYY-MM-DD settlement-day key in IST.
func DateKeyIST(t time.Time) string {
	ist := time.FixedZone("IST", 5*3600+1800)
	return t.In(ist).Format("2006-01-02")
}

// InvoiceNumberFor builds the human-readable invoice number.
func InvoiceNumberFor(dcsCode, dateKey string, seq int) string {
	return fmt.Sprintf("INV-%s-%s-%04d", dcsCode, dateKey, seq)
}
