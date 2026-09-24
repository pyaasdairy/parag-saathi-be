package consumer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// ─────────────────────────────────────────────────────────────────────────────
// Rider ops — THE DAY'S ROUTE and THE CRATE INVENTORY (RIDER_API.md §3.4, §3.6)
//
// Two screens hang off this file, and both are computed from records that
// already exist rather than from anything the rider console is told to believe:
//
//	GET  /route/today          the day at a glance — every number is a count or
//	                           a sum over the rider's OWN delivery tasks for the
//	                           current IST day. Nothing is stored to produce it,
//	                           so it can never drift from the queue the rider is
//	                           looking at on the next tab.
//	POST /route/complete       "I have finished my run." Refused with
//	                           ROUTE_NOT_COMPLETE while any stop is still open —
//	                           a route marked complete over a live stop is how a
//	                           customer's milk quietly becomes nobody's problem.
//	GET  /inventory/today      the morning pickup sheet, DERIVED from the real
//	                           demand: the order lines behind today's tasks,
//	                           rolled up per product. Persisted once per rider
//	                           per day so the verification has something stable
//	                           to attach to.
//	POST /inventory/{id}/verify what the rider ACTUALLY loaded. Taking less than
//	                           the demand is a declared shortage, recorded here
//	                           so the centre reconciles it at the dock instead of
//	                           the rider discovering it at a customer's door.
//
// A rider with no tasks today gets zeros and an empty line list — the honest
// answer, which the console renders as a proper empty state. No sample stop, no
// placeholder product, ever: this whole surface exists because the app used to
// show invented data.
// ─────────────────────────────────────────────────────────────────────────────

// riderRouteMaxTasks caps a single day's read. A rider runs tens of stops, not
// hundreds; the cap only stops a corrupt query from pulling a whole collection
// into memory behind a phone on a 2G connection.
const riderRouteMaxTasks = 500

// riderRouteDayKey is the IST calendar day a route belongs to. The morning run
// starts at 05:00 IST — deriving the day from UTC would roll it over at 05:30,
// i.e. in the middle of the run, splitting one route across two "days".
func riderRouteDayKey(now time.Time) string { return now.In(istZone).Format("2006-01-02") }

// ── GET /route/today ────────────────────────────────────────────────────────

// riderRouteSummaryResponse is the shape RouteSummary.fromWire reads
// (lib/models/rider.dart). Every field is a fact about the rider's own tasks.
type riderRouteSummaryResponse struct {
	Total         int     `json:"total"`
	Pending       int     `json:"pending"`
	Delivered     int     `json:"delivered"`
	NotDelivered  int     `json:"not_delivered"`
	Priority      int     `json:"priority"`
	CashPending   int     `json:"cash_pending"`
	CashToCollect float64 `json:"cash_to_collect"`
	DistanceKm    float64 `json:"distance_km"`
	StartedAt     string  `json:"started_at,omitempty"`
	Slot          string  `json:"slot,omitempty"`
}

