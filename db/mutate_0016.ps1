# Mutation test for migration 0016 (position reconciles to validated fills).
#
# A control that has never been shown to fail is not evidence of a control. Each
# mutation below disables one specific behaviour in one specific trigger, and the
# test is only meaningful if the corresponding tests go red. A mutation that
# leaves the suite green means the behaviour it removed was not load-bearing, and
# the corresponding test is worthless.
#
# Each mutation is applied to a copy of the migration, the database is rebuilt
# from scratch so the mutated definition is the real one, and the position tests
# are run. The original file is always restored.

param(
    [string]$Container = 'aitc-pg17',
    [string]$Database = 'aitc'
)

$ErrorActionPreference = 'Stop'

# This script DROPS every schema and re-applies the migration series once per
# mutation, so it must only ever be pointed at a disposable database. migrate.ps1
# refuses a database whose name looks like production; these mutations bypass
# that check by dropping schemas directly, so the guard is repeated on both
# halves of the target here.
if ($Container -ne 'aitc-pg17' -or $Database -ne 'aitc') {
    throw "mutate_0016 rewrites and re-applies the whole migration series, which drops every schema. It refuses to run against '$Container'/'$Database': only the local disposable pair aitc-pg17/aitc is permitted. Point it at that container, or edit this guard deliberately if you have another disposable target."
}

$migration = Join-Path $PSScriptRoot '..\db\migrations\0016_position_reconciles_to_validated_fills.sql'
$backup = "$migration.orig"
$env:AITC_TEST_DATABASE_URL = 'postgres://postgres:aitc_local_dev_only@localhost:55439/aitc?sslmode=disable'

Copy-Item -LiteralPath $migration -Destination $backup -Force

function Invoke-Mutation {
    param(
        [string]$Name,
        [string]$Description,
        [scriptblock]$Mutate,
        [string]$MustFailPattern
    )

    Write-Host ""
    Write-Host "=== MUTATION: $Name ==="
    Write-Host "    $Description"

    # Restored in a finally block so an exception thrown by the file write or by
    # PowerShell itself still leaves the original bytes in place. A mutant that
    # outlives its run makes the next migrate.ps1 report DRIFT against a file
    # nobody remembers editing.
    try {
        Copy-Item -LiteralPath $backup -Destination $migration -Force
        $source = [System.IO.File]::ReadAllText($migration, [System.Text.Encoding]::UTF8)
        $mutated = & $Mutate $source
        if ($mutated -eq $source) {
            Write-Host "    !! the mutation did not change the file -- the pattern did not match"
            return $false
        }
        [System.IO.File]::WriteAllText($migration, $mutated, (New-Object System.Text.UTF8Encoding($false)))

        $apply = & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot '..\db\migrate.ps1') -Container $Container -Database $Database -Reset 2>&1
        if ($LASTEXITCODE -ne 0) {
            Write-Host "    migration failed to apply: $apply"
            return $false
        }

        $out = & go test ./dbtest/... -count=1 -run 'Position|Fill' 2>&1
        $failed = @(Select-String -InputObject ($out -join "`n") -Pattern '^\s*--- FAIL: (\w+)' -AllMatches |
            ForEach-Object { $_.Matches } | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)

        if ($failed.Count -eq 0) {
            Write-Host "    RESULT: no test failed -- the removed behaviour was NOT load-bearing"
            return $false
        }

        Write-Host "    RESULT: caught. Failing tests:"
        foreach ($f in $failed) { Write-Host "      - $f" }

        $unrelated = @($failed | Where-Object { $_ -notmatch $MustFailPattern })
        if ($unrelated.Count -gt 0) {
            Write-Host "    !! over-broad: also failed tests outside '$MustFailPattern': $($unrelated -join ', ')"
            return $false
        }
        return $true
    } finally {
        Copy-Item -LiteralPath $backup -Destination $migration -Force
    }
}

$results = @{}

