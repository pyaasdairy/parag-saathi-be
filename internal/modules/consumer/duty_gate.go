package consumer

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// ── Attendance gates the instant offer pool (founder decision 10) ──────────
//
// The OFFER POOL (unclaimed broadcast instant orders, GET
// /delivery/tasks/available) is shown only to riders who are ON DUTY today,
// and only they may claim from it. It never blocks a delivery:
//
//  1. A task already assigned to a rider always shows in their queue and can
//     always be accepted, picked up and delivered, on duty or not. Only the
//     unclaimed pool is gated.
//  2. The store manager may assign any of the store's riders, on duty or not
//     (the hand-assign, delivery_svc.go assignRiderAt), and may mark a rider
//     on (or off) duty for today (POST /stores/{id}/riders/{rider}/duty).
//  3. If NO rider of a store is on duty, that store's pool falls back to all
//     of its riders, exactly as before this gate existed, and the fallback is
//     logged (at most once per store per fallbackLogEvery). A missed check-in
//     therefore never strands an order.
//  4. A duty lookup that fails opens the pool (fail open), for the same
//     reason.
//
// ON DUTY for an IST day = the manager's override for that day when there is
// one (on or off), else the rider's own attendance being ON_DUTY (checked in,
// not yet checked out). Attendance itself is untouched: an override is never
// written into rider_attendance, which is a payroll record backed by the
// rider's own selfie.

// collRiderDutyOverrides — one row per (rider, IST day) a manager set.
const collRiderDutyOverrides = "rider_duty_overrides"

// Duty sources on the wire (riderSummary.dutySource, the duty endpoint).
const (
	dutySourceAttendance = "attendance" // the rider's own check-in
	dutySourceManager    = "manager"    // a manager's override for today
	dutySourceNone       = "none"       // not checked in, no override
)

// auditActionDutyOverride is the audit_logs action for a manager's duty mark.
const auditActionDutyOverride = "consumer.rider.duty_override"

// fallbackLogEvery throttles the "nobody on duty" log line per store: the
// offer pool is polled every few seconds by every rider on the app.
const fallbackLogEvery = 10 * time.Minute

// riderDutyOverrideDoc is a manager's duty mark for one rider for one IST day.
type riderDutyOverrideDoc struct {
	RiderPartyID string         `bson:"rider_party_id"`
	Day          string         `bson:"day"`
	StoreID      string         `bson:"store_id"`
	OnDuty       bool           `bson:"on_duty"`
	By           *assignedByDoc `bson:"by,omitempty"`
	Reason       string         `bson:"reason,omitempty"`
	CreatedAt    time.Time      `bson:"created_at"`
	UpdatedAt    time.Time      `bson:"updated_at"`
}

// riderDuty is one rider's duty for one day, and what decided it.
type riderDuty struct {
	OnDuty bool
	Source string
}

// riderDutyView is the duty endpoint's answer.
type riderDutyView struct {
	PartyID    string `json:"partyId"`
	Day        string `json:"day"`
	OnDuty     bool   `json:"onDuty"`
	DutySource string `json:"dutySource"`
}

// dutyForRiders reads the duty of each rider on an IST day: the manager's
// override when present, else the rider's attendance. Every rider asked about
// is in the answer (dutySourceNone when neither exists).
func (r *repository) dutyForRiders(ctx context.Context, riderIDs []string, day string) (map[string]riderDuty, error) {
	out := make(map[string]riderDuty, len(riderIDs))
	if len(riderIDs) == 0 {
		return out, nil
	}
	ids := bson.A{}
	for _, id := range riderIDs {
		out[id] = riderDuty{Source: dutySourceNone}
		ids = append(ids, id)
	}
	filter := bson.D{{Key: "rider_party_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "day", Value: day}}

	cur, err := r.riderColl(collRiderAttendance).Find(ctx, filter,
		options.Find().SetProjection(bson.D{{Key: "rider_party_id", Value: 1}, {Key: "state", Value: 1}}))
	if err != nil {
		return nil, errInternal("rider attendance lookup failed")
	}
	var att []struct {
		RiderPartyID string `bson:"rider_party_id"`
		State        string `bson:"state"`
	}
	if err := cur.All(ctx, &att); err != nil {
		return nil, errInternal("rider attendance decode failed")
	}
	for _, a := range att {
		if a.State == riderDutyOnDuty {
			out[a.RiderPartyID] = riderDuty{OnDuty: true, Source: dutySourceAttendance}
		}
	}

	ocur, err := r.riderColl(collRiderDutyOverrides).Find(ctx, filter,
		options.Find().SetProjection(bson.D{{Key: "rider_party_id", Value: 1}, {Key: "on_duty", Value: 1}}))
	if err != nil {
		return nil, errInternal("rider duty override lookup failed")
	}
	var ovr []struct {
		RiderPartyID string `bson:"rider_party_id"`
		OnDuty       bool   `bson:"on_duty"`
	}
	if err := ocur.All(ctx, &ovr); err != nil {
		return nil, errInternal("rider duty override decode failed")
	}
	for _, o := range ovr {
		out[o.RiderPartyID] = riderDuty{OnDuty: o.OnDuty, Source: dutySourceManager}
	}
	return out, nil
}

// upsertDutyOverride writes the manager's mark for (rider, day).
func (r *repository) upsertDutyOverride(ctx context.Context, doc riderDutyOverrideDoc) error {
	_, err := r.riderColl(collRiderDutyOverrides).UpdateOne(ctx,
		bson.D{{Key: "rider_party_id", Value: doc.RiderPartyID}, {Key: "day", Value: doc.Day}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "store_id", Value: doc.StoreID},
				{Key: "on_duty", Value: doc.OnDuty},
				{Key: "by", Value: doc.By},
				{Key: "reason", Value: doc.Reason},
				{Key: "updated_at", Value: doc.UpdatedAt},
			}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: doc.CreatedAt}}},
		},
		options.Update().SetUpsert(true))
	if err != nil {
		return errInternal("rider duty override write failed")
	}
	return nil
}

