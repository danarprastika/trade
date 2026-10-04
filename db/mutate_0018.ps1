# Mutation test for migration 0018 (reconciliation breaks block risk).
#
# Same discipline as mutate_0016/0017.ps1. A control that has never been shown to
# fail is not evidence of a control, and a mutation that leaves the suite green
# means the behaviour it removed was not load-bearing.
#
# 0018 is the second time in this repository that a schema documented a control
# and implemented nothing (the first was config.revision in 0001). The whole
# point of this file is to establish that the second occurrence is actually
# fixed rather than merely annotated.

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
    throw "mutate_0018 rewrites and re-applies the whole migration series, which drops every schema. It refuses to run against '$Container'/'$Database': only the local disposable pair aitc-pg17/aitc is permitted. Point it at that container, or edit this guard deliberately if you have another disposable target."
}

$migration = Join-Path $PSScriptRoot '..\db\migrations\0018_reconciliation_breaks_block_risk.sql'
$backup = "$migration.orig"
$env:AITC_TEST_DATABASE_URL = 'postgres://postgres:aitc_local_dev_only@localhost:55439/aitc?sslmode=disable'

Copy-Item -LiteralPath $migration -Destination $backup -Force
$script:n = 0

function Invoke-Mutation {
    param(
        [string]$Name,
        [string]$Description,
        [scriptblock]$Mutate,
        [string]$MustFailPattern
    )

    $script:n++
    Write-Host ""
    Write-Host "=== MUTATION $script$n : $Name ==="
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
            Write-Host "    migration failed to apply -- the mutant is not valid SQL:"
            ($apply | Select-String -Pattern 'ERROR:' | Select-Object -First 1) | ForEach-Object { Write-Host "      $($_.Line)" }
            return $false
        }

        $out = (& go test ./dbtest/... -count=1 -run 'Break|Material|RiskReducing|NonMaterial|Resolved|Accepted|Reopened|Uninterpretable|NoBreakAtAll' 2>&1) -join "`n"
        # (?m) is load-bearing. Without it ^ anchors to the start of the whole joined
        # string, so this reported only the FIRST failing test and the MustFailPattern
        # check below was made against one test out of all of them.
        $failed = @([regex]::Matches($out, '(?m)^\s*--- FAIL: (\w+)') |
            ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)

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

$results = [ordered]@{}

# A MOVED TO db/mutate_0019.ps1 -- this is a relocation, not a deletion of coverage.
#
# This mutation used to strip the reconciliation gate call out of
# ops.assert_risk_increase_permitted *in this file*, and it silently proved
# nothing. Migration 0019 issues its own CREATE OR REPLACE FUNCTION for
# ops.assert_risk_increase_permitted, so the live definition is 0019's and 0018's
# body is discarded when the series is applied. Deleting the call here changed the
# live schema by zero bytes, no test went red, and the harness correctly reported
# "no test failed -- the removed behaviour was NOT load-bearing". The harness was
# right; the mutation was aimed at a dead copy of the function.
#
# The gate call is load-bearing and has to stay proven, so the mutation now lives in
# db/mutate_0019.ps1 as mutation I, applied to the file that actually supplies the
# function. The split follows where each piece of the control is defined: this file
# owns reconciliation.assert_break_permitted and reconciliation.scope_covers, which
# is why mutations B-G below genuinely bite, while 0019 owns the risk-increase gate
# that calls them.

# B: risk-reducing activity is no longer exempt. This is the dangerous
# direction -- a control that blocks flattening converts a data quality problem
# into an open position.
$results['B'] = Invoke-Mutation -Name 'risk-reducing activity is no longer exempt' `
    -Description 'A material break would block REDUCE_ONLY orders, trapping exposure that must be flattened.' `
    -MustFailPattern 'RiskReducing|BreakGate' `
    -Mutate {
        param($s)
        $s -replace '(?s)(CREATE OR REPLACE FUNCTION reconciliation\.assert_break_permitted.*?)    IF NOT p_is_risk_increasing THEN\r?\n        RETURN;\r?\n    END IF;', '$1    -- MUTANT B: no risk-reducing exemption'
    }

# C: every severity blocks, not just MATERIAL. A control that stops on every
# rounding difference gets switched off, which is worse than not having it.
$results['C'] = Invoke-Mutation -Name 'any severity blocks' `
    -Description 'A cosmetic LOW difference would stop trading, making the control unusable.' `
    -MustFailPattern 'NonMaterial' `
    -Mutate {
        param($s)
        $s -replace "AND c\.severity = 'MATERIAL'", "AND c.severity IS NOT NULL"
    }

# D: the terminal states are ignored, so a resolved case blocks forever.
$results['D'] = Invoke-Mutation -Name 'resolved cases never stop blocking' `
    -Description 'A resolved or accepted difference would block trading permanently, with no way to clear it.' `
    -MustFailPattern 'Resolved|Accepted|Reopened' `
    -Mutate {
        param($s)
        $s -replace "AND c\.status IN \('OPEN','INVESTIGATING','REOPENED'\)", 'AND c.status IS NOT NULL'
    }

# E: scope is ignored, so one account's break halts every account.
$results['E'] = Invoke-Mutation -Name 'blocked_scope no longer honoured' `
    -Description 'A break on one account would stop trading on every account: a denial of service on correct accounts.' `
    -MustFailPattern 'BreakOn|BreakInOne' `
    -Mutate {
        param($s)
        $s -replace 'AND reconciliation\.scope_covers\(c\.blocked_scope, p_account_id, p_instrument_id, p_venue_id\)', 'AND true'
    }

# F: an uninterpretable scope is treated as covering nothing instead of
# everything. This is the under-blocking direction and is the single most
# dangerous mutation in this file.
$results['F'] = Invoke-Mutation -Name 'uninterpretable scope under-blocks' `
    -Description 'A scope the code cannot read would stop blocking, so a real break could be missed.' `
    -MustFailPattern 'Uninterpretable' `
    -Mutate {
        param($s)
        # Every fail-closed "return true" becomes "return false", which is
        # precisely the trade this migration refused to make.
        [regex]::Replace($s, '(?s)(CREATE OR REPLACE FUNCTION reconciliation\.scope_covers.*?)\bRETURN true;', '$1RETURN false;')
    }

# G: environment is no longer part of the match, so a paper break halts shadow.
$results['G'] = Invoke-Mutation -Name 'environment no longer part of the match' `
    -Description 'A break in one environment would stop trading in every environment.' `
    -MustFailPattern 'BreakInOne' `
    -Mutate {
        param($s)
        $s -replace 'WHERE c\.environment = p_environment', 'WHERE true'
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
