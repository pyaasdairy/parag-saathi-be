// Command seed loads idempotent baseline data for development and demos:
// the cooperative org tree (PCDF → union → plant/BMC/DCS per the PCDF
// constitution), one party per MVP role, an active rate chart, sample
// animals, education-hub content, and the demo product master.
//
// ID scheme: `_id` is always a Mongo-generated ObjectID. Seeding is
// idempotent via NATURAL business keys (org `code`, party `phone`, animal
// `pashu_aadhaar`) — find-or-insert on the key, then wire relations with the
// returned ObjectIDs. Re-running never duplicates and never rewrites.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
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

	// ── Org tree (find-or-insert by unique `code`) ──────────────────────────
	federation, err := upsertOrg(ctx, db, domain.OrgUnit{
		Type: domain.OrgTypeFederation, Name: "PCDF / Parag (Uttar Pradesh)", NameHi: "पीसीडीएफ / पराग (उत्तर प्रदेश)", Code: "PCDF",
		Path: []primitive.ObjectID{}, State: "Uttar Pradesh", Active: true, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	union, err := upsertOrg(ctx, db, domain.OrgUnit{
		Type: domain.OrgTypeMilkUnion, Name: "Lucknow Dugdh Utpadak Sahkari Sangh", NameHi: "लखनऊ दुग्ध उत्पादक सहकारी संघ", Code: "UNION-LKO",
		ParentID: &federation.ID, Path: []primitive.ObjectID{federation.ID},
		District: "Lucknow", State: "Uttar Pradesh", Active: true, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	plant, err := upsertOrg(ctx, db, domain.OrgUnit{
		Type: domain.OrgTypeProcessingPlant, Name: "Parag Dairy Plant, Lucknow", NameHi: "पराग डेयरी प्लांट, लखनऊ", Code: "PLANT-LKO-01",
		ParentID: &union.ID, Path: []primitive.ObjectID{federation.ID, union.ID},
		District: "Lucknow", State: "Uttar Pradesh", Active: true, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	bmc, err := upsertOrg(ctx, db, domain.OrgUnit{
		Type: domain.OrgTypeBMC, Name: "BMC Malihabad", NameHi: "बीएमसी मलिहाबाद", Code: "BMC-LKO-007",
		ParentID: &union.ID, Path: []primitive.ObjectID{federation.ID, union.ID},
		District: "Lucknow", State: "Uttar Pradesh", Active: true, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	dcs1, err := upsertOrg(ctx, db, domain.OrgUnit{
		Type: domain.OrgTypeDCS, Name: "DCS Kasmandi Kalan", NameHi: "डीसीएस कसमंडी कलां", Code: "DCS-01842",
		ParentID: &bmc.ID, Path: []primitive.ObjectID{federation.ID, union.ID, bmc.ID},
		Village: "Kasmandi Kalan", District: "Lucknow", State: "Uttar Pradesh", Active: true, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	dcs2, err := upsertOrg(ctx, db, domain.OrgUnit{
		Type: domain.OrgTypeDCS, Name: "DCS Rahimabad", NameHi: "डीसीएस रहीमाबाद", Code: "DCS-01907",
		ParentID: &bmc.ID, Path: []primitive.ObjectID{federation.ID, union.ID, bmc.ID},
		Village: "Rahimabad", District: "Lucknow", State: "Uttar Pradesh", Active: true, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	orgs := []*domain.OrgUnit{federation, union, plant, bmc, dcs1, dcs2}

	// ── Parties (find-or-insert by unique `phone`) + role assignments ───────
	type seedParty struct {
		phone, name, lang, tier, role string
		org                           primitive.ObjectID
	}
	seeds := []seedParty{
		{cfg.SeedAdminPhone, "Platform Admin", "en", domain.KYCTierHighest, domain.RoleSuperAdmin, federation.ID},
		{"9000000001", "Ramesh Kumar", "hi", domain.KYCTierHigh, domain.RoleSamitiSacheev, dcs1.ID},
		{"9000000002", "Sunita Devi", "hi", domain.KYCTierHigh, domain.RoleSamitiAdhyaksh, dcs1.ID},
		{"9000000003", "Anil Verma", "hi", domain.KYCTierStandard, domain.RoleMilkTester, dcs1.ID},
		{"9000000011", "Mahesh Yadav", "hi", domain.KYCTierFarmer, domain.RoleFarmer, dcs1.ID},
		{"9000000012", "Geeta Devi", "hi", domain.KYCTierFarmer, domain.RoleFarmer, dcs1.ID},
		{"9000000021", "Salim Khan", "hi", domain.KYCTierRider, domain.RoleVanRider, union.ID},
		{"9000000031", "Vikas Singh", "hi", domain.KYCTierStandard, domain.RoleBMCOperator, bmc.ID},
		{"9000000041", "Rajeev Ranjan", "hi", domain.KYCTierHigh, domain.RolePlantOperator, plant.ID},
		{"9000000042", "Priya Sharma", "en", domain.KYCTierHigh, domain.RolePlantLabAnalyst, plant.ID},
		{"9000000061", "Dr. A. K. Verma", "hi", domain.KYCTierHigh, domain.RoleVeterinarian, union.ID},
		// Organising Manager — the ground-level field worker who captures and
		// first-level-approves KYC (Dairy Development Department pattern).
		{"9000000071", "Kavita Nishad", "hi", domain.KYCTierHigh, domain.RoleOrganisingManager, union.ID},
		{"9000000051", "Arjun Mehta", "en", domain.KYCTierMinimal, domain.RoleConsumer, federation.ID},
	}

	var mahesh, geeta, sacheev primitive.ObjectID
	for _, sp := range seeds {
		party, err := upsertParty(ctx, db, domain.Party{
			Phone: sp.phone, FullName: sp.name, PreferredLanguage: sp.lang,
			KYCTier: sp.tier, Status: domain.PartyStatusActive, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("party %s: %w", sp.name, err)
		}
		if err := upsertRoleAssignment(ctx, db, domain.RoleAssignment{
			PartyID: party.ID, RoleCode: sp.role, OrgUnitID: sp.org,
			ValidFrom: now.Add(-24 * time.Hour), Status: domain.RoleAssignmentActive, CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("role %s→%s: %w", sp.name, sp.role, err)
		}
		switch sp.phone {
		case "9000000001":
			sacheev = party.ID
		case "9000000011":
			mahesh = party.ID
		case "9000000012":
			geeta = party.ID
		}
		fmt.Printf("  party %-16s %-22s %s\n", sp.phone, sp.role, party.ID.Hex())
	}

	// Multi-role reality: the Sacheev IS ALSO a farmer of the same society
	// (blueprint §4.1 — one phone, many roles). Grant Ramesh an additional
	// FARMER assignment so the login role-picker (farmer + sachiv) is exercised.
	if !sacheev.IsZero() {
		if err := upsertRoleAssignment(ctx, db, domain.RoleAssignment{
			PartyID: sacheev, RoleCode: domain.RoleFarmer, OrgUnitID: dcs1.ID,
			ValidFrom: now.Add(-24 * time.Hour), Status: domain.RoleAssignmentActive, CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("multi-role farmer grant to sacheev: %w", err)
		}
		fmt.Println("  multi-role: 9000000001 (Ramesh) is SAMITI_SACHEEV + FARMER @ DCS-01842 → login shows role picker")
	}

	// Verified bank accounts for the producing farmers so settlement can
	// actually pay them (payouts require a reviewer-VERIFIED bank; §18-B).
	for _, farmer := range []primitive.ObjectID{mahesh, geeta} {
		if farmer.IsZero() {
			continue
		}
		last4 := farmer.Hex()[len(farmer.Hex())-4:]
		if err := upsertByFilter(ctx, db.Collection(mongodb.CollKYCRecords),
			bson.D{{Key: "party_id", Value: farmer}, {Key: "bank_verified", Value: true}},
			domain.KYCRecord{
				PartyID: farmer, RequestedTier: domain.KYCTierFarmer, Status: domain.KYCStatusVerified,
				BankAccountMasked: "****" + last4, BankIFSC: "SBIN0001234", BankVerified: true,
				BankNameMatch: 0.95, VerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
			return fmt.Errorf("verified bank KYC for farmer %s: %w", farmer.Hex(), err)
		}
	}

	// ── Rate chart (find-or-insert by org+name) ─────────────────────────────
	admin, err := upsertParty(ctx, db, domain.Party{Phone: cfg.SeedAdminPhone, Status: domain.PartyStatusActive, KYCTier: domain.KYCTierHighest, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return err
	}
	if err := upsertByFilter(ctx, db.Collection(mongodb.CollRateCharts),
		bson.D{{Key: "org_unit_id", Value: union.ID}, {Key: "name", Value: "Union LKO standard 2026"}},
		domain.RateChart{
			OrgUnitID: union.ID, Name: "Union LKO standard 2026", Version: "LKO-2026-06",
			BaseRatePerLitre: 8.0, FatRatePerPoint: 5.5, SNFRatePerPoint: 1.0,
			EffectiveFrom: now.Add(-30 * 24 * time.Hour), Active: true,
			CreatedBy: admin.ID, CreatedAt: now,
		}); err != nil {
		return fmt.Errorf("rate chart: %w", err)
	}

	// ── Product master (find-or-insert by unique sku) — backs GET /products
	// and the product-lot mint's product_id derivation. ──────────────────────
	products := []domain.Product{
		{SKU: "PARAG-TM-500", Name: "Parag Toned Milk 500ml", NameHi: "पराग टोन्ड दूध 500ml", Category: "MILK", UnitSize: "500ml", MRP: 27, ShelfLifeDays: 4, Active: true, CreatedAt: now, UpdatedAt: now},
		{SKU: "PARAG-FCM-1L", Name: "Parag Full Cream Milk 1L", NameHi: "पराग फुल क्रीम दूध 1L", Category: "MILK", UnitSize: "1L", MRP: 66, ShelfLifeDays: 3, Active: true, CreatedAt: now, UpdatedAt: now},
		{SKU: "PARAG-DAHI-400", Name: "Parag Dahi 400g", NameHi: "पराग दही 400g", Category: "DAHI", UnitSize: "400g", MRP: 45, ShelfLifeDays: 12, Active: true, CreatedAt: now, UpdatedAt: now},
	}
	for _, p := range products {
		if err := upsertByFilter(ctx, db.Collection(mongodb.CollProducts),
			bson.D{{Key: "sku", Value: p.SKU}}, p); err != nil {
			return fmt.Errorf("product %s: %w", p.SKU, err)
		}
	}

	// ── Sample animals (find-or-insert by unique pashu_aadhaar) ─────────────
	animals := []domain.Animal{
		{PashuAadhaar: "356729481027", OwnerPartyID: mahesh, DCSID: dcs1.ID, Species: "COW", Breed: "Sahiwal", Sex: "F", LactationStatus: "LACTATING", Status: domain.AnimalStatusActive, CreatedAt: now, UpdatedAt: now},
		{PashuAadhaar: "356729481034", OwnerPartyID: geeta, DCSID: dcs1.ID, Species: "BUFFALO", Breed: "Murrah", Sex: "F", LactationStatus: "LACTATING", Status: domain.AnimalStatusActive, CreatedAt: now, UpdatedAt: now},
	}
	for _, a := range animals {
		if err := upsertByFilter(ctx, db.Collection(mongodb.CollAnimals),
			bson.D{{Key: "pashu_aadhaar", Value: a.PashuAadhaar}}, a); err != nil {
			return fmt.Errorf("animal %s: %w", a.PashuAadhaar, err)
		}
	}

	// ── Education hub content (find-or-insert by topic+language+title) ──────
	// Published rows the GET /cattle/education hub lists on day one (§10).
	education := []domain.EducationContent{
		{Topic: "CLEAN_MILKING", Title: "स्वच्छ दुहाई के 5 कदम", Language: "hi", MediaType: "VIDEO",
			MediaURL:    "https://saathi-media.s3.ap-south-1.amazonaws.com/education/clean-milking-hi.mp4",
			DurationSec: 240, Published: true, CreatedAt: now},
		{Topic: "FEED_RATION", Title: "संतुलित पशु आहार कैसे बनाएं", Language: "hi", MediaType: "AUDIO",
			MediaURL:    "https://saathi-media.s3.ap-south-1.amazonaws.com/education/feed-ration-hi.mp3",
			DurationSec: 180, Published: true, CreatedAt: now},
		{Topic: "BREED_CARE", Title: "Sahiwal cow care basics", Language: "en", MediaType: "INFOGRAPHIC",
			MediaURL:  "https://saathi-media.s3.ap-south-1.amazonaws.com/education/sahiwal-care-en.png",
			Published: true, CreatedAt: now},
	}
	for _, e := range education {
		if err := upsertByFilter(ctx, db.Collection(mongodb.CollEducation),
			bson.D{
				{Key: "topic", Value: e.Topic},
				{Key: "language", Value: e.Language},
				{Key: "title", Value: e.Title},
			}, e); err != nil {
			return fmt.Errorf("education %s/%s: %w", e.Topic, e.Language, err)
		}
	}

	fmt.Println("✔ seed complete:")
	for _, o := range orgs {
		fmt.Printf("  org %-14s %-28s %s\n", o.Code, o.Name, o.ID.Hex())
	}
	fmt.Printf("  super admin phone: %s (OTP_DEV_MODE returns the OTP in the login response)\n", cfg.SeedAdminPhone)
	fmt.Println("  demo phones: sacheev 9000000001 · adhyaksh 9000000002 · farmer 9000000011 · rider 9000000021")
	fmt.Println("               bmc-op 9000000031 · plant-op 9000000041 · lab 9000000042 · org-manager 9000000071")
	return nil
}

// upsertOrg finds-or-inserts an org unit by its unique code and returns the
// stored document (with its generated ObjectID).
func upsertOrg(ctx context.Context, db *mongo.Database, o domain.OrgUnit) (*domain.OrgUnit, error) {
	var out domain.OrgUnit
	err := db.Collection(mongodb.CollOrgUnits).FindOneAndUpdate(ctx,
		bson.D{{Key: "code", Value: o.Code}},
		bson.D{{Key: "$setOnInsert", Value: o}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&out)
	if err != nil {
		return nil, fmt.Errorf("org %s: %w", o.Code, err)
	}
	return &out, nil
}

// upsertParty finds-or-inserts a party by its unique phone.
func upsertParty(ctx context.Context, db *mongo.Database, p domain.Party) (*domain.Party, error) {
	var out domain.Party
	err := db.Collection(mongodb.CollParties).FindOneAndUpdate(ctx,
		bson.D{{Key: "phone", Value: p.Phone}},
		bson.D{{Key: "$setOnInsert", Value: p}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// upsertRoleAssignment finds-or-inserts by the (party, role, org) natural key.
func upsertRoleAssignment(ctx context.Context, db *mongo.Database, ra domain.RoleAssignment) error {
	_, err := db.Collection(mongodb.CollRoleAssignments).UpdateOne(ctx,
		bson.D{
			{Key: "party_id", Value: ra.PartyID},
			{Key: "role_code", Value: ra.RoleCode},
			{Key: "org_unit_id", Value: ra.OrgUnitID},
		},
		bson.D{{Key: "$setOnInsert", Value: ra}},
		options.Update().SetUpsert(true),
	)
	return err
}

// upsertByFilter inserts doc if nothing matches filter (generic natural-key upsert).
func upsertByFilter(ctx context.Context, coll *mongo.Collection, filter bson.D, doc any) error {
	_, err := coll.UpdateOne(ctx, filter,
		bson.D{{Key: "$setOnInsert", Value: doc}},
		options.Update().SetUpsert(true),
	)
	return err
}