// riderRouteToday handles GET /consumer/delivery/rider/route/today.
func (h *handler) riderRouteToday(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	tasks, err := h.svc.repo.riderRouteTasksForDay(r.Context(), actor.PartyID, riderRouteDayKey(time.Now()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, riderSummariseRoute(tasks))
}

// riderSummariseRoute rolls a day's tasks into the header numbers. Pure, so the
// definitions below are readable in one place and testable without Mongo.
//
// CANCELLED stops are left OUT of `total` entirely: a cancelled order is not a
// stop the rider owes anyone, and counting it would make total > pending +
// delivered + not_delivered and leave the console's progress ring permanently
// short of full.
// riderStoreCancelledReason is the default failure_reason storeCancelDelivery
// stamps when a manager cancels a task without typing one (delivery_svc.go).
const riderStoreCancelledReason = "Cancelled by the store"

// riderTaskIsCancelled reports whether a task should be left out of the day's
// work entirely.
//
// Testing only for status "CANCELLED" was inert: storeCancelDelivery marks the
// TASK "FAILED" and cancels the parent ORDER, so a crate the manager cancelled
// at 04:30 still counted as a not-delivered stop against the rider and still
// sat in the pickup sheet as demand.
//
// KNOWN LIMIT, stated rather than papered over: a manager who types their own
// cancellation reason is not matched here, because the only fully reliable
// signal is the parent order's status and that needs a join this pure summary
// does not have. Such a task keeps the pre-existing behaviour (it counts as
// not-delivered) — the guard is conservative, never falsely "cleared".
func riderTaskIsCancelled(d *delivery) bool {
	return d.Status == "CANCELLED" || strings.TrimSpace(d.FailureReason) == riderStoreCancelledReason
}

// riderRoundName is the short label the route header's pill is sized for.
// Derived from the delivery lane rather than echoing the per-stop slot string.
func riderRoundName(d *delivery) string {
	switch strings.ToLower(strings.TrimSpace(d.Lane)) {
	case "instant":
		return "Instant"
	case "morning":
		return "Morning"
	}
	// No lane recorded: fall back to the window out of the stop's own label
	// ("2026-08-31 · 05:00 - 07:30 AM" -> "05:00 - 07:30 AM") rather than
	// repeating the date.
	if _, after, found := strings.Cut(d.Slot, "·"); found {
		return strings.TrimSpace(after)
	}
	return strings.TrimSpace(d.Slot)
}

func riderSummariseRoute(tasks []delivery) riderRouteSummaryResponse {
	out := riderRouteSummaryResponse{}
	earliest := "" // RFC3339 UTC sorts lexicographically, so min = string min
	for i := range tasks {
		d := &tasks[i]
		if riderTaskIsCancelled(d) {
			continue
		}
		out.Total++
		// DistanceKm is deliberately NOT summed here.
		//
		// delivery.distance_km is the straight line from the store to that one
		// door. Adding thirty of them produces a number that is not any distance
		// anybody travels — and the client paints it verbatim in a pill next to
		// a route icon ("90.0 km"), which a rider reasonably reads as their run.
		// There is no routing engine behind this surface, so the honest value is
		// none: the field is omitempty and the client hides the pill at zero.
		if out.Slot == "" {
			// The round's name, not one stop's schedule label. d.Slot carries the
			// full "2026-08-31 · 05:00 - 07:30 AM" string, which overflows a pill
			// sized for "Morning" and repeats a date the rider already knows.
			out.Slot = riderRoundName(d)
		}
		switch d.Status {
		case "DELIVERED":
			out.Delivered++
		case "FAILED":
			out.NotDelivered++
		default:
			// ASSIGNED / ACCEPTED / OUT_FOR_DELIVERY — still owed to a customer.
			out.Pending++
			// Priority mirrors the client's own definition (rider_data.dart:
			// instant lane, perishable cold-chain, or cash in the bag) so the
			// header pill and the Priority tab can never disagree.
			if d.Lane == "instant" || d.Perishable || d.PaymentMode == "COD" {
				out.Priority++
			}
			// Cash still to come INTO the pouch. A delivered COD stop has
			// already been collected — what happens to that money afterwards is
			// the cash surface's business (rider_ops_money.go), not the route's.
			if d.PaymentMode == "COD" {
				out.CashPending++
				out.CashToCollect += d.Amount
			}
		}
		// The route started when the rider first acted on it — accepted a stop
		// or left the centre with it — whichever came first.
		for _, ts := range []string{d.AcceptedAt, d.OutForDeliveryAt} {
			if ts != "" && (earliest == "" || ts < earliest) {
				earliest = ts
			}
		}
	}
	out.DistanceKm = round2(out.DistanceKm)
	out.CashToCollect = round2(out.CashToCollect)
	// Re-emit through rfc3339 rather than echoing the stored string, so a value
	// written with an offset by some older path still leaves here normalised.
	if earliest != "" {
		if t, err := time.Parse(time.RFC3339, earliest); err == nil {
			out.StartedAt = rfc3339(t)
		}
	}
	return out
}

// riderRouteTasksForDay reads the rider's OWN delivery tasks for one IST day.
//
// The day anchor is assigned_at: the moment the task landed on THIS rider's
// plate (it is re-stamped on assign and on claim), which is exactly the day the
// rider owes the stop. created_at would date a subscription order to the night
// it was minted, and delivered_at only exists once the work is already done.
//
// assigned_at is stored as an RFC3339 string in UTC (delivery_svc.go), so a
// string range query is the correct comparison — the format is fixed-width and
// lexicographic order is chronological order. The rest of this file compares
// those timestamps the same way the offer fence already does.
func (r *repository) riderRouteTasksForDay(ctx context.Context, riderPartyID, day string) ([]delivery, error) {
	if strings.TrimSpace(riderPartyID) == "" {
		// Defensive: an empty id would match every UNASSIGNED task in the
		// collection, since rider_party_id is "" until a rider takes one.
		return nil, httpx.Unauthorized("authentication required")
	}
	from, to, err := istDayBounds(day)
	if err != nil {
		return nil, err
	}
	// The day's work is "assigned today" OR "still open", not "assigned today"
	// alone.
	//
	// assigned_at is stamped when the MANAGER assigns, which is routinely the
	// evening before a 05:00 morning round — and a stop nobody answered
	// yesterday is still ASSIGNED this morning. Scoping on assigned_at alone
	// meant the client's own stop list (RiderStore.pending, which is unscoped)
	// showed those stops while this summary did not count them: the header read
	// "0 pending" over a list of stops, and worse, POST /route/complete would
	// happily close a day with live deliveries still on it, because its
	// ROUTE_NOT_COMPLETE guard reads the same set.
	//
	// Neither branch may reach a stop due on a LATER day. Under the noon lock
	// tomorrow's morning tasks exist (and are assigned) from 12:00 today, so a
	// rider given tomorrow's round in the afternoon had it counted as today's
	// pending work, POST /route/complete refused until the next morning, and
	// today's pickup sheet listed tomorrow's crates. An undated task (an
	// instant order) belongs to whenever it is open.
	openStatuses := bson.A{"ASSIGNED", "ACCEPTED", "OUT_FOR_DELIVERY"}
	cur, err := r.deliveries.Find(ctx,
		bson.D{
			{Key: "rider_party_id", Value: riderPartyID},
			{Key: "$and", Value: bson.A{
				bson.D{{Key: "$or", Value: bson.A{
					bson.D{{Key: "assigned_at", Value: bson.D{
						{Key: "$gte", Value: rfc3339(from)},
						{Key: "$lt", Value: rfc3339(to)},
					}}},
					bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: openStatuses}}}},
				}}},
				bson.D{{Key: "$or", Value: bson.A{
					bson.D{{Key: "delivery_date", Value: bson.D{{Key: "$exists", Value: false}}}},
					bson.D{{Key: "delivery_date", Value: nil}},
					bson.D{{Key: "delivery_date", Value: bson.D{{Key: "$lte", Value: day}}}}, // "" sorts first
				}}},
			}},
		},
		options.Find().SetSort(bson.D{{Key: "assigned_at", Value: 1}}).SetLimit(riderRouteMaxTasks),
	)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find rider tasks for %s: %w", day, err))
	}
	out := []delivery{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode rider tasks for %s: %w", day, err))
	}
	return out, nil
}

