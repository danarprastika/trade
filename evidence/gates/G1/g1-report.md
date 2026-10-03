# G1 — Domain Foundation: gate report

| | |
|---|---|
| **Gate** | G1 — Domain Foundation |
| **Criteria** | `docs/11_EXECUTION_GATES.md:9` |
| **Verdict** | **PASS** (recorded 2026-09-30T11:20:00Z; was FAIL until 08:00Z) |
| **Reported** | 2026-09-30T11:20:00Z |
| **Repository** | `C:\trade` |
| **Baseline commit** | `b971dda` (all work uncommitted) |
| **Reviewer** | self-attested, plus one independent read-only review (below) |

> **Scope limit on this PASS.** G1 is *Domain Foundation*. This pass means the
> nine criteria in `docs/11_EXECUTION_GATES.md:9` are implemented and verified.
> It does **not** mean the platform can trade. Nothing can place an order: there
> is no repository, no OMS orchestrator, no ledger poster, no reconciliation
> executor, no identity service and no configuration service. The four domain
> packages are pure logic with no database access. Doc 11 **G3 (Trading Core)**
> is NOT_STARTED and is the next gate. Live trading remains disabled and G11 is
> BLOCKED.

`docs/11_EXECUTION_GATES.md:51`: *"A gate is PASS only when every listed
criterion is satisfied; partial completion is FAIL, not a percentage."*

---

## Why PASS, and why that word is narrow

All nine criteria are implemented and verified. Two of them — canonical IDs and
domain tests — were **not** at any earlier point in this work, and both were
unmet for the same underlying reason: something existed in one place and its
counterpart did not, and nobody had compared them.

Canonical IDs were the sharper case. `contracts.NewID` and
`common.new_canonical_id` agreed on alphabet, length and entropy — everything a
format test can observe — were documented as "opaque so the encodings may differ
by design", and produced different identifiers. That sentence was an assumption
recorded as a justification, and `0020` found it false.

Domain tests were unmet because there was no domain code to test. Four packages
now hold the state machines, halt precedence and the 15-control risk gate, with
78 unit tests and 6 Go/SQL parity tests, mutation tested 11/11.

Two things this report is deliberately **not** claiming. It is not claiming the
platform can trade — nothing can place an order, because no service wires these
packages to persistence; that is G3. And it is not claiming the domain packages
are free of defects. Three were found and fixed while writing them, one of them
a test that passed while asserting nothing, and mutation testing is what found
it. The evidence supports the criteria being met; it does not support the
absence of further bugs.

An earlier draft of this evidence set carried `result: PASS` while two criteria
were unmet. That was wrong, and the report said FAIL until both were closed.
See "Correction record" below.

---

## Criterion-by-criterion

| # | Criterion (`11_EXECUTION_GATES.md:9`) | Verdict | Primary evidence |
|---|---|---|---|
| 1 | Canonical IDs | **SUPPORTED** | `contracts/id.go` (`IDFromBytes`), `common.canonical_id_from_bytes`, `TestTheGoAndSQLCanonicalEncodersAgree` (8 vectors × 29 entity types, mutation tested) |
| 2 | Timestamps | **SUPPORTED** | `contracts/time.go`, `common.ns_bound_to_timestamptz`, `TestNsToTimestamptzRoundsToTheNearestMicrosecond`, `TestARecordWhoseTimestampAndNanosecondsDisagreeIsRefused` (mutation tested) |
| 3 | Money/quantity types | **SUPPORTED** | `contracts/money.go` (`big.Int`, no `float64`), `common.assert_no_floating_point()` in all 21 migrations, `TestNoFloatingPointColumnsInAuthoritativeSchemas` |
| 4 | Errors | **SUPPORTED** | `contracts/errors.go`, `TestSchemaFileDeclaresSameVocabulariesAsGo` |
| 5 | Envelopes | **SUPPORTED** | `contracts/envelope.go`, `CommandEnvelope.Validate`, `EventEnvelope.Validate` |
| 6 | Idempotency | **SUPPORTED** | `contracts/idempotency.go`, `ops.idempotency_record`, `TestDuplicateCommandCannotCreateASecondOrder` |
| 7 | Configuration boundaries | **SUPPORTED** | `0017`, `TestAConfigDigestThatDisagreesWithItsDocumentIsRejected`, mutation tested 8/8 |
| 8 | Migrations | **SUPPORTED** | 21 of 21 applied, `drift=0`, per-file SHA-256 verified by `db/migrate.ps1` |
| 9 | Domain tests | **SUPPORTED** | `domain/oms`, `domain/strategy`, `domain/halt`, `domain/risk` — 78 unit tests, plus 6 Go/SQL parity tests in `dbtest/domain_parity_test.go`, mutation tested 11/11 |

