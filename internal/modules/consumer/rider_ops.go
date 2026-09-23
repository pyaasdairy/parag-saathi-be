package consumer

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// ─────────────────────────────────────────────────────────────────────────────
// Rider operations console — the surface behind /api/v1/consumer/delivery/rider
//
// The last-mile task lifecycle (offer → claim → pickup → deliver) already lives
// in delivery_svc.go. THIS file and its rider_ops_*.go siblings add everything
// the rider console needs AROUND that lifecycle: duty attendance, the day's
// route, cash collection, crate inventory, earnings, penalties, performance,
// KYC documents, support, referral and emergency contacts — plus the per-task
// extras (customer OTP, product scan, door photo, undo).
//
// These are OPERATOR routes. They mount inside the existing DELIVERY_RIDER
// group in module.go, so they inherit Authenticate + RequireRoles, and they
// speak the operator wire format: httpx.JSON writes {"data": …} and httpx.Error
// writes {"error":{code,message}}. Never use the consumer-side writeJSON here —
// the Flutter client unwraps the {data} envelope.
//
// The contract is pinned by the client, which is already written and shipped
// against it: pyaas-saathi/docs/RIDER_API.md, lib/api/rider_api.dart and
// lib/models/rider.dart. Reads are parsed defensively there (snake_case and
// camelCase both accepted, nulls tolerated) but WRITES arrive snake_case, so
// every request DTO below uses snake_case json tags.
//
// Rider identity always comes from the ROLE token, never from a body field —
// see riderActor. A rider can only ever read or mutate their own records.
// ─────────────────────────────────────────────────────────────────────────────

// Rider-ops collections. All are new and owned by this module; none of them
// touch operator supply-side state.
const (
	collRiderProfiles    = "rider_profiles"
	collRiderAttendance  = "rider_attendance"
	collRiderRouteDays   = "rider_route_days"
	collRiderCash        = "rider_cash_requests"
	collRiderInventory   = "rider_inventory_sessions"
	collRiderEarnings    = "rider_earnings"
	collRiderPenalties   = "rider_penalties"
	collRiderFeedback    = "rider_feedback"
	collRiderDocuments   = "rider_documents"
	collRiderSupport     = "rider_support_concerns"
	collRiderReferrals   = "rider_referrals"
	collRiderEmergency   = "rider_emergency_contacts"
	collRiderTaskScans   = "rider_task_scans"
	collDeliveryOTP      = "consumer_delivery_otp"
	collDoorReferencePic = "consumer_door_photos"
)

// riderColl hands back a rider-ops collection from the same database the rest
// of the module uses. Derived from an existing handle so the repository struct
// stays untouched — these collections are read and written only from the
// rider_ops_*.go files.
func (r *repository) riderColl(name string) *mongo.Collection {
	return r.deliveries.Database().Collection(name)
}

// ── identity ────────────────────────────────────────────────────────────────

// riderActor resolves the authenticated DELIVERY_RIDER from the ROLE token.
// The party id IS the rider id everywhere in this surface — no endpoint ever
// takes a rider id from the body or the query string, so one rider can never
// read or mutate another's attendance, cash, earnings or penalties.
func riderActor(r *http.Request) (auth.Actor, error) {
	a, ok := auth.ActorFrom(r.Context())
	if !ok || strings.TrimSpace(a.PartyID) == "" {
		return auth.Actor{}, httpx.Unauthorized("authentication required")
	}
	return a, nil
}

// ── time ────────────────────────────────────────────────────────────────────

// Every "today" in the rider console — attendance, route, inventory, the daily
// performance timeline — is an IST calendar day, because that is the day the
// rider and the centre are working. Deriving it from UTC would roll the day
// over at 05:30 IST, in the middle of the morning milk run. istDay/istZone are
// the module's existing IST helpers (crm_offers.go, trial.go); the bounds
// helpers below extend them to range queries.

