# Saathi — Frontend Integration Brief

> **Audience:** the `parag-saathi-fe` (Expo/React Native) team wiring the app to the Go backend
> (`parag-saathi-be`). This is the **complete, authoritative** contract: connect, auth, every
> endpoint, the wire conventions, the real-time stream, the error codes, and the exact FE-side
> reconciliations to make before flipping `MODE=http`.
>
> The backend is the **canonical source of truth** for field names, enums and status codes. Where
> the app's mock used different names, the fix is in the FE mapper (`mapRequest`/`mapResponse`),
> never the backend. Section 9 lists every one of those.

---

## 1. Connect

```bash
# parag-saathi-fe/.env
EXPO_PUBLIC_API_MODE=http
EXPO_PUBLIC_API_BASE_URL=http://10.0.2.2:8080/api/v1   # Android emulator → host
# real device on same LAN: http://<mac-lan-ip>:8080/api/v1
```

Bring the backend up and seed it:

```bash
cd ~/parag-saathi-be
make run          # API on :8080   (or: make dev  → hot-reload)
make seed         # demo org tree + one party per role + verified banks + rate chart
```

Health: `GET /healthz` · readiness (Mongo ping): `GET /readyz` · `GET /version`.

**Seeded test phones** (OTP is returned in the response in dev mode — `dev_otp`):
super-admin `9999999999`, **Ramesh `9000000001` = SAMITI_SACHEEV + FARMER** (multi-role → role
picker), adhyaksh `9000000002`, farmer `9000000011`/`9000000012`, van-rider `9000000021`,
bmc-op `9000000031`, plant-op `9000000041`, lab `9000000042`, org-manager `9000000071`.

---

## 2. Wire conventions (every request)

| Concern | Contract |
|---|---|
| Base | `EXPO_PUBLIC_API_BASE_URL` already includes `/api/v1` |
| Success | `{ "data": <payload>, "meta"?: { "limit", "offset", "total" } }` — unwrap `.data` |
| Error | `{ "error": { "code": "UPPER_SNAKE", "message": "...", "details"?: {...} } }` |
| Auth | `Authorization: Bearer <access_token>` |
| 401 handling | one silent `POST /auth/refresh` + retry, then sign out (never refresh on `/auth/*`) |
| Field case | **snake_case on the wire** — map to camelCase per endpoint |
| IDs | Mongo **ObjectID** = 24-hex string; opaque to the client. **Never send `""` for an ID field** — omit it or the server 400s (`INVALID_ID`) |
| Dates | `YYYY-MM-DD` (IST) for day keys (`pour_date`, `invoice_date`, `date`); ISO-8601 for timestamps |
| Idempotency | pours carry `client_event_id` (device-minted, unique) — reuse the SAME id on a retry so offline replays dedupe |
| Pagination | `?limit=` (default 50, max 200) `&offset=`; response `meta.total` |

---

## 3. Auth & the multi-role flow (one phone = many roles)

It is a **two-call login**, then a **role selection** that pins a scope:

```
1. POST /auth/otp/request  {phone}                 → {phone, expires_at, dev_otp?}
2. POST /auth/otp/verify   {phone, otp}            → {access_token (SESSION), refresh_token, party}
3. GET  /parties/me        (Bearer SESSION)         → {party, assignments[], kyc?}
      • assignments.length === 1 → auto-select it
      • else                     → show the role picker
4. POST /auth/role/select  {role_assignment_id}    → {access_token (ROLE), role_code, org_unit_id, org_type}
      • swap ONLY the access token to this ROLE token; keep the refresh token
5. render the dashboard for role_code; every business call now uses the ROLE token
```

Each item of `assignments[]` is a flattened `RoleAssignment` **plus** `org_name`, `org_type`,
`org_code` (org enrichment) — enough to render the picker with no extra call. Switching roles =
call `/auth/role/select` again with the other `id`. Losing a role = one revoked assignment, never
an account deletion.

**Multi-role is real and works:** Ramesh (`9000000001`) returns both `FARMER` and `SAMITI_SACHEEV`
assignments; select either.

**KYC gate:** `role/select` returns `403 KYC_TIER_INSUFFICIENT` (with `details.current_tier` /
`required_tier`) if the party's KYC tier is below the role's requirement. Surface this — the user
must complete KYC (or be approved) before entering that role.

---

## 4. Endpoint reference (by module, all under `/api/v1`)

Legend — **public** = no token · **session** = any logged-in party · **role:X** = a ROLE token
with one of those roles (SUPER_ADMIN always allowed).

