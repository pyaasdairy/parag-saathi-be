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
	// A pause with no end ("Close instant now" in the Zone tab, switched on
	// before pauses carried one) holds until they turn it off: it does NOT end at
	// the next opening time, so the label names no time (the consumer app reads
	// "Instant resumes when the store turns it back on").
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

// After a close-now, "resumes …" names the moment instant really comes back:
// the close-now's end when the hours are open then, else the first opening
// after it. Before: the label was worked out from the next opening after NOW,
// so hours moved earlier than the close-now's end named a day-off opening
// ("tomorrow at 6:00 AM" at 06:30 when instant reopens at 07:00 today).
func TestInstantWindow_CloseNowResumesWhenItEnds(t *testing.T) {
	jul := func(day, h, m int) time.Time { return time.Date(2026, 7, day, h, m, 0, 0, istZone) }
	until := jul(31, 7, 0) // close-now at 21:00 on the 30th, hours 07:00-22:00: shut until 07:00 on the 31st
	wire := until.UTC().Format(time.RFC3339)
	for _, c := range []struct {
		name  string
		open  int
		at    time.Time
		label string
		when  string
	}{
		{"hours unchanged, that night", 420, jul(30, 21, 30), "tomorrow at 7:00 AM", wire},
		{"hours unchanged, small hours", 420, jul(31, 6, 30), "today at 7:00 AM", wire},
		{"opening moved to 06:00, that night", 360, jul(30, 23, 0), "tomorrow at 7:00 AM", wire},
		{"opening moved to 06:00, at 06:30", 360, jul(31, 6, 30), "today at 7:00 AM", wire},
		{"opening moved to 08:00, at 06:30", 480, jul(31, 6, 30), "today at 8:00 AM", jul(31, 8, 0).UTC().Format(time.RFC3339)},
	} {
		z := zone{InstantOpenMin: c.open, InstantCloseMin: 1320, InstantClosedUntil: &until}
		open, label, at := instantWindow(z, c.at)
		if open || label != c.label || at != c.when {
			t.Fatalf("%s: open=%v label=%q at=%q, want closed %q at %q", c.name, open, label, at, c.label, c.when)
		}
	}
	// And it does come back at that moment.
	z := zone{InstantOpenMin: 360, InstantCloseMin: 1320, InstantClosedUntil: &until}
	if open, _, _ := instantWindow(z, until); !open {
		t.Fatalf("07:00 on the 31st, hours 06:00-22:00: the close-now has ended, instant is open")
	}
}

// A pause switched on through the console carries its end, the next opening
// time (instant_pause_test.go): "resumes …" names that moment, and at that
// moment the pause is over and the hours decide again. A pause with no end
// (stored before ends were) keeps its old meaning: TestInstantWindow_PausedAndNoHours.
func TestInstantWindow_PauseEndsAtItsEnd(t *testing.T) {
	jul := func(day, h, m int) time.Time { return time.Date(2026, 7, day, h, m, 0, 0, istZone) }
	until := jul(31, 7, 0) // switched on at 21:00 on the 30th, hours 07:00-22:00
	wire := until.UTC().Format(time.RFC3339)
	z := zone{InstantOpenMin: 420, InstantCloseMin: 1320, InstantPaused: true, InstantPausedUntil: &until}
	for _, c := range []struct {
		at    time.Time
		label string
	}{{jul(30, 21, 30), "tomorrow at 7:00 AM"}, {jul(31, 6, 59), "today at 7:00 AM"}} {
		if open, label, at := instantWindow(z, c.at); open || label != c.label || at != wire {
			t.Fatalf("paused until 07:00, at %s: open=%v label=%q at=%q", c.at.Format("01-02 15:04"), open, label, at)
		}
		if !instantPausedAt(&z, c.at) {
			t.Fatalf("paused until 07:00, at %s: not paused", c.at.Format("01-02 15:04"))
		}
	}
	if open, _, _ := instantWindow(z, until); !open || instantPausedAt(&z, until) {
		t.Fatalf("07:00: the pause has ended, the hours are open")
	}
	// Ended outside the hours (the opening moved later since): the next opening.
	late := z
	late.InstantOpenMin = 540
	if open, label, at := instantWindow(late, jul(31, 8, 0)); open || label != "today at 9:00 AM" || at != jul(31, 9, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("pause ended at 07:00, opens 09:00, at 08:00: open=%v label=%q at=%q", open, label, at)
	}
	if open, label, _ := instantWindow(late, jul(30, 23, 0)); open || label != "tomorrow at 9:00 AM" {
		t.Fatalf("paused until 07:00, opens 09:00, at 23:00: open=%v label=%q", open, label)
	}
	// With a close-now as well, instant is back when the later of the two ends.
	closed := jul(31, 8, 0)
	both := z
	both.InstantClosedUntil = &closed
	if open, label, _ := instantWindow(both, jul(31, 6, 0)); open || label != "today at 8:00 AM" {
		t.Fatalf("paused until 07:00 + closed until 08:00: open=%v label=%q", open, label)
	}
	pausedLater := jul(31, 9, 0)
	both.InstantPausedUntil = &pausedLater
	if open, label, _ := instantWindow(both, jul(31, 6, 0)); open || label != "today at 9:00 AM" {
		t.Fatalf("paused until 09:00 + closed until 08:00: open=%v label=%q", open, label)
	}
	// An extension never reopens a pause that is still on.
	ext := jul(30, 23, 0)
	withExt := z
	withExt.InstantExtendedUntil = &ext
	if open, _, _ := instantWindow(withExt, jul(30, 22, 30)); open {
		t.Fatalf("paused until 07:00 + extension to 23:00 should stay closed at 22:30")
	}
	// Switched off, whatever end is stored, is not a pause.
	off := zone{InstantOpenMin: 420, InstantCloseMin: 1320, InstantPausedUntil: &until}
	if instantPausedAt(&off, jul(30, 21, 30)) {
		t.Fatalf("switch off: not paused")
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
