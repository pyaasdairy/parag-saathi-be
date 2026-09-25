# CRM messages: what the member is told, when, and whether it leaves the building

Every message the CRM engine can send, as of `integration/delivery` on 24 Sep 2026
(after the CRM completion stage: `8edd9e1` .. `81e2ff6`, then human_call, the matrix
test and the inbox refs, and the merge of `feature/founding-referrals`, which adds
FF-01 .. FF-03), plus the noon rules of 24 Sep afternoon (`feature/noon-rules`: D-07
wired to the noon lock's skipped day, B-02 held until the lock has decided and not
sent beside D-07, A-05 and FF-01 naming the real day; then W-01 naming the morning the
free pack really comes, B-06 by push too, every body under its own SMS / WhatsApp
registration, and a refused send recorded on its dispatch row), plus FF-04 of 25 Sep
(`decisions/pricing`: a referred friend's Rs 99 join moves a waiting referrer up the
line). The source of truth is `internal/modules/consumer/crm_triggers.json`;
this file is written from it by hand, so when the two disagree the JSON wins and this
file is out of date. How the engine works: `docs/HANDOFF-CODEV-2026-09-24.md` section 4.
The per-trigger audit that led here: `docs/CRM-AUDIT-2026-09-24.md`.

`crm_matrix_test.go` fires every trigger marked FIRES / INBOX-ONLY / HUMAN below, end to
end, and fails if a trigger is added without a scenario or an `awaiting_event` mark.

## How to read the tables

