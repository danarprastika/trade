# G2 — Market Data: structural control audit

Status: **audit complete. Remediation implemented for 16a, 16b and 16d. 16c is
deliberately not implemented and is blocked on a decision no blueprint
document answers. G2 is NOT passed.**
Scope: `db/migrations/0002_market_data.sql` against `16_MARKET_DATA_AND_VENUE_ADAPTERS.md`.
Method: the same audit that produced defects 14 and 15 — read every comment that
asserts enforcement, then look for the trigger, function, or CHECK that
implements it. What the audit found is that the whole G2 safety surface is
storage-only.

The defect descriptions below are kept as written, describing the repository as
it stood when the audit was made. Migration 0019 has since closed three of the
four; see **Remediation outcome** at the end of this document for what each one
actually became, and for the one that remains open.

## Headline finding

```
SELECT count(*) FROM pg_trigger
 WHERE NOT tgisinternal
   AND tgrelid::regclass::text LIKE 'market.%';
```

```
 count
-------
     0
```

Not one trigger, on any table, in the entire `market` schema. `0002_market_data.sql`
declares four safety controls in its comments and implements none of them. The
tables are well designed — the columns exist, the types are right, several CHECK
constraints are real — and none of the enforcement language in the file is
backed by anything.

This is defect 14's pattern, at a larger scale. Defects 14 and 15 were single
controls in `config` and `reconciliation`; this is the entire market-data
boundary.

## Defect 16a — instrument capabilities are declared and never read

`0002_market_data.sql:30-36` defines the capability set:

```sql
order_types       common.order_type[] NOT NULL,
supports_shorting BOOLEAN     NOT NULL DEFAULT false,
supports_leverage BOOLEAN     NOT NULL DEFAULT false,
trading_status    common.trading_status NOT NULL DEFAULT 'UNKNOWN',
```

preceded by the comment *"Capabilities and rules are DATA, not scattered
conditional logic (16_..._ADAPTERS.md §5)."* The claim is half true and the half
that is false is the load-bearing half. They are data. **No trigger or function
anywhere in the database reads them.** A repo-wide search for these column names
returns only their own `CREATE TABLE` and the enum definition in `0001`.

What the platform therefore permits today, with the schema as written:

- an order of type `MARKET` against an instrument whose `order_types` is `[LIMIT]`
- a short sale against an instrument with `supports_shorting = false`
- a new order against an instrument with `trading_status = 'DELISTED'`

`trading_status` is the most serious of the three, because `0002:38-39` states
*"tradability is never inferred from a wall-clock schedule alone when venue status
is available"* — and `market.instrument` is precisely where venue status lives.
The column defaults to `'UNKNOWN'`, which by the file's own reasoning should be
a deny condition, and is not.

The `0002:24-25` comment makes the same claim for increments: *"An unknown
increment is a deny condition for risk-increasing activity."* `min_quantity`,
`price_increment`, `tick_size` and `min_notional` are likewise referenced nowhere
outside their own `CREATE TABLE`. Order quantity is never checked against
`min_quantity`, price is never checked against `price_increment`, notional is
never checked against `min_notional`.

The concrete failure is a non-conforming order leaving the platform. A quantity
below the venue minimum, or a price off the tick grid, is an order the venue will
reject — or, worse, accept at a price the platform never intended to trade. The
adapter is the last line of defence and it is not in the control plane.

## Defect 16b — `feed_unknown_not_healthy` is a tautology

`0002_market_data.sql:213`:

```sql
CONSTRAINT feed_unknown_not_healthy CHECK (health_state <> 'UNKNOWN' OR health_state = 'UNKNOWN')
```

`x <> 'A' OR x = 'A'` is true for every value of `x`. The constraint is
satisfied by construction and rejects nothing. It sits directly beneath the
comment it appears to implement:

> UNKNOWN is a distinct state from DOWN. An unverifiable feed is a deny
> condition for risk-increasing activity, not an implicit pass.

`health_state IN ('HEALTHY','DEGRADED','DOWN','UNKNOWN')` on the line above is a
real constraint. This one is the appearance of a control, and a CHECK constraint
that always passes is harder to notice than a missing one, because `pg_constraint`
reports it as present.

