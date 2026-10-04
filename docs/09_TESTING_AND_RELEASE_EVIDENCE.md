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
| `db/mutate_0019.ps1` | The migration series itself, proving each rule it enforces is load-bearing | `9/9` caught |
| `db/mutate_0020_0021.ps1` | The same, for the canonical-ID bit layout and the nanosecond binding | `6/6` caught |
| `db/mutate_0018.ps1` | The same, for the reconciliation gate | `6/6` caught |
| `db/mutate_0016.ps1`, `mutate_0017.ps1` | The same, for the position-reconciliation and configuration-boundary migrations | `5/5` and `8/8` caught, re-run 2026-10-03 |

The re-run covered every harness whose extraction or pattern changed. `0019`, `0020` and `0021` were last touched on 2026-10-03, and `mutate_0020_0021.ps1` builds five of its six mutation patterns inside the script rather than as literals, so its anchors cannot be confirmed by reading the file and only running it settles them. `0016`, `0017` and `0018` were re-run because the failure-extraction defect described below invalidated the verdicts they had recorded; all three passed again, with the same tallies and materially fuller attribution.

**A mutation aimed at a superseded definition reports itself honestly, and is still worthless.** `mutate_0018.ps1` recorded a mutation A that removed the reconciliation gate call from `ops.assert_risk_increase_permitted` in migration 0018. Migration 0019 re-issues that function, so 0018's body is discarded at apply time: the mutation changed the live schema by zero bytes, and the harness printed "no test failed -- the removed behaviour was NOT load-bearing". The harness was correct and the claim it supported was not. A green mutation harness proves only that the mutations it ran did something; a mutation that silently targets a dead copy is indistinguishable from a control nobody wrote, and the tally it contributes to is worse than no tally, because it looks like coverage.

The check that catches this is not a stronger pattern. It is asking, for each mutation, which migration supplies the definition the live database is actually running. `reconciliation.assert_break_permitted` and `reconciliation.scope_covers` are supplied only by 0018, so mutations B–G belong there; `ops.assert_risk_increase_permitted` is supplied by 0013, 0018 and finally 0019, so the gate-call mutation belongs to 0019 and now runs there as mutation I. Every harness in this table should be read the same way: confirm the migration that owns the definition before trusting the tally.

**And a harness can reach a verdict from a single test while its tally still reads as complete.** The five harnesses that decide `MustFailPattern` extracted failures with `Select-String -InputObject $out -Pattern '^\s*--- FAIL: (\w+)'`. With `-InputObject` taking the joined `go test` output as one string, `^` anchors to position zero rather than to each line, so the expression returned the first failing test and dropped every other. Measured against a three-failure input it returned one name; the `(?m)` form returned three. Five tallies — 0016 5/5, 0017 8/8, 0018 6/6, 0019 9/9, 0020/0021 6/6 — were each decided from one test while presenting as complete coverage.

It was not found by reading the code. It was found because a reviewer disagreed with a recorded number: mutation I was documented as caught by exactly one test, when removing the gate call leaves `reconciliation.assert_break_permitted` with no caller anywhere in the series and three tests must fail. Re-running with the corrected extraction produced that answer and also surfaced a genuinely over-broad `MustFailPattern` on 0019's mutation F, which had been reported as caught only because the first test to fail happened to match its pattern. No tally moved. What moved is that they now mean what they say — a tally is worth exactly as much as its weakest extraction step.

Each harness makes the same four guarantees, and each exists because its absence produced a false result at least once:

1. **The mutation is proven to have applied.** A pattern that no longer matches leaves the file untouched and is recorded as not-detected, never as a pass. A replacement that is not valid SQL is likewise recorded as not-detected rather than aborting the run. A mutation that does not apply proves as little as one that fails to compile.
2. **The mutation is caught by the assertion that names it.** Being caught by some unrelated test that happened to break is not proof, so a mutation whose failing tests fall outside its `MustFailPattern` is reported as over-broad and fails the run. This only works if *every* failing test is enumerated, and the enumeration has to be multiline-aware: `Select-String -InputObject $out -Pattern '^\s*--- FAIL: (\w+)'` anchors `^` to the start of the whole joined `go test` output, so it returns the first match and drops the rest. Five harnesses used that form and were therefore deciding every verdict from one test out of all of them. `db/mutate_domain.ps1` carries the correct idiom: `[regex]::Matches($out, '(?m)^\s*--- FAIL: (\w+)')`.
3. **Collateral failure is declared, never absorbed.** A mutation that changes something widely depended on will fail tests that are not about the control it removed. Widening the `MustFailPattern` to cover them hides the very distinction the over-broad check draws, so `db/mutate_0020_0021.ps1` takes a separate `-ExpectedCollateral` parameter instead: a collateral failure has to be named, with a comment saying why it is collateral. Anything undeclared still fails the run.
4. **The original is restored.** Every mutation is written to a copy that is restored in a `finally` block, and the harness refuses to report success unless both the gate and `db/migrate.ps1` are green afterwards.

Negative controls are what make the positive results mean anything. Each dimension pairs a mutation that must be detected with one that must **not** be: an index left `NOT VALID` by its own migration, a trigger dropped and recreated inside a single migration file, and a correct database reported clean. If a control ever fires, the harness is detecting its own construction rather than the defect it was built to find.

Restoring the mutated state is the step that fails quietly, so a tally is only meaningful when the repository and the database were inspected afterwards. `try/finally` restores a file when a script fails; it does not run when a process is killed, so a terminated run can leave a mutated migration behind that makes the next run report drift against a file nobody remembers editing. Treat a killed harness run as suspect until `git status` on `db/migrations` is clean.

## CI gates

Every merge: formatting, lint, unit tests, contract validation, SAST, dependency scan. Protected branches additionally require integration tests and artifact build. Release candidates require end-to-end, performance, fault injection, migration rehearsal, backup restore verification, SBOM generation, provenance signing, and security approval.

## Release strategy

Use immutable versioned artifacts. Promotion is dev -> test -> staging -> paper -> shadow -> live. Rollback restores the previous artifact and configuration snapshot. Database migrations must be backward-compatible with the immediately previous release.

## Evidence package

Each release stores commit SHA, artifact digest, test report, coverage report, security scan results, SBOM, migration result, deployment manifest, configuration fingerprint, approval records, and rollback verification.
