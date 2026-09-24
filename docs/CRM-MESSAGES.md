# CRM messages: what the member is told, when, and whether it leaves the building

Every message the CRM engine can send, as of `integration/delivery` on 24 Sep 2026
(after the CRM completion stage: `8edd9e1` .. `81e2ff6`, then human_call, the matrix
test and the inbox refs). The source of truth is `internal/modules/consumer/crm_triggers.json`;
this file is written from it by hand, so when the two disagree the JSON wins and this
file is out of date. How the engine works: `docs/HANDOFF-CODEV-2026-09-24.md` section 4.
The per-trigger audit that led here: `docs/CRM-AUDIT-2026-09-24.md`.

`crm_matrix_test.go` fires every trigger marked FIRES / INBOX-ONLY / HUMAN below, end to
end, and fails if a trigger is added without a scenario or an `awaiting_event` mark.

## How to read the tables

- **When (IST).** Event triggers fire on the next worker tick (the worker runs every 60 s)
  after their product event, plus the trigger's `delay` when it has one; a delayed
  trigger re-checks its conditions when it fires and skips an order cancelled
  meanwhile. Scheduled triggers fire on the first tick at or after the time shown.
  Every message is claimed exactly once per (trigger, member, IST day, order or
  complaint), so two orders on one day each get their own message.
- **Channels, in order.** The first is the primary; `+x` runs in parallel; `then x` is a
  fallback taken only when the channel before it is unavailable or refused. The in-app
  inbox is not listed because it ALWAYS gets the message (except human_call-only E-05,
  which must never auto-respond).
- **Env to leave the building.** Nothing leaves without `CRM_ENABLED=true`. Then per
  channel:
  - `push`: `EXPO_PUSH_ENABLED=true` (optionally `EXPO_ACCESS_TOKEN`) AND a device token,
    which needs the EAS projectId in the consumer app (`eas init`; none today, so no
    device has a token).
  - `whatsapp`: `CRM_WA_TOKEN` + `CRM_WA_PHONE_ID` + the trigger in
    `CRM_WA_TEMPLATE_NAMES` (a Meta-approved template).
  - `sms`: `CRM_MSG91_AUTHKEY` + the trigger in `CRM_DLT_TEMPLATE_IDS` (a DLT-registered
    template). Never for a `promotional` trigger (no DND scrub exists).
  - `human_call`: nothing; the row lands on the operator queue
    `GET /consumer/admin/crm/callbacks` (admin CRM auth).
  - `admin_console`: nothing; the Saathi admin notifications bell. `email`, `ai_call`
    and `rcs` are not built.
- **Status today**, with the production env as last seen (DLT ids for W-01 and W-07 only,
  no WhatsApp keys, push off, no device tokens):
  - **FIRES + SMS**: inbox and a real SMS.
  - **INBOX-ONLY**: fires into the in-app inbox; nothing reaches a phone until the
    channel's env above exists.
  - **CONSENT**: promotional; fires into the inbox only for a member with a marketing
    grant under 7 days old, otherwise logged SUPPRESSED `G2_consent`.
  - **HUMAN**: a call-back row for an operator.
  - **OPERATOR**: the admin bell, no member message by design.
  - **AWAITING**: in `meta.awaiting_event`; the engine never evaluates it until the
    product event or runner named in the reason exists.
- **Body** is the English template rendered with realistic values. Every template also
  has Hindi (roman, and Devanagari where written); the inbox stores both.

## 1. Live messages (32)