`0002:203-205` makes a second claim in the same block: *"Expected sequence
watermark. A source_sequence below this indicates a sequence gap; the feed is
degraded until reconciled."* `sequence_no_regression` on line 214 checks
`observed_sequence >= 0` and nothing else. No code computes `sequence_gaps`, and
nothing sets `health_state` to `DEGRADED` when a gap appears.

## Defect 16c — freshness controls are never consulted

`0002_market_data.sql:220`, on `market.feed_health`:

> The Risk Engine rejects risk-increasing commands when required inputs are stale
> or degraded; the platform never substitutes a cached value without labelling its
> age and source.

`0002:246`, on `market.freshness_policy`:

> Missing limits deny risk-increasing activity; no default value may silently
> imply permission to trade (17_CONFIGURATION_AND_RISK_POLICY.md §2).

A repo-wide search for `feed_health`, `freshness_policy` and
`last_source_timestamp` outside `0002_market_data.sql` returns **zero hits**.

This is the most consequential of the three, because it is the direct G2
counterpart to what `0018` just closed. `0018` established the pattern: an
unresolved material reconciliation break must block risk-increasing activity at
the boundary. `0002` describes the identical control for market data — stale or
degraded inputs deny risk-increasing activity — and it is not wired to anything.
An operator watching a DEGRADED feed see it recorded in a table that no risk path
reads, and reasonably conclude trading is being blocked.

`0002:189-193` is explicit that the design anticipated this: *"The recorded
last-good source timestamp is what the Risk Engine reads; it is a NULLABLE,
explicitly-checked value so that 'never observed' fails closed rather than
defaulting to fresh."* The nullable/explicitly-checked design is correct. The
check is not there.

## Defect 16d — a venue symbol mapping can be rewritten after it takes effect

`0002_market_data.sql:81-82`, on `market.venue_symbol`:

> Versioned, audited venue-to-instrument symbol mapping. A change creates a new
> version rather than mutating the existing mapping, so history remains
> reconstructible.

`venue_symbol` has no triggers. `changed_by` and `change_reason` are `NOT NULL`,
so an update must claim provenance, but nothing stops an `UPDATE` rewriting the
`symbol` of an already-effective mapping in place, and nothing versions the
change. Both halves of the claim fail: the mapping can be mutated, and history
is not reconstructible.

This is the highest-severity item in this document by consequence, because
`venue_symbol` is the join between platform identity and venue reality.
`0002:12-14` states the principle correctly — *"A venue symbol is NEVER a globally
unique identifier"* — and that principle is what makes a silent in-place rewrite
dangerous. `venue_symbol_unique_per_version` stops one symbol mapping to two
instruments, but it does nothing to stop an existing mapping being *repointed* to
a different symbol or a different instrument. Two accounts, an order routed by a
mutated mapping, and fills arriving against the wrong instrument is the failure
this table exists to make impossible.

This is the same class as defect 14b (`config.content_digest` bound to nothing):
a column whose stated purpose is auditability, holding a value that was never
checked against the thing it describes.

## Why this was not caught earlier

Defects 14, 15 and 16 were all found the same way, and all three are visible
without running anything: read the comment, then look for the trigger. The reason
they survived is that G1's audit covered the OMS, ledger, audit, config and
reconciliation schemas — the ones the execution gates name explicitly in G1 and
G3 — and `market` was assumed to be covered by "migrations applied cleanly".

`0002` applies cleanly. It is well-formed, correctly typed, and has real CHECK
constraints. Nothing in a green migration run distinguishes a table with
enforcement from a table that only records an intention to enforce.

## Remediation plan (migration 0019, not yet written)

> Superseded. This was the plan as written before any of it was implemented. It
> is kept because the ordering it chose — by consequence, not effort — turned out
> to be right, and because a plan that is quietly deleted when it is executed
> cannot be compared against what was built. What was actually built follows
> under **Remediation outcome**.

Ordered by consequence, not by effort:

