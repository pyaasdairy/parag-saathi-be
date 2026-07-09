# Saathi Backend — How It Works (Flow Diagrams)

> **Purpose:** a visual map of how the backend behaves *right now*, so you can review it, spot gaps,
> and decide where to intervene. Every diagram reflects the actual code in `internal/` and `cmd/`.
> Companion docs: [`strict_brief.md`](strict_brief.md) (per-endpoint contract),
> [`WORKFLOW_GUIDE.md`](WORKFLOW_GUIDE.md) (role flows), [`TECHNICAL_NOTES.md`](TECHNICAL_NOTES.md)
> (architecture decisions).
>
> Diagrams are Mermaid — they render in GitHub, GitLab and VS Code.

---

## 1. The whole system at a glance

```mermaid
flowchart TB
    subgraph clients["Clients (separate repos)"]
        FE["Farmer / Sacheev / Admin apps\n(role-based dashboards)"]
        PUB["Public QR scan\n(no login)"]
    end

    subgraph api["Saathi API (Go modular monolith — one deployable)"]
        direction TB
        RTR["HTTP Router + global middleware\n(internal/httpapi)"]
        MODS["10 domain modules\n(internal/modules/*)"]
        PLAT["Platform services\n(internal/platform/*)"]
        RTR --> MODS --> PLAT
    end

    subgraph stores["State"]
        MONGO[("MongoDB\n28 collections\n+ provenance ledger")]
        REDIS[("Redis (optional)\nshared rate-limit / future SSE fan-out")]
    end

    FE -->|"REST JSON /api/v1 + JWT"| RTR
    FE -.->|"SSE /events/stream (live badge)"| RTR
    PUB -->|"GET /public/qr/... (no auth)"| RTR
    PLAT --> MONGO
    PLAT --> REDIS

    EXT["Mocked DPI edge:\nAadhaar · Bank · PFMS/DBT ·\nBharat Pashudhan · SMS · Payment Aggregator"]
    PLAT -.->|"single named seams,\nswap mock → real"| EXT
```

**Read this as:** clients hit one Go service over REST (+ an SSE stream for live updates); the service
is internally split into 10 isolated modules over shared platform services; all state lives in MongoDB
(Redis is optional, only for cross-replica concerns); external gov systems sit behind mock seams.

---

## 2. The global request pipeline (every request passes through this)

Order is exactly as wired in `internal/httpapi/router.go`.

```mermaid
flowchart LR
    IN(["HTTP request"]) --> RID["RequestID\n(correlation id)"]
    RID --> RIP["RealIP"]
    RIP --> MET["Metrics\n(Prometheus)"]
    MET --> LOG["RequestLogger\n(1 structured line/request)"]
    LOG --> RL["RateLimit\n(memory or Redis)"]
    RL --> REC["Recoverer\n(panic → 500)"]
    REC --> TO["Timeout 30s"]
    TO --> BR{"route?"}
    BR -->|"/healthz /readyz\n/metrics /version"| OPS["operational\n(no auth)"]
    BR -->|"/api/v1/public/*"| PUBH["public handlers\n(no auth)"]
    BR -->|"/api/v1/events/stream"| SSEH["Authenticate → SSE stream"]
    BR -->|"/api/v1/* (modules)"| AUD["AuditMutations\n(writes only)"] --> MOD["module route\n(auth + RBAC)"]
    BR -->|"unmatched"| NF["JSON 404 / 405\n(never chi plaintext)"]
```

Key facts to note for analysis:
- **Rate limiting runs before auth** (cheap abuse protection). It's per-IP; `REDIS_URL` makes it shared across replicas.
- **AuditMutations** wraps every state-changing `/api/v1` call — the immutable who-did-what trail.
- **Public routes and the operational endpoints deliberately skip authentication.**
- A single `request_id` threads through the request log and every module log line — that's how you trace one request end-to-end.

---

## 3. Inside a request — the layered flow (handler → service → repo)

Every module follows the same four-file contract. Example: recording a milk pour.

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant MW as Middleware<br/>(auth + RBAC)
    participant H as handler.go<br/>(HTTP only)
    participant S as service.go<br/>(business logic)
    participant R as repo.go<br/>(Mongo only)
    participant L as Ledger / Bus / Audit
    participant DB as MongoDB

    C->>MW: POST /api/v1/collection/pours + role JWT
    MW->>MW: verify token → Actor on ctx;<br/>RequireRoles(SACHEEV, MILK_TESTER)
    MW->>H: (authorised)
    H->>H: decode JSON, parse ObjectIDs (httpx)
    H->>S: CreatePour(ctx, actor, req)
    S->>S: RequireInScope(dcs) · farmer is member? ·<br/>price via rate chart · plausibility gate
    S->>R: idempotency check (client_event_id)
    R->>DB: findOne / insert
    S->>L: append pour.recorded · publish bus · queue SMS
    L->>DB: provenance event (hash-chained)
    S-->>H: pour struct or *AppError
    H-->>C: JSON envelope {data:...} or {error:...}
