# G1 / G3 — Structural Financial Control Evidence

| Field | Value |
|---|---|
| `gate_id` | G1 (domain foundation) / G3 (trading core) — **partial** |
| `result` | **SUPERSEDED — see `evidence/gates/G1/g1-report.md` for the authoritative verdict, which is PASS (recorded 2026-09-30T11:20:00Z)** |
| `superseded_by` | `evidence/gates/G1/g1-report.md`. The `FAIL` recorded in this file was correct as of `reviewed_at_utc` 2026-09-30T03:20:00Z and is retained unmodified below. It is superseded, not withdrawn: `g1-report.md` closed both unmet criteria — domain tests now exist (78 unit tests plus 6 Go/SQL parity tests), and `0020` made the Go and PostgreSQL canonical ID encodings identical with a parity test over 8 fixed vectors and all 29 entity types. Nothing in this file's body was edited to reach that verdict. |
| `scope` | Canonical contracts (Go) and the authoritative PostgreSQL schema, exercised by negative tests |
| `environment` | PostgreSQL 17.11, container `aitc-pg17`, host port 55439; Go 1.26.2 windows/amd64 |
| `migrations` | `0001_foundation` … `0019_market_data_instrument_and_symbol_enforcement`, 19 of 19 applied from an empty database |
| `evidence_locations` | `contracts/`, `db/migrations/`, `db/migrate.ps1`, `db/mutate_0016.ps1`, `db/mutate_0017.ps1`, `db/mutate_0018.ps1`, `db/mutate_0019.ps1`, `db/mutate_0020_0021.ps1`, `db/mutate_domain.ps1`, `dbtest/`, `evidence/gates/G0/`, `evidence/gates/G2/` |
| `reviewer` | Principal Engineering Agent (implementation executor), **plus one independent read-only review** (`g1-report.md`, "Independent review") |
| `reviewed_at_utc` | 2026-09-30T03:20:00Z |
| `exceptions` | See "Known gaps" — the gate does not cover domain services, API, adapters or the web application |
| `next_action` | G1 domain services and repositories beyond the audit path; the derivation path that *writes* positions, which no control yet produces |

> **Why FAIL, in short — SUPERSEDED, retained as the record of the state at
> `reviewed_at_utc`.** The verdict is now PASS; see the `result` and
> `superseded_by` rows above and `g1-report.md`. The reasoning below is left
> exactly as written because it was correct when recorded, and rewriting it
> would destroy the evidence that the correction was earned rather than
> asserted. Seven of the nine criteria at
> `11_EXECUTION_GATES.md:9` are supported by evidence that survived independent
> review. Two are not. **Domain tests** cannot be satisfied, because the domain
> services they would test do not exist — a fact this document states in its own
> Known Gaps section. **Canonical IDs** are verified within each language but not
> across the Go/PostgreSQL encoding boundary. Under
> `11_EXECUTION_GATES.md:51`, partial completion is FAIL, and the domain-services
> gap is not waivable.
>
> An earlier draft of this file read `result: PASS for the database control
> surface only`. That was wrong. The corrections made in response to the
> independent review — including two defects it found and one finding it got
> wrong — are recorded in `g1-report.md` under "Correction record".

## What this evidence does and does not claim

It claims that the **financial invariants are structural**, meaning the database
refuses a forbidden write rather than relying on application code. It does
**not** claim that the trading system exists: there are no domain services, no
repositories, no API, no adapters and no web application yet. The schema and the
contracts are the foundation those will be built on, and this report exists so
that the controls already proven here are not silently lost later.

## Commands and results

### 1. Migration application from an empty database

```powershell
db\migrate.ps1 -Container aitc-pg17 -Reset
```

```
OK    0001_foundation  (625 ms)
OK    0002_market_data  (401 ms)
OK    0003_strategy_and_risk  (561 ms)
OK    0004_oms_and_execution  (561 ms)
OK    0005_ledger_portfolio_reconciliation  (416 ms)
OK    0006_audit_integrity  (914 ms)
OK    0007_outbox_halts_ops  (517 ms)
OK    0008_structural_enforcement  (370 ms)
OK    0009_audit_canonical_hash  (585 ms)
OK    0010_checkpoint_hash_binding  (372 ms)
OK    0011_audit_details_digest  (305 ms)
OK    0012_checkpoint_key_binding  (297 ms)
OK    0013_halt_enforcement_and_chain_sev1  (351 ms)
OK    0014_operational_mode_enforcement  (436 ms)
OK    0015_workload_identity_environment_ceiling  (368 ms)
OK    0016_position_reconciles_to_validated_fills  (273 ms)
OK    0017_configuration_boundary_enforcement  (531 ms)
OK    0018_reconciliation_breaks_block_risk  (434 ms)
OK    0019_market_data_instrument_and_symbol_enforcement  (531 ms)
applied_now=21 total_files=21 failures=0 drift=0
```

All twenty-one migrations apply to a recreated database in a single clean run. This
matters because a migration chain that only works on an already-populated
database hides ordering dependencies.

### 2. Migration drift detection

`db/migrate.ps1` re-hashes every already-applied migration and compares it with
the digest recorded in `public.schema_migration`. The detector was proven by
deliberately perturbing an applied migration:

```
DRIFT 0007_outbox_halts_ops: applied digest a3aa31143bbed017969f1109eb3663e6c89e54c9677115b2bc566d8a1be4dc66
       != file digest 009a7dff5608181538b9c1e193fa25902d1563b135d486be9efd3a9989d74abf
applied_now=0 total_files=21 failures=0 drift=1
exit=2
```

After reverting the perturbation the run returns `drift=0` and `exit=0`. A
modified migration is a hard failure (exit code 2), not a skip.

### 3. Test suites

```powershell
$env:AITC_TEST_DATABASE_URL='postgres://postgres:aitc_local_dev_only@localhost:55439/aitc?sslmode=disable'
go test ./... -count=1
```

```
ok  	github.com/aitc/trade/contracts	2.149s
ok  	github.com/aitc/trade/dbtest	23.703s
```

| Suite | Tests | Result |
|---|---|---|
| `contracts` | 60 | all pass |
| `dbtest` | 213 | all pass |
| **total** | **351** | **all pass** |

The suite was run three consecutive times **without** re-migrating in between,
and passed three times. This was not true on the first attempt, and it stopped
being true again later for a different reason: the cross-transaction helper
added for `0016` truncated `audit.record` during cleanup, which desynchronised
the partition head and made the audit service tests fail on every run after the
first. Both are recorded under "Defect 13c" and under "Corrections made while
producing this evidence". The harness was corrected to generate per-run unique
identifiers, sequences and digests, and to clean only what it creates, rather
than weakening the append-only control to allow cleanup.

## Defects this gate found and fixed

The first version of the schema **declared** closed state machines and balanced
journals without **enforcing** them. Each item below was a real hole, confirmed by
a failing test, and each now has a negative test that fails if the control is
removed.

