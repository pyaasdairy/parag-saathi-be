# Saathi — Frontend Integration Status & How-To (per surface)

> **Audience:** the `parag-saathi-fe` (Expo/React Native) developer wiring the app to
> `parag-saathi-be`. This is the **status-oriented** companion to
> [`FRONTEND_INTEGRATION.md`](FRONTEND_INTEGRATION.md) (the full endpoint contract) and
> [`strict_brief.md`](strict_brief.md) (per-endpoint behaviour).
>
> It answers two questions for **every** surface: **(1) is it integrated / integrable now,
> or does something still block it?** and **(2) exactly how do I wire it?** Each item is
> marked with a status and, where blocked, says who fixes it (FE mapper vs backend change)
> and the interim behaviour.
>
> Every backend fact below was **verified against the actual Go source** (routes, DTOs,
> enums) during the integration pass — not assumed. Where the earlier `backend-gaps.md`
> was inaccurate, the correction is called out inline (⚠️).

---

## ✅ UPDATE — every 🔴 backend gap below is now BUILT

The backend has since implemented **all** the 🔴 backend-gap items in this doc, verified green
(build · vet · gofmt · 11 test pkgs · `make smoke` 23/23 · `dryrun.sh` · a 6-point live
spot-check). What shipped:

- **Products readable by session** — `GET /products` (active catalogue, with `name_hi`+`category`); seeded with 3 SKUs.
- **Product-lot mint** accepts `product_id` → derives sku/name/unit_size/mrp and expiry (from product `shelf_life_days`).
- **`GET /parties?role=`** now returns `{party_id, full_name, org_unit_id, org_name, org_code, village, role_assignment_id}`.
- **Onboarding** stores the rich capture (village, cattle, vehicle, employee_id, document, photo URLs, full_name_hi).
- **QC queue** carries `input_litres`; **trace-back** carries per-society `volume_litres`/`volume_share`/`pour_count`/`collected_on` + `name_hi`/`village` + batch `input_litres`.
- **Society dashboard**: `avg_fat_pct`, `avg_snf_pct`, `quality_failures_30d`, `open_consignment` (now populated), `trend[]`; **farmer** `animal_count`.
- **`GET /logistics/consignments/{id}`**; **DCS→Union B2B invoice**: `POST …/approve-union` + `GET …/invoice` (HSN 0401, GST-exempt).
- **`GET`/`PUT /admin/sachiv-cap`** `{cap, appointed}`; **DBT** allows FARMER-self + `amount: 0` + returns `scheme_name`/`scheme_name_hi`.
- **Org** `name_hi` + `village` on create/patch; **SSE** now broadcasts `settlement.changed` / `quality.changed` / `pour.recorded`.

**The remaining work is FE-side wiring** (mappers, path fixes, the settlement-batch-id rewire) —
the step-by-step recipe per endpoint is in the FE repo at
`parag-saathi-fe/docs/BACKEND_WIRING_GUIDE.md`. The 🔴 markers below now read as "was a gap → fixed;
here is the shape to map to."

---

## 0. Status legend

| Badge | Meaning | Who acts |
|---|---|---|
| ✅ **LIVE** | Route + fields exist; flip `mockOnly:false` and add the mapper | FE (mapper only) |
| 🟡 **FE-ONLY** | Backend is ready; FE must fix a path / enum / body / status-vocab | FE |
| 🔴 **BACKEND-GAP** | FE needs a route/field/behaviour the backend does **not** provide | Backend (FE keeps `mockOnly` until then) |
| ⚪ **BY-DESIGN** | Not a gap; the design intentionally lives elsewhere (e.g. on-device) | FE |

**FE convention reminder:** endpoints not yet reconciled carry `mockOnly: true` in their
`endpoint({...})` spec (`src/core/api/client.ts`) so they run the in-app mock even in
`http` mode. Grep `mockOnly: true` to find every one; flip it off once its row below says ✅/🟡 done.

---

## 1. Quick-start (connect + flip to http)

