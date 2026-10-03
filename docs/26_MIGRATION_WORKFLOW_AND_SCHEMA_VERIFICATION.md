# Migration Workflow and Schema Verification

## Applying migrations

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File db/migrate.ps1 -Container aitc-pg17 -Database aitc
```

Migrations are plain `.sql` files in `db/migrations/`, applied in filename order. `migrate.ps1` records each one in `public.schema_migration` with the SHA-256 digest of the bytes it applied, so the ledger is the authority on what the database was actually built from — not a record that a file with that name ran once.

It exits `0` when nothing is left to do, `1` when a migration failed, and **`2` on drift**, which is a hard failure: the schema in the database was built from different bytes than the repository claims. The ledger row is written inside the same transaction as the migration, so a failed migration cannot leave a row claiming success.

The duration it records comes from `clock_timestamp()`, not `now()`, because a transaction that opens many subtransactions would otherwise report near-zero elapsed time for work it actually performed. A row is expected to carry a non-zero duration; a zero means the measurement is wrong.

## An applied migration is immutable

**To change the schema, add a migration. Do not edit one that has been applied.**

This is not a style preference. Editing an applied file changes its digest, and the digest check will report `DRIFT` on every subsequent run, permanently, until the file is reconciled with the database. The drift is not cosmetic: it is the only signal that the repository and the database have diverged.

The tempting repair is to re-record the digest so the file and the ledger agree again. **Never do this.** It is the one change that destroys the evidence — it makes the edit permanently invisible while the schema was never rebuilt from the edited bytes.

### If you must reconcile an edited applied migration

`db/migrate.ps1` prints this procedure whenever it reports `DRIFT`. It is repeated here because it is the procedure you will need at the moment you discover you need it.

1. **Decide whether the edit changes the resulting schema.** A comment-only or documentation-only edit does not: the live schema is already correct and only the file's bytes disagree with the ledger.
2. **Make the file re-runnable.** Every `CREATE TRIGGER` and every `ADD CONSTRAINT` must be preceded by a matching `DROP ... IF EXISTS` in the same file. A duplicate object name is a hard error, so without this the file cannot be applied at all and the drift is unrecoverable. Note that `CREATE OR REPLACE FUNCTION`, `COMMENT ON`, and `DROP ... IF EXISTS` are already idempotent.
3. **Delete that version's row from `public.schema_migration`, then re-run the script.** A migration that is transactional rolls back whole on failure, so a failed re-apply leaves nothing behind and the attempt is bounded rather than dangerous. A migration marked `-- no-transaction` (needed for `CREATE INDEX CONCURRENTLY`) cannot be rolled back, which is exactly why its re-runnability matters more.
4. **Do not re-record the digest.**

Step 2 is mechanical but is currently absent from most of the set. Measured: 34 object creations across `db/migrations/` have no preceding drop, so most applied migrations are not re-runnable as written. **Do not fix this by editing those files.** Editing an applied file to add a `DROP` changes its digest, which creates precisely the drift you are trying to avoid. Leave them alone; if one ever needs reconciling, do the procedure above for that one file.

## Verifying the schema

### The gate: database against repository

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File db/verify-schema-live.ps1
```

Compares the live database against the migration files and exits non-zero on any difference. Current measurement: `compared_functions=64`, `compared_triggers=27`, `compared_indexes=85`, `compared_constraints=262`, `drift=0`.

It checks function bodies against `pg_proc.prosrc`, and for triggers their existence, table, executed function and `tgenabled`; for indexes their schema, table, uniqueness, key count, column names, and `indisvalid`/`indisready`; for named constraints their existence, type, and validated state. It also reports objects that exist live but that no migration declares, which is the direction a files-only comparison cannot see.

