package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// ─────────────────────────────────────────────────────────────────────────────
// Per-task extras — RIDER_API.md §2.1–2.5.
//
// Everything here hangs off ONE delivery the rider is currently holding:
// the customer's door OTP, the crate/bag scan, the door reference photo, the
// time-boxed undo of a completed marking, and the door step checklist.
//
// The lifecycle itself (offer → claim → pickup → deliver/fail) stays in
// delivery_svc.go; these endpoints only ever decorate or reverse it.
//
// EVERY handler in this file starts the same way, and it is the security
// contract of the whole surface: riderTaskForCaller loads the delivery by the
// path id and returns 404 unless its rider_party_id is the caller's party id.
// 404 and not 403 — a rider probing ids must not be able to learn that someone
// else's task exists.
// ─────────────────────────────────────────────────────────────────────────────

const (
	// riderOTPResendCooldown throttles customer delivery OTPs. The rider taps
	// "resend" when the customer says "nothing came"; without a cooldown that
	// tap SMS-bombs the customer (and bills us per message).
	riderOTPResendCooldown = 60 * time.Second

	// riderUndoWindow is how long after a DELIVERED/FAILED marking the rider may
	// still reverse it. Short on purpose: past this the money has settled in the
	// customer's mind (and in their ledger) and the correction belongs to
	// support, not to the person holding the phone at the door.
	riderUndoWindow = 15 * time.Minute

	// Input bounds. A scan payload is a handful of crate labels, not a catalogue.
	// (riderMaxReasonLen / riderMaxPhotoRefLen are the surface-wide bounds
	// already defined for the money routes — free text is capped identically
	// wherever a rider types it.)
	riderMaxScanCodes   = 60
	riderMaxScanCodeLen = 64
	riderMinScanCodeLen = 3
)

// otpScopeDelivery is the OTP namespace for the customer's door code, keyed by
// delivery id (the cash-collection surface owns its own "cash" scope).
const otpScopeDelivery = "delivery"

// riderSentResponse is the §2.1 send acknowledgement. The code itself is NEVER
// in a response body: the rider must read it off the customer's phone, which is
// the entire point of the check.
type riderSentResponse struct {
	Sent bool `json:"sent"`
}

// riderTaskForCaller loads the delivery named by {deliveryId} and proves the
// caller owns it. Any failure is a 404 — an unknown id and someone else's task
// are deliberately indistinguishable.
func (h *handler) riderTaskForCaller(r *http.Request) (*delivery, error) {
	actor, err := riderActor(r)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(chi.URLParam(r, "deliveryId"))
	if id == "" {
		return nil, httpx.BadRequest("INVALID_ID", "a delivery id is required")
	}
	d, derr := h.svc.repo.findDeliveryByID(r.Context(), id)
	if derr != nil {
		return nil, toHTTPErr(derr) // already a typed not-found
	}
	if d.RiderPartyID == "" || d.RiderPartyID != actor.PartyID {
		return nil, httpx.NotFound("delivery")
	}
	return d, nil
}

// ── 2.1 Customer delivery OTP ───────────────────────────────────────────────