```bash
# backend
cd ~/parag-saathi-be && make run   # API on :8080  (make dev = hot reload)
make seed                          # org tree + one party per role + verified banks + rate chart

# frontend  (parag-saathi-fe/.env)
EXPO_PUBLIC_API_MODE=http
EXPO_PUBLIC_API_BASE_URL=http://10.0.2.2:8080/api/v1   # Android emulator → host
# real device on LAN: http://<mac-lan-ip>:8080/api/v1
```

**Wire conventions** (all requests): success `{data, meta?}` → unwrap `.data`; error
`{error:{code,message,details?}}`; `Authorization: Bearer <token>`; **snake_case on the
wire**; ObjectID = 24-hex string, **never send `""` for an ID** (omit it or you get
`400 INVALID_ID`); day keys `YYYY-MM-DD`, timestamps ISO-8601; pours carry a device-minted
`client_event_id` (reuse on retry). One silent `POST /auth/refresh` on 401, then sign out.

**Seed phones** (dev returns `dev_otp` in the response): super-admin `9999999999`,
**Ramesh `9000000001` = SAMITI_SACHEEV + FARMER** (multi-role → picker), adhyaksh `9000000002`,
farmer `9000000011`/`9000000012`, van-rider `9000000021`, bmc-op `9000000031`,
plant-op `9000000041`, lab `9000000042`, org-manager `9000000071`.

---

## 2. Surface-by-surface integration guide

### 2.1 Auth & role switch — ✅ LIVE (already the app's spine)
Two-call login then a role selection that pins scope:
```
POST /auth/otp/request {phone}              → {phone, expires_at, dev_otp?}
POST /auth/otp/verify  {phone, otp}         → {access_token (SESSION), refresh_token, party}
GET  /parties/me                            → {party, assignments[], kyc?}
      assignments.length===1 → auto-select ;  else → role picker
POST /auth/role/select {role_assignment_id} → {access_token (ROLE), role_code, org_unit_id, org_type}
```
- Swap **only** the access token to the ROLE token; keep the refresh token.
- Each `assignments[]` item is a flattened `RoleAssignment` **plus** `org_name`, `org_type`,
  `org_code` — enough to render the picker with no extra call. (This enrichment exists here
  and on `/parties/me` — **not** on `/roles/assignments`; see 2.3.)
- KYC gate: `role/select` → `403 KYC_TIER_INSUFFICIENT` (with `details.current_tier`/
  `required_tier`) if tier is below the role's requirement. Surface it.
- **FE map:** add `ORGANISING_MANAGER`, `CONSUMER`, `ONBOARDING_EXECUTIVE`, `STORE_MANAGER`
  to `ROLES[code]` and **guard the lookup** so an unknown code never crashes the picker.

### 2.2 Profile & KYC (self) — ✅ LIVE
`GET/PATCH /parties/me` (PATCH: `full_name`, `preferred_language`, `public_consent`) ·
`POST /kyc/aadhaar` (→ PENDING, stores last-4 + vault ref only) · `POST /kyc/bank`
(penny-drop, masked tail) · `GET /kyc/me`. KYC tier accepts `SERVICE`↔`MTLS` (both valid).

### 2.3 Party/role lookup lists — 🔴 BACKEND-GAP (org enrichment)
- `GET /parties?role=CODE&org_unit_id=?` → returns **raw `domain.Party`** (`id, phone,
  full_name, preferred_language, kyc_tier, status, public_consent, …`) — **no org info.**
- ⚠️ **Correction to `backend-gaps.md` A1d:** it suggested pointing the Sachiv picker at
  `GET /roles/assignments?role_code=` because "`assignmentWithOrg` carries both". **It does
  not** — that list returns raw `RoleAssignment` (has `org_unit_id`, **no `org_name`**). The
  enriched shape is used only by `/auth/roles` and `/parties/me`.