1. **16d** — make `market.venue_symbol` append-only once effective: refuse
   `UPDATE` of identity columns on a row whose `effective_at` has passed, and
   refuse `DELETE` of a row that was ever effective. Small, high value, no
   judgement calls.
2. **16a** — enforce instrument capabilities and increments at the OMS order
   boundary, inside `ops.assert_risk_increase_permitted`, so the check happens
   under the same transaction and principal as the order it refuses. `UNKNOWN`
   `trading_status` and a missing increment both deny.
3. **16c** — wire `feed_health` and `freshness_policy` into the same boundary
   function, as the market-data counterpart to `0018`. Deny on `DEGRADED`,
   `DOWN`, `UNKNOWN`, on a missing policy row, and on an age past the policy
   bound. This is the one that needs a scope decision, recorded in the migration.
4. **16b** — replace the tautology. Decide whether `UNKNOWN` is a valid
   persistable state (it should be: a feed nobody has checked yet) and move the
   actual control to the boundary function in 3, where "UNKNOWN denies" can be
   tested against a real order. A CHECK constraint on the state column is the
   wrong place for this control regardless of how the predicate is written.

## Open question for the operator

`market.freshness_policy` is keyed by `(policy_key, environment, market_class)`.
16c needs a defined answer to "which policy applies to this order?" — by
`market_class` alone, or also by `venue_id` and `instrument_id`. The blueprint
does not say. Absent a decision, the conservative implementation is to deny when
**no** policy row matches, which is safe and may be stricter than intended. This
is the same class of gap as doc 17 §3 risk-limit attributes: a schema-level
question that needs an owner, not a guess.

