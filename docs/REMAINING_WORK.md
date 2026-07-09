# Saathi — Remaining Work (go-live tracker)

> Single place to track everything not yet built, since we're going live on **real data**
> (no `mockOnly`). Each item is tagged **when** it's needed, with the reasoning, so we can
> schedule honestly. Sources: `SPEC_CONFORMANCE.md` (backend vs Developer Note) and
> `parag-saathi-fe/docs/BACKEND_WIRING_GUIDE.md` (FE wiring).
>
> **Priority key:** 🔴 **NOW** (before real go-live) · 🟡 **SOON** (do early — cheap + real
> value) · ⚪ **LATER** (schedule after go-live; nothing here blocks the app running).

---

## TL;DR — the "now or later?" answer

- **The backend needs NOTHING to go live.** It's functionally complete and green
  (build · vet · 11 test pkgs · smoke 23/23 · dry-run · live spot-check). Every §D item below is
  *conformance/integrity hardening*, not a functional blocker.
- **The only remaining FUNCTIONAL task is FE-side:** the **settlement dual-control rewire**. It's
  needed **only if paying farmers is in the day-1 go-live scope** (the current mock doesn't move
  money). Backend is ready; this is FE UX work.
- **Two cheap backend items are worth doing early** (🟡): seal-verify at pickup, and pinning the QC
  limit-set version — both small, both raise the integrity/audit story.
- **Everything else is LATER** — safe to run the pilot without it.

**One decision drives the cut line:** *is real farmer payment (settlement) live on day 1?*
If **yes** → do the FE settlement rewire first. If **no** (capture-first pilot) → nothing is
blocking; ship and schedule the rest.

---

## 1. Frontend — the 6 remaining `mockOnly`

Goal is zero mock on go-live. Verified count: 6 (payments ×3, logistics ×2, collections ×1).

| # | Endpoint(s) | Blocks a user flow? | When | Remaining work |
|---|---|---|---|---|
| 1 | `approveInvoices` · `settleInvoices` · `approveRelease` (`payments.ts`) | ✅ **Yes** — the only remaining user-facing flow (it moves money) | 🔴 **NOW** *if payments are day-1* — else 🟡 | **Settlement multi-role UX rewire** — Sachiv `POST /settlements` (capture batch `id`, PENDING_APPROVAL) → **Adhyaksh/President** `/{id}/approve` (approver ≠ initiator) → `/{id}/execute`. Backend is **ready**; this is FE screen work (a second approver + thread the batch id). Do it as a dedicated, separately-verified change. Recipe: `BACKEND_WIRING_GUIDE.md` §3. |
| 2 | `startTrip` (`logistics.ts`) | ❌ No — cosmetic | ⚪ LATER | **Drop it / keep mock.** The backend has no "start" — a trip auto-progresses PLANNED→(first pickup)→IN_PROGRESS. Nothing to wire. |
| 3 | `pushVanLocation` (`logistics.ts`) | ❌ No — feature choice | ⚪ LATER | **Decision, not a bug.** No backend live-location route exists (the "dense GPS track" the Note describes; backend only does discrete stop/cold-chain samples — see §2 D-8). Keep mock, **or** backend adds `POST /logistics/trips/{id}/location` for live van tracking on the map. |
| 4 | `photoOcrExtract` (`collections.ts`) | ❌ No | ⚪ LATER | **By design** — on-device OCR → `POST /collection/readings` with `mode=PHOTO_OCR` + `photo_object_key` + `ocr_confidence`. Stays mock until on-device OCR + the S3 photo seam are wired. Not a backend gap. |

**Net:** only **#1 (settlement)** is real remaining integration; #2 is a delete, #3/#4 are later features.

---

## 2. Backend — in-pilot gaps vs the Developer Note (`SPEC_CONFORMANCE.md` §D)

None blocks the running app. Ranked by when to do them.

