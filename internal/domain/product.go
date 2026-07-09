package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Product is one entry in the platform product master (blueprint §12 admin
// control tower). It is the catalogue backbone the consumer-commerce and store
// modules will read from once those phases land; here it is managed only
// through the admin master surface.
//
// ID scheme: `_id` is a generated ObjectID (relations always reference it);
// SKU is the human-readable unique business key used for upsert idempotency —
// PUT /admin/products keys on SKU, never on the ObjectID.
type Product struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	SKU       string             `bson:"sku"        json:"sku"` // unique business key, e.g. "MILK-TP-500"
	Name      string             `bson:"name"       json:"name"`
	MRP       float64            `bson:"mrp"        json:"mrp"`
	UnitSize  string             `bson:"unit_size"  json:"unit_size"` // e.g. "500ml", "1L", "200g"
	Active    bool               `bson:"active"     json:"active"`
	CreatedAt time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time          `bson:"updated_at" json:"updated_at"`
}