```

The rule the whole codebase obeys: **handlers do HTTP, services do logic, repos do Mongo, and modules
never import each other** — cross-module effects go through the event bus.

---

## 4. Identity: login → KYC approval → position → dashboard

This is the onboarding spine (the "which UI does the user get" question).

```mermaid
flowchart TD
    A["POST /auth/otp/request\n+ /auth/otp/verify"] --> B["Party created\ntier MINIMAL · no roles"]
    B --> C{"has a usable role?"}
    C -->|no| D["Frontend shows\n'pending / submit KYC'"]
    D --> E["POST /kyc/aadhaar\n→ KYCRecord PENDING"]
    E --> F["Reviewer: GET /kyc/pending\n(Organising Manager / admin)"]
    F --> G["POST /kyc/{id}/approve\n(atomic: status=PENDING guard)"]
    G --> H["party tier upgraded\n(upward only)"]
    H --> I["Admin: POST /roles/assignments\n= assign the POSITION\n(role_code @ org unit)"]
    I --> C
    C -->|yes| J["GET /auth/roles"]
    J --> K["POST /auth/role/select\n(checks KYC tier ≥ required)"]
    K --> L["ROLE JWT: role_code + org scope"]
    L --> M["Frontend renders THAT dashboard\nfrom role_code"]
```

Analysis hooks:
- A role token is **only** issuable once KYC tier satisfies the role (`403 KYC_TIER_INSUFFICIENT` before approval).
- Position = a `RoleAssignment` row (not a field on the user), so one human can hold several and switch.
- **Open question flagged earlier:** a brand-new party with no assignment is only visible to federation-wide reviewers (Super Admin / PCDF Admin), not a scoped Organising Manager — first-touch KYC routing is a product decision.

---

## 5. The core milk loop (pour → QR → payment) with the safety gate

```mermaid
flowchart LR
    R["reading\n(anti-tamper)"] --> P["pour\n(idempotent, priced)"]
    P --> INV["same-day invoice"]
    P --> CON["consignment\n(pooling boundary §7.4)"]
    CON --> TRIP["route trip\n+ cold chain"]
    TRIP --> LOT["BMC lot"]
    LOT --> G1{"QC gate\nBMC_RAPID"}
    G1 -->|pass| BAT["processing batch"]
    G1 -->|fail| BLK["BLOCKED\nquarantine + supervisor alert"]
    BAT --> G2{"QC gate\nPLANT_LAB"}
    G2 -->|pass| PL["product lot"]
    G2 -->|fail| BLK
    PL --> QR["signed QR"]
    QR --> SCAN["public scan\nsamiti-set provenance +\nintact hash chain"]
    INV --> SET["settlement\ninitiate → approve → execute\n(dual control)"]
    SET --> PAY["farmer paid + SMS"]

    classDef gate fill:#fde,stroke:#c39;
    class G1,G2 gate;
```

Every box appends a provenance event; a `BLOCKED` subject can never advance (dispatch/batch/product/QR all re-check). FSSAI limits (AFM1 ≤ 0.5 µg/kg, coliform ≤ 10 CFU/ml, phosphatase negative) are enforced in `domain/quality.go`.

---

## 6. Module map + how modules talk (event bus, never direct)

```mermaid
flowchart TB
    subgraph modules["internal/modules/*  (future microservice seams)"]
        ID["identity\nauth · KYC · roles"]
        OR["orgs"]
        CA["cattle"]
        CO["collection\n(core loop)"]
        LO["logistics"]
        PL["plant"]
        QU["quality\n(gate verdict)"]
        SE["settlement"]
        PT["publictrace"]
        PO["platformops\nadmin · audit · notifications"]
    end

    BUS(["eventbus (in-process pub/sub)\n→ swaps to Kafka/NATS at scale"])

    CO -->|pour.recorded / invoice.issued| BUS
    QU -->|qc.gate_blocked| BUS
    SE -->|payout.credited| BUS
    CA -->|mvu.dispatched| BUS
    ID -->|kyc.queue_changed| BUS

    BUS --> PO
    BUS --> SSE(["SSE hub → live dashboards"])

    PO -.->|queues SMS / notifications| OUTBOX[("notifications\noutbox")]
