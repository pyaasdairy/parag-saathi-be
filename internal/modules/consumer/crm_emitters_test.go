package consumer

// Every product event the audit found without an emitter now reaches the
// outbox from its real choke point, with the payload keys the config's
// conditions and templates read, and the message that rides it lands in the
// inbox.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMEmit -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// inboxBodyEN reads the EN body of a consumer's newest inbox row for a trigger.
func inboxBodyEN(t *testing.T, db *mongo.Database, cid primitive.ObjectID, trigger string) string {
	t.Helper()
	var row struct {
		BodyEN string `bson:"body_en"`
	}
	if err := db.Collection(collConsumerInbox).FindOne(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: trigger}}).Decode(&row); err != nil {
		t.Fatalf("%s inbox row: %v", trigger, err)
	}
	return row.BodyEN
}

func TestCRMEmitUserRegistered(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	const phone = "9000007401"
	w.svc.deps.Cfg.OTPTTL = 5 * time.Minute // the chain world sets no OTP lifetime
	code, _, err := w.svc.requestOTP(ctx, phone)
	if err != nil || code == "" {
		t.Fatalf("requestOTP: %q %v", code, err)
	}
	if _, err := w.svc.verifyOTP(ctx, phone, code); err != nil {
		t.Fatalf("verifyOTP: %v", err)
	}
	acct, err := w.svc.repo.findAccountByPhone(ctx, "+91"+phone)
	if err != nil || acct == nil {
		t.Fatalf("account: %v", err)
	}
	if evs := crmEventsOf(t, w.db, acct.ID, "user.registered"); len(evs) != 1 || evs[0].Payload["source"] != "otp" {
		t.Fatalf("user.registered on the creating verify: %+v", evs)
	}
	// A re-login of the same shopper registers nothing.
	code, _, _ = w.svc.requestOTP(ctx, phone)
	if _, err := w.svc.verifyOTP(ctx, phone, code); err != nil {
		t.Fatalf("second verifyOTP: %v", err)
	}
	if evs := crmEventsOf(t, w.db, acct.ID, "user.registered"); len(evs) != 1 {
		t.Fatalf("a re-login must not register again: %d events", len(evs))
	}
	// The Play review account is provisioned, never registered.
	if _, err := w.svc.verifyOTP(ctx, reviewPhone, reviewOTP); err != nil {
		t.Fatalf("review verify: %v", err)
	}
	rev, _ := w.svc.repo.findAccountByPhone(ctx, "+91"+reviewPhone)
	if rev == nil {
		t.Fatal("review account missing")
	}
	if evs := crmEventsOf(t, w.db, rev.ID, "user.registered"); len(evs) != 0 {
		t.Fatalf("the review account must not emit user.registered: %+v", evs)
	}
	// A-01 is queued (PT2H), not sent on the draining tick.
	w.svc.crmProcessEvents(ctx)
	if rows := crmScheduleRows(t, w.db, acct.ID, "A-01"); len(rows) != 1 {
		t.Fatalf("A-01 schedule rows = %d", len(rows))
	}
}

