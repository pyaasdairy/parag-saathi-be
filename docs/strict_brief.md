# Saathi Backend — Strict Brief (How Every API Works)

> **Purpose.** This is the single source of truth for *how the backend is built and how each API
> behaves* — the contract a frontend engineer, a new backend dev, or an LLM needs to integrate
> without reading every file. It is generated from the actual routes in `internal/modules/*/module.go`
> and kept in lockstep with the code.
>
> **Scope reminder:** backend only. The frontend is a separate repo. Consumer commerce is
> descoped (only the public QR scan is consumer-facing). Every route below is live and covered by
> `scripts/smoke.sh` (21-step E2E).

---

## 1. Ground rules that apply to EVERY endpoint

### 1.1 Identifiers — ObjectID everywhere, business keys are separate
- Every document's `_id` is a **MongoDB ObjectID** (24-hex string in JSON, e.g. `6a4b607236c46b14a294ec4d`).
- **Every cross-collection reference stores the ObjectID**, never a name or code:
  `farmer_party_id`, `dcs_id`, `org_unit_id`, `batch_id`, `settlement_batch_id`, … This is what
  makes `$lookup` / populate trivial and mapping unambiguous.
- **Human-readable business keys are separate, unique-indexed fields** — used for display and
  lookup, never for joins: org `code` (`DCS-01842`), party `phone`, `invoice_number`,
  `batch_number`, `qr_code`, `pashu_aadhaar`.
- **One deliberate exception:** `milk_pours.client_event_id` is a **string** minted on the device
  (offline-first idempotency key, blueprint §3.1) — the server still assigns a proper ObjectID `_id`.
- In JWTs and query params, ObjectIDs travel as hex strings and are parsed server-side via
  `httpx.ParseID` / `httpx.PathID`. A malformed id → `400 INVALID_ID`.

### 1.2 Response envelope (uniform)
Success:
```json
{ "data": { ... }, "meta": { "limit": 50, "offset": 0, "total": 137 } }   // meta only on list endpoints
```
Error:
```json
{ "error": { "code": "SAFETY_GATE_BLOCKED", "message": "human readable", "details": {...}? } }
```
`code` is a stable machine string the frontend switches on. `500`s are opaque (`INTERNAL`) — the
real cause is logged server-side, never leaked.

### 1.3 Auth — two token kinds
1. `SESSION` token — proves "this phone logged in". Enough for identity/profile endpoints.
2. `ROLE` token — a session that has **selected one role**; carries `role_code` + `org_unit_id` +
   `org_type`. **Required for every business action.** Obtain via `POST /auth/role/select`.

Middleware chain per route group:
`Authenticate` (verifies JWT → puts `Actor` on context) → `RequireRoles(...)` or `RequireSession`
→ handler → (mutations) `AuditMutations` writes the audit trail. **`SUPER_ADMIN` passes every
`RequireRoles` gate** (and every such call is audited).

### 1.4 Org scoping (multi-tenant safety)
Before touching a resource that belongs to an org unit, the service calls
`d.Orgs.RequireInScope(actor, targetOrgID)`. Passes when the actor's org == target, or the
actor's org is an **ancestor** in the target's denormalized `path[]` (O(1), no tree walk).
`SUPER_ADMIN` and `STATE_AUDITOR` are federation-wide. Failure → `403 FORBIDDEN`.

### 1.5 Provenance (append-only, hash-chained)
State-changing steps in the milk→QR chain append an immutable event to `provenance_events`
(SHA-256 chained on the previous hash). Corrections never mutate — they append a new event with a
`supersedes` ref. This is what the public QR scan re-verifies (`ledger.intact`).

### 1.6 Logging & error handling (debugging backbone)
- **One structured log line per request** (global middleware): `method, path, status, duration_ms,
  request_id, ip, actor_party_id, actor_role`. `4xx → WARN`, `5xx → ERROR`.
- **Inside each module** (tagged `module=<name>`, correlatable by `request_id`):
  `INFO` on every creation/state transition, `WARN` on every business rejection (gate block,
  dual-control violation, scope denial, idempotent replay, 409s), `ERROR` with the wrapped cause
  before any `500`. Repo errors are wrapped with the operation name (`fmt.Errorf("insert pour: %w")`).

### 1.7 Pagination
List endpoints accept `?limit=` (default 50, max 200) & `?offset=`, and return `meta.total`.