### 9. Domain tests — SUPPORTED (was UNSUPPORTED)

This was the only unmet criterion, and it was unmet for one reason: there
was no domain code to test. `evidence/gates/G1/structural-controls.md`
recorded that "only the schema and the canonical contracts exist". The 207
database tests were negative probes against schema objects — attempt a
forbidden write, assert it is refused. They are real tests, they caught
nineteen genuine defects, and they are not domain tests.

Four packages now exist, holding the domain logic doc 04 specifies:

| Package | Owns | Tests |
|---|---|---|
| `domain/oms` | the closed order state machine, 26 transitions | 28 |
| `domain/strategy` | the strategy lifecycle, 12 transitions with actor authority | 28 |
| `domain/halt` | halt hierarchy, precedence and scope resolution | 22 |
| `domain/risk` | the 15-control deterministic pre-trade gate | 59 |

Doc 09 assigns unit tests to exactly this: "Unit tests cover domain invariants
and deterministic risk rules." That is now true.

**What this does not establish.** It does not mean trading works. Nothing can
place an order. There is still no repository, no OMS orchestrator, no ledger
poster, no reconciliation executor, no identity service and no configuration
service — the four packages above are pure logic with no database access and no
network. Wiring them to persistence and running the integrated system is
doc 11 **G3 (Trading Core)**, which remains NOT_STARTED. The four packages are
the foundation G3 builds on, not G3 itself.

**Authority and the two-implementation problem.** `0013` already enforces the
OMS state machine in PostgreSQL, and `0003` and `0007` already hold the
strategy transitions and the halt rank. So each machine now exists twice: once
in Go, once in SQL. That is the exact shape of the canonical-ID defect this
report already records — two encoders that agreed on everything observable and
produced different values, justified by a comment.

Rather than assume parity, `dbtest/domain_parity_test.go` reads the **live
tables** and compares them row for row against the Go tables: the 26 OMS
transitions with their two boolean attributes, the 12 strategy transitions with
required role and dual-control flag, `ops.halt_rank` for all five levels, both
state enums, and the `REDUCE_ONLY`/`CLOSE_POSITION` predicate for every order
type in the enum. Reading the database rather than the migration file matters:
these compare against what is actually enforced, not against what a file once
said.

`0013`'s comment claiming the two canonical encoders "did not need to" match
was corrected in place by `0020`; the equivalent claim is now tested rather than
asserted.

#### 21 — the strategy lookup silently dropped three rules

Found by the first test written for the new package.
`strategy.Rule` was keyed on `From` alone, assuming the lifecycle was a strict
chain with one successor per state. It is not: `DEPLOYED` reaches both `PAUSED`
and `RETIRED`, `PAUSED` reaches both `DEPLOYED` and `RETIRED`, and `SHADOW`
reaches both `APPROVED` and `RETIRED`. Three of the twelve rules were therefore
never returned by any lookup, and the three transitions they governed — pause,
resume and approve — reported "no such rule in the closed set".

The failure was quiet in the worst way: the table was fully populated, the
package compiled, every test passed, and the three missing rules were exactly
the ones governing the ability to pause, resume and approve a deployed
strategy. `TestMultiSuccessorStatesAreNotLosingRules` names those three states
explicitly.

#### 22 — one risk fact had inverted polarity

