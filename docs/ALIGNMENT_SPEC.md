# Saathi Backend ⇄ Frontend Alignment Spec

> **Goal:** align the Go backend (`parag-saathi-be`) to the 80%-built React Native app
> (`parag-saathi-fe`), reconciled with the **Developer Technical Note** (the authoritative
> traceability build spec). Integration (wiring the FE mappers) comes later — **first the backend
> is made spec-clean and capable**: we ADD genuinely-missing fields/endpoints/relations; we do NOT
> pollute the backend with FE-name aliases (those are FE-mapper fixes at integration time).
>
> This doc is derived from a 3-agent deep analysis of both repos (identity round) + the full
> Developer Note. It is the running plan; each **Piece** is fixed completely before the next.

---

## STATUS — audit complete, all confirmed findings fixed (backend integration-ready)

Every confirmed audit finding (23) is fixed or built. Verified green: `go build ./...`, `go vet`,
gofmt, 11 test packages, `scripts/smoke.sh` (23 steps), `scripts/dryrun.sh`, and a live spot-check
of all new endpoints. What was done since the initial spec:

- **Correctness/security fixes:** AFM1 gate-bypass closed (names normalized before every guard);
  settlement no-verified-bank → FAILED payout + PARTIAL batch (never a false PAID); MVU list PII
  leak + farmer-self scoping; cross-union pooling blocked in `CreateBatch`; wrong-BMC pooling
  blocked in `CreateBMCLot`; trip-creation IDOR + deliver-BMC scope; STATE_AUDITOR federation-wide reads.
- **Note-conformance builds:** pour `assurance` A|B|C + weakest-inherited on consignment (§6.2);
  `rate_chart_version` pinned (§6.3); consignment `seal_code`+`sealed_at` (§6.4);
  batch `contributing_dcs_ids` + public trace consented `farmers_public` roster + per-society
  `volume_share` (§7.4/§6.7); `Party.public_consent` (§6.7).
- **New endpoints/modules:** dashboards (farmer/society); quality `qc-queue` + `certificate` +
  `trace-back`; admin `stats` + product master; onboarding-request queue (approve → creates
  Party+KYC+RoleAssignment) + `GET /parties?role=`; CMS `content` (versioned delta pull) + helpline;
  live SSE badge + KYC reviewer notification.
- **Remaining for integration time (FE-mapper only, not backend):** the field-name/enum mapping
  in the FE endpoint files (e.g. `total_litres`→`total_quantity_litres`, `AFM1`→`AFLATOXIN_M1`),
  and the FE `recordPickup` path fix. The backend is canonical and complete.

---

## 0. What's already aligned (no work needed)

- **Wire conventions**: base `/api/v1`, success `{data, meta?}`, error `{error:{code,message,details?}}`,
  `Authorization: Bearer`, refresh-on-401, snake_case wire, ObjectID hex ids, ISO-8601 / `YYYY-MM-DD`
  dates, `client_event_id` idempotency. ✅
- **Module mounts** match the FE's `backend-integration.md` map exactly (identity, orgs, collection,
  logistics, plant, quality, settlement, cattle, platformops, publictrace). ✅
- **Multi-role model** (one phone = one Party = many org-scoped RoleAssignments) — the farmer+sachiv
  picker works: `/parties/me` returns all usable assignments org-enriched; `/auth/role/select` mints
  a role-scoped token for either. ✅ (verified live)

---

## 1. Cross-cutting mismatch catalog (from the identity-round analysis)

These recur across every entity. Classification drives WHO fixes it:

| Kind | Rule | Examples |
|---|---|---|
| **Field-name mismatch** (backend HAS the data under a different name) | **FE-mapper fix at integration** — backend stays canonical | `total_litres`→`total_quantity_litres`, invoice `date`→`invoice_date`, batch `volume_litres`→`input_litres`, QC `passed`→`pass`/`overall_pass`, `recorded_by_party_id`→`analyst_party_id`, MVU `created_at`→`requested_at`, health `by_role`→`recorded_by_role` |
| **Enum value mismatch** (spec canonical differs) | **fix the WRONG side vs Developer Note** | 🔴 QC `AFM1`→**`AFLATOXIN_M1`** (FE sends wrong → silently bypasses the aflatoxin gate — FE fix, safety-critical); pour `ANALYZER_AUTO`→**`ANALYZER_DIRECT`** (FE fix); org `UNION`↔`MILK_UNION`, `PLANT`↔`PROCESSING_PLANT`; health `AI`↔`ARTIFICIAL_INSEMINATION`, `ILLNESS`↔`DISEASE_REPORT`; QC subject `PLANT_BATCH`→`PROCESSING_BATCH` |
| **Missing backend field** (Developer Note / FE needs it) | **backend ADDS it** | `assurance` A\|B\|C on pour, `rate_chart_version` on pour, `seal_code`+`wavg_fat/snf` on consignment, `Party.public_consent`, cattle yield/lactation fields, batch `contributing_dcs_ids`, LOTSUMMARY materialization |
| **Missing backend endpoint** (FE 404s) | **backend BUILDS it** | onboarding-request queue, `GET /parties?role=`, `/quality/qc-queue`, certificate + trace-back, OCR extract, `/dashboards/*`, `/products`, CMS `/content?since=` |
| **Role/tier catalog gap** | **backend ADDS to catalog** | `ONBOARDING_EXECUTIVE`, `STORE_MANAGER`, `MTLS` tier |

