package platformops

import (
	"strings"
	"testing"

	"github.com/pyaas/saathi-backend/internal/domain"
)

// TestRenderSMSPourReceiptIncludesQuantity locks the producer↔renderer param
// contract: the collection module queues the pour receipt with the key
// "quantity", and the renderer must surface the litres — the one number a
// low-literacy farmer needs to verify against the analyzer display.
func TestRenderSMSPourReceiptIncludesQuantity(t *testing.T) {
	// Exactly the param keys collection.announcePour produces.
	params := map[string]string{
		"quantity": "10.50",
		"rate":     "52.75",
		"amount":   "553.88",
	}
	english, hindi := renderSMS(domain.TemplatePourReceipt, params)
	if !strings.Contains(english, "10.50") {
		t.Fatalf("english pour receipt %q missing litres 10.50", english)
	}
	if !strings.Contains(hindi, "10.50") {
		t.Fatalf("hindi pour receipt %q missing litres 10.50", hindi)
	}
	if !strings.Contains(english, "52.75") || !strings.Contains(english, "553.88") {
		t.Fatalf("english pour receipt %q missing rate/amount", english)
	}
}

// TestRenderSMSKYCLines locks the KYC outcome templates: an approval must
// surface the unlocked tier, a rejection the reason — the two facts the party
// needs to act (retry role login, or fix and resubmit).
func TestRenderSMSKYCLines(t *testing.T) {
	en, hi := renderSMS(domain.TemplateKYCApproved, map[string]string{"tier": "FARMER"})
	if !strings.Contains(en, "FARMER") {
		t.Fatalf("english KYC approval %q missing tier FARMER", en)
	}
	if !strings.Contains(hi, "FARMER") {
		t.Fatalf("hindi KYC approval %q missing tier FARMER", hi)
	}

	en, hi = renderSMS(domain.TemplateKYCRejected, map[string]string{"reason": "penny-drop name mismatch"})
	if !strings.Contains(en, "penny-drop name mismatch") {
		t.Fatalf("english KYC rejection %q missing reason", en)
	}
	if !strings.Contains(hi, "penny-drop name mismatch") {
		t.Fatalf("hindi KYC rejection %q missing reason", hi)
	}
}

// TestMVUDispatchedPayloadContract round-trips the exact payload cattle
// publishes on eventbus.TopicMVUDispatched through the structural decode and
// asserts the case ID lands — the farmer SMS must never render "(case )".
func TestMVUDispatchedPayloadContract(t *testing.T) {
	published := map[string]any{
		"case_id":         "case-123",
		"farmer_party_id": "party-farmer-mahesh",
		"animal_id":       "animal-gomti",
		"dcs_id":          "org-dcs-1",
	}
	var event mvuDispatchedEvent
	if err := decodeBusPayload(published, &event); err != nil {
		t.Fatalf("decodeBusPayload: %v", err)
	}
	caseID := event.CaseID
	if caseID == "" {
		caseID = event.MVUCaseID
	}
	if caseID == "" {
		caseID = event.ID
	}
	if caseID != "case-123" {
		t.Fatalf("case id = %q, want case-123", caseID)
	}
	if event.FarmerPartyID != "party-farmer-mahesh" {
		t.Fatalf("farmer_party_id = %q", event.FarmerPartyID)
	}

	// Tolerance fallback: a raw domain document (ID under "id") still yields
	// a non-empty case reference.
	var fromRaw mvuDispatchedEvent
	if err := decodeBusPayload(map[string]any{"id": "case-raw"}, &fromRaw); err != nil {
		t.Fatalf("decodeBusPayload raw: %v", err)
	}
	got := fromRaw.CaseID
	if got == "" {
		got = fromRaw.MVUCaseID
	}
	if got == "" {
		got = fromRaw.ID
	}
	if got != "case-raw" {
		t.Fatalf("raw-document fallback case id = %q, want case-raw", got)
	}
}

// TestRedactNotificationSecrets ensures the login OTP can never be read back
// through the notifications surface — neither via params nor rendered meta.
func TestRedactNotificationSecrets(t *testing.T) {
	n := StoredNotification{
		Notification: domain.Notification{
			TemplateKey: domain.TemplateOTP,
			Params:      map[string]string{"otp": "123456"},
		},
		Meta: map[string]any{
			"provider":    "SMS-MOCK",
			"rendered_en": "Your Saathi OTP is 123456. Do not share it with anyone.",
			"rendered_hi": "आपका साथी OTP 123456 है।",
		},
	}
	redactNotificationSecrets(&n)
	if n.Params["otp"] != redactedValue {
		t.Fatalf("params.otp = %q, want redacted", n.Params["otp"])
	}
	if n.Meta["rendered_en"] != redactedValue || n.Meta["rendered_hi"] != redactedValue {
		t.Fatalf("rendered meta not redacted: %#v", n.Meta)
	}
	if n.Meta["provider"] != "SMS-MOCK" {
		t.Fatalf("non-secret meta must survive redaction, got %#v", n.Meta)
	}

	// Non-OTP templates pass through untouched.
	receipt := StoredNotification{
		Notification: domain.Notification{
			TemplateKey: domain.TemplatePourReceipt,
			Params:      map[string]string{"quantity": "10.50"},
		},
	}
	redactNotificationSecrets(&receipt)
	if receipt.Params["quantity"] != "10.50" {
		t.Fatalf("pour receipt params must not be redacted, got %#v", receipt.Params)
	}
}
