# System Architecture

## 1. Runtime topology

```text
Browser / Telegram
       |
       v
API Gateway / Web Application Firewall
       |
       v
Go Control Plane
 |       |        |        |        |
Auth  Strategy   Risk     OMS   Reconciliation
 |       |        |        |        |
 +-------+--------+--------+--------+
                  |
             PostgreSQL 17
                  |
          Transactional Outbox
                  |
          Event Dispatcher
                  |
            Venue Adapters
                  |
             External Venue

Python Research/Backtest ---- versioned artifacts ----> Control Plane validation
Rust components ------------- contract boundary -----> owning Go service
```

## 2. Bounded contexts

1. Identity and Access — users, service identities, roles, scopes, sessions.
2. Configuration — environment-scoped configuration and immutable release snapshots.
3. Market Data — normalized instruments, prices, candles, order-book snapshots, feed health.
4. Research — datasets, experiments, features, strategies, model artifacts.
5. Strategy — strategy lifecycle, signals, approvals, deployment state.
6. Risk — deterministic pre-trade and portfolio controls.
7. OMS — canonical order lifecycle.
8. Execution — venue-specific translation and submission.
9. Reconciliation — external-vs-internal state convergence.
10. Portfolio and Ledger — positions, cash, fees, funding, realized/unrealized P&L, immutable financial facts.
11. Intelligence — news and contextual market information, never authoritative for execution.
12. Audit — append-only security, decision, configuration, and financial audit records.
13. Operations — health, alerts, incidents, deployment, recovery.

## 3. Authority hierarchy

1. Owner/operator approval controls activation and high-impact configuration.
2. Risk Engine is the authoritative financial veto.
3. OMS is authoritative for internal order state.
4. Reconciliation is authoritative for resolving venue discrepancies.
5. Ledger is authoritative for financial facts.
6. Strategy and AI components propose actions only.
7. UI, Telegram, research workers, caches, and analytics projections have no financial authority.

## 4. Environment isolation

Environments are `dev`, `test`, `staging`, `paper`, `shadow`, and `live`. Each environment has separate credentials, database, encryption keys, deployment identity, network policy, and configuration namespace. Live credentials are never available to lower environments.

## 5. Deployment model

The first production implementation is a modular Go control-plane deployment with independently deployable TypeScript frontend, Python workers, and venue adapters. Services are separated operationally only when load, fault isolation, or security boundaries justify the split. Premature microservice fragmentation is prohibited.

## 6. Failure rule

When authoritative state cannot be established, the system fails closed for new risk-increasing actions, preserves existing observable state, records the uncertainty, and starts reconciliation. Unknown external outcomes are never converted into deterministic failure by assumption.

## 7. Enterprise control-plane decomposition

The Go control plane is organized into explicit modules with one-way authority flow:

| Module | Owns | Must never own |
|---|---|---|
| Identity | subject/session/context validation | financial state |
| Policy | RBAC/ABAC decisions and policy versions | order state |
| Configuration | typed configuration and release snapshots | runtime secrets |
| Risk | deterministic authorization/veto | venue connectivity |
| OMS | canonical order state machine | external venue truth |
| Reconciliation | convergence between internal and external observations | policy authority |
| Ledger | immutable financial facts | strategy decisions |
| Audit | security/decision/configuration evidence | business truth outside evidence records |
| Operations | deployment/recovery/health state | financial authorization |

No module may invoke another module by reaching around its public contract. Direct table writes outside the owning module are prohibited. Cross-module mutations occur through explicit application commands or transactional events.

## 8. State-machine discipline

Authoritative state machines are closed sets. Every state transition declares: actor, command, precondition, resulting state, event, idempotency key, audit record, and failure behavior. Unknown states, unknown transitions, missing sequence numbers, or invalid version vectors are rejected and placed into a controlled reconciliation path.

The OMS, reconciliation engine, configuration service, and recovery controller must use optimistic concurrency/version checks. Last-write-wins is prohibited for authoritative financial or security state.

## 9. Isolation boundaries

The system uses four hard trust zones: edge, control, research, and recovery. Research workloads cannot route to live control-plane data stores. Recovery infrastructure uses separately managed administrative credentials and trusted artifact manifests. The edge cannot access databases. Venue adapters cannot modify policy, risk configuration, or ledger facts.

## 10. Architectural invariants

1. No AI/model output can authorize its own execution.
2. No cache can become authoritative through fallback behavior.
3. No retry may transform an unknown external outcome into a second exposure-increasing command.
4. No restored environment may leave `RECOVERY_HOLD` without independent verification.
5. No stale authorization context may be accepted beyond the configured revocation window.
6. No undocumented configuration can enter the live configuration snapshot.
7. No schema migration may require simultaneous deployment of an incompatible producer and consumer.
8. No telemetry pipeline outage may disable preservation of mandatory security and financial evidence.