**Danger findings (silent breaks) — must not ship:**
1. 🔴 **QC `AFM1` vs `AFLATOXIN_M1`** — the FE posts `name:"AFM1"`; the backend gate only matches `AFLATOXIN_M1`, so **the aflatoxin safety gate silently does not fire**. (FE fix; backend could also accept the alias defensively.)
2. 🔴 Consignment `total_litres`, invoice `date`/amounts, batch `volume_litres`/`qc_status` — all silently default to 0/'' in the FE (mapper reads wrong keys).
3. 🔴 Role-picker crash: FE `ROLES[code]` is unguarded; a backend-only code (`ORGANISING_MANAGER`, `CONSUMER`) in an assignment throws. (FE fix: guard + add codes.)

---

## 2. Per-Piece build roadmap (one role/functionality at a time)

Priorities: **P0** = FE calls it today and it 404s / breaks integration. **P1** = Developer-Note
correctness. **P2** = dormant / Phase-3.

### ✅ Piece 1 — Identity / Auth core  — **DONE**
- Added roles `ONBOARDING_EXECUTIVE`, `STORE_MANAGER` to `AllRoles` + `RequiredKYCTier` + granter
  matrix; `ONBOARDING_EXECUTIVE` wired as a KYC reviewer + enrolment granter (peer of `ORGANISING_MANAGER`)
  via new `domain.OnboardingReviewerRoles`.
- Added KYC tier `MTLS` (alias of `SERVICE`).
- Added `Party.public_consent` (§6.7) + `PATCH /parties/me {public_consent}`.
- Seed: the Sacheev (9000000001, Ramesh) now ALSO holds a FARMER role → login role-picker exercised.
- **Verified live:** multi-role `/parties/me` (SAMITI_SACHEEV + FARMER, org-enriched), role-select on
  both, `public_consent` toggle, and `ONBOARDING_EXECUTIVE`/`STORE_MANAGER` grants accepted.

