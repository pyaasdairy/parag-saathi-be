# Saathi — Roles & Complete Workflow Guide

> **Who does what, in what order, through which APIs.** This is the operational companion to
> [`TECHNICAL_NOTES.md`](TECHNICAL_NOTES.md) (architecture) and the blueprint PDF (product spec).
> All diagrams are Mermaid — GitHub/GitLab/VS Code render them inline.
>
> One rule powers everything: **one phone number = one Party = many org-scoped roles.** The same
> human can be a Farmer *and* the Samiti Sacheev; they log in once and switch roles in-app.

---

## 1. The big picture — milk's journey from cow to QR scan

```mermaid
flowchart LR
    F["👨‍🌾 Farmer\npours milk"] --> D["🏠 DCS / Samiti\n(village society)"]
    D -->|"same-day invoice\n+ SMS receipt"| F
    D -->|"consignment\n(day + shift)"| V["🚐 Van Rider\nroute trip + cold chain"]
    V --> B["🏭 BMC / Chilling Centre\npool into lot, chill"]
    B -->|"rapid QC:\nAFM1 + coliform"| G1{"FSSAI\ngate"}
    G1 -->|PASS| P["🏭 Processing Plant\nbatch"]
    G1 -->|FAIL| X1["⛔ BLOCKED\nquarantine + alert\n+ trace to source"]
    P -->|"full lab QC:\nAFM1, coliform,\nphosphatase, antibiotics"| G2{"FSSAI\ngate"}
    G2 -->|PASS| L["📦 Product Lot\n+ signed QR"]
    G2 -->|FAIL| X2["⛔ BLOCKED"]
    L --> C["📱 Anyone scans QR\n(public, no login)\nsees samiti set + QC cert"]
    D -.->|"settlement:\ninitiate → approve → execute"| M["💰 Farmer's bank\nsame-day payout + SMS"]
```

Every arrow above appends an **immutable, hash-chained provenance event** — the public scan
re-verifies the chain and shows `ledger.intact: true`.

---

## 2. The cooperative hierarchy (where every role lives)

```mermaid
flowchart TD
    FED["🏛 FEDERATION — PCDF / Parag (state apex)\nroles: PCDF Admin, Mission Official, Super Admin, State Auditor"]
    UN["🏢 MILK UNION / Sangh (district)\nroles: Union President, Union Field Supervisor, Veterinarian, Van Rider"]
    PL["🏭 PROCESSING PLANT\nroles: Plant Operator, Plant Lab Analyst"]
    BMC["❄️ BMC / Chilling Centre\nroles: BMC Operator"]
    DCS["🏠 DCS / SAMITI (village)\nroles: Farmer, Samiti Sacheev, Samiti Adhyaksh,\nMilk Tester, LRP, AI Tech"]

    FED --> UN
    UN --> PL
    UN --> BMC
    BMC --> DCS
```

A role is always granted **at** one of these org units, and its powers stop at that unit's
subtree (enforced by `RequireInScope` on every API). A Union President sees their district;
a Sacheev sees only their samiti; Super Admin and State Auditor see everything (all audited).

---

## 3. Role-by-role: who they are and what they do

### 🏠 Village tier (the samiti)

| Role | Real person | What they do in Saathi | Key APIs |
|---|---|---|---|
| **FARMER** (Dugdh Utpadak) | Milk producer | Sees own pours, today's bill, payment history, own cattle; requests 1962 vet; applies KYC | `GET /collection/pours` `GET /collection/invoices` `GET /settlements/payouts` `POST /cattle/mvu-cases` `POST /kyc/aadhaar` |
| **SAMITI_SACHEEV** (DCS Secretary) | Runs the collection counter | Records readings + pours (works offline), generates same-day invoices, seals the consignment, **initiates** settlement | `POST /collection/readings` `POST /collection/pours` `POST /collection/pours/batch-sync` `POST /collection/invoices/generate` `POST /logistics/consignments` `…/dispatch` `POST /settlements` |
| **SAMITI_ADHYAKSH** (DCS Chairman, elected) | Governance co-sign | **Approves/rejects** settlements (dual control — can never approve one they initiated), grants FARMER/tester roles in own samiti | `POST /settlements/{id}/approve` `/reject` `POST /roles/assignments` |
| **MILK_TESTER** | Fat/quality tester | Same collection console as Sacheev (readings + pours) | `POST /collection/readings` `POST /collection/pours` |
| **LRP** | NDDB village extension worker | Assists onboarding, registers animals | `POST /cattle/animals` |
| **AI_TECH** | Artificial-insemination tech | Logs AI/breeding events against Pashu Aadhaar → syncs to Bharat Pashudhan | `POST /cattle/animals/{id}/health-events` |

