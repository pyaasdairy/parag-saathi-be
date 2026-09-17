# Handoff — the delivery UI, and what it needs from the API

**For:** the backend co-dev.
**From:** the UI pass on the Saathi app (repo `pyaas-saathi`, branch
`feature/simple-delivery`, commit `0a9a4c7` + this round).
**Scope:** the store-manager → rider dispatch is UNCHANGED. Nothing about
assignment, the state machine or money moved. This is the consoles, plus the
small API changes they needed — those are listed in §3 for you to own.

---

## 1. What the two consoles now do

### Rider (`lib/screens/rider/`)
- **Tabs: Deliveries · Done · Cash.** The old *Priority* tab is gone — it
  listed the same stops a second time.
- **Two lane sections inside Deliveries**, each with a count: ⚡ **Instant**
  (`lane == "instant"`, or a `slot` beginning "by ") and 🌅 **Morning**
  (everything else). Same split on Done. An empty lane still prints a line, so
  "nothing instant right now" never looks like a failed load.
- **Every action is a swipe**, one per state:
  `ASSIGNED → swipe to accept` · `ACCEPTED → swipe when picked up` ·
  `OUT_FOR_DELIVERY → swipe to deliver`, and an OFFERED broadcast is accepted
  with the same gesture on the card. A tap cannot fire one.
- **Completing a delivery is a photo and a geotag, nothing else**:
  `swipe to take the photo → swipe to capture the geotag → swipe to finish`.
  The OTP prompt, the product scan, the door-reference photo and the note field
  are **removed from the flow** (see §4 for the endpoints that frees up).
- The card shows the items in words, the customer's instructions, and the
  pickup point sits on top of the day.

### Store manager (`lib/screens/store/`)
- Orders tab splits into the same two lane sections, each with a count and how
  many still need a rider; completed orders stay in their own section below.
- Cards list the items in words and the customer's instructions; the order
  detail shows the same instructions panel the rider sees at the door.
- An OFFERED (instant) order now reads *"broadcast to nearby riders"* instead
  of *"no rider assigned"*, because it is not waiting on the manager.

---

## 2. The contract the UI reads (no change needed from you)

Per task, off `GET /consumer/delivery/tasks` and `GET /consumer/stores/{id}/orders`:

| Field | Used for |
|---|---|
| `lane` | which section the stop goes in (`instant` \| `morning`; absent → morning) |
| `deliveryDate`, `slot`, `etaAt` | **when it is due** — the chip on every card and both detail screens, and the sort order inside a lane. `deliveryDate` (YYYY-MM-DD IST) renders as Today / Tomorrow / Thu 24 Sep; `slot` supplies the window and may arrive as `"2026-09-18 · 05:00 - 07:30 AM"`; `etaAt` drives the instant countdown ("in 12 min", red once late). Keep sending all three — with none of them a card can only say which lane it is in. |
| `items[]`, `amount`, `paymentMode` | "2 × Toned Milk", COD vs prepaid |
| `geo`, `addressLine`, `addressLabel`, `landmark` | navigation + what is shown |
| `phone` | the call button (never derived from `phoneMasked`) |
| `deliveryPrefs` | the customer-instructions panel — see §3.1 |
| `status` | which swipe is offered |

The delivery write is unchanged:
`POST /consumer/delivery/tasks/{id}/deliver` with `client_op_id`/`event_id`,
`proof:{geofence_ok, photo_ref}`, `geo:[lat,lng]`. `proof_note` is now always
`""` and `otp_ok` is never sent.

---

## 3. API changes made in this branch — please review, they are yours now

### 3.1 Doorstep instructions actually reach the task
`resolveDeliveryPrefs` (consumer/delivery_svc.go) merges three sources,
most specific per field winning:

1. the **saved address's** capture (`receiver_name`, `ring_bell`, `call_before`,
   `instructions`) — customers set it there, and nothing downstream read it;
2. the account's standing prefs (`PATCH /me delivery_prefs`);
3. the order's own prefs (checkout).

