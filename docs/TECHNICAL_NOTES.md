# Saathi Backend — Technical Notes & Handoff

> **Audience:** the next engineer or LLM working on this repo. Read this file plus
> `SAATHI_Architecture_Framework_Parag.pdf` (the blueprint, source of truth) and you have the
> complete picture. Blueprint section references look like (§8.3).
>
> **Status:** P0 + P1 implemented end-to-end (identity → pour → invoice → logistics → safety
> gate → batch → QR → public trace → settlement), with P2 foundations (cattle/health, MVU,
> education, dormant collar path). Consumer commerce is **deliberately descoped** (dormant flag).

---

## 1. Stack & system design decision

| Decision | Choice | Why |
|---|---|---|
| Language | **Go 1.23+** | memory-safe/typed per §15; single static binary; trivial horizontal scaling for 1cr+ users |
| Database | **MongoDB** (env-configured URI only) | team decision; normalized ("scattered") collections with ID references, not embedded blobs |
| Architecture | **Modular monolith** with hard module seams | §6 prescribes event-driven microservices as end-state; at greenfield stage one deployable with 10 isolated modules + an in-process event bus gives the same boundaries with none of the ops tax. Each module = one future microservice. |
| HTTP | chi router, REST JSON under `/api/v1` | frontend lives in a separate repo; this is a pure API |
| Events | in-process pub/sub (`internal/platform/eventbus`) | mirrors a broker API; swap to Kafka/NATS (§15) without touching module code |
| Provenance | **hash-chained append-only event ledger in MongoDB** (§7.3 "ledger-style" option) | tamper-evident without running a graph DB; graph traversal implemented over `refs` edges; can be projected to Neo4j later |
| IDs | **UUID strings**, not ObjectIDs | offline-first devices (§3.1) must mint IDs locally with zero coordination; string IDs also travel cleanly through JSON. References still work exactly like ObjectID refs (`farmer_party_id`, `dcs_id`, …) |

### Scaling posture (1cr+ users)
- Stateless server → run N replicas behind a gateway/LB; per-IP rate limiting is per-replica by
  design (global fairness belongs to the WAF/gateway tier, §6).
- Every hot query path has a compound index (see `internal/platform/mongodb/indexes.go` — the
  single index catalog). No collection scans on request paths.
- In-memory caches with short TTLs for org-tree (60s) and feature flags (30s) — the two
  read-on-every-request lookups.
- Write bursts (twice-daily pour shifts, §16) are absorbed by: connection pool 200, idempotent
  ingest (unique `client_event_id`), and batch sync endpoint.
- Mongo can be sharded later (shard keys would be `dcs_id`/date for pours); the provenance
  ledger's global sequence is the one serialization point — at true national scale, switch
  `Append` to per-entity chains or a partitioned ledger (documented trade-off, §20-2).

## 2. Repository layout

```
cmd/server/          boot: config → logger → mongo → indexes → deps → router (graceful shutdown)
cmd/seed/            idempotent demo/dev data (org tree, parties per role, rate chart, animals)
internal/config/     env-only config (.env supported; MONGO_URI required, no in-code default)
internal/domain/     THE CONTRACT: pure entities/enums/invariants; no deps; modules depend on it
internal/platform/   shared services: httpx, auth (JWT/OTP/HMAC), middleware (authn/rbac/audit/
                     ratelimit/metrics), mongodb (client+index catalog), provenance (hash-chain
                     ledger), orgscope (path-based scope checks), flags, audit, eventbus, deps
internal/httpapi/    router assembly + module registration
internal/modules/    10 domain modules (identity, orgs, cattle, collection, logistics, plant,
                     quality, settlement, publictrace, platformops)
scripts/smoke.sh     end-to-end test of the entire loop over live HTTP + MongoDB
```

**Module contract** (enforced): `Register(r chi.Router, d *deps.Deps)`; layering
`handler.go` (HTTP only) → `service.go` (business logic) → `repo.go` (Mongo only) + `models.go`
(DTOs). Modules never import each other; cross-module reactions go through the event bus;
collection names only via `mongodb.Coll*` consts.

## 3. Identity model (§4) — how auth works

1. `POST /api/v1/auth/otp/request {phone}` → 6-digit OTP (stored **HMAC-hashed**, TTL, max 5
   attempts). Dev mode (`OTP_DEV_MODE=true`) returns `dev_otp` in the response.
2. `POST /api/v1/auth/otp/verify` → finds-or-creates the **Party** (one phone = one human) →
   returns a **session JWT** + rotating **refresh token** (stored hashed).
3. `GET /api/v1/auth/roles` → the party's org-scoped **RoleAssignments** (23-role catalog §5.2).
4. `POST /api/v1/auth/role/select {role_assignment_id}` → **role JWT** pinned to
   role+org (this IS the role switcher; KYC tier gates apply, §4.2).
5. Protected routes: `middleware.Authenticate` (JWT) → `middleware.RequireRoles(...)`
   (SUPER_ADMIN passes all gates; every mutation is audit-logged) → `orgscope.RequireInScope`
   (path-array ancestry check, O(1)).

## 4. The core loop (what the smoke test drives)

