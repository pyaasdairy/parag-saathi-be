package consumer

// R1-15: the scoped claim index is migrated at boot, but during a Render
// rolling deploy the OLD binary keeps claiming per-day triggers with no
// scope_key (null) while the NEW one claims with scope_key "". The unique
// index treats null and "" as different keys, so both were admitted and
// W-07 / B-01 / W-03a could go out twice when a deploy overlapped their
// morning run. And when the scoped build is refused (legacy index kept),
// the second order's D-01 / D-06 were dropped with no trace at all.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMClaimDeploy -v

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestCRMClaimDeployOldBinaryClaimHoldsTheDay(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	live := w.svc.repo.crmDispatchCol()
	day := istDay(time.Now())
	cid := primitive.NewObjectID()
	// The old binary's W-07 claim: no scope_key at all.
	if _, err := live.InsertOne(ctx, bson.D{{Key: "trigger_id", Value: "W-07"}, {Key: "consumer_id", Value: cid},
		{Key: "ist_day", Value: day}, {Key: "status", Value: "SENT"}}); err != nil {
		t.Fatalf("old-binary claim: %v", err)
	}
	if _, won := w.svc.crmClaimDispatchScoped(ctx, crmTrigger{ID: "W-07", Category: "service_implicit"}, cid, day, ""); won {
		t.Fatal("the new binary claimed a day the old binary already claimed: W-07 goes out twice")
	}
	// Another day is still free.
	if _, won := w.svc.crmClaimDispatchScoped(ctx, crmTrigger{ID: "W-07", Category: "service_implicit"}, cid, addDaysIST(day, 1), ""); !won {
		t.Fatal("a different day must still be claimable")
	}
}

func TestCRMClaimDeployLostScopedClaimIsLogged(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	var logs bytes.Buffer
	w.svc.log = slog.New(slog.NewTextHandler(&logs, nil))
	live := w.svc.repo.crmDispatchCol()
	// Degraded mode: the scoped build was refused and the legacy index kept.
	_, _ = live.Indexes().DropOne(ctx, crmClaimIndexScoped)
	if _, err := live.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "trigger_id", Value: 1}, {Key: "consumer_id", Value: 1}, {Key: "ist_day", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		t.Fatalf("legacy index: %v", err)
	}
	cid := primitive.NewObjectID()
	day := istDay(time.Now())
	d01 := crmTrigger{ID: "D-01", Category: "service_implicit"}
	if _, won := w.svc.crmClaimDispatchScoped(ctx, d01, cid, day, "ord_first"); !won {
		t.Fatal("first order's D-01 must claim")
	}
	if _, won := w.svc.crmClaimDispatchScoped(ctx, d01, cid, day, "ord_second"); won {
		t.Fatal("the legacy index admits one D-01 a day (degraded mode)")
	}
	if out := logs.String(); !strings.Contains(out, "legacy") || !strings.Contains(out, "D-01") || !strings.Contains(out, "ord_second") {
		t.Fatalf("a lost event-scoped claim must be logged with its trigger and scope, got %q", out)
	}
}
