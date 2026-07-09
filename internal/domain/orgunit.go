package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Org-unit types — the cooperative backbone RBAC is scoped to (blueprint §5.1,
// PCDF constitution: Samiti/DCS → Sangh/Union → PCDF apex).
const (
	OrgTypeFederation      = "FEDERATION"       // PCDF / Parag (state apex)
	OrgTypeMilkUnion       = "MILK_UNION"       // district Sangh
	OrgTypeProcessingPlant = "PROCESSING_PLANT" // PCDF plant
	OrgTypeBMC             = "BMC"              // bulk milk cooler / chilling centre
	OrgTypeDCS             = "DCS"              // village samiti
)

// AllOrgTypes is the closed set of org-unit types.
var AllOrgTypes = []string{
	OrgTypeFederation, OrgTypeMilkUnion, OrgTypeProcessingPlant, OrgTypeBMC, OrgTypeDCS,
}

// ValidOrgParent encodes the allowed hierarchy edges.
var ValidOrgParent = map[string][]string{
	OrgTypeFederation:      {}, // root
	OrgTypeMilkUnion:       {OrgTypeFederation},
	OrgTypeProcessingPlant: {OrgTypeMilkUnion, OrgTypeFederation},
	OrgTypeBMC:             {OrgTypeMilkUnion},
	OrgTypeDCS:             {OrgTypeMilkUnion, OrgTypeBMC},
}

// OrgUnit is a node in the cooperative hierarchy.
//
// ID scheme: `_id` is a generated ObjectID (relations always reference it);
// Code is the human-readable unique business key (e.g. "DCS-01842") used for
// display, lookups and seed idempotency — never for joins.
//
// Path holds the ObjectIDs of all ancestors from the root down to (and
// excluding) this node — denormalised so "is X inside Y's scope" is a single
// indexed array-containment check with no recursive queries on the hot path.
type OrgUnit struct {
	ID        primitive.ObjectID   `bson:"_id,omitempty" json:"id"`
	Type      string               `bson:"type"          json:"type"`
	Name      string               `bson:"name"          json:"name"`
	NameHi    string               `bson:"name_hi,omitempty" json:"name_hi,omitempty"` // vernacular display name
	Code      string               `bson:"code"          json:"code"`                  // unique business key, e.g. "DCS-01842"
	ParentID  *primitive.ObjectID  `bson:"parent_id,omitempty" json:"parent_id,omitempty"`
	Path      []primitive.ObjectID `bson:"path"          json:"path"`
	Village   string               `bson:"village,omitempty"  json:"village,omitempty"` // the village a DCS serves
	District  string               `bson:"district,omitempty" json:"district,omitempty"`
	State     string               `bson:"state,omitempty"    json:"state,omitempty"`
	GeoLat    float64              `bson:"geo_lat,omitempty"  json:"geo_lat,omitempty"`
	GeoLng    float64              `bson:"geo_lng,omitempty"  json:"geo_lng,omitempty"`
	Active    bool                 `bson:"active"        json:"active"`
	CreatedAt time.Time            `bson:"created_at"    json:"created_at"`
	UpdatedAt time.Time            `bson:"updated_at"    json:"updated_at"`
}

// IsValidOrgType reports whether t is a known org-unit type.
func IsValidOrgType(t string) bool {
	for _, v := range AllOrgTypes {
		if v == t {
			return true
		}
	}
	return false
}
