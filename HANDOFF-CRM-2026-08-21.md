# CRM Welcome Litre — Developer Handoff

**Date:** 21 Aug 2026 · **Status:** built, reviewed, E2E-tested, **NOT deployed** (safe behind branch + env gate)

---

## TL;DR

The CRM Rev3 "Welcome Litre" campaign is fully implemented across backend + consumer app:
free ₹0 pack at enrolment → settled ₹500 recharge within 7 days → second free pack → else a
mandatory "nothing has been charged" expiry message. Plus general wallet-health messages
(B-01/B-02) for **every** subscriber. Everything is inert until `CRM_ENABLED=true` is set on the
server — with it off, the binary behaves byte-identically to the pre-CRM build (proven by the full
test suite + live app testing).

| Repo | Branch | Commits | State |
|---|---|---|---|
| `parag-saathi-be` | `feature/crm-welcome-litre` | `31902ae` feat · `e34f472` fix (23 review findings) · `cd30bdd` B-triggers | pushed, **not merged** — Render pins `release/26.07.03`, so nothing is live |
| `pyaas-consumer` | `feature/consumer-revamp-phase2` | `1267a19` | pushed — **inert against the deployed backend** (CRM routes 404 → every surface hides itself) |

---

## Campaign flow (what a household experiences)

1. **Enrol** (promoter / store manager, via the operator console): creates the account
   (canonical `+91…` phone — same doc the app login resolves), a normal daily subscription
   (2 × gold-500ml = 1 L/day), and a **standalone ₹0 promotional order** for pack 1 next morning.
   W-01 welcome message lands in the in-app inbox.
2. **Pack 1 delivered** (real rider settle; the ₹0 task writes a ₹0 wallet gate row so the app's
   settle sweep can never back-charge it) → offer state `pack1=delivered`, W-02.
3. **Recharge ≥ ₹500 (settled)** any time up to and including day 7 — or even **before** pack 1
   lands → pack 2 unlocks, its ₹0 order is minted for next morning, W-04. The daily plan starts
   shipping the moment it's funded (the sweep never pauses an unfunded subscription — it just
   skips days and retries every tick).
4. **No recharge by day 7** → day 8 morning: W-07 with the **mandatory** line
   "Nothing has been charged" / "Koi shulk nahi laga hai", then the offer expires. A later
   recharge does NOT resurrect it (CAS from `locked` only).

## Load-bearing design decisions (don't undo these)

- **The free pack is a STANDALONE ₹0 order with NO `subscription_id`.** The settle sweep, the 2+2
  trial engine, and every wallet floor are structurally blind to it — zero shared-code edits were
  needed. Promo fields (`is_promotional`, `promotional_value`) are server-minted only; the client
  price authority (rejects price ≤ 0 from clients) is untouched.
- **Offer-first enrolment arbitration.** The offer doc (unique `consumer_id + offer_id`) is
  inserted **before** any minting; a concurrent double-enrol creates nothing, and an interrupted
  enrolment **resumes** on retry instead of erroring.
- **Event outbox is at-least-once.** `crm_events`: `NEW → PROCESSING` (leased, 10-min crash
  reaper) `→ DONE`, back to `NEW` on transient error, `FAILED` after 5 attempts. All handlers are
  CAS-idempotent, so replays are harmless — losing a settled recharge was not.
- **One IST-day predicate family for all deadlines.** Recharge unlock, W-06's advertised date,
  and the expiry sweep all use `daysSinceFirstDelivery` — day 7 always honours the recharge, the
  sweep expires only from day 8, and a recharge before first delivery unlocks too.
- **W-07 dispatches BEFORE the irreversible CAS** (dispatch-log claim + per-offer cap keep it
  exactly-once) — a transient send failure retries next tick instead of silently losing the one
  message the spec makes non-negotiable.
- **Canonical phone.** Any server-side account create/lookup must use `crmCanonicalPhone()`
  (`+91` + 10 digits) — the OTP login keys accounts that way. A bare-10-digit key orphans the
  whole offer behind a duplicate account.
- **2+2 exclusivity (Option B).** Enrolment refuses anyone with paid order history or trial
  activity, and exhausts the enrollee's 2+2 ledger so the offers can never stack.

---

## Backend map (`internal/modules/consumer/`)