// ── POST /route/complete ────────────────────────────────────────────────────

// riderRouteCompleteResponse is the acknowledgement the console shows as
// "Route marked complete".
type riderRouteCompleteResponse struct {
	Completed bool `json:"completed"`
}

// riderRouteDayDoc is the closed route: one row per rider per IST day, held
// unique by the (rider_party_id, day) index so a double-tap — or two phones —
// can only ever close the day once. The counts are a snapshot of what was true
// at closing time; payroll and the centre's day-end read them without having to
// re-derive a route from tasks that keep moving afterwards.
type riderRouteDayDoc struct {
	RiderPartyID string    `bson:"rider_party_id"`
	Day          string    `bson:"day"`
	Total        int       `bson:"total"`
	Delivered    int       `bson:"delivered"`
	NotDelivered int       `bson:"not_delivered"`
	DistanceKm   float64   `bson:"distance_km"`
	CompletedAt  time.Time `bson:"completed_at"`
	CreatedAt    time.Time `bson:"created_at"`
}

// riderRouteComplete handles POST /consumer/delivery/rider/route/complete.
// No request body is read: the day and the rider both come from the server.
func (h *handler) riderRouteComplete(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.repo.riderCompleteRouteDay(r.Context(), actor.PartyID, riderRouteDayKey(time.Now())); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, riderRouteCompleteResponse{Completed: true})
}

