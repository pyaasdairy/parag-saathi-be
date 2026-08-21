// Consent capture — the DPDP/Play evidence trail AND the CRM promo gate's
// source of truth.
//
//	POST /users/me/consents  (alias /me/consents) — batch upsert current state
//	GET  /users/me/consents  (alias /me/consents) — current state map
//
// Two stores:
//
//   - consumer_consents  — CURRENT STATE, one doc per (consumer, kind), in
//     EXACTLY the shape crmHasPromoConsent reads: {consumer_id ObjectID,
//     kind string, revoked_at null-when-active, created_at grant-time}.
//     The TTL guard evaluates created_at at query time, so created_at is the
//     CLIENT occurred_at (when the human actually consented) — a mirror-queue
//     replay days later must never refresh the consent window.
//   - consumer_consent_log — APPEND-ONLY audit of every record received
//     (client occurred_at + server received_at), deduped by a unique
//     (consumer, kind, occurred_at, granted) index so at-least-once replays
//     append once while same-instant opposite-polarity events both survive.
//
// The CRM guard reads a single kind, "promotional". The app captures
// per-channel marketing consents, so this file maintains "promotional" as a
// DERIVED aggregate doc: active iff ANY marketing_* channel is currently
// granted (created_at = that grant's time), revoked the moment none is.
// Recomputed from the per-channel rows after every batch — convergent under
// replay, and a revoke always lands (opt-out beats a stale grant; the guard's
// fail-closed polarity is never flipped here).
//
// Idempotent & at-least-once safe: a state write applies only when the
// incoming occurred_at is newer than the stored one — EXCEPT a revoke, which
// also applies on an equal timestamp (ties go to opt-out). Replayed or
// out-of-order batches can therefore never roll state backwards.
package consumer

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	collConsentLog   = "consumer_consent_log"
	consentKindPromo = "promotional" // derived aggregate — the ONE kind crmHasPromoConsent reads
)

// consentTypes is the CLOSED enum the app may send. Anything else is a 400 —
// a typo must never mint a consent kind the compliance trail can't account for.
var consentTypes = map[string]bool{
	"privacy_terms":       true,
	"marketing_sms":       true,
	"marketing_whatsapp":  true,
	"marketing_email":     true,
	"marketing_push":      true,
	"disclosure_phone":    true,
	"disclosure_location": true,
}

// marketingConsentKinds feed the derived "promotional" aggregate.
var marketingConsentKinds = []string{"marketing_sms", "marketing_whatsapp", "marketing_email", "marketing_push"}

func isMarketingConsent(kind string) bool { return strings.HasPrefix(kind, "marketing_") }

// consentDoc is the consumer_consents CURRENT-STATE row. Field names are
// LOAD-BEARING: crmHasPromoConsent queries consumer_id / kind / revoked_at /
// created_at literally — never rename these.
type consentDoc struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"`
	Kind       string             `bson:"kind"`
	RevokedAt  *time.Time         `bson:"revoked_at"`  // nil/null = active grant (the guard matches nil)
	CreatedAt  time.Time          `bson:"created_at"`  // grant time (client occurred_at) — the TTL anchor
	OccurredAt time.Time          `bson:"occurred_at"` // last applied client event time — the ordering anchor
	Version    string             `bson:"version,omitempty"`
	Language   string             `bson:"language,omitempty"`
	AppVersion string             `bson:"app_version,omitempty"`
	UpdatedAt  time.Time          `bson:"updated_at"`
}

// consentLogRow is one append-only audit record (consumer_consent_log).
type consentLogRow struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"`
	Kind       string             `bson:"kind"`
	Granted    bool               `bson:"granted"`
	Version    string             `bson:"version,omitempty"`
	Language   string             `bson:"language,omitempty"`
	AppVersion string             `bson:"app_version,omitempty"`
	OccurredAt time.Time          `bson:"occurred_at"` // client event time
	ReceivedAt time.Time          `bson:"received_at"` // server receipt time
}

func (r *repository) consentLog() *mongo.Collection {
	return r.accounts.Database().Collection(collConsentLog)
}

// ── Input & validation ──────────────────────────────────────────────────────

type consentInput struct {
	Type       string    `json:"type"`
	Granted    *bool     `json:"granted"` // pointer: an ABSENT granted must be a 400, never a silent revoke
	Version    string    `json:"version"`
	Language   string    `json:"language"`
	AppVersion string    `json:"app_version"`
	OccurredAt time.Time `json:"occurred_at"` // RFC3339; zero → server now
}

// validateConsentInputs checks the WHOLE batch before anything is written (a
// bad row must never half-apply a batch) and normalizes timestamps in place:
// UTC, defaulted to now when absent, and clamped to now when in the future —
// a skewed client clock must never extend the promo TTL window or wedge a
// later revoke behind an unreachable timestamp.
// maxConsentBatch caps one POST's batch. The legal enum has 7 kinds and the
// app sends at most one row per kind per drain, so 32 is generous headroom —
// the cap only exists so an authenticated client cannot amplify one request
// into an unbounded number of audit+state writes.
const maxConsentBatch = 32

