# verify-schema-live-mutations.ps1 — proves db/verify-schema-live.ps1 actually DETECTS
# divergence, rather than merely reporting drift=0 on a healthy database.
#
# WHY THIS IS A DURABLE SCRIPT AND NOT A ONE-OFF
#
# db/verify-schema-live.ps1 has been green on every run since it was written. A gate that has
# only ever been exercised against a correct schema has been shown to do nothing at all: it
# would just as happily report drift=0 against a database with every index dropped. The only
# way to know a detector works is to fire something at it and confirm it goes off.
#
# Every mutation below is applied to the live database, the gate is run, and the finding is
# asserted. Every mutation is restored from the exact definition captured beforehand, and the
# script ends with a full green re-verification of BOTH the gate and db/migrate.ps1.
#
# THE DIMENSIONS, AND WHY EACH HAS A NEGATIVE CONTROL
#
# Indexes (M1-M7) and named constraints (C1-C5) were covered from the start. Triggers
# (T1-T5) were added when the gate's trigger walk was fixed to read each file's statements in
# order -- a change that could have turned the trigger section into a no-op just as silently as
# the bug it fixed. Functions (F1-F5) were added last, and their absence was the largest hole
# here: the function comparison is the gate's original and biggest dimension at 64 bodies, its
# parser is the most complex, it had already failed silently twice, and the harness's own closing
# line named only the other three. A dimension with only positive cases cannot distinguish a
# working detector from one that reports everything, so each dimension ends with a negative
# control: C5 (a
# constraint migration 0023 declares deliberately unvalidated must NOT be reported), T5 (a
# trigger migration 0026 drops and recreates in the same file must NOT be reported).
#
# THIS SCRIPT MUTATES THE LIVE SCHEMA. It restores unconditionally, and refuses to report
# success unless the final state is green, but it should only be run against a disposable or
# local database. Do not point it at anything you care about.
#
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\db\verify-schema-live-mutations.ps1
[CmdletBinding()]
param(
    [string] $Container = 'aitc-pg17',
    [string] $Database  = 'aitc',
    [string] $User      = 'postgres',
    [string] $GatePath  = ''
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
# $PSScriptRoot is not reliably bound inside a param() default under Windows PowerShell when
# the script is launched via -File, so the default is resolved here instead. Measured: it
# arrived empty and Split-Path failed before the script did any work.
if ([string]::IsNullOrEmpty($GatePath)) { $GatePath = Join-Path $repoRoot 'db/verify-schema-live.ps1' }
$results = @()

# psql reports benign conditions on stderr: "NOTICE: index ... does not exist, skipping" from
# DROP INDEX IF EXISTS, and a bare "CREATE TABLE" from CREATE DATABASE. Under
# $ErrorActionPreference='Stop', Windows PowerShell promotes such a stderr line to a
# terminating NativeCommandError even though psql exited 0, which killed an earlier draft of
# this harness mid-run and left a mutation applied. The EXIT CODE is the authority; stderr
# is not. Every wrapper here therefore relaxes the preference locally and judges by exit code.
function Invoke-Sql {
    param([string] $Sql)
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = & docker exec $Container psql -U $User -d $Database -tAq -c $Sql 2>&1
        $code = $LASTEXITCODE
    } finally { $ErrorActionPreference = $prev }

    if ($code -ne 0) {
        $text = ($out | ForEach-Object { if ($_ -is [string]) { $_ } else { [string]$_ } }) -join ' '
        throw "psql failed (exit $code): $Sql -- $text"
    }
    return (($out | Where-Object { $_ -is [string] -and $_ -notmatch '^(NOTICE|WARNING):' }) -join "`n").Trim()
}

function Invoke-Script {
    param([string] $Path, [string[]] $Arguments = @())
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = & powershell -NoProfile -ExecutionPolicy Bypass -File $Path @Arguments 2>&1
        $code = $LASTEXITCODE
    } finally { $ErrorActionPreference = $prev }
    return [pscustomobject]@{
        Exit = $code
        Text = (($out | ForEach-Object { if ($_ -is [string]) { $_ } else { [string]$_ } }) -join "`n")
    }
}

function Invoke-Gate { return (Invoke-Script -Path $GatePath) }

# Capture the authoritative live definition so restoration is exact rather than re-derived
# from a hand-written approximation. $null when the index does not exist yet -- the
# undeclared-index mutation creates its target, so there is nothing to capture.
function Get-IndexDef {
    param([string] $Qualified)
    try { return (Invoke-Sql "SELECT pg_get_indexdef('$Qualified'::regclass);") }
    catch { return $null }
}

# Apply a mutation, converting "it did not apply" into a value rather than an exception.
#
# This is the single most repeated lesson in this harness's history. A mutation whose SQL never
# reaches the database has proved nothing about the detector, and an exception thrown from here
# propagates out of the calling test function AFTER its finally block has already run -- so the
# restore happens, the harness dies, and every dimension after this one is silently never
# executed. Measured: T2 of the trigger dimension named a trigger that does not exist, psql raised,
# and the run ended at T2 with T3, T4 and the negative control T5 never running. The earlier three
# cases (a replacement string that did not match, an uncompiled mutant, dollar-quoting mangled by
# docker exec) all reported green, which is the version that costs real time.
#
# Returns $null when the mutation applied, or the error text when it did not. Callers record a
# failure to apply as NOT DETECTED, because that is what it is: the detector was not exercised.
function Invoke-Mutation {
    param([string] $Sql)
    try { [void] (Invoke-Sql $Sql); return $null }
    catch { return $_.Exception.Message }
}

# Report a mutation that never reached the database, in one place, so the dimensions cannot
# drift in how they describe it.
function Write-NotApplied {
    param([string] $Name, [string] $Target, [string] $ErrorText)
    Write-Output '  RESULT: *** NOT DETECTED *** (the mutation itself failed to apply, so the detector was never exercised)'
    Write-Output "    psql: $ErrorText"
    $script:results += [pscustomobject]@{ Mutation = $Name; Target = $Target; Detected = $false }
}

function Test-Mutation {
    param(
        [string] $Name,
        [string] $Target,
        [string] $MutateSql,
        [string] $RestoreSql,
        [string] $ExpectPattern,
        [switch] $SkipPreDrop,
        [switch] $Verbose
    )
    $definition = Get-IndexDef $Target
    Write-Output ''
    Write-Output "=== $Name ==="
    Write-Output "  target:   $Target"
    Write-Output "  original: $(if ($null -eq $definition) { '<did not exist>' } else { $definition })"
    try {
        $applyError = Invoke-Mutation $MutateSql
        if ($null -ne $applyError) { Write-NotApplied $Name $Target $applyError; return }
        $gate = Invoke-Gate
        if ($Verbose) {
            ($gate.Text -split "`r?`n") | Where-Object { $_.Trim() } | ForEach-Object { Write-Output "  |$_" }
        } else {
            ($gate.Text -split "`r?`n") | Where-Object { $_ -match 'MISSING_INDEX|INDEX_DRIFT|INDEX_INVALID|UNDECLARED_INDEX|MISSING_CONSTRAINT|CONSTRAINT_TYPE_DRIFT|CONSTRAINT_NOT_VALIDATED|PARSE_INCOMPLETE|^\s{2}\S' } |
                ForEach-Object { Write-Output "  |$_" }
        }
        $detected = ($gate.Text -match $ExpectPattern) -and $gate.Exit -eq 1
        if ($detected) {
            Write-Output "  RESULT: DETECTED (exit=$($gate.Exit))"
        } else {
            Write-Output "  RESULT: *** NOT DETECTED *** (exit=$($gate.Exit)) expected to match /$ExpectPattern/"
        }
        $script:results += [pscustomobject]@{ Mutation = $Name; Target = $Target; Detected = $detected }
    } finally {
        # Restoration is unconditional AND must not itself be able to abort.
        #
        # The pre-drop is required, not cosmetic: a mutation that repoints an index to another
        # table in the SAME schema leaves the name occupied, so re-creating the captured
        # definition fails with "relation already exists" and the restore silently does nothing.
        # Measured: an earlier draft died exactly that way and left
        # identity.session_revocation_idx living on role_binding.
        #
        # Every step is individually guarded, because an unguarded throw INSIDE a finally
        # ABORTS the statements after it. That happened twice during development: a benign
        # NOTICE skipped the re-creation and left the schema mutated.
        $note = $null
        try {
            if (-not $SkipPreDrop) { [void] (Invoke-Sql "DROP INDEX IF EXISTS $Target;") }
        } catch { $note = "pre-drop failed: $_" }
        if (-not $note) { try { [void] (Invoke-Sql $RestoreSql) } catch { $note = "re-create failed: $_" } }

        $after = $null
        try { $after = Get-IndexDef $Target } catch { $note = "post-check failed: $_" }

        if ($note) {
            Write-Output "  RESTORE ERROR for $Target -- $note"
            Write-Output "    ORIGINAL DEFINITION (re-apply by hand): $definition"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $Target; Detected = $false }
        } elseif ($null -eq $definition -and $null -eq $after) {
            Write-Output '  restored OK (absent before, absent after -- as required)'
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $Target; Detected = $true }
        } elseif ($null -eq $definition) {
            Write-Output "  RESTORE FAILED for $Target -- did not exist beforehand but now does: $after"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $Target; Detected = $false }
        } elseif ($after -ne $definition) {
            Write-Output "  RESTORE FAILED for $Target -- definition is not byte-identical"
            Write-Output "    expected: $definition"
            Write-Output "    actual:   $after"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $Target; Detected = $false }
        } else {
            Write-Output '  restored OK (definition byte-identical)'
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $Target; Detected = $true }
        }
    }
}

