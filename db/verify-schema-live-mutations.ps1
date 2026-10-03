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
# the bug it fixed. A dimension with only positive cases cannot distinguish a working detector
# from one that reports everything, so each dimension ends with a negative control: C5 (a
# constraint migration 0023 declares deliberately unvalidated must NOT be reported) and T5 (a
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
    return (($out | Where-Object { $_ -is [string] -and $_ -notmatch '^(NOTICE|WARNING):' }) | Out-String).Trim()
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
        Text = (($out | ForEach-Object { if ($_ -is [string]) { $_ } else { [string]$_ } }) | Out-String)
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

# Report a mutation that never reached the database, in one place, so the three dimensions cannot
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
# SELF-TEST: does the harness itself notice a mutation that never applied?
#
# Every mutation in this file is applied, verified through the gate, and restored. The guard added
# to all three dimensions converts a failure to apply into a recorded NOT DETECTED instead of an
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
Write-Output 'OVERALL: PASS - every index, constraint and trigger mutation was detected and the schema is green again.'
exit 0