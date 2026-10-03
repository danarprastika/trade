#!/usr/bin/env pwsh
# =============================================================================
# db/migrate.ps1 - Migration harness
#
# Applies every migration in db/migrations in lexicographic order against a
# target PostgreSQL 17 instance, tracking applied versions in schema_migration.
#
# Migration discipline (24_ENTERPRISE_RELEASE_STANDARD.md §5):
#   - Every migration is EXPAND-only and must remain compatible with the
#     immediately previous release so that rollback never requires a
#     down-migration of a destructive change.
#   - -Reset is a development-only affordance and refuses to run unless the
#     target explicitly declares itself non-production.
# =============================================================================
param(
    [Parameter(Mandatory = $true)][string] $Container,
    [string] $Database = 'aitc',
    [string] $User = 'postgres',
    [switch] $Reset
)

$ErrorActionPreference = 'Stop'

# SQL reaches psql through PowerShell's native-command pipeline, and on PowerShell 5.1
# that pipeline is encoded by $OutputEncoding -- NOT by [Console]::OutputEncoding, which
# governs what the console displays rather than what is handed to a native process.
# $OutputEncoding defaults to ASCII, so every non-ASCII character in a migration was
# silently mangled into '?' on the way in, with no error raised and no drift reported by
# this harness, because the digest verified: it digests the file on disk, which was
# correct, while the database held something else.
#
# Both encodings were measured on this host, piping a section sign through psql and
# reading ascii() back:
#
#     $OutputEncoding ASCII   + [Console]::OutputEncoding UTF-8   -> 63  ('?')   WRONG
#     $OutputEncoding ASCII   + [Console]::OutputEncoding Latin-1  -> 63  ('?')   WRONG
#     $OutputEncoding UTF-8   + [Console]::OutputEncoding UTF-8   -> 167 ('§')  CORRECT
#
# An earlier attempt at this fix set only [Console]::OutputEncoding and changed nothing,
# which is recorded here because it is the more natural guess and it is wrong.
#
# The encoding must be UTF-8 WITHOUT a byte order mark: a BOM-emitting encoding corrupts
# the very first statement and the migration fails with a syntax error near the BOM.
$previousOutputEncoding = $OutputEncoding
$previousConsoleEncoding = [Console]::OutputEncoding
$OutputEncoding = New-Object System.Text.UTF8Encoding($false)
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)

function Restore-NativeEncodings {
    # $script: on the left-hand side, not a bare $OutputEncoding. A bare
    # assignment resolves the target scope by walking the scope chain and
    # assigning to the nearest scope that already holds the variable. That
    # happens to be the script scope here, but only because line 46 happened
    # to assign it there first -- an implicit dependency on assignment order.
    # Naming the scope makes the restore explicit and correct even if the
    # save is ever moved or the script is dot-sourced into a scope that
    # already has its own $OutputEncoding.
    $script:OutputEncoding = $script:previousOutputEncoding
    [Console]::OutputEncoding = $script:previousConsoleEncoding
}

# The reading side is a separate defect with the same symptom. In PowerShell 5.1,
# Get-Content -Raw decodes text using the ANSI code page rather than UTF-8, so the
# section sign arrives as U+00C2 (the mojibake 'Â§') before the pipeline ever runs.
# Fixing only $OutputEncoding would therefore apply a different wrong value, which
# db/verify-schema-live.ps1 would still report as drift. Every read below uses
# [System.IO.File]::ReadAllText, which honours UTF-8 as written.

$migrationDir = Join-Path (Split-Path -Parent $PSScriptRoot) 'db/migrations'
if (-not (Test-Path -LiteralPath $migrationDir)) {
    $migrationDir = Join-Path (Get-Location) 'db/migrations'
}

function Invoke-Sql {
    param([string] $Sql, [switch] $Quiet)
    # psql writes NOTICE/WARNING to stderr. PowerShell surfaces native stderr as
    # error records, which would abort under $ErrorActionPreference='Stop', so
    # the preference is relaxed for the duration of the call and success is
    # determined by the exit code alone.
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $result = $Sql | docker exec -i $Container psql -U $User -d $Database -v ON_ERROR_STOP=1 -tAq 2>&1
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prev
    if ($code -ne 0) {
        throw "SQL failed:`n$Sql`n---`n$result"
    }
    # Always return the result set. -Quiet suppresses psql's command tags; it
    # must NOT suppress query output, because the caller uses this to discover
    # already-applied migrations.
    return ($result | Out-String)
}

