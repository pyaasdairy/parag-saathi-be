# Handoff — consumer app (pyaas-consumer) and the simple delivery flow

**For:** the agent working on `pyaasdairy/pyaas-consumer` (Expo / React Native).

## Nothing is required

The backend now fills the fields the app already reads, so the published build works as is:

| App code | Field | Now |
|---|---|---|
| `app/order/[id].tsx` rider card + `RiderTrackMap` | `riders.full_name`, `riders.phone`, `riders.current_lat/lng` | Set when the rider picks the order up; position updates every ~20 s while they are on the way |
| trip origin (`storeOrigin`) | `store_lat`, `store_lng` | The admin-set pickup point, filled once a rider is on the order |
| "Delivered · see photo" card | `proof_photo_url` | A signed image URL valid ~1 h, refreshed on every `GET /orders/{id}` (the screen polls every 10 s) |
| status chips | `status` | `placed` → `assigned` (rider accepted) → `out_for_delivery` (picked up) → `delivered` |

A new subscription places today's order immediately (when it is due and the wallet covers it) instead of waiting for the next 15-minute server tick.

## Optional polish (small, safe)

1. **Show the live map for morning orders from `out_for_delivery`.** The rider card map already renders for `assigned|out_for_delivery`; nothing to change unless you want the full-screen hero map (currently instant-lane only) for subscriptions too.
2. **Delivered time.** `delivered_at` (UTC RFC3339) is now on the order; show "Delivered at 6:42 AM".
3. **Doorstep instructions.** Whatever the customer saves on their address (`ring_bell`, `call_before`, `receiver_name`, `instructions`) is now what the rider and the store manager see. If the app ever stops sending `ring_bell` explicitly, "do not ring the bell" silently becomes "no preference" — keep sending it as a real boolean.

Do not change the debit ref (`delivery:<orderId>`). The backend relies on it to charge exactly once.
