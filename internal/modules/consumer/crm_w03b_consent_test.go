package consumer

// W-03b is promotional, so it stays consent-gated (owner decision, 24 Sep:
// promotional messages need affirmative consent under DPDP / TRAI). The open
// question from the 20 Sep audit was whether the derived "promotional"
// consent row the G2 guard reads is ever written from the member's
// marketing_* grants. It is: POST /users/me/consents recomputes it
// (recomputePromoConsentOnce). This pins the whole chain, from the app's
// consent POST to the 10:32 sweep: a household that granted marketing gets
// W-03b; one that did not is SUPPRESSED on G2 and gets no message.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMW03b -v

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMW03bConsentGate(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	h := &handler{svc: w.svc}

	enrol := func(phone, line string) primitive.ObjectID {
		t.Helper()
		res, err := w.svc.crmEnrol(ctx, "w03b-test-operator", crmEnrolInput{
			Phone: phone, Name: "W03b Household", Line1: line, Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
		})
		if err != nil {
			t.Fatalf("crmEnrol %s: %v", phone, err)
		}
		cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)
		return cid
	}
	postConsents := func(cid primitive.ObjectID, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/users/me/consents", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
		rec := httptest.NewRecorder()
		h.postConsents(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST consents: %d %s", rec.Code, rec.Body.String())
		}
	}
	at := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	row := func(typ string, granted bool) string {
		return fmt.Sprintf(`{"type":%q,"granted":%v,"version":"2026-07","app_version":"1.0.0","occurred_at":%q}`, typ, granted, at)
	}

	// The app's complete-profile batch, exactly as lib/consentSync.ts sends it.
	granted := enrol("9000008301", "Flat 1, Consent Tower")
	postConsents(granted, `{"consents":[`+strings.Join([]string{
		row("privacy_terms", true), row("marketing_push", true), row("marketing_whatsapp", true),
		row("marketing_sms", true), row("marketing_email", false),
	}, ",")+`]}`)
	declined := enrol("9000008302", "Flat 2, Consent Tower")
	postConsents(declined, `{"consents":[`+strings.Join([]string{
		row("privacy_terms", true), row("marketing_push", false), row("marketing_whatsapp", false),
		row("marketing_sms", false), row("marketing_email", false),
	}, ",")+`]}`)

	// The derived aggregate the G2 guard reads is WRITTEN from the grants.
	var agg consentDoc
	if err := w.db.Collection(collConsents).FindOne(ctx, bson.D{
		{Key: "consumer_id", Value: granted}, {Key: "kind", Value: consentKindPromo},
	}).Decode(&agg); err != nil || agg.RevokedAt != nil {
		t.Fatalf("derived promotional row for the granting member: %+v %v", agg, err)
	}
	if n, _ := w.db.Collection(collConsents).CountDocuments(ctx, bson.D{
		{Key: "consumer_id", Value: declined}, {Key: "kind", Value: consentKindPromo}, {Key: "revoked_at", Value: nil},
	}); n != 0 {
		t.Fatalf("a member who granted nothing holds %d active promotional rows", n)
	}

	// Day 0 (pack 1 landed this morning), pack 2 locked, the 10:32 block.
	nowIST := time.Now().In(istZone)
	sched := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 10, 35, 0, 0, istZone)
	for _, cid := range []primitive.ObjectID{granted, declined} {
		forceFirstDelivery(t, w.db, cid, sched.Add(-5*time.Hour))
		if off := mustOffer(t, ctx, w.svc, cid); off.Pack2State != pack2Locked {
			t.Fatalf("pack 2 must start locked: %+v", off)
		}
	}
	w.svc.crmProcessSchedules(ctx, sched)

	if got := crmDispatchStatuses(t, w.db, granted, "W-03b"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("W-03b for the member who granted marketing: %v", got)
	}
	if n := inboxCount(t, w.db, granted, "W-03b"); n != 1 {
		t.Fatalf("W-03b inbox rows (granted) = %d, want 1", n)
	}
	var sup crmDispatchRow
	if err := w.db.Collection(collCRMDispatch).FindOne(ctx, bson.D{
		{Key: "consumer_id", Value: declined}, {Key: "trigger_id", Value: "W-03b"},
	}).Decode(&sup); err != nil || sup.Status != "SUPPRESSED" || sup.Guard != "G2_consent" {
		t.Fatalf("W-03b for the member who declined: %+v %v", sup, err)
	}
	if n := inboxCount(t, w.db, declined, "W-03b"); n != 0 {
		t.Fatalf("W-03b reached a member without consent: %d rows", n)
	}
}
