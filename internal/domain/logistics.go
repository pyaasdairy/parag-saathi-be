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
	ID                  primitive.ObjectID   `bson:"_id,omitempty" json:"id"`
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

// RouteStop is one DCS pickup on a trip, with the cold-chain temperature
// captured at pickup time.
type RouteStop struct {
	DCSID         primitive.ObjectID `bson:"dcs_id"         json:"dcs_id"`
	ConsignmentID primitive.ObjectID `bson:"consignment_id" json:"consignment_id"`
	PickedUpAt    *time.Time         `bson:"picked_up_at,omitempty" json:"picked_up_at,omitempty"`
	TempC         float64            `bson:"temp_c,omitempty"       json:"temp_c,omitempty"`
	Notes         string             `bson:"notes,omitempty"        json:"notes,omitempty"`
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
	ID               primitive.ObjectID  `bson:"_id,omitempty" json:"id"`
	RouteName        string              `bson:"route_name"    json:"route_name"`
	UnionID          primitive.ObjectID  `bson:"union_id"      json:"union_id"`
	VanRiderPartyID  primitive.ObjectID  `bson:"van_rider_party_id" json:"van_rider_party_id"`
	Date             string              `bson:"date"          json:"date"` // YYYY-MM-DD
	Shift            string              `bson:"shift"         json:"shift"`
	Stops            []RouteStop         `bson:"stops"         json:"stops"`
	ColdChain        []ColdChainEntry    `bson:"cold_chain,omitempty" json:"cold_chain,omitempty"`
	Status           string              `bson:"status"        json:"status"`
	DeliveredToBMCID *primitive.ObjectID `bson:"delivered_to_bmc_id,omitempty" json:"delivered_to_bmc_id,omitempty"`
	DeliveredAt      *time.Time          `bson:"delivered_at,omitempty"        json:"delivered_at,omitempty"`
	ProvenanceSeq    int64               `bson:"provenance_seq,omitempty"      json:"provenance_seq,omitempty"`
	CreatedAt        time.Time           `bson:"created_at"    json:"created_at"`
}