| # | Defect | Control installed | Negative test |
|---|---|---|---|
| 1 | A risk-increasing order could reach `SUBMITTING` with no risk decision. Nothing enforced `oms.state_transition_rule`; the table was documentation. | `oms.guard_order_update` requires the transition to exist in the closed rule set and, where `requires_risk_decision` is set, requires a durable `risk_decision_id` on the order. | `TestRiskIncreasingOrderCannotReachSubmissionStateWithoutDecision` |
| 2 | An order could be inserted directly into `SUBMITTING`, `FILLED` or any terminal state, bypassing the state machine entirely. | `oms.guard_order_entry_state` allows entry only at `CREATED` / `RISK_PENDING`. | `TestOrderCannotBeCreatedInATerminalState` |
| 3 | `oms.order` terms were mutable: quantity and side could be changed after acceptance, silently re-specifying the exposure a risk decision authorised. | `oms.guard_order_update` treats order identity terms as immutable; a changed order is a new command. | `TestOrderTermsAreImmutableAfterAcceptance` |
| 4 | `filled_quantity` could be set to any value, inflating position accounting with no venue-reported execution behind it. | `filled_quantity` must always equal the sum of the order's `FILLED` / `PARTIALLY_FILLED` event deltas, and `average_fill_price` must be positive and within the observed fill range. | `TestFillAccountingCannotBeAdvancedWithoutAFillEvent` |
| 5 | `oms.order_event` was a free-form log: it could claim transitions the order never made, and its sequence could skip. | `oms.guard_order_event` requires `from_state` to equal the order's current state, the sequence to continue without a gap, and the `(from, to, kind)` triple to exist in the closed rule set. | `TestOrderEventJournalRejectsAGapInTheSequence` |
| 6 | The strategy lifecycle rule table was unenforced, so a strategy could jump `DRAFT` → `DEPLOYED`. | `strategy.guard_strategy_transition` requires a recorded transition event and a rule row; dual-control transitions additionally require a live, unexpired, independent approval. | `TestStrategyCannotSkipItsGovernedPromotionPath`, `TestStrategyTransitionRequiresARecordedEvent`, `TestDualControlStrategyTransitionRequiresALiveApproval` |
| 7 | `ledger.assert_journal_balanced()` was a no-op stub. Nothing prevented an unbalanced internal journal from committing, and the old `ledger.assert_balanced()` summed a rolling one-day window, which is not a journal. | `ledger.journal` requires `debit_total = credit_total`; a DEFERRABLE INITIALLY DEFERRED constraint trigger re-verifies every journal at `COMMIT`; internal entry kinds must belong to a journal, and external one-sided flows must not invent one. | `TestBalancedJournalCommits`, `TestUnbalancedJournalCannotCommit`, `TestUnbalancedJournalHeaderIsRejectedAtInsert`, `TestInternalLedgerEntryMustBelongToAJournal` |
| 8 | A direct `INSERT` into `audit.record` bypassed the per-partition advisory lock, the sequence check and the chain head, despite the comment claiming `audit.append_record` was the only supported path. | `audit.reject_unauthorised_insert` independently re-derives the chain head and rejects any row that does not continue it, in addition to a transaction-local append token that `append_record` scopes to its own `INSERT` and then clears. | `TestDirectAuditInsertIsRejected` |
| 9 | `audit.record` was partitioned to 2027-03 only. The first record written in April 2027 would fail with "no partition of relation found", silently stopping evidence capture. | `audit.ensure_partition` creates the monthly partition on demand, called by `append_record` on every append. | `TestAuditPartitionIsCreatedOnDemandBeyondTheStaticRange` |
| 10 | `audit.append_record` took `p_record_hash` from the caller and stored it verbatim. The chain enforced *continuity* — sequence and `previous_hash` — but never recomputed the hash, so the party being audited chose the value. A chain whose hashes are supplied by its subject is not tamper-evident. | `audit.canonical_payload` and `audit.compute_record_hash` define the §3 canonical form in SQL; `append_record` computes the hash and rejects any caller-supplied value that disagrees. `audit.verify_partition` recomputes from stored content, and `audit.verify_partition_head` catches tail truncation. | `TestAuditAppendRejectsDisagreeingSuppliedHash`, `TestAuditVerifyPartitionRecomputesRatherThanTrusts`, `TestAuditVerifyPartitionHeadDetectsTailTruncation`, `TestCanonicalAuditPayloadMatchesDatabaseImplementation` |
| 11 | `audit.checkpoint` enforced only its own arithmetic (`record_count = last_sequence - first_sequence + 1`) and the *shape* of its hashes (`~ '^[0-9a-f]{64}$'`). Neither looked at `audit.record`, so a signed checkpoint could attest to sequences that were never written, or to hashes belonging to no record. | `audit.guard_checkpoint_binding` (BEFORE INSERT) requires `first_hash`/`last_hash` to equal the `record_hash` of the records at those sequences, requires every sequence in the sealed range to be present, and binds `expected_next_hash` to the successor record. `audit.verify_checkpoints` recomputes the claim. | `TestCheckpointMustBeBoundToTheRecordsItAttestsTo`, `TestCheckpointCannotAttestToRecordsThatDoNotExist`, `TestCheckpointSealingBeforeTheHeadMustNameItsSuccessor`, `TestCheckpointSealingAtTheHeadMustNotNameASuccessor`, `TestCheckpointSealedRangeMayNotContainGaps`, `TestVerifyCheckpointsDetectEvidenceRewrittenAfterSigning` |
| 12 | `audit.signing_key` existed with a status machine, an algorithm and a rotation deadline, and **nothing referenced it**: `checkpoint.signing_key_id` is `TEXT NOT NULL` with no foreign key. Any string could be recorded as the sealing key, a COMPROMISED or RETIRED key could sign, an overdue key could keep signing, and the only signature check was `octet_length > 0` — so `decode('00','hex')`, which the fixtures in this very suite were using, was an acceptable signature. | `audit.guard_checkpoint_key_binding` (BEFORE INSERT) requires a registered key, a status legitimate *at signing time*, an unpassed rotation deadline, a signature of plausible size, and agreement with the registry. `audit.canonical_checkpoint_body` fixes the exact signed bytes. `audit.verify_checkpoint_key_binding` reports retroactive compromise. | `TestCheckpointMustNameARegisteredSigningKey`, `TestCompromisedOrRetiredKeyMayNotSignNewEvidence`, `TestOversdueSigningKeyMayNotSignNewEvidence`, `TestPlaceholderSignatureIsRejected`, `TestCompromisedKeyIsRetrospectivelyReported` |
| 13 | A position was an independently writable number. Nothing derived it from validated fills, so quantity and watermark could be set to anything and stay set — the precise drift mandatory invariant 5 forbids. | Migration `0016`: validated fills are immutable and uniquely sequenced per account/instrument/venue; two deferred commit-time controls require a position's quantity to equal the signed sum of validated fills and its watermark to equal the latest validated fill. | `TestPositionMustEqualTheValidatedFills`, `TestPositionPinnedToAnOldFillSequenceIsRejected`, `TestACommittedPositionGoingStaleOnALaterFillIsRejected`, `TestACorruptedCommittedPositionIsRejectedWithNoNewFill` (mutation tested, 5/5) |
| 14 | `config.revision` claimed immutability it did not have. `0001_foundation.sql` carries the comment "-- Immutability after activation: enforced by trigger below", but `config.revision` had **zero triggers** and the `config` schema had **zero functions**. An ACTIVE snapshot could be rewritten in place by anyone with write access, with no record that it had changed. Separately, `content_digest` was checked only for *format* and never against the document it claims to describe, so the release `signature` — defined as a signature over the digest — attested to nothing: sign an honest digest, then replace the document, and the signature still verified. | Migration `0017`: digest bound to document by CHECK; content columns immutable from INSERT at every status; closed lifecycle with database-owned timestamps; `DELETE` refused except for DRAFT; one ACTIVE revision per environment; recursive no-embedded-secrets walk; promotion provenance with dual control into live. | `TestAnActiveConfigRevisionCannotBeEditedInPlace`, `TestAConfigDigestThatDisagreesWithItsDocumentIsRejected`, `TestAPromotionIntoLiveWithoutASecondApproverIsRejected`, `TestAConfigWithAnEmbeddedSecretIsRejected` and 20 more (mutation tested, 8/8) |
| 15 | `reconciliation.case` documented that it blocks and did not. `0005` says twice — in the table header and on the `blocked_scope` column — that "a MATERIAL unresolved break BLOCKS affected risk-increasing scope". The reconciliation schema had **zero triggers and zero functions**. `blocked_scope` is `JSONB NOT NULL`: the system records which scope a break covers and then never reads it. | Migration `0018`: the block is *derived* from `reconciliation.case` at the moment risk-increasing activity is attempted, inside `ops.assert_risk_increase_permitted`, so it cannot desynchronise from the case causing it and needs no separate clearing. Risk-reducing activity always passes. | `TestAnOpenMaterialBreakBlocksRiskIncreasingOrder`, `TestAResolvedMaterialBreakNoLongerBlocks`, `TestABreakOnOneAccountDoesNotBlockAnother`, `TestAnUninterpretableScopeBlocksEverything` and 13 more (mutation tested, 7/7) |
| 16 | **The entire `market` schema was storage-only.** `0002_market_data.sql` declares four safety controls in comments and implements none. At the time it was audited the schema had **zero triggers, on every table**. Instrument `order_types`, `supports_shorting`, `trading_status`, `min_quantity`, `price_increment`, `min_notional` were read by nothing; `market.venue_symbol` could be rewritten in place after taking effect; `feed_health` and `freshness_policy` were referenced by no other file in the repository. | Migration `0019` (partial — 16c deliberately left open, see below): capabilities and increments enforced at order creation, tradability enforced at the risk boundary, effective venue symbol mappings made append-only, sequence watermark made monotonic, and the tautological CHECK dropped. | `TestAnOrderTypeTheInstrumentDoesNotSupportIsRefused`, `TestAnEffectiveVenueSymbolCannotBeRepointedAtAnotherInstrument`, `TestAnInstrumentThatStoppedTradingIsRefusedAtRiskApproval` and 22 more (mutation tested, 8/8) |
| 17 | `feed_unknown_not_healthy` was a **tautology** that shipped in `0002` and survived sixteen migrations. `CHECK (health_state <> 'UNKNOWN' OR health_state = 'UNKNOWN')` is `A OR NOT A` — true for every value, rejecting nothing — sitting directly beneath a comment claiming "An unverifiable feed is a deny condition for risk-increasing activity, not an implicit pass." It was found by independent review, not by the original audit, and it is the clearest argument in this document that a green migration run proves syntax and not enforcement. | Dropped in `0019` rather than rewritten. A `feed_health` row is not an order; declaring a feed confirmed at write time would assert something the platform has not seen. The control moved to where an order is evaluated, as `market.instrument_is_tradable`, where "UNKNOWN denies" is tested against a real order. | `TestAnInstrumentWithUnknownTradingStatusIsRefusedAtRiskApproval`, `TestEveryNonOpenTradingStatusDenies` (mutation E, 8/8) |
| 18 | `sequence_no_regression` did not enforce non-regression. `CHECK (observed_sequence IS NULL OR expected_sequence IS NULL OR observed_sequence >= 0)` is a non-negativity test; a sequence watermark is free to move backwards through it. A stale or duplicated venue report could overwrite a newer watermark and feed the gap counter from a value that has gone backwards, so the feed reports healthy on a stream replaying old data. Found by independent review (M1); the constraint's *name* asserted a property it did not have. | Split in `0019` into `feed_observed_sequence_non_negative` (the real row constraint, honestly named) and `market.guard_feed_sequence_monotonic` (the real transition constraint, which is where a non-regression property has to live). | `TestASequenceWatermarkCannotMoveBackwards`, `TestTheFirstWatermarkCanBeSetFromNull` (mutation H, 8/8) |
| 19 | The nanosecond columns were unbound. `occurred_at_ns` / `recorded_at_ns` sit beside their `TIMESTAMPTZ` columns with no constraint between them, so a record could carry `occurred_at` of `2026-09-30T03:00:00Z` beside an `occurred_at_ns` describing `2020-01-01T00:00:00Z` and every constraint would accept it. Worse, a consistency check **had** been written and then removed, on the reasoning at `0009:216` that it would produce false positives. That reasoning diagnosed a real caller defect (two independent clock samples) and drew the wrong conclusion from it, leaving the repository with two unreconcilable representations of when a record happened and no test able to tell. | Migration `0021` binds the pair to `common.ns_bound_to_timestamptz`. Because TIMESTAMPTZ is microsecond-resolution, the relationship is stated as **rounding to the nearest microsecond**, not equality — an equality that could never hold is presumably why the original check "failed" and was dropped. `common.ns_to_timestamptz` is the single definition used by the constraint and by consumers. The removed-check note at `0009` is retained and corrected rather than deleted, since deleting the record of a reversed decision would hide the reversal. | `TestNsToTimestamptzRoundsToTheNearestMicrosecond`, `TestARecordWhoseTimestampAndNanosecondsDisagreeIsRefused`, `TestASubMicrosecondNanosecondValueIsAcceptedWhenTheTimestampIsItsRounding` (mutations D/E/F) |
| 20 | A guard function that nothing called. `common.assert_canonical_entropy` was fully written and reviewed but never invoked — a control present in the source and absent from the behaviour, the same shape as defect 15 (a comment) and the idle digest guard in `0017`. It contributed the appearance of a check while enforcing nothing. | Migration `0020` invokes the guard from inside the canonical encoder, so the entropy check is on the execution path rather than beside it. | `TestTheGoAndSQLCanonicalEncodersAgree` (mutation C) |

### Defect 13 in detail: three further defects found *inside* the fix

Defect 13's own control was wrong twice before it was right, and both errors are
worth recording because each passed a green suite.

**13a — a deferred trigger checks the tuple it was queued with, not the state
being committed.** The first version read `NEW`. A deferred constraint trigger
fires with the `NEW` row captured when it was *queued*, so a position
`INSERT`ed and then `UPDATE`d in one transaction was checked against its
pre-`UPDATE` quantity and refused — even though the state being committed was
correct. Since insert-then-update is the normal shape of building a position
from several fills, this control would have trained operators to work around it.
A control that refuses correct state is worse than no control. Fixed by
re-reading the current row; mutation C pins it.

