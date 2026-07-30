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
	// Manual "close now" during open hours → shut, resumes at the next open (tomorrow 7AM).
	zp := zone{InstantOpenMin: 420, InstantCloseMin: 1320, InstantPaused: true}
	if open, label, _ := instantWindow(zp, istAt(10, 0)); open || label != "tomorrow at 7:00 AM" {
		t.Fatalf("paused@10:00 should be closed, resume tomorrow 7AM; got open=%v label=%q", open, label)
	}
	// No hours configured (close==0) → 24h instant unless manually paused.
	if open, _, _ := instantWindow(zone{}, istAt(3, 0)); !open {
		t.Fatalf("no-hours zone should be 24h open; got closed at 03:00")
	}
	// No hours + paused → closed, default resume 07:00 IST.
	if open, label, _ := instantWindow(zone{InstantPaused: true}, istAt(3, 0)); open || label != "today at 7:00 AM" {
		t.Fatalf("no-hours paused@03:00 should resume today 7AM; got open=%v label=%q", open, label)
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

func TestEffHours(t *testing.T) {
	if effOpenMin(&zone{}) != 420 || effCloseMin(&zone{}) != 1320 {
		t.Fatalf("unconfigured zone should default to 07:00–22:00 (420–1320)")
	}
	z := &zone{InstantOpenMin: 480, InstantCloseMin: 1200}
	if effOpenMin(z) != 480 || effCloseMin(z) != 1200 {
		t.Fatalf("configured zone should return its own hours")
	}
}