// riderCompleteRouteDay closes the rider's IST day, refusing while any stop is
// still open.
//
// ROUTE_NOT_COMPLETE is the exact code the client switches on. The message is
// shown to the rider verbatim (translated client-side), so it says how many
// stops are left rather than just "conflict".
//
// Idempotent by construction: the write is $setOnInsert under the unique
// (rider, day) index, so calling it again on an already-closed day changes
// nothing and still answers "completed" — a retry on a flaky UP network must
// not look like a failure, and must not move the closing time.
func (r *repository) riderCompleteRouteDay(ctx context.Context, riderPartyID, day string) error {
	tasks, err := r.riderRouteTasksForDay(ctx, riderPartyID, day)
	if err != nil {
		return err
	}
	sum := riderSummariseRoute(tasks)
	if sum.Pending > 0 {
		return httpx.Conflict("ROUTE_NOT_COMPLETE",
			fmt.Sprintf("%d stop(s) are still open — deliver or mark them not-delivered first", sum.Pending))
	}
	now := time.Now().UTC()
	doc := riderRouteDayDoc{
		RiderPartyID: riderPartyID,
		Day:          day,
		Total:        sum.Total,
		Delivered:    sum.Delivered,
		NotDelivered: sum.NotDelivered,
		DistanceKm:   sum.DistanceKm,
		CompletedAt:  now,
		CreatedAt:    now,
	}
	_, err = r.riderColl(collRiderRouteDays).UpdateOne(ctx,
		bson.D{{Key: "rider_party_id", Value: riderPartyID}, {Key: "day", Value: day}},
		bson.D{{Key: "$setOnInsert", Value: doc}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		// Two concurrent completions can race the upsert into a duplicate key
		// on the unique index. The other one won; the day IS closed.
		if mongo.IsDuplicateKeyError(err) {
			return nil
		}
		return httpx.Internal(fmt.Errorf("close route day %s: %w", day, err))
	}
	return nil
}

// ── GET /inventory/today ────────────────────────────────────────────────────

// riderInventoryLineDoc is one product line of the day's pickup. DemandQty is
// derived from real order lines and never edited by the rider; TakenQty is what
// the rider declares at the dock; Short is the recorded difference, kept as its
// own field so a shortage survives even if demand is later recomputed.
type riderInventoryLineDoc struct {
	ProductID    string   `bson:"product_id"`
	Name         string   `bson:"name"`
	DemandQty    int      `bson:"demand_qty"`
	Unit         string   `bson:"unit,omitempty"`
	Variant      string   `bson:"variant,omitempty"`
	Scannable    bool     `bson:"scannable"`
	TakenQty     int      `bson:"taken_qty"`
	ScannedCodes []string `bson:"scanned_codes,omitempty"`
	Short        int      `bson:"short,omitempty"`
}

// riderInventoryDoc is the day's pickup session — unique per (rider, day).
type riderInventoryDoc struct {
	SessionID    string                  `bson:"session_id"`
	RiderPartyID string                  `bson:"rider_party_id"`
	Day          string                  `bson:"day"`
	CenterName   string                  `bson:"center_name,omitempty"`
	Lines        []riderInventoryLineDoc `bson:"lines"`
	Verified     bool                    `bson:"verified"`
	VerifiedAt   *time.Time              `bson:"verified_at,omitempty"`
	CreatedAt    time.Time               `bson:"created_at"`
	UpdatedAt    time.Time               `bson:"updated_at"`
}

type riderInventoryLineResponse struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	DemandQty int    `json:"demand_qty"`
	Unit      string `json:"unit,omitempty"`
	// Variant is the pack size ("500ml", "1L") as its own key; the Saathi
	// sheet prints variant ?? unit (lib/models/rider.dart InventoryLine.size),
	// and unit stays for the deployed build that reads only unit.
	Variant   string `json:"variant,omitempty"`
	Scannable bool   `json:"scannable"`
	TakenQty  int    `json:"taken_qty"`
}

type riderInventorySessionResponse struct {
	ID         string                       `json:"id"`
	Verified   bool                         `json:"verified"`
	VerifiedAt string                       `json:"verified_at,omitempty"`
	CenterName string                       `json:"center_name,omitempty"`
	Lines      []riderInventoryLineResponse `json:"lines"`
}

func riderInventoryView(doc *riderInventoryDoc) riderInventorySessionResponse {
	lines := make([]riderInventoryLineResponse, 0, len(doc.Lines))
	for _, l := range doc.Lines {
		lines = append(lines, riderInventoryLineResponse{
			ProductID: l.ProductID,
			Name:      l.Name,
			DemandQty: l.DemandQty,
			Unit:      l.Unit,
			Variant:   l.Variant,
			Scannable: l.Scannable,
			TakenQty:  l.TakenQty,
		})
	}
	return riderInventorySessionResponse{
		ID:         doc.SessionID,
		Verified:   doc.Verified,
		VerifiedAt: rfc3339Ptr(doc.VerifiedAt),
		CenterName: doc.CenterName,
		Lines:      lines, // [] not null — the console renders a real empty state
	}
}