Write-Output '### INDEX DIMENSION MUTATION PROOFS'
Write-Output "    gate: $GatePath"

# M1: index dropped entirely.
Test-Mutation -Name 'M1 index dropped -> MISSING_INDEX' -Target 'identity.session_revocation_idx' `
    -MutateSql  'DROP INDEX identity.session_revocation_idx;' `
    -RestoreSql (Get-IndexDef 'identity.session_revocation_idx') `
    -ExpectPattern 'MISSING_INDEX[\s\S]*session_revocation_idx'

# M2: repointed to the wrong table. identity.role_binding also carries revoked_at, so this
# is valid SQL that lands the SAME NAME in the SAME SCHEMA on the wrong relation -- the case
# that proves the table comparison is live rather than assumed.
$m2 = Get-IndexDef 'identity.session_revocation_idx'
Test-Mutation -Name 'M2 wrong table -> INDEX_DRIFT (table)' -Target 'identity.session_revocation_idx' `
    -MutateSql  'DROP INDEX identity.session_revocation_idx; CREATE INDEX session_revocation_idx ON identity.role_binding (revoked_at);' `
    -RestoreSql $m2 `
    -ExpectPattern 'INDEX_DRIFT[\s\S]*session_revocation_idx[\s\S]*identity\.role_binding'

# M3: a UNIQUE index silently loses uniqueness. Nothing errors, duplicate rows become
# acceptable, and the invariant the application relies on is simply gone.
$m3 = Get-IndexDef 'portfolio.balance_identity_idx'
Test-Mutation -Name 'M3 uniqueness lost -> INDEX_DRIFT (uniqueness)' -Target 'portfolio.balance_identity_idx' `
    -MutateSql  'DROP INDEX portfolio.balance_identity_idx; CREATE INDEX balance_identity_idx ON portfolio.balance (account_id, environment, currency);' `
    -RestoreSql $m3 `
    -ExpectPattern 'INDEX_DRIFT[\s\S]*balance_identity_idx[\s\S]*uniqueness'

# M4: rebuilt on the wrong columns.
$m4 = Get-IndexDef 'oms.order_state_idx'
Test-Mutation -Name 'M4 wrong columns -> INDEX_DRIFT (key count + columns)' -Target 'oms.order_state_idx' `
    -MutateSql  'DROP INDEX oms.order_state_idx; CREATE INDEX order_state_idx ON oms.order (environment, state);' `
    -RestoreSql $m4 `
    -ExpectPattern 'INDEX_DRIFT[\s\S]*order_state_idx'

# M5: left INVALID, the residue of a failed CREATE INDEX CONCURRENTLY. The index exists, is
# named correctly, and is silently ignored by the planner.
Test-Mutation -Name 'M5 invalid index -> INDEX_INVALID' -Target 'ops.outbox_aggregate_id_idx' `
    -MutateSql  "UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'ops.outbox_aggregate_id_idx'::regclass;" `
    -RestoreSql "UPDATE pg_index SET indisvalid = true WHERE indexrelid = 'ops.outbox_aggregate_id_idx'::regclass;" `
    -SkipPreDrop `
    -ExpectPattern 'INDEX_INVALID[\s\S]*outbox_aggregate_id_idx'

