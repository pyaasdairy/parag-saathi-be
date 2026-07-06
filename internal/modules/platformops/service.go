package platformops

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// exportMaxRows caps the immutable audit export (§12) — bounded by contract.
const exportMaxRows = 5000

// workerClaimCap bounds one mock-SMS worker run to 100 claims.
const workerClaimCap = 100

// settableFlagKeys is the closed set of PUT-able feature flags. Capability
// gating is a deliberate act, not a free-text store (blueprint principle #6):
// an unknown key is NotFound, never an implicit insert.
var settableFlagKeys = map[string]struct{}{
	flags.FlagCollarTelemetry:  {},
	flags.FlagPhotoOCR:         {},
	flags.FlagONDC:             {},
	flags.FlagConsumerCommerce: {},
}

// service holds all platformops business logic.
type service struct {
	deps *deps.Deps
	repo *repository
}

// newService wires the service.
func newService(d *deps.Deps, repo *repository) *service {
	return &service{deps: d, repo: repo}
}

// ---- Admin: feature flags (§12) ----

// listFlags returns every stored feature flag for the admin console.
func (s *service) listFlags(ctx context.Context) ([]flags.Flag, error) {
	all, err := s.deps.Flags.All(ctx)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list feature flags: %w", err))
	}
	if all == nil {
		all = []flags.Flag{}
	}
	return all, nil
}

// setFlag flips one well-known feature flag and writes the explicit
// "admin.flag_set" audit entry.
func (s *service) setFlag(ctx context.Context, actor auth.Actor, key string, enabled bool) (*SetFlagResponse, error) {
	if _, ok := settableFlagKeys[key]; !ok {
		return nil, httpx.NotFound("feature flag " + key)
	}
	if err := s.deps.Flags.Set(ctx, key, enabled, actor.PartyID); err != nil {
		return nil, httpx.Internal(fmt.Errorf("set feature flag: %w", err))
	}
	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "admin.flag_set",
		TargetType: "FEATURE_FLAG",
		TargetID:   key,
		Meta:       map[string]any{"key": key, "enabled": enabled},
	})
	return &SetFlagResponse{Key: key, Enabled: enabled}, nil
}

// ---- Auditor: read-only audit-log surface (§12) ----

// listAuditLogs returns a page of audit entries, newest first.
func (s *service) listAuditLogs(ctx context.Context, f auditLogFilter, page httpx.Page) ([]audit.Entry, int64, error) {
	return s.repo.listAuditLogs(ctx, f, page)
}

// exportAuditLogs assembles the capped attachment body for the GIGW/DPDP
// immutable audit export.
func (s *service) exportAuditLogs(ctx context.Context, from, to *time.Time) (*AuditExport, error) {
	entries, err := s.repo.exportAuditLogs(ctx, auditLogFilter{From: from, To: to}, exportMaxRows)
	if err != nil {
		return nil, err
	}
	return &AuditExport{
		ExportedAt: time.Now().UTC(),
		From:       from,
		To:         to,
		Count:      len(entries),
		Entries:    entries,
	}, nil
}

// ---- Notifications outbox + mock SMS worker (§12, §13) ----

// listNotifications returns a page of outbox documents with credential
// material redacted: the login OTP must never be readable through the
// outbox surface, or anyone with this route could mint any user's session
// (request OTP for the victim's phone, read it here, verify).
func (s *service) listNotifications(ctx context.Context, phone, status string, page httpx.Page) ([]StoredNotification, int64, error) {
	items, total, err := s.repo.listNotifications(ctx, phone, status, page)
	if err != nil {
		return nil, 0, err
	}
	for i := range items {
		redactNotificationSecrets(&items[i])
	}
	return items, total, nil
}

// redactedValue replaces credential material in read surfaces.
const redactedValue = "[REDACTED]"

// redactNotificationSecrets strips the plaintext OTP (params + rendered meta)
// from an outbox document before it leaves the API.
func redactNotificationSecrets(n *StoredNotification) {
	if n.TemplateKey != domain.TemplateOTP {
		return
	}
	if n.Params != nil {
		params := make(map[string]string, len(n.Params))
		for k, v := range n.Params {
			if k == "otp" {
				v = redactedValue
			}
			params[k] = v
		}
		n.Params = params
	}
	if n.Meta != nil {
		meta := make(map[string]any, len(n.Meta))
		for k, v := range n.Meta {
			if k == "rendered_en" || k == "rendered_hi" {
				v = redactedValue
			}
			meta[k] = v
		}
		n.Meta = meta
	}
}

