package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
)

// TestConsentsEndToEnd is the CRITICAL integration pin for the consents API:
// records written through the REAL handler must flip crmHasPromoConsent —
// grant → true, revoke → false, TTL expiry → false, out-of-order replay →
// no state regression — with the guard's fail-closed polarity never inverted.
//
// Gated on CONSUMER_MONGO_TEST_URI like the other integration tests:
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run ConsentsEndToEnd -v
func TestConsentsEndToEnd(t *testing.T) {
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the consents integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer client.Disconnect(ctx)
	db := client.Database("consumer_consents_test")
	_ = db.Drop(ctx)
	defer db.Drop(ctx)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &deps.Deps{
		Cfg: &config.Config{JWTSecret: "consents-test-secret"},
		Log: log, DB: db,
		Flags: flags.NewService(db),
		Bus:   eventbus.New(log),
	}
	repo := newRepository(db)
	svc := newService(d, repo, log)
	if err := repo.ensureIndexes(ctx); err != nil {
		t.Fatalf("ensureIndexes (incl. the load-bearing consent uniques): %v", err)
	}
	h := &handler{svc: svc}

	// ── HTTP helpers: real handlers, context-authenticated like production ──
	asActor := func(r *http.Request, id primitive.ObjectID) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), consumerCtxKey, consumerActor{ID: id.Hex()}))
	}
	post := func(id primitive.ObjectID, body string) *httptest.ResponseRecorder {
		req := asActor(httptest.NewRequest(http.MethodPost, "/users/me/consents", strings.NewReader(body)), id)
		rec := httptest.NewRecorder()
		h.postConsents(rec, req)
		return rec
	}
	getState := func(id primitive.ObjectID) map[string]consentStateView {
		req := asActor(httptest.NewRequest(http.MethodGet, "/users/me/consents", nil), id)
		rec := httptest.NewRecorder()
		h.getConsents(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET consents: %d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Consents map[string]consentStateView `json:"consents"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET consents decode: %v", err)
		}
		return out.Consents
	}
	logCount := func(id primitive.ObjectID) int64 {
		n, err := db.Collection(collConsentLog).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: id}})
		if err != nil {
			t.Fatalf("log count: %v", err)
		}
		return n
	}
	ts := func(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }
	one := func(typ string, granted bool, at time.Time) string {
		return fmt.Sprintf(`{"consents":[{"type":%q,"granted":%v,"version":"1.0","language":"en","app_version":"26.8.1","occurred_at":%q}]}`,
			typ, granted, ts(at))
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	cid := primitive.NewObjectID()
	// A real account so the erasure step exercises the full cascade.
	if _, err := db.Collection(collAccounts).InsertOne(ctx, bson.D{
		{Key: "_id", Value: cid}, {Key: "phone", Value: "+919000000031"},
		{Key: "status", Value: "ACTIVE"}, {Key: "created_at", Value: now}, {Key: "updated_at", Value: now},
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// ── 0) Fail closed from birth: no consent rows → guard false ──
	if svc.crmHasPromoConsent(ctx, cid) {
		t.Fatal("guard must be FALSE with no consent rows (fail closed)")
	}

	// ── 1) Grant marketing_whatsapp via the handler → guard TRUE ──
	t1 := now.Add(-10 * time.Minute)
	if rec := post(cid, one("marketing_whatsapp", true, t1)); rec.Code != 200 {
		t.Fatalf("grant POST: %d %s", rec.Code, rec.Body.String())
	}
	if !svc.crmHasPromoConsent(ctx, cid) {
		t.Fatal("guard must be TRUE after a fresh marketing_whatsapp grant")
	}
	st := getState(cid)
	if v, ok := st["marketing_whatsapp"]; !ok || !v.Granted || v.Version != "1.0" || !v.OccurredAt.Equal(t1) {
		t.Fatalf("state after grant wrong: %+v", st)
	}
	if _, leaked := st["promotional"]; leaked {
		t.Fatal("the derived promotional aggregate must never appear in the GET map")
	}

	// ── 2) Revoke → guard FALSE (opt-out wins) ──
	t2 := now.Add(-5 * time.Minute)
	if rec := post(cid, one("marketing_whatsapp", false, t2)); rec.Code != 200 {
		t.Fatalf("revoke POST: %d %s", rec.Code, rec.Body.String())
	}
	if svc.crmHasPromoConsent(ctx, cid) {
		t.Fatal("guard must be FALSE after revoke")
	}
	if v := getState(cid)["marketing_whatsapp"]; v.Granted {
		t.Fatal("state must show granted=false after revoke")
	}

	// ── 3) Out-of-order replay of the OLD grant → NO state regression ──
	before := logCount(cid)
	if rec := post(cid, one("marketing_whatsapp", true, t1)); rec.Code != 200 {
		t.Fatalf("replay POST: %d %s", rec.Code, rec.Body.String())
	}
	if svc.crmHasPromoConsent(ctx, cid) {
		t.Fatal("a replayed stale grant must NOT resurrect promo consent")
	}
	if v := getState(cid)["marketing_whatsapp"]; v.Granted {
		t.Fatal("a replayed stale grant must not flip state back to granted")
	}
	// …and the at-least-once audit append deduped (same consumer/kind/occurred_at).
	if after := logCount(cid); after != before {
		t.Fatalf("audit log must dedup replays: %d → %d", before, after)
	}

	// ── 4) Equal-timestamp tie goes to opt-out ──
	t3 := now.Add(-4 * time.Minute)
	if rec := post(cid, one("marketing_sms", true, t3)); rec.Code != 200 {
		t.Fatalf("sms grant: %d %s", rec.Code, rec.Body.String())
	}
	if !svc.crmHasPromoConsent(ctx, cid) {
		t.Fatal("guard must be TRUE after the sms grant")
	}
	if rec := post(cid, one("marketing_sms", false, t3)); rec.Code != 200 { // SAME timestamp
		t.Fatalf("sms tie revoke: %d %s", rec.Code, rec.Body.String())
	}
	if svc.crmHasPromoConsent(ctx, cid) {
		t.Fatal("an equal-timestamp revoke must win (ties go to opt-out)")
	}
	if rec := post(cid, one("marketing_sms", true, t3)); rec.Code != 200 { // replay the tied grant
		t.Fatalf("sms tie replay: %d %s", rec.Code, rec.Body.String())
	}
	if svc.crmHasPromoConsent(ctx, cid) {
		t.Fatal("replaying the tied grant must not beat the revoke")
	}

	// ── 5) Multi-channel aggregate: any ACTIVE marketing grant keeps the
	//        gate open; revoking the last one closes it ──
	cid3 := primitive.NewObjectID()
	if rec := post(cid3, one("marketing_sms", true, now.Add(-10*time.Minute))); rec.Code != 200 {
		t.Fatalf("cid3 sms grant: %d", rec.Code)
	}
	if rec := post(cid3, one("marketing_whatsapp", true, now.Add(-9*time.Minute))); rec.Code != 200 {
		t.Fatalf("cid3 wa grant: %d", rec.Code)
	}
	if rec := post(cid3, one("marketing_whatsapp", false, now.Add(-8*time.Minute))); rec.Code != 200 {
		t.Fatalf("cid3 wa revoke: %d", rec.Code)
	}
	if !svc.crmHasPromoConsent(ctx, cid3) {
		t.Fatal("guard must stay TRUE while another marketing channel is still granted")
	}
	// G2b channel granularity: the aggregate is open (sms still granted), but
	// the per-channel check the dispatcher runs before each transport must say
	// sms=yes, whatsapp=no — a partial opt-in never unlocks every transport.
	if !svc.crmHasChannelConsent(ctx, cid3, "sms") {
		t.Fatal("G2b: sms channel consent must be TRUE while marketing_sms is granted")
	}
	if svc.crmHasChannelConsent(ctx, cid3, "whatsapp") {
		t.Fatal("G2b: whatsapp channel consent must be FALSE after its revoke, even while the aggregate is open")
	}
	if rec := post(cid3, one("marketing_sms", false, now.Add(-7*time.Minute))); rec.Code != 200 {
		t.Fatalf("cid3 sms revoke: %d", rec.Code)
	}
	if svc.crmHasPromoConsent(ctx, cid3) {
		t.Fatal("guard must be FALSE once the last marketing channel is revoked")
	}
	if svc.crmHasChannelConsent(ctx, cid3, "sms") {
		t.Fatal("G2b: sms channel consent must close on revoke")
	}

	// ── 6) TTL expiry: a grant older than ConsentTTLDays no longer passes ──
	ttlDays := crmConfigLoad().Guards.ConsentTTLDays
	cid2 := primitive.NewObjectID()
	stale := now.AddDate(0, 0, -(ttlDays + 1))
	if rec := post(cid2, one("marketing_whatsapp", true, stale)); rec.Code != 200 {
		t.Fatalf("stale grant POST: %d %s", rec.Code, rec.Body.String())
	}
	if svc.crmHasPromoConsent(ctx, cid2) {
		t.Fatalf("guard must be FALSE for a grant older than the %d-day TTL", ttlDays)
	}
	if svc.crmHasChannelConsent(ctx, cid2, "whatsapp") {
		t.Fatal("G2b: an expired channel grant must fail the channel-granular check too")
	}
	// The state map still shows the (expired) grant — evidence retained, gate closed.
	if v := getState(cid2)["marketing_whatsapp"]; !v.Granted {
		t.Fatal("TTL expiry closes the guard but must not rewrite recorded state")
	}

	// ── 7) Unknown type → 400, and NOTHING from that batch applies ──
	rec := post(cid, `{"consents":[{"type":"marketing_email","granted":true},{"type":"marketing_fax","granted":true}]}`)
	if rec.Code != 400 {
		t.Fatalf("unknown type must 400 the batch, got %d %s", rec.Code, rec.Body.String())
	}
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Message == "" {
		t.Fatalf("400 body must carry {message}: %s", rec.Body.String())
	}
	if _, applied := getState(cid)["marketing_email"]; applied {
		t.Fatal("a 400 batch must apply NOTHING (validated before any write)")
	}

	// ── 8) Non-marketing kinds live alongside without touching the guard ──
	if rec := post(cid, one("privacy_terms", true, now.Add(-time.Minute))); rec.Code != 200 {
		t.Fatalf("privacy_terms POST: %d", rec.Code)
	}
	if svc.crmHasPromoConsent(ctx, cid) {
		t.Fatal("privacy_terms must never open the promo gate")
	}
	if v := getState(cid)["privacy_terms"]; !v.Granted {
		t.Fatal("privacy_terms state missing")
	}

	// ── 9) Erasure cascade removes BOTH collections' rows ──
	if err := svc.erase(ctx, cid); err != nil {
		t.Fatalf("erase: %v", err)
	}
	for _, coll := range []string{collConsents, collConsentLog} {
		n, err := db.Collection(coll).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}})
		if err != nil {
			t.Fatalf("%s count: %v", coll, err)
		}
		if n != 0 {
			t.Fatalf("erasure must empty %s for the consumer, %d rows remain", coll, n)
		}
	}
	if svc.crmHasPromoConsent(ctx, cid) {
		t.Fatal("guard must be FALSE after erasure")
	}
}