# M6: an undeclared index added live.
Test-Mutation -Name 'M6 undeclared index -> UNDECLARED_INDEX' -Target 'ops.zz_probe_idx' `
    -MutateSql  'CREATE INDEX zz_probe_idx ON ops.outbox (outbox_id);' `
    -RestoreSql 'DROP INDEX IF EXISTS ops.zz_probe_idx;' `
    -ExpectPattern 'UNDECLARED_INDEX[\s\S]*zz_probe_idx'

# =========================================================================================
# M7: PARSE_INCOMPLETE, proven in an isolated sandbox rather than by corrupting the repo.
#
# The fail-closed path is the one that matters most: if a declared index fails to parse, the
# gate must REFUSE to issue a verdict rather than report drift=0 while comparing fewer
# indexes than the repository declares. That cannot be demonstrated against the real
# migrations without editing them, so this builds a throwaway tree containing a copy of the
# gate and two fixture migrations: one complete CREATE INDEX, and one whose statement has no
# terminating semicolon and therefore cannot parse.
#
# It also re-proves the comment stripper from the other direction: the fixture's comment
# mentions CREATE INDEX, and the declared count must be 2, not 3. If a comment mention were
# counted, the count would be 3 and this assertion would fail.
# =========================================================================================
Write-Output ''
Write-Output '=== M7 unparseable index statement -> PARSE_INCOMPLETE (fail closed) ==='
$sandbox = Join-Path ([System.IO.Path]::GetTempPath()) ('schema-gate-parse-probe-' + [Guid]::NewGuid().ToString('N'))
try {
    $sbDb    = Join-Path $sandbox 'db'
    $sbMigr  = Join-Path $sbDb 'migrations'
    [void] (New-Item -ItemType Directory -Path $sbMigr -Force)
    Copy-Item $GatePath (Join-Path $sbDb 'verify-schema-live.ps1') -Force

    # One valid function so the function section passes and execution REACHES the index
    # section; deliberately no triggers, so the trigger section is a no-op.
    @'
CREATE OR REPLACE FUNCTION common.sandbox_ok() RETURNS boolean AS $$
SELECT true;
$$ LANGUAGE sql;
'@ | Set-Content -LiteralPath (Join-Path $sbMigr '0001_base.sql') -Encoding UTF8

    @'
-- The CREATE INDEX below is INCOMPLETE: no terminating semicolon, so the statement pattern
-- cannot match it. The bare mention still counts toward declared_by_files, which is exactly
-- why the gate must notice and refuse rather than silently omit it.
CREATE INDEX sandbox_complete_idx ON ops.outbox (outbox_id);

CREATE INDEX sandbox_incomplete_idx ON ops.outbox (aggregate_id)
'@ | Set-Content -LiteralPath (Join-Path $sbMigr '0002_incomplete.sql') -Encoding UTF8

    $m7 = Invoke-Script -Path (Join-Path $sbDb 'verify-schema-live.ps1')
    ($m7.Text -split "`r?`n") | Where-Object { $_ -match 'PARSE_INCOMPLETE|declared_by_files|compared_' } |
        ForEach-Object { Write-Output "  |$_" }

    # declared=2 (not 3) proves the comment mention was not counted; parsed=1 proves the
    # incomplete statement really did fail to parse; exit=1 proves it failed closed.
    $m7ok = ($m7.Text -match 'declare 2 CREATE \[UNIQUE\] INDEX statements but only 1 could be parsed') -and $m7.Exit -eq 1
    if ($m7ok) {
        Write-Output '  RESULT: DETECTED (exit=1, declared=2 not 3 -- comment stripper confirmed)'
    } else {
        Write-Output "  RESULT: *** NOT DETECTED *** (exit=$($m7.Exit))"
    }
    $script:results += [pscustomobject]@{ Mutation = 'M7 unparseable index -> PARSE_INCOMPLETE'; Target = 'sandbox'; Detected = $m7ok }
} finally {
    if (Test-Path -LiteralPath $sandbox) { Remove-Item -LiteralPath $sandbox -Recurse -Force }
}

# Capture a constraint's authoritative live definition, exactly as for indexes. pg_get_constraintdef
# includes the trailing "NOT VALID" when the constraint is unvalidated, so re-adding this text
# reproduces the original validation state too -- which matters, because the repository contains
# one deliberately unvalidated constraint and a restore that silently validated it would be a
# real, permanent schema change.
function Get-ConstraintDef {
    param([string] $Schema, [string] $Table, [string] $Name)
    try {
        return (Invoke-Sql @"
SELECT pg_get_constraintdef(c.oid)
  FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
  JOIN pg_namespace n ON n.oid = t.relnamespace
 WHERE n.nspname = '$Schema' AND t.relname = '$Table' AND c.conname = '$Name';
"@)
    } catch { return $null }
}

function Test-ConstraintMutation {
    param(
        [string] $Name,
        [string] $Schema,
        [string] $Table,
        [string] $Constraint,
        [string] $MutateSql,
        [string] $ExpectPattern,
        [switch] $Verbose
    )
    $qualified = "$Schema.$Table"
    $definition = Get-ConstraintDef $Schema $Table $Constraint
    Write-Output ''
    Write-Output "=== $Name ==="
    Write-Output "  target:   $qualified.$Constraint"
    Write-Output "  original: $(if ($null -eq $definition) { '<did not exist>' } else { $definition })"
    try {
        $applyError = Invoke-Mutation $MutateSql
        if ($null -ne $applyError) { Write-NotApplied $Name $qualified $applyError; return }
        $gate = Invoke-Gate
        if ($Verbose) {
            ($gate.Text -split "`r?`n") | Where-Object { $_.Trim() } | ForEach-Object { Write-Output "  |$_" }
        } else {
            ($gate.Text -split "`r?`n") | Where-Object { $_ -match 'MISSING_CONSTRAINT|CONSTRAINT_TYPE_DRIFT|CONSTRAINT_NOT_VALIDATED|PARSE_INCOMPLETE|compared_constraints|drift=|^\s{2}\S' } |
                ForEach-Object { Write-Output "  |$_" }
        }
        $detected = ($gate.Text -match $ExpectPattern) -and $gate.Exit -eq 1
        if ($detected) {
            Write-Output "  RESULT: DETECTED (exit=$($gate.Exit))"
        } else {
            Write-Output "  RESULT: *** NOT DETECTED *** (exit=$($gate.Exit)) expected to match /$ExpectPattern/"
        }
        $script:results += [pscustomobject]@{ Mutation = $Name; Target = "$qualified.$Constraint"; Detected = $detected }
    } finally {
        # Same unconditional, individually-guarded restore discipline as the index mutations,
        # for the same reason: a throw inside a finally aborts the statements after it, which
        # during index development left the schema mutated twice.
        $note = $null
        try { [void] (Invoke-Sql "ALTER TABLE $qualified DROP CONSTRAINT IF EXISTS $Constraint;") } catch { $note = "pre-drop failed: $_" }
        if (-not $note -and $null -ne $definition) {
            try { [void] (Invoke-Sql "ALTER TABLE $qualified ADD CONSTRAINT $Constraint $definition;") } catch { $note = "re-add failed: $_" }
        }

        $after = $null
        try { $after = Get-ConstraintDef $Schema $Table $Constraint } catch { $note = "post-check failed: $_" }

        if ($note) {
            Write-Output "  RESTORE ERROR for $qualified.$Constraint -- $note"
            Write-Output "    ORIGINAL DEFINITION (re-apply by hand): $definition"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = "$qualified.$Constraint"; Detected = $false }
        } elseif ($null -eq $definition -and $null -eq $after) {
            Write-Output '  restored OK (absent before, absent after -- as required)'
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = "$qualified.$Constraint"; Detected = $true }
        } elseif ($null -eq $definition) {
            Write-Output "  RESTORE FAILED for $qualified.$Constraint -- did not exist beforehand but now does: $after"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = "$qualified.$Constraint"; Detected = $false }
        } elseif ($after -ne $definition) {
            Write-Output "  RESTORE FAILED for $qualified.$Constraint -- definition is not identical"
            Write-Output "    expected: $definition"
            Write-Output "    actual:   $after"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = "$qualified.$Constraint"; Detected = $false }
        } else {
            Write-Output '  restored OK (definition identical)'
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = "$qualified.$Constraint"; Detected = $true }
        }
    }
}

# Capture a trigger's authoritative live definition.
#
# A trigger is NOT a regclass, so 'schema.table.trigger'::regclass -- the shape Get-IndexDef uses
# -- is a syntax-level relation lookup and always fails. Measured: the first draft of this helper
# returned nothing for every target, so the harness believed each trigger "did not exist" and
# restored nothing. The first run of the trigger dimension then left the live database one trigger
# short, which the gate immediately reported as MISSING_TRIGGER -- the detector working correctly
# on a schema the harness had broken.
#
# pg_get_triggerdef(t.oid, true) emits a standalone CREATE TRIGGER ... statement, so the captured
# text is itself the exact restore statement. The enabled state is appended so a restore is
# byte-comparable rather than merely "the trigger exists again".
function Get-TriggerDef {
    param([string] $Schema, [string] $Table, [string] $Trigger)
    try {
        return (Invoke-Sql @"
SELECT pg_get_triggerdef(t.oid, true) || ' /* tgenabled=' || t.tgenabled::text || ' */'
  FROM pg_trigger t
  JOIN pg_class c ON c.oid = t.tgrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = '$Schema' AND c.relname = '$Table' AND t.tgname = '$Trigger';
"@)
    } catch { return $null }
}

# Split a captured definition back into a statement and its enabled marker. The restore path
# needs the bare statement to feed back to psql; the comparison wants the full captured text.
function Split-TriggerDef {
    param([string] $Definition)
    if ([string]::IsNullOrEmpty($Definition)) { return $null }
    return ($Definition -replace '\s*/\* tgenabled=([A-Z])\s*\*/\s*$', '').Trim()
}

function Test-TriggerMutation {
    param(
        [string] $Name,
        [string] $Schema,
        [string] $Table,
        [string] $Trigger,
        [string] $MutateSql,
        [string] $RestoreSql,
        [string] $ExpectPattern,
        [switch] $SkipPreDrop,
        [switch] $Verbose
    )
    $target   = "$Schema.$Table.$Trigger"
    $dropSql  = "DROP TRIGGER IF EXISTS $Trigger ON `"$Schema`".`"$Table`";"
    $definition = Get-TriggerDef $Schema $Table $Trigger
    Write-Output ''
    Write-Output "=== $Name ==="
    Write-Output "  target:   $target"
    Write-Output "  original: $(if ([string]::IsNullOrEmpty($definition)) { '<did not exist>' } else { $definition })"
    try {
        # A mutation that fails to apply proves nothing about the detector, and -- worse -- an
        # exception thrown here would propagate out of Test-TriggerMutation after the finally had
        # already run, aborting the whole harness and leaving every later dimension unrun.
        # Measured: the first trigger-dimension run aborted exactly this way, part-way through T2.
        $applyError = Invoke-Mutation $MutateSql
        if ($null -ne $applyError) { Write-NotApplied $Name $target $applyError; return }
        $gate = Invoke-Gate
        if ($Verbose) {
            ($gate.Text -split "`r?`n") | Where-Object { $_.Trim() } | ForEach-Object { Write-Output "  |$_" }
        } else {
            ($gate.Text -split "`r?`n") | Where-Object { $_ -match 'MISSING_TRIGGER|TRIGGER_DRIFT|TRIGGER_DISABLED|UNDECLARED_TRIGGER|PARSE_INCOMPLETE|compared_triggers|drift=|^\s{2}\S' } |
                ForEach-Object { Write-Output "  |$_" }
        }
        $detected = ($gate.Text -match $ExpectPattern) -and $gate.Exit -eq 1
        if ($detected) {
            Write-Output "  RESULT: DETECTED (exit=$($gate.Exit))"
        } else {
            Write-Output "  RESULT: *** NOT DETECTED *** (exit=$($gate.Exit)) expected to match /$ExpectPattern/"
        }
        $script:results += [pscustomobject]@{ Mutation = $Name; Target = $target; Detected = $detected }
    } finally {
        # Identical restore discipline to the index mutations: unconditional, individually guarded,
        # and verified byte-identical afterwards. A trigger that is dropped and silently not
        # recreated would be a permanent schema change that no later run of this harness would
        # notice, since every subsequent mutation is expressed relative to the live database.
        #
        # The pre-drop uses the real PostgreSQL shape -- DROP TRIGGER <name> ON <table> -- and NOT
        # the qualified "schema.table.trigger" form, which is a syntax error. Measured: the first
        # run aborted on that syntax error, and because a throw inside a finally stops the
        # statements after it, the re-create never ran either.
        $note = $null
        if (-not $SkipPreDrop) {
            try { [void] (Invoke-Sql $dropSql) } catch { $note = "pre-drop failed: $_" }
        }
        if (-not $note -and -not [string]::IsNullOrEmpty($RestoreSql)) {
            try { [void] (Invoke-Sql $RestoreSql) } catch { $note = "re-create failed: $_" }
        }

        $after = $null
        try { $after = Get-TriggerDef $Schema $Table $Trigger } catch { $note = "post-check failed: $_" }

        if ($note) {
            Write-Output "  RESTORE ERROR for $target -- $note"
            Write-Output "    ORIGINAL DEFINITION (re-apply by hand): $definition"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $target; Detected = $false }
        } elseif ([string]::IsNullOrEmpty($definition) -and [string]::IsNullOrEmpty($after)) {
            Write-Output '  restored OK (absent before, absent after -- as required)'
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $target; Detected = $true }
        } elseif ([string]::IsNullOrEmpty($definition)) {
            Write-Output "  RESTORE FAILED for $target -- did not exist beforehand but now does: $after"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $target; Detected = $false }
        } elseif ($after -ne $definition) {
            Write-Output "  RESTORE FAILED for $target -- definition is not identical"
            Write-Output "    expected: $definition"
            Write-Output "    actual:   $after"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $target; Detected = $false }
        } else {
            Write-Output '  restored OK (definition identical)'
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $target; Detected = $true }
        }
    }
}

