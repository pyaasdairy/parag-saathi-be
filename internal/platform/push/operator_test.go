package push

// Operator fan-out: quiet hours hold every alert but an order or delivery
// one, a dead token is pruned while the others still get theirs, and the
// registry keeps one row per device that follows whoever signed in last.
//
//	go test ./internal/platform/push/ -run Operator -v
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 go test ./internal/platform/push/ -run OperatorRegistry -v

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type fakeDevices struct {
	tokens map[primitive.ObjectID][]string
	pruned []string
	asked  [][]primitive.ObjectID
}

func (f *fakeDevices) TokensFor(_ context.Context, parties []primitive.ObjectID) ([]string, error) {
	f.asked = append(f.asked, parties)
	var out []string
	for _, p := range parties {
		out = append(out, f.tokens[p]...)
	}
	return out, nil
}

func (f *fakeDevices) Prune(_ context.Context, token string) error {
	f.pruned = append(f.pruned, token)
	return nil
}

type fakeFCM struct {
	sent []FCMMessage
	dead map[string]bool
	down map[string]bool
}

func (f *fakeFCM) Enabled() bool { return true }

func (f *fakeFCM) Send(_ context.Context, m FCMMessage) (string, error) {
	if f.dead[m.Token] {
		return "", ErrUnregistered
	}
	if f.down[m.Token] {
		return "", ErrUnavailable
	}
	f.sent = append(f.sent, m)
	return "projects/p/messages/1", nil
}

// ist builds a fixed IST moment on the test day.
func ist(h, m int) time.Time { return time.Date(2026, 9, 26, h, m, 0, 0, istZone) }

func TestOperatorQuietHoursBoundaries(t *testing.T) {
	for _, c := range []struct {
		at    time.Time
		quiet bool
	}{
		{ist(21, 59), false}, {ist(22, 0), true}, {ist(23, 30), true}, {ist(2, 0), true},
		{ist(6, 59), true}, {ist(7, 0), false}, {ist(12, 0), false},
		// The same instants read in UTC: 16:30 UTC is 22:00 IST.
		{time.Date(2026, 9, 26, 16, 29, 0, 0, time.UTC), false},
		{time.Date(2026, 9, 26, 16, 30, 0, 0, time.UTC), true},
		{time.Date(2026, 9, 26, 1, 30, 0, 0, time.UTC), false}, // 07:00 IST
	} {
		if got := InQuietHours(c.at); got != c.quiet {
			t.Fatalf("InQuietHours(%s) = %v, want %v", c.at.In(istZone).Format("15:04"), got, c.quiet)
		}
	}
}

func TestOperatorNotifyHoldsAlertsInQuietHoursButNeverOrders(t *testing.T) {
	mgr := primitive.NewObjectID()
	devs := &fakeDevices{tokens: map[primitive.ObjectID][]string{mgr: {"tok-mgr"}}}
	fcm := &fakeFCM{}
	o := newOperatorWith(devs, fcm)
	ctx := context.Background()

	res, err := o.Notify(ctx, ist(23, 0), []primitive.ObjectID{mgr}, OperatorAlert{Kind: "STORE_INSTANT_CLOSED", Title: "Instant delivery is now closed"})
	if err != nil || !res.HeldQuiet || len(fcm.sent) != 0 || len(devs.asked) != 0 {
		t.Fatalf("an informational alert at 23:00 must be held without touching the registry: %+v err %v sent %d", res, err, len(fcm.sent))
	}
	res, err = o.Notify(ctx, ist(6, 30), []primitive.ObjectID{mgr}, OperatorAlert{Kind: "STORE_LOW_STOCK", Title: "Low stock"})
	if err != nil || !res.HeldQuiet || len(fcm.sent) != 0 {
		t.Fatalf("low stock at 06:30 must wait: %+v", res)
	}

	res, err = o.Notify(ctx, ist(23, 0), []primitive.ObjectID{mgr}, OperatorAlert{Kind: "STORE_INSTANT_ORDER", Title: "New instant order", Urgent: true,
		Data: map[string]string{"delivery_id": "DLV1"}, CollapseKey: "order-DLV1"})
	if err != nil || res.HeldQuiet || res.Sent != 1 || len(fcm.sent) != 1 {
		t.Fatalf("an order alert rings in quiet hours: %+v err %v", res, err)
	}
	m := fcm.sent[0]
	if m.Token != "tok-mgr" || !m.Urgent || m.Data["type"] != "STORE_INSTANT_ORDER" || m.Data["delivery_id"] != "DLV1" ||
		m.AndroidChannelID != "store_orders" || m.CollapseKey != "order-DLV1" {
		t.Fatalf("order message %+v", m)
	}

	res, err = o.Notify(ctx, ist(9, 0), []primitive.ObjectID{mgr}, OperatorAlert{Kind: "STORE_LOW_STOCK", Title: "Low stock", Body: "Toned 500ml (4)"})
	if err != nil || res.Sent != 1 || fcm.sent[1].Urgent || fcm.sent[1].AndroidChannelID != "store_alerts" || fcm.sent[1].Data["type"] != "STORE_LOW_STOCK" {
		t.Fatalf("daytime alert: %+v %+v", res, fcm.sent)
	}
}

