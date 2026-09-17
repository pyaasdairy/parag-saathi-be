# Handoff — Delivery CRM for pyaasdairy.com/admin

**For:** the agent working on the PYAAS website (`kushagrakaush1k/pyaas`, Next.js 16 App Router, JavaScript).
**Goal:** add a **Deliveries** section to the existing password-gated `/admin` page that shows every consumer order, its delivery, the riders, subscriptions and money, backed by the Saathi Go backend.

Do not change any existing admin tab or route. This is additive.

---

## 1. How it connects (read first)

```
Browser (/admin page)  ──POST {password, action, params}──▶  Next.js route  app/api/admin/delivery/route.js
                                                                │  checks ADMIN_PASSWORD (existing passwordMatches)
                                                                │  adds X-Admin-Key: SAATHI_ADMIN_API_KEY
                                                                ▼
                                             Saathi backend  https://saathi-backend-xcxn.onrender.com/api/v1/consumer/admin/...
```

- The **admin key never reaches the browser.** Only the server route knows it. Never prefix these env vars with `NEXT_PUBLIC_`.
- Reuse the existing pattern in `app/api/admin/route.js`: `export const runtime = 'nodejs'`, `export const dynamic = 'force-dynamic'`, `rateLimit(ipKey(req))`, `passwordMatches(body.password, process.env.ADMIN_PASSWORD)`.
- The backend wraps every success as `{ "data": ... }` and every failure as `{ "error": { "code", "message" } }`. Unwrap `data` in the route and return it; pass backend errors through with their HTTP status.
- The backend is on Render's free plan and can take **30–60 s** to wake up. Use a 70 s fetch timeout and show a "Waking the server…" state instead of failing.

### Environment variables (Vercel → Project → Settings → Environment Variables)

| Name | Value |
|---|---|
| `SAATHI_API_URL` | `https://saathi-backend-xcxn.onrender.com/api/v1` |
| `SAATHI_ADMIN_API_KEY` | the same value set as `ADMIN_API_KEY` on the Render backend (the founder has it) |

### The route (implement exactly this whitelist — no generic proxy)

`POST /api/admin/delivery` with body `{ password, action, params }`:

| `action` | Backend call | `params` |
|---|---|---|
| `summary` | `GET /consumer/admin/crm/summary?from&to` | `{from?, to?}` |
| `orders` | `GET /consumer/admin/crm/orders?…` | see §2.2 |
| `order` | `GET /consumer/admin/crm/orders/{orderId}` | `{orderId}` |
| `riders` | `GET /consumer/admin/crm/riders` | — |
| `subscriptions` | `GET /consumer/admin/crm/subscriptions?status&page&limit` | `{status?, page?, limit?}` |
| `duplicates` | `GET /consumer/admin/crm/duplicate-subscription-orders` | — |
| `getSettings` | `GET /consumer/admin/delivery-settings` | — |
| `saveSettings` | `PUT /consumer/admin/delivery-settings` body `{pickup:{name,address,lat,lng}}` | `{name, address, lat, lng}` |
| `assign` | `POST /consumer/admin/crm/deliveries/{deliveryId}/assign` body `{rider_party_id}` | `{deliveryId, riderPartyId}` (`""` = back to all riders) |
| `cancel` | `POST /consumer/admin/crm/orders/{orderId}/cancel` body `{reason}` | `{orderId, reason}` |

Validate `orderId` / `deliveryId` against `^[a-z]+_[0-9a-f]{6,32}$` and `riderPartyId` against `^([0-9a-f]{24})?$` before building the URL, and `encodeURIComponent` every query value. Reject unknown actions with 400.

---

## 2. Backend API reference (what the route returns after unwrapping `data`)

All dates are **IST calendar days** (`YYYY-MM-DD`) in parameters; all timestamps in responses are **UTC RFC3339**. Render timestamps in `Asia/Kolkata`. Money is in **rupees** (not paise).

### 2.1 `summary` — the overview numbers

Query: `from`, `to` (inclusive IST days; default = last 30 days).