// riderInventoryToday handles GET /consumer/delivery/rider/inventory/today.
func (h *handler) riderInventoryToday(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	doc, err := h.svc.repo.riderInventorySessionForDay(r.Context(), actor.PartyID, riderRouteDayKey(time.Now()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, riderInventoryView(doc))
}

// riderInventorySessionForDay returns the rider's pickup session for an IST day,
// opening it from the real demand if it does not exist yet.
//
// Two behaviours worth stating plainly:
//
//   - While the session is UNVERIFIED its lines are re-derived on every read. A
//     rider often opens the app before the centre has finished assigning the
//     morning round; a session frozen at that first glance would send them out
//     with a sheet that is short of what is actually in their crate.
//   - Once VERIFIED the lines are frozen. They are then a record of what the
//     rider declared they took, and re-deriving over it would erase a declared
//     shortage.
func (r *repository) riderInventorySessionForDay(ctx context.Context, riderPartyID, day string) (*riderInventoryDoc, error) {
	coll := r.riderColl(collRiderInventory)
	filter := bson.D{{Key: "rider_party_id", Value: riderPartyID}, {Key: "day", Value: day}}

	var doc riderInventoryDoc
	err := coll.FindOne(ctx, filter).Decode(&doc)
	switch {
	case err == nil:
		if doc.Verified {
			return &doc, nil
		}
		lines, derr := r.riderInventoryDemandForDay(ctx, riderPartyID, day)
		if derr != nil {
			return nil, derr
		}
		centre := doc.CenterName
		if centre == "" {
			centre = r.riderRouteCentreName(ctx, riderPartyID)
		}
		if riderInventoryLinesUnchanged(doc.Lines, lines) && centre == doc.CenterName {
			return &doc, nil
		}
		// Guarded on verified:false so this refresh can never race ahead of — or
		// over the top of — a verification that landed a moment ago.
		set := bson.D{
			{Key: "lines", Value: lines},
			{Key: "updated_at", Value: time.Now().UTC()},
		}
		if centre != "" {
			set = append(set, bson.E{Key: "center_name", Value: centre})
		}
		guarded := append(append(bson.D{}, filter...), bson.E{Key: "verified", Value: false})
		res, uerr := coll.UpdateOne(ctx, guarded, bson.D{{Key: "$set", Value: set}})
		if uerr != nil {
			return nil, httpx.Internal(fmt.Errorf("refresh inventory demand: %w", uerr))
		}
		if res.MatchedCount == 0 {
			// The rider verified from another screen while we were deriving the
			// demand. Their declaration is the truth — hand back the STORED
			// session rather than the sheet we just recomputed over it.
			var settled riderInventoryDoc
			if rerr := coll.FindOne(ctx, filter).Decode(&settled); rerr != nil {
				return nil, httpx.Internal(fmt.Errorf("reread inventory session: %w", rerr))
			}
			return &settled, nil
		}
		doc.Lines = lines
		doc.CenterName = centre
		return &doc, nil

	case errors.Is(err, mongo.ErrNoDocuments):
		lines, derr := r.riderInventoryDemandForDay(ctx, riderPartyID, day)
		if derr != nil {
			return nil, derr
		}
		now := time.Now().UTC()
		fresh := riderInventoryDoc{
			SessionID:    newRiderOpsID("inv"),
			RiderPartyID: riderPartyID,
			Day:          day,
			CenterName:   r.riderRouteCentreName(ctx, riderPartyID),
			Lines:        lines,
			Verified:     false,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if _, ierr := coll.InsertOne(ctx, fresh); ierr != nil {
			// Two opens of the screen at once: the unique (rider, day) index
			// picks one winner. Read the winner back rather than failing — both
			// callers must see the SAME session id, or two verifications would
			// be recorded for one pickup.
			if mongo.IsDuplicateKeyError(ierr) {
				var won riderInventoryDoc
				if rerr := coll.FindOne(ctx, filter).Decode(&won); rerr != nil {
					return nil, httpx.Internal(fmt.Errorf("reread inventory session: %w", rerr))
				}
				return &won, nil
			}
			return nil, httpx.Internal(fmt.Errorf("open inventory session: %w", ierr))
		}
		return &fresh, nil

	default:
		return nil, httpx.Internal(fmt.Errorf("load inventory session: %w", err))
	}
}

// riderInventoryLinesUnchanged reports whether a re-derived demand matches what is
// stored, so an unchanged refresh costs no write. Compared on the derived
// fields only — taken_qty on an unverified session always tracks demand.
func riderInventoryLinesUnchanged(have, want []riderInventoryLineDoc) bool {
	if len(have) != len(want) {
		return false
	}
	for i := range have {
		if have[i].ProductID != want[i].ProductID ||
			have[i].DemandQty != want[i].DemandQty ||
			have[i].Name != want[i].Name ||
			have[i].Unit != want[i].Unit ||
			have[i].Variant != want[i].Variant ||
			have[i].Scannable != want[i].Scannable {
			return false
		}
	}
	return true
}

// riderInventoryAgg is one product's running roll-up while the day's order lines
// are being folded together.
type riderInventoryAgg struct {
	productID string
	name      string
	variant   string
	qty       int
}

// riderInventoryDemandForDay derives the day's crate demand: every product line behind
// every task assigned to this rider today, rolled up per product.
//
// The quantities come from the ORDER's items, not the delivery task's, because
// the order line is the one that carries the product id — and because the store
// manager's damage adjustment (storeAdjustDelivery) rewrites both in lockstep,
// so the order is never the staler of the two. A task whose order cannot be
// read falls back to the task's own items, which carry a real name and a real
// quantity; that line is keyed by its name so it is still verifiable, rather
// than being dropped from the sheet and discovered missing at a door.
//
// CANCELLED tasks are excluded — nothing is loaded for them. FAILED tasks are
// NOT: their goods went out on the vehicle and have to reconcile back at the
// centre, so they belong on the pickup sheet.
func (r *repository) riderInventoryDemandForDay(ctx context.Context, riderPartyID, day string) ([]riderInventoryLineDoc, error) {
	tasks, err := r.riderRouteTasksForDay(ctx, riderPartyID, day)
	if err != nil {
		return nil, err
	}
	orderIDs := bson.A{}
	live := make([]*delivery, 0, len(tasks))
	for i := range tasks {
		d := &tasks[i]
		if riderTaskIsCancelled(d) {
			continue
		}
		live = append(live, d)
		if d.OrderID != "" {
			orderIDs = append(orderIDs, d.OrderID)
		}
	}
	if len(live) == 0 {
		return []riderInventoryLineDoc{}, nil
	}

	// One read for every order behind the day's tasks.
	byOrder := map[string][]orderItem{}
	if len(orderIDs) > 0 {
		cur, ferr := r.orders.Find(ctx,
			bson.D{{Key: "order_id", Value: bson.D{{Key: "$in", Value: orderIDs}}}},
			options.Find().SetProjection(bson.D{
				{Key: "order_id", Value: 1},
				{Key: "order_items", Value: 1},
			}).SetLimit(riderRouteMaxTasks),
		)
		if ferr != nil {
			return nil, httpx.Internal(fmt.Errorf("find orders for inventory demand: %w", ferr))
		}
		var rows []struct {
			OrderID string      `bson:"order_id"`
			Items   []orderItem `bson:"order_items"`
		}
		if derr := cur.All(ctx, &rows); derr != nil {
			return nil, httpx.Internal(fmt.Errorf("decode orders for inventory demand: %w", derr))
		}
		for _, row := range rows {
			byOrder[row.OrderID] = row.Items
		}
	}

	rolled := map[string]*riderInventoryAgg{}
	add := func(key, productID, name, variant string, qty int) {
		if qty <= 0 || strings.TrimSpace(name) == "" {
			return
		}
		if a, ok := rolled[key]; ok {
			a.qty += qty
			return
		}
		rolled[key] = &riderInventoryAgg{productID: productID, name: name, variant: variant, qty: qty}
	}
	for _, d := range live {
		if items, ok := byOrder[d.OrderID]; ok && len(items) > 0 {
			for _, it := range items {
				// A promotional ₹0 line is still a physical pack the rider has
				// to carry, so it counts towards the crate demand.
				key := strings.TrimSpace(it.ProductID)
				if key == "" {
					key = riderInventoryKeyFromName(it.Name)
				}
				add(key+riderInventorySizeSuffix(it.Variant), strings.TrimSpace(it.ProductID), it.Name, strings.TrimSpace(it.Variant), it.Qty)
			}
			continue
		}
		// The order could not be read: the task's own lines carry the name
		// and the pack size, and the size keeps 500ml and 1L apart.
		for _, it := range d.Items {
			add(riderInventoryKeyFromName(it.Name)+riderInventorySizeSuffix(it.Variant), "", it.Name, strings.TrimSpace(it.Variant), it.Qty)
		}
	}
	if len(rolled) == 0 {
		return []riderInventoryLineDoc{}, nil
	}
	// Verify indexes the sheet by product_id, so a product carried in two
	// sizes needs one id per size.
	sizesOf := map[string]int{}
	for _, a := range rolled {
		if a.productID != "" {
			sizesOf[a.productID]++
		}
	}

	// Pack size for the sheet, read from the catalogue where the SKU is known.
	units := r.riderInventoryProductUnits(ctx, rolled)

	out := make([]riderInventoryLineDoc, 0, len(rolled))
	for key, a := range rolled {
		unit := units[a.productID]
		if unit == "" {
			// The order line's own variant ("500ml") is the next best real
			// descriptor; an unknown unit stays empty and the console shows its
			// own default rather than a number dressed up as a pack size.
			unit = strings.TrimSpace(a.variant)
		}
		id := a.productID
		if id == "" {
			id = key
		} else if sizesOf[id] > 1 {
			id += ":" + strings.ToLower(a.variant)
		}
		out = append(out, riderInventoryLineDoc{
			ProductID: id,
			Name:      a.name,
			DemandQty: a.qty,
			Unit:      unit,
			Variant:   a.variant,
			// Scannable stays FALSE: nothing in this system yet records which
			// products ship in code-bearing crates. Claiming true would make the
			// console demand a scan of every box before it lets the rider verify
			// — codes that do not exist, on a screen that would then never
			// close. It flips on per product the day that registry lands.
			Scannable: false,
			TakenQty:  a.qty, // the sheet opens at "took everything"
		})
	}
	// Stable, human order — the same sheet every time it is read.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ProductID < out[j].ProductID
	})
	return out, nil
}

