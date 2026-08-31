package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// ─────────────────────────────────────────────────────────────────────────────
// Rider MONEY — cash collection, the monthly earnings statement, and penalties.
// RIDER_API.md §3.5, §3.7, §3.8.
//
// This is the only rider surface where a bug costs somebody actual rupees, so
// three rules run through every handler below:
//
//  1. NOTHING IS INVENTED. A cash due exists only because a real COD delivery
//     was really marked DELIVERED by this rider. An earnings statement is only
//     a payout when finance has published one; otherwise the rider is told the
//     truth — days worked, deliveries done, net ₹0, status PENDING — rather
//     than shown a plausible number they will later be argued out of.
//  2. EVERY MUTATION IS CONDITIONAL. Collect, undo, dispute and waive are all
//     one guarded UpdateOne whose filter contains the status the caller thinks
//     the record is in. A double tap (or two phones on one account) matches
//     zero documents on the second write and gets a 409 — never a second
//     settlement. Read-then-write would race here.
//  3. THE RIDER IS ALWAYS THE TOKEN'S RIDER. Every filter carries
//     rider_party_id from riderActor; the {cashId}/{penaltyId} in the path is
//     only ever an additional key, never the authorisation. A record belonging
//     to someone else is 404, not 403 — a 403 would confirm it exists.
// ─────────────────────────────────────────────────────────────────────────────

// riderCashModes are the only ways a due can be settled at the door. Closed set:
// a mode we do not recognise is a client bug or a probe, and silently storing it
// would put an unreconcilable row in front of the centre's cashier.
var riderCashModes = map[string]bool{"CASH": true, "UPI": true, "CHEQUE": true}

// riderCashUndoWindow is how long after a collection the rider may reverse it.
// Matches the delivery-undo window in RIDER_API.md §2.4: long enough to fix
// "wrong customer", short enough that the money is still in the rider's hand
// and nothing downstream has counted it.
const riderCashUndoWindow = 15 * time.Minute

// riderCashLookbackDays bounds how far back the cash sweep looks for delivered
// COD tasks with no due raised yet. A due older than this is a reconciliation
// problem for the centre, not something a rider collects on their phone today.
const riderCashLookbackDays = 45

// riderCashScanLimit caps the delivered-COD scan. A rider does tens of drops a
// day, not thousands; the cap keeps a bad index or a data anomaly from turning
// one screen open into an unbounded read.
const riderCashScanLimit = 500

// riderPenaltyListLimit caps the penalty history the console renders.
const riderPenaltyListLimit = 100

// riderPenaltyStreakScanLimit bounds the read that feeds the clean-day walk.
// It is deliberately larger than the display limit: the walk needs every
// penalised DAY inside riderWaiverStreakLookbackDays, and a day can carry
// several penalties, so sizing this to the page limit could hide recent ones.
const riderPenaltyStreakScanLimit = 1000

// riderWaiverStreakLookbackDays bounds the clean-day streak walk. Beyond this we
// stop counting and report what we actually verified rather than guessing.
const riderWaiverStreakLookbackDays = 90

// Free-text bounds. Reject rather than truncate: a silently clipped dispute note
// is evidence the rider believes they filed and we did not keep.
const (
	riderMaxIDLen       = 64
	riderMaxReasonLen   = 300
	riderMaxNoteLen     = 1000
	riderMaxPhotoRefLen = 512
)

// riderConcernReasons are the exact dispute codes the client sends
// (RIDER_API.md §3.8), mapped to the stable English label stored on the concern.
// Display text goes through tr() on the device, so English here is correct.
var riderConcernReasons = map[string]string{
	"GPS_WRONG":      "GPS location was wrong",
	"CUSTOMER_ISSUE": "Customer-side issue",
	"TRAFFIC":        "Traffic or road conditions",
	"APP_ISSUE":      "App or device issue",
	"OTHER":          "Other",
}

// ── stored shapes ───────────────────────────────────────────────────────────

// riderCashDoc is one cash due (collRiderCash). It is BACKED BY A DELIVERY —
// there is no such thing as a free-standing due — and its id is DERIVED from
// that delivery id (riderCashIDFor), so the unique index on cash_id is what
// guarantees exactly one due per delivery even if two requests sweep at once.
type riderCashDoc struct {
	CashID       string  `bson:"cash_id"`
	RiderPartyID string  `bson:"rider_party_id"`
	DeliveryID   string  `bson:"delivery_id"`
	OrderCode    string  `bson:"order_code,omitempty"`
	CustomerName string  `bson:"customer_name"`
	AddressLine  string  `bson:"address_line"`
	Phone        string  `bson:"phone,omitempty"`
	Amount       float64 `bson:"amount"` // what the customer owes (the delivery's bill)
	// CollectedAmount is what the rider actually took. Kept SEPARATE from
	// Amount: a ₹640 due settled at ₹600 must stay visibly short, not quietly
	// become a ₹600 due.
	CollectedAmount float64    `bson:"collected_amount,omitempty"`
	Status          string     `bson:"status"` // PENDING | COLLECTED
	Mode            string     `bson:"mode,omitempty"`
	Day             string     `bson:"day"` // IST business day of the delivery
	RaisedAt        time.Time  `bson:"raised_at"`
	CollectedAt     *time.Time `bson:"collected_at,omitempty"`
	UndoneAt        *time.Time `bson:"undone_at,omitempty"`
	UndoReason      string     `bson:"undo_reason,omitempty"`
	UndoCount       int        `bson:"undo_count,omitempty"`
	// RemittedAt is stamped when the centre takes the money off the rider.
	// Once set, an undo is a cash-book correction and no longer the rider's to
	// make from the app.
	RemittedAt *time.Time `bson:"remitted_at,omitempty"`
	// HighPriority is an OPS override ("collect from this customer first").
	// It is OR-ed with the derived aged-due flag at read time, never trusted
	// as the only source.
	HighPriority bool      `bson:"high_priority,omitempty"`
	CreatedAt    time.Time `bson:"created_at"`
	UpdatedAt    time.Time `bson:"updated_at"`
}

// riderEarningsDoc is a PUBLISHED payout statement (collRiderEarnings). Its
// absence is meaningful: no document means finance has not run the month, and
// the endpoint says exactly that instead of computing a number nobody has
// approved.
type riderEarningsDoc struct {
	RiderPartyID string                 `bson:"rider_party_id"`
	Month        string                 `bson:"month"` // "2006-01", IST
	PresentDays  int                    `bson:"present_days"`
	Deliveries   int                    `bson:"deliveries"`
	Net          float64                `bson:"net"`
	Status       string                 `bson:"status"` // PENDING | PROCESSING | PAID
	PaidOn       *time.Time             `bson:"paid_on,omitempty"`
	Lines        []riderEarningsLineDoc `bson:"lines"`
	PublishedAt  time.Time              `bson:"published_at,omitempty"`
}

