package consumer

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// ─────────────────────────────────────────────────────────────────────────────
// Rider performance — RIDER_API.md §3.9
//
//	GET /consumer/delivery/rider/performance           the score card
//	GET /consumer/delivery/rider/performance/feedback   real customer ratings
//	GET /consumer/delivery/rider/performance/daily      one IST day's timeline
//
// NOTHING here is stored as a "score". Every number is recomputed from the
// rider's own delivery history (consumer_deliveries) and the customer feedback
// actually written about them (rider_feedback). That is deliberate: a rider who
// walks into the centre and says "why is my score 71?" must be answerable with
// records we can point at, line by line — see riderScoreOf for the formula and
// the comment above it for the plain-language version of it.
//
// A rider with no history gets honest zeros and an empty suggestions list. We
// never fill that screen with a plausible-looking sample; the client renders a
// proper empty state and the rider learns nothing false about themselves.
// ─────────────────────────────────────────────────────────────────────────────

// riderPerfWindowDays is the rolling window every performance number is measured
// over: the last 30 IST business days, ending with today. Long enough that one
// bad morning does not tank the card, short enough that a rider who fixed their
// timekeeping three weeks ago sees it move.
const riderPerfWindowDays = 30

// Band thresholds on the 0..100 score. These are the labels the client renders
// verbatim (translated client-side), so they are named here rather than being
// magic numbers buried in an if-chain — ops tunes performance policy by editing
// exactly these four lines.
const (
	riderBandExceptionalMin = 90.0
	riderBandGoodMin        = 75.0
	riderBandFairMin        = 55.0
)

// Score weights. They do NOT have to sum to 100: a component with no data is
// dropped and the remaining weights are re-normalised (see riderScoreOf), so a
// rider whose customers have not rated them yet is not silently scored as if
// every customer had given them zero stars.
const (
	riderWeightOnTime        = 50.0
	riderWeightRating        = 30.0
	riderWeightComplaintFree = 20.0
)

// riderComplaintRatingMax — a feedback row at or below this rating is counted as
// a complaint. 2★ and below is the line the support team already works to.
const riderComplaintRatingMax = 2

// riderRatingScale is the top of the customer rating scale (1..5).
const riderRatingScale = 5.0

// Suggestion gates. A tip is only ever emitted when the rider's OWN numbers
// justify it — never as filler. Below these floors there is not enough of the
// rider's own history to say anything true, so we say nothing.
const (
	riderOnTimeAdviceMinTasks = 5    // measured deadlines needed before commenting on punctuality
	riderOnTimeAdviceBelowPct = 85.0 // …and only when they are actually missing slots
	riderRatingAdviceMinCount = 3    // ratings needed before commenting on the rating
	riderRatingAdviceBelow    = 4.0
	riderMaxSuggestions       = 3
)

// riderFeedbackPageLimit caps the feedback list. The screen is a scroll of recent
// notes, not an archive; an unbounded list on a rider with two years of ratings
// is a slow request on a phone in a lane in Lucknow.
const riderFeedbackPageLimit = 50

// riderDailyMaxLookbackDays bounds how far back the daily timeline may be asked
// for. Without it, ?date= is an unbounded scan knob on a public route.
const riderDailyMaxLookbackDays = 90

// riderPerfTaskScanLimit caps a window read. 30 days of a very busy rider is a
// few hundred rows; this is the safety rail, not the expected size.
const riderPerfTaskScanLimit int64 = 3000

// ── wire shapes ─────────────────────────────────────────────────────────────

// riderPerformanceResponse is the score card (client: PerformanceSnapshot).
type riderPerformanceResponse struct {
	Score       float64  `json:"score"`
	Band        string   `json:"band"`
	OnTimePct   float64  `json:"on_time_pct"`
	Deliveries  int      `json:"deliveries"`
	Complaints  int      `json:"complaints"`
	Rating      float64  `json:"rating"`
	Trend       float64  `json:"trend"`
	Suggestions []string `json:"suggestions"`
}

// riderFeedbackResponse is one customer rating (client: FeedbackItem).
type riderFeedbackResponse struct {
	CustomerName string `json:"customer_name"`
	Rating       int    `json:"rating"`
	Comment      string `json:"comment,omitempty"`
	At           string `json:"at,omitempty"`
}

