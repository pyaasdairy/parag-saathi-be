package logistics

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
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
	SealCode string   `json:"seal_code,omitempty"` // §6.4 anti-swap: verified against the consignment's dispatch seal
	Lat      *float64 `json:"lat,omitempty"`
	Lng      *float64 `json:"lng,omitempty"`
	PhotoURI string   `json:"photo_uri,omitempty"`
	// Per-samiti pickup evidence (F4): the analyser display photo and the
	// rider-measured volume; both persisted on the stop AND the consignment.
	AnalyzerPhotoURI     string   `json:"analyzer_photo_uri,omitempty"`
	MeasuredVolumeLitres *float64 `json:"measured_volume_litres,omitempty"`
	Notes                string   `json:"notes,omitempty"`
}

// coldChainRequest logs an in-transit temperature (and optional location)
// sample — tamper-evidence for a perishable load.
type coldChainRequest struct {
	TempC  *float64 `json:"temp_c"`
	GeoLat float64  `json:"geo_lat,omitempty"`
	GeoLng float64  `json:"geo_lng,omitempty"`
}

// deliverRequest hands the whole trip's load to a receiving facility — a BMC
// (chilling branch) OR a PROCESSING_PLANT (direct-to-plant branch, F5).
// facility_id is the canonical field; bmc_id is kept as an accepted alias so
// existing callers keep working.
type deliverRequest struct {
	BMCID      primitive.ObjectID `json:"bmc_id"`
	FacilityID primitive.ObjectID `json:"facility_id"`
}

// plantAcceptRequest is the PLANT_OPERATOR's intake approval of one delivered
// per-samiti batch (F6): an intake photo plus optional notes.
type plantAcceptRequest struct {
	PhotoURI string `json:"photo_uri,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// plantRejectRequest is the PLANT_OPERATOR's intake rejection of one delivered
// per-samiti batch (F6). Reason is required.
type plantRejectRequest struct {
	Reason string `json:"reason"`
}

// ConsignmentPlantDecidedEvent is published on
// eventbus.TopicConsignmentPlantAccepted / ...PlantRejected after the F6 plant
// intake decision — platformops notifies the samiti sachiv. IDs travel as hex
// strings so subscribers decode structurally.
type ConsignmentPlantDecidedEvent struct {
	ConsignmentID string  `json:"consignment_id"`
	BatchCode     string  `json:"batch_code"`
	DCSID         string  `json:"dcs_id"`
	PlantID       string  `json:"plant_id"`
	Litres        float64 `json:"litres"`
	Reason        string  `json:"reason,omitempty"`
}

// locationRequest is one live GPS ping from the van while en route (§7.1). The
// rider's device streams these; they refresh the trip's last-known position.
type locationRequest struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// tripTrack is the minimal, privacy-safe live view a source Sachiv or the
// destination BMC may read — where the van is now and how far along, NOT the
// full multi-DCS manifest (pooling privacy).
type tripTrack struct {
	TripID           string     `json:"trip_id"`
	RouteName        string     `json:"route_name"`
	Status           string     `json:"status"`
	Shift            string     `json:"shift"`
	Date             string     `json:"date"`
	VanRiderPartyID  string     `json:"van_rider_party_id"`
	UnionID          string     `json:"union_id"`
	LastGeoLat       float64    `json:"last_geo_lat,omitempty"`
	LastGeoLng       float64    `json:"last_geo_lng,omitempty"`
	LastLocationAt   *time.Time `json:"last_location_at,omitempty"`
	DeliveredToBMCID string     `json:"delivered_to_bmc_id,omitempty"`
	StopsTotal       int        `json:"stops_total"`
	StopsCollected   int        `json:"stops_collected"`
}

// toTripTrack projects a RouteTrip onto the minimal track view.
func toTripTrack(t *domain.RouteTrip) tripTrack {
	collected := 0
	for _, s := range t.Stops {
		if s.PickedUpAt != nil {
			collected++
		}
	}
	tt := tripTrack{
		TripID:          t.ID.Hex(),
		RouteName:       t.RouteName,
		Status:          t.Status,
		Shift:           t.Shift,
		Date:            t.Date,
		VanRiderPartyID: t.VanRiderPartyID.Hex(),
		UnionID:         t.UnionID.Hex(),
		LastGeoLat:      t.LastGeoLat,
		LastGeoLng:      t.LastGeoLng,
		LastLocationAt:  t.LastLocationAt,
		StopsTotal:      len(t.Stops),
		StopsCollected:  collected,
	}
	if t.DeliveredToBMCID != nil {
		tt.DeliveredToBMCID = t.DeliveredToBMCID.Hex()
	}
	return tt
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
