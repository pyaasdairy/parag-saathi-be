package platformops

// The party inbox (GET /notifications/me, POST /notifications/{id}/read)
// against a real MongoDB, with rows shaped exactly as their writers store
// them. The operator alerts (STORE_INSTANT_CLOSING / _CLOSED in
// consumer/instant_alerts.go, STORE_LOW_STOCK in consumer/lowstock.go, the
// CRM operator notices in consumer/crm_engine.go) write an explicit
// read_at: null; the identity / platformops writers leave read_at out. Both
// must be markable read, and a read row keeps its first read time.
//
// Gated on CONSUMER_MONGO_TEST_URI like the consumer integration tests:
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/platformops/ -run NotificationsInbox -v

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// inboxWorld is a throwaway database with the platformops repo and service.
func inboxWorld(t *testing.T) (*mongo.Database, *repository, *service) {
	t.Helper()
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the notifications inbox integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	db := client.Database("platformops_notifications_inbox_test")
	_ = db.Drop(ctx)
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo := newRepository(db)
	return db, repo, newService(&deps.Deps{Log: log, DB: db}, repo, log)
}

// insertInboxRow writes one notifications document as-is and returns its id.
func insertInboxRow(t *testing.T, db *mongo.Database, doc bson.D) primitive.ObjectID {
	t.Helper()
	res, err := db.Collection("notifications").InsertOne(context.Background(), doc)
	if err != nil {
		t.Fatalf("insert notification: %v", err)
	}
	return res.InsertedID.(primitive.ObjectID)
}

func TestNotificationsInboxMarkReadNullAndMissingReadAt(t *testing.T) {
	db, repo, svc := inboxWorld(t)
	ctx := context.Background()
	party := primitive.NewObjectID()
	queued := time.Date(2026, 9, 24, 16, 15, 0, 0, time.UTC) // 21:45 IST
	first := time.Date(2026, 9, 24, 16, 20, 0, 0, time.UTC)
	later := time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC)

	// The instant-closing alert as upsertInstantAlert stores it: read_at null.
	closing := insertInboxRow(t, db, bson.D{
		{Key: "party_id", Value: party}, {Key: "phone", Value: "+919900000077"},
		{Key: "channel", Value: "APP"}, {Key: "template_key", Value: "STORE_INSTANT_CLOSING"},
		{Key: "language", Value: "hi"},
		{Key: "params", Value: bson.M{"store_id": "s1", "headline": "Instant delivery closes at 10:00 PM"}},
		{Key: "status", Value: "QUEUED"}, {Key: "queued_at", Value: queued},
		{Key: "read_at", Value: nil},
	})
	// The low-stock alert as raiseLowStock stores it: read_at null, and the
	// SMS worker has since claimed it (status SENT, sent_at).
	lowStock := insertInboxRow(t, db, bson.D{
		{Key: "party_id", Value: party}, {Key: "phone", Value: "+919900000077"},
		{Key: "channel", Value: "APP"}, {Key: "template_key", Value: "STORE_LOW_STOCK"},
		{Key: "language", Value: "hi"},
		{Key: "params", Value: bson.M{"store_id": "s1", "store": "PYAAS Store", "summary": "Toned Milk 500ml (4)", "item_count": "1"}},
		{Key: "status", Value: "SENT"}, {Key: "queued_at", Value: queued}, {Key: "sent_at", Value: queued},
		{Key: "read_at", Value: nil},
	})
	// A role grant as the identity / roles writers store it: no read_at.
	granted := insertInboxRow(t, db, bson.D{
		{Key: "party_id", Value: party}, {Key: "phone", Value: "+919900000077"},
		{Key: "channel", Value: "SMS"}, {Key: "template_key", Value: "ROLE_GRANTED"},
		{Key: "language", Value: "hi"},
		{Key: "params", Value: bson.M{"role": "STORE_MANAGER", "org_name": "PYAAS Store"}},
		{Key: "status", Value: "QUEUED"}, {Key: "queued_at", Value: queued},
	})

	unread := func() int {
		t.Helper()
		items, _, err := svc.listMyNotifications(ctx, auth.Actor{PartyID: party.Hex()}, httpx.Page{Limit: 50})
		if err != nil {
			t.Fatalf("listMyNotifications: %v", err)
		}
		n := 0
		for _, it := range items {
			if !it.Read {
				n++
			}
		}
		return n
	}
	if n := unread(); n != 3 {
		t.Fatalf("before any read: %d unread, want 3", n)
	}

	for _, c := range []struct {
		name string
		id   primitive.ObjectID
	}{{"instant closing (read_at null)", closing}, {"low stock (read_at null)", lowStock}, {"role granted (no read_at)", granted}} {
		n, err := repo.markNotificationRead(ctx, c.id, party, first)
		if err != nil {
			t.Fatalf("%s: mark read: %v", c.name, err)
		}
		if n.ReadAt == nil || !n.ReadAt.Equal(first) {
			t.Fatalf("%s: read_at after the read = %v, want %v", c.name, n.ReadAt, first)
		}
		if v := inboxView(n); !v.Read {
			t.Fatalf("%s: the returned row still reads unread", c.name)
		}
		// Idempotent: a second tap keeps the FIRST read time.
		again, err := repo.markNotificationRead(ctx, c.id, party, later)
		if err != nil {
			t.Fatalf("%s: second read: %v", c.name, err)
		}
		if again.ReadAt == nil || !again.ReadAt.Equal(first) {
			t.Fatalf("%s: read_at after a second read = %v, want the first %v", c.name, again.ReadAt, first)
		}
	}
	if n := unread(); n != 0 {
		t.Fatalf("after reading every row: %d unread, want 0 (the Saathi bell never clears)", n)
	}

	// The stored document really carries the read time (not just the reply).
	var stored struct {
		ReadAt *time.Time `bson:"read_at"`
	}
	if err := db.Collection("notifications").FindOne(ctx, bson.D{{Key: "_id", Value: closing}}).Decode(&stored); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.ReadAt == nil || !stored.ReadAt.Equal(first) {
		t.Fatalf("stored read_at = %v, want %v", stored.ReadAt, first)
	}

	// Another party's row stays theirs: 404, and untouched.
	other := insertInboxRow(t, db, bson.D{
		{Key: "party_id", Value: primitive.NewObjectID()}, {Key: "channel", Value: "APP"},
		{Key: "template_key", Value: "STORE_LOW_STOCK"}, {Key: "status", Value: "QUEUED"},
		{Key: "queued_at", Value: queued}, {Key: "read_at", Value: nil},
	})
	_, err := repo.markNotificationRead(ctx, other, party, first)
	var he *httpx.AppError
	if !errors.As(err, &he) || he.Status != http.StatusNotFound {
		t.Fatalf("a foreign row: %v, want 404", err)
	}
	var foreign bson.M
	if err := db.Collection("notifications").FindOne(ctx, bson.D{{Key: "_id", Value: other}}).Decode(&foreign); err != nil {
		t.Fatalf("reload foreign: %v", err)
	}
	if foreign["read_at"] != nil {
		t.Fatalf("a foreign row was marked read: %v", foreign["read_at"])
	}
}

