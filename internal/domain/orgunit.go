package domain

import "time"

// Org-unit types — the cooperative backbone RBAC is scoped to (blueprint §5.1).
// FEDERATION → MILK_UNION → {BMC, PROCESSING_PLANT, DCS}.
const (
	OrgTypeFederation      = "FEDERATION"       // PCDF / Parag (state apex, tier 3)
	OrgTypeMilkUnion       = "MILK_UNION"       // district Sangh (tier 2)
	OrgTypeProcessingPlant = "PROCESSING_PLANT" // PCDF plant
	OrgTypeBMC             = "BMC"              // bulk milk cooler / chilling centre
	OrgTypeDCS             = "DCS"              // village samiti (tier 1)
)

// AllOrgTypes is the closed set of org-unit types.
var AllOrgTypes = []string{
	OrgTypeFederation, OrgTypeMilkUnion, OrgTypeProcessingPlant, OrgTypeBMC, OrgTypeDCS,
}

// ValidOrgParent encodes the allowed hierarchy edges.
var ValidOrgParent = map[string][]string{
	OrgTypeFederation:      {},                       // root
	OrgTypeMilkUnion:       {OrgTypeFederation},      // union under federation
	OrgTypeProcessingPlant: {OrgTypeMilkUnion, OrgTypeFederation},
	OrgTypeBMC:             {OrgTypeMilkUnion},
	OrgTypeDCS:             {OrgTypeMilkUnion, OrgTypeBMC},
}

// OrgUnit is a node in the cooperative hierarchy. Path holds the IDs of all
// ancestors from the root down to (and excluding) this node — kept denormalised
// so "is X inside Y's scope" is a single indexed array-containment check, with
// no recursive queries on the hot path.
type OrgUnit struct {
	ID        string    `bson:"_id"        json:"id"`
	Type      string    `bson:"type"       json:"type"`
	Name      string    `bson:"name"       json:"name"`
	Code      string    `bson:"code"       json:"code"` // human-readable, e.g. "DCS-01842"
	ParentID  string    `bson:"parent_id,omitempty"  json:"parent_id,omitempty"`
	Path      []string  `bson:"path"       json:"path"`
	District  string    `bson:"district,omitempty"   json:"district,omitempty"`
	State     string    `bson:"state,omitempty"      json:"state,omitempty"`
	GeoLat    float64   `bson:"geo_lat,omitempty"    json:"geo_lat,omitempty"`
	GeoLng    float64   `bson:"geo_lng,omitempty"    json:"geo_lng,omitempty"`
	Active    bool      `bson:"active"     json:"active"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
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
