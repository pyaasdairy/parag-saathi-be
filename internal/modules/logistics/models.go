package logistics

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// createConsignmentRequest asks to pool one DCS shift's RECORDED pours.
type createConsignmentRequest struct {
	DCSID primitive.ObjectID `json:"dcs_id"`
	Date  string             `json:"date,omitempty"` // YYYY-MM-DD; defaults to today (IST)
	Shift string             `json:"shift"`
}

// tripStopRequest names one pickup on a planned route.
type tripStopRequest struct {
	DCSID         primitive.ObjectID `json:"dcs_id"`
	ConsignmentID primitive.ObjectID `json:"consignment_id"`
}

// createTripRequest plans a van run across DCS stops for one date+shift.
type createTripRequest struct {
	RouteName       string              `json:"route_name"`
	UnionID         primitive.ObjectID  `json:"union_id"`
	Date            string              `json:"date,omitempty"` // YYYY-MM-DD; defaults to today (IST)
	Shift           string              `json:"shift"`
	VanRiderPartyID *primitive.ObjectID `json:"van_rider_party_id,omitempty"` // defaults to the actor when a VAN_RIDER plans
	Stops           []tripStopRequest   `json:"stops"`
}

// pickupRequest records collecting one consignment at a stop. TempC is a
// pointer so a legitimate 0.0 °C reading is distinguishable from "missing";
// Lat/Lng are pointers so "absent" is distinguishable from zero.
type pickupRequest struct {
	TempC    *float64 `json:"temp_c"`
	Lat      *float64 `json:"lat,omitempty"`
	Lng      *float64 `json:"lng,omitempty"`
	PhotoURI string   `json:"photo_uri,omitempty"`
	Notes    string   `json:"notes,omitempty"`
}

// coldChainRequest logs an in-transit temperature (and optional location)
// sample — tamper-evidence for a perishable load.
type coldChainRequest struct {
	TempC  *float64 `json:"temp_c"`
	GeoLat float64  `json:"geo_lat,omitempty"`
	GeoLng float64  `json:"geo_lng,omitempty"`
}

// deliverRequest hands the whole trip's load to a BMC.
type deliverRequest struct {
	BMCID primitive.ObjectID `json:"bmc_id"`
}

// consignmentListQuery carries the GET /consignments filters. Zero ObjectID
// means "not filtered".
type consignmentListQuery struct {
	DCSID  primitive.ObjectID
	Date   string
	Status string
}

// tripListQuery carries the GET /trips filters. Zero ObjectIDs mean "not
// filtered".
type tripListQuery struct {
	UnionID         primitive.ObjectID
	Date            string
	VanRiderPartyID primitive.ObjectID
}

// consignmentInvoice is the DCS→Union B2B settlement document for one sealed
// consignment. Fresh milk is GST-exempt (HSN 0401): the taxable value is the
// pooled milk's worth (Σ pour amounts), tax is zero and total == taxable. The
// "line item" is the single pooled consignment (qty + weighted fat/SNF), not a
// per-SKU array. Seller is the DCS; buyer is its parent Union.
type consignmentInvoice struct {
	InvoiceNo       string              `json:"invoice_no"`
	ConsignmentID   primitive.ObjectID  `json:"consignment_id"`
	ConsignmentCode string              `json:"consignment_code"`
	FromDCSID       primitive.ObjectID  `json:"from_dcs_id"`
	ToUnionID       *primitive.ObjectID `json:"to_union_id,omitempty"`
	Date            string              `json:"date"`
	Shift           string              `json:"shift"`
	HSNCode         string              `json:"hsn_code"` // 0401 — fresh milk
	GSTNote         string              `json:"gst_note"`
	TotalLitres     float64             `json:"total_litres"`
	AvgFatPct       float64             `json:"avg_fat_pct"`
	AvgSNFPct       float64             `json:"avg_snf_pct"`
	FarmerCount     int                 `json:"farmer_count"`
	TaxableAmount   float64             `json:"taxable_amount"`
	TaxAmount       float64             `json:"tax_amount"`
	TotalAmount     float64             `json:"total_amount"`
	GeneratedByID   *primitive.ObjectID `json:"generated_by_id,omitempty"`
	GeneratedAt     *time.Time          `json:"generated_at,omitempty"`
}

// listMeta is the pagination envelope for list responses.
type listMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}