### 1.8 Concurrency & duplicate prevention (how "only one wins")
Every "two actors at once → only one completes" case is enforced by a **MongoDB atomic
status-guarded update** — the state check lives *inside* the update filter
(`update where _id=X AND status=EXPECTED`), so exactly one writer matches and the rest get a
`409`. This is used for KYC approve/reject, settlement `execute` (with a resume lease), and the
BMC-lot→batch claim (`DISPATCHED → POOLED`, so one lot can never feed two batches). **No Redis
lock is used for this** — a single-document atomic update is strictly safer than an external lock
(one source of truth, no lock-vs-DB drift, no TTL failure modes). Offline pour ingest dedupes the
same way via the unique `client_event_id`.

### 1.9 Real-time (live dashboards)
`GET /api/v1/events/stream` is a **Server-Sent Events** stream (any authenticated party). The
server pushes typed events down the open connection so an already-open dashboard updates without a
refresh — e.g. `kyc.pending.changed` nudges reviewer dashboards to re-fetch their scoped
`/kyc/pending/count` badge. The nudge is never trusted as the value; the client re-queries the
authoritative scoped count (so it can never drift). Single-instance today; at multi-replica scale
the cross-node fan-out swaps to Redis pub/sub with no route changes.

### 1.10 Rate limiting (single-instance vs replicas)
Per-IP token bucket. **In-process by default** (each replica limits what it sees). Set `REDIS_URL`
and it switches to a **shared Redis token bucket** (atomic Lua) for global fairness across
replicas — fails *open* on a Redis outage so the limiter can never take down the API. Over-limit →
`429 RATE_LIMITED`.

---

## 2. Layered architecture (how a request flows through the code)

```
HTTP request
  → router (internal/httpapi)         global middleware: request-id, metrics, request-log, rate-limit, recover
  → module.go                         mounts routes + auth/RBAC; wires handler→service→repo; subscribes to bus
  → handler.go                        HTTP ONLY: decode (httpx.DecodeJSON), parse ids, call service, write envelope
  → service.go                        ALL business logic: validation, RBAC/scope, gates, provenance, bus, logging
  → repo.go                           ALL MongoDB access: collection consts, ObjectID filters, no business logic
  → models.go                         request/response DTOs for the module
```
Modules never import each other. Cross-module effects go through the **in-process event bus**
(`eventbus`), which is the seam that becomes Kafka/NATS when this splits into microservices.
Shared services arrive via one `deps.Deps` container: `Cfg, Log, DB, JWT, Ledger, Audit, Flags, Orgs, Bus`.

---

## 3. The 10 modules & every endpoint

> Legend — **Auth**: `public` (none) · `session` · `role:X,Y` (needs a role token with one of those
> roles; `SUPER_ADMIN` always allowed). Paths are under `/api/v1` unless marked public.

### 3.1 `identity` — auth, parties, KYC approval, role grants
The heart of "one phone = one Party = many roles" + the KYC approval workflow.

| Method | Path | Auth | What it does |
|---|---|---|---|
| POST | `/auth/otp/request` | public | Sends 6-digit OTP (stored HMAC-hashed, 5-min TTL, max 5 attempts). Dev mode returns `dev_otp`. |
| POST | `/auth/otp/verify` | public | Verifies OTP → find-or-create Party (tier `MINIMAL`) → returns `session` token + rotating refresh token. |
| POST | `/auth/refresh` | public | Rotates refresh token → new session token. |
| POST | `/auth/logout` | session | Deletes the refresh token. |
| GET | `/auth/roles` | session | Lists my usable role assignments, each enriched with org `{id, code, name, type}`. **This is what the frontend reads to know which dashboard(s) to offer.** |
| POST | `/auth/role/select` | session | Body `{role_assignment_id}`. Checks it's mine + usable + my KYC tier satisfies the role's `RequiredKYCTier` (else `403 KYC_TIER_INSUFFICIENT`). Returns a `role` token. **This is the role switcher.** |
| GET | `/parties/me` | session | My party + active assignments + KYC summary (masked). |
| PATCH | `/parties/me` | session | Update `full_name` / `preferred_language`. |
| POST | `/kyc/aadhaar` | session | Body `{aadhaar_number, consent:true, requested_tier}`. Stores **only last-4 + a vault ref** (never the number). Creates a **PENDING** KYC record. **No auto-approval.** |
| POST | `/kyc/bank` | session | Mock penny-drop; attaches masked bank details to a PENDING record. |
| GET | `/kyc/me` | session | My KYC records (masked; vault ref never serialized). |
| GET | `/kyc/pending` | role:`ORGANISING_MANAGER, DISTRICT_VERIFIER, PCDF_ADMIN, SUPER_ADMIN` | Pending records **within the reviewer's org scope**, enriched with party `{phone, name, tier}`. Audited. |
| GET | `/kyc/pending/count` | role:(same) | Live badge value — scoped count of pending KYC. The dashboard renders this and re-fetches it on each SSE `kyc.pending.changed` nudge. Returns `{count, capped}`. |
| POST | `/kyc/{id}/approve` | role:(same) | Verifies scope + **not self-review** + `CanApproveKYCTier(role, tier)`; sets `VERIFIED` via an **atomic status-guarded update** (two simultaneous approvals → exactly one wins, the other gets `409 KYC_NOT_PENDING`), **upgrades party tier upward only**, notifies the party. Audited. |
| POST | `/kyc/{id}/reject` | role:(same) | `{reason}` → `REJECTED` + reason, notifies party. Audited. |
| POST | `/roles/assignments` | role:`SUPER_ADMIN, PCDF_ADMIN, UNION_PRESIDENT, SAMITI_ADHYAKSH, ORGANISING_MANAGER` | Grants a role at an org unit (this is **"deciding a user's position"**). Scope-checked; grant matrix limits who can grant what. Audited (`role.grant`). |
| DELETE | `/roles/assignments/{id}` | role:(same) | Revokes (status → `REVOKED`, never deleted). Audited (`role.revoke`). |
| GET | `/roles/assignments` | role:(same) | Lists assignments at an org (scope-checked, paged). |

