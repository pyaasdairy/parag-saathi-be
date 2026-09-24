package consumer

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// INSTANT HOURS AT CLOSING TIME (founder, 24 Sep). The store's instant lane is
// open inside its saved hours (the ones the Zone tab shows; an unsaved zone is
// 07:00-22:00 IST) unless the manager's persistent pause is on. At closing time
// the manager decides for tonight only:
//
//   - EXTEND by 30, 60 or 120 minutes (POST .../zone/instant/extend). The
//     extension runs from the later of now, tonight's closing time and the
//     current extension, and never past 02:00 IST the morning after the
//     window opened. It simply expires at the stored moment.
//   - CLOSE NOW (POST .../zone/instant/close-now): instant shuts until the next
//     opening time and reopens by itself; the persistent pause is not touched.
//
// Either one replaces the other (the manager's latest word wins). The Zone
// tab's Save (PUT) never touches them. The closing alert that asks the manager
// to decide is instant_alerts.go.

// Extension lengths the console offers, in minutes.
var instantExtendMinutes = map[int]bool{30: true, 60: true, 120: true}

// instantExtendCapMin is the latest an extension may run: 02:00 IST.
const instantExtendCapMin = 120

// hoursSpanMin is the length of a zone's saved-hours window in minutes (1440
// for an all-day window, which is also what open == close means).
func hoursSpanMin(z *zone) int {
	span := effCloseMin(z) - effOpenMin(z)
	if span <= 0 {
		span += 1440
	}
	return span
}

// hoursWindowAt is the saved-hours window [openAt, closeAt) that contains t,
// or, when t is outside every window, the most recent one that closed before
// t (inside=false). Times are IST.
func hoursWindowAt(z *zone, t time.Time) (openAt, closeAt time.Time, inside bool) {
	openMin, span := effOpenMin(z), hoursSpanMin(z)
	tIST := t.In(istZone)
	day0 := time.Date(tIST.Year(), tIST.Month(), tIST.Day(), 0, 0, 0, 0, istZone)
	found := false
	for d := 0; d >= -2; d-- {
		o := day0.AddDate(0, 0, d).Add(time.Duration(openMin) * time.Minute)
		c := o.Add(time.Duration(span) * time.Minute)
		if !t.Before(o) && t.Before(c) {
			return o, c, true
		}
		if !c.After(t) && (!found || c.After(closeAt)) {
			openAt, closeAt, found = o, c, true
		}
	}
	return openAt, closeAt, false
}

// nextInstantOpening is the first opening moment of the saved hours strictly
// after t, in IST.
func nextInstantOpening(z *zone, t time.Time) time.Time {
	openMin := effOpenMin(z)
	tIST := t.In(istZone)
	day0 := time.Date(tIST.Year(), tIST.Month(), tIST.Day(), 0, 0, 0, 0, istZone)
	for d := 0; ; d++ {
		if o := day0.AddDate(0, 0, d).Add(time.Duration(openMin) * time.Minute); o.After(t) {
			return o
		}
	}
}

// instantClosesAt is the moment the instant lane that is open at `now` will
// close: the end of the hours window it is in or the extension, whichever is
// later. ok=false when the lane is shut at now, or open round the clock
// (saved 00:00-24:00: the next window opens the moment this one closes).
func instantClosesAt(z *zone, now time.Time) (time.Time, bool) {
	if open, _, _ := instantWindow(*z, now); !open {
		return time.Time{}, false
	}
	var at time.Time
	if _, c, inside := hoursWindowAt(z, now); inside {
		if hoursSpanMin(z) >= 1440 {
			return time.Time{}, false
		}
		at = c
	}
	if z.InstantExtendedUntil != nil && z.InstantExtendedUntil.After(now) && z.InstantExtendedUntil.After(at) {
		at = *z.InstantExtendedUntil
	}
	return at.In(istZone), !at.IsZero()
}

// instantExtendBase is where an extension asked for at `now` would start (the
// later of now, the close of the hours window now is in or last left, and the
// current extension) and the latest it may run to: 02:00 IST the morning after
// that window opened. base >= limit means no extension is possible.
func instantExtendBase(z *zone, now time.Time) (base, limit time.Time) {
	openAt, closeAt, _ := hoursWindowAt(z, now)
	base = now
	if closeAt.After(base) {
		base = closeAt
	}
	if z.InstantExtendedUntil != nil && z.InstantExtendedUntil.After(base) {
		base = *z.InstantExtendedUntil
	}
	oIST := openAt.In(istZone)
	limit = time.Date(oIST.Year(), oIST.Month(), oIST.Day(), 0, 0, 0, 0, istZone).
		AddDate(0, 0, 1).Add(instantExtendCapMin * time.Minute)
	return base, limit
}