# =========================================================================================
# CONSTRAINT DIMENSION MUTATION PROOFS
#
# Same argument as the index dimension: the constraint section reports drift=0 on a healthy
# database, which proves nothing until something is fired at it. C1-C3 cover each finding it can
# emit, C4 proves it fails closed rather than under-reporting, and C5 is a NEGATIVE CONTROL --
# without it, C3 would be satisfied by a gate that flags every unvalidated constraint, including
# the one migration 0023 declares deliberately.
# =========================================================================================
Write-Output ''
Write-Output '### CONSTRAINT DIMENSION MUTATION PROOFS'

# C1: a declared named CHECK dropped outright.
Test-ConstraintMutation -Name 'C1 constraint dropped -> MISSING_CONSTRAINT' `
    -Schema 'ops' -Table 'halt' -Constraint 'halt_id_format' `
    -MutateSql  'ALTER TABLE ops.halt DROP CONSTRAINT halt_id_format;' `
    -ExpectPattern 'MISSING_CONSTRAINT[\s\S]*halt_id_format'

# C2: same name and same table, but a different constraint TYPE. order_id is the primary key,
# so the replacement UNIQUE is valid SQL and cannot fail on existing data -- a mutation that
# errored out would prove nothing about the detector.
Test-ConstraintMutation -Name 'C2 type changed -> CONSTRAINT_TYPE_DRIFT' `
    -Schema 'oms' -Table 'order' -Constraint 'order_id_format' `
    -MutateSql  'ALTER TABLE oms.order DROP CONSTRAINT order_id_format; ALTER TABLE oms.order ADD CONSTRAINT order_id_format UNIQUE (order_id);' `
    -ExpectPattern 'CONSTRAINT_TYPE_DRIFT[\s\S]*order_id_format'

