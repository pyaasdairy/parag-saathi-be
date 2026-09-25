package consumer

// MEMBERS-ONLY PYAAS MILK ON A PLAN (spec rule 5.1, FOUNDING_PYAAS_MEMBERS_ONLY;
// the founder switches it on at launch: "PYAAS own-brand milk is members-only").
//
// pyaasPlanGate (founding.go) keeps a PYAAS milk plan from delivering a
// morning its member's perks do not cover. On its own that stop was silent:
// the plan stayed active, the app kept showing a next delivery date, the CRM
// kept counting the plan in the wallet sums, and nobody was told. So the
// first morning the gate refuses (the preview, the catch-up or the noon lock)
// the server pauses the plan (pause_reason founding_required, a state the app
// already shows) and records founding.pyaas_plan_paused. A resume is gated on
// the same perks (setSubscriptionStatusAt), so the member can bring it back as
// soon as they cover the next morning. The CRM wallet sums leave out a plan
// the gate refuses (crm_engine.go), and GET /consumer/admin/founding/
// members-only-impact lists, before the switch goes on, every plan it will stop.

import (
	"context"
	"net/http"
	"time"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// pauseReasonFoundingRequired is the pause_reason of a PYAAS milk plan the
// server paused because FOUNDING_PYAAS_MEMBERS_ONLY refused its next morning.
// Every other pause is the member's own ("member").
const pauseReasonFoundingRequired = "founding_required"

// pausePlanFoundingRequired pauses an active PYAAS milk plan on day, the first
// morning pyaasPlanGate refused, and records founding.pyaas_plan_paused once
// for that pause. The update is guarded on the plan still being active, so a
// member's own change or another replica's pause wins and this call does
// nothing. changed_at is stamped like a member's pause: a morning already past
// its noon cut-off (one the perks still cover) is decided as it stood, and the
// still-editable previews are cancelled and their days released
// (syncPlanPreviews), so nothing further is previewed, locked or counted.
// Returns whether this call paused the plan.
func (s *service) pausePlanFoundingRequired(ctx context.Context, sub *subscription, day string, now time.Time) bool {
	if sub == nil || sub.Status != "active" {
		return false
	}
	updated, err := s.repo.updateSubscription(ctx, sub.SubscriptionID, sub.ConsumerID,
		bson.D{
			{Key: "status", Value: "paused"},
			{Key: "pause_reason", Value: pauseReasonFoundingRequired},
			{Key: "changed_at", Value: now.UTC()},
		},
		bson.D{{Key: "status", Value: "active"}})
	if err != nil || updated == nil {
		return false
	}
	sub.Status, sub.PauseReason, sub.ChangedAt = updated.Status, updated.PauseReason, updated.ChangedAt
	s.syncPlanPreviews(ctx, updated, now)
	memberStatus := ""
	if m, _ := s.repo.findFoundingMember(ctx, sub.ConsumerID); m != nil {
		memberStatus = m.Status
	}
	s.log.InfoContext(ctx, "members-only: a PYAAS milk plan the member's perks do not cover was paused",
		"subscription", sub.SubscriptionID, "consumer", sub.ConsumerID.Hex(), "day", day, "member_status", memberStatus)
	s.emitCRMEvent(ctx, "founding.pyaas_plan_paused", sub.ConsumerID, map[string]any{
		"subscription_id": sub.SubscriptionID, "product_id": sub.ProductID, "day": day,
		"reason": pauseReasonFoundingRequired, "member_status": memberStatus,
		"scope_key": sub.SubscriptionID + ":" + pauseReasonFoundingRequired + ":" + now.UTC().Format(time.RFC3339Nano),
	})
	return true
}

// membersOnlyStop is one plan the switch stops, with who it belongs to.
type membersOnlyStop struct {
	sub          subscription
	memberStatus string
	perksUntil   string
}

// pyaasPlansMembersOnlyStops lists the active PYAAS milk plans that
// FOUNDING_PYAAS_MEMBERS_ONLY stops on day: every plan pyaasPlanRefusal
// refuses there, whether the switch is on yet or not. Oldest plan first.
func (s *service) pyaasPlansMembersOnlyStops(ctx context.Context, day string) ([]membersOnlyStop, error) {
	subs, err := s.repo.listActiveSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	out := []membersOnlyStop{}
	for i := range subs {
		if s.pyaasPlanRefusal(ctx, &subs[i], day) == nil {
			continue
		}
		row := membersOnlyStop{sub: subs[i]}
		if m, _ := s.repo.findFoundingMember(ctx, subs[i].ConsumerID); m != nil {
			row.memberStatus, row.perksUntil = m.Status, m.PerksUntil
		}
		out = append(out, row)
	}
	return out, nil
}

// adminFoundingMembersOnlyImpact is GET /consumer/admin/founding/
// members-only-impact: the owner's pre-launch list of the PYAAS milk plans the
// members-only switch stops, judged on the first morning still open to change
// (?day=YYYY-MM-DD judges another). Read-only; the admin read audit records it.
func (h *handler) adminFoundingMembersOnlyImpact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	day := firstEditableDay(h.svc.now())
	if q := r.URL.Query().Get("day"); q != "" {
		if _, ok := parseDay(q); !ok {
			httpx.Error(w, r, toHTTPErr(errBadRequest("day must be YYYY-MM-DD")))
			return
		}
		day = q
	}
	rows, err := h.svc.pyaasPlansMembersOnlyStops(ctx, day)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	ids := make([]primitive.ObjectID, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].sub.ConsumerID)
	}
	accts := h.svc.accountsByID(ctx, ids)
	items := make([]map[string]any, 0, len(rows))
	for i := range rows {
		v := crmSubscriptionView(&rows[i].sub)
		v["memberStatus"], v["perksUntil"] = rows[i].memberStatus, rows[i].perksUntil
		if a, ok := accts[rows[i].sub.ConsumerID]; ok {
			v["customerPhone"] = a.Phone
			if a.FullName != nil {
				v["customerName"] = *a.FullName
			}
		}
		items = append(items, v)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"day": day, "switchOn": h.svc.deps.Cfg.FoundingPyaasMembersOnly, "items": items, "total": len(items),
	})
}

// accountsByID reads the accounts of ids in one query (missing ids are left out).
func (s *service) accountsByID(ctx context.Context, ids []primitive.ObjectID) map[primitive.ObjectID]account {
	out := map[primitive.ObjectID]account{}
	if len(ids) == 0 {
		return out
	}
	cur, err := s.repo.accounts.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return out
	}
	var rows []account
	if cur.All(ctx, &rows) == nil {
		for _, a := range rows {
			out[a.ID] = a
		}
	}
	return out
}
