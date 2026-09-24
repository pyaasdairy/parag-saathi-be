package consumer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// THE CLOSING ALERT (founder, 24 Sep): at closing time the store manager is
// asked to keep instant open or let it close, instead of the lane closing
// silently. A light one-minute worker writes, for every active zone that has
// an instant lane (zoneHasInstantLane: an instant radius, or any served zone
// while INSTANT_TEST_OPEN widens instant to it) whose pause does not hold
// (instantPausedAt: a pause is over at its end, the next opening), into the
// operator inbox that Saathi's bell already polls (GET /notifications/me), on
// the STORE_LOW_STOCK model (lowstock.go: channel APP, status QUEUED, unread):
//
//   - STORE_INSTANT_CLOSING, 15 minutes before the lane closes (the saved
//     hours' close, or the extended close after an extension, which gets its
//     own alert): "Instant delivery closes at 10:00 PM" + how to extend, or,
//     when extend would refuse (a close at or past the 02:00 cap), that it
//     cannot be extended any further;
//   - STORE_INSTANT_CLOSED, at the close itself (not when extended past it):
//     "Instant delivery is now closed. It reopens at 7:00 AM."
//
// Recipients: every ACTIVE STORE_MANAGER of that store (role_assignments). No
// alert for a paused lane, a zone without an instant lane, an inactive zone
// or store, a lane the manager closed themselves (close-now), or hours saved
// as 00:00-24:00 (the lane never closes). Exactly once per (store, kind,
// closing moment): a claim row under a unique index is inserted first, so two
// instances ticking together and a restart never alert twice.
//
// Params carry the copy (headline, message) so a client can render them; the
// keys sort so that Saathi's generic inbox row, which lists params in key
// order, reads the headline first until it gets its own renderer.

const (
	templateStoreInstantClosing = "STORE_INSTANT_CLOSING"
	templateStoreInstantClosed  = "STORE_INSTANT_CLOSED"

	collStoreInstantAlerts = "store_instant_alerts"

	// instantClosingLead is how long before the close the manager is asked.
	instantClosingLead = 15 * time.Minute
	// instantClosedGrace bounds how late a "now closed" may still go out (a
	// server asleep through the close must not announce it hours later).
	instantClosedGrace = time.Hour

	instantClosingMessage = "Keep instant open tonight: extend by 30 min, 1 h or 2 h from the Zone tab, or let it close. Orders already placed are not affected."
	// instantClosingFinalMessage replaces it when extend would answer
	// EXTEND_TOO_LATE: a close at or past the 02:00 cap.
	instantClosingFinalMessage = "It cannot be extended any further. Orders already placed are not affected."
)

// ensureInstantAlertIndexes builds the exactly-once claim index: one alert per
// (store, kind, closing moment).
func (r *repository) ensureInstantAlertIndexes(ctx context.Context) error {
	_, err := r.accounts.Database().Collection(collStoreInstantAlerts).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "store_id", Value: 1}, {Key: "kind", Value: 1}, {Key: "close_at", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("uniq_store_kind_close"),
	})
	return err
}

