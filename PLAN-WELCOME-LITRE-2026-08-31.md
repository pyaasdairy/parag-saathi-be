# Welcome Litre — Launch Plan

**31 Aug 2026 · analysis of Kushagra's pushes (3 repos) + the two campaign PDFs · plan only, nothing implemented yet**

---

## Part 1 — Kushagra's changes: verdict SAFE, with a short fix list

27-agent adversarial audit over all new commits. **Nothing existing breaks**: all 13 backend
packages + the full CRM E2E pass on the merged tree (the one failure was our own date-frozen
test, already fixed + pushed). Razorpay is code-untouched. Reviewer login, ₹99, price
authority, CRM-off inertness — all intact.

**What he shipped (all high quality, verified):**
- **BE** — Phase B SMS/WhatsApp transports behind the existing channel seam: truly inert
  without keys, every external send behind the full guard chain + exactly-once claim, plus a
  stricter per-channel consent guard. A consents API that feeds our G2 promo guard in
  exactly the right shape (derived `promotional` aggregate, fail-closed, erasure-covered).
  Per-language DLT template slots (skipped for now per founder — wire IDs when approved).
  A 36-endpoint rider console the Saathi app was already calling — clean route mounting,
  rider-owned collections, money paths reuse our exactly-once gates (double-debit traced and
  refuted).
- **Consumer** — consents/wallet-unlock/leads now reach the backend (consents even ride our
  mirror queue correctly); Hermes V1 opt-out fixes the iOS launch freeze, prebuild-safe.
- **Saathi** — dead demo/legacy code removed (verified dead), pour outbox kept + fixed
  (UTC timestamps the deployed backend accepts — works against production TODAY).

**Confirmed findings to fix (before the deploy pin moves):**

