# Decision Register

| ID | Decision | Status |
|---|---|---|
| ADR-001 | Go is the authoritative backend/control plane | Accepted |
| ADR-002 | TypeScript/React/Next.js is the operator frontend | Accepted |
| ADR-003 | Python is restricted to research/backtest/model workloads | Accepted |
| ADR-004 | Rust requires an ADR and bounded responsibility | Accepted |
| ADR-005 | PostgreSQL 17 is the financial system of record | Accepted |
| ADR-006 | Transactional outbox is the baseline event delivery mechanism | Accepted |
| ADR-007 | Redis is cache only | Accepted |
| ADR-008 | Risk Engine is the deterministic financial veto | Accepted |
| ADR-009 | OMS owns canonical internal order state | Accepted |
| ADR-010 | Reconciliation owns external discrepancy resolution | Accepted |
| ADR-011 | Ledger is append-only and corrected by compensating entries | Accepted |
| ADR-012 | Live credentials are isolated from lower environments | Accepted |
| ADR-013 | Unknown venue outcomes are reconciled before exposure-increasing retry | Accepted |
| ADR-014 | Contract-first integration is mandatory | Accepted |
| ADR-015 | Immutable release artifacts with SBOM/provenance are mandatory | Accepted |

No architectural decision remains open in this final blueprint.


## Additional binding decisions

| ADR | Decision | Rationale / boundary |
|---|---|---|
| ADR-016 | Initial jurisdiction review target is Indonesia, subject to explicit account-holder declaration and legal review | Location, IP, language, or currency never proves residence or eligibility; no live enablement from this designation alone. |
| ADR-017 | Keycloak is the reference workforce identity broker; OIDC Authorization Code + PKCE is the application protocol | SAML is accepted only at the federation boundary; application authorization remains local and server-side. |
| ADR-018 | Open Policy Agent evaluates identity/resource authorization policy; Go domain code owns trading-risk decisions | Authorization policy cannot approve or override financial risk. Policy bundles are signed, versioned, and fail closed when stale. |
| ADR-019 | Kubernetes is the production workload platform; managed PostgreSQL is the financial system of record | Stateless services scale independently; financial writes remain single-region/single-primary. |
| ADR-020 | PostgreSQL transactional outbox is the initial event backbone; NATS JetStream is introduced only at the defined fan-out/lag threshold | No exactly-once assumption; consumers remain idempotent. |
| ADR-021 | Recovery is active-passive across regions; no automatic resumption of risk-increasing activity after failover | Failover requires reconciliation and explicit two-person re-enable approval. |
| ADR-022 | Audit integrity uses signed hash-chain checkpoints plus independent immutable retention | No public blockchain; evidence remains independently verifiable and privacy-controlled. |
| ADR-023 | Indonesia crypto scope must verify current OJK provider, instrument, and rule eligibility at activation and continuously thereafter | A stale, revoked, or unverified registry entry disables affected live scope. |
| ADR-024 | Live capability requires an eligible adult account holder and current account/venue/product authorization | No guardian or account-sharing workaround; exceptions require explicit legal determination and product policy, not an operator override. |
| ADR-025 | Cross-region recovery target is RTO ≤30 minutes and RPO ≤5 minutes, subject to tested evidence | The platform must not claim these objectives until restore/failover drills demonstrate them. |
