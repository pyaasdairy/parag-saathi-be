package consumer

// THE CLOSING ALERT (founder, 24 Sep): instead of a silent closing time, the
// store manager is asked at closing time to extend instant or let it close.
// A one-minute worker writes into the operator inbox Saathi's bell already
// shows (GET /notifications/me), exactly like STORE_LOW_STOCK:
//
//   - 15 minutes before the lane closes: STORE_INSTANT_CLOSING, "Instant
//     delivery closes at 10:00 PM" (again before an extended close);
//   - at the close itself: STORE_INSTANT_CLOSED, "Instant delivery is now
//     closed. It reopens at 7:00 AM."
//
// Once per store per closing moment per kind, across concurrent ticks and
// restarts; never for a paused store, a store without an instant radius, or
// a lane the manager closed themselves. Fixed IST clock throughout.

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

const ihClosingMessage = "Keep instant open tonight: extend by 30 min, 1 h or 2 h from the Zone tab, or let it close. Orders already placed are not affected."

type ihNote struct {
	PartyID     primitive.ObjectID `bson:"party_id"`
	Phone       string             `bson:"phone"`
	Channel     string             `bson:"channel"`
	TemplateKey string             `bson:"template_key"`
	Language    string             `bson:"language"`
	Params      map[string]string  `bson:"params"`
	Status      string             `bson:"status"`
	QueuedAt    time.Time          `bson:"queued_at"`
	ReadAt      *time.Time         `bson:"read_at"`
}

// ihNotes is every instant alert in a party's inbox, oldest first.
func ihNotes(t *testing.T, w *chainWorld, party string) []ihNote {
	t.Helper()
	oid, err := primitive.ObjectIDFromHex(party)
	if err != nil {
		t.Fatalf("party id %q", party)
	}
	cur, err := w.db.Collection("notifications").Find(context.Background(), bson.D{
		{Key: "party_id", Value: oid},
		{Key: "template_key", Value: bson.D{{Key: "$in", Value: bson.A{templateStoreInstantClosing, templateStoreInstantClosed}}}},
	}, options.Find().SetSort(bson.D{{Key: "queued_at", Value: 1}}))
	if err != nil {
		t.Fatalf("notes: %v", err)
	}
	var out []ihNote
	if err := cur.All(context.Background(), &out); err != nil {
		t.Fatalf("notes decode: %v", err)
	}
	return out
}

// ihCount counts a party's instant alerts of one kind.
func ihCount(t *testing.T, w *chainWorld, party, key string) int {
	t.Helper()
	n := 0
	for _, x := range ihNotes(t, w, party) {
		if x.TemplateKey == key {
			n++
		}
	}
	return n
}

// ihAlertWorld is the chain world with the alert index built (module.go builds
// it at boot) and store B with its own manager and an instant zone.
func ihAlertWorld(t *testing.T) (*chainWorld, func(), primitive.ObjectID, auth.Actor) {
	t.Helper()
	w, done := newChainWorld(t)
	if err := w.svc.repo.ensureInstantAlertIndexes(context.Background()); err != nil {
		done()
		t.Fatalf("alert index: %v", err)
	}
	storeB, mgrB := ihSecondStore(t, w)
	return w, done, storeB, mgrB
}

func ihZoneFor(t *testing.T, w *chainWorld, storeID primitive.ObjectID, z zone) {
	t.Helper()
	z.StoreID, z.Active = storeID.Hex(), true
	z.Center = newGeoPoint(26.85, 81.0)
	if _, err := w.svc.repo.upsertZone(context.Background(), &z); err != nil {
		t.Fatalf("zone %s: %v", storeID.Hex(), err)
	}
}

// ── one "closes at" per store per day at 21:45, one "now closed" at 22:00 ──