// riderTaskOTPSend issues the customer's 6-digit door code and SMSes it to the
// number on the task. POST /consumer/delivery/tasks/{deliveryId}/otp/send.
//
// Only meaningful for a task that is actually out for delivery: a code sent
// while the parcel is still at the store is a code that has expired by the time
// the rider reaches the door.
func (h *handler) riderTaskOTPSend(w http.ResponseWriter, r *http.Request) {
	d, err := h.riderTaskForCaller(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if d.Status != "OUT_FOR_DELIVERY" {
		httpx.Error(w, r, httpx.Conflict("NOT_OUT", "mark the order picked up before sending the customer's code"))
		return
	}
	phone := strings.TrimSpace(d.Phone)
	if phone == "" {
		// Honest failure: we cannot text a number we do not hold. The rider
		// falls back to photo + geotag, which are mandatory proof anyway.
		httpx.Error(w, r, httpx.Unprocessable("NO_CUSTOMER_PHONE", "this order has no customer phone number to send a code to"))
		return
	}
	ctx := r.Context()
	// Claim the send slot BEFORE minting anything. The claim is a single
	// conditional upsert, so two taps racing on two replicas still produce one
	// SMS (see riderClaimOTPSendSlot).
	ok, cerr := h.svc.repo.riderClaimOTPSendSlot(ctx, otpScopeDelivery, d.ID, riderOTPResendCooldown)
	if cerr != nil {
		httpx.Error(w, r, cerr)
		return
	}
	if !ok {
		httpx.Error(w, r, httpx.TooManyRequests("a code was just sent — wait a minute before sending another"))
		return
	}
	code, ierr := h.svc.issueRiderOTP(ctx, otpScopeDelivery, d.ID, d.RiderPartyID)
	if ierr != nil {
		httpx.Error(w, r, ierr)
		return
	}
	if h.svc.sms.Enabled() {
		if serr := h.svc.sms.SendOTP(ctx, phone, code); serr != nil {
			// The SMS did not go out, so the code is unusable. Burn the
			// challenge (and with it the cooldown slot) on a detached context —
			// the request context may already be cancelled — so the rider can
			// retry immediately instead of waiting out a minute for a code the
			// customer never received.
			h.svc.riderDropOTPChallenge(otpScopeDelivery, d.ID)
			h.svc.log.ErrorContext(ctx, "delivery otp sms send failed", "delivery", d.ID, "err", serr)
			httpx.Error(w, r, httpx.Internal(fmt.Errorf("send delivery otp sms: %w", serr)))
			return
		}
	} else if h.svc.deps.Cfg.OTPDevMode {
		// Dev/demo only: with no SMS transport configured the code has to reach
		// the operator somehow, and the log is the one place the RIDER cannot
		// see it. Never on a production build.
		h.svc.log.InfoContext(ctx, "delivery otp (dev echo)", "delivery", d.ID, "otp", code)
	} else {
		// Production with no transport is a misconfiguration, not a success —
		// claiming "sent" here would have the rider waiting at a door for an SMS
		// that will never arrive.
		h.svc.riderDropOTPChallenge(otpScopeDelivery, d.ID)
		httpx.Error(w, r, httpx.Internal(errors.New("delivery otp: sms transport is not configured")))
		return
	}
	httpx.JSON(w, http.StatusOK, riderSentResponse{Sent: true})
}

// riderOTPVerifyRequest is the door code the rider keyed in. `otp` is what the
// client sends; `code` is accepted as an alias for parity with the login route.
type riderOTPVerifyRequest struct {
	OTP  string `json:"otp"`
	Code string `json:"code"`
}

// riderTaskOTPVerify checks the customer's door code.
// POST /consumer/delivery/tasks/{deliveryId}/otp/verify.
//
// A correct code is single-use (verifyRiderOTP consumes the challenge) and is
// stamped on the task as evidence — "the customer read us their code at 06:12"
// is exactly the record a delivery dispute needs.
func (h *handler) riderTaskOTPVerify(w http.ResponseWriter, r *http.Request) {
	d, err := h.riderTaskForCaller(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderOTPVerifyRequest
	if derr := httpx.DecodeJSON(r, &req); derr != nil {
		httpx.Error(w, r, derr)
		return
	}
	code := strings.TrimSpace(req.OTP)
	if code == "" {
		code = strings.TrimSpace(req.Code)
	}
	if d.Status != "OUT_FOR_DELIVERY" {
		httpx.Error(w, r, httpx.Conflict("NOT_OUT", "this order is not out for delivery"))
		return
	}
	ctx := r.Context()
	if verr := h.svc.verifyRiderOTP(ctx, otpScopeDelivery, d.ID, code); verr != nil {
		httpx.Error(w, r, verr) // OTP_INVALID — the code the client switches on
		return
	}
	// Evidence, best-effort: a failed stamp must not fail a verification the
	// rider has already passed at the door. Guarded to the owner + the state we
	// verified in, so it can never touch a task that moved on underneath us.
	_, _ = h.svc.repo.deliveries.UpdateOne(ctx,
		bson.D{
			{Key: "delivery_id", Value: d.ID},
			{Key: "rider_party_id", Value: d.RiderPartyID},
			{Key: "status", Value: "OUT_FOR_DELIVERY"},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "otp_verified_at", Value: time.Now().UTC()},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}})
	httpx.JSON(w, http.StatusOK, okResponse{OK: true})
}

// riderClaimOTPSendSlot is the atomic resend throttle. It bumps the challenge's
// created_at only when the last one is older than `cooldown`; the (scope,
// ref_id) unique index turns every racing caller into a duplicate-key error,
// which is precisely the "too soon" answer.
//
// Racing on the very first send is covered too: the upsert INSERTS a stub the
// challenge write then replaces, so exactly one caller can win the slot.
func (r *repository) riderClaimOTPSendSlot(ctx context.Context, scope, refID string, cooldown time.Duration) (bool, error) {
	now := time.Now().UTC()
	filter := bson.D{
		{Key: "scope", Value: scope},
		{Key: "ref_id", Value: refID},
		{Key: "created_at", Value: bson.D{{Key: "$lte", Value: now.Add(-cooldown)}}},
	}
	update := bson.D{{Key: "$set", Value: bson.D{{Key: "created_at", Value: now}}}}
	if _, err := r.riderColl(collDeliveryOTP).UpdateOne(ctx, filter, update, options.Update().SetUpsert(true)); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil // a live challenge is younger than the cooldown
		}
		return false, httpx.Internal(fmt.Errorf("claim otp send slot: %w", err))
	}
	return true, nil
}

// riderDropOTPChallenge deletes an issued challenge whose code never reached the
// customer. Detached context: the caller is usually in an error path where the
// request context is already gone, and leaving the challenge behind would keep
// the rider locked out by the resend cooldown for a code nobody has.
func (s *service) riderDropOTPChallenge(scope, refID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.repo.riderColl(collDeliveryOTP).DeleteOne(ctx,
		bson.D{{Key: "scope", Value: scope}, {Key: "ref_id", Value: refID}})
}

// ── 2.2 Product scan ────────────────────────────────────────────────────────

type riderScanRequest struct {
	Codes []string `json:"codes"`
}

// riderScanResponse tells the rider exactly which labels did not stick, so the
// wrong crate is found at the van and not at the customer's door.
type riderScanResponse struct {
	Accepted int      `json:"accepted"`
	Rejected []string `json:"rejected"`
}