# C3: the dangerous one. The constraint still exists, still has the declared name and type, and
# still passes every structural check -- but it is NOT VALID, so it is not enforced against rows
# that already exist. A declaration-only review cannot see this.
Test-ConstraintMutation -Name 'C3 silently unvalidated -> CONSTRAINT_NOT_VALIDATED' `
    -Schema 'oms' -Table 'order' -Constraint 'order_command_format' `
    -MutateSql  'ALTER TABLE oms.order DROP CONSTRAINT order_command_format; ALTER TABLE oms.order ADD CONSTRAINT order_command_format CHECK (order_id IS NOT NULL) NOT VALID;' `
    -ExpectPattern 'CONSTRAINT_NOT_VALIDATED[\s\S]*order_command_format'

# C4: PARSE_INCOMPLETE for the constraint section, proven in an isolated sandbox rather than by
# corrupting the repository. A named constraint that appears before any CREATE TABLE or ALTER
# TABLE cannot be attributed to a table; the gate must refuse to issue a verdict instead of
# quietly comparing fewer constraints than the migrations declare.
Write-Output ''
Write-Output '=== C4 unattributable constraint -> PARSE_INCOMPLETE (fail closed) ==='
$c4sandbox = Join-Path ([System.IO.Path]::GetTempPath()) ('schema-gate-constraint-probe-' + [Guid]::NewGuid().ToString('N'))
try {
    $c4db   = Join-Path $c4sandbox 'db'
    $c4migr = Join-Path $c4db 'migrations'
    [void] (New-Item -ItemType Directory -Path $c4migr -Force)
    Copy-Item $GatePath (Join-Path $c4db 'verify-schema-live.ps1') -Force

    # A valid function so the function section passes and execution REACHES the constraint
    # section. It is absent from the live database, which is fine: missing functions are
    # reported at the end and do not short-circuit the later sections.
    @'
CREATE OR REPLACE FUNCTION common.sandbox_ok() RETURNS boolean AS $$
SELECT true;
$$ LANGUAGE sql;
'@ | Set-Content -LiteralPath (Join-Path $c4migr '0001_base.sql') -Encoding UTF8

    # The bare CONSTRAINT below names no table: there is no CREATE TABLE or ALTER TABLE before
    # it anywhere in this file, and the previous file declares no table either.
    @'
CONSTRAINT sandbox_unattributable CHECK (true);
'@ | Set-Content -LiteralPath (Join-Path $c4migr '0002_unattributable.sql') -Encoding UTF8

    $c4 = Invoke-Script -Path (Join-Path $c4db 'verify-schema-live.ps1')
    ($c4.Text -split "`r?`n") | Where-Object { $_ -match 'PARSE_INCOMPLETE|compared_' } | ForEach-Object { Write-Output "  |$_" }
    $c4ok = ($c4.Text -match 'PARSE_INCOMPLETE[\s\S]*constraint') -and $c4.Exit -eq 1
    if ($c4ok) {
        Write-Output '  RESULT: DETECTED (exit=1)'
    } else {
        Write-Output "  RESULT: *** NOT DETECTED *** (exit=$($c4.Exit))"
    }
    $script:results += [pscustomobject]@{ Mutation = 'C4 unattributable constraint -> PARSE_INCOMPLETE'; Target = 'sandbox'; Detected = $c4ok }
} finally {
    if (Test-Path -LiteralPath $c4sandbox) { Remove-Item -LiteralPath $c4sandbox -Recurse -Force }
}

# C5: NEGATIVE CONTROL. audit.record.audit_signing_key_required is genuinely NOT VALID in the
# live database, and migration 0023 declares it so (1294 pre-existing audit rows violate it and
# no signing key can honestly be attributed to records written without one). The gate must
# therefore NOT report it. Without this assertion C3 would also pass against a gate that simply
# flags every unvalidated constraint -- i.e. C3 alone does not establish the declared-vs-live
# comparison the constraint section claims to perform.
Write-Output ''
Write-Output '=== C5 negative control -> declared NOT VALID must NOT be reported ==='
# Cast to int deliberately. Selecting the raw boolean returns psql's DISPLAY form, which is 't'/'f',
# not the literal 'true'/'false' and not '0'/'1' -- an earlier draft of this assertion compared
# against '0' and failed while the gate had in fact behaved correctly. This is the second time
# this column's two output forms have produced a false result in this work; both times the bug was
# in the check, never in the gate.
$c5live = Invoke-Sql "SELECT c.convalidated::int FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname='audit' AND t.relname='record' AND c.conname='audit_signing_key_required';"
Write-Output "  live convalidated = $c5live  (expect 0, i.e. genuinely unvalidated)"
$c5gate = Invoke-Gate
$c5ok = ($c5live.Trim() -eq '0') -and ($c5gate.Exit -eq 0) -and ($c5gate.Text -notmatch 'CONSTRAINT_NOT_VALIDATED')
if ($c5ok) {
    Write-Output '  RESULT: DETECTED (gate green, and the deliberate NOT VALID is not reported)'
} else {
    Write-Output "  RESULT: *** FAILED *** (live convalidated=$c5live exit=$($c5gate.Exit) reported=$($c5gate.Text -match 'CONSTRAINT_NOT_VALIDATED'))"
}
$script:results += [pscustomobject]@{ Mutation = 'C5 negative control -> declared NOT VALID not reported'; Target = 'audit.record.audit_signing_key_required'; Detected = $c5ok }

# =========================================================================================
# TRIGGER DIMENSION MUTATION PROOFS
#
# Added with the ordered trigger walk. The trigger section had reported drift=0 on every run
# since it was written and had never been fired at, so "it is green" was not evidence that it
# detects anything -- and the ordering fix it received is exactly the kind of change that can
# turn a detector into a no-op. T1-T4 cover each finding the trigger section can emit; T5 is the
# NEGATIVE CONTROL for the fix itself.
# =========================================================================================
Write-Output ''
Write-Output '### TRIGGER DIMENSION MUTATION PROOFS'

$t1schema = 'ops'; $t1table = 'outbox'; $t1trigger = 'ops_outbox_delete_preserves_submitting_order'
$t1def = Split-TriggerDef (Get-TriggerDef $t1schema $t1table $t1trigger)

# T1: a declared trigger dropped outright. This is the finding the ordering bug produced
# spuriously in reverse, so it is the case that matters most: a parser that has become too
# eager to register declarations would also fail to notice a genuinely absent trigger.
Test-TriggerMutation -Name 'T1 trigger dropped -> MISSING_TRIGGER' `
    -Schema $t1schema -Table $t1table -Trigger $t1trigger `
    -MutateSql  "DROP TRIGGER $t1trigger ON ops.outbox;" `
    -RestoreSql $t1def `
    -ExpectPattern 'MISSING_TRIGGER[\s\S]*ops_outbox_delete_preserves_submitting_order'

# T2: same name, same table, wrong function. The trigger still fires, so nothing errors at
# runtime; the only evidence that the guard was swapped is the repository comparison. The
# replacement keeps the original's event so the drop/create is valid SQL.
Test-TriggerMutation -Name 'T2 wrong function -> TRIGGER_DRIFT' `
    -Schema 'ops' -Table 'outbox' -Trigger 'outbox_principal_guard' `
    -MutateSql  'DROP TRIGGER outbox_principal_guard ON ops.outbox; CREATE TRIGGER outbox_principal_guard BEFORE INSERT OR UPDATE ON ops.outbox FOR EACH ROW EXECUTE FUNCTION ops.guard_outbox_delete_preserves_submitting_order();' `
    -RestoreSql (Split-TriggerDef (Get-TriggerDef 'ops' 'outbox' 'outbox_principal_guard')) `
    -ExpectPattern 'TRIGGER_DRIFT[\s\S]*outbox_principal_guard'

# T3: enabled -> disabled. A disabled guard raises no error and writes no audit row; the write
# it was there to block simply succeeds. This is the failure mode a "does it still exist" check
# cannot see, which is why the gate compares tgenabled.
Test-TriggerMutation -Name 'T3 disabled trigger -> TRIGGER_DISABLED' `
    -Schema 'oms' -Table 'order' -Trigger 'oms_order_entry_state' `
    -MutateSql  'ALTER TABLE oms."order" DISABLE TRIGGER oms_order_entry_state;' `
    -RestoreSql 'ALTER TABLE oms."order" ENABLE TRIGGER oms_order_entry_state;' -SkipPreDrop `
    -ExpectPattern 'TRIGGER_DISABLED[\s\S]*oms_order_entry_state'

# T4: a trigger added live that no migration declares. The mirror image of T1: the gate must
# notice objects the repository does not account for, not only objects it expects and cannot find.
Test-TriggerMutation -Name 'T4 undeclared trigger -> UNDECLARED_TRIGGER' `
    -Schema 'ops' -Table 'outbox' -Trigger 'zz_probe_trigger' `
    -MutateSql  'CREATE TRIGGER zz_probe_trigger BEFORE INSERT ON ops.outbox FOR EACH ROW EXECUTE FUNCTION ops.guard_principal_on_write();' `
    -RestoreSql 'DROP TRIGGER IF EXISTS zz_probe_trigger ON ops.outbox;' -SkipPreDrop `
    -ExpectPattern 'UNDECLARED_TRIGGER[\s\S]*zz_probe_trigger'

# T5: NEGATIVE CONTROL for the ordered walk. Migration 0026 drops and recreates this very
# trigger inside one file so that its no-transaction migration is re-runnable. The repository
# therefore both declares and drops the name, and the live database holds the replacement.
#
# The previous parser grouped its per-file scan into "all CREATEs, then all DROPs", so the
# trailing DROP erased the declaration the file had made moments earlier and the gate reported
# a trigger as missing from a database that had it. Asserting only T1-T4 would have passed
# against that broken parser too, because those cases mutate the live database and say nothing
# about how the files are read. This case is the only one that pins the reading of the files.
$t5gate = Invoke-Gate
$t5ok = ($t5gate.Text -notmatch $t1trigger) -and ($t5gate.Exit -eq 0) `
    -and ($t5gate.Text -match 'compared_triggers=(\d+)') `
    -and ([int] $Matches[1] -gt 0)
Write-Output ''
Write-Output '=== T5 negative control -> drop-then-recreate in one migration is not reported ==='
Write-Output "  target:   ops.outbox.$t1trigger (declared and dropped by 0026 in the same file)"
Write-Output "  compared_triggers reported: $(if ($t5gate.Text -match 'compared_triggers=(\d+)') { $Matches[1] } else { '<none>' })"
if ($t5ok) {
    Write-Output '  RESULT: DETECTED (gate green, and the drop-then-recreate is correctly not reported)'
} else {
    Write-Output "  RESULT: *** FAILED *** (exit=$($t5gate.Exit) reported=$($t5gate.Text -match $t1trigger))"
}
$script:results += [pscustomobject]@{ Mutation = 'T5 negative control -> drop-then-recreate in one migration not reported'; Target = "ops.outbox.$t1trigger"; Detected = $t5ok }

# =========================================================================================
# FUNCTION DIMENSION MUTATION PROOFS
#
# Added 2026-10-03. This was the gate's FIRST dimension and it is its LARGEST -- 64 function
# bodies compared against pg_proc.prosrc -- and it was the only dimension with no case in this
# file at all. The harness's closing line said "index, constraint and trigger", which was honest
# but incomplete, so a regression in the largest comparison could have been entirely silent.
#
# That gap mattered more here than for the other three, because the function parser is the most
# complex in the gate and has already failed silently, twice:
#
#   1. Its header pattern used [^$]*? between the function name and the body delimiter, so any
#      function whose SIGNATURE contained a dollar sign was not matched at all --
#      config.assert_no_embedded_secrets(p_document JSONB, p_path TEXT DEFAULT '$') is exactly
#      such a function. It was dropped from the comparison with no warning: the gate printed
#      expected_functions=62 while the files declared 63, and reported drift=0 while that
#      function was live holding a corrupted body. F2 fires at that exact function, so a
#      regression of that specific bug can no longer pass unnoticed.
#
#   2. It matched only CREATE OR REPLACE, so migration 0026's plain CREATE FUNCTION was reported
#      as UNDECLARED -- a false accusation against a correct database.
#
# WHY THE MUTATION INSERTS A COMMENT RATHER THAN REPLACING THE BODY. prosrc stores in-body
# comments verbatim and the gate compares body text, so a comment changes prosrc and must be
# reported while leaving the function's behaviour completely unchanged. An earlier proof of this
# dimension replaced a guard body with a bare RETURN NULL, which proves the same byte-sensitivity
# but with real blast radius if a restore ever fails -- on a financial guard. Inserting a comment
# proves detection of exactly the value the gate compares, and cannot leave a guard disabled even
# if restoration were skipped entirely.
#
# The mutation is built from the CAPTURED pg_get_functiondef rather than a hand-written
# CREATE OR REPLACE, so it cannot be wrong about argument names, defaults or the return type.
# Reconstructing a signature by hand is how an earlier draft of this harness came to mutate a
# trigger name that does not exist.
# =========================================================================================
Write-Output ''
Write-Output '### FUNCTION DIMENSION MUTATION PROOFS'

# A FUNCTION BODY CANNOT TRAVEL THIS WAY, and both obvious alternatives were measured failing on
# this host before a third was adopted.
#
# Invoke-Sql above passes SQL to psql with -c as a native argument. That mangles a multi-line body:
# newlines collapse, so an inserted "-- comment" swallows the rest of the body and the dollar-quoted
# string is left unterminated. The first attempt at this dimension failed exactly that way, and every
# case correctly reported NOT APPLIED rather than a false pass.
#
# Piping to `psql -f -` is no better. PowerShell's native-stdin path is text-mode, so every embedded
# LF was rewritten to CRLF: a restored prosrc came back 2627 characters against an original 2627 --
# the same length, different bytes, which is worse than an obvious mismatch because it reads like
# success. migrate.ps1 also pipes, and its encoding fix addresses character encoding rather than the
# text-mode conversion, so it does not make this route safe for a body.
#
# Writing the bytes to a file and letting psql read it with -f removes the pipe from the path, so
# the server stores exactly what migrate.ps1 reads from disk. Measured round-trip after the change:
# both function bodies re-applied from their own pg_get_functiondef came back byte-identical,
# including the one containing a section sign, which is the body that exposed the CRLF problem.
#
# Scoped to this dimension deliberately. Invoke-Sql remains in use for the index, constraint and
# trigger mutations, whose SQL is single-line and demonstrably unaffected by either hazard; changing
# the proven path to accommodate a new one would trade a known-good mechanism for a fresh risk.
#
# NEVER reintroduce Out-String in this file. It was used to format psql's result and it wraps long
# single-line values at the console width, measured at 60 on this host, inserting newlines into the
# middle of the value it is supposed to be passing through. It silently corrupted this harness twice,
# in two different ways that looked like unrelated bugs:
#   - a 1802-character hex string came back as 1832 characters, so a correctly restored 901-byte body
#     was reported as 916 bytes and judged unequal to its own original (F1/F2 restore failures); and
#   - it formatted the GATE's text in Invoke-Script, splitting the long PARSE_INCOMPLETE sentence
#     across lines so that no single-line pattern could ever match it (F4), while the cases whose
#     expected patterns were short kept passing and hid the cause.
# Every wrapper here joins psql's lines with "`n" instead, which reassembles multi-line output
# exactly and leaves single-line values untouched.
$script:sqlSeq = 0
function Invoke-SqlBytes {
    param([string] $Sql)
    $script:sqlSeq++
    $name = "harness_sql_$PID`_$($script:sqlSeq).sql"
    $hostPath = Join-Path ([System.IO.Path]::GetTempPath()) $name
    $ctrPath = "/tmp/$name"
    # BOM-less UTF-8, for the reason migrate.ps1 records: a BOM corrupts the first statement.
    [System.IO.File]::WriteAllText($hostPath, $Sql, (New-Object System.Text.UTF8Encoding($false)))
    $prevEA = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & docker cp $hostPath "$($Container):$ctrPath" 2>&1 | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "docker cp failed (exit $LASTEXITCODE)" }
        # ON_ERROR_STOP=1 is essential here and was nearly omitted: psql reading a script continues
        # past a failed statement and would exit 0, so a mutation that did not apply would be
        # indistinguishable from one that did.
        $out = & docker exec $Container psql -U $User -d $Database -tAq -v ON_ERROR_STOP=1 -f $ctrPath 2>&1
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prevEA
        & docker exec $Container rm -f $ctrPath 2>&1 | Out-Null
        Remove-Item -LiteralPath $hostPath -Force -ErrorAction SilentlyContinue
    }
    if ($code -ne 0) {
        $text = ($out | ForEach-Object { if ($_ -is [string]) { $_ } else { [string]$_ } }) -join ' '
        throw "psql failed (exit $code): $text"
    }
    return (($out | Where-Object { $_ -is [string] -and $_ -notmatch '^(NOTICE|WARNING):' }) -join "`n").Trim()
}