func validateConsentInputs(items []consentInput, now time.Time) *apiError {
	if len(items) == 0 {
		return errBadRequest("consents is required and must be non-empty")
	}
	if len(items) > maxConsentBatch {
		return errBadRequest("too many consent records in one batch")
	}
	for i := range items {
		it := &items[i]
		if !consentTypes[it.Type] {
			return errBadRequest("unknown consent type: " + it.Type)
		}
		if it.Granted == nil {
			return errBadRequest("granted is required for consent type " + it.Type)
		}
		if it.OccurredAt.IsZero() {
			it.OccurredAt = now
		} else {
			it.OccurredAt = it.OccurredAt.UTC()
		}
		if it.OccurredAt.After(now) {
			it.OccurredAt = now
		}
	}
	return nil
}

// ── Repo ────────────────────────────────────────────────────────────────────

// appendConsentLog appends one audit row; a replay (same consumer, kind,
// occurred_at, granted — the unique index) is silently absorbed.
func (r *repository) appendConsentLog(ctx context.Context, row consentLogRow) *apiError {
	if _, err := r.consentLog().InsertOne(ctx, row); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil // at-least-once replay — already on file
		}
		return errInternal("consent log append failed")
	}
	return nil
}

// upsertConsentState applies one record to the (consumer, kind) current-state
// doc under the ordering rule: grants apply only when strictly newer than the
// stored occurred_at; revokes apply when newer OR EQUAL (ties go to opt-out).
// An older replay is a silent no-op — state never rolls backwards.
func (r *repository) upsertConsentState(ctx context.Context, cid primitive.ObjectID, in consentInput, now time.Time) *apiError {
	granted := *in.Granted
	// 1) Conditional update of an existing doc, ordering guard in the filter.
	cond := bson.D{{Key: "consumer_id", Value: cid}, {Key: "kind", Value: in.Type}}
	if granted {
		cond = append(cond, bson.E{Key: "occurred_at", Value: bson.D{{Key: "$lt", Value: in.OccurredAt}}})
	} else {
		cond = append(cond, bson.E{Key: "occurred_at", Value: bson.D{{Key: "$lte", Value: in.OccurredAt}}})
	}
	set := bson.D{
		{Key: "occurred_at", Value: in.OccurredAt},
		{Key: "version", Value: in.Version},
		{Key: "language", Value: in.Language},
		{Key: "app_version", Value: in.AppVersion},
		{Key: "updated_at", Value: now},
	}
	if granted {
		// A (re-)grant resets the TTL anchor to the CLIENT consent time and
		// clears the revocation (literal null — the guard matches nil).
		set = append(set,
			bson.E{Key: "revoked_at", Value: nil},
			bson.E{Key: "created_at", Value: in.OccurredAt})
	} else {
		set = append(set, bson.E{Key: "revoked_at", Value: in.OccurredAt})
	}
	res, err := r.consents.UpdateOne(ctx, cond, bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return errInternal("consent update failed")
	}
	if res.MatchedCount == 1 {
		return nil // applied
	}
	// 2) No match: either the doc doesn't exist yet, or the ordering guard
	// refused an old replay. $setOnInsert-only upsert creates iff absent and
	// no-ops iff present — exactly the two outcomes we want. The unique
	// (consumer_id, kind) index turns a concurrent create into a dup-key,
	// which we absorb: the racing writer's doc is already the current state.
	doc := consentDoc{
		ConsumerID: cid, Kind: in.Type,
		CreatedAt: in.OccurredAt, OccurredAt: in.OccurredAt,
		Version: in.Version, Language: in.Language, AppVersion: in.AppVersion,
		UpdatedAt: now,
	}
	if !granted {
		t := in.OccurredAt
		doc.RevokedAt = &t
	}
	_, err = r.consents.UpdateOne(ctx,
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "kind", Value: in.Type}},
		bson.D{{Key: "$setOnInsert", Value: doc}},
		options.Update().SetUpsert(true))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return errInternal("consent upsert failed")
	}
	return nil
}