// riderScanBinding ties one physical crate/bag label to one delivery. The
// unique index on `code` (rider_ops.go) IS the business rule: a label lives on
// exactly one task until someone unbinds it.
type riderScanBinding struct {
	Code         string    `bson:"code"`
	DeliveryID   string    `bson:"delivery_id"`
	OrderID      string    `bson:"order_id"`
	RiderPartyID string    `bson:"rider_party_id"`
	ScannedAt    time.Time `bson:"scanned_at"`
}

// riderTaskScan binds the scanned crate/bag codes to this task.
// POST /consumer/delivery/tasks/{deliveryId}/scan.
//
// The client re-posts the WHOLE list every time the scan screen is closed, so
// re-scanning a code already bound to this task must count as accepted, not
// rejected — otherwise a rider who adds one more crate gets everything they
// scanned before thrown back at them as errors.
func (h *handler) riderTaskScan(w http.ResponseWriter, r *http.Request) {
	d, err := h.riderTaskForCaller(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if d.Status == "DELIVERED" || d.Status == "FAILED" || d.Status == "CANCELLED" {
		httpx.Error(w, r, httpx.Conflict("TASK_CLOSED", "this order is already closed"))
		return
	}
	var req riderScanRequest
	if derr := httpx.DecodeJSON(r, &req); derr != nil {
		httpx.Error(w, r, derr)
		return
	}
	if len(req.Codes) == 0 {
		httpx.Error(w, r, httpx.BadRequest("NO_CODES", "send at least one scanned code"))
		return
	}
	if len(req.Codes) > riderMaxScanCodes {
		httpx.Error(w, r, httpx.BadRequest("TOO_MANY_CODES",
			fmt.Sprintf("at most %d codes per scan", riderMaxScanCodes)))
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	accepted := 0
	rejected := []string{}
	seen := make(map[string]struct{}, len(req.Codes))
	for _, raw := range req.Codes {
		code, ok := riderNormalizeScanCode(raw)
		if !ok {
			// A label we cannot store is reported back rather than silently
			// dropped: the rider needs to see WHICH one to key in again.
			if trimmed := strings.TrimSpace(raw); trimmed != "" {
				rejected = append(rejected, trimmed)
			}
			continue
		}
		if _, dup := seen[code]; dup {
			continue // the same label twice in one payload is one binding
		}
		seen[code] = struct{}{}
		bound, berr := h.svc.repo.riderBindScanCode(ctx, riderScanBinding{
			Code: code, DeliveryID: d.ID, OrderID: d.OrderID,
			RiderPartyID: d.RiderPartyID, ScannedAt: now,
		})
		if berr != nil {
			httpx.Error(w, r, berr)
			return
		}
		if bound {
			accepted++
			continue
		}
		rejected = append(rejected, code)
	}
	httpx.JSON(w, http.StatusOK, riderScanResponse{Accepted: accepted, Rejected: rejected})
}

// riderNormalizeScanCode trims + uppercases a label and checks it looks like a
// printed code. The client already normalises; we repeat it because the wire is
// not the client, and a stored code that differs only in case would defeat the
// unique index that makes the binding exclusive.
func riderNormalizeScanCode(raw string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if len(code) < riderMinScanCodeLen || len(code) > riderMaxScanCodeLen {
		return "", false
	}
	for _, c := range code {
		switch {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '/':
		default:
			return "", false
		}
	}
	return code, true
}

// riderReleaseScanCodes frees every crate label bound to a delivery.
//
// The unique index on `code` is what makes "this label is already on another
// task" detectable, but it is also a permanent claim — so every path that ends
// a task's claim on its crates has to release them, or the label is retired
// from the system the first time it is scanned onto a task that is later undone.
func (r *repository) riderReleaseScanCodes(ctx context.Context, deliveryID string) (int64, error) {
	res, err := r.riderColl(collRiderTaskScans).DeleteMany(ctx,
		bson.D{{Key: "delivery_id", Value: deliveryID}})
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("release scan codes: %w", err))
	}
	return res.DeletedCount, nil
}

// riderBindScanCode inserts one binding. bound=true means the code now belongs
// to this delivery — either because we just wrote it, or because it was already
// bound to THIS task (an idempotent re-scan). bound=false means the label is
// held by a different task and the rider has the wrong crate in hand.
func (r *repository) riderBindScanCode(ctx context.Context, b riderScanBinding) (bool, error) {
	coll := r.riderColl(collRiderTaskScans)
	if _, err := coll.InsertOne(ctx, b); err != nil {
		if !mongo.IsDuplicateKeyError(err) {
			return false, httpx.Internal(fmt.Errorf("bind scan code: %w", err))
		}
		var existing riderScanBinding
		e := coll.FindOne(ctx, bson.D{{Key: "code", Value: b.Code}}).Decode(&existing)
		if errors.Is(e, mongo.ErrNoDocuments) {
			// Unbound between our insert and this read (another task released
			// it). Report it as rejected rather than guessing — the rider
			// re-scans and the next attempt succeeds cleanly.
			return false, nil
		}
		if e != nil {
			return false, httpx.Internal(fmt.Errorf("read scan binding: %w", e))
		}
		if existing.DeliveryID == b.DeliveryID {
			return true, nil // re-scan of a label already on this task
		}
		// SELF-HEAL: crates are REUSED every morning, but a binding released
		// only on undo made each label single-use — one delivered task and the
		// physical crate was unscannable forever. A label whose holding task is
		// already CLOSED (delivered/failed/cancelled) is stale by definition:
		// steal it for the live task. The delete is guarded on the exact
		// (code, delivery_id) pair, so racing scanners fall through to a plain
		// rejected and simply re-scan.
		if holder, herr := r.findDeliveryByID(ctx, existing.DeliveryID); herr == nil && holder != nil {
			switch holder.Status {
			case "DELIVERED", "FAILED", "CANCELLED":
				if _, derr := coll.DeleteOne(ctx, bson.D{
					{Key: "code", Value: b.Code}, {Key: "delivery_id", Value: existing.DeliveryID},
				}); derr == nil {
					if _, ierr := coll.InsertOne(ctx, b); ierr == nil {
						return true, nil
					}
				}
			}
		}
		return false, nil
	}
	return true, nil
}