// riderEarningsLineDoc is one row of the statement. Deductions are their own
// rows with Deduction=true and a POSITIVE amount — never netted into a credit —
// because a rider disputing ₹1,120 has to be able to point at the ₹1,120.
type riderEarningsLineDoc struct {
	Label     string  `bson:"label"`
	Amount    float64 `bson:"amount"`
	Hint      string  `bson:"hint,omitempty"`
	Deduction bool    `bson:"deduction,omitempty"`
}

// riderPenaltyDoc is one deduction raised against a rider (collRiderPenalties).
// Written by the ops/finance side; this file only reads it and applies the two
// rider-initiated transitions (dispute, self-service waiver).
type riderPenaltyDoc struct {
	PenaltyID      string     `bson:"penalty_id"`
	RiderPartyID   string     `bson:"rider_party_id"`
	Category       string     `bson:"category"`
	Reason         string     `bson:"reason"`
	Amount         float64    `bson:"amount"`
	Status         string     `bson:"status"` // APPLIED | UNDER_REVIEW | WAIVED | REVERSED
	WaiverEligible bool       `bson:"waiver_eligible,omitempty"`
	ConcernID      string     `bson:"concern_id,omitempty"`
	DeliveryID     string     `bson:"delivery_id,omitempty"`
	AppliedAt      time.Time  `bson:"applied_at"`
	WaivedAt       *time.Time `bson:"waived_at,omitempty"`
	UpdatedAt      time.Time  `bson:"updated_at,omitempty"`
}

// riderConcernDoc is the support ticket a penalty dispute opens
// (collRiderSupport — the same inbox GET /rider/support/concerns renders, so a
// dispute raised from the penalty screen is visible on the support screen too).
type riderConcernDoc struct {
	ConcernID    string    `bson:"concern_id"`
	RiderPartyID string    `bson:"rider_party_id"`
	Type         string    `bson:"type"` // "PENALTY"
	Title        string    `bson:"title"`
	Description  string    `bson:"description"`
	ReasonCode   string    `bson:"reason_code"`
	PhotoRef     string    `bson:"photo_ref,omitempty"`
	PenaltyID    string    `bson:"penalty_id,omitempty"`
	Status       string    `bson:"status"` // OPEN
	RaisedAt     time.Time `bson:"raised_at"`
	UpdatedAt    time.Time `bson:"updated_at"`
}

// ── wire shapes ─────────────────────────────────────────────────────────────

// riderCashWire is CashRequest in lib/models/rider.dart.
type riderCashWire struct {
	ID              string  `json:"id"`
	CustomerName    string  `json:"customer_name"`
	AddressLine     string  `json:"address_line"`
	Amount          float64 `json:"amount"`
	RequestedAmount float64 `json:"requested_amount"`
	Status          string  `json:"status"`
	Phone           string  `json:"phone,omitempty"`
	RaisedAt        string  `json:"raised_at,omitempty"`
	HighPriority    bool    `json:"high_priority"`
	Mode            string  `json:"mode,omitempty"`
	CollectedAt     string  `json:"collected_at,omitempty"`
}

// riderCashOTPResponse answers the "SMS the customer a code" call. `sent` is the
// TRUTH about delivery, not a courtesy: when no SMS transport is configured the
// challenge is still issued (and logged) but we do not claim the customer got
// it, because a rider who believes a code is coming will stand at a door
// waiting for one.
type riderCashOTPResponse struct {
	Sent    bool   `json:"sent"`
	Message string `json:"message,omitempty"`
}

// riderCashCollectRequest — snake_case, as the client writes it.
type riderCashCollectRequest struct {
	Amount float64 `json:"amount"`
	Mode   string  `json:"mode"`
	OTP    string  `json:"otp"`
}

type riderCashUndoRequest struct {
	Reason string `json:"reason"`
}

// riderEarningsWire is EarningsSummary in lib/models/rider.dart.
type riderEarningsWire struct {
	Month       string                  `json:"month"`
	PresentDays int                     `json:"present_days"`
	Deliveries  int                     `json:"deliveries"`
	Net         float64                 `json:"net"`
	Status      string                  `json:"status"`
	PaidOn      string                  `json:"paid_on,omitempty"`
	Lines       []riderEarningsLineWire `json:"lines"`
}

type riderEarningsLineWire struct {
	Label     string  `json:"label"`
	Amount    float64 `json:"amount"`
	Hint      string  `json:"hint,omitempty"`
	Deduction bool    `json:"deduction"`
}

// riderPenaltyWire is PenaltyItem in lib/models/rider.dart.
type riderPenaltyWire struct {
	ID             string  `json:"id"`
	Date           string  `json:"date"`
	Category       string  `json:"category"`
	Reason         string  `json:"reason"`
	Amount         float64 `json:"amount"`
	Status         string  `json:"status"`
	WaiverEligible bool    `json:"waiver_eligible"`
	WaiverStreak   int     `json:"waiver_streak"`
	ConcernID      string  `json:"concern_id,omitempty"`
}

type riderPenaltyConcernRequest struct {
	Reason   string `json:"reason"`
	Note     string `json:"note"`
	PhotoRef string `json:"photo_ref"`
}

type riderConcernResponse struct {
	ConcernID string `json:"concern_id"`
	Status    string `json:"status"`
}

// ── small helpers ───────────────────────────────────────────────────────────

// riderCashIDFor derives a due's id from the delivery it settles. Deterministic
// on purpose: the unique index on cash_id then makes "one due per delivery" a
// database guarantee rather than an application hope, so two concurrent sweeps
// cannot both raise a due for the same drop. The id is guessable from a delivery
// id, which is harmless — every mutation is filtered by rider_party_id anyway.
func riderCashIDFor(deliveryID string) string {
	return "cash_" + strings.TrimPrefix(deliveryID, "del_")
}

// riderPathID reads and bounds an opaque record id from the URL.
func riderPathID(r *http.Request, param string) (string, error) {
	v := strings.TrimSpace(chi.URLParam(r, param))
	if v == "" || len(v) > riderMaxIDLen {
		return "", httpx.BadRequest("INVALID_ID", param+" is missing or malformed")
	}
	return v, nil
}

// riderMoneySane rejects the values a float can carry that rupees cannot.
// Silently coercing NaN to 0 would settle a due for nothing.
func riderMoneySane(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < 1e7
}

// riderText trims free text and enforces a hard cap (reject, never truncate).
// The cap counts RUNES, not bytes: riders type Hindi, and a byte cap would cut
// a Devanagari note to a third of the length an English one gets.
func riderText(v, field string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if utf8.RuneCountInString(v) > max {
		return "", httpx.BadRequest("TEXT_TOO_LONG", fmt.Sprintf("%s must be %d characters or fewer", field, max))
	}
	return v, nil
}

// riderISTDay is the IST business day a moment belongs to.
func riderISTDay(t time.Time) string { return t.In(istZone).Format("2006-01-02") }

// ═════════════════════════════════════════════════════════════════════════════
// 3.5 CASH COLLECTION
// ═════════════════════════════════════════════════════════════════════════════

