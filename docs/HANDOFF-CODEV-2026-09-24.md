# Handoff - PYAAS integration to release/26.07.03 (24 September 2026)

**For:** the co-developer (backend Go + Flutter + Expo) who finishes the integration and takes
it to `release/26.07.03`.
**Continues:** `parag-saathi-be/docs/HANDOFF-CODEV-2026-09-20.md`, `pyaas-consumer/HANDOFF-CODEV-2026-09-20.md`,
`pyaas-saathi/HANDOFF-CODEV-2026-09-20.md`. Those three still hold the audit findings, the
20 Sep CRM table and the device test plan; this file is what changed since, the contracts
now in force, and the work left.

Every claim below that is not a plain git fact carries a `(source: NN)` tag naming the stage
report in `docs/handoff-2026-09-24-reports/NN-*.json` (committed next to this file: the raw
implementer reports and the independent reviewer verdicts, one JSON per stage). The full
54-trigger CRM audit is `docs/CRM-AUDIT-2026-09-24.md`.

**State of play.** The owner stopped all automated work at 02:45 IST on 24 September so the
team can read this document before anything else is fixed. Two backend stages were stopped
mid-way and are described exactly as they stand, with the work that is left spelled out step
by step: the CRM completion stage (section 4.6) and the Founding Family / referrals / noon-lock
stage on `feature/founding-referrals` (section 5), whose merge into `integration/delivery` is
NOT done. Nothing is uncommitted anywhere; every commit named below is on origin. Section 6
is the work list; section 8 is the register of every finding with its status.

---

## 1. Read this first

### 1.1 The three branches and their origin state (git log -1, taken 24 Sep ~02:30 IST)

| Repo | Branch | HEAD | Subject | origin |
|---|---|---|---|---|
| `parag-saathi-be` (`C:\Users\jaina\Downloads\parag-saathi-be`) | `integration/delivery` | `81e2ff6` | feat(crm): D-09 tells the member when a delivery fails | `origin/integration/delivery` == HEAD; full suite green at this commit (consumer package 79.3 s, run 24 Sep 02:50) |
| `pyaas-consumer` (`C:\Users\jaina\Downloads\pyaas-consumer`) | `feature/consumer-revamp-phase2` | `37a4f4a` | fix(referrals): sign-out forgets the session copy of the referral code | `origin/feature/consumer-revamp-phase2` == HEAD |
| `pyaas-saathi` (`C:\Users\jaina\Downloads\pyaas-saathi`) | `integration/delivery` | `c31bccb` | fix(rider): a task with no delivery pin says so instead of "too far" | `origin/integration/delivery` == HEAD |

Other refs you will meet:

- Backend `origin/release/26.07.03` = `8394476` (the deployed branch: `render.yaml` pins it, so
  merging into it IS deploying). It is an ancestor of `integration/delivery` since the merge
  commit `80f4a0b` (source: 06).
- Backend worktree `D:/dev/pyaas/be-features` on `feature/founding-referrals` = `e5d12ab` (docs)
  on `d2f8df9` (feat) on `80f4a0b`; pushed to `origin/feature/founding-referrals`. Reviewed
  (verdict ISSUES, four confirmed defects, section 5.3) and **NOT merged** into
  `integration/delivery`; the fix pass was stopped before its first commit, so the branch is
  exactly the two commits. Remove the worktree after the merge:
  `git worktree remove D:/dev/pyaas/be-features --force && git worktree prune`.