// instantAlertWorker runs for the process lifetime: one tick a minute. It does
// not alert until the claim index exists (the index IS the exactly-once guard),
// retrying the build every tick.
func (s *service) instantAlertWorker(ctx context.Context) {
	select {
	case <-time.After(20 * time.Second): // let boot settle
	case <-ctx.Done():
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	indexed := false
	for {
		if !indexed {
			ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := s.repo.ensureInstantAlertIndexes(ictx); err != nil {
				s.log.Warn("instant closing alerts: claim index not built yet, no alerts this tick", slog.Any("err", err))
			} else {
				indexed = true
			}
			cancel()
		}
		if indexed {
			tctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			s.instantAlertsTick(tctx, s.now())
			cancel()
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

// instantAlertsTick raises every alert due at `now`. Best-effort per store: a
// failure is logged and retried on the next tick.
func (s *service) instantAlertsTick(ctx context.Context, now time.Time) {
	zones, err := s.repo.listActiveZones(ctx)
	if err != nil {
		s.log.Warn("instant closing alerts: zone list failed", slog.Any("err", err))
		return
	}
	testOpen := instantTestOpenOn()
	for i := range zones {
		z := &zones[i]
		if !zoneHasInstantLane(z, testOpen) || instantPausedAt(z, now) {
			continue
		}
		kind, closeAt, due := instantAlertDue(z, now)
		if !due {
			continue
		}
		if err := s.raiseInstantAlert(ctx, z, kind, closeAt, now); err != nil {
			s.log.Warn("instant closing alert failed, retrying next tick",
				slog.String("store_id", z.StoreID), slog.String("kind", kind), slog.Any("err", err))
		}
	}
}

// instantAlertDue decides, for one unpaused zone with an instant lane, which
// alert (if any) is due at now and the closing moment it is about.
func instantAlertDue(z *zone, now time.Time) (kind string, closeAt time.Time, due bool) {
	if open, _, _ := instantWindow(*z, now); open {
		at, ok := instantClosesAt(z, now) // !ok: open round the clock
		if ok && !now.Before(at.Add(-instantClosingLead)) {
			return templateStoreInstantClosing, at, true
		}
		return "", time.Time{}, false
	}
	// Shut. Only a close by the clock is announced: a lane the manager closed
	// themselves (close-now) needs no telling.
	if z.InstantClosedUntil != nil && now.Before(*z.InstantClosedUntil) {
		return "", time.Time{}, false
	}
	// The most recent moment the lane closed: the end of the last hours window
	// or of an extension, whichever came later.
	var last time.Time
	if _, c, inside := hoursWindowAt(z, now); !inside {
		last = c
	}
	if z.InstantExtendedUntil != nil && !z.InstantExtendedUntil.After(now) && z.InstantExtendedUntil.After(last) {
		last = *z.InstantExtendedUntil
	}
	if last.IsZero() || now.Sub(last) >= instantClosedGrace {
		return "", time.Time{}, false
	}
	return templateStoreInstantClosed, last, true
}

// raiseInstantAlert claims (store, kind, closeAt) and, if this caller won the
// claim, writes the alert to every STORE_MANAGER of the store. A failed write
// releases the claim so the next tick retries; the per-recipient upsert is
// keyed on the same moment, so a retry never doubles a delivered one.
func (s *service) raiseInstantAlert(ctx context.Context, z *zone, kind string, closeAt, now time.Time) error {
	storeOID, err := primitive.ObjectIDFromHex(z.StoreID)
	if err != nil {
		return nil // not a STORE org unit id: nobody to tell
	}
	var store struct {
		Name string `bson:"name"`
	}
	if err := s.repo.orgUnits.FindOne(ctx, bson.D{
		{Key: "_id", Value: storeOID}, {Key: "type", Value: "STORE"}, {Key: "active", Value: true},
	}).Decode(&store); err != nil {
		if isNoDocs(err) {
			return nil // an inactive or unknown store
		}
		return err
	}

	claims := s.repo.accounts.Database().Collection(collStoreInstantAlerts)
	closeUTC := closeAt.UTC()
	claimID := primitive.NewObjectID()
	if _, err := claims.InsertOne(ctx, bson.D{
		{Key: "_id", Value: claimID},
		{Key: "store_id", Value: z.StoreID}, {Key: "kind", Value: kind}, {Key: "close_at", Value: closeUTC},
		{Key: "day", Value: closeAt.In(istZone).Format("2006-01-02")}, {Key: "created_at", Value: now.UTC()},
	}); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil // another tick or instance already alerted for this close
		}
		return err
	}

	params := bson.M{
		"store_id":     z.StoreID,
		"store":        store.Name,
		"window_close": closeUTC.Format(time.RFC3339),
	}
	if kind == templateStoreInstantClosing {
		params["headline"] = "Instant delivery closes at " + closeAt.In(istZone).Format("3:04 PM")
		params["message"] = instantClosingMessage
		if base, limit := instantExtendBase(z, now); !base.Before(limit) {
			params["message"] = instantClosingFinalMessage // extend would refuse
		}
	} else {
		reopen := nextInstantOpening(z, now)
		params["headline"] = "Instant delivery is now closed"
		params["message"] = "It reopens at " + reopen.Format("3:04 PM") + "."
		params["window_reopen"] = reopen.UTC().Format(time.RFC3339)
	}

	managers, err := s.repo.storeManagerRecipients(ctx, storeOID)
	if err == nil {
		for _, m := range managers {
			if err = s.repo.upsertInstantAlert(ctx, m, kind, z.StoreID, params, now); err != nil {
				break
			}
		}
	}
	if err != nil {
		_, _ = claims.DeleteOne(context.WithoutCancel(ctx), bson.D{{Key: "_id", Value: claimID}})
		return err
	}
	s.log.Info("instant closing alert raised", slog.String("store_id", z.StoreID), slog.String("kind", kind),
		slog.String("close_at", closeUTC.Format(time.RFC3339)), slog.Int("recipients", len(managers)))
	return nil
}

// upsertInstantAlert writes one manager's alert in the STORE_LOW_STOCK shape,
// once: keyed on (party, template, store, closing moment), inserted only.
func (r *repository) upsertInstantAlert(ctx context.Context, to adminRecipient, kind, storeID string, params bson.M, now time.Time) error {
	filter := bson.D{
		{Key: "party_id", Value: to.id},
		{Key: "template_key", Value: kind},
		{Key: "params.store_id", Value: storeID},
		{Key: "params.window_close", Value: params["window_close"]},
	}
	update := bson.D{{Key: "$setOnInsert", Value: bson.D{
		{Key: "party_id", Value: to.id},
		{Key: "phone", Value: to.phone},
		{Key: "channel", Value: domain.ChannelApp},
		{Key: "template_key", Value: kind},
		{Key: "language", Value: "hi"},
		{Key: "params", Value: params},
		{Key: "status", Value: domain.NotificationQueued},
		{Key: "queued_at", Value: now.UTC()},
		{Key: "read_at", Value: nil},
	}}}
	if _, err := r.notifications.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true)); err != nil {
		return httpx.Internal(fmt.Errorf("upsert instant alert: %w", err))
	}
	return nil
}