func TestCRMEmitWalletCredited(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007402", 0)

	// Top-up: the refundable account, worded as a recharge, one per ledger ref.
	if _, err := w.svc.creditTopup(ctx, cid, 500, "razorpay", "order_t1"); err != nil {
		t.Fatalf("creditTopup: %v", err)
	}
	if _, err := w.svc.creditTopup(ctx, cid, 500, "razorpay", "order_t1"); err != nil { // replayed webhook
		t.Fatalf("creditTopup replay: %v", err)
	}
	evs := crmEventsOf(t, w.db, cid, "wallet.credited")
	if len(evs) != 1 || evs[0].Payload["account"] != "topup" || evs[0].Payload["reason"] != "recharge" || evs[0].Payload["scope_key"] != "order_t1" {
		t.Fatalf("wallet.credited on top-up: %+v", evs)
	}
	w.svc.crmProcessEvents(ctx)
	if got := crmDispatchScopes(t, w.db, cid, "B-06"); got["order_t1"] != "SENT" {
		t.Fatalf("B-06 on the top-up: %v", got)
	}
	if body := inboxBodyEN(t, w.db, cid, "B-06"); !strings.Contains(body, "500 added to your Wallet (recharge). Refundable.") {
		t.Fatalf("B-06 top-up body = %q", body)
	}
	// A second top-up the same day is a second event and a second message.
	if _, err := w.svc.creditTopup(ctx, cid, 300, "razorpay", "order_t2"); err != nil {
		t.Fatalf("creditTopup 2: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	if got := crmDispatchScopes(t, w.db, cid, "B-06"); len(got) != 2 || got["order_t2"] != "SENT" {
		t.Fatalf("B-06 twice in one day (scoped by ref): %v", got)
	}

	// Promo credit: the non-refundable branch of the conditional template.
	if _, err := w.svc.promoCredit(ctx, cid, 75, "promo_w1", "Welcome credit"); err != nil {
		t.Fatalf("promoCredit: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	var promoRow crmDispatchRow
	if err := w.db.Collection(collCRMDispatch).FindOne(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "scope_key", Value: "promo_w1"}}).Decode(&promoRow); err != nil {
		t.Fatalf("B-06 promo row: %v", err)
	}
	if promoRow.Status != "SENT" || promoRow.Template != "T-B05-PROMO" {
		t.Fatalf("B-06 promo row must use the else branch: %+v", promoRow)
	}
	var inbox struct {
		BodyEN string `bson:"body_en"`
	}
	if err := w.db.Collection(collConsumerInbox).FindOne(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "template_id", Value: "T-B05-PROMO"}}).Decode(&inbox); err != nil {
		t.Fatalf("promo inbox row: %v", err)
	}
	if !strings.Contains(inbox.BodyEN, "75 Pyaas credit added (Welcome credit)") {
		t.Fatalf("promo body = %q", inbox.BodyEN)
	}

	// Refund: refundable account, the remark as the reason.
	if _, err := w.svc.refund(ctx, cid, 35, "rf_1", "one pack short"); err != nil {
		t.Fatalf("refund: %v", err)
	}
	evs = crmEventsOf(t, w.db, cid, "wallet.credited")
	last := evs[len(evs)-1].Payload
	if last["account"] != "topup" || last["reason"] != "one pack short" || last["scope_key"] != "rf_1" {
		t.Fatalf("wallet.credited on refund: %+v", last)
	}

	// The rider's undo hands the delivery debit back.
	o := instantOrderDelivered(t, w, cid)
	var task delivery
	if err := w.db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: o.OrderID}}).Decode(&task); err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := w.svc.riderReverseDeliveryDebit(ctx, &task, time.Now().UTC()); err != nil {
		t.Fatalf("riderReverseDeliveryDebit: %v", err)
	}
	evs = crmEventsOf(t, w.db, cid, "wallet.credited")
	last = evs[len(evs)-1].Payload
	if last["account"] != "topup" || last["reason"] != "delivery "+task.OrderCode+" reversed" {
		t.Fatalf("wallet.credited on undo: %+v", last)
	}
	if _, has := last["order_id"]; has {
		t.Fatal("the undo credit must not carry order_id (the claim scopes on the undo ref)")
	}
	// D-02 now carries promotional_only, so the instant dispatch fires.
	w.svc.crmProcessEvents(ctx)
	if got := crmDispatchScopes(t, w.db, cid, "D-02"); got[o.OrderID] != "SENT" {
		t.Fatalf("D-02 on an instant order out for delivery: %v", got)
	}
}