func TestInstantAlertClosingAndClosedOncePerStorePerDay(t *testing.T) {
	w, done, storeB, mgrB := ihAlertWorld(t)
	defer done()
	ctx := context.Background()
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000}) // store A, 07:00-22:00 as shown
	ihZoneFor(t, w, storeB, zone{StandardRadiusM: 8000})            // store B: no instant radius

	// A second manager on store A gets the alert too; the rider does not.
	mgrA2 := primitive.NewObjectID()
	if _, err := w.db.Collection("parties").InsertOne(ctx, bson.D{{Key: "_id", Value: mgrA2}, {Key: "phone", Value: "+919900000078"}}); err != nil {
		t.Fatalf("manager A2: %v", err)
	}
	if _, err := w.db.Collection("role_assignments").InsertOne(ctx, bson.D{
		{Key: "party_id", Value: mgrA2}, {Key: "role_code", Value: "STORE_MANAGER"},
		{Key: "status", Value: "ACTIVE"}, {Key: "org_unit_id", Value: w.storeID},
	}); err != nil {
		t.Fatalf("manager A2 role: %v", err)
	}

	w.svc.instantAlertsTick(ctx, ihAt(21, 44))
	if n := len(ihNotes(t, w, w.mgr.PartyID)); n != 0 {
		t.Fatalf("21:44 is too early: %d alerts", n)
	}
	for _, m := range []int{45, 46, 50, 59} {
		w.svc.instantAlertsTick(ctx, ihAt(21, m))
	}
	for _, mgr := range []string{w.mgr.PartyID, mgrA2.Hex()} {
		notes := ihNotes(t, w, mgr)
		if len(notes) != 1 || notes[0].TemplateKey != templateStoreInstantClosing {
			t.Fatalf("manager %s after 21:45-21:59: %+v", mgr, notes)
		}
		n := notes[0]
		if n.Params["headline"] != "Instant delivery closes at 10:00 PM" || n.Params["message"] != ihClosingMessage {
			t.Fatalf("closing copy: %q / %q", n.Params["headline"], n.Params["message"])
		}
		if n.Params["store_id"] != w.storeID.Hex() || n.Params["store"] != "PYAAS Full-Chain Store" || n.Params["window_close"] != ihUTC(ihAt(22, 0)) {
			t.Fatalf("closing params: %v", n.Params)
		}
		// The STORE_LOW_STOCK model: in-app channel, queued, unread, Hindi default.
		if n.Channel != "APP" || n.Status != "QUEUED" || n.ReadAt != nil || n.Language != "hi" || !n.QueuedAt.Equal(ihAt(21, 45)) {
			t.Fatalf("closing note shape: %+v", n)
		}
	}
	if phone := ihNotes(t, w, mgrA2.Hex())[0].Phone; phone != "+919900000078" {
		t.Fatalf("recipient phone: %q", phone)
	}

	for _, m := range []int{0, 1, 30} {
		w.svc.instantAlertsTick(ctx, ihAt(22, m))
	}
	notes := ihNotes(t, w, w.mgr.PartyID)
	if len(notes) != 2 || notes[1].TemplateKey != templateStoreInstantClosed {
		t.Fatalf("after 22:00-22:30: %+v", notes)
	}
	c := notes[1]
	if c.Params["headline"] != "Instant delivery is now closed" || c.Params["message"] != "It reopens at 7:00 AM." ||
		c.Params["window_close"] != ihUTC(ihAt(22, 0)) || c.Params["window_reopen"] != ihUTC(ihAt(24+7, 0)) {
		t.Fatalf("closed copy: %v", c.Params)
	}
	// Nothing late or doubled overnight; nothing for anyone else.
	for _, at := range []time.Time{ihAt(23, 5), ihAt(24+3, 0), ihAt(24+7, 0), ihAt(24+12, 0)} {
		w.svc.instantAlertsTick(ctx, at)
	}
	if n := len(ihNotes(t, w, w.mgr.PartyID)); n != 2 {
		t.Fatalf("overnight ticks added alerts: %d", n)
	}
	if n := len(ihNotes(t, w, mgrB.PartyID)); n != 0 {
		t.Fatalf("store B (no instant radius) manager got %d alerts", n)
	}
	if n := len(ihNotes(t, w, w.rider.PartyID)); n != 0 {
		t.Fatalf("the rider got %d alerts", n)
	}
	// The next evening is a new day: one more of each.
	for _, at := range []time.Time{ihAt(24+21, 45), ihAt(24+21, 50), ihAt(24+22, 0), ihAt(24+22, 5)} {
		w.svc.instantAlertsTick(ctx, at)
	}
	if a, b := ihCount(t, w, w.mgr.PartyID, templateStoreInstantClosing), ihCount(t, w, w.mgr.PartyID, templateStoreInstantClosed); a != 2 || b != 2 {
		t.Fatalf("second day: %d closing, %d closed; want 2 and 2", a, b)
	}
}