### 🧑‍🏫 Field / organisation tier

| Role | Real person | What they do in Saathi | Key APIs |
|---|---|---|---|
| **ORGANISING_MANAGER** | Ground-level field worker (Dairy Development Dept pattern — "promotes and organises new samitis") | **Does doorstep KYC and approves it**, then **decides the user's position** by granting a role. This is the human gate between "someone signed up" and "someone can log in as a Farmer/Sacheev/etc." | `GET /kyc/pending` `POST /kyc/{id}/approve` `POST /kyc/{id}/reject` `POST /roles/assignments` |

### 🚐 Logistics tier

| Role | What they do | Key APIs |
|---|---|---|
| **VAN_RIDER** | Picks up consignments DCS-by-DCS, logs temperature at pickup + in transit, delivers to BMC | `POST /logistics/trips` `…/stops/{cid}/pickup` `…/cold-chain` `…/deliver` |
| **DELIVERY_RIDER** | Last-mile consumer delivery — **descoped for now** (Phase 3) | — |

### 🏭 Union / plant tier

| Role | What they do | Key APIs |
|---|---|---|
| **BMC_OPERATOR** | Pools delivered consignments into a lot, logs chilling temp, runs **rapid QC strips** (AFM1 + coliform), dispatches passed lots to plant | `POST /plant/bmc-lots` `…/close` `POST /quality/qc-results` (stage `BMC_RAPID`) `…/dispatch` |
| **UNION_FIELD_SUPERVISOR** | Oversees a cluster of DCS+BMCs, reviews anomaly-flagged readings, receives **safety-block alerts** | `GET /collection/readings` `GET /logistics/*` (scoped reads) |
| **UNION_PRESIDENT** | District oversight, approves rates, grants union-tier roles | `POST /collection/rate-charts` `POST /roles/assignments` |
| **PLANT_OPERATOR** | Creates processing batches from **passed** BMC lots, completes batches, creates product lots, issues QRs | `POST /plant/batches` `…/complete` `POST /plant/product-lots` `POST /plant/qrs` |
| **PLANT_LAB_ANALYST** | Full lab QC (ELISA/HPLC AFM1, culture coliform, phosphatase, antibiotics) — the **plant gate**; can recall product lots | `POST /quality/qc-results` (stage `PLANT_LAB`) `POST /plant/product-lots/{id}/recall` |

### 🏛 State / district / health tier

| Role | What they do | Key APIs |
|---|---|---|
| **PCDF_ADMIN** | Federation config: org tree, rate policy, education content, DBT | `POST /orgs` `POST /collection/rate-charts` `POST /dbt/requests` |
| **MISSION_OFFICIAL** | Nand Baba Mission oversight: DBT/subsidy status, trace tools, dashboards | `POST /dbt/requests` `GET /trace/*` |
| **DISTRICT_VERIFIER** | Verifies scheme applications (Phase 2 surface) | — |
| **VETERINARIAN** | 1962 MVU vet: case dispatch/close, health events, e-prescriptions, Bharat Pashudhan push | `POST /cattle/mvu-cases/{id}/dispatch` `/close` `POST /cattle/animals/{id}/health-events` `…/bp-sync` |
| **MVU_DRIVER** | MVU ambulance dispatch/route | `POST /cattle/mvu-cases/{id}/dispatch` |

### ⚙️ Platform tier

| Role | What they do | Key APIs |
|---|---|---|
| **SUPER_ADMIN** | Break-glass admin: feature flags, any role grant, full access — **every action audited** | `PUT /admin/flags/{key}` `POST /roles/assignments` |
| **STATE_AUDITOR** | Read-only everywhere + immutable audit-log export | `GET /audit/logs` `…/export` `GET /trace/*` |
| **SUPPORT_AGENT** | Helpdesk: limited PII lookup (each lookup itself audit-logged) | `GET /support/parties/lookup` |
| **SERVICE_ACCOUNT** | Machines (AMCU devices, connectors) — e.g. the dormant collar telemetry endpoint | `POST /cattle/telemetry` (403 until flag flips) |
| **CONSUMER** | **Descoped** — no consumer accounts. The public QR scan needs no login at all. | `GET /public/qr/{code}` (public) |

---

## 4. Flow 1 — Onboarding & the role switcher (§4)

