package orgs

import "time"

// CreateOrgRequest is the body of POST /orgs.
type CreateOrgRequest struct {
	Type     string   `json:"type"`
	Name     string   `json:"name"`
	Code     string   `json:"code"`
	ParentID string   `json:"parent_id,omitempty"`
	District string   `json:"district,omitempty"`
	State    string   `json:"state,omitempty"`
	GeoLat   *float64 `json:"geo_lat,omitempty"`
	GeoLng   *float64 `json:"geo_lng,omitempty"`
}

// UpdateOrgRequest is the body of PATCH /orgs/{id}. Pointer fields
// distinguish "absent" from "set to zero value". Type and ParentID are
// decoded solely to reject hierarchy moves, which are unsupported in v1.
type UpdateOrgRequest struct {
	Name     *string  `json:"name"`
	District *string  `json:"district"`
	Active   *bool    `json:"active"`
	GeoLat   *float64 `json:"geo_lat"`
	GeoLng   *float64 `json:"geo_lng"`
	Type     *string  `json:"type"`
	ParentID *string  `json:"parent_id"`
}

// Member is one row of GET /orgs/{id}/members: an ACTIVE role assignment at
// the org joined in memory with its party's identity fields.
type Member struct {
	PartyID          string     `json:"party_id"`
	Phone            string     `json:"phone,omitempty"`
	FullName         string     `json:"full_name,omitempty"`
	KYCTier          string     `json:"kyc_tier,omitempty"`
	RoleCode         string     `json:"role_code"`
	RoleAssignmentID string     `json:"role_assignment_id"`
	ValidFrom        time.Time  `json:"valid_from"`
	ValidTo          *time.Time `json:"valid_to,omitempty"`
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