// istDayBounds returns [start, end) in UTC for the IST calendar day `day`
// ("2006-01-02"), for range queries against UTC-stored timestamps.
func istDayBounds(day string) (time.Time, time.Time, error) {
	d, err := time.ParseInLocation("2006-01-02", day, istZone)
	if err != nil {
		return time.Time{}, time.Time{}, httpx.BadRequest("INVALID_DATE", "date must be YYYY-MM-DD")
	}
	return d.UTC(), d.AddDate(0, 0, 1).UTC(), nil
}

// istMonth is the business month a moment belongs to, in IST: "2006-01".
func istMonth(t time.Time) string { return t.In(istZone).Format("2006-01") }

// istMonthBounds returns [start, end) in UTC for the IST month `month` ("2006-01").
func istMonthBounds(month string) (time.Time, time.Time, error) {
	m, err := time.ParseInLocation("2006-01", month, istZone)
	if err != nil {
		return time.Time{}, time.Time{}, httpx.BadRequest("INVALID_MONTH", "month must be YYYY-MM")
	}
	return m.UTC(), m.AddDate(0, 1, 0).UTC(), nil
}

// rfc3339 renders a timestamp the way every other wire field in this backend
// does, and the way Go's json decoder will accept it back.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// rfc3339Ptr renders an optional timestamp, omitting the zero value.
func rfc3339Ptr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return rfc3339(*t)
}

// Money: every rupee amount this surface returns or stores goes through the
// module's existing round2 (service.go), so a float never renders as
// 639.9999999999999 on a rider's payout screen.

// ── OTP ─────────────────────────────────────────────────────────────────────

// otpDigits is the length of every OTP this surface issues (customer delivery
// OTP and cash-collection OTP), matching the login OTP.
const otpDigits = 6

// otpTTL is how long an issued OTP stays valid.
const otpTTL = 10 * time.Minute

// otpMaxAttempts caps verification attempts per issued code before it is burned.
const otpMaxAttempts = 5

// newOTP mints a cryptographically random numeric code. crypto/rand, not
// math/rand: a guessable delivery OTP is a way to mark someone else's parcel
// delivered.
func newOTP() (string, error) {
	const digits = "0123456789"
	b := make([]byte, otpDigits)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(digits))))
		if err != nil {
			return "", httpx.Internal(fmt.Errorf("mint otp: %w", err))
		}
		b[i] = digits[n.Int64()]
	}
	return string(b), nil
}

// riderOTPChallenge is one issued code. The plaintext is never stored — only a hash
// keyed by the server's OTP secret, exactly as the login OTP is handled.
type riderOTPChallenge struct {
	Scope     string    `bson:"scope"`  // "delivery" | "cash"
	RefID     string    `bson:"ref_id"` // delivery id / cash request id
	Rider     string    `bson:"rider_party_id"`
	CodeHash  string    `bson:"code_hash"`
	Attempts  int       `bson:"attempts"`
	ExpiresAt time.Time `bson:"expires_at"`
	CreatedAt time.Time `bson:"created_at"`
}

// issueRiderOTP mints, hashes and upserts a challenge for (scope, refID), replacing
// any previous unexpired code for the same target. Returns the plaintext so the
// caller can hand it to the SMS sender — it is never returned to the rider.
func (s *service) issueRiderOTP(ctx context.Context, scope, refID, riderPartyID string) (string, error) {
	code, err := newOTP()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	doc := riderOTPChallenge{
		Scope:     scope,
		RefID:     refID,
		Rider:     riderPartyID,
		CodeHash:  auth.HMACHash(s.deps.Cfg.OTPHashSecret, scope+":"+refID+":"+code),
		Attempts:  0,
		ExpiresAt: now.Add(otpTTL),
		CreatedAt: now,
	}
	filter := bson.D{{Key: "scope", Value: scope}, {Key: "ref_id", Value: refID}}
	if _, err := s.repo.riderColl(collDeliveryOTP).ReplaceOne(ctx, filter, doc, options.Replace().SetUpsert(true)); err != nil {
		return "", httpx.Internal(fmt.Errorf("store otp challenge: %w", err))
	}
	return code, nil
}

