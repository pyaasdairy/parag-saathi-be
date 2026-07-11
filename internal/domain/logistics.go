package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// DCS consignment statuses — a day+shift's pooled cans travelling to a BMC.
const (
	ConsignmentStatusOpen      = "OPEN"       // still accepting pours
	ConsignmentStatusDispatch  = "DISPATCHED" // sealed by Sacheev, awaiting pickup
	ConsignmentStatusPickedUp  = "PICKED_UP"  // on the van
	ConsignmentStatusDelivered = "DELIVERED"  // handed to BMC
	ConsignmentStatusAccepted  = "ACCEPTED"   // pooled into a BMC lot
	ConsignmentStatusRejected  = "REJECTED"   // failed BMC rapid test / spoiled
)

// DCSConsignment aggregates a DCS's pours for one date+shift into the unit
// the van picks up. It is the pooling boundary: past this point milk traces
// to a *set* of contributors (blueprint §7.4).
type DCSConsignment struct {
	ID primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	// ConsignmentCode is the human-readable unique business key minted at
	// creation: CON-<last4 of DCS code>-<YYMMDD>-<M|E>, e.g. CON-1842-260710-M.
	ConsignmentCode     string               `bson:"consignment_code" json:"consignment_code"`
	DCSID               primitive.ObjectID   `bson:"dcs_id"    json:"dcs_id"`
	Date                string               `bson:"date"      json:"date"` // YYYY-MM-DD
	Shift               string               `bson:"shift"     json:"shift"`
	PourIDs             []primitive.ObjectID `bson:"pour_ids"  json:"pour_ids"`
	TotalQuantityLitres float64              `bson:"total_quantity_litres" json:"total_quantity_litres"`
	CanCount            int                  `bson:"can_count,omitempty"   json:"can_count,omitempty"`
	// AvgFatPct/AvgSNFPct are the QUANTITY-WEIGHTED averages the Developer Note
	// §6.4 calls wavg_fat / wavg_snf — computed over the sealed pour set.
	AvgFatPct float64 `bson:"avg_fat_pct,omitempty" json:"avg_fat_pct,omitempty"`
	AvgSNFPct float64 `bson:"avg_snf_pct,omitempty" json:"avg_snf_pct,omitempty"`
	// Assurance is the WEAKEST capture assurance among the pooled pours (§6.2).
	Assurance string `bson:"assurance,omitempty" json:"assurance,omitempty"`
	// SealCode freezes the physical cans to this shift's pour set at dispatch
	// (§6.4, Appendix A: SEAL-<consignment>-<checksum>); the van verifies it.
	SealCode      string              `bson:"seal_code,omitempty" json:"seal_code,omitempty"`
	SealedAt      *time.Time          `bson:"sealed_at,omitempty" json:"sealed_at,omitempty"`
	Status        string              `bson:"status"    json:"status"`
	CreatedBy     primitive.ObjectID  `bson:"created_by"    json:"created_by"`
	DispatchedAt  *time.Time          `bson:"dispatched_at,omitempty" json:"dispatched_at,omitempty"`
	RouteTripID   *primitive.ObjectID `bson:"route_trip_id,omitempty" json:"route_trip_id,omitempty"`
	BMCLotID      *primitive.ObjectID `bson:"bmc_lot_id,omitempty"    json:"bmc_lot_id,omitempty"`
	ProvenanceSeq int64               `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
	// Per-samiti batch identity (§6.4-6.7 refinement): minted by the backend at
	// van pickup — PARAG-<DDMMYYYY>-<HHMM>-<farmer count>-<samiti ref>, IST.
	// Unique per samiti per pickup; carried by every consignment read.
	BatchCode     string     `bson:"batch_code,omitempty"      json:"batch_code,omitempty"`
	BatchMintedAt *time.Time `bson:"batch_minted_at,omitempty" json:"batch_minted_at,omitempty"`
	// Van-entered pickup evidence (F4): the measured volume and the analyser
	// display photo captured at the samiti stop, mirrored from the trip stop so
	// the plant queue and the public batch report read them off the consignment.
	MeasuredVolumeLitres *float64   `bson:"measured_volume_litres,omitempty" json:"measured_volume_litres,omitempty"`
	AnalyzerPhotoURI     string     `bson:"analyzer_photo_uri,omitempty"     json:"analyzer_photo_uri,omitempty"`
	PickupPhotoURI       string     `bson:"pickup_photo_uri,omitempty"       json:"pickup_photo_uri,omitempty"`
	PickedUpAt           *time.Time `bson:"picked_up_at,omitempty"           json:"picked_up_at,omitempty"`
	DeliveredAt          *time.Time `bson:"delivered_at,omitempty"           json:"delivered_at,omitempty"`
	// Plant intake (F6): the PLANT_OPERATOR approves (with a photo) or rejects
	// each delivered per-samiti batch.
	AcceptedAt      *time.Time          `bson:"accepted_at,omitempty"       json:"accepted_at,omitempty"`
	AcceptedBy      *primitive.ObjectID `bson:"accepted_by,omitempty"       json:"accepted_by,omitempty"`
	AcceptedPlantID *primitive.ObjectID `bson:"accepted_plant_id,omitempty" json:"accepted_plant_id,omitempty"`
	IntakePhotoURI  string              `bson:"intake_photo_uri,omitempty"  json:"intake_photo_uri,omitempty"`
	IntakeNotes     string              `bson:"intake_notes,omitempty"      json:"intake_notes,omitempty"`
	RejectedAt      *time.Time          `bson:"rejected_at,omitempty"       json:"rejected_at,omitempty"`
	RejectedBy      *primitive.ObjectID `bson:"rejected_by,omitempty"       json:"rejected_by,omitempty"`
	RejectReason    string              `bson:"reject_reason,omitempty"     json:"reject_reason,omitempty"`
	// Per-batch QC verdict (F7): stamped when the PLANT_LAB_ANALYST records the
	// batch QC — PASS keeps ACCEPTED (+ QR minted), REJECT flips to REJECTED,
	// HOLD keeps ACCEPTED with qc_hold=true and allows a re-test (§13.5).
	QCOverallPass *bool      `bson:"qc_overall_pass,omitempty" json:"qc_overall_pass,omitempty"`
	QCVerdict     string     `bson:"qc_verdict,omitempty"      json:"qc_verdict,omitempty"` // PASS | HOLD | REJECT
	QCHold        bool       `bson:"qc_hold,omitempty"         json:"qc_hold,omitempty"`
	QCTestedAt    *time.Time `bson:"qc_tested_at,omitempty"    json:"qc_tested_at,omitempty"`
	// DCS→Union B2B settlement leg: fresh milk is GST-exempt (HSN 0401), so the
	// consignment invoice values the pooled milk (Σ pour amounts) with zero tax.
	// These are stamped when the DCS submits the consignment to its parent Union.
	UnionApproved      bool                `bson:"union_approved,omitempty"          json:"union_approved,omitempty"`
	UnionApprovedAt    *time.Time          `bson:"union_approved_at,omitempty"       json:"union_approved_at,omitempty"`
	UnionApprovedByID  *primitive.ObjectID `bson:"union_approved_by_id,omitempty"    json:"union_approved_by_id,omitempty"`
	UnionInvoiceNo     string              `bson:"union_invoice_no,omitempty"        json:"union_invoice_no,omitempty"`
	UnionInvoiceAmount float64             `bson:"union_invoice_amount,omitempty"    json:"union_invoice_amount,omitempty"`
	CreatedAt          time.Time           `bson:"created_at" json:"created_at"`
}

// Route trip statuses.
const (
	TripStatusPlanned    = "PLANNED"
	TripStatusInProgress = "IN_PROGRESS"
	TripStatusDelivered  = "DELIVERED"
)

// RouteStop is one DCS pickup on a trip, with the cold-chain temperature and
// optional geo/photo evidence captured at pickup time.
type RouteStop struct {
	DCSID         primitive.ObjectID `bson:"dcs_id"         json:"dcs_id"`
	ConsignmentID primitive.ObjectID `bson:"consignment_id" json:"consignment_id"`
	PickedUpAt    *time.Time         `bson:"picked_up_at,omitempty" json:"picked_up_at,omitempty"`
	TempC         float64            `bson:"temp_c,omitempty"       json:"temp_c,omitempty"`
	Lat           float64            `bson:"lat,omitempty"          json:"lat,omitempty"`
	Lng           float64            `bson:"lng,omitempty"          json:"lng,omitempty"`
	PhotoURI      string             `bson:"photo_uri,omitempty"    json:"photo_uri,omitempty"`
	Notes         string             `bson:"notes,omitempty"        json:"notes,omitempty"`
	// Per-samiti pickup evidence (F4): analyser display photo, the rider's
	// measured volume, and the batch code minted for this stop's consignment.
	AnalyzerPhotoURI     string   `bson:"analyzer_photo_uri,omitempty"     json:"analyzer_photo_uri,omitempty"`
	MeasuredVolumeLitres *float64 `bson:"measured_volume_litres,omitempty" json:"measured_volume_litres,omitempty"`
	BatchCode            string   `bson:"batch_code,omitempty"             json:"batch_code,omitempty"`
}

// ColdChainEntry is a timestamped temperature+location sample during transit.
type ColdChainEntry struct {
	TS     time.Time `bson:"ts"      json:"ts"`
	TempC  float64   `bson:"temp_c"  json:"temp_c"`
	GeoLat float64   `bson:"geo_lat,omitempty" json:"geo_lat,omitempty"`
	GeoLng float64   `bson:"geo_lng,omitempty" json:"geo_lng,omitempty"`
}

// RouteTrip is one van run: pick up consignments across DCS stops, log
// cold-chain, deliver to a BMC.
type RouteTrip struct {
	ID              primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	RouteName       string             `bson:"route_name"    json:"route_name"`
	UnionID         primitive.ObjectID `bson:"union_id"      json:"union_id"`
	VanRiderPartyID primitive.ObjectID `bson:"van_rider_party_id" json:"van_rider_party_id"`
	Date            string             `bson:"date"          json:"date"` // YYYY-MM-DD
	Shift           string             `bson:"shift"         json:"shift"`
	Stops           []RouteStop        `bson:"stops"         json:"stops"`
	ColdChain       []ColdChainEntry   `bson:"cold_chain,omitempty" json:"cold_chain,omitempty"`
	Status          string             `bson:"status"        json:"status"`
	// Live location of the van, refreshed while the trip is IN_PROGRESS so the
	// source Sachiv and the destination BMC can watch the load move (§7.1).
	LastGeoLat       float64             `bson:"last_geo_lat,omitempty"     json:"last_geo_lat,omitempty"`
	LastGeoLng       float64             `bson:"last_geo_lng,omitempty"     json:"last_geo_lng,omitempty"`
	LastLocationAt   *time.Time          `bson:"last_location_at,omitempty" json:"last_location_at,omitempty"`
	DeliveredToBMCID *primitive.ObjectID `bson:"delivered_to_bmc_id,omitempty" json:"delivered_to_bmc_id,omitempty"`
	// Facility semantics (F5): a trip may deliver to a BMC OR directly to a
	// PROCESSING_PLANT. delivered_to_facility_id mirrors delivered_to_bmc_id
	// (kept for compatibility); delivered_facility_type names the org type.
	DeliveredToFacilityID *primitive.ObjectID `bson:"delivered_to_facility_id,omitempty" json:"delivered_to_facility_id,omitempty"`
	DeliveredFacilityType string              `bson:"delivered_facility_type,omitempty"  json:"delivered_facility_type,omitempty"`
	DeliveredAt           *time.Time          `bson:"delivered_at,omitempty"        json:"delivered_at,omitempty"`
	ProvenanceSeq         int64               `bson:"provenance_seq,omitempty"      json:"provenance_seq,omitempty"`
	CreatedAt             time.Time           `bson:"created_at"    json:"created_at"`
}