### identity — `/auth`, `/parties`, `/kyc`, `/roles`
| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/auth/otp/request` | public | send OTP |
| POST | `/auth/otp/verify` | public | verify → SESSION token + party |
| POST | `/auth/refresh` | public | rotate token pair |
| POST | `/auth/logout` | session | revoke refresh token |
| GET | `/auth/roles` | session | my usable assignments (org-enriched) |
| POST | `/auth/role/select` | session | pick a role → ROLE token |
| GET/PATCH | `/parties/me` | session | my profile (PATCH: `full_name`, `preferred_language`, `public_consent`) |
| GET | `/parties?role=CODE&org_unit_id=?` | role: reviewers | list parties holding a role (org_unit_id optional — defaults to caller's scope) |
| POST | `/kyc/aadhaar` | session | submit Aadhaar KYC → **PENDING** (no auto-verify) |
| POST | `/kyc/bank` | session | penny-drop; masked tail persisted |
| GET | `/kyc/me` | session | my KYC records (masked) |
| GET | `/kyc/pending` · `/kyc/pending/count` | role: reviewers | review queue + live badge count |
| POST | `/kyc/{id}/approve` · `/kyc/{id}/reject` | role: reviewers | verify → tier upgraded |
| POST | `/roles/assignments` | role: granters | grant a role (assign a position) |
| DELETE | `/roles/assignments/{id}` | role: granters | revoke |
| GET | `/roles/assignments?org_unit_id=&role_code=` | role: granters | list grants |

### orgs — `/orgs`
`GET /orgs?type=&district=&code=` · `POST /orgs` · `GET|PATCH /orgs/{id}` ·
`GET /orgs/{id}/children` · `GET /orgs/{id}/tree` · `GET /orgs/{id}/members`.

### collection (core loop) — `/collection`
| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/collection/rate-charts` | role:PCDF_ADMIN,UNION_PRESIDENT | create chart (carries `version`) |
| GET | `/collection/rate-charts/active?dcs_id=` | session | active chart for a DCS |
| GET/POST | `/collection/readings` | role:SACHEEV,MILK_TESTER (+auditor GET) | analyzer reading (anti-tamper; `photo_object_key` evidence) |
| POST | `/collection/pours` | role:SACHEEV,MILK_TESTER | record pour → returns `{pour, idempotent_replay?}`; pour has `assurance` A\|B\|C + `rate_chart_version` |
| POST | `/collection/pours/batch-sync` | role:SACHEEV,MILK_TESTER | offline replay (≤500) |
| POST | `/collection/pours/{id}/supersede` | role:SACHEEV,MILK_TESTER | append-only correction |
| GET | `/collection/pours?dcs_id=&date=&shift=&farmer_party_id=` | role:… (FARMER forced own) | list |
| POST | `/collection/invoices/generate` | role:SACHEEV | issue same-day invoices |
| GET | `/collection/invoices?dcs_id=&date=&farmer_party_id=&status=` · `/collection/invoices/{id}` | role:… (FARMER own) | list/detail |

### logistics — `/logistics`
`GET|POST /logistics/consignments` · `POST /logistics/consignments/{id}/dispatch` (mints
`seal_code` + `sealed_at` + weakest `assurance`) · `GET|POST /logistics/trips` ·
`GET /logistics/trips/{tripID}` · `POST /logistics/trips/{tripID}/stops/{consignmentID}/pickup`
(body `{temp_c, notes?}`) · `POST /logistics/trips/{tripID}/cold-chain` ·
`POST /logistics/trips/{tripID}/deliver` (body `{bmc_id}`).

### plant — `/plant`
`GET|POST /plant/bmc-lots` (+ `/{id}/close`, `/{id}/dispatch`) ·
`GET|POST /plant/batches` (+ `GET /{id}`, `/{id}/complete`) — batch carries `contributing_dcs_ids` ·
`POST /plant/product-lots` (+ `/{id}/recall`) · `GET|POST /plant/qrs`.
The safety gate is enforced: dispatch/batch/product/QR all refuse a `BLOCKED` subject
(`422 SAFETY_GATE_BLOCKED`).

### quality (safety gate) — `/quality`
`GET /quality/limits` · `GET /quality/qc-queue` (BMC lots + batches pending QC, with
mandatory-vs-recorded params) · `GET|POST /quality/qc-results` · `GET /quality/qc-results/{id}` ·
`POST /quality/batches/{id}/certificate` (issue certificate; requires PASSED) ·
`GET /quality/batches/{id}/trace-back` (failed batch → contributing societies).
**Send canonical test names** — `AFLATOXIN_M1` not `AFM1` (see §9). AFM1 ≤ 0.5 µg/kg, coliform ≤ 10 CFU/ml.

### settlement — `/settlements`, `/dbt`
`POST /settlements` (initiate) · `POST /settlements/{id}/approve` (dual-control: approver ≠
initiator) · `POST /settlements/{id}/reject` · `POST /settlements/{id}/execute` ·
`GET /settlements?dcs_id=&date=&status=` · `GET /settlements/{id}` ·
`GET /settlements/payouts?farmer_party_id=` (FARMER own).
DBT: `GET|POST /dbt/requests` · `POST /dbt/requests/{id}/status`.
**Note:** a farmer with no VERIFIED bank → payout `FAILED`, invoice stays unpaid, batch ends `PARTIAL`.

