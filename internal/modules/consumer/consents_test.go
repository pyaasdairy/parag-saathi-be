package consumer

import (
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

// The closed enum: every shipped type passes, anything else is a 400 before a
// single byte is written.
func TestConsentValidateClosedEnum(t *testing.T) {
	now := time.Now().UTC()
	for typ := range consentTypes {
		items := []consentInput{{Type: typ, Granted: boolPtr(true)}}
		if err := validateConsentInputs(items, now); err != nil {
			t.Fatalf("valid type %q rejected: %v", typ, err)
		}
	}
	for _, typ := range []string{"", "marketing_fax", "promotional", "PRIVACY_TERMS", "marketing_whatsapp "} {
		items := []consentInput{{Type: typ, Granted: boolPtr(true)}}
		err := validateConsentInputs(items, now)
		if err == nil || err.status != 400 {
			t.Fatalf("type %q must be a 400, got %v", typ, err)
		}
	}
	// "promotional" is the guard's internal aggregate — the app must never be
	// able to write it directly.
	if consentTypes[consentKindPromo] {
		t.Fatal("the derived promotional kind must not be an accepted input type")
	}
}

func TestConsentValidateRequiredFields(t *testing.T) {
	now := time.Now().UTC()
	if err := validateConsentInputs(nil, now); err == nil || err.status != 400 {
		t.Fatalf("empty batch must be a 400, got %v", err)
	}
	// Absent granted must be a 400 — it must NEVER default to a silent revoke
	// (nor a silent grant).
	items := []consentInput{{Type: "marketing_sms"}}
	if err := validateConsentInputs(items, now); err == nil || err.status != 400 {
		t.Fatalf("missing granted must be a 400, got %v", err)
	}
	// An oversized batch is a 400 before any write — the write amplification
	// cap (every row costs an audit insert plus state updates).
	big := make([]consentInput, maxConsentBatch+1)
	for i := range big {
		big[i] = consentInput{Type: "marketing_sms", Granted: boolPtr(true)}
	}
	if err := validateConsentInputs(big, now); err == nil || err.status != 400 {
		t.Fatalf("oversized batch must be a 400, got %v", err)
	}
	// One bad row poisons the whole batch (validated before any write).
	items = []consentInput{
		{Type: "marketing_sms", Granted: boolPtr(true)},
		{Type: "nope", Granted: boolPtr(true)},
	}
	if err := validateConsentInputs(items, now); err == nil || err.status != 400 {
		t.Fatalf("batch with one unknown type must be a 400, got %v", err)
	}
}

func TestConsentValidateTimestampNormalization(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	items := []consentInput{
		{Type: "privacy_terms", Granted: boolPtr(true)},                                     // zero → now
		{Type: "marketing_sms", Granted: boolPtr(true), OccurredAt: now.Add(2 * time.Hour)}, // future → clamped
		{Type: "marketing_push", Granted: boolPtr(true), OccurredAt: now.Add(-time.Hour)},   // past → kept
	}
	if err := validateConsentInputs(items, now); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !items[0].OccurredAt.Equal(now) {
		t.Fatalf("zero occurred_at must default to server now, got %v", items[0].OccurredAt)
	}
	// A skewed client clock must never push the TTL anchor into the future
	// (that would both extend the promo window and wedge later revokes).
	if !items[1].OccurredAt.Equal(now) {
		t.Fatalf("future occurred_at must clamp to server now, got %v", items[1].OccurredAt)
	}
	if !items[2].OccurredAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("past occurred_at must be preserved, got %v", items[2].OccurredAt)
	}
}
