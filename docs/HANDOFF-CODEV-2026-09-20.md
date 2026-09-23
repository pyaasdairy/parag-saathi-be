# Handoff — delivery integration, audit findings and the work left

**Date:** 20 September 2026
**For:** the co-developer picking this up
**From:** the integration + audit pass on all three repos

Everything described here is **committed locally and pushed nowhere**. Nothing is
deployed. Read §1 and §2 first; they are the two things that will bite you if you
skip them.

---

## 1. Where the code is

| Repo | Branch | Tip | What it holds |
|---|---|---|---|
| `parag-saathi-be` | `integration/delivery` | `94297c7` | `release/26.07.03` + your `feature/simple-delivery` bundle + 4 fix commits |
| `pyaas-saathi` | `integration/delivery` | `c18e50a` | `UI-REVAMP` **merged** with your bundle + 4 fix commits |
| `pyaas-consumer` | `feature/consumer-revamp-phase2` | `26d20c3` | your 18–20 Sep push + 2 fix commits |

Your two bundles are still in `PYAAS-codev-handoff/` (untracked). They were brought
in as `codev/simple-delivery` and `codev/ui-revamp` and merged, **never reset** — a
reset would have deleted `953bfa1` (sign-out no longer destroys milk records), which
your bundle forked before.

**Verification state, all green:**

```bash
# backend — needs a local Mongo for the integration tests
D:\dev\tools\mongodb\bin\mongod.exe --dbpath D:\dev\data\mongo --port 27017
cd parag-saathi-be
CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 go test ./... -count=1   # all packages ok
go vet ./...                                                               # clean

cd ../pyaas-saathi && flutter analyze && flutter test                      # clean, 59 pass
cd ../pyaas-consumer && npx tsc --noEmit                                   # clean
```

Without `CONSUMER_MONGO_TEST_URI` the consumer package's Mongo tests **skip
silently** and a green run proves almost nothing. Always set it.

---

## 2. Read this before you deploy anything

1. **Merging into `release/26.07.03` IS deploying.** `render.yaml` pins that branch.
2. **Set `ADMIN_API_KEY`** (32+ random chars) in the Render dashboard **and** on
   Vercel as a server-side variable. It is `sync: false`, so Render will not create
   it. Without it, key auth is off and the website gets 401s from a service that
   looks perfectly healthy. The boot log now says which door is open — look for
   `admin CRM: key auth ENABLED`.
3. **Redeploy BOTH Render services** (`d8ee` and `xcxn`) against the one Atlas DB.
   Deploying one and not the other is exactly how the rider console 404'd in
   September.
4. **Check every active STORE org-unit has `geo_lat` / `geo_lng`** before the first
   order. See §3.1 for why.
5. **`pyaas-secrets-backup-2026-09-02.zip` is sitting in the backend repo root,
   untracked.** One `git add -A` publishes your secrets. Move it out of the repo
   directory today.

---

## 3. What was fixed, and why (so you do not undo it)

### 3.1 The console blindness — `delivery.go`

`listDeliveries` used to sort `assigned_at` ASC with `SetLimit(500)`. Once a store
passed 500 lifetime tasks it returned its 500 **oldest** rows, so every new order
was missing from the queue while the screen said there was no work. Proven from
production data: one store crossed that line on 9 Aug and **3,432 later tasks**,
including 23 of 29 instant orders, never reached a phone.

It is now **two queries**, deliberately:

- open work (`deliveryOpenStatuses`) — its own query, **never capped**;
- finished work — separate, `updated_at` within `deliveryHistoryDays` (3), capped 500.

Do not "simplify" this back into one query with an `$or` and a limit. Whatever such a
query sorts by, the cap eventually cuts something, and sorted newest-first it cuts the
oldest rows — which is precisely an old stuck `OFFERED`/`ASSIGNED` task.

Indexes for both paths are in `ensureDeliveryIndexes`.

### 3.2 `nearestStore` must never refuse

An earlier version of my fix filtered out stores with no coordinates and returned an
error when none remained. That made `createDeliveryForOrder` mint **no task at all** —
invisible order, no console, no rider — while serviceability deliberately fails *open*
in the same state ("never go dark"). Ordering would stay on while fulfilment went
silent.

The geo filter now **ranks only**. With no usable store it still routes to the first
one and returns `errNoStoreGeo`, which the caller logs at ERROR and continues. If you
see that log line, fix the store's master data; do not make the function refuse again.

### 3.3 Pack size — `variant`, never inside the name

