# Saathi Backend — Strict Conformance Audit vs the Developer Note

> **Question answered:** after the FE-alignment build, is the backend *complete*
> against `docs/Saathi_Developer_Note.pdf`, or are relations / parts /
> architecture still to do?
>
> **Verdict:** the **pilot traceability loop and all its relations are complete
> and green** (build · vet · gofmt · 11 test pkgs · `make smoke` 23/23 ·
> `dryrun.sh` · live spot-check). A handful of Note items are **deferred by
> design/phase** (incl. DigiLocker KYC and Redis, both intentionally left as open
> seams), and a short list of **in-pilot gaps** remain genuinely un-built. This
> doc is the honest line-by-line. **Nothing here was auto-fixed** — it is a status
> report; deferred and open items are called out, not silently closed.
>
> Legend: ✅ done · ◑ divergent-but-functional (conscious choice) · ⏸ deferred
> (phase-2/4 or explicitly deferred) · ❌ real in-pilot gap (Note expects it, not built).

---

## A. ✅ Done — the core loop and every relation it needs

| Note § | Requirement | Status |
|---|---|---|
| §1, §3 | Golden path capture→pool→transport→gate→lot→QR→consumer→pay | ✅ end-to-end, smoke-proven |
| §4 | Domain entities + the two aggregation fan-ins (pours→consignment, →batch/lot) as first-class links | ✅ (`pour_ids`, `consignment_ids`, `bmc_lot_ids`, `contributing_dcs_ids`) |
| §5 | Append-only hash-chained ledger (`prev_hash`/`event_hash`), corrections append-not-edit | ✅ (`provenance_events`, pour supersede) |
| §6.1 | Farmer profile, cattle counter (+ dormant Pashu Aadhaar), helpline/schemes/videos via CMS | ✅ (cattle, CMS `content`+`helpline`, education) |
| §6.2 | Assurance A/B/C on every pour; **weakest inherited** by consignment/batch/lot | ✅ (`WeakestAssurance`) |
| §6.3 | Pour pins `rate_chart_version`; invoice reproducible by replaying pours+chart | ✅ (defensible invoice) |
| §6.5 | Test-**before**-tip: only gate-passed milk advances; AFM1 ≤ 0.5 heat-stable gate; block quarantines forever | ✅ (BMC + plant gates, status-guarded) |
| §6.6 | Lot-level QR, HMAC-signed, unguessable, revocable (recall → notice, not 404) | ✅ (`PRG-…` + signed token) |
| §6.7 | Public no-login set-valued trace: societies + `volume_share`, `farmers_total`, **consented** roster, quality cert, `ledger.intact` | ✅ (`public_consent`-gated) |
| §8 | Idempotent writes (`client_event_id`); RBAC party+role scoped; audit on mutations | ✅ |
| §10 | Aadhaar never in clear (last-4 + vault ref, `json:"-"`); phone/bank masked; no PII on public surface; RBAC + audit | ✅ |
| §13-1 | Pour carries farmer/society/shift/time/**device_id**/chart-version/assurance; duplicate ≈ 0 | ✅ |
| §13-6/7 | Scan resolves O(1) to societies/farmers/window/quality; **no single-farmer attribution** | ✅ |
| §13-10 | Privacy/security acceptance (public surface clean, consent, RBAC, audit) | ✅ |
| — | Dual-control settlement, DBT rail, dashboards, onboarding saga, admin, sachiv-cap, B2B consignment invoice | ✅ (FE-alignment build) |

---

## B. ◑ Divergent but functional — conscious choices, not gaps

These work end-to-end; they differ from a *literal* reading of the Note. Each is a
product/architecture decision, documented so no one mistakes it for a bug.

| Note § | Note says | Backend does | Why it's OK |
|---|---|---|---|
| §6.3 | Invoice on a **10-day cycle** (`cycle_id`, `gross`, `deductions[]`, `net_payable`, signed PDF) | **Same-day** invoice (`invoice_date`, `total_amount`); deductions/payout on the settlement batch | Pilot chose same-day settlement; still reproducible from immutable pours + `rate_chart_version` (the §6.3 "defensible" property holds) |
| §6.3 | Rate = **two-axis matrix** OR **kg-fat/kg-snf** formula | Linear `rate_per_l = base + fat·fatRate + snf·snfRate` | A third data-driven model; versioned + replayable. To match the Note exactly the chart model would change (data, not code) — a Union-config decision |
| §5 | **Per-chain** ledgers (society/trip/plant line) | Single global monotonic chain | Documented national-scale trade-off (§20-2); simpler + correct for pilot |
| §6.4, §4 | Van **mints a BATCH** (`BAT-…`, Appendix A) binding consignments; own state machine `DRAFT→MINTED→IN_TRANSIT→AT_PLANT→RECEIVED` | `RouteTrip` (pickup→deliver) + `BMCLot` pooling — no `batch_code` object | The trip+lot chain carries the same custody + pooling relations; the Note's named "batch code" artifact is not minted (see D-2) |
| §6.6, App B | QR URL `trace.parag.in/t/<token>`, `token=base62(lot_ref)·HMAC6` | `GET /public/qr/{qr_code}` (`PRG-…`) + separate signed token | Functionally identical (unforgeable, revocable, resolves to LOTSUMMARY) |
| §8 | Flat endpoints (`POST /pours`, `/consignments/{id}/seal`, `GET /t/{token}`) | Nested REST (`/api/v1/collection/pours`, `/logistics/…/dispatch`, `/public/qr/…`) | Note §8 is explicitly "representative"; the live nested surface is the contract |

---

## C. ⏸ Deferred — phase-2/4 by the Note, or explicitly deferred by us

**Not built on purpose. Seams are left open; do not treat as gaps.**