- **Needed for the Sachiv picker** (`listSachivs`): `org_unit_id` **and** `org_name`.
- **Backend fix (pick one):** (a) enrich `GET /parties?role=` response with each holder's
  `{org_unit_id, org_name}`, or (b) add `org_name` to the `GET /roles/assignments` list
  response. Either unblocks the picker.
- **Interim FE:** keep `listSachivs` on mock, or show `org_unit_id` only.

### 2.4 Assisted onboarding — 🔴 BACKEND-GAP (rich capture)
- Backend `onboarding_requests` stores exactly `{phone, full_name, requested_role,
  org_unit_id, requested_tier, note, document_refs[]}` (+ server lifecycle fields). Approve
  atomically creates **Party + VERIFIED KYC + RoleAssignment** (+ tier upgrade + SMS) — this
  half is ✅ correct and powerful.
- The FE reviewer card also captures `village, cattle_count, cattle_breed, vehicle_number,
  employee_id, profile_photo, kyc_photo, document_type, document_number, full_name_hi` — none
  are stored, so they'd blank in the queue on http.
- **Backend fix (pick one):** (a) extend `onboarding_requests` + the submit DTO with those
  structured fields (keeps the reviewer UI rich — **preferred**), or (b) accept that the FE
  folds them into `note` (string) + uploads photos to S3 and passes URLs as `document_refs[]`.
- **Interim FE:** the 6 onboarding endpoints stay `mockOnly`. When (a) or (b) lands: drop
  `mockOnly`; add `mapRequest` (`fullName→full_name`, `role→requested_role`,
  `orgUnitId→org_unit_id`, derive `requested_tier ∈ {MINIMAL,FARMER,STANDARD,RIDER,HIGH}`) and
  a shared `mapResponse`.

### 2.5 Orgs — ✅ LIVE (one enrichment gap)
`GET /orgs?type=&district=&code=` · `POST /orgs` · `GET|PATCH /orgs/{id}` ·
`/orgs/{id}/children` · `/orgs/{id}/tree` · `/orgs/{id}/members`.
- **FE map:** org types `FEDERATION|MILK_UNION|PROCESSING_PLANT|BMC|DCS` — map FE
  `UNION→MILK_UNION`, `PLANT→PROCESSING_PLANT`.
- 🔴 **`name_hi` gap (B3):** `OrgUnit` has only `name` (no bilingual field). FE keeps a
  tolerant read (`name_hi` stays `undefined`). Backend fix: add `name_hi` if wanted.

### 2.6 Collection — core loop — ✅ LIVE (mapper renames)
| Call | Endpoint | Notes |
|---|---|---|
| rate chart | `POST /collection/rate-charts` / `GET …/active?dcs_id=` | chart carries `version` |
| reading | `POST /collection/readings` | anti-tamper envelope; `mode` ∈ `ANALYZER_DIRECT\|PHOTO_OCR\|MANUAL`; `photo_object_key`, `ocr_confidence` |
| pour | `POST /collection/pours` | returns `{pour, idempotent_replay?}`; pour has `assurance` A\|B\|C + `rate_chart_version`; read weighted rate from **`rate_per_litre`** |
| offline replay | `POST /collection/pours/batch-sync` | ≤500; per-item created/duplicate/error |
| correction | `POST /collection/pours/{id}/supersede` | append-only |
| list pours | `GET /collection/pours?dcs_id=&date=&shift=&farmer_party_id=` | FARMER forced own |
| invoices | `POST /collection/invoices/generate` · `GET /collection/invoices?…` · `/{id}` | **same-day** invoice |

- **FE map (safety-critical):** pour `source` = **`ANALYZER_DIRECT`** (not `ANALYZER_AUTO`).
  Pour status is only `RECORDED|SUPERSEDED|CANCELLED` — derive IN_LOT/INVOICED/PAID client-side.
- **Invoice** (⚠️ a real model difference, not a rename): it is **same-day** and carries
  `invoice_date` (not `date`), `total_amount`, `total_quantity_litres` — **no
  gross/deductions/net on the invoice**. Deductions & payment live on the **settlement batch**
  (see 2.11). Status: `ISSUED|SETTLEMENT_PENDING|PAID|HOLD` (**not** DRAFT/APPROVED/SETTLED).

