# Saathi — Running the Backend + FE↔BE Integration Notes

> Portable reference for running the Go backend locally and wiring the Expo app to it,
> including a full FE↔BE contract-alignment audit. Written for the "one folder" setup
> where the backend lives inside the frontend repo.

---

## 0. Recommended one-folder layout

Merging the Go backend flat into the Expo root causes file collisions (`.env`, `README.md`,
`docs/`, `.gitignore`, `SAATHI_Architecture_Framework_Parag.pdf` exist in BOTH repos).
Cleanest is to drop the backend into a **subfolder**:

```
parag-saathi-fe/            # Expo app at root (app/, src/, package.json, app.json)
├─ backend/                 # ← copy the entire parag-saathi-be repo here
│  ├─ cmd/  internal/  scripts/
│  ├─ go.mod  go.sum  Makefile  docker-compose.yml
│  └─ .env                  # backend env (Mongo, JWT, OTP) — separate from the app's .env
├─ .env                     # frontend env (EXPO_PUBLIC_*)
├─ package.json  app.json
└─ src/ app/ ...
```

Backend commands then run from `backend/` (`cd backend && go run ./cmd/server`).
If you instead merge flat at the root, keep ONE combined `.env` (see §4) and manually
resolve the duplicate `README.md` / `docs/` / PDF.

> `go.mod` (module `github.com/pyaas/saathi-backend`) and `package.json` coexist with zero
> conflict — Go and Node ignore each other's files.

---

## 1. Prerequisites

| Tool | Needed for | Install |
|---|---|---|
| Go 1.23+ | backend | already present (`go version`) |
| Node 20+ / npm | Expo app | already present |
| **MongoDB Community** | backend datastore | `brew tap mongodb/brew && brew install mongodb-community` |
| mongosh | `make smoke` E2E test | `brew install mongosh` |
| watchman | Metro hot-reload on Node 26 | `brew install watchman` |
| Android Studio + emulator | run the app | AVD named `parag_pixel` |

