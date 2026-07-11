package domain

import (
	"fmt"
	"math"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
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
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	OrgUnitID primitive.ObjectID `bson:"org_unit_id"   json:"org_unit_id"`
	Name      string             `bson:"name"          json:"name"`
	// Version is the immutable version string a pour pins so pricing is
	// reproducible and a mid-cycle chart change never rewrites history
	// (Developer Note §6.3, Appendix C). Human-readable, e.g. "GON-2026-06".
	Version          string             `bson:"version"       json:"version"`
	BaseRatePerLitre float64            `bson:"base_rate_per_litre" json:"base_rate_per_litre"`
	FatRatePerPoint  float64            `bson:"fat_rate_per_point"  json:"fat_rate_per_point"`
	SNFRatePerPoint  float64            `bson:"snf_rate_per_point"  json:"snf_rate_per_point"`
	EffectiveFrom    time.Time          `bson:"effective_from"      json:"effective_from"`
	Active           bool               `bson:"active"        json:"active"`
	CreatedBy        primitive.ObjectID `bson:"created_by"    json:"created_by"`
	CreatedAt        time.Time          `bson:"created_at"    json:"created_at"`
}

// Assurance levels (Developer Note §6.2) — how fat/SNF was captured. Every
// pour carries one; every downstream aggregate (consignment, batch, lot)
// inherits the WEAKEST assurance in its set, so capture quality is measurable
// and drives societies toward instrument-linked capture.
const (
	AssuranceInstrument = "A" // analyzer streamed the reading (RS-232/BLE) — strongest
	AssuranceOCRPhoto   = "B" // OCR of an analyzer-screen photo, photo retained as evidence
	AssuranceManual     = "C" // keyed by hand — weakest, drives supervisor sampling
)

// AssuranceForSource maps a reading/pour capture mode to its assurance level.
func AssuranceForSource(source string) string {
	switch source {
	case ReadingModeDirect:
		return AssuranceInstrument
	case ReadingModePhotoOCR:
		return AssuranceOCRPhoto
	default:
		return AssuranceManual
	}
}

// AssuranceForCapture derives the pour's assurance from the capture EVIDENCE,
// not just the claimed source (Developer Note §6.2/§12.3): A requires the
// analyzer path WITH a device identity; B requires the retained analyzer-screen
// photo — a stronger-than-manual claim backed by nothing downgrades to C.
func AssuranceForCapture(source, deviceID, photoObjectKey string) string {
	switch source {
	case ReadingModeDirect:
		if deviceID != "" {
			return AssuranceInstrument
		}
		// Analyzer claimed but no device identity: photo evidence still earns B.
		if photoObjectKey != "" {
			return AssuranceOCRPhoto
		}
		return AssuranceManual
	case ReadingModePhotoOCR:
		if photoObjectKey != "" {
			return AssuranceOCRPhoto
		}
		return AssuranceManual // never B without the photo evidence
	default:
		return AssuranceManual
	}
}

// NormalizeReadingMode maps loose client spellings onto the canonical reading
// modes ("analyzer" → ANALYZER_DIRECT, "ocr"/"photo" → PHOTO_OCR, "manual" →
// MANUAL). Unknown values are returned upper-cased so validation still rejects
// them explicitly.
func NormalizeReadingMode(source string) string {
	switch v := strings.ToUpper(strings.TrimSpace(source)); v {
	case "ANALYZER", "ANALYSER", "ANALYZER_DIRECT", "ANALYSER_DIRECT", "DIRECT":
		return ReadingModeDirect
	case "OCR", "PHOTO", "PHOTO_OCR":
		return ReadingModePhotoOCR
	case "MANUAL", "HAND", "KEYED":
		return ReadingModeManual
	default:
		return v
	}
}

// assuranceRank orders assurance so "weakest in a set" is a max-rank pick
// (higher rank = weaker).
var assuranceRank = map[string]int{AssuranceInstrument: 0, AssuranceOCRPhoto: 1, AssuranceManual: 2}