**The position/onboarding flow (exact):** login → `POST /kyc/aadhaar` (PENDING) → reviewer
`POST /kyc/{id}/approve` (tier upgraded) → an admin `POST /roles/assignments` to set the position
(e.g. `SAMITI_SACHEEV @ DCS-01842`) → user logs in, `GET /auth/roles` shows it, `POST /auth/role/select`
issues the role token whose `role_code` tells the frontend which UI to render.

### 3.2 `orgs` — cooperative hierarchy (Federation → Union → Plant/BMC/DCS)

| Method | Path | Auth | What it does |
|---|---|---|---|
| POST | `/orgs` | role:`SUPER_ADMIN, PCDF_ADMIN` | Create org unit; validates parent-type edge; builds ancestor `path[]`; unique `code`. |
| PATCH | `/orgs/{id}` | role:`SUPER_ADMIN, PCDF_ADMIN` | Update name/district/active/geo (no type/parent moves in v1). |
| GET | `/orgs` | session | List/filter (`?type=`, `?district=`, **`?code=DCS-01842`** exact business-key lookup). |
| GET | `/orgs/{id}` | session | One org unit. |
| GET | `/orgs/{id}/children` | session | Direct children (paged). |
| GET | `/orgs/{id}/tree` | role:`SUPER_ADMIN, PCDF_ADMIN, MISSION_OFFICIAL, UNION_PRESIDENT, STATE_AUDITOR` | Subtree via `path` containment (capped 500). |
| GET | `/orgs/{id}/members` | role:`SAMITI_SACHEEV, SAMITI_ADHYAKSH, UNION_FIELD_SUPERVISOR, UNION_PRESIDENT, PCDF_ADMIN` | Active role-holders at this org (scope-checked, paged). |

### 3.3 `collection` — THE CORE LOOP (readings → pours → invoices)

