# Architecture and Compliance Decision Closure

## 1. Purpose and authority

This document closes architecture choices that must be explicit before implementation. It is binding with the README, decision register, canonical contracts, risk specification, security specification, compliance boundary, deployment standard, and execution gates. If summaries conflict, the more restrictive safety control wins; the Risk Engine, OMS, reconciliation, and ledger authority order remains unchanged.

## 2. Deployment and service topology

| Concern | Binding decision | Operational boundary |
|---|---|---|
| Operator UI | TypeScript, React, Next.js | Browser is untrusted; no direct DB, queue, or venue access. |
| Authoritative API/domain | Go modular monolith on managed Kubernetes | Identity, configuration, risk, OMS, ledger, and reconciliation remain one policy authority. |
| Research/backtest | Python workers, isolated runtime | No live credentials; outputs are versioned artifacts submitted for validation. |
| Specialized compute | Rust only under an approved ADR | No independent authorization or financial policy authority. |
| Identity | Keycloak reference broker, OIDC Authorization Code + PKCE; SAML only upstream | MFA/WebAuthn for privileged users; app session and authorization remain server-side. |
| Authorization | OPA for RBAC/ABAC policy evaluation | Go Risk Engine independently enforces financial risk; explicit deny wins. |
| System of record | Managed PostgreSQL 17 HA primary | Single writer for authoritative financial state; analytics use replicas/projections. |
| Eventing | PostgreSQL transactional outbox first | Add NATS JetStream when sustained outbox lag exceeds 5 seconds for 10 minutes, or independent consumer fan-out exceeds 5 critical consumer groups. Migration requires replay/idempotency proof. |
| Secrets/keys | Managed secret store and KMS/HSM-backed signing | Workload-scoped, short-lived credentials; no secret in source, prompts, logs, or artifacts. |
| Audit evidence | Signed hash chain, signed daily checkpoints, immutable cross-region retention | Independent verifier detects missing sequence, invalid hash, signature failure, and retention gaps. |
| Recovery | Active-passive regional recovery | No active-active financial writes; recovery enters `RECOVERY_HOLD`. |
| Infrastructure | Terraform-managed, immutable OCI images, signed release artifacts | Drift is detected; production changes require reviewed plan and release evidence. |

## 3. Polyglot ownership and service boundaries

Polyglot means distinct languages are assigned to bounded responsibilities, not that every service may choose its own stack. TypeScript owns presentation; Go owns authoritative control and financial state transitions; Python owns offline research and deterministic backtesting; Rust is exceptional and requires an ADR demonstrating measurable benefit and maintainability. SQL migrations are reviewed and versioned. All inter-language boundaries use OpenAPI/JSON for external APIs and versioned event schemas for asynchronous contracts. No language-specific object serialization crosses a service boundary.

The initial deployment is not a microservice-per-domain architecture. The Go control plane is a modular monolith with package-level ownership and explicit interfaces. Independent workers are deployed separately only for isolation, scaling, or failure containment. This avoids distributed transactions across the risk decision and order-state mutation path.

## 4. Added enterprise capabilities and why they are required

| Capability | Required implementation | Failure behavior / acceptance |
|---|---|---|
| Zero Trust | Workload identity, mTLS for service traffic, short-lived audience-bound tokens, deny-by-default egress, device posture for privileged access | Unknown identity, invalid audience, or policy failure denies access. |
| SSO and lifecycle | OIDC broker, optional upstream SAML, MFA/WebAuthn, SCIM deprovisioning, session revocation ≤60 seconds | Revoked identity cannot mutate; no local production password fallback. |
| Granular authorization | RBAC roles plus ABAC by environment, account, market, instrument, strategy, action, and approval state | No wildcard live grants; scope mismatch denied and audited. |
| Dual control | Independent requester/approver for live activation, policy changes, high-impact config, break-glass | Same identity cannot satisfy both approvals; any diff invalidates approval. |
| Tamper-evident audit | Append-only audit event, monotonic sequence, hash chaining, signed checkpoint, immutable replica | Gaps/signature failure raise SEV-1 evidence-integrity alert; financial mutation is blocked if required audit commit fails. |
| Regulatory eligibility registry | Versioned jurisdiction/account/venue/instrument/product/activity approvals with expiry and official-source evidence | Unknown, expired, revoked, or stale scope blocks risk-increasing activity. |
| Reconciliation case management | Difference classification, owner, severity, evidence, resolution, aging SLA, re-open support | Material unresolved break blocks affected risk-increasing scope. |
| Model governance | Dataset/model lineage, independent approval, drift alerts, rollback artifact, no model self-promotion | Drift threshold or lineage gap pauses affected deployment. |
| DR and cyber recovery | Immutable backups, cross-region WAL, restore drills, recovery hold, credential/key compromise runbook | Recovery never automatically resumes live operations. |
| Data governance | Classification, minimization, access/export/deletion workflow, legal hold, retention by data class | Restricted data excluded from prompts/logs; legal hold overrides ordinary deletion. |
| Supply-chain security | Pinned dependencies, SBOM, provenance, signed images, SAST/SCA/container scans, protected releases | Critical unresolved vulnerability blocks release unless approved time-bounded compensating control exists. |
| Operator safety | Global/market/venue/account/strategy halts, cancel-only mode, read-only recovery mode, kill-switch test | Halt precedence is monotonic; re-enable requires separate approval. |