// riderCashList — GET /consumer/delivery/rider/cash.
//
// The list is DERIVED, not authored: every due traces to a COD delivery this
// rider marked DELIVERED. The sweep below raises a PENDING due for any such
// delivery that has none yet, which is why a rider never has to be "given" a
// cash task — doing the drop creates it.
//
// It returns everything still outstanding (however old) plus anything collected
// TODAY, so the console can show the "Collected ₹x" tally and keep the undo
// affordance reachable for the window in which undo is still legal.
func (h *handler) riderCashList(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()

	// Best-effort: a sweep failure must not blank a screen that can still show
	// the dues already raised. Log it and serve what we have.
	if err := h.svc.repo.riderRaiseCashForDeliveredCOD(ctx, actor.PartyID, now); err != nil {
		h.svc.log.ErrorContext(ctx, "rider cash sweep failed", slog.String("rider", actor.PartyID), slog.Any("err", err))
	}

	dayStart, _, derr := istDayBounds(riderISTDay(now))
	if derr != nil {
		httpx.Error(w, r, derr)
		return
	}
	docs, err := h.svc.repo.riderListCash(ctx, actor.PartyID, dayStart)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]riderCashWire, 0, len(docs))
	for i := range docs {
		out = append(out, riderCashToWire(&docs[i], dayStart))
	}
	// Pending first (that is the rider's job list), high priority above that,
	// then oldest-raised first — the due that has been open longest is the one
	// most likely to be forgotten. Collected rows fall to the bottom, newest
	// first, because they are a receipt, not a task.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ap, bp := a.Status == "PENDING", b.Status == "PENDING"
		if ap != bp {
			return ap
		}
		if ap {
			if a.HighPriority != b.HighPriority {
				return a.HighPriority
			}
			return a.RaisedAt < b.RaisedAt
		}
		return a.CollectedAt > b.CollectedAt
	})
	httpx.JSON(w, http.StatusOK, out)
}

// riderCashToWire renders a stored due. high_priority is RE-DERIVED here: a due
// raised before today's IST day start is money the rider is still carrying from
// a previous run, which is exactly the thing the centre wants collected first.
// An explicit ops flag ORs in on top.
func riderCashToWire(d *riderCashDoc, todayStart time.Time) riderCashWire {
	amount := round2(d.Amount)
	if d.Status == "COLLECTED" {
		amount = round2(d.CollectedAmount)
	}
	return riderCashWire{
		ID:              d.CashID,
		CustomerName:    d.CustomerName,
		AddressLine:     d.AddressLine,
		Amount:          amount,
		RequestedAmount: round2(d.Amount),
		Status:          d.Status,
		Phone:           d.Phone,
		RaisedAt:        rfc3339(d.RaisedAt),
		HighPriority:    d.HighPriority || (d.Status == "PENDING" && d.RaisedAt.Before(todayStart)),
		Mode:            d.Mode,
		CollectedAt:     rfc3339Ptr(d.CollectedAt),
	}
}

