package orgs

import (
	"net/http"
	"testing"

	"github.com/pyaas/saathi-backend/internal/domain"
)

// TestValidateHierarchy exercises the full parent-type matrix from
// domain.ValidOrgParent (blueprint §5.1): every child type against no parent
// and against every possible parent type.
func TestValidateHierarchy(t *testing.T) {
	// noParent marks the "no parent_id supplied" case.
	const noParent = ""

	type tc struct {
		name       string
		childType  string
		parentType string
		wantOK     bool
		wantStatus int    // checked only when !wantOK
		wantCode   string // checked only when !wantOK
	}

	cases := []tc{
		// FEDERATION — unique root, never has a parent.
		{"federation as root", domain.OrgTypeFederation, noParent, true, 0, ""},
		{"federation under federation", domain.OrgTypeFederation, domain.OrgTypeFederation, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"federation under union", domain.OrgTypeFederation, domain.OrgTypeMilkUnion, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"federation under plant", domain.OrgTypeFederation, domain.OrgTypeProcessingPlant, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"federation under bmc", domain.OrgTypeFederation, domain.OrgTypeBMC, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"federation under dcs", domain.OrgTypeFederation, domain.OrgTypeDCS, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},

		// MILK_UNION — only under the federation.
		{"union without parent", domain.OrgTypeMilkUnion, noParent, false, http.StatusBadRequest, "ORG_PARENT_REQUIRED"},
		{"union under federation", domain.OrgTypeMilkUnion, domain.OrgTypeFederation, true, 0, ""},
		{"union under union", domain.OrgTypeMilkUnion, domain.OrgTypeMilkUnion, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"union under plant", domain.OrgTypeMilkUnion, domain.OrgTypeProcessingPlant, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"union under bmc", domain.OrgTypeMilkUnion, domain.OrgTypeBMC, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"union under dcs", domain.OrgTypeMilkUnion, domain.OrgTypeDCS, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},

		// PROCESSING_PLANT — under a union or directly under the federation.
		{"plant without parent", domain.OrgTypeProcessingPlant, noParent, false, http.StatusBadRequest, "ORG_PARENT_REQUIRED"},
		{"plant under federation", domain.OrgTypeProcessingPlant, domain.OrgTypeFederation, true, 0, ""},
		{"plant under union", domain.OrgTypeProcessingPlant, domain.OrgTypeMilkUnion, true, 0, ""},
		{"plant under plant", domain.OrgTypeProcessingPlant, domain.OrgTypeProcessingPlant, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"plant under bmc", domain.OrgTypeProcessingPlant, domain.OrgTypeBMC, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"plant under dcs", domain.OrgTypeProcessingPlant, domain.OrgTypeDCS, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},

		// BMC — only under a union.
		{"bmc without parent", domain.OrgTypeBMC, noParent, false, http.StatusBadRequest, "ORG_PARENT_REQUIRED"},
		{"bmc under federation", domain.OrgTypeBMC, domain.OrgTypeFederation, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"bmc under union", domain.OrgTypeBMC, domain.OrgTypeMilkUnion, true, 0, ""},
		{"bmc under plant", domain.OrgTypeBMC, domain.OrgTypeProcessingPlant, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"bmc under bmc", domain.OrgTypeBMC, domain.OrgTypeBMC, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"bmc under dcs", domain.OrgTypeBMC, domain.OrgTypeDCS, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},

		// DCS — under a union or a BMC.
		{"dcs without parent", domain.OrgTypeDCS, noParent, false, http.StatusBadRequest, "ORG_PARENT_REQUIRED"},
		{"dcs under federation", domain.OrgTypeDCS, domain.OrgTypeFederation, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"dcs under union", domain.OrgTypeDCS, domain.OrgTypeMilkUnion, true, 0, ""},
		{"dcs under plant", domain.OrgTypeDCS, domain.OrgTypeProcessingPlant, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},
		{"dcs under bmc", domain.OrgTypeDCS, domain.OrgTypeBMC, true, 0, ""},
		{"dcs under dcs", domain.OrgTypeDCS, domain.OrgTypeDCS, false, http.StatusUnprocessableEntity, "ORG_PARENT_TYPE_INVALID"},

		// Unknown child type.
		{"unknown type", "VILLAGE", noParent, false, http.StatusBadRequest, "INVALID_ORG_TYPE"},
		{"unknown type with parent", "VILLAGE", domain.OrgTypeMilkUnion, false, http.StatusBadRequest, "INVALID_ORG_TYPE"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := validateHierarchy(c.childType, c.parentType)
			if c.wantOK {
				if got != nil {
					t.Fatalf("validateHierarchy(%q, %q) = %v, want nil", c.childType, c.parentType, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("validateHierarchy(%q, %q) = nil, want error code %s", c.childType, c.parentType, c.wantCode)
			}
			if got.Status != c.wantStatus {
				t.Errorf("status = %d, want %d", got.Status, c.wantStatus)
			}
			if got.Code != c.wantCode {
				t.Errorf("code = %s, want %s", got.Code, c.wantCode)
			}
		})
	}
}