// Every Saathi login queues an 'OTP' transport row addressed to the party
// (identity.requestOTP: the audit record of the code's SMS, carrying the code
// itself only in local development). It is not a message for the inbox: it
// rendered as a bare "OTP" row and counted unread on the store manager's
// bell, one more per login. GET /notifications/me leaves those rows out (the
// list and its total); the audit outbox (GET /notifications?phone=) keeps
// them, redacted as before.
func TestNotificationsInboxLeavesOutOTPTransportRows(t *testing.T) {
	db, repo, svc := inboxWorld(t)
	ctx := context.Background()
	party := primitive.NewObjectID()
	phone := "+919900000078"
	at := func(min int) time.Time { return time.Date(2026, 9, 24, 3, min, 0, 0, time.UTC) }

	// Two logins' OTP rows, as identity writes them (dev: the code in params).
	for i, code := range []string{"482913", "105577"} {
		insertInboxRow(t, db, bson.D{
			{Key: "party_id", Value: party}, {Key: "phone", Value: phone},
			{Key: "channel", Value: "SMS"}, {Key: "template_key", Value: "OTP"},
			{Key: "language", Value: "hi"}, {Key: "params", Value: bson.M{"otp": code}},
			{Key: "status", Value: "QUEUED"}, {Key: "queued_at", Value: at(i)},
		})
	}
	// A real alert for the same party.
	alert := insertInboxRow(t, db, bson.D{
		{Key: "party_id", Value: party}, {Key: "phone", Value: phone},
		{Key: "channel", Value: "APP"}, {Key: "template_key", Value: "STORE_INSTANT_CLOSING"},
		{Key: "language", Value: "hi"}, {Key: "params", Value: bson.M{"store_id": "s1"}},
		{Key: "status", Value: "QUEUED"}, {Key: "queued_at", Value: at(5)},
		{Key: "read_at", Value: nil},
	})

	items, total, err := svc.listMyNotifications(ctx, auth.Actor{PartyID: party.Hex()}, httpx.Page{Limit: 50})
	if err != nil {
		t.Fatalf("listMyNotifications: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != alert.Hex() || items[0].Read {
		t.Fatalf("inbox = %d rows (total %d): %+v, want only the unread alert", len(items), total, items)
	}

	// The audit outbox still has both OTP rows, the code redacted on read.
	outbox, n, err := repo.listNotifications(ctx, phone, "", httpx.Page{Limit: 50})
	if err != nil {
		t.Fatalf("listNotifications: %v", err)
	}
	otps := 0
	for i := range outbox {
		if outbox[i].TemplateKey == "OTP" {
			otps++
			redactNotificationSecrets(&outbox[i])
			if outbox[i].Params["otp"] != redactedValue {
				t.Fatalf("outbox OTP row not redacted: %v", outbox[i].Params)
			}
		}
	}
	if n != 3 || otps != 2 {
		t.Fatalf("audit outbox: %d rows, %d OTP rows; want 3 and 2", n, otps)
	}
}
