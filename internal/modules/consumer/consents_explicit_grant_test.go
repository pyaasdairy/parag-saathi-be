package consumer

// Founder decision 5 (25 Sep 2026): consent defaults UNTICKED (DPDP "clear
// affirmative action", TRAI promotional rules, Apple). The server side of it:
// promotional consent exists ONLY where the member's app sent an explicit
// marketing_* grant. Signing up, completing the profile, a promoter's Welcome
// Litre enrolment, accepting the Privacy Policy and Terms, the phone and
// location disclosures, reading the consent state back, and a sign-up batch
// with every marketing channel unticked never open the promotional gate (G2),
// the per-channel gate (G2b), or the app's "Offers" state.
//
// What the server cannot do is tell a tick the member made from a box the
// sign-up form ticked for them: both arrive as granted:true. See
// docs/CRM-MESSAGES.md "Consent" for which records that covers.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 \
//	  go test ./internal/modules/consumer/ -run ConsentPromotional -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestConsentPromotionalOnlyFromAnExplicitMarketingGrant(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	rows := func(cid primitive.ObjectID) int64 {
		t.Helper()
		n, err := w.svc.repo.consents.CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}})
		if err != nil {
			t.Fatalf("consent rows: %v", err)
		}
		return n
	}
	closed := func(step string, cid primitive.ObjectID) {
		t.Helper()
		if w.svc.crmHasPromoConsent(ctx, cid) {
			t.Fatalf("%s: the promotional gate (G2) is open", step)
		}
		for _, ch := range []string{"push", "whatsapp", "sms", "email"} {
			if w.svc.crmHasChannelConsent(ctx, cid, ch) {
				t.Fatalf("%s: the %s channel gate (G2b) is open", step, ch)
			}
		}
		st, err := w.svc.consentState(ctx, cid)
		if err != nil {
			t.Fatalf("%s: consent state: %v", step, err)
		}
		for kind, v := range st {
			if isMarketingConsent(kind) && v.Granted {
				t.Fatalf("%s: %s reads granted", step, kind)
			}
		}
	}

	// 1) OTP sign-up, then the profile step: no consent row at all.
	const phone = "9000014301"
	w.svc.deps.Cfg.OTPTTL = 5 * time.Minute
	code, _, err := w.svc.requestOTP(ctx, phone)
	if err != nil || code == "" {
		t.Fatalf("requestOTP: %q %v", code, err)
	}
	if _, err := w.svc.verifyOTP(ctx, phone, code); err != nil {
		t.Fatalf("verifyOTP: %v", err)
	}
	acct, err := w.svc.repo.findAccountByPhone(ctx, "+91"+phone)
	if err != nil || acct == nil {
		t.Fatalf("account: %v", err)
	}
	cid := acct.ID
	if _, err := w.svc.updateMe(ctx, cid, map[string]any{"full_name": "Unticked Member"}); err != nil {
		t.Fatalf("updateMe: %v", err)
	}
	if n := rows(cid); n != 0 {
		t.Fatalf("sign-up wrote %d consent rows", n)
	}
	closed("after sign-up", cid)

	// 2) A promoter's Welcome Litre enrolment grants nothing either.
	res, err := w.svc.crmEnrol(ctx, "consent-test-operator", crmEnrolInput{
		Phone: "9000014302", Name: "Enrolled Household", Line1: "Flat 3, Consent Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("crmEnrol: %v", err)
	}
	enrolled, _ := primitive.ObjectIDFromHex(res.ConsumerID)
	if n := rows(enrolled); n != 0 {
		t.Fatalf("enrolment wrote %d consent rows", n)
	}
	closed("after enrolment", enrolled)

	// 3) The sign-up batch with the offers box left UNTICKED: the required
	// consents and the disclosures granted, every marketing channel false.
	yes, no := true, false
	at := time.Now().UTC().Add(-time.Minute)
	batch := []consentInput{
		{Type: "privacy_terms", Granted: &yes, Version: "2026-07", OccurredAt: at},
		{Type: "disclosure_phone", Granted: &yes, Version: "2026-08-18.2", OccurredAt: at},
		{Type: "disclosure_location", Granted: &yes, Version: "1", OccurredAt: at},
	}
	for _, k := range marketingConsentKinds {
		batch = append(batch, consentInput{Type: k, Granted: &no, Version: "2026-07", OccurredAt: at})
	}
	if aerr := w.svc.applyConsents(ctx, cid, batch); aerr != nil {
		t.Fatalf("unticked sign-up batch: %v", aerr)
	}
	if n, _ := w.svc.repo.consents.CountDocuments(ctx, bson.D{
		{Key: "consumer_id", Value: cid}, {Key: "kind", Value: consentKindPromo}, {Key: "revoked_at", Value: nil},
	}); n != 0 {
		t.Fatalf("an unticked batch left an active promotional aggregate (%d)", n)
	}
	closed("after the unticked sign-up batch", cid)

	// A promotional message is refused at the consent guard, not sent.
	if st, g := w.svc.crmDispatchAt(ctx, "E-07", cid, nil, istDayAt("2026-10-06", 11, 0)); st != "SUPPRESSED" || g != "G2_consent" {
		t.Fatalf("E-07 without an explicit grant: %s %s", st, g)
	}

	// 4) Only the member's own grant opens it.
	if aerr := w.svc.applyConsents(ctx, cid, []consentInput{{Type: "marketing_push", Granted: &yes, Version: "2026-07"}}); aerr != nil {
		t.Fatalf("explicit grant: %v", aerr)
	}
	if !w.svc.crmHasPromoConsent(ctx, cid) || !w.svc.crmHasChannelConsent(ctx, cid, "push") {
		t.Fatal("an explicit marketing_push grant must open the promotional gate for push")
	}
	if w.svc.crmHasChannelConsent(ctx, cid, "whatsapp") || w.svc.crmHasChannelConsent(ctx, cid, "sms") {
		t.Fatal("a push grant must not open WhatsApp or SMS")
	}
}
