package publictrace

import (
	"time"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
)

// ProductInfo is the pack-label subset of a product lot shown to consumers.
type ProductInfo struct {
	Name       string `json:"name"`
	SKU        string `json:"sku"`
	UnitSize   string `json:"unit_size"`
	MfgDate    string `json:"mfg_date"`
	ExpiryDate string `json:"expiry_date"`
}

// PlantInfo identifies the processing plant that produced the batch.
type PlantInfo struct {
	Name     string `json:"name"`
	District string `json:"district,omitempty"`
}

// QualityInfo is the PLANT_LAB pass certificate attached to the batch.
type QualityInfo struct {
	CertificateNumber string          `json:"certificate_number"`
	Stage             string          `json:"stage"`
	Tests             []domain.QCTest `json:"tests"`
	RecordedAt        time.Time       `json:"recorded_at"`
}

// SamitiInfo is one contributing village society (DCS) — the honest pooled
// provenance grain (§7.4): the set of samitis, never individual farmers.
// Litres/Share are this society's pooled volume contribution to the lot: the
// honest set-valued view names the members of the pool AND how much each
// contributed, but never de-anonymises which litres came from whom.
type SamitiInfo struct {
	Name     string `json:"name"`
	Code     string `json:"code"`
	District string `json:"district,omitempty"`
	// Litres is the total litres this DCS contributed to the traced lot,
	// summed over its contributing consignments' total_quantity_litres.
	Litres float64 `json:"volume_litres"`
	// Share is Litres / (sum of all contributing DCS litres), in [0,1].
	Share float64 `json:"volume_share"`
	// Society coordinates (when registered on the org) so the public trace can
	// map the contributing samitis — society granularity only, never a farm.
	GeoLat *float64 `json:"geo_lat,omitempty"`
	GeoLng *float64 `json:"geo_lng,omitempty"`
}

// FarmerPublicInfo is one CONSENTED farmer on the public roster (§6.7): named
// ONLY when Party.public_consent == true. Non-consenting contributors are
// counted in SourcingInfo.FarmersTotal but NEVER appear here. Fields beyond
// name are surfaced only where the Party record carries them.
type FarmerPublicInfo struct {
	Name        string `json:"name"`
	Village     string `json:"village,omitempty"`
	Cattle      int    `json:"cattle,omitempty"`
	MemberSince string `json:"member_since,omitempty"`
}

// SourcingInfo summarises where and when the milk was collected.
type SourcingInfo struct {
	Message         string       `json:"message"`
	Samitis         []SamitiInfo `json:"samitis"`
	CollectionDates []string     `json:"collection_dates"`
	// FarmersTotal is the count of DISTINCT farmers who contributed pours to
	// the traced lot — consenting and non-consenting alike.
	FarmersTotal int `json:"farmers_total"`
	// FarmersPublic is the bounded, consent-gated roster (§6.7): only farmers
	// whose public_consent is true, capped at rosterCap. Never nil.
	FarmersPublic []FarmerPublicInfo `json:"farmers_public"`
}

// LedgerInfo reports the tamper-evidence check over the traced events.
type LedgerInfo struct {
	Events        int      `json:"events"`
	Intact        bool     `json:"intact"`
	VerifiedRange [2]int64 `json:"verified_range"`
	// Partial is true when the traced span exceeded the per-scan verification
	// cap and only the most recent portion of the range was re-hashed.
	Partial bool `json:"partial,omitempty"`
}

// QRScanResponse is the consumer-facing provenance view for one QR scan.
type QRScanResponse struct {
	Product      ProductInfo  `json:"product"`
	BatchNumber  string       `json:"batch_number"`
	Plant        PlantInfo    `json:"plant"`
	Quality      *QualityInfo `json:"quality,omitempty"`
	Sourcing     SourcingInfo `json:"sourcing"`
	Ledger       LedgerInfo   `json:"ledger"`
	ScanCount    int64        `json:"scan_count"`
	Recalled     bool         `json:"recalled,omitempty"`
	RecallNotice string       `json:"recall_notice,omitempty"`
}

// LedgerVerifyResponse is the public tamper-evidence check result.
type LedgerVerifyResponse struct {
	Intact      bool   `json:"intact"`
	BrokenAtSeq *int64 `json:"broken_at_seq,omitempty"`
	From        int64  `json:"from"`
	To          int64  `json:"to"`
}

// TraceGraphResponse is the full event graph around one entity: upstream
// (what it was made from) and downstream (what it went into).
type TraceGraphResponse struct {
	Upstream   []provenance.Event `json:"upstream"`
	Downstream []provenance.Event `json:"downstream"`
}