The catalog keeps the size in `variant` and the name is only the product, so both
consoles read "Full Cream Milk - Parag Gold ×2" with no way to tell 500 ml from 1 L.

`deliveryItemsFor` is now the single copy path for both writers and carries
`product_id` + `variant`. **The wire `name` stays verbatim.** Folding "(500ml)" into
the name is tempting — it would show the size on builds already installed — but the
Saathi store screen reconciles stock by *exact* name match
(`models/store.dart` `matches()` / `canon()`), so a decorated name silently stops
"held", "delivered" and "sold today" counting against any product row. Both apps now
join `name` + `variant` at render time (`DeliveryItem.label`).

### 3.4 The bell

Three writers send `ring_bell`, and each defaults it to `false`:

- the account's standing prefs **hard-code** it (`lib/deliveryPrefs.ts:18`) and post it
  on every save of any unrelated field → a `false` there means nothing, and is dropped
  by `bellSaid()`;
- the address capture shows a **visibly preselected** "Hang it outside" → that `false`
  is a real instruction and is kept;
- the cart derives `handover: HAND_TO_CUSTOMER` from the same untouched switch.

That last pair is why an ordinary order used to show a red "Do NOT ring the bell"
directly above "Hand to the customer". Both the rider step text
(`riderHandoverSubtitle`) and the app chip (`delivery_prefs_view.dart`) now suppress
the warning **only** when it sits beside an explicit `HAND_TO_CUSTOMER`. With `DROP`,
or with no handover mode, a genuine "do not ring" still reaches the rider.

`TestBellInstructionRules` pins all of this.

### 3.5 Everything else fixed

**Backend**
- Doorstep note was dropped: the app sends `notes`, the backend read `note`. Now reads
  `note` / `notes` / `instructions`.
- Prefs precedence is account → address → order (least to most specific).
- The 300 m fence is **measured server-side**, not just asserted by the phone, and only
  when the task carries the customer's own pin (`GeoExact`) — an order without
  coordinates points the task at the store, where a rider at the real door is
  legitimately far away.
- `pickup_address` / `pickup_geo` are written at pickup (declared and rendered by the
  admin CRM, never populated before).
- `GET /orders` signs proof photos like `GET /orders/{id}` did.
- Admin CRM: GET reads are audited (7 of 10 routes are GETs that export the customer
  book), assign is scoped to the store's own riders, dashboard and duplicate queries
  are bounded, boot logs the auth mode.
- The at-handover item adjust keys on `product_id`, falling back to the bare name for
  older clients. Two pack sizes of one product share a name, and a name-keyed edit
  rewrote **both** lines and re-billed the customer.
- A subscription day claimed but never created is released and logged at ERROR instead
  of being skipped forever in silence.
- **New:** complaints API + admin surface, push device registry, structured address
  (`society`/`society_id`/`tower`/`floor`/`unit`) stored, returned and copied onto the
  task. Both new collections join the erasure cascade.

**pyaas-saathi**
- `ALREADY_DELIVERED` / `ORDER_CANCELLED` close the delivery screen. The rider used to
  be stuck forever: the idempotency key is dropped on success, so every retry read as a
  fresh delivery on a delivered task and was refused again.
- The offer swipe no longer animates "accepted" for the rider who lost the race.
- Zone tab infinite-width crash (the same pattern that blanked the Catalog tab).
- Pickup card survives an empty queue — 4:30 AM is when the rider needs it.
- Route numbers stop reshuffling between polls (tie-break on task id).
- Long-press + confirm as a non-drag path.

**pyaas-consumer**
- The settle sweep no longer fires inside the rider's 15-minute undo window, and a
  **missing** `delivered_at` now means *wait*, not *charge*.
- The rating sheet offers the store to every rating (filtering by sentiment is banned
  by both stores, and this app has been removed once).
- The cart quotes the ₹500 it will actually ask for.
- "Auto top-up" → "Low-balance reminder", disclosure before the switch.
- Reminder + rating flags keyed per account.
- `.specstory/` gitignored (it holds full AI session transcripts).

---

## 4. Push notifications — the honest state

**Nothing server-initiated reaches anybody.** There is no FCM/APNs sender in the Go
tree. What exists:

| Layer | Who sees it |
|---|---|
| In-app feed + bell (local rows) | App open, or retroactively when next opened |
| CRM inbox merged into the same feed | App open |
| OS **local** notification | Only while the JS process is alive |
| OS **remote** push | **Nobody. Does not exist.** |

At 5:30 AM, with the app killed, a delivered order produces **silence**.