// runWorker is the MOCK SMS provider dispatch (real deployment: MSG91 /
// telecom DLT gateway, §13). It claims up to workerClaimCap QUEUED documents
// via a race-safe FindOneAndUpdate loop, stamps each SENT with a mock
// provider reference, and renders the English/Hindi lines into meta for
// visibility. The manual trigger keeps the demo deterministic; production
// runs this as a cron/queue consumer.
func (s *service) runWorker(ctx context.Context) (*WorkerRunResponse, error) {
	sent := 0
	for i := 0; i < workerClaimCap; i++ {
		providerRef := "SMS-MOCK-" + uuid.NewString()[0:8]
		claimed, err := s.repo.claimQueuedNotification(ctx, providerRef, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		if claimed == nil {
			break // queue drained
		}
		sent++

		english, hindi := renderSMS(claimed.TemplateKey, claimed.Params)
		if claimed.TemplateKey == domain.TemplateOTP {
			// Never persist the rendered OTP text — the provider gets the
			// message transiently; the readable outbox keeps no plaintext code.
			english, hindi = redactedValue, redactedValue
		}
		meta := map[string]any{
			"provider":    "SMS-MOCK",
			"rendered_en": english,
			"rendered_hi": hindi,
		}
		if err := s.repo.setNotificationMeta(ctx, claimed.ID, meta); err != nil {
			// The claim already succeeded — meta is cosmetic, log and move on.
			s.deps.Log.Error("platformops: stamp notification meta failed",
				slog.String("notification_id", claimed.ID), slog.Any("err", err))
		}
	}
	return &WorkerRunResponse{Sent: sent}, nil
}

// renderSMS produces one simple English and one Hindi line per template key —
// mock rendering for visibility; real templates live with the DLT-registered
// provider (§13). Missing params render as empty strings, never a panic.
func renderSMS(templateKey string, params map[string]string) (english, hindi string) {
	p := func(key string) string { return params[key] } // nil-map reads are safe

	switch templateKey {
	case domain.TemplatePayoutCredited:
		return fmt.Sprintf("Rs %s credited to your bank account. UTR %s. - Saathi", p("amount"), p("utr")),
			fmt.Sprintf("₹%s आपके बैंक खाते में जमा हुए। UTR %s। - साथी", p("amount"), p("utr"))
	case domain.TemplateSafetyBlock:
		return fmt.Sprintf("SAFETY ALERT: %s %s blocked at %s. Reasons: %s. Quarantine and inspect immediately. - Saathi",
				p("subject_type"), p("subject_id"), p("stage"), p("reasons")),
			fmt.Sprintf("सुरक्षा चेतावनी: %s %s को %s पर रोका गया। कारण: %s। तुरंत जांच करें। - साथी",
				p("subject_type"), p("subject_id"), p("stage"), p("reasons"))
	case domain.TemplateMVUDispatched:
		return fmt.Sprintf("MVU 1962 van dispatched for your animal (case %s). Please stay reachable. - Saathi", p("case_id")),
			fmt.Sprintf("आपके पशु के लिए 1962 एमवीयू वैन रवाना (केस %s)। कृपया संपर्क में रहें। - साथी", p("case_id"))
	case domain.TemplatePourReceipt:
		return fmt.Sprintf("%sL @ Rs %s = Rs %s credited to today's bill. - Saathi", p("quantity"), p("rate"), p("amount")),
			fmt.Sprintf("%s लीटर @ ₹%s = ₹%s आज के बिल में जुड़े। - साथी", p("quantity"), p("rate"), p("amount"))
	case domain.TemplateInvoiceIssued:
		return fmt.Sprintf("Invoice %s for Rs %s issued. - Saathi", p("invoice_number"), p("amount")),
			fmt.Sprintf("₹%s का बिल %s जारी हुआ। - साथी", p("amount"), p("invoice_number"))
	case domain.TemplateOTP:
		return fmt.Sprintf("Your Saathi OTP is %s. Do not share it with anyone.", p("otp")),
			fmt.Sprintf("आपका साथी OTP %s है। इसे किसी से साझा न करें।", p("otp"))
	default:
		return "Saathi update: " + templateKey, "साथी सूचना: " + templateKey
	}
}

// ---- Support: audited limited-PII lookup (§5.2 role 20) ----

// lookupParty resolves a phone to the limited support view. EVERY call —
// hit or miss — is recorded as "support.pii_lookup": PII access is audited,
// full stop.
func (s *service) lookupParty(ctx context.Context, phone string) (*PartyLookupResponse, error) {
	party, findErr := s.repo.findPartyByPhone(ctx, phone)

	entry := audit.Entry{
		Action:     "support.pii_lookup",
		TargetType: "PARTY",
		Meta:       map[string]any{"phone": phone, "found": findErr == nil},
	}
	if findErr == nil {
		entry.TargetID = party.ID
	}
	s.deps.Audit.Record(ctx, entry)

	if findErr != nil {
		return nil, findErr
	}

	assignments, err := s.repo.listActiveAssignmentsForParty(ctx, party.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	roles := []PartyRoleView{}
	for _, assignment := range assignments {
		if !assignment.UsableAt(now) {
			continue
		}
		orgName := ""
		if org, orgErr := s.deps.Orgs.Get(ctx, assignment.OrgUnitID); orgErr == nil {
			orgName = org.Name
		}
		roles = append(roles, PartyRoleView{
			RoleCode:  assignment.RoleCode,
			OrgUnitID: assignment.OrgUnitID,
			OrgName:   orgName,
		})
	}

	// Limited PII by construction: no KYC document numbers, no bank details.
	return &PartyLookupResponse{
		PartyID:  party.ID,
		FullName: party.FullName,
		KYCTier:  party.KYCTier,
		Status:   party.Status,
		Roles:    roles,
	}, nil
}

// ---- Event-bus reactions: cross-module SMS queuing ----

// decodeBusPayload structurally re-decodes a bus payload into this module's
// mirror struct — modules never import each other's types (module contract).
func decodeBusPayload(payload any, dst any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal bus payload: %w", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("unmarshal bus payload: %w", err)
	}
	return nil
}

// onPayoutCredited queues the "₹ credited, UTR …" SMS to the farmer
// (blueprint §8.1 last step).
func (s *service) onPayoutCredited(ctx context.Context, payload any) {
	var event payoutCreditedEvent
	if err := decodeBusPayload(payload, &event); err != nil {
		s.deps.Log.Error("platformops: bad payout.credited payload", slog.Any("err", err))
		return
	}
	if event.FarmerPartyID == "" && event.Phone == "" {
		s.deps.Log.Error("platformops: payout.credited payload has no addressee")
		return
	}
	s.queueNotification(ctx, event.FarmerPartyID, event.Phone, domain.TemplatePayoutCredited, map[string]string{
		"amount": strconv.FormatFloat(event.Amount, 'f', 2, 64),
		"utr":    event.UTR,
	})
}

// onGateBlocked alerts the blocked subject's UNION_FIELD_SUPERVISOR(s): the
// subject's org unit is resolved from its document, walked up to the union
// ancestor, and every usable supervisor assignment there gets the safety SMS.
func (s *service) onGateBlocked(ctx context.Context, payload any) {
	var event gateBlockedEvent
	if err := decodeBusPayload(payload, &event); err != nil {
		s.deps.Log.Error("platformops: bad qc.gate_blocked payload", slog.Any("err", err))
		return
	}

	orgUnitID, err := s.repo.subjectOrgUnit(ctx, event.SubjectType, event.SubjectID)
	if err != nil {
		s.deps.Log.Error("platformops: gate_blocked org resolution failed", slog.Any("err", err))
		return
	}
	union, err := s.unionAncestor(ctx, orgUnitID)
	if err != nil {
		s.deps.Log.Error("platformops: gate_blocked union walk failed",
			slog.String("org_unit_id", orgUnitID), slog.Any("err", err))
		return
	}
	if union == nil {
		s.deps.Log.Error("platformops: gate_blocked org has no union ancestor",
			slog.String("org_unit_id", orgUnitID))
		return
	}

	supervisors, err := s.repo.listActiveRoleHolders(ctx, union.ID, domain.RoleUnionFieldSupervisor)
	if err != nil {
		s.deps.Log.Error("platformops: gate_blocked supervisor lookup failed", slog.Any("err", err))
		return
	}

	params := map[string]string{
		"subject_type": event.SubjectType,
		"subject_id":   event.SubjectID,
		"stage":        event.Stage,
		"reasons":      strings.Join(event.FailureReasons, ", "),
	}
	now := time.Now().UTC()
	notified := map[string]bool{}
	for _, assignment := range supervisors {
		if !assignment.UsableAt(now) || notified[assignment.PartyID] {
			continue
		}
		notified[assignment.PartyID] = true
		s.queueNotification(ctx, assignment.PartyID, "", domain.TemplateSafetyBlock, params)
	}
}

// onMVUDispatched queues the "MVU van on its way" SMS to the farmer (§10).
func (s *service) onMVUDispatched(ctx context.Context, payload any) {
	var event mvuDispatchedEvent
	if err := decodeBusPayload(payload, &event); err != nil {
		s.deps.Log.Error("platformops: bad mvu.dispatched payload", slog.Any("err", err))
		return
	}
	if event.FarmerPartyID == "" && event.Phone == "" {
		s.deps.Log.Error("platformops: mvu.dispatched payload has no addressee")
		return
	}
	caseID := event.CaseID
	if caseID == "" {
		caseID = event.MVUCaseID
	}
	if caseID == "" {
		caseID = event.ID
	}
	params := map[string]string{"case_id": caseID}
	if event.ETA != "" {
		params["eta"] = event.ETA
	}
	s.queueNotification(ctx, event.FarmerPartyID, event.Phone, domain.TemplateMVUDispatched, params)
}

// queueNotification inserts one QUEUED outbox document, resolving phone and
// preferred language from the party record when available. Failures are
// logged, never propagated — a lost SMS must not fail the publishing flow.
func (s *service) queueNotification(ctx context.Context, partyID, phone, templateKey string, params map[string]string) {
	language := "hi" // vernacular default (blueprint §13)
	if partyID != "" {
		if party, err := s.repo.findPartyByID(ctx, partyID); err == nil {
			if phone == "" {
				phone = party.Phone
			}
			if party.PreferredLanguage != "" {
				language = party.PreferredLanguage
			}
		}
	}
	if phone == "" {
		s.deps.Log.Error("platformops: dropping notification with no phone",
			slog.String("template_key", templateKey), slog.String("party_id", partyID))
		return
	}

	notification := &StoredNotification{Notification: domain.Notification{
		ID:          uuid.NewString(),
		PartyID:     partyID,
		Phone:       phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: templateKey,
		Language:    language,
		Params:      params,
		Status:      domain.NotificationQueued,
		QueuedAt:    time.Now().UTC(),
	}}
	if err := s.repo.insertNotification(ctx, notification); err != nil {
		s.deps.Log.Error("platformops: queue notification failed",
			slog.String("template_key", templateKey), slog.Any("err", err))
	}
}

// unionAncestor returns the MILK_UNION org unit at or above orgUnitID, walking
// the denormalised ancestor Path nearest-first via d.Orgs.Get. Returns nil
// when no union sits on the chain (e.g. a plant directly under the federation).
func (s *service) unionAncestor(ctx context.Context, orgUnitID string) (*domain.OrgUnit, error) {
	org, err := s.deps.Orgs.Get(ctx, orgUnitID)
	if err != nil {
		return nil, err
	}
	if org.Type == domain.OrgTypeMilkUnion {
		return org, nil
	}
	// Path holds ancestors root→down; walk nearest ancestor first.
	for i := len(org.Path) - 1; i >= 0; i-- {
		ancestor, err := s.deps.Orgs.Get(ctx, org.Path[i])
		if err != nil {
			return nil, err
		}
		if ancestor.Type == domain.OrgTypeMilkUnion {
			return ancestor, nil
		}
	}
	return nil, nil
}