```mermaid
sequenceDiagram
    autonumber
    participant U as Any user (phone)
    participant OM as 🧑‍🏫 Organising Manager / admin
    participant API as Saathi API
    participant DB as MongoDB

    U->>API: POST /auth/otp/request {phone}
    API->>DB: store HMAC-hashed OTP (5-min TTL)
    API-->>U: SMS with OTP (dev mode: in response)
    U->>API: POST /auth/otp/verify {phone, otp}
    API->>DB: find-or-create Party (tier MINIMAL, no roles yet)
    API-->>U: session token + refresh token
    Note over U,API: at this point the user has NO role →<br/>frontend shows a "pending / submit KYC" screen

    U->>API: POST /kyc/aadhaar {aadhaar, consent, requested_tier}
    API->>DB: KYCRecord status PENDING (stores last-4 + vault ref only)
    OM->>API: GET /kyc/pending  (within my org scope)
    OM->>API: POST /kyc/{id}/approve
    Note over API: not self-review · CanApproveKYCTier(role, tier)<br/>→ VERIFIED, party tier upgraded (upward only)
    OM->>API: POST /roles/assignments {party_id, role_code, org_unit_id}
    Note over API: THIS is "deciding the position" —<br/>Farmer / Sacheev / Sangh staff / etc.

    U->>API: GET /auth/roles
    API-->>U: my assignments, each with org {id, code, name, type}
    U->>API: POST /auth/role/select {role_assignment_id}
    Note over API: validity window + KYC tier satisfies role<br/>(before approval this returns 403 KYC_TIER_INSUFFICIENT)
    API-->>U: ROLE token carrying role_code + org scope
    Note over U: frontend renders THAT dashboard from role_code.<br/>Switching hats = role/select again.<br/>Losing an election = one revoked assignment, never a deleted account.
```

**The position is stored as the `RoleAssignment`** (`role_code` + `org_unit_id`), not a field on the
user — because one human can hold several (a Sacheev *is also* a Farmer). The role token's
`role_code` is the single value the frontend switches on to pick the dashboard; the assignment's
ObjectID is the lookup/revocation handle.

---

## 5. Flow 2 — A morning at the samiti: pour → receipt → same-day invoice (§8.1, §8.2)

```mermaid
sequenceDiagram
    autonumber
    participant F as 👨‍🌾 Farmer (with milk)
    participant S as 🧑‍💼 Sacheev / Tester (app, works OFFLINE)
    participant API as Saathi API
    participant L as 🔗 Provenance Ledger

    F->>S: brings can of milk
    S->>API: POST /collection/readings (fat 6.5, SNF 9.0, geotag, device id)
    Note over API: anti-tamper: plausibility bounds,<br/>OCR confidence, clock-skew & geotag flags
    API->>L: reading.recorded
    S->>API: POST /collection/pours {client_event_id, farmer, qty 10.5L, fat, snf}
    Note over API: price from org-scoped rate chart:<br/>8.00 + 6.5×5.50 + 9.0×1.00 = ₹52.75/L → ₹553.88
    API->>L: pour.recorded (refs: farmer, animal, reading)
    API-->>F: 📱 SMS receipt: "10.5L @ ₹52.75 = ₹553.88"
    Note over S,API: offline all morning? queue locally, then<br/>POST /collection/pours/batch-sync — replays are<br/>harmless (client_event_id is idempotent)
    S->>API: POST /collection/invoices/generate {dcs_id}
    API->>L: invoice.issued (refs: all pours) — late pours merge via invoice.amended
    API-->>F: 📱 SMS: today's bill ready
```

---

## 6. Flow 3 — Same-day money: the dual-control settlement (§8.1 guardrail)

**Saathi computes and initiates. A different human approves. A licensed PA executes. Money never moves autonomously.**

```mermaid
sequenceDiagram
    autonumber
    participant S as 🧑‍💼 Sacheev (initiates)
    participant A as 👩‍⚖️ Adhyaksh (approves — MUST be a different person)
    participant API as Saathi API
    participant PA as 🏦 Payment Aggregator (mock)
    participant F as 👨‍🌾 Farmer

    S->>API: POST /settlements {dcs_id, date}
    Note over API: bundles the day's ISSUED invoices → PENDING_APPROVAL
    A->>API: POST /settlements/{id}/approve
    Note over API: dual control: initiator ≠ approver<br/>(self-approval → 403, even for Super Admin)
    S->>API: POST /settlements/{id}/execute
    API->>PA: payout instructions (one per invoice)
    PA-->>API: SUCCESS + UTR per farmer
    Note over API: crash-safe: EXECUTING holds a 2-min lease,<br/>an interrupted run resumes without double-paying
    API-->>F: 📱 SMS: "₹553.88 credited, UTR ..."
    Note over API: invoices → PAID · settlement.executed +<br/>payout.credited chained to the ledger
```