| Item | Note basis | State | Note |
|---|---|---|---|
| **DigiLocker KYC** | §6.1/§10 identity | ⏸ **deferred (you)** | `POST /kyc/aadhaar` exists as the mock seam (stores last-4 + vault ref, PENDING). DigiLocker doc-pull to be wired later — **left open-ended, untouched.** |
| **Redis** (shared rate-limit + cross-replica SSE fan-out) | §6, §1.10/§7 | ⏸ **deferred (you)** | In-process limiter + single-node SSE work today; `REDIS_URL` seam is present and inert. To be enabled when the Redis string lands — **untouched.** |
| **Device signatures** (`device_sig` ed25519 over `event_hash`) | §5 event envelope | ⏸ not built | Ledger is hash-chained + tamper-evident without it; device-key signing is hardening. Adds a `device_sig` column + verify on sync when device keys exist |
| **Merkle root external anchoring** | §5, §14 Phase-4 | ⏸ not built | Explicitly "optional phase-2 hardening (§14)" in the Note |
| **Serialized per-pack QR** + clone/scan-count analytics | §6.6, §14 Phase-4 | ⏸ not built | Lot-level QR is the "pilot default" (§6.6). ✅ pilot-complete |
| **Pashu Aadhaar herd binding** (national ear-tag) | §6.1, §14 Phase-4 | ⏸ dormant | `pashu_aadhaar` stored when present, behind the dormant path — as the Note prescribes |
| **Consumer commerce / ONDC / collar telemetry** | §11, §9 | ⏸ flag-gated OFF | Descoped by product decision; public QR trace stays live |
| **TLS / at-rest encryption / KMS / CERT-In log shipping** | §10, §11 | ⏸ deployment | Infra/ops, not application code; structured logs are ready |

---

## D. ❌ In-pilot gaps — the Note expects these for Phase 1-2 and they are NOT built

**This is the honest "yet to complete" list.** Left as-is per your instruction (report,
don't fix now). Each is small-to-medium and additive; none blocks the core loop.

1. **Seal verification at van pickup** (§6.4; §13-2/3). The Note: the driver *verifies* the
   `seal_code`; a mismatch is flagged and **blocks a clean pickup**. Today `POST /logistics/trips/
   {id}/stops/{consignmentID}/pickup` takes only `{temp_c, notes}` and **ignores the seal** — the FE
   even sends `seal_code`, and the backend drops it. → add a seal-match check (`409 SEAL_MISMATCH`).
2. **Van batch-code mint** (§6.4; App A `BAT-<union>-<route>-<date>-<shift>-<seq>·<hmac6>`; §13-3
   "every van trip mints a batch code binding its consignments"). Not minted — the trip has no
   `batch_code`. (The pooling relation itself IS recorded via the trip→consignments edges.)
3. **Quality HOLD state + re-test** (§6.5, §7; §13-5 "PASS/HOLD/REJECT"). Backend gate is binary
   `PASSED`/`BLOCKED`. No `HOLD` (borderline → re-test → PASS/REJECT) and no `TIPPED`/`DIVERTED`
   sub-states. A borderline sample can only pass or be quarantined.
4. **QC limit-set version pinned on each result** (§10 "every QC result stores the limit-set version
   it was judged against"; §13-5). `QCResult` records tests + verdict but **not** the FSSAI limit-set
   version — so a future limit change isn't retro-attributable per result.
5. **Weighbridge + `sample_id` at intake** (§6.5 `INTAKE {weighbridge_l, sample_id}`). BMC lot records
   `chilling_temp_c` but not a weighbridge reading or a drawn sample id.
6. **Quantity-delta / seal-mismatch anomaly flags** (§12-4; §13-3 "qty-delta anomalies are detected
   and flagged"). No reconciliation of pour-sum vs pickup vs weighbridge; large deltas aren't surfaced.
7. **Consignment post-seal `CORRECTION_PENDING` + Union approval** (§7 state machine). Pour-level
   supersede exists (blocked once consigned); the consignment-level "post-seal fix → Union approves"
   workflow is not modelled.
8. **Generic offline sync** (§8 `/sync/batch` for *all* event types; §9 `/reference?since=` delta for
   rate charts/roster/config). Today offline is **pour-focused**: idempotent pours + `/collection/
   pours/batch-sync`, and `/content?since=` for CMS only. Consignment/pickup/QC are online writes.
9. **Signed-PDF invoice + `deductions[]` + `net_payable`** (§6.3). Invoice carries `total_amount`
   only; no server-rendered signed PDF, no deduction lines. (Ties to B-invoice-cycle.)

---

## E. What to do with §D (when you decide to)

Rough order by value-vs-effort (for a future pass — **not done now**):

- **Quick, in-scope, safety-relevant:** D-1 (seal-mismatch at pickup — FE already sends the field),
  D-4 (pin `qc_limit_version` on `QCResult` — one field + the current limit-set id).
- **Medium:** D-3 (add `HOLD` to the gate state machine + a re-test path), D-5 (weighbridge/sample_id
  on intake), D-6 (qty-delta anomaly flags), D-2 (mint a `batch_code` on trip close, App-A format).
- **Product decisions first:** D-7 (consignment correction workflow + who approves), D-8 (broaden
  the offline outbox to all event types), D-9 (cycle invoicing + signed PDF + deductions — pairs with
  the same-day-vs-cycle choice in §B).
- **Open seams, on your signal:** DigiLocker (§C), Redis (§C), device signatures / Merkle (§C).

**Bottom line:** the backend is a **complete, verified pilot** of the Note's central claim — a
carton traces honestly and set-valued back to societies and consented farmers, with a tamper-evident
chain, the food-safety gate, assurance grading, and farmer payment. §D is the real remaining
build-list against a maximal reading of the Note; §C is intentionally open.