| Method | Path | Auth | What it does |
|---|---|---|---|
| POST | `/collection/rate-charts` | role:`PCDF_ADMIN, UNION_PRESIDENT` | Create a fat/SNF rate chart; deactivates prior active charts of that org. |
| GET | `/collection/rate-charts/active?dcs_id=` | session | Resolves the nearest-ancestor active chart for a DCS. |
| POST | `/collection/readings` | role:`SAMITI_SACHEEV, MILK_TESTER` | Records an analyzer reading with the anti-tamper envelope (geotag, device-time skew, OCR confidence, plausibility flags). PHOTO_OCR gated by feature flag. |
| GET | `/collection/readings` | role:`SAMITI_SACHEEV, MILK_TESTER, UNION_FIELD_SUPERVISOR` | List (scope-checked, paged). |
| POST | `/collection/pours` | role:`SAMITI_SACHEEV, MILK_TESTER` | Records a pour. Farmer must be an active member of the DCS. Prices via active chart (`rate = base + fat·fatRate + snf·snfRate`; e.g. `8.0 + 6.5·5.5 + 9.0·1.0 = ₹52.75/L`). **Idempotent** on `client_event_id` (replay → 200 with stored pour). Implausible values hard-rejected (`422`). Appends `pour.recorded`, publishes bus event, queues receipt SMS. |
| POST | `/collection/pours/batch-sync` | role:`SAMITI_SACHEEV, MILK_TESTER` | The offline reconnect path: up to 500 pours; per-item `created`/`duplicate`/`error`, never aborts the batch. |
| POST | `/collection/pours/{id}/supersede` | role:`SAMITI_SACHEEV, MILK_TESTER` | Append-only correction: old → `SUPERSEDED`, new pour references it. Blocked once consigned/invoiced. |
| GET | `/collection/pours` | role:`SAMITI_SACHEEV, MILK_TESTER, UNION_FIELD_SUPERVISOR, FARMER` | List (FARMER forced to own; others scope-checked). |
| POST | `/collection/invoices/generate` | role:`SAMITI_SACHEEV` | Groups a day's un-invoiced pours per farmer → one invoice each (unique farmer+dcs+date). Late pours merge (`invoice.amended`). |
| GET | `/collection/invoices` | role:`SAMITI_SACHEEV, SAMITI_ADHYAKSH, FARMER, …` | List (FARMER forced own; paged). |
| GET | `/collection/invoices/{id}` | role:(same) | One invoice. |

### 3.4 `logistics` — consignment → route trip → cold chain

