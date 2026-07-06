package logistics

import "go.mongodb.org/mongo-driver/bson/primitive"

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
// pointer so a legitimate 0.0 °C reading is distinguishable from "missing".
type pickupRequest struct {
	TempC *float64 `json:"temp_c"`
	Notes string   `json:"notes,omitempty"`
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

// listMeta is the pagination envelope for list responses.
type listMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}