// ── 2.3 Door reference photo ────────────────────────────────────────────────

type riderDoorPhotoRequest struct {
	PhotoRef string `json:"photo_ref"`
}

// riderTaskDoorPhoto stores the door reference photo AGAINST THE ADDRESS, not
// the task. POST /consumer/delivery/tasks/{deliveryId}/door-photo.
//
// The picture answers "which door is B-204?" for whoever delivers there NEXT,
// so keying it to a task that ends today would throw away the only thing that
// makes it worth taking. One row per address (upsert): the newest photo of a
// door replaces the older one, because doors get repainted and gates get moved.
func (h *handler) riderTaskDoorPhoto(w http.ResponseWriter, r *http.Request) {
	d, err := h.riderTaskForCaller(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// A door photo can only be taken AT the door: refuse it from a task the
	// rider has not yet picked up.
	switch d.Status {
	case "OUT_FOR_DELIVERY", "DELIVERED", "FAILED":
	default:
		httpx.Error(w, r, httpx.Conflict("NOT_OUT", "this order is not out for delivery"))
		return
	}
	var req riderDoorPhotoRequest
	if derr := httpx.DecodeJSON(r, &req); derr != nil {
		httpx.Error(w, r, derr)
		return
	}
	ref, ok := riderDoorPhotoRef(req.PhotoRef)
	if !ok {
		httpx.Error(w, r, httpx.BadRequest("INVALID_PHOTO_REF",
			"photo_ref must be the view URL returned by the upload presign"))
		return
	}
	key := riderAddressKey(d.ConsumerID, d.AddressLabel, d.AddressLine)
	if key == "" {
		httpx.Error(w, r, httpx.Unprocessable("NO_ADDRESS", "this order has no address to attach the photo to"))
		return
	}
	if serr := h.svc.repo.riderSaveDoorPhoto(r.Context(), key, ref, d); serr != nil {
		httpx.Error(w, r, serr)
		return
	}
	httpx.JSON(w, http.StatusOK, savedResponse{Saved: true})
}

// riderAddressKey is the stable identity of a doorstep. Orders carry no address
// id (only the label + line copied onto the task), so the key is the customer
// plus their normalised address text: same customer + same address text = same
// door. Hashed so the key is fixed-width and carries no address in the clear;
// the readable label and line are stored alongside it for a human to check.
//
// A re-typed address ("B 204" → "B-204") starts a new key and loses the old
// photo. That is the honest trade: a wrong photo of the wrong door is worse
// than none, and the next delivery re-captures it.
func riderAddressKey(consumerID, label, line string) string {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	consumerID = strings.TrimSpace(consumerID)
	l, ln := norm(label), norm(line)
	if consumerID == "" || (l == "" && ln == "") {
		return ""
	}
	sum := sha256.Sum256([]byte(consumerID + "\n" + l + "\n" + ln))
	return "addr_" + hex.EncodeToString(sum[:16])
}

// riderDoorPhotoRef validates the stored media reference. Door photos are
// rendered by ANOTHER rider's app later, so only the shapes our own presign
// hands back are accepted — never a javascript:/data: URI smuggled through.
func riderDoorPhotoRef(raw string) (string, bool) {
	ref := strings.TrimSpace(raw)
	if ref == "" || len(ref) > riderMaxPhotoRefLen {
		return "", false
	}
	if strings.ContainsAny(ref, " \t\r\n\"'<>") {
		return "", false
	}
	if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "https://") {
		return ref, true
	}
	return "", false
}

// riderSaveDoorPhoto upserts the one door photo row for an address.
func (r *repository) riderSaveDoorPhoto(ctx context.Context, addressKey, photoRef string, d *delivery) error {
	now := time.Now().UTC()
	_, err := r.riderColl(collDoorReferencePic).UpdateOne(ctx,
		bson.D{{Key: "address_key", Value: addressKey}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "consumer_id", Value: d.ConsumerID},
				{Key: "address_label", Value: d.AddressLabel},
				{Key: "address_line", Value: d.AddressLine},
				{Key: "photo_ref", Value: photoRef},
				{Key: "geo", Value: d.Geo},
				// Provenance: who took it, on which run. A door photo that turns
				// out to be of the wrong flat has to be traceable to a rider.
				{Key: "captured_by", Value: d.RiderPartyID},
				{Key: "captured_on_delivery", Value: d.ID},
				{Key: "updated_at", Value: now},
			}},
			{Key: "$setOnInsert", Value: bson.D{
				{Key: "address_key", Value: addressKey},
				{Key: "created_at", Value: now},
			}},
		},
		options.Update().SetUpsert(true))
	if err != nil {
		return httpx.Internal(fmt.Errorf("save door photo: %w", err))
	}
	return nil
}

