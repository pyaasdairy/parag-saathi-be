package consumer

// The phase-2 frontend contracts, proven against a real Mongo.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run Phase2 -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// clearAddresses drops the fixture address newChainWorld's customer() creates,
// so a test reads only the row it staged itself.
func clearAddresses(t *testing.T, w *chainWorld, cid primitive.ObjectID) {
	t.Helper()
	if _, err := w.db.Collection(collAddresses).DeleteMany(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}}); err != nil {
		t.Fatalf("clear addresses: %v", err)
	}
}

// ── Structured address ─────────────────────────────────────────────────────

// The society dropdowns capture a machine-groupable door. Before this change
// the backend dropped all five fields, and the app then hydrated its own local
// copy back to null from the response — so the captured door was destroyed.
func TestPhase2StructuredAddressRoundTrips(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000004001", 0)
	clearAddresses(t, w, cid)
	lat, lng := 26.7712, 81.0123
	floor := 8
	created, err := w.svc.createAddress(ctx, cid, addressInput{
		Label: "Home", Line1: "P4-805", Line2: "8th floor, Chandra Panorama, Sushant Golf City",
		City: "Lucknow", Pincode: "226030", Lat: &lat, Lng: &lng,
		Society: "Chandra Panorama", SocietyID: "chandra-panorama",
		Tower: "P4", Floor: &floor, Unit: "805",
	})
	if err != nil {
		t.Fatalf("createAddress: %v", err)
	}
	if created.SocietyID != "chandra-panorama" || created.Tower != "P4" || created.Unit != "805" {
		t.Fatalf("structured parts dropped on create: %+v", created)
	}
	if created.Floor == nil || *created.Floor != 8 {
		t.Fatalf("floor not stored: %+v", created.Floor)
	}

	// They must survive the round trip — this is the exact read the app uses to
	// overwrite its local row, so anything missing here erases the member's data.
	list, err := w.svc.repo.listAddresses(ctx, cid)
	if err != nil || len(list) != 1 {
		t.Fatalf("listAddresses: %v (%d rows)", err, len(list))
	}
	got := list[0]
	if got.Society != "Chandra Panorama" || got.SocietyID != "chandra-panorama" ||
		got.Tower != "P4" || got.Unit != "805" || got.Floor == nil || *got.Floor != 8 {
		t.Fatalf("structured parts lost in the round trip: %+v", got)
	}
}

// GROUND FLOOR IS 0, NOT "UNSET" — the reason Floor is a pointer. A member on
// the ground floor must not read back as a member with no floor.
func TestPhase2GroundFloorIsNotUnset(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000004002", 0)
	clearAddresses(t, w, cid)
	ground := 0
	if _, err := w.svc.createAddress(ctx, cid, addressInput{
		Label: "Home", Line1: "P1-001", City: "Lucknow", Pincode: "226030",
		Society: "Chandra Panorama", SocietyID: "chandra-panorama",
		Tower: "P1", Floor: &ground, Unit: "001",
	}); err != nil {
		t.Fatalf("createAddress: %v", err)
	}
	list, _ := w.svc.repo.listAddresses(ctx, cid)
	if len(list) != 1 || list[0].Floor == nil {
		t.Fatal("ground floor (0) came back as 'no floor' — a pointer is required")
	}
	if *list[0].Floor != 0 {
		t.Fatalf("floor: %d want 0", *list[0].Floor)
	}
}

// A typed address carries none of it and must stay clean — no empty strings,
// no zero floor masquerading as the ground floor.
func TestPhase2TypedAddressStaysUnstructured(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000004003", 0)
	clearAddresses(t, w, cid)
	a, err := w.svc.createAddress(ctx, cid, addressInput{
		Label: "Home", Line1: "12, Some Lane", City: "Lucknow", Pincode: "226030",
	})
	if err != nil {
		t.Fatalf("createAddress: %v", err)
	}
	if a.SocietyID != "" || a.Tower != "" || a.Unit != "" || a.Floor != nil {
		t.Fatalf("a typed address invented structure: %+v", a)
	}
}

