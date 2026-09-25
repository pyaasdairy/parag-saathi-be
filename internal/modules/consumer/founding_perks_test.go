package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

// The member view says whether the perks apply today (additive
// perks_active), exactly as orders are billed: an active member, and a
// stopped or re-joined member inside the month already paid. The app reads
// it to show "Delivery charge FREE" to the members the server bills nothing.
func TestFoundingMemberViewSaysWhetherThePerksApply(t *testing.T) {
	const day = "2026-10-06"
	for _, c := range []struct {
		name string
		m    foundingMember
		want bool
	}{
		{"active", foundingMember{Status: memberActive, LineNumber: 1}, true},
		{"waiting", foundingMember{Status: memberWaiting, LineNumber: 2}, false},
		{"waiting inside a paid month", foundingMember{Status: memberWaiting, PerksUntil: "2026-10-10"}, true},
		{"stopped, last paid day", foundingMember{Status: memberStopped, PerksUntil: day}, true},
		{"stopped, month over", foundingMember{Status: memberStopped, PerksUntil: "2026-10-05"}, false},
		{"stopped while waiting", foundingMember{Status: memberStopped}, false},
	} {
		if got := memberView(&c.m, "", day).PerksActive; got != c.want {
			t.Errorf("%s: perks_active %v want %v", c.name, got, c.want)
		}
	}

	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	h := &handler{svc: w.svc}
	seedTestFarms(t, w, 1)
	a := w.customer(t, "9000019301", 300)
	code, body := foundingAPI(t, w, a, http.MethodPost, "/founding-family/join", `{"farm_id":"gonard-dairy"}`, h.foundingJoin)
	if code != 200 {
		t.Fatalf("join: %d %s", code, body)
	}
	var joined struct {
		Member map[string]any `json:"member"`
	}
	_ = json.Unmarshal([]byte(body), &joined)
	if joined.Member["perks_active"] != true {
		t.Fatalf("join answer of an active member: %v", joined.Member)
	}
	code, body = foundingAPI(t, w, a, http.MethodGet, "/founding-family", "", h.foundingView)
	var view struct {
		Member map[string]any `json:"member"`
	}
	_ = json.Unmarshal([]byte(body), &view)
	if code != 200 || view.Member["perks_active"] != true {
		t.Fatalf("view of an active member: %d %s", code, body)
	}
	// Stopped: the paid month still runs, so the perks still apply.
	code, body = foundingAPI(t, w, a, http.MethodPost, "/founding-family/stop", "", h.foundingStop)
	_ = json.Unmarshal([]byte(body), &joined)
	if code != 200 || joined.Member["status"] != memberStopped || joined.Member["perks_active"] != true {
		t.Fatalf("stopped inside the paid month: %d %s", code, body)
	}
	// Once the paid month is over they do not.
	m, _ := w.svc.repo.findFoundingMember(ctx, a)
	if _, err := w.db.Collection(collFoundingMembers).UpdateByID(ctx, m.ID, bson.D{{Key: "$set", Value: bson.D{{Key: "perks_until", Value: "2026-01-01"}}}}); err != nil {
		t.Fatalf("age the month: %v", err)
	}
	_, body = foundingAPI(t, w, a, http.MethodGet, "/founding-family", "", h.foundingView)
	_ = json.Unmarshal([]byte(body), &view)
	if view.Member["perks_active"] != false {
		t.Fatalf("after the paid month: %s", body)
	}
}

// RV-PR-10: a one-off order is judged by the member's standing on the day it
// is DELIVERED, on the order's own clock (createOrderAt's at), not by today's
// wall clock: the One Voice fee, the level-3 price and the members-only gate
// all read it. A stopped member whose paid month ends on D pays the Rs 5 and
// level 1 on a morning after D, and nothing extra on a morning inside it; an
// instant order goes out today, so today decides.
func TestOneOffOrderIsJudgedOnItsDeliveryDay(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedPyaasToned(t, w)
	seedTestFarms(t, w, 1)
	const D = "2026-10-06"
	at := istDayAt(D, 9, 0) // before noon: the first open morning is D+1
	m := w.customer(t, "9000019311", 1000)
	if _, err := w.svc.joinFoundingFamily(ctx, m, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	perksUntil := func(day string) {
		t.Helper()
		if _, err := w.db.Collection(collFoundingMembers).UpdateOne(ctx, bson.D{{Key: "consumer_id", Value: m}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: memberStopped}, {Key: "perks_until", Value: day}}}}); err != nil {
			t.Fatalf("perks_until: %v", err)
		}
	}
	place := func(lane, date string, items ...orderItem) (*order, error) {
		t.Helper()
		return w.svc.createOrderAt(ctx, m.Hex(), orderInput{
			Items: items, PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
			Lane: lane, DeliveryDate: date,
		}, at)
	}
	gold := orderItem{ProductID: "gold-500ml", Qty: 1, Price: 35}
	pyaas := orderItem{ProductID: "pyaas-toned-1l", Qty: 1, Price: 85}
	mustPlace := func(lane, date string, items ...orderItem) *order {
		t.Helper()
		o, err := place(lane, date, items...)
		if err != nil {
			t.Fatalf("order %s %s: %v", lane, date, err)
		}
		return o
	}

	perksUntil(D) // the paid month ends today
	if o := mustPlace("morning", addDaysIST(D, 3), gold); o.DeliveryDate != addDaysIST(D, 3) || o.DeliveryFee != 5 || o.Total != 40 {
		t.Fatalf("a morning 3 days after the paid month: date %s fee %v total %v, want Rs 5", o.DeliveryDate, o.DeliveryFee, o.Total)
	}
	if o := mustPlace("morning", "", pyaas); o.DeliveryDate != addDaysIST(D, 1) || o.Items[0].Price != 85 || o.DeliveryFee != 5 {
		t.Fatalf("tomorrow's morning after the paid month: date %s line %v fee %v, want level 1 and Rs 5", o.DeliveryDate, o.Items[0].Price, o.DeliveryFee)
	}
	if o := mustPlace("instant", "", gold); o.DeliveryFee != 0 || o.Total != 35 {
		t.Fatalf("an instant order today, inside the paid month: fee %v total %v, want free", o.DeliveryFee, o.Total)
	}

	perksUntil(addDaysIST(D, 3)) // the month runs through D+3
	if o := mustPlace("morning", addDaysIST(D, 3), pyaas); o.Items[0].Price != 83 || o.DeliveryFee != 0 || o.Total != 83 {
		t.Fatalf("a morning on the last paid day: line %v fee %v total %v, want level 3 and free", o.Items[0].Price, o.DeliveryFee, o.Total)
	}

	// Members-only on: PYAAS milk for a morning the perks do not cover is
	// refused; one they cover is not.
	w.svc.deps.Cfg.FoundingPyaasMembersOnly = true
	defer func() { w.svc.deps.Cfg.FoundingPyaasMembersOnly = false }()
	_, err := place("morning", addDaysIST(D, 4), pyaas)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "FOUNDING_REQUIRED" {
		t.Fatalf("PYAAS milk for a morning after the paid month: %v, want FOUNDING_REQUIRED", err)
	}
	if _, err := place("morning", addDaysIST(D, 2), pyaas); err != nil {
		t.Fatalf("PYAAS milk for a morning inside the paid month: %v", err)
	}
}