func TestCRMEmitSubscriptionLifecycle(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007403", 0)
	nowIST := time.Now().In(istZone)
	t0 := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 15, 0, 0, 0, istZone)

	// Plans made at 10:00 today (a fixed hour: the noon lock decides the
	// first morning). A plan starting today is past today's cut-off, so the
	// first morning A-03 can promise is tomorrow (R2F-16).
	today := istToday(time.Now())
	madeAt := istDayAt(today, 10, 0)
	sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{
		ProductID: "gold-500ml", Variant: "500ml", Qty: 2, Frequency: "daily", StartDate: today,
	}, madeAt)
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	evs := crmEventsOf(t, w.db, cid, "subscription.activated")
	if len(evs) != 1 || evs[0].Payload["subscription_id"] != sub.SubscriptionID || evs[0].Payload["start_label"] != "tomorrow" {
		t.Fatalf("subscription.activated: %+v", evs)
	}
	unpaid := crmEventsOf(t, w.db, cid, "subscription.created_unpaid")
	if len(unpaid) != 1 || crmPayloadInt(unpaid[0].Payload["first_cycle_amount"]) != 70 {
		t.Fatalf("subscription.created_unpaid (empty wallet): %+v", unpaid)
	}
	w.svc.crmProcessEventsAt(ctx, t0)
	if got := crmDispatchStatuses(t, w.db, cid, "A-03"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("A-03: %v", got)
	}
	if body := inboxBodyEN(t, w.db, cid, "A-03"); !strings.HasPrefix(body, "Your morning milk starts tomorrow, delivered by 7 am.") {
		t.Fatalf("A-03 body = %q", body)
	}

	// A funded plan is activated without the unpaid nudge.
	funded := w.customer(t, "9000007404", 500)
	if _, err := w.svc.createSubscriptionAt(ctx, funded, subscriptionInput{
		ProductID: "gold-1l", Qty: 1, Frequency: "alternate", StartDate: addDaysIST(today, 1),
	}, madeAt); err != nil {
		t.Fatalf("createSubscription funded: %v", err)
	}
	if n := len(crmEventsOf(t, w.db, funded, "subscription.created_unpaid")); n != 0 {
		t.Fatalf("a funded plan must not emit created_unpaid: %d", n)
	}
	if evs := crmEventsOf(t, w.db, funded, "subscription.activated"); len(evs) != 1 || evs[0].Payload["start_label"] != "tomorrow" {
		t.Fatalf("funded activated: %+v", evs)
	}

	// Pause: subscription.modified change=paused -> C-03 after PT1H.
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, sub.SubscriptionID, "pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	mods := crmEventsOf(t, w.db, cid, "subscription.modified")
	if len(mods) != 1 || mods[0].Payload["change"] != "paused" {
		t.Fatalf("subscription.modified on pause: %+v", mods)
	}
	w.svc.crmProcessEventsAt(ctx, t0)
	if got := crmDispatchStatuses(t, w.db, cid, "C-03"); len(got) != 0 {
		t.Fatalf("C-03 must wait its hour: %v", got)
	}
	w.svc.crmProcessSchedules(ctx, t0.Add(time.Hour))
	if got := crmDispatchStatuses(t, w.db, cid, "C-03"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("C-03 an hour after the pause: %v", got)
	}
	// Resume is a change C-03 does not read; the event still exists.
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, sub.SubscriptionID, "resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if mods = crmEventsOf(t, w.db, cid, "subscription.modified"); len(mods) != 2 || mods[1].Payload["change"] != "resumed" {
		t.Fatalf("subscription.modified on resume: %+v", mods)
	}
	// Quantity edits name the direction.
	one := 1
	if _, err := w.svc.patchSubscription(ctx, cid, sub.SubscriptionID, struct {
		Qty          *int             `json:"qty"`
		Frequency    *string          `json:"frequency"`
		DeliverySlot *string          `json:"delivery_slot"`
		StartDate    *string          `json:"start_date"`
		Vacations    *[]vacationRange `json:"vacations"`
	}{Qty: &one}); err != nil {
		t.Fatalf("patch qty: %v", err)
	}
	mods = crmEventsOf(t, w.db, cid, "subscription.modified")
	if len(mods) != 3 || mods[2].Payload["change"] != "quantity_reduced" || crmPayloadInt(mods[2].Payload["previous_qty"]) != 2 {
		t.Fatalf("subscription.modified on a reduction: %+v", mods)
	}
	two := 2
	if _, err := w.svc.patchSubscription(ctx, cid, sub.SubscriptionID, struct {
		Qty          *int             `json:"qty"`
		Frequency    *string          `json:"frequency"`
		DeliverySlot *string          `json:"delivery_slot"`
		StartDate    *string          `json:"start_date"`
		Vacations    *[]vacationRange `json:"vacations"`
	}{Qty: &two}); err != nil {
		t.Fatalf("patch qty up: %v", err)
	}
	if mods = crmEventsOf(t, w.db, cid, "subscription.modified"); len(mods) != 4 || mods[3].Payload["change"] != "quantity_increased" {
		t.Fatalf("subscription.modified on an increase: %+v", mods)
	}
}