// recomputePromoConsent rebuilds the derived "promotional" doc from the
// per-channel marketing rows. Convergent (derived purely from current state),
// so concurrent or replayed batches always settle on the same answer.
// FAIL-CLOSED both ways: no active channel grant → the aggregate is revoked;
// any read/write error → errInternal (the guard itself already fails closed
// on a missing/stale doc).
func (r *repository) recomputePromoConsent(ctx context.Context, cid primitive.ObjectID, now time.Time) *apiError {
	cur, err := r.consents.Find(ctx, bson.D{
		{Key: "consumer_id", Value: cid},
		{Key: "kind", Value: bson.D{{Key: "$in", Value: marketingConsentKinds}}},
	})
	if err != nil {
		return errInternal("consent read failed")
	}
	var rows []consentDoc
	if err := cur.All(ctx, &rows); err != nil {
		return errInternal("consent decode failed")
	}
	var best *consentDoc // most recent ACTIVE grant across channels
	for i := range rows {
		if rows[i].RevokedAt != nil {
			continue
		}
		if best == nil || rows[i].CreatedAt.After(best.CreatedAt) {
			best = &rows[i]
		}
	}
	filter := bson.D{{Key: "consumer_id", Value: cid}, {Key: "kind", Value: consentKindPromo}}
	if best == nil {
		// Opt-out wins: no active marketing grant → stamp the aggregate revoked
		// (no doc at all is equally closed for the guard; only stamp existing).
		if _, err := r.consents.UpdateOne(ctx,
			append(filter, bson.E{Key: "revoked_at", Value: nil}),
			bson.D{{Key: "$set", Value: bson.D{
				{Key: "revoked_at", Value: now}, {Key: "updated_at", Value: now},
			}}}); err != nil {
			return errInternal("consent aggregate update failed")
		}
		return nil
	}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "revoked_at", Value: nil},
		{Key: "created_at", Value: best.CreatedAt}, // the guard's TTL anchor = the human's grant time
		{Key: "occurred_at", Value: best.OccurredAt},
		{Key: "version", Value: best.Version},
		{Key: "language", Value: best.Language},
		{Key: "app_version", Value: best.AppVersion},
		{Key: "updated_at", Value: now},
	}}}
	if _, err := r.consents.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true)); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			// Lost an upsert race on the unique (consumer_id, kind) index —
			// the doc exists now; re-apply as a plain update.
			if _, err2 := r.consents.UpdateOne(ctx, filter, update); err2 == nil {
				return nil
			}
		}
		return errInternal("consent aggregate update failed")
	}
	return nil
}

// Indexes for both collections live in ensureIndexes' FATAL specs table
// (repo.go): the unique (consumer_id, kind) state index IS the opt-out-wins
// invariant (a revoke must never leave a second active grant doc behind for
// crmHasPromoConsent to count), and the unique (consumer_id, kind,
// occurred_at) log index is the audit trail's at-least-once dedup.

// ── Service ─────────────────────────────────────────────────────────────────

func (s *service) applyConsents(ctx context.Context, cid primitive.ObjectID, items []consentInput) *apiError {
	now := time.Now().UTC()
	if aerr := validateConsentInputs(items, now); aerr != nil {
		return aerr
	}
	touchedMarketing := false
	for _, it := range items {
		// Audit first (append-only evidence, deduped), then state. A failure
		// after the audit append is safe: the mirror queue replays the batch,
		// the dup audit row is absorbed, the state write retries.
		if aerr := s.repo.appendConsentLog(ctx, consentLogRow{
			ConsumerID: cid, Kind: it.Type, Granted: *it.Granted,
			Version: it.Version, Language: it.Language, AppVersion: it.AppVersion,
			OccurredAt: it.OccurredAt, ReceivedAt: now,
		}); aerr != nil {
			return aerr
		}
		if aerr := s.repo.upsertConsentState(ctx, cid, it, now); aerr != nil {
			return aerr
		}
		if isMarketingConsent(it.Type) {
			touchedMarketing = true
		}
	}
	if touchedMarketing {
		if aerr := s.repo.recomputePromoConsent(ctx, cid, now); aerr != nil {
			return aerr
		}
	}
	return nil
}

// consentStateView is one entry of the GET map — exactly what the app needs
// to rehydrate its local consent cache after a reinstall.
type consentStateView struct {
	Granted    bool      `json:"granted"`
	Version    string    `json:"version,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

func (s *service) consentState(ctx context.Context, cid primitive.ObjectID) (map[string]consentStateView, *apiError) {
	cur, err := s.repo.consents.Find(ctx, bson.D{{Key: "consumer_id", Value: cid}})
	if err != nil {
		return nil, errInternal("consent read failed")
	}
	var rows []consentDoc
	if err := cur.All(ctx, &rows); err != nil {
		return nil, errInternal("consent decode failed")
	}
	out := make(map[string]consentStateView, len(rows))
	for _, d := range rows {
		if !consentTypes[d.Kind] {
			continue // the derived "promotional" aggregate (and anything else internal) never leaves the server
		}
		out[d.Kind] = consentStateView{
			Granted:    d.RevokedAt == nil,
			Version:    d.Version,
			OccurredAt: d.OccurredAt,
		}
	}
	return out, nil
}

// ── HTTP ────────────────────────────────────────────────────────────────────

func (h *handler) getConsents(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	state, err := h.svc.consentState(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"consents": state})
}

func (h *handler) postConsents(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Consents []consentInput `json:"consents"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.svc.applyConsents(r.Context(), id, body.Consents); err != nil {
		writeErr(w, err)
		return
	}
	// Echo the resulting state map — the app can reconcile its cache in one round trip.
	state, err := h.svc.consentState(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"consents": state})
}