`POST /consumer/push/register` now exists (`push.go`) and matches what the app sends:
`token`, `platform`, `provider`. It stores one row per token, moving the token to its
newest owner — a handset changes hands, and sending to the previous owner would put
one member's order news on another member's phone. The response says
`"delivery":"pending_sender"` so the state is documented rather than mysterious.

**To make push real, in order:**

1. **`eas init`** in `pyaas-consumer` — `app.json` has no `extra.eas.projectId`, and
   `getExpoPushTokenAsync()` is called bare, so it **throws on every device** and zero
   tokens exist today. *(Founder — account-level.)*
2. **Firebase project + `google-services.json` + `android.googleServicesFile`.**
   *(Founder / external.)*
3. **APNs key** in EAS credentials for iOS. *(Founder / external.)*
4. **A sender in the backend** (FCM v1 or Expo push) plus call sites on
   `out_for_delivery`, `delivered` and CRM dispatch. *(Dev.)*
5. **Call `registerForPush()` at boot and after sign-in**, not only from the settings
   screen. *(Dev.)*
6. **A tap handler** — nothing registers `addNotificationResponseReceivedListener`, so
   the `href` packed into each notification goes nowhere. *(Dev.)*
7. **Unbind on sign-out** — add `DELETE /consumer/push/register` and call it, or a
   shared phone keeps notifying the previous member. *(Dev.)*
8. **An Android notification icon** — without one Android draws a white square.

---

## 5. CRM / DLT — what actually sends today

`crm_triggers.json` ships **56 trigger ids**. **11 have a dispatch call site** in Go.
The other **45 are dead config** — every A-, C-, D-, E-, F- trigger, including the
whole order lifecycle (confirmed, out for delivery, delivered, delayed, skipped) and
every complaint trigger. They fire on no channel at all.

Environment: `CRM_DLT_TEMPLATE_IDS` maps **only W-01 and W-07**; `CRM_MSG91_SENDER` is
empty; there are **no `CRM_WA_*` keys**.

| id | Fires (IST) | Sends today? | Why |
|---|---|---|---|
| **W-01** enrolment | instant, 24×7 | **Yes — inbox + SMS** | DLT id mapped; WhatsApp is primary but nil, so it falls to SMS immediately |
| **W-02** pack-1 delivered | delivery +≤60 s | Inbox only | primary is push (dead), no DLT id |
| **W-03a** day 0, zero balance | first tick ≥ 10:30 | Inbox only | no DLT id |
| **W-03b** day 0, pack 2 locked | first tick ≥ 10:32 | Silent skip for almost everyone | needs a `marketing_*` consent < 7 days old; SMS refuses promotional outright |
| **W-04** pack 2 minted | on mint, any hour | Inbox only | no DLT id |
| **W-05** pack-2 delivered | delivery +≤60 s | Inbox only | no DLT id |
| **W-06** days 3 and 5 | first tick ≥ 10:30 | Inbox only | no DLT id |
| **W-07** offer expiring | first tick ≥ 10:30, day 8+ | **Yes — inbox + SMS** | DLT id mapped |
| **B-01** low cover | first tick ≥ 09:00, ≤1/7 days | Inbox only | no DLT id |
| **B-02** short for tomorrow | first tick ≥ 12:00 | Inbox only, SMS refused | warn-logged "no DLT template id mapped" |
| **W-09 / W-10** | on enrol / ≥ 18:00 | Admin bell only | bypasses the guard chain by design |
| **manual** `POST /crm/message` | operator click | Inbox only, never SMS | |
| **the other 45** | — | **Never** | no call site exists |

**In one sentence: exactly two SMS messages can reach a customer — the enrolment
welcome (W-01) and the offer-expiring notice (W-07).** Everything else lands in the
in-app inbox and waits for the member to open the app and tap the bell. With no push,
"the member was notified" means "the member will find out if they look".

The empty `CRM_MSG91_SENDER` is **not** the problem — MSG91 takes the header from the
DLT flow registration. Confirm at MSG91 which header W-01 and W-07 were approved
under; the code will not tell you if it is wrong.

Timing mechanics that **are** sound: 60-second worker tick, IST anchored correctly,
each trigger claimed exactly-once per `(trigger_id, consumer_id, ist_day)`, a missed
minute fires late rather than never, and W-07 goes out before the irreversible pack-2
expiry.

### CRM bugs to fix (not yet done)

