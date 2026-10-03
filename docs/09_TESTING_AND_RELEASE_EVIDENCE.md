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

## Mutation testing

A gate that has only ever been run against a correct repository has been shown to do nothing: it reports clean with exactly the same confidence when the thing it checks has been removed. Every verification gate in this repository is therefore paired with a committed harness that breaks the gate on purpose and requires it to notice. These harnesses are evidence rather than tooling, and they are committed so the claim outlives the session that measured it.

| Harness | What it breaks | Verified tally |
| --- | --- | --- |
| `db/verify-schema-live-mutations.ps1` | The live schema gate across five dimensions: function bodies, triggers, indexes, named constraints, and the fail-closed parser | `assertions=25 not_detected=0`, every restore byte-identical |
| `db/mutate_domain.ps1` | Twelve domain invariants in Go source, one behaviour at a time | `12/12` caught, each by the test it names |
| `db/mutate_0019.ps1` | The migration series itself, proving each rule it enforces is load-bearing | `8/8` caught |
| `db/mutate_0020_0021.ps1` | The same, for the canonical-ID bit layout and the nanosecond binding | `6/6` caught |
| `db/mutate_0016.ps1`, `mutate_0017.ps1`, `mutate_0018.ps1` | The same, for the remaining migrations in the series | Proven when recorded; not re-run in the 2026-10-03 pass |

The re-run covered the harnesses whose migrations had changed recently. `0019`, `0020` and `0021` were last touched on 2026-10-03, and `mutate_0020_0021.ps1` builds five of its six mutation patterns inside the script rather than as literals, so its anchors cannot be confirmed by reading the file and only running it settles them. `0016`, `0017` and `0018` were last touched on 2026-09-30 and their literal anchors match the current migrations, which is as far as a static check can go: it shows a mutation would still apply, never that it is still caught.

Each harness makes the same three guarantees, and each exists because its absence produced a false result at least once:

1. **The mutation is proven to have applied.** A pattern that no longer matches leaves the file untouched and is recorded as not-detected, never as a pass. A replacement that is not valid SQL is likewise recorded as not-detected rather than aborting the run. A mutation that does not apply proves as little as one that fails to compile.
2. **The mutation is caught by the assertion that names it.** Being caught by some unrelated test that happened to break is not proof, so a mutation whose named test did not fail is reported as `NOT PROVEN` and fails the run.
3. **The original is restored.** Every mutation is written to a copy that is restored in a `finally` block, and the harness refuses to report success unless both the gate and `db/migrate.ps1` are green afterwards.

Negative controls are what make the positive results mean anything. Each dimension pairs a mutation that must be detected with one that must **not** be: an index left `NOT VALID` by its own migration, a trigger dropped and recreated inside a single migration file, and a correct database reported clean. If a control ever fires, the harness is detecting its own construction rather than the defect it was built to find.

Restoring the mutated state is the step that fails quietly, so a tally is only meaningful when the repository and the database were inspected afterwards. `try/finally` restores a file when a script fails; it does not run when a process is killed, so a terminated run can leave a mutated migration behind that makes the next run report drift against a file nobody remembers editing. Treat a killed harness run as suspect until `git status` on `db/migrations` is clean.

## CI gates

Every merge: formatting, lint, unit tests, contract validation, SAST, dependency scan. Protected branches additionally require integration tests and artifact build. Release candidates require end-to-end, performance, fault injection, migration rehearsal, backup restore verification, SBOM generation, provenance signing, and security approval.

## Release strategy

Use immutable versioned artifacts. Promotion is dev -> test -> staging -> paper -> shadow -> live. Rollback restores the previous artifact and configuration snapshot. Database migrations must be backward-compatible with the immediately previous release.

## Evidence package

Each release stores commit SHA, artifact digest, test report, coverage report, security scan results, SBOM, migration result, deployment manifest, configuration fingerprint, approval records, and rollback verification.
