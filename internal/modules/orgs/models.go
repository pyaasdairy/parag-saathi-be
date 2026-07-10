package orgs

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// CreateOrgRequest is the body of POST /orgs. ParentID arrives as a plain
// ObjectID hex string in JSON and unmarshals natively; nil means "no parent"
// (hierarchy root).
type CreateOrgRequest struct {
	Type     string              `json:"type"`
	Name     string              `json:"name"`
	NameHi   string              `json:"name_hi,omitempty"`
	Code     string              `json:"code"`
	ParentID *primitive.ObjectID `json:"parent_id,omitempty"`
	Village  string              `json:"village,omitempty"`
	District string              `json:"district,omitempty"`
	State    string              `json:"state,omitempty"`
	GeoLat   *float64            `json:"geo_lat,omitempty"`
	GeoLng   *float64            `json:"geo_lng,omitempty"`
}

// UpdateOrgRequest is the body of PATCH /orgs/{id}. Pointer fields
// distinguish "absent" from "set to zero value". Type and ParentID are
// decoded solely to reject hierarchy moves, which are unsupported in v1 —
// they stay *string so any supplied value (even a malformed id) maps to the
// ORG_MOVE_UNSUPPORTED rejection rather than a decode error.
type UpdateOrgRequest struct {
	Name     *string  `json:"name"`
	NameHi   *string  `json:"name_hi"`
	Village  *string  `json:"village"`
	District *string  `json:"district"`
	Active   *bool    `json:"active"`
	GeoLat   *float64 `json:"geo_lat"`
	GeoLng   *float64 `json:"geo_lng"`
	Type     *string  `json:"type"`
	ParentID *string  `json:"parent_id"`
}

// Member is one row of GET /orgs/{id}/members: an ACTIVE role assignment at
// the org joined in memory with its party's identity fields. ObjectIDs
// marshal to plain hex strings on the wire.
type Member struct {
	PartyID          primitive.ObjectID `json:"party_id"`
	Phone            string             `json:"phone,omitempty"`
	FullName         string             `json:"full_name,omitempty"`
	KYCTier          string             `json:"kyc_tier,omitempty"`
	RoleCode         string             `json:"role_code"`
	RoleAssignmentID primitive.ObjectID `json:"role_assignment_id"`
	ValidFrom        time.Time          `json:"valid_from"`
	ValidTo          *time.Time         `json:"valid_to,omitempty"`
}

// ListMeta echoes pagination inputs plus the platform-wide Total (full
// matching count — the key every other module's list meta exposes). Count
// (items on this page) is kept for backward compatibility.
type ListMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
	Count  int   `json:"count"`
}

// TreeMeta describes a subtree response. Truncated is true when the subtree
// hit the node cap and the flat list is incomplete.
type TreeMeta struct {
	Count     int  `json:"count"`
	Truncated bool `json:"truncated"`
}