// ── an extension gets its own "closes at", and the close moves with it ────

func TestInstantAlertAfterAnExtension(t *testing.T) {
	w, done, _, _ := ihAlertWorld(t)
	defer done()
	ctx := context.Background()
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	store := w.storeID.Hex()

	w.svc.instantAlertsTick(ctx, ihAt(21, 45))
	ihClock(w, ihAt(21, 50))
	if code, v := ihOp(t, w, w.mgr, store, "extend", 60); code != 200 {
		t.Fatalf("extend: %d %v", code, v)
	}
	for _, m := range []int{0, 1, 30, 44} {
		w.svc.instantAlertsTick(ctx, ihAt(22, m))
	}
	if a, b := ihCount(t, w, w.mgr.PartyID, templateStoreInstantClosing), ihCount(t, w, w.mgr.PartyID, templateStoreInstantClosed); a != 1 || b != 0 {
		t.Fatalf("extended past 22:00: %d closing, %d closed; want 1 and 0 (no 'now closed' while extended)", a, b)
	}
	w.svc.instantAlertsTick(ctx, ihAt(22, 45))
	w.svc.instantAlertsTick(ctx, ihAt(22, 50))
	notes := ihNotes(t, w, w.mgr.PartyID)
	if len(notes) != 2 || notes[1].TemplateKey != templateStoreInstantClosing ||
		notes[1].Params["headline"] != "Instant delivery closes at 11:00 PM" || notes[1].Params["window_close"] != ihUTC(ihAt(23, 0)) {
		t.Fatalf("second 'closes at' before the extended close: %+v", notes)
	}
	w.svc.instantAlertsTick(ctx, ihAt(23, 0))
	w.svc.instantAlertsTick(ctx, ihAt(23, 1))
	notes = ihNotes(t, w, w.mgr.PartyID)
	if len(notes) != 3 || notes[2].TemplateKey != templateStoreInstantClosed || notes[2].Params["window_close"] != ihUTC(ihAt(23, 0)) ||
		notes[2].Params["message"] != "It reopens at 7:00 AM." {
		t.Fatalf("'now closed' at the extended close: %+v", notes)
	}
}

// ── never for a paused store, no instant radius, an inactive zone, a lane
//    the manager closed themselves, or a lane open round the clock ─────────

func TestInstantAlertNeverForAStoreThatIsNotClosingByTheClock(t *testing.T) {
	w, done, _, _ := ihAlertWorld(t)
	defer done()
	ctx := context.Background()
	store := w.storeID.Hex()
	evening := []time.Time{ihAt(21, 45), ihAt(21, 50), ihAt(22, 0), ihAt(22, 10)}
	none := func(what string) {
		t.Helper()
		if n := len(ihNotes(t, w, w.mgr.PartyID)); n != 0 {
			t.Fatalf("%s: %d alerts, want none", what, n)
		}
	}

	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantPaused: true})
	for _, at := range evening {
		w.svc.instantAlertsTick(ctx, at)
	}
	none("paused")

	ihZone(t, w, zone{StandardRadiusM: 8000})
	for _, at := range evening {
		w.svc.instantAlertsTick(ctx, at.AddDate(0, 0, 1))
	}
	none("no instant radius")

	z := zone{StoreID: store, Active: false, Center: newGeoPoint(guardCenter.Lat, guardCenter.Lng), InstantRadiusM: 2500, StandardRadiusM: 8000}
	if _, err := w.svc.repo.upsertZone(ctx, &z); err != nil {
		t.Fatalf("inactive zone: %v", err)
	}
	for _, at := range evening {
		w.svc.instantAlertsTick(ctx, at.AddDate(0, 0, 2))
	}
	none("inactive zone")

	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	ihClock(w, ihAt(72+21, 30))
	if code, v := ihOp(t, w, w.mgr, store, "close-now", 0); code != 200 {
		t.Fatalf("close-now: %d %v", code, v)
	}
	for _, at := range evening {
		w.svc.instantAlertsTick(ctx, at.AddDate(0, 0, 3))
	}
	none("closed by the manager")

	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantOpenMin: 0, InstantCloseMin: 1440})
	for _, at := range []time.Time{ihAt(96+21, 45), ihAt(96+23, 45), ihAt(120, 0), ihAt(120, 5)} {
		w.svc.instantAlertsTick(ctx, at)
	}
	none("round the clock")
}