// ── 2.4 Undo a completed delivery ───────────────────────────────────────────

type riderUndoRequest struct {
	Reason string `json:"reason"`
}

// riderTaskUndo reverses a DELIVERED/FAILED marking inside a 15-minute window.
// POST /consumer/delivery/tasks/{deliveryId}/undo.
//
// This one moves money, so the order of operations is deliberate:
//
//  1. validate ownership, state and the window;
//  2. REVERSE THE WALLET DEBIT FIRST, keyed to the delivered marking so the
//     whole undo can be retried safely;
//  3. only then flip the task, with a conditional update that a second tap
//     cannot match.
//
// Money before status is the safer order here. Every money step is idempotent
// by a unique ledger ref, so a retry (or a racing double-tap) reverses at most
// once; while the reverse order would leave a crash between the two steps
// looking like a delivery that never happened but was still paid for.
func (h *handler) riderTaskUndo(w http.ResponseWriter, r *http.Request) {
	d, err := h.riderTaskForCaller(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderUndoRequest
	if derr := httpx.DecodeJSON(r, &req); derr != nil {
		httpx.Error(w, r, derr)
		return
	}
	// Reject an over-long reason rather than clipping it: a truncated
	// explanation is evidence the rider believes they filed and we did not keep.
	reason, terr := riderText(req.Reason, "reason", riderMaxReasonLen)
	if terr != nil {
		httpx.Error(w, r, terr)
		return
	}
	if reason == "" {
		// The client already insists on one. Undoing a completed delivery
		// without a stated reason leaves nothing for the centre to reconcile.
		httpx.Error(w, r, httpx.BadRequest("REASON_REQUIRED", "tell us why this delivery is being undone"))
		return
	}
	if d.Status != "DELIVERED" && d.Status != "FAILED" {
		httpx.Error(w, r, httpx.Conflict("NOT_COMPLETED", "only a completed order can be undone"))
		return
	}
	markedAt, ok := riderCompletionTime(d)
	if !ok || time.Since(markedAt) > riderUndoWindow {
		// UNDO_WINDOW_CLOSED is the exact code the client switches on. An
		// unknown completion time counts as closed: we cannot prove the marking
		// is fresh, and reversing money on a guess is not an option.
		httpx.Error(w, r, httpx.Conflict("UNDO_WINDOW_CLOSED",
			"this delivery can no longer be undone here — raise a concern with the centre"))
		return
	}
	ctx := r.Context()
	prev := d.Status
	// Only a DELIVERED task ever moved money (failDelivery debits nothing).
	if prev == "DELIVERED" {
		if rerr := h.svc.riderReverseDeliveryDebit(ctx, d, markedAt); rerr != nil {
			httpx.Error(w, r, rerr)
			return
		}
	}
	moved, uerr := h.svc.repo.riderUndoCompletion(ctx, d, prev, reason)
	if uerr != nil {
		httpx.Error(w, r, uerr)
		return
	}
	if moved {
		// Release this task's crate labels. The unique index on `code` is a
		// permanent claim, so a binding left behind on an undone delivery would
		// make that physical label unscannable forever — including for the
		// rider who is reassigned the very same crate the next morning.
		if _, rerr := h.svc.repo.riderReleaseScanCodes(ctx, d.ID); rerr != nil {
			h.svc.log.ErrorContext(ctx, "scan bindings not released on delivery undo",
				slog.String("delivery_id", d.ID), slog.Any("err", rerr))
		}
	}
	if !moved {
		// Someone (a double tap, a second device) already undid this marking.
		// The money reversal above is idempotent, so nothing was reversed twice.
		httpx.Error(w, r, httpx.Conflict("ALREADY_UNDONE", "this delivery was already undone"))
		return
	}
	if prev == "DELIVERED" {
		// Put the customer's order back where the task now is, so their app does
		// not keep showing a delivered order with a live rider on it.
		h.svc.riderRestoreOrderOutForDelivery(ctx, d)

		// A COD task that was marked delivered also raised a CASH DUE, and the
		// rider may already have collected against it. Undoing the delivery
		// without reversing that due left the money recorded as taken while the
		// task went back out — and because the due's id is derived from the
		// delivery id, redelivering raised no new due, so the customer who
		// actually receives the order is never asked to pay. Reverse it here,
		// with the same guarded update the manual undo uses: it only ever
		// matches a COLLECTED, un-remitted due for THIS rider, so a due that was
		// never collected, or already banked at the centre, is left alone.
		if strings.EqualFold(d.PaymentMode, "COD") {
			cashID := riderCashIDFor(d.ID)
			// A due that was raised but never collected is WITHDRAWN, not
			// reversed: the cash id is derived from the delivery id, so exactly
			// one due can ever exist for this task. Leaving a stale PENDING one
			// attached to the rider who undid the delivery would both show them
			// money they are not holding and stop the sweep raising a due for
			// whoever actually delivers it after reassignment.
			if _, derr := h.svc.repo.riderWithdrawPendingCash(ctx, d.RiderPartyID, cashID); derr != nil {
				h.svc.log.ErrorContext(ctx, "pending cash due not withdrawn on delivery undo",
					slog.String("delivery_id", d.ID), slog.String("cash_id", cashID),
					slog.Any("err", derr))
			}
			if _, cerr := h.svc.repo.riderReverseCash(
				ctx, d.RiderPartyID, cashID,
				"delivery undone: "+reason,
				time.Now().UTC(),
				// No freshness cutoff of its own: the undo window above already
				// bounded how old this marking can be, and a due raised by that
				// marking cannot predate it.
				time.Time{},
			); cerr != nil {
				// The delivery is already reversed and the customer's debit is
				// already back. Failing the whole request now would tell the
				// rider nothing was undone, which is worse than a due the centre
				// reconciles by hand — so log it and report the undo.
				h.svc.log.ErrorContext(ctx, "cash due not reversed on delivery undo",
					slog.String("delivery_id", d.ID), slog.String("cash_id", cashID),
					slog.Any("err", cerr))
			}
		}
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "OUT_FOR_DELIVERY"})
}