**13b — checking quantity against the watermark the position *claims* is not
reconciliation.** The fill-side control recomputed the expected quantity through
`derived_from_fill_sequence`. A position pinned at watermark 1 therefore
reconciled to its own stale watermark and passed, while silently ignoring every
validated fill since. Because the position-side trigger cannot fire when no
position row is written, a position committed in transaction N went stale in
transaction N+1 and nothing objected. This is the normal operating pattern, not
an edge case. Fixed by requiring the watermark to equal the *latest* validated
fill on both paths; mutation E pins it.

**13c — a test helper must not corrupt the thing it is cleaning.** Adding
cross-transaction tests required a committed-setup helper, and its first version
truncated `audit.record` after each test. `audit.partition_month` stores each
chain's head (last sequence, last hash), and truncating the rows while leaving
that bookkeeping intact produces a chain beginning at a record that no longer
exists — so `TestAuditServiceAppendProducesAVerifyingChain` passed on the first
run after a reset and failed on every run after that, purely depending on
whether the helper had already run. The audit design was working correctly; the
harness was the defect. The helper now truncates only what those tests create,
and the suite is verified repeatable across three consecutive runs with no reset.

Defect 13a and 13b are the reason this document treats a green suite as weak
evidence on its own. Both were green for the entire time they were wrong.

### Defect 14 in detail: an enforcement comment with nothing behind it

Defect 14 is the most embarrassing kind of control failure, because the schema
documented the control correctly and simply did not implement it. `0001` wrote
`-- Immutability after activation: enforced by trigger below` and then defined
no trigger. The reader checking the table would have been told the guarantee
existed. This is the same defect class as 0008's unenforced state-transition
rule table, and it survived 0008's fix because it sits in a different migration
and a different schema.

The second half is subtler and more damaging than the missing immutability.
`content_digest` was constrained to a 64-character hex string and nothing else.
`signature` is defined as a signature over that digest. With the digest unbound
to the document, the intended attack needs no forgery at all:

1. Author a document, compute its digest, get it genuinely signed and approved.
2. `UPDATE` the document to something else. The digest column is untouched, so
   it still matches the signature. The signature still verifies. The approval
   still stands.
3. The configuration now in force is not the configuration anyone approved, and
   every signature check in the system passes.

That is why `0017` binds the digest with a CHECK rather than treating the digest
as trusted input.

**Defect 14a — the false green.** The secrets control shipped broken twice, and
both times the tests passed. `assert_no_embedded_secrets` referenced
`jsonb_each` columns that do not exist, and every negative test for it went green
against that unrelated error. After fixing that, the array branch still used
`jsonb_each`, which only accepts objects, and the nested-array test went green
against `cannot call jsonb_each on a non-object`. In both cases the control was
absent and the suite reported confidence it had not earned.

The fix is `dbtest.ExpectRejectedBecause`, which asserts not only that a write
was refused but that it was refused *for the stated reason*. It is now used by
every negative test in `config_boundary_test.go`, and it is what turned both of
those failures into visible errors instead of silent passes.

**Defect 14b — the mutation harness lied twice.** Two of the eight mutations
reported "not load-bearing" when the mutation had never applied: one produced
invalid SQL through PowerShell quoting, and one *renamed* a trigger, which does
not disable it — the new name still fires. Both looked exactly like a real
finding. A mutation harness that reports false negatives is worse than no
harness, because it converts "I have not verified this" into "this is
unnecessary". Both were fixed and both mutations are now caught. The 0016 harness
had the same `-replace` count-parameter bug earlier and was corrected the same
way.

Defect 14a and 14b together are the strongest argument in this document for
mutation testing: the two controls most likely to have been believed were the two
that the test harness was actively concealing.

### Defect 15 in detail: a safety control that was only a comment

Defect 15 is the second occurrence of defect 14's pattern, in a different schema
and a different migration, and it is the more serious of the two because the
control it left unenforced is safety-relevant rather than administrative.

`0005_ledger_portfolio_reconciliation.sql` describes the control twice. The table
header says:

> with explicit severity; a MATERIAL unresolved break BLOCKS affected
> risk-increasing scope

and the `blocked_scope` column says:

> Affected scope. A MATERIAL case blocks risk-increasing activity within it.

The reconciliation schema had no triggers and no functions. `blocked_scope` is
`JSONB NOT NULL` — the system records which scope a break covers, and nothing
ever reads it.

The schema is otherwise careful about a break: an SLA is mandatory, resolution
metadata is all-or-nothing, a reopened case must state why, a difference cannot
be negative. All of that governs how a break is *recorded*. None of it governed
what a break *did*, which is the part that matters.

The concrete failure: a venue reports a fill the platform never received, or a
balance that does not reconcile, by an amount large enough to stop trading. The
engine correctly opens a MATERIAL case. The case sits in the table. The OMS
carries on raising new exposure on an account whose true state is unknown. G11
requires that no material break exist before live activation, and a system that
only *records* breaks cannot establish that condition.

**Why the block is derived rather than duplicated.** `0018` computes the block
from `reconciliation.case` at the moment activity is attempted, rather than
maintaining a second copy — no auto-raised halt row to keep in step. There is
therefore nothing to desynchronise, and resolving the case clears the block with
no separate administrative act. `01_SYSTEM_ARCHITECTURE.md` §3 makes
reconciliation authoritative for venue discrepancies, so the case table is the
right thing to read rather than a projection of it.

**The scope shape is a local decision, and the failure direction is deliberate.**
`blocked_scope` is JSONB and no blueprint document specifies its shape, so
`0018` defines a minimal one and says so in the migration header. The
interpretation is one-sided: a scope that cannot be confidently read — null,
non-object, unrecognised key, wrong value type — is treated as covering
*everything*. Under-blocking creates exposure the operator believes is not there
and the cost of that failure is unmeasurable; over-blocking a malformed scope is
an operational problem somebody can see and fix. The opposite trade is defensible
in isolation and was not chosen here. Mutation F reverses the asymmetry, and
`TestAnUninterpretableScopeBlocksEverything` is what catches it.

**Defect 15a — a control masked by the caller above it.** The risk-reducing
exemption inside `reconciliation.assert_break_permitted` was unreachable through
the order path, because `ops.assert_risk_increase_permitted` returns early for
risk-reducing activity before it calls the reconciliation gate. Mutation B
removed the guard entirely and every test stayed green — including the one whose
name claims to cover risk-reducing activity. This is the same shape as defect
13a/13b and 0016's mutation A: a control silently carried by another control.
Fixed by testing the gate's own contract directly, since it is public and
documented as reusable.

**One limitation is stated rather than fixed.** The block reads committed case
rows, so a case opened in the *same* transaction as the order cannot block that
order. Reconciliation and OMS are separate bounded contexts and cross-domain
writes go through domain services, so this is not an expected path — but it is a
real gap in a safety control and is recorded here rather than papered over.

Defect 9 is the one that would have been discovered in production, at the worst
possible moment. An audit system that stops recording is worse than one that is
down, because the absence of evidence is indistinguishable from the absence of
events.

Defect 10 is the one that invalidates evidence rather than losing it. Every other
defect in this table produces a wrong value; this one produced a *right-looking*
value that no later reader could trust, which is the failure mode an audit trail
exists to prevent.

### Defect 10 in detail: the canonical hash is now a cross-language contract

`22_AUDIT_INTEGRITY_AND_EVIDENCE.md` §3 requires
`record_hash = SHA-256(canonical_json(record_without_record_hash_and_signature))`
and states that the canonical serialisation "is fixed and tested". Two
implementations of that serialisation now exist and are required to agree
byte-for-byte:

| Implementation | Location | Role |
|---|---|---|
| SQL | `audit.canonical_payload`, `db/migrations/0009_audit_canonical_hash.sql` | **Authority.** Computes the stored `record_hash`. |
| Go | `contracts.CanonicalAuditPayload`, `contracts/audit.go` | Independent cross-check value supplied by the audit service on every append. |

The canonical form is a flat JSON object with a fixed member order and no
insignificant whitespace. It is flat on purpose: every member is a JSON string, a
JSON integer, or `null`, so no member's encoding can depend on the serialiser's
key-ordering rules. The free-form `details` object is not embedded; its SHA-256
digest is embedded instead, which covers the content just as well while keeping
the form trivially reproducible in any language.

The cross-check is enforced rather than documentary. `append_record` accepts a
caller-supplied hash so that the Go implementation can be checked on every
append, but it may not *differ*:

```
ERROR:  audit record hash mismatch for aud_...: caller supplied aaaaaaaa...
        but the canonical payload hashes to 40f2dffd.... The database is the
        authority for record_hash.  (SQLSTATE 23514)
```

**Evidence that the conformance test can actually fail.** A test that agrees with
a broken implementation is not evidence. The member order of
`audit.canonical_payload` was deliberately perturbed (exchanging `action` and
`target_type`) and the suite re-run against a freshly reset database:

```
--- FAIL: TestCanonicalAuditPayloadMatchesDatabaseImplementation/every_optional_member_present
    audit_canonical_test.go:181: canonical serialisation differs between Go and SQL
--- FAIL: TestCanonicalAuditPayloadMatchesDatabaseImplementation/every_optional_member_NULL
--- FAIL: .../market_scope_present,_reason_with_markup_characters
```

The perturbation was then reverted and the full suite returned to green. The test
detects divergence, so a green result is meaningful.

**Evidence that the verifier recomputes.** `TestAuditVerifyPartitionRecomputesRatherThanTrusts`
writes an intact three-record chain, requires every row to verify, then alters
record 2's content with `session_replication_role = 'replica'` (which disables
the immutability trigger) and requires exactly one row to fail. Leaving a forged
record in place is not a realistic attack, so the control is otherwise untestable;
this simulates the only realistic one — a privileged operator with direct table
access, defeating the application-level controls.

### What defect 12 does and does not establish

0012 binds a checkpoint to a **registered, legitimate key**. It does not make
the signature bytes a valid signature, and the two are different claims:

| Established by the database | Not established |
|---|---|
| The key named exists in the registry | That the key produced these bytes |
| Its status was `ACTIVE`/`RETIRING` **at sealing time** | That the signature verifies against the public key |
| Its rotation deadline had not passed | That the key has not since been compromised |
| The signature is of plausible size for the algorithm | That the signed body was the one the signer used |

Status is checked *at signing time*, not against the registry's current value.
A key that was ACTIVE when it sealed a batch remains a valid signer for that
batch after rotation; requiring the current status would invalidate history on
every rotation, which is precisely what "overlapping verification keys during
planned rotation" exists to prevent. Compromise discovered later is a
**retroactive alarm** surfaced by `audit.verify_checkpoint_key_binding` rather
than a refusal — those batches were legitimately sealed, and hiding them would
be worse than surfacing them.