1. **HIGH — W-07 can be lost while the pack expires anyway.** `crm_engine.go:845-849`:
   `crmDispatchAt` returns nothing and `transitionPack(pack2Locked → pack2Expired)`
   runs unconditionally on the next line. If the dispatch is suppressed — or the claim
   insert hits a blip, leaving no dispatch row at all — the member's free pack silently
   disappears with no notice. Only expire when the dispatch records SENT.
2. **HIGH — B-02 states a deadline that has already passed.** The body says "recharge
   by 12 noon tomorrow" (`crm_triggers.json:290`) but the code evaluates *tomorrow's*
   order at 12:00 *today* (`crm_engine.go:950`), and the rider leaves at ~05:00. The
   config intended a D-2 horizon. **Fix the horizon or the copy — not both
   independently.** This one needs the founder's call.
3. **MEDIUM — W-01's SMS runs inline on the HTTP request** (`crm_offers.go:571`,
   10-second MSG91 POST on `r.Context()`). If the app backgrounds mid-enrol the member
   gets nothing, and the dispatch row is stuck at `CLAIMED` with the day's claim burned.
   Move it to the worker.
4. **MEDIUM — the manual operator send has no caps, dedup or quiet hours** when it is
   `service_implicit` (`crm_engine.go:308`, `:485-487`).
5. **MEDIUM — the WhatsApp gap is invisible:** a nil transport is skipped with no log,
   and the dispatch row reads `SENT / inapp` as if inbox had always been the plan.
6. **Unresolved:** W-03b needs a promotional consent row < 7 days old. One reading says
   `recomputePromoConsentOnce` derives it from the member's `marketing_*` grants;
   another says nothing writes it. If the second is right, **W-03b never sends on any
   channel**. Worth 20 minutes against the live DB before relying on it.

---

## 6. Work still open

**Backend (dev)**
- `GET /orders` signs proof photos serially, up to 200 orders — sign the newest ~10 or
  do it concurrently.
- The CRM bugs in §5.
- Wire the order-lifecycle and complaint triggers (D-01/02/06, E-02/06) — customers get
  no order-confirmed, out-for-delivery, delivered or complaint-received message today.
- **Upcoming days:** the store console cannot see tomorrow's subscription round. A
  preview has no delivery task until the midnight lock, and the console only reads
  tasks. Two options, founder's choice: expose the previews read-only, or create the
  task earlier in a `SCHEDULED` state the rider cannot start.
- Resolve an order's address by **id**, not label — the `addrs[0]` fallback can stamp
  the wrong tower/flat onto a task.
- Copy `society_id` onto the task and group the round by tower/floor.
- Unique index on `(subscription_id, scheduled_for)` as a DB backstop; `crmClaimDispatch`
  lets dispatch run when its index build failed.
