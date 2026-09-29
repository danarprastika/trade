# Persistence, Eventing and Reconciliation

## PostgreSQL schema domains

Core schemas: `identity`, `config`, `market`, `strategy`, `risk`, `oms`, `execution`, `reconciliation`, `portfolio`, `ledger`, `audit`, `ops`.

Each schema owns its tables. Cross-domain writes occur through domain services, not ad-hoc SQL from unrelated modules.

## Transaction boundary

An authoritative command executes domain validation, state mutation, audit record creation, and outbox insertion in one PostgreSQL transaction. A committed state mutation always has a corresponding durable outbox record.

## Outbox

The dispatcher reads committed outbox rows using `FOR UPDATE SKIP LOCKED`, publishes the event, and marks delivery state. Duplicate publication is acceptable; consumers must be idempotent. Financial correctness never depends on exactly-once transport.

## Ledger

Ledger entries are append-only and balanced. Corrections are represented by compensating entries, never destructive updates. Every entry references its source command/event and correlation identifier.

## Reconciliation

Reconciliation runs continuously for live/paper venue adapters and at startup. It compares orders, fills, balances, positions, and fees. Differences create reconciliation cases with explicit severity. A material unresolved break blocks affected risk-increasing actions.

## Backup

Production PostgreSQL uses continuous WAL archiving, daily full backup, and weekly restore verification. Backups are encrypted, access-controlled, and retained according to the defined retention schedule: 35 daily restore points, 12 monthly restore points.

## Data retention

Operational logs: 90 days hot, 365 days archived. Audit and financial records: 7 years. Research artifacts: 3 years unless tagged as required historical evidence. Deletion never removes records under legal hold or financial audit retention.