| # | Gap (Note §) | When | Effort | Why that timing |
|---|---|---|---|---|
| D-1 | **Seal verification at van pickup** (§6.4, §13) — backend ignores the `seal_code` the FE sends | 🟡 **SOON** | S | Small + high-trust: it's an acceptance criterion (seal-mismatch detected) and the FE already sends `seal_code`. Add a match check → `409 SEAL_MISMATCH`. |
| D-4 | **QC limit-set version pinned per result** (§10, §13-5) | 🟡 **SOON** | S | One field on `QCResult` (the FSSAI limit-set id/version it was judged against). Cheap; makes food-safety verdicts audit-defensible if limits change later. |
| D-3 | **QC `HOLD` state + re-test** (§6.5, §7) — gate is only PASS/BLOCK | ⚪ LATER | M | Current default is **safe** (borderline → BLOCK/quarantine, conservative). HOLD is an efficiency win (re-test instead of discard), not a safety fix. |
| D-2 | **Van batch-code mint** (§6.4, App A `BAT-…`) | ⚪ LATER | M | The pooling relation is already recorded (trip→consignments, lot→consignments). This adds the Note's named code/label — conformance + ops readability, not function. |
| D-5 | **Weighbridge reading + `sample_id` at intake** (§6.5) | ⚪ LATER | S–M | Extra intake fields for traceability detail; the gate already works on the sample. |
| D-6 | **Qty-delta / seal-mismatch anomaly flags** (§12-4, §13-3) | ⚪ LATER | M | Leakage/fraud surfacing (pour-sum vs pickup vs weighbridge). Valuable for ops at scale; not needed to run. Pairs with D-1/D-5. |
| D-7 | **Consignment post-seal `CORRECTION_PENDING` + Union approval** (§7) | ⚪ LATER | M | Pour-level supersede already covers pre-consignment fixes. The consignment-level post-seal correction (Union approves) is a rarer workflow. |
| D-8 | **Generic offline sync** — `/sync/batch` (all events) + `/reference?since=` (§8, §9) | ⚪ LATER | L | The field-critical path — **pours** — is already offline + idempotent (`client_event_id`, `/pours/batch-sync`). Consignment/pickup/QC are online writes. **Watch:** if van/DCS connectivity is poor, van pickup offline may need this sooner. |
| D-9 | **Signed-PDF invoice + `deductions[]` + `net_payable`** (§6.3) | ⚪ LATER | M | Backend issues same-day invoices with `total_amount`. Deductions (feed/advances) + a signed PDF are enhancements — do only if deductions are day-1. |

---

## 3. Deferred open seams (on your signal — do NOT auto-do)

| Seam | When | Note |
|---|---|---|
| **DigiLocker KYC** | ⚪ LATER (explicitly deferred) | `POST /kyc/aadhaar` mock seam works today (last-4 + vault ref, PENDING → reviewer approves). DigiLocker doc-pull wires behind the same endpoint — no FE change expected. |
| **Redis** (shared rate-limit + multi-replica SSE) | ⚪ LATER (on getting the string) | In-process limiter + single-node SSE work now; `REDIS_URL` seam is inert until enabled. Only needed at multi-replica scale. Zero FE impact. |
| **Device signatures** (ed25519 `device_sig`) | ⚪ LATER (phase-2) | Ledger is hash-chained + tamper-evident without it; device-key signing is hardening. |
| **Merkle root external anchoring** | ⚪ LATER (phase-2/4) | Explicitly "optional phase-2 hardening" in the Note (§5, §14). |
| **Serialized per-pack QR** + clone/scan analytics | ⚪ LATER (phase-4) | Lot-level QR is the pilot default and is live. |

---

## 4. Recommended go-live cut line

**Before flipping real users on:**
- If paying farmers day-1 → **FE settlement rewire** (§1 #1). *(Only hard requirement, and it's FE.)*
- 🟡 **D-1** seal-verify at pickup and 🟡 **D-4** QC limit-version — both small, both strengthen the
  traceability/food-safety story we're selling. Worth squeezing in.
- FE: delete the `startTrip` stub (§1 #2).

**Everything else → after go-live**, scheduled by need:
- If field connectivity is poor at pickup → bump **D-8** (offline) up.
- If deductions are part of settlement → do **D-9** with the payment work.
- DigiLocker / Redis / device-sig / Merkle / serialized-QR → on your signal.

_Last updated during the go-live pass. Backend verified green; app live end-to-end except the 6 FE
`mockOnly` above (only settlement is real work)._