| # | Sev | Where | Finding |
|---|---|---|---|
| K1 | major | BE+FE | **Promo pipeline is grant-dead**: no screen can ever grant a marketing consent (the only writer revokes all channels) → every promotional trigger + the whole Phase B promo build stays suppressed at G2 forever. Needs an opt-in surface (wire ConsentSheet's dead toggles + a settings toggle). |
| K2 | major | BE | OTP attempt-carry turns the brute-force fix into an unauthenticated per-phone **login-lockout DoS**. |
| K3 | major | BE | Rider collections copy consumer PII (cash dues carry raw phone; door photos) but are **missing from the DPDP erasure cascade**. |
| K4 | major | BE | **Crate scan bindings never release** on DELIVERED/FAILED/CANCELLED — a reusable crate label dies after one task. |
| K5 | major | Saathi | **Sign-out destroys queued (unsynced) pours**; the rejected-pour store is write-only (no screen shows rejects). |
| K6 | minor | BE/FE | Consent edge-cases: create-race can drop a newer revoke; aggregate recompute race; 401-drained consent batch permanently discarded; pre-Phase-B 404 consents never backfilled. |
| K7 | minor | FE | Leads replay deletes parked rows on 403 (the known wrong-app-key failure mode); walletGate cart probe widens the fail-open window to ~15s. |
| K8 | minor | BE/Saathi | Undo's order-status restore silently best-effort; Saathi 30s resend timer vs new 60s server cooldown; pour-outbox enqueue/drain lost-update window; stale local `android/` would ship the regressed Hermes in a local release build. |

## Part 2 — The PDFs vs what's built

The published terms (Hindi binding, v1.0) + campaign doc confirm **Option C is exactly what we
built**: ₹0 pack 1 → single settled recharge ≥ ₹500 within 7 days of the *actual* first
delivery → pack 2 free → else expiry with "nothing has been charged". Already compliant, no
change: single-recharge rule (two ₹250s don't qualify), settled-funds-only, day-7 grace
anchored at real delivery, pause-never-cancel on low balance, one-per-phone (server-side claim
registry) + one-per-address with flag-not-block and a human review path, promoter
reconciliation, proof of delivery, cancel keeps earned packs, honest expiry message.

**The headline decision the PDFs force — the deferred "2+2 question" is now answered:**
Terms §3.1: *"Start a subscription in the app. No payment and no wallet balance is
required."* The Welcome Litre is the app's own acquisition offer, self-serve. The pay-first
2+2 funnel (and its ₹140 top-up gate) is retired by this document — the two offers cannot
coexist on the landing screen.

### Changes needed (P0 = before launch, P1 = launch week, P2 = after)

| # | P | Side | Change |
|---|---|---|---|
| 1 | P0 | FE+BE | **Self-serve Welcome Litre funnel** replaces the 2+2: new landing copy (§15.6), offer-terms summary screen BEFORE registration (§15.7 — CCPA compliance hinge), subscribe with **no wallet gate** (₹140 ask removed), plan choice honoured (daily/alternate, toned/FCM, 500ml/1L — free packs stay 2×FCM-500). BE: consumer-JWT self-enrol endpoint reusing the existing `crmEnrol` machinery (zone check, per-phone/address gates, pack-1 mint) minus promoter fields. 2+2 code stays for existing mid-trial members; the pitch surfaces switch off. |
| 2 | P0 | BE | **Pack-2 rides the next delivery** (terms §4.4–4.5): today we mint it blindly for next morning — if the subscription is paused that's a lone ₹15 drop and off-terms. Change: attach pack-2 to the next actual delivery day, 14-day cap from recharge when paused (then it lapses, nothing charged). |
| 3 | P0 | BE | **Noon shortfall notice** (terms §5.3 "we tell you"): move B-02 from 17:00 to ~12:05 IST. The engine keeps locking at midnight — we promise noon, deliver better; only the *notification* duty moves. |
| 4 | P0 | FE+BE | **Marketing opt-in surface** (fixes K1, enables Phase B): consent sheet + settings toggle, in-app opt-out (terms §12.3), 90-day win-back bar already in guard config. |
| 5 | P1 | BE | **₹500→₹300 gate knob** via the flags service (campaign: drop the gate if conversion < 25% — no deploy; changing it also needs terms v1.1 republished). |
| 6 | P1 | FE | **Recharge screen per §6.4**: add ₹300 preset, "≈ N mornings" day-equivalents, equal-weight tiles (Interface-Interference compliance), failure-recovery copy that never claims "no money deducted" unless gateway-confirmed. |
| 7 | P1 | FE+BE | **Device signals** (§7): send device id at registration → flag repeat installs across numbers (offer doc already has the field). |
| 8 | P1 | BE | **Pilot dashboard** (§13): daily metrics rollup from collections we already have (funnel counts, subscription→recharge %, litres/drop, abuse rate, delivery success) + stop-loss alerts riding the existing admin-notification path. |
| 9 | P2 | — | Deferred: DLT/WA template IDs (per founder — wire when approved), website fixes (W-19 "launching soon", /terms + /privacy pages the terms link to), referral programme (reward on first paid delivery — needs server-side referrals), Pyaas-credit expiry mechanics, closure-refund ops log, STOP-keyword handling (MSG91 webhook). |

**Explicitly NOT changing** (verified safe): midnight billing lock (over-delivers vs the noon
promise), subscription sweep, trial engine internals, wallet/Razorpay flows, delivery settle,
catalog/serviceability, all Saathi store flows.

## Part 3 — Deploy sequencing (hard gates, in order)

1. Fix K1–K5 (+ cheap minors) on `feature/crm-welcome-litre` / consumer / saathi branches.
2. Build P0 items 1–4; full test pass + emulator walk of both funnels.
3. **Render dashboard first**: paste MSG91 keys (Kushagra's prod boot guard makes the service
   REFUSE TO BOOT without them — merging before pasting = full backend outage), B2 keys
   (photo uploads 500 without them), decide the `saathi_dev` → production DB question,
   `CRM_ENABLED=true`, free→starter for reliable sweeps.
4. Move the render pin → deploy → smoke (reviewer login, order, recharge, rider console).
5. Only then: ship the Saathi APK (it 404s across ~15 screens against the old backend) and the
   consumer build with the new funnel.
6. Templates arrive later → flip per-trigger SMS/WA flags; nothing else changes.

## Part 4 — Open items for the founder

- **Confirm the funnel swap** (2+2 → Welcome Litre self-serve) — the one product decision in
  this plan; everything else follows the published terms mechanically.
- The campaign doc's own blocking checklist needs answers from LMU/ops (pack configuration in
  writing, "consumer created" definition, website link live, Play Data-Safety correction) —
  outside the codebase but blocking print.
- Recharge-bonus leak fix still sits undeployed on the branch; the deployed backend keeps
  granting retired bonuses until the pin moves (or a cherry-pick).