// ── repository ──────────────────────────────────────────────────────────────

// setInstantOverride stores tonight's override on a store's zone: exactly one
// of an extension or a close-now (nil unsets). Returns the stored zone.
func (r *repository) setInstantOverride(ctx context.Context, storeID string, extendedUntil, closedUntil *time.Time, by string, now time.Time) (*zone, error) {
	set := bson.D{{Key: "updated_by", Value: by}, {Key: "updated_at", Value: now.UTC()}}
	unset := bson.D{}
	if extendedUntil != nil {
		set = append(set, bson.E{Key: "instant_extended_until", Value: extendedUntil.UTC()})
	} else {
		unset = append(unset, bson.E{Key: "instant_extended_until", Value: ""})
	}
	if closedUntil != nil {
		set = append(set, bson.E{Key: "instant_closed_until", Value: closedUntil.UTC()})
	} else {
		unset = append(unset, bson.E{Key: "instant_closed_until", Value: ""})
	}
	update := bson.D{{Key: "$set", Value: set}}
	if len(unset) > 0 {
		update = append(update, bson.E{Key: "$unset", Value: unset})
	}
	var out zone
	err := r.storeZones.FindOneAndUpdate(ctx, bson.D{{Key: "store_id", Value: storeID}}, update,
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if isNoDocs(err) {
		return nil, errUnprocessable("INSTANT_NOT_CONFIGURED", "this store has no instant delivery area yet; draw it on the Zone tab first")
	}
	if err != nil {
		return nil, errInternal("instant override save failed")
	}
	return &out, nil
}

// ── service ─────────────────────────────────────────────────────────────────

// instantZoneFor loads the manager's own zone and refuses a store with no
// instant lane to act on.
func (s *service) instantZoneFor(ctx context.Context, actor auth.Actor, storeID string) (*zone, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	z, err := s.repo.getZone(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if z == nil || z.InstantRadiusM <= 0 {
		return nil, errUnprocessable("INSTANT_NOT_CONFIGURED", "this store has no instant delivery area yet; draw it on the Zone tab first")
	}
	return z, nil
}

// extendInstant keeps the store's instant lane open `minutes` longer tonight.
func (s *service) extendInstant(ctx context.Context, actor auth.Actor, storeID string, minutes int, now time.Time) (*zone, error) {
	z, err := s.instantZoneFor(ctx, actor, storeID)
	if err != nil {
		return nil, err
	}
	if !instantExtendMinutes[minutes] {
		return nil, errUnprocessable("INVALID_EXTENSION", "extend instant by 30, 60 or 120 minutes")
	}
	if z.InstantPaused {
		return nil, errUnprocessable("INSTANT_PAUSED", "instant is switched off for this store; turn it back on in the Zone tab first")
	}
	base, limit := instantExtendBase(z, now)
	if !base.Before(limit) {
		return nil, errUnprocessable("EXTEND_TOO_LATE", "instant can stay open until 2:00 AM at the latest")
	}
	until := base.Add(time.Duration(minutes) * time.Minute)
	if until.After(limit) {
		until = limit
	}
	return s.repo.setInstantOverride(ctx, storeID, &until, nil, actor.PartyID, now)
}

// closeInstantNow shuts the store's instant lane until its next opening time,
// clearing any extension; the persistent pause is not touched.
func (s *service) closeInstantNow(ctx context.Context, actor auth.Actor, storeID string, now time.Time) (*zone, error) {
	z, err := s.instantZoneFor(ctx, actor, storeID)
	if err != nil {
		return nil, err
	}
	until := nextInstantOpening(z, now)
	return s.repo.setInstantOverride(ctx, storeID, nil, &until, actor.PartyID, now)
}

// ── handlers ────────────────────────────────────────────────────────────────

// extendInstant — POST /consumer/stores/{storeId}/zone/instant/extend
// {minutes: 30|60|120} (STORE_MANAGER, own store). Answers the zone in the
// GET /zone shape.
func (h *handler) extendInstant(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	storeID := chi.URLParam(r, "storeId")
	var body struct {
		Minutes int `json:"minutes"`
	}
	if err := decode(r, &body); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	now := h.svc.now()
	z, err := h.svc.extendInstant(r.Context(), actor, storeID, body.Minutes, now)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, zoneViewAt(z, storeID, now))
}

// closeInstantNow — POST /consumer/stores/{storeId}/zone/instant/close-now
// (STORE_MANAGER, own store). Answers the zone in the GET /zone shape.
func (h *handler) closeInstantNow(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	storeID := chi.URLParam(r, "storeId")
	now := h.svc.now()
	z, err := h.svc.closeInstantNow(r.Context(), actor, storeID, now)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, zoneViewAt(z, storeID, now))
}
