package identity

import (
	"testing"

	"github.com/pyaas/saathi-backend/internal/domain"
)

// TestGranterMayGrant pins the role-grant permission matrix (blueprint §5.2):
// admins are unrestricted, UNION_PRESIDENT covers village/logistics/union
// tiers, SAMITI_ADHYAKSH covers only village workers, everyone else nothing.
func TestGranterMayGrant(t *testing.T) {
	cases := []struct {
		name        string
		granterRole string
		roleCode    string
		want        bool
	}{
		// Admin granters: unrestricted.
		{"super admin grants farmer", domain.RoleSuperAdmin, domain.RoleFarmer, true},
		{"super admin grants super admin", domain.RoleSuperAdmin, domain.RoleSuperAdmin, true},
		{"pcdf admin grants plant operator", domain.RolePCDFAdmin, domain.RolePlantOperator, true},
		{"pcdf admin grants union president", domain.RolePCDFAdmin, domain.RoleUnionPresident, true},

		// UNION_PRESIDENT: village + logistics + union tiers.
		{"union president grants farmer", domain.RoleUnionPresident, domain.RoleFarmer, true},
		{"union president grants sacheev", domain.RoleUnionPresident, domain.RoleSamitiSacheev, true},
		{"union president grants adhyaksh", domain.RoleUnionPresident, domain.RoleSamitiAdhyaksh, true},
		{"union president grants milk tester", domain.RoleUnionPresident, domain.RoleMilkTester, true},
		{"union president grants lrp", domain.RoleUnionPresident, domain.RoleLRP, true},
		{"union president grants ai tech", domain.RoleUnionPresident, domain.RoleAITech, true},
		{"union president grants van rider", domain.RoleUnionPresident, domain.RoleVanRider, true},
		{"union president grants delivery rider", domain.RoleUnionPresident, domain.RoleDeliveryRider, true},
		{"union president grants bmc operator", domain.RoleUnionPresident, domain.RoleBMCOperator, true},
		{"union president grants field supervisor", domain.RoleUnionPresident, domain.RoleUnionFieldSupervisor, true},
		{"union president grants union president", domain.RoleUnionPresident, domain.RoleUnionPresident, true},
		{"union president cannot grant plant operator", domain.RoleUnionPresident, domain.RolePlantOperator, false},
		{"union president cannot grant pcdf admin", domain.RoleUnionPresident, domain.RolePCDFAdmin, false},
		{"union president cannot grant super admin", domain.RoleUnionPresident, domain.RoleSuperAdmin, false},
		{"union president cannot grant veterinarian", domain.RoleUnionPresident, domain.RoleVeterinarian, false},

		// SAMITI_ADHYAKSH: only FARMER / MILK_TESTER / LRP.
		{"adhyaksh grants farmer", domain.RoleSamitiAdhyaksh, domain.RoleFarmer, true},
		{"adhyaksh grants milk tester", domain.RoleSamitiAdhyaksh, domain.RoleMilkTester, true},
		{"adhyaksh grants lrp", domain.RoleSamitiAdhyaksh, domain.RoleLRP, true},
		{"adhyaksh cannot grant sacheev", domain.RoleSamitiAdhyaksh, domain.RoleSamitiSacheev, false},
		{"adhyaksh cannot grant adhyaksh", domain.RoleSamitiAdhyaksh, domain.RoleSamitiAdhyaksh, false},
		{"adhyaksh cannot grant van rider", domain.RoleSamitiAdhyaksh, domain.RoleVanRider, false},
		{"adhyaksh cannot grant union president", domain.RoleSamitiAdhyaksh, domain.RoleUnionPresident, false},

		// Roles outside the granter set grant nothing (RBAC middleware
		// blocks them anyway — the matrix must still deny defensively).
		{"farmer cannot grant farmer", domain.RoleFarmer, domain.RoleFarmer, false},
		{"sacheev cannot grant farmer", domain.RoleSamitiSacheev, domain.RoleFarmer, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := granterMayGrant(tc.granterRole, tc.roleCode); got != tc.want {
				t.Errorf("granterMayGrant(%q, %q) = %v, want %v", tc.granterRole, tc.roleCode, got, tc.want)
			}
		})
	}
}

// TestUpgradedKYCTier pins the tier upgrade rule: verification moves a party
// upward only; parallel tier-1 proofs (FARMER vs STANDARD) never overwrite
// each other.
func TestUpgradedKYCTier(t *testing.T) {
	cases := []struct {
		name      string
		current   string
		requested string
		want      string
	}{
		{"minimal upgrades to farmer", domain.KYCTierMinimal, domain.KYCTierFarmer, domain.KYCTierFarmer},
		{"minimal upgrades to standard", domain.KYCTierMinimal, domain.KYCTierStandard, domain.KYCTierStandard},
		{"farmer keeps farmer on re-verify", domain.KYCTierFarmer, domain.KYCTierFarmer, domain.KYCTierFarmer},
		{"farmer not sideswapped to standard", domain.KYCTierFarmer, domain.KYCTierStandard, domain.KYCTierFarmer},
		{"standard not sideswapped to farmer", domain.KYCTierStandard, domain.KYCTierFarmer, domain.KYCTierStandard},
		{"rider never downgraded to farmer", domain.KYCTierRider, domain.KYCTierFarmer, domain.KYCTierRider},
		{"high never downgraded to standard", domain.KYCTierHigh, domain.KYCTierStandard, domain.KYCTierHigh},
		{"highest never downgraded", domain.KYCTierHighest, domain.KYCTierStandard, domain.KYCTierHighest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upgradedKYCTier(tc.current, tc.requested); got != tc.want {
				t.Errorf("upgradedKYCTier(%q, %q) = %q, want %q", tc.current, tc.requested, got, tc.want)
			}
		})
	}
}

// TestMaskAccount pins the bank-account masking used before persistence.
func TestMaskAccount(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"123456789", "****6789"},
		{"000000001234", "****1234"},
		{"12", "****"},
	}
	for _, tc := range cases {
		if got := maskAccount(tc.in); got != tc.want {
			t.Errorf("maskAccount(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