### cattle & 1962 — `/cattle`
`GET|POST /cattle/animals` · `GET /cattle/animals/{id}` · `GET|POST /cattle/animals/{id}/health-events`
· `POST /cattle/animals/{id}/bp-sync` · `GET|POST /cattle/mvu-cases` (list accepts
`?dcs_id=&farmer_party_id=&status=`; FARMER forced own) · `POST /cattle/mvu-cases/{id}/dispatch`
· `POST /cattle/mvu-cases/{id}/close` · `GET|POST /cattle/education` · `POST /cattle/telemetry`
(dormant → `403 FEATURE_DISABLED`).

### public trace — `/public`, `/trace`
`GET /public/qr/{qr_code}` (**no auth** — product, batch, plant, **societies with `volume_share`**,
`farmers_total`, consented `farmers_public` roster, quality cert, `ledger.intact`, `recalled?`) ·
`GET /public/ledger/verify?from=&to=` · `GET /trace/{entity_type}/{entity_id}` (+ `/timeline`, role: officials).

### dashboards — `/dashboards`
`GET /dashboards/farmer/{partyID}` (today/month/pending + 12-day trend; FARMER forced self) ·
`GET /dashboards/society/{dcsID}` (today/month, active farmers, member count; org-scoped).

### admin & platform — `/admin`, `/audit`, `/notifications`, `/support`
`GET /admin/flags` · `PUT /admin/flags/{key}` · `GET /admin/stats` (control-tower counts) ·
`GET /admin/products` · `PUT /admin/products` (product master, upsert by sku) ·
`GET /audit/logs` · `GET /audit/logs/export` · `GET /notifications?phone=&status=` ·
`POST /notifications/worker/run` · `GET /support/parties/lookup?phone=`.

### onboarding (assisted) — `/onboarding`
`POST /onboarding/requests` (executive submits phone+role+org+tier) ·
`GET /onboarding/requests?status=&submitted_by=` ·
`POST /onboarding/requests/{id}/approve` (→ creates Party + VERIFIED KYC + RoleAssignment) ·
`POST /onboarding/requests/{id}/reject`.

### CMS content — `/content`
`GET /content?type=&since=<version>&scope=` (**versioned delta pull** — pass the last `version`
you saw; response `meta` carries the new max) · `GET /content/helpline?scope=` (region helpline
numbers) · `POST /content` · `PUT /content/{id}` (role: PCDF_ADMIN/MISSION_OFFICIAL).

### real-time — `/events/stream`
`GET /events/stream` (SSE, Bearer). Stream via XHR. Events: `ready`, then `kyc.pending.changed`
(reviewers) and others — on receipt, **re-fetch the relevant scoped count/list** (nudge, then
re-query; never trust the event as the value). Heartbeat comment every ~25s.

---

## 5. Onboarding → verification → position (the full relation)

```
new user logs in (OTP)                → Party, tier MINIMAL, no roles → "pending" screen
  ↓ POST /kyc/aadhaar                  → KYCRecord PENDING  +  KYC_PENDING notification queued to reviewers/super-admin  +  live SSE badge
  ↓ reviewer POST /kyc/{id}/approve    → tier upgraded, record VERIFIED, party notified
  ↓ admin POST /roles/assignments      → the POSITION (role_code @ org) — a RoleAssignment
  ↓ user re-login → GET /auth/roles    → sees the role → /auth/role/select → dashboard
```
The **assisted** variant (`/onboarding/requests` approve) does all of party+KYC+role atomically for
an executive-submitted request.

---

## 6. Error codes to handle

`UNAUTHORIZED` (401) · `FORBIDDEN` (403) · `KYC_TIER_INSUFFICIENT` (403, has `details`) ·
`SAFETY_GATE_BLOCKED` (422) · `DUAL_CONTROL_VIOLATION` (403) · `IMPLAUSIBLE_VALUES` (422) ·
`FARMER_NOT_MEMBER` (422) · `WRONG_DELIVERY_BMC` / `CONSIGNMENT_NOT_DELIVERED` (422) ·
`SHIFT_CONSIGNED` / `POUR_CONSIGNED` (409) · `KYC_NOT_PENDING` / `KYC_SELF_REVIEW` (409/403) ·
`FEATURE_DISABLED` (403) · `INVALID_ID` (400) · `RATE_LIMITED` (429) ·
`NOT_FOUND` / `CONFLICT` / `MALFORMED_JSON`.

---

