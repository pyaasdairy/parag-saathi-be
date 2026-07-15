# Saathi Backend — Brief

## What this is

The backend for **Saathi**, the dairy supply-chain and traceability platform of the
Nand Baba Dugdh Mission (UP / PCDF–Parag). One Go binary follows milk from a farmer's
pour to a consumer's QR scan: identity/RBAC → pour → same-day invoice → logistics →
FSSAI safety gate → batch → QR → public trace → settlement, plus a consumer e-commerce
bridge for the separate PARAG shopper app.

## Tech stack

- **Go 1.24** modular monolith, **chi v5** router, **MongoDB** (mongo-driver v1.17).
- 14 domain modules under `internal/modules/`, each a future microservice seam:
  handler (HTTP) → service (logic) → repo (Mongo). Modules never import each other;
  cross-module effects go through an in-process event bus.
- Append-only, hash-chained **provenance ledger** (`provenance_events`) as the trace
  source of truth; immutable **audit log** wraps every mutating `/api/v1` call.
- SSE stream (`/api/v1/events/stream`) for live client updates; optional Redis for
  shared rate limiting across replicas.

## How to run

```bash
cp .env.example .env   # set MONGO_URI (server refuses to boot without it)
make tidy && make run  # API on :8080
make seed              # idempotent demo data (org tree, one party per role, rate chart)
make test && make smoke  # unit tests + E2E pour→payment→QR loop
```

With `OTP_DEV_MODE=true` the OTP comes back in the login response (no SMS needed).
Seeded phones: super-admin `9999999999`, sacheev `9000000001`, farmer `9000000011`, …
Health: `GET /healthz`, `/readyz` (Mongo ping), `/version`.

## Architecture map

- `cmd/server`, `cmd/seed` — the API binary and the idempotent seeder.
- `internal/httpapi` — router, global middleware pipeline (RequestID → metrics →
  logging → rate limit → recover → 30s timeout), module registration.
- `internal/platform` — shared services: `auth` (JWT + OTP + HMAC), `middleware`
  (RBAC), `mongodb` (client + canonical collection names/indexes), `provenance`,
  `audit`, `eventbus`, `flags`, `orgscope`, `ratelimit`, `sse`, `httpx`, `logger`.
- `internal/domain` — pure entity contract (roles, FSSAI limits, statuses).
- `internal/modules/` — the 14 modules:
  - **identity** — OTP login, refresh rotation, role selection, Party registry, KYC approval workflow.
  - **orgs** — FEDERATION → MILK_UNION → {PLANT, BMC, DCS} org tree, directory, membership roster.
  - **onboarding** — ONBOARDING_EXECUTIVE doorstep-enrolment queue; approval runs a find-or-create-party + KYC + role-grant saga.
  - **collection** — rate charts, anti-tamper analyzer readings, offline-first idempotent milk pours, per-farmer daily invoices.
  - **logistics** — DCS consignments, van-rider route trips with cold-chain temps, handover to BMC; every hop chained into provenance.
  - **quality** — THE safety-gate verdict writer (AFM1 ≤ 0.5 µg/kg etc.); the only module that moves a lot/batch out of QC_PENDING. Also mints consignment batch QRs (below).
  - **plant** — BMC lots, processing batches, product lots, product QR issuance; enforces that a blocked subject never advances.
  - **settlement** — dual-control same-day payment rail (below); subsidies ride a separate PFMS/DBT seam.
  - **cattle** — Pashu-Aadhaar animal registry, health events, mock Bharat Pashudhan sync, 1962 MVU dispatch, dormant collar path.
  - **consumer** — bridge for the PARAG shopper app (below): auth, wallet (Razorpay), orders, last-mile delivery, traceability bridge.
  - **publictrace** — unauthenticated `/public/qr/{code}` consumer scan + ledger tamper-check.
  - **dashboards** — read-only home-screen aggregates (farmer summary, society stats).
  - **cms** — versioned delta-pull content (scheme cards, videos, helplines).
  - **platformops** — feature flags, auditor export, notifications outbox + mock SMS worker, support-agent lookup.

## Key flows

**Auth & RBAC.** Phone + OTP (`POST /api/v1/auth/otp/request` → `/otp/verify`) issues a
**session token** ("this phone logged in"). `POST /auth/role/select` exchanges it for a
**role token** pinned to one active, org-scoped role assignment. `RequireRoles(...)`
middleware demands a role token (distinct `ROLE_TOKEN_REQUIRED` error so the app
re-selects) and allows `SUPER_ADMIN` through any role gate. Services additionally
enforce org scope (`RequireInScope`).