// The rider's task must carry the door so a route can be grouped by
// (societyId, tower, floor) — one lift ride, one stop.
func TestPhase2DeliveryTaskCarriesTheDoor(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000004004", 500)
	floor := 8
	// Replace the fixture address with a society one.
	if _, err := w.db.Collection(collAddresses).DeleteMany(ctx,
		bson.D{{Key: "consumer_id", Value: cid}}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	lat, lng := 26.7712, 81.0123
	if _, err := w.svc.createAddress(ctx, cid, addressInput{
		Label: "Home", Line1: "P4-805", Line2: "8th floor, Chandra Panorama",
		City: "Lucknow", Pincode: "226030", Lat: &lat, Lng: &lng, IsDefault: true,
		Society: "Chandra Panorama", SocietyID: "chandra-panorama",
		Tower: "P4", Floor: &floor, Unit: "805",
	}); err != nil {
		t.Fatalf("address: %v", err)
	}

	// The app composes address_text as [line1, line2, city, pincode] joined.
	addrText := "P4-805, 8th floor, Chandra Panorama, Lucknow, 226030"
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: addrText,
		Lane: "morning", ConsumerName: "Society Tester", Phone: "9000004004",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}

	queue, err := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	if err != nil {
		t.Fatalf("storeOrders: %v", err)
	}
	var task *delivery
	for i := range queue {
		if queue[i].OrderID == ord.OrderID {
			task = &queue[i]
		}
	}
	if task == nil {
		t.Fatal("order not in the store queue")
	}
	if task.SocietyID != "chandra-panorama" || task.Tower != "P4" || task.Unit != "805" {
		t.Fatalf("the rider's task lost the door: society=%q tower=%q unit=%q",
			task.SocietyID, task.Tower, task.Unit)
	}
	if task.Floor == nil || *task.Floor != 8 {
		t.Fatalf("task floor: %v want 8", task.Floor)
	}
}

// A door we cannot resolve with CERTAINTY must yield nothing — mis-grouping a
// route is worse than not grouping it.
func TestPhase2AmbiguousAddressYieldsNoDoor(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000004005", 500)
	if _, err := w.db.Collection(collAddresses).DeleteMany(ctx,
		bson.D{{Key: "consumer_id", Value: cid}}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	lat, lng := 26.7712, 81.0123
	floor := 3
	// TWO saved addresses that compose to the SAME text, different towers.
	for _, tower := range []string{"P1", "P2"} {
		if _, err := w.svc.createAddress(ctx, cid, addressInput{
			Label: "Home", Line1: "Same Line", City: "Lucknow", Pincode: "226030",
			Lat: &lat, Lng: &lng,
			Society: "Chandra Panorama", SocietyID: "chandra-panorama",
			Tower: tower, Floor: &floor, Unit: "301",
		}); err != nil {
			t.Fatalf("address %s: %v", tower, err)
		}
	}
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home",
		AddressText: "Same Line, Lucknow, 226030",
		Lane:        "morning", ConsumerName: "Ambiguous", Phone: "9000004005",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	queue, _ := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	for i := range queue {
		if queue[i].OrderID == ord.OrderID && queue[i].Tower != "" {
			t.Fatalf("guessed a tower from an ambiguous address: %q", queue[i].Tower)
		}
	}
}

// ── Complaints ─────────────────────────────────────────────────────────────

// The app retries offline rows on every refresh, so the SAME reference must
// never become two complaints in the support queue.
func TestPhase2ComplaintIsIdempotentByRef(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000004006", 0)

	in := complaintInput{Ref: "PYS-4K2Q9", Category: "late", Detail: "Milk arrived at 9am"}
	first, err := w.svc.fileComplaint(ctx, cid, in)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if first.Status != "open" {
		t.Fatalf("a filed complaint should be open, got %q", first.Status)
	}

	// An operator answers it.
	if _, err := w.svc.complaintsCol().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: first.ID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: "resolved"},
			{Key: "resolution", Value: "We refunded the delivery fee."},
		}}}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The app retries the same ref three times.
	for i := 0; i < 3; i++ {
		again, err := w.svc.fileComplaint(ctx, cid, in)
		if err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		// It must hand back the LIVE row, not a fresh open one — otherwise a
		// retry would wipe the answer the member is waiting to read.
		if again.Status != "resolved" || again.Resolution == "" {
			t.Fatalf("retry returned a fresh row, losing the resolution: %+v", again)
		}
	}
	list, err := w.svc.listComplaints(ctx, cid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("DUPLICATE COMPLAINTS: %d rows for one reference", len(list))
	}
}