`OrderFacts.DuplicateOrder` was the only field in a struct where `true` meant
*failed*; every other field used `true` to mean *passed*. A caller who assumed
uniformity would have passed every order through the duplicate-order check,
because the engine denied on `false` and that field's `false` meant "no
duplicate found". It is now `NoDuplicateOrder`, and the name states which way
the truth value points. Found by `TestAFullyConfiguredOrderIsApproved`.

#### 23 — a test that passed while asserting nothing

Found by mutation C in `db/mutate_domain.ps1`, and the most instructive result
in this increment. `TestRiskIncreasingTransitionsRequireADecision` collected the
transitions whose `RequiresRiskDecision` was true and asserted on them. Clearing
that flag on `RISK_APPROVED -> SUBMITTING` — which removes the control from
exactly the transition doc 09 invariant 1 exists to protect — removed the rule
from the collection, so the test passed having checked nothing.

Only the Go/SQL parity test noticed. The test now compares the table against a
literal list of the three exposure-creating transitions written out in the
source. `TestDualControlIsRequiredForRiskAuthorisingTransitions` had the same
shape and was rewritten the same way.

A test that derives its expectations from the thing under test verifies
self-consistency, not correctness. This project has hit that three times now —
the `LedgerBalanceAssertionCoveredNothing`, the `ExpectRejectedBecause`
no-op path, and this one — and it is the reason the mutation suite exists.

#### Mutation testing: 11/11 caught

`db/mutate_domain.ps1` disables one behaviour per run: the strategy lookup
key, UNKNOWN escapable by assumption, the risk-decision requirement, the
mandatory-failure rejection, an unmeasurable figure, halt precedence,
risk-reducing actions, dual control, boundary inclusivity, an unexamined halt
gate, and the required-role check. All eleven are caught by named tests, none by
a compile error.

### 1. Canonical IDs — SUPPORTED (was PARTIAL, closed by 0020)

`common.new_canonical_id` (`0013:412-424`) mapped each random byte to a
character with `get_byte(b,i)/8`. `contracts.NewID` accumulated a big-endian bit
stream at 5 bits per character. Both emitted 20 Crockford characters with 100
bits of entropy, both satisfied `is_canonical_id`, and **neither produced the
other's values** — while `0013` described the SQL function as "the SQL twin of
contracts.NewID".

`0020` rewrites the SQL encoder to mirror `contracts.encodeCrockford` line for
line: expand the bytes into a most-significant-bit-first stream, take five bits
at a time. `contracts.IDFromBytes` was added so the encoding can be exercised
from fixed entropy rather than random values, and
`TestTheGoAndSQLCanonicalEncodersAgree` pins the two implementations against
each other on 8 vectors — all-zero, all-one, ascending, descending, a lone high
bit, a lone low bit, alternating nibbles, and a 0..15 walk — for all 29 entity
types. Mutation testing reverts the SQL encoder to the pre-0020 per-byte
mapping and reverses the bit order within each group; both are caught.

The `ns_bound`/`assert_canonical_entropy` guard was originally written as a
standalone function that nothing called — a control that exists and is never
invoked, the same shape as defect 15. Mutation test C caught it, and the guard
is now invoked from inside the encoder. That is the fourth time in this project
that mutation testing has found a control which was present in the source and
absent from the behaviour.

### 2. Timestamps — SUPPORTED (the ns gap is closed by 0021)

`occurred_at_ns` / `recorded_at_ns` sat beside their `TIMESTAMPTZ` columns with
nothing connecting them, so a record could carry an `occurred_at` of
2026-09-30T03:00:00Z beside an `occurred_at_ns` describing 2020-01-01T00:00:00Z.

An earlier consistency check had been written and **removed**, on the reasoning
recorded at `0009:216`: *"a caller that samples the clock for the nanosecond
value and then lets the statement fill the TIMESTAMPTZ from now() samples the
clock twice, so a real, correct record trips the check."* That diagnosis was
correct and the conclusion was wrong. The check was sound; the callers were
unsound. `0021` keeps the comment, corrects the conclusion, binds the stored
pair, and — because TIMESTAMPTZ has microsecond resolution — states the true
relationship rather than an equality that could never hold: **the TIMESTAMPTZ is
the nanosecond value rounded to the nearest microsecond**, with
`common.ns_to_timestamptz` as the single definition used by the constraint, the
fixtures and any consumer.