- `etaAt` on the consumer order (without it the app's countdown sticks on "Arriving any
  moment"); rider position on `GET /orders/{id}`.
- Complaint photos arrive as a device-local `file://` URI that is never uploaded.

**pyaas-saathi**
- Cut a new build — the pack size only appears on a fresh one.
- The new-order snack compares list *length*, and the list is now a rolling window, so
  a task ageing out on the same poll as a new order hides it. Diff task ids.
- Send `product_id` with the item adjust (the backend already prefers it).
- Stale doc comment in `lib/models/store.dart:15-17`.

**pyaas-consumer**
- Push items 1–8 in §4.
- Stale "Delivered" burst on an account switch.
- `autoTopup` legacy-key migration is read-only — re-save under the scoped key and call
  `clearAutoTopup` on sign-out (it currently has no call sites).
- Opening the notifications screen re-POSTs `read` for every CRM row, every time.
- Cart ₹500 copy still overstates the unlock requirement (the gate is ₹100; ₹500 is the
  minimum top-up).

**Founder / external**
- `eas init`, Firebase project, APNs key, `ADMIN_API_KEY`, DLT registration for the
  remaining templates, `CRM_WA_*` keys, Terms/Privacy PDFs (still print `99996 80081`),
  Welcome Litre artwork (3 creatives out of rotation), and the decisions below.

**Decisions waiting**
- B-02: change the horizon or the copy?
- Should attendance gate the rider queue? (Today it does not.)
- Should the store console be able to hand-assign an unclaimed `OFFERED` order?
  (Today nobody can; the admin CRM can.)
- Delete or keep the now-unused rider endpoints `/otp/send`, `/otp/verify`,
  `/door-photo`, `/compliance` — **`/scan` and `/undo` must survive**, the inventory
  screen and the 15-minute undo still use them.

---

## 7. Test on a device, in this order

1. **Place a real order and confirm a task appears in the store console.** The canary
   for §3.2. Check the store org-units have coordinates first.
2. **A customer with "Ring the bell" ON**, whose address was saved without touching the
   chip: read the card *and* the step text. They must agree.
3. **`GET /orders` on the account with the longest delivered history, cold cache** —
   time it, watch for "proof photo url failed" in the Render logs.
4. **Leave a store console open across the 3-day boundary** with steady volume: the
   new-order snack still fires, and an old `ASSIGNED` task is still listed.
5. **Store adjust on an order with both a 500 ml and a 1 L pack of one product** — the
   steppers must not move together, and the bill must be right.
6. **Shared phone:** A sets the low-balance reminder and signs out, B signs in without
   killing the app. B inherits nothing.
7. **Swipe vs long-press on the delivered bar**, repeatedly, on a slow network: exactly
   one submission, and `ALREADY_DELIVERED` closes cleanly.
8. **Enrol a customer in Welcome Litre** — the W-01 SMS arrives, under the expected
   header. Note how long the button hangs (see §5 bug 3).
9. **Boot log:** `admin CRM: key auth ENABLED`.

---

## 8. Conventions worth keeping

- The **backend is the source of truth**; the apps persist nothing but the cart.
- Money moves **only at delivery**, keyed `delivery:<order_id>` — the uniqueness of that
  ledger row is the whole exactly-once guarantee.
- The rider flow is **photo + geotag**, by the founder's decision on 20 Sep. No customer
  OTP, no scan, no door photo, no note in the delivery flow.
- Never decorate a wire `name` that something else matches on.
- New Mongo-backed tests belong behind `CONSUMER_MONGO_TEST_URI`, using
  `newChainWorld(t)` from `fullchain_e2e_test.go`.

---

## 9. 24 September 2026: referrals, Founding Family, the noon cut-off

**Branch:** `feature/founding-referrals` (from `integration/delivery` at `80f4a0b`).
**Spec:** the consumer app on `origin/feature/ui-revamp` `5f92d2e` is the contract
(`lib/referrals.ts`, `lib/foundingFamily.ts`, `lib/subscriptions.ts`,
`lib/orderTracking.ts`, `pyaas-app-spec.md`, `pyaas-one-voice.md`,
`HANDOFF-FRONTEND-2026-09-21.md` in that commit). Everything is additive: no
path, key, status or error code the app already reads was renamed or removed.

Verification, as before but on a private Mongo so the fixed database names do not
collide with another run:

```bash
mongod --port 27018 --dbpath D:/dev/cache/mongo-wt1
go build ./... && go vet ./... && CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 go test ./... -count=1
```

### 9.1 Referrals (`referrals.go`)

| Route (under `/consumer`) | Body / reply |
|---|---|
| `GET /referrals/code` | `{code}` |
| `GET /referrals` | `[{id, name, status, reward_amount, created_at}]`, newest first |
| `POST /referrals/apply` | `{code}` -> `{id, code, status, reward_amount, created_at}` |

- The code is the app's own `codeFromUid` derivation (`h = h*31 + charCode`,
  unsigned 32-bit, `("PG" + base36 upper + "XXXX").slice(0, 6)`) over the consumer
  id, minted lazily into the account's `referral_code` field. A code shared from the
  phone before the server ever minted it still resolves: `applyReferral` derives
  the code for every account without one and stores the match.
- `status` is `pending` until the referee's first delivered order, then
  `credited`: that is the app's `ReferralStatus` union (`refer.tsx` branches on
  `credited`), so the task's `rewarded` wording lives in the CRM event name
  (`referral.rewarded`), not on the wire.