## 7. Real-time & offline
- **SSE** for live badges (see §4). Multi-replica prod fans out via Redis (transparent to the app).
- **Offline**: pours are idempotent on `client_event_id` — queue locally, replay via
  `/collection/pours/batch-sync`; a replay returns the stored pour with `idempotent_replay: true`.

---

## 8. What's mocked server-side (so you can plan real flows)
Aadhaar eKYC, bank penny-drop, PFMS/DBT, Bharat Pashudhan sync, licensed-PA payout, SMS provider
are single named mock seams — the wire shapes are final; only the provider swaps later.
On-device OCR is the design (§8.2): the app OCRs the analyzer photo, uploads it (gets a key), and
posts a reading with `mode=PHOTO_OCR` + `photo_object_key` + `ocr_confidence`. There is **no**
server `/pours/ocr-extract` — remove that mock and wire on-device OCR → `POST /collection/readings`.

---

## 9. FE-side reconciliation checklist (do these in the mappers before `MODE=http`)

The backend is canonical; adjust the FE `mapRequest`/`mapResponse` to these names/enums.

**Safety-critical**
- QC test name: send **`AFLATOXIN_M1`** (not `AFM1`). Subject type: **`PROCESSING_BATCH`** (not `PLANT_BATCH`), **`BMC_LOT`** (not `BMC_INTAKE`). *(The backend now also normalizes `AFM1`, but send canonical.)*

**Pour**
- Read weighted rate from **`rate_per_litre`**; source enum is **`ANALYZER_DIRECT`** (not `ANALYZER_AUTO`), `PHOTO_OCR`, `MANUAL`. New readable fields: `assurance` (A|B|C), `rate_chart_version`. Backend pour status is only `RECORDED|SUPERSEDED|CANCELLED` (no IN_LOT/INVOICED/PAID — derive lifecycle client-side).

**Consignment / lot**
- `total_quantity_litres` (not `total_litres`); `avg_fat_pct`/`avg_snf_pct` are the weighted averages; new: `seal_code`, `sealed_at`, `assurance`. Status enum: `OPEN|DISPATCHED|PICKED_UP|DELIVERED|ACCEPTED|REJECTED`.

**Invoice**
- `invoice_date` (not `date`); amount is `total_amount` + `total_quantity_litres` (no gross/deductions/net on the invoice — settlement lives on the settlement batch). Status: `ISSUED|SETTLEMENT_PENDING|PAID|HOLD`.

**Batch / QC**
- Batch: `input_litres` (not `volume_litres`); QC verdict from batch `status` (`PASSED`/`BLOCKED`) + `QCResult.overall_pass`; new `contributing_dcs_ids`. QC test: `pass` (not `passed`), result `overall_pass`, `analyst_party_id` (not `recorded_by_party_id`).

**BMC lot**
- Status enum `OPEN|QC_PENDING|PASSED|BLOCKED|DISPATCHED|POOLED`; trips are `route_trip_ids` (array). Trip delivered field: `delivered_to_bmc_id`.

**Org / roles**
- Org types: `FEDERATION|MILK_UNION|PROCESSING_PLANT|BMC|DCS` (map `UNION`→`MILK_UNION`, `PLANT`→`PROCESSING_PLANT`). Add `ORGANISING_MANAGER` and `CONSUMER` to the FE role map and **guard `ROLES[code]`** against unknown codes so the picker never crashes. `ONBOARDING_EXECUTIVE` and `STORE_MANAGER` are valid backend roles. KYC tier: `SERVICE` (backend) ↔ `MTLS` (both accepted).

**Cattle / MVU**
- MVU list: read `requested_at` (not `created_at`); pass `farmer_party_id` for a farmer's own list. Health event: `recorded_by_role`, sync via `bp_sync_status` (`SYNCED`); enums `ARTIFICIAL_INSEMINATION`/`DISEASE_REPORT`.

**Logistics pickup**
- Path is keyed by the **consignment id**: `POST /logistics/trips/{tripID}/stops/{consignmentID}/pickup` — send the consignment id in the `{consignmentID}` slot (not the DCS id), and include `temp_c`.

**Never send empty-string IDs** (e.g. `org_unit_id: ""`) — omit the field.

---

## 10. Verify your integration
The backend ships two harnesses that exercise the full contract end-to-end — run them to see the
exact request/response shapes your mappers must match:
```bash
make smoke     # 23-step pour→QR→settlement + KYC-approval + concurrency loop
bash scripts/dryrun.sh   # multi-role login, KYC→notification→approve→assignment, dashboards, assurance/seal
```
Both must print `PASSED`. Companion docs: `strict_brief.md` (per-endpoint contract),
`BACKEND_FLOW.md` (diagrams), `ALIGNMENT_SPEC.md` (the reconciliation gaps + status),
`WORKFLOW_GUIDE.md` (role flows).
