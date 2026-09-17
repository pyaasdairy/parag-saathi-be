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
