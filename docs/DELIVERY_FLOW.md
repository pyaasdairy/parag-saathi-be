# Consumer delivery — the flow, and what each console shows

The dispatch itself is unchanged: an order creates a task at the nearest store,
the **store manager** assigns a rider, and the **rider** accepts → picks up →
delivers with photo + geotag proof. Money still settles exactly once on
delivery (`delivery:<order>` ref). This note covers what was added around it.

## Two lanes, never mixed

Every task carries `lane`: `instant` (~20-minute promise, broadcast to nearby
riders as OFFERED) or `morning` (the 05:00–07:30 subscription round, assigned by
the manager). Both consoles now render them as **separate sections with counts**
— the rider's Deliveries tab and the store's Orders tab — because an urgent
instant order used to sit invisibly among tomorrow's planned drops.

## The customer's doorstep instructions reach both consoles

`deliveryPrefs` on every task: `note`, `receiver`, `call_before`, `ring_bell`,
`handover`. They are merged from three places, most specific per field winning
(`resolveDeliveryPrefs`, delivery_svc.go):

1. the **saved address's** doorstep capture (`receiver_name`, `ring_bell`,
   `call_before`, `instructions`) — where customers actually set this, and the
   one source nothing downstream read before;
2. the account's standing prefs (`PATCH /me delivery_prefs`);
3. the order's own prefs (checkout).

`ring_bell` is a **tri-state** (`null` never said · `false` DO NOT ring ·
`true` please ring). As a plain bool, Go's `omitempty` dropped the explicit
`false` from the wire, so "do not ring the bell, baby sleeping" — the most
common instruction there is — never reached the rider at all.

## Completing a delivery: photo + geotag, nothing else

The rider's flow is a sequence of **swipes**, one decision per screen:

```
swipe to accept → swipe when picked up → swipe to deliver
                                            ├─ swipe to take the photo   (camera → upload)
                                            ├─ swipe to capture the geotag (real device fix)
                                            └─ swipe to finish            (writes the delivery)
```

The OTP prompt, the product scan, the door-reference photo and the note field
are **gone** — each was another decision at a customer's door. The geotag is
fail-closed: it is never derived from the address, and a rider further than
`deliveryGeofenceMeters` (300 m) from the pin cannot reach the last swipe.
"Could not deliver" remains, deliberately as a quiet text button.

## The store manager can hand-assign an unclaimed instant order

An instant order is broadcast to the store's riders as `OFFERED`, and the
first rider to claim it wins. When nobody claims it, the store manager can now
give it to one of the store's own riders from the same assign sheet the
morning round uses:

`POST /consumer/stores/{storeId}/orders/{deliveryId}/assign`
`{"rider_party_id": "...", "reason": "optional, one line"}`

- **Assignable:** `ASSIGNED` (unassigned or not yet accepted), `FAILED` (the
  delivery side failed it) and now an **unclaimed** `OFFERED` task (no rider
  yet). An instant order a rider already claimed stays theirs: the assign
  answers `409 CLAIMED_BY_OTHER` and changes nothing (only the admin CRM can
  move it). The manager's write and a rider's claim race on one atomic
  update, so exactly one of them wins.
- **Scoping is unchanged:** the manager's own store only, and only a rider on
  that store's roster (`403` / `400` otherwise).
- **What happens:** the task becomes `ASSIGNED` to the chosen rider, leaves
  every other rider's offer pool at once and appears in the rider's queue
  (they swipe to accept, as for any assigned task). The member's order moves
  to `assigned` with the rider, exactly as on a claim. Reassigning an instant
  order the first rider never accepted moves the member's order to the new
  rider too.
- **The record:** every manager assign stamps the task with `assigned_by`
  (`party_id`, `role`, `name`), `assigned_at`, `assign_source:
  "manager_assign"` and `assign_reason`, and writes a row to the platform
  audit log (`audit_logs`, action `consumer.delivery.manager_assign`, with the
  store, order, lane, rider, previous status and rider, and the reason). On
  the task JSON these are the additive keys `assignedBy`, `assignSource`,
  `assignReason`. A claim stamps `assign_source: "rider_claim"`, the admin
  CRM's assign stamps `"admin_assign"` with the admin, and a rider's decline
  clears all three (the task is back in the pool with nobody assigned).