// riderCompletionTime is the moment the task was marked complete. DELIVERED
// carries an explicit delivered_at; FAILED has no failed-at field of its own,
// so its updated_at (stamped by that very transition) is the honest answer.
func riderCompletionTime(d *delivery) (time.Time, bool) {
	if d.Status == "DELIVERED" && strings.TrimSpace(d.DeliveredAt) != "" {
		if t, err := time.Parse(time.RFC3339, d.DeliveredAt); err == nil {
			return t.UTC(), true
		}
	}
	if !d.UpdatedAt.IsZero() {
		return d.UpdatedAt.UTC(), true
	}
	return time.Time{}, false
}

// riderUndoCompletion flips a completed task back to OUT_FOR_DELIVERY.
// Conditional on the exact state we validated (owner + status + the completion
// stamp), so a second tap matches nothing and is reported as a conflict instead
// of undoing an undo. Returns false when nothing matched.
func (r *repository) riderUndoCompletion(ctx context.Context, d *delivery, prev, reason string) (bool, error) {
	now := time.Now().UTC()
	filter := bson.D{
		{Key: "delivery_id", Value: d.ID},
		{Key: "rider_party_id", Value: d.RiderPartyID},
		{Key: "status", Value: prev},
	}
	if prev == "DELIVERED" {
		// Pin the exact marking being undone: if it was re-delivered in between,
		// delivered_at differs and this update correctly matches nothing.
		filter = append(filter, bson.E{Key: "delivered_at", Value: d.DeliveredAt})
	}
	update := bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "status", Value: "OUT_FOR_DELIVERY"},
			{Key: "undone_at", Value: now},
			{Key: "undo_reason", Value: reason},
			{Key: "undone_by", Value: d.RiderPartyID},
			{Key: "updated_at", Value: now},
		}},
		// The old proof must go: it belongs to a delivery that, as far as the
		// record is now concerned, did not happen. Keeping a stale photo would
		// let the next deliver call inherit someone else's evidence.
		{Key: "$unset", Value: bson.D{
			{Key: "delivered_at", Value: ""},
			{Key: "proof_note", Value: ""},
			{Key: "proof_photo_uri", Value: ""},
			{Key: "proof_geo", Value: ""},
			{Key: "delivery_event_id", Value: ""},
			{Key: "geofence_ok", Value: ""},
			{Key: "failure_reason", Value: ""},
		}},
	}
	res, err := r.deliveries.UpdateOne(ctx, filter, update)
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("undo delivery: %w", err))
	}
	return res.MatchedCount == 1, nil
}

