# RELEASE.md — store-compliance invariants (Saathi backend). Do not break these.

- The backend must be LIVE during store review (Apple 2.1 / Google rejection
  otherwise) and matched to the submitted build's env.
- Payments: gateway (RBI-authorised aggregator) only; card data tokenised,
  never stored. Wallet is closed-loop, single-merchant, physical-goods-only;
  refunds return to source; outstanding balances are RETURNED on account
  deletion, never voided. Retain financial records 10 years (RBI PPI).
- Account deletion (POST /me/erasure pattern): anonymise PII irreversibly,
  keep only legally-required financial/tax records re-keyed to a pseudonymous ID.
- DPDP: consent notice before collecting personal data; data minimisation;
  masked PII in ops surfaces; rider location ephemeral + duty-scoped;
  named grievance/Data Protection officer; data residency in India for payments.
- RBAC server-side (RequireRoles); SUPER_ADMIN bypass is deliberate; the
  consumer traceability gate (X-Parag-App-Key) FAILS CLOSED when unset.
- DLT-registered SMS templates (TRAI) for every transactional SMS when the
  gateway lands.
- Settlement stays dual-control: initiator ≠ approver (Union/Sangh approves).