### Piece 2 — Collection / Sachiv (the core loop) — P0/P1  ← NEXT
- **DB fields:** `MilkPour.assurance` (A|B|C, enum), `MilkPour.rate_chart_version` (string; store the
  chart's version alongside `rate_chart_id`), `RateChart.version` string. Assurance propagates as
  **weakest-in-set** to consignment/batch/lot.
- **Endpoints:** `POST /collection/pours/ocr-extract` (photo→{fat,snf,confidence}, mock OCR);
  `POST /collection/invoices/approve` (DRAFT→APPROVED per DCS/date — reconcile daily-DRAFT lifecycle
  the FE uses with the backend's ISSUED/SETTLEMENT_PENDING/PAID).
- **Reconcile:** invoice status vocabulary (FE `DRAFT|APPROVED|SETTLED`); assurance on readings.
- Multi-role note: a Sacheev logging a pour is often also a farmer — pours are keyed by
  `farmer_party_id`, never the actor, so self-pour is fine.

### Piece 3 — Farmer — P0/P1
- Farmer dashboard aggregate `GET /dashboards/farmer/:partyId` (today/month/pending + 12-day trend).
- Cattle fields the FE reads (`age_months`/`lactation_no`/`avg_daily_yield_litres` OR FE maps from
  `birth_date`/`lactation_status` — decide). Health-event enums.
- Schemes (`/schemes` → reconcile with `/dbt/requests`), education/CMS content.

### Piece 4 — Logistics / Van + Delivery — P1
- `Consignment.seal_code` + `sealed_at` + `wavg_fat/wavg_snf` (§6.4 seal); `RouteStop.seal_code`
  verify (409 SEAL_MISMATCH). Van batch-mint code (Developer Note `BATCH`), GPS track.

### Piece 5 — BMC + Plant — P1
- Batch `contributing_dcs_ids` denormalized (§7.4); product outputs → QR wiring; LOTSUMMARY
  materialization at lot seal (consumer O(1) read).

### Piece 6 — Quality / Lab — P0
- `GET /quality/qc-queue`; `POST /quality/batches/{id}/certificate`; `GET .../trace-back`.
- Accept `AFM1` alias defensively so the gate can't be bypassed by name.

### Piece 7 — Traceability (public) — P1
- Serve the materialized LOTSUMMARY shape (societies + volume shares, farmers_total,
  consented `farmers_public` roster driven by `Party.public_consent`, quality).

### Piece 8 — Admin / governance — P0/P2
- `GET /admin/stats`, `GET /admin/parties?q`, `/admin/feature-flags` (reconcile `/admin/flags`),
  `/admin/products` master, `/admin/audit-events` (reconcile `/audit/logs`).

### Piece 9 — Onboarding-executive assisted flow — P1
- Onboarding-request entity + `POST /onboarding/requests`, list, approve/reject; `GET /parties?role=`.
- Relation: approve → creates Party + KYCRecord + RoleAssignment.

### Piece 10 — Cattle / 1962 — P1
- `POST /cattle/animals/{id}/health-events` (POST exists? confirm); vet-request↔mvu-case enum reconcile.

### Piece 11 — Commerce / Store / Delivery — P2 (dormant)
- `GET /products` catalogue, orders, delivery — behind `consumer_commerce_enabled` flag.

### Piece 12 — Dashboards + Live sync — P1
- `GET /dashboards/society/:dcsId`; SSE already emits `kyc.queue_changed` — add
  `settlement.changed`, `quality.changed`, `pour.recorded` topics the FE `useLiveSync` listens for.

---

## 3. Enum reconciliation table (canonical = Developer Note / backend domain)

| Concept | Backend (canonical) | FE sends/reads | Action |
|---|---|---|---|
| Pour source | `ANALYZER_DIRECT`, `PHOTO_OCR`, `MANUAL` | `ANALYZER_AUTO`… | FE fix (send DIRECT) |
| QC test name | `AFLATOXIN_M1`, `COLIFORM`, `TPC`, `ANTIBIOTIC_TETRACYCLINE`, `PHOSPHATASE` | `AFM1`… | FE fix (safety); backend accept alias defensively |
| QC subject type | `DCS_CONSIGNMENT`, `BMC_LOT`, `PROCESSING_BATCH` | `LOT`, `BMC_INTAKE`, `PLANT_BATCH` | FE fix |
| Org type | `FEDERATION`, `MILK_UNION`, `PROCESSING_PLANT`, `BMC`, `DCS` | `UNION`, `PLANT`, `STORE`, `DISTRICT` | FE fix (map) |
| Health event | `ARTIFICIAL_INSEMINATION`, `DISEASE_REPORT`, … | `AI`, `ILLNESS`, `CHECKUP` | FE fix |
| Invoice status | `ISSUED`, `SETTLEMENT_PENDING`, `PAID`, `HOLD` | `DRAFT`, `APPROVED`, `SETTLED` | add DRAFT/APPROVED lifecycle (Piece 2) |
| Batch stage | `CREATED`, `QC_PENDING`, `PASSED`, `BLOCKED`, `COMPLETED` | `RECEIVED`, `PASTEURIZED`, `PACKED`, … | FE map |

---

## 4. New DB fields/relations added or planned

| Collection | Field | Status | Why |
|---|---|---|---|
| parties | `public_consent` bool | ✅ added (P1) | §6.7 consumer-trace roster |
| milk_pours | `assurance` A\|B\|C | planned (P2) | §6.2 capture assurance |
| milk_pours | `rate_chart_version` string | planned (P2) | §6.3 reproducible pricing |
| rate_charts | `version` string | planned (P2) | pins pour pricing |
| dcs_consignments | `seal_code`, `sealed_at`, `wavg_fat`, `wavg_snf` | planned (P4) | §6.4 tamper seal |
| processing_batches | `contributing_dcs_ids` []ObjectID | planned (P5) | §7.4 honest pooling |
| product_lots / new `lot_summaries` | materialized LOTSUMMARY | planned (P5/P7) | O(1) consumer scan |
| animals | yield/lactation fields | planned (P3) | farmer cattle view |
| new `onboarding_requests` | full entity | planned (P9) | assisted onboarding queue |
| new `products` | catalogue | planned (P11, dormant) | consumer commerce |
| new `cms_content` / extend education | helpline + versioned delta | planned (P3) | §6.1 CMS |