// storeManagerRecipients resolves a store's ACTIVE STORE_MANAGER holders
// (distinct) with their phone, as adminRecipients does for the platform admins.
func (r *repository) storeManagerRecipients(ctx context.Context, storeOID primitive.ObjectID) ([]adminRecipient, error) {
	cur, err := r.roleAssignments.Find(ctx, bson.D{
		{Key: "org_unit_id", Value: storeOID},
		{Key: "role_code", Value: domain.RoleStoreManager},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}, options.Find().SetProjection(bson.D{{Key: "party_id", Value: 1}}).SetLimit(50))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find store managers: %w", err))
	}
	var rows []struct {
		PartyID primitive.ObjectID `bson:"party_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode store managers: %w", err))
	}
	seen := map[primitive.ObjectID]struct{}{}
	ids := make([]primitive.ObjectID, 0, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.PartyID]; ok {
			continue
		}
		seen[row.PartyID] = struct{}{}
		ids = append(ids, row.PartyID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	pcur, err := r.parties.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}},
		options.Find().SetProjection(bson.D{{Key: "phone", Value: 1}}))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find store manager parties: %w", err))
	}
	var prows []struct {
		ID    primitive.ObjectID `bson:"_id"`
		Phone string             `bson:"phone"`
	}
	if err := pcur.All(ctx, &prows); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode store manager parties: %w", err))
	}
	out := make([]adminRecipient, 0, len(prows))
	for _, p := range prows {
		out = append(out, adminRecipient{id: p.ID, phone: p.Phone})
	}
	return out, nil
}