// riderCashOTP — POST /cash/{cashId}/otp.
//
// The code goes to the CUSTOMER, never into this response: the whole point of
// the OTP is that the rider cannot produce it themselves, which is what stops
// "collected" being marked on money never handed over.
func (h *handler) riderCashOTP(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	cashID, err := riderPathID(r, "cashId")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()

	// Scoped by rider: another rider's due is simply not found.
	rec, err := h.svc.repo.riderFindCash(ctx, actor.PartyID, cashID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if rec.Status != "PENDING" {
		httpx.Error(w, r, httpx.Conflict("ALREADY_COLLECTED", "this amount has already been collected"))
		return
	}
	if strings.TrimSpace(rec.Phone) == "" {
		httpx.Error(w, r, httpx.Unprocessable("NO_CUSTOMER_PHONE", "this customer has no phone number on the order — collect without the code and tell your centre"))
		return
	}

	// Resend throttle, mirroring the delivery OTP route. Without it a rider in a
	// dispute with a customer — or an app retrying on a flaky 2G link — could
	// fire a real SMS to that customer's number on every tap, at our cost and
	// on their handset.
	slot, cerr := h.svc.repo.riderClaimOTPSendSlot(ctx, "cash", cashID, riderOTPResendCooldown)
	if cerr != nil {
		httpx.Error(w, r, cerr)
		return
	}
	if !slot {
		httpx.Error(w, r, httpx.TooManyRequestsCode("OTP_COOLDOWN",
			"a code was just sent to the customer — wait a moment before sending another"))
		return
	}

	code, err := h.svc.issueRiderOTP(ctx, "cash", cashID, actor.PartyID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// An UNDELIVERABLE code must not be left standing. A live challenge is what
	// makes riderCashCollect demand an OTP, so minting one the customer can
	// never receive locks the money for the whole 10-minute TTL: the rider is at
	// the door holding cash they cannot record. Drop the challenge and say so,
	// exactly as the delivery OTP route does — the rider then collects without
	// a code rather than being stuck behind one.
	if !h.svc.sms.Enabled() {
		h.svc.riderDropOTPChallenge("cash", cashID)
		h.svc.log.ErrorContext(ctx, "cash otp requested with no sms transport",
			slog.String("rider", actor.PartyID), slog.String("cash_id", cashID))
		httpx.Error(w, r, httpx.Unprocessable("OTP_UNAVAILABLE",
			"we cannot text the customer a code right now — collect and tell your centre"))
		return
	}
	if err := h.svc.sms.SendOTP(ctx, rec.Phone, code); err != nil {
		// Same reasoning: a challenge whose SMS failed would gate the collection
		// on a code nobody has.
		h.svc.riderDropOTPChallenge("cash", cashID)
		h.svc.log.ErrorContext(ctx, "cash otp sms send failed",
			slog.String("rider", actor.PartyID), slog.String("cash_id", cashID), slog.Any("err", err))
		httpx.Error(w, r, httpx.Internal(fmt.Errorf("send cash otp: %w", err)))
		return
	}
	h.svc.log.InfoContext(ctx, "cash otp sent", slog.String("rider", actor.PartyID), slog.String("cash_id", cashID))
	httpx.JSON(w, http.StatusOK, riderCashOTPResponse{Sent: true})
}

// riderCashCollect — POST /cash/{cashId}/collect. The money-critical path.
//
// Ordering matters: validate, then verify the OTP (which BURNS the challenge),
// then settle with a filter that still contains status=PENDING. If the settle
// loses the race the OTP is already spent, which is the safe direction — a
// second attempt needs a fresh code from the customer.
func (h *handler) riderCashCollect(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	cashID, err := riderPathID(r, "cashId")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderCashCollectRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()

	rec, err := h.svc.repo.riderFindCash(ctx, actor.PartyID, cashID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if rec.Status != "PENDING" {
		httpx.Error(w, r, httpx.Conflict("ALREADY_COLLECTED", "this amount has already been collected"))
		return
	}

	if !riderMoneySane(req.Amount) || req.Amount <= 0 {
		httpx.Error(w, r, httpx.BadRequest("INVALID_AMOUNT", "enter the amount you actually collected"))
		return
	}
	amount := round2(req.Amount)
	// Over-collection is refused outright, not clamped. The client blocks it
	// too; the server has to as well, because a clamp would record a number the
	// customer never agreed to and the rider never took.
	if amount > round2(rec.Amount)+0.005 {
		httpx.Error(w, r, httpx.Unprocessable("AMOUNT_EXCEEDS_DUE",
			"you cannot collect more than the amount due"))
		return
	}
	mode := strings.ToUpper(strings.TrimSpace(req.Mode))
	if !riderCashModes[mode] {
		httpx.Error(w, r, httpx.BadRequest("INVALID_MODE", "payment type must be CASH, UPI or CHEQUE"))
		return
	}

	// An OTP is required exactly while a LIVE challenge exists for this due. A
	// challenge that has expired is not a lock — otherwise an undeliverable SMS
	// would strand the money permanently.
	live, err := h.svc.repo.riderCashOTPPending(ctx, cashID, time.Now().UTC())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if live {
		if strings.TrimSpace(req.OTP) == "" {
			httpx.Error(w, r, httpx.BadRequest("OTP_REQUIRED", "enter the code the customer received"))
			return
		}
		if err := h.svc.verifyRiderOTP(ctx, "cash", cashID, req.OTP); err != nil {
			httpx.Error(w, r, err) // OTP_INVALID — the code the client switches on
			return
		}
	}

	now := time.Now().UTC()
	matched, err := h.svc.repo.riderSettleCash(ctx, actor.PartyID, cashID, amount, mode, now)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if matched == 0 {
		// Someone (another tab, a double tap, a retried request) settled it
		// between our read and our write. Conflict, never a silent second write.
		httpx.Error(w, r, httpx.Conflict("ALREADY_COLLECTED", "this amount has already been collected"))
		return
	}
	// Money moved: log it as an audit trail line, including the short-collection
	// case, which is the one a centre will ask about.
	h.svc.log.InfoContext(ctx, "rider cash collected",
		slog.String("rider", actor.PartyID), slog.String("cash_id", cashID),
		slog.String("delivery_id", rec.DeliveryID), slog.String("mode", mode),
		slog.Float64("amount", amount), slog.Float64("due", round2(rec.Amount)))

	httpx.JSON(w, http.StatusOK, statusResponse{Status: "COLLECTED"})
}

// riderCashUndo — POST /cash/{cashId}/undo.
//
// A reversal is legitimate ("I marked the wrong customer") but it is also the
// obvious way to make collected money disappear, so it is fenced four ways: the
// collecting rider only, inside riderCashUndoWindow, not after the day's route
// was closed, and never once the centre has taken the cash in.
func (h *handler) riderCashUndo(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	cashID, err := riderPathID(r, "cashId")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderCashUndoRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	reason, err := riderText(req.Reason, "reason", riderMaxReasonLen)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// A reversal with no stated reason is unauditable, so it is not accepted.
	if reason == "" {
		httpx.Error(w, r, httpx.BadRequest("REASON_REQUIRED", "say why you are undoing this collection"))
		return
	}
	ctx := r.Context()

	rec, err := h.svc.repo.riderFindCash(ctx, actor.PartyID, cashID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if rec.Status != "COLLECTED" {
		httpx.Error(w, r, httpx.Conflict("NOT_COLLECTED", "this amount has not been collected"))
		return
	}
	if rec.RemittedAt != nil {
		httpx.Error(w, r, httpx.Conflict("ALREADY_REMITTED", "this money is already deposited at the centre — ask your centre to correct it"))
		return
	}
	now := time.Now().UTC()
	cutoff := now.Add(-riderCashUndoWindow)
	if rec.CollectedAt == nil || rec.CollectedAt.Before(cutoff) {
		httpx.Error(w, r, httpx.Conflict("UNDO_WINDOW_CLOSED", "it is too late to undo this collection — ask your centre"))
		return
	}
	// Closing the route is the rider's own declaration that the day's cash is
	// final; after it, a reversal is a cash-book correction, not an app action.
	if h.svc.repo.riderRouteDayClosed(ctx, actor.PartyID, rec.Day) {
		httpx.Error(w, r, httpx.Conflict("ROUTE_COMPLETED", "your route for that day is already closed — ask your centre"))
		return
	}

	matched, err := h.svc.repo.riderReverseCash(ctx, actor.PartyID, cashID, reason, now, cutoff)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if matched == 0 {
		httpx.Error(w, r, httpx.Conflict("UNDO_WINDOW_CLOSED", "it is too late to undo this collection — ask your centre"))
		return
	}
	h.svc.log.InfoContext(ctx, "rider cash collection reversed",
		slog.String("rider", actor.PartyID), slog.String("cash_id", cashID),
		slog.Float64("amount", round2(rec.CollectedAmount)), slog.String("reason", reason))

	httpx.JSON(w, http.StatusOK, statusResponse{Status: "PENDING"})
}

// ═════════════════════════════════════════════════════════════════════════════
// 3.7 EARNINGS
// ═════════════════════════════════════════════════════════════════════════════

// riderEarnings — GET /earnings?month=YYYY-MM (default: the current IST month).
//
// Two honest answers, never a third invented one:
//
//   - finance has PUBLISHED a statement → it is returned verbatim, deductions
//     kept as their own lines;
//   - nothing published → the month's real, countable facts (days present,
//     deliveries completed) with net ₹0 and status PENDING. The payout has not
//     been decided, and saying so is the only truthful thing to render.
func (h *handler) riderEarnings(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()

	month := strings.TrimSpace(r.URL.Query().Get("month"))
	if month == "" {
		month = istMonth(now)
	}
	from, to, err := istMonthBounds(month)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	if doc, err := h.svc.repo.riderFindEarnings(ctx, actor.PartyID, month); err != nil {
		httpx.Error(w, r, err)
		return
	} else if doc != nil {
		httpx.JSON(w, http.StatusOK, h.svc.riderPublishedEarnings(ctx, doc, month))
		return
	}

	// Not published. Count what actually happened — both numbers come from real
	// records, so the rider can check them against their own day.
	presentDays, err := h.svc.repo.riderCountPresentDays(ctx, actor.PartyID,
		from.In(istZone).Format("2006-01-02"), to.In(istZone).Format("2006-01-02"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	deliveries, err := h.svc.repo.riderCountDelivered(ctx, actor.PartyID, from, to)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, riderEarningsWire{
		Month:       month,
		PresentDays: presentDays,
		Deliveries:  deliveries,
		Net:         0,
		Status:      "PENDING",
		Lines:       []riderEarningsLineWire{}, // [] not null: an empty statement, not a broken one
	})
}

// riderPublishedEarnings renders a published statement. The stored net is
// authoritative (it is what finance approved and will pay), but a net that does
// not reconcile with its own lines is a data fault worth an operator's
// attention, so it is logged rather than silently smoothed over.
func (s *service) riderPublishedEarnings(ctx context.Context, doc *riderEarningsDoc, month string) riderEarningsWire {
	lines := make([]riderEarningsLineWire, 0, len(doc.Lines))
	var credits, deductions float64
	for _, l := range doc.Lines {
		amt := round2(l.Amount)
		if l.Deduction {
			deductions += amt
		} else {
			credits += amt
		}
		lines = append(lines, riderEarningsLineWire{
			Label:     l.Label,
			Amount:    amt,
			Hint:      l.Hint,
			Deduction: l.Deduction,
		})
	}
	if len(doc.Lines) > 0 && math.Abs(round2(credits-deductions)-round2(doc.Net)) > 0.01 {
		s.log.WarnContext(ctx, "published rider payout does not reconcile with its lines",
			slog.String("rider", doc.RiderPartyID), slog.String("month", month),
			slog.Float64("net", round2(doc.Net)), slog.Float64("lines_net", round2(credits-deductions)))
	}
	status := strings.ToUpper(strings.TrimSpace(doc.Status))
	switch status {
	case "PENDING", "PROCESSING", "PAID":
	default:
		status = "PENDING" // an unknown state is not a payout
	}
	m := doc.Month
	if m == "" {
		m = month
	}
	return riderEarningsWire{
		Month:       m,
		PresentDays: doc.PresentDays,
		Deliveries:  doc.Deliveries,
		Net:         round2(doc.Net),
		Status:      status,
		PaidOn:      rfc3339Ptr(doc.PaidOn),
		Lines:       lines,
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// 3.8 PENALTIES
// ═════════════════════════════════════════════════════════════════════════════

// riderPenalties — GET /penalties. Real records, newest first, empty when the
// rider has none (which is the common and desirable case).
func (h *handler) riderPenalties(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()

	docs, err := h.svc.repo.riderListPenalties(ctx, actor.PartyID, riderPenaltyListLimit)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]riderPenaltyWire, 0, len(docs))
	if len(docs) == 0 {
		httpx.JSON(w, http.StatusOK, out)
		return
	}

	// The streak is only computed when there is a penalty to show it against —
	// it costs two extra queries and is meaningless on an empty list.
	streak := h.svc.riderCleanDayStreak(ctx, actor.PartyID, time.Now().UTC())
	for i := range docs {
		d := &docs[i]
		status := strings.ToUpper(strings.TrimSpace(d.Status))
		if status == "" {
			status = "APPLIED"
		}
		out = append(out, riderPenaltyWire{
			ID:       d.PenaltyID,
			Date:     rfc3339(d.AppliedAt),
			Category: d.Category,
			Reason:   d.Reason,
			Amount:   round2(d.Amount),
			Status:   status,
			// Only a live, undisputed penalty can still be waived — the flag
			// must not invite a tap that is guaranteed to 409.
			WaiverEligible: d.WaiverEligible && status == "APPLIED" && d.ConcernID == "",
			WaiverStreak:   streak,
			ConcernID:      d.ConcernID,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// riderCleanDayStreak counts CONSECUTIVE CLEAN DAYS backwards from today: days
// the rider was actually present (a real attendance record) with no live
// penalty dated to them. Waived and reversed penalties are forgiven and do not
// break the streak — that is what "waived" means.
//
// This is a derived count, not a stored score. If either query fails we return
// 0, because a wrong streak in front of a rider is worse than no streak: it is
// the number they will believe entitles them to a waiver.
func (s *service) riderCleanDayStreak(ctx context.Context, riderPartyID string, now time.Time) int {
	today := now.In(istZone)
	fromDay := today.AddDate(0, 0, -(riderWaiverStreakLookbackDays - 1)).Format("2006-01-02")
	toDay := today.AddDate(0, 0, 1).Format("2006-01-02")

	present, err := s.repo.riderPresentDays(ctx, riderPartyID, fromDay, toDay)
	if err != nil {
		s.log.ErrorContext(ctx, "clean-day streak: attendance read failed",
			slog.String("rider", riderPartyID), slog.Any("err", err))
		return 0
	}
	if len(present) == 0 {
		return 0
	}
	penalised, err := s.repo.riderPenalisedDays(ctx, riderPartyID, fromDay, toDay)
	if err != nil {
		s.log.ErrorContext(ctx, "clean-day streak: penalty read failed",
			slog.String("rider", riderPartyID), slog.Any("err", err))
		return 0
	}
	// present is newest-first; walk back until a day carries a live penalty.
	streak := 0
	for _, day := range present {
		if penalised[day] {
			break
		}
		streak++
	}
	return streak
}

// riderPenaltyConcern — POST /penalties/{id}/concern.
//
// The support ticket is written FIRST and the penalty is then moved under review
// by a guarded update that also requires concern_id to still be unset. That
// order means concern_id on a penalty always points at a ticket that exists; if
// the guard loses the race (a second dispute, a supervisor waiving it in the
// same second) the just-written ticket is rolled back and the caller gets 409.
func (h *handler) riderPenaltyConcern(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	penaltyID, err := riderPathID(r, "penaltyId")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderPenaltyConcernRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	reasonCode := strings.ToUpper(strings.TrimSpace(req.Reason))
	label, ok := riderConcernReasons[reasonCode]
	if !ok {
		httpx.Error(w, r, httpx.BadRequest("INVALID_REASON", "choose a reason for the dispute"))
		return
	}
	note, err := riderText(req.Note, "note", riderMaxNoteLen)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	photoRef, err := riderText(req.PhotoRef, "photo_ref", riderMaxPhotoRefLen)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()

	pen, err := h.svc.repo.riderFindPenalty(ctx, actor.PartyID, penaltyID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if pen.ConcernID != "" {
		httpx.Error(w, r, httpx.Conflict("CONCERN_EXISTS", "you have already raised a concern on this penalty"))
		return
	}
	if strings.ToUpper(pen.Status) != "APPLIED" {
		// WAIVED / REVERSED are already in the rider's favour; UNDER_REVIEW is
		// a dispute in flight. None of them take a new concern.
		httpx.Error(w, r, httpx.Conflict("NOT_DISPUTABLE", "this penalty cannot be disputed in its current state"))
		return
	}

	now := time.Now().UTC()
	concernID := newRiderOpsID("con")
	description := note
	if description == "" {
		description = label // the rider chose a reason and typed nothing — keep the reason as the body
	}
	title := "Penalty dispute"
	if strings.TrimSpace(pen.Category) != "" {
		title = pen.Category + " — penalty dispute"
	}
	ticket := riderConcernDoc{
		ConcernID:    concernID,
		RiderPartyID: actor.PartyID,
		Type:         "PENALTY",
		Title:        title,
		Description:  description,
		ReasonCode:   reasonCode,
		PhotoRef:     photoRef,
		PenaltyID:    penaltyID,
		Status:       "OPEN",
		RaisedAt:     now,
		UpdatedAt:    now,
	}
	if err := h.svc.repo.riderInsertConcern(ctx, ticket); err != nil {
		httpx.Error(w, r, err)
		return
	}
	matched, err := h.svc.repo.riderAttachConcern(ctx, actor.PartyID, penaltyID, concernID, now)
	if err != nil {
		h.svc.repo.riderDeleteConcern(ctx, concernID)
		httpx.Error(w, r, err)
		return
	}
	if matched == 0 {
		h.svc.repo.riderDeleteConcern(ctx, concernID) // never leave an orphan ticket behind
		httpx.Error(w, r, httpx.Conflict("CONCERN_EXISTS", "you have already raised a concern on this penalty"))
		return
	}
	h.svc.log.InfoContext(ctx, "rider penalty disputed",
		slog.String("rider", actor.PartyID), slog.String("penalty_id", penaltyID),
		slog.String("concern_id", concernID), slog.String("reason", reasonCode))

	httpx.JSON(w, http.StatusOK, riderConcernResponse{ConcernID: concernID, Status: "UNDER_REVIEW"})
}

// riderPenaltyWaiver — POST /penalties/{id}/waiver.
//
// Self-service forgiveness, allowed ONLY where the record itself says it is
// allowed. Every refusal uses WAIVER_NOT_ELIGIBLE — the single code the client
// switches on — with a message that explains which of the reasons applies.
// The update is guarded on status AND waiver_eligible, and clears the flag, so
// a second tap (or a second device) can never spend the same waiver twice.
func (h *handler) riderPenaltyWaiver(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	penaltyID, err := riderPathID(r, "penaltyId")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()

	pen, err := h.svc.repo.riderFindPenalty(ctx, actor.PartyID, penaltyID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	switch {
	case !pen.WaiverEligible:
		httpx.Error(w, r, httpx.Conflict("WAIVER_NOT_ELIGIBLE", "this penalty cannot be waived"))
		return
	case strings.ToUpper(pen.Status) == "WAIVED":
		httpx.Error(w, r, httpx.Conflict("WAIVER_NOT_ELIGIBLE", "this penalty is already waived"))
		return
	case strings.ToUpper(pen.Status) != "APPLIED":
		httpx.Error(w, r, httpx.Conflict("WAIVER_NOT_ELIGIBLE", "this penalty cannot be waived in its current state"))
		return
	}

	now := time.Now().UTC()
	matched, err := h.svc.repo.riderWaivePenalty(ctx, actor.PartyID, penaltyID, now)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if matched == 0 {
		httpx.Error(w, r, httpx.Conflict("WAIVER_NOT_ELIGIBLE", "this penalty cannot be waived"))
		return
	}
	h.svc.log.InfoContext(ctx, "rider penalty waived (self-service)",
		slog.String("rider", actor.PartyID), slog.String("penalty_id", penaltyID),
		slog.Float64("amount", round2(pen.Amount)))

	httpx.JSON(w, http.StatusOK, statusResponse{Status: "WAIVED"})
}

// ═════════════════════════════════════════════════════════════════════════════
// Repository
// ═════════════════════════════════════════════════════════════════════════════

// riderRaiseCashForDeliveredCOD is the sweep that turns real COD drops into cash
// dues. It reads this rider's DELIVERED, non-prepaid, non-zero deliveries inside
// the lookback window, skips the ones that already carry a due, and inserts the
// rest as PENDING.
//
// Idempotency comes from the id, not from this function: cash_id is derived from
// the delivery id and uniquely indexed, so a duplicate insert is a duplicate-key
// error we can safely ignore, even if two requests sweep concurrently.
func (r *repository) riderRaiseCashForDeliveredCOD(ctx context.Context, riderPartyID string, now time.Time) error {
	since := rfc3339(now.AddDate(0, 0, -riderCashLookbackDays))
	// delivered_at is stored as an RFC3339 STRING on the delivery (delivery.go),
	// always UTC and always the same width, so a lexicographic range is exactly
	// a chronological one here.
	cur, err := r.deliveries.Find(ctx, bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "status", Value: "DELIVERED"},
		{Key: "payment_mode", Value: "COD"},
		{Key: "amount", Value: bson.D{{Key: "$gt", Value: 0}}},
		{Key: "delivered_at", Value: bson.D{{Key: "$gte", Value: since}}},
	}, options.Find().
		// NEWEST FIRST. This Find is capped, and without a sort Mongo serves it
		// off the {rider_party_id:1} index in insertion order — oldest first —
		// so a rider busy enough to exceed the cap inside the lookback window
		// would silently stop having dues raised for their most RECENT drops,
		// which are exactly the ones with money still in their bag.
		SetSort(bson.D{{Key: "delivered_at", Value: -1}}).
		SetLimit(riderCashScanLimit))
	if err != nil {
		return httpx.Internal(fmt.Errorf("scan delivered cod deliveries: %w", err))
	}
	var dels []delivery
	if err := cur.All(ctx, &dels); err != nil {
		return httpx.Internal(fmt.Errorf("decode delivered cod deliveries: %w", err))
	}
	if len(dels) == 0 {
		return nil
	}

	// One round trip to find what already exists, rather than one insert per drop.
	ids := make(bson.A, 0, len(dels))
	for i := range dels {
		ids = append(ids, riderCashIDFor(dels[i].ID))
	}
	coll := r.riderColl(collRiderCash)
	ecur, err := coll.Find(ctx,
		bson.D{{Key: "cash_id", Value: bson.D{{Key: "$in", Value: ids}}}},
		options.Find().SetProjection(bson.D{{Key: "cash_id", Value: 1}}))
	if err != nil {
		return httpx.Internal(fmt.Errorf("scan existing cash dues: %w", err))
	}
	var existing []struct {
		CashID string `bson:"cash_id"`
	}
	if err := ecur.All(ctx, &existing); err != nil {
		return httpx.Internal(fmt.Errorf("decode existing cash dues: %w", err))
	}
	have := make(map[string]bool, len(existing))
	for _, e := range existing {
		have[e.CashID] = true
	}

	docs := make([]any, 0, len(dels))
	for i := range dels {
		d := &dels[i]
		cashID := riderCashIDFor(d.ID)
		if have[cashID] {
			continue
		}
		// The due is dated to the drop, not to this sweep, so a due raised the
		// morning after still reads as yesterday's money — which is what makes
		// the aged-due high-priority flag meaningful.
		raised := now
		if t, perr := time.Parse(time.RFC3339, d.DeliveredAt); perr == nil {
			raised = t.UTC()
		}
		name := strings.TrimSpace(d.ConsumerName)
		if name == "" {
			name = "Customer" // honest fallback: we have no name, not a made-up one
		}
		docs = append(docs, riderCashDoc{
			CashID:       cashID,
			RiderPartyID: riderPartyID,
			DeliveryID:   d.ID,
			OrderCode:    d.OrderCode,
			CustomerName: name,
			AddressLine:  d.AddressLine,
			Phone:        d.Phone,
			Amount:       round2(d.Amount),
			Status:       "PENDING",
			Day:          riderISTDay(raised),
			RaisedAt:     raised,
			CreatedAt:    now,
			UpdatedAt:    now,
		})
	}
	if len(docs) == 0 {
		return nil
	}
	// Unordered: one duplicate (a concurrent sweep) must not stop the rest.
	if _, err := coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false)); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil // the due already exists — exactly what we wanted
		}
		return httpx.Internal(fmt.Errorf("raise cash dues: %w", err))
	}
	return nil
}

// riderListCash returns everything still outstanding plus anything collected
// since todayStart. Always scoped to the caller.
func (r *repository) riderListCash(ctx context.Context, riderPartyID string, todayStart time.Time) ([]riderCashDoc, error) {
	filter := bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "status", Value: "PENDING"}},
			bson.D{
				{Key: "status", Value: "COLLECTED"},
				{Key: "collected_at", Value: bson.D{{Key: "$gte", Value: todayStart}}},
			},
		}},
	}
	cur, err := r.riderColl(collRiderCash).Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "raised_at", Value: -1}}).SetLimit(riderCashScanLimit))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list cash dues: %w", err))
	}
	out := []riderCashDoc{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode cash dues: %w", err))
	}
	return out, nil
}