// riderTimelineStop is one completed task on the day's timeline (client:
// TimelineStop). Subtitle is short display text, rendered in the same colour as
// the dot, so it must never contradict on_time.
type riderTimelineStop struct {
	Label    string `json:"label"`
	At       string `json:"at"`
	OnTime   bool   `json:"on_time"`
	Subtitle string `json:"subtitle,omitempty"`
}

// ── storage shapes ──────────────────────────────────────────────────────────

// riderFeedbackDoc is a customer's rating of one delivery, in rider_feedback.
// Written by the consumer side when a shopper rates their delivery; this file
// only ever reads it.
type riderFeedbackDoc struct {
	RiderPartyID string    `bson:"rider_party_id"`
	DeliveryID   string    `bson:"delivery_id,omitempty"`
	CustomerName string    `bson:"customer_name"`
	Rating       int       `bson:"rating"`
	Comment      string    `bson:"comment,omitempty"`
	At           time.Time `bson:"at"`
}

// riderPerfTask is the projection of consumer_deliveries this file needs — the
// customer's name for the timeline label, and the three fields that decide
// punctuality. Deliberately a narrow struct rather than the full `delivery`:
// nothing here should be able to leak a phone number or a proof photo onto a
// performance screen.
type riderPerfTask struct {
	DeliveryID   string `bson:"delivery_id"`
	ConsumerName string `bson:"consumer_name"`
	Slot         string `bson:"slot"`
	EtaAt        string `bson:"eta_at"`
	DeliveredAt  string `bson:"delivered_at"`
}

// ── handlers ────────────────────────────────────────────────────────────────