```json
{
  "from": "2026-08-16", "to": "2026-09-14",
  "ordersTotal": 128,
  "ordersByStatus":     {"placed": 4, "assigned": 1, "out_for_delivery": 2, "delivered": 115, "cancelled": 6},
  "deliveriesByStatus": {"NO_TASK": 3, "TO_PICK_UP": 5, "ON_THE_WAY": 2, "DELIVERED": 115, "FAILED": 3},
  "ordersByType":       {"subscription": 96, "one_time": 30, "offer": 2},
  "ordersByPayment":    {"PAID": 101, "FREE": 12, "PENDING": 7, "NOT_CHARGED": 6},
  "orderValue": 6420.5, "deliveredValue": 5980,
  "riders": [{"partyId": "6a…", "name": "PYAAS Rider 1", "delivered": 60, "onTheWay": 1, "avgMinutesToDeliver": 14}],
  "perDay": [{"date": "2026-09-14", "placed": 6, "delivered": 5}],
  "subscriptions": {"active": 40, "paused": 3, "cancelled": 5},
  "liveOpenDeliveries": 7,
  "liveUnassignedDeliveries": 5,
  "pickup": {"name": "PYAAS Store · Chandra Panorama", "address": "P4 - 805, …", "lat": 26.7738, "lng": 81.0089}
}
```

`orders*` counts are for orders **placed** in the range. `live*` counts are "right now", regardless of range.

### 2.2 `orders` — the order table

Query params (all optional):

| Param | Meaning |
|---|---|
| `from`, `to` | IST days, inclusive (default last 30 days) |
| `date_field` | `placed` (default) or `delivery` (filter on the day it is delivered) |
| `status` | comma list of order statuses: `placed,assigned,out_for_delivery,delivered,cancelled` |
| `delivery_status` | comma list: `NO_TASK,TO_PICK_UP,ON_THE_WAY,DELIVERED,FAILED` (filters **within the page**) |
| `type` | `subscription` \| `one_time` \| `offer` |
| `lane` | `morning` \| `instant` |
| `rider` | rider `partyId` |
| `q` | search in order id, phone, customer name, address |
| `page`, `limit` | 1-based page; limit 1–200 (default 50) |

Response:

```json
{
  "items": [ /* crmOrderRow, below */ ],
  "total": 128, "page": 1, "limit": 50, "from": "2026-08-16", "to": "2026-09-14"
}
```

`crmOrderRow`:

```json
{
  "orderId": "ord_ba31e9594abb",
  "placedAt": "2026-09-14T16:11:52.245Z",
  "deliveryDate": "2026-09-15",
  "type": "subscription",
  "subscriptionId": "sub_2d144d61efb9",
  "lane": "morning",
  "slot": "2026-09-15 · 05:00 - 07:30 AM",
  "orderStatus": "delivered",
  "deliveryStatus": "DELIVERED",
  "deliveryId": "del_046096ece70e",
  "customer": {"id": "6aa8…", "name": "Asha Verma", "phone": "+919123456781"},
  "addressLabel": "Home",
  "address": "Tower B-704, Sushant Golf City, Lucknow, 226030",
  "addressGeo": {"lat": 26.7702, "lng": 81.0141},
  "items": [{"productId": "taaza-500ml", "name": "Toned Milk - Parag Taaza", "variant": "500ml", "qty": 2, "price": 29, "lineTotal": 58}],
  "itemsCount": 2,
  "subtotal": 58, "deliveryFee": 0, "total": 58,
  "paymentMethod": "wallet",
  "paymentStatus": "PAID",
  "trialFree": false, "offerPack": 0,
  "rider": {"id": "6aa8…", "name": "PYAAS Rider 1", "phone": "9000090001"},
  "assignedAt": "2026-09-15T00:41:10Z",
  "startedAt": "2026-09-15T00:41:10Z",
  "deliveredAt": "2026-09-15T00:55:02Z",
  "minutesToDeliver": 13,
  "pickupAddress": "P4 - 805, Chandra Panorama, Sushant Golf City, Lucknow",
  "hasProofPhoto": true,
  "proofDistanceM": 12,
  "failureReason": "",
  "rating": 5, "reviewComment": "On time"
}
```