// riderFindCash loads one due BY (rider, cash id). A due belonging to another
// rider returns NOT_FOUND, not FORBIDDEN — a 403 would confirm it exists.
func (r *repository) riderFindCash(ctx context.Context, riderPartyID, cashID string) (*riderCashDoc, error) {
	var doc riderCashDoc
	err := r.riderColl(collRiderCash).FindOne(ctx, bson.D{
		{Key: "cash_id", Value: cashID},
		{Key: "rider_party_id", Value: riderPartyID},
	}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("cash request")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("load cash due: %w", err))
	}
	return &doc, nil
}

// riderCashOTPPending reports whether a LIVE (unexpired) collection challenge
// exists for this due. Expiry is checked here rather than trusting the TTL
// monitor, which reaps lazily — an expired code must not gate the money.
func (r *repository) riderCashOTPPending(ctx context.Context, cashID string, now time.Time) (bool, error) {
	n, err := r.riderColl(collDeliveryOTP).CountDocuments(ctx, bson.D{
		{Key: "scope", Value: "cash"},
		{Key: "ref_id", Value: cashID},
		{Key: "expires_at", Value: bson.D{{Key: "$gt", Value: now}}},
	})
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("check cash otp challenge: %w", err))
	}
	return n > 0, nil
}

// riderSettleCash is the guarded settlement. status:"PENDING" IS the lock: the
// second of two concurrent collects matches nothing and the caller reports the
// conflict. Returns MatchedCount so the handler can tell "already collected"
// from "wrote it".
func (r *repository) riderSettleCash(ctx context.Context, riderPartyID, cashID string, amount float64, mode string, now time.Time) (int64, error) {
	res, err := r.riderColl(collRiderCash).UpdateOne(ctx,
		bson.D{
			{Key: "cash_id", Value: cashID},
			{Key: "rider_party_id", Value: riderPartyID},
			{Key: "status", Value: "PENDING"},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: "COLLECTED"},
			{Key: "collected_amount", Value: amount},
			{Key: "mode", Value: mode},
			{Key: "collected_at", Value: now},
			{Key: "updated_at", Value: now},
		}}},
	)
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("settle cash due: %w", err))
	}
	return res.MatchedCount, nil
}