| Method | Path | Auth | What it does |
|---|---|---|---|
| POST | `/logistics/consignments` | role:`SAMITI_SACHEEV` | Aggregates a DCS shift's pours into one consignment (**the pooling boundary** — §7.4). Unique dcs+date+shift. |
| POST | `/logistics/consignments/{consignmentID}/dispatch` | role:`SAMITI_SACHEEV` | `OPEN → DISPATCHED` (seals it; re-aggregates the shift's pours). |
| GET | `/logistics/consignments` | role (readers) | List (scope-checked). |
| POST | `/logistics/trips` | role:`UNION_FIELD_SUPERVISOR, VAN_RIDER` | Creates a van run over dispatched consignments. |
| POST | `/logistics/trips/{tripID}/stops/{consignmentID}/pickup` | role:`VAN_RIDER` (the trip's rider) | Records pickup + temperature; consignment → `PICKED_UP`. |
| POST | `/logistics/trips/{tripID}/cold-chain` | role:`VAN_RIDER` (trip's) | Appends a temp/geo sample (tamper-evidence for perishables). |
| POST | `/logistics/trips/{tripID}/deliver` | role:`VAN_RIDER` (trip's) | `→ DELIVERED` to a BMC; consignments → `DELIVERED`. |
| GET | `/logistics/trips` · `/logistics/trips/{tripID}` | role (readers) | List / detail (rider forced own). |

### 3.5 `plant` — BMC lots → batches → product lots → QR (safety-gate enforcement)

| Method | Path | Auth | What it does |
|---|---|---|---|
| GET/POST | `/plant/bmc-lots` | role:`BMC_OPERATOR` (+ readers on GET) | Pool delivered consignments into a lot. |
| POST | `/plant/bmc-lots/{id}/close` | role:`BMC_OPERATOR` | `OPEN → QC_PENDING` (+ chilling temp). |
| POST | `/plant/bmc-lots/{id}/dispatch` | role:`BMC_OPERATOR` | **Only a `PASSED` lot may dispatch** (else `422 SAFETY_GATE_BLOCKED`). |
| GET/POST | `/plant/batches` | role:`PLANT_OPERATOR` (+ readers) | Batch from **PASSED+DISPATCHED** lots; each lot atomically claimed `→ POOLED` (one batch only). |
| GET | `/plant/batches/{id}` | role (readers) | Batch detail + its QC results. |
| POST | `/plant/batches/{id}/complete` | role:`PLANT_OPERATOR` | Requires plant-lab `PASSED` → `COMPLETED`. |
| POST | `/plant/product-lots` | role:`PLANT_OPERATOR, PLANT_LAB_ANALYST` | Only from a `COMPLETED` batch. |
| POST | `/plant/product-lots/{id}/recall` | role:`PCDF_ADMIN, PLANT_LAB_ANALYST` | FSSAI recall path. |
| POST | `/plant/qrs` | role:`PLANT_OPERATOR, PLANT_LAB_ANALYST` | Issues an HMAC-signed QR (`qr_code` + signed token over `productLotID.Hex()`). |
| GET | `/plant/qrs` | role:(same) | List QRs. |

### 3.6 `quality` — QC recording + the FSSAI gate verdict (§8.3)

| Method | Path | Auth | What it does |
|---|---|---|---|
| GET | `/quality/limits` | session | The FSSAI reference limits (AFM1 ≤ 0.5 µg/kg, coliform ≤ 10 CFU/ml, …). |
| POST | `/quality/qc-results` | role:`BMC_OPERATOR` (BMC_RAPID), `PLANT_LAB_ANALYST` (PLANT_LAB) | Records tests, evaluates against FSSAI limits. **Evidence + ledger events persist first, then the subject flips** `PASSED`/`BLOCKED`. A block quarantines the subject forever + alerts the supervisor (bus). Unknown test names & missing mandatory tests are rejected. |
| GET | `/quality/qc-results` · `/quality/qc-results/{id}` | role (readers) | List / detail. |

### 3.7 `settlement` — same-day pay (dual-control) + DBT subsidy

| Method | Path | Auth | What it does |
|---|---|---|---|
| POST | `/settlements` | role:`SAMITI_SACHEEV` | **Initiates** a day's invoices → `PENDING_APPROVAL`. |
| POST | `/settlements/{id}/approve` | role:`SAMITI_ADHYAKSH, UNION_PRESIDENT` | **Approver must differ from initiator** (`403 DUAL_CONTROL_VIOLATION`). |
| POST | `/settlements/{id}/reject` | role:(same) | Releases invoices back to `ISSUED`. |
| POST | `/settlements/{id}/execute` | role:`SAMITI_SACHEEV, SAMITI_ADHYAKSH` | Mock licensed-PA payout (crash-safe lease); invoices → `PAID`; payout SMS. |
| GET | `/settlements` · `/settlements/{id}` | role (readers) | List / detail. |
| GET | `/settlements/payouts?farmer_party_id=` | role (readers); FARMER forced own | Payout history (scope-checked — no cross-farmer IDOR). |
| POST | `/dbt/requests` · `/dbt/requests/{id}/status` · GET `/dbt/requests` | role:`MISSION_OFFICIAL, PCDF_ADMIN` (FARMER lists own) | Subsidy tracked strictly on the PFMS/DBT rail (mock). |

### 3.8 `cattle` — animals, health, 1962 MVU, education, dormant collar

| Method | Path | Auth | What it does |
|---|---|---|---|
| POST | `/cattle/animals` | role:`FARMER, SAMITI_SACHEEV, LRP, AI_TECH, VETERINARIAN` | Register animal (unique `pashu_aadhaar`); FARMER only own. |
| GET | `/cattle/animals` · `/cattle/animals/{animalID}` | session / role | List / detail (scope-checked). |
| GET | `/cattle/animals/{animalID}/health-events` | role | Animal health history (scope-checked). |
| POST | `/cattle/animals/{animalID}/health-events` | role:`VETERINARIAN, AI_TECH` | Log a health event (Bharat Pashudhan sync PENDING). |
| POST | `/cattle/animals/{animalID}/bp-sync` | role:`VETERINARIAN, PCDF_ADMIN` | Mock push to Bharat Pashudhan. |
| POST/GET | `/cattle/mvu-cases` (+ `/{caseID}/dispatch`, `/{caseID}/close`) | role per action | 1962 MVU vet dispatch workflow. |
| GET/POST | `/cattle/education` | session (read) / `PCDF_ADMIN, MISSION_OFFICIAL` (write) | Vernacular education hub. |
| POST | `/cattle/telemetry` | role:`SERVICE_ACCOUNT` | **Dormant** — returns `403 FEATURE_DISABLED` until the `collar_telemetry_enabled` flag flips (§9). |

### 3.9 `publictrace` — consumer QR scan (PUBLIC) + official trace tools

| Method | Path | Auth | What it does |
|---|---|---|---|
| GET | `/public/qr/{qr_code}` | **public** | Verifies the signed QR, returns the **honest samiti-SET provenance** (never per-farmer) + QC certificate + `ledger.intact`. Recalled lots show a recall notice (200, not 404). |
| GET | `/public/ledger/verify?from=&to=` | **public** | Re-verifies a hash-chain segment (tamper-evidence). |
| GET | `/trace/{entity_type}/{entity_id}` | role:`MISSION_OFFICIAL, PCDF_ADMIN, UNION_PRESIDENT, UNION_FIELD_SUPERVISOR, PLANT_LAB_ANALYST, STATE_AUDITOR` | Full upstream + downstream provenance graph (the recall/root-cause tool). |
| GET | `/trace/{entity_type}/{entity_id}/timeline` | role:(same) | One entity's event timeline. |

### 3.10 `platformops` — admin, audit, notifications, support

| Method | Path | Auth | What it does |
|---|---|---|---|
| GET | `/admin/flags` | role:`SUPER_ADMIN, PCDF_ADMIN` | List feature flags. |
| PUT | `/admin/flags/{key}` | role:`SUPER_ADMIN` | Flip a flag (closed key-set; audited). Capability gating (§6). |
| GET | `/audit/logs` · `/audit/logs/export` | role:`STATE_AUDITOR, SUPER_ADMIN` | Immutable audit trail (read-only) + JSON export (§12). |
| GET | `/notifications` | role:`SUPER_ADMIN` | Outbox view (audited; OTP text redacted). |
| POST | `/notifications/worker/run` | role:`SUPER_ADMIN` | Drains the SMS outbox (mock provider). |
| GET | `/support/parties/lookup?phone=` | role:`SUPPORT_AGENT, SUPER_ADMIN` | Limited PII lookup; **every call audited**. |

### Cross-cutting endpoint
`GET /api/v1/events/stream` — SSE live event stream (any authenticated party); see §1.9.

### Operational endpoints (no `/api/v1`, no auth)
`GET /healthz` · `GET /readyz` (Mongo ping) · `GET /metrics` (Prometheus) · `GET /version`.

---

## 4. How to exercise it

```bash
cp .env.example .env      # set MONGO_URI (env-only; server refuses to boot without it)
make seed                 # org tree + one party per role (prints their ObjectIDs)
make run                  # API on :8080
make smoke                # the full 21-step E2E (login → pour → gate → QR → settlement → KYC approval)
```
Seeded demo phones (OTP returned in the response in dev mode): super-admin `9999999999`,
sacheev `9000000001`, adhyaksh `9000000002`, farmer `9000000011`, van-rider `9000000021`,
bmc-op `9000000031`, plant-op `9000000041`, lab `9000000042`, **org-manager `9000000071`**.

---

## 5. Error codes the frontend should handle

| Code | HTTP | Meaning |
|---|---|---|
| `UNAUTHORIZED` / `FORBIDDEN` | 401 / 403 | No/invalid token · role or scope denied |
| `KYC_TIER_INSUFFICIENT` | 403 | Role selected before KYC approved to the required tier |
| `KYC_NOT_PENDING` / `KYC_SELF_REVIEW` / `KYC_TIER_NOT_APPROVABLE` | 409 / 403 | KYC approval guardrails |
| `SAFETY_GATE_BLOCKED` | 422 | A blocked (failed-QC) lot/batch cannot advance |
| `DUAL_CONTROL_VIOLATION` | 403 | Settlement approver == initiator |
| `IMPLAUSIBLE_VALUES` | 422 | Pour fat/SNF/qty outside physical bounds |
| `FARMER_NOT_MEMBER` | 422 | Pour for a farmer not assigned to that DCS |
| `INVALID_ID` | 400 | Malformed ObjectID |
| `FEATURE_DISABLED` | 403 | Capability-gated (dormant) endpoint |
| `RATE_LIMITED` | 429 | Per-IP throttle |
| `NOT_FOUND` / `CONFLICT` / `MALFORMED_JSON` | 404 / 409 / 400 | Standard |

---

## 6. What is mocked vs real (so you know what to wire next)

**Real:** auth, RBAC, org scoping, KYC approval workflow, pricing, idempotent ingest, the safety
gate, hash-chained ledger + trace, settlement state machine + dual control, audit, flags, metrics,
structured logging.

**Mocked (single named seams — swap without touching the flow):** Aadhaar eKYC, bank penny-drop,
Bharat Pashudhan sync, PFMS/DBT, licensed-PA payout execution, SMS provider.

**Dormant (flag-gated OFF):** collar telemetry, ONDC, consumer commerce.

See [`TECHNICAL_NOTES.md`](TECHNICAL_NOTES.md) for architecture/scaling and the ordered next-steps
list; [`WORKFLOW_GUIDE.md`](WORKFLOW_GUIDE.md) for role-by-role flows with diagrams; and
[`PCDF_Cooperative_Constitution.md`](PCDF_Cooperative_Constitution.md) for *why* the org/role model
is shaped this way (the UP cooperative structure it is grounded in).
