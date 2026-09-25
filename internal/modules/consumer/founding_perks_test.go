package consumer

import (
	"context"
	"encoding/json"
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