function Invoke-MutationBytes {
    param([string] $Sql)
    try { [void] (Invoke-SqlBytes $Sql); return $null }
    catch { return $_.Exception.Message }
}

function Get-FunctionDef {
    param([string] $Qualified)
    try { return (Invoke-SqlBytes "SELECT pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname || '.' || p.proname = '$Qualified' ORDER BY p.oid LIMIT 1;") }
    catch { return $null }
}

# NOTE: there is deliberately no 'SELECT p.prosrc' reader here. It reads through Invoke-SqlBytes,
# which ends in .Trim(), so it silently drops the newline that prosrc begins and ends with, and it
# returns a value that looks like the body while not being the bytes the gate compares. Use
# Get-FunctionBodyExact below for anything that must reproduce stored bytes.

# The authoritative comparison, and it exists because a trimmed string comparison proved worthless
# here. Invoke-SqlBytes ends in .Trim(), so a body that had lost or gained leading or trailing
# whitespace compared EQUAL while the stored bytes differed -- and the gate, which is the real
# authority, correctly reported drift on a database the harness had just called restored. Hex has
# no normalisation to hide behind, so a restore is verified only if these two match exactly.
function Get-FunctionBodyHex {
    param([string] $Qualified)
    try { return (Invoke-SqlBytes "SELECT encode(convert_to(p.prosrc,'UTF8'),'hex') FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname || '.' || p.proname = '$Qualified' ORDER BY p.oid LIMIT 1;") }
    catch { return $null }
}

# The exact body, for building a statement that must reproduce the stored bytes.
#
# Two separate defects made 'SELECT p.prosrc' unusable for this, and both had to be removed
# before a restore could be byte-exact:
#
#   1. Out-String, used to format the result, wraps long single-line values at the console
#      width -- measured at 60 on this host. A 1802-character hex string came back as 1832
#      characters with 30 newlines injected into it, so a body that had been restored to the
#      correct 901 bytes was reported as 916 and judged unequal to its own original.
#   2. .Trim(), which Invoke-SqlBytes ends with, removes leading and trailing whitespace from
#      prosrc. This body's real prosrc begins and ends with a newline (903 bytes), so the
#      trimmed read returned 901 and a restore built from it silently dropped two bytes.
#
# Hex has neither problem: it contains no whitespace for a wrapper to normalise, no line for
# a wrapper to break, and decoding it restores the leading and trailing newlines exactly.
function ConvertFrom-HexText {
    param([string] $Hex)
    $n = [int]($Hex.Length / 2)
    $bytes = New-Object byte[] $n
    for ($i = 0; $i -lt $n; $i++) { $bytes[$i] = [Convert]::ToByte($Hex.Substring($i * 2, 2), 16) }
    return [System.Text.Encoding]::UTF8.GetString($bytes)
}

function Get-FunctionBodyExact {
    param([string] $Qualified)
    $hex = Get-FunctionBodyHex $Qualified
    if ([string]::IsNullOrEmpty($hex)) { return $null }
    return (ConvertFrom-HexText $hex)
}

# Rebuild a function definition with its body replaced by an exact body string.
#
# This is what makes restoration correct by construction rather than by luck. pg_get_functiondef
# renders the body as PostgreSQL chooses to render it, so "re-apply the captured definition and hope
# prosrc returns unchanged" is an assumption, and on this host it was measured to be false for one
# function before the transport was fixed. Splicing the ORIGINAL prosrc back between the dollar-quote
# tags cannot be wrong: the bytes the gate compares are the bytes being written.
function Get-FunctionDefWithBody {
    param([string] $Definition, [string] $Body)
    $asIdx = $Definition.IndexOf('AS $')
    if ($asIdx -lt 0) { return $null }
    $tagStart = $asIdx + 4
    $tagEnd = $Definition.IndexOf('$', $tagStart)
    if ($tagEnd -lt 0) { return $null }
    $tag = $Definition.Substring($tagStart, $tagEnd - $tagStart)
    $openSeq = 'AS $' + $tag + '$'
    $closeSeq = '$' + $tag + '$'
    $openIdx = $Definition.IndexOf($openSeq)
    $closeIdx = $Definition.LastIndexOf($closeSeq)
    if ($openIdx -lt 0 -or $closeIdx -le $openIdx) { return $null }
    $head = $Definition.Substring(0, $openIdx + $openSeq.Length)
    $tail = $Definition.Substring($closeIdx)
    return $head + $Body + $tail
}

function Test-FunctionMutation {
    param(
        [string] $Name,
        [string] $Qualified,
        [string] $ExpectPattern,
        [switch] $Verbose
    )
    $definition = Get-FunctionDef $Qualified
    $hexBefore  = Get-FunctionBodyHex $Qualified
    # The exact body, NOT the trimmed 'SELECT p.prosrc' read: this function's stored prosrc
    # begins and ends with a newline, and a trimmed read would restore 901 bytes instead of 903.
    $bodyBefore = Get-FunctionBodyExact $Qualified
    Write-Output ''
    Write-Output "=== $Name ==="
    Write-Output "  target:   $Qualified"
    Write-Output "  prosrc bytes before: $(if ($null -eq $hexBefore) { '<absent>' } else { [int]($hexBefore.Length / 2) })"

    # The probe comment goes INSIDE the body, so it lands in prosrc, which is the value the gate
    # compares. Behaviour is unchanged: if restoration were skipped entirely, the guard still
    # enforces exactly what it enforced before.
    $probeBody = $bodyBefore + "`n-- HARNESS PROBE: prosrc altered, behaviour unchanged"
    $mutate = if ($null -eq $definition) { $null } else { Get-FunctionDefWithBody $definition $probeBody }
    $restore = if ($null -eq $definition) { $null } else { Get-FunctionDefWithBody $definition $bodyBefore }
    if ($null -eq $mutate -or $null -eq $restore) {
        Write-NotApplied $Name $Qualified 'could not build a mutation from the captured definition'
        return
    }
    try {
        # Invoke-MutationBytes, NOT Invoke-Mutation: $mutate is a multi-line function definition, and
        # the -c transport used by Invoke-Mutation collapses its newlines, which is exactly how the
        # first attempt at this dimension failed with an unterminated dollar-quoted string.
        $applyError = Invoke-MutationBytes $mutate
        if ($null -ne $applyError) { Write-NotApplied $Name $Qualified $applyError; return }

        # Prove the mutation LANDED before believing any result from the gate. This rule has been
        # learned four times in this harness: a mutation that does not apply is indistinguishable
        # from a mutation the detector does not catch. Compared as hex, because a trimmed string
        # comparison reported two lossy restores as byte-identical.
        $hexDuring = Get-FunctionBodyHex $Qualified
        if ($hexDuring -eq $hexBefore) {
            Write-NotApplied $Name $Qualified 'prosrc is unchanged after the CREATE OR REPLACE, so the mutation did not land'
            return
        }

        $gate = Invoke-Gate
        if ($Verbose) {
            ($gate.Text -split "`r?`n") | Where-Object { $_.Trim() } | ForEach-Object { Write-Output "  |$_" }
        } else {
            ($gate.Text -split "`r?`n") | Where-Object { $_ -match 'LIVE_DRIFT|UNDECLARED_IN_DATABASE|PARSE_INCOMPLETE|compared_functions|drift=|^\s{2}\S' } |
                ForEach-Object { Write-Output "  |$_" }
        }
        $detected = ($gate.Text -match $ExpectPattern) -and $gate.Exit -eq 1
        if ($detected) {
            Write-Output "  RESULT: DETECTED (exit=$($gate.Exit))"
        } else {
            Write-Output "  RESULT: *** NOT DETECTED *** (exit=$($gate.Exit)) expected to match /$ExpectPattern/"
        }
        $script:results += [pscustomobject]@{ Mutation = $Name; Target = $Qualified; Detected = $detected }
    } finally {
        # Restore by splicing the ORIGINAL prosrc back between the tags, then verify prosrc is
        # byte-identical to what was captured. Verifying is the only thing that separates a
        # completed run from one that quietly left a financial guard altered.
        $note = $null
        try { [void] (Invoke-SqlBytes $restore) } catch { $note = "re-apply failed: $_" }
        $hexAfter = $null
        try { $hexAfter = Get-FunctionBodyHex $Qualified } catch { $note = "post-check failed: $_" }

        if ($note) {
            Write-Output "  RESTORE ERROR for $Qualified -- $note"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $Qualified; Detected = $false }
        } elseif ([string]::IsNullOrEmpty($hexAfter)) {
            # $hexBefore cannot be null or empty on this path -- the caller returns early when the
            # definition is absent -- so an empty read here means the function could not be read
            # back at all. Written out explicitly because '$null -eq $null' is true in PowerShell,
            # which would let a pair of failed reads compare EQUAL and report a restore that never
            # happened. Test-Mutation already had this branch; Test-FunctionMutation did not.
            Write-Output "  RESTORE FAILED for $Qualified -- prosrc could not be read back after the restore"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $Qualified; Detected = $false }
        } elseif ($hexAfter -eq $hexBefore) {
            Write-Output '  restored OK (prosrc byte-identical, compared as hex)'
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $Qualified; Detected = $true }
        } else {
            $b = if ($null -eq $hexBefore) { '?' } else { [int]($hexBefore.Length / 2) }
            $a = if ($null -eq $hexAfter)  { '?' } else { [int]($hexAfter.Length / 2) }
            Write-Output "  RESTORE FAILED for $Qualified -- prosrc bytes differ from the captured original ($b vs $a)"
            $script:results += [pscustomobject]@{ Mutation = "$Name [restore]"; Target = $Qualified; Detected = $false }
        }
    }
}