- Consumer `origin/feature/ui-revamp` = `5f92d2e` (Kushagra's 21 Sep list). It is an ancestor
  of `feature/consumer-revamp-phase2` since `359288c`, so Kushagra fast-forwards:
  `git checkout feature/ui-revamp && git merge --ff-only origin/feature/consumer-revamp-phase2`
  (source: 10). Consumer `origin/release/26.07.03` = `81a33ec`.
- Saathi `origin/release/26.07.03` = `9239553` (owner's KYC upload fix), merged into
  `integration/delivery` at `e7901be`.

Working-tree state at write time (do not be surprised by it):

- Backend: clean; every CRM-stage edit was committed (`804205f`, `7cbb043`, `81e2ff6`, section
  2.1). Untracked in the repo root, deliberately never staged: `pyaas-secrets-backup-2026-09-02.zip`,
  `PYAAS-SYSTEM-MAP-2026-09-02.md`, `PYAAS-codev-handoff/` (your two bundles).
- Saathi: ` M analysis_options.yaml` is a tool-made edit (the Flutter tool added
  `build/**` and `android/**` excludes); it is in no commit and every stage left it alone
  (source: 01, 05, 09). Leave it out of your commits too.

### 1.2 What merges where

1. `feature/founding-referrals` -> backend `integration/delivery`: NOT DONE. Do it only after
   the four confirmed defects in section 5.3 are fixed on the feature branch (the merge
   procedure is section 5.5).
2. Backend `integration/delivery` -> `release/26.07.03` = production. Deploy backend contract C3
   (`geoExact` on every task) **before or with** the Saathi build (section 3, C3).
3. Consumer: `feature/consumer-revamp-phase2` is the integration branch; Kushagra fast-forwards
   `feature/ui-revamp` to it; release is cut from there.
4. Saathi `integration/delivery` -> `release/26.07.03`; cut a new build (the pack size only
   appears on a fresh one).

### 1.3 Owner rules (in force for every commit)

- **The frontend is the spec.** The consumer app on `5f92d2e` and the Saathi app on
  `integration/delivery` define the wire; the backend is built to what they read and send.
  Frontend edits are integration fixes only (source: PYAAS_STATUS, owner decision 24 Sep ~02:00).
- **Kushagra's UI is untouched.** The ui-revamp merge (`359288c`) left 34 of his 55 files
  byte-identical and changed only conflict resolutions and seam files; the reviewer verified
  this against a recomputed 3-way merge (source: 10). Keep it that way.
- **Fixes go to the backend**, not around it in the apps.
- **Nothing local on the consumer but**: the cart (`store/cart.ts`), auth tokens (SecureStore,
  `lib/apiClient.ts`), the session pointer (`parag_current_uid`), UI preferences (language,
  reminder setting, rating flags, seen/snooze flags, setup-done gate, version stamp) and the
  offline outboxes whose rows are deleted once their replay lands (addresses, subscriptions,
  profile, delivery prefs, consents, complaints, promos, leads, referral-apply). Every other
  screen value is an in-memory copy of a server read, keyed by account and cleared on
  sign-out (source: 02, 04, 07). Owner-decided device-local exceptions: `user_location`,
  `milk_scans`, favorites, the local notices feed and the `vip` evidence row (source: 04, 07).
- **Profiles in scope:** rider + store manager (Saathi) and the consumer app only. Other Saathi
  profiles (onboarding/KYC, collection, pour, plant, cattle) stay untouched unless one of the
  three needs them (source: PYAAS_STATUS, owner scope rule 24 Sep 01:40).
- **Push every commit.** All three branches are on origin now; keep them there.
- **Never `git add -A` in the backend root.** `pyaas-secrets-backup-2026-09-02.zip` is untracked
  there; one `-A` publishes it. Stage by path. Move the zip out of the repo directory
  (section 6b).

---

## 2. What was done, per repo

Test columns: backend = `go build ./... && go vet ./... && CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 go test ./... -count=1`,
every package `ok`, consumer package > 10 s (Mongo tests ran). The Go suite is not counted per
test by the reports; the durations are the proof they executed. Consumer = `./node_modules/.bin/tsc --noEmit`
exit 0. Saathi = `flutter analyze` clean + `flutter test` counts.

### 2.1 Backend `parag-saathi-be`, `integration/delivery` (oldest first, after the 20 Sep tip `482bcdb`)

| Hash | One line | Tests at that point |
|---|---|---|
| `7897c58` | fix(catalog): PATCH variants[] is the product's full variant list (backend adapts to the shipped Saathi catalog tab) | suite green (source: 01) |
| `6b374f9` | perf(orders): GET /orders no longer signs proof photos (the detail route still does) | green |
| `731be2e` | fix(delivery): geoExact on the wire; the phone's fence verdict counts only on a real pin (C3) | green |
| `3c02bb1` | feat(delivery): societyId copied onto the task (C3) | green |
| `f00e790` | fix(delivery): a failed or store-cancelled task cancels the consumer order (also fixed the Mongo updated_at path conflict that made the store cancel 500) | green |
| `083ecf1` | feat(consumer): GET /stores/{storeId}/upcoming lists tomorrow's previews (C1) | green |
| `edc438e` | feat(consumer): POST /uploads/presign for complaint and door photos (C4) | green |
| `29936df` | chore(rider): remove five per-task routes (reverted below, see 2.1.b) | green |
| `4a2dc46` | feat(orders): address_id picks the door; the label stays the fallback | green |
| `9d7a29a` | style: gofmt after the variants and societyId fields | green |
| `47cd83e` | feat(crm): emit the order, complaint and rating lifecycle into the outbox (C6) | green |
| `f7aaef1` | feat(crm): route every event trigger by topic and conditions, failing closed (data-driven router, see 2.1.d) | green |
| `8afae8f` | fix(crm): pack 2 expires only once W-07 is on record as SENT (20 Sep bug 1) | green |
| `a1b8623` | fix(crm): W-01 is sent by the worker, not on the enrol request (20 Sep bug 3) | green |
| `c722996` | fix(crm): an unplugged channel is logged and the row records the intended primary (20 Sep bug 5) | green |
| `e9c609c` | fix(crm): the manual operator send dedups by content, not by the clock (20 Sep bug 4) | green |
| `c09b9c8` | fix(crm): trigger config routes to shipped channels and names fillable tokens | green |
| `70a2a8e` | chore(deploy): CRM and Expo push env keys declared in render.yaml and .env.example | green |
| `dc17eda` | feat(consumer): DELETE /push/register unbinds a device on sign-out (C2) | green |
| `aacb25d` | feat(crm): Expo push transport behind EXPO_PUSH_ENABLED (C5) | green |
| `fea7931` | refactor(push): the Expo pipe lives in internal/platform/push (C5) | green, consumer 24.8 s / 27.9 s (source: 01) |
| `bf083ef` | test(consumer): erasure empties consumer_push_devices for the member | green, consumer 31.8 s, reviewer run (source: 01) |
| `5b16775` | Revert "remove the five per-task routes" (deployed-route restore, 2.1.b) | green |
| `b00cc7d` | feat(orders): unique (subscription_id, scheduled_for) backstop under the day claim | green |
| `d6145f9` | feat(crm): dry-run origins for MSG91, WhatsApp and Expo behind three env keys (capture seams, section 4.3) | green |
| `5146fb1` | docs(consumer): C4 says the upload method comes from the response and is POST | green |
| `80f4a0b` | Merge origin/release/26.07.03 (owner's `775f370`, `640cff7`, `8394476`) into integration/delivery (2.1.c) | green, consumer 45.7 s impl / 66.6 s reviewer, 19/19 TestPhase2* PASS (source: 06) |
| `8edd9e1` | feat(crm): the dispatch claim is scoped to the order or complaint (`scope_key` = order or complaint id; two instant orders the same day each get D-01/D-06; claim index migrated with a failed-build guard) | CRM completion stage; suite green; **not independently reviewed** (the two reviewers were stopped before they ran) |
| `8d24915` | feat(crm): a trigger's delay is honoured through `crm_schedules` (`crm_schedule.go`; due at event time + delay; conditions re-checked at fire; a cancelled order never gets the delayed message) | same |
| `804205f` | feat(crm): every product event the audit found unemitted now reaches the outbox: `user.registered`, `wallet.credited` (top-up receipt, promo, refund, rider-undo credit, each scoped on its ledger ref), `subscription.activated` / `created_unpaid` / `modified`, `payment.failed` (Razorpay webhook + INSUFFICIENT_FUNDS mandate charge), `order.line_cancelled`, `delivery.delayed` (detector in `crm_detectors.go` on the scheduler tick: out for delivery past window end + 5 min, once per task), `serviceability.checked`; router facts for their conditions; `order.dispatched` carries `promotional_only` so D-02 fires; plus a bug fix: `storeAdjustDelivery` named `updated_at` in a `$set` that `updateDelivery` also stamps, so the task update was rejected after a re-bill | same; `crm_emitters_test.go` (564 lines) |
| `7cbb043` | fix(crm): conditions read what the emitters send (E-04 `complaint.type in ['missing']`, E-05 `== 'quality'`, A-05 `wallet.covers_first_cycle == false`, E-07 unparsable consent lines removed, W-01 routed on `offer.finalized` through the generic router); `meta.awaiting_event` names the 22 triggers with no product event or runner, each with its reason, and the router skips them; the claim scope prefers `complaint_id` over `order_id` | same; `crm_conditions_test.go` |
| `81e2ff6` | feat(crm): D-09 tells the member when a delivery fails (`order.failed` had no trigger); per_order, push then WhatsApp then SMS, inbox always; T-D09 EN + HI with `[LABELLED_PRODUCT]`, `[REASON]` (rider picklist label only), `[SUPPORT_NUMBER]`; `meta.trigger_count` 55, 47 templates | same; `crm_d09_test.go`; full suite green (run by the handoff author, 24 Sep 02:50) |

**2.1.a The CRM audit numbers.** The 20 Sep handoff counted 56 trigger ids with 11 dispatch
call sites and 45 dead. After `47cd83e`/`f7aaef1` the reviewer counted about 38 event
triggers still without an emitter (subscription.*, delivery.delayed/late/miss, payment.failed,
substitution, line_cancelled, route.changed, serviceability.checked, user.registered,
wallet.credited, abuse_flag_raised) (source: 01). The full re-audit of all 54 triggers is
`docs/CRM-AUDIT-2026-09-24.md` (per-trigger emitter, route, render, claim, delay, transport
and STATUS, then the concrete fix for every dead or partial row); `804205f` and `7cbb043`
implemented its emitter and condition fixes, `81e2ff6` added D-09, so `crm_triggers.json`
holds 55 ids at HEAD with 22 marked `awaiting_event` in `meta`. What that stage did not get
to is listed in section 4.6. Note: the stage-1/2 backend implementer report is not among the
committed reports (01 starts at stage 3); the reviewer's contract check in 01 and the commit
bodies stand in for it.

**2.1.b The deployed-route restore (`5b16775`).** `29936df` removed
`POST /consumer/delivery/tasks/{id}/otp/send`, `/otp/verify`, `/scan`, `/door-photo` and
`GET .../compliance` because no screen on `integration/delivery` calls them. The reviewer
found the DEPLOYED Saathi `release/26.07.03` still sends all five (`lib/api/rider_api.dart:203-224`,
from `proof_sheet.dart` and `compliance_screen.dart`), so the removal turned every proof
sheet on a rider's phone into a 404 at the door (source: 01). The revert restores the handlers
byte-identical and adds `TestRiderTaskRoutesAnswerForTheRider`. Rule: never remove a route a
deployed frontend still sends; `/scan` and `/undo` in particular must survive (the inventory
screen and the 15-minute undo use them).

**2.1.c The release merge reconciliation (`80f4a0b`).** The owner built the same phase-2
features (complaints, push registry, structured address) in parallel on `release/26.07.03`
with a contract test file `phase2_contracts_mongo_test.go`. The merge keeps exactly one
implementation per feature: integration's `complaints.go` (`cmp_...` wire id, CRM
`complaint.created`/`complaint.resolved` events, admin CRM handlers) plus the owner's
behaviours (pre-insert FindOne so a retried filing is idempotent with the index absent,
unknown category -> 400, missing ref -> 400, `listAllComplaints`/`updateComplaint` by ref,
`/ops/complaints` routes in the STORE_MANAGER/SUPER_ADMIN group so they cannot collide with the
member's `/complaints`); integration's `push.go` (DELETE, `app_version`, Expo seam) plus the
owner's strict platform check (ios/android/web); `delivery_svc.go`: `address_id` picks the door
outright, and without it the owner's certainty rule (door copied only when every candidate
address with that label is `sameDoor`); complaint/push indexes built non-fatally in `module.go`
(owner policy). The owner's spec file passes with no assertion changed; three symbols are
spelled the integration way (`repo.complaints()`, `repo.pushDevices()`, `MongoID`) (source: 06).
Behaviour changes for the deployed consumer app are listed in 06 and are all additive or
stricter-on-invalid-input.

**2.1.d The data-driven CRM router (`f7aaef1`, `crm_router.go`).** Before it, each trigger
needed a Go call site. Now every `kind: event` trigger in `crm_triggers.json` whose `event`
equals the outbox topic and whose `conditions` hold fires by config; conditions that cannot be
evaluated fail closed and are logged once per process; `crmTemplateResolvable` refuses a
half-rendered message before the day claim; the explicit Welcome Litre state machine in
`crm_engine.go` `crmRouteEvent` still runs first and calls `crmRouteGeneric` after
(`crm_router_test` proves no double fire on order.delivered with offer_pack > 0) (source: 01).
See section 4.

**2.1.e The pack size chain.** The 20 Sep rule "the size lives in `variant`, never inside the
name" holds on every hop: consumer catalog -> cart -> `POST /orders order_items[].variant` ->
`orders.go` copies verbatim -> `deliveryItemsFor` -> task wire `items[].variant` ->
`GET /stores/{id}/orders`, `/upcoming` (snake_case), `/delivery/tasks` (source: 03, trace table
of 15 hops). Two refuters then proved holes and they were fixed: (1) `crm_offers.go:673`
`crmCreateSubscription` set no `Variant`, so a Welcome Litre plan on a 1 L SKU produced
morning orders, tasks and upcoming rows with no size, reproduced live (source: 03, vote 1);
the fix (catalog `variantFor(sku)` at create, derive at `insertSubscriptionOrder`/`refreshSubOrder`
for legacy docs, replace the `:879` literal `"500ml"`) is NOT DONE: it is finding F17, listed
in section 4.6 stage D item 1. (2) Saathi rendering lost sizes in five widgets
(joined `'2 x A, 1 x B'` strings under a 2-line ellipsis); fixed by `7822ce2` and the follow-ups
in 2.3 (source: 03, 05, 09).

### 2.2 Consumer `pyaas-consumer`, `feature/consumer-revamp-phase2` (oldest first, after the 20 Sep tip `26d20c3`)

tsc is clean at every commit unless stated. Commit hashes are the consumer repo's.

| Hash | One line | Stage |
|---|---|---|
| `e98556f` | docs: consumer handoff (20 Sep) | - |
| `81a33ec` | fix(push): the device must follow whoever is signed in now (your commit, on origin) | merged at `ba994bf` |
| `f028def` | fix(orders): send the address id with the order, not only its label | phase A |
| `e705ad0` | fix(tracking): the Home poll reads orders, it does not settle them | phase A |
| `bfdfa8c` | feat(push): the app's half of push registration (EAS projectId gate, boot + sign-in) | phase A |
| `f3a18dc` | fix(auth): sign-out revokes the refresh token | phase A |
| `3808b56` | fix(notifications): mark read only the CRM rows that were unread | phase A |
| `4f4d34b` | fix(cart): the unlock copy says what the app will actually ask for | phase A |
| `b9a2e44` | feat(uploads): complaint and door photos travel as uploaded file URLs (C4) | phase A |
| `2e85d9e` | fix(tracking): no stale "Delivered" burst after an account switch | phase A |
| `f32a7de`, `59ee930` | ASCII header; forget the low-balance reminder on sign-out | phase A |
| `f745c46` .. `7fe34b7` (16 commits: `f745c46 4e3fe9d 040b620 1310a9a bb702a3 2eb65bc 8e8143f 5ed5d6b 04de6b7 f7b450d 14655d0 24f7b6c 8fae327 cb77c9b 73bc235 7fe34b7`) | phase B persistence migration: every server-owned row (wallet ledger, free-pack claims, referred-by, complaints, delivery prefs, profile, autopay mandate, trial, addresses, subscriptions + vacations, notices dedupe, consents) becomes an in-memory server copy plus an outbox | phase B (source: 02) |
| `3d94f95 236f4b2 a87e0f0 8c6ec89 4d38f7a 93d7630 226701a 72653ba 6edaf54 8882b55 5a48351` | follow-up 1: server cadences only; generation counters on the caches; sign-out clears every in-memory copy; complaint permanent rejection drops the row; delivery-prefs outbox carries changed keys only; failed mandate cancel stops deletion; wallet unlock and offer qualification become session values; no phone-side auto-pause; quality/plus never local; uploads doc; handoff | (source: 04) |
| `c0836e1 5b45db4 7f1849c f7e19be` | follow-up 2a: 401/403 on a replay keeps the row; support/rating show a rejection; sign-out zeroes the wallet store; a read resolving after sign-out is not written back | (source: 07) |
| `ba994bf` | Merge origin/feature/consumer-revamp-phase2 (`81a33ec`); `lib/notifications.ts` resolved to one exactly-once registration model (source: 07) | |
| `287bc3f 35768bb 507286d c5c98c1 f655ce8 8a622dc 7ae7075` | follow-up 2b: refused status change reloads; wallet tab shows a failed AutoPay cancel; backend-mode paused plan resumes whatever the wallet holds; vip row left in place; delivery-prefs follows the drop rule; legacy sub-status/sub-edit replays drop their op; handoff | (source: 07) |
| `359288c` | Merge origin/feature/ui-revamp (`5f92d2e`) with five conflict resolutions (2.2.c) | (source: 10) |
| `7a83faa d0039ae 8f30fc5 460732c 35b663e c7e2704` | the six seam fixes (2.2.c) and the handoff section | (source: 10) |
| `37a4f4a` | fix(referrals): sign-out forgets the session copy of the referral code (closes the LOW in 10) | after 10; tsc not re-run by this handoff, run it (section 7) |

**2.2.a Why the persistence migration.** The 20 Sep rule is that the backend is the source
of truth; before phase B the app persisted mirrors of server rows and rendered them, so a
shared phone showed the previous member's addresses, plans and balance, and a stale mirror
could re-place an order the server had cancelled. Phase B made every such table either an
in-memory session copy or an outbox (source: 02).

**2.2.b The six guards (G1-G6)** every consumer reviewer re-proves at each stage (all HOLD at
`c7e2704`, source: 10; at `7ae7075`, source: 07; at `5a48351`, source: 04; at `7fe34b7`,
source: 02):

- G1 settle sweep: `lib/subscriptionSweep.ts:111-118` orders only rows with no `backend_id` and
  a non-recurring cadence; `subscriptionFromRemote` sets `backend_id` on every server row, so no
  server plan can be placed twice by the phone.
- G2 free days: a failed `/trial/me` read returns the last same-uid answer or `TRIAL_UNKNOWN`
  (phase `free`, active false), never the local anchor.
- G3 mandate: `lib/profileApi.ts:72-88` a mandate read failure, or a live mandate whose cancel
  fails, throws before `POST /me/erasure` (the erasure cascade does not touch mandates).
- G4 subscribe never throws from an unhydrated list: `listAddresses().catch(() => [])`,
  `listSubscriptions().catch(() => [])` in the duplicate guard.
- G5 no complete-profile flash: `lib/session.ts` seeds the profile from the OTP response before
  emit; `app/_layout.tsx:159` waits for profileLoaded and setupDone.
- G6 consent freshness: hydration writes with `insertRow` directly and carries
  `occurred_at_by_type`; a hydration record is never echoed as a fresh grant.

**2.2.c The ui-revamp merge and its integration seams (`359288c` + six fixes).** Kushagra's
`5f92d2e` (Founding Family, notifications, active orders, P-DAN, legal sync, 55 files) was
analysed against the backend and phase B by two independent readers (source: 08): six
endpoints missing on the backend, five textual conflicts, three persistence regressions, a
money hazard and a double tap handler. Resolutions: `RateAppSheet.tsx` deletion accepted
(native store review); ONE tap subscription (`installTapHandler`, routes View Cart action ->
`data.href` -> identifier, cold-start read once per process); both session effects and his
AppState hooks kept in `app/_layout.tsx`, `lib/referrals` imported at boot so its mirror
handler exists before the first drain; `lib/referrals.ts` server-first with no local write in
backend mode (memory-cached code, `referral-apply` mirror op targeted by the code itself);
`lib/subscriptions.ts` duplicate guard reads `listSubscriptions()` and maps the server's 409
`DUPLICATE_SUBSCRIPTION` to the same error. Seam fixes: `lib/taglines.ts` `offersOn` reads the
server's marketing consent (F1) and the order list with `fetchOrders` not `listOrders`, which
runs the wallet settle sweep (F2, the money hazard: `taglines.ts:116` ran the sweep on every
backgrounding); sign-out goodbyes live in `session.signOut` (F3); message-preferences unknown
record shows the channels off (F4); the sign-out KEEP filter spares only `parag:vip` (F5)
(source: 10).

### 2.3 Saathi `pyaas-saathi`, `integration/delivery` (oldest first, after the 20 Sep tip `c18e50a`)

| Hash | One line | analyze / test |
|---|---|---|
| `6348c5e` | docs: Saathi handoff (20 Sep) | 59 pass |
| `9239553` | fix(onboarding): a failed KYC upload must not fabricate a photo URL (owner, on release) | merged at `e7901be` |
| `02f34db` | fix(store): item adjust keyed by product id, not by name | clean / 63 |
| `92042b6` | fix(store): new-order alert diffs task ids, not the list length | 69 |
| `51e4936` | feat(rider,store): read the structured door and group the morning round by it | 77 |
| `8f31455` | fix(rider): fence the finish only when the pin is the customer's own (C3) | 80 |
| `4deb0fa` | fix(rider): cash collect offers the manual path when no OTP can be sent | 84 |
| `5f19ffb` | fix(rider,store): DROP handover reads "Leave at the door" | 86 |
| `ef41d7b` | feat(store): Upcoming days shows tomorrow's previews from the upcoming route (C1) | 90 |
| `0bf6f8b`, `61118c4`, `ba5fc5d` | drop the dead per-task calls (keep undo); catalog comment; ASCII header | 90 (source: 01) |
| `7822ce2` | feat(rider,store): the pack size is on every row a rider delivers from and a manager packs from (`PackItem`, `PackLines`, `PackSizePill`, e2e harness `test/e2e/` + `tool/e2e/`) | 102 pass / 1 skip (source: 03) |
| `0f7356c` | feat(rider): the route Overview row shows each pack with its size | 103 / 1 |
| `a7b9af8` | fix: the size pill is drawn even when the name already states it (the ellipsis path) | 105 / 1 |
| `3458ce9` | fix(store): Inventory keeps stock per pack size (`StoreSku` id = name plus variant) | 114 / 1 |
| `114a455` | test(e2e): one live test per surface (fixes the vacuous pushed-route assertion found in 03) | 114 / 11 |
| `792c7a3` | fix(rider): an absent geoExact keeps the phone-side fence of release 26.07.03 | 115 / 11 |
| `5d86bdc` | fix(rider): OTP_REQUIRED reopens the OTP box; RING_BELL always reads "Ring the bell" | 117 / 11 |
| `d90af82` | docs: handoff brought up to date; orphaned ComplianceStep removed | 117 / 11 (source: 05) |
| `e7901be` | Merge origin/release/26.07.03 (`9239553`) | - |
| `9119b26` | fix(rider): the Verify-inventory sheet prints the pack size as its own pill | 122 / 11 |
| `768ead7` | fix(store): deriveStock takes the exact size row first (4 documented passes, `unmatched` bucket) | 124 / 11 |
| `f978b6e` | chore(store): ASCII source in the inventory row, store.dart doc comments, e2e headers | 124 / 11 |
| `01f3692` | fix(store): each pack size prices at its own MRP when the stock line carries one | 127 / 11 |
| `c31bccb` | fix(rider): a task with no delivery pin says so instead of "too far" | 128 / 11 (source: 09) |

The 11 skipped are the opt-in live e2e legs in `test/e2e/pack_volume_live_e2e_test.dart`
(`skip: !kE2E`). Why the geofence went tri-state (`792c7a3`): the deployed backend never sends
`geoExact`, never measures a fence itself and refuses `geofence_ok:false`; a build that fenced
only on `geoExact == true` would have had NO fence against production. Absent = the old 300 m
phone fence, `false` = no fence (store fallback pin), `true` = fence, server measures too
(source: 05).

---

## 3. Contracts now in force between the apps and the backend

Paths are under `/api/v1/consumer` unless written in full. Backend file:line were taken at
`8d24915`; the three later CRM commits touch only `crm_*` files and `delivery_svc.go`, so
lines in those files may have moved by a few dozen. Consumer at `37a4f4a`; Saathi at `c31bccb`.

| Id | Contract | Backend | Consumer / Saathi side |
|---|---|---|---|
| C1 | `GET /stores/{storeId}/upcoming` -> `{data:[{order_id, delivery_date, delivery_window, lane, consumer_name, phone, address_label, address_text, society, society_id, tower, floor (null ok), unit, items[{product_id,name,variant,qty}], source: subscription_preview or scheduled_order}]}`; read-only, skips orders that already have a task; STORE_MANAGER group | `internal/modules/consumer/module.go:365`; `upcoming.go:32-56` | Saathi `lib/api/store_console_api.dart:165-167` (`upcoming`), `:95` (`parseUpcomingRows`, 404/405/501 -> `[]`); rendered read-only under "Upcoming days" in `store_home.dart` (source: 01) |
| C2 | `DELETE /push/register {token}` -> 204, deletes only `{token, consumer_id: caller}` | `module.go:212`; `push.go:118` (service), `:172` (handler) | consumer `lib/notifications.ts:424` `unregisterPush`, called from `lib/session.ts` signOut while the access token is still valid; `registeredFor` latch at `lib/notifications.ts:328-409` (source: 01, 10) |
| C3 | Delivery task carries `geoExact` (bool, always emitted) and `societyId` (omitempty) plus `society/tower/floor/unit`; the server applies the phone's `geofence_ok` verdict and its own 300 m measurement only when `GeoExact` | `delivery.go:131` (`GeoExact json:"geoExact"`), `:121` (`SocietyID json:"societyId,omitempty"`); `delivery_svc.go` deliver handler (fence only when `d.GeoExact`) | Saathi `lib/api/delivery_api.dart:172,221` (`geoExact` tri-state via `_tri(w['geoExact'] ?? w['geo_exact'])`), `:167,209` (`societyId`); `lib/screens/rider/delivery/deliver_swipe_screen.dart:96` `_fenced => geoExact != false`, `:102-105` `_tooFar`, `:71` 300 m; `RiderStore.groupByDoor` (source: 01, 05) |
| C4 | `POST /uploads/presign {kind: complaint_photo or door_photo, content_type}` -> `{upload_url, method: "POST", headers{Authorization, X-Bz-File-Name, Content-Type, X-Bz-Content-Sha1}, file_url: "/api/v1/uploads/view/..."}`; 503 `MEDIA_UNAVAILABLE` until `B2_*` set. Method is POST because B2's `b2_upload_file` accepts only POST | `module.go:216`; `uploads_presign.go:44-47`, `:88` | consumer `lib/uploads.ts:11-16` (doc), `:37` (`UPLOADS_VIEW_PREFIX`), method taken from the response with PUT fallback; callers `lib/complaints.ts` (`photo_uri`) and `components/AddressCapture.tsx` (`door_photo_uri`) (source: 01, 07) |
| C5 | Expo push transport: `internal/platform/push/expo.go` posts `[{to,title,body,data,channelId,sound,priority}]` to `https://exp.host/--/api/v2/push/send`; `data = {href, trigger_id, category}`; `DeviceNotRegistered` prunes the token; plugged only when `EXPO_PUSH_ENABLED=true` | `crm_push.go:50-53` (env), `:173` (data); `crm_channels.go:288` (`m["push"]`) | consumer reads only `data.href`: `lib/notifications.ts:252` `hrefFromResponse`, `:285` `installTapHandler`; Android `channelId` from the app's `CHANNELS` (source: 01, 10) |
| C6 | Lifecycle events into the CRM outbox: `order.confirmed {order_id, labelled_product, promotional_only, eta}`, `order.dispatched {order_id, labelled_product, partner, eta_min}`, `order.delivered {order_id, offer_pack, promotional_only, labelled_product}`, `order.failed {order_id, labelled_product, reason}`, `complaint.created {complaint_id, ref, category, order_id}`, `complaint.resolved {complaint_id, ref, resolution}`, `rating.submitted {order_id, rating}`; template params UPPERCASE | `delivery_svc.go:107, :1002, :1027, :1055`; `complaints.go:188, :255`; `orders.go:447`; `crm_router.go:24-27` (`crmLifecycleTopics`) | consumer consumes the result through `GET /crm/inbox` (`lib/crm.ts:74`) and `POST /crm/inbox/{id}/read` (`:85`); `lib/notificationCenter.ts` `dropNoticesTheServerSent` pairs local notices with D-01/D-02/D-06 rows (source: 01, 02) |

Other seams verified in the reports:

- `POST /orders` sends `address_id` (`lib/api.ts:594`); backend `orders.go` reads it and
  `delivery_svc.go` picks the door outright, label-only orders fall to the certainty rule
  (source: 06, 07).
- `POST /complaints` (`lib/complaints.ts:134`) / `GET /complaints` (`:272`) <-> `module.go:206-207`;
  operator queue `GET /ops/complaints`, `PATCH /ops/complaints/{ref}` at `module.go:355-356`
  (STORE_MANAGER/SUPER_ADMIN; Saathi has no caller yet, section 6a); website admin at
  `/consumer/admin/crm/complaints` (source: 06).
- `POST/GET /users/me/consents` (`lib/consentSync.ts:125`, `:16`) <-> `module.go:230-231`;
  `lib/taglines.ts:93` reads the server consent for the Offers gate (source: 10).
- `POST /auth/otp/verify` always returns `profile.full_name` for a returning member
  (`service.go issueTokens`, `models.go:35`), which G5 depends on (source: 07).
- `POST /subscriptions` -> 409 `DUPLICATE_SUBSCRIPTION` is what the consumer maps at
  `lib/subscriptions.ts:287` (`:306` const). The backend half exists only on
  `feature/founding-referrals` (`d2f8df9`, `subscription_duplicate_test.go`); until that branch
  is merged the production server still mints a twin (section 5).
- The owner's phase-2 contract spec `phase2_contracts_mongo_test.go` (19 tests) is the
  authority for complaints, push and structured address; keep it green (source: 06).

**Founding Family / referral shapes the consumer app on `5f92d2e` expects** (the app never
types a price or a status; these are the contract the backend is being built to; source: 08, 10):

```
GET  /consumer/founding-family                      lib/foundingFamily.ts:92
     -> { price_month, member: null | { status: waiting|active|stopped, farm_id,
          line_number, referral_code, joined_at, next_bill_date },
          farms: [{ id, name, farmer, place, note, photo_url, unlocks_at, claimed,
                    status: filling|unlocked, unlocked_packs }],
          savings?: { level1_per_litre, level3_per_litre, delivery_fee } }
     any non-2xx -> the app says "Founding Family opens in the app soon"
POST /consumer/founding-family/join { farm_id }     lib/foundingFamily.ts:132
     -> { member }; error codes WALLET_SHORT (shortfall in the message or a field),
        FARM_UNLOCKED, ALREADY_MEMBER
POST /consumer/founding-family/stop                 lib/foundingFamily.ts:143  -> { member }

GET  /consumer/referrals/code                       lib/referrals.ts:79   -> { code }
     mint with the app's derivation for existing members: h = h*31 + charCode over the
     consumer id, unsigned 32-bit, ("PG" + base36(h).toUpperCase() + "XXXX").slice(0, 6)
GET  /consumer/referrals                            lib/referrals.ts:121
     -> [{ id, name, status: pending|credited, reward_amount, created_at }]
POST /consumer/referrals/apply { code }             lib/referrals.ts:160, :178
     2xx = linked; a 4xx other than 401/403/408/429 is final (app shows "not recorded");
     5xx/network is replayed from the mirror queue, so it must be idempotent per (consumer, code)
```

---

## 4. CRM: how the engine works now

### 4.1 Pipeline (inert unless `CRM_ENABLED=true`; `crm_offers.go:71`)

1. **Outbox.** An emitter calls `emitCRMEvent(ctx, topic, consumerID, payload)`; it inserts a
   row into `crm_events` (`crm_engine.go:51`, `:289`). Emitters are double-gated on
   `crmEnabled()` at the call site and inside `emitCRMEvent` (source: 01).
2. **Worker.** `crmWorker` (`crm_engine.go:760`) ticks every 60 s, IST-anchored, drains the
   outbox with a lease (PROCESSING older than ten minutes is handed back), and runs the
   schedules (`crmFireDueSchedules`, `crm_schedule.go:119`).
3. **Router.** `crmRouteEvent` (`crm_engine.go:832`) runs the explicit Welcome Litre cases, then
   `crmRouteGeneric` (`crm_router.go`) fires every `kind: event` trigger whose `event` equals the
   topic, whose `category` is not `internal`, that has a template, and whose `conditions` hold.
   Conditions fail closed and are logged once per process. A trigger with a non-zero `delay`
   (A-01 PT2H, C-03 PT1H, E-07 PT4H, A-05, C-07) is written to `crm_schedules` due at
   event-time + delay, unique per (trigger, event); the scheduler re-evaluates the conditions
   and SKIPS an event whose order was cancelled meanwhile (`8d24915`). Triggers listed in
   `meta.awaiting_event` (22 at HEAD, each with its reason) are skipped by the router
   (`7cbb043`). The detector for `delivery.delayed` runs on the same tick (`crm_detectors.go`,
   `804205f`).
4. **Template gate.** `crmFireTrigger` refuses a message with an unresolved `[TOKEN]` before
   any claim, so a later tick with the value present can still fire.
5. **Claim.** Exactly-once per `(trigger_id, consumer_id, ist_day, scope_key)` in
   `crm_dispatch_log` (`crm_engine.go:52`, `crmClaimDispatchScoped` `:469`). `scope_key` is the
   order or complaint id for event triggers, `""` for scheduled and per-day triggers, so two
   instant orders on one day each get their own D-01 and D-06 while W-07 and the sweeps claim
   once a day (`8edd9e1`). `ensureCRMClaimIndex` migrates legacy rows and keeps the legacy
   index in force if the new build is refused.
6. **Transports.** `crmDispatchWith` walks the trigger's channel order through `crmTransports()`
   (`crm_channels.go:282` sms, `:285` whatsapp, `:288` push; inapp always writes the inbox row).
   An unplugged channel is logged once per dispatch and the row records the intended primary
   (`c722996`). SMS never attempts a trigger absent from `CRM_DLT_TEMPLATE_IDS` (unregistered
   content is a TRAI violation) and refuses promotional triggers outright; WhatsApp is inert
   unless both `CRM_WA_TOKEN` and `CRM_WA_PHONE_ID` are set and the trigger is in
   `CRM_WA_TEMPLATE_NAMES`; push is bound per dispatch to the member's tokens from
   `consumer_push_devices` (provider expo, newest 5), needs `EXPO_PUSH_ENABLED=true`, and prunes
   `DeviceNotRegistered` tokens (source: 01). `ai_call`, `admin_console`, `email`
   resolve to nothing (`crm_channels.go:95`); `human_call` is the operator call-back queue
   since `e76bd1a` (`crm_callbacks.go`, `GET /consumer/admin/crm/callbacks`).
7. **W-01 / W-07 mechanics.** W-01 is sent by the worker, not inline on the enrol request
   (`a1b8623`); pack 2 expires only once W-07 is on record as SENT (`8afae8f`); the manual
   operator send dedups by content (`e9c609c`).

### 4.2 What leaves the building with today's env

With the production env as the 20 Sep handoff found it (`CRM_DLT_TEMPLATE_IDS` maps only
W-01 and W-07, no `CRM_WA_*`, `EXPO_PUSH_ENABLED` unset, no EAS projectId in the app so zero
push tokens exist): exactly two SMS can reach a customer, **W-01** (enrolment) and **W-07**
(offer expiring). Every other trigger, including the whole order lifecycle D-01/D-02/D-06 and
the complaint triggers that now have emitters (C6), lands in the in-app inbox only and waits
for the member to open the app (20 Sep handoff section 5; the emitters are new, the channel
state is not). The per-trigger status table is `docs/CRM-AUDIT-2026-09-24.md` section 1 (as of
`bf083ef`, before the five completion commits; re-derive the column STATUS after section 4.6 is
finished). `docs/CRM-MESSAGES.md` (the rendered-body matrix, with each trigger's status today) now exists.

### 4.3 Env keys (declared in `render.yaml` and `.env.example`)

| Key | What it does | Where read |
|---|---|---|
| `CRM_ENABLED` | master switch; anything but `"true"` = pre-CRM binary (no worker, no inbox, no sends) | `crm_offers.go:71` |
| `CRM_MSG91_AUTHKEY` | MSG91 Flow API auth key for campaign SMS (separate from the login `MSG91_AUTHKEY`) | `crm_channels.go:377` |
| `CRM_MSG91_SENDER` | DLT header; MSG91 takes the header from the flow registration, so empty is not the blocker | `crm_channels.go:378` |
| `CRM_DLT_TEMPLATE_IDS` | JSON trigger id -> DLT template id (plain or `{"en":..,"hi":..}`); a trigger absent here never attempts SMS | `crm_channels.go:381` |
| `CRM_WA_TOKEN`, `CRM_WA_PHONE_ID` | WhatsApp Cloud API; inert unless both set | `crm_channels.go:530-531` |
| `CRM_WA_TEMPLATE_NAMES` | JSON trigger id -> approved WA template name | `crm_channels.go:534` |
| `EXPO_PUSH_ENABLED` | exactly `"true"` plugs the push transport | `crm_push.go:50` |
| `EXPO_ACCESS_TOKEN` | optional Bearer for Expo push security | `crm_push.go:50` |
| `CRM_MSG91_BASE_URL` | dry-run origin for BOTH MSG91 callers (CRM flow channel and the login/delivery OTP client); replaces scheme://host only, path/method/headers/body unchanged | `internal/platform/sms/msg91.go:57` |
| `CRM_WA_BASE_URL` | dry-run origin for the Graph API (`/v19.0/<phone_id>/messages`) | `crm_channels.go:532` |
| `EXPO_PUSH_BASE_URL` | dry-run origin for Expo (`/--/api/v2/push/send`) | `crm_push.go:53` |
| `CRM_PACK2_MIN_PAISE` | pack-2 minimum recharge threshold | `crm_engine.go:260` |
| `ADMIN_API_KEY` | website admin CRM key auth (32+ chars; unset = off; boot logs which door is open) | `admin_crm.go:55`, `module.go:314` |

Unset, every `*_BASE_URL` resolves to the real endpoint and the binary is byte-identical
(`d6145f9`).

### 4.4 Capture seams for the dry run

Point the three `*_BASE_URL` keys at one local stub (for example `http://127.0.0.1:9009`);
the stub receives MSG91 at `/api/v5/flow/` (and OTP at `/api/v5/otp`), WhatsApp at
`/v19.0/<CRM_WA_PHONE_ID>/messages`, Expo at `/--/api/v2/push/send` with exactly the
production payloads. For a channel to reach the stub it must be PLUGGED: set dummy
`CRM_MSG91_AUTHKEY`, a `CRM_DLT_TEMPLATE_IDS` map for the triggers you want to see over SMS,
dummy `CRM_WA_TOKEN`/`CRM_WA_PHONE_ID` and `CRM_WA_TEMPLATE_NAMES`, `EXPO_PUSH_ENABLED=true`
and a registered device row (the consumer harness registers one when it has an Expo token; on
a bare jest harness insert a `consumer_push_devices` row by hand). Tests for the seams:
`crm_dryrun_seams_test.go` (source: `d6145f9` body).

### 4.5 How to add a message

1. Add the trigger and its template to `internal/modules/consumer/crm_triggers.json`:
   `kind: "event"`, `event: "<topic>"`, `category` (lifecycle vs promotional decides SMS
   eligibility and push priority), `channels` in order, optional `conditions`, optional
   `delay` (ISO 8601 duration), a template whose tokens are UPPERCASE `[LABELLED_PRODUCT]`,
   `[ETA]`, `[ETA_MIN]`, `[AMOUNT]`, `[REF]`, `[SLA]`, `[LINK]`, `[SUPPORT_NUMBER]`, `[DATE]`.
2. If the topic is new: add the `emitCRMEvent` call site (gated on `crmEnabled()`), add the
   topic to `crmLifecycleTopics` in `crm_router.go`, and make `crmEventParams` fill every
   token from the payload. `crm_tokens_test.go` walks every reachable event trigger and fails
   on an unfillable token (source: 01).
3. Add a test in the style of `crm_router_test.go` (fires once, conditions, no double fire).
4. For SMS: register the DLT template, add its id to `CRM_DLT_TEMPLATE_IDS`; for WhatsApp:
   the approved name to `CRM_WA_TEMPLATE_NAMES`. Without these the message is inbox (and push)
   only, by design.

### 4.6 STOPPED: CRM completion stage, exactly where it stands

> **Update (24 Sep, later the same day): items 6 to 9 and stage D items 1 to 5 are DONE**
> on `integration/delivery`, each with a test that failed before its fix: `f7384d9` (the
> top-up message fires only once the money is in; every credit path proven),
> `e76bd1a` (human_call queue, E-05 reaches a person), `22759d4` (W-03b consent chain
> proven), `3b8851f` (F17 pack size on Welcome Litre plans), `8f369e8` (F18 address
> replay), `cb5ede3` (F19 inbox refs; consumer `ee2a07f` pairs by them), `71286d2` (F20
> inventory sizes), `482133c` (the matrix test), `eb9d9ae` (20 Sep handoff
> corrections) and `docs/CRM-MESSAGES.md`. What is still open from this section is the
> independent review (6a.0.c). The text below is kept as it was written.

Planned as nine items plus a "stage D" of five, then two independent reviewers (compat and
CRM-matrix). Stopped by the owner after item 5. Every landed commit passed the full suite
before it was pushed; the handoff author re-ran the suite at `81e2ff6` (green, consumer
package 79.3 s). **None of the five commits has had an independent review**: treat them as
implementer-verified only and give them the review described in 6a.0.c.

Done (all on `integration/delivery`, all on origin):

| Item | Commit | What to know |
|---|---|---|
| 1 per-order claim scope | `8edd9e1` | `crm_dispatch_log` unique claim is now `(trigger_id, consumer_id, ist_day, scope_key)`; `ensureCRMClaimIndex` migrates legacy rows and keeps the old index if the new build is refused. |
| 2 delay honoured | `8d24915` | `crm_schedule.go`; A-01 PT2H, C-03 PT1H, E-07 PT4H, A-05, C-07 now fire late, not on the next tick. |
| 3 emitters | `804205f` | see the 2.1 table row; `crm_detectors.go` holds the `delivery.delayed` detector. |
| 4 conditions + awaiting_event | `7cbb043` | `meta.awaiting_event` (22 triggers) is the list of messages that cannot fire until a product event or a runner exists; the reasons are in the json. |
| 5 D-09 delivery failed | `81e2ff6` | the only NEW template added; DLT registration needed before it can go over SMS. |

Not done (in the order they were planned; each with where to start):

- **Item 6, `human_call` transport.** E-05 (`complaint.type == 'quality'`, critical) routes
  primary `human_call`, which resolves to nothing (`crm_channels.go:95`), so a safety complaint
  reaches nobody. Build a `crmTransport` named `human_call` that inserts a row into a new
  `crm_callbacks` collection `{consumer_id, trigger_id, reason, payload, created_at, status:
  open}` and returns SENT, and expose `GET /consumer/admin/crm/callbacks` (read-only, same
  auth as `/crm/dispatch-log` in `admin_crm.go`). E-05 has `template: null`; the router skips
  template-less triggers (`crm_router.go:47`), so give it a minimal inbox template or special-
  case `human_call` triggers to bypass the template gate. Acceptance: file a `quality`
  complaint in a Mongo test, drain the worker, assert one callback row and one dispatch row
  with `intended: human_call`.
- **Item 7, W-03b.** Never sends today: it is `promotional` and gated on a consent record the
  app writes only from the marketing toggles (see the audit row W-03b and section 4 gap list).
  Owner decision needed (keep consent-gated, or reclassify per the config's own note); then
  either wire `readServerConsents`-backed consent into the gate or change the category.
- **Item 8, the matrix test.** `crm_tokens_test.go` proves tokens resolve; there is no test
  that FIRES every non-awaiting trigger end to end. Write `crm_matrix_test.go`: for each
  trigger in the json not in `meta.awaiting_event`, emit its topic with a realistic payload,
  drain, assert an inbox row (or the documented SUPPRESSED reason); the test must fail when
  someone adds a trigger without an emitter and without the `awaiting_event` mark.
- **Item 9, `docs/CRM-MESSAGES.md`.** The table the founder asked for: id, topic, when it
  fires, channels, env needed to leave the building, status; plus "how to add a message" in
  five steps (section 4.5 is the draft of that).
- **Stage D (none started):**
  1. Welcome Litre plan pack size: `crm_offers.go` `crmCreateSubscription` (~:673) sets no
     `Variant`; the `:879` literal `"500ml"`; derive at `insertSubscriptionOrder` /
     `refreshSubOrder` when `sub.Variant == ""` (legacy documents). Reproduced live: a 1 L
     Welcome Litre plan reaches both consoles as "1 x Full Cream Milk - Parag Gold" (source: 03).
     Add `variantFor(sku)` to `catalogPriceIndex` (`catalog_price.go:92-113`; `products_seed.json`
     carries the variant). Test: enrol a `gold-1l` plan, sweep, assert the task item has `1L`.
  2. `POST /addresses` idempotency: same consumer + same label (trimmed, case-insensitive) +
     pin within 5 m (or identical text with no pin) returns the existing row with the same
     status and shape the app reads (`lib/api.ts addAddress` / `addressFromRemote`). Guards the
     consumer's offline-first-session replay duplicate (section 6c).
  3. Inbox rows carry `order_id` and `complaint_ref` (optional, omitempty) from the dispatch
     payload so `lib/notificationCenter.ts dropNoticesTheServerSent` matches exactly instead of
     by class within 24 h.
  4. Rider Verify-inventory lines carry `variant` (`rider_ops_route.go:649-669` sends the size
     only as `unit`); fix the fallback at `:641-643` that keys by name with an empty variant when
     the order cannot be read (two sizes merge into one line). Saathi already reads
     `variant ?? unit` (`lib/models/rider.dart`, `9119b26`).
  5. `docs/HANDOFF-CODEV-2026-09-20.md` section 6: the five rider routes are restored (not
     removed); point the CRM section at the audit file; document the dry-run origin keys and
     the two unique indexes with their boot guards.
- **The two reviews** (compat: every route and key both app branches send, five rider routes
  answer, unique indexes cannot crash boot, `CRM_ENABLED` unset = zero change; CRM-matrix:
  independently verify every trigger status, two instant orders same day both get D-01, a
  top-up through EACH credit path produces the message, a delayed trigger skips a cancelled
  order, the matrix test fails on an emitter-less trigger).

---

## 5. STOPPED: Founding Family, referrals, 12-noon lock, duplicate guard, stale orders

Built on `feature/founding-referrals` (worktree `D:/dev/pyaas/be-features`, own mongod on
`:27018` because the suite uses fixed database names), reviewed independently, **not fixed,
not merged**. Owner decisions that shaped it (24 Sep ~02:00): the lock is 12 noon IST the day
before delivery (the app copy at `app/subscriptions.tsx:152` and `components/StartDatePicker.tsx:102`
already says so; the worker locked at IST midnight); Founding Family is built to the frontend
(`5f92d2e` + `pyaas-app-spec.md` in that commit) rather than deferred; `createSubscription`
gains the server-side `DUPLICATE_SUBSCRIPTION` 409; the sweep closes locked morning orders
whose day passed.

### 5.1 What is on the branch (source: 11)

| Commit | Content |
|---|---|
| `d2f8df9` | feat(consumer): referrals, Founding Family, noon cut-off, duplicate guard, missed orders. 29 files, +1308/-155 non-doc (31 files, +4155/-155 with tests and docs). Landed as one commit because `module.go`, `subscriptions.go`, `admin_crm.go` and `config.go` carry pieces of several steps. |
| `e5d12ab` | docs(handoff): section 9 (9.1 referrals, 9.2 Founding Family, 9.3 the noon lock, 9.4 DUPLICATE_SUBSCRIPTION, 9.5 stale locked orders, 9.6 founder decisions) in `docs/HANDOFF-CODEV-2026-09-20.md` on that branch, plus the noon label in `docs/HANDOFF-WEBSITE-DELIVERY-CRM.md`. Read it: it is the detailed design note. |

Suite on the branch: every package ok, consumer package 83.8 s (implementer) and 89.6 s
(reviewer, own mongod on `:27019`).

1. **Referrals** (`internal/modules/consumer/referrals.go`, `referrals_test.go`). Routes in
   the authenticated consumer group: `GET /referrals/code` -> `{code}`, `GET /referrals` ->
   `[{id, name, status: pending|credited, reward_amount, created_at}]` (`[]` when empty),
   `POST /referrals/apply {code}` -> 200 `{id, code, status, reward_amount, created_at}`;
   404 `REFERRAL_CODE_NOT_FOUND`, 422 `SELF_REFERRAL`, 409 `ALREADY_REFERRED`. The code is the
   app's `codeFromUid` byte for byte (JS-computed vectors pinned by
   `TestReferralCodeMatchesTheAppDerivation`), minted lazily into the account's `referral_code`;
   a code that was shared before it was stored still resolves by derivation. Reward:
   `REFERRAL_REWARD_PAISE` (default 10000 = Rs 100) credited to the REWARDS bucket of BOTH
   sides from `syncOrderDelivered` on the referee's first delivered order, exactly once (ledger
   refs `referral:<id>:referrer` / `:referee`), status `pending` -> `credited` (the app's
   `ReferralStatus` branches on `credited`; "rewarded" is only the CRM event name). CRM events
   `referral.applied`, `referral.rewarded` are emitted but have no json trigger yet.
2. **Founding Family** (`founding.go`, `founding_test.go`; `cmd/seed/founding_farms.json`
   embedded by `cmd/seed/main.go:35` and seeded by `consumer.SeedFoundingFarms` at `:171`:
   four placeholder farms with a `founder_todo` marker; `crm_triggers.json` FF-01..03 with
   T-FF01..03, counts 57/49 on the branch; `catalog_price.go` `priceForMember` and
   `member_price`; `catalog.go`, `orders.go`; `dolibarr/client.go` multiprices;
   `dolibarr_sync.go` level 3 -> `member_price`, FOUNDING-99 / DELIVERY-FEE services;
   `admin_crm.go` `GET/PUT /consumer/admin/founding/farms`). Routes: `GET /founding-family` ->
   `{price_month, member, farms, savings}` (404 `NOT_AVAILABLE` when closed or no farms, which
   the app renders as "opening soon"); `POST /founding-family/join {farm_id}` -> `{member}` with
   422 `WALLET_SHORT` (+ `shortfall` field and "short by N rupees" message the app regex reads),
   409 `FARM_UNLOCKED`, 409 `ALREADY_MEMBER`, 404 `NOT_FOUND`; `POST /founding-family/stop` ->
   `{member}` (idempotent). Error bodies use the consumer module's flat `{code, message}`
   (what `apiClient` reads), not the operator envelope. Joining debits `price_month` from the
   wallet server-side exactly once (ledger `FOUNDING-99`), assigns `line_number`, status
   `waiting`; when `claimed` reaches `unlocks_at` the farm flips to `unlocked` and every waiting
   member becomes `active`. Pricing: no discount on Parag ever; PYAAS milk lines
   (`pyaas-*`, `dol-pys-*`, `PYS-*`) at level 3 for active members (ERP `multiprices_ttc["3"]`,
   else level 1 minus `FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE`), delivery fee 0 for members;
   reviewer-verified on a mixed order (34->33, 20->20, 139->137, Parag 35 and 29 unchanged).
   Config keys added in `internal/config/config.go`: `REFERRAL_REWARD_PAISE`,
   `FOUNDING_PRICE_MONTH_PAISE`, `FOUNDING_DELIVERY_FEE_PAISE`,
   `FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE`, `FOUNDING_SAVINGS_SKU`, `FOUNDING_BILL_RETRY_DAYS`,
   `FOUNDING_FAMILY_CLOSED`, `FOUNDING_PYAAS_MEMBERS_ONLY` (spec 5.1, default off),
   `FOUNDING_PYAAS_NONMEMBER_FEE` (spec 5.3, default off, "needs your yes"). Not yet in
   `.env.example` / `render.yaml` (add them).
3. **Noon lock** (`subscriptions.go`: `lockHourIST`, `lockedThroughDay`, `firstEditableDay`,
   `changed_at`, `next_delivery_date` on the plan; `upcoming.go` `storeUpcomingAt`;
   `subscriptions_noon_test.go`; five existing tests re-dated). A plan created or resumed after
   noon starts the day after tomorrow; `next_delivery_date` is returned so the app's picker
   can read it (today it still defaults to tomorrow at any hour, an app follow-up for Kushagra).
4. **`DUPLICATE_SUBSCRIPTION`** 409 in `createSubscription` (same `product_id` + normalised
   variant, non-cancelled plan; message names the plan; `subscription_duplicate_test.go`).
5. **Missed locked orders**: `closeMissedSubscriptionOrders` in the sweep marks the order
   `cancelled` with `cancelled_by: missed`, the task `FAILED`, emits `order.failed` reason
   `missed`, moves no money.

### 5.2 Contract check by the reviewer (source: 11)

Every call in `lib/foundingFamily.ts` and `lib/referrals.ts` on `5f92d2e` is served with the
keys, status codes and error codes the app reads (file:line list in report 11,
`contract_check`). The catalog keeps every existing key; `member_price` / `memberPrice` are
added on PYAAS lines only and ignored by the current app build; `POST /orders` bills
`priceForMember`.

### 5.3 Confirmed defects, NOT fixed (the fix pass was stopped before its first commit)

Each was reproduced by the reviewer on the real service (source: 11). Fix them on
`feature/founding-referrals` before merging; the planned fix is stated with each.

1. **HIGH - billing retries per hour, not per day.** `founding.go:794-835` increments
   `bill_attempts` on every failed debit and `foundingBillingWorker` (`:839-852`) ticks hourly,
   so a short wallet stops the membership after ~3 hours; spec 5.6 promises 3 billing DAYS and
   the message says "we will try again tomorrow". `TestFoundingBillingRolloverAndRetryThenStop`
   drives one call per day and never sees it. Fix: count one attempt per IST bill day
   (`last_attempt_date` guard), stop after the third distinct day. Test: hourly ticks on one
   day keep attempts at 1; three days stop on day 3.
2. **MEDIUM - stop then re-join inside the paid month forfeits the perks and charges Rs 99
   again.** `founding.go:661-681` re-join `$unset`s `perks_until` and sets `waiting` on the new
   farm, so `foundingStanding` (`480-492`) returns inactive at once (level-1 prices and the fee
   return) and a second FOUNDING-99 debit lands (reproduced: 500 -> 401 -> 302). With
   `FARM_UNLOCKED` at `624-626` a stopped member can never re-join the farm they paid for once
   it unlocked, while the app offers "Re-join" to every stopped member. Rule to implement: a
   re-join before `perks_until` keeps `perks_until`, charges nothing, restores status (`active`
   if the farm is unlocked, `waiting` otherwise) and may target the farm the member previously
   held even if unlocked; a join after `perks_until` is a fresh Rs 99 join. Owner to confirm.
3. **LOW - the LOCK step honours edits made in the 15 minutes after noon.**
   `subscriptions.go:975-1000` re-reads the live plan without a
   `subChangedBefore(lockMomentFor(day))` check (the catch-up step at `~1055` has it), so a
   pause or qty edit stamped 12:05 reaches tomorrow on the 12:15 tick. Fix: when the plan
   changed after the day's lock moment, lock the preview as it stands. Test: pause at 12:05,
   sweep 12:15, tomorrow still delivered as previewed.
4. **LOW - duplicate line numbers after a waiting member stops.** `releaseFarmSeat`
   (`founding.go:344-350`) decrements `claimed` and the next join takes `line = claimed` after
   `$inc` (`:644`): A#1 B#2 C#3, B stops, D joins -> D is #3 while C is #3. The number is the
   member's identity on the card. Fix: a monotonic per-farm `next_line` counter that never goes
   down; `claimed` stays the live seat count.
5. **LOW (test gap, money guard) - the founding and referral indexes are not built in the test
   world.** `newChainWorld` (`fullchain_e2e_test.go:79-88`) builds `ensureIndexes` and the day
   index only; without the `consumer_id` unique index, 5 concurrent joins made 5 member rows, 5
   seats and 5 Rs 99 debits (the pre-check at `founding.go:617-623` is racy). With the index
   (built non-fatally at `module.go:~80`) the same run gives 1 row, 1 seat, 1 debit. Fix: add
   `ensureFoundingIndexes` / `ensureReferralIndexes` to the test world, add a concurrent-join
   test, and make `join` (and `referrals/apply`) answer 503 `FOUNDING_UNAVAILABLE` when the boot
   flag says the unique index failed to build, so money is never guarded by the pre-check alone.
6. **Not built - noon cut-off for one-off morning orders.** `orders.go:380-392` still accepts
   `delivery_date = tomorrow` at any hour and mints the task at once, while the app says
   "Order by 12 noon, delivery by 7 AM" on the product page and picker. Fix: after 12:00 IST a
   morning one-off for tomorrow is refused with 422 `CUTOFF_PASSED`, message "Order by 12 noon
   for tomorrow; next available <date>" and a `next_delivery_date` field; instant lane untouched.
   Test 11:59 accepted, 12:01 refused.

Observations from the same review (no code change decided): after noon the app's start-date
picker defaults to tomorrow and the home strip promises tomorrow while the server first
delivers the day after (app follow-up: read `next_delivery_date`); the basket shows level-1
prices + Rs 15 fee while the server bills level 3 + Rs 0 for members (shown != charged, in the
member's favour, until spec screen 9 ships); the Rs 99 seat and the monthly bill are debited
promo-first (`service.go:730-734`), so a Rs 100 referral reward can pay a Founding seat
(founder decision whether REWARDS money may fund membership).

Still open from the build itself (`docs/HANDOFF-CODEV-2026-09-20.md` section 9.6 on the
branch): spec 6 billing reminders / wallet-short notice (3 days before, "we will try again
tomorrow"), the 60-day Rs 99 refund (spec 7), Plus-member migration (spec 8), the B-02 copy
decision, CRM json triggers for `referral.applied` / `referral.rewarded`, the founder data
(farm names, photos, consent, thresholds, ERP FOUNDING-99 / DELIVERY-FEE / PYS-* items),
`FOUNDING_PYAAS_MEMBERS_ONLY` and `FOUNDING_PYAAS_NONMEMBER_FEE` defaults.

### 5.4 Deviations from the frontend contract to know

- Referral `status` is `pending` -> `credited` (the app type), the CRM event is `rewarded`.
- Error bodies are the consumer module's flat `{code, message}`; `WALLET_SHORT` adds `shortfall`.
- The app's 4-character code derivation collides for accounts created in the same second
  (oldest stored code wins); documented in 9.1 on the branch.

### 5.5 How to merge the branch once 5.3 is fixed

```bash
cd C:/Users/jaina/Downloads/parag-saathi-be
git fetch origin
git merge origin/feature/founding-referrals          # merge commit, never rebase
# expected overlaps with the CRM completion commits: subscriptions.go (noon lock + duplicate
# guard + stale orders vs subscription.* emitters), module.go (routes), crm_triggers.json
# (FF-01..03 vs D-09 and the awaiting_event meta: keep the union, byte-level CRLF edits, fix
# meta.trigger_count / template count), crm_engine.go / crm_router.go (emit helpers), wallet
# and pricing code, docs/HANDOFF-CODEV-2026-09-20.md (keep both sections).
go build ./... && go vet ./... && CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 go test ./... -count=1
git push origin integration/delivery
git worktree remove D:/dev/pyaas/be-features --force && git worktree prune
```

Then have someone else re-run the suite and re-check: every route the consumer app calls in
`lib/referrals.ts`, `lib/foundingFamily.ts`, `lib/subscriptions.ts` exists in `module.go`;
`crm_triggers.json` parses with matching meta counts; the noon lock, the per-order claim scope
and the subscription unique index coexist (a plan edited after noon applies to the day after
tomorrow; two instant orders the same day both get D-01).

---

## 6. What is left

### 6a. Engineering the co-dev must do

**0. Finish the two stopped backend stages, in this order** (nothing else in this list makes
sense before it, because the E2E and the release both need the merged backend):

- a. On `feature/founding-referrals`: fix 5.3 items 1-6 (each has its planned fix and its
  acceptance test), suite green on a private mongod, push.
- b. On `integration/delivery`: finish 4.6 (human_call transport, W-03b decision, the matrix
  test, `docs/CRM-MESSAGES.md`, stage D items 1-5), suite green, push.
- c. Independent review of the five unreviewed CRM commits `8edd9e1 8d24915 804205f 7cbb043
  81e2ff6` with the two lenses in 4.6 (compat and CRM-matrix). Reproduce, do not read only:
  two instant orders the same IST day both get D-01; a top-up through each credit path
  (`creditTopup`, promo, refund, rider undo, Razorpay webhook, mandate execution) produces a
  message; a delayed trigger skips a cancelled order; `CRM_ENABLED` unset changes nothing.
- d. Merge per 5.5, review the merge, push.
- e. Only then the E2E (item 1), the GTG (item 2) and the release.

**1. The frontend-driven E2E dry run** (owner requirement: E2E is driven from the apps'
own API layers against a booted backend, never from curl; source: PYAAS_STATUS).

Starting point and exact steps:

- Backend: `cd parag-saathi-be && go build -o <scratch>/saathi-server ./cmd/server`, then run
  it from a directory that does NOT hold the repo's `.env` (the server loads `./.env` from
  its working directory and the repo's copy points at the real deployment):
  `ENV=dev PORT=18080 OTP_DEV_MODE=true JWT_SECRET=dev-only-e2e MONGO_URI=mongodb://127.0.0.1:27017 MONGO_DB=pyaas_dryrun_<date> CRM_ENABLED=true CONSUMER_CATALOG_SEED_SERVE=true CRM_MSG91_BASE_URL=http://127.0.0.1:9009 CRM_WA_BASE_URL=http://127.0.0.1:9009 EXPO_PUSH_BASE_URL=http://127.0.0.1:9009 <plugged-channel dummies from 4.4> ./saathi-server`
  (`OTP_DEV_MODE=true` returns the OTP in the response and enables the dev-only
  `POST /consumer/subscriptions/sweep` tick; `CONSUMER_CATALOG_SEED_SERVE=true` serves the
  bundled catalog; both used by the live runs in 03). Run a capture stub on :9009 that logs
  method, path, headers and body.
- Seed: `python pyaas-saathi/tool/e2e/seed_pack_volume_e2e.py --uri mongodb://127.0.0.1:27017 --db pyaas_dryrun_<date> --reset`
  prints `SAATHI_E2E_STORE_ID=<id>` (store org with geo, a STORE_MANAGER on 9000000031, a
  DELIVERY_RIDER on 9000000021). Founding Family farms (after the branch is merged):
  `go run ./cmd/seed` seeds `cmd/seed/founding_farms.json` (placeholders marked `founder_todo`)
  through `consumer.SeedFoundingFarms`; or `PUT /consumer/admin/founding/farms` with the admin key.
- Consumer: there is no jest config in `pyaas-consumer` today (`package.json` has no `jest`
  entry), so install the harness first: `npx expo install jest-expo jest @types/jest`, add a
  `jest.config.js` with preset `jest-expo`, shim only native storage (AsyncStorage, SecureStore)
  and run the REAL `lib/apiClient.ts`, `lib/api.ts`, `lib/session.ts`, `lib/subscriptions.ts`,
  `lib/walletApi.ts`, `lib/complaints.ts`, `lib/notificationCenter.ts`, `lib/crm.ts` with
  `EXPO_PUBLIC_API_URL=http://127.0.0.1:18080/api/v1/consumer`. Flow: OTP login -> profile ->
  address with pin -> order 500ml x2 + 1L x1 -> wallet top-up (dev seam) -> inbox holds the
  top-up message and D-01 -> subscription create (daily) -> a second create for the same
  product is refused (client and, after section 5 lands, server 409).
- Saathi: `flutter test test/e2e/pack_volume_live_e2e_test.dart --dart-define=SAATHI_E2E=true --dart-define=API_URL=http://127.0.0.1:18080/api/v1 --dart-define=SAATHI_E2E_STORE_ID=<id>`
  (defines at `test/e2e/pack_volume_live_e2e_test.dart:58-62`; `tool/e2e/README.md` is the
  runbook). Manager sees the order with both sizes and the upcoming row; rider sees sizes,
  pickup, deliver; route Overview and Inventory rows. Note the upcoming leg needs the worker to
  have previewed tomorrow; after the noon-lock merge the preview window is "until 12:00 IST the
  day before", so run the Saathi leg before noon or use the dev-only sweep endpoint.
- Consumer again: order delivered -> wallet debited exactly once (`delivery:<order_id>` ledger
  row) -> inbox D-02/D-06 -> complaint filed -> operator resolves via
  `PATCH /consumer/ops/complaints/{ref}` -> E-06 -> rating -> E-01/E-07 per the json ->
  sign-out (DELETE /push/register observed, tokens revoked).
- CRM matrix: every trigger that FIRES is captured at the stub with its rendered body; list
  the `awaiting_event` triggers.
- Cleanup (must leave no test data): drop `pyaas_dryrun_<date>`, kill the server, verify
  `db.adminCommand({listDatabases:1})` shows only `admin`, `config`, `local`, `saathi_local`
  (the founder's own local dev db: NEVER touch it; source: PYAAS_STATUS), `git status` clean in
  all three repos, grep the repos for committed fixtures.

Acceptance: every step above passes from the apps' own code paths; the stub log is attached
to the GTG review; the Mongo database list is back to the four names.

**2. The GTG gate.** An independent reviewer re-runs the dry run and the three suites
(section 7) from a clean checkout and signs off before `integration/delivery` merges into
`release/26.07.03`.

**3. Saathi store-manager complaints screen.** `GET /consumer/ops/complaints` (optional
`?status=`, unknown status -> 400; `{data:[...]}` envelope, newest first, limit 500) and
`PATCH /consumer/ops/complaints/{ref}` `{status, resolution}` (resolution written verbatim
to the member; `status: resolved` emits `complaint.resolved` through `answerComplaint`)
exist at `module.go:355-356` in the STORE_MANAGER/SUPER_ADMIN group (the `{ref}` segment
also takes the row's complaint id `cmp_...`, and a ref two members share answers 409
`AMBIGUOUS_REF`, so the screen should send the row's `id`); Saathi has no caller
(`grep -rn ops/complaints lib/` is empty; source: 06). Starting point: `lib/api/store_console_api.dart`
(same `ApiClient` unwrap of `{data}`), a list + resolve screen under the store console.
Acceptance: an operator resolves a complaint the consumer harness filed and the consumer's
inbox shows E-06.

**4. Reviewer LOW items still open** (fixed ones are omitted; each tag names the report):

- Backend: `forgetPushDevices` in `push.go` is dead code (the cascade deletes the collection
  directly) (01). `eta_at` on the consumer order and rider position on `GET /orders/{id}`;
  without them the app's countdown sticks on "Arriving any moment" (01, 20 Sep handoff).
  Em dashes in `crm_offers.go` (`crmLabelledSuffix`, W-01 comment) and in the OTP/refresh
  error strings `consumer/service.go:176,212,219`, `identity/service.go:169,192,307,315`,
  `middleware/ratelimit.go`; the founder wants them gone (01, 08). `rider_ops_route.go:641-643`:
  when the order behind a task cannot be read, two sizes of one product merge into one
  inventory line with no size (05). `createMandate` has no one-live guard and `currentMandate`
  returns the first non-REVOKED mandate, so a second live mandate would outlive erasure
  (04). `structuredDoorFor` returns the chosen row even with an empty `SocietyID` (cosmetic,
  06); `answerComplaint`'s 404 text says "no complaint with that reference" on the by-id path
  too (06). Upcoming carries only subscription previews in practice because a scheduled
  one-off mints its task immediately (design; founder question in 6b) (03).
- Consumer: `updateSubscription` and `reactivateSubscription` (`lib/subscriptions.ts`)
  invalidate only on success; a 409 on a qty/frequency edit leaves the stale plan on screen
  until the next refresh-asking read (07). The sign-out-during-in-flight-POST reordering
  residual in `lib/notifications.ts` (07, design choice). `preserveReferralCode` in
  `lib/session.ts:86-87` writes a local `referral_code` row nothing reads in backend mode
  (10). The backend-mode sign-out purge removes the uid-keyed `mirror_queue`, so an unlanded
  consent opt-out or queued referral-apply is discarded at sign-out (04, 10; phase B design,
  decide). `useConsents.save` re-sends every channel with a fresh `occurred_at` on a single
  toggle (04, pre-existing). `app/refer.tsx` does not display `referralListError()` and
  `setReferredBy` does not surface the server's invalid-code message (Kushagra UI decisions,
  10). `lib/autoTopup.ts` DEFAULTS `on: true` is a product change from `5f92d2e`, left as is
  (10). `components/PromoGate.tsx:164` still prints the client constant `PLUS_PRICE_MONTH`
  (08). `autopayErr` on the wallet tab clears only on the next tap (cosmetic, 07). Push:
  Android notification icon (white square without one) (20 Sep).
- Saathi: `lib/screens/store/store_home.dart:315` still carries an em dash in the low-stock
  banner string (09). Rider Inventory "Products" metric counts lines, not distinct names;
  `ScanProductsScreen` subtitle is `line.name` only, so two sizes open identically titled
  screens (09). `mrpFor`/`soldValueOf` public without `@visibleForTesting` (nit, 09).
  `lib/screens/delivery/` is unrouted dead code that would hide sizes if re-routed (05).
- Consumer, from the features review (app-side, for Kushagra): the start-date picker after
  noon should read `next_delivery_date` (or default to the day after tomorrow); the basket
  should show level-3 prices and Rs 0 fee for active members (spec screen 9); `app/refer.tsx`
  could read `referralListError()` (11, 10).
  Saathi keeps a local pour outbox (`lib/api/collection_api.dart:657` `PourOutbox`,
  SharedPreferences), pour drafts and idempotency keys; outside the consumer rule and outside
  the three profiles, ask the owner whether the rule applies (01).

**5. Founding Family founder data** (needed before the feature is real; section 5.3 and
`docs/HANDOFF-CODEV-2026-09-20.md` 9.6 on the branch): farms with `name, farmer, place, note,
photo_url, unlocks_at`, the seat counts,
photos, ERP items `FOUNDING-99` and `DELIVERY-FEE`, level-3 prices on PYAAS milk in the ERP
(else the `FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE` derivation applies), and the website route
`www.pyaasdairy.com/foundationfamily?ref=CODE` that the share link points at (08).

**6. Cut a new Saathi build** (pack size, tri-state fence, inventory per size only appear on a
fresh one) after backend C3 is deployed; run the live e2e once more against it (05).

### 6b. Founder / external decisions

- **DLT template registration** for every lifecycle template that should go over SMS
  (D-01/D-02/D-06, E-02/E-06, B-01/B-02, W-02..W-06) and the `CRM_DLT_TEMPLATE_IDS` mapping
  (only W-01 and W-07 today); confirm at MSG91 which header W-01 and W-07 were approved under;
  flow variables lowercased (`##labelled_product##` etc.) as MSG91 requires.
- `CRM_MSG91_AUTHKEY` (the Flow API key; distinct from the login `MSG91_AUTHKEY`).
- `CRM_WA_TOKEN`, `CRM_WA_PHONE_ID`, `CRM_WA_TEMPLATE_NAMES` (WhatsApp Cloud API, approved
  templates).
- **EAS projectId** (`eas init` in `pyaas-consumer`; `app.json` has no `extra.eas.projectId`,
  so `getExpoPushTokenAsync()` throws on every device and zero tokens exist), then
  `EXPO_PUSH_ENABLED=true` and optionally `EXPO_ACCESS_TOKEN` on Render; Firebase project +
  `google-services.json` + `android.googleServicesFile`; APNs key in EAS credentials.
- **B-02 copy vs horizon:** the body says "recharge by 12 noon tomorrow" but the check runs at
  12:00 today for tomorrow's order and the rider leaves ~05:00; fix the horizon or the copy,
  not both independently (20 Sep bug 2; untouched, source: 01).
- **Upcoming one-offs:** should the store's Upcoming section show scheduled one-off orders?
  They mint a task immediately, so `/upcoming` only ever carries subscription previews (03).
- **Deploy order:** backend C3 before the Saathi build (or the absent-flag fallback keeps the
  old fence) (05).
- **The secrets zip:** `pyaas-secrets-backup-2026-09-02.zip` sits untracked in the backend
  repo root; move it out of the repo directory today.
- **Referral reward policy:** the Refer screen promises "Gift Rs 100, get Rs 100"; the app
  spec 5.8 / One Voice say a referral only moves the referrer up the Founding Family line.
  The features stage implemented the Rs 100 both-sides reward per the owner's 24 Sep call
  (`REFERRAL_REWARD_PAISE`); confirm (08, section 5).
- **Consent defaults:** `5f92d2e` pre-ticks marketing/whatsapp/sms and drops the email row;
  the CRM's derived `promotional` consent flips true for every new member by default. DPDP /
  TRAI / Apple 4.5.4 exposure is flagged in Kushagra's own handoff; the backend should not
  treat a default-true row as affirmative consent until the founder rules (08).
- **Noon-lock comms:** the app already says 12 noon; tell members and the store the lock moved
  (section 5).
- **Founding Family rules to confirm** (section 5.3): a stopped member re-joining inside the
  paid month keeps the paid perks and pays nothing; whether REWARDS (referral) money may fund
  the Rs 99 seat; `FOUNDING_PYAAS_MEMBERS_ONLY` and `FOUNDING_PYAAS_NONMEMBER_FEE` (both off).
- **W-03b:** keep it consent-gated (it then almost never sends) or reclassify (section 4.6).
- From the 20 Sep handoff, still open: `ADMIN_API_KEY` on Render and Vercel; Terms/Privacy PDFs
  still print `99996 80081`; three Welcome Litre creatives out of rotation; attendance gating
  the rider queue; store console hand-assigning an unclaimed OFFERED order; a clean production
  database instead of `saathi_dev`.

### 6c. Known behaviour consequences the owner accepted

- Offline in a session that has not yet read `GET /addresses`, the address list is the outbox
  only; nothing local stands in for the server's book; a pin dropped in that state creates a
  new address that, on replay, may duplicate the server's (hence the POST /addresses
  idempotency item in 4.6) (02, 04).
- `addVacation` needs a live plan on the server; with none it throws "Start a subscription
  first" instead of recording a standing range (02).
- `one_time` plans are hidden in backend mode; the server accepts only daily/alternate/weekly
  (`subscriptions.go`); a server plan on any other cadence renders as a read-only pill (04).
- Low-balance auto-pause is no longer decided on the phone; the server worker skips a day the
  wallet cannot cover and B-01/B-02 message the member; the app shows a reminder and a
  "delivery may be skipped" notice, and a paused plan can be resumed whatever the wallet holds
  (04, 07).
- `PATCH /me` replaces the whole `delivery_prefs` document (`service.go updateMe`,
  `sanitizeDeliveryPrefs`), so the delivery-prefs replay lays the queued keys over a fresh
  `GET /me` and sends the full object; a true partial PATCH needs a backend merge (04).
- Replay error classes (`lib/mirrorQueue.ts` `mirrorOutcomeFor`): network, timeout, 5xx, 408,
  429 retry; 401 and 403 retry (session states, not verdicts); every other 4xx drops the row and
  is shown once; promos never drop (07).
- Sign-out zeroes the wallet store and clears every in-memory server copy; the next member
  never sees the previous balance; the purchase unlock latches only from a proven refresh or
  the ledger (04, 07).
- The `vip` row is spared on sign-out and neither read nor deleted in backend mode: it is the
  only evidence of a Plus month an older build debited from the server wallet, and no
  `GET /membership` exists (07).
- Sign-out purges the uid-keyed outboxes and the mirror queue (phase B design; see 6a.4).
- Complaint `id` is the `cmp_...` wire id, opaque to the app; unknown category -> 400; missing
  ref -> 400; platform outside ios/android/web -> 400 (06).
- A label-only order (deployed consumer app sends no `address_id`) gets the door only when its
  label matches one door or identical rows; ambiguity stamps nothing (06).
- Saathi geofence tri-state: absent = old 300 m phone fence, `false` = no fence, `true` =
  fence and the server measures; absent + no pin says "This task has no delivery pin, ask the
  store" and does not finish (05, 09).
- A rider FAILED marking or a store cancel now flips the consumer order to `cancelled`
  (previously the store cancel 500ed and a FAILED task left the order out_for_delivery
  forever); a re-assign or rider undo walks it forward again (01).
- The consumer's local order/complaint notices are deduped against the CRM inbox by event
  class within 24 h until the inbox row carries the order id / ref (02, 4.6).
- Pack size rule: the wire `name` stays verbatim (stock reconciliation matches it exactly); a
  size pill is drawn even when the name already states it; a name embedding its size and
  longer than ~34 characters is the part the ellipsis cuts, never the pill (03, 05).

---

## 7. How to verify

Backend (`C:\Users\jaina\Downloads\parag-saathi-be`):

```bash
# a local mongod; the suite uses FIXED database names, so run ONE suite per mongod
# (the features worktree used --port 27018 --dbpath D:/dev/cache/mongo-wt1 for that reason)
D:\dev\tools\mongodb\bin\mongod.exe --dbpath D:\dev\data\mongo --port 27017
go build ./... && go vet ./... && CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 go test ./... -count=1
# every package ok; internal/modules/consumer must take > 10 s (30-90 s seen; 79.3 s at 81e2ff6), or the Mongo
# tests skipped silently and the run proves almost nothing
CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 go test ./internal/modules/consumer/ -run 'Phase2' -count=1 -v
# 19 TestPhase2* PASS, zero SKIP (source: 06)
gofmt -l internal/   # models.go has 36 lines of pre-existing drift; nothing else (source: 06)
```

Consumer (`C:\Users\jaina\Downloads\pyaas-consumer`):

```bash
npm install                      # once; expo-store-review arrived with 5f92d2e
./node_modules/.bin/tsc --noEmit # exit 0, no output (source: 10 at c7e2704; re-run clean at 37a4f4a by the handoff author)
```

Saathi (`C:\Users\jaina\Downloads\pyaas-saathi`):

```bash
flutter analyze   # "No issues found!"
flutter test      # 128 passed, 11 skipped at c31bccb (source: 09); the 11 are test/e2e/ opt-in
# live legs: see section 6a.1 (needs a booted backend and the seed)
```

Git state (each repo): `git log -1 --format='%H %s'` matches section 1.1; `git status -sb`
shows `...origin/<branch>` with no ahead/behind; the only local change anywhere is Saathi's
tool-made `analysis_options.yaml` (section 1.1). The backend worktree `D:/dev/pyaas/be-features`
still exists until section 5.5 is done.

---

## 8. Findings register (everything found, with its status)

Status: FIXED (commit) or OPEN (where it is described). Severity is the reviewer's. Sources are
the report files in `docs/handoff-2026-09-24-reports/`.

| # | Where | Severity | Finding | Status |
|---|---|---|---|---|
| F01 | backend | BLOCKER | `PATCH /skus/{id}` had no `variants` field although the shipped Saathi catalog tab sends the full list | FIXED `7897c58` |
| F02 | backend | HIGH | no push sender existed; `/push/register` stored tokens nothing read | FIXED `aacb25d`, `fea7931`, `dc17eda` (C2, C5) |
| F03 | backend | HIGH | order lifecycle events (`order.confirmed` / `dispatched` / `delivered` / `failed`, complaints, rating) were never emitted; D-/E- triggers dead | FIXED `47cd83e`, `f7aaef1` |
| F04 | backend | HIGH | W-07 loss: pack 2 expired before the message was on record as SENT | FIXED `8afae8f` |
| F05 | backend | HIGH | store item adjust keyed by name, so two sizes of one product could not be told apart | FIXED backend `9a10786` (pre-series) + Saathi `02f34db` |
| F06 | backend | HIGH | `geoExact` missing on the task wire; the phone fenced on store-fallback pins | FIXED `731be2e` + Saathi `8f31455`, `792c7a3` |
| F07 | backend | HIGH | a failed / store-cancelled task left the consumer order live forever; the store cancel returned 500 (Mongo `updated_at` path conflict) | FIXED `f00e790` |
| F08 | backend | MEDIUM | no `/upcoming` for the store console; no presign for consumer photos; `GET /orders` signed every proof photo; no `address_id` on orders; `societyId` missing | FIXED `083ecf1`, `edc438e`, `6b374f9`, `4a2dc46`, `3c02bb1` |
| F09 | backend | HIGH (regression by this series) | `29936df` removed five rider routes the DEPLOYED Saathi `release/26.07.03` still calls | FIXED `5b16775` (revert) |
| F10 | backend | MEDIUM | the owner's `release/26.07.03` phase-2 commits (`775f370`, `640cff7`, `8394476`) were not in `integration/delivery`; the co-dev had built the same features in parallel | FIXED `80f4a0b` (merge; 19/19 owner contract tests pass unchanged) |
| F11 | backend | MEDIUM | subscription orders had no unique backstop under the day claim | FIXED `b00cc7d` |
| F12 | backend | MEDIUM | no way to capture what CRM would send in a dry run | FIXED `d6145f9` (`CRM_MSG91_BASE_URL`, `CRM_WA_BASE_URL`, `EXPO_PUSH_BASE_URL`) |
| F13 | backend CRM | HIGH | per-order triggers claimed once per consumer per day: a second order the same day got no D-01/D-02/D-06 | FIXED `8edd9e1` (unreviewed) |
| F14 | backend CRM | MEDIUM | trigger `delay` ignored | FIXED `8d24915` (unreviewed) |
| F15 | backend CRM | HIGH | ~38 triggers had no emitter (the wallet top-up message the owner reported among them); complaint conditions used category names the app never sends; `order.failed` had no message | FIXED `804205f`, `7cbb043`, `81e2ff6` (unreviewed); 22 triggers remain `awaiting_event` by design |
| F16 | backend CRM | MEDIUM | E-05 (`human_call`) reaches nobody; W-03b never sends; no matrix test; no `docs/CRM-MESSAGES.md` | FIXED `e76bd1a` (human_call queue), `22759d4` (W-03b is consent-gated by owner decision and proven to fire for a consenting member), `482133c` (matrix), `docs/CRM-MESSAGES.md`; pre-ticked consent defaults remain a founder decision (6b) |
| F17 | backend | HIGH (owner-visible) | Welcome Litre plans created server-side carry no pack size: every morning order, task, store row and rider row shows no volume (reproduced live) | FIXED `3b8851f` |
| F18 | backend | MEDIUM | offline replay of an address create can duplicate a server row | FIXED `8f369e8` |
| F19 | backend | LOW | inbox rows carry no order id / complaint ref for exact dedupe | FIXED `cb5ede3` + consumer `ee2a07f` |
| F20 | backend | LOW | rider Verify-inventory lines carry the size only as `unit`; the order-unreadable fallback merges sizes | FIXED `71286d2` |
| F21 | backend features | HIGH | Founding Family billing retries per hour: a short wallet stops the membership in ~3 hours | OPEN, 5.3.1 |
| F22 | backend features | MEDIUM | stop then re-join inside the paid month forfeits perks and charges Rs 99 again; own unlocked farm cannot be re-joined | OPEN, 5.3.2 (owner rule) |
| F23 | backend features | LOW | LOCK step honours edits made 12:00-12:15 | OPEN, 5.3.3 |
| F24 | backend features | LOW | duplicate line numbers after a seat is released | OPEN, 5.3.4 |
| F25 | backend features | LOW (money) | founding / referral unique indexes absent from the test world; racy pre-check is the only guard when the index build fails | OPEN, 5.3.5 |
| F26 | backend features | MEDIUM | one-off morning orders got no noon cut-off although the app says "Order by 12 noon" | OPEN, 5.3.6 |
| F27 | backend | MEDIUM | `DUPLICATE_SUBSCRIPTION` server guard exists only on the unmerged branch; production still mints twins | OPEN until 5.5 |
| F28 | backend | LOW | `eta_at` / rider position on `GET /orders/{id}`; `forgetPushDevices` dead code; em dashes in error strings; `createMandate` no one-live guard; two cosmetic 404 texts | OPEN, 6a.4 |
| F29 | consumer | HIGH | persisted mirrors of server rows were the display source (previous member's addresses, plans, balance on a shared phone; a stale mirror could re-place a cancelled order); the AutoPay settle wrote a local ledger credit | FIXED phase B `f745c46..7fe34b7` + follow-ups (six guards proven at every stage) |
| F30 | consumer | HIGH | push registration had no EAS projectId gate, no unregister on sign-out, no tap handler; refresh token never revoked; `markAllRead` re-POSTed every row; `address_id` not sent | FIXED phase A `f028def..59ee930` |
| F31 | consumer | MEDIUM | sign-out blocked on two network calls; stale "Delivered" burst after an account switch | FIXED `f745c46`, `2e85d9e` |
| F32 | consumer | MEDIUM | one_time plans dead in backend mode; cache invalidation race; caches surviving sign-out; complaint 4xx retried forever; partial prefs outbox reset other keys; failed mandate CANCEL let erasure proceed; derived flags persisted; fabricated quality rows and a legacy vip row rendered | FIXED `3d94f95..5a48351` |
| F33 | consumer | MEDIUM | a 401 from a timed-out token refresh deleted a queued complaint; unhandled throws in support chat and rating sheet; wallet store not reset on sign-out; write-after-clear races; 409 on pause left a stale plan; silent mandate-cancel failure on the wallet tab | FIXED `c0836e1..7ae7075` |
| F34 | consumer | MEDIUM | Kushagra's `5f92d2e` merge: three persistence regressions (referral code mirror, referred_by written regardless of POST, taglines gate on local consents defaulting to true), the settle sweep on every backgrounding, two tap handlers, duplicate-plan guard reading the outbox | FIXED `359288c` + `7a83faa..35b663e` (34 of his 55 files byte-identical) |
| F35 | consumer | LOW | referral code cache not cleared on sign-out | FIXED `37a4f4a` |
| F36 | consumer | LOW | `updateSubscription` / `reactivateSubscription` invalidate only on success; `preserveReferralCode` dead write; sign-out purges unlanded outboxes (design); `PromoGate` prints a client constant; picker after noon | OPEN, 6a.4 |
| F37 | consumer | product | six endpoints `5f92d2e` needs did not exist (founding-family x3, referrals x3) | BUILT on the unmerged branch (section 5) |
| F38 | Saathi | HIGH (owner-visible) | pack size lost in five rider / store widgets (joined strings under an ellipsis); no items on the offer card and the swipe screen; overview rows and inventory rows merged sizes; a latent ellipsis path; MRP keyed by name | FIXED `7822ce2`, `0f7356c`, `a7b9af8`, `3458ce9`, `9119b26`, `768ead7`, `01f3692` (24/24 and 21/21 reviewer probes at 360 px Roboto) |
| F39 | Saathi | MEDIUM | new-order alert diffed list length; DROP handover warned; cash collect had no manual path; structured door not read; upcoming not shown; dead per-task calls | FIXED `92042b6`, `5f19ffb`, `4deb0fa`, `51e4936`, `ef41d7b`, `0bf6f8b` |
| F40 | Saathi | MEDIUM | a build fencing only on `geoExact == true` would have had NO fence against the deployed backend | FIXED `792c7a3` (tri-state) |
| F41 | Saathi | LOW | live e2e assertions satisfied by a still-pushed route; OTP 5xx copy; RING_BELL line; no-pin copy; non-ASCII glyphs; orphaned `ComplianceStep` | FIXED `114a455`, `5d86bdc`, `c31bccb`, `f978b6e`, `d90af82` |
| F42 | Saathi | LOW | Inventory "Products" metric counts lines; scan screen subtitle name-only; low-stock banner em dash; unrouted `lib/screens/delivery/`; pour outbox outside the consumer rule | OPEN, 6a.4 |
| F43 | Saathi | product | no screen for the owner's `/ops/complaints` operator routes | OPEN, 6a.3 |
| F44 | all | process | the Bash tool's permission classifier refuses `git push`; PowerShell pushes work; Windows `git worktree` under the Blue_shell repo fails on long paths, so backend worktrees are created by hand on `D:` | note |

---

## 9. Files committed with this handoff

- `docs/HANDOFF-CODEV-2026-09-24.md` (this file)
- `docs/CRM-AUDIT-2026-09-24.md` (the 54-trigger audit at `bf083ef`: mechanics, per-trigger
  matrix, fixes per dead row, unnamed topics, structural gaps)
- `docs/handoff-2026-09-24-reports/00..11-*.json` (raw stage reports and reviewer verdicts
  the `(source: NN)` tags point at; agent prose, kept verbatim as evidence)

---

## 10. Noon rules, 24 Sep afternoon

Branch `feature/noon-rules` (from `integration/delivery` at `0304b23`, after the
founding/noon-lock merge and its post-merge fixes). The owner's decisions of 24 Sep; the
design is the stage 2b design note. Every time-dependent test runs on an injected clock.

### 10.1 Low wallet, the noon wake and the copy that names the day

| commit | what |
|---|---|
| `fef95fc` | the noon lock decides a member's day once, on the wallet at 12:00 |
| `7f23949` | a free trial day costs nothing at the noon lock (decision 9) |
| `388f51e` | the worker wakes at 12:00:05 IST to lock tomorrow's orders |
| `8a8b82e` | D-07 tells a day skipped at noon, once; B-02 waits for the lock and stays quiet beside D-07; A-05 and FF-01 name the real day |
| `747e490` | `pause_reason: "member"` on POST /pause |

**The rule, as built** (`subscriptions.go` `lockConsumerDay`):

- One member's previews for one day are decided together, once, after that day's cut-off
  (12:00 IST the day before). Re-checked against the plan (a plan changed before the
  cut-off decides it as it is now; one changed after keeps the day as previewed), then
  funded **oldest plan first** from `walletAsOf(12:00:00)`: the Available balance now with
  every ledger row stamped after 12:00:00 played back out. **Debits are added back as well
  as credits taken off** (owner's decision 1), so a tick at 12:00:05 and one at 12:14:59
  decide the same. Less what the member already owes that day: subscription orders already
  locked and wallet-paid one-off morning orders dated that day (G7; a one-off is fixed once
  its day's cut-off has passed). Each order costs what the door will take: a 2+2 free trial
  day costs Rs 0 (decision 9), and the locked order's `trial_free` follows the trial as it
  stood at 12:00:00 (days charged at the door after the lock moment are played back out,
  `phaseAsOf`; 10.3).
- Covered: locked, store task minted, D-01 as before. Not covered: the preview is closed at
  once as `cancelled`, `cancelled_by: "wallet_short"`, `skipped_at`; **the day claim is
  kept** (never re-previewed), no task, no money; `subscription.day_skipped` is emitted
  `{subscription_id, day, reason, shortfall, resume_label, tomorrow_blocked, scope_key}`.
  Nothing is retried on later ticks, so a top-up after noon (or in the night) never mints a
  late task. The plan stays `active` and the next editable day is previewed as usual:
  recharge before 12 noon, the next morning is delivered; after noon, the one after.
- A preview no lock decided before its route left (05:00, the server down over noon)
  expires (cancelled, no `cancelled_by`, no message), as `d560e13` made it.
- The catch-up (a day past its cut-off that was never previewed: an outage) now claims the
  day, writes the preview and runs the same lock. A day whose route has already left is not
  caught up at all (before: a locked order and task for a round already gone at, say,
  09:00).
- `listActiveSubscriptions` is sorted oldest plan first, so the catch-up funds in the same
  order.
- New non-fatal index `consumer_wallet_txns {consumer_id, created_at}` (`consumer_created_at`).
- The worker (`runSubscriptionWorker`): one tick at boot, one every 15 minutes, and a
  one-shot wake at the next 12:00:05 IST, armed from the clock before the boot tick and again
  after each wake (no stored state). **`render.yaml` still says `plan: free`** (spins down
  when idle): a sleeping instance runs neither timer, and the first request after noon wakes
  it; the lock outcome is the same whenever it runs, only the store sees tomorrow's tasks
  later. An always-on plan or an external 12:00:05 ping is the ops decision.
- `pause_reason: "member"` is stored and returned on POST /pause and cleared on resume. The
  server never pauses for a low wallet and never resumes a pause (Option A); a test pins
  that a credit plus a day of ticks leaves a member's pause paused with no orders.

**CRM** (`crm_triggers.json`, `docs/CRM-MESSAGES.md` updated):

- **D-07** is live on `subscription.day_skipped` (condition `tomorrow.delivery_blocked ==
  true`: the skipped day is tomorrow, the plan is active, and nothing else still arrives that
  morning: no other plan's locked order, no one-off morning order, no free Welcome Litre
  pack; 10.3). Once per member and day (`scope_key: day_skipped:<day>`). Channels:
  sms, +whatsapp, +push, inbox always. Body: "No delivery tomorrow - your wallet was short at
  12 noon. Recharge by 12 noon tomorrow and your milk resumes 8 Oct." [DATE] is the next
  morning the plan delivers after the skipped one (the day after tomorrow for a daily plan),
  whose own cut-off is never before 12 noon tomorrow, so the deadline is true for every
  cadence.
- **B-02** claims its day only once a subscription sweep has run tomorrow's lock and
  catch-up to the end (`consumer_noon_locks`, 10.3), and then once no preview for tomorrow is
  still undecided (from 13:00 regardless), so it always runs after the lock, also after a
  boot or wake past noon; it is not sent to a member D-07 reached at that noon. It stays on the CRM minute tick (it fires within a minute of the 12:00:05 lock):
  the subscription worker's boot tick can run before the CRM claim indexes are built, and a
  claim without its unique index could send the day twice.
- **A-05** "One step left — recharge your Wallet and your morning milk starts [DATE]."
  [DATE] is read from the plan when the message fires (a tomorrow skipped at noon inside the
  two hours is not offered). **FF-01** "First delivery [DATE] by 7 AM", the first morning an
  order placed at the unlock reaches.
- T-A05, T-D07 and T-FF01 changed wording: register the new DLT / Meta templates before
  mapping them in `CRM_DLT_TEMPLATE_IDS` / `CRM_WA_TEMPLATE_NAMES`. Until then they are
  inbox (and push, once enabled) only.

**What the apps see**

- Member: a skipped day is a `cancelled` order for that date in Orders (`cancelled_by` is
  not on the wire); the shipped build's cancelled line reads "Any amount held has been
  released", which is harmless. `next_delivery_date` moves past a skipped day. The new
  optional `pause_reason` key. D-07 in the inbox.
- Store: an unfunded tomorrow leaves Upcoming at 12:00:05 for good instead of lingering as
  `awaiting_funds` until its day; `awaiting_funds` is now true only between 12:00 and the
  lock tick (seconds). Funded tomorrow tasks appear seconds after noon, not up to 15
  minutes later. No task is minted after the route has left.
- Rider: no late "morning" task minted after the round.

**Tests** (`CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018`): `wallet_asof_test.go`,
`subscriptions_wallet_lock_test.go` (noon wallet, 12:00:01 / 12:14:59 / 23:59 and month end,
recharge before and after noon, two plans one wallet, the one-off reservation, the free
trial day, catch-up), `subscriptions_worker_test.go` (the wake and the boot tick, no sleeps),
`crm_noon_copy_test.go` (D-07 / B-02 / A-05 / FF-01), `subscriptions_pause_reason_test.go`.
Rewritten because the rule changed: `subscriptions_route_start_test.go` (a night top-up no
longer revives a day skipped at noon; the expiry case is an outage),
`TestStoreUpcomingFlagsPreviewsAwaitingFunds`. Re-dated to a fixed clock:
`TestFullChainSubscriptionMorningDelivery`, the doorstep-prefs sweep (03:00, before the
route). The test world stamps its seed top-ups at 2026-01-01 (`chainStampLedger`).

**Residuals, accepted and known**

- The as-of window ends at the tick's clock: a movement in the seconds between the tick
  starting and the member's wallet read counts as before noon. A credit or debit whose ledger
  row is written but whose balance move has not landed at the instant of the read is
  miscounted by that one amount (milliseconds). The Play-review account's wallet floor moves
  money without a ledger row.
- An instant order delivered after 12:00 is added back by the as-of rule, so the door can
  still find the wallet short the next morning (the owner's decision 1).
- Not changed here: W-01 ("Tomorrow by 7 am") follows the pack 1 date, which the G9 item
  owns (done in 10.2: an afternoon enrolment gets the unregistered T-W01-LATER naming the
  real morning); W-03a ("Recharge by 12 noon today") is only true when its 10:30 tick runs
  before noon; the pack-2 attach still checks the current wallet before noon; `T-SMS-SKIP`
  (a standalone SMS template no trigger uses) still carries the old alternative-text block.

### 10.2 Orders, plan changes, the store and the rider, and the SMS caveats

| commit | what |
|---|---|
| `5fadafb` | R4(i): patch, pause, resume and cancel bring the plan's editable previews into line in the request (`syncPlanPreviews`) |
| `fe448a6` | R2: a one-off morning order for a closed or past morning is moved to the first open one and accepted (`requested_date`, `date_moved`); D-01 names the real day (`createDeliveryForOrderAt`) |
| `ddeae1e` | G4: a start date on a locked morning is re-anchored on the first editable one |
| `3fcdffa` | G7: a member's cancel of a morning order past its noon cut-off is 409 `ORDER_LOCKED` |
| `341cde7` | G9: the Welcome Litre pack 1 (and a plan minted with it) is for the first open morning |
| `97e6cea` | the store's `completed_today` counts on the IST day; R4(iii)/(iv) verified by tests |
| `8b28bbb` | R4(v): racing noon sweeps from two instances decide each day once (test) |
| `986ec90` | CRM caveat 1: each body goes out only under its own SMS / WhatsApp registration |
| `3ac09f8` | CRM caveat 2: the rider-undo credit's [REASON] uses a 6-character order code |
| `57a6016` | CRM caveat 3: B-06 ("money added") also goes by push, in parallel |
| `e1a7e48` | CRM caveat 4: a refused send is logged at ERROR and kept as `channel_errors` |
| `76049af` | W-01 names the morning pack 1 really comes (T-W01-LATER) |
| `2dcf9a3` | CRM caveat 5: `.env.example` / `render.yaml` wording, `docs/CRM-MESSAGES.md` |

**Plan changes reach the store at once (R4(i)).** `setSubscriptionStatusAt` and
`patchSubscriptionAt` end with `syncPlanPreviews(plan, now)`: the sweep's RECONCILE and
PREVIEW for that one plan, for days from `firstEditableDay(now)`. A preview the plan no
longer delivers (pause, cancel, vacation, moved start) is cancelled and its day released;
one it still delivers takes the plan's line; an active plan previews its first editable
day. Days past their cut-off are never touched (`lockPreviewsBeforeChange` decided them
first). Best-effort and idempotent against a tick or a replica; `/upcoming` is unchanged
(`813099a`'s batched reads stay). A new plan was already previewed by the create handler's
kick; that is unchanged.

**One-off morning orders (R2).** `morningDeliveryDate` in `orders.go`: no date → the first
open morning (as before); a date that is closed (tomorrow from 12:00 IST, today) or past →
moved to `firstEditableDay` and ACCEPTED, `date_moved: true`, `requested_date` = the day
asked for; an open date → kept (`requested_date` = it); more than 7 days ahead → 422
`BAD_DELIVERY_DATE` (unchanged); a malformed date → 400. The order and its task carry the
real `delivery_date`, and D-01's [ETA] is worded against the moment the order was placed
("8 Oct by 7 am"). The two keys are stored, so `GET /orders` and `GET /orders/{id}` carry
them; the instant lane carries none of the three. `CUTOFF_PASSED` is no longer sent (the
constant `errCodeCutoffPassed` and `apiError.next_delivery_date` stay for old builds).

**Re-anchor (G4).** `reanchorLockedStart`: a start date from today through
`lockedThroughDay(now)` becomes `firstEditableDay(now)`, on create and on a patch that sends
a DIFFERENT start date. A past start is an anchor and is kept (the shipped app re-sends the
original start on resume); re-sending the stored start moves nothing. So after noon an
alternate or weekly plan "from tomorrow" first delivers the day after tomorrow, not 3 or 8
days later; A-03's weekly case now says "8 Oct". For a new plan the new app's
`next_delivery_date: w.start_date` fallback now names the right first morning; it should
still read the server's `next_delivery_date` (lib seam G6).

**Cancel after the cut-off (G7).** `cancelOrderAt`: a member's `POST /orders/{id}/cancel`
of a morning order (one-off or subscription, locked or not yet) whose delivery day is on
or before `lockedThroughDay(now)` answers 409 `ORDER_LOCKED`, "Orders lock at 12 noon the
day before delivery, so this one can no longer be cancelled.", and changes nothing. The
store's cancel (`storeCancelDelivery`), the rider's marks, operator paths and the instant
lane are untouched. The noon lock already reserves a member's one-off morning orders due
that day (`listMemberDayCommitted`, `fef95fc`); with G7 that reservation is exact.

**Welcome Litre pack 1 (G9).** `crmEnrolCore` mints pack 1 for `firstEditableDay(at)`
(`pack1_scheduled_for` says so), and a plan it mints (`crmCreateSubscriptionAt`) starts that
morning. `offer.finalized` carries `pack1_day`; W-01 picks T-W01 ("Tomorrow by 7 am", the
registered body) when pack 1 is tomorrow as seen when W-01 is sent (`offer.pack1_tomorrow`),
else T-W01-LATER ("You're set. 8 Oct by 7 am: …", same words, not registered: inbox and push
only until DLT / Meta approve it and it is mapped under `T-W01-LATER`). The promoter's door
script still says "tomorrow".

**Store and rider (R4(iii)-(v)).** Verified by tests, no change needed: at the lock the
store's queue gains tomorrow's funded orders as tasks dated tomorrow with the pack size and
the dated slot, and Upcoming moves on to the day after
(`TestStoreSeesTomorrowsLockedOrdersAfterTheLock`); the rider's today (`/route/today`, the
`/route/complete` guard, `/inventory/today` and its session) is every stop due today,
whatever its state and whenever it was assigned (10.3), plus overdue open ones, never a
later day's (`bd68a11`; `TestRiderTodayKeepsOverdueStopsAndLeavesLaterDays`);
five racing sweeps (two instances at 12:00:05, a restart at 12:14:59) and three racing CRM
ticks leave one task per funded order, one skip and one `day_skipped` per short member and
one B-02 (`TestConcurrentNoonSweeps`). Fixed: `GET /stores/{id}/riders` `completed_today`
compared UTC dates; it now counts deliveries on the IST day (`deliveredOnISTDay`), so the
route's first stops before 05:30 IST count for today.

**CRM SMS caveats.**

1. `crmRegistrationFor` (SMS and WhatsApp): the routed template id is looked up first
   ("T-B05-PROMO"), then the trigger id, and the trigger id covers only the trigger's own
   template (plain, or a conditional's `then`). B-06's promo / referral credits no longer
   go out under the refund body's DLT id; with only `B-06` mapped they are inbox and push
   only. Existing trigger-keyed mappings (W-01, W-07) behave exactly as before.
2. The rider-undo credit's reason is "delivery 5D05C6 reversed" (`crmShortOrderCode`: the
   last six characters of the order id in capitals, 24 characters in all).
3. B-06 has `parallel: ["push"]`. Live service triggers that still name no push (unchanged,
   for the owner to decide): A-02, A-03, A-05, B-02, B-03, C-03, D-05, E-01, E-02, E-04,
   E-05 (human_call by design), E-06, W-06, W-07, W-08.
4. A channel that tried and failed (provider refusal, unknown outcome, content it could not
   render) is logged at ERROR with trigger, template, channel and role, and recorded on the
   dispatch row as `channel_errors` `[{channel, role, template, error (300 chars), transient}]`,
   shown by the operator's dispatch log (`GET /consumer/crm/dispatch-log/{phone}`, additive
   key).
   `channel` still names only what delivered. Unavailable channels stay a Warn; push to a
   member with no device is "no recipient" (`errCRMNoRecipient`), not an error.
5. `.env.example` / `render.yaml`: the values are MSG91's template ids, not the 19-digit DLT
   ids; keys are trigger or template ids; `CRM_MSG91_SENDER` is optional. B-06's SMS
   variables (`##x##`, `##reason##`) are in `docs/CRM-MESSAGES.md` section 3.

**What the apps see**

- Shipped app (`release/26.07.03`): a morning order after noon now succeeds instead of
  422, but its cart footer and order screen still say "tomorrow" (only D-01 names the
  moved day); a cancel of a locked morning order shows the ORDER_LOCKED message where it
  used to succeed; an alternate plan created after noon runs one day out of phase with the
  app's local calendar (G4, accepted). Its low-balance pause / resume flow is unchanged.
- New app (`feature/consumer-revamp-phase2`): the home card already reads `delivery_date`;
  lib seam still open: `placeOrder` should return the created order (or `delivery_date`,
  `requested_date`, `date_moved`) so the cart can say "moved to Thu 8 Oct"; the order
  screen's "tomorrow morning" copy is UI.
- Saathi: Upcoming reflects member changes on the next 12 s poll; tomorrow's tasks appear
  at the lock; a moved one-off order appears under its real day; `completed_today` is right
  before 05:30 IST. Rider badges still count later days (app follow-up).

**Tests** (`CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018`, injected clocks, simulated at
11:59, 12:00:01, 12:14:59, 23:59 and month end where the rule is time-dependent):
`subscriptions_write_sync_test.go`, `orders_cutoff_test.go` (rewritten:
`TestMorningOrderAfterNoonMovesToTheFirstOpenMorning`, `TestMovedMorningOrderOnTheWire`,
`TestD01NamesTheMovedDay`), `subscriptions_reanchor_test.go`, `orders_cancel_lock_test.go`,
`crm_pack1_noon_test.go`, `store_rider_day_test.go`, `subscriptions_concurrency_test.go`,
`crm_template_registration_test.go`, `crm_b06_push_test.go`, `crm_channel_errors_test.go`,
`crm_w01_noon_test.go`. Made date- and hour-independent because of G4 / G7 / G9: tests
that make plans for fixed October days now make them at `chainPlanMadeAt` (1 Sep); member
cancels run at an explicit moment; `planFromBeforeNoon` models a 09:00 enrolment fully;
the plan-variant and dry-run W-01 tests enrol at a fixed morning.

**Residuals, known**

- The referral rewards' remarks ("Referral reward: a family you invited took their first
  delivery", 64 characters) exceed a DLT variable's 30: shorten them before T-B05-PROMO is
  mapped for SMS.
- T-W01-LATER needs DLT and Meta registration before an afternoon enrolment's W-01 can
  leave by SMS or WhatsApp.
- The `/upcoming` trims the design offered as optional (skip the task lookup for previews,
  one `$in` address read) were not made; the poll is as `813099a` left it.
- `awaiting_funds` on `/upcoming` stays (true only in the seconds before the lock tick).

### 10.3 Review fixes (nr), 24 Sep evening

| commit | what |
|---|---|
| `109858d` | nr-1: two deciders racing one member-day (a second instance at 12:00:05, or a member's change racing the tick) no longer count one plan's order twice, so a plan the wallet covers is never skipped (`memberDayBudget`) |
| `042f098` | nr-2: D-07's `tomorrow_blocked` is decided once the member's whole day is decided, and only when no live morning order arrives that day (`memberMorningDeliveryDue`: a locked plan order, a one-off morning order, a Welcome Litre pack); the D-07 condition and B-02 read it again at send time |
| `460c441` | nr-3: B-02 waits until a sweep has run tomorrow's noon lock to the end. From noon, step 4b of `sweepSubscriptionOrders` writes `consumer_noon_locks {_id: <tomorrow>, ran_at}` once LOCK and CATCH-UP finished inside the tick's budget; `crmWalletHealthSweep` claims `B-02-SWEEP` only once that row exists. After a boot at 13:30 (or a wake at 12:30 with nothing previewed) a skipped member got B-02 and D-07; now D-07 alone |
| `a25262f` | nr-4: the rider's today counts every stop whose `delivery_date` is today, so a stop assigned the afternoon before and delivered or failed this morning stays in the header, the complete guard and the pickup sheet |
| `1972c77` | nr-7: the lock prices a 2+2 trial day by the trial as it stood at 12:00. Each delivered trial day is stamped `at` when it is charged at the door, and `phaseAsOf` plays the days charged after the lock moment back out, as `walletAsOf` does for the wallet |

- New collection `consumer_noon_locks`: one row per day, `_id` the day; no index needed,
  only the B-02 gate reads it. If no sweep after noon completes (every tick over its
  2-minute budget, or the worker not running), B-02 does not go out that day: it needs the
  lock to have run.
- A trial charge written before `1972c77` has no `at` and counts as before any lock. A
  trial stop marked after 12:00 counts from the next lock. The door still charges by the
  trial as it stands at delivery, so a trial stop that lands between 12:00 and the next
  morning can make the door's price differ from the lock's (before this, the same held
  between the tick and the morning).
- Changed because the rule changed: `TestCRMWelcomeLitreE2E` (7d) and
  `TestCRMMatrixEveryLiveTriggerFires` judge B-02 without driving the lock, so they record
  the lock as run (`noonLockRanAt`).
- Tests: `TestNoonLockCountsAPlanAnotherDeciderLockedOnce`,
  `TestRacingNoonLocksStaggeredFundBothPlans`, `TestMemberDayBudgetCountsACandidateOnce`,
  `TestConcurrentNoonSweeps` (a member whose wallet covers both plans),
  `TestCRMNoonCopyNoD07WhenAnotherDeliveryArrivesTomorrow`,
  `TestCRMNoonCopyB02WaitsForTheLockAfterABootPastNoon`,
  `TestRiderTodayKeepsOverdueStopsAndLeavesLaterDays` (extended),
  `TestWalletLockReadsTheTrialAsOfNoon`, `TestTrialChargeStampsTheDeliveredDay`,
  `TestTrialPhaseAsOfReplaysTheLedger`.

### 10.4 Merged beside the instant hours (section 11)

`feature/noon-rules` is merged into `integration/delivery` (`--no-ff`, no commit rewritten)
on top of the order guard `c0171d0` and the instant hours of section 11. Where the two meet:

- `POST /orders` (`createOrderAt`) decides the morning first (`morningDeliveryDate`: a
  closed morning moved to the first open one, 422 `BAD_DELIVERY_DATE` beyond 7 days, 400 for
  a malformed date), then the serviceability guard judges the order as it will be
  delivered: `NOT_SERVICEABLE` on either lane, `INSTANT_CLOSED` / `INSTANT_OUT_OF_RANGE` on
  the instant lane only. A morning order placed at 22:30 while instant is shut is moved and
  accepted; an address we do not serve is refused, never moved and stored. A date error now
  answers before `NOT_SERVICEABLE` (only a broken client sends one).
- The member's cancel (`cancelOrder`, 409 `ORDER_LOCKED`) reads the service clock
  (`s.now()`, section 11) as `createOrder` does, so a test that pins the clock drives both;
  production still reads the wall clock.
- Both branches made the catch-up leave a day whose route has left (`34b3fb4` on
  `integration/delivery`, and the noon lock here): the check is kept once. The doorstep-prefs
  and full-chain morning tests run at 03:00 on a fixed day.
- CRM: 52 templates (each branch added one, T-E04-REDELIVER and T-W01-LATER;
  `crm_tokens_test.go` pins it). Under 10.2's one registration per body, E-04's
  redelivery-only body T-E04-REDELIVER needs its own WhatsApp key: a key on `E-04` covers
  T-E04 only. `docs/CRM-MESSAGES.md`: B-06 promises no "Valid 30 days" (`49a9865`), in its
  SMS registration text as well; D-01 shows both the every-line label and the moved-day
  [ETA]; E-04 names both keys.
- Tests: `orders_noon_guard_test.go` (`TestOrderGuardJudgesTheMorningAsItWillBeDelivered`,
  `TestMemberCancelReadsTheServiceClock`). Everything now runs on the one Mongo at 27017
  (`CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017`); 27018 was the noon worktree's own.

---

## 11. Instant hours and the closing alert (added 24 Sep, after the stop)

Founder decision: no "the manager must press Save once" and no silent 24 h. At closing time
the store manager is asked to extend instant or let it close. Backend `integration/delivery`
`f1ebfc5`, `7ba129b`, `eee3474`, `6070032`; Saathi `integration/delivery` `620a0e8` (lib seam
only). Review fixes (ICR, same day): backend `94ddcbe`, `60ab660`, `a1ccec0`, `b19f545`,
`4b09206`, `7ffb118`, `803f0b3`; Saathi `4cfe9ef` (lib seam only).

**Rules now in force** (`geofence.go` `instantWindow`, `instant_hours.go`, `instant_alerts.go`):

- Hours that were never saved (`instant_close_min` 0) are the 07:00-22:00 IST the Zone tab
  shows, on `/serviceability`, in the `POST /orders` guard (422 `INSTANT_CLOSED`) and in the
  console. Saved hours are unchanged.
- A close saved as `0` through the PUT is midnight: stored as `1440`, echoed back as `0`, so the
  Zone tab's "Closes 12:00 AM" (its picker ends at 23:59) is 09:00-24:00, not 07:00-22:00, and a
  re-save is stable. All day = 12:00 AM to 12:00 AM (`0` / `0`, stored `0` / `1440`); an API
  client's `1440` still means midnight. Only a zone never saved (close `0` in the database)
  takes the 07:00-22:00 default.
- `INSTANT_TEST_OPEN` keeps its interaction: with a zone drawn it widens instant and the hours
  still gate it; with no zone drawn it is not hours-gated. With the flag on, a zone drawn with
  a standard radius only therefore has an instant lane: it gets the closing alerts and the
  manager can extend, close or reopen it. With the flag off nothing changed (no lane, 422).
- `POST /consumer/stores/{storeId}/zone/instant/extend {minutes: 30|60|120, request_id?}`
  (STORE_MANAGER, own store, 403 otherwise): open until the later of now / tonight's close /
  the current extension, plus the minutes, never past 02:00 IST. A compare-and-set, so two
  managers tapping together both count (409 `INSTANT_BUSY` only after 5 lost races).
  `request_id` (or `requestId`, at most 100 chars, else 422 `INVALID_REQUEST_ID`): minted by
  the client once per tap and reused on a retry; a repeated one answers the zone unchanged.
  422 `INSTANT_PAUSED`, `INSTANT_NOT_CONFIGURED` (no zone or no instant lane),
  `EXTEND_TOO_LATE`, `INVALID_EXTENSION`.
- `POST .../zone/instant/close-now`: shut until the next opening time, extension cleared,
  `instant_paused` untouched, so it reopens by itself. `POST .../zone/instant/reopen` undoes
  it at once (clears the close-now only; tonight's close is not moved and after the hours it
  stays shut; 200 unchanged when there is nothing to undo; 422 `INSTANT_PAUSED`,
  `INSTANT_NOT_CONFIGURED`). Extend and close-now replace each other; the zone PUT never
  touches either. All answer the GET `/zone` shape, which gains `instantOpenNow`,
  `instantClosesAt`, `instantExtendedUntil`, `instantClosedUntil` (+ snake);
  `instantOpenNow` is false and `instantClosesAt` null for a zone with no instant lane or an
  inactive zone.
- After a close-now, `/serviceability`'s "resumes ..." names its end when the hours are open
  then, else the first opening after it (it used to be a day off when the opening moved
  earlier).
- The pause switch still means "until I turn it back on". `/serviceability` now labels a
  paused lane "when the store turns it back on" (it used to promise "tomorrow at 7:00 AM").
- Alerts: a one-minute worker writes to the store's ACTIVE STORE_MANAGERs only, into the inbox
  Saathi's bell polls (`GET /notifications/me`, every 45 s while the app is open), on the
  STORE_LOW_STOCK model. `STORE_INSTANT_CLOSING` 15 min before the close (again before an
  extended close); where extend would refuse (a close at or past the 02:00 cap: an 18:00-02:00
  window, a lane already extended to 02:00, a 22:00-06:00 window) its message is "It cannot be
  extended any further. Orders already placed are not affected." `STORE_INSTANT_CLOSED` at the
  close if not extended (within the hour). Params: `headline`, `message`, `store`, `store_id`,
  `window_close`, `window_reopen`. Never for a paused lane, no instant lane, a close-now, or
  00:00-24:00. Exactly once per
  (store, kind, close) via the unique index on `store_instant_alerts`, across instances and
  restarts. In-app only: the Saathi app has no phone push, so a manager whose app is closed
  sees it on opening the app.

**Behaviour table** (a store with unsaved hours or saved 07:00-22:00; times IST; the consumer
app is `pyaas-consumer` as shipped, whose `lib/instantHours.ts` adds its own 06:00-23:00 floor):

| Time | Consumer app shows | Store manager gets |
|---|---|---|
| 23:00-05:59 | "Instant opens at 6:00 AM" (the app's own floor; the backend opens at 07:00) | nothing |
| 06:00-06:59 | "Instant resumes today at 7:00 AM" | nothing |
| 07:00-21:44 | Instant offered | nothing |
| 21:45 | Instant offered | "Instant delivery closes at 10:00 PM" + "Keep instant open tonight: extend by 30 min, 1 h or 2 h from the Zone tab, or let it close. Orders already placed are not affected." |
| 22:00, no action | "Instant resumes tomorrow at 7:00 AM"; an instant order is refused (422 `INSTANT_CLOSED`); morning unaffected | "Instant delivery is now closed" + "It reopens at 7:00 AM." |
| Extend 1 h at 21:50 | Instant offered until 23:00, then "Instant resumes tomorrow at 7:00 AM" | no 22:00 "now closed"; "closes at 11:00 PM" at 22:45; "now closed" at 23:00 |
| Extend past 23:00 (up to 02:00) | Hidden from 23:00 by the app's floor ("Instant opens at 6:00 AM") although the backend still accepts instant orders | "closes at" 15 min before the extended close, "now closed" at it |
| Close now at 21:00 | "Instant resumes tomorrow at 7:00 AM" from 21:00; back at 07:00 by itself | nothing that night (they closed it) |
| Close now at 10:00, reopen at 11:00 | Instant offered again from 11:00 until 22:00 | "closes at 10:00 PM" at 21:45, "now closed" at 22:00 |
| Saved 18:00-02:00, at 01:45 | Instant offered (after 23:00 hidden by the app's floor) | "closes at 2:00 AM" + "It cannot be extended any further. ..." |
| Pause switch on | "Instant resumes when the store turns it back on"; shut past 07:00 until switched off | nothing |
| Saved 00:00-24:00 | Instant offered whenever the app's floor is open | nothing |

**Kushagra UI follow-up (Saathi; nothing on screen changed):**

1. `lib/screens/notifications_screen.dart` `_render`: add `STORE_INSTANT_CLOSING` and
   `STORE_INSTANT_CLOSED` (title `p['headline']`, body `p['message']`). Until then they fall to
   the default branch: the raw key as the title and the params in key order as the body
   ("headline: ...", then "message: ...", two lines).
2. Same file, the row's `onTap` (today it only marks read): for `STORE_INSTANT_*` open the
   store console on the Zone tab (`store_home.dart` `ModuleShell`, tab index 2; ModuleShell
   has no initial-tab parameter yet), or offer Extend 30 min / 1 h / 2 h and Close now in the
   row via `ZoneApi.extendInstant(p['store_id'], m, requestId: <one per tap>)` /
   `ZoneApi.closeInstantNow(p['store_id'])`. Hide Extend when `p['message']` is the
   "cannot be extended any further" copy.
3. `lib/screens/store/zone_tab.dart`, "Instant delivery hours" card, beside the "Close instant
   now" switch: the one-tap buttons (`ZoneApi.extendInstant` 30/60/120 with a `requestId`
   minted per tap and reused on a retry, `ZoneApi.closeInstantNow`, and "Reopen now" via
   `ZoneApi.reopenInstant` while `instantClosedUntil` is set), `_zone = returned zone` after
   the call, the state from `instantOpenNow` / `instantClosesAt` / `instantExtendedUntil` /
   `instantClosedUntil` (UTC, show in IST), and the `ApiException` message on 409/422.
4. Same card, the switch subtitle "Instant is shut until you turn this off or the next opening
   time" is false (the pause holds until turned off): "Instant is shut until you turn this off".

**Consumer app follow-up (not Saathi):** the `lib/instantHours.ts` floor (06:00-23:00,
`EXPO_PUBLIC_INSTANT_OPEN_HOUR` / `_CLOSE_HOUR`) disagrees with the backend default: 00:00-06:00
says 6:00 AM for a lane that opens at 7:00, and an extension past 23:00 never shows. Set the
open hour to 7 and let a backend `instant: true` pass the close floor (or move it to 2).

Verify: `go test ./internal/modules/consumer/ -run 'TestInstant|TestOrderGuard' -count=1 -v`
(Mongo on 27017, fixed IST clock, no wall-clock dependence); Saathi `flutter test
test/zone_api_test.dart`.