- **When (IST).** Event triggers fire on the next worker tick (the worker runs every 60 s)
  after their product event, plus the trigger's `delay` when it has one; a delayed
  trigger re-checks its conditions when it fires and skips an event undone
  meanwhile (an order cancelled, a plan changed back, a failure undone, a failed
  payment paid by a retry). Scheduled triggers fire on the first tick at or after the time shown.
  Every message is claimed exactly once per (trigger, member, IST day, order or
  complaint), so two orders on one day each get their own message; a trigger
  capped per event (per_order, per_complaint, per_credit, ...) claims its order or
  complaint once whatever the day, so a replay on a later day sends nothing. FF-03
  is claimed once per join and FF-01 once per farm; FF-02 carries the same farm scope,
  so a member who re-joins another farm the same day is told about that farm too.
  FF-04 is claimed once per referral.
  **Quiet hours, 22:00 to 07:00 IST** (founder decision 6, section 5): only order,
  delivery and money messages, the answer to what the member just did, and critical ones
  go out inside them; every other message waits for 07:00, and a promotional one for its
  own 10:00 to 21:00 window.
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
  - `sms`: `CRM_MSG91_AUTHKEY` + the trigger in `CRM_DLT_TEMPLATE_IDS`, whose value is the
    MSG91 panel's template id for the DLT-registered template (NOT the 19-digit DLT id;
    `CRM_MSG91_SENDER` is optional). Never for a `promotional` trigger (no DND scrub
    exists).
  - **One registration per body** (both maps): a key is a template id (`T-B05-PROMO`,
    looked up first) or a trigger id (`B-06`), and a trigger id covers only the trigger's
    own template (its plain one, or a conditional's `then` branch). A second body of the
    same trigger (B-06's Pyaas-credit wording T-B05-PROMO, W-01's T-W01-LATER, E-04's
    redelivery-only T-E04-REDELIVER) needs its own key, else it goes to the inbox and push
    only, never under another body's registration.
  - A channel that TRIED and was refused (or failed with an unknown outcome) is logged at
    ERROR with the trigger and template and kept on the dispatch row as `channel_errors`
    (shown on the operator's dispatch log); the row's `channel` names only what carried
    the message.
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

## 1. Live messages (37)

| id | topic / schedule | when (IST) | channels | env to leave the building | status today | body (EN) |
|---|---|---|---|---|---|---|
| A-01 | `user.registered` | 2 h after a new account is created by OTP sign-in, if the member still has not started (no order that is not cancelled, no recharge, no plan); due between 22:00 and 07:00 it waits for 07:00 and is re-checked then | push, then whatsapp, then sms | push; or WA A-01; or DLT A-01 | INBOX-ONLY | Welcome to Pyaas! Fresh milk is 2 taps away — recharge your Wallet & place your first order. Help: 96672 60050. |
| A-02 | `order.delivered` | on delivery, when the member's first-ever delivered order is a Quick Pyaas (instant) order. Counted as of that delivery (the event's `orders_count` snapshot, founder decision 7): two first orders delivered inside one worker tick thank the first, where both used to read 2 and neither was thanked; once per member ever (`per_customer`) | whatsapp, then sms | WA A-02; or DLT A-02 | INBOX-ONLY | Delivered! Thanks for trying Full Cream Milk - Parag Gold 500ml — delivered by PYAAS. Reorder anytime in the app. |
| A-03 | `subscription.activated` | when the member creates a plan in the app (not the Welcome Litre plan, which W-01 announces) | whatsapp | WA A-03 | INBOX-ONLY | Your morning milk starts tomorrow, delivered by 7 am. Pause/resume anytime — changes before 12 noon apply from the next morning. Keep your Wallet topped up so it never stops. |
| A-05 | `subscription.created_unpaid` | 2 h after an app plan is created with a wallet that cannot cover its first day; skipped if the member topped up meanwhile or the plan is no longer active. [DATE] is the morning the plan starts as seen when the message is SENT (a tomorrow the noon lock skipped in those two hours is not offered); due between 22:00 and 07:00 it waits for 07:00 and is re-checked then | whatsapp, then sms | WA A-05; or DLT A-05 (both need the new wording registered) | INBOX-ONLY | One step left — recharge your Wallet and your morning milk starts tomorrow. / after noon: … starts 8 Oct. |
| B-01 | `0 9 * * *` (code) | first tick after 09:00, when the wallet covers under 4 days of the member's daily plans; at most once in 7 days; not during a live Welcome Litre journey; a run that only comes after 22:00 (worker down all day) sends nothing and the next morning's decides | whatsapp, +push, then sms | WA B-01; push; or DLT B-01 | INBOX-ONLY | Your Wallet is running low. Recharge to keep your morning milk coming. |
| B-02 | `0 12 * * *` (code: right after the noon lock) | first tick after 12:00 on day D once a subscription sweep has run the noon lock for D+1 to the end (`consumer_noon_locks`) and then no D+1 preview is still undecided (from 13:00 regardless once the lock has run), when the plans due on D+2 (whose order locks at 12 noon on D+1, the day the copy names) cost more than the wallet will hold once D+1's locked delivery is paid; every day it stays short; NOT to a member D-07 told at that noon that D+1 was skipped (one clear message) | sms, +whatsapp | DLT B-02; WA B-02 | INBOX-ONLY (SMS refused: no DLT id) | Low balance — recharge by 12 noon tomorrow to receive your delivery. (Owner, 24 Sep: the copy stays, the horizon was fixed.) |
| B-03 | `payment.failed` | 10 min after a Razorpay top-up payment fails (webhook, once per payment), skipped if a retry paid that same order meanwhile; or an AutoPay mandate charge finds the wallet short (once per mandate per day) | sms, +whatsapp | DLT B-03; WA B-03 | INBOX-ONLY | Your recharge didn't go through. Try again or use another method — no money was deducted. |
| B-06 | `wallet.credited` | every wallet credit, once per ledger ref, after the money is in: top-up (app verify, Razorpay webhook, reconcile sweep, dev top-up), refund, rider undo (refundable wording, T-B05-REFUND); promo credit and the referral reward (Pyaas-credit wording, T-B05-PROMO) | whatsapp, +push, then sms | WA B-06 (refund body) / WA T-B05-PROMO; push; or DLT B-06 (refund body only) / DLT T-B05-PROMO. SMS variables: `##x##` the rupees ("500"), `##reason##` ("recharge", "delivery 5D05C6 reversed", "Welcome credit"), a DLT variable holds at most 30 characters (see section 3) | INBOX-ONLY (push once enabled) | ₹500 added to your Wallet (recharge). Refundable. Ready for your next order. / promo: ₹75 Pyaas credit added (Welcome credit). Usable on orders; not refundable in cash. |
| C-03 | `subscription.modified` | 1 h after the member pauses a plan or reduces its quantity; skipped if the plan was resumed or the quantity restored meanwhile; due between 22:00 and 07:00 it waits for 07:00 | whatsapp | WA C-03 | INBOX-ONLY | We noticed a change. Everything okay with your deliveries? Reply here or we can call. |
| D-01 | `order.confirmed` | an instant order is placed; a one-off morning order is placed (for a morning already closed at 12 noon it is moved to the first open one, and [ETA] names that day: "8 Oct by 7 am"); a subscription morning order goes live at the lock; never for a free promo pack | push, then whatsapp, then sms | push; or WA D-01; or DLT D-01 | INBOX-ONLY | Order confirmed ✅ Full Cream Milk - Parag Gold 500ml x2 + 1L x1 — delivered by PYAAS. Arriving tomorrow by 7 am. (Every [LABELLED_PRODUCT] names each line: one product's sizes with counts, a single pack as "name size", several products as "first + N more"; an SMS variable is cut on a word at 30 characters.) |
| D-02 | `order.dispatched` | the rider picks up an instant (Quick Pyaas) order | push, then whatsapp, then sms | push; or WA D-02; or DLT D-02 | INBOX-ONLY | Out for delivery — Ravi, ETA 12 min. |
| D-03 | `delivery.delayed` | a task on the road is 5 min past its window end (detector on the worker tick, once per task) | push, +whatsapp | push; WA D-03 | INBOX-ONLY | Running a little late — new ETA about 7:52 am. Sorry for the wait. |
| D-05 | `order.line_cancelled` | the store removes a line from an order before delivery | whatsapp, +sms | WA D-05; DLT D-05 | INBOX-ONLY | Full Cream Milk - Parag Gold 500ml — delivered by PYAAS is unavailable today; Rs35 has been taken off your bill and nothing is charged for it. Sorry! |
| D-06 | `order.delivered` | an order is delivered; never for a free promo pack (W-02 / W-05 own those) | push, then whatsapp, then sms | push; or WA D-06; or DLT D-06 | INBOX-ONLY | Delivered ✅ Full Cream Milk - Parag Gold 500ml — delivered by PYAAS. Enjoy! Tap to rate. |
| D-07 | `subscription.day_skipped` | the noon lock (12:00:05 on D) skips D+1 because the wallet as it stood at 12:00:00 did not cover it; once per member and day however many plans were skipped; not for a day anything else still arrives (another plan's locked order, a one-off morning order, a free Welcome Litre pack: CH-03 day 0), not for a day that is not tomorrow (a catch-up after an outage), not for a plan paused or cancelled after the cut-off. [DATE] is the next morning the plan delivers after the skipped one | sms, +whatsapp, +push | DLT D-07; WA D-07; push (all need the new wording registered) | INBOX-ONLY | No delivery tomorrow - your wallet was short at 12 noon. Recharge by 12 noon tomorrow and your milk resumes 8 Oct. |
| D-09 | `order.failed` | 15 min after the rider marks the task not delivered, or the store cancels it (the rider's undo window); skipped if the order is live again by then (undo or reassign); also when the sweep closes a locked morning order whose day passed undelivered (from noon the day after; [REASON] "the delivery day passed without a delivery") | push, then whatsapp, then sms | push; or WA D-09; or DLT D-09 | INBOX-ONLY | Not delivered - Full Cream Milk - Parag Gold 500ml — delivered by PYAAS could not reach you (customer not at home). Nothing has been charged for it. Tap to order again, or call 96672 60050. |
| E-01 | `rating.submitted` | the member rates an order 3 or lower | whatsapp | WA E-01 | INBOX-ONLY | Sorry we missed the mark. What went wrong? We'll make it right. |
| E-02 | `complaint.created` | every new complaint (a retried ref sends nothing) | whatsapp, then sms | WA E-02; or DLT E-02 | INBOX-ONLY | Got it — we're on it. You'll hear back within 24 hours with a fix. |
| E-04 | `complaint.created` | a complaint of category `missing` that names the member's own order (condition `complaint.has_order`; an order-less one gets E-02 only) | whatsapp, +human_call | WA E-04 (the refund body, T-E04) / WA T-E04-REDELIVER (its own key; without it that body is inbox only); the call-back needs nothing | INBOX-ONLY + HUMAN | Sorry! We'll redeliver Full Cream Milk - Parag Gold 500ml x2 — delivered by PYAAS or refund ₹70 to your Wallet — your choice. ([AMOUNT] is what the member paid for the goods: the delivery debit, or the cash a rider took; never the delivery fee.) / nothing paid yet (not delivered, cash not taken, a free day, a debit the rider's undo gave back), T-E04-REDELIVER: Sorry! We'll redeliver Full Cream Milk - Parag Gold 500ml x2 — delivered by PYAAS. Nothing has been charged for it. |
| E-05 | `complaint.created` | a complaint of category `quality` (milk quality / safety) | human_call, then sms | none: the call-back row is the delivery | HUMAN (no member message: the config forbids an automatic reply; E-02 still acknowledges) | (no template, by design) |
| E-06 | `complaint.resolved` | an operator marks a complaint `resolved` (a move straight to `closed` sends nothing) | whatsapp, then sms | WA E-06; or DLT E-06 | INBOX-ONLY | Fixed ✅ Thanks for your patience. Anything else? We're here. |
| E-07 | `rating.submitted` (promotional) | 4 h after a 5-star rating, with no open complaint, 10:00–21:00 (a due time outside the window waits for the next 10:00) | whatsapp | WA E-07 + the member's `marketing_whatsapp` grant | CONSENT | Thank you! Love Pyaas? Refer a neighbour — you both get Pyaas credit after their first paid order. |
| W-01 | `offer.finalized` | a Welcome Litre enrolment completes (promoter or the app's own funnel). Pack 1 comes on the first open morning (tomorrow before 12 noon, the day after tomorrow from noon), and the body follows it (`offer.pack1_tomorrow`, read when W-01 is sent): tomorrow, T-W01; any other morning, T-W01-LATER naming it | whatsapp, +push, then sms | T-W01: DLT W-01 (mapped); WA W-01; push. T-W01-LATER: not registered, push only | FIRES + SMS (tomorrow); INBOX-ONLY (a later morning) | You're set. Tomorrow by 7 am: 500 ml Parag Full Cream — delivered by PYAAS, free. No payment now. Recharge Rs500 any time in the next 7 days and your second 500 ml pack comes free with your first paid delivery. Pause or cancel any time. / later: You're set. 8 Oct by 7 am: 500 ml Parag Full Cream — … (same words) |
| W-02 | `order.delivered` (pack 1) | the free pack 1 is delivered | push, then whatsapp | push; or WA W-02 | INBOX-ONLY | Delivered. 500 ml Parag Full Cream — delivered by PYAAS. That one's on us. Want milk tomorrow too? Recharge by 12 noon today. |
| W-03a | `30 10 * * *` | day 0 (pack 1 landed today), after 10:30, if the member has not topped up | whatsapp, +push | WA W-03a; push | INBOX-ONLY | Milk tomorrow? Recharge by 12 noon today. Rs500 is about 8 mornings. Pause or cancel any time. https://pyaasdairy.com/app |
| W-03b | `32 10 * * *` (promotional) | day 0, after 10:32, while pack 2 is locked | whatsapp | WA W-03b + the member's `marketing_whatsapp` grant | CONSENT (owner decision 24 Sep: stays consent-gated) | And your second 500 ml Full Cream is waiting — it comes free with your first paid delivery once you recharge Rs500. Valid 7 days. https://pyaasdairy.com/app STOP to opt out. |
| W-04 | `wallet.recharge_settled` | a settled top-up of ₹500 or more within the 7-day window, once pack 2 is attached to tomorrow's delivery | whatsapp, +push | WA W-04; push | INBOX-ONLY | Recharge received. Your second 500 ml Parag Full Cream — delivered by PYAAS — is scheduled free with tomorrow's delivery. |
| W-05 | `order.delivered` (pack 2) | the free pack 2 is delivered | push, then whatsapp | push; or WA W-05 | INBOX-ONLY | Delivered. Full Cream Milk - Parag Gold 500ml — delivered by PYAAS plus your second free 500 ml. That completes the welcome offer — from here it's just milk, every morning. |
| W-06 | `30 10 * * *` | days 3 and 5 after pack 1, after 10:30, while pack 2 is locked (a run that only comes after 22:00 sends nothing that night) | whatsapp | WA W-06 | INBOX-ONLY | Your second 500 ml Full Cream is still waiting — recharge Rs500 by 1 Oct and it comes free with your next delivery. https://pyaasdairy.com/app |
| W-07 | `30 10 * * *` | from day 8 (the 7-day window is over), after 10:30, while pack 2 is locked; the pack expires only once this is SENT (a run that only comes after 22:00 sends nothing that night: the next 10:30 sends it and expires the pack) | whatsapp, then sms | DLT W-07 (mapped); WA W-07 | FIRES + SMS | Your second free pack has expired — the 7 days are up. Nothing has been charged. Whenever you'd like milk again, it's one tap in the app. https://pyaasdairy.com/app |
| W-08 | `serviceability.checked` | a signed-in member's check comes back out of zone (self-enrol refused, or the waitlist join with a session), and only for a member we cannot serve: no order at all and no saved address inside a zone, both decided when the check is made (a member with orders or a serviceable home who checks a far pin gets nothing; an event without these facts fails closed); once per member ever | whatsapp, then sms | WA W-08; or DLT W-08 | INBOX-ONLY | We don't deliver to your area yet. We've noted your pincode and we'll tell you the day we do — that's the only message you'll get from us. |
| W-09 | `abuse_flag_raised` (internal) | a second Welcome Litre enrolment at an address already enrolled | admin_console | none | OPERATOR | (operator bell: "address_hash match — second offer at the same address (review, do not auto-reject)") |
| W-10 | `0 18 * * *` (internal) | first tick after 18:00: free packs issued vs enrolments that day | admin_console, +email | none (email not built) | OPERATOR (bell only on a variance) | (operator bell: "packs issued N vs consumers created M — variance V") |
| FF-01 | `founding.farm_unlocked` | the join that fills a Founding Family farm unlocks it: every waiting member of that farm. [DATE] is the first morning an order placed then reaches: tomorrow before 12 noon, the day after from noon | push, then whatsapp, then sms | push; or WA FF-01; or DLT FF-01 | INBOX-ONLY | Gonard Dairy is unlocked! Whole Farm Milk from Harsh Singh and the full PYAAS range are open for you. First delivery tomorrow by 7 AM. / after noon: … First delivery 8 Oct by 7 AM. |
| FF-02 | `founding.member_active` | with FF-01, for each waiting member who turns Active at that unlock | push, then whatsapp, then sms | push; or WA FF-02; or DLT FF-02 | INBOX-ONLY | You are in. Your Founding Family price is on at Gonard Dairy: Rs 2 off every litre of PYAAS milk and free delivery, every morning. Stop any month in Me > Founding Family. |
| FF-03 | `founding.seat_waiting` | a Rs 99 join takes a seat on a farm still filling | push, then whatsapp, then sms | push; or WA FF-03; or DLT FF-03 | INBOX-ONLY | Your seat at Gonard Dairy is held: you are #12 in line. 53 more homes and it unlocks. Share your link with your society group. |
| FF-04 | `founding.line_moved` | a friend who applied the member's code pays the Rs 99 (a Founding Family join that moved money, once per referral ever) while the member waits on a farm still filling: the member swaps places with the waiting member directly ahead on their own farm's line. Not sent to an active member, a stopped one, or one already first in line (nothing moves). The Rs 100 referral credit (B-06) is separate | push, then whatsapp, then sms | push; or WA FF-04; or DLT FF-04 (T-FF04-UNLOCKED is its own registration) | INBOX-ONLY | Your friend Neha joined. You moved up to #11. They claimed Mishra Dairy: 79 more to unlock. / when the friend's join unlocked their farm (T-FF04-UNLOCKED): … They claimed Mishra Dairy, and it has unlocked. The friend is named by first name, else "(number ending 1234)". |

## 2. Awaiting a product event (21) and the alias

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
| D-08 | `route.changed` | whatsapp, +sms | zones carry no effective date and their edits emit no event |
| E-03 | `delivery.late_confirmed` | whatsapp | no lateness confirmation and no production grant path for the Rs20 apology credit |
| F-01 .. F-09 | schedules | ai_call | ai_call is not a transport (F-02 also needs a server cart; F-05 a high_value segment) |

**Events recorded with no message yet** (in the outbox, no trigger, so nothing is sent
and no count above changes): `referral.applied`, `referral.rewarded`, and
`founding.pyaas_plan_paused`. The last is the member's PYAAS milk plan that the server
paused (`pause_reason: founding_required`) on the first morning
`FOUNDING_PYAAS_MEMBERS_ONLY` refused it because their Founding Family perks did not
cover it. It is emitted once per pause, with `subscription_id`, `product_id`, `day` (the
first refused morning) and `member_status`. The plan shows as paused in the app, and a
resume works again once the perks cover the next morning. Its message (FF-05) waits for
the founder's copy. Add it then as an event trigger on this topic (section 4).

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
| rider undo within 15 minutes | B-06 | "₹<debit> added to your Wallet (delivery 5D05C6 reversed). Refundable." (the order's short code: the last six characters of its id in capitals, so [REASON] fits a 30-character DLT variable) | case 7 |
| AutoPay mandate execution | none | it DEBITS the wallet (subscription auto-renewal), so no "added" message; a short wallet sends B-03 | case 8, `TestCRMEmitPaymentFailed` |
| a credit whose balance update fails | none | no message for money that never arrived (fixed in `f7384d9`) | case 9 |
| referral reward (both sides, once the referee's first paid delivery has outlived the rider's 15-minute undo window) | B-06 (T-B05-PROMO) | one each: "₹100 Pyaas credit added (Referral reward: …). Usable on orders; not refundable in cash." | case 10 |

A Welcome Litre household that tops up ₹500 or more inside its 7-day window also gets
W-04 for the same recharge: that one is about the free pack, B-06 is the receipt.

**Registering B-06 for SMS.** Two bodies, two registrations. T-B05-REFUND (key `B-06` or
`T-B05-REFUND`): "₹##x## added to your Wallet (##reason##). Refundable. Ready for your
next order." (hi_roman: "₹##x## aapke Wallet mein (##reason##) jama. Refundable. Agle
order ke liye taiyaar."). T-B05-PROMO (key `T-B05-PROMO`): "₹##x## Pyaas credit added
(##reason##). Usable on orders; not refundable in cash." The SMS goes
with the hi_roman body when one exists, so register that wording (or map
`{"en":…,"hi":…}`). `##x##` is the rupee amount with no symbol or decimals for a whole
amount ("500"); `##reason##` is "recharge" for a top-up, the refund remark, "delivery
<6-char code> reversed" for a rider undo (24 characters), or the promo remark. A DLT
variable holds at most 30 characters: the top-up and undo reasons fit, but the referral
rewards' remarks do not ("Referral reward: a family you invited took their first
delivery" is 64, "Referral reward: welcome to PYAAS" 33), so shorten them before
T-B05-PROMO is mapped. Until it has its own key, a promo credit reaches the inbox and
push only.

## 4. How to add a message

The engine is data-driven: most new messages are a config change only.

1. **Write the trigger and its template** in `internal/modules/consumer/crm_triggers.json`
   (and bump `meta.trigger_count`; `crm_test.go` and `crm_tokens_test.go` pin the counts):
   `kind: "event"`, `event: "<topic>"`, `category` (`service_implicit`, `transactional`,
   or `promotional`, which needs consent, the 10:00–21:00 window and never goes by SMS;
   a member message waits out the quiet hours unless `guards.G5_quiet_hours.quiet_hours.always_send`
   covers it by category, section, id or `critical: true`, and `crm_quiet_hours_test.go`
   pins every live trigger's class, so add it there too),
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
4. **Let it leave the building**: register the SMS template on DLT, create it in MSG91,
   and add MSG91's template id to `CRM_DLT_TEMPLATE_IDS` under the trigger id (or, for a
   second body of the same trigger, under its template id); get the WhatsApp template
   approved and add its name to `CRM_WA_TEMPLATE_NAMES` the same way. Without these it is
   inbox (and push, once enabled) only, by design. A refused send shows up as
   `channel_errors` on the dispatch row.
5. **Update this file**: a row in section 1 with its status today.

## 5. Quiet hours (founder decision 6, 25 Sep 2026)

22:00 to 07:00 IST: "order, delivery and money added always send; offers and the
quirky taglines wait for morning". The hours are
`guards.G5_quiet_hours.windows.service_transactional.avoid`; what still goes out inside
them is `guards.G5_quiet_hours.quiet_hours.always_send` (`crm_schedule.go`
`crmSendWindow`). Change either there, not in Go.

| class | live triggers | inside the quiet hours |
|---|---|---|
| always send: money (category `transactional`), `critical`, section D (order and delivery), E (a complaint or rating and its answer), and by id A-02, A-03, FF-01..FF-03 (the member's own Rs 99 seat), W-01, W-02, W-04, W-05, W-08 | A-02, A-03, B-02, B-03, B-06, D-01, D-02, D-03, D-05, D-06, D-07, D-09, E-01, E-02, E-04, E-05, E-06, W-01, W-02, W-03a, W-04, W-05, W-08, FF-01, FF-02, FF-03 (and the operator's W-09, W-10) | sent at once, as by day: a 05:30 delivery's D-06, a 02:00 top-up's B-06 |
| deferred | A-01, A-05, B-01, C-03, FF-04, W-06, W-07 | waits for 07:00; never dropped. FF-04 (a friend's Rs 99 moved the member up the line) is news about someone else, so it is not in the FF-01..FF-03 always-send ids |
| promotional (never always send) | E-07, W-03b | only inside 10:00 to 21:00, which lies inside the 07:00 to 22:00 day |

A trigger added later is deferred unless the rule covers it; `crm_quiet_hours_test.go`
fails until its class is written down there (and here).

How a message waits:

- **Event trigger** (A-01, A-05, C-03, or a future one with no delay): a `crm_schedules`
  row due at 07:00, reason "waiting for the quiet hours to end". At 07:00 its conditions,
  the stale-event check and every guard run again, exactly as after a delay: A-01 two
  hours after a 21:00 sign-up comes at 07:00, and not at all if the member ordered or
  recharged overnight.
- **Scheduled sweep** (B-01 from 09:00, W-06 and W-07 from 10:30): these reach the quiet
  hours only when the worker was down until after 22:00. That run sends nothing and does
  not use up the day's claim; the next run decides again: B-01 at the next 09:00, W-07
  (and the pack's expiry, which waits for it) at the next 10:30. W-06's day-3 nudge is
  then not sent; its day-5 one is.
- **Promotional**: a delayed one (E-07) waits for the next 10:00, as before; a scheduled
  one caught outside its window (W-03b) is suppressed and logged `G5_quiet_hours`, as
  before.
- **Not held**: an operator's manual send (a person chose the moment; a promotional one
  still needs the window) and the operator bell.

The app's own notifications keep the same hours (consumer `lib/quietHours.ts`): the
taglines come every 2 hours from 07:00 to 21:00 IST, 8 a day instead of 12, and a cart
reminder that would land between 22:00 and 07:00 comes at 07:00.

## 6. Consent (founder decision 5, 25 Sep 2026)

Consent defaults UNTICKED (DPDP "clear affirmative action", TRAI's promotional rules,
Apple). Delivery, wallet and order messages are service and keep flowing; offers are asked
for in context after the first delivery.

- **The server grants nothing by itself.** The promotional gate (G2) and the per-channel
  gate (G2b) read only `consumer_consents`, which only `POST /users/me/consents` writes;
  the derived `promotional` row is active only while some `marketing_*` row is an active
  grant (`consents.go` `recomputePromoConsent`). OTP sign-up, the profile step, a
  promoter's Welcome Litre enrolment, `privacy_terms`, the phone and location disclosures,
  reading the state back, and a batch with every marketing channel `granted:false` never
  open it (`consents_explicit_grant_test.go`).
- **The pre-ticked box is in the app.** The sign-up screen's "Send me offers and updates"
  starts ticked (`app/complete-profile.tsx` `useState(true)`, and
  `components/ConsentSheet.tsx` `defaultChoices()` returns offers, WhatsApp and SMS on),
  so a member who does not untick it is recorded as granting offers on push, WhatsApp and
  SMS. Both are screens (Kushagra's); the one-line changes are in the decisions report.
  Until the build with them ships, nothing else changes.
- **Grants already recorded under the pre-ticked box cannot be told apart** from a tick
  the member made: both arrive as `granted:true`. What the log does show: the sign-up
  form records every choice in one batch, so a `marketing_*` row in
  `consumer_consent_log` with `granted:true` and the SAME `occurred_at` as the member's
  first `privacy_terms` row came from that form; its `app_version` says which build. A
  grant with its own `occurred_at` is a switch flipped later in Message preferences, which
  is an affirmative action. Read-only query:

  ```js
  db.consumer_consent_log.aggregate([
    {$match: {kind: "privacy_terms", granted: true}},
    {$sort: {occurred_at: 1}},
    {$group: {_id: "$consumer_id", first: {$first: "$occurred_at"}, app: {$first: "$app_version"}}},
    {$lookup: {from: "consumer_consent_log", let: {c: "$_id", t: "$first"}, as: "form_grants",
      pipeline: [{$match: {$expr: {$and: [{$eq: ["$consumer_id", "$$c"]}, {$eq: ["$occurred_at", "$$t"]},
        {$eq: ["$granted", true]}, {$eq: [{$substrCP: ["$kind", 0, 10]}, "marketing_"]}]}}}]}},
    {$match: {"form_grants.0": {$exists: true}}},
  ])
  ```
- **A Message-preferences save re-sent every switch** in consumer builds before `e4e9b88`
  (the shipped release/26.07.03 build included). Each save records the whole set of
  switches, and those builds sent every type with the save's time; the server treats a
  strictly newer grant as a re-grant, so it moved each untouched grant's 7-day anchor to
  the save and logged another `granted:true` row. In the log, a `granted:true` row whose
  kind's previous row was already `granted:true` is such a re-send, not a tick; the switch
  the member flipped is the kind whose value changed at that `occurred_at`. From
  `e4e9b88` the app sends each type with the time the member last changed it
  (`lib/consentSync.ts` `consentChangeRecords`), so an untouched switch is a no-op here.
- **What those grants still reach.** The server's promotional messages need a grant under
  7 days old (G2 `explicit_consent_ttl_days`), so a sign-up grant stops opening them 7
  days after sign-up. The app's taglines and cart reminder read the current state with no
  expiry, so they keep running for such a member until Offers is switched off. A
  re-consent prompt for those members is the founder's call (a screen).
- **Asking in context.** Consumer `lib/offersAsk.ts` decides when the "Send me offers?"
  prompt is due (signed in, a delivered order, offers not granted, not asked on this
  device) and remembers the answer; the prompt itself is a screen.