| File | What it owns |
|---|---|
| `crm_offers.go` | Offer state machine (CAS-only transitions, append-only log), enrolment, ₹0 pack minting, recharge unlock, abuse address-hash flag (flag + admin notify, never reject) |
| `crm_engine.go` | `go:embed crm_triggers.json` (54-trigger Rev3 config), guard chain G1–G10, dispatch log, in-app channel, event outbox + 1-min worker, schedule sweeps (W + B triggers), 18:00 reconciliation |
| `crm_http.go` | Consumer routes + the operator console (below) |
| `crm_triggers.json` | Byte-copy of the Rev3 spec config (embedded at compile time) |
| `crm_test.go` / `crm_e2e_integration_test.go` | Unit pins + the full-journey Mongo E2E |

**Mongo collections (all in the main DB — the full audit trail the founder asked for):**

- `consumer_offers` — per-household campaign state + every transition (who enrolled, every
  override with mandatory reason + operator id)
- `crm_dispatch_log` — one row per message attempt: trigger, consumer, template, IST day,
  `SENT` / `SUPPRESSED` + which guard blocked it. Unique `(trigger, consumer, ist_day)` = the
  exactly-once claim
- `consumer_inbox` — delivered message bodies (EN + Hindi), CTA, `read_at`
- `crm_events` — the cause trail (recharge settled, pack delivered) with status + attempts

**Routes** (mounted under `/api/v1/consumer`):

- Consumer (JWT): `GET /crm/inbox` · `POST /crm/inbox/{id}/read` · `GET /crm/offer`
  (whitelisted view — never leaks abuse flag / operator ids / promoter attribution)
- Operator console, aka **the handover profile** (`STORE_MANAGER` or `SUPER_ADMIN` role token):
  - `POST /crm/enrol` `{phone, name, line1, pincode, lat, lng, society_id, promoter_id, asset_type}`
  - `POST /crm/message` `{phone, body_en, body_hi, category}` — manual send through the FULL guard
    chain; category is a closed enum, anything but `service_implicit` runs promotional guards
  - `GET /crm/offers/{phone}` (full doc) · `POST /crm/offers/{phone}/override`
    `{pack_no, from, to, reason}` (explicit CAS, reason mandatory)
  - `GET /crm/dispatch-log/{phone}` · `POST /crm/flags` `{trigger_id, enabled}` (per-trigger kill switch)

**Message triggers currently live:**

- **W-01…W-07** — the Welcome Litre journey (enrol / delivered / day-0 nudges / unlock / complete / day-3+5 nudges / expiry)
- **B-01** (09:00 IST): any active subscriber with < 4 days of wallet cover — max once per 7 days
- **B-02** (17:00 IST, critical): balance can't cover tomorrow's subscription day — once per day,
  re-fires daily while short. Live Welcome Litre households are excluded (W-03a/W-06 own them)
- **W-09/W-10** internal: abuse flags + daily reconciliation → operator notifications collection
- Guard chain applies to everything incl. manual sends: kill switches, promo consent (with
  TCCCPR TTL), opt-out, quiet hours 10:00–21:00, frequency caps, 30-min dedup, template registry

**Env (documented in `.env.example`):**

```
CRM_ENABLED=true          # master switch — everything is a no-op without it
CRM_MSG91_AUTHKEY=        # Phase B — SMS channel (pending keys from founder)
CRM_WA_TOKEN=             # Phase B — WhatsApp channel
CRM_WA_PHONE_ID=
```

---

## Consumer app map (`pyaas-consumer`)