## 5. Jurisdiction rule model

Jurisdiction is not a single global boolean. Eligibility is evaluated over the tuple:

`declared_residence + account_holder + legal_entity + venue + account_type + market_class + instrument + product + activity + API_permission + effective_date`.

The initial review profile is Indonesia, only when explicitly declared. A compliance decision is required for each enabled tuple and records source authority, rule citation, reviewer, decision, restrictions, effective date, expiry, and next review. The system checks current official sources at activation and on the configured monitoring cadence. API connectivity, provider marketing, or another user’s approval is not evidence of eligibility.

For Indonesia digital financial assets/crypto, the rule register must track OJK’s current framework, including POJK 27/2024 as amended by POJK 23/2025 and PADK OJK 3/2026 (effective 1 September 2026), as well as current official provider and instrument lists. The applicable requirements may differ by provider, instrument, product, and activity. This document does not certify any venue, account, asset, or person as approved.

Other market classes require separate determinations for the relevant securities, derivatives, foreign-exchange, commodities, and cross-border rules. Unknown rules disable the affected scope. Regulatory source changes create a case; failure to refresh a source within 72 hours blocks new risk-increasing activity in the affected scope. Eligibility records expire after at most 30 days unless a stricter interval is required.

Live capability is restricted to a legally eligible adult account holder with verified identity and account authority. The system does not support shared credentials, false age/residence declarations, or bypasses. Where law creates a special account arrangement, it remains disabled until counsel confirms applicability and the platform implements the required controls.

## 6. Operational numeric defaults

| Control | Default |
|---|---:|
| Authorization policy cache maximum age | 5 minutes |
| Session revocation propagation | ≤60 seconds |
| High-impact approval validity | 24 hours; exact diff only |
| Break-glass duration | ≤30 minutes, two-person release |
| Eligibility record maximum age | 30 days |
| Official-source refresh failure tolerance | 72 hours, then block affected scope |
| NATS introduction threshold | Outbox lag >5 seconds for 10 minutes or >5 critical consumer groups |
| Cross-region RTO / RPO | ≤30 minutes / ≤5 minutes, evidence required |
| Audit checkpoint | Daily and on key rotation / high-impact security event |
| Regulatory monitoring | Daily |

These values are platform defaults, not statements of legal or venue requirements. A stricter rule always prevails. A relaxed value requires a reviewed ADR, risk assessment, and updated tests.

## 7. Toolchain, API, and release controls

Language/runtime versions are selected at repository bootstrap from stable, security-supported releases, pinned exactly, and recorded in release evidence. End-of-life runtimes are prohibited. Critical/high security fixes are applied within 14 days; routine patch updates are quarterly. CI rejects floating dependencies and toolchain drift. API requests are capped at 1 MiB (commands 64 KiB unless a stricter route limit applies); collection reads use cursor pagination with a default of 50 and maximum of 200. Operator API defaults are 60 reads/minute and 10 privileged mutations/minute; financial command throughput remains bounded by account policy and venue limits.

Availability measurement excludes no more than two hours of approved, 72-hour-noticed maintenance per month. Capacity acceptance requires 60 minutes at baseline, 5x for 15 minutes, 20x for 60 seconds, and backlog recovery within 15 minutes without loss of authoritative events. G11 requires exact-scope eligibility, verified adult/account authority, distinct dual approvers, no unresolved material reconciliation break or UNKNOWN order, reviewed risk configuration, immutable evidence, and a narrow canary with explicit abort criteria.

## 8. Acceptance evidence

The architecture is accepted for implementation when the repository contains: (1) architecture decision register matching this document; (2) API/event schemas and compatibility tests; (3) identity and authorization policy test vectors; (4) jurisdiction registry schema and fail-closed integration tests; (5) ledger/outbox/idempotency invariants; (6) audit chain verifier and tamper tests; (7) infrastructure and recovery manifests; (8) workload sizing tests against the stated capacity baseline; and (9) gate evidence linking commit, artifact digest, configuration digest, test report, reviewer, and decision.

The blueprint is complete as a specification. Runtime production readiness is earned only when implementation evidence passes G0–G11; no documentation statement substitutes for a test, security review, restore drill, or approval.