Field notes:
- `orderStatus` is what the customer sees; `deliveryStatus` is the rider side (`NO_TASK` = a subscription day not yet confirmed; it locks at midnight IST).
- `paymentStatus`: `PAID` (wallet debited), `FREE` (₹0 trial/offer day), `PENDING` (not delivered yet), `NOT_CHARGED` (cancelled), `COD`. Money only ever moves on delivery, so a `PENDING` order has never been charged.
- `proofDistanceM` = metres between the rider's GPS when the photo was taken and the customer's saved pin. Flag rows over **300 m** in amber (not an error, just worth a look).
- `lane` is `instant` or `morning`. Group the table the way the apps do: the two lanes are different operations, not a filter of one list.
- `deliveryPrefs` (order detail) carries the customer's doorstep instructions. `ring_bell` is a TRI-STATE: absent = never said, `false` = **do not ring** (show it as a warning), `true` = ring. Never render a missing value as "ring the bell".
- Optional fields may be missing or empty. Never crash on a missing field.

### 2.3 `order` — the detail drawer

```json
{
  "order": { /* crmOrderRow */ },
  "timeline": [
    {"at": "2026-09-14T16:11:52Z", "event": "Order placed", "by": "customer"},
    {"at": "2026-09-14T18:30:00Z", "event": "Subscription day confirmed (midnight lock)", "by": "system"},
    {"at": "2026-09-14T18:30:00Z", "event": "Sent to riders", "by": "system"},
    {"at": "2026-09-15T00:41:10Z", "event": "Picked up · out for delivery", "by": "PYAAS Rider 1"},
    {"at": "2026-09-15T00:55:02Z", "event": "Delivered (photo taken)", "by": "PYAAS Rider 1"}
  ],
  "delivery": {
    "id": "del_…", "status": "DELIVERED", "paymentMode": "PREPAID",
    "pickupAddress": "…", "pickupGeo": {"lat": 26.7738, "lng": 81.0089},
    "addressGeo": {"lat": 26.7702, "lng": 81.0141},
    "proofGeo": {"lat": 26.7703, "lng": 81.0140}, "proofDistanceM": 12, "geofenceOk": true,
    "lastKnownGeo": {"lat": 26.7703, "lng": 81.0140}, "lastLocationAt": "2026-09-15T00:55:02Z",
    "proofNote": "Delivered",
    "proofPhotoUrl": "https://f005.backblazeb2.com/file/…?Authorization=…"
  },
  "customer": {"id": "…", "name": "Asha Verma", "phone": "+919123456781", "email": "…", "joinedAt": "…", "status": "ACTIVE", "walletAvailable": 442},
  "payments": [{"type": "DEBIT", "bucket": "CASH", "amount": 58, "remark": "Delivery ord_…", "at": "…"}],
  "subscription": {"id": "sub_…", "productId": "taaza-500ml", "name": "…", "variant": "500ml", "qty": 2, "unitPrice": 29, "perDelivery": 58, "frequency": "daily", "status": "active", "startDate": "2026-09-14", "vacations": [], "lastOrderDate": "2026-09-15", "createdAt": "…"},
  "deliveryPrefs": {"note": "Do not ring the bell, baby sleeping", "ring_bell": false, "call_before": true, "receiver": "Guard at Gate 2"},
  "buyerGstin": ""
}
```

- `proofPhotoUrl` is a **signed link valid for about 1 hour**. Render it with a plain `<img>` (not `next/image`, whose optimiser would cache an expiring URL). Re-fetch the order when the drawer is reopened. Empty string = no photo.
- `delivery`, `customer`, `subscription` may be absent.

### 2.4 `riders`

```json
[{"partyId": "6aa8…", "name": "PYAAS Rider 1", "phone": "9000090001",
  "online": true, "lastSeenAt": "2026-09-15T00:55:02Z", "lastLocation": {"lat": 26.77, "lng": 81.01},
  "onTheWay": 1, "deliveredToday": 9, "deliveredAllTime": 240}]
```

`online` = the rider app sent a GPS fix in the last 10 minutes.

### 2.5 `subscriptions`

`{items: [subscription + customerName + customerPhone], total, page, limit}`. The item shape is the same as `subscription` in §2.3.

### 2.6 `duplicates`

Subscription days that have **more than one live order**, the leftovers of a bug fixed on the backend in this release:

```json
[{"subscriptionId": "sub_…", "day": "2026-09-12", "count": 2,
  "orders": [{"orderId": "ord_a", "status": "delivered", "total": 58, "placedAt": "…", "customerName": "…", "phone": "…"},
             {"orderId": "ord_b", "status": "placed", "total": 58, "placedAt": "…", "customerName": "…", "phone": "…"}]}]
```