func TestOperatorNotifyPrunesDeadTokensAndKeepsGoing(t *testing.T) {
	a, b := primitive.NewObjectID(), primitive.NewObjectID()
	devs := &fakeDevices{tokens: map[primitive.ObjectID][]string{a: {"dead-a", "live-a"}, b: {"flaky-b", "live-b"}}}
	fcm := &fakeFCM{dead: map[string]bool{"dead-a": true}, down: map[string]bool{"flaky-b": true}}
	res, err := newOperatorWith(devs, fcm).Notify(context.Background(), ist(10, 0), []primitive.ObjectID{a, b},
		OperatorAlert{Kind: "STORE_INSTANT_CLOSING", Title: "Instant delivery closes at 10:00 PM", Urgent: true})
	if err != nil {
		t.Fatalf("one good send makes the fan-out a success: %v", err)
	}
	if res.Devices != 4 || res.Sent != 2 || res.Pruned != 1 || res.Failed != 1 {
		t.Fatalf("result %+v, want 4 devices, 2 sent, 1 pruned, 1 failed", res)
	}
	if len(devs.pruned) != 1 || devs.pruned[0] != "dead-a" {
		t.Fatalf("only the UNREGISTERED token is pruned, got %v", devs.pruned)
	}

	// Every send failing surfaces the error so the caller logs it.
	allDown := &fakeFCM{down: map[string]bool{"live-a": true}}
	one := &fakeDevices{tokens: map[primitive.ObjectID][]string{a: {"live-a"}}}
	if _, err := newOperatorWith(one, allDown).Notify(context.Background(), ist(10, 0), []primitive.ObjectID{a}, OperatorAlert{Urgent: true}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a fan-out where nothing went out must say why, got %v", err)
	}
}

func TestOperatorInertWithoutFCM(t *testing.T) {
	var nilOp *Operator
	if nilOp.Enabled() {
		t.Fatal("nil operator must be inert")
	}
	if res, err := nilOp.Notify(context.Background(), ist(10, 0), []primitive.ObjectID{primitive.NewObjectID()}, OperatorAlert{Urgent: true}); err != nil || res.Sent != 0 {
		t.Fatalf("nil operator notify: %+v %v", res, err)
	}
	o := NewOperator(nil, NewFCM(FCMConfig{}))
	if o.Enabled() {
		t.Fatal("an operator without a service account must be inert")
	}
}

// ── registry (Mongo) ──

func operatorRegistryDB(t *testing.T) *mongo.Database {
	t.Helper()
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the operator registry test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	db := client.Database("push_operator_registry_test")
	_ = db.Drop(ctx)
	t.Cleanup(func() {
		c, cl := context.WithTimeout(context.Background(), 10*time.Second)
		defer cl()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	})
	return db
}