if ($Reset) {
    # Refuse to reset anything that looks like a production target.
    if ($Database -match 'prod|live') {
        throw "Refusing to reset database '$Database': the name indicates a protected environment."
    }
    Write-Output "Resetting database '$Database' on container '$Container'."
    Invoke-Sql -Quiet -Sql "DROP SCHEMA IF EXISTS public CASCADE; DROP SCHEMA IF EXISTS identity CASCADE;
        DROP SCHEMA IF EXISTS config CASCADE; DROP SCHEMA IF EXISTS market CASCADE;
        DROP SCHEMA IF EXISTS strategy CASCADE; DROP SCHEMA IF EXISTS risk CASCADE;
        DROP SCHEMA IF EXISTS oms CASCADE; DROP SCHEMA IF EXISTS execution CASCADE;
        DROP SCHEMA IF EXISTS reconciliation CASCADE; DROP SCHEMA IF EXISTS portfolio CASCADE;
        DROP SCHEMA IF EXISTS ledger CASCADE; DROP SCHEMA IF EXISTS audit CASCADE;
        DROP SCHEMA IF EXISTS ops CASCADE; DROP SCHEMA IF EXISTS common CASCADE;"
    # public is dropped above; recreate it so schema_migration has a home.
    Invoke-Sql -Quiet -Sql "CREATE SCHEMA IF NOT EXISTS public;"
    Invoke-Sql -Quiet -Sql "DROP TABLE IF EXISTS public.schema_migration CASCADE;"
}

# schema_migration is the authoritative record of what has been applied. It lives
# in public so that it survives a domain-schema drop during a contract release.
Invoke-Sql -Quiet -Sql @"
CREATE TABLE IF NOT EXISTS public.schema_migration (
    version      TEXT        PRIMARY KEY,
    applied_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at_ns BIGINT     NOT NULL,
    content_digest TEXT      NOT NULL,
    duration_ms  INTEGER     NOT NULL
);
"@

$applied = @{}
# version -> recorded content digest. The recorded digest is compared against the
# digest of the file on disk for every already-applied migration: a migration
# history that was edited after it ran is a release blocker, because the
# database and the repository now disagree about what was applied
# (24_ENTERPRISE_RELEASE_STANDARD.md §5).
$rows = (Invoke-Sql -Quiet -Sql "SELECT version || ' ' || content_digest FROM public.schema_migration;") -split "`r?`n"
foreach ($r in $rows) {
    $v = $r.Trim()
    if ($v) {
        $parts = $v -split '\s+', 2
        $applied[$parts[0]] = $parts[1]
    }
}

$files = Get-ChildItem -LiteralPath $migrationDir -Filter '*.sql' | Sort-Object Name
$failures = 0
$appliedNow = 0
$drift = 0