```

Modules read shared services from one `deps.Deps` container: `JWT, Ledger, Audit, Flags, Orgs, Bus, SSE, RateLimiter, DB, Cfg, Log`.

---

## 7. Real-time: the live "pending KYC" badge (SSE)

```mermaid
sequenceDiagram
    autonumber
    participant OM as Reviewer dashboard
    participant API as API
    participant BUS as eventbus
    participant HUB as SSE hub
    participant NEW as New user

    OM->>API: GET /events/stream (SSE, held open)
    API->>HUB: register connection (role = reviewer)
    API-->>OM: event: ready
    OM->>API: GET /kyc/pending/count → badge shows N
    NEW->>API: POST /kyc/aadhaar (submits KYC)
    API->>BUS: publish kyc.queue_changed
    BUS->>HUB: broadcast to reviewer roles
    HUB-->>OM: event: kyc.pending.changed  (nudge)
    OM->>API: GET /kyc/pending/count → badge shows N+1
    Note over OM,API: "nudge, then re-count" — the scoped count<br/>query is the source of truth, so the badge<br/>can never drift. No refresh needed.
```

Today the hub fans out within one process. At multi-replica scale the broadcast swaps to Redis pub/sub — no route changes.

---

## 8. Concurrency: "two at once → only one wins" (no Redis lock)

```mermaid
flowchart TD
    A["Reviewer A: approve KYC X"] --> DB{{"MongoDB atomic update\nupdate WHERE _id=X AND status=PENDING"}}
    B["Reviewer B: approve KYC X\n(same instant)"] --> DB
    DB -->|matched 1 doc| WIN["one wins → 200\nstatus=VERIFIED"]
    DB -->|matched 0 docs| LOSE["other → 409 KYC_NOT_PENDING"]
```

The state check lives **inside** the update filter, so the check-and-write is one indivisible operation.
Same pattern guards settlement `execute` (with a resume lease) and the BMC-lot→batch claim
(`DISPATCHED → POOLED`, so one lot can't feed two batches). This is safer than a Redis lock — one
source of truth, no lock-vs-DB drift. Proven by smoke step 22 (`200` + `409`).

---

## 9. Data stores (what lives where)

```mermaid
flowchart LR
    subgraph mongo["MongoDB (single source of truth)"]
        direction TB
        IDENT["parties · role_assignments ·\nkyc_records · consents ·\notp_challenges · refresh_tokens"]
        ORG["org_units (denormalised path[])"]
        LOOP["milk_pours · invoices · rate_charts ·\nanalyzer_readings · dcs_consignments ·\nroute_trips · bmc_lots · processing_batches ·\nproduct_lots · batch_qrs · qc_results"]
        MONEY["settlement_batches · payout_instructions ·\ndbt_requests"]
        HERD["animals · health_events · mvu_cases · education"]
        SYS["provenance_events (hash chain) ·\naudit_logs · notifications · feature_flags · counters"]
    end
    REDIS[("Redis (optional)\nrate-limit buckets")]
    OBJ["Object store (planned)\nanalyzer photos · PoD"]
```

Every `_id` is an ObjectID; every cross-collection reference is an ObjectID (native `$lookup`/populate).
Human-readable keys (`phone`, `code`, `qr_code`, `invoice_number`, `pashu_aadhaar`) are separate unique
indexes. The `provenance_events` collection is append-only and hash-chained.

---

## 10. Where to intervene — analysis checklist

Points a human reviewer should weigh (honest current state):

| Area | Current state | Decision / next step |
|---|---|---|
| **Deployment** | single modular monolith, stateless | scale = N replicas + Mongo replica-set/shard; split a module out only when a bottleneck is measured |
| **Real-time** | SSE within one process | add Redis pub/sub for cross-replica fan-out when >1 replica |
| **Rate limiting** | in-process default; Redis when `REDIS_URL` set | turn on Redis in prod (compose already wired) |
| **First-touch KYC routing** | scoped reviewers can't see assignment-less new parties | product decision: auto-route new KYC to a district queue? |
| **External DPI** | all mocked behind named seams | wire real Aadhaar / bank / PFMS / Bharat Pashudhan / SMS / PA one seam at a time |
| **Notifications** | outbox + manual worker endpoint | move worker to cron/queue; add real SMS provider |
| **Object store** | `photo_object_key` fields exist, no upload endpoint | add S3-compatible presigned upload |
| **Multi-doc invariants** | safe via atomic guards + unique indexes | optional Mongo transactions (needs replica set) to tighten crash windows |
| **Observability** | structured logs + Prometheus metrics + health/ready | add OpenTelemetry tracing when microservices land |
| **CI/CD, IaC, K8s** | Dockerfile + compose only | add pipeline + manifests before go-live |

---

### One-command reality check
`make smoke` runs the entire flow above (23 asserted steps: pour → gate → QR → settlement → KYC
approval → concurrency proof → live SSE badge) against a scratch MongoDB. If it's green, this diagram is true.