### 2.7 Collection — OCR extract — ⚪ BY-DESIGN
No server `POST /collection/pours/ocr-extract`. Design: on-device OCR → upload photo (S3 seam)
→ `POST /collection/readings` with `mode=PHOTO_OCR` + `photo_object_key` + `ocr_confidence`.
Keep `photoOcrExtract` mock until on-device OCR + the photo upload seam are wired. **Not a
backend change.**

### 2.8 Collection — DCS→Union B2B GST invoice — 🔴 BACKEND-GAP (out of scope today)
No backend route/entity for a DCS→Union B2B e-invoice (HSN 0401, GST-exempt). `approveForUnion`
/ `getConsignmentInvoice` stay `mockOnly`. **Backend fix (only if this leg is in scope):**
model the B2B consignment invoice + `POST /collection/consignments/{id}/approve-union` +
`GET …/invoice`. Confirm with product whether the pilot needs it.

### 2.9 Logistics — ✅ LIVE (one path fix)
`GET|POST /logistics/consignments` · `POST /logistics/consignments/{id}/dispatch` (mints
`seal_code`, `sealed_at`, weakest `assurance`) · `GET|POST /logistics/trips` ·
`GET /logistics/trips/{tripID}` · `POST /logistics/trips/{tripID}/stops/{consignmentID}/pickup`
· `…/cold-chain` · `…/deliver {bmc_id}`.
- 🟡 **Pickup path fix:** it is keyed by the **consignment id** —
  `…/stops/{consignmentID}/pickup` — send the consignment id in that slot (not the DCS id),
  body `{temp_c, notes?}`.
- **FE map:** consignment `total_quantity_litres` (not `total_litres`); `avg_fat_pct`/
  `avg_snf_pct`; status `OPEN|DISPATCHED|PICKED_UP|DELIVERED|ACCEPTED|REJECTED` (backend
  `DISPATCHED` = "sealed"); trip delivered field `delivered_to_bmc_id`.
- 🔴 **Single-consignment GET (A7):** there is **no** `GET /logistics/consignments/{id}` —
  only the list. FE either sources one from the list result (current mock behaviour) or the
  backend adds the detail route.

### 2.10 BMC branch console — 🟡 FE-ONLY (status-vocab mapper)
Routes exist and bodies align: `GET /plant/bmc-lots`, `POST /plant/bmc-lots/{id}/close`
(body `chilling_temp_c`), `…/dispatch`.
- The backend `BMCLot.status` is `OPEN|QC_PENDING|PASSED|BLOCKED|DISPATCHED|POOLED`; `close`
  does **OPEN→QC_PENDING while recording `chilling_temp_c`** (not `RECEIVED→CHILLED`).
- **FE fix:** add a `mapBmcLot` response mapper + a status bridge
  (`OPEN→received`, `QC_PENDING→chilled`, `DISPATCHED→dispatched_to_plant`), then drop
  `mockOnly` on `listBmcIntakes`/`chillIntake`/`dispatchIntakeToPlant`. (Plant-operator read
  `plant.ts listBmcLots` is already on http.)

### 2.11 Settlement & DBT — 🟡 FE-ONLY (rewire) + 🔴 (DBT amount)
Backend lifecycle (all live): `POST /settlements {dcs_id, date}` → returns a **SettlementBatch
with `id`** (PENDING_APPROVAL) → `POST /settlements/{id}/approve` (dual-control: **approver ≠
initiator**, else 403) → `/{id}/reject` → `/{id}/execute` (successful payouts → invoice `PAID`
+ SMS; batch ends `EXECUTED`, or `PARTIAL` if a farmer has no VERIFIED bank).
- 🟡 **FE rewire:** the FE's `DRAFT→APPROVED→SETTLED` "approve invoices" pre-step has **no
  backend equivalent**, and posting a DCS id into `/settlements/{id}/…` → `INVALID_ID`. Rewire
  the Sachiv settlement UI to: `POST /collection/invoices/generate` → `POST /settlements`
  (**capture the returned batch `id`**) → `/{id}/approve` (as Adhyaksh/President) →
  `/{id}/execute`; thread the batch id through screen state. Then drop `mockOnly` on the three
  writes. Reads `listInvoices` / `getReleaseSummary` are already http. Batch ref field is
  **`pa_ref`** (not `union_ref`).