# F1: the basic case -- a body that differs from the repository must be reported as LIVE_DRIFT.
Test-FunctionMutation -Name 'F1 function body altered -> LIVE_DRIFT' `
    -Qualified 'market.guard_feed_sequence_monotonic' `
    -ExpectPattern 'LIVE_DRIFT[\s\S]*guard_feed_sequence_monotonic'

# F2: THE CASE THAT EARNS THIS DIMENSION. config.assert_no_embedded_secrets carries a dollar
# sign inside its SIGNATURE (p_path TEXT DEFAULT '$'). The gate's original header pattern used
# [^$]*? between the function name and the body delimiter, so it silently declined to match any
# such function: the count it printed was two lower than the files declared, no warning was
# emitted, and the gate reported drift=0 while this very function held a corrupted body. Only
# firing a mutation AT this function distinguishes a parser that compares it from one that skips
# it -- F1 would pass against the broken parser too, because F1's target has a plain signature.
Test-FunctionMutation -Name 'F2 function with a dollar sign in its signature is compared, not skipped' `
    -Qualified 'config.assert_no_embedded_secrets' `
    -ExpectPattern 'LIVE_DRIFT[\s\S]*assert_no_embedded_secrets'

# F3: the reverse direction -- an object added live that no migration declares. The index,
# constraint and trigger dimensions each have this case, and its absence would let the gate
# ignore anything added to the database outside the repository.
$f3name = 'zz_probe_function'
Write-Output ''
Write-Output '=== F3 undeclared function added live -> UNDECLARED_IN_DATABASE ==='
Write-Output "  target:   ops.$f3name"
$f3mutate = @"
CREATE FUNCTION ops.$f3name() RETURNS integer LANGUAGE sql AS `$probe`$ SELECT 1 `$probe`$;
"@
$f3existsBefore = (Invoke-Sql "SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='ops' AND p.proname='$f3name';")
try {
    $f3err = Invoke-MutationBytes $f3mutate
    if ($null -ne $f3err) {
        Write-NotApplied 'F3 undeclared function -> UNDECLARED_IN_DATABASE' "ops.$f3name" $f3err
    } else {
        # Same rule as everywhere else in this file: assert the mutation landed before believing
        # the gate. The index probe originally carried a duplicated ON clause, so the trigger was
        # never created and the case was reported as a pass against a detector that was never fired.
        $f3existsDuring = (Invoke-Sql "SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='ops' AND p.proname='$f3name';")
        if ($f3existsDuring -eq '0') {
            Write-NotApplied 'F3 undeclared function -> UNDECLARED_IN_DATABASE' "ops.$f3name" 'probe_present_in_live=0, so the function was never created'
        } else {
            $f3gate = Invoke-Gate
            ($f3gate.Text -split "`r?`n") | Where-Object { $_ -match 'UNDECLARED_IN_DATABASE|LIVE_DRIFT|drift=|^\s{2}\S' } |
                ForEach-Object { Write-Output "  |$_" }
            $f3ok = ($f3gate.Text -match "UNDECLARED_IN_DATABASE[\s\S]*$f3name") -and ($f3gate.Exit -eq 1)
            if ($f3ok) {
                Write-Output "  RESULT: DETECTED (exit=$($f3gate.Exit))"
            } else {
                Write-Output "  RESULT: *** NOT DETECTED *** (exit=$($f3gate.Exit))"
            }
            $script:results += [pscustomobject]@{ Mutation = 'F3 undeclared function -> UNDECLARED_IN_DATABASE'; Target = "ops.$f3name"; Detected = $f3ok }
        }
    }
} finally {
    $f3note = $null
    try { [void] (Invoke-Sql "DROP FUNCTION IF EXISTS ops.$f3name();") } catch { $f3note = "drop failed: $_" }
    $f3existsAfter = $null
    try { $f3existsAfter = (Invoke-Sql "SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='ops' AND p.proname='$f3name';") } catch { $f3note = "post-check failed: $_" }
    if ($f3note) {
        Write-Output "  RESTORE ERROR for ops.$f3name -- $f3note"
        $script:results += [pscustomobject]@{ Mutation = 'F3 undeclared function [restore]'; Target = "ops.$f3name"; Detected = $false }
    } elseif ($f3existsAfter -eq $f3existsBefore) {
        Write-Output '  restored OK (probe absent again)'
        $script:results += [pscustomobject]@{ Mutation = 'F3 undeclared function [restore]'; Target = "ops.$f3name"; Detected = $true }
    } else {
        Write-Output "  RESTORE FAILED for ops.$f3name -- probe count $f3existsBefore -> $f3existsAfter"
        $script:results += [pscustomobject]@{ Mutation = 'F3 undeclared function [restore]'; Target = "ops.$f3name"; Detected = $false }
    }
}