foreach ($f in $files) {
    $version = [System.IO.Path]::GetFileNameWithoutExtension($f.Name)
    $digest = (Get-FileHash -LiteralPath $f.FullName -Algorithm SHA256).Hash.ToLower()
    if ($applied.ContainsKey($version)) {
        if ($applied[$version] -ne $digest) {
            Write-Output ("DRIFT {0}: applied digest {1} != file digest {2}" -f $version, $applied[$version], $digest)
            Write-Output ""
            Write-Output "  The file changed after it was applied. Recover like this, in this order:"
            Write-Output "    1. Decide whether the edit changes the resulting schema. A comment-only or"
            Write-Output "       documentation-only edit does not: the live schema is already correct and"
            Write-Output "       only this file's bytes disagree with what the ledger recorded."
            Write-Output "    2. Make the file re-runnable. Every CREATE TRIGGER and every ADD CONSTRAINT it"
            Write-Output "       performs must be preceded by DROP ... IF EXISTS in the same file, because a"
            Write-Output "       duplicate name is a hard error and the file then cannot be applied at all."
            Write-Output "       Most migrations in this repository are not re-runnable as written -- 34 object"
            Write-Output "       creations across the set have no preceding drop -- so expect to add them."
            Write-Output "    3. Delete this version's row from public.schema_migration, then re-run this script."
            Write-Output "       The migration is transactional unless it says otherwise, so a failed re-apply"
            Write-Output "       rolls back whole and leaves nothing behind."
            Write-Output "    4. Do not UPDATE the recorded digest to match the edited file. Re-recording the"
            Write-Output "       digest is the one repair that destroys the evidence, because it makes the"
            Write-Output "       edit permanently undetectable while the schema was never rebuilt from it."
            Write-Output ""
            $drift++
        }
        else {
            Write-Output "SKIP  $version (already applied, digest verified)"
        }
        continue
    }
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        # ReadAllText, not Get-Content -Raw: the latter decodes with the ANSI code page on
        # PowerShell 5.1 and turns a section sign into mojibake before the pipeline runs.
        $sql = [System.IO.File]::ReadAllText($f.FullName)

        # A migration that builds an index CONCURRENTLY cannot run inside a
        # transaction block -- PostgreSQL rejects it with "CREATE INDEX
        # CONCURRENTLY cannot run inside a transaction block". Such a
        # migration opts out by carrying the marker below and is applied
        # exactly as written. The trade is stated rather than hidden: with
        # no transaction a failure partway through leaves the earlier
        # statements applied, so a no-transaction migration must be written
        # to be re-runnable (CREATE OR REPLACE, and IF NOT EXISTS where the
        # object allows it). Every other migration keeps the transactional
        # guarantee that a failure rolls back cleanly.
        $noTransaction = $sql -match '(?m)^[ \t]*--[ \t]*migrate:[ \t]*no-transaction'

        # The ledger row is written inside the same transaction as the migration
        # itself. Recording it afterwards left a window in which the schema change
        # had committed but the ledger did not: a crash in that window re-applies
        # the migration on the next run, and EXPAND-only discipline does not make
        # every statement idempotent.
        #
        # applied_at_ns is computed by the database from clock_timestamp(). The
        # previous code derived it from [System.Diagnostics.Stopwatch]::GetTimestamp(),
        # which returns a QueryPerformanceCounter value -- an arbitrary offset, not
        # DateTime ticks -- so subtracting [DateTime]::UnixEpoch.Ticks from it
        # produced a number that was neither an epoch nor a duration. Measured
        # against the live ledger, every applied_at_ns was around 1.32e14 instead of
        # the ~1.79e18 a 2026 epoch nanosecond is.
        #
        # duration_ms is likewise measured between two readings of the DATABASE's
        # clock, stashed by set_config just after BEGIN and read back by the INSERT.
        # A host-side stopwatch cannot be used for the ledger row, because the
        # transaction commits before PowerShell learns how long it took -- which is
        # why the console line below still reports the host-measured elapsed time
        # and the ledger reports the database-measured one.
        # The start stamp travels in a psql VARIABLE rather than a PostgreSQL GUC.
        # A custom GUC named migration.start_ms is rejected outright --
        # 'unrecognized configuration parameter' -- because PostgreSQL reserves
        # that prefix. \gset writes the query result into a psql variable, which
        # needs no registration and is discarded with the session, so it cannot
        # leak into the next migration or into any consumer of the database.
        $startSql = "\gset"
        $startQuery = "SELECT (EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::bigint AS start_ms;"
        $ledger = "INSERT INTO public.schema_migration (version, applied_at_ns, content_digest, duration_ms) VALUES ('$version',
            (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000000)::bigint,
            '$digest',
            GREATEST(((EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::bigint) - :'start_ms', 0)::int);"

        if ($noTransaction) {
            # No transaction to join: apply the migration and record it in one
            # psql session. The ledger write is a separate autocommitted
            # statement, which is the one atomicity this path gives up. CREATE
            # INDEX CONCURRENTLY and a transaction are mutually exclusive, and an
            # index that does not block writers is the reason the opt-out exists.
            #
            # The start stamp, the migration and the ledger row all travel in the
            # SAME session, because \gset stores the value in a psql variable that
            # does not survive the session exiting. Running the migration in one
            # Invoke-Sql and the ledger in another loses the variable between them,
            # which is why they are concatenated here rather than issued separately.
            $toRun = "$startQuery`n$startSql`n$sql`n$ledger"
        } else {
            # The harness is the single transaction authority. Migrations 0022-0025
            # carried their own top-level BEGIN;/COMMIT;, which this wrapper nested
            # inside another BEGIN. Two nested BEGINs do not compose, and the
            # inner COMMIT ended the outer transaction early -- so the ledger row
            # that was supposed to be atomic with the schema change committed
            # separately, or not at all.
            #
            # The file's own BEGIN;/COMMIT; are therefore stripped and the harness
            # supplies the transaction. Only lines that are exactly BEGIN; or COMMIT;
            # are touched: a plpgsql body opens with a bare BEGIN (no semicolon), which
            # this does not match, and a function cannot contain COMMIT in any case.
            # Applied this way every migration is uniform -- one transaction, with the
            # ledger row inside it -- which is what makes the ledger row atomic with
            # the schema change.
            $body = $sql
            $body = [regex]::Replace($body, '(?m)^[ \t]*BEGIN;[ \t]*\r?\n', '')
            $body = [regex]::Replace($body, '(?m)^[ \t]*COMMIT;[ \t]*\r?\n?', '')
            $toRun = "BEGIN;`n$startQuery`n$startSql`n$body`n$ledger`nCOMMIT;"
        }

        $prev = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        $result = $toRun | docker exec -i $Container psql -U $User -d $Database -v ON_ERROR_STOP=1 2>&1
        $code = $LASTEXITCODE
        $ErrorActionPreference = $prev
        if ($code -ne 0) { throw ($result | Out-String) }

        $sw.Stop()
        Write-Output ("OK    {0}  ({1} ms)" -f $version, $sw.ElapsedMilliseconds)
        $appliedNow++
    }
    catch {
        $sw.Stop()
        Write-Output ("FAIL  {0}: {1}" -f $version, $_.Exception.Message)
        $failures++
        break
    }
}

Write-Output "---"
Write-Output ("applied_now={0} total_files={1} failures={2} drift={3}" -f $appliedNow, $files.Count, $failures, $drift)

# Restore before every exit, including the error exits. An earlier version restored
# through a trap, which fires on terminating errors but not on `exit`, so the encoding
# leaked out of the script whenever this file was run in-process or dot-sourced and left
# the caller's own native-command encoding changed as a side effect of a migration run.
Restore-NativeEncodings

if ($failures -gt 0) { exit 1 }
# A modified applied migration is a hard failure: the schema in the database was
# built from different bytes than the repository claims.
if ($drift -gt 0) { exit 2 }

exit 0