```
analyzer reading (anti-tamper: geotag/device-time/OCR-confidence/plausibility §8.2)
→ pour (idempotent client_event_id; priced off org-scoped rate chart; SMS receipt) 
→ same-day invoice (unique farmer+dcs+date)
→ consignment (DCS day+shift aggregate = the pooling boundary §7.4)
→ route trip (pickup temp, cold-chain log, deliver to BMC)
→ BMC lot (pool consignments, chill temp) → QC BMC_RAPID   ── FSSAI gate §8.3
→ processing batch (only PASSED lots can enter)  → QC PLANT_LAB ── gate again
→ product lot (only COMPLETED batches) → QR (HMAC-signed, forgery-proof)
→ PUBLIC scan /api/v1/public/qr/{code} → honest samiti-SET provenance + QC certificate
→ settlement: initiate (Sacheev) → approve (DIFFERENT person — dual control §8.1) 
  → execute (mock licensed-PA) → farmer paid, UTR, SMS
```

Every hop appends a **provenance event** to the hash chain with `refs` edges; the public scan
returns `ledger.intact` from re-verifying the chain segment. Corrections (pour supersede) are
new events referencing the old — nothing is ever edited in place (§3.4).

**FSSAI limits enforced in software** (`internal/domain/quality.go`): AFM1 ≤ 0.5 µg/kg,
coliform ≤ 10 CFU/ml, tetracycline MRL 0.1 mg/kg, phosphatase must be negative. A failed subject
is BLOCKED and *every* forward path re-checks status (dispatch, batch creation, product lot, QR).

## 5. Hashing / crypto inventory

| What | Mechanism | Where |
|---|---|---|
| OTP codes | HMAC-SHA256(secret, phone, code) — DB never sees plaintext | `platform/auth/otp.go` |
| Refresh tokens | opaque 32-byte random, stored as HMAC digest, rotated on every refresh | identity module |
| Provenance chain | SHA-256 over (prev_hash, seq, type, entity, refs, actor, payload, ts) | `platform/provenance/ledger.go` |
| QR tokens | HMAC-SHA256 signature over (qr_code, product_lot_id, issued_at) | plant module + public verify |
| JWT | HS256, 15-min access, kind=session/role | `platform/auth/jwt.go` |
| Aadhaar | **never stored** — last-4 + mock Data-Vault ref only (§18-A) | identity/KYC |

## 6. What is mocked (clearly labelled in code) vs real

**Real:** everything in the loop above — auth, RBAC, org scoping, pricing, idempotent ingest,
gate logic, hash-chained ledger, trace, settlements state machine, audit trail, flags, metrics.

**Mocked (intentional — DPI integrations are Phase-gated, §13):** Aadhaar eKYC (always verifies,
stores masked ref), bank penny-drop (0.92 name-match), Bharat Pashudhan sync (marks SYNCED),
PFMS/DBT (ref generation only), licensed-PA payout execution (instant SUCCESS + mock UTR),
SMS provider (worker marks SENT). Each mock is a single clearly named function — replacing it
with the real connector does not touch the surrounding flow.

**Dormant (capability-gated, OFF):** collar telemetry (`collar_telemetry_enabled` §9), ONDC,
consumer commerce (descoped by product decision — only the public QR scan is consumer-facing).

## 7. How to run

```bash
# prerequisites: Go 1.23+, MongoDB running locally (or any URI)
cp .env.example .env             # then set MONGO_URI (env is the ONLY source)
make tidy                        # deps
make run                         # start API on :8080
make seed                        # demo org tree + parties + rate chart (idempotent)
make test                        # unit tests
make smoke                       # full E2E loop against a scratch DB (starts its own server)
# docker alternative:
make docker-up                   # mongo + api via compose
```

Demo phones (OTP_DEV_MODE returns the OTP in the response): super-admin 9999999999,
sacheev 9000000001, adhyaksh 9000000002, farmer 9000000011, rider 9000000021,
bmc-op 9000000031, plant-op 9000000041, lab 9000000042.

## 8. What to do next (ordered)

1. **Real DPI connectors** behind the existing mock seams: Aadhaar KUA/AUA, penny-drop PA,
   SMS/IVR DLT gateway, PFMS, Bharat Pashudhan. One module file each; contracts already shaped.
2. **Move OTP/SMS + notification worker to a real queue** (the outbox collection is already the
   queue table; today a manual worker endpoint drains it).
3. **Mongo replica set** in any real environment → then optionally wrap multi-doc updates
   (consignment↔pour, settlement↔invoices) in transactions; current design is safe without them
   (unique indexes + status guards) but transactions tighten crash windows.
4. **Object store** (S3-compatible, in-India §14) for analyzer photos/PoD — `photo_object_key`
   fields already exist; add a presigned-upload endpoint.
5. **Split-out path when scale demands:** each `internal/modules/*` maps 1:1 to a service;
   replace `eventbus` with Kafka/NATS; the `deps` container becomes per-service wiring.
6. **Compliance work items** (§18): DPIA + SDF confirmation, CERT-In log shipping (JSON logs
   ready), VAPT before go-live, Legal Metrology verification flow for analyzers/scales.
7. **Open blueprint decisions still open** (§20): NDDB AMCS federate-vs-build, provenance store
   final form at national scale, settlement authority final approval chain.

## 9. Conventions for future contributors (humans & LLMs)

- **Never** edit `internal/domain` semantics casually — modules and the frontend contract depend
  on it; additive changes only, statuses are closed sets.
- New endpoint? Follow the module layering; RBAC via `RequireRoles` + `RequireInScope`; return
  `httpx` envelopes; add the index for any new query shape to `indexes.go`; append provenance
  events for anything traceability-relevant; emit bus topics for cross-module effects.
- Provenance discipline: **no updates, no deletes** on `provenance_events` — corrections are new
  events with a `supersedes` ref.
- All secrets/URIs via env only. `.env` is gitignored; `.env.example` documents every variable.
- Tests: pure logic gets table-driven unit tests next to the code; the loop is guarded by
  `scripts/smoke.sh` — extend it when you extend the loop.