# F4: fail closed rather than under-report. If a migration declares a CREATE FUNCTION the parser
# cannot read, the gate must refuse to issue a verdict instead of comparing fewer things than the
# repository declares and reporting the difference as agreement. Mirrors M7 and C4, which exist for
# the index and constraint sections for the same reason.
Write-Output ''
Write-Output '=== F4 unparseable function -> PARSE_INCOMPLETE (fail closed) ==='
$f4sandbox = Join-Path ([System.IO.Path]::GetTempPath()) ('schema-gate-func-probe-' + [Guid]::NewGuid().ToString('N'))
try {
    $f4db   = Join-Path $f4sandbox 'db'
    $f4migr = Join-Path $f4db 'migrations'
    [void] (New-Item -ItemType Directory -Path $f4migr -Force)
    Copy-Item $GatePath (Join-Path $f4db 'verify-schema-live.ps1') -Force

    # Two dollar-quoted functions the parser reads, plus one it deliberately cannot. The readable
    # ones are not decoration: without them a parse failure could be satisfied by the gate failing
    # on an empty migration set rather than by the specific under-parse being detected.
    #
    # The unparseable one is given a SINGLE-QUOTED body, not a missing semicolon. That distinction
    # was measured, not guessed. This fixture originally omitted the terminating semicolon on the
    # theory that the gate's statement pattern needed one, and the gate parsed it anyway -- it
    # reported all three functions MISSING_IN_DATABASE and never reached PARSE_INCOMPLETE, so the
    # case reported NOT DETECTED while looking like a detector failure. The gate's function pattern
    # requires a dollar-quoted body, AS $tag$ ... $tag$, and has no semicolon requirement at all,
    # so a single-quoted body is the form that genuinely under-parses. The gate documents this as
    # its intended fail-closed direction: such a function is valid PostgreSQL, is compared by
    # nothing, and must produce a refusal rather than quiet coverage.
    @'
CREATE OR REPLACE FUNCTION common.sandbox_ok() RETURNS boolean AS $$
SELECT true;
$$ LANGUAGE sql;
'@ | Set-Content -LiteralPath (Join-Path $f4migr '0001_base.sql') -Encoding UTF8

    @'
CREATE OR REPLACE FUNCTION common.sandbox_third() RETURNS boolean AS $body$ SELECT true $body$;

-- The CREATE FUNCTION below uses a single-quoted body rather than a dollar-quoted one. That is
-- valid PostgreSQL, and it is deliberately NOT parseable by the gate's function pattern, so the
-- gate compares one fewer function than the files declare. It must refuse and say so instead of
-- reporting coverage it does not have. The bare phrase mention inside this comment must NOT be
-- counted toward declared_by_files, which is what makes declared=3 rather than 4 the assertion.
CREATE OR REPLACE FUNCTION common.sandbox_single_quoted() RETURNS boolean AS 'SELECT true'
'@ | Set-Content -LiteralPath (Join-Path $f4migr '0002_unparseable.sql') -Encoding UTF8

    $f4 = Invoke-Script -Path (Join-Path $f4db 'verify-schema-live.ps1')
    ($f4.Text -split "`r?`n") | Where-Object { $_ -match 'PARSE_INCOMPLETE|declared_by_files|compared_functions' } |
        ForEach-Object { Write-Output "  |$_" }

    # Assert the claim rather than one wording of it: the gate must have declared 3 functions,
    # must have parsed FEWER than it declared, and must have failed closed. The previous form of
    # this check matched one literal count inside one long sentence, which is fragile twice over --
    # it broke when the parsed count was not the one guessed here, and it could never match at all
    # while Invoke-Script formatted the gate's text through Out-String, because that wraps long
    # lines and splits the sentence across lines. Capturing the count also asserts the stronger
    # property that the under-parse was real, instead of only that some number appeared.
    #
    # The count is pinned to exactly 2 rather than asserted as merely 'fewer than 3', and an
    # independent review is why. '-lt 3' is also satisfied by parsed=0, which a gate whose
    # function pattern matched NOTHING would produce -- so the assertion would pass against a
    # gate that had stopped parsing functions altogether. 2 is the measured value for this
    # fixture: sandbox_ok and sandbox_third parse, sandbox_single_quoted does not.
    $f4m = [regex]::Match($f4.Text, 'declare 3 CREATE \[OR REPLACE\] FUNCTION statements but only (\d+) could be parsed')
    $f4parsed = if ($f4m.Success) { [int]$f4m.Groups[1].Value } else { -1 }
    $f4ok = $f4m.Success -and ($f4parsed -eq 2) -and ($f4.Exit -eq 1)
    if ($f4ok) {
        Write-Output "  RESULT: DETECTED (exit=1, declared=3 not 4 -- comment stripper confirmed; parsed=$f4parsed of 3)"
    } else {
        Write-Output "  RESULT: *** NOT DETECTED *** (exit=$($f4.Exit))"
    }
    $script:results += [pscustomobject]@{ Mutation = 'F4 unparseable function -> PARSE_INCOMPLETE'; Target = 'sandbox'; Detected = $f4ok }
} finally {
    if (Test-Path -LiteralPath $f4sandbox) { Remove-Item -LiteralPath $f4sandbox -Recurse -Force }
}

# F5: NEGATIVE CONTROL for the dimension. A detector that fires on everything satisfies F1-F4, so
# something must assert that it stays silent on a correct database -- and for this section the
# silent failure was not a phantom finding but a MISSING one. The dollar-sign bug made the gate
# compare fewer functions than it expected while still printing drift=0, so the arithmetic
# compared_functions=expected_functions is the assertion that catches it, and it is invisible to a
# case that only fires mutations.
#
# It also covers the declaration arithmetic the function walk has to get right: the files contain
# more CREATE statements than there are live functions, because a function may be redefined by a
# later migration (last definition wins, matching apply order) and because some are dropped by a
# later migration. Neither may be reported as missing.
$f5gate = Invoke-Gate
$f5reported = if ($f5gate.Text -match 'compared_functions=(\d+)\s+expected_functions=(\d+)') {
    "$($Matches[1])/$($Matches[2])"
} else { '<none>' }
Write-Output ''
Write-Output '=== F5 negative control -> nothing reported against a correct database ==='
Write-Output "  compared/expected functions reported: $f5reported"
$f5countsMatch = ($f5gate.Text -match 'compared_functions=(\d+)\s+expected_functions=(\d+)') `
    -and ($Matches[1] -eq $Matches[2]) `
    -and ([int] $Matches[1] -gt 0)
$f5ok = $f5countsMatch -and ($f5gate.Exit -eq 0) -and ($f5gate.Text -notmatch 'LIVE_DRIFT|UNDECLARED_IN_DATABASE|PARSE_INCOMPLETE')
if ($f5ok) {
    Write-Output '  RESULT: DETECTED (gate green, compared == expected, and nothing reported)'
} else {
    Write-Output "  RESULT: *** FAILED *** (exit=$($f5gate.Exit) counts=$f5reported)"
}
$script:results += [pscustomobject]@{ Mutation = 'F5 negative control -> correct database reported clean'; Target = 'live database'; Detected = $f5ok }

# =========================================================================================
# SELF-TEST: does the harness itself notice a mutation that never applied?
#
# Every mutation in this file is applied, verified through the gate, and restored. The guard added
# to every dimension converts a failure to apply into a recorded NOT DETECTED instead of an
# exception that would abort the run -- and it has been trusted in exactly one way so far, which is
# the way this whole file exists to distrust. A guard that has only ever run on the happy path has
# been shown to do nothing.
#
# There are three cases and the third is the one that protects the harness rather than reporting on
# it. psql writes "NOTICE: index ... does not exist, skipping" to stderr for every DROP ... IF
# EXISTS, and Invoke-Sql already judges by exit code rather than by stderr because of exactly that.
# If the new guard treated a benign NOTICE as a failure to apply, every mutation that guards its own
# pre-drop would be reported as not-applied -- a false positive that would make the harness look
# broken on a healthy database, which is the same failure as a gate that reports phantom drift.
# =========================================================================================
Write-Output ''
Write-Output '### HARNESS SELF-TEST'

$st1 = Invoke-Mutation 'SELECT 1;'
$st2 = Invoke-Mutation 'THIS IS NOT VALID SQL;'
$st3 = Invoke-Mutation 'DROP INDEX IF EXISTS public.zz_self_test_absent_idx;'

$st1ok = ($null -eq $st1)
$st2ok = ($null -ne $st2) -and ($st2 -match 'syntax error|ERROR')
$st3ok = ($null -eq $st3)

foreach ($st in @(
    @{ n = 'H1 valid SQL applies, not reported as a failure'; ok = $st1ok; d = "returned '$st1'" },
    @{ n = 'H2 invalid SQL is reported, not silently accepted'; ok = $st2ok; d = "returned '$st2'" },
    @{ n = 'H3 a benign NOTICE on stderr is not a failure to apply'; ok = $st3ok; d = "returned '$st3'" }
)) {
    if ($st.ok) {
        Write-Output ("  [{0}] {1}" -f 'PASS', $st.n)
    } else {
        Write-Output ("  [{0}] {1} -- {2}" -f 'FAIL', $st.n, $st.d)
    }
    $script:results += [pscustomobject]@{ Mutation = $st.n; Target = 'harness self-test'; Detected = $st.ok }
}

# =========================================================================================
Write-Output ''
Write-Output '========================================================================================='
$failed = @($results | Where-Object { -not $_.Detected })
$results | ForEach-Object {
    $mark = if ($_.Detected) { 'PASS' } else { 'FAIL' }
    Write-Output ("  [{0}] {1} ({2})" -f $mark, $_.Mutation, $_.Target)
}
Write-Output '========================================================================================='
$assertions = @($results | Where-Object { $_.Mutation -notmatch '\[restore\]' })
Write-Output "assertions=$($assertions.Count) not_detected=$($failed.Count)"

Write-Output ''
Write-Output '=== final verification: the gate and db/migrate.ps1 must both be green again ==='
$finalGate = Invoke-Gate
$finalGate.Text.Trim() -split "`r?`n" | ForEach-Object { Write-Output "  $_" }
Write-Output "  gate exit=$($finalGate.Exit)"

$migrate = Invoke-Script -Path (Join-Path $repoRoot 'db/migrate.ps1') `
    -Arguments @('-Container', $Container, '-Database', $Database, '-User', $User)
$migrate.Text.Trim() -split "`r?`n" | Where-Object { $_ -match 'applied_now|failures|drift' } |
    ForEach-Object { Write-Output "  migrate: $_" }
Write-Output "  migrate exit=$($migrate.Exit)"

if ($failed.Count -gt 0 -or $finalGate.Exit -ne 0 -or $migrate.Exit -ne 0) {
    Write-Output 'OVERALL: FAIL'
    exit 1
}
Write-Output 'OVERALL: PASS - every index, constraint, trigger and function mutation was detected and the schema is green again.'
exit 0