func TestOperatorRegistryOneRowPerDeviceFollowsTheSignerIn(t *testing.T) {
	db := operatorRegistryDB(t)
	reg := NewOperatorRegistry(db)
	ctx := context.Background()
	if err := reg.EnsureIndexes(ctx); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	mgr, rider := primitive.NewObjectID(), primitive.NewObjectID()
	t0 := ist(9, 0)

	for _, bad := range []RegisterInput{
		{Token: "", Platform: "android"},
		{Token: "tok", Platform: "web"},
		{Token: "tok", Platform: "android", Provider: "expo"},
	} {
		var be ErrBadDevice
		if err := reg.Register(ctx, mgr, "STORE_MANAGER", bad, t0); !errors.As(err, &be) {
			t.Fatalf("register %+v must be refused as a bad device, got %v", bad, err)
		}
	}

	// Launch, relaunch: still one row.
	for i := 0; i < 2; i++ {
		if err := reg.Register(ctx, mgr, "STORE_MANAGER", RegisterInput{Token: " shared-phone ", Platform: "Android", AppVersion: "1.7.4"}, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	if n, _ := db.Collection(CollOperatorPushDevices).CountDocuments(ctx, bson.D{}); n != 1 {
		t.Fatalf("rows %d, want 1", n)
	}
	var row OperatorDevice
	if err := db.Collection(CollOperatorPushDevices).FindOne(ctx, bson.D{{Key: "token", Value: "shared-phone"}}).Decode(&row); err != nil {
		t.Fatalf("row: %v", err)
	}
	if row.PartyID != mgr || row.Platform != "android" || row.Provider != "fcm" || row.AppVersion != "1.7.4" || row.RoleCode != "STORE_MANAGER" {
		t.Fatalf("row %+v", row)
	}

	// The rider signs in on the same handset: the device is theirs now, and
	// the manager's stale unregister cannot take it away from them.
	if err := reg.Register(ctx, rider, "DELIVERY_RIDER", RegisterInput{Token: "shared-phone", Platform: "android"}, t0.Add(time.Hour)); err != nil {
		t.Fatalf("rider register: %v", err)
	}
	if err := reg.Unregister(ctx, mgr, "shared-phone"); err != nil {
		t.Fatalf("stale unregister: %v", err)
	}
	if got, _ := reg.TokensFor(ctx, []primitive.ObjectID{rider}); len(got) != 1 || got[0] != "shared-phone" {
		t.Fatalf("rider tokens %v", got)
	}
	if got, _ := reg.TokensFor(ctx, []primitive.ObjectID{mgr}); len(got) != 0 {
		t.Fatalf("the manager no longer holds the shared phone, got %v", got)
	}

	// The manager's own phones: newest five only, distinct across parties.
	for i := 0; i < 7; i++ {
		tok := "mgr-phone-" + string(rune('a'+i))
		if err := reg.Register(ctx, mgr, "STORE_MANAGER", RegisterInput{Token: tok, Platform: "ios"}, t0.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("register %s: %v", tok, err)
		}
	}
	got, err := reg.TokensFor(ctx, []primitive.ObjectID{mgr, rider, mgr})
	if err != nil {
		t.Fatalf("tokens: %v", err)
	}
	if len(got) != 6 || got[0] != "mgr-phone-g" {
		t.Fatalf("tokens %v, want the manager's newest five and the rider's phone, newest first", got)
	}
	hasShared := false
	for _, g := range got {
		hasShared = hasShared || g == "shared-phone"
	}
	if !hasShared {
		t.Fatalf("the rider's phone is missing from %v", got)
	}
	for _, old := range []string{"mgr-phone-a", "mgr-phone-b"} {
		for _, g := range got {
			if g == old {
				t.Fatalf("%s is older than the newest five and must not be sent to", old)
			}
		}
	}

	// Sign-out unbinds; prune drops a dead token whoever holds it.
	if err := reg.Unregister(ctx, rider, "shared-phone"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if err := reg.Prune(ctx, "mgr-phone-g"); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n, _ := db.Collection(CollOperatorPushDevices).CountDocuments(ctx, bson.D{{Key: "token", Value: bson.D{{Key: "$in", Value: bson.A{"shared-phone", "mgr-phone-g"}}}}}); n != 0 {
		t.Fatalf("unregistered and pruned rows must be gone, %d left", n)
	}
	if err := reg.Unregister(ctx, rider, "shared-phone"); err != nil {
		t.Fatalf("a second unregister is a no-op, got %v", err)
	}
}