Binding it exposed a real defect immediately. The shared audit fixture passed
`NowNs()` from Go for `occurred_at_ns` while PostgreSQL filled `occurred_at`
from its own `now()` — two clock reads, microseconds apart, disagreeing, and
accepted for as long as they existed. The same pattern appeared in two other
audit fixtures. All three now derive both columns from one instant.

Binding it also caught a defect in the constraint itself: the first version used
a `VOID`-returning function, and a void value is never `NULL`, so
`CHECK (assert(...) IS NULL)` was false for every row. `common.ns_bound_to_timestamptz`
returns a predicate instead. A check is a predicate.

---


## Evidence artifacts

The digests of every evidence file live in `evidence/manifest.sha256`, generated
by `evidence/evidence.sha256.ps1` and checked by the same script with `-Verify`.

They are not written out here, and that is a correction rather than a stylistic
choice. An inline digest is stale the moment either file is edited, and this
report quotes `structural-controls.md`, which had been edited — so the table
this section replaces was already asserting something false while looking like
verification. The same reasoning that put a drift check on migrations
(`db/migrate.ps1`) applies to the evidence that certifies those migrations:

```
powershell -NoProfile -ExecutionPolicy Bypass -File evidence/evidence.sha256.ps1 -Verify
# evidence digest check passed: files=6 drift=0 unlisted=0   (exit 0)
```

The verifier fails nonzero on a changed digest, a missing file, **and on a file
present in the tree but absent from the manifest**, because an evidence file
nobody recorded is an unrecorded claim. Both failure modes were exercised: a
one-line tamper produced `DRIFT` and exit 1, and a stray `probe.tmp` produced
`UNLISTED` and exit 1. After each, the file was restored and the verifier
returned to passing, so the check is demonstrated in both directions rather than
merely asserted to work.

Regenerate with `evidence/evidence.sha256.ps1` (no `-Verify`) after editing any
evidence file.

## Verified state at time of report

```
applied_now=21 total_files=21 failures=0 drift=0
contracts  60 tests, all pass
domain     78 tests (oms 16, strategy 17, halt 17, risk 28), all pass
dbtest    213 tests, all pass
total     351 top-level tests, all pass, three consecutive full-suite runs
gofmt / go build / go vet: clean
```

Mutation testing: `0016` 5/5, `0017` 8/8, `0018` 7/7, `0019` 8/8, `0020`+`0021` 6/6, `domain` 11/11 — every one
caught by a distinct test that names the control it removed.

---

## Independent review

A read-only review was run against this evidence set by a second agent that had
not authored it. It returned **FAIL** and raised four blockers and six major
findings. Disposition of each:

| ID | Finding | Disposition |
|---|---|---|
| B1 | Evidence stale: claimed 18 migrations / 231 tests while 19 / 256 existed | **Accepted.** Corrected in `structural-controls.md` and in this report. |
| B2 | G1's own scope statement concedes the domain services do not exist | **Accepted, and now closed.** This was the primary reason for the earlier FAIL. Four domain packages now exist with 78 unit tests. The *services* still do not, which is G3 scope, not G1. |
| B3 | A tautological CHECK shipped in `0002:213` and survived 16 migrations | **Accepted.** Recorded as defect 17 and fixed in `0019`. It was found by the review, not by the original audit. |
| B4 | `mutate_0017.ps1` mutation G's anchor does not exist, so 8/8 was not reproduced by the script as written | **Refuted.** The anchor is present at `0017:290`; the reviewer's search missed it. `db/mutate_0017.ps1` was re-run and reproduced 8/8, including mutation G, with each mutation caught by its named test. |
| M1 | `sequence_no_regression` is a non-negativity test, not a regression test | **Accepted.** Recorded as defect 18 and fixed in `0019`. |
| M2 | `dbtest` count claimed 171, actual 172 | **Accepted** (superseded; now 196). |
| M3 | Go/PostgreSQL canonical ID encodings diverge with no cross-check | **Accepted** as a scope limitation; see criterion 1. |
| M4 | Halt and operational-mode gates cover the OMS state machine only | **Accepted** as an outstanding gap, unchanged. |
| M5 | Evidence claimed PASS while its own gaps list unimplemented mandatory items | **Accepted.** This report is the corrected verdict. |
| M6 | One test was green while exercising a different control than the one it named | **Accepted** as a known pattern; see correction record. |
| N1–N5 | Positive findings on `ExpectRejectedBecause`, deferred-trigger helpers, `TestCanonicalAuditPayloadMatchesDatabaseImplementation`, mutation-anchor verification, and the no-floating-point control | Recorded as evidence strength. |

