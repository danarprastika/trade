# Polyglot Engineering Standard

## TypeScript / Next.js

Responsibilities: operator UI, dashboards, configuration forms, audit views, research visualization, live operational controls.

Rules: no direct database access; no broker credentials; no order submission endpoint outside the Go API; all money values displayed from canonical decimal strings; all privileged actions require server-side authorization.

## Go

Responsibilities: authoritative API, domain services, Risk Engine, OMS, reconciliation, ledger writes, identity enforcement, configuration validation, adapter orchestration.

Rules: explicit errors, context propagation, structured logging, deterministic validation, SQL through sqlc-generated accessors, database transactions around state transitions, no floating-point financial arithmetic.

## Python

Responsibilities: data science, feature engineering, backtesting, experiment execution, offline model evaluation, dataset generation.

Rules: no live credentials, no direct venue submission, no authoritative ledger writes, artifact outputs must be content-addressed and versioned before promotion.

## Rust

Rust is permitted only for a bounded component where profiling demonstrates a material requirement for memory safety, deterministic performance, or specialized systems integration. Every Rust component requires an ADR and a contract test suite. Rust does not create an independent policy authority.

## Contract-first interoperability

JSON over HTTPS is the default synchronous boundary. Protobuf/gRPC is used for high-volume internal calls where measured throughput warrants it. AsyncAPI-compatible event schemas define event contracts. OpenAPI defines HTTP contracts. Schema changes require backward compatibility for one release window unless a coordinated migration is executed.

## Dependency policy

Dependencies are pinned by lockfiles. Production artifacts are built from immutable versions. Critical vulnerabilities require remediation or a documented compensating control before promotion. SBOM and provenance are generated for every release artifact. Toolchain selection follows the lifecycle rule in `00_README.md`: latest stable security-supported release at bootstrap, exact versions pinned, monthly security review, critical/high fixes within 14 days, routine patch updates quarterly, and no end-of-life runtime in a production artifact. CI rejects floating dependency versions, uncommitted lockfile changes, and toolchain drift.

## 8. Polyglot ownership and build discipline

Every executable component has exactly one owning team, one language/runtime, one build definition, one artifact identity, one operational owner, and one rollback procedure. Shared business rules are not copied between languages. Where a rule must cross a language boundary, the canonical contract is versioned and generated or validated from one authoritative schema.

### TypeScript boundary

The frontend is an untrusted client. It may request actions and render authoritative responses but cannot establish authorization, risk approval, order state, or financial balances. Browser storage may hold only non-sensitive presentation state. Tokens are handled through the approved identity flow and never embedded into URLs or logs.

### Go boundary

Go is the authoritative implementation language for control-plane state transitions. Domain rules that determine authorization, risk veto, OMS state, reconciliation, and ledger writes execute here. The implementation must expose deterministic unit-testable functions for every authoritative transition.

### Python boundary

Python is restricted to research, data preparation, offline evaluation, and backtest workloads. It receives immutable/versioned datasets and produces signed/versioned artifacts. It has no production credentials and no direct write path to authoritative financial tables.

### Rust boundary

Rust is an exception-based component language. It is introduced only through an ADR that documents the safety/performance requirement, FFI or protocol boundary, operational owner, observability, build provenance, compatibility contract, and rollback behavior. Rust is not a second location for business authority.

## 9. Cross-language correctness

Contract tests run in every producer/consumer language. Canonical decimal, timestamp, identifier, enum, error, and pagination representations are tested against shared fixtures. Serialization changes require backward/forward compatibility evidence. A component cannot promote if its contract fixtures disagree with the canonical contract repository.

## 10. Supply-chain baseline

Every artifact is built from a locked dependency graph and emits an SBOM, provenance statement, source revision, compiler/runtime identity, configuration schema version, and reproducible artifact digest. Release promotion verifies these values before deployment. Development convenience dependencies are excluded from production artifacts.
