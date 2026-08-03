package consumer

import (
	"testing"
	"time"
)

// The cadence law must be IDENTICAL to the FE (lib/subscriptions.ts):
// daily every day, alternate every 2nd day, weekly every 7th, from start_date.
func TestSubscriptionDeliversOn(t *testing.T) {
	cases := []struct {
		freq, start, day string
		want             bool
	}{
		// daily — every day from start
		{"daily", "2026-08-04", "2026-08-04", true},
		{"daily", "2026-08-04", "2026-08-05", true},
		{"daily", "2026-08-04", "2026-08-03", false}, // before start
		// alternate — d % 2 == 0
		{"alternate", "2026-08-04", "2026-08-04", true},
		{"alternate", "2026-08-04", "2026-08-05", false},
		{"alternate", "2026-08-04", "2026-08-06", true},
		{"alternate", "2026-08-04", "2026-08-10", true},
		{"alternate", "2026-08-04", "2026-08-11", false},
		// weekly — d % 7 == 0
		{"weekly", "2026-08-04", "2026-08-04", true},
		{"weekly", "2026-08-04", "2026-08-10", false},
		{"weekly", "2026-08-04", "2026-08-11", true},
		{"weekly", "2026-08-04", "2026-08-18", true},
		// month boundary (31-day month)
		{"alternate", "2026-08-30", "2026-09-01", true},
		{"weekly", "2026-08-28", "2026-09-04", true},
		// unknown frequency never delivers
		{"custom", "2026-08-04", "2026-08-04", false},
		// malformed dates never deliver
		{"daily", "not-a-date", "2026-08-04", false},
		{"daily", "2026-08-04", "nope", false},
	}
	for _, c := range cases {
		if got := subscriptionDeliversOn(c.freq, c.start, c.day); got != c.want {
			t.Errorf("deliversOn(%s, start %s, day %s) = %v, want %v", c.freq, c.start, c.day, got, c.want)
		}
	}
}

func TestSubscriptionDueOnVacations(t *testing.T) {
	sub := &subscription{Status: "active", Frequency: "daily", StartDate: "2026-08-01",
		Vacations: []vacationRange{
			{Start: "2026-08-10", End: "2026-08-12"}, // range pause
			{Start: "2026-08-20", End: "2026-08-20"}, // one-day skip
		}}
	cases := []struct {
		day  string
		want bool
	}{
		{"2026-08-09", true},
		{"2026-08-10", false}, // vacation start
		{"2026-08-11", false}, // inside
		{"2026-08-12", false}, // vacation end (inclusive)
		{"2026-08-13", true},
		{"2026-08-20", false}, // skip day
		{"2026-08-21", true},
	}
	for _, c := range cases {
		if got := subscriptionDueOn(sub, c.day); got != c.want {
			t.Errorf("dueOn(%s) = %v, want %v", c.day, got, c.want)
		}
	}
	// A paused/cancelled subscription is never due.
	for _, st := range []string{"paused", "cancelled"} {
		s2 := *sub
		s2.Status = st
		if subscriptionDueOn(&s2, "2026-08-09") {
			t.Errorf("a %s subscription must never be due", st)
		}
	}
}

// The IST day key: 23:00 UTC is ALREADY the next civil day in India (04:30 IST)
// — the worker must order for the Indian morning, never the UTC date.
func TestISTToday(t *testing.T) {
	utc := time.Date(2026, 8, 4, 23, 0, 0, 0, time.UTC)
	if got := istToday(utc); got != "2026-08-05" {
		t.Errorf("istToday(23:00 UTC Aug 4) = %s, want 2026-08-05", got)
	}
	utcMorning := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	if got := istToday(utcMorning); got != "2026-08-04" {
		t.Errorf("istToday(10:00 UTC Aug 4) = %s, want 2026-08-04", got)
	}
}

func TestSubscriptionTransitions(t *testing.T) {
	legal := []struct{ from, to string }{
		{"active", "paused"}, {"active", "cancelled"},
		{"paused", "active"}, {"paused", "cancelled"},
	}
	for _, c := range legal {
		if !subscriptionTransitions[c.from][c.to] {
			t.Errorf("%s→%s should be legal", c.from, c.to)
		}
	}
	// cancelled is terminal
	for _, to := range []string{"active", "paused"} {
		if subscriptionTransitions["cancelled"][to] {
			t.Errorf("cancelled→%s must be illegal", to)
		}
	}
}

// The schedule phase computes "tomorrow" on the IST calendar — month and year
// boundaries must roll correctly.
func TestAddDaysIST(t *testing.T) {
	cases := []struct{ day, want string }{
		{"2026-08-04", "2026-08-05"},
		{"2026-08-31", "2026-09-01"}, // month roll
		{"2026-12-31", "2027-01-01"}, // year roll
		{"2028-02-28", "2028-02-29"}, // leap day
	}
	for _, c := range cases {
		if got := addDaysIST(c.day, 1); got != c.want {
			t.Errorf("addDaysIST(%s, 1) = %s, want %s", c.day, got, c.want)
		}
	}
	if got := addDaysIST("garbage", 1); got != "garbage" {
		t.Errorf("addDaysIST must pass through an unparseable day, got %s", got)
	}
}

// The mandate plan map must now carry all three cadences (the FE parity gap).
func TestMandatePlansHaveAlternate(t *testing.T) {
	want := map[string]time.Duration{
		"daily":     24 * time.Hour,
		"alternate": 48 * time.Hour,
		"weekly":    7 * 24 * time.Hour,
	}
	for plan, d := range want {
		if mandatePlans[plan] != d {
			t.Errorf("mandatePlans[%s] = %v, want %v", plan, mandatePlans[plan], d)
		}
	}
}