// ── the closing alert offers an extension only when one is possible ───────
//
// Before: STORE_INSTANT_CLOSING always said "extend by 30 min, 1 h or 2 h",
// also for a close at or past the 02:00 cap (an 18:00-02:00 window, a lane
// already extended to 02:00, a 22:00-06:00 window), where extend answers 422
// EXTEND_TOO_LATE.

const ihNoExtendMessage = "It cannot be extended any further. Orders already placed are not affected."

func TestInstantAlertOffersAnExtensionOnlyWhenOneIsPossible(t *testing.T) {
	w, done, _, _ := ihAlertWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ctx := context.Background()
	store := w.storeID.Hex()
	latest := func(at time.Time, headline, message string) {
		t.Helper()
		w.svc.instantAlertsTick(ctx, at)
		notes := ihNotes(t, w, w.mgr.PartyID)
		if len(notes) == 0 {
			t.Fatalf("%s: no alert", at.In(istZone).Format("01-02 15:04"))
		}
		n := notes[len(notes)-1]
		if n.TemplateKey != templateStoreInstantClosing || n.Params["headline"] != headline || n.Params["message"] != message {
			t.Fatalf("%s: %s %q / %q, want %q / %q", at.In(istZone).Format("01-02 15:04"), n.TemplateKey, n.Params["headline"], n.Params["message"], headline, message)
		}
	}

	// 18:00-02:00 closes at the cap: no extension is offered, and none is possible.
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantOpenMin: 1080, InstantCloseMin: 120})
	latest(ihAt(24+1, 45), "Instant delivery closes at 2:00 AM", ihNoExtendMessage)
	ihClock(w, ihAt(24+1, 50))
	if code, e := ihOp(t, w, w.mgr, store, "extend", 30); code != 422 || e["code"] != "EXTEND_TOO_LATE" {
		t.Fatalf("extend at 01:50 in an 18:00-02:00 window: %d %v", code, e)
	}

	// 22:00-06:00 closes past the cap.
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantOpenMin: 1320, InstantCloseMin: 360})
	latest(ihAt(48+5, 45), "Instant delivery closes at 6:00 AM", ihNoExtendMessage)

	// 07:00-22:00 already extended to 02:00: the alert before 02:00 offers none.
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	ihClock(w, ihAt(72+21, 50))
	for _, want := range []time.Time{ihAt(96, 0), ihAt(96+2, 0)} {
		if code, v := ihOp(t, w, w.mgr, store, "extend", 120); code != 200 || v["instantExtendedUntil"] != ihUTC(want) {
			t.Fatalf("extend 120: %d %v, want %s", code, v["instantExtendedUntil"], ihUTC(want))
		}
	}
	latest(ihAt(96+1, 45), "Instant delivery closes at 2:00 AM", ihNoExtendMessage)

	// Unchanged where an extension is possible: 22:00, and an extended close before 02:00.
	latest(ihAt(120+21, 45), "Instant delivery closes at 10:00 PM", ihClosingMessage)
	ihClock(w, ihAt(120+21, 50))
	if code, v := ihOp(t, w, w.mgr, store, "extend", 60); code != 200 {
		t.Fatalf("extend 60: %d %v", code, v)
	}
	latest(ihAt(120+22, 45), "Instant delivery closes at 11:00 PM", ihClosingMessage)
}