// riderReverseCash puts a collected due back to PENDING. The window is enforced
// IN THE FILTER as well as in the handler, so a request that sat in a queue
// past the cutoff cannot land late; the collected amount is cleared and the
// reversal is recorded on the record itself for the centre to see.
// riderWithdrawPendingCash removes a derived cash due that was never collected.
//
// Used when the delivery that produced it is undone. Guarded on status PENDING
// so it can never delete money a rider actually took — that path is
// riderReverseCash, which puts the due back rather than removing it.
func (r *repository) riderWithdrawPendingCash(ctx context.Context, riderPartyID, cashID string) (int64, error) {
	res, err := r.riderColl(collRiderCash).DeleteOne(ctx, bson.D{
		{Key: "cash_id", Value: cashID},
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "status", Value: "PENDING"},
	})
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("withdraw pending cash due: %w", err))
	}
	return res.DeletedCount, nil
}

func (r *repository) riderReverseCash(ctx context.Context, riderPartyID, cashID, reason string, now, cutoff time.Time) (int64, error) {
	res, err := r.riderColl(collRiderCash).UpdateOne(ctx,
		bson.D{
			{Key: "cash_id", Value: cashID},
			{Key: "rider_party_id", Value: riderPartyID},
			{Key: "status", Value: "COLLECTED"},
			{Key: "collected_at", Value: bson.D{{Key: "$gte", Value: cutoff}}},
			{Key: "remitted_at", Value: bson.D{{Key: "$in", Value: bson.A{nil}}}},
		},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "status", Value: "PENDING"},
				{Key: "undone_at", Value: now},
				{Key: "undo_reason", Value: reason},
				{Key: "updated_at", Value: now},
			}},
			{Key: "$unset", Value: bson.D{
				{Key: "collected_amount", Value: ""},
				{Key: "collected_at", Value: ""},
				{Key: "mode", Value: ""},
			}},
			{Key: "$inc", Value: bson.D{{Key: "undo_count", Value: 1}}},
		},
	)
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("reverse cash collection: %w", err))
	}
	return res.MatchedCount, nil
}

