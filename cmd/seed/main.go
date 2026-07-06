// Command seed loads idempotent baseline data for development and demos:
// the cooperative org tree (PCDF → union → plant/BMC/DCS), one party per MVP
// role, an active rate chart, and sample animals. Fixed IDs + upserts → safe
// to run any number of times.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// Fixed IDs so re-seeding never duplicates and smoke tests can reference them.
const (
	OrgFederation = "org-federation-pcdf"
	OrgUnionLKO   = "org-union-lucknow"
	OrgPlantLKO   = "org-plant-lucknow-01"
	OrgBMCMali    = "org-bmc-malihabad"
	OrgDCSKasmandi = "org-dcs-kasmandi-kalan"
	OrgDCSRahimabad = "org-dcs-rahimabad"

	PartySuperAdmin  = "party-super-admin"
	PartySacheev     = "party-sacheev-ramesh"
	PartyAdhyaksh    = "party-adhyaksh-sunita"
	PartyTester      = "party-tester-anil"
	PartyFarmerMahesh = "party-farmer-mahesh"
	PartyFarmerGeeta  = "party-farmer-geeta"
	PartyVanRider    = "party-vanrider-salim"
	PartyBMCOperator = "party-bmcop-vikas"
	PartyPlantOp     = "party-plantop-rajeev"
	PartyLabAnalyst  = "party-lab-priya"
	PartyVet         = "party-vet-dr-verma"
	PartyConsumer    = "party-consumer-arjun"

	RateChartDefault = "ratechart-union-lko-2026"

	AnimalGomti = "animal-gomti"
	AnimalShyama = "animal-shyama"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed failed:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, db, err := mongodb.Connect(ctx, cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		return err
	}
	defer client.Disconnect(context.Background())

	if err := mongodb.EnsureIndexes(ctx, db); err != nil {
		return err
	}

	now := time.Now().UTC()

	// ── Org tree ────────────────────────────────────────────────────────────
	orgs := []domain.OrgUnit{
		{ID: OrgFederation, Type: domain.OrgTypeFederation, Name: "PCDF / Parag (Uttar Pradesh)", Code: "PCDF", Path: []string{}, State: "Uttar Pradesh", Active: true},
		{ID: OrgUnionLKO, Type: domain.OrgTypeMilkUnion, Name: "Lucknow Dugdh Utpadak Sahakari Sangh", Code: "UNION-LKO", ParentID: OrgFederation, Path: []string{OrgFederation}, District: "Lucknow", State: "Uttar Pradesh", Active: true},
		{ID: OrgPlantLKO, Type: domain.OrgTypeProcessingPlant, Name: "Parag Dairy Plant, Lucknow", Code: "PLANT-LKO-01", ParentID: OrgUnionLKO, Path: []string{OrgFederation, OrgUnionLKO}, District: "Lucknow", State: "Uttar Pradesh", Active: true},
		{ID: OrgBMCMali, Type: domain.OrgTypeBMC, Name: "BMC Malihabad", Code: "BMC-LKO-007", ParentID: OrgUnionLKO, Path: []string{OrgFederation, OrgUnionLKO}, District: "Lucknow", State: "Uttar Pradesh", Active: true},
		{ID: OrgDCSKasmandi, Type: domain.OrgTypeDCS, Name: "DCS Kasmandi Kalan", Code: "DCS-01842", ParentID: OrgBMCMali, Path: []string{OrgFederation, OrgUnionLKO, OrgBMCMali}, District: "Lucknow", State: "Uttar Pradesh", Active: true},
		{ID: OrgDCSRahimabad, Type: domain.OrgTypeDCS, Name: "DCS Rahimabad", Code: "DCS-01907", ParentID: OrgBMCMali, Path: []string{OrgFederation, OrgUnionLKO, OrgBMCMali}, District: "Lucknow", State: "Uttar Pradesh", Active: true},
	}
	for _, o := range orgs {
		o.CreatedAt, o.UpdatedAt = now, now
		if err := upsert(ctx, db.Collection(mongodb.CollOrgUnits), o.ID, o); err != nil {
			return fmt.Errorf("org %s: %w", o.Code, err)
		}
	}

	// ── Parties (one per MVP role) ──────────────────────────────────────────
	type seedParty struct {
		p    domain.Party
		role string
		org  string
	}
	parties := []seedParty{
		{domain.Party{ID: PartySuperAdmin, Phone: cfg.SeedAdminPhone, FullName: "Platform Admin", PreferredLanguage: "en", KYCTier: domain.KYCTierHighest}, domain.RoleSuperAdmin, OrgFederation},
		{domain.Party{ID: PartySacheev, Phone: "9000000001", FullName: "Ramesh Kumar", PreferredLanguage: "hi", KYCTier: domain.KYCTierHigh}, domain.RoleSamitiSacheev, OrgDCSKasmandi},
		{domain.Party{ID: PartyAdhyaksh, Phone: "9000000002", FullName: "Sunita Devi", PreferredLanguage: "hi", KYCTier: domain.KYCTierHigh}, domain.RoleSamitiAdhyaksh, OrgDCSKasmandi},
		{domain.Party{ID: PartyTester, Phone: "9000000003", FullName: "Anil Verma", PreferredLanguage: "hi", KYCTier: domain.KYCTierStandard}, domain.RoleMilkTester, OrgDCSKasmandi},
		{domain.Party{ID: PartyFarmerMahesh, Phone: "9000000011", FullName: "Mahesh Yadav", PreferredLanguage: "hi", KYCTier: domain.KYCTierFarmer}, domain.RoleFarmer, OrgDCSKasmandi},
		{domain.Party{ID: PartyFarmerGeeta, Phone: "9000000012", FullName: "Geeta Devi", PreferredLanguage: "hi", KYCTier: domain.KYCTierFarmer}, domain.RoleFarmer, OrgDCSKasmandi},
		{domain.Party{ID: PartyVanRider, Phone: "9000000021", FullName: "Salim Khan", PreferredLanguage: "hi", KYCTier: domain.KYCTierRider}, domain.RoleVanRider, OrgUnionLKO},
		{domain.Party{ID: PartyBMCOperator, Phone: "9000000031", FullName: "Vikas Singh", PreferredLanguage: "hi", KYCTier: domain.KYCTierStandard}, domain.RoleBMCOperator, OrgBMCMali},
		{domain.Party{ID: PartyPlantOp, Phone: "9000000041", FullName: "Rajeev Ranjan", PreferredLanguage: "hi", KYCTier: domain.KYCTierHigh}, domain.RolePlantOperator, OrgPlantLKO},
		{domain.Party{ID: PartyLabAnalyst, Phone: "9000000042", FullName: "Priya Sharma", PreferredLanguage: "en", KYCTier: domain.KYCTierHigh}, domain.RolePlantLabAnalyst, OrgPlantLKO},
		{domain.Party{ID: PartyVet, Phone: "9000000061", FullName: "Dr. A. K. Verma", PreferredLanguage: "hi", KYCTier: domain.KYCTierHigh}, domain.RoleVeterinarian, OrgUnionLKO},
		{domain.Party{ID: PartyConsumer, Phone: "9000000051", FullName: "Arjun Mehta", PreferredLanguage: "en", KYCTier: domain.KYCTierMinimal}, domain.RoleConsumer, OrgFederation},
	}
	for _, sp := range parties {
		sp.p.Status = domain.PartyStatusActive
		sp.p.CreatedAt, sp.p.UpdatedAt = now, now
		if err := upsert(ctx, db.Collection(mongodb.CollParties), sp.p.ID, sp.p); err != nil {
			return fmt.Errorf("party %s: %w", sp.p.FullName, err)
		}
		ra := domain.RoleAssignment{
			ID:        "ra-" + sp.p.ID + "-" + sp.role,
			PartyID:   sp.p.ID,
			RoleCode:  sp.role,
			OrgUnitID: sp.org,
			GrantedBy: PartySuperAdmin,
			ValidFrom: now.Add(-24 * time.Hour),
			Status:    domain.RoleAssignmentActive,
			CreatedAt: now,
		}
		if err := upsert(ctx, db.Collection(mongodb.CollRoleAssignments), ra.ID, ra); err != nil {
			return fmt.Errorf("role %s→%s: %w", sp.p.FullName, sp.role, err)
		}
	}

	// ── Rate chart (union-wide) ─────────────────────────────────────────────
	rc := domain.RateChart{
		ID: RateChartDefault, OrgUnitID: OrgUnionLKO, Name: "Union LKO standard 2026",
		BaseRatePerLitre: 8.0, FatRatePerPoint: 5.5, SNFRatePerPoint: 1.0,
		EffectiveFrom: now.Add(-30 * 24 * time.Hour), Active: true,
		CreatedBy: PartySuperAdmin, CreatedAt: now,
	}
	if err := upsert(ctx, db.Collection(mongodb.CollRateCharts), rc.ID, rc); err != nil {
		return fmt.Errorf("rate chart: %w", err)
	}

	// ── Sample animals (Pashu Aadhaar keyed) ────────────────────────────────
	animals := []domain.Animal{
		{ID: AnimalGomti, PashuAadhaar: "356729481027", OwnerPartyID: PartyFarmerMahesh, DCSID: OrgDCSKasmandi, Species: "COW", Breed: "Sahiwal", Sex: "F", LactationStatus: "LACTATING", Status: domain.AnimalStatusActive},
		{ID: AnimalShyama, PashuAadhaar: "356729481034", OwnerPartyID: PartyFarmerGeeta, DCSID: OrgDCSKasmandi, Species: "BUFFALO", Breed: "Murrah", Sex: "F", LactationStatus: "LACTATING", Status: domain.AnimalStatusActive},
	}
	for _, a := range animals {
		a.CreatedAt, a.UpdatedAt = now, now
		if err := upsert(ctx, db.Collection(mongodb.CollAnimals), a.ID, a); err != nil {
			return fmt.Errorf("animal %s: %w", a.ID, err)
		}
	}

	fmt.Println("✔ seed complete:")
	fmt.Printf("  org units: %d | parties+roles: %d | rate chart: 1 | animals: %d\n", len(orgs), len(parties), len(animals))
	fmt.Printf("  super admin phone: %s (OTP_DEV_MODE returns the OTP in the login response)\n", cfg.SeedAdminPhone)
	fmt.Println("  demo phones: sacheev 9000000001 · adhyaksh 9000000002 · farmer 9000000011 · rider 9000000021")
	fmt.Println("               bmc-op 9000000031 · plant-op 9000000041 · lab 9000000042 · consumer 9000000051")
	return nil
}

// upsert replaces the document with _id=id (insert if missing).
func upsert(ctx context.Context, coll *mongo.Collection, id string, doc any) error {
	_, err := coll.ReplaceOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		doc,
		options.Replace().SetUpsert(true),
	)
	return err
}
