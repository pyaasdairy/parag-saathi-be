package domain

import "time"

// BMC lot statuses. A lot cannot advance past QC_PENDING without a passing
// QC result — the safety gate is enforced in software (blueprint §8.3).
const (
	BMCLotStatusOpen       = "OPEN"        // accepting deliveries
	BMCLotStatusQCPending  = "QC_PENDING"  // closed, awaiting rapid tests
	BMCLotStatusPassed     = "PASSED"      // rapid tests within limits
	BMCLotStatusBlocked    = "BLOCKED"     // failed — quarantined, cannot advance
	BMCLotStatusDispatched = "DISPATCHED"  // tankered to plant
	BMCLotStatusPooled     = "POOLED"      // terminal: consumed by a processing batch
)

// BMCLot pools consignments delivered to one bulk-milk-cooler for a date+shift.
type BMCLot struct {
	ID                  string     `bson:"_id"    json:"id"`
	BMCID               string     `bson:"bmc_id" json:"bmc_id"`
	Date                string     `bson:"date"   json:"date"` // YYYY-MM-DD
	Shift               string     `bson:"shift"  json:"shift"`
	ConsignmentIDs      []string   `bson:"consignment_ids" json:"consignment_ids"`
	RouteTripIDs        []string   `bson:"route_trip_ids,omitempty" json:"route_trip_ids,omitempty"`
	TotalQuantityLitres float64    `bson:"total_quantity_litres" json:"total_quantity_litres"`
	ChillingTempC       float64    `bson:"chilling_temp_c,omitempty" json:"chilling_temp_c,omitempty"`
	Status              string     `bson:"status" json:"status"`
	// BatchID stamps the processing batch that pooled (consumed) this lot —
	// set atomically by the DISPATCHED→POOLED claim so one lot can never feed
	// two batches.
	BatchID     string   `bson:"batch_id,omitempty" json:"batch_id,omitempty"`
	QCResultIDs []string `bson:"qc_result_ids,omitempty" json:"qc_result_ids,omitempty"`
	BlockReason string   `bson:"block_reason,omitempty"  json:"block_reason,omitempty"`
	CreatedBy           string     `bson:"created_by" json:"created_by"`
	ClosedAt            *time.Time `bson:"closed_at,omitempty"     json:"closed_at,omitempty"`
	DispatchedAt        *time.Time `bson:"dispatched_at,omitempty" json:"dispatched_at,omitempty"`
	ProvenanceSeq       int64      `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
	CreatedAt           time.Time  `bson:"created_at" json:"created_at"`
}

// Processing batch statuses — same gate discipline as BMC lots.
const (
	BatchStatusCreated   = "CREATED"
	BatchStatusQCPending = "QC_PENDING"
	BatchStatusPassed    = "PASSED"
	BatchStatusBlocked   = "BLOCKED"
	BatchStatusCompleted = "COMPLETED" // product lots issued
)

// ProcessingBatch pools BMC lots at a plant into one production run.
type ProcessingBatch struct {
	ID            string     `bson:"_id"      json:"id"`
	PlantID       string     `bson:"plant_id" json:"plant_id"`
	BatchNumber   string     `bson:"batch_number" json:"batch_number"` // human-readable, printed on packs
	BMCLotIDs     []string   `bson:"bmc_lot_ids"  json:"bmc_lot_ids"`
	ProductType   string     `bson:"product_type" json:"product_type"` // TONED_MILK, FULL_CREAM, GHEE...
	InputLitres   float64    `bson:"input_litres" json:"input_litres"`
	Status        string     `bson:"status"       json:"status"`
	QCResultIDs   []string   `bson:"qc_result_ids,omitempty" json:"qc_result_ids,omitempty"`
	BlockReason   string     `bson:"block_reason,omitempty"  json:"block_reason,omitempty"`
	StartedAt     time.Time  `bson:"started_at"   json:"started_at"`
	CompletedAt   *time.Time `bson:"completed_at,omitempty" json:"completed_at,omitempty"`
	CreatedBy     string     `bson:"created_by"   json:"created_by"`
	ProvenanceSeq int64      `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
	CreatedAt     time.Time  `bson:"created_at"   json:"created_at"`
}

// Product lot statuses.
const (
	ProductLotStatusActive   = "ACTIVE"
	ProductLotStatusRecalled = "RECALLED" // FSSAI recall path
	ProductLotStatusExpired  = "EXPIRED"
)

// ProductLot is a packaged SKU output of a passed batch. QR issuance is only
// legal for lots whose batch has Status == PASSED/COMPLETED.
type ProductLot struct {
	ID            string    `bson:"_id"      json:"id"`
	BatchID       string    `bson:"batch_id" json:"batch_id"`
	PlantID       string    `bson:"plant_id" json:"plant_id"`
	SKU           string    `bson:"sku"      json:"sku"`
	ProductName   string    `bson:"product_name" json:"product_name"`
	Units         int       `bson:"units"        json:"units"`
	UnitSize      string    `bson:"unit_size"    json:"unit_size"` // e.g. "500ml" — Legal Metrology net quantity
	MRP           float64   `bson:"mrp,omitempty" json:"mrp,omitempty"`
	MfgDate       string    `bson:"mfg_date"     json:"mfg_date"` // YYYY-MM-DD
	ExpiryDate    string    `bson:"expiry_date"  json:"expiry_date"`
	Status        string    `bson:"status"       json:"status"`
	RecallReason  string    `bson:"recall_reason,omitempty" json:"recall_reason,omitempty"`
	ProvenanceSeq int64     `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
	CreatedAt     time.Time `bson:"created_at"   json:"created_at"`
}

// BatchQR is the printed/packed QR for a product lot. QRCode is the short
// public identifier; SignedToken is the HMAC-signed payload the consumer app
// resolves, so a QR cannot be forged by guessing IDs.
type BatchQR struct {
	ID            string    `bson:"_id"       json:"id"`
	QRCode        string    `bson:"qr_code"   json:"qr_code"` // short public code, e.g. "PRG-7F3K9QX2"
	ProductLotID  string    `bson:"product_lot_id" json:"product_lot_id"`
	SignedToken   string    `bson:"signed_token"   json:"signed_token"`
	IssuedBy      string    `bson:"issued_by"  json:"issued_by"`
	ScanCount     int64     `bson:"scan_count" json:"scan_count"`
	ProvenanceSeq int64     `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
	IssuedAt      time.Time `bson:"issued_at"  json:"issued_at"`
}