Four of four blockers were checked against the repository before being acted on.
One did not survive that check.

---

## Correction record

Three corrections were made to this evidence set as a direct result of the
review, and are recorded here because a gate document that quietly fixes its own
findings is not an audit trail.

1. **The verdict was wrong.** The evidence previously read `result: PASS`. Under
   `11_EXECUTION_GATES.md:51` it is FAIL. Two criteria are unmet and one of them
   is not waivable.
2. **A defect was missing.** Defects 17 and 18 (the tautological CHECK and the
   mis-named sequence constraint) were found by the reviewer, not by the audit
   that produced defects 1–16. They are now in the defect table and are fixed in
   `0019`.
3. **A reviewer finding was wrong and was not actioned.** B4 was checked by
   searching `0017` for the mutation G anchor before changing anything. It was
   present; the mutation harness was re-run and reproduced its stated result.

---

## What was required to make G1 pass

Two things, both of which were code rather than documentation. Neither is
"nearly done".

1. **Domain tests.** There had to be domain code to test. `domain/oms`,
   `domain/strategy`, `domain/halt` and `domain/risk` now exist with 78 unit
   tests, 6 Go/SQL parity tests, and 11/11 mutation coverage. Finding the
   defects recorded as 21, 22 and 23 above is what the tests were for.
2. **Canonical IDs.** `contracts.NewID` and `common.new_canonical_id` produced
   different identifiers from the same entropy, behind a comment asserting that
   was fine. `0020` made them identical and pinned them with a parity test over
   8 fixed vectors and all 29 entity types.

Both were unmet at the point this report last said FAIL. Neither is waived,
rounded up, or argued toward.

## What is NOT established

Stated plainly, because a PASS on G1 is easy to over-read:

- **Nothing can place an order.** No repository, OMS orchestrator, ledger
  poster, reconciliation executor, identity service or configuration service
  exists. The four domain packages are pure logic with no database access.
- **G3 (Trading Core) is NOT_STARTED** and is the next gate. It is where the
  services, the integrated order path, and the ledger and reconciliation
  behaviour get built and tested.
- **The risk gate denies by default.** With no limits configured, every
  risk-increasing order is rejected as `not configured`. That is doc 17 §3
  working as written — the platform supplies no universal numeric trading
  limits — but it means the gate has never approved anything in a real
  configuration, and the code path that *permits* a limit-compliant order is
  exercised only by unit tests, never against a live policy.
- **Doc 17 §3's six-attribute limit schema is still unimplemented.** This
  increment built the attribute *type* and its validation, which closes the
  "no schema at all" gap, but the JSON document schema for a persisted policy
  still needs an owner. The `Limit` type is now that schema's shape in Go; it is
  not a persisted format.
- **Feed freshness (0019 16c) remains open**, as do the invariant-8 primary
  control and `audit.verify_checkpoint_signature_crypto`.

The defects found while building this are recorded above because a gate report
that lists only its successes is not reviewable. Nineteen defects in the schema
layer, three in the domain layer, and two test defects that were passing while
asserting nothing.

---
## Scope of this report

This report covers the **schema and canonical-contract surface only**. It says
nothing about API, adapters, the web application, workers, or live trading.
Live mode remains disabled; G11 requires a separate, dual-approved,
scope-specific activation and is not attempted.
