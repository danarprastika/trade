# Testing and Release Evidence

## Test pyramid

Unit tests cover domain invariants and deterministic risk rules. Integration tests cover PostgreSQL transactions, outbox behavior, adapter contracts, and reconciliation. End-to-end tests cover complete command-to-ledger flows. Property tests cover state-machine invariants. Fault-injection tests cover timeouts, duplicate events, stale data, venue failure, database failover, and credential rejection.

## Mandatory financial invariants

1. No risk-rejected command creates a live submission.
2. Every accepted order has exactly one canonical OMS identity.
3. Duplicate commands do not increase exposure.
4. Ledger corrections are compensating entries.
5. Portfolio positions reconcile to validated fills.
6. Unknown submission outcomes trigger reconciliation.
7. Halt state blocks prohibited actions.
8. Lower-environment credentials cannot access live resources.

## CI gates

Every merge: formatting, lint, unit tests, contract validation, SAST, dependency scan. Protected branches additionally require integration tests and artifact build. Release candidates require end-to-end, performance, fault injection, migration rehearsal, backup restore verification, SBOM generation, provenance signing, and security approval.

## Release strategy

Use immutable versioned artifacts. Promotion is dev -> test -> staging -> paper -> shadow -> live. Rollback restores the previous artifact and configuration snapshot. Database migrations must be backward-compatible with the immediately previous release.

## Evidence package

Each release stores commit SHA, artifact digest, test report, coverage report, security scan results, SBOM, migration result, deployment manifest, configuration fingerprint, approval records, and rollback verification.