**This question is still open and is now the only thing standing between G1 and
a passed G2.** It is restated in full, with the three candidate answers and the
reason each was rejected as a guess, in the migration itself
(`0019_market_data_instrument_and_symbol_enforcement.sql`, "OPEN QUESTION --
defect 16c"). It is recorded in one place rather than two so that the two
cannot disagree.

## Remediation outcome

Migration `0019_market_data_instrument_and_symbol_enforcement.sql` (548 lines)
implements three of the four defects. Enforcement lives in `dbtest/market_data_enforcement_test.go`,
26 tests, all against the live PostgreSQL 17 instance rather than against a mock.

### 16a — closed

`market.increment_multiple`, `market.instrument_permits` and
`market.instrument_is_tradable` evaluate the instrument's declared capabilities;
`market.assert_order_terms_permitted` refuses an order whose type, quantity,
price or notional the instrument does not permit; and the trigger
`oms_order_instrument_terms_guard` fires it on the OMS order table. The check
therefore runs under the same transaction, principal and order as the thing it
refuses — not in the adapter, which would be too late to matter.

`UNKNOWN` `trading_status` denies, as 0002's own reasoning required. A
market order that cannot be valued against `min_notional` is refused rather than
passed. Risk-*reducing* activity is explicitly not blocked, so none of these can
trap an operator in an open position.

Covered by 14 tests, including the negative controls
`TestASupportedOrderTypeIsAccepted`, `TestAWholeNumberOfIncrementsIsAccepted`,
`TestAnOrderAtOrAboveMinNotionalIsAccepted`, `TestClosingActivityIsNotTreatedAsAShort`
and `TestTradabilityDoesNotBlockRiskReducingActivity`.

### 16b — closed, by removing the constraint rather than rewriting it

`feed_unknown_not_healthy` is **dropped**, not reworded. It promised that
UNKNOWN is a deny condition for risk-increasing activity, which is a property of
an *order*, not of a feed-health row: `market.feed_health` holds several feeds,
and declaring any one of them non-UNKNOWN at write time would assert the platform
has confirmed a feed it has not seen. `feed_health_state_valid` is real and is
kept.

The second half of 16b is closed too. 0002 lines 203-205 promised that a source
sequence below the expected watermark degrades the feed; `sequence_no_regression`
checked only `observed_sequence >= 0` and nothing computed `sequence_gaps`. The
trigger `feed_sequence_monotonic` with
`market.guard_feed_sequence_monotonic` now refuses a watermark that moves
backwards, covered by `TestASequenceWatermarkCannotMoveBackwards`,
`TestASequenceWatermarkMayAdvance` and `TestTheFirstWatermarkCanBeSetFromNull` —
the last being a negative control, since refusing the null→value transition would
have been the easy wrong answer.

### 16d — closed

`market.guard_venue_symbol_append_only` plus the trigger
`venue_symbol_append_only` make an effective mapping append-only: no repointing
at another instrument, no rename, no change of `effective_at`, no delete. A
mapping is retired by superseding it with a new version, which is what the
0002 comment claimed the design did.

Covered by 7 tests. Two matter more than the rest, because they are what
distinguishes "append-only" from "append-only in a way that makes corrections
impossible": `TestAnEffectiveVenueSymbolCanStillBeRetired` and
`TestAFutureDatedVenueSymbolMappingCanBeCorrected`. A guard that refuses every
update would pass the other five and fail both.

### 16c — NOT closed, and not a pending task

The migration ends with a long comment declining to implement 16c, and that is
the correct outcome rather than a shortfall to be tidied away. The blocker is
that `market.freshness_policy` is keyed `(policy_key, environment, market_class)`
and wiring it to an order requires choosing which policy governs a given order.
The three candidate answers — `market_class` alone, plus `venue_id`, plus
`instrument_id` — are not refinements of one another. The loosest permits an
order to be governed by a policy written for a different venue's latency
profile; the tightest denies every order whenever the operator has not written a
per-instrument row.

The "deny when no policy matches" default is available under all three and would
be safe to ship, which is exactly why it was not shipped. 16c is a live-trading
safety control, and doc 11 line 49 requires that all venue-, instrument- and
strategy-specific risk limits be "configured and independently reviewed" before
G11. Implementing a guessed scope rule would create the appearance of a reviewed
control.

#### The scope question is the second thing to settle, not the first

Measured 2026-10-03 against the live database and the repository:

```
Go references to feed_health or freshness_policy in domain/, services/,
  adapters/, contracts/                                                     : 0
Rows in market.feed_health                                                 : 0
Triggers on market.feed_health  : feed_sequence_monotonic only
```

`market.feed_health` has no maintainer. The one trigger on it refuses a
watermark that moves backwards and computes nothing; `health_state` is written
by no production statement anywhere in this repository. The only writes are two
dbtest fixtures, both inside transactions that roll back.

This rules out the obvious partial fix, which is what makes the measurement worth
recording. Reading `health_state` without the age bound looks like the safe half
of 16c, because every direction it can refuse in is the refusing direction. But
it reads a column no process maintains, over a table that is empty, and an
aggregate over that table is one of exactly two things:

- `NOT EXISTS (any feed that is not HEALTHY)` — vacuously **true**, so the control
  *passes* on absent data. A fail-closed control becomes fail-open the moment the
  table is empty, which is its default state.
- `EXISTS (a HEALTHY feed for this market)` — vacuously **false**, so every order
  is denied for a reason naming feed health when the real cause is that nothing
  feeds the table.

Neither is a control. The first is strictly worse than today's unconditional
denial, because it *looks* like the defect this audit exists to catch — a
documented enforcement backed by nothing — which is the exact class 0019 was
written to close. The second reproduces the original 16c failure verbatim: a
DEGRADED-shaped refusal pointing an operator at a table that is empty.

So 16c is not only blocked on a decision. It is **downstream of a component that
does not exist**:

```
concrete venue adapter  ->  feed health writer  ->  16c freshness control
                                                         ->  MAX_ORDER_NOTIONAL reference price
```

The last link was already recorded as a dependency of 16c. The first two were
not, and they are what turns this from a scheduling question into a sequencing
one. The scope decision is still required — a feed health writer does not tell
you which `freshness_policy` row governs which order — but it is the second thing
to settle, not the first.

**Consequence to carry forward: until this is answered, `market.feed_health` and
`market.freshness_policy` record observations that no risk path reads.** An
operator watching a DEGRADED feed recorded in `market.feed_health` may
reasonably conclude trading is blocked. It is not, because no control consumes
those rows. 0019 closed the *instrument* half of the market-data boundary, which
is the half that could be closed without inventing policy or inventing a data
source.