- 🔴 **DBT (A5):** `POST /dbt/requests` requires `{scheme_code, farmer_party_id, amount>0}`;
  the FE apply flow collects no amount → `applyScheme` stays `mockOnly`. **FE fix:** add an
  amount field (or a `scheme_code→subsidy` table), then drop `mockOnly`. `listSchemes`
  (`GET /dbt/requests`) is fine on http but returns `scheme_code` only (no `scheme_name`) —
  derive the label FE-side.

### 2.12 Plant — batches → product lots → QR — ✅ LIVE except product-lot fields
`GET|POST /plant/batches` (+ `GET /{id}`, `/{id}/complete`) — batch carries
`contributing_dcs_ids`, `input_litres` (not `volume_litres`) · `POST /plant/product-lots`
(+ `/{id}/recall`) · `GET|POST /plant/qrs`. Safety gate enforced: dispatch/batch/product/QR
refuse a `BLOCKED` subject (`422 SAFETY_GATE_BLOCKED`).
- 🔴 **Product-lot mint (A3):** `POST /plant/product-lots` requires `{batch_id, sku,
  product_name, units, unit_size, expiry_date}` (`mrp`/`mfg_date` optional). The FE sends only
  `{productId, units}` → fails `MISSING_FIELD: batch_id`. **FE fix:** resolve
  `sku/product_name/unit_size` from the products catalogue by `productId`, capture
  `expiry_date` in the pack sheet, send `batch_id`. **Or backend fix:** accept a `product_id`
  and derive `sku/name/unit_size` server-side from the product master, leaving only
  `expiry_date` on the client. QR mint (`POST /plant/qrs`) depends on a real product lot — same gate.

### 2.13 Products catalogue (read by plant) — 🔴 BACKEND-GAP
- The catalogue is only `GET /admin/products` (read gate: SUPER_ADMIN, PCDF_ADMIN **and
  SUPPORT_AGENT** — ⚠️ `backend-gaps.md` A10 omitted SUPPORT_AGENT). A `PLANT_OPERATOR` calling
  it gets 403, so the plant batch screen can't list product options on http.
- **Backend fix:** expose a session-readable `GET /products`, **or** add plant roles to the
  `/admin/products` read gate. Until then `commerce.ts listProducts` stays `mockOnly`.
- This blocks 2.12 (product-lot mint needs `sku`/`unit_size` from the catalogue).

### 2.14 Quality / Lab — ✅ LIVE gate; 🟡/🔴 queue & trace-back projections
- ✅ **The safety gate is fully live and correct:** `GET /quality/limits`,
  `POST /quality/qc-results` (`recordTest`), `GET /quality/qc-results` (`listTests`). **Send
  `AFLATOXIN_M1`** (the backend also normalizes `AFM1`, `COLIFORM`, etc., so the aflatoxin gate
  **cannot** be bypassed by alias). Subject types: `PROCESSING_BATCH`, `BMC_LOT`,
  `DCS_CONSIGNMENT`. QC test uses `pass` (not `passed`), `overall_pass`, `analyst_party_id`.