The 64-byte signature floor rejects the placeholder that this suite itself was
using (`decode('00','hex')`, one null byte) and nothing more. It is named
`audit.minimum_signature_bytes` and its comment says so, because a size check
that is easy to mistake for verification is worse than no check at all.



`append_record` also cross-checked `p_occurred_at_ns` against `p_occurred_at`.
It was removed, and the reason is recorded here because removing a control needs
to be as visible as installing one.

The check is unsound, not merely strict. A caller that samples the clock for the
nanosecond value and then lets the statement fill the `TIMESTAMPTZ` from `now()`
samples the clock twice; the round trip between the two is the false positive.
Measured on this machine the gap was ~350 µs, so *every* legitimate record
tripped the check:

```
ERROR:  audit record ...: occurred_at_ns 1790697767611603500 does not agree
        with occurred_at 2026-09-29 16:02:47.601246+00
```

A control whose failure is indistinguishable from corruption trains operators to
expect false positives and to reach for a bypass, which is worse than no control.
Nothing is lost by omitting it: the hash is computed over the nanosecond values,
so a verifier recomputing from the stored record reaches the same hash whether or
not the two time columns agree. The columns are stored for queryability, not
because the hash depends on both.

## Corrections made while producing this evidence

Recorded because each of them was a defect in the *evidence and harness*, not in
the system under test, and an unrecorded correction would make this report
overstate its own reliability.

1. **The negative-test harness could pass for the wrong reason.**
   `ExpectRejected` originally executed the forbidden statement directly inside
   the fixture transaction. A rejected statement leaves PostgreSQL in the aborted
   state, so every later statement in the same test failed with `25P02` — a test
   with two negative assertions would "pass" the second one for the wrong reason.
   It now runs each forbidden statement inside its own `SAVEPOINT` and always
   rolls back to it. This was masking nothing in the current suite, but it would
   have masked a missing control in the next one.

2. **A test helper had a 32-value identifier space.**
   `CanonicalID` indexed the Crockford alphabet with `(i + seed) mod 32`, so it
   produced only 32 distinct identifiers per prefix regardless of seed, and
   collided as soon as a suite generated more than a handful of fixtures. It now
   expands 20 characters from a 64-bit FNV-1a hash of `(prefix, seed, run nonce)`.

3. **A cleanup routine tried to delete append-only rows.**
   `TestBalancedJournalCommits` initially deleted its fixtures after committing.
   `ledger_entry_append_only` correctly refused the `DELETE`. Rather than disabling
   the trigger for test convenience, the fixtures now carry per-run unique
   identities, sequences and digests and are left in place, and the suite documents
   that it expects a disposable, freshly migrated database.

4. **An assertion did not name the control it was testing.**
   `TestFillAccountingCannotBeAdvancedWithoutAFillEvent` originally also changed
   the order state, so the state-machine trigger fired first and the fill-accounting
   control was never exercised. The test now changes only the execution progress,
   and asserts the rejection message names the fill-event control.

5. **A test asserted the wrong refusal reason.**
   `TestRiskReducingOrderMayExistWithoutDecisionBeforeSubmission` asserted a
   trigger message but was actually being refused by the pre-existing
   `order_risk_decision_present` CHECK constraint, which is a stronger control
   earlier in the pipeline. The test now asserts on whichever control refuses the
   write and records that both exist.

6. **A published contract declared an identifier prefix that does not exist.**
   `contracts/schema/common.schema.json` listed `cqt` in the `entityType` enum. It
   has no counterpart in Go, no table, no column prefix and no mention in the
   blueprint: 30 values in the schema against 29 in the package. A consumer
   generating code from the schema would have minted `cqt_` identifiers that
   `ParseID` rejects.

   It survived because `TestSchemaFileDeclaresSameVocabulariesAsGo` compared
   `errorCode`, `environment`, `marketClass` and `schemaVersion` — and not
   `entityType`. The test existed and passed while examining the wrong set, which
   is worse than no test: it was evidence for something it never checked.

   Both halves are fixed. The stray value is removed, and `entityType` is now
   compared through a new `contracts.EntityTypes()` enumerator. The check was
   confirmed to fail before the schema was corrected:

   ```
   --- FAIL: TestSchemaFileDeclaresSameVocabulariesAsGo
       fixtures_test.go:446: schema "entityType" has 30 members, Go has 29
   ```

7. **Three defects in the controls written for defects 10 and 11, all found by
   the tests written alongside them.** These are recorded separately because they
   were caught during this work rather than before it, and each one is a case of
   a test that appeared to pass while proving less than its name claimed.

   a. **A control that never fired.** `guard_checkpoint_binding` was meant to
      reject a checkpoint sealing before the head that names no successor. The
      first version tested `v_next_actual IS NULL` and then compared
      `NEW.expected_next_hash <> v_next_actual`. In SQL, `NULL <> 'abc'`
      evaluates to NULL, not TRUE, so the `IF` did not fire and the checkpoint
      was **accepted**. The test caught it:

      ```
      --- FAIL: TestCheckpointSealingBeforeTheHeadMustNameItsSuccessor
          audit_checkpoint_test.go:110: database ACCEPTED a write that must be
          structurally rejected
      ```

      This is the worst shape a control can take: present, documented, and
      inert. The fix is an explicit `IF NEW.expected_next_hash IS NULL` before
      any comparison.

   b. **A verifier that was tautological in the case it exists for.**
      `audit.verify_checkpoints` was first written to compare a checkpoint's
      hashes against the record's *stored* `record_hash` column. Rewriting a
      record's content does not change that column, so the verifier reported a
      checkpoint as valid after the evidence it attested to had been rewritten —
      precisely the scenario it was added to detect. It now recomputes each
      record's hash from its content via `audit.compute_record_hash`. The test
      caught it:

      ```
      --- FAIL: TestVerifyCheckpointsDetectEvidenceRewrittenAfterSigning
          audit_checkpoint_test.go:254: rewriting a sealed record left 0
          failing checkpoint(s), want 1
      ```

   c. **A service that could not write its first record.** `services/audit`
      read the partition head before appending, but `append_record` is what
      calls `ensure_partition`. On a month with no prior evidence there is no
      `partition_month` row, so the very first record of a partition failed with
      `no rows in result set`. The service now calls `audit.ensure_partition`
      first. It also no longer accepts a `PartitionKey` from the caller: the
      partition is the month of `RecordedAt`, and deriving it removes the
      possibility of a key inconsistent with the record's own timestamp.

## Corrections to earlier claims

An earlier working summary asserted that the reduce-only constraint *allowed*
`RISK_APPROVED` without a decision. That was wrong. The constraint
`order_risk_decision_present` does refuse it, because a `REDUCE_ONLY` order is
only exempt from the decision requirement in `CREATED` and `RISK_PENDING`. The
defect was in the *test*, which expected that write to succeed. The correction
is recorded here rather than silently dropped, because the earlier claim would
have led to removing a control that is in fact correct.

## Harness caveat: the skip path

The `dbtest` suite **skips every test** when `AITC_TEST_DATABASE_URL` is unset,
so a green `go test ./...` in an environment without a database means "no
database tests ran", not "the database is correct". `TestMain` prints an explicit
warning in that case. Any gate that relies on `dbtest` must confirm the
environment variable was set, and must record the test count (59) rather than
only the exit status, because a fully skipped suite also exits 0.

## Known gaps

This evidence does not cover, and does not claim:

- **Repositories, the configuration service, and the audit hash/signing service.**
  These still do not exist. Note the change from the previous wording of this
  bullet, which read "Domain services ... (G1 not yet implemented; only the
  schema and the canonical contracts exist)". The domain layer is now
  implemented — `domain/oms`, `domain/strategy`, `domain/halt` and
  `domain/risk` — so that sentence was over-broad. What remains absent is the
  *service* layer: nothing wires the domain logic to persistence.
- **OMS orchestration, ledger posting, and reconciliation execution.** The
  domain invariants they would enforce are implemented and tested; the services
  that exercise them are G3 scope and are not started. The schema permits only
  what the blueprint requires, but no service has yet exercised the permitted
  paths in anger.
- **The risk gate has never approved a real order.** It denies by default with
  no limits configured (doc 17 §3 forbids the platform supplying numeric
  limits), so the permissive path is covered only by unit tests.
- **Doc 17 §3's persisted limit-document schema** is still unimplemented. The
  `risk.Limit` type now defines the six required attributes and validates them,
  which is the shape the persisted document needs, but no owner has supplied the
  document schema itself.
- API, market-data and venue adapters, the web application, worker processes.
- Infrastructure: a Terraform tree, observability configuration, ten runbooks and a
  1 217-line live-activation guard exist, and the guard parses. None of it is
  verified: there is no `terraform` binary on PATH, no cloud credentials,
  `infra/tests/` contains zero files, and the guard has never been executed. It
  is unverified code, not working infrastructure, and no gate may claim otherwise.
- The audit hash computation itself. **Closed by `0009_audit_canonical_hash`**;
  see "Defect 10 in detail" above. `services/audit` now supplies the independent
  cross-check value on every append and the database rejects disagreement, so
  the control is proven against two implementations rather than one.
- `audit.checkpoint` hash binding. **Closed by
  `0010_checkpoint_hash_binding`**; see defect 11 in the table above. The
  signature itself is still unverified — see the separate entry below.
- The Go cross-check is **narrower than it may appear**, and this is a deliberate
  narrowing, not an oversight. `services/audit` supplies an independently computed
  `record_hash` on every append and the database rejects any disagreement — but
  the `details_digest` member is obtained by calling `audit.details_digest` in
  the database rather than by reimplementing PostgreSQL's `jsonb` text rendering
  in Go. The cross-check therefore covers the canonical *form* (member order,
  encoding, NULL handling, the outer twenty members) and takes the one
  DB-owned member from the database. Reimplementing `jsonb` rendering in Go would
  have been a second definition that is correct only until PostgreSQL changes a
  detail, and its divergence would surface as an unexplained hash mismatch on
  every append.
- `audit.checkpoint.signature` is **not cryptographically verified**, and nothing
  in this repository claims it is. `audit.signing_key` deliberately stores only
  a provider reference — key material never enters this database — so no SQL
  function can verify an ECDSA or Ed25519 signature, and writing one that
  appeared to would be theatre. What 0012 installs is everything the database
  *can* decide: the exact signed bytes, that the named key exists, that it was
  legitimate to sign with at that moment, and that the signature is of plausible
  size. `22_AUDIT_INTEGRITY_AND_EVIDENCE.md` §6 lists "signature verification"
  among the required acceptance tests; that item is **outstanding** and requires
  the KMS/HSM. The pending function is named `audit.verify_checkpoint_signature_crypto`
  in the 0012 comment so its absence is greppable rather than merely missing.