// offerPoolStores returns the stores, of those given, whose unclaimed offers
// the rider may see and claim now: all of them when the rider is on duty;
// otherwise only the stores where no rider at all is on duty (the fallback).
// Any lookup failure opens the store (never strand an order).
func (s *service) offerPoolStores(ctx context.Context, riderPartyID string, stores []string, now time.Time) []string {
	if len(stores) == 0 {
		return stores
	}
	day := istDay(now)
	self, err := s.repo.dutyForRiders(ctx, []string{riderPartyID}, day)
	if err != nil {
		s.log.WarnContext(ctx, "rider duty lookup failed - offer pool left open", "rider", riderPartyID, "err", err)
		return stores
	}
	if self[riderPartyID].OnDuty {
		return stores
	}
	out := make([]string, 0, len(stores))
	for _, st := range stores {
		roster, rerr := s.repo.ridersForStore(ctx, st)
		if rerr != nil {
			out = append(out, st)
			continue
		}
		duty, derr := s.repo.dutyForRiders(ctx, roster, day)
		if derr != nil {
			s.log.WarnContext(ctx, "store duty lookup failed - offer pool left open", "store", st, "err", derr)
			out = append(out, st)
			continue
		}
		anyOn := false
		for _, d := range duty {
			if d.OnDuty {
				anyOn = true
				break
			}
		}
		if !anyOn {
			if s.dutyFallbackLog.allow(st, now, fallbackLogEvery) {
				s.log.WarnContext(ctx, "no rider of the store is on duty - the instant offer pool falls back to every store rider",
					slog.String("store", st), slog.String("day", day), slog.Int("riders", len(roster)))
			}
			out = append(out, st)
		}
	}
	return out
}

// errNotOnDuty refuses a claim from the gated pool. 403, not 409: the Saathi
// rider app reads a 409 as "another rider took it" and shows this message
// verbatim otherwise.
func errNotOnDuty() *apiError {
	return &apiError{status: http.StatusForbidden, Code: "NOT_ON_DUTY", Message: "Mark attendance at the centre to take new orders"}
}

// claimAllowed applies the pool gate to one claim: the rider may claim an
// OFFERED task only from a store whose pool they can see now.
func (s *service) claimAllowed(ctx context.Context, riderPartyID, taskID string, now time.Time) error {
	t, err := s.repo.findDeliveryByID(ctx, taskID)
	if err != nil || t.Status != "OFFERED" {
		return nil // the claim itself answers not-found / already taken
	}
	if len(s.offerPoolStores(ctx, riderPartyID, []string{t.StoreID}, now)) == 0 {
		return errNotOnDuty()
	}
	return nil
}

// setRiderDutyAt is the manager's duty mark for today (IST): on duty puts the
// rider in the offer pool even without a check-in (a phone that cannot take a
// selfie, a rider who forgot); off duty takes a rider who forgot to check out
// out of it. It lasts for the IST day only and never touches attendance.
func (s *service) setRiderDutyAt(ctx context.Context, actor auth.Actor, storeID, riderPartyID string, onDuty bool, reason string, now time.Time) (*riderDutyView, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	roster, err := s.repo.ridersForStore(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if !contains(roster, riderPartyID) {
		return nil, errBadRequest("rider is not assigned to this store")
	}
	day := istDay(now)
	before := riderDuty{Source: dutySourceNone}
	if prev, perr := s.repo.dutyForRiders(ctx, []string{riderPartyID}, day); perr == nil {
		before = prev[riderPartyID]
	}
	by := s.assignerOf(ctx, actor)
	reason = cleanAssignReason(reason)
	if err := s.repo.upsertDutyOverride(ctx, riderDutyOverrideDoc{
		RiderPartyID: riderPartyID, Day: day, StoreID: storeID, OnDuty: onDuty,
		By: by, Reason: reason, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}); err != nil {
		return nil, err
	}
	meta := map[string]any{
		"store_id": storeID, "day": day, "on_duty": onDuty,
		"previous_on_duty": before.OnDuty, "previous_duty_source": before.Source,
	}
	if reason != "" {
		meta["reason"] = reason
	}
	if by.Name != "" {
		meta["set_by_name"] = by.Name
	}
	s.recordAudit(ctx, auditEntry(actor, auditActionDutyOverride, "rider", riderPartyID, meta))
	return &riderDutyView{PartyID: riderPartyID, Day: day, OnDuty: onDuty, DutySource: dutySourceManager}, nil
}

// setRiderDuty — POST /stores/{storeId}/riders/{riderPartyId}/duty
// {"on_duty": true|false, "reason": "..."}.
func (h *handler) setRiderDuty(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body struct {
		OnDuty *bool  `json:"on_duty"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &body); err != nil || body.OnDuty == nil {
		httpx.Error(w, r, toHTTPErr(errBadRequest("on_duty (true or false) is required")))
		return
	}
	v, err := h.svc.setRiderDutyAt(r.Context(), actor, chi.URLParam(r, "storeId"),
		strings.TrimSpace(chi.URLParam(r, "riderPartyId")), *body.OnDuty, body.Reason, h.svc.now())
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

// throttledLog lets one line per key through at most once per interval. The
// zero value is ready to use.
type throttledLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (t *throttledLog) allow(key string, now time.Time, every time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	if prev, ok := t.last[key]; ok && now.Sub(prev) < every && !now.Before(prev) {
		return false
	}
	t.last[key] = now
	return true
}