- 🟡/🔴 **Queue & trace-back are lean projections:**
  - `GET /quality/qc-queue` → `{items:[{subject_type, subject_id, stage, reference,
    org_unit_id, mandatory_tests[], recorded_tests[], pending_tests[], created_at}], total}` —
    **no full batch object.**
  - `GET /quality/batches/{id}/trace-back` → `{batch_id, batch_number, plant_id, status,
    block_reason?, contributing_societies:[{org_unit_id, code, name, district, resolved}],
    qc_results[]}` — **no `volume_share`, no `pour_count`, no embedded batch.**
  - `POST /quality/batches/{id}/certificate` (PLANT_LAB_ANALYST; requires `PASSED` **or**
    `COMPLETED`, else `409 BATCH_NOT_PASSED`).
  - **Fix (pick a side):** narrow the FE `QcQueueItem`/`traceBack` types to the lean shape and
    fetch the batch separately (**FE-only, unblocks now**), **or** enrich the backend DTOs to
    echo the batch + per-society `volume_share`/`pour_count`. Until decided, keep those two
    reads `mockOnly` to avoid an empty Lab screen. `recordTest`/`listTests` go live regardless.

### 2.15 Cattle & 1962 MVU — ✅ LIVE reads; 🟡 health-event body
Reads are http-correct: `listAnimals`, `getAnimal`, `requestVet`, `listVetRequests`
(reconciled: `recorded_by_role`, `bp_sync_status=SYNCED`, MVU `requested_at`; MVU list accepts
`?dcs_id=&farmer_party_id=&status=`, FARMER forced own). `POST /cattle/animals/{id}/bp-sync`,
`…/mvu-cases/{id}/dispatch|close`, `GET|POST /cattle/education`. Telemetry dormant → `403
FEATURE_DISABLED`.
- 🟡 **Health-event log (A12):** canonical is `POST /cattle/animals/{animalID}/health-events`,
  body `{type, details, occurred_at?}`, gated to **VETERINARIAN / AI_TECH**. The FE posts
  `{title, notes, byPartyId}` to a different path. **FE fix:** switch to the canonical
  route+body (put title/notes inside `details`), then drop `mockOnly`. Enums:
  `ARTIFICIAL_INSEMINATION` (not `AI`), `DISEASE_REPORT` (not `ILLNESS`).

### 2.16 Public QR trace (consumer scan) — ✅ LIVE; 🟡 roster shape
`GET /public/qr/{qr_code}` (no auth) → product, batch, plant, `ledger.intact`, `recalled?`,
and a `sourcing` block. Also `GET /public/ledger/verify?from=&to=`; officials-only
`GET /trace/{entity_type}/{entity_id}` (+ `/timeline`).
- 🟡 **Roster shape (B2):** `sourcing.farmers_total` and the consented `sourcing.farmers_public[]`
  roster sit at the **`sourcing` top level** (not per-society); each `sourcing.samitis[]` carries
  `volume_litres` + **`volume_share`** (per-society) but **no per-samiti `consented_farmers`**.
  **FE fix:** read the roster from `sourcing.farmers_public[]` (add a `TraceResult`-level slot),
  keep reading per-society `volume_share`. The scan is **set-valued** — never render per-farmer
  origin.