```mermaid
stateDiagram-v2
    [*] --> PENDING_APPROVAL: Sacheev initiates
    PENDING_APPROVAL --> APPROVED: Adhyaksh approves (different person!)
    PENDING_APPROVAL --> REJECTED: Adhyaksh rejects (invoices released)
    APPROVED --> EXECUTING: execute (2-min lease)
    EXECUTING --> EXECUTED: all payouts SUCCESS
    EXECUTING --> PARTIAL: some payouts failed
    EXECUTING --> FAILED: PA error (re-executable)
    FAILED --> EXECUTING: retry / resume stale lease
```

---

## 7. Flow 4 — The milk's physical journey: consignment → van → BMC (§7.1)

```mermaid
sequenceDiagram
    autonumber
    participant S as 🧑‍💼 Sacheev
    participant R as 🚐 Van Rider
    participant B as ❄️ BMC Operator
    participant API as Saathi API
    participant L as 🔗 Ledger

    S->>API: POST /logistics/consignments {dcs, date, shift}
    Note over API: aggregates the shift's pours — this is the<br/>POOLING BOUNDARY: past here milk traces to a<br/>SET of samitis, never one farmer (§7.4)
    API->>L: consignment.created (refs: pours)
    S->>API: POST .../dispatch (seals it; late pours now 409)
    R->>API: POST /logistics/trips {route, stops:[{dcs, consignment}]}
    R->>API: POST .../stops/{cid}/pickup {temp_c: 4.2}
    API->>L: consignment.picked_up
    R->>API: POST .../cold-chain {temp_c} (repeat en route)
    R->>API: POST .../deliver {bmc_id}
    API->>L: trip.delivered
    B->>API: POST /plant/bmc-lots {consignment_ids} → pool into lot
    B->>API: POST .../close {chilling_temp_c: 3.8} → QC_PENDING
```

---

## 8. Flow 5 — The FSSAI safety gate: the heart of "disease-free milk" (§8.3)

**Why it's shaped this way:** Aflatoxin M1 survives pasteurisation. If it isn't caught at the
BMC/plant, it ends up in the packet. So the gate fires **twice**, and a failed subject is
physically unable to advance through the API.

```mermaid
flowchart TD
    IN["Lot / batch closed → QC_PENDING"] --> T["QC recorded:\nBMC_RAPID by BMC Operator\nPLANT_LAB by Lab Analyst"]
    T --> E{"All mandatory tests present\nand within FSSAI limits?\nAFM1 ≤ 0.5 µg/kg · coliform ≤ 10 CFU/ml\nphosphatase negative (plant)"}
    E -->|"YES"| P["✅ PASSED\ncertificate number issued\nqc.gate_passed chained"]
    E -->|"NO"| B["⛔ BLOCKED + reason\nqc.gate_blocked chained\nSMS alert → Union Field Supervisor"]
    P --> N["may dispatch / enter batch / yield product lot / get QR"]
    B --> Q["quarantined forever:\ndispatch → 422\nbatch create → 422\nproduct lot → 422\nQR → impossible"]
    B --> TR["trace upstream via ledger:\nwhich samitis contributed →\ninvestigate feed / animals (§8.3)"]
```

```mermaid
stateDiagram-v2
    [*] --> OPEN: BMC lot created (pools consignments)
    OPEN --> QC_PENDING: close (chilling temp logged)
    QC_PENDING --> PASSED: rapid tests within limits
    QC_PENDING --> BLOCKED: any limit exceeded — terminal quarantine
    PASSED --> DISPATCHED: tanker to plant
    DISPATCHED --> POOLED: claimed by exactly ONE batch (atomic)
    BLOCKED --> [*]
```

Gate hardening (post-review): unknown test names are rejected, a PASS requires the complete
mandatory test set per stage, evidence + ledger events are persisted **before** the status flips,
and one lot can never feed two batches.

---

## 9. Flow 6 — Plant: batch → product lot → signed QR → public scan (§8.4)