// riderInventoryKeyFromName is the fallback roll-up key for a line with no
// product id (an older order, or a task read without its order). Lower-cased
// and space-collapsed so "Toned Milk  1L" and "toned milk 1L" are one line.
func riderInventoryKeyFromName(name string) string {
	return "name:" + strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// riderInventorySizeSuffix keeps two pack sizes of one product on separate
// lines ("|500ml", "|1l"); a line with no size keeps the plain key, so a
// sheet derived before sizes were keyed reads the same.
func riderInventorySizeSuffix(variant string) string {
	if v := strings.ToLower(strings.TrimSpace(variant)); v != "" {
		return "|" + v
	}
	return ""
}

// riderInventoryProductUnits resolves pack sizes ("500 ml", "1 kg") for the known SKUs in
// one catalogue read. Best-effort: a product the catalogue does not know simply
// has no unit, and the caller falls back to the order line's variant.
func (r *repository) riderInventoryProductUnits(ctx context.Context, rolled map[string]*riderInventoryAgg) map[string]string {
	ids := bson.A{}
	for _, a := range rolled {
		if a.productID != "" {
			ids = append(ids, a.productID)
		}
	}
	units := map[string]string{}
	if len(ids) == 0 {
		return units
	}
	cur, err := r.catalog.Find(ctx,
		bson.D{{Key: "sku_id", Value: bson.D{{Key: "$in", Value: ids}}}},
		options.Find().SetProjection(bson.D{{Key: "sku_id", Value: 1}, {Key: "unit", Value: 1}}),
	)
	if err != nil {
		return units // cosmetic field — never fail a pickup sheet over it
	}
	var rows []struct {
		SkuID string `bson:"sku_id"`
		Unit  string `bson:"unit"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return units
	}
	for _, row := range rows {
		// A store overlay row may carry no unit of its own; the first non-empty
		// value for the SKU wins.
		if units[row.SkuID] == "" {
			units[row.SkuID] = strings.TrimSpace(row.Unit)
		}
	}
	return units
}

// riderRouteCentreName resolves the display name of the centre the rider is
// posted to (their active DELIVERY_RIDER org unit). Best-effort: an unresolved
// centre returns "" and the console simply omits the chip — it never invents a
// hub name.
func (r *repository) riderRouteCentreName(ctx context.Context, riderPartyID string) string {
	stores, err := r.storesForRider(ctx, riderPartyID)
	if err != nil || len(stores) == 0 {
		return ""
	}
	if name, ok := r.storeName(ctx, stores[0]); ok {
		return name
	}
	return ""
}

// ── POST /inventory/{sessionId}/verify ──────────────────────────────────────

// Verification input bounds. A crate sheet has tens of lines and a line has at
// most a few hundred physical codes; anything past these is a malformed or
// hostile body, and it is rejected rather than silently truncated — a truncated
// pickup is a shortage nobody declared.
const (
	riderInventoryMaxLines    = 200
	riderInventoryMaxCodes    = 500
	riderInventoryMaxCodeLen  = 64
	riderInventoryMaxQtyPerLn = 100000
)

type riderInventoryVerifyLine struct {
	ProductID    string   `json:"product_id"`
	TakenQty     *int     `json:"taken_qty"`
	ScannedCodes []string `json:"scanned_codes"`
}

type riderInventoryVerifyRequest struct {
	Lines []riderInventoryVerifyLine `json:"lines"`
}

type riderInventoryShortage struct {
	ProductID string `json:"product_id"`
	Short     int    `json:"short"`
}

type riderInventoryVerifyResponse struct {
	Verified  bool                     `json:"verified"`
	Shortages []riderInventoryShortage `json:"shortages"`
}

// riderInventoryVerify handles POST /consumer/delivery/rider/inventory/{sessionId}/verify.
func (h *handler) riderInventoryVerify(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	sessionID := strings.TrimSpace(chi.URLParam(r, "sessionId"))
	if sessionID == "" {
		httpx.Error(w, r, httpx.BadRequest("INVALID_SESSION", "session id is required"))
		return
	}
	var req riderInventoryVerifyRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if len(req.Lines) > riderInventoryMaxLines {
		httpx.Error(w, r, httpx.BadRequest("TOO_MANY_LINES",
			fmt.Sprintf("a pickup cannot declare more than %d lines", riderInventoryMaxLines)))
		return
	}
	out, err := h.svc.repo.riderVerifyInventory(r.Context(), actor.PartyID, sessionID, req.Lines)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// riderVerifyInventory records what the rider actually loaded.
//
// Ownership first: the session is looked up by (session_id, rider_party_id)
// together, so another rider's session is a 404 — never a 403, which would
// confirm that the session exists.
//
// A line taken SHORT of the demand is the whole point of the screen: it is
// stored on the session and echoed back, so the centre reconciles the gap at the
// dock. Taking MORE than the demand is rejected rather than clamped — a crate
// count above what any customer ordered is a data error somewhere upstream, and
// silently accepting it would hide it.
func (r *repository) riderVerifyInventory(ctx context.Context, riderPartyID, sessionID string, in []riderInventoryVerifyLine) (riderInventoryVerifyResponse, error) {
	var zero riderInventoryVerifyResponse
	coll := r.riderColl(collRiderInventory)
	filter := bson.D{
		{Key: "session_id", Value: sessionID},
		{Key: "rider_party_id", Value: riderPartyID},
	}
	var doc riderInventoryDoc
	if err := coll.FindOne(ctx, filter).Decode(&doc); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return zero, httpx.NotFound("inventory session")
		}
		return zero, httpx.Internal(fmt.Errorf("load inventory session for verify: %w", err))
	}
	if doc.Verified {
		return zero, httpx.Conflict("ALREADY_VERIFIED", "this pickup has already been verified")
	}

	// Index the session's own lines; only these product ids may be declared.
	idx := make(map[string]int, len(doc.Lines))
	for i, l := range doc.Lines {
		idx[l.ProductID] = i
	}
	lines := make([]riderInventoryLineDoc, len(doc.Lines))
	copy(lines, doc.Lines)

	seen := make(map[string]struct{}, len(in))
	for _, want := range in {
		pid := strings.TrimSpace(want.ProductID)
		i, ok := idx[pid]
		if !ok {
			return zero, httpx.BadRequest("UNKNOWN_PRODUCT",
				"that product is not on today's pickup sheet: "+riderRouteClip(pid))
		}
		if _, dup := seen[pid]; dup {
			return zero, httpx.BadRequest("DUPLICATE_LINE",
				"the same product was declared twice: "+riderRouteClip(pid))
		}
		seen[pid] = struct{}{}

		// A missing taken_qty means "unchanged" — the sheet opened at the full
		// demand, so that is what it stays.
		taken := lines[i].DemandQty
		if want.TakenQty != nil {
			taken = *want.TakenQty
		}
		if taken < 0 || taken > riderInventoryMaxQtyPerLn {
			return zero, httpx.Unprocessable("INVALID_QTY", "a taken quantity must be zero or more")
		}
		if taken > lines[i].DemandQty {
			return zero, httpx.Unprocessable("OVER_DEMAND",
				fmt.Sprintf("%s: %d taken is more than the %d demanded — check the crate before verifying",
					lines[i].Name, taken, lines[i].DemandQty))
		}
		codes, cerr := riderInventoryCleanCodes(want.ScannedCodes)
		if cerr != nil {
			return zero, cerr
		}
		lines[i].TakenQty = taken
		lines[i].Short = lines[i].DemandQty - taken
		lines[i].ScannedCodes = codes
	}

	now := time.Now().UTC()
	// Conditional on verified:false — the guard IS the double-submission
	// defence, so two taps on a slow connection can only record one pickup.
	res, err := coll.UpdateOne(ctx,
		append(append(bson.D{}, filter...), bson.E{Key: "verified", Value: false}),
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "lines", Value: lines},
			{Key: "verified", Value: true},
			{Key: "verified_at", Value: now},
			{Key: "updated_at", Value: now},
		}}},
	)
	if err != nil {
		return zero, httpx.Internal(fmt.Errorf("verify inventory session: %w", err))
	}
	if res.MatchedCount == 0 {
		return zero, httpx.Conflict("ALREADY_VERIFIED", "this pickup has already been verified")
	}

	shortages := []riderInventoryShortage{}
	for _, l := range lines {
		if l.Short > 0 {
			shortages = append(shortages, riderInventoryShortage{ProductID: l.ProductID, Short: l.Short})
		}
	}
	return riderInventoryVerifyResponse{Verified: true, Shortages: shortages}, nil
}

// riderInventoryCleanCodes normalises the physical codes declared against a line:
// trimmed, uppercased, de-duplicated, bounded. Codes are free text off a
// scanner, so they are length-capped and count-capped before they are stored.
func riderInventoryCleanCodes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > riderInventoryMaxCodes {
		return nil, httpx.BadRequest("TOO_MANY_CODES",
			fmt.Sprintf("a line cannot carry more than %d scanned codes", riderInventoryMaxCodes))
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if len(c) > riderInventoryMaxCodeLen {
			return nil, httpx.BadRequest("INVALID_CODE",
				fmt.Sprintf("a scanned code cannot be longer than %d characters", riderInventoryMaxCodeLen))
		}
		if _, dup := seen[c]; dup {
			continue // the same box scanned twice is one box
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// riderRouteClip bounds caller-supplied text before it is echoed in an error message.
func riderRouteClip(s string) string {
	const max = 64
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