- Halt enforcement is scoped to **order state transitions only**. It is not yet
  wired to the OMS submission path, order cancellation, or the risk-decision
  service, so a halt blocks an order becoming possible exposure but does not yet
  interrupt a service that has already produced a submission intent. That
  remaining wiring is a G1 gap, not a completed control.
- Risk-reducing permission during a halt is proven only for the OMS order state
  machine. `TestHaltPermitsRiskReducingOrder` is the exact mirror of
  `TestActiveHaltBlocksRiskIncreasingOrder` — same instrument, account, venue,
  halt and attached risk decision, with `order_type` the only difference — so
  the halt is provably the only possible cause of the divergence in both
  directions. It does not prove that any other write path permits flattening
  under a halt; no other path reads `ops.effective_halt` yet.
- The audit record itself has no signature column. §3 refers to
  "record_without_record_hash_and_signature"; signature lives on
  `audit.checkpoint`, which closes a batch rather than a record. Whether the
  blueprint intends a per-record signature is **unresolved** and is a question for
  the architecture owner, not something to be settled by an implementation
  choice.
- The Go audit service exists but is the only service. Domain services,
  repositories, configuration service, risk, OMS, ledger posting, reconciliation
  execution, API, adapters, the web application and the workers are all
  unimplemented.
- Independent review. Everything here is self-attested.

### 0013 — halt enforcement and audit-chain SEV-1

Before 0013 `ops.halt` was fully specified, trigger-protected for its own
lifecycle, and **read by nothing**. A halt did not halt. `chain_state` on
`audit.partition_month` was never written by any function and was likewise
read by nothing. Two tests existed for halts; both asserted constraints on the
halt table itself, and both would have passed with the enforcement deleted
wholesale.

That matters more than an ordinary missing feature. `09_TESTING_AND_RELEASE_EVIDENCE.md`
lists "Halt state blocks prohibited actions" as one of seven **mandatory**
financial invariants, and `11_EXECUTION_GATES.md` prohibits waivers for halt
controls. The suite reported green on a control that did not exist.

`0013_halt_enforcement_and_chain_sev1.sql` installs:

| Object | Purpose |
|---|---|
| `ops.halt_precedence_rank(ops.halt_level)` | Ranks `SYSTEM_HALT` > `INSTRUMENT_HALT` > `VENUE_HALT` > `ACCOUNT_HALT` |
| `ops.effective_halt(env, account, instrument, market, venue, strategy)` | Highest-ranked `ACTIVE` halt matching a scope |
| `ops.assert_risk_increase_permitted(...)` | The single decision point; raises `23514` on refusal |
| `ops.guard_halt_clear()` | Dual control on clearing, plus monotonicity in severity |
| `audit.mark_chain_broken(partition, reason, actor)` | Sets `chain_state='BROKEN'` and raises an emergency `SYSTEM_HALT` |
| `common.new_canonical_id(prefix)` | Mints canonical ids; nothing in the schema could before |

`assert_risk_increase_permitted` is the only place the policy decision lives, and
it evaluates the audit-chain state **before** any halt. That ordering is
deliberate: a broken evidence chain is not overridable by a halt record,
because a halt is a system-authored row and therefore cannot itself be trusted
once the chain carrying its justification has a gap. The reason text names
`22_AUDIT_INTEGRITY_AND_EVIDENCE.md` §3 so the provenance of that rule is
greppable.

The OMS trigger calls it at the transition out of `CREATED`/`RISK_PENDING` —
the point where an order stops being an intention and becomes possible
exposure. `market_class` is looked up from `market.instrument` rather than read
from `oms.order`, which has no such column; passing `NULL` there would have
failed to match a `MARKET_HALT` and the halt would have silently not applied.

**Risk-reducing orders are deliberately not blocked.** `REDUCE_ONLY` and
`CLOSE_POSITION` advance normally under a halt, because a halt that also blocked
flattening would convert a contained incident into unbounded loss with no
operator remedy. `TestHaltPermitsRiskReducingOrder` is the deliberate mirror of
`TestActiveHaltBlocksRiskIncreasingOrder`: identical except for `order_type`,
with a risk decision attached in both, since a reduce-only order also cannot
reach a submission state without one. Omitting it there would have made the
test pass for the wrong reason.

**Mutation test.** Passing tests prove nothing if they would also pass with the
control removed, so this was checked rather than assumed. The halt gate body was
replaced with `PERFORM 1` and the suite re-run against a fresh migrate:

| Test | With enforcement | Enforcement removed |
|---|---|---|
| `TestActiveHaltBlocksRiskIncreasingOrder` | PASS | **FAIL** |
| `TestBrokenAuditChainBlocksRiskIncreasingActivity` | PASS | **FAIL** |
| `TestHaltPermitsRiskReducingOrder` | PASS | PASS (correct — asserts permission) |
| `TestClearedHaltNoLongerBlocks` | PASS | PASS (correct — over-blocking guard) |
| `TestHaltScopeDoesNotLeakToOtherAccounts` | PASS | PASS (correct — over-blocking guard) |

Exactly the two tests that assert **blocking** fail when blocking is removed, and
the three that assert **permission** correctly continue to pass. The three
permission tests are not evidence of enforcement and are not counted as such;
they exist to catch the opposite failure, an over-broad halt that freezes
unrelated accounts or a system that cannot be restarted.

**Known limits of 0013**, carried forward from the outstanding list above:
`common.new_canonical_id` derives 100 bits of entropy with a 256→32 byte
mapping rather than Go's bit layout. Identifiers are opaque, so alphabet,
length and entropy all match; the encodings differ by design and the comment
says so. The scope filter is a simple `IS NOT DISTINCT FROM` conjunction
rather than a coverage-aware temporal model, which is adequate for the current
single-active-halt-per-scope reality and is noted in the migration comment as
the place to extend. `0029` is deliberately left empty for halt policy content.

### 0014 — operational mode enforcement (RECOVERY_HOLD, kill switch, restricted modes)

The halt defect in 0013 turned out to be a pattern, so the same search was run
across the remaining control tables. `ops.system_state` had the same disease,
worse because the schema asserted the control in its own documentation.

`0007_outbox_halts_ops.sql` ended with:

> `COMMENT ON TABLE ops.system_state IS 'Per-environment operational mode.
> RECOVERY_HOLD and the kill switch each independently block risk-increasing
> activity and cannot be disabled without dual control. Recovery never
> automatically resumes risk-increasing operation.'`

Neither clause was implemented. `recovery_hold` and `kill_switch_engaged` were
**read by nothing in any migration**. The only constraints were row-shape
CHECKs requiring a reason and an actor to be present *while the flag was set* —
which says nothing about what the flag does.

Contradicted requirements:

- `10_OPERATIONS_AND_DISASTER_RECOVERY.md` — after recovery, "new
  risk-increasing commands, strategy deployment, credential rotation, and live
  activation remain blocked".
- The recovery decision table asks "Can a recovered region automatically resume
  risk-increasing behavior?" and answers **No**. Nothing enforced the answer: a
  recovered region resumed exactly as an unrecovered one would.
- Objective 4: "No restored environment may leave `RECOVERY_HOLD` without
  independent verification."
- `11_EXECUTION_GATES.md` prohibits waivers for recovery objectives, halt
  controls and financial invariants.

**A second, opposite defect.** `restricted_mode_dual_control` read:

```sql
CHECK ((mode IN ('NORMAL','READ_ONLY')) OR mode_approved_by IS NOT NULL)
```

It required a second approver to **enter** a restricted mode and said nothing
about **leaving** one. That is backwards, and dangerous: during the failover
that caused a recovery, the database could refuse to enter `RECOVERY_HOLD`
because a second operator was not yet awake. `06_SECURITY_AND_ACCESS_CONTROL.md`
line 23 states the principle for the analogous control — "Emergency halt is
always available to authorized halt operators and does not require a second
approver; re-enable does."

Even the dual control it did provide was nominal: `mode_approved_by` only had to
be non-null, and nothing stopped it being the same identity as `mode_changed_by`.
`ops.halt` has a `cleared_approved_by`; `identity.role_binding` has a test
asserting a distinct approver. `ops.system_state` was the outlier in accepting a
self-approval.

| Object | Purpose |
|---|---|
| `ops.guard_system_state_transition()` | Asymmetric: engage needs one accountable actor, release needs dual control + distinct identities + independent verification |
| `ops.assert_operational_mode_permitted(...)` | The single gate; **fails closed** when an environment has no recorded mode |
| `order_operational_mode_guard` trigger | Applies the gate at the OMS risk boundary |
| `recovery_release_recorded`, `kill_switch_release_recorded` | Row-shape invariants so release evidence cannot be half-written |
| `recovery_verified_by` | Verifier distinct from *both* releaser and approver |

Deliberate design decisions, each recorded in the migration:

- **Fail closed on an unrecorded environment.** An unrecorded operational mode is
  not an unrestricted one. This only works because all six environments are
  seeded at `NORMAL` in the migration;
  `TestEveryEnvironmentHasARecordedOperationalMode` asserts the seeding rather
  than trusting it, and `TestRiskIncreasingActivityFailsClosedForAnUnrecordedEnvironment`
  deletes a row to prove the branch is real.
- **Risk-reducing activity is never blocked**, for the same reason halts do not
  block it: a mode that also prevents an operator from flattening converts a
  contained incident into unbounded loss. The instrument for freezing
  risk-reducing activity is an `ACTIVE` halt, which is scoped and reasoned. This
  is an *interpretation* of the blueprint, which names what `RECOVERY_HOLD`
  blocks and does not list risk-reducing orders; it is flagged in the migration
  comment for the architecture owner.
- **A separate trigger, not another section in `oms.guard_order_update`.** The
  halt gate and the mode gate answer different questions and are owned
  differently; one combined guard would let a failure in one mask the other.
- **The kill switch needs dual control to release but not a third-party
  verifier.** Unlike `RECOVERY_HOLD`, it is engaged during a live incident, and
  requiring a third person to stand it down would make it a one-way door.

**A misnamed test, corrected.** `TestRecoveryHoldCannotBeLeftWithoutAuthorisation`
claimed to cover *release* but only ever asserted that malformed rows are
rejected — it tested creation, and only the row shape. It is now
`TestRecoveryHoldRowMustBeWellFormed`, and genuine release coverage lives in
`operational_mode_test.go`. The test passing was never evidence that leaving a
recovery hold was controlled.

**Mutation test.** As with 0013, passing tests prove nothing if they would also
pass with the control deleted. The gate body was short-circuited with
`RETURN NEW` and the suite re-run against a fresh migrate:

| Test | With enforcement | Enforcement removed |
|---|---|---|
| `TestRiskIncreasingActivityFailsClosedForAnUnrecordedEnvironment` | PASS | **FAIL** |
| `TestRecoveryHoldBlocksRiskIncreasingActivity` | PASS | **FAIL** |
| `TestKillSwitchBlocksRiskIncreasingActivity` | PASS | **FAIL** |
| `TestRestrictedOperationalModesBlockRiskIncreasingActivity` (3 subtests) | PASS | **FAIL** |
| `TestOperationalModePermitsRiskReducingOrder` | PASS | PASS (correct — asserts permission) |
| `TestNormalModeDoesNotBlock` | PASS | PASS (correct — asserts permission) |
| Release/dual-control tests | PASS | PASS (correct — separate trigger) |

Exactly the five blocking cases fail; the permission cases correctly survive,
and the release controls are unaffected because they live in their own trigger.
The permission tests are not counted as evidence of enforcement.

### 0015 — workload identity and environment ceiling (mandatory invariant 8)

With 0013 and 0014, invariants 7 and the recovery/kill-switch requirements had
structural enforcement. Invariant 8 had **none at all**. It is now the last of
the eight mandatory financial invariants with a storage control.

`09_TESTING_AND_RELEASE_EVIDENCE.md`: "Lower-environment credentials cannot
access live resources." `11_EXECUTION_GATES.md` prohibits waivers for "live
credential isolation" specifically by name. There was no workload identity model,
no credential-to-environment binding, and no enforcement. `ops.workload_identity`
did not exist. `identity.role_binding` and `identity.session` carry an
`environment` column, but they describe what a *human* may do, not what a
*credential* can reach.

**An honest statement of scope.** `01_SYSTEM_ARCHITECTURE.md` section 4 requires
that each environment have "separate credentials, database, encryption keys,
deployment identity, network policy, and configuration namespace". This
repository has **one** database holding `live`, `paper`, `shadow`, `staging`,
`test` and `dev` rows distinguished by an `environment` column. That is not the
specified architecture and no in-database control makes it so. The primary
control for invariant 8 is architectural — separate databases, separate secret
stores, mTLS workload identity, network policy — and those are infrastructure
deliverables in `infra/`, not this migration.

What 0015 adds is **defence in depth for the shared-database reality that
exists today**, and it is worth having for one concrete reason: in a single
shared database, a paper service holding a valid credential with valid grants
could write live rows simply by naming `environment = 'live'`. That is a routine
mistake, not an attack, and it is exactly how a paper rehearsal becomes a live
trade.

| Object | Purpose |
|---|---|
| `common.environment_rank(env)` | Mirrors `contracts.Environment.Rank()`; parity asserted across the language boundary |
| `ops.trust_zone` | The four hard trust zones of `01_SYSTEM_ARCHITECTURE.md` |
| `ops.workload_identity` | Binds each credential to a zone and an environment **ceiling** |
| `ops.assert_principal_permitted(env, ctx)` | The single gate; fails closed |
| `*_principal_guard` triggers on `oms.order`, `ledger.entry`, `ops.outbox` | Applies the gate at write time |
| `edge_has_no_database_credential` | CHECK: no edge principal may exist at all |
| `research_has_no_live_identity` | CHECK: research workloads have no live identity |
| `live_ceiling_requires_control_or_recovery` | CHECK: only control/recovery may hold a live ceiling |

Two of the four trust zones are expressed as refusals, and both are properties of
the **data**, not of a code path: an edge principal cannot be registered, and a
research principal can neither hold a live ceiling nor write to any
authoritative financial table in any environment.

**A defect I introduced and caught, worth recording.** I first wrote the guard
against `session_user` only, on the stated reasoning that `current_user` "can be
changed by `SET ROLE` inside the session, so it is the wrong thing to authorise
against." That reasoning was backwards, and the test suite caught it: the
`SET ROLE` tests were all permitted. `session_user` is the role that
*authenticated* the connection and is **fixed** for the session's lifetime;
`current_user` is the one that changes under `SET ROLE`. Authorising on
`session_user` alone would have meant the guard was escapable by exactly the
mechanism it exists to constrain — a session with a live-capable credential
could `SET ROLE` to a lower or unregistered role and the guard would still see
the live-capable identity. The function now requires **both** to be registered,
active, out of the research zone and within their own ceiling, so the effective
authority is the more restrictive of the two.

**Deliberate exclusions, recorded so they are not mistaken for oversights:**

- `ops.idempotency_record` is not guarded because it has no `environment` column
  and does not need one. `contracts.IdempotencyScope` includes `Environment` and
  states "a command is never idempotent across environments", so the environment
  is already mixed into `scope_hash`, the table's uniqueness key. Adding a
  redundant column purely to compare would create a second source of truth for a
  fact the primary key already fixes.
- `ops.workload_identity` itself is not guarded. Guarding the credential registry
  with the credential check would make it impossible to register the first
  identity or revoke a compromised one — precisely the window an attacker needs.
  **A compromised but still-`ACTIVE` credential is therefore a residual risk**,
  and is carried in the outstanding list below.
- The bootstrap registers the migration-owner role explicitly rather than by
  blanket exemption, so the exemption is auditable. Running migrations as a
  different role fails loudly, naming an unregistered principal.

**Mutation test.** As with 0013 and 0014, the gate was short-circuited and the
suite re-run against a fresh migrate:

| Test | With enforcement | Enforcement removed |
|---|---|---|
| `TestLowerEnvironmentCredentialCannotReachLiveRows` | PASS | **FAIL** |
| `TestUnregisteredCredentialIsRefused` | PASS | **FAIL** |
| `TestRevokedCredentialIsRefusedEverywhere` | PASS | **FAIL** |
| `TestResearchZoneHasNoWritePathToFinancialTables` | PASS | **FAIL** |
| `TestLiveCredentialMayReachLowerEnvironments` | PASS | PASS (correct — asserts permission) |
| `TestEnvironmentRankMatchesGo` | PASS | PASS (independent) |
| CHECK-constraint tests (edge, research-live) | PASS | PASS (independent) |

`TestPrincipalGuardIsAttachedToAuthoritativeTables` asserts the triggers exist on
`oms.order`, `ledger.entry` and `ops.outbox` by querying `pg_trigger`. A guard
function that nothing calls is the exact defect class this work has been hunting,
so its attachment is asserted rather than assumed.

### 0017 — configuration boundaries (the last named G1 criterion)

Doc 11 §9 lists G1's criteria as "canonical IDs, timestamps, money/quantity
types, errors, envelopes, idempotency, **configuration boundaries**, migrations,
and domain tests". Configuration boundaries was the one criterion with no
enforcement behind it: `config.revision` had the right columns and no controls
(see Defect 14). Migration `0017` supplies them.

| Control | Kind | Defends against |
|---|---|---|
| `config.document_digest` | function | one definition of the digest, so the Go side calls the database rather than reimplementing `jsonb` text rendering |
| `content_digest_binds_document` | CHECK | a valid signature attesting to a document nobody approved |
| `config.guard_revision_mutation` | BEFORE UPDATE/DELETE | editing a revision in place, re-opening a revoked one, deleting release evidence, backdating a transition |
| `config_revision_single_active_idx` | UNIQUE partial index | two ACTIVE snapshots in one environment, i.e. a last-write-wins reader |
| `config.assert_no_embedded_secrets` | BEFORE INSERT | a credential committed into a configuration document |
| `config.guard_promotion_provenance` | deferred constraint trigger | an automated single-identity copy of configuration into live |

Three design choices are worth stating explicitly, because each could reasonably
have gone the other way.

**The promotion rule is structural, not procedural.** Doc 17 §1 says live
configuration cannot be copied automatically from lower environments. A revision
that declares a source must name a real source at a strictly lower rung of the
ladder, and a promotion *into live* must carry a second approver distinct from
its author. An automated pipeline has exactly one identity, so it cannot supply
a second approver and cannot commit. There is no flag to set and no environment
variable to flip. A revision authored directly in live is still permitted —
doc 17 §1 forbids automatic *copying*, not direct authorship, and forcing a
fabricated parent would push operators to invent provenance, which is worse than
having none.

**Content is immutable from INSERT, including for DRAFT.** Allowing a draft to
be edited means the artifact under review has no stable identity: the thing that
was reviewed would not be the thing that gets activated.

**The secrets matcher is segment-anchored, not a substring search.** A substring
search for `token` would reject `token_endpoint` and `token_ttl`. A control that
rejects valid configuration gets switched off within a day, so the matcher
requires the final path segment to be the secret-bearing word, and
`TestInnocentKeyNamesContainingSecretWordsAreAccepted` pins that it does not
mis-fire.

**Mutation testing: 8/8 caught**, each by a distinct test
(`db/mutate_0017.ps1`):

| Mutation | Behaviour removed | Caught by |
|---|---|---|
| A | digest no longer bound to document | `TestAConfigDigestThatDisagreesWithItsDocumentIsRejected` |
| B | content immutability | `TestAnActiveConfigRevisionCannotBeEditedInPlace` |
| C | closed lifecycle | `TestAnActivatedConfigRevisionCannotReturnToDraft` |
| D | `DELETE` refused only for DRAFT | `TestAnActivatedConfigRevisionCannotBeDeleted` |
| E | one ACTIVE per environment | `TestASecondActiveRevisionInOneEnvironmentIsRejected` |
| F | embedded secrets | `TestAConfigWithAnEmbeddedSecretIsRejected` |
| G | dual control into live | `TestAPromotionIntoLiveWithoutASecondApproverIsRejected` |
| H | monotonic promotion ladder | `TestAPromotionMustGoForwardOnTheLadder` |

### 0016 — positions are derived from validated fills (mandatory invariant 5)

This migration closes the gap recorded under "Invariant 5 is thin" below. It
makes a position a *derived* value rather than an independently entered one: the
database now refuses to commit a position whose quantity does not equal the
signed sum of validated fills, and refuses a position whose
`derived_from_fill_sequence` is anything other than the latest validated fill.

**Controls added**

| Control | Table | Catches |
|---|---|---|
| `oms.guard_fill_immutable` | `oms.fill` | a validated fill being edited or deleted after the fact |
| `portfolio.reconciled_position_quantity` | — | one definition of "derived", shared by both triggers and by tests |
| `portfolio.guard_position_reconciles_to_fills` | `portfolio.position` | a position written wrong, with no fill involved |
| `portfolio.guard_new_fill_reconciles_positions` | `oms.fill` | a new fill invalidating a position nobody updated |

Both reconciliation triggers are `DEFERRABLE INITIALLY DEFERRED` constraint
triggers, so a position may be built from several fills within one transaction
and is checked once, at the point the state is actually committed. Validated
fills are additionally unique per `(account, instrument, venue, fill_sequence)`,
which keeps the watermark unambiguous.

