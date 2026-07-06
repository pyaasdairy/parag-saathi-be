package plant

import "go.mongodb.org/mongo-driver/bson/primitive"

// CreateBMCLotRequest pools DELIVERED consignments into a new BMC lot for a
// date+shift at one bulk-milk cooler. ObjectID fields unmarshal from plain
// hex JSON strings.
type CreateBMCLotRequest struct {
	BMCID          primitive.ObjectID   `json:"bmc_id"`
	Date           string               `json:"date,omitempty"` // YYYY-MM-DD; defaults to today (IST)
	Shift          string               `json:"shift"`
	ConsignmentIDs []primitive.ObjectID `json:"consignment_ids"`
}

// CloseBMCLotRequest closes an OPEN lot for QC, recording the chilling
// temperature at close time. Pointer so an explicit 0°C is distinguishable
// from an omitted field.
type CloseBMCLotRequest struct {
	ChillingTempC *float64 `json:"chilling_temp_c"`
}

// CreateBatchRequest pools DISPATCHED (gate-passed) BMC lots into one
// production run at a plant.
type CreateBatchRequest struct {
	PlantID     primitive.ObjectID   `json:"plant_id"`
	BMCLotIDs   []primitive.ObjectID `json:"bmc_lot_ids"`
	ProductType string               `json:"product_type"`
}

// CreateProductLotRequest packages a COMPLETED batch into a SKU lot.
type CreateProductLotRequest struct {
	BatchID     primitive.ObjectID `json:"batch_id"`
	SKU         string             `json:"sku"`
	ProductName string             `json:"product_name"`
	Units       int                `json:"units"`
	UnitSize    string             `json:"unit_size"` // Legal Metrology net quantity, e.g. "500ml"
	MRP         float64            `json:"mrp,omitempty"`
	MfgDate     string             `json:"mfg_date,omitempty"` // YYYY-MM-DD; defaults to today (IST)
	ExpiryDate  string             `json:"expiry_date"`        // YYYY-MM-DD
}

// RecallProductLotRequest pulls a product lot from market (FSSAI §18-C).
type RecallProductLotRequest struct {
	Reason string `json:"reason"`
}

// IssueQRRequest mints one signed QR for a product lot.
type IssueQRRequest struct {
	ProductLotID primitive.ObjectID `json:"product_lot_id"`
}

// ListMeta is the pagination metadata returned alongside every list response.
type ListMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}
