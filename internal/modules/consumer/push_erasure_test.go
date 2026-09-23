package consumer

// DPDP erasure takes the push registry with the account: a deleted member
// must not keep receiving order news on their old phone, and another
// member's device must be untouched by it.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run ErasureForgetsPushDevices -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestErasureForgetsPushDevices(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	gone := w.customer(t, "9000007113", 0)
	stays := w.customer(t, "9000007114", 0)
	for _, reg := range []struct {
		who primitive.ObjectID
		tok string
	}{
		{gone, "ExponentPushToken[ffffffffffffffffffffff]"},
		{gone, "ExpoPushToken[gggggggggggggggggggggg]"},
		{stays, "ExponentPushToken[hhhhhhhhhhhhhhhhhhhhhh]"},
	} {
		if err := w.svc.registerPushDevice(ctx, reg.who, pushRegisterInput{Token: reg.tok, Platform: "android", Provider: "expo"}); err != nil {
			t.Fatalf("register %s: %v", reg.tok, err)
		}
	}
	count := func(who primitive.ObjectID) int64 {
		n, err := w.db.Collection(collPushDevices).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: who}})
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if count(gone) != 2 || count(stays) != 1 {
		t.Fatalf("setup: gone=%d stays=%d", count(gone), count(stays))
	}
	if err := w.svc.erase(ctx, gone); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if n := count(gone); n != 0 {
		t.Fatalf("erasure must empty consumer_push_devices for the member, %d rows remain", n)
	}
	if n := count(stays); n != 1 {
		t.Fatalf("another member's device must survive an erasure, got %d rows", n)
	}
	// The transport sees no device for the erased member either: the push
	// side of a dispatch would fall through, never address the old phone.
	if toks, err := w.svc.repo.expoPushTokens(ctx, gone); err != nil || len(toks) != 0 {
		t.Fatalf("erased member must have no sendable token, got %v (%v)", toks, err)
	}
}