# A: the fill-side control is neutered entirely.
$results['A'] = Invoke-Mutation -Name 'A: fill-side control removed' `
    -Description 'A validated fill arrives and no position row is touched; the transaction is wrongly allowed to settle.' `
    -MustFailPattern 'Fill' `
    -Mutate {
        param($s)
        [regex]::Replace($s,
            '(CREATE OR REPLACE FUNCTION portfolio\.guard_new_fill_reconciles_positions\(\)[\s\S]*?BEGIN\r?\n)',
            "`$1`n    RETURN NULL; -- MUTANT A`n", 1)
    }

# B: the position-side control is neutered entirely.
$results['B'] = Invoke-Mutation -Name 'B: position-side control removed' `
    -Description 'A position is written with a quantity that no validated fill supports; the transaction is wrongly allowed to settle.' `
    -MustFailPattern 'Position' `
    -Mutate {
        param($s)
        [regex]::Replace($s,
            '(CREATE OR REPLACE FUNCTION portfolio\.guard_position_reconciles_to_fills\(\)[\s\S]*?BEGIN\r?\n)',
            "`$1`n    RETURN NULL; -- MUTANT B`n", 1)
    }

# C: the deferred trigger is reverted to reading the queued tuple instead of the
#    current row. This is the bug that was actually found and fixed: a position
#    INSERTed and then UPDATEd in one transaction is checked against its
#    pre-UPDATE values and refused even though the committed state is correct.
$results['C'] = Invoke-Mutation -Name 'C: deferred trigger reads the queued tuple, not the current row' `
    -Description 'The accepted path breaks: a position built and corrected within one transaction is refused against pre-UPDATE values.' `
    -MustFailPattern 'Position' `
    -Mutate {
        param($s)
        # Re-read replaced by the queued tuple. This is the bug that was actually
        # found while building this control, so it is the one most worth pinning.
        $s -replace 'SELECT \* INTO r\r?\n(\s*)FROM portfolio\.position\r?\n\s*WHERE position_id = NEW\.position_id;', "r := NEW; -- MUTANT C`r`n`$1-- (re-read removed)"
    }

# D: the empty-book coalesce is removed, so an account with no validated fills
#    can never hold a position at all.
$results['D'] = Invoke-Mutation -Name 'D: empty-book watermark coalesce removed' `
    -Description 'MAX() returns NULL instead of 0, so a flat account is refused a zero position.' `
    -MustFailPattern 'Position' `
    -Mutate {
        param($s)
        $s -replace 'COALESCE\(MAX\(f\.fill_sequence\), 0\)', 'MAX(f.fill_sequence)'
    }

# E: the watermark is allowed to be any older sequence rather than the latest.
$results['E'] = Invoke-Mutation -Name 'E: stale watermark accepted' `
    -Description 'A position may pin itself to an older fill sequence and silently ignore every validated fill since -- the exact drift invariant 5 exists to prevent.' `
    -MustFailPattern 'Position' `
    -Mutate {
        param($s)
        $s -replace 'IS DISTINCT FROM v_max_seq', '> 1000000000'
    }

Write-Host ""
Write-Host "=== RESTORED (original bytes back, schema rebuilt from them) ==="
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot '..\db\migrate.ps1') -Container $Container -Database $Database -Reset 2>&1 |
    Select-String -Pattern 'applied_now'

# The backup is removed only after the schema has been rebuilt from the restored
# file. Removing it earlier would leave no way back if the rebuild failed.
Remove-Item -LiteralPath $backup -Force

$caught = @($results.Keys | Where-Object { $results[$_] })
$missed = @($results.Keys | Where-Object { -not $results[$_] })
Write-Host ""
Write-Host "mutations caught: $($caught.Count)/$($results.Count)  [$($caught -join ', ')]"
if ($missed.Count -gt 0) {
    Write-Host "MUTATIONS SURVIVED (not load-bearing): $($missed -join ', ')"
    exit 1
}
exit 0