// errOTPInvalid is the single verification failure the client switches on. It
// deliberately does not distinguish "wrong code" from "expired" from "too many
// attempts" in the message, so a caller cannot probe the challenge state.
var errOTPInvalid = httpx.BadRequest("OTP_INVALID", "that code is not valid — ask the customer to read it again")

// verifyRiderOTP checks a submitted code against the stored challenge, counting the
// attempt. A correct code consumes the challenge (single use).
func (s *service) verifyRiderOTP(ctx context.Context, scope, refID, code string) error {
	code = strings.TrimSpace(code)
	if len(code) != otpDigits {
		return errOTPInvalid
	}
	coll := s.repo.riderColl(collDeliveryOTP)
	filter := bson.D{{Key: "scope", Value: scope}, {Key: "ref_id", Value: refID}}
	var ch riderOTPChallenge
	if err := coll.FindOne(ctx, filter).Decode(&ch); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return errOTPInvalid
		}
		return httpx.Internal(fmt.Errorf("load otp challenge: %w", err))
	}
	if time.Now().UTC().After(ch.ExpiresAt) {
		// Expired. Safe to drop: callers gate on `expires_at > now`, so an
		// expired challenge already counts as "no live gate" either way, and
		// the TTL index would reap it regardless.
		_, _ = coll.DeleteOne(ctx, filter)
		return errOTPInvalid
	}
	if ch.Attempts >= otpMaxAttempts {
		// EXHAUSTED — and deliberately NOT deleted.
		//
		// Deleting it here was a way to remove the gate rather than close it:
		// riderCashOTPPending (rider_ops_money.go) decides whether an OTP is
		// required by counting live challenges for the reference, so a rider who
		// asked for a code in front of the customer and then posted six wrong
		// ones would find the challenge gone and the next collect accepted with
		// no verification at all — the exact opposite of what burning the
		// attempts should mean. Leaving the row in place keeps the gate closed
		// until a NEW code is issued (which replaces this row) or it expires.
		return errOTPInvalid
	}
	// Count the attempt BEFORE comparing, so a crash mid-verify cannot hand an
	// attacker a free guess.
	if _, err := coll.UpdateOne(ctx, filter, bson.D{{Key: "$inc", Value: bson.D{{Key: "attempts", Value: 1}}}}); err != nil {
		return httpx.Internal(fmt.Errorf("count otp attempt: %w", err))
	}
	want := auth.HMACHash(s.deps.Cfg.OTPHashSecret, scope+":"+refID+":"+code)
	if subtle.ConstantTimeCompare([]byte(want), []byte(ch.CodeHash)) != 1 {
		return errOTPInvalid
	}
	_, _ = coll.DeleteOne(ctx, filter) // single use
	return nil
}

// ── ids ─────────────────────────────────────────────────────────────────────