- Reward: `REFERRAL_REWARD_PAISE` (default 10000, the Refer screen's "Gift Rs 100,
  get Rs 100") credited to the REWARDS bucket of BOTH sides from the delivered sync
  (`syncOrderDelivered` -> `rewardReferralOnDelivery`), exactly once per referral
  (ledger refs `referral:<id>:referrer` / `:referee`, status flip guarded on
  `pending`). CRM events `referral.applied` and `referral.rewarded` go to the
  outbox; no trigger routes them yet.
- Errors, all in the consumer module's `{code, message}` body the app's
  `apiClient` reads: unknown code -> 404 `REFERRAL_CODE_NOT_FOUND`; own code -> 422
  `SELF_REFERRAL`; a second, different code -> 409 `ALREADY_REFERRED`; the same
  code again -> 200 with the existing link.
- **Collision rule to know:** four base-36 characters collide. Accounts created in
  the same second derive the same code (same-second ObjectIDs differ only in the
  counter, and the hash keeps the top digits). On a stored-code collision the
  oldest account wins; a stored code always beats the derivation. The founder may
  want a longer code in a later app build.

### 9.2 Founding Family (`founding.go`, spec sections 3 to 6)

| Route | Reply |
|---|---|
| `GET /consumer/founding-family` | `{price_month, member, farms[], savings}`; **404 `NOT_AVAILABLE`** while no farm is on record or `FOUNDING_FAMILY_CLOSED=true` (the app says opening soon) |
| `POST /consumer/founding-family/join` `{farm_id}` | `{member}`; errors `WALLET_SHORT` (422, `shortfall` field and "short by N rupees" in the message), `FARM_UNLOCKED` (409), `ALREADY_MEMBER` (409) |
| `POST /consumer/founding-family/stop` | `{member}` with `status: stopped`; perks run to the end of the paid month |
| `GET /consumer/admin/founding/farms` | operator `{data: [...]}` envelope, SUPER_ADMIN token or `X-Admin-Key` |
| `PUT /consumer/admin/founding/farms` `{farms: [{id?, name, farmer, place, note, photo_url, unlocks_at, unlocked_packs, status?, sort?}]}` | upserts the founder's fields; never touches `claimed`; `status: unlocked` by hand activates the farm's waiting members through the same path a full farm uses |

Shapes are exactly `lib/foundingFamily.ts`: `member = {status: waiting|active|stopped,
farm_id, line_number, referral_code, joined_at, next_bill_date}`, `farms[] = {id,
name, farmer, place, note, photo_url, unlocks_at, claimed, status: filling|unlocked,
unlocked_packs}`, `savings = {level1_per_litre, level3_per_litre, delivery_fee}`
(null when no 1 L PYAAS line is priced yet).

**Data source.** Spec rule one says prices, stock, member status and farm status come
from the ERP through the backend. What the Dolibarr integration already reads is
used as-is; what it does not, the backend keeps:

| Value | Source today |
|---|---|
| Product master, level-1 price, stock | ERP sync (`dolibarr_sync.go`), unchanged |
| Level-3 (member) price on `PYS-*` milk | ERP `multiprices_ttc["3"]` when set and below level 1, mirrored to `member_price` on the catalog row; else derived as level 1 minus `FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE` (default 200) per litre, rounded to the rupee (1 L -> 2, 500 ml -> 1, 450 ml -> 1, 200 ml -> 0, matching the spec's table) |
| FOUNDING-99, DELIVERY-FEE | ERP services of those refs when the sync sees them (`consumer_founding_prices`); until they exist, `FOUNDING_PRICE_MONTH_PAISE` (9900) and `FOUNDING_DELIVERY_FEE_PAISE` (500) |
| Wallet | the backend wallet (the ERP customer advance is not read by this backend) |
| Farms, seats, members | `consumer_founding_farms` / `consumer_founding_members`, the admin endpoint above, and the seed `cmd/seed/founding_farms.json` (insert-only, both seed modes) |

**Rules implemented.** Join takes `price_month` from the wallet server-side, exactly
once (ledger row `ref_type: founding`, remark `FOUNDING-99`, ref
`founding:join:<member>:<n>`), assigns the next `line_number` on the farm
atomically, status `waiting`; a debit that fails releases the seat and the row.
When `claimed` reaches `unlocks_at` the farm flips to `unlocked` (once), claims
close, every waiting member becomes `active` with `next_bill_date` one month on,
and `founding.farm_unlocked` + `founding.member_active` are emitted per member;
otherwise `founding.seat_waiting` goes to the joiner. Billing
(`foundingBillingWorker`, hourly): Rs 99 on `next_bill_date`, once per
`(member, bill day)`, rolled on the anchor day-of-month (31 Jan -> 28 Feb -> 31
Mar); a short wallet is retried for `FOUNDING_BILL_RETRY_DAYS` (3) then the
membership stops with perks to the day before the missed bill. Stop keeps the perks
until `perks_until` (the day before the next bill); a waiting member's seat goes
back. A stopped member can re-join a filling farm.

**Member pricing.** Parag is never discounted. PYAAS milk lines (seeded `pyaas-*`
cards, the sync's `dol-pys-*` additions, any row with a `PYS-*` base id; ghee
excluded) bill at level 3 for a member whose perks apply and level 1 otherwise, in
`createOrder`, `createSubscription` and every morning order the worker
materialises (`subscriptionLinePrice` re-reads the standing each time).
`catalogPriceIndex.priceForMember` is the one resolver. Members pay no delivery
fee on one-off orders. `GET /consumer/catalog` now carries `member_price` on every
served PYAAS milk line (overrides, additions and their variants as `memberPrice`),
absent on everything else.

**CRM.** Three data-driven triggers in `crm_triggers.json` (FF-01/02/03, templates
T-FF01/02/03, EN + romanised HI in the file's register), on topics
`founding.farm_unlocked`, `founding.member_active`, `founding.seat_waiting`, routed
like D-01 (push, WhatsApp after 15 min, SMS, inbox always). Tokens `[FARM]`,
`[FARMER]`, `[LINE]`, `[TOGO]` are filled by `crmEventParams`. Trigger count is
57, templates 49.

### 9.3 The lock is 12 noon IST the day before (`subscriptions.go`)

- `lockHourIST = 12`. A delivery day's previews lock at 12:00 IST the day before
  (`lockMomentFor`), when the store task is created. Until then the preview is
  editable: edits reconcile it, pause/vacation cancel it and release the day.
- The preview for the first still-editable day (`firstEditableDay`: tomorrow before
  noon, the day after tomorrow from noon) is created on any tick, so the store sees
  it a full day ahead. `scheduleFromHourIST` (13:00) is gone.
- A change made after noon applies to the day after tomorrow: locked orders are
  never reconciled. A plan created, resumed or edited after the cut-off does not
  reach a day already past it: the catch-up branch serves only plans whose last
  member change (`changed_at`, stamped on create / patch / pause / resume)
  predates that day's lock moment, which is the server-down case.
- `next_delivery_date` (additive) is on every subscription reply so the app can
  show when a plan created after noon really starts. The app's start-date picker
  still defaults to tomorrow at any hour; after noon that day is already locked, so
  the picker should read this field or offer the day after tomorrow.
- Exactly-once is unchanged: the per-day claim plus the unique
  `(subscription_id, scheduled_for)` index.
- An unfunded preview is retried every tick until its day passes (as before).
- `GET /stores/{storeId}/upcoming` now lists the window from tomorrow through the
  first editable day; each row's `delivery_date` says which day. The admin CRM
  timeline event reads "Subscription day confirmed (noon lock)".
- The CRM B-02 shortfall notice (12:00) now fires at the lock itself; the
  "recharge by 12 noon tomorrow" copy question in section 5 still stands.

### 9.4 `DUPLICATE_SUBSCRIPTION`

`createSubscription` refuses a second non-cancelled plan for the same
`product_id` + normalised `variant` with 409 `DUPLICATE_SUBSCRIPTION` and a message
naming the plan ("You already have a daily plan for Milk gold-500ml 500ml. Change
its quantity or days in My subscriptions."). Paused counts as live; cancelled
frees the line; another variant is its own line. The app's own guard keys on
`product_id` alone, so it is the stricter of the two.

### 9.5 Stale locked orders

The sweep's new MISSED step (`closeMissedSubscriptionOrders`) closes a locked
morning order whose delivery day passed with no delivery task or a task never
completed: status `cancelled` (the one terminal status the app draws besides
`delivered`), `cancelled_by: missed`, the task `FAILED` with reason `missed`,
`order.failed` emitted with `reason: "missed"`, no money moved. Yesterday's orders
close from noon (the morning is left for a late delivered mark); older ones on any
tick. A task already `DELIVERED` is never touched (the order sync owns it).

### 9.6 Founder decisions still open

1. **Farm records are placeholders**: `cmd/seed/founding_farms.json` carries the
   four farms from spec 3.3 (Sri Radha Mohan Dairy / Ram / 150; Mishra Dairy /
   Abhishek Mishra / 80; Gonard Dairy / Harsh Singh / 65; Ranjeet Singh Dairy /
   Ranjeet Singh / 55), no photos, and every farmer's written consent (One Voice
   3.7 rule 5) is open. Sri Radha Mohan is "unlocked and delivering" on the website:
   set its status and `unlocked_packs` by hand once its members are migrated.
2. **Seat thresholds** (`unlocks_at`) are the spec's planned numbers, not confirmed.
3. **ERP items missing** (spec section 9): Whole Farm Milk 1 L bottle / pouch
   (`PYS-*` refs with a farm extrafield), price level 3 set to level 1 minus Rs 2/L
   on `PYS-*` milk (levels 3 to 5 equal level 1 today, so the backend derives it),
   the `FOUNDING-99` service (Rs 99, monthly) and `DELIVERY-FEE` (Rs 5), member and
   farm records in the ERP, the customer-facing names, the stray Parag records.
   Until then the fallbacks in 9.2 apply.
4. **DELIVERY-FEE on PYAAS-milk orders by non-members** (spec 5.3, "needs your
   yes"): built behind `FOUNDING_PYAAS_NONMEMBER_FEE`, off by default. The app's
   one-off fee stays Rs 15 below Rs 199 (both apps agree on that number today; One
   Voice says Rs 5, and shown must equal charged).
5. **PYAAS milk members-only** (spec 5.1): built behind
   `FOUNDING_PYAAS_MEMBERS_ONLY`, off by default because the app at `5f92d2e` has no
   shop lock state yet (its handoff lists screens 1, 2, 8, 9 as not built) and an
   existing PYAAS subscriber would be refused with no screen to explain it. Flip it
   with the app build that ships the lock states.
6. **Month one starts on activation**, not "on the first delivery" (spec 5.6): the
   first delivery date is not known at unlock. Say if the bill date should move to
   the first delivered PYAAS order instead.
7. **A waiting member who stops** gets the seat released and no automatic refund
   (spec 7 only covers a farm that does not unlock within 60 days). That 60-day
   refund, the 3-days-before reminder and the wallet-short notice (spec 6) are not
   built; the notification texts exist in the spec.
8. **Referral reward vs spec 5.8**: the Refer screen promises Rs 100 each; the spec
   and One Voice say a referral moves you up the line with no cash. The backend
   pays what the screen promises (config `REFERRAL_REWARD_PAISE`; set 0 to stop
   paying). `crm_triggers.json` config also lists 7500 paise per side under
   `credits_paise`, a third number.
9. **Moving today's Plus members** (spec section 8) is not built: there is no Plus
   record in this backend to migrate from.
10. **The app's start-date picker after noon** (9.3): tomorrow is already locked;
    the picker should offer the day after tomorrow or read `next_delivery_date`.

### 9.7 Review defects fixed (24 Sep handoff, section 5.3)

Each fix landed as its own commit with a test that failed before it.

1. **Billing retried per hour, not per day**: fixed in `21cf84c`. `last_attempt_date` counts one attempt per IST day; the third distinct short day stops the membership; a same-day top-up is still billed within the hour (`TestFoundingBillingRetriesOncePerDay`).
2. **Re-join inside the paid month charged Rs 99 again and dropped the perks**: fixed in `78cdaea`. Before `perks_until` the re-join is free and keeps `perks_until`; the farm the member was active on can be taken back even though it unlocked (active, same line, same bill date); a filling farm seats them as waiting with the perks intact and bills month two from the day after the paid month; after `perks_until` it is a fresh Rs 99 join (`TestFoundingRejoinInsidePaidMonth`).
3. **LOCK step honoured edits made 12:00-12:15**: fixed in `1346e2a`. A plan changed after the day's lock moment locks its preview as it stands; a pause before noon still cancels tomorrow on the late tick (`TestNoonLockStepIgnoresEditsAfterTheCutOff`).
4. **Duplicate line numbers after a waiting member stops**: fixed in `7b9a990`. A per-farm `next_line` that never decreases; `claimed` stays the live seat count (`TestFoundingLineNumbersNeverRepeat`).
5. **Founding and referral unique indexes missing from the test world**: fixed in `fe5ea18`. `newChainWorld` builds both; five concurrent joins give one member, one seat, one debit; a failed boot build makes `POST /founding-family/join` answer 503 `FOUNDING_UNAVAILABLE` and `POST /referrals/apply` 503 `REFERRAL_UNAVAILABLE` (flat `{code, message}`) until a retry builds the index (`index_guard.go`).
6. **Noon cut-off for one-off morning orders**: built in `f8fcfe5`. A morning order whose `delivery_date` is already locked (tomorrow, from 12:00 IST) gets 422 `CUTOFF_PASSED`, "Order by 12 noon for tomorrow; next available <day>.", plus `next_delivery_date`; the day after tomorrow and the instant lane are untouched; an order with no `delivery_date` (the app's first delivery of a new subscription) is not refused (`TestOneOffMorningOrderNoonCutOff`). App follow-up: the cart always sends tomorrow, so after noon it shows this message; it should offer `next_delivery_date` instead.

Also: `e0d7ed1` breaks the referral-code collision tie on the ObjectID (the oldest account now always wins; `TestReferralDerivedCodeResolvesWithoutAStoredOne` failed about one run in five before it), and `a892a14` declares the nine referral / Founding Family keys in `.env.example` and `render.yaml` with the defaults `config.go` applies.