### 2.17 Dashboards — ✅ LIVE; 🟡 missing tiles
`GET /dashboards/farmer/{partyID}` (FARMER forced self) → `{farmer_party_id, today, month,
pending_amount, pending_invoices, trend[12]}` — **the 12-day litres `trend` already exists**
(⚠️ `backend-gaps.md` B1 said it was missing detail — it isn't). Only **`animalCount` is
absent** on the farmer side.
`GET /dashboards/society/{dcsID}` (org-scoped) → `{dcs_id, date, today, month, active_farmers,
member_count, open_consignment}`.
- 🔴 **Society gaps:** `avg_fat_pct`, `avg_snf_pct`, `quality_failures_30d`, and a 12-day trend
  are all **absent**; also `open_consignment` is declared but the service never sets it → always
  `false` (a **latent backend bug** to fix). FE defaults these to `0`/`[]`/`false` so nothing
  crashes. Backend fix: populate them if the tiles should be live.

### 2.18 Admin / platform — ✅ LIVE; 🔴 sachiv-cap
`GET /admin/flags` · `PUT /admin/flags/{key}` (closed key set: `collar_telemetry_enabled`,
`photo_ocr_enabled`, `ondc_enabled`, `consumer_commerce_enabled`) · `GET /admin/stats` ·
`GET|PUT /admin/products` · `GET /audit/logs` (+ `/export`) · `GET /notifications?phone=&status=`
· `POST /notifications/worker/run` · `GET /support/parties/lookup?phone=`.
- 🔴 **Sachiv-cap (A9):** there is **no** `/admin/sachiv-cap` route and flags are booleans (no
  numeric knob). **Backend fix:** expose the max-Sachivs-per-DCS cap as a small settings/config
  value, or confirm it is enforced server-side only and drop the FE control. Until then keep it
  `mockOnly`.

### 2.19 CMS content — ✅ LIVE
`GET /content?type=&since=<version>&scope=` (**versioned delta pull** — pass the last `version`
you saw; response `meta` carries the new max) · `GET /content/helpline?scope=` ·
`POST /content` · `PUT /content/{id}` (PCDF_ADMIN/MISSION_OFFICIAL write).

### 2.20 Real-time (SSE) — ✅ LIVE (kyc only); 🔴 more topics
`GET /events/stream` (SSE, Bearer; stream via XHR). Today it broadcasts exactly **one** app
topic: **`kyc.pending.changed`** (to reviewer roles) — FE `useLiveSync` nudges on it, then
re-queries `/kyc/pending/count` (nudge, then re-fetch; never trust the event as the value).
Heartbeat comment ~25s.
- 🔴 **More live nudges (C1):** `settlement`/`quality`/`pour` do **not** push to SSE (those
  eventbus topics only queue SMS). **Backend fix (if wanted):** also broadcast
  `settlement.changed` / `quality.changed` / `pour.recorded` to the SSE hub; FE re-queries the
  scoped value on any nudge. Until then those screens poll/refetch on navigation.

### 2.21 Notifications (SMS outbox) — ✅ LIVE (no FE change)
The backend queues mock-provider SMS on four transitions — **KYC-pending**, **payout-credited**,
**MVU-dispatched**, **gate-blocked** (→ Union Field Supervisor). No FE work; drained by
`POST /notifications/worker/run` (or a real provider later).

### 2.22 Consumer app (delivery / store / wallet) — ⚪ OUT OF SCOPE
A **separate** service (`EXPO_PUBLIC_CONSUMER_API_URL`). **Do not** point these at
`parag-saathi-be`. Only the public QR scan (2.16) is consumer-facing here.

---

## 3. Canonical reconciliation checklist (do in the FE mappers before `MODE=http`)

The backend is canonical; adjust `mapRequest`/`mapResponse` to these. **Safety-critical first.**

| Area | FE was sending/reading | Canonical (backend) |
|---|---|---|
| 🔴 QC test name | `AFM1` | **`AFLATOXIN_M1`** (alias normalized, but send canonical) |
| 🔴 QC subject type | `PLANT_BATCH`, `BMC_INTAKE` | `PROCESSING_BATCH`, `BMC_LOT` |
| Pour source | `ANALYZER_AUTO` | `ANALYZER_DIRECT` (+ `PHOTO_OCR`, `MANUAL`) |
| Pour rate field | `rate` | `rate_per_litre` (+ new `assurance`, `rate_chart_version`) |
| Pour status | IN_LOT/INVOICED/PAID | `RECORDED\|SUPERSEDED\|CANCELLED` (derive lifecycle FE-side) |
| Consignment litres | `total_litres` | `total_quantity_litres` (+ `seal_code`, `sealed_at`, `assurance`) |
| Invoice date/amounts | `date`, gross/net | `invoice_date`, `total_amount`, `total_quantity_litres` (no deductions on invoice) |
| Invoice status | DRAFT/APPROVED/SETTLED | `ISSUED\|SETTLEMENT_PENDING\|PAID\|HOLD` |
| Batch litres | `volume_litres` | `input_litres` (+ `contributing_dcs_ids`) |
| QC test result | `passed`, `recorded_by_party_id` | `pass`, `overall_pass`, `analyst_party_id` |
| BMC lot status | RECEIVED/CHILLED | `OPEN\|QC_PENDING\|PASSED\|BLOCKED\|DISPATCHED\|POOLED` |
| Trip delivered | — | `delivered_to_bmc_id` |
| Org type | `UNION`, `PLANT` | `MILK_UNION`, `PROCESSING_PLANT` |
| Settlement ref | `union_ref` | `pa_ref` |
| Health event enums | `AI`, `ILLNESS` | `ARTIFICIAL_INSEMINATION`, `DISEASE_REPORT`; MVU `requested_at` (not `created_at`) |
| Roles map | unguarded `ROLES[code]` | add `ORGANISING_MANAGER`, `CONSUMER`, `ONBOARDING_EXECUTIVE`, `STORE_MANAGER`; **guard the lookup** |
| KYC tier | — | `SERVICE`↔`MTLS` both accepted |
| Pickup path | keyed by DCS id | keyed by **consignment id** |
| Any empty ID | `org_unit_id:""` | **omit the field** (never send `""`) |

---

## 4. Backend-change backlog (the real 🔴 gaps, prioritized)

These are what the backend team should pick up so the FE can flip the remaining `mockOnly`s.
Each says the cheapest unblock (FE-side) vs the richer fix (backend-side).

**P0 — blocks a live FE screen**
1. **Product catalogue readable by plant** (2.13) — add `GET /products` (session) **or** add
   plant roles to `/admin/products` read. *Unblocks the plant batch screen + product-lot mint.*
2. **Product-lot mint contract** (2.12) — either FE resolves full fields from the catalogue
   (needs #1), or backend accepts `product_id` and derives `sku/name/unit_size`. *Unblocks QR.*
3. **Sachiv-picker org name** (2.3) — enrich `GET /parties?role=` (or `/roles/assignments`)
   with `{org_unit_id, org_name}`.

**P1 — reviewer/lab/farmer polish**
4. **Onboarding rich fields** (2.4) — extend `onboarding_requests` (preferred) or accept
   note+`document_refs[]`.
5. **QC queue / trace-back enrichment** (2.14) — echo the batch + per-society
   `volume_share`/`pour_count`; *or* FE narrows types (FE-only, no backend change).
6. **Society dashboard tiles** (2.17) — add `avg_fat_pct`, `avg_snf_pct`,
   `quality_failures_30d`, a 12-day trend, and **fix `open_consignment` never being set**; add
   farmer `animal_count`.
7. **Public roster slot** (2.16) — FE reads `sourcing.farmers_public[]` (FE-only; no backend
   change needed — already served).
8. **Single-consignment GET** (2.9) — add `GET /logistics/consignments/{id}` (or FE sources
   from list).
9. **Extra SSE topics** (2.20) — broadcast `settlement.changed`/`quality.changed`/`pour.recorded`.
10. **DBT amount** (2.11) — FE adds the amount field (FE-only).

**P2 — scope-dependent / cosmetic**
11. **DCS→Union B2B invoice leg** (2.8) — only if the pilot needs it.
12. **Org `name_hi`** (2.5) — bilingual org names.

**No backend change (pure FE):** settlement rewire (2.11), BMC status mapper (2.10),
health-event body (2.15), on-device OCR (2.7), the entire §3 reconciliation table.

---

## 5. Verify your integration
```bash
make smoke               # 23-step pour→QR→settlement + KYC-approval + concurrency loop
bash scripts/dryrun.sh   # multi-role login, KYC→notification→approve→assignment, dashboards, assurance/seal
```
Both must print `PASSED` — they exercise the exact request/response shapes your mappers must
match. Companion docs: [`FRONTEND_INTEGRATION.md`](FRONTEND_INTEGRATION.md) (full endpoint
contract), [`strict_brief.md`](strict_brief.md) (per-endpoint behaviour),
[`BACKEND_FLOW.md`](BACKEND_FLOW.md) (diagrams), [`WORKFLOW_GUIDE.md`](WORKFLOW_GUIDE.md)
(role flows).
