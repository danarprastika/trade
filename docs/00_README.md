# AI Trading Company — Final Production Blueprint

## Status

This package contains 26 authoritative Markdown specifications and a manifest. It is the single engineering authority for implementation. It replaces every earlier documentation structure. Earlier version folders, draft documents, superseded concepts, historical archives, and duplicate specifications are intentionally excluded.

**Specification completeness: 100%.** The package defines the product boundary, authoritative decisions, security and identity controls, compliance eligibility process, interfaces, operational targets, failure semantics, acceptance gates, and implementation sequence. All account-specific facts and runtime evidence are handled by explicit deny-by-default gates rather than guessed values.

The phrase production-ready in this document means **production-ready as an engineering blueprint**: the architecture, contracts, operational standards, security controls, testing model, recovery model, and acceptance criteria are fully specified. A running deployment still requires execution of the defined verification evidence; evidence generation is an implementation activity, not an unresolved design question.

## Product boundary

The system is an AI-native proprietary trading platform with separate market scopes for crypto, FX, equities, and commodities. It supports Research, Backtesting, Simulation, Paper, Shadow, and Live operating modes. Live mode is isolated from lower environments and remains disabled until the defined technical and owner approval gates are satisfied.

The platform is designed around one non-negotiable authority rule:

> AI may propose. Deterministic controls decide. The OMS records the authoritative order state. Reconciliation resolves external truth. The ledger records financial facts.

## Authoritative technology baseline

| Layer | Technology | Authority |
|---|---|---|
| Operator UI | TypeScript, React, Next.js App Router | Presentation only |
| API / control plane | Go, Chi, pgx, sqlc | Authoritative application layer; exact supported toolchain pinned at repository bootstrap |
| Research / backtest | Python | Offline/non-authoritative research; exact supported toolchain pinned at repository bootstrap |
| Safety-critical extensions | Rust | Only where an approved ADR requires memory safety/performance; exact stable toolchain pinned per component |
| Database | PostgreSQL 17 | System of record |
| Event delivery | PostgreSQL transactional outbox; NATS JetStream when asynchronous scale requires it | Delivery infrastructure, not financial authority |
| Cache | Redis 8 | Ephemeral only; never source of financial truth |
| Observability | OpenTelemetry, Prometheus-compatible metrics, Grafana-compatible dashboards | Operational evidence |
| CI/CD | GitHub Actions | Controlled promotion |
| Containers | OCI images, Docker/BuildKit | Reproducible packaging |
| Infrastructure | Terraform | Declarative infrastructure |

## Full-scale enterprise release posture

This is not an MVP architecture. The baseline includes long-lived configuration governance, feature-flag safety, dependency resilience, migration discipline, chaos engineering, business continuity, cyber recovery, supply-chain provenance, accessibility, privacy lifecycle controls, capacity governance, incident management, and quarterly architecture review. `24_ENTERPRISE_RELEASE_STANDARD.md` is binding for these cross-cutting requirements. `25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md` closes the deep audit and defines the binding full-scale release evidence profile.

## Enterprise controls included

The baseline includes Zero Trust access, OIDC-based SSO with SAML federation at the identity-provider boundary, granular RBAC plus contextual authorization, phishing-resistant step-up authentication, SCIM lifecycle support, dual-control approval for privileged financial changes, break-glass access, tamper-evident audit chaining with independent immutable retention, jurisdiction/venue eligibility profiles, policy versioning, data lineage, key rotation, supply-chain provenance, and tested regional recovery. These controls are implementation requirements, not optional enhancements.

## Toolchain lifecycle

The architecture does not freeze obsolete compiler/runtime patch versions into the blueprint. At repository bootstrap, engineering must select the latest stable release that is still security-supported for each approved language/runtime, pin exact versions and dependency hashes in repository toolchain/lock files, and record the selected versions in the G1 evidence. Unsupported or end-of-life versions are prohibited. Security patches are evaluated monthly and applied within 14 days for critical/high severity fixes; routine supported patch updates are applied quarterly. A major-version upgrade requires compatibility tests, migration notes, and an approved ADR when behavior or operational assumptions change. CI must fail on unpinned dependencies or toolchain drift.

## Canonical repository shape

```text
/apps/web
/services/control-plane
/workers/research
/workers/backtest
/components/risk-engine
/components/oms
/components/reconciliation
/adapters/venues
/contracts
/db/migrations
/infra/terraform
/ops/runbooks
/tests
/docs
```

## Operating principle

No component may create a second source of truth for order status, financial balances, risk authorization, or identity. Polyglot components communicate through versioned contracts and explicit ownership boundaries.


## Final architecture decision addendum

`23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md` is the binding closure for deployment topology, policy enforcement, eventing thresholds, disaster recovery, jurisdiction eligibility, toolchain lifecycle, API limits, SLO measurement, and live activation evidence. Where it is more specific than an earlier summary, the addendum controls.

Initial jurisdiction posture: Indonesia is the initial review target only when explicitly declared by the account holder; location signals are never used to infer legal residence. This is not a legal approval. Live capability remains disabled until current eligibility evidence is approved for the exact account, venue, product, and activity. Under-age or otherwise ineligible users cannot activate live capability; no age or identity control may be bypassed.


## Final deep audit

`25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md` is the binding audit addendum for full-scale enterprise classification, architecture invariants, failure-mode disposition, enterprise capability evidence, and document precedence. It does not substitute document completion for runtime G0–G11 evidence.