**Two controls, same checks, different triggers.** Both reconciliation triggers
perform the same two checks — quantity against validated fills, and watermark
against the *latest* validated fill. They are not redundant, because they fire
on different writes: the position-side trigger cannot observe a fill that lands
in a later transaction, and the fill-side trigger cannot observe a position
written wrong without a new fill. This is asserted by two tests that isolate
each firing path (`TestACorruptedCommittedPositionIsRejectedWithNoNewFill` and
`TestACommittedPositionGoingStaleOnALaterFillIsRejected`); see "Defect 13".

**Mutation testing.** `db/mutate_0016.ps1` disables one behaviour per run and
requires the suite to go red. All five are caught, each by a distinct test:

| Mutation | Behaviour removed | Caught by |
|---|---|---|
| A | fill-side control removed entirely | `TestACommittedPositionGoingStaleOnALaterFillIsRejected` |
| B | position-side control removed entirely | `TestACorruptedCommittedPositionIsRejectedWithNoNewFill` |
| C | deferred trigger reads the queued tuple instead of the current row | `TestACorruptedCommittedPositionIsRejectedWithNoNewFill` |
| D | empty-book watermark coalesce removed | `TestZeroPositionWithNoFillsIsAccepted` |
| E | stale watermark accepted | `TestPositionPinnedToAnOldFillSequenceIsRejected` |

```
mutations caught: 5/5  [C, E, A, D, B]
```

Mutation A is the one that mattered. Before these tests existed, the suite was
fully green with the fill-side control deleted — the position-side trigger was
quietly covering for it, and no single-transaction test could tell the
difference. A control that can be removed without any test noticing is not a
control.

### 0018 — an unresolved reconciliation break blocks risk-increasing activity

This migration closes defect 15. The reconciliation schema could record a
material break but had no mechanism to act on one, so a MATERIAL break recorded
in good faith did not stop trading.

**Controls added**

| Control | Catches |
|---|---|
| `reconciliation.scope_covers(scope, account, instrument, venue)` | one interpretation of `blocked_scope`, shared by the gate and by tests |
| `reconciliation.assert_break_permitted(environment, account, instrument, venue, is_risk_increasing, context)` | the block itself; a public gate usable by any bounded context, not only the OMS |
| a new step in `ops.assert_risk_increase_permitted` | risk-increasing OMS activity, at the same point halts and operational mode are already screened |

The gate sits inside the existing risk boundary rather than beside it. That
placement means the break is evaluated under the same transaction and the same
principal as the activity it is about to refuse, so there is no window in which
a break is recorded but an order escapes it.

**Risk-reducing activity always passes, and that is not a relaxation.** A
material break with an unexplained balance difference is a reason not to add
exposure. It is not a reason to be unable to reduce what is already there. A
control that blocks flattening converts a data quality problem into an open
position, and the operator has then disabled the control that was protecting
them. Risk-reducing is defined structurally — the OMS computes it from
`REDUCE_ONLY` and `CLOSE_POSITION` — rather than by asking the caller to
assert it.

**The block is derived, so there is nothing to clear.** No halt row, no flag, no
second copy of the case. Resolving the case is the whole clearing procedure.
`01_SYSTEM_ARCHITECTURE.md` §3 makes reconciliation authoritative for venue
discrepancies, so the case table is read directly rather than projected.

**Mutation testing: 7/7 caught**, each by a distinct test
(`db/mutate_0018.ps1`):

| Mutation | Behaviour removed | Caught by |
|---|---|---|
| A | the gate is never called (restores the pre-0018 world) | `TestAnOpenMaterialBreakBlocksRiskIncreasingOrder` |
| B | risk-reducing exemption | `TestTheBreakGateItselfExemptsRiskReducingActivity` |
| C | only MATERIAL blocks | `TestANonMaterialBreakDoesNotBlockTrading` |
| D | terminal cases still block | `TestAResolvedMaterialBreakNoLongerBlocks` |
| E | `blocked_scope` ignored | `TestABreakOnOneAccountDoesNotBlockAnother` |
| F | uninterpretable scope under-blocks | `TestAnUninterpretableScopeBlocksEverything` |
| G | environment not matched | `TestABreakInOneEnvironmentDoesNotBlockAnother` |

```
mutations caught: 7/7  [A, B, C, D, E, F, G]
```

Mutation A is the important one, because it reinstates exactly the defect:
`0005`'s comment with no enforcement behind it. It is caught by the same test
that the original audit would have needed, and it is caught by exactly one
test, which is a fair measure of how thin the original coverage was.

Mutation B is the second instance of a control carried by its caller — the
third overall, after 13a/13b and 0016's mutation A. The gate's guard is
unreachable through the order path, so it was tested directly instead. See
"Defect 15a".

Mutation F is the only mutation in this repository that tests a decision rather
than a mechanism. There is no blueprint text to say which way an uninterpretable
scope should fall, and the two directions have different kinds of cost; the
choice and its reasoning are recorded in the migration header and in "Defect 15"
above rather than left implicit in the SQL.

### 0019 — the market schema stops being storage-only

Defect 16, in full, is at `evidence/gates/G2/findings.md`. At the moment it was
audited:

```
SELECT count(*) FROM pg_trigger
 WHERE NOT tgisinternal AND tgrelid::regclass::text LIKE 'market.%';
-- 0
```

Not one trigger, on any table, in the whole `market` schema. `0002` declares
four safety controls in comments and implements none of them. This is the
largest single finding in the project, and it survived seventeen green migration
runs and a 231-test suite, because a migration that applies cleanly and a test
that passes are both silent about whether a control exists.

**Controls added**

| Control | Table / function | Closes |
|---|---|---|
| `oms_order_instrument_terms_guard` | `oms.order` (BEFORE INSERT) | 16a — capabilities and increments at order creation |
| `market.instrument_permits` | — | order type declared by the instrument; sell where shorting is unsupported |
| `market.increment_multiple` | — | quantity and price on the increment grid, exactly, in NUMERIC |
| `market.assert_order_terms_permitted` | — | the combined term gate |
| `ops.assert_risk_increase_permitted` step 4 | — | 16a (liveness) — tradability at risk approval, not at creation |
| `market.instrument_is_tradable` | — | 16b — the control the tautological CHECK only claimed |
| `venue_symbol_append_only` | `market.venue_symbol` | 16d — an in-force mapping cannot be repointed, renamed, re-dated or deleted |
| `feed_sequence_monotonic` | `market.feed_health` | 18 — a sequence watermark cannot move backwards |

**Terms are checked once, liveness is checked twice.** `oms.guard_order_update`
already makes order terms immutable, so a term validated at INSERT cannot become
invalid later and needs no re-checking. `trading_status` is different: an
instrument can be delisted between creation and risk approval, so tradability is
re-checked at the risk boundary where `0013`, `0014` and `0018` already screen.
`TestAnInstrumentThatStoppedTradingIsRefusedAtRiskApproval` creates the order
against an `OPEN` instrument and delists it before risk approval, so the check
cannot be satisfied by testing at creation alone.

**Three deliberate decisions worth stating.**

*Closing activity is not a short.* `market.instrument_permits` treats
`REDUCE_ONLY` and `CLOSE_POSITION` as not-shorting even where
`supports_shorting` is false. Selling to close an existing long is not opening
short exposure, and a control that blocked it would trap exposure on exactly the
instruments where closing matters most.

*An unverifiable min_notional denies.* An instrument that declares
`min_notional` and a market order with no price at submission: the floor cannot
be evaluated, so the order is refused rather than admitted on the assumption
that it will probably be large enough.

*`UNKNOWN` denies, and the deny set is closed.* Only `OPEN` is tradable, so a
state added to the enum later denies by default.
`TestEveryNonOpenTradingStatusDenies` pins all eight non-`OPEN` states
individually, so that claim is tested rather than asserted.

**The shared test fixture had to change, and that is the point.** 171 existing
tests failed against `0019` because `MustInsertInstrument` created an instrument
with `order_types = [LIMIT]` and the default `trading_status = 'UNKNOWN'` —
neither deliberate, because the columns were read by nothing and no test had a
reason to care. The fixture was corrected rather than the migration relaxed. A
shared fixture deliberately hobbled to dodge a control is precisely how a control
ends up unenforced in production while every test is green. Tests that need a
constrained instrument now build their own, so the constraint under test is
visible in the test.

**Mutation testing: 8/8 caught**, each by a distinct test
(`db/mutate_0019.ps1`):

| Mutation | Behaviour removed | Caught by |
|---|---|---|
| A | order type support | `TestAnOrderTypeTheInstrumentDoesNotSupportIsRefused` |
| B | shorting support | `TestAShortAgainstANonShortableInstrumentIsRefused` |
| C | increment grid | `TestAQuantityOffTheIncrementGridIsRefused` |
| D | `min_notional` | `TestAnOrderBelowMinNotionalIsRefused` |
| E | tradability at risk approval | `TestAnInstrumentThatStoppedTradingIsRefusedAtRiskApproval` |
| F | in-force mapping repointed | `TestAnEffectiveVenueSymbolCannotBeRepointedAtAnotherInstrument` |
| G | in-force mapping deleted | `TestAnEffectiveVenueSymbolCannotBeDeleted` |
| H | sequence watermark rewind | `TestASequenceWatermarkCannotMoveBackwards` |

Mutations H and defects 17 and 18 were found by independent review, not by this
audit. That is the most useful thing in this section: the audit method found
sixteen defects and then stopped finding them, and the review found three more
immediately.

**Defect 16c is deliberately still open.** `market.feed_health` and
`market.freshness_policy` are still read by no risk path. Wiring them requires
deciding whether a policy is selected by `market_class` alone, or also by
`venue_id`, or also by `instrument_id` — a question no blueprint document
answers, and one whose answers are not refinements of each other. The safe
default (deny when no policy matches) is available under all three, which makes
implementing it now tempting. It is not implemented now because doc 11:49
requires that "all account-, venue-, instrument- and strategy-specific risk
limits are configured and independently reviewed" before G11, and shipping a
guessed scope rule would create the appearance of a reviewed control. **G2 does
not pass.**

### Outstanding after 0019

- **Invariant 8 is only defence in depth.** The primary control is architectural
  and unimplemented: separate databases, secret stores, mTLS workload identity
  and network policy per environment. This migration does not close G11 or
  invariant 8 as the blueprint means it.
- **A compromised but still-`ACTIVE` credential is not contained.** Registration
  and revocation are operator actions against an audited schema, not
  self-enforcing.