// riderPerformance handles GET /consumer/delivery/rider/performance.
func (h *handler) riderPerformance(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.riderPerformanceCard(r.Context(), actor.PartyID, time.Now().UTC())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// riderPerformanceFeedback handles GET /consumer/delivery/rider/performance/feedback:
// the real notes customers left about THIS rider, newest first. No feedback yet
// is an empty list, not a friendly fake review.
func (h *handler) riderPerformanceFeedback(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	rows, err := h.svc.repo.riderRecentFeedback(r.Context(), actor.PartyID, riderFeedbackPageLimit)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]riderFeedbackResponse, 0, len(rows))
	for _, f := range rows {
		name := strings.TrimSpace(f.CustomerName)
		if name == "" {
			// A rating whose customer name was never captured is still real
			// feedback — show it, attributed honestly rather than to a
			// made-up "Anita Verma".
			name = "Customer"
		}
		out = append(out, riderFeedbackResponse{
			CustomerName: name,
			Rating:       f.Rating,
			Comment:      strings.TrimSpace(f.Comment),
			At:           rfc3339Ptr(&f.At),
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// riderPerformanceDaily handles GET /consumer/delivery/rider/performance/daily
// ?date=YYYY-MM-DD (default: today in IST) — the day's real delivery timeline,
// one stop per completed task, in the order the rider actually did them.
func (h *handler) riderPerformanceDaily(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	now := time.Now().UTC()
	day := strings.TrimSpace(r.URL.Query().Get("date"))
	if day == "" {
		day = istDay(now) // "today" is the IST business day the rider is working
	}
	from, to, err := istDayBounds(day)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// Bound the knob: no future days (there is nothing there to report), and a
	// fixed lookback so ?date=1970-01-01 cannot be used as a scan.
	todayStart, _, err := istDayBounds(istDay(now))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if from.After(todayStart) {
		httpx.Error(w, r, httpx.BadRequest("DATE_IN_FUTURE", "that day has not happened yet"))
		return
	}
	if from.Before(todayStart.AddDate(0, 0, -riderDailyMaxLookbackDays)) {
		httpx.Error(w, r, httpx.BadRequest("DATE_OUT_OF_RANGE",
			fmt.Sprintf("the daily timeline goes back %d days", riderDailyMaxLookbackDays)))
		return
	}

	tasks, err := h.svc.repo.riderDeliveredTasks(r.Context(), actor.PartyID, from, to)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]riderTimelineStop, 0, len(tasks))
	for _, t := range tasks {
		at, ok := riderParseWireTime(t.DeliveredAt)
		if !ok {
			// No trustworthy completion time means no honest place on a
			// timeline — drop the row rather than pin it to "now".
			continue
		}
		label := strings.TrimSpace(t.ConsumerName)
		if label == "" {
			label = "Delivery"
		}
		onTime, subtitle := riderStopVerdict(t, at)
		out = append(out, riderTimelineStop{
			Label:    label,
			At:       rfc3339(at),
			OnTime:   onTime,
			Subtitle: subtitle,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// ── the score ───────────────────────────────────────────────────────────────

// riderWindowStats is everything measurable about one window of a rider's work.
// Counts are kept separate from the derived percentages so the formula below
// can tell "0% on time" apart from "no task in this window had a deadline".
type riderWindowStats struct {
	Deliveries     int // DELIVERED tasks completed in the window
	OnTimeMeasured int // …of those, the ones that had a comparable deadline
	OnTime         int // …of those, the ones met on or before it
	Complaints     int // feedback rows at or below riderComplaintRatingMax
	RatingCount    int // feedback rows with a usable 1..5 rating
	RatingSum      int // sum of those ratings
}

// OnTimePct is the share of MEASURABLE tasks delivered by their deadline. Tasks
// with no comparable deadline are excluded from the denominator entirely — a
// morning-lane task whose slot we cannot read is not evidence of punctuality in
// either direction, and quietly counting it as on-time would inflate the number
// the rider's payout band is argued over.
func (st riderWindowStats) OnTimePct() float64 {
	if st.OnTimeMeasured == 0 {
		return 0
	}
	return round2(100 * float64(st.OnTime) / float64(st.OnTimeMeasured))
}

// Rating is the mean of the real ratings in the window; no feedback → 0.
func (st riderWindowStats) Rating() float64 {
	if st.RatingCount == 0 {
		return 0
	}
	return round2(float64(st.RatingSum) / float64(st.RatingCount))
}

// riderScoreOf turns a window into the 0..100 score, and reports whether there
// was anything at all to measure.
//
// THE FORMULA, in the words we owe a rider who disputes it:
//
//	Your score is a weighted average of three things, each out of 100:
//	  • punctuality (weight 50) — the % of your deliveries that had a promised
//	    time and were made by it;
//	  • customer rating (weight 30) — your average star rating × 20;
//	  • complaint-free deliveries (weight 20) — 100 minus the % of your
//	    deliveries that drew a 2★-or-lower rating.
//	Anything we have no record of is left OUT and the other weights grow to fill
//	its place. So a rider nobody has rated yet is scored purely on punctuality.
//
// Note the complaint component needs RATING evidence, not just deliveries.
// Complaints are counted from ratings, so a rider nobody has rated has zero
// complaints by construction — gating this component on deliveries alone scored
// that silence as a perfect 20/20 and quietly inflated every unrated rider above
// a rated one who had a single bad day. Absence of evidence is not evidence of
// a clean record, and a score a rider disputes has to survive being explained.
//
// ok=false means the rider has no measurable history in this window at all
// (no deliveries and no ratings) — the caller reports an honest zero.
func riderScoreOf(st riderWindowStats) (float64, bool) {
	var weighted, weight float64

	if st.OnTimeMeasured > 0 {
		weighted += riderWeightOnTime * st.OnTimePct()
		weight += riderWeightOnTime
	}
	if st.RatingCount > 0 {
		weighted += riderWeightRating * (st.Rating() / riderRatingScale * 100)
		weight += riderWeightRating
	}
	if st.Deliveries > 0 && st.RatingCount > 0 {
		// Complaint-free share of the work actually done — included ONLY when
		// customers have actually rated this rider, because Complaints is
		// derived from ratings and would otherwise read 0 for pure silence.
		// Clamped at 0 because a rider can in principle collect more complaints
		// than the deliveries counted in this window (a rating written today
		// about last month's delivery), and a negative component would be
		// nonsense, not a signal.
		free := 100 * (1 - float64(st.Complaints)/float64(st.Deliveries))
		if free < 0 {
			free = 0
		}
		weighted += riderWeightComplaintFree * free
		weight += riderWeightComplaintFree
	}
	if weight == 0 {
		return 0, false
	}
	score := weighted / weight
	if score > 100 {
		score = 100
	}
	return round2(score), true
}

// riderBandOf labels a score. The client renders this string as-is, so it must
// stay inside the four values the contract pins.
func riderBandOf(score float64) string {
	switch {
	case score >= riderBandExceptionalMin:
		return "Exceptional"
	case score >= riderBandGoodMin:
		return "Good"
	case score >= riderBandFairMin:
		return "Fair"
	default:
		return "Bad"
	}
}

// riderPerformanceCard computes the score card from real records only.
func (s *service) riderPerformanceCard(ctx context.Context, riderPartyID string, now time.Time) (riderPerformanceResponse, error) {
	// Window arithmetic runs on IST day boundaries: [today-29 … end of today).
	// Deriving it from UTC would cut the window at 05:30 IST, mid-milk-run.
	_, todayEnd, err := istDayBounds(istDay(now))
	if err != nil {
		return riderPerformanceResponse{}, err
	}
	curFrom := todayEnd.AddDate(0, 0, -riderPerfWindowDays)
	prevFrom := curFrom.AddDate(0, 0, -riderPerfWindowDays)

	cur, err := s.riderWindow(ctx, riderPartyID, curFrom, todayEnd)
	if err != nil {
		return riderPerformanceResponse{}, err
	}
	score, measured := riderScoreOf(cur)

	// Trend is the change against the immediately preceding window of the same
	// length. With no prior window to compare against, the honest answer is 0 —
	// not a flattering "+4.2" invented out of a single window.
	trend := 0.0
	if measured {
		prev, err := s.riderWindow(ctx, riderPartyID, prevFrom, curFrom)
		if err != nil {
			return riderPerformanceResponse{}, err
		}
		if prevScore, prevMeasured := riderScoreOf(prev); prevMeasured {
			trend = round2(score - prevScore)
		}
	}

	// Band: a rider with nothing measured yet is NOT "Bad" — being unmeasured is
	// not a failure. They get the neutral middle label (the same one the client
	// defaults to) alongside an honest zero score.
	band := "Fair"
	if measured {
		band = riderBandOf(score)
	}

	return riderPerformanceResponse{
		Score:       score,
		Band:        band,
		OnTimePct:   cur.OnTimePct(),
		Deliveries:  cur.Deliveries,
		Complaints:  cur.Complaints,
		Rating:      cur.Rating(),
		Trend:       trend,
		Suggestions: riderSuggestions(cur),
	}, nil
}

// riderWindow measures one [from, to) window of a rider's real work.
func (s *service) riderWindow(ctx context.Context, riderPartyID string, from, to time.Time) (riderWindowStats, error) {
	st := riderWindowStats{}

	tasks, err := s.repo.riderDeliveredTasks(ctx, riderPartyID, from, to)
	if err != nil {
		return st, err
	}
	st.Deliveries = len(tasks)
	for _, t := range tasks {
		at, ok := riderParseWireTime(t.DeliveredAt)
		if !ok {
			continue
		}
		deadline, has := riderTaskDeadline(t, at)
		if !has {
			continue // no comparable deadline → out of the punctuality denominator
		}
		st.OnTimeMeasured++
		if !at.After(deadline) {
			st.OnTime++
		}
	}

	fb, err := s.repo.riderFeedbackBetween(ctx, riderPartyID, from, to)
	if err != nil {
		return st, err
	}
	for _, f := range fb {
		if f.Rating < 1 || f.Rating > riderRatingScale {
			continue // a malformed row is not a 0★ rating
		}
		st.RatingCount++
		st.RatingSum += f.Rating
		if f.Rating <= riderComplaintRatingMax {
			st.Complaints++
		}
	}
	return st, nil
}

// riderSuggestions derives coaching from the rider's actual numbers, or returns
// an empty list. This is the whole reason this endpoint was rewritten: a canned
// tip ("Start the route by 5:05 AM") presented as personalised analysis is
// fabrication, and a rider who is already punctual reading it learns that the
// score card lies. Every line below quotes a number we can point at.
func riderSuggestions(st riderWindowStats) []string {
	out := []string{}
	if st.Deliveries == 0 {
		return out // nothing has happened yet; there is nothing true to say
	}

	if st.OnTimeMeasured >= riderOnTimeAdviceMinTasks && st.OnTimePct() < riderOnTimeAdviceBelowPct {
		late := st.OnTimeMeasured - st.OnTime
		out = append(out, fmt.Sprintf(
			"%d of your %d deliveries with a promised time in the last %d days were late. Leaving the centre earlier is the fastest way to lift your score.",
			late, st.OnTimeMeasured, riderPerfWindowDays))
	}
	if st.Complaints > 0 {
		out = append(out, fmt.Sprintf(
			"%d customer rating(s) in the last %d days were %d stars or lower. Open Feedback to read what they wrote.",
			st.Complaints, riderPerfWindowDays, riderComplaintRatingMax))
	}
	if st.RatingCount >= riderRatingAdviceMinCount && st.Rating() < riderRatingAdviceBelow {
		out = append(out, fmt.Sprintf(
			"Your customer rating is %.1f out of 5 across %d ratings. The comments under Feedback say what customers are asking for.",
			st.Rating(), st.RatingCount))
	}
	if len(out) > riderMaxSuggestions {
		out = out[:riderMaxSuggestions]
	}
	return out
}

// ── punctuality ─────────────────────────────────────────────────────────────

// riderTaskDeadline resolves the moment a task was PROMISED for, from what the
// delivery record actually carries (delivery.go):
//
//	eta_at — set only on the instant lane: placed-at + 20 min, an exact promise;
//	slot   — the morning/scheduled lane's human label, e.g.
//	         "2026-08-31 · 05:00 - 07:30 AM" (slotLabel in delivery_svc.go).
//
// has=false means this task carried no comparable deadline. The caller must
// then leave it out of the punctuality measure entirely rather than assume it
// was on time.
func riderTaskDeadline(t riderPerfTask, deliveredAt time.Time) (time.Time, bool) {
	if eta, ok := riderParseWireTime(t.EtaAt); ok {
		return eta, true
	}
	return riderSlotDeadline(t.Slot, deliveredAt)
}

// riderSlotDeadline reads the END of a slot window as the promise. The label is
// display text, not a structured field, so this parser is deliberately strict:
// anything it is not certain of returns false and the task drops out of the
// denominator. Guessing a deadline wrong is worse than not measuring one — it
// shows a rider as late for a delivery they made on time.
//
// The IST day comes from the label when it carries one (a +N-days scheduled
// order), otherwise from the day the task was completed.
func riderSlotDeadline(slot string, deliveredAt time.Time) (time.Time, bool) {
	slot = strings.TrimSpace(slot)
	if slot == "" {
		return time.Time{}, false
	}
	day := deliveredAt.In(istZone)
	window := ""
	for _, part := range strings.Split(slot, "·") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if d, err := time.ParseInLocation("2006-01-02", part, istZone); err == nil {
			day = d
			continue
		}
		window = part
	}
	if window == "" {
		return time.Time{}, false // a bare date promises no time of day
	}

	// "05:00 - 07:30 AM" → "07:30 AM". Riders' slots are written with a plain
	// hyphen today; en/em dashes are normalised in case ops ever types one.
	window = strings.NewReplacer("–", "-", "—", "-").Replace(window)
	segs := strings.Split(window, "-")
	if len(segs) < 2 {
		return time.Time{}, false // a one-sided window has no closing promise
	}
	end := strings.ToUpper(strings.Join(strings.Fields(segs[len(segs)-1]), " "))
	if end == "" {
		return time.Time{}, false
	}

	// A bare hour with no meridiem ("9") is deliberately NOT accepted: 09:00 and
	// 21:00 are twelve hours apart and picking wrong would brand an on-time
	// rider late.
	for _, layout := range []string{"3:04 PM", "3:04PM", "3 PM", "3PM", "15:04"} {
		if tm, err := time.Parse(layout, end); err == nil {
			return time.Date(day.Year(), day.Month(), day.Day(), tm.Hour(), tm.Minute(), 0, 0, istZone), true
		}
	}
	return time.Time{}, false
}

// riderStopVerdict labels one timeline stop. The subtitle is rendered in the
// same colour as the dot, so the two must agree.
//
// A task with no comparable deadline is shown as a plain completed stop, NOT as
// a late one: the client only has two colours, and colouring an unmeasurable
// delivery amber accuses a rider of lateness we have no record of. The subtitle
// says exactly what we know — "Delivered" — and claims nothing about timing.
func riderStopVerdict(t riderPerfTask, deliveredAt time.Time) (bool, string) {
	deadline, has := riderTaskDeadline(t, deliveredAt)
	if !has {
		return true, "Delivered"
	}
	if !deliveredAt.After(deadline) {
		return true, "On time"
	}
	return false, "Late by " + riderLateBy(deliveredAt.Sub(deadline))
}

// riderLateBy renders a lateness the way a person says it out loud.
func riderLateBy(d time.Duration) string {
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins < 1 {
		mins = 1 // past the deadline at all is at least a minute late
	}
	if mins < 60 {
		return fmt.Sprintf("%d min", mins)
	}
	h, m := mins/60, mins%60
	if m == 0 {
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}

// riderParseWireTime parses the RFC3339 timestamps the delivery record stores as
// strings (delivered_at, eta_at). Empty or malformed → not usable.
func riderParseWireTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// ── repo ────────────────────────────────────────────────────────────────────

// riderDeliveredTasks returns THIS rider's DELIVERED tasks completed inside
// [from, to), oldest first (the order the rider worked them).
//
// Note on the range filter: consumer_deliveries stores delivered_at as an
// RFC3339 STRING in UTC (delivery_svc.go writes time.Now().UTC().Format(...)),
// not as a BSON date. Fixed-width UTC RFC3339 sorts and compares
// lexicographically in exactly the same order as the instants it encodes, so a
// string $gte/$lt range is correct here — and it is the only kind of range the
// stored data supports. The same property makes the sort below meaningful.
// Rows with an empty or absent delivered_at fall outside the range and are
// excluded, which is what we want: an undated completion is not evidence.
func (r *repository) riderDeliveredTasks(ctx context.Context, riderPartyID string, from, to time.Time) ([]riderPerfTask, error) {
	if strings.TrimSpace(riderPartyID) == "" {
		return []riderPerfTask{}, nil
	}
	filter := bson.D{
		// Scoped to the caller's own party id — never a body or query value.
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "status", Value: "DELIVERED"},
		{Key: "delivered_at", Value: bson.D{
			{Key: "$gte", Value: rfc3339(from)},
			{Key: "$lt", Value: rfc3339(to)},
		}},
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "delivered_at", Value: 1}}).
		SetLimit(riderPerfTaskScanLimit).
		SetProjection(bson.D{
			{Key: "delivery_id", Value: 1},
			{Key: "consumer_name", Value: 1},
			{Key: "slot", Value: 1},
			{Key: "eta_at", Value: 1},
			{Key: "delivered_at", Value: 1},
		})
	cur, err := r.deliveries.Find(ctx, filter, opts)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find rider deliveries: %w", err))
	}
	out := []riderPerfTask{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode rider deliveries: %w", err))
	}
	return out, nil
}

// riderFeedbackBetween returns this rider's customer feedback with at in
// [from, to). `at` is stored as a real BSON date, so this is a plain date range.
func (r *repository) riderFeedbackBetween(ctx context.Context, riderPartyID string, from, to time.Time) ([]riderFeedbackDoc, error) {
	if strings.TrimSpace(riderPartyID) == "" {
		return []riderFeedbackDoc{}, nil
	}
	filter := bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "at", Value: bson.D{
			{Key: "$gte", Value: from},
			{Key: "$lt", Value: to},
		}},
	}
	cur, err := r.riderColl(collRiderFeedback).Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}}).SetLimit(riderPerfTaskScanLimit))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find rider feedback: %w", err))
	}
	out := []riderFeedbackDoc{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode rider feedback: %w", err))
	}
	return out, nil
}

// riderRecentFeedback returns this rider's newest feedback rows, capped.
func (r *repository) riderRecentFeedback(ctx context.Context, riderPartyID string, limit int64) ([]riderFeedbackDoc, error) {
	if strings.TrimSpace(riderPartyID) == "" {
		return []riderFeedbackDoc{}, nil
	}
	if limit <= 0 || limit > riderFeedbackPageLimit {
		limit = riderFeedbackPageLimit
	}
	cur, err := r.riderColl(collRiderFeedback).Find(ctx,
		bson.D{{Key: "rider_party_id", Value: riderPartyID}},
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find rider feedback: %w", err))
	}
	out := []riderFeedbackDoc{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode rider feedback: %w", err))
	}
	// The index is (rider_party_id, at desc), but a row written without `at`
	// sorts to the end with a zero date; keep the newest-first promise explicit
	// so the client never has to re-sort.
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}