> **Docker note:** `brew install --cask docker-desktop` fails on this Mac — it needs a sudo
> password (to link its CLI into `/usr/local/bin`) plus a GUI first-run that can't be scripted.
> Use the Homebrew MongoDB path below; it needs no password. (If you want Docker later, run the
> cask install yourself in Terminal and accept Docker Desktop's first-run, then `docker compose up`.)

---

## 2. Run the backend (:8080)

```bash
cd backend                              # (or repo root if merged flat)

# 2a. Mongo as a background service on :27017
brew services start mongodb-community
mongosh --quiet --eval 'db.runCommand({ping:1}).ok'   # → 1

# 2b. Make sure nothing else holds :8080 (the old parag-bridge did)
lsof -nP -iTCP:8080 -sTCP:LISTEN -t | xargs -r kill

# 2c. Boot the API (first run compiles + downloads deps, ~1–2 min)
go run ./cmd/server          # or: make run
# → "saathi backend listening port=8080 ... mongodb connected db=saathi"

# 2d. Seed demo data (idempotent: org tree, one party per role, rate chart)
go run ./cmd/seed            # or: make seed

# 2e. Verify
curl -s localhost:8080/healthz   # {"data":{"status":"ok"}}
curl -s localhost:8080/readyz    # {"data":{"status":"ready"}}   (pings Mongo)
curl -s localhost:8080/version   # {"data":{"service":"saathi-backend","version":"0.1.0-dev"}}

# 2f. (optional) Full end-to-end smoke test — spins its OWN server on :8091 against a
#     scratch DB (saathi_smoke), drives the whole pour→QR→settlement loop, 23 assertions.
make smoke                   # needs mongosh + python3 + curl
```

Backend `.env` (minimum — the server refuses to boot without `MONGO_URI` and `JWT_SECRET`):

```dotenv
ENV=dev
PORT=8080
LOG_LEVEL=info
MONGO_URI=mongodb://localhost:27017
MONGO_DB=saathi
JWT_SECRET=dev-only-jwt-secret-change-me
QR_SIGNING_SECRET=dev-only-qr-secret-change-me
OTP_HASH_SECRET=dev-only-otp-secret-change-me
OTP_DEV_MODE=true                # dev OTP echoed in the login response
ACCESS_TOKEN_TTL_MINUTES=15
REFRESH_TOKEN_TTL_DAYS=30
OTP_TTL_MINUTES=5
RATE_LIMIT_RPS=50
RATE_LIMIT_BURST=100
SEED_ADMIN_PHONE=9999999999
```

---

## 3. Run the frontend + point it at the backend

```bash
cd parag-saathi-fe           # the root of the merged folder
export ANDROID_HOME="$HOME/Library/Android/sdk"
export JAVA_HOME="/Applications/Android Studio.app/Contents/jbr/Contents/Home"
export PATH="/opt/homebrew/bin:$ANDROID_HOME/platform-tools:$ANDROID_HOME/emulator:$JAVA_HOME/bin:$PATH"
emulator -avd parag_pixel &
npx expo start --dev-client --clear --android
```

Frontend `.env` (already correct):

```dotenv
EXPO_PUBLIC_API_MODE=http
EXPO_PUBLIC_API_BASE_URL=http://10.0.2.2:8080/api/v1
```

- `10.0.2.2` = the Android emulator's alias for the host Mac's `localhost`. On a **physical
  device** on the same Wi-Fi, swap it for your Mac's LAN IP, e.g. `http://192.168.x.x:8080/api/v1`.
- **Log in with the BE seed phones (http mode), NOT the mock ones.** The OTP is returned in the
  login response (`OTP_DEV_MODE=true`):

  | Role | Phone |
  |---|---|
  | Super Admin | `9999999999` |
  | Sacheev + Farmer (role picker) | `9000000001` |
  | Samiti Adhyaksh | `9000000002` |
  | Farmer | `9000000011` |
  | Van Rider | `9000000021` |
  | BMC Operator | `9000000031` |
  | Plant Operator | `9000000041` |
  | Plant Lab Analyst | `9000000042` |
  | Organising Manager | `9000000071` |

  > The `9876…` phones + OTP `123456` documented in the FE `CLAUDE.md` are **mock-mode only**
  > (`EXPO_PUBLIC_API_MODE=mock`); they do not exist in the backend seed. Public `/trace` needs no login.
- If every action shows "Something went wrong", you're in http mode with no backend reachable —
  start the backend (§2) or flip `EXPO_PUBLIC_API_MODE=mock` and restart Metro.

---

## 4. If you merge flat (one shared `.env`)

A single root `.env` works because Go reads unprefixed keys and Expo only inlines `EXPO_PUBLIC_*`:

```dotenv
# ── App (Expo) ──
EXPO_PUBLIC_API_MODE=http
EXPO_PUBLIC_API_BASE_URL=http://10.0.2.2:8080/api/v1
EXPO_PUBLIC_MAPS_PROVIDER=osm
# ── Backend (Go) ──
MONGO_URI=mongodb://localhost:27017
MONGO_DB=saathi
JWT_SECRET=dev-only-jwt-secret-change-me
OTP_DEV_MODE=true
PORT=8080
```

Also concatenate the two `.gitignore`s (backend ignores `.env`, `bin/`, the `seed` binary;
frontend ignores `node_modules/`, `.expo/`).

---

## 5. FE ↔ BE integration alignment (audited 2026-07-10, http mode, base `/api/v1`)

**The entire core loop is aligned and proven** — all 23 `make smoke` assertions pass
(pour → invoice → logistics → FSSAI safety gate → batch → QR → public trace → settlement,
plus KYC gate, concurrency guard, live SSE badge). Transport, auth (OTP dev flow, role select,
refresh), the `{data}` / `{error:{code,message,details}}` envelopes, role codes, and the
pour/QC/QR DTOs all match. **68 FE calls map cleanly to a BE route.** The gaps below are all in
**secondary features**, not the core pour→payment→QR path.

### 🔴 Real http-mode 404s (6) — FE calls a route the BE doesn't serve, and it's NOT flagged mock-only
These fire a real request in http mode and 404. Several carry FE comments wrongly claiming they're live.

| FE call | Missing BE route |
|---|---|
| `commerce.listProducts` (commerce.ts:44) | `GET /products` (only `GET /admin/products` exists, admin-gated) |
| `collections.getLot` (collections.ts:469) | `GET /logistics/consignments/{id}` |
| `collections.approveForUnion` (collections.ts:626) | `POST /logistics/consignments/{id}/approve-union` |
| `collections.getConsignmentInvoice` (collections.ts:670) | `GET /logistics/consignments/{id}/invoice` |
| `admin.getSachivCap` / `setSachivCap` (admin.ts:445,458) | `GET` / `PUT /admin/sachiv-cap` |

**Fix:** for each, either add the BE route or add `mockOnly: true` to the FE endpoint spec.

### 🟠 Latent 404s (13) — `// TODO(backend)` but missing `mockOnly: true`
A `// TODO(backend)` comment does NOT stop the request — only `mockOnly: true` does (see
`src/core/api/client.ts:188`). So these 13 fire real requests and 404 in http mode:

- `delivery.ts` (×9): `listMyDeliveries, getDelivery, acceptDelivery, startDelivery, completeDelivery, pushRiderLocation, failDelivery, getWallet, listWalletTxns` → `/delivery/tasks…`, `/wallet`, `/wallet/txns`
- `store.ts` (×3): `listInventory, listStoreOrders, listStoreRiders` → `/stores/{id}/inventory|orders|riders`
- `traceability.ts`: `listQrCodes` → `/trace/codes`

**Fix (safe/mechanical):** add `mockOnly: true` to each so the "mock only" intent is actually enforced.

### 🟡 Contract nits (don't crash the core loop)
- **Invoice status enums differ:** FE `DRAFT/APPROVED/SETTLED` vs BE `ISSUED/SETTLEMENT_PENDING/PAID/HOLD`.
  Read path is bridged by `mapInvoiceStatus`; the **write path is not** — a `?status=` filter sends the
  FE word verbatim and BE would 400. Mirror `listLots` and reverse-translate before setting `?status=`.
- **Pour `temperature_c`:** FE sends it; BE `CreatePourRequest`/`domain.MilkPour` have no such field, so Go
  silently drops it → cold-chain temp never persists. Add `TemperatureC` to the BE pour DTO + repo if needed.
- **`DUAL_CONTROL_VIOLATION`:** FE keys a dedicated message on this code, but BE returns code `FORBIDDEN`
  (the string sits only in `details.reason`). Either emit the specific code from BE, or have the FE fall
  back to `details.reason`.
- **Public-trace test name:** BE stamps canonical `AFLATOXIN_M1`; FE casts `t.name` raw instead of routing
  through `mapParameter()` (which normalizes `AFLATOXIN_M1 → AFM1`). Fix in `traceability.ts:156`.
- **List pagination `meta` dropped:** FE http client returns only `json.data`, never `json.meta`
  (`{limit,offset,total}`). Harmless today (no FE screen paginates); return `meta` when paging lands.
- **No CORS on the BE:** irrelevant for the Android emulator (React Native's native `fetch` doesn't enforce
  CORS), but an **Expo-web / browser** build hitting a cross-origin host would fail preflight. Add
  `go-chi/cors` middleware only if you build for web.

### ✅ Settlement writes — deliberately mock-only, so no runtime break
FE `approveInvoices` / `settleInvoices` / `approveRelease` (`payments.ts`) are `mockOnly: true`. The BE's
real settlement flow is **batch-id keyed with dual control** (`POST /settlements` → `{id}/approve` →
`{id}/execute`), not the invoice-id shape the FE mocks. Wire the FE to the batch flow when you take
settlement live.

---

## 6. Handy commands

```bash
# health
curl -s localhost:8080/readyz

# a full manual login (http mode, as the app does it)
BASE=http://localhost:8080/api/v1
OTP=$(curl -s -X POST $BASE/auth/otp/request -H 'Content-Type: application/json' \
      -d '{"phone":"9000000001"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["dev_otp"])')
curl -s -X POST $BASE/auth/otp/verify -H 'Content-Type: application/json' \
     -d "{\"phone\":\"9000000001\",\"otp\":\"$OTP\"}"

# stop / restart
lsof -nP -iTCP:8080 -sTCP:LISTEN -t | xargs -r kill   # stop backend
brew services stop mongodb-community                  # stop Mongo
```

---

## 7. Priority checklist to "fully connect" the app

1. **Add `mockOnly: true`** to the 13 §5-🟠 endpoints (delivery ×9, store ×3, `listQrCodes`) — stops the latent 404s, zero risk.
2. **Decide per §5-🔴 route** (6): build the BE endpoint, or gate the FE call with `mockOnly: true`.
3. **Translate the invoice `?status=` filter** and (if used) **persist pour `temperature_c`**.
4. **Normalize public-trace test names** through `mapParameter()`.
5. Wire settlement writes to the BE batch flow when going live.

_Core pour→payment→QR loop needs none of the above — it already works end to end._
