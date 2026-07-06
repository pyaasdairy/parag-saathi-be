# Saathi — Backend

> Backend services for **Saathi**, a digital dairy supply-chain & traceability platform for the
> **Nand Baba Dugdh Mission** (Uttar Pradesh / PCDF–Parag).

**Status:** v0.2 — the backend is implemented as a **Go + MongoDB modular monolith** covering
P0 + P1 of the blueprint (identity/RBAC → pour → same-day invoice → logistics → FSSAI safety
gate → batch → QR → public trace → settlement) plus P2 foundations. The blueprint
([`SAATHI_Architecture_Framework_Parag.pdf`](SAATHI_Architecture_Framework_Parag.pdf)) remains
the source of truth for *what* to build; [`docs/TECHNICAL_NOTES.md`](docs/TECHNICAL_NOTES.md)
documents *what has been built* and what comes next — read it first.

> ⚠️ **Confidential.** This document set is the proprietary property of PYAAS and is disclosed on a
> strictly confidential basis. Do not fork, publish, or share outside the authorised team. The
> GitHub repository is **private** — keep it that way.

---

## What Saathi is

An end-to-end system that follows milk **from a single cow to the consumer's QR scan**, with:

- **Same-day farmer payment** (replacing today's ~10-day cycle)
- **Milk-safety gating** — block unsafe lots on aflatoxin (AFM1) / coliform thresholds (FSSAI)
- **Full batch traceability** — append-only provenance graph: pour → DCS lot → route → BMC → plant batch → product QR
- **Cattle-health readiness** — interoperable with Bharat Pashudhan (Pashu Aadhaar), collar-ready but dormant
- **Government-grade compliance** — DPDP, Aadhaar, CERT-In, FSSAI, GIGW, and more

Saathi is positioned as a **state-level (UP/PCDF) orchestration super-app** that *federates* the
national Digital Public Infrastructure (Aadhaar, DigiLocker, UPI/AePS, PFMS/DBT, Bharat Pashudhan,
1962 MVU) rather than rebuilding it.

## Core design principles (non-negotiables)

From the field spec — these constrain every service you build:

1. **Offline-first, sync-later.** Every collection-point / rider action works fully offline and reconciles on reconnect.
2. **Low / no digital literacy.** Icon + voice + vernacular UX, never text-form-first. (Clients; backend must support IVR/SMS + voice flows.)
3. **DPI-first, don't duplicate.** Reuse Aadhaar / DigiLocker / AA / UPI / Bharat Pashudhan. Every avoided rebuild is avoided liability.
4. **Provenance is append-only.** Traceability records are never edited in place — corrections are new events referencing the old.
5. **Tamper-awareness.** Human-entered / photo-derived data gets integrity controls (evidence retention, geotag/time binding, plausibility bounds).
6. **Capability gating over schema churn.** Future-scheme features (collars) ship dormant behind feature flags.
7. **Pooling reality.** Once milk is pooled, a pack traces to a *set* of contributing samitis, not one farmer. Model that honestly.

## Architecture at a glance

- **Event-driven microservices** behind an API gateway, with **BFFs** tailoring payloads per client (farmer app payload ≪ official console).
- **Append-only provenance store** as the source of truth for traceability (graph DB or hash-chained event log projected to a graph).
- **Thin, well-guarded integration edge** to the DPI systems.
- **Identity:** one phone number = one **Party** = many time-bounded, **org-scoped Role Assignments** (RBAC). A role switcher lazy-loads each role's dashboard module. See §4.

### Data stores (recommended, §14)

| Concern | Technology (recommendation) | Holds |
|---|---|---|
| Transactional (per service) | PostgreSQL (managed, in-India region) | Parties, roles, pours, batches, orders |
| Provenance / traceability | Graph DB **or** hash-chained append-only event log | The pour→QR graph, immutable |
| Time-series | Timescale / Influx | Cold-chain temps, collar/RFID telemetry |
| Object store | S3-compatible, in-India | Analyzer photos, QC certs, PoD, evidence |
| Analytics | Columnar DWH + BI | Yield, procurement, safety trends, DBT |
| Secrets / Aadhaar | HSM-backed vault (Aadhaar Data Vault) | Tokenised Aadhaar refs, keys |
| Offline (device) | Local encrypted DB + sync engine | Collection-point offline queue |

**Data residency:** all stores in India (MeitY-empanelled CSP / GI-Cloud / NIC).

### Reference tech stack (§15 — swappable)

The **non-negotiables** are: offline-first clients, append-only provenance, in-India hosting, DPI
integration, and audit-grade logging. Languages/DBs are swappable — **the backend language is still
an open decision (see below).**

- **Backend:** microservices in a memory-safe/typed stack — **Go / Kotlin-JVM / TypeScript-Node** — with an event bus (Kafka/NATS), gateway + WAF.
- **Provenance:** append-only event store projected to a graph (Neo4j/JanusGraph) or ledger-style hash chaining.
- **Infra:** containerised (Kubernetes) on a MeitY-empanelled Indian cloud / NIC / GI-Cloud; IaC; full observability; NTP-synced (CERT-In).

## Roles (RBAC)

23 roles across Village / Logistics / Union / Plant / State / District / Health / Consumer /
Platform / System tiers (full catalog in §5.2 of the blueprint). **MVP roles** to prove the
pour→payment→batch→QR loop: `FARMER`, `SAMITI_SACHEEV`, `SAMITI_ADHYAKSH`, `VETERINARIAN`,
`PLANT_OPERATOR`, `PLANT_LAB_ANALYST`, `CONSUMER`, `SUPER_ADMIN`.

## Key flows to implement (§8)

1. **Daily pour → same-day invoice → payment** (settlement via licensed PA; subsidy strictly via DBT/PFMS; money-movement stays an authorised human/finance action).
2. **Analyzer reading → autofill** — direct device integration preferred; photo-OCR as the anti-tamper bridge for legacy analyzers.
3. **Safety gate** — AFM1 ≤ 0.5 µg/kg and coliform ≤ 10 CFU/ml enforced in software at BMC + Plant; a failed lot cannot advance. (AFM1 survives pasteurisation → gate upstream.)
4. **Consumer QR scan** — samiti-set-level provenance + quality certificate.

## Phased roadmap (§19)

| Phase | Goal | Deliverables |
|---|---|---|
| **P0 — Foundation** | Identity + one loop | Party/RBAC + onboarding/KYC; role switcher; offline collection console; pour receipt |
| **P1 — Traceability + payment** | Pour→QR + same-day pay | Provenance graph; analyzer integration + photo-OCR bridge; safety gate; batch QR; consumer scan; settlement/DBT rail |
| **P2 — Mission value** | Health + oversight + subsidy | Cattle/health + Bharat Pashudhan sync; collar-ready dormant path; 1962; education hub; scheme/subsidy; official dashboards |
| **P3 — Consumer + scale** | Commerce + delivery + hardening | Consumer commerce; last-mile delivery; ONDC (optional); SDF audit; full compliance certification; scale-out |

## External integrations (§13)

Aadhaar eKYC (KUA/AUA) · DigiLocker · Account Aggregator (Sahamati) · UPI/AePS · PFMS/DBT ·
Bharat Pashudhan / NDLM · 1962 MVU · FSSAI FoSCoS + NABL labs · GST e-invoice (IRP) · SMS/IVR gateway.

## Compliance (§17–18) — build these in from day one

Very likely a **Significant Data Fiduciary (SDF)** under DPDP — plan for the heavier obligations up front.

- **Privacy/data:** DPDP Act 2023 + Rules 2025; Aadhaar Act (first-8-digit masking, Data Vault, never mandatory-to-deny); CERT-In (6-hr incident reporting, ≥180-day in-India log retention, NTP sync); IT Act.
- **Financial:** RBI (licensed PA, nodal/escrow, tokenisation); NPCI; PFMS/DBT; GST e-invoicing.
- **Food safety:** FSSAI (FSS Act, FoSCoS licensing, AFM1/coliform/TPC/MRL/phosphatase limits, recall + traceability); BIS/IS standards; Legal Metrology (stamped scales/analyzers).
- **Gov IT:** GIGW 3.0; WCAG 2.1 AA; STQC + VAPT by CERT-In-empanelled auditor pre-go-live; MeitY-empanelled hosting; ISO 27001/27701, SOC 2.

Full register in §18 of the blueprint — treat each item as a work item with an owner.

## Open decisions the team must resolve first (§20)

These block architecture and should be settled before heavy build:

1. **Build-vs-federate** with NDDB AMCS / i-DIS where already deployed.
2. **Provenance store** — native graph DB vs hash-chained event log projected to a graph.
3. **Payment settlement authority** — approval flow + PA vs direct cooperative banking rail.
4. **SDF classification** — confirm with legal (drives DPIA / annual audit / localisation).
5. **Analyzer fleet reality** — % of samitis with integrable analyzers vs legacy-only (sizes the photo-OCR load).
6. **Traceability granularity promise** — samiti-level (achievable) vs any per-farmer pack claim (not achievable post-pooling).
7. **Weights & Measures** — periodic Legal-Metrology verification/stamping process.

---

## Getting started (for the next developer)

Prerequisites: **Go 1.23+** and **MongoDB** (local `mongod` or any URI — connection is
env-only, never hardcoded).

```bash
cp .env.example .env      # set MONGO_URI here (required — server refuses to boot without it)
make tidy                 # resolve Go dependencies
make run                  # start the API on :8080
make seed                 # idempotent demo data: org tree, one party per role, rate chart
make test                 # unit tests
make smoke                # full E2E of the pour→payment→QR loop against a scratch DB
make docker-up            # alternative: MongoDB + API via docker compose
```

Quick check once running:

```bash
curl -s localhost:8080/healthz     # {"data":{"status":"ok"}}
curl -s localhost:8080/readyz      # MongoDB ping
curl -s localhost:8080/version
```

With `OTP_DEV_MODE=true`, `POST /api/v1/auth/otp/request` returns the OTP in the response —
seeded demo phones are listed by `make seed` (super-admin `9999999999`, sacheev `9000000001`,
farmer `9000000011`, …).

Then read:

- [`docs/WORKFLOW_GUIDE.md`](docs/WORKFLOW_GUIDE.md) — **who does what**: every role (farmer →
  sacheev → adhyaksh → rider → BMC → plant → admin) with end-to-end flow diagrams.
- [`docs/TECHNICAL_NOTES.md`](docs/TECHNICAL_NOTES.md) — architecture decisions, module map, API
  conventions, what's mocked vs real, and the ordered next-steps list.

## Repository layout

```
.
├── README.md                                # this file
├── SAATHI_Architecture_Framework_Parag.pdf  # v0.1 architecture blueprint (source of truth)
├── docs/WORKFLOW_GUIDE.md                   # roles + end-to-end flows with diagrams
├── docs/TECHNICAL_NOTES.md                  # implementation handoff (read after the blueprint)
├── cmd/                                     # server + seed binaries
├── internal/
│   ├── config/                              # env-only configuration
│   ├── domain/                              # pure entity contract (roles, FSSAI limits, statuses)
│   ├── platform/                            # auth, RBAC, provenance ledger, audit, flags, mongo
│   ├── httpapi/                             # router + module registration
│   └── modules/                             # 10 domain modules (future microservice seams)
├── scripts/smoke.sh                         # end-to-end loop test
├── Dockerfile · docker-compose.yml · Makefile
└── .env.example                             # every environment variable, documented
```

---

© PYAAS — Confidential. Internal use only.