## Attendance gates the instant offer pool, and never blocks a delivery

The **offer pool** is the list of unclaimed broadcast instant orders a rider's
app polls (`GET /consumer/delivery/tasks/available`) and claims from
(`POST .../tasks/{id}/claim`). It is shown only to riders who are **on duty
today** (IST), and only they may claim from it (`403 NOT_ON_DUTY`, message
"Mark attendance at the centre to take new orders", otherwise). Code:
`duty_gate.go`.

**On duty** at a store for an IST day = **that store's** manager's mark for
that day when there is one (on or off), otherwise the rider's own attendance
being `ON_DUTY` (checked in, not yet checked out). Yesterday's check-in and a
check-out both mean off duty. The manager's mark never writes
`rider_attendance` (that is a payroll record backed by the rider's selfie); it
lives in `rider_duty_overrides`, one row per rider per day per store. A rider
who rides for two stores is marked by each store's manager for that store
only: the manager of store A can neither pull a shared rider out of store B's
pool nor push one into it, and store B's roster never shows store A's mark.

A rider may claim only an offer of a store they ride for. The pool never lists
another store's offers, and a claim by id of one is refused with `403
FORBIDDEN` ("This order is from another store"), on duty or not.

What the gate never does:

1. **Assigned work always shows.** A task already assigned to a rider stays in
   their queue and can be accepted, picked up and delivered, checked in or
   not. Only the unclaimed pool is gated.
2. **The manager overrides it.** The manager may assign any of the store's
   riders, on duty or not (the audit row records `rider_on_duty` so the
   choice is visible), and may mark a rider on or off duty for today:
   `POST /consumer/stores/{storeId}/riders/{riderPartyId}/duty`
   `{"on_duty": true|false, "reason": "optional"}` →
   `{partyId, day, onDuty, dutySource}`. Own store and own riders only, and
   the mark counts at that store only; audited as
   `consumer.rider.duty_override`.
3. **Nobody on duty → everybody.** If no rider of a store is on duty, that
   store's pool falls back to every rider on its roster, exactly as before the
   gate, and the backend logs "no rider of the store is on duty - the instant
   offer pool falls back to every store rider" (at most once per store per 10
   minutes, since riders poll every few seconds). A missed check-in can never
   strand an order.
4. **Fail open.** If the duty lookup fails, the pool is left open.

The store's rider roster (`GET /consumer/stores/{storeId}/riders`) carries the
additive keys `onDuty` and `dutySource` (`attendance` | `manager` | `none`).

The "New instant order" push to Saathi phones (`docs/OPERATOR-PUSH.md`) follows
the same rule: the store's managers always ring, and of its riders only those
whose pool shows the order (the on-duty riders, or every rider when nobody is
on duty or the lookup fails). A rider who cannot see or claim the offer is not
woken for it.

## Admin delivery CRM

`/consumer/admin/*` — SUPER_ADMIN role token, or `X-Admin-Key: $ADMIN_API_KEY`
from a server. Orders + detail/timeline, riders, subscriptions, the pickup
point, reassign, cancel, and a duplicate-day scan. Full reference for the
website: [`HANDOFF-WEBSITE-DELIVERY-CRM.md`](HANDOFF-WEBSITE-DELIVERY-CRM.md).

## Also fixed here

The subscription sweep re-claimed **today** after scheduling **tomorrow**, so
from 1 PM IST every daily subscriber got a duplicate order — and its own
delivery task and debit — on alternating 15-minute ticks. Claims are now per
day (`ordered_days`) with a no-live-order check;
`GET /consumer/admin/crm/duplicate-subscription-orders` lists duplicates already
in the database.

Env: `ADMIN_API_KEY` (≥ 32 chars) enables the CRM's server-to-server access.

Tests:
`CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 go test ./internal/modules/consumer/`