func TestCRMEmitPaymentFailed(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007405", 0)

	// Razorpay reports a failed top-up payment against our order.
	ord, err := w.svc.createTopupOrder(ctx, cid, 50000)
	if err != nil {
		t.Fatalf("createTopupOrder: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"event": "payment.failed",
		"payload": map[string]any{"payment": map[string]any{"entity": map[string]any{
			"id": "pay_f1", "order_id": ord.OrderID, "status": "failed", "amount": 50000,
			"error_description": "Payment was declined by the bank",
		}}},
	})
	for i := 0; i < 2; i++ { // Razorpay retries webhooks
		if err := w.svc.razorpayWebhookEvent(ctx, body); err != nil {
			t.Fatalf("webhook: %v", err)
		}
	}
	evs := crmEventsOf(t, w.db, cid, "payment.failed")
	if len(evs) != 2 || evs[0].Payload["payment_order_id"] != ord.OrderID || evs[0].Payload["scope_key"] != "pay_f1" || evs[0].Payload["source"] != "razorpay" {
		t.Fatalf("payment.failed from the webhook: %+v", evs)
	}
	w.svc.crmProcessEvents(ctx)
	w.svc.crmFireDueSchedules(ctx, time.Now().Add(11*time.Minute)) // B-03 waits PT10M for a checkout retry
	if got := crmDispatchScopes(t, w.db, cid, "B-03"); len(got) != 1 || got["pay_f1"] != "SENT" {
		t.Fatalf("B-03 once per gateway payment: %v", got)
	}
	// A failed payment for an order we never issued is ignored.
	other, _ := json.Marshal(map[string]any{"event": "payment.failed", "payload": map[string]any{"payment": map[string]any{"entity": map[string]any{
		"id": "pay_x", "order_id": "order_unknown", "status": "failed"}}}})
	if err := w.svc.razorpayWebhookEvent(ctx, other); err != nil {
		t.Fatalf("unknown order webhook: %v", err)
	}
	if n, _ := w.db.Collection(collCRMEvents).CountDocuments(ctx, bson.D{{Key: "topic", Value: "payment.failed"}}); n != 2 {
		t.Fatalf("an unknown order must emit nothing: %d events", n)
	}

	// The mandate charge cannot be funded.
	m := &mandate{
		ID: primitive.NewObjectID(), MandateID: "mnd_test1", ConsumerID: cid, Plan: "daily", Status: "active",
		Amount: 100, MaxAmount: 100, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := w.svc.repo.insertMandate(ctx, m); err != nil {
		t.Fatalf("insertMandate: %v", err)
	}
	at := time.Now()
	for i := 0; i < 2; i++ { // the sweep retries every tick
		if _, err := w.svc.runMandateCharge(ctx, cid, m.MandateID, at); err == nil {
			t.Fatal("an unfunded mandate charge must fail")
		}
	}
	evs = crmEventsOf(t, w.db, cid, "payment.failed")
	if len(evs) != 4 || evs[2].Payload["source"] != "mandate" || evs[2].Payload["scope_key"] != mandateChargeRef(m.MandateID, dayKey(at)) {
		t.Fatalf("payment.failed from the mandate: %+v", evs)
	}
	w.svc.crmProcessEvents(ctx)
	w.svc.crmFireDueSchedules(ctx, time.Now().Add(11*time.Minute))
	if got := crmDispatchScopes(t, w.db, cid, "B-03"); len(got) != 2 || got[mandateChargeRef(m.MandateID, dayKey(at))] != "SENT" {
		t.Fatalf("B-03 once per (mandate, day): %v", got)
	}
}

func TestCRMEmitLineCancelledAndFailedReason(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007406", 1000)

	o, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items: []orderItem{
			{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35},
			{ProductID: "gold-1l", Name: "Milk gold-1l", Variant: "1l", Qty: 1, Price: 69},
		},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Adjust Tester", Phone: "9000007406",
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
		if queue[i].OrderID == o.OrderID {
			task = &queue[i]
		}
	}
	if task == nil {
		t.Fatal("task missing from the store queue")
	}
	if _, err := w.svc.storeAdjustDelivery(ctx, w.mgr, w.storeID.Hex(), task.ID, []itemAdjust{{ProductID: "gold-1l", Qty: 0}}); err != nil {
		t.Fatalf("storeAdjustDelivery: %v", err)
	}
	evs := crmEventsOf(t, w.db, cid, "order.line_cancelled")
	if len(evs) != 1 {
		t.Fatalf("order.line_cancelled events = %d", len(evs))
	}
	p := evs[0].Payload
	if p["order_id"] != o.OrderID || p["before_delivery"] != true || crmPayloadInt(p["amount"]) != 69 ||
		p["labelled_product"] != "Milk gold-1l 1l"+crmLabelledSuffix || p["scope_key"] != o.OrderID+":"+o.Items[1].ID {
		t.Fatalf("order.line_cancelled payload: %+v", p)
	}
	w.svc.crmProcessEvents(ctx)
	if got := crmDispatchScopes(t, w.db, cid, "D-05"); got[o.OrderID+":"+o.Items[1].ID] != "SENT" {
		t.Fatalf("D-05: %v", got)
	}
	if body := inboxBodyEN(t, w.db, cid, "D-05"); !strings.Contains(body, "Milk gold-1l 1l"+crmLabelledSuffix) || !strings.Contains(body, "69") {
		t.Fatalf("D-05 body = %q", body)
	}
	// A reduction that keeps the line is not a cancellation.
	if _, err := w.svc.storeAdjustDelivery(ctx, w.mgr, w.storeID.Hex(), task.ID, []itemAdjust{{ProductID: "gold-500ml", Qty: 1}}); err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if n := len(crmEventsOf(t, w.db, cid, "order.line_cancelled")); n != 1 {
		t.Fatalf("a reduced line must not emit line_cancelled: %d", n)
	}

	// order.failed carries only the customer-readable cause of the rider's note.
	inst, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "instant", Priority: "normal", Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
	})
	if err != nil {
		t.Fatalf("instant createOrder: %v", err)
	}
	var itask delivery
	if err := w.db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: inst.OrderID}}).Decode(&itask); err != nil {
		t.Fatalf("instant task: %v", err)
	}
	if _, err := w.svc.claimOfferedDelivery(ctx, w.rider, itask.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := w.svc.failDelivery(ctx, w.rider, itask.ID, "Customer not available | rang twice, nobody home | photo=nd/abc.jpg | geo=26.77120,81.01230 | called=true"); err != nil {
		t.Fatalf("failDelivery: %v", err)
	}
	fev := crmEventsOf(t, w.db, cid, "order.failed")
	if len(fev) != 1 || fev[0].Payload["reason"] != "Customer not available" {
		t.Fatalf("order.failed reason must be the picklist label only: %+v", fev)
	}
	for in, want := range map[string]string{
		"CUSTOMER_UNAVAILABLE":            "Customer not available",
		"Cancelled by the store":          "Cancelled by the store",
		"":                                "the delivery could not be completed",
		"photo=nd/x.jpg | geo=1,2":        "the delivery could not be completed",
		"GATE_CLOSED | lift out of order": "Society gate / lift closed",
	} {
		if got := crmFailureReasonForCustomer(in); got != want {
			t.Errorf("crmFailureReasonForCustomer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCRMEmitDeliveryDelayed(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007407", 1000)

	// An instant order on the road: eta_at = placed + 20 min.
	o, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "instant", Priority: "normal", Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	var task delivery
	if err := w.db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: o.OrderID}}).Decode(&task); err != nil {
		t.Fatalf("task: %v", err)
	}
	end, ok := crmDeliveryWindowEnd(&task)
	if !ok || end.Sub(o.PlacedAt.Truncate(time.Second)) != 20*time.Minute { // eta_at is RFC3339, whole seconds
		t.Fatalf("instant window end: %v ok=%v (placed %v)", end, ok, o.PlacedAt)
	}
	// Not out yet: past the window, but no honest ETA to give -> nothing.
	w.svc.crmProcessSchedules(ctx, end.Add(30*time.Minute))
	if n := len(crmEventsOf(t, w.db, cid, "delivery.delayed")); n != 0 {
		t.Fatalf("a task still at the store must not be called late: %d events", n)
	}
	if _, err := w.svc.claimOfferedDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	// Inside the grace: still on time.
	w.svc.crmProcessSchedules(ctx, end.Add(2*time.Minute))
	if n := len(crmEventsOf(t, w.db, cid, "delivery.delayed")); n != 0 {
		t.Fatalf("inside the grace must not be late: %d events", n)
	}
	late := end.Add(10 * time.Minute)
	w.svc.crmProcessSchedules(ctx, late)
	w.svc.crmProcessSchedules(ctx, late.Add(time.Minute)) // once per task
	evs := crmEventsOf(t, w.db, cid, "delivery.delayed")
	if len(evs) != 1 || evs[0].Payload["order_id"] != o.OrderID || evs[0].Payload["new_eta_known"] != true {
		t.Fatalf("delivery.delayed: %+v", evs)
	}
	eta, _ := evs[0].Payload["eta"].(string)
	if !strings.HasPrefix(eta, "about ") || !(strings.HasSuffix(eta, " am") || strings.HasSuffix(eta, " pm")) {
		t.Fatalf("eta wording: %q", eta)
	}
	w.svc.crmProcessEventsAt(ctx, late)
	if got := crmDispatchScopes(t, w.db, cid, "D-03"); got[o.OrderID] != "SENT" {
		t.Fatalf("D-03: %v", got)
	}
	if body := inboxBodyEN(t, w.db, cid, "D-03"); !strings.Contains(body, "new ETA about ") {
		t.Fatalf("D-03 body = %q", body)
	}

	// A morning task's window ends at the slot's end on its delivery day.
	morning := &delivery{Slot: "2026-09-25 " + string(rune(0xb7)) + " 05:00 - 07:30 AM", DeliveryDate: "2026-09-25"}
	if end, ok := crmDeliveryWindowEnd(morning); !ok || end.In(istZone).Format("2006-01-02 15:04") != "2026-09-25 07:30" {
		t.Fatalf("morning window end: %v ok=%v", end, ok)
	}
	if _, ok := crmDeliveryWindowEnd(&delivery{}); ok {
		t.Fatal("a task with no window has no end")
	}
}