// WeakestAssurance returns the weakest (lowest-trust) assurance among the
// given levels — the value an aggregate inherits (§6.2). Empty input → "".
func WeakestAssurance(levels ...string) string {
	weakest, seen := "", -1
	for _, l := range levels {
		if r, ok := assuranceRank[l]; ok && r > seen {
			weakest, seen = l, r
		}
	}
	return weakest
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
	IntegrityFlagLowOCRConfidence = "LOW_OCR_CONFIDENCE"
	IntegrityFlagImplausibleValue = "IMPLAUSIBLE_VALUE"
	IntegrityFlagMissingGeotag    = "MISSING_GEOTAG"
	IntegrityFlagClockSkew        = "DEVICE_CLOCK_SKEW"
	IntegrityFlagManualEntry      = "MANUAL_ENTRY"
)

// AnalyzerReading is one fat/SNF/CLR measurement with its anti-tamper
// envelope: device identity, geotag, server-vs-device time, evidence photo,
// OCR confidence, plausibility verdict.
type AnalyzerReading struct {
	ID               primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	DCSID            primitive.ObjectID `bson:"dcs_id"        json:"dcs_id"`
	DeviceID         string             `bson:"device_id,omitempty" json:"device_id,omitempty"`
	Mode             string             `bson:"mode"          json:"mode"`
	FatPct           float64            `bson:"fat_pct"       json:"fat_pct"`
	SNFPct           float64            `bson:"snf_pct"       json:"snf_pct"`
	CLR              float64            `bson:"clr,omitempty"       json:"clr,omitempty"`
	WaterPct         float64            `bson:"water_pct,omitempty" json:"water_pct,omitempty"`
	QuantityLitres   float64            `bson:"quantity_litres,omitempty" json:"quantity_litres,omitempty"`
	PhotoObjectKey   string             `bson:"photo_object_key,omitempty" json:"photo_object_key,omitempty"` // immutable evidence in object store
	OCRConfidence    float64            `bson:"ocr_confidence,omitempty"   json:"ocr_confidence,omitempty"`   // 0..1
	GeoLat           float64            `bson:"geo_lat,omitempty" json:"geo_lat,omitempty"`
	GeoLng           float64            `bson:"geo_lng,omitempty" json:"geo_lng,omitempty"`
	DeviceTimestamp  time.Time          `bson:"device_timestamp,omitempty" json:"device_timestamp,omitempty"`
	ServerReceivedAt time.Time          `bson:"server_received_at"         json:"server_received_at"`
	IntegrityFlags   []string           `bson:"integrity_flags,omitempty"  json:"integrity_flags,omitempty"`
	PlausibilityOK   bool               `bson:"plausibility_ok" json:"plausibility_ok"`
	RecordedBy       primitive.ObjectID `bson:"recorded_by"     json:"recorded_by"`
	CreatedAt        time.Time          `bson:"created_at"      json:"created_at"`
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
//
// ID scheme: `_id` is a server-generated ObjectID. ClientEventID is the
// offline-first idempotency key — a STRING minted on the device with no
// server coordination (blueprint §3.1); its unique index makes replays
// harmless. All relations (farmer, DCS, reading, chart) are ObjectID refs.
type MilkPour struct {
	ID                primitive.ObjectID  `bson:"_id,omitempty" json:"id"`
	ClientEventID     string              `bson:"client_event_id"  json:"client_event_id"` // unique, device-generated
	FarmerPartyID     primitive.ObjectID  `bson:"farmer_party_id"  json:"farmer_party_id"`
	AnimalID          *primitive.ObjectID `bson:"animal_id,omitempty" json:"animal_id,omitempty"`
	DCSID             primitive.ObjectID  `bson:"dcs_id"        json:"dcs_id"`
	Shift             string              `bson:"shift"         json:"shift"`
	PourDate          string              `bson:"pour_date"     json:"pour_date"` // YYYY-MM-DD in IST — the settlement day key
	QuantityLitres    float64             `bson:"quantity_litres" json:"quantity_litres"`
	FatPct            float64             `bson:"fat_pct"       json:"fat_pct"`
	SNFPct            float64             `bson:"snf_pct"       json:"snf_pct"`
	CLR               float64             `bson:"clr,omitempty" json:"clr,omitempty"`
	TemperatureC      *float64            `bson:"temperature_c,omitempty" json:"temperature_c,omitempty"` // cold-chain reading at pour time (°C); pointer so 0.0 is distinguishable from "absent"
	RatePerLitre      float64             `bson:"rate_per_litre" json:"rate_per_litre"`
	Amount            float64             `bson:"amount"        json:"amount"`
	RateChartID       primitive.ObjectID  `bson:"rate_chart_id" json:"rate_chart_id"`
	RateChartVersion  string              `bson:"rate_chart_version" json:"rate_chart_version"` // §6.3 pinned pricing version
	AnalyzerReadingID *primitive.ObjectID `bson:"analyzer_reading_id,omitempty" json:"analyzer_reading_id,omitempty"`
	Source            string              `bson:"source"        json:"source"`    // reading mode that produced the values
	Assurance         string              `bson:"assurance"     json:"assurance"` // §6.2 capture assurance A|B|C
	// ParchiPhotoURI is the photo of the paper slip (parchi) handed to the
	// farmer at pour time — receipt-side evidence alongside the analyzer photo.
	ParchiPhotoURI string `bson:"parchi_photo_uri,omitempty" json:"parchi_photo_uri,omitempty"`
	// PhotoObjectKey is the retained analyzer-display photo evidence (object
	// store key) that backs an assurance-B capture (§6.2/§12.3).
	PhotoObjectKey string `bson:"photo_object_key,omitempty" json:"photo_object_key,omitempty"`
	Status            string              `bson:"status"        json:"status"`
	SupersedesPourID  *primitive.ObjectID `bson:"supersedes_pour_id,omitempty" json:"supersedes_pour_id,omitempty"`
	PouredAt          time.Time           `bson:"poured_at"     json:"poured_at"`
	RecordedBy        primitive.ObjectID  `bson:"recorded_by"   json:"recorded_by"`
	DeviceID          string              `bson:"device_id,omitempty" json:"device_id,omitempty"`
	GeoLat            float64             `bson:"geo_lat,omitempty"   json:"geo_lat,omitempty"`
	GeoLng            float64             `bson:"geo_lng,omitempty"   json:"geo_lng,omitempty"`
	ProvenanceSeq     int64               `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
	CreatedAt         time.Time           `bson:"created_at"    json:"created_at"`
}

// Invoice statuses through the same-day payment loop (blueprint §8.1).
const (
	InvoiceStatusIssued            = "ISSUED"
	InvoiceStatusSettlementPending = "SETTLEMENT_PENDING"
	InvoiceStatusPaid              = "PAID"
	InvoiceStatusHold              = "HOLD" // e.g. safety-gate payment hold
)

// Invoice aggregates one farmer's pours at one DCS for one day — the artefact
// behind "same-day farmer payment". InvoiceNumber is the human-readable
// unique business key; relations use ObjectIDs.
type Invoice struct {
	ID                  primitive.ObjectID   `bson:"_id,omitempty" json:"id"`
	InvoiceNumber       string               `bson:"invoice_number" json:"invoice_number"` // unique, e.g. INV-DCS01842-20260706-0001
	FarmerPartyID       primitive.ObjectID   `bson:"farmer_party_id" json:"farmer_party_id"`
	DCSID               primitive.ObjectID   `bson:"dcs_id"        json:"dcs_id"`
	InvoiceDate         string               `bson:"invoice_date"  json:"invoice_date"` // YYYY-MM-DD
	PourIDs             []primitive.ObjectID `bson:"pour_ids"      json:"pour_ids"`
	TotalQuantityLitres float64              `bson:"total_quantity_litres" json:"total_quantity_litres"`
	TotalAmount         float64              `bson:"total_amount"  json:"total_amount"`
	Status              string               `bson:"status"        json:"status"`
	SettlementBatchID   *primitive.ObjectID  `bson:"settlement_batch_id,omitempty" json:"settlement_batch_id,omitempty"`
	IssuedAt            time.Time            `bson:"issued_at"     json:"issued_at"`
	ProvenanceSeq       int64                `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
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