// newRiderOpsID mints a short opaque id for records the client round-trips as
// an opaque string (cash requests, penalties, concerns, inventory sessions).
func newRiderOpsID(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// A failed CSPRNG read is fatal for uniqueness; fall back to a
		// timestamp so the write still succeeds rather than colliding on "".
		return prefix + "_" + fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// ── shared response shapes ──────────────────────────────────────────────────

// okResponse is the acknowledgement several rider endpoints return.
type okResponse struct {
	OK bool `json:"ok"`
}

// savedResponse acknowledges a stored artefact.
type savedResponse struct {
	Saved bool `json:"saved"`
}

// statusResponse reports a record's new state after a transition.
type statusResponse struct {
	Status string `json:"status"`
}

// faceVerifyResponse is the attendance result the client parses into
// FaceVerifyResult (lib/models/rider.dart). See rider_ops_attendance.go for
// what `matched` actually asserts — it is an attendance decision, NOT a
// biometric match claim.
type faceVerifyResponse struct {
	Matched          bool    `json:"matched"`
	Score            float64 `json:"score,omitempty"`
	Message          string  `json:"message,omitempty"`
	AttendanceMarked bool    `json:"attendance_marked,omitempty"`
}

// ── route registration ──────────────────────────────────────────────────────

// registerRiderOps mounts every rider-console route. Called from module.go
// INSIDE the existing DELIVERY_RIDER group, so all of these already sit behind
// Authenticate + RequireRoles(DELIVERY_RIDER).
//
// Handler methods live in the rider_ops_*.go files:
//
//	rider_ops_profile.go     me, documents, support, referral, emergency contacts, nd-reasons
//	rider_ops_attendance.go  attendance today/check-in/check-out/history, face enrol
//	rider_ops_route.go       route today/complete, inventory today/verify
//	rider_ops_money.go       cash list/otp/collect/undo, earnings, penalties/concern/waiver
//	rider_ops_performance.go performance, feedback, daily timeline
//	rider_ops_tasks.go       per-task otp send/verify, scan, door photo, undo, compliance
func registerRiderOps(dr chi.Router, h *handler) {
	// Per-task extras — these hang off the existing /delivery/tasks/{deliveryId}
	// lane and act on one delivery the rider currently holds.
	dr.Post("/delivery/tasks/{deliveryId}/otp/send", h.riderTaskOTPSend)
	dr.Post("/delivery/tasks/{deliveryId}/otp/verify", h.riderTaskOTPVerify)
	dr.Post("/delivery/tasks/{deliveryId}/scan", h.riderTaskScan)
	dr.Post("/delivery/tasks/{deliveryId}/door-photo", h.riderTaskDoorPhoto)
	dr.Post("/delivery/tasks/{deliveryId}/undo", h.riderTaskUndo)
	dr.Get("/delivery/tasks/{deliveryId}/compliance", h.riderTaskCompliance)

	dr.Route("/delivery/rider", func(rr chi.Router) {
		rr.Get("/me", h.riderMe)
		rr.Get("/nd-reasons", h.riderNDReasons)

		rr.Route("/attendance", func(ar chi.Router) {
			ar.Get("/today", h.riderAttendanceToday)
			ar.Post("/check-in", h.riderCheckIn)
			ar.Post("/check-out", h.riderCheckOut)
			ar.Get("/history", h.riderAttendanceHistory)
		})
		rr.Post("/face/enrol", h.riderFaceEnrol)

		rr.Route("/route", func(rt chi.Router) {
			rt.Get("/today", h.riderRouteToday)
			rt.Post("/complete", h.riderRouteComplete)
		})

		rr.Route("/cash", func(cr chi.Router) {
			cr.Get("/", h.riderCashList)
			cr.Post("/{cashId}/otp", h.riderCashOTP)
			cr.Post("/{cashId}/collect", h.riderCashCollect)
			cr.Post("/{cashId}/undo", h.riderCashUndo)
		})

		rr.Route("/inventory", func(ir chi.Router) {
			ir.Get("/today", h.riderInventoryToday)
			ir.Post("/{sessionId}/verify", h.riderInventoryVerify)
		})

		rr.Get("/earnings", h.riderEarnings)

		rr.Route("/penalties", func(pr chi.Router) {
			pr.Get("/", h.riderPenalties)
			pr.Post("/{penaltyId}/concern", h.riderPenaltyConcern)
			pr.Post("/{penaltyId}/waiver", h.riderPenaltyWaiver)
		})

		rr.Route("/performance", func(pf chi.Router) {
			pf.Get("/", h.riderPerformance)
			pf.Get("/feedback", h.riderPerformanceFeedback)
			pf.Get("/daily", h.riderPerformanceDaily)
		})

		rr.Route("/documents", func(dc chi.Router) {
			dc.Get("/", h.riderDocuments)
			dc.Post("/", h.riderUploadDocument)
		})

		rr.Route("/support", func(sp chi.Router) {
			sp.Get("/types", h.riderSupportTypes)
			sp.Get("/concerns", h.riderSupportConcerns)
			sp.Post("/concerns", h.riderRaiseConcern)
		})

		rr.Get("/referral", h.riderReferral)

		rr.Get("/emergency-contacts", h.riderEmergencyContacts)
		rr.Put("/emergency-contacts", h.riderSetEmergencyContacts)
	})
}

// ensureRiderOpsIndexes creates the indexes the rider console's hot paths need.
// Called once at startup alongside the module's other index work. Every query
// this surface runs is scoped by rider_party_id, so that is the leading key of
// nearly every index here — without them each screen open is a collection scan
// that grows with every rider on the platform.
func (r *repository) ensureRiderOpsIndexes(ctx context.Context) error {
	specs := []struct {
		coll string
		keys bson.D
		opts *options.IndexOptions
	}{
		// One attendance record per rider per IST day — the unique index is what
		// makes a double check-in impossible even under a double-tap race.
		{collRiderAttendance, bson.D{{Key: "rider_party_id", Value: 1}, {Key: "day", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderAttendance, bson.D{{Key: "rider_party_id", Value: 1}, {Key: "day", Value: -1}}, nil},
		{collRiderProfiles, bson.D{{Key: "rider_party_id", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderProfiles, bson.D{{Key: "referral_code", Value: 1}}, options.Index().SetSparse(true)},
		{collRiderRouteDays, bson.D{{Key: "rider_party_id", Value: 1}, {Key: "day", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderCash, bson.D{{Key: "rider_party_id", Value: 1}, {Key: "status", Value: 1}}, nil},
		{collRiderCash, bson.D{{Key: "cash_id", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderInventory, bson.D{{Key: "session_id", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderInventory, bson.D{{Key: "rider_party_id", Value: 1}, {Key: "day", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderEarnings, bson.D{{Key: "rider_party_id", Value: 1}, {Key: "month", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderPenalties, bson.D{{Key: "penalty_id", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderPenalties, bson.D{{Key: "rider_party_id", Value: 1}, {Key: "applied_at", Value: -1}}, nil},
		{collRiderFeedback, bson.D{{Key: "rider_party_id", Value: 1}, {Key: "at", Value: -1}}, nil},
		{collRiderDocuments, bson.D{{Key: "rider_party_id", Value: 1}, {Key: "type", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderSupport, bson.D{{Key: "concern_id", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderSupport, bson.D{{Key: "rider_party_id", Value: 1}, {Key: "raised_at", Value: -1}}, nil},
		{collRiderReferrals, bson.D{{Key: "referrer_party_id", Value: 1}}, nil},
		{collRiderEmergency, bson.D{{Key: "rider_party_id", Value: 1}}, options.Index().SetUnique(true)},
		// One scan binding per physical code — this unique index IS the
		// "already bound to another task" rejection.
		{collRiderTaskScans, bson.D{{Key: "code", Value: 1}}, options.Index().SetUnique(true)},
		{collRiderTaskScans, bson.D{{Key: "delivery_id", Value: 1}}, nil},
		// OTP challenges expire themselves.
		{collDeliveryOTP, bson.D{{Key: "scope", Value: 1}, {Key: "ref_id", Value: 1}}, options.Index().SetUnique(true)},
		{collDeliveryOTP, bson.D{{Key: "expires_at", Value: 1}}, options.Index().SetExpireAfterSeconds(0)},
		{collDoorReferencePic, bson.D{{Key: "address_key", Value: 1}}, options.Index().SetUnique(true)},
	}
	// Keep going after a failure instead of returning: this loop creates the
	// UNIQUE indexes that enforce one attendance record per rider per day, one
	// binding per crate label and one cash due per delivery. Bailing out on the
	// first error silently skipped every spec after it, so a single pre-existing
	// index with different options (an earlier deploy, a hand-created index)
	// would quietly leave the rest of those guards uncreated — and the handlers
	// rely on a duplicate-key error to make a double check-in impossible. The
	// caller logs this and boots anyway, so it must describe EVERYTHING missing.
	var errs []error
	for _, sp := range specs {
		model := mongo.IndexModel{Keys: sp.keys, Options: sp.opts}
		if _, err := r.riderColl(sp.coll).Indexes().CreateOne(ctx, model); err != nil {
			errs = append(errs, fmt.Errorf("rider-ops index on %s: %w", sp.coll, err))
		}
	}
	return errors.Join(errs...)
}