**`ring_bell` is now a tri-state (`*bool`)**: `null` never said · `false` DO NOT
ring · `true` please ring. As a plain `bool` with `omitempty`, an explicit
`false` was dropped from the JSON entirely — so "do not ring the bell, baby
sleeping" reached nobody. If you ever touch this struct, keep the pointer.
Old tasks simply have no `ring_bell`; nothing to migrate.

Tests: `delivery_prefs_test.go` (`-run DoorstepPrefs`).

### 3.2 `GET /consumer/delivery/pickup`
Read-only, `DELIVERY_RIDER` or `STORE_MANAGER`, returns `{name,address,lat,lng}`.
The super admin sets it at `GET|PUT /consumer/admin/delivery-settings`. Without
this route the pickup address was settable but invisible to the people who go
there.

### 3.3 Duplicate subscription orders (a live bug, fixed)
The sweep re-claimed **today** after scheduling **tomorrow**, so from 13:00 IST
every daily subscriber got a duplicate order — and its own delivery task and its
own debit — on alternating 15-minute ticks. Claims are per day now
(`ordered_days` array) plus a "no live order for that day" check
(`claimSubscriptionDay`). Regression test: `-run NeverDuplicates`.
`GET /consumer/admin/crm/duplicate-subscription-orders` lists duplicates already
in production — worth running before the next release.

### 3.4 Smaller
- `delivery_extras.go` — pickup point, the due-day helper and short-lived signed
  URLs for proof photos (a private bucket the consumer app cannot authenticate
  against), split out of the delivery files.
- Delivery tasks carry `delivery_date` (the consoles' due chip reads it);
  consumer orders carry `store_lat/lng`,
  `delivered_at` and a signed `proof_photo_url` (fields the shipped consumer app
  already reads).
- Admin delivery CRM under `/consumer/admin/*` — see
  [`HANDOFF-WEBSITE-DELIVERY-CRM.md`](HANDOFF-WEBSITE-DELIVERY-CRM.md).

---

## 4. Things for you to decide (the UI is honest about all of them today)

1. **Nobody can hand-assign an unclaimed instant order.** `assignRider` guards on
   `status ∈ {ASSIGNED, FAILED}`, and instant tasks sit at `OFFERED`, so if no
   rider accepts, the store console has no move. (The admin CRM's assign does
   allow `OFFERED`.) Widening the guard is a dispatch decision, not a UI one.
2. **Attendance does not gate anything.** The rider console shows the check-in
   banner, but orders are visible and workable without it. The copy no longer
   claims otherwise. If attendance SHOULD gate the queue, that is a server rule.
3. **These endpoints are now unused by the rider flow** (still mounted, still
   tested): `/delivery/tasks/{id}/otp/send`, `/otp/verify`, `/scan`,
   `/door-photo`, `/compliance`. Don't delete them blind — `/scan` is still used
   by the rider **inventory** screen, and the retired `screens/delivery/`
   module (not routed) references the old proof sheet.
4. **Proof photos need `B2_KEY_ID` / `B2_APP_KEY` set**, or
   `POST /uploads/presign` 500s and a rider cannot finish a delivery. This is the
   one hard dependency of the new flow.
5. **The geofence is 300 m**, enforced server-side in `deliverDelivery` and
   mirrored by the app (`deliveryGeofenceMeters` ↔ `geofenceMeters`). If you
   change one, change both, or riders will be blocked by a check they cannot see.

---

## 5. Running it

```bash
# backend
go build ./... && go vet ./...
CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 go test ./...

# app
cd ../pyaas-saathi && flutter analyze && flutter test
```

The app talks to whatever `API_URL` is built in:
`flutter run --dart-define=API_URL=http://10.0.2.2:18080/api/v1` for a local
backend from an Android emulator. A full local test drive (seeded store, one
morning + one instant order, both consoles) is scripted in
[`../pyaas-saathi/docs/TEST_ON_EMULATOR.md`](../pyaas-saathi/docs/TEST_ON_EMULATOR.md).