| File | Change |
|---|---|
| `lib/crm.ts` (new) | Defensive API seam + unread store. **Contract: every CRM call degrades to `[]` / `null` / `0` on 404 / old backend / CRM off / offline — never throws to a screen.** 401/403/404 are authoritative (bell clears); network blips keep the last count |
| `app/inbox.tsx` (new) | Messages screen — bilingual (DiscLang toggle), unread dot, mark-read on open, CTA routing (recharge / shop / track / refer) |
| `components/HomeHeader.tsx` | Bell chip rendered **only while unread > 0** — header is byte-identical for everyone else |
| `app/(tabs)/wallet.tsx` | "1 free delivery on us — nothing charged" chip on the balance card (only for entitled households) |
| `app/(tabs)/profile.tsx` | Messages tile in the Rewards grid |
| `lib/subscriptions.ts` | `syncServerSubscriptions()` — **ADD-ONLY** merge of backend-created plans into the local cache (the campaign plan is the first subscription the app didn't create itself). Never touches/removes local rows |
| `lib/freePack.ts` | `freePackShowEligible()` now refuses CRM-enrolled households — the 2+2 funnel can never stack on the campaign |

**Sales-funnel guarantee (live-verified on emulator, both directions):** a fresh non-enrolled
user sees the exact pre-CRM app — 2+2 FREE TRIAL funnel, no bell, no chips. An enrolled household
sees bell/inbox/wallet chip and is never pitched the 2+2 (two independent guards).

---

## How to run and test

```bash
# Backend — full suite (13 packages must stay green)
cd ~/parag-saathi-be && go test ./...

# Full-journey CRM E2E against a real local Mongo (brew services start mongodb-community)
CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
  go test ./internal/modules/consumer/ -run TestCRMWelcomeLitreE2E -v -count=1

# Local server with CRM on (scratch DB — never point this at prod)
MONGO_URI=mongodb://localhost:27017 MONGO_DB=saathi_crm_dev \
  JWT_SECRET=<any-32-chars> ENV=dev CRM_ENABLED=true PORT=8080 go run ./cmd/server
go run ./cmd/seed -minimal   # seeds store manager 8307474208 (role token needed for /crm/*)
```

The E2E covers: enrol → ₹0 pack + delivery task → real rider settle (gate row) → W-02 →
recharge → pack-2 mint + W-04 → deliver → W-05 → day-0/3/5 nudges dedup → day-7 recharge honoured
→ day-8 expiry + W-07 mandatory line → post-expiry recharge does NOT resurrect → abuse flag →
B-01/B-02 for a normal subscriber → CRM-off inertness.

**App side:** debug build + Metro against a local backend (`EXPO_PUBLIC_API_URL=http://10.0.2.2:<port>/api/v1/consumer`
in `.env` — remember the Metro transform-cache trap: env changes need `npx expo start --clear`).
Note: the `parag_pixel` emulator currently has a **debug** build of `in.pyaasdairy.app` installed
(replaced the release build during testing). Reinstall the release APK/AAB if you need it.

---

## Go-live checklist (when the founder says go)

1. Merge `feature/crm-welcome-litre` → `release/26.07.03` (Render auto-deploys that branch).
2. Set `CRM_ENABLED=true` in Render env. Consider the Starter (always-on) plan — the free tier
   sleeps and the 09:00/10:30/17:00 sweeps need a running worker.
3. Ship the consumer app build from `feature/consumer-revamp-phase2` (the CRM surfaces
   self-activate the moment the backend answers; against the old backend they stay hidden).
4. Wire Phase B channels once MSG91 + WhatsApp keys arrive (env names above) — the `crmChannel`
   interface in `crm_engine.go` is the seam; in-app inbox keeps working regardless.
5. Optional: per-trigger kill switches at runtime via `POST /crm/flags` — no deploy needed.

## Open items / in flight

- **MSG91 authkey + WhatsApp token/phone-id** — awaited from the founder; SMS/WhatsApp channel
  implementation is the only unbuilt piece of the messaging stack.
- **Persistence audit** (everything-on-DB-not-local) — in progress. Verified so far: wallet,
  orders, trial money-core, CRM = fully backend-owned. Known fragilities being fixed:
  vacation mirror is fire-and-forget (a failed PATCH silently under-syncs; a reinstall can
  overwrite server vacations with an empty list) and the 2+2 claim registry is device-local
  (the code's own `TODO(api)` — server-side per-phone claim endpoint is the fix).
- 2+2 spec decision deferred by founder: current paid-first 2+2 stays; CRM exclusivity is
  Option B (switchable later without conflict).

## Standing invariants (do not break)

- Play reviewer login `9999900000` / OTP `123456` stays hardcoded and always on; wallet ₹500 for it is intentional.
- PYAAS Plus ₹99 stays as is.
- Never modify Dolibarr ERP structure; the curated SKU map only removes from OUR catalog.
- `parag-saathi-fe` is an abandoned repo — never edit it. The Flutter store/rider app is `pyaas-saathi`.
- Backend price authority: client-supplied prices ≤ 0 are rejected; only the server mints ₹0 promo lines.