- **`audit.verify_checkpoint_signature_crypto` does not exist** (0012). Real
  signature verification requires the KMS/HSM; `audit.signing_key` stores only a
  provider reference.
- ~~**`common.new_canonical_id` is not the Go bit layout** (0013).~~ **Closed by
  `0020`.** The bullet previously ended "identifiers are opaque so the encodings
  may differ by design" — an assumption that was never tested and turned out to
  be false. See `0020` below.
- **The halt and operational-mode gates cover the OMS order state machine only.**
  Neither is yet wired to the OMS submission path, order cancellation, or the
  risk-decision service, and neither is yet applied to `ledger.entry` or
  `ops.outbox`. A halt blocks an order becoming possible exposure; it does not
  yet interrupt a service that has already produced a submission intent.
- **An unresolved break cannot block an order written in the same transaction**
  (0018). The gate reads committed `reconciliation.case` rows, so a case opened
  after an order in the same transaction will not stop that order. Reconciliation
  and the OMS are separate bounded contexts and cross-domain writes go through
  domain services rather than shared transactions, so no expected path does this
  — but it is a genuine gap in a safety control and is recorded rather than
  papered over. A serializable-isolation check against uncommitted case rows
  would close it at the cost of cross-context contention, which is a decision
  worth making deliberately rather than by omission.
- **Doc 17 §3 risk-limit attribute validation is NOT enforced** (open gap, not
  closed by `0017`). Every numeric limit must specify currency-or-unit,
  aggregation scope, measurement window, boundary inclusivity, source of truth,
  and behaviour on missing data, and live activation is blocked until the
  complete policy is validated. This is not implemented because it requires a
  document schema — JSON key names for six attributes — that no blueprint
  document specifies, and inventing one would amount to the platform supplying
  universal live numeric trading limits, which doc 17 §3 explicitly prohibits.
  Resolving it needs an owner-supplied schema, not a guess. The existing
  `live_requires_signature` CHECK and the dual-control promotion rule mean a
  live configuration still cannot be authored or promoted without human
  authority; what is missing is validation of the limit values inside it.
- **Invariant 5 is now enforced in both directions and mutation tested** (0016,
  above). A position's quantity must equal the signed sum of validated fills and
  its watermark must be the latest validated fill, checked at commit by two
  complementary deferred triggers. What remains unenforced is the *derivation
  path* itself: there is still no service that computes and writes positions, so
  the schema proves positions cannot be wrong but nothing yet produces a correct
  one. Invariant 5 is a property of accepted writes, not of a working system.
  That derivation service is **G3 scope** (doc 11 §17: "risk gate, OMS state
  machine, order invariants, halt behavior, ledger, and reconciliation tests"),
  not G1, and is not started.
- **Independent review has now been performed once, and the gate is FAIL.**
  A read-only review by an agent that did not author this document returned
  FAIL: four blockers, six major findings. Three blockers were accepted and are
  fixed (a tautological CHECK that shipped in `0002`, a mis-named sequence
  constraint, and a verdict that claimed PASS). One blocker was **refuted** — the
  reviewer reported that `mutate_0017.ps1` mutation G's anchor was absent from
  `0017`, which would have made its 8/8 claim unreproducible. The anchor is
  present at `0017:290`; the harness was re-run and reproduced 8/8, each mutation
  caught by its named test. The full disposition table is in
  `evidence/gates/G1/g1-report.md`. One review has not made this evidence
  independently verified in general — it made it correct once.
- **`occurred_at_ns` / `recorded_at_ns` are stored beside their `TIMESTAMPTZ`
  columns with no cross-check.** A consistency check was written and then removed
  because it produced false positives. The nanosecond columns are therefore not
  bound to the timestamps they claim to refine, and no test asserts the Go
  `TimestampLayout` matches the database storage format.
---

## 0020 — canonical ID bit-layout parity across the Go/PostgreSQL boundary

`0020` closes the last remaining canonical-contract divergence, and it is a
divergence that had been documented as a design choice rather than tested as one.

### What the two implementations actually did

| | `contracts.NewID` (Go) | `common.new_canonical_id` (0013) |
|---|---|---|
| Character source | 5 bits at a time from a big-endian bit stream | `get_byte(b,i) / 8` |
| Bits consumed per character | 5, aligned to the stream | 5, from the low bits of each byte |
| Characters emitted | 20 | 20 |
| Entropy | 100 bits | 100 bits |
| `is_canonical_id` | pass | pass |

Both are valid 20-character Crockford Base32 strings over
`0123456789abcdefghjkmnpqrstvwxyz` with 100 bits of entropy, and both pass the
schema's own `is_canonical_id` check. Neither produces the other's values. The
migration described the SQL function as "the SQL twin of `contracts.NewID`",
which was not true.

The previous evidence bullet closed with "identifiers are opaque so the encodings
may differ by design". That is the kind of sentence that is unfalsifiable until
someone writes the test — and the test is trivial, so not writing it was a
choice with a cost, not a neutral position.

### The fix

`0020` rewrites the SQL encoder to mirror `contracts.encodeCrockford` step for
step: expand the entropy bytes into a most-significant-bit-first stream, then
take five bits at a time. `contracts.IDFromBytes` was added so the encoding can
be driven from fixed entropy rather than from randomness, which is what makes a
parity assertion possible at all.

`TestTheGoAndSQLCanonicalEncodersAgree` pins the two implementations against each
other over 8 fixed vectors — all-zero, all-one, ascending, descending, a lone
high bit, a lone low bit, alternating nibbles, and a 0..15 walk — crossed with
all 29 entity types, so 232 encoder comparisons run per test. A single
agreement vector would not have caught a bit-order regression; a lone high bit
and a lone low bit catch opposite failure modes, which is why both are present.

### Mutation testing

`db/mutate_0020_0021.ps1`, `db/mutate_domain.ps1` mutation A reverts the SQL encoder to the pre-0020
per-byte mapping; mutation B reverses the bit order within each character group.
Both are caught. The test fails on the real defect it was written for.

### The control that was never called

Mutation C restores an earlier state of the file in which
`common.assert_canonical_entropy` existed as a fully-formed function that
**nothing invoked** — the same shape as defect 15, where the safety control was
a comment rather than code, and as the idle guard in `0017`. A guard that is
written, reviewed, and never called provides the appearance of a control while
contributing none. The guard is now invoked from inside the encoder, so the
entropy check is on the path rather than beside it.

This is the fourth time mutation testing has found a control that was present in
the source and absent from the behaviour, which is the strongest argument in this
project for keeping the mutation suite. Reading the source does not surface this
class of defect; disabling the control and re-running does.

---

## 0021 — the nanosecond/timestamp binding

`occurred_at_ns` and `recorded_at_ns` sit beside their `TIMESTAMPTZ` columns with
nothing connecting them. A record could carry `occurred_at` of
`2026-09-30T03:00:00Z` beside an `occurred_at_ns` describing
`2020-01-01T00:00:00Z`, and every constraint in the schema would accept it.

### The check that had been written and removed

`0009:216` carried this note on `audit.append_record`:

> this function deliberately does NOT cross-check `p_occurred_at_ns` against
> `p_occurred_at` ... a caller that samples the clock for the nanosecond value and
> then lets the statement fill the TIMESTAMPTZ from `now()` samples the clock
> twice, so a real, correct record trips the check by the round-trip time.

The diagnosis is correct. The conclusion drawn from it was not. The check was
sound; the callers were unsound. A caller that samples the clock twice and then
expects the two samples to agree has produced an unbound pair, and the available
responses are to bind the pair to one sample or to state the relationship
precisely — not to delete the only mechanism that could have reported the
mistake. The note also concluded "nothing is lost by omitting it", on the grounds
that the hash is computed over the nanoseconds so a verifier reaches the same
hash either way. That is true about the *hash* and irrelevant to the *record*:
an operator querying evidence by time, or a verifier ordering by
`occurred_at`, gets a different answer from the one the ns column holds.

`0021` keeps the note, corrects the conclusion, and binds the pair. The comment
at `0009` is retained rather than deleted because the reasoning is instructive
and because deleting the record of a reversed decision would hide the reversal.

### Rounding, not equality

TIMESTAMPTZ has microsecond resolution. An equality constraint between a
nanosecond value and a TIMESTAMPTZ could never hold, which is presumably why the
check "failed" and was removed. The true relationship is:

> the TIMESTAMPTZ is the nanosecond value rounded to the nearest microsecond.

`common.ns_to_timestamptz` is that single definition, used by the constraint, by
the Go fixtures, and by any consumer. The Go side is `NsToTimestamptz`, pinned by
`TestNsToTimestamptzRoundsToTheNearestMicrosecond` against a hand-computed
boundary case.

Rounding is used rather than truncation deliberately: 123457789ns truncates to
.123457 and rounds to .123458, and a record that reads back as a *different*
nanosecond than the one stored would reintroduce, in a smaller way, exactly the
divergence this migration exists to close. Mutation D replaces the rounding with
truncation and is caught.

### What binding the pair exposed

The constraint could not be written correctly on the first attempt, and the way it
failed is worth recording. The first version used a `void`-returning function
(`common.ns_bound_to_timestamptz(...)`) with `CHECK (assert(...) IS NULL)`. A
void value is never `NULL`, so that check was false for **every row** — the
migration would have applied cleanly, created the constraint, and enforced
nothing. The fix returns a predicate. A check is a predicate; a void function
called in a check position is a check that is always false, and a constraint that
is always false fails closed loudly in testing but would be indistinguishable
from "no data" in production.

Binding the pair also exposed a live defect in the fixtures themselves. The shared
audit fixture passed `NowNs()` from Go for `occurred_at_ns` while PostgreSQL
filled `occurred_at` from its own `now()` — two clock reads, microseconds apart,
disagreeing, and accepted for as long as they existed. The same two-clock pattern
appeared in two further audit fixtures. All three now derive both columns from a
single instant. This is precisely the caller defect the removed check had
described, which is the strongest available confirmation that the original
diagnosis was right and only the remedy was wrong.

### Mutation testing

`db/mutate_0020_0021.ps1`, `db/mutate_domain.ps1` covers both migrations: A (pre-0020 per-byte encoding),
B (reversed bit order), C (uncalled entropy guard), D (`round` → `trunc`),
E (dropped CHECK constraint), F (default `occurred_at` of `now()` restored).
**6/6 caught.**

An earlier run of the same suite reported mutation D surviving. That report was
wrong and the suite was rerun from a clean migration state; the observed cause
was that the sub-microsecond fixture had not yet been corrected, so the mutant
was accidentally still satisfiable. The corrected suite catches it. A mutation
result that disagrees with a direct check is a question about the harness, not
about the mutant, and it was resolved by re-deriving the result rather than by
accepting the first number.
