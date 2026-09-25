package consumer

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

// ── Who assigned a task, and the audit trail behind it ─────────────────────
//
// A delivery task records how its current rider got it (assign_source) and,
// when a person did it, who (assigned_by). The store manager's hand-assign
// also writes a row to the platform's insert-only audit log (audit_logs,
// read by the STATE_AUDITOR console), so an order a manager steered to a
// particular rider can always be traced back to the manager who did it.

// Assign sources stamped on the task.
const (
	assignSourceManager = "manager_assign" // the store manager's assign sheet
	assignSourceAdmin   = "admin_assign"   // the website's delivery CRM
	assignSourceClaim   = "rider_claim"    // first-accept-wins on an instant offer
)

// assignReasonMaxLen bounds the optional free-text reason a manager gives.
const assignReasonMaxLen = 200

// Audit actions this module writes (audit.Entry.Action).
const (
	auditActionManagerAssign = "consumer.delivery.manager_assign"
	auditTargetDelivery      = "consumer_delivery"
)

// assignedByDoc names the person who assigned a task's current rider.
type assignedByDoc struct {
	PartyID string `bson:"party_id"       json:"partyId"`
	Role    string `bson:"role"           json:"role"`
	Name    string `bson:"name,omitempty" json:"name,omitempty"`
}

// assignerOf resolves the acting operator into an assigned_by record. The name
// is the party's full name when one is on file, else left out: a party id
// shown as a name would read like a person nobody has heard of.
func (s *service) assignerOf(ctx context.Context, actor auth.Actor) *assignedByDoc {
	by := &assignedByDoc{PartyID: actor.PartyID, Role: actor.RoleCode}
	if name, _ := s.repo.riderName(ctx, actor.PartyID); name != "" && name != actor.PartyID {
		by.Name = name
	}
	return by
}

// assignmentStamp is the $set fragment every person-made assignment writes, so
// the three fields always move together and a new assignment never inherits
// the previous one's reason.
func assignmentStamp(by *assignedByDoc, source, reason string) bson.D {
	return bson.D{
		{Key: "assigned_by", Value: by},
		{Key: "assign_source", Value: source},
		{Key: "assign_reason", Value: reason},
	}
}

// cleanAssignReason trims the manager's reason to one bounded line.
func cleanAssignReason(raw string) string {
	r := strings.Join(strings.Fields(raw), " ")
	if len([]rune(r)) > assignReasonMaxLen {
		r = string([]rune(r)[:assignReasonMaxLen])
	}
	return r
}

// recordAudit writes one row to the platform audit log. Nil-safe: a service
// built without the recorder (unit tests, tools) simply records nothing, and
// the recorder itself writes in the background, so the request never waits
// on it.
func (s *service) recordAudit(ctx context.Context, e audit.Entry) {
	if s.deps == nil || s.deps.Audit == nil {
		return
	}
	s.deps.Audit.Record(ctx, e)
}

// auditManagerAssign records a store manager's assignment: who, which task,
// which rider, what the task was before (an unclaimed instant offer, an
// unassigned morning task, a failed one) and why, if they said.
func (s *service) auditManagerAssign(ctx context.Context, actor auth.Actor, before, after *delivery, by *assignedByDoc, extra map[string]any, now time.Time) {
	meta := map[string]any{
		"store_id":                after.StoreID,
		"order_id":                after.OrderID,
		"lane":                    after.Lane,
		"rider_party_id":          after.RiderPartyID,
		"previous_status":         before.Status,
		"previous_rider_party_id": before.RiderPartyID,
		"assign_source":           assignSourceManager,
		"assigned_at":             now.UTC().Format(time.RFC3339),
	}
	if by != nil && by.Name != "" {
		meta["assigned_by_name"] = by.Name
	}
	if after.AssignReason != "" {
		meta["reason"] = after.AssignReason
	}
	for k, v := range extra {
		meta[k] = v
	}
	s.recordAudit(ctx, audit.Entry{
		ActorPartyID: actor.PartyID,
		ActorRole:    actor.RoleCode,
		Action:       auditActionManagerAssign,
		TargetType:   auditTargetDelivery,
		TargetID:     after.ID,
		Meta:         meta,
	})
}
