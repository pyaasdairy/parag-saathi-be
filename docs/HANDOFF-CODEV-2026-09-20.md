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

> **Superseded (24 Sep).** This section is the 20 Sep state and is kept as the record
> of what was found. The per-trigger audit that replaced it is
> `docs/CRM-AUDIT-2026-09-24.md`; the current table of every message (topic, when,
> channels, env, status, rendered body) is `docs/CRM-MESSAGES.md`; the engine as it
> works now is `docs/HANDOFF-CODEV-2026-09-24.md` section 4. Bugs 1, 3 and 5 below are
> fixed (`8afae8f`, `a1b8623`, `c722996`); bug 4 is addressed by `e9c609c` (the manual
> send now dedups by content); bug 6 is answered (the derived consent row IS written
> from the member's `marketing_*` grants, `TestCRMW03bConsentGate`); bug 2 (B-02 copy
> vs horizon) is still the founder's call.

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

> **Corrections (24 Sep).** Read these before the list below.
>
> - **The five rider per-task routes are RESTORED, not removed.** `29936df` removed
>   `POST /consumer/delivery/tasks/{id}/otp/send`, `/otp/verify`, `/scan`,
>   `/door-photo` and `GET .../compliance`; the deployed Saathi `release/26.07.03`
>   still sends all five (`lib/api/rider_api.dart`, from `proof_sheet.dart` and
>   `compliance_screen.dart`), so `5b16775` reverted the removal byte-identical and
>   `TestRiderTaskRoutesAnswerForTheRider` pins it. Rule: never remove a route a
>   deployed frontend still sends. The "Delete or keep" decision at the end of this
>   section is closed: keep all five until no deployed build calls them.
> - **CRM:** see `docs/CRM-AUDIT-2026-09-24.md` (the 54-trigger audit) and
>   `docs/CRM-MESSAGES.md` (every message and its status today). Section 5 here is
>   history.
> - **Dry-run origins** (`d6145f9`). Three env keys replace a provider's
>   scheme://host and keep every path, header and body exactly as production sends
>   them, so one local stub captures the real requests. Unset, the real endpoint is
>   used and the binary behaves byte-identically.
>
>   | Key | Replaces | Path the stub receives |
>   |---|---|---|
>   | `CRM_MSG91_BASE_URL` | `https://control.msg91.com` for BOTH MSG91 callers (the CRM flow channel and the login/delivery OTP client) | `/api/v5/flow/`, `/api/v5/otp` |
>   | `CRM_WA_BASE_URL` | `https://graph.facebook.com` | `/v19.0/<CRM_WA_PHONE_ID>/messages` |
>   | `EXPO_PUSH_BASE_URL` | `https://exp.host` | `/--/api/v2/push/send` |
>
>   A channel reaches the stub only when it is plugged: `CRM_MSG91_AUTHKEY` plus a
>   `CRM_DLT_TEMPLATE_IDS` entry per trigger, `CRM_WA_TOKEN` + `CRM_WA_PHONE_ID` +
>   `CRM_WA_TEMPLATE_NAMES`, `EXPO_PUSH_ENABLED=true` plus a device row.
>   `crm_dryrun_seams_test.go` covers the seams.
> - **The two unique indexes and their boot guards.** Neither can take the backend
>   down at boot; each keeps an older guard in force when its build is refused.
>   - `subscription_day_unique` on `consumer_orders (subscription_id, scheduled_for)`,
>     partial on `subscription_id` existing and a LIVE status (`placed`, `confirmed`,
>     `assigned`, `out_for_delivery`, `delivered`; `cancelled` is left out so a
>     resumed day can be placed again) (`b00cc7d`). Built by
>     `ensureSubscriptionDayIndex` at module boot; a refusal because older rows
>     already collide is logged as an ERROR with the number of colliding
>     (subscription, day) pairs, and `claimSubscriptionDay` stays the guard until an
>     operator cleans them (the admin CRM lists them at
>     `GET /consumer/admin/crm/duplicate-subscription-orders`). `$in` in a partial
>     filter needs MongoDB 6.0+.
>   - `crm_claim_scoped` on `crm_dispatch_log (trigger_id, consumer_id, ist_day,
>     scope_key)` (`8edd9e1`), the CRM's exactly-once claim. `ensureCRMClaimIndex`
>     first stamps `scope_key: ""` on legacy rows, builds the scoped index, and only
>     then drops the legacy `trigger_id_1_consumer_id_1_ist_day_1`. If the build is
>     refused the legacy per-day index is KEPT (logged as an ERROR with the duplicate
>     count), so claims stay exactly-once, only coarser. The CRM worker retries
>     `ensureCRMIndexes` on every tick until all CRM indexes are in place.

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
- ~~Delete or keep the now-unused rider endpoints `/otp/send`, `/otp/verify`,
  `/door-photo`, `/compliance`~~ **Closed (24 Sep): keep all five.** The deployed Saathi
  build still sends them (see the corrections at the top of this section); `/scan` and
  `/undo` must survive in any case, the inventory screen and the 15-minute undo use
  them.

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
