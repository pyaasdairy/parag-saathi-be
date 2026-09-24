package consumer

import (
	"testing"
	"time"
)

// istAt builds a fixed IST wall-clock time so the instant-hours logic is tested
// deterministically (instantWindow evaluates "now" in IST).
func istAt(hour, min int) time.Time {
	return time.Date(2026, 7, 31, hour, min, 0, 0, istZone)
}

func TestInstantWindow_Hours(t *testing.T) {
	// Store instant hours 07:00–22:00 IST, not paused.
	z := zone{InstantOpenMin: 420, InstantCloseMin: 1320}

	if open, label, at := instantWindow(z, istAt(10, 0)); !open || label != "" || at != "" {
		t.Fatalf("10:00 within hours should be OPEN; got open=%v label=%q at=%q", open, label, at)
	}
	if open, label, _ := instantWindow(z, istAt(5, 0)); open || label != "today at 7:00 AM" {
		t.Fatalf("05:00 (before open) should resume today 7AM; got open=%v label=%q", open, label)
	}
	if open, label, _ := instantWindow(z, istAt(23, 0)); open || label != "tomorrow at 7:00 AM" {
		t.Fatalf("23:00 (after close) should resume tomorrow 7AM; got open=%v label=%q", open, label)
	}
}

func TestInstantWindow_PausedAndNoHours(t *testing.T) {
	// The manager's pause ("Close instant now" in the Zone tab) holds until they
	// turn it off: it does NOT end at the next opening time, so the label names no
	// time (the consumer app reads "Instant resumes when the store turns it back on").
	zp := zone{InstantOpenMin: 420, InstantCloseMin: 1320, InstantPaused: true}
	for _, at := range []time.Time{istAt(10, 0), istAt(23, 0), istAt(7, 0)} {
		if open, label, resumesAt := instantWindow(zp, at); open || label != pausedResumesLabel || resumesAt != "" {
			t.Fatalf("paused@%s: open=%v label=%q at=%q", at.Format("15:04"), open, label, resumesAt)
		}
	}
	if pausedResumesLabel != "when the store turns it back on" {
		t.Fatalf("paused label: %q", pausedResumesLabel)
	}
	// No hours saved (close==0) → the 07:00-22:00 the console shows, so 03:00 is shut.
	if open, label, _ := instantWindow(zone{}, istAt(3, 0)); open || label != "today at 7:00 AM" {
		t.Fatalf("no-hours zone@03:00 should be closed, resume today 7AM; got open=%v label=%q", open, label)
	}
	// No hours + paused → closed until the manager turns it back on.
	if open, label, _ := instantWindow(zone{InstantPaused: true}, istAt(3, 0)); open || label != pausedResumesLabel {
		t.Fatalf("no-hours paused@03:00: open=%v label=%q", open, label)
	}
	// An extension never reopens a paused lane.
	ext := istAt(23, 0)
	if open, _, _ := instantWindow(zone{InstantPaused: true, InstantExtendedUntil: &ext}, istAt(22, 30)); open {
		t.Fatalf("paused + extension should stay closed")
	}
}

func TestInstantWindow_Overnight(t *testing.T) {
	// Evening store: open 22:00, close 06:00 (window crosses midnight).
	z := zone{InstantOpenMin: 1320, InstantCloseMin: 360}
	if open, _, _ := instantWindow(z, istAt(23, 30)); !open {
		t.Fatalf("23:30 in an overnight window should be open")
	}
	if open, _, _ := instantWindow(z, istAt(2, 0)); !open {
		t.Fatalf("02:00 in an overnight window should be open")
	}
	if open, label, _ := instantWindow(z, istAt(12, 0)); open || label != "today at 10:00 PM" {
		t.Fatalf("noon should be closed, resume today 10PM; got open=%v label=%q", open, label)
	}
}

// A zone whose hours were never saved (instant_close_min 0) keeps the hours the
// store console shows for it, 07:00-22:00 IST, instead of an invisible 24 h.
func TestInstantWindow_UnsavedHoursAreTheConsoleHours(t *testing.T) {
	z := zone{InstantRadiusM: 2500, StandardRadiusM: 8000}
	for _, c := range []struct {
		h, m  int
		open  bool
		label string
	}{
		{6, 59, false, "today at 7:00 AM"},
		{7, 0, true, ""},
		{21, 59, true, ""},
		{22, 0, false, "tomorrow at 7:00 AM"},
		{22, 1, false, "tomorrow at 7:00 AM"},
		{23, 59, false, "tomorrow at 7:00 AM"},
	} {
		open, label, at := instantWindow(z, istAt(c.h, c.m))
		if open != c.open || label != c.label {
			t.Fatalf("%02d:%02d: open=%v label=%q, want open=%v label=%q", c.h, c.m, open, label, c.open, c.label)
		}
		if !c.open && at == "" {
			t.Fatalf("%02d:%02d: a closed lane names the moment it resumes", c.h, c.m)
		}
	}
	// What the console shows is what is enforced.
	if effOpenMin(&z) != 420 || effCloseMin(&z) != 1320 {
		t.Fatalf("console hours for an unsaved zone: %d-%d", effOpenMin(&z), effCloseMin(&z))
	}
}

// Saved hours are enforced exactly as before, and a store that wants instant
// round the clock saves 00:00-24:00 (0..1440), which the PUT already accepts.
func TestInstantWindow_SavedHoursUnchanged(t *testing.T) {
	z := zone{InstantOpenMin: 480, InstantCloseMin: 1200} // 08:00-20:00
	if open, _, _ := instantWindow(z, istAt(19, 59)); !open {
		t.Fatalf("19:59 inside saved 08:00-20:00 should be open")
	}
	if open, label, _ := instantWindow(z, istAt(20, 1)); open || label != "tomorrow at 8:00 AM" {
		t.Fatalf("20:01 after saved close: open=%v label=%q", open, label)
	}
	allDay := zone{InstantOpenMin: 0, InstantCloseMin: 1440}
	for _, hm := range [][2]int{{0, 0}, {3, 0}, {22, 1}, {23, 59}} {
		if open, _, _ := instantWindow(allDay, istAt(hm[0], hm[1])); !open {
			t.Fatalf("saved 00:00-24:00 should be open at %02d:%02d", hm[0], hm[1])
		}
	}
}

func TestEffHours(t *testing.T) {
	if effOpenMin(&zone{}) != 420 || effCloseMin(&zone{}) != 1320 {
		t.Fatalf("unconfigured zone should default to 07:00–22:00 (420–1320)")
	}
	z := &zone{InstantOpenMin: 480, InstantCloseMin: 1200}
	if effOpenMin(z) != 480 || effCloseMin(z) != 1200 {
		t.Fatalf("configured zone should return its own hours")
	}
}