// Two different members may legitimately generate the same reference.
func TestPhase2ComplaintRefsAreScopedPerMember(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	a := w.customer(t, "9000004007", 0)
	b := w.customer(t, "9000004008", 0)

	in := complaintInput{Ref: "PYS-SAME1", Category: "quality", Detail: "Sour milk"}
	if _, err := w.svc.fileComplaint(ctx, a, in); err != nil {
		t.Fatalf("member A: %v", err)
	}
	if _, err := w.svc.fileComplaint(ctx, b, in); err != nil {
		t.Fatalf("member B blocked by member A's reference: %v", err)
	}
	for _, cid := range []primitive.ObjectID{a, b} {
		l, _ := w.svc.listComplaints(ctx, cid)
		if len(l) != 1 {
			t.Fatalf("each member should see exactly their own: got %d", len(l))
		}
	}
}

func TestPhase2ComplaintValidation(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000004009", 0)

	if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: "PYS-1", Category: "aliens", Detail: "x"}); err == nil {
		t.Fatal("an unknown category must be refused, not stored")
	}
	if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: "PYS-2", Category: "late", Detail: "   "}); err == nil {
		t.Fatal("an empty detail must be refused")
	}
	if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: "", Category: "late", Detail: "x"}); err == nil {
		t.Fatal("a missing reference must be refused — it is what support searches by")
	}
	// A blank category defaults rather than failing: the member already typed
	// a complaint and must not lose it to a dropdown they skipped.
	c, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: "PYS-3", Detail: "Something went wrong"})
	if err != nil || c.Category != "other" {
		t.Fatalf("blank category should default to other: %v %+v", err, c)
	}
}

// ── Push devices ───────────────────────────────────────────────────────────