| id | topic / schedule | when (IST) | channels | env to leave the building | status today | body (EN) |
|---|---|---|---|---|---|---|
| A-01 | `user.registered` | 2 h after a new account is created by OTP sign-in, if the member still has no delivered order | push, then whatsapp, then sms | push; or WA A-01; or DLT A-01 | INBOX-ONLY | Welcome to Pyaas! Fresh milk is 2 taps away — recharge your Wallet & place your first order. Help: 96672 60050. |
| A-02 | `order.delivered` | on delivery, when the member's first-ever delivered order is a Quick Pyaas (instant) order | whatsapp, then sms | WA A-02; or DLT A-02 | INBOX-ONLY | Delivered! Thanks for trying Full Cream Milk - Parag Gold 500ml — delivered by PYAAS. Reorder anytime in the app. |
| A-03 | `subscription.activated` | when the member creates a plan in the app (not the Welcome Litre plan, which W-01 announces) | whatsapp | WA A-03 | INBOX-ONLY | Your morning milk starts tomorrow, delivered by 7 am. Pause/resume anytime — changes before 12 noon apply from the next morning. Keep your Wallet topped up so it never stops. |
| A-05 | `subscription.created_unpaid` | 2 h after an app plan is created with a wallet that cannot cover its first day; skipped if the member topped up meanwhile | whatsapp, then sms | WA A-05; or DLT A-05 | INBOX-ONLY | One step left — recharge your Wallet to start tomorrow's delivery. |
| B-01 | `0 9 * * *` (code) | first tick after 09:00, when the wallet covers under 4 days of the member's daily plans; at most once in 7 days; not during a live Welcome Litre journey | whatsapp, +push, then sms | WA B-01; push; or DLT B-01 | INBOX-ONLY | Your Wallet is running low. Recharge to keep your morning milk coming. |
| B-02 | config says `0 17 * * *`; code runs at 12:00 | first tick after 12:00, when tomorrow's plan costs more than the wallet holds; every day it stays short | sms, +whatsapp | DLT B-02; WA B-02 | INBOX-ONLY (SMS refused: no DLT id) | Low balance — recharge by 12 noon tomorrow to receive your delivery. (Copy vs horizon is the founder's open call, handoff 6b.) |
| B-03 | `payment.failed` | a Razorpay top-up payment fails (webhook, once per payment), or an AutoPay mandate charge finds the wallet short (once per mandate per day) | sms, +whatsapp | DLT B-03; WA B-03 | INBOX-ONLY | Your recharge didn't go through. Try again or use another method — no money was deducted. |
| B-06 | `wallet.credited` | every wallet credit, once per ledger ref, after the money is in: top-up (app verify, Razorpay webhook, reconcile sweep, dev top-up), refund, rider undo (refundable wording); promo credit (Pyaas-credit wording) | whatsapp, then sms | WA B-06; or DLT B-06 | INBOX-ONLY | ₹500 added to your Wallet (recharge). Refundable. Ready for your next order. / promo: ₹75 Pyaas credit added (Welcome credit). Usable on orders; not refundable in cash. Valid 30 days. |
| C-03 | `subscription.modified` | 1 h after the member pauses a plan or reduces its quantity | whatsapp | WA C-03 | INBOX-ONLY | We noticed a change. Everything okay with your deliveries? Reply here or we can call. |
| D-01 | `order.confirmed` | an instant order is placed; a morning order goes live at the lock; never for a free promo pack | push, then whatsapp, then sms | push; or WA D-01; or DLT D-01 | INBOX-ONLY | Order confirmed ✅ Full Cream Milk - Parag Gold 500ml — delivered by PYAAS. Arriving tomorrow by 7 am. |
| D-02 | `order.dispatched` | the rider picks up an instant (Quick Pyaas) order | push, then whatsapp, then sms | push; or WA D-02; or DLT D-02 | INBOX-ONLY | Out for delivery — Ravi, ETA 12 min. |
| D-03 | `delivery.delayed` | a task on the road is 5 min past its window end (detector on the worker tick, once per task) | push, +whatsapp | push; WA D-03 | INBOX-ONLY | Running a little late — new ETA about 7:52 am. Sorry for the wait. |
| D-05 | `order.line_cancelled` | the store removes a line from an order before delivery | whatsapp, +sms | WA D-05; DLT D-05 | INBOX-ONLY | Full Cream Milk - Parag Gold 500ml — delivered by PYAAS is unavailable today; Rs35 has been taken off your bill and nothing is charged for it. Sorry! |
| D-06 | `order.delivered` | an order is delivered; never for a free promo pack (W-02 / W-05 own those) | push, then whatsapp, then sms | push; or WA D-06; or DLT D-06 | INBOX-ONLY | Delivered ✅ Full Cream Milk - Parag Gold 500ml — delivered by PYAAS. Enjoy! Tap to rate. |
| D-09 | `order.failed` | 15 min after the rider marks the task not delivered, or the store cancels it (the rider's undo window); skipped if the order is live again by then (undo or reassign) | push, then whatsapp, then sms | push; or WA D-09; or DLT D-09 | INBOX-ONLY | Not delivered - Full Cream Milk - Parag Gold 500ml — delivered by PYAAS could not reach you (customer not at home). Nothing has been charged for it. Tap to order again, or call 96672 60050. |
| E-01 | `rating.submitted` | the member rates an order 3 or lower | whatsapp | WA E-01 | INBOX-ONLY | Sorry we missed the mark. What went wrong? We'll make it right. |
| E-02 | `complaint.created` | every new complaint (a retried ref sends nothing) | whatsapp, then sms | WA E-02; or DLT E-02 | INBOX-ONLY | Got it — we're on it. You'll hear back within 24 hours with a fix. |
| E-04 | `complaint.created` | a complaint of category `missing` that names its order | whatsapp, +human_call | WA E-04 (the call-back needs nothing) | INBOX-ONLY + HUMAN | Sorry! We'll redeliver Full Cream Milk - Parag Gold 500ml — delivered by PYAAS or refund ₹70 to your Wallet — your choice. |
| E-05 | `complaint.created` | a complaint of category `quality` (milk quality / safety) | human_call, then sms | none: the call-back row is the delivery | HUMAN (no member message: the config forbids an automatic reply; E-02 still acknowledges) | (no template, by design) |
| E-06 | `complaint.resolved` | an operator marks a complaint `resolved` (a move straight to `closed` sends nothing) | whatsapp, then sms | WA E-06; or DLT E-06 | INBOX-ONLY | Fixed ✅ Thanks for your patience. Anything else? We're here. |
| E-07 | `rating.submitted` (promotional) | 4 h after a 5-star rating, with no open complaint, 10:00–21:00 | whatsapp | WA E-07 + the member's `marketing_whatsapp` grant | CONSENT | Thank you! Love Pyaas? Refer a neighbour — you both get Pyaas credit after their first paid order. |
| W-01 | `offer.finalized` | a Welcome Litre enrolment completes (promoter or the app's own funnel) | whatsapp, +push, then sms | DLT W-01 (mapped); WA W-01; push | FIRES + SMS | You're set. Tomorrow by 7 am: 500 ml Parag Full Cream — delivered by PYAAS, free. No payment now. Recharge Rs500 any time in the next 7 days and your second 500 ml pack comes free with your first paid delivery. Pause or cancel any time. |
| W-02 | `order.delivered` (pack 1) | the free pack 1 is delivered | push, then whatsapp | push; or WA W-02 | INBOX-ONLY | Delivered. 500 ml Parag Full Cream — delivered by PYAAS. That one's on us. Want milk tomorrow too? Recharge by 12 noon today. |
| W-03a | `30 10 * * *` | day 0 (pack 1 landed today), after 10:30, if the member has not topped up | whatsapp, +push | WA W-03a; push | INBOX-ONLY | Milk tomorrow? Recharge by 12 noon today. Rs500 is about 8 mornings. Pause or cancel any time. https://pyaasdairy.com/app |
| W-03b | `32 10 * * *` (promotional) | day 0, after 10:32, while pack 2 is locked | whatsapp | WA W-03b + the member's `marketing_whatsapp` grant | CONSENT (owner decision 24 Sep: stays consent-gated) | And your second 500 ml Full Cream is waiting — it comes free with your first paid delivery once you recharge Rs500. Valid 7 days. https://pyaasdairy.com/app STOP to opt out. |
| W-04 | `wallet.recharge_settled` | a settled top-up of ₹500 or more within the 7-day window, once pack 2 is attached to tomorrow's delivery | whatsapp, +push | WA W-04; push | INBOX-ONLY | Recharge received. Your second 500 ml Parag Full Cream — delivered by PYAAS — is scheduled free with tomorrow's delivery. |
| W-05 | `order.delivered` (pack 2) | the free pack 2 is delivered | push, then whatsapp | push; or WA W-05 | INBOX-ONLY | Delivered. Full Cream Milk - Parag Gold 500ml — delivered by PYAAS plus your second free 500 ml. That completes the welcome offer — from here it's just milk, every morning. |
| W-06 | `30 10 * * *` | days 3 and 5 after pack 1, after 10:30, while pack 2 is locked | whatsapp | WA W-06 | INBOX-ONLY | Your second 500 ml Full Cream is still waiting — recharge Rs500 by 1 Oct and it comes free with your next delivery. https://pyaasdairy.com/app |
| W-07 | `30 10 * * *` | from day 8 (the 7-day window is over), after 10:30, while pack 2 is locked; the pack expires only once this is SENT | whatsapp, then sms | DLT W-07 (mapped); WA W-07 | FIRES + SMS | Your second free pack has expired — the 7 days are up. Nothing has been charged. Whenever you'd like milk again, it's one tap in the app. https://pyaasdairy.com/app |
| W-08 | `serviceability.checked` | a signed-in member's address is out of zone (self-enrol refused, or the waitlist join with a session); once per member ever | whatsapp, then sms | WA W-08; or DLT W-08 | INBOX-ONLY | We don't deliver to your area yet. We've noted your pincode and we'll tell you the day we do — that's the only message you'll get from us. |
| W-09 | `abuse_flag_raised` (internal) | a second Welcome Litre enrolment at an address already enrolled | admin_console | none | OPERATOR | (operator bell: "address_hash match — second offer at the same address (review, do not auto-reject)") |
| W-10 | `0 18 * * *` (internal) | first tick after 18:00: free packs issued vs enrolments that day | admin_console, +email | none (email not built) | OPERATOR (bell only on a variance) | (operator bell: "packs issued N vs consumers created M — variance V") |

## 2. Awaiting a product event (22) and the alias

These keep their copy and routing in the config; the engine never evaluates them. Remove
an entry from `meta.awaiting_event` only together with the emitter or runner that makes
it real (the matrix test then needs its scenario).

| id | topic / schedule | channels | why it cannot fire yet (meta.awaiting_event) |
|---|---|---|---|
| A-04 | `*/15 * * * *` cart | push, then whatsapp | no server-side cart: the basket lives on the phone; no scheduled runner |
| A-06 | `0 10 * * *` | whatsapp | no scheduled runner for days_since_registration (the data exists) |
| B-04 | `0 10 * * *` | whatsapp | wallet plans have no renewal date; the nearest fact is a mandate's next charge |
| B-05 | `30 10 * * *` | ai_call | ai_call is not a transport; the pause action is not built |
| C-01 | `0 11 * * *` | whatsapp | no scheduled runner for days_since_last_order |
| C-02 | `0 11 * * *` promotional | whatsapp, then sms | no scheduled runner; the Rs75 promo credit has no production grant path |
| C-04 | alias of B-04 | - | an alias, never routed |
| C-05 | `0 11 * * *` | sms | no delivery or read-receipt store to count unread WhatsApp messages |
| C-06 | `0 12 * * *` | ai_call | ai_call is not a transport; no high_value segment |
| C-07 | `delivery.miss_detected` | whatsapp | no miss detector: a failed task emits order.failed (D-09), a late one delivery.delayed (D-03) |
| D-04 | `order.substitution_proposed` | whatsapp, then push | no substitution flow: the store adjust is reduce-only |
| D-07 | `0 12 * * *` | sms, +whatsapp | the template carries an unrendered alternative-text block; B-02 covers the unfunded cause |
| D-08 | `route.changed` | whatsapp, +sms | zones carry no effective date and their edits emit no event |
| E-03 | `delivery.late_confirmed` | whatsapp | no lateness confirmation and no production grant path for the Rs20 apology credit |
| F-01 .. F-09 | schedules | ai_call | ai_call is not a transport (F-02 also needs a server cart; F-05 a high_value segment) |

## 3. Wallet credit paths and the top-up message (the owner's 24 Sep complaint)

Every path that adds money, through its real entry point, drained through the real
worker (`TestCRMTopupMessageEveryCreditPath`):

| credit path | trigger | message | proof |
|---|---|---|---|
| app confirm `POST /wallet/verify` | B-06 (T-B05-REFUND) | exactly one: "₹500 added to your Wallet (recharge). Refundable. …" | case 1 |
| Razorpay webhook `payment.captured` / `order.paid` (and its retries) | B-06 | exactly one, also when verify arrives later for the same payment | case 2 |
| verify and webhook racing on one payment | B-06 | exactly one (the ledger gate admits one credit) | case 2b |
| reconcile sweep | B-06 | exactly one; a second pass adds nothing | case 3 |
| dev `POST /wallet/topup` (OTP dev mode only) | B-06 | "₹250 added to your Wallet (recharge). Refundable." | case 4 |
| promo credit (dev only today) | B-06 (T-B05-PROMO) | "₹75 Pyaas credit added (Welcome credit). Usable on orders; not refundable in cash." | case 5 |
| refund (dev only today) | B-06 | "₹35 added to your Wallet (one pack short). Refundable." | case 6 |
| rider undo within 15 minutes | B-06 | "₹<debit> added to your Wallet (delivery <code> reversed). Refundable." | case 7 |
| AutoPay mandate execution | none | it DEBITS the wallet (subscription auto-renewal), so no "added" message; a short wallet sends B-03 | case 8, `TestCRMEmitPaymentFailed` |
| a credit whose balance update fails | none | no message for money that never arrived (fixed in `f7384d9`) | case 9 |

A Welcome Litre household that tops up ₹500 or more inside its 7-day window also gets
W-04 for the same recharge: that one is about the free pack, B-06 is the receipt.

## 4. How to add a message

The engine is data-driven: most new messages are a config change only.

1. **Write the trigger and its template** in `internal/modules/consumer/crm_triggers.json`
   (and bump `meta.trigger_count`; `crm_test.go` and `crm_tokens_test.go` pin the counts):
   `kind: "event"`, `event: "<topic>"`, `category` (`service_implicit`, `transactional`,
   or `promotional`, which needs consent, the 10:00–21:00 window and never goes by SMS),
   `delivery` (primary / parallel / fallback), optional `conditions` (one expression per
   line; the keys the router knows are listed in `crm_router.go` `fact`; an unknown key
   fails closed), optional `delay` (ISO 8601, e.g. `PT2H`), and a template in EN + HI
   whose tokens are UPPERCASE (`[LABELLED_PRODUCT]`, `[ETA]`, `[ETA_MIN]`, `[AMOUNT]`,
   `[X]`, `[REASON]`, `[REF]`, `[SLA]`, `[LINK]`, `[SUPPORT_NUMBER]`, `[DATE]`).
2. **Only if the topic is new**: add ONE `emitCRMEvent(ctx, "<topic>", consumerID, payload)`
   at the product choke point (after the state change is committed), add the topic to
   `crmLifecycleTopics` in `crm_router.go`, and make `crmEventParams` fill every token
   from the payload. An existing topic (order.*, complaint.*, rating.submitted,
   wallet.credited, subscription.*, ...) needs no Go at all.
3. **Add its line to the matrix** in `crm_matrix_test.go`: the payload the emitter writes
   and what it must leave (an inbox row, a call-back, the operator bell). The matrix
   fails until you do, and `crm_tokens_test.go` fails if a token cannot be filled. A
   message whose product event does not exist yet goes in `meta.awaiting_event` with the
   reason instead.
4. **Let it leave the building**: register the SMS template on DLT and add its id to
   `CRM_DLT_TEMPLATE_IDS`; get the WhatsApp template approved and add its name to
   `CRM_WA_TEMPLATE_NAMES`. Without these it is inbox (and push, once enabled) only, by
   design.
5. **Update this file**: a row in section 1 with its status today.