### The harness: proving the gate detects anything

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File db/verify-schema-live-mutations.ps1
```

A gate that has only ever run against a correct schema has demonstrated nothing — it would report `drift=0` just as happily against a database with every index dropped. This harness mutates each dimension in turn and asserts the gate notices, restoring the database and verifying the restore is byte-identical after every case. It asserts **25 of 25** mutations detected with `not_detected=0`, and it refuses to report success unless both the gate and `db/migrate.ps1` are green at the end.

It covers five dimensions: **function bodies** (altered, dollar-sign signature, undeclared-but-live, unparseable-declaration, plus a negative control), **indexes** (dropped, wrong table, uniqueness lost, wrong columns, invalid, undeclared, unparseable), **constraints** (dropped, type changed, silently unvalidated, unattributable, plus a negative control), **triggers** (dropped, wrong function, disabled, undeclared, plus a negative control), and a **self-test** of the harness's own SQL transport.

Three of these are negative controls, and they are the reason the others mean anything: one asserts that a `NOT VALID` constraint which migration 0023 genuinely declares unvalidated is *not* reported; one asserts that a trigger dropped and recreated inside a single migration is *not* reported as missing; and one asserts the function dimension stays silent against a correct database, by requiring `compared_functions == expected_functions` — the arithmetic that would expose a comparison silently skipping functions it could not match. A detector that flags everything would pass every positive case.

#### Reading psql output inside the harness

The harness's own result handling has failed in three distinct ways, each of which looked like a detector defect rather than a harness defect. All three are now guarded in code, and they are recorded here because the guard is invisible until you have been bitten by it.

- **`Out-String` must never be used to format psql output in the harness.** It wraps long single-line values at the console width — measured at 60 on this host — inserting newlines into the middle of the value it is meant to pass through. It corrupted the harness twice, in ways that appeared unrelated. A 1802-character hex string came back as 1832 characters, so a correctly restored 901-byte body was reported as 916 bytes and judged unequal to its own original, failing the restore check on a byte-perfect restore. And it formatted the *gate's* text, splitting the long `PARSE_INCOMPLETE` sentence across lines so that no single-line pattern could ever match it — while the cases whose expected patterns were short kept passing and hid the cause. Every wrapper joins psql's lines with `` `n `` instead.
- **Never read a body with `SELECT p.prosrc`.** `Invoke-SqlBytes` ends in `.Trim()`, so it returns a value that looks like the body but is not the stored bytes: bodies here end with a newline and 55 of the 63 non-empty bodies also begin with one, so that read silently dropped bytes. Read the body as hex and decode it instead — hex has no whitespace to normalise and no line to break.
- **In the function dimension, a restore is verified by comparing hex, never by comparing strings.** A trimmed string comparison reported two lossy restores as byte-identical, which is the failure mode the restore check exists to prevent. Note the scope: the index, constraint and trigger dimensions still restore by comparing deparsed definition strings. That is a known asymmetry, not a claim that they were converted — their definitions are single-line and are not affected by either hazard above, but if one of them ever grows a multi-line or trailing-whitespace-bearing body, it needs the hex treatment too.

#### Writing SQL that changes a function body

Two transport hazards apply, and both have destroyed a restore:

- `psql -c` cannot be used for a multi-line function definition. It collapses the newlines, which is how a dollar-quoted body arrives unterminated. Trigger, index and constraint mutations are single-line and demonstrably unaffected, so they still use it.
- PowerShell's native-stdin path is text-mode, so `| psql -f -` rewrites every embedded LF to CRLF. A restored `prosrc` came back at the same length with different bytes, which is worse than an obvious mismatch because it reads like success.

The harness therefore writes the SQL to a BOM-less UTF-8 file, `docker cp`s it, and runs `psql -f`. `ON_ERROR_STOP=1` is required there: psql reading a script continues past a failed statement and exits `0`, so a mutation that did not apply would be indistinguishable from one the detector did not catch.

### Also

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File db/verify-gofmt.ps1        # packages=16, go_files_examined=88, unformatted=0
go vet ./...
powershell -NoProfile -ExecutionPolicy Bypass -File evidence/evidence.sha256.ps1 -Verify
```

## Rules the schema gate imposes on migration text

These are not stylistic. Each one exists because violating it produced a wrong verdict against a correct database, and a gate that reports phantom differences on a healthy schema is worse than no gate, because its green result stops being believed.

- **DDL mentioned in a comment is not a declaration.** Migration text is read through a comment stripper, but the wording of an error message is still SQL text to a human reading the file. Migration 0027's `RAISE EXCEPTION` deliberately avoids the words `CREATE INDEX` and `DROP INDEX` because the scanner would otherwise count them as declarations and fail closed against a correct repository.
- **A `DROP` and a `CREATE` of the same name must be adjacent in source order.** The trigger walk processes statements in the order they appear, so `DROP` then `CREATE` correctly resolves to the replacement. Grouping the statements into all-creates-then-all-drops instead makes the gate report a live trigger as missing, which is how a correct database was once reported as drifted.
- **Declare a `NOT VALID` constraint as `NOT VALID`.** A constraint silently unvalidated has the right name, the right type and every structural property the gate compares, so a declaration-only review cannot see it — yet it no longer applies to rows that already exist. Declaring it `VALID` when it is not gives `CONSTRAINT_NOT_VALIDATED`.

  Seven constraints are deliberately `NOT VALID`: 0021 adds six nanosecond bounds (two on `audit.record`, one each on `market.trade`, `market.quote`, `market.candle` and `market.feed_health`), and 0023 adds `audit.record.audit_signing_key_required`, because historical rows violate it. `NOT VALID` is not a weakening for anything written from that migration onward — it is the only way to add a constraint to a table that already holds rows the constraint would reject. 0021 records the corresponding `VALIDATE CONSTRAINT` statement, commented out, for each of its six, to be run once the backfill makes it possible; 0023 does not, so removing that one requires a separate migration.

  If you query `pg_constraint` directly you will find far more than seven rows, because `audit.record` is partitioned and each of its three unvalidated constraints is replicated onto all 15 partitions. The gate excludes partition children for the same reason it excludes constraint-backed indexes: they are a consequence of the layout rather than declared objects, and counting them would report dozens of phantom differences against a correct database.

## Running database tests

Tests require `AITC_TEST_DATABASE_URL`. The packages share one database, so run them serially; concurrent packages deadlock each other and produce failures that look like product defects. Use `-count=1` after any change to the database or to a migration, because a cached pass cannot verify a post-restore state.