func TestPhase2PushRegisterIsIdempotent(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000004010", 0)

	in := pushRegisterInput{Token: "ExponentPushToken[abc123]", Platform: "android", Provider: "expo"}
	for i := 0; i < 4; i++ {
		if err := w.svc.registerPushDevice(ctx, cid, in); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	devices, err := w.svc.pushDevicesFor(ctx, cid)
	if err != nil {
		t.Fatalf("devices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("one device registered %d times became %d rows", 4, len(devices))
	}
	if devices[0].Platform != "android" || devices[0].Provider != "expo" {
		t.Fatalf("device stored wrong: %+v", devices[0])
	}
}

// A shared handset: one member signs out, another signs in. The device must
// follow the CURRENT member, or one member's push lands on another's phone.
func TestPhase2PushTokenFollowsTheCurrentMember(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	a := w.customer(t, "9000004011", 0)
	b := w.customer(t, "9000004012", 0)

	in := pushRegisterInput{Token: "ExponentPushToken[shared]", Platform: "ios"}
	if err := w.svc.registerPushDevice(ctx, a, in); err != nil {
		t.Fatalf("A: %v", err)
	}
	if err := w.svc.registerPushDevice(ctx, b, in); err != nil {
		t.Fatalf("B: %v", err)
	}
	da, _ := w.svc.pushDevicesFor(ctx, a)
	db, _ := w.svc.pushDevicesFor(ctx, b)
	if len(da) != 0 {
		t.Fatalf("the previous member still owns the handset: %d devices", len(da))
	}
	if len(db) != 1 {
		t.Fatalf("the current member does not own the handset: %d devices", len(db))
	}
}

func TestPhase2PushValidation(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000004013", 0)

	if err := w.svc.registerPushDevice(ctx, cid, pushRegisterInput{Token: "", Platform: "ios"}); err == nil {
		t.Fatal("an empty token must be refused")
	}
	if err := w.svc.registerPushDevice(ctx, cid, pushRegisterInput{Token: "t", Platform: "windows"}); err == nil {
		t.Fatal("an unknown platform must be refused")
	}
	// Provider defaults to expo — the app may omit it.
	if err := w.svc.registerPushDevice(ctx, cid, pushRegisterInput{Token: "tok-default", Platform: "ios"}); err != nil {
		t.Fatalf("provider should default: %v", err)
	}
	d, _ := w.svc.pushDevicesFor(ctx, cid)
	if len(d) != 1 || d[0].Provider != "expo" {
		t.Fatalf("provider default: %+v", d)
	}
	_ = time.Now
}

// ── Fixes from the adversarial review ──────────────────────────────────────

// DUPLICATE ROWS, ONE DOOR. The app re-POSTs an address when it loses the
// response, so two rows composing to the same text is ordinary. Refusing them
// would throw the grouping away for exactly the members who retried — so
// ambiguity is judged by the DOOR, not by the row count.
func TestPhase2DuplicateRowsSameDoorStillGroup(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000005001", 500)
	clearAddresses(t, w, cid)
	lat, lng := 26.7712, 81.0123
	floor := 8
	for i := 0; i < 2; i++ { // the same door, saved twice by a lost response
		if _, err := w.svc.createAddress(ctx, cid, addressInput{
			Label: "Home", Line1: "P4-805", City: "Lucknow", Pincode: "226030",
			Lat: &lat, Lng: &lng, IsDefault: true,
			Society: "Chandra Panorama", SocietyID: "chandra-panorama",
			Tower: "P4", Floor: &floor, Unit: "805",
		}); err != nil {
			t.Fatalf("address %d: %v", i, err)
		}
	}
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home",
		AddressText: "P4-805, Lucknow, 226030",
		Lane:        "morning", ConsumerName: "Dup", Phone: "9000005001",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	queue, _ := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	for i := range queue {
		if queue[i].OrderID == ord.OrderID {
			if queue[i].Tower != "P4" || queue[i].Unit != "805" {
				t.Fatalf("identical duplicates lost the door: tower=%q unit=%q",
					queue[i].Tower, queue[i].Unit)
			}
			return
		}
	}
	t.Fatal("order not in the queue")
}

// sameDoor must treat "ground floor" and "no floor" as DIFFERENT answers.
func TestPhase2SameDoorTreatsNilFloorAsDistinct(t *testing.T) {
	zero, eight := 0, 8
	base := func(f *int) *address {
		return &address{SocietyID: "s", Tower: "P1", Unit: "101", Floor: f}
	}
	if !sameDoor(base(nil), base(nil)) {
		t.Fatal("two unfloored doors are the same door")
	}
	if sameDoor(base(nil), base(&zero)) {
		t.Fatal("no floor and ground floor must not be treated as equal")
	}
	if !sameDoor(base(&zero), base(&zero)) {
		t.Fatal("two ground floors are the same door")
	}
	if sameDoor(base(&zero), base(&eight)) {
		t.Fatal("different floors are different doors")
	}
}

// DPDP: erasure must take the phase-2 PII with the account — a complaint is
// free text the member wrote, and a push device is a live handle to their phone.
func TestPhase2ErasureTakesComplaintsAndDevices(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000005002", 0)
	if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{
		Ref: "PYS-ERASE1", Category: "quality", Detail: "Erasure probe",
	}); err != nil {
		t.Fatalf("file: %v", err)
	}
	if err := w.svc.registerPushDevice(ctx, cid, pushRegisterInput{
		Token: "ExponentPushToken[erase-probe]", Platform: "ios",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := w.svc.repo.deleteAccountCascade(ctx, cid); err != nil {
		t.Fatalf("erase: %v", err)
	}

	if n, _ := w.svc.complaintsCol().CountDocuments(ctx,
		bson.D{{Key: "consumer_id", Value: cid}}); n != 0 {
		t.Fatalf("erased account left %d complaints behind", n)
	}
	if n, _ := w.svc.pushCol().CountDocuments(ctx,
		bson.D{{Key: "consumer_id", Value: cid}}); n != 0 {
		t.Fatalf("erased account left %d push devices behind — a later sender would push to a stranger", n)
	}
}

// The register must be answerable: support moves the status and writes the
// resolution the member reads.
func TestPhase2ComplaintCanBeAnswered(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000005003", 0)
	if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{
		Ref: "PYS-ANS01", Category: "missing", Detail: "One pack short",
	}); err != nil {
		t.Fatalf("file: %v", err)
	}
	upd, err := w.svc.updateComplaint(ctx, "PYS-ANS01", complaintUpdateInput{
		Status: "resolved", Resolution: "We refunded the missing pack to your wallet.",
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if upd.Status != "resolved" || upd.Resolution == "" {
		t.Fatalf("answer did not stick: %+v", upd)
	}
	// The member sees it on their own list.
	mine, _ := w.svc.listComplaints(ctx, cid)
	if len(mine) != 1 || mine[0].Resolution == "" {
		t.Fatalf("the member cannot see the resolution: %+v", mine)
	}
	// Unknown status is refused rather than stored.
	if _, err := w.svc.updateComplaint(ctx, "PYS-ANS01", complaintUpdateInput{Status: "banished"}); err == nil {
		t.Fatal("an unknown status must be refused")
	}
	// An empty update is refused rather than silently bumping updated_at.
	if _, err := w.svc.updateComplaint(ctx, "PYS-ANS01", complaintUpdateInput{}); err == nil {
		t.Fatal("an empty update must be refused")
	}
	// A reference nobody filed is a 404, not a silent no-op.
	if _, err := w.svc.updateComplaint(ctx, "PYS-NOPE", complaintUpdateInput{Status: "closed"}); err == nil {
		t.Fatal("an unknown reference must be refused")
	}
}

// The support queue can be read and filtered.
func TestPhase2SupportQueue(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	a := w.customer(t, "9000005004", 0)
	b := w.customer(t, "9000005005", 0)
	_, _ = w.svc.fileComplaint(ctx, a, complaintInput{Ref: "PYS-Q1", Category: "late", Detail: "late"})
	_, _ = w.svc.fileComplaint(ctx, b, complaintInput{Ref: "PYS-Q2", Category: "rider", Detail: "rude"})
	if _, err := w.svc.updateComplaint(ctx, "PYS-Q2", complaintUpdateInput{Status: "closed"}); err != nil {
		t.Fatalf("close: %v", err)
	}
	all, err := w.svc.listAllComplaints(ctx, "")
	if err != nil || len(all) < 2 {
		t.Fatalf("queue should hold both members' rows: %v (%d)", err, len(all))
	}
	open, err := w.svc.listAllComplaints(ctx, "open")
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	for _, c := range open {
		if c.Status != "open" {
			t.Fatalf("filter leaked a %q row", c.Status)
		}
	}
	if _, err := w.svc.listAllComplaints(ctx, "nonsense"); err == nil {
		t.Fatal("an unknown status filter must be refused")
	}
}

// Complaint idempotency must survive the index being ABSENT — its build is
// non-fatal at boot, so a failed build must degrade nothing that matters.
func TestPhase2ComplaintIdempotentWithoutTheIndex(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	// Drop the guard the production boot may have failed to create.
	if _, err := w.svc.complaintsCol().Indexes().DropAll(ctx); err != nil {
		t.Fatalf("drop indexes: %v", err)
	}
	cid := w.customer(t, "9000005006", 0)
	in := complaintInput{Ref: "PYS-NOIDX", Category: "app", Detail: "no index here"}
	first, err := w.svc.fileComplaint(ctx, cid, in)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := w.svc.fileComplaint(ctx, cid, in)
		if err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		if again.ID != first.ID {
			t.Fatal("a retry filed a DUPLICATE with the index absent")
		}
	}
	list, _ := w.svc.listComplaints(ctx, cid)
	if len(list) != 1 {
		t.Fatalf("index absent: one reference became %d rows", len(list))
	}
}
