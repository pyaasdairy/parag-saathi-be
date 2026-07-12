# Consumer backend (PARAG consumer app)

The consumer backend powers the **PARAG consumer e-commerce app** (a separate
React-Native app, repo `parag-consumer`). It is an **add-only** module inside the
same Saathi Go binary, mounted under `/api/v1/consumer`, and is deliberately
isolated from the operator (Saathi) side.

> **Which collection stores users?** The shopper (consumer) accounts live in
> **`consumer_accounts`**. A "user" of the consumer app is a row there, keyed by
> phone (`+91XXXXXXXXXX`). They are **NOT** Saathi `parties` — a shopper is never
> an operator. Store managers and delivery riders, by contrast, ARE Saathi
> operators (rows in `parties` + `role_assignments`), because "apart from the
> user, everything logs in from Saathi."

---

## 1. Isolation principles

- **Own collections** — everything the consumer owns is prefixed `consumer_*`; it
  never writes operator collections. (The delivery flow *reads* operator identity
  — `parties`, `role_assignments`, `org_units` — read-only, to resolve the store
  manager / rider / store.)
- **Own JWT** — consumer tokens are `kind:"consumer"`, issuer `saathi-consumer`,
  signed with a key **derived** from the shared secret via HMAC domain separation
  (`HMAC(JWTSecret, "consumer-jwt-v1")`). So a consumer token is cryptographically
  rejected on an operator route and vice-versa, even in one binary.
- **Raw-JSON wire** for consumer-app endpoints (the shipped `apiClient` reads
  `res.json()` + `{message}` errors) — NOT the operator `{data}` envelope.
- **Exception (operator surfaces)** — the store-manager / delivery-rider endpoints
  are consumed by the **Saathi FE client**, so they reuse Saathi's auth
  (`Authenticate` + `RequireRoles`) and the operator wire format (`{data}`
  envelope, camelCase, `{error:{code,message}}`).

---

## 2. Collections

| Collection | Purpose |
|---|---|
| **`consumer_accounts`** | **The users (shoppers).** phone, name, email, milk pref, status. |
| `consumer_otp_challenges` | Login OTP challenges (hashed code, TTL, attempt cap). |
| `consumer_refresh_tokens` | Rotating refresh tokens (hashed at rest). |
| `consumer_addresses` | Delivery addresses (single-default invariant). |
| `consumer_wallets` | Dual-bucket wallet: `cash_balance` (real) + `rewards_balance` (promo) + `held_amount`, `seq`. |
| `consumer_wallet_txns` | Append-only money ledger. Unique partial index `(consumer_id, ref_id, type)` = the **exactly-once money gate**. |
| `consumer_payment_orders` | Razorpay top-up orders (amount bound server-side; idempotency anchor). |
| `consumer_orders` | The order aggregate (items, totals, status, address, rider). |
| `consumer_deliveries` | The last-mile delivery task per order (status, rider, proof, geo). |
| `consumer_consents` | DPDP consent records. |

Operator collections **read** by the delivery flow: `parties`, `role_assignments`,
`org_units` (STORE org + its manager/riders).

---

## 3. End-to-end flows

### 3.1 Auth (consumer)
`POST /consumer/auth/otp/request {phone}` → mints an OTP (dev mode echoes
`dev_otp`; the consumer OTP screen shows it for testing until the SMS API lands).
`POST /consumer/auth/otp/verify {phone, code}` → **find-or-create** the
`consumer_accounts` row, seed an empty `consumer_wallets`, issue `{access_token,
refresh_token, profile}`. `POST /consumer/auth/refresh` rotates; `logout` revokes.
OTP has an attempt cap (burns the challenge after 5 wrong codes).

### 3.2 Profile + DPDP
`GET/PATCH /consumer/users/me` (whitelisted fields). `DELETE /consumer/users/me`
(erasure) cascades across addresses, wallet, ledger, consents, refresh, payment
orders, **and orders** (which carry PII), plus OTP-by-phone.

### 3.3 Wallet (dual-bucket, server-authoritative)
- **Money in (real)** — Razorpay: `POST /consumer/wallet/order {amountPaise}`
  binds the amount server-side → `POST /consumer/wallet/verify {payment_id,
  order_id, signature}` HMAC-verifies then credits **exactly once** (Cash +
  Rewards bonus per the FE tiers ≥200→50, ≥500→100, ≥1000→250, ≥10000→1000).