// riderReverseDeliveryDebit gives back the wallet debit deliverDelivery took.
//
// The delivery path debits EXACTLY ONCE against ref "delivery:<orderID>" (a
// unique (consumer, ref, type) ledger row is the gate). Reversing therefore has
// to do TWO things, and doing only the first is the classic bug:
//
//  1. credit the money back;
//  2. RELEASE the debit ref, so a re-delivery of the same order charges again.
//
// Leaving the gate row in place would make the re-delivery a silent no-op and
// hand the customer their order for free.
//
// Both halves are idempotent. Step (1) re-keys the original DEBIT row to a
// spent-ref and marks it REVERSED — a single atomic FindOneAndUpdate, so only
// one caller can ever claim it, and it is preserved (never deleted) for audit.
// Step (2) inserts the credit under a ref derived from the delivered marking,
// so a retried undo dedupes into a no-op.
func (s *service) riderReverseDeliveryDebit(ctx context.Context, d *delivery, markedAt time.Time) error {
	// COD collects at the door; nothing was ever taken from the wallet.
	if d.PaymentMode != "PREPAID" {
		return nil
	}
	consumerID, cerr := primitive.ObjectIDFromHex(d.ConsumerID)
	if cerr != nil {
		return httpx.Internal(fmt.Errorf("undo: bad consumer id %q on delivery %s", d.ConsumerID, d.ID))
	}
	debitRef := "delivery:" + d.OrderID
	// Deterministic per delivered marking, so retrying the SAME undo credits at
	// most once while a later, genuinely new delivery+undo gets its own ref.
	undoRef := debitRef + ":undo:" + rfc3339(markedAt)
	now := time.Now().UTC()

	after := options.After
	var row walletTxn
	err := s.repo.walletTxns.FindOneAndUpdate(ctx,
		bson.D{
			{Key: "consumer_id", Value: consumerID},
			{Key: "ref_id", Value: debitRef},
			{Key: "type", Value: "DEBIT"},
			{Key: "status", Value: "SUCCESS"},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: "REVERSED"},
			{Key: "ref_id", Value: undoRef + ":spent"}, // frees the delivery ref
			{Key: "reversal_ref", Value: undoRef},
			{Key: "reversed_at", Value: now},
		}}},
		options.FindOneAndUpdate().SetReturnDocument(after),
	).Decode(&row)
	if err != nil {
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return httpx.Internal(fmt.Errorf("claim delivery debit for reversal: %w", err))
		}
		// Nothing live to claim. Either this order was never debited (a ₹0
		// promotional delivery whose gate row we still want released, or a task
		// delivered before this code existed), or a previous attempt at THIS
		// undo already claimed it and died before crediting. Only the second
		// case owes the customer money, and it is identifiable by our ref.
		e := s.repo.walletTxns.FindOne(ctx, bson.D{
			{Key: "consumer_id", Value: consumerID},
			{Key: "reversal_ref", Value: undoRef},
			{Key: "type", Value: "DEBIT"},
		}).Decode(&row)
		if errors.Is(e, mongo.ErrNoDocuments) {
			return nil // no debit to reverse — undo proceeds, no money moves
		}
		if e != nil {
			return httpx.Internal(fmt.Errorf("read reversed delivery debit: %w", e))
		}
	}
	amount := round2(row.Amount)
	if amount <= 0 {
		// The ₹0 gate row of a free trial / Welcome Litre day. Releasing its ref
		// (above) is the entire reversal — there is no money to hand back.
		return nil
	}
	// Which bucket to credit. debitWalletAtomic spends promo (REWARDS) first but
	// records only the bucket that COVERED the charge, so a part-promo part-cash
	// debit cannot be split back exactly. We return what the ledger row itself
	// claims, and default to CASH — the ambiguous case then favours the customer
	// (they get spendable cash back, never less than they paid) for at most one
	// order's value inside a 15-minute window.
	cashDelta, rewardsDelta := amount, 0.0
	bucket := "CASH"
	if row.Bucket == "REWARDS" {
		cashDelta, rewardsDelta, bucket = 0, amount, "REWARDS"
	}
	gate := walletTxn{
		ID: primitive.NewObjectID(), ConsumerID: consumerID,
		Type: "REFUND", Bucket: bucket, Amount: amount,
		RefType: "delivery_undo", RefID: undoRef, Status: "SUCCESS",
		Remark: "Delivery " + d.OrderCode + " undone by the rider", CreatedAt: now,
	}
	dup, gerr := s.repo.insertWalletTxnGate(ctx, gate)
	if gerr != nil {
		return toHTTPErr(gerr)
	}
	if dup {
		return nil // this undo already credited — idempotent replay
	}
	updated, ierr := s.repo.incWallet(ctx, consumerID, cashDelta, rewardsDelta, 0, 1)
	if ierr != nil {
		// No money moved: drop the gate row on a detached context (mirroring
		// debit's rollback) so a retry of this undo can still credit.
		rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.repo.deleteWalletTxn(rbCtx, gate.ID)
		cancel()
		return toHTTPErr(ierr)
	}
	s.repo.updateWalletTxnBalances(ctx, gate.ID, updated.ID, updated.Seq, updated.CashBalance, updated.RewardsBalance)
	s.log.InfoContext(ctx, "delivery debit reversed by rider undo",
		"delivery", d.ID, "order", d.OrderID, "amount", amount, "bucket", bucket)
	return nil
}

// riderRestoreOrderOutForDelivery walks the consumer's order back from
// delivered. Guarded on "delivered" so a cancelled or already-moved order is
// left alone, and best-effort like the rest of the order sync in delivery_svc.go
// — the task is the system of record for the rider.
//
// This is not cosmetic. The consumer app runs a settle sweep that debits any
// DELIVERED order whose charge it cannot see (orders.go), and the reversal above
// deliberately released that charge's ref. An order left showing "delivered"
// would therefore be re-charged by the customer's own phone for a delivery that
// has just been undone.
//
// Note what canNOT be walked back: the order.delivered CRM event has already
// been emitted, and a 2+2 trial day is already recorded. The trial charge is
// idempotent per (customer, IST day), so re-delivering the same day re-uses the
// same decision and never burns a second free day.
func (s *service) riderRestoreOrderOutForDelivery(ctx context.Context, d *delivery) {
	// LOAD-BEARING, so a failure is retried once and then logged at ERROR: an
	// order left "delivered" while its task is undone is exactly what the
	// consumer app's settle sweep bills — a silent miss here charges the
	// customer for milk the rider took back.
	filter := bson.D{{Key: "order_id", Value: d.OrderID}, {Key: "status", Value: "delivered"}}
	update := bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "status", Value: "out_for_delivery"},
			{Key: "can_review", Value: false},
			{Key: "updated_at", Value: time.Now().UTC()},
		}},
		{Key: "$unset", Value: bson.D{{Key: "proof_photo_url", Value: ""}}},
	}
	if _, err := s.repo.orders.UpdateOne(ctx, filter, update); err != nil {
		if _, err2 := s.repo.orders.UpdateOne(ctx, filter, update); err2 != nil {
			s.log.ErrorContext(ctx, "UNDO ORDER RESTORE FAILED — order still shows delivered; the settle sweep may bill it (fix by hand)",
				slog.String("order_id", d.OrderID), slog.Any("err", err2))
		}
	}
}