Show each group, keep the first order, and offer **Cancel** on the extra ones that are not `delivered`. A delivered duplicate cannot be cancelled. Show it with a note "delivered twice, refund manually if the customer was charged twice".

### 2.7 `getSettings` / `saveSettings` — rider pickup point

`{"pickup": {"name", "address", "lat", "lng", "updatedAt", "updatedBy"}}`. Saving requires a non-empty address (≤ 300 chars) and real coordinates. Riders see the change on their next refresh (≤ 15 s).

### 2.8 `assign` / `cancel`

- `assign` works only before pickup (`TO_PICK_UP`). Otherwise the backend returns `409 NOT_ASSIGNABLE`. `riderPartyId: ""` puts it back in the shared pool every rider sees.
- `cancel` fails with `409 ALREADY_DELIVERED` for delivered orders. Nothing is ever charged before delivery, so cancelling never needs a refund.
- Both return the updated row. Refresh the table afterwards.

---

## 3. What to build in `/admin`

Add one new top-level tab **Deliveries** (alongside the existing ones), rendered only after the password gate. Keep the existing visual language (Tailwind classes already used on the page, `text-ink-mute`, etc.). Mobile-friendly: tables scroll horizontally inside their own container.

1. **Overview** (default sub-tab)
   - Date range picker (Today / Yesterday / Last 7 days / Last 30 days / custom). Default **Today**.
   - KPI tiles: Orders placed · Delivered · On the way · Waiting for pickup (`liveUnassignedDeliveries`) · Delivered value ₹ · Active subscriptions.
   - Placed vs delivered per day (simple bar chart; `perDay`).
   - Rider leaderboard (`riders` from summary): delivered, on the way, avg minutes.
   - Current pickup point with an "Edit" button (opens Settings).
   - Auto-refresh every 30 s while the tab is visible.

2. **Orders**
   - Filters: date range + date field (placed/delivery), order status, delivery status, type, rider, search box (debounced 400 ms).
   - Columns: Placed (IST) · Deliver on · Lane (⚡ instant / 🌅 morning) · Customer (name + phone, `tel:` link) · Items ("2 × Taaza 500ml") · Total ₹ · Type · Order status · Delivery status · Rider · Delivered at · Mins · Payment · Photo ✓.
   - Status colours: delivered green, on the way blue, to pick up amber, failed/cancelled red.
   - Pagination (50/page) and **Export CSV** of the current filter (page through all results, max 5 000 rows, IST timestamps, one row per order with items joined by `; `).
   - Row click → **Order drawer**: timeline, proof photo (click to enlarge), map links (Google Maps for customer pin, photo location, pickup point), customer + wallet, payments ledger, subscription card, doorstep notes, and actions **Reassign** (rider dropdown from `riders`, plus "All riders") and **Cancel** (reason required, confirm dialog), both visible only when allowed.

3. **Riders**: cards with online dot, last seen ("3 min ago"), on the way / today / all-time counts, phone link, "Open location" Google Maps link for `lastLocation`. Refresh every 30 s.

4. **Subscriptions**: table with customer, product × qty, per-delivery ₹, frequency, status, start date, last order date, vacations count. Status filter.

5. **Duplicates**: the §2.6 list with cancel buttons. Show a green "No duplicate orders" when empty.

6. **Settings**: the pickup point form (name, address, lat, lng + "Open in Google Maps" preview link).

### States every view must handle
Loading skeleton · empty ("No orders in this range") · backend waking (> 5 s) · error with retry (show the backend `message`) · 401 from our route (re-show the password gate).

---

## 4. Acceptance checklist

- [ ] `SAATHI_ADMIN_API_KEY` is only read in `app/api/admin/delivery/route.js`; `grep -r SAATHI_ADMIN_API_KEY app components lib` shows no client component.
- [ ] Wrong password → 401, no backend call made.
- [ ] Unknown `action` → 400; ids with `/` or `..` are rejected.
- [ ] Orders table matches the Saathi app: a rider marking an order delivered shows up within one refresh with rider name, time and photo.
- [ ] Proof photo renders in the drawer and still renders after closing/reopening an hour later (re-fetch).
- [ ] CSV opens in Excel with correct IST times and ₹ values.
- [ ] Reassign and Cancel refresh the row and show the backend error text on 409.
- [ ] Works on a 390 px wide phone.
- [ ] `npm run build` passes.