- **Dev top-up** — `POST /consumer/wallet/topup` direct-credits, **gated to dev
  mode** (never mints money in prod).
- **Spend** — `POST /consumer/wallet/debit` spends **promo-first** in a single
  atomic guarded update (no overdraw race), idempotent by ref.
- **Refund** — `POST /consumer/wallet/refund` (dev-gated; real refunds are
  order-driven later).
- **Reads** — `GET /consumer/wallet`, `GET /consumer/wallet/txns`.
The FE `walletApi.ts` reads/writes these when configured (local ledger is the
offline fallback).

### 3.4 Orders
`POST /consumer/orders` — totals **recomputed server-side** from items (client
total ignored; fair-use qty/units caps); auto-creates a delivery task at the
nearest Parag Store. `GET /consumer/orders` (scoped to the token), `GET
/consumer/orders/{id}`, `cancel` (placed/confirmed guard), `review` (delivered
guard). Money is debited **on delivery** (see 3.5), never at placement.

### 3.5 Last-mile delivery (store manager + rider = Saathi operators)
Auth: **Saathi operator token** (OTP→session→`role/select`) with role
`STORE_MANAGER` / `DELIVERY_RIDER`, scoped to a `STORE` org unit.
- **Store manager** — `GET /consumer/stores/{storeId}/orders`,
  `GET /consumer/stores/{storeId}/riders?delivery_id=` (ranked by **15 → 30 → 60
  km tier**; distance behind a maps seam — currently store→address haversine),
  `POST /consumer/stores/{storeId}/orders/{deliveryId}/assign {rider_party_id}`.
- **Delivery rider** — `GET /consumer/delivery/tasks`, `.../accept`, `.../pickup`
  (→ out-for-delivery, streams location), `.../location`, `.../deliver`
  (photo+geo+geofence proof), `.../fail`.
- **Wallet cut on delivery** — `deliver` debits the consumer wallet **exactly
  once**, keyed to the order (same ref as the FE `settleDeliveredOrders` sweep, so
  it can never double-charge). Riders are **salaried** — no delivery-payment logic.
- **Consumer tracking** — pickup / location / deliver sync the consumer order's
  status + rider + live location, so the user sees the rider move and the order
  complete via `GET /consumer/orders/{id}`.

### 3.6 QR / traceability bridge (the one Saathi↔consumer touchpoint)
`GET /consumer/traceability/{code}` resolves a scanned **pack QR** by reusing the
operator public trace resolver (read-only) → the consumer `MilkBatch`.
`GET /consumer/traceability/{code}/label` renders an HTML provenance passport
(all values + a server-rendered QR image) the app prints to PDF.
**Consumer-app-only**: printed QRs encode `parag://trace/<code>` (opens the PARAG
app only), and both endpoints require `X-Parag-App-Key` = `CONSUMER_APP_KEY`
(403 for any other client).

---

## 4. Env vars

| Var | Purpose |
|---|---|
| `CONSUMER_APP_KEY` | App-key gate for the traceability bridge (FE sends `X-Parag-App-Key`). |
| `RAZORPAY_KEY_ID` / `RAZORPAY_KEY_SECRET` | Real Razorpay top-ups; empty → offline dev seam (dev mode only). |
| `OTP_DEV_MODE` | Echoes `dev_otp` + enables dev-only paths (topup/refund/advance). Prod refuses to boot with it on. |

Shared with Saathi: `JWT_SECRET`, `OTP_HASH_SECRET`, `QR_SIGNING_SECRET`, `MONGO_URI`.

---

## 5. Verification (dry-run harnesses)

Driven through the **real** shipped consumer FE lib (+ real operator login) —
`consumer/harness/`:

| Harness | Covers | Result |
|---|---|---|
| `c0-phase0`, `c0-guards` | auth, profile, wallet, addresses + money guards | 19/19 + 5/5 |
| `c1-payments` | Razorpay order→verify money-in | 8/8 |
| `c2-wallet`, `c2-orders`, `c2-guards` | server wallet, orders, security fixes | 7/7 + 8/8 + 7/7 |
| `c3-trace`, `c4-qr` | QR bridge + consumer-app-only + PDF label | 4/4 + 5/5 |
| `c5-delivery` | full last-mile: order → store assign by tier → rider deliver → wallet cut → tracking | 11/11 |

All money is server-authoritative and exactly-once; the full Saathi Go suite stays
green (the consumer module is add-only).