func TestCRMEmitServiceabilityChecked(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007408", 0)
	// Move the saved address far outside the store fence.
	if _, err := w.db.Collection(collAddresses).UpdateMany(ctx, bson.D{{Key: "consumer_id", Value: cid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "lat", Value: 28.6139}, {Key: "lng", Value: 77.2090}, {Key: "pincode", Value: "110001"}}}}); err != nil {
		t.Fatalf("move address: %v", err)
	}
	if _, err := w.svc.crmSelfEnrol(ctx, cid, crmEnrolInput{}); err == nil || crmErrCode(err) != "NOT_SERVICEABLE" {
		t.Fatalf("self enrol out of zone: %v", err)
	}
	evs := crmEventsOf(t, w.db, cid, "serviceability.checked")
	if len(evs) != 1 || evs[0].Payload["in_zone"] != false || evs[0].Payload["pincode"] != "110001" {
		t.Fatalf("serviceability.checked on enrol: %+v", evs)
	}
	w.svc.crmProcessEvents(ctx)
	if got := crmDispatchStatuses(t, w.db, cid, "W-08"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("W-08: %v", got)
	}

	// The waitlist route learns the member from the bearer token only.
	acct, _ := w.svc.repo.findAccountByID(ctx, cid)
	w.svc.deps.Cfg.AccessTokenTTL = time.Hour // the chain world sets no token lifetime
	tok, err := w.svc.issueTokens(ctx, acct, time.Now().UTC())
	if err != nil {
		t.Fatalf("issueTokens: %v", err)
	}
	if id, _, perr := w.svc.parseAccessToken(tok.AccessToken); perr != nil || id != cid.Hex() {
		t.Fatalf("parseAccessToken: %q %v", id, perr)
	}
	w.svc.appKey = "test-app-key"
	h := &handler{svc: w.svc}
	post := func(bearer string) int {
		req := httptest.NewRequest(http.MethodPost, "/consumer/waitlist", strings.NewReader(`{"phone":"9000007408","pincode":"110001","name":"Far Away"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Parag-App-Key", "test-app-key")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.joinWaitlist(rec, req)
		return rec.Code
	}
	if code := post(""); code != http.StatusOK {
		t.Fatalf("waitlist without a token: %d", code)
	}
	if n := len(crmEventsOf(t, w.db, cid, "serviceability.checked")); n != 1 {
		t.Fatalf("no bearer, no member event (the phone is never trusted): %d", n)
	}
	if code := post(tok.AccessToken); code != http.StatusOK {
		t.Fatalf("waitlist with a token: %d", code)
	}
	evs = crmEventsOf(t, w.db, cid, "serviceability.checked")
	if len(evs) != 2 || evs[1].Payload["source"] != "waitlist" {
		t.Fatalf("serviceability.checked from the waitlist: %+v", evs)
	}
	// One message ever: the day claim holds today, and the per_customer cap
	// suppresses a later day.
	w.svc.crmProcessEvents(ctx)
	if got := crmDispatchStatuses(t, w.db, cid, "W-08"); len(got) != 1 {
		t.Fatalf("W-08 must not send twice: %v", got)
	}
	if st, g := w.svc.crmDispatchAt(ctx, "W-08", cid, nil, time.Now().AddDate(0, 0, 1)); st != "SUPPRESSED" || g != "G6_frequency_cap" {
		t.Fatalf("W-08 tomorrow: %s %s", st, g)
	}
}