// ── 2.5 Per-order compliance steps ──────────────────────────────────────────

// riderComplianceStep is one door step. `code` drives client behaviour (SCAN
// opens the scanner, CALL dials, COLLECT shows the cash amount, PROOF is the
// screen's CTA) so the codes are a closed set shared with the client; `voice`
// is the Hindi line spoken at the door.
type riderComplianceStep struct {
	Code      string `json:"code"`
	Title     string `json:"title"`
	Subtitle  string `json:"subtitle,omitempty"`
	Voice     string `json:"voice,omitempty"`
	Mandatory bool   `json:"mandatory"`
}

// riderTaskCompliance returns the ordered door steps for THIS task.
// GET /consumer/delivery/tasks/{deliveryId}/compliance.
//
// The client ships kDefaultComplianceSteps as its fallback, so this list is
// deliberately the same set in the same order — server and client must never
// disagree about what a rider is asked to do at a door. What the server adds is
// truth about the ACTUAL order: the CALL step only when the customer asked to
// be called, the COLLECT step only on a cash order (and with the real amount in
// it), the handover line the customer actually chose.
func (h *handler) riderTaskCompliance(w http.ResponseWriter, r *http.Request) {
	d, err := h.riderTaskForCaller(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, riderComplianceStepsFor(d))
}

// riderComplianceStepsFor derives the checklist from the task itself. Nothing
// here is invented: every step that is conditional is conditioned on a field
// stored on the delivery.
func riderComplianceStepsFor(d *delivery) []riderComplianceStep {
	steps := []riderComplianceStep{{
		Code:      "REACH",
		Title:     "Reach the customer address",
		Subtitle:  "Park safely and carry the cold box to the door",
		Voice:     "ग्राहक के पते पर पहुँचें।",
		Mandatory: true,
	}}

	// CALL — only for a customer who asked to be called first. When they did
	// ask, it is not optional: it is the instruction they left on the order.
	if d.DeliveryPrefs != nil && d.DeliveryPrefs.CallBefore {
		steps = append(steps, riderComplianceStep{
			Code:      "CALL",
			Title:     "Call the customer",
			Subtitle:  "This customer asked to be called before delivery",
			Voice:     "ग्राहक को कॉल करें।",
			Mandatory: true,
		})
	}

	// SCAN — nothing to scan on a task with no lines.
	if len(d.Items) > 0 {
		steps = append(steps, riderComplianceStep{
			Code:      "SCAN",
			Title:     "Scan the products",
			Subtitle:  "Scan every box/bag code assigned to this order",
			Voice:     "सामान स्कैन करें।",
			Mandatory: true,
		})
	}

	steps = append(steps, riderComplianceStep{
		Code:      "HANDOVER",
		Title:     "Hand over the order",
		Subtitle:  riderHandoverSubtitle(d.DeliveryPrefs),
		Voice:     "ग्राहक को सामान दें।",
		Mandatory: true,
	})

	// COLLECT — a prepaid order has nothing to collect, and a rider asked to
	// collect on one would be taking money twice. The amount is the task's own
	// (the store may have re-billed it after an adjustment), so what the rider
	// asks for is always what the customer owes.
	if d.PaymentMode == "COD" && d.Amount > 0 {
		amt := strconv.FormatFloat(round2(d.Amount), 'f', -1, 64)
		steps = append(steps, riderComplianceStep{
			Code:      "COLLECT",
			Title:     "Collect the payment",
			Subtitle:  "Cash order — collect exactly ₹" + amt,
			Voice:     "₹" + amt + " लेना ना भूलें।",
			Mandatory: true,
		})
	}

	steps = append(steps, riderComplianceStep{
		Code:      "PROOF",
		Title:     "Capture proof of delivery",
		Subtitle:  "Photo at the door + geotag",
		Voice:     "डिलीवरी की फोटो लें।",
		Mandatory: true,
	})
	return steps
}

// riderHandoverSubtitle turns the customer's stored handover preference into
// the line the rider reads. Falls back to the generic instruction when the
// order carries no preference — never to a guess about what they wanted.
func riderHandoverSubtitle(p *deliveryPrefsDoc) string {
	if p != nil {
		// The bell is a separate instruction from the handover mode, so it is
		// CARRIED ALONG rather than dropped: the task card paints an explicit
		// "do not ring" in red, and the step text used to answer "hand the order
		// to the customer in person" on the very same task — two screens telling
		// the rider opposite things about the same door.
		bell := ""
		if p.RingBell != nil {
			if *p.RingBell {
				bell = "ring the bell"
			} else {
				bell = "do NOT ring the bell"
			}
		}
		with := func(line string) string {
			if bell == "" {
				return line
			}
			return line + " — " + bell
		}
		if note := strings.TrimSpace(p.Note); note != "" {
			return with(note) // the customer's own words win over any template
		}
		switch p.Handover {
		case "HAND_TO_CUSTOMER":
			return with("Hand the order to the customer in person")
		case "DROP":
			return with("Leave the order safely at the door")
		case "RING_BELL":
			return "Ring the bell and wait for the customer"
		}
		if p.RingBell != nil {
			if *p.RingBell {
				return "Ring the bell and wait for the customer"
			}
			return "Do NOT ring the bell"
		}
	}
	return "Follow the customer's handover preference"
}