// riderRouteDayClosed reports whether the rider already closed that IST day's
// route. The route-day document is owned by rider_ops_route.go, so this reads it
// defensively (raw document, either shape) rather than binding to a struct this
// file does not own. Unknown → false: a missing route day must not block a
// legitimate correction.
func (r *repository) riderRouteDayClosed(ctx context.Context, riderPartyID, day string) bool {
	var doc bson.M
	err := r.riderColl(collRiderRouteDays).FindOne(ctx, bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "day", Value: day},
	}).Decode(&doc)
	if err != nil {
		return false
	}
	if v, ok := doc["completed"].(bool); ok && v {
		return true
	}
	switch v := doc["completed_at"].(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case time.Time:
		return !v.IsZero()
	case nil:
		return false
	default:
		return true // a completed_at of some other type is still a completion stamp
	}
}

// riderPresentDayFilter matches attendance records that represent a day the
// rider actually turned up. The attendance document is owned by
// rider_ops_attendance.go, so any ONE of its "present" signals counts — this
// file must not depend on a single field name it does not own.
func riderPresentDayFilter(riderPartyID, fromDay, toDay string) bson.D {
	return bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "day", Value: bson.D{{Key: "$gte", Value: fromDay}, {Key: "$lt", Value: toDay}}},
		{Key: "$or", Value: bson.A{
			// $nin:[null] also excludes a missing field, which is what we want.
			bson.D{{Key: "check_in_at", Value: bson.D{{Key: "$nin", Value: bson.A{nil, ""}}}}},
			bson.D{{Key: "present", Value: true}},
			bson.D{{Key: "state", Value: bson.D{{Key: "$in", Value: bson.A{"ON_DUTY", "CHECKED_OUT"}}}}},
		}},
	}
}

// riderCountPresentDays counts real attendance days in [fromDay, toDay).
func (r *repository) riderCountPresentDays(ctx context.Context, riderPartyID, fromDay, toDay string) (int, error) {
	n, err := r.riderColl(collRiderAttendance).CountDocuments(ctx, riderPresentDayFilter(riderPartyID, fromDay, toDay))
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("count present days: %w", err))
	}
	return int(n), nil
}

