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
	// ONBOARDING_EXECUTIVE is the mobile-app onboarding worker the field client
	// uses for the same doorstep KYC / assisted-onboarding job as
	// ORGANISING_MANAGER. Both are valid onboarding/review roles; the app codes
	// this one, the backend historically coded the other.
	RoleOnboardingExecutive = "ONBOARDING_EXECUTIVE"

	// Logistics tier
	RoleVanRider      = "VAN_RIDER"
	RoleDeliveryRider = "DELIVERY_RIDER"

	// Retail / commerce tier (Phase-3 store module; dormant behind a flag but a
	// grantable role so the store console can be assigned).
	RoleStoreManager = "STORE_MANAGER"

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
	RoleOrganisingManager, RoleOnboardingExecutive,
	RoleVanRider, RoleDeliveryRider, RoleStoreManager,
	RoleBMCOperator, RoleUnionFieldSupervisor, RoleUnionPresident,
	RolePlantOperator, RolePlantLabAnalyst,
	RolePCDFAdmin, RoleMissionOfficial, RoleDistrictVerifier,
	RoleVeterinarian, RoleMVUDriver,
	RoleConsumer,
	RoleSupportAgent, RoleStateAuditor, RoleSuperAdmin,
	RoleServiceAccount,
}

// OnboardingReviewerRoles are the roles authorised to run the KYC review /
// assisted-onboarding console. Both the historical ORGANISING_MANAGER and the
// app-coded ONBOARDING_EXECUTIVE qualify, plus the district/federation tiers.
var OnboardingReviewerRoles = []string{
	RoleOnboardingExecutive, RoleOrganisingManager, RoleDistrictVerifier, RolePCDFAdmin, RoleSuperAdmin,
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
	KYCTierMTLS     = "MTLS"     // alias the field app uses for SERVICE-tier machine accounts
)

// RequiredKYCTier maps each role to the minimum KYC tier a party must hold
// before a role token for it can be issued (blueprint §5.2 "KYC tier").
// Tier upgrades happen through an AUTHORISED action, never self-service: either
// the KYC approval workflow (submit → PENDING → reviewed → VERIFIED) OR an admin
// role grant, which per Developer Note §2 ("a person receives the modules their
// role grants") vouches the party up to the granted role's required tier.
var RequiredKYCTier = map[string]string{
	RoleFarmer:               KYCTierFarmer,
	RoleSamitiSacheev:        KYCTierHigh,
	RoleSamitiAdhyaksh:       KYCTierHigh,
	RoleMilkTester:           KYCTierStandard,
	RoleLRP:                  KYCTierStandard,
	RoleAITech:               KYCTierStandard,
	RoleOrganisingManager:    KYCTierHigh,
	RoleOnboardingExecutive:  KYCTierHigh,
	RoleVanRider:             KYCTierRider,
	RoleDeliveryRider:        KYCTierRider,
	RoleStoreManager:         KYCTierStandard,
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
	KYCTierMTLS:     4, // alias of SERVICE
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
	RoleOrganisingManager:   {KYCTierMinimal, KYCTierFarmer, KYCTierStandard, KYCTierRider},
	RoleOnboardingExecutive: {KYCTierMinimal, KYCTierFarmer, KYCTierStandard, KYCTierRider},
	RoleDistrictVerifier:    {KYCTierMinimal, KYCTierFarmer, KYCTierStandard, KYCTierRider},
	RolePCDFAdmin:           {KYCTierMinimal, KYCTierFarmer, KYCTierStandard, KYCTierRider, KYCTierHigh},
	RoleSuperAdmin:          {KYCTierMinimal, KYCTierFarmer, KYCTierStandard, KYCTierRider, KYCTierHigh, KYCTierHighest, KYCTierService},
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

// RoleAllowedOrgTypes maps each role to the org-unit type(s) at which a
// RoleAssignment for it is meaningful. A role granted at the wrong org type
// mints a token that role-selects fine but lands on an empty/broken console
// (its scoped reads resolve to the wrong tier of the hierarchy), so grants are
// validated against this map (blueprint §5.1/§5.2). Roles absent from the map
// (CONSUMER, SERVICE_ACCOUNT) carry no org-type restriction — CONSUMER may hold
// anywhere, machine accounts are provisioned out-of-band.
var RoleAllowedOrgTypes = map[string][]string{
	// Village tier — the DCS/samiti console.
	RoleFarmer:         {OrgTypeDCS},
	RoleSamitiSacheev:  {OrgTypeDCS},
	RoleSamitiAdhyaksh: {OrgTypeDCS},
	RoleMilkTester:     {OrgTypeDCS},
	RoleLRP:            {OrgTypeDCS},
	RoleAITech:         {OrgTypeDCS},

	// BMC tier.
	RoleBMCOperator: {OrgTypeBMC},

	// Plant tier.
	RolePlantOperator:   {OrgTypeProcessingPlant},
	RolePlantLabAnalyst: {OrgTypeProcessingPlant},

	// Union / field / logistics / health tier — rooted at the MILK_UNION.
	RoleVanRider:             {OrgTypeMilkUnion},
	RoleUnionFieldSupervisor: {OrgTypeMilkUnion},
	RoleUnionPresident:       {OrgTypeMilkUnion},
	RoleOrganisingManager:    {OrgTypeMilkUnion},
	RoleOnboardingExecutive:  {OrgTypeMilkUnion},
	RoleVeterinarian:         {OrgTypeMilkUnion},
	RoleMVUDriver:            {OrgTypeMilkUnion},

	// Consumer last-mile tier — a Parag STORE (retail/delivery hub). The store
	// manager runs the hub; the delivery rider is assigned to it. (No existing
	// assignments — safe to scope to STORE.)
	RoleStoreManager:  {OrgTypeStore},
	RoleDeliveryRider: {OrgTypeStore},

	// State / federation apex tier.
	RolePCDFAdmin:        {OrgTypeFederation},
	RoleMissionOfficial:  {OrgTypeFederation},
	RoleDistrictVerifier: {OrgTypeFederation},
	RoleSuperAdmin:       {OrgTypeFederation},
	RoleStateAuditor:     {OrgTypeFederation},
	RoleSupportAgent:     {OrgTypeFederation},
}

// RoleAllowedAtOrgType reports whether a role may be assigned at the given
// org-unit type. Roles with no entry in RoleAllowedOrgTypes are unrestricted.
func RoleAllowedAtOrgType(roleCode, orgType string) bool {
	allowed, ok := RoleAllowedOrgTypes[roleCode]
	if !ok {
		return true
	}
	for _, t := range allowed {
		if t == orgType {
			return true
		}
	}
	return false
}
