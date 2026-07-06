package logistics

// createConsignmentRequest asks to pool one DCS shift's RECORDED pours.
type createConsignmentRequest struct {
	DCSID string `json:"dcs_id"`
	Date  string `json:"date,omitempty"` // YYYY-MM-DD; defaults to today (IST)
	Shift string `json:"shift"`
}

// tripStopRequest names one pickup on a planned route.
type tripStopRequest struct {
	DCSID         string `json:"dcs_id"`
	ConsignmentID string `json:"consignment_id"`
}

// createTripRequest plans a van run across DCS stops for one date+shift.
type createTripRequest struct {
	RouteName       string            `json:"route_name"`
	UnionID         string            `json:"union_id"`
	Date            string            `json:"date,omitempty"` // YYYY-MM-DD; defaults to today (IST)
	Shift           string            `json:"shift"`
	VanRiderPartyID string            `json:"van_rider_party_id,omitempty"` // defaults to the actor when a VAN_RIDER plans
	Stops           []tripStopRequest `json:"stops"`
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
	BMCID string `json:"bmc_id"`
}

// consignmentListQuery carries the GET /consignments filters.
type consignmentListQuery struct {
	DCSID  string
	Date   string
	Status string
}

// tripListQuery carries the GET /trips filters.
type tripListQuery struct {
	UnionID         string
	Date            string
	VanRiderPartyID string
}

// listMeta is the pagination envelope for list responses.
type listMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}
