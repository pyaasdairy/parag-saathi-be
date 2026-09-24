package consumer

// G9 copy (owner, 24 Sep: "any server copy that says 'tomorrow' but means
// another day must name the real day"): W-01, the Welcome Litre
// confirmation, says "Tomorrow by 7 am: 500 ml ... free". From 12:00 IST
// pack 1 comes the day after tomorrow, so an afternoon enrolment is told
// the real morning ("8 Oct by 7 am") by a variant body, T-W01-LATER. Its
// wording is not registered with DLT or Meta, so it goes to the inbox (and
// push) only; the registered "tomorrow" body is never sent for a pack that
// does not come tomorrow.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run W01NamesThePack1Morning -v

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMW01NamesThePack1Morning(t *testing.T) {
	clearDryRunEnv(t)
	stub := newStubProvider(t, `{"type":"success","request_id":"dry-run"}`)
	t.Setenv("CRM_MSG91_AUTHKEY", "dry-run-key")
	t.Setenv("CRM_DLT_TEMPLATE_IDS", `{"W-01":"1207160000000000001"}`)
	t.Setenv("CRM_MSG91_BASE_URL", stub.srv.URL)
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"

	enrol := func(phone, line string, hour int) primitive.ObjectID {
		t.Helper()
		at := istDayAt(D, hour, 0)
		res, err := w.svc.crmEnrolAt(ctx, "w01-operator", crmEnrolInput{
			Phone: phone, Name: "W01 Household", Line1: line, Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
		}, at)
		if err != nil {
			t.Fatalf("enrol at %d:00: %v", hour, err)
		}
		cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)
		w.svc.crmProcessEventsAt(ctx, at.Add(60e9)) // the worker's next minute
		return cid
	}
	body := func(cid primitive.ObjectID) (string, string) {
		t.Helper()
		var row struct {
			EN       string `bson:"body_en"`
			Template string `bson:"template_id"`
		}
		if err := w.db.Collection(collConsumerInbox).FindOne(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "W-01"}}).Decode(&row); err != nil {
			t.Fatalf("W-01 inbox row: %v", err)
		}
		if tok := crmTokenRe.FindString(row.EN); tok != "" {
			t.Fatalf("W-01 left %s unresolved: %q", tok, row.EN)
		}
		return row.Template, row.EN
	}

	morning := enrol("9000015601", "Flat 1, W01 Tower", 10)
	if tpl, en := body(morning); tpl != "T-W01" || !strings.HasPrefix(en, "You're set. Tomorrow by 7 am: 500 ml Parag Full Cream") {
		t.Fatalf("a morning enrolment's W-01: %s %q", tpl, en)
	}
	stub.only(t) // the registered body went out by SMS

	afternoon := enrol("9000015602", "Flat 2, W01 Tower", 14)
	tpl, en := body(afternoon)
	if tpl != "T-W01-LATER" || !strings.HasPrefix(en, "You're set. 8 Oct by 7 am: 500 ml Parag Full Cream") {
		t.Fatalf("an afternoon enrolment's W-01 must name the day pack 1 comes: %s %q", tpl, en)
	}
	if strings.Contains(strings.ToLower(en), "tomorrow") {
		t.Fatalf("an afternoon enrolment's W-01 says tomorrow: %q", en)
	}
	stub.only(t) // still one: the variant is not registered, so no SMS carries it
	if got := crmDispatchStatuses(t, w.db, afternoon, "W-01"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("the afternoon W-01 dispatch: %v", got)
	}
}
