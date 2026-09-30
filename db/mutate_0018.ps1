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
    [string]$Container = 'aitc-pg17'
)

$ErrorActionPreference = 'Stop'
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

    Copy-Item -LiteralPath $backup -Destination $migration -Force
    $source = [System.IO.File]::ReadAllText($migration, [System.Text.Encoding]::UTF8)
    $mutated = & $Mutate $source
    if ($mutated -eq $source) {
        Write-Host "    !! the mutation did not change the file -- the pattern did not match"
        return $false
    }
    [System.IO.File]::WriteAllText($migration, $mutated, (New-Object System.Text.UTF8Encoding($false)))

    $apply = & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot '..\db\migrate.ps1') -Container $Container -Reset 2>&1
    if ($LASTEXITCODE -ne 0) {
        Write-Host "    migration failed to apply -- the mutant is not valid SQL:"
        ($apply | Select-String -Pattern 'ERROR:' | Select-Object -First 1) | ForEach-Object { Write-Host "      $($_.Line)" }
        return $false
    }

    $out = (& go test ./dbtest/... -count=1 -run 'Break|Material|RiskReducing|NonMaterial|Resolved|Accepted|Reopened|Uninterpretable|NoBreakAtAll' 2>&1) -join "`n"
    $failed = @(Select-String -InputObject $out -Pattern '^\s*--- FAIL: (\w+)' -AllMatches |
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
}

$results = [ordered]@{}

# A: the gate stops being called at all. This is the exact state 0005 left the
# system in -- the case is recorded and nothing reads it -- so this mutation
# restores the pre-0018 world and MUST be caught.
$results['A'] = Invoke-Mutation -Name 'gate no longer called from the risk-increase path' `
    -Description 'Unresolved material breaks are recorded and never acted on: the pre-0018 state.' `
    -MustFailPattern 'Break|Uninterpretable' `
    -Mutate {
        param($s)
        [regex]::Replace($s,
            '(    -- 3\. An unresolved MATERIAL reconciliation break covering this scope\.[\s\S]*?    PERFORM reconciliation\.assert_break_permitted\(\s*p_environment, p_account_id, p_instrument_id, p_venue_id,\s*p_is_risk_increasing, p_context\);)',
            '    -- MUTANT A: the reconciliation gate is not called', 1)
    }

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

Copy-Item -LiteralPath $backup -Destination $migration -Force
Remove-Item -LiteralPath $backup -Force

Write-Host ""
Write-Host "=== RESTORED ==="
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot '..\db\migrate.ps1') -Container $Container -Reset 2>&1 |
    Select-String -Pattern 'applied_now'

$caught = @($results.Keys | Where-Object { $results[$_] })
$missed = @($results.Keys | Where-Object { -not $results[$_] })
Write-Host ""
Write-Host "mutations caught: $($caught.Count)/$($results.Count)  [$($caught -join ', ')]"
if ($missed.Count -gt 0) {
    Write-Host "MUTATIONS SURVIVED (not load-bearing): $($missed -join ', ')"
    exit 1
}
exit 0
