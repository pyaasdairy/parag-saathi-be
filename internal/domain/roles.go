// Package domain holds the pure entity model of Saathi — structs, enums and
// invariants shared by every module. It has no behaviour beyond validation
// and no dependencies outside the standard library and BSON primitives.
// This is the contract layer: modules depend on domain, never the other way
// around.
package domain

// Role codes — the role catalog from blueprint §5.2, grounded in the PCDF
// cooperative constitution (Farmer → Samiti/DCS → Sangh/Union → PCDF).
// A Party may hold many time-bounded, org-scoped RoleAssignments.
//
// The catalog is deliberately open for extension: adding a future role is a
// new constant + AllRoles entry + RequiredKYCTier entry — nothing else.
const (
	// Village tier
	RoleFarmer         = "FARMER"
	RoleSamitiSacheev  = "SAMITI_SACHEEV"  // DCS paid Secretary (UPCS Act §31) — runs the collection console
	RoleSamitiAdhyaksh = "SAMITI_ADHYAKSH" // DCS Chairman/Sabhapati (elected, honorary, §30)
	RoleMilkTester     = "MILK_TESTER"
	RoleLRP            = "LRP" // Local Resource Person (NDDB village extension)
	RoleAITech         = "AI_TECH"

	// Field / organisation tier
	// ORGANISING_MANAGER is the ground-level field worker (Dairy Development
	// Department pattern: "promotes and organises new samitis") who performs
	// doorstep KYC capture and first-level verification.
	RoleOrganisingManager = "ORGANISING_MANAGER"

	// Logistics tier
	RoleVanRider      = "VAN_RIDER"
	RoleDeliveryRider = "DELIVERY_RIDER"

	// Union tier
	RoleBMCOperator          = "BMC_OPERATOR"
	RoleUnionFieldSupervisor = "UNION_FIELD_SUPERVISOR"
	RoleUnionPresident       = "UNION_PRESIDENT"

	// Plant tier
	RolePlantOperator   = "PLANT_OPERATOR"
	RolePlantLabAnalyst = "PLANT_LAB_ANALYST"

	// State / district tier
	RolePCDFAdmin        = "PCDF_ADMIN"
	RoleMissionOfficial  = "MISSION_OFFICIAL"
	RoleDistrictVerifier = "DISTRICT_VERIFIER"

	// Health tier
	RoleVeterinarian = "VETERINARIAN"
	RoleMVUDriver    = "MVU_DRIVER"

	// Consumer tier
	RoleConsumer = "CONSUMER"

	// Platform tier
	RoleSupportAgent = "SUPPORT_AGENT"
	RoleStateAuditor = "STATE_AUDITOR"
	RoleSuperAdmin   = "SUPER_ADMIN"

	// System tier
	RoleServiceAccount = "SERVICE_ACCOUNT"
)

// AllRoles is the closed set of grantable role codes (extend here).
var AllRoles = []string{
	RoleFarmer, RoleSamitiSacheev, RoleSamitiAdhyaksh, RoleMilkTester, RoleLRP, RoleAITech,
	RoleOrganisingManager,
	RoleVanRider, RoleDeliveryRider,
	RoleBMCOperator, RoleUnionFieldSupervisor, RoleUnionPresident,
	RolePlantOperator, RolePlantLabAnalyst,
	RolePCDFAdmin, RoleMissionOfficial, RoleDistrictVerifier,
	RoleVeterinarian, RoleMVUDriver,
	RoleConsumer,
	RoleSupportAgent, RoleStateAuditor, RoleSuperAdmin,
	RoleServiceAccount,
}

var roleSet = func() map[string]struct{} {
	s := make(map[string]struct{}, len(AllRoles))
	for _, r := range AllRoles {
		s[r] = struct{}{}
	}
	return s
}()

// IsValidRole reports whether code is one of the catalog roles.
func IsValidRole(code string) bool {
	_, ok := roleSet[code]
	return ok
}

