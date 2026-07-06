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

// TestCanApproveKYCTier pins the approver-role → approvable-tier authority
// map that gates KYC approve/reject (blueprint §5.2). Ground staff clear the
// field tiers, PCDF_ADMIN additionally clears HIGH, and SUPER_ADMIN clears
// everything — but SUPER_ADMIN still passes *through* the map, it is not a
// short-circuit.
func TestCanApproveKYCTier(t *testing.T) {
	cases := []struct {
		name         string
		approverRole string
		tier         string
		want         bool
	}{
		// ORGANISING_MANAGER: field tiers only, never HIGH/HIGHEST.
		{"om approves farmer", domain.RoleOrganisingManager, domain.KYCTierFarmer, true},
		{"om approves standard", domain.RoleOrganisingManager, domain.KYCTierStandard, true},
		{"om approves rider", domain.RoleOrganisingManager, domain.KYCTierRider, true},
		{"om cannot approve high", domain.RoleOrganisingManager, domain.KYCTierHigh, false},
		{"om cannot approve highest", domain.RoleOrganisingManager, domain.KYCTierHighest, false},

		// DISTRICT_VERIFIER: same field-tier ceiling as ground staff.
		{"district verifier approves rider", domain.RoleDistrictVerifier, domain.KYCTierRider, true},
		{"district verifier cannot approve high", domain.RoleDistrictVerifier, domain.KYCTierHigh, false},

		// PCDF_ADMIN: field tiers + HIGH, never HIGHEST.
		{"pcdf admin approves high", domain.RolePCDFAdmin, domain.KYCTierHigh, true},
		{"pcdf admin approves farmer", domain.RolePCDFAdmin, domain.KYCTierFarmer, true},
		{"pcdf admin cannot approve highest", domain.RolePCDFAdmin, domain.KYCTierHighest, false},

		// SUPER_ADMIN: everything, but still via the map.
		{"super admin approves high", domain.RoleSuperAdmin, domain.KYCTierHigh, true},
		{"super admin approves highest", domain.RoleSuperAdmin, domain.KYCTierHighest, true},
		{"super admin approves service", domain.RoleSuperAdmin, domain.KYCTierService, true},

		// Roles outside the reviewer set approve nothing (defence in depth —
		// the RBAC middleware already blocks them at the route).
		{"farmer approves nothing", domain.RoleFarmer, domain.KYCTierFarmer, false},
		{"union president approves nothing", domain.RoleUnionPresident, domain.KYCTierFarmer, false},

		// An unknown tier is never approvable, even for SUPER_ADMIN.
		{"unknown tier never approvable", domain.RoleSuperAdmin, "GALAXY", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.CanApproveKYCTier(tc.approverRole, tc.tier); got != tc.want {
				t.Errorf("CanApproveKYCTier(%q, %q) = %v, want %v", tc.approverRole, tc.tier, got, tc.want)
			}
		})
	}
}

// TestApproveTierUpgrade pins the PENDING→VERIFIED party-tier upgrade rule the
// approve flow applies: the party's tier only ever moves UPWARD to the
// approved requested tier, and parallel tier-1 proofs (FARMER vs STANDARD)
// never overwrite one another. This mirrors the newTier computation in
// service.approveKYC.
func TestApproveTierUpgrade(t *testing.T) {
	cases := []struct {
		name        string
		currentTier string // party.kyc_tier before approval
		approved    string // record.requested_tier being approved
		wantTier    string // party.kyc_tier after approval
	}{
		{"minimal party approved for farmer upgrades", domain.KYCTierMinimal, domain.KYCTierFarmer, domain.KYCTierFarmer},
		{"minimal party approved for high upgrades", domain.KYCTierMinimal, domain.KYCTierHigh, domain.KYCTierHigh},
		{"farmer party approved for rider upgrades", domain.KYCTierFarmer, domain.KYCTierRider, domain.KYCTierRider},
		{"farmer re-approved for farmer stays", domain.KYCTierFarmer, domain.KYCTierFarmer, domain.KYCTierFarmer},
		{"farmer approved for standard stays (parallel)", domain.KYCTierFarmer, domain.KYCTierStandard, domain.KYCTierFarmer},
		{"high party approved for standard never downgrades", domain.KYCTierHigh, domain.KYCTierStandard, domain.KYCTierHigh},
		{"rider party approved for farmer never downgrades", domain.KYCTierRider, domain.KYCTierFarmer, domain.KYCTierRider},
		{"highest party approved for high never downgrades", domain.KYCTierHighest, domain.KYCTierHigh, domain.KYCTierHighest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := upgradedKYCTier(tc.currentTier, tc.approved)
			if got != tc.wantTier {
				t.Errorf("upgradedKYCTier(%q, %q) = %q, want %q", tc.currentTier, tc.approved, got, tc.wantTier)
			}
			// Upgrade is upward-only: the result must satisfy the prior tier.
			if !domain.KYCTierSatisfies(got, tc.currentTier) {
				t.Errorf("post-approval tier %q must still satisfy prior tier %q", got, tc.currentTier)
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