**QR system.** Two QR families, both HMAC-signed with `QR_SIGNING_SECRET`:
per-samiti **consignment batch QRs**, auto-minted in the quality module when the lab
records a QC **PASS** (`token = HMAC(secret, batch_code)[:8]`,
`quality/consignment_qc.go`), and **product-lot QRs** (plant module) whose token
carries its signed payload (`b64(payload).hexmac`). Both resolve at the public
`GET /public/qr/{code}`. Because tokens are pure derivations of durable fields + the
secret, `consumer/qrresign.go` runs at boot (background, best-effort) and re-signs
every stored token after a secret rotation — otherwise all previously printed QRs
would fail the integrity gate (409 `QR_INTEGRITY_FAILED`) forever.

**Consumer bridge** (`/api/v1/consumer`, docs/consumer-backend.md). Shoppers live in
`consumer_accounts` (never `parties`) with their own JWT kind (`consumer`, key derived
via HMAC domain separation) and a raw-JSON wire format. Covers OTP auth, DPDP erasure,
a dual-bucket wallet with Razorpay top-up (exactly-once credit gate), server-priced
orders, and last-mile delivery driven by Saathi operators (STORE_MANAGER /
DELIVERY_RIDER). The traceability bridge — `GET /consumer/traceability/{code}` and
`/{code}/label` — is gated by the `X-Parag-App-Key` header (constant-time check when
`CONSUMER_APP_KEY` is set); `/label` renders a printable label page (all values + QR
image) that the apps turn into a PDF.

**Settlement dual-control** (`settlement/service.go`). A Sachiv computes and initiates
a settlement batch over ISSUED invoices (atomically claimed so each invoice lands in
exactly one batch); a **different** human (Adhyaksh) approves. `validateApproval`
checks `InitiatedBy != approver` on the party identity itself, first — so not even
SUPER_ADMIN's role bypass can approve a batch it initiated. A mocked licensed Payment
Aggregator then executes payouts.

## Key MongoDB collections

Canonical list + indexes: `internal/platform/mongodb/indexes.go` (~35 collections).
Most important:

| Collection | Holds |
|---|---|
| `parties` / `role_assignments` | One phone = one Party; org-scoped, time-bounded roles |
| `org_units` | The cooperative tree (Federation → Union → Plant/BMC/DCS) |
| `milk_pours` / `invoices` | The pour → same-day invoice loop (idempotent by client_event_id) |
| `dcs_consignments` | Pooled per-samiti shift consignments (logistics) |
| `consignment_qc` / `consignment_batch_qrs` | Lab batch verdicts + the QRs minted on PASS |
| `processing_batches` / `batch_qrs` | Plant batches and product-lot QRs |
| `settlement_batches` | Dual-control payment batches |
| `provenance_events` | Hash-chained append-only trace ledger |
| `audit_logs` | Immutable who-did-what trail |
| `consumer_*` (10 collections) | Shopper accounts, wallets/ledger, orders, deliveries |

## Config & env

Env-only, documented in `.env.example` and `render.yaml`: `MONGO_URI` + `MONGO_DB`
(required), `JWT_SECRET`, `QR_SIGNING_SECRET`, `OTP_HASH_SECRET`, `CONSUMER_APP_KEY`,
`RAZORPAY_KEY_ID`/`RAZORPAY_KEY_SECRET`, `OTP_DEV_MODE`, token TTLs, rate-limit knobs,
seed settings. Secrets are never in git (`sync: false` in render.yaml).

## Current release state

- Release branch `release/26.07.02` exists in all three repos (backend, Saathi FE, consumer FE).
- Production backend: **https://saathi-backend-xcxn.onrender.com** (Render, Dockerfile
  + `render.yaml` blueprint, MongoDB Atlas; see `DEPLOY_RENDER.md`).
- QR labels download as PDF from the consumer app and print as PDF from the lab screen.
- `OTP_DEV_MODE=true` is intentional for this phase (OTP echoed in the response); flip
  it and wire an SMS provider before production proper.

## Where to read more

- `docs/strict_brief.md` — the API contract: every endpoint, auth, ground rules.
- `docs/BACKEND_FLOW.md` — Mermaid diagrams of the system as built.
- `docs/WORKFLOW_GUIDE.md` — who does what, role by role, with flow diagrams.
- `docs/TECHNICAL_NOTES.md` — architecture decisions, what's mocked vs real, next steps.
- `docs/consumer-backend.md` — the consumer bridge module in depth.
- `docs/FRONTEND_INTEGRATION.md` — mobile-app integration (auth, routes, SSE, errors).
- `docs/PCDF_Cooperative_Constitution.md` — the real cooperative structure behind the org model.