// KYC assurance tiers (blueprint §4.2). Higher tiers unlock higher-trust roles.
const (
	KYCTierMinimal  = "MINIMAL"  // mobile OTP only (consumer)
	KYCTierFarmer   = "FARMER"   // Aadhaar OTP eKYC or offline XML + penny-drop
	KYCTierStandard = "STANDARD" // Aadhaar eKYC
	KYCTierRider    = "RIDER"    // Aadhaar eKYC + DL/RC
	KYCTierHigh     = "HIGH"     // eKYC + org record / V-CIP for officials
	KYCTierHighest  = "HIGHEST"  // platform admin: MFA + dual-control
	KYCTierService  = "SERVICE"  // machine accounts: mTLS / scoped keys
)

// RequiredKYCTier maps each role to the minimum KYC tier a party must hold
// before a role token for it can be issued (blueprint §5.2 "KYC tier").
// Tier upgrades happen ONLY through the KYC approval workflow: submit →
// PENDING → reviewed by an authorised approver → VERIFIED. There is no
// self-service tier upgrade.
var RequiredKYCTier = map[string]string{
	RoleFarmer:               KYCTierFarmer,
	RoleSamitiSacheev:        KYCTierHigh,
	RoleSamitiAdhyaksh:       KYCTierHigh,
	RoleMilkTester:           KYCTierStandard,
	RoleLRP:                  KYCTierStandard,
	RoleAITech:               KYCTierStandard,
	RoleOrganisingManager:    KYCTierHigh,
	RoleVanRider:             KYCTierRider,
	RoleDeliveryRider:        KYCTierRider,
	RoleBMCOperator:          KYCTierStandard,
	RoleUnionFieldSupervisor: KYCTierHigh,
	RoleUnionPresident:       KYCTierHigh,
	RolePlantOperator:        KYCTierHigh,
	RolePlantLabAnalyst:      KYCTierHigh,
	RolePCDFAdmin:            KYCTierHigh,
	RoleMissionOfficial:      KYCTierHigh,
	RoleDistrictVerifier:     KYCTierHigh,
	RoleVeterinarian:         KYCTierHigh,
	RoleMVUDriver:            KYCTierStandard,
	RoleConsumer:             KYCTierMinimal,
	RoleSupportAgent:         KYCTierHigh,
	RoleStateAuditor:         KYCTierHigh,
	RoleSuperAdmin:           KYCTierHighest,
	RoleServiceAccount:       KYCTierService,
}

// kycRank orders tiers so "tier X satisfies requirement Y" is a comparison.
var kycRank = map[string]int{
	KYCTierMinimal:  0,
	KYCTierFarmer:   1,
	KYCTierStandard: 1, // Farmer and Standard are parallel tier-1 proofs
	KYCTierRider:    2,
	KYCTierHigh:     3,
	KYCTierHighest:  4,
	KYCTierService:  4,
}

// KYCTierSatisfies reports whether a party at tier `have` meets `need`.
// Parallel tier-1 proofs (FARMER vs STANDARD) satisfy each other.
func KYCTierSatisfies(have, need string) bool {
	h, okH := kycRank[have]
	n, okN := kycRank[need]
	if !okH || !okN {
		return false
	}
	return h >= n
}

// KYCApprovableTiers maps an approver ROLE to the KYC tiers it may approve.
// Ground staff clear the field tiers; only federation admins clear HIGH;
// only SUPER_ADMIN clears HIGHEST. Extend when new approver roles arrive.
var KYCApprovableTiers = map[string][]string{
	RoleOrganisingManager: {KYCTierMinimal, KYCTierFarmer, KYCTierStandard, KYCTierRider},
	RoleDistrictVerifier:  {KYCTierMinimal, KYCTierFarmer, KYCTierStandard, KYCTierRider},
	RolePCDFAdmin:         {KYCTierMinimal, KYCTierFarmer, KYCTierStandard, KYCTierRider, KYCTierHigh},
	RoleSuperAdmin:        {KYCTierMinimal, KYCTierFarmer, KYCTierStandard, KYCTierRider, KYCTierHigh, KYCTierHighest, KYCTierService},
}

// CanApproveKYCTier reports whether approverRole may approve a KYC record
// requesting the given tier.
func CanApproveKYCTier(approverRole, tier string) bool {
	for _, t := range KYCApprovableTiers[approverRole] {
		if t == tier {
			return true
		}
	}
	return false
}