// riderPresentDays lists the IST days the rider was present, NEWEST FIRST — the
// order the clean-day streak walks.
func (r *repository) riderPresentDays(ctx context.Context, riderPartyID, fromDay, toDay string) ([]string, error) {
	cur, err := r.riderColl(collRiderAttendance).Find(ctx,
		riderPresentDayFilter(riderPartyID, fromDay, toDay),
		options.Find().
			SetProjection(bson.D{{Key: "day", Value: 1}}).
			SetSort(bson.D{{Key: "day", Value: -1}}).
			SetLimit(riderWaiverStreakLookbackDays))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list present days: %w", err))
	}
	var rows []struct {
		Day string `bson:"day"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode present days: %w", err))
	}
	days := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Day != "" {
			days = append(days, row.Day)
		}
	}
	return days, nil
}

// riderCountDelivered counts DELIVERED tasks in [from, to) — the real deliveries
// behind the earnings statement's "deliveries" figure.
func (r *repository) riderCountDelivered(ctx context.Context, riderPartyID string, from, to time.Time) (int, error) {
	n, err := r.deliveries.CountDocuments(ctx, bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "status", Value: "DELIVERED"},
		{Key: "delivered_at", Value: bson.D{
			{Key: "$gte", Value: rfc3339(from)},
			{Key: "$lt", Value: rfc3339(to)},
		}},
	})
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("count delivered tasks: %w", err))
	}
	return int(n), nil
}

// riderFindEarnings loads a published statement. (nil, nil) means "not
// published" — a distinct, meaningful answer, not an error.
func (r *repository) riderFindEarnings(ctx context.Context, riderPartyID, month string) (*riderEarningsDoc, error) {
	var doc riderEarningsDoc
	err := r.riderColl(collRiderEarnings).FindOne(ctx, bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "month", Value: month},
	}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("load earnings statement: %w", err))
	}
	return &doc, nil
}

// riderListPenalties returns the rider's penalties, newest applied first.
func (r *repository) riderListPenalties(ctx context.Context, riderPartyID string, limit int64) ([]riderPenaltyDoc, error) {
	cur, err := r.riderColl(collRiderPenalties).Find(ctx,
		bson.D{{Key: "rider_party_id", Value: riderPartyID}},
		options.Find().SetSort(bson.D{{Key: "applied_at", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list penalties: %w", err))
	}
	out := []riderPenaltyDoc{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode penalties: %w", err))
	}
	return out, nil
}

// riderPenalisedDays returns the set of IST days carrying a LIVE penalty
// (APPLIED or UNDER_REVIEW). Waived and reversed penalties are excluded — a
// forgiven day is a clean day.
func (r *repository) riderPenalisedDays(ctx context.Context, riderPartyID, fromDay, toDay string) (map[string]bool, error) {
	from, _, err := istDayBounds(fromDay)
	if err != nil {
		return nil, err
	}
	to, _, err := istDayBounds(toDay)
	if err != nil {
		return nil, err
	}
	cur, ferr := r.riderColl(collRiderPenalties).Find(ctx, bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"APPLIED", "UNDER_REVIEW"}}}},
		{Key: "applied_at", Value: bson.D{{Key: "$gte", Value: from}, {Key: "$lt", Value: to}}},
	}, options.Find().
		SetProjection(bson.D{{Key: "applied_at", Value: 1}}).
		// Newest first, for the same reason as the COD sweep: the cap must only
		// ever drop days OLDER than the streak walk can reach, never the recent
		// penalties that are the whole point of the walk.
		SetSort(bson.D{{Key: "applied_at", Value: -1}}).
		SetLimit(riderPenaltyStreakScanLimit))
	if ferr != nil {
		return nil, httpx.Internal(fmt.Errorf("list penalised days: %w", ferr))
	}
	var rows []struct {
		AppliedAt time.Time `bson:"applied_at"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode penalised days: %w", err))
	}
	days := make(map[string]bool, len(rows))
	for _, row := range rows {
		days[riderISTDay(row.AppliedAt)] = true
	}
	return days, nil
}

// riderFindPenalty loads one penalty BY (rider, penalty id) — another rider's
// penalty is NOT_FOUND.
func (r *repository) riderFindPenalty(ctx context.Context, riderPartyID, penaltyID string) (*riderPenaltyDoc, error) {
	var doc riderPenaltyDoc
	err := r.riderColl(collRiderPenalties).FindOne(ctx, bson.D{
		{Key: "penalty_id", Value: penaltyID},
		{Key: "rider_party_id", Value: riderPartyID},
	}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("penalty")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("load penalty: %w", err))
	}
	return &doc, nil
}

// riderInsertConcern writes the dispute ticket into the rider's support inbox.
func (r *repository) riderInsertConcern(ctx context.Context, doc riderConcernDoc) error {
	if _, err := r.riderColl(collRiderSupport).InsertOne(ctx, doc); err != nil {
		return httpx.Internal(fmt.Errorf("raise penalty concern: %w", err))
	}
	return nil
}

// riderDeleteConcern removes a ticket whose penalty link lost the guard race.
// Best-effort by design: the caller is already returning a 409 and an orphaned
// ticket is the lesser fault, so a failure here is not worth failing louder.
func (r *repository) riderDeleteConcern(ctx context.Context, concernID string) {
	_, _ = r.riderColl(collRiderSupport).DeleteOne(ctx, bson.D{{Key: "concern_id", Value: concernID}})
}

// riderAttachConcern moves a penalty under review and links the ticket. The
// filter demands status APPLIED and no existing concern, so exactly one dispute
// can ever be attached.
func (r *repository) riderAttachConcern(ctx context.Context, riderPartyID, penaltyID, concernID string, now time.Time) (int64, error) {
	res, err := r.riderColl(collRiderPenalties).UpdateOne(ctx,
		bson.D{
			{Key: "penalty_id", Value: penaltyID},
			{Key: "rider_party_id", Value: riderPartyID},
			{Key: "status", Value: "APPLIED"},
			{Key: "concern_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: "UNDER_REVIEW"},
			{Key: "concern_id", Value: concernID},
			{Key: "concern_raised_at", Value: now},
			{Key: "updated_at", Value: now},
		}}},
	)
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("attach penalty concern: %w", err))
	}
	return res.MatchedCount, nil
}

// riderWaivePenalty applies the self-service waiver. Guarded on BOTH the applied
// status and the eligibility flag, and it clears the flag in the same write, so
// the waiver is spent exactly once.
func (r *repository) riderWaivePenalty(ctx context.Context, riderPartyID, penaltyID string, now time.Time) (int64, error) {
	res, err := r.riderColl(collRiderPenalties).UpdateOne(ctx,
		bson.D{
			{Key: "penalty_id", Value: penaltyID},
			{Key: "rider_party_id", Value: riderPartyID},
			{Key: "status", Value: "APPLIED"},
			{Key: "waiver_eligible", Value: true},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: "WAIVED"},
			{Key: "waiver_eligible", Value: false},
			{Key: "waived_at", Value: now},
			{Key: "waived_by", Value: "SELF_SERVICE"},
			{Key: "updated_at", Value: now},
		}}},
	)
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("waive penalty: %w", err))
	}
	return res.MatchedCount, nil
}