// ── two instances ticking at once, then a restart: still exactly one ──────

func TestInstantAlertIdempotentAcrossConcurrentTicksAndARestart(t *testing.T) {
	w, done, _, _ := ihAlertWorld(t)
	defer done()
	ctx := context.Background()
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})

	race := func(at time.Time) {
		// Two instances (separate services over the same database) tick at the
		// same moment.
		other := newService(w.svc.deps, w.svc.repo, w.svc.log)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, s := range []*service{w.svc, other, w.svc, other} {
			wg.Add(1)
			go func(s *service) {
				defer wg.Done()
				<-start
				s.instantAlertsTick(ctx, at)
			}(s)
		}
		close(start)
		wg.Wait()
	}
	race(ihAt(21, 45))
	// A restart: a brand-new service with no memory of the last tick.
	newService(w.svc.deps, w.svc.repo, w.svc.log).instantAlertsTick(ctx, ihAt(21, 46))
	if n := ihCount(t, w, w.mgr.PartyID, templateStoreInstantClosing); n != 1 {
		t.Fatalf("closing alerts after concurrent ticks + restart: %d, want 1", n)
	}
	race(ihAt(22, 0))
	newService(w.svc.deps, w.svc.repo, w.svc.log).instantAlertsTick(ctx, ihAt(22, 2))
	if n := ihCount(t, w, w.mgr.PartyID, templateStoreInstantClosed); n != 1 {
		t.Fatalf("closed alerts after concurrent ticks + restart: %d, want 1", n)
	}
	if n, _ := w.db.Collection(collStoreInstantAlerts).CountDocuments(ctx, bson.D{}); n != 2 {
		t.Fatalf("alert claims: %d, want 2 (one per kind for tonight's close)", n)
	}
}

// ── each store's alert reaches that store's managers only ─────────────────

func TestInstantAlertReachesOnlyTheStoresOwnManagers(t *testing.T) {
	w, done, storeB, mgrB := ihAlertWorld(t)
	defer done()
	ctx := context.Background()
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})                                                        // A closes 22:00
	ihZoneFor(t, w, storeB, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantOpenMin: 480, InstantCloseMin: 1380}) // B closes 23:00

	w.svc.instantAlertsTick(ctx, ihAt(21, 45))
	if a, b := len(ihNotes(t, w, w.mgr.PartyID)), len(ihNotes(t, w, mgrB.PartyID)); a != 1 || b != 0 {
		t.Fatalf("21:45: manager A %d, manager B %d; want 1 and 0", a, b)
	}
	w.svc.instantAlertsTick(ctx, ihAt(22, 0))
	w.svc.instantAlertsTick(ctx, ihAt(22, 45))
	bNotes := ihNotes(t, w, mgrB.PartyID)
	if len(bNotes) != 1 || bNotes[0].Params["store_id"] != storeB.Hex() || bNotes[0].Params["store"] != "PYAAS Second Store" ||
		bNotes[0].Params["headline"] != "Instant delivery closes at 11:00 PM" {
		t.Fatalf("manager B at 22:45: %+v", bNotes)
	}
	w.svc.instantAlertsTick(ctx, ihAt(23, 0))
	if a := len(ihNotes(t, w, w.mgr.PartyID)); a != 2 {
		t.Fatalf("manager A got store B's alerts: %d", a)
	}
	for _, n := range ihNotes(t, w, w.mgr.PartyID) {
		if n.Params["store_id"] != w.storeID.Hex() {
			t.Fatalf("manager A holds another store's alert: %v", n.Params)
		}
	}
	if b := ihNotes(t, w, mgrB.PartyID); len(b) != 2 || b[1].TemplateKey != templateStoreInstantClosed || b[1].Params["message"] != "It reopens at 8:00 AM." {
		t.Fatalf("manager B after 23:00: %+v", b)
	}
}
