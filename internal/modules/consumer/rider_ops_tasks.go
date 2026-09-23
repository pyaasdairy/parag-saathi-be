package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
// Per-task extras - RIDER_API.md section 2.4.
//
// Everything here hangs off ONE delivery the rider is currently holding: the
// time-boxed undo of a completed marking. The customer OTP, crate scan, door
// photo and compliance checklist routes (sections 2.1-2.3, 2.5) were removed
// on 23 Sep 2026: the rider flow is photo + geotag by the founder's decision,
// and no shipped Saathi screen ever called them.
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
	// riderOTPResendCooldown throttles OTP resends. The customer door OTP is
	// gone; the cash-collection OTP (rider_ops_money.go) still uses it so a
	// "resend" tap cannot SMS-bomb the customer (and bill us per message).
	riderOTPResendCooldown = 60 * time.Second

	// riderUndoWindow is how long after a DELIVERED/FAILED marking the rider may
	// still reverse it. Short on purpose: past this the money has settled in the
	// customer's mind (and in their ledger) and the correction belongs to
	// support, not to the person holding the phone at the door.
	riderUndoWindow = 15 * time.Minute
)

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

// -- Shared OTP plumbing: the cash-collection flow (rider_ops_money.go) uses these --

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
	if !moved {
		// Someone (a double tap, a second device) already undid this marking.
		// The money reversal above is idempotent, so nothing was reversed twice.
		httpx.Error(w, r, httpx.Conflict("ALREADY_UNDONE", "this delivery was already undone"))
		return
	}
	if prev == "FAILED" {
		// The failed marking cancelled the customer's order; walk that back too,
		// or the redelivery is refused as ORDER_CANCELLED at the door.
		h.svc.syncOrderFailedUndone(ctx, d)
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

// -- Handover wording (kept: TestBellInstructionRules pins the bell rule) --

// riderHandoverSubtitle turns the customer's stored handover preference into
// the line the rider reads. Falls back to the generic instruction when the
// order carries no preference — never to a guess about what they wanted.
func riderHandoverSubtitle(p *deliveryPrefsDoc) string {
	if p != nil {
		// The bell rides ALONG with the handover mode instead of being dropped —
		// the card paints an explicit "do not ring" and the step text used to
		// answer "hand the order to the customer in person" on the same task,
		// two screens telling the rider opposite things about one door.
		//
		// One exception: a "do not ring" next to an explicit HAND_TO_CUSTOMER is
		// what the consumer app produces from an UNTOUCHED bell switch (cart.tsx
		// derives the mode from that same default), so repeating it there would
		// print a doorstep instruction on nearly every order that nobody gave.
		// A bell the customer really did switch off comes with DROP or with no
		// handover mode at all, and still reaches the rider.
		bell := ""
		if p.RingBell != nil {
			if *p.RingBell {
				bell = "ring the bell"
			} else if p.Handover != "HAND_TO_CUSTOMER" {
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
