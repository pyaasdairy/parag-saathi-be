package consumer

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// bootstrapDevDelivery makes the consumer last-mile flow immediately testable on
// a fresh deployment (e.g. Render + a minimal Atlas): it ensures a Parag STORE
// org plus a store manager (9876500015) and a delivery rider (9876500013) exist,
// scoped to that store. GATED to OTP dev mode and fully idempotent — a no-op in
// production, where the store + operators come from real Saathi KYC onboarding.
func (s *service) bootstrapDevDelivery(ctx context.Context) {
	if !s.deps.Cfg.OTPDevMode {
		return
	}
	storeID, err := s.ensureDevStore(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "dev delivery bootstrap: store", "err", err)
		return
	}
	s.ensureDevOperator(ctx, storeID, "9876500015", "Parag Store Manager", "STANDARD", "STORE_MANAGER")
	s.ensureDevOperator(ctx, storeID, "9876500013", "Parag Delivery Rider", "RIDER", "DELIVERY_RIDER")
	s.log.InfoContext(ctx, "dev delivery bootstrap ready", "store_id", storeID.Hex())
}

func (s *service) ensureDevStore(ctx context.Context) (primitive.ObjectID, error) {
	var existing struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	if err := s.repo.orgUnits.FindOne(ctx, bson.D{{Key: "type", Value: "STORE"}}).Decode(&existing); err == nil {
		return existing.ID, nil
	}
	// Parent + ancestor path from a district union when one exists.
	var union struct {
		ID       primitive.ObjectID   `bson:"_id"`
		Path     []primitive.ObjectID `bson:"path"`
		District string               `bson:"district"`
		State    string               `bson:"state"`
	}
	_ = s.repo.orgUnits.FindOne(ctx, bson.D{{Key: "type", Value: "MILK_UNION"}}).Decode(&union)
	now := time.Now().UTC()
	doc := bson.D{
		{Key: "name", Value: "Parag Store — Gomti Nagar, Lucknow"},
		{Key: "name_hi", Value: "पराग स्टोर — गोमती नगर, लखनऊ"},
		{Key: "type", Value: "STORE"},
		{Key: "code", Value: "STORE-LKO-01"},
		{Key: "active", Value: true},
		{Key: "district", Value: firstNonEmpty(union.District, "Lucknow")},
		{Key: "state", Value: firstNonEmpty(union.State, "Uttar Pradesh")},
		{Key: "geo_lat", Value: 26.8560},
		{Key: "geo_lng", Value: 81.0060},
		{Key: "created_at", Value: now},
		{Key: "updated_at", Value: now},
	}
	if !union.ID.IsZero() {
		doc = append(doc,
			bson.E{Key: "parent_id", Value: union.ID},
			bson.E{Key: "path", Value: append(append([]primitive.ObjectID{}, union.Path...), union.ID)},
		)
	} else {
		doc = append(doc, bson.E{Key: "path", Value: []primitive.ObjectID{}})
	}
	res, err := s.repo.orgUnits.InsertOne(ctx, doc)
	if err != nil {
		return primitive.NilObjectID, err
	}
	return res.InsertedID.(primitive.ObjectID), nil
}

func (s *service) ensureDevOperator(ctx context.Context, storeID primitive.ObjectID, phone, name, tier, role string) {
	var p struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	now := time.Now().UTC()
	if err := s.repo.parties.FindOne(ctx, bson.D{{Key: "phone", Value: phone}}).Decode(&p); err != nil {
		res, e := s.repo.parties.InsertOne(ctx, bson.D{
			{Key: "phone", Value: phone}, {Key: "full_name", Value: name}, {Key: "kyc_tier", Value: tier},
			{Key: "status", Value: "ACTIVE"}, {Key: "preferred_language", Value: "en"},
			{Key: "public_consent", Value: false}, {Key: "created_at", Value: now}, {Key: "updated_at", Value: now},
		})
		if e != nil {
			s.log.WarnContext(ctx, "dev operator party", "phone", phone, "err", e)
			return
		}
		p.ID = res.InsertedID.(primitive.ObjectID)
	} else {
		// Keep the tier high enough for role/select.
		_, _ = s.repo.parties.UpdateByID(ctx, p.ID, bson.D{{Key: "$set", Value: bson.D{{Key: "kyc_tier", Value: tier}, {Key: "status", Value: "ACTIVE"}}}})
	}
	n, _ := s.repo.roleAssignments.CountDocuments(ctx, bson.D{{Key: "party_id", Value: p.ID}, {Key: "role_code", Value: role}, {Key: "org_unit_id", Value: storeID}})
	if n == 0 {
		_, _ = s.repo.roleAssignments.InsertOne(ctx, bson.D{
			{Key: "party_id", Value: p.ID}, {Key: "org_unit_id", Value: storeID}, {Key: "role_code", Value: role},
			{Key: "status", Value: "ACTIVE"}, {Key: "valid_from", Value: now}, {Key: "created_at", Value: now},
		})
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