```mermaid
sequenceDiagram
    autonumber
    participant PO as 🏭 Plant Operator
    participant LA as 🔬 Lab Analyst
    participant API as Saathi API
    participant PUB as 📱 Anyone (no login)

    PO->>API: POST /plant/batches {bmc_lot_ids, product_type}
    Note over API: every lot must be PASSED+DISPATCHED —<br/>atomically claimed → POOLED (one batch only)
    LA->>API: POST /quality/qc-results (PLANT_LAB: AFM1, coliform, phosphatase)
    Note over API: gate #2 → batch PASSED
    PO->>API: POST /plant/batches/{id}/complete
    PO->>API: POST /plant/product-lots {sku, units, mfg, expiry}
    PO->>API: POST /plant/qrs {product_lot_id}
    Note over API: qr_code PRG-XXXXXXXX +<br/>HMAC-SHA256 signed token → unforgeable
    PUB->>API: GET /public/qr/PRG-E74X27WV
    API-->>PUB: product + batch + plant + QC certificate<br/>+ "made from milk collected on 6 Jul from<br/>2 samitis in Lucknow district" (the HONEST view §7.4)<br/>+ ledger.intact: true (chain re-verified)
```

The scan never claims per-farmer origin — pooled milk traces to the **set of contributing
samitis**, and the API is built so it *cannot* say otherwise.

---

## 10. Flow 7 — Cattle health & 1962 MVU (§9, §10)

```mermaid
sequenceDiagram
    autonumber
    participant F as 👨‍🌾 Farmer
    participant V as 🩺 Veterinarian (1962 MVU)
    participant API as Saathi API
    participant BP as 🇮🇳 Bharat Pashudhan (mock)

    F->>API: POST /cattle/mvu-cases {animal, symptoms}
    V->>API: POST /cattle/mvu-cases/{id}/dispatch
    API-->>F: 📱 SMS: MVU on the way (case ref)
    V->>API: POST /cattle/animals/{id}/health-events {TREATMENT, diagnosis}
    V->>API: POST /cattle/mvu-cases/{id}/close {visit notes}
    V->>API: POST /cattle/animals/{id}/bp-sync
    API->>BP: push events keyed on 12-digit Pashu Aadhaar
    Note over API: collar telemetry endpoint exists TODAY but is<br/>flag-gated OFF — when a govt scheme lands,<br/>flip collar_telemetry_enabled, zero schema change (§9)
```

---

## 11. Flow 8 — Admin, auditor & the control tower (§12)

```mermaid
flowchart LR
    SA["🔑 Super Admin"] -->|"PUT /admin/flags/{key}\n(audited, closed key set)"| FL["Feature flags:\ncollar OFF · photo-OCR ON\nONDC OFF · commerce OFF"]
    SA -->|"POST /roles/assignments"| RA["Grant / revoke any role"]
    AU["🕵️ State Auditor"] -->|"GET /audit/logs + /export"| AL["Immutable audit trail:\nwho did what, when, from where\n(every mutation + every PII lookup)"]
    AU -->|"GET /trace/{type}/{id}"| TR["Full upstream + downstream\nprovenance of ANY entity"]
    SUP["🎧 Support Agent"] -->|"GET /support/parties/lookup"| PL["Limited PII view —\nthe lookup itself is audit-logged"]
```

---

## 12. Quick RBAC cheat-sheet (who can hit what)

| Action | Allowed roles |
|---|---|
| Record reading / pour | Sacheev, Milk Tester |
| Generate invoices | Sacheev |
| Create/dispatch consignment | Sacheev |
| Run trip / pickup / deliver | Van Rider (only their own trip) |
| Create/close/dispatch BMC lot | BMC Operator |
| QC at BMC (`BMC_RAPID`) | BMC Operator |
| QC at plant (`PLANT_LAB`) | Plant Lab Analyst |
| Create batch / product lot / QR | Plant Operator (QR also Lab Analyst) |
| Recall product lot | PCDF Admin, Lab Analyst |
| Initiate settlement | Sacheev |
| Approve settlement | Adhyaksh, Union President (never the initiator) |
| Rate charts | PCDF Admin, Union President |
| Org tree | Super Admin, PCDF Admin |
| Review / approve KYC | Organising Manager, District Verifier, PCDF Admin, Super Admin (each within their scope + tier) |
| Role grants ("assign position") | Super Admin, PCDF Admin, Union President (their tiers), Adhyaksh (farmers in own DCS), Organising Manager (Farmer/Consumer in scope) |
| DBT subsidy | Mission Official, PCDF Admin |
| Feature flags | Super Admin |
| Audit logs / export | State Auditor, Super Admin |
| Public QR scan / ledger verify | **anyone — no login** |

Plus, always: **Super Admin bypasses role gates (fully audited)** and **every write is
org-scope-checked** — your role only works inside your own subtree of the cooperative.

---

## 13. Try the whole story locally in one command

```bash
make smoke
```

That script is this document executed: it logs in as each role above and drives
pour → receipt → invoice → consignment → trip → BMC → gate (pass **and** blocked paths) →
batch → QR → public scan → dual-control settlement → SMS worker — 20 steps, all asserted.
