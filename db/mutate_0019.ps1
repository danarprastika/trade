# Mutation test for migration 0019 (instrument capability, increments, venue
# symbol append-only, sequence monotonicity, and the risk-increase gate).
#
# The last of those is inherited rather than introduced. 0019 re-issues
# CREATE OR REPLACE FUNCTION ops.assert_risk_increase_permitted in order to add its
# own step 4, which makes 0019's copy of that function -- including the reconciliation
# gate call carried forward from 0018 -- the definition that is actually live. A
# mutation of that gate must therefore be applied to THIS file; see mutation I.
#
# The market schema had ZERO triggers before this migration. A green test suite
# proved nothing about a control that did not exist, which is the whole reason
# the previous seventeen migrations could pass unnoticed while 0002 declared
# four safety controls in comments and implemented none of them.
#
# These mutations restore, one at a time, the pre-0019 behaviour. Each MUST turn
# the suite red, and each MUST be caught by a test that is about the control it
# removed -- not by an unrelated test failing as collateral.

param(
    [string]$Container = 'aitc-pg17',
    [string]$Database = 'aitc'
)

$ErrorActionPreference = 'Stop'

# This script DROPS every schema and re-applies the migration series once per
# mutation. It must only ever be pointed at a disposable database, so the
# container and database are pinned here rather than trusted from the caller.
# migrate.ps1 refuses a database whose name looks like production; these
# mutations bypass that check by dropping schemas directly, so the guard has to
# be repeated on both halves of the target. Naming an override explicitly is
# still possible for a genuinely disposable remote instance.
if ($Container -ne 'aitc-pg17' -or $Database -ne 'aitc') {
    throw "mutate_0019 rewrites and re-applies the whole migration series, which drops every schema. It refuses to run against '$Container'/'$Database': only the local disposable pair aitc-pg17/aitc is permitted. Point it at that container, or edit this guard deliberately if you have another disposable target."
}
$migration = Join-Path $PSScriptRoot '..\db\migrations\0019_market_data_instrument_and_symbol_enforcement.sql'
$backup = "$migration.orig"
$env:AITC_TEST_DATABASE_URL = 'postgres://postgres:aitc_local_dev_only@localhost:55439/aitc?sslmode=disable'

Copy-Item -LiteralPath $migration -Destination $backup -Force

$results = [ordered]@{}

function Invoke-Mutation {
    param(
        [string]$Name,
        [string]$Description,
        [scriptblock]$Mutate,
        [string]$MustFailPattern
    )

    Write-Host ""
    Write-Host "=== MUTATION : $Name ==="
    Write-Host "    $Description"

    # The migration file is restored in the finally block below whatever
    # happens here. An exception thrown by go test, by the file write, or by
    # PowerShell itself therefore still leaves the working tree holding the
    # original bytes rather than a mutant that silently outlives the run and
    # makes the next migrate.ps1 report DRIFT against a file nobody remembers
    # editing.
    try {
        # Restored from the backup first so every attempt begins from the
        # original, even though the caller above already did so.
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

        $out = (& go test ./dbtest/... -count=1 2>&1) -join "`n"
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

# A: capability check deleted. Restores the pre-0019 world in which
# market.instrument.order_types was read by nothing.
$results['A'] = Invoke-Mutation -Name 'order type support no longer enforced' `
    -Description 'An order type the instrument does not declare is accepted.' `
    -MustFailPattern 'OrderType|Supported' `
    -Mutate {
        param($s)
        $s -replace '    IF NOT \(p_order_type = ANY \(v_allowed_types\)\) THEN\r?\n        RETURN false;\r?\n    END IF;', ''
    }

# B: shorting check deleted.
$results['B'] = Invoke-Mutation -Name 'shorting support no longer enforced' `
    -Description 'A sell against supports_shorting = false is accepted.' `
    -MustFailPattern 'Short|Closing' `
    -Mutate {
        param($s)
        $s -replace "(?s)    -- A sell is short exposure unless it is a closing action\..*?    END IF;\r?\n", ''
    }

# C: increment grid deleted. This is the defect that lets a non-conforming order
# leave the platform and be rejected by the venue adapter at best.
$results['C'] = Invoke-Mutation -Name 'increment grid no longer enforced' `
    -Description 'Quantity and price may sit off the instrument increment grid.' `
    -MustFailPattern 'Increment|TickGrid' `
    -Mutate {
        param($s)
        $s -replace '    RETURN p_value / p_increment = trunc\(p_value / p_increment\);', '    RETURN true;'
    }

# D: min_notional deleted.
$results['D'] = Invoke-Mutation -Name 'min_notional no longer enforced' `
    -Description 'An order below the instrument min_notional floor is accepted.' `
    -MustFailPattern 'Notional|ValuedAgainst' `
    -Mutate {
        param($s)
        $s -replace '    IF v_inc\.min_notional IS NOT NULL THEN', '    IF false THEN'
    }

# E: tradability at the risk boundary deleted. The order can be created against
# a tradable instrument and the instrument can be delisted before risk approval.
$results['E'] = Invoke-Mutation -Name 'tradability no longer checked at risk approval' `
    -Description 'An instrument delisted after order creation is still risk-approved.' `
    -MustFailPattern 'StoppedTrading|UnknownTradingStatus|Halted|NonOpen' `
    -Mutate {
        param($s)
        $s -replace '    RETURN v_status = ''OPEN'';', "    RETURN v_status IS NOT NULL;"
    }

# F: the append-only guard no longer freezes identity. Restores defect 16d --
# an effective mapping repointed at another instrument.
$results['F'] = Invoke-Mutation -Name 'effective venue symbol mapping can be repointed' `
    -Description 'An in-force mapping can be rewritten to a different instrument or symbol.' `
    -MustFailPattern 'Repointed|Renamed|MoveItsEffectiveAt' `
    -Mutate {
        param($s)
        $s -replace '    IF OLD\.effective_at <= now\(\) THEN\r?\n        IF NEW\.instrument_id', '    IF false THEN
        IF NEW.instrument_id'
    }

# G: DELETE of an effective mapping allowed. Removing the row does not unmap the
# venue; it destroys the record of what the venue was told.
$results['G'] = Invoke-Mutation -Name 'effective venue symbol mapping can be deleted' `
    -Description 'An in-force mapping can be deleted, losing the venue-truth record.' `
    -MustFailPattern 'CannotBeDeleted' `
    -Mutate {
        param($s)
        $s -replace '        IF OLD\.effective_at <= now\(\) THEN\r?\n            RAISE EXCEPTION\r?\n                ''venue symbol mapping', '        IF false THEN
            RAISE EXCEPTION
                ''venue symbol mapping'
    }

# H: sequence watermark may rewind. Found by independent review (M1): the
# original sequence_no_regression CHECK never tested monotonicity at all.
$results['H'] = Invoke-Mutation -Name 'sequence watermark may move backwards' `
    -Description 'A stale venue report overwrites a newer watermark, and the gap counter regresses.' `
    -MustFailPattern 'SequenceWatermark' `
    -Mutate {
        param($s)
        $s -replace '       AND NEW\.observed_sequence < OLD\.observed_sequence THEN', '       AND false THEN'
    }

# I: the reconciliation gate call removed from the risk-increase path. 0019 does not
# introduce this control, it comes from 0018 -- but 0019 REPLACES the function that
# contains it in order to add its own step 4, so 0019's copy is the definition that is
# actually live. This mutation used to sit in mutate_0018.ps1, where it stripped the
# identical call from 0018's copy and changed the live schema by zero bytes: 0019's
# definition overwrote it at apply time, so nothing went red and the harness reported
# the mutation as not load-bearing. Aiming it here is what makes it prove anything.
$results['I'] = Invoke-Mutation -Name 'gate no longer called from the risk-increase path' `
    -Description 'Unresolved material breaks are recorded and never acted on: the pre-0018 state.' `
    -MustFailPattern 'Break|Uninterpretable' `
    -Mutate {
        param($s)
        [regex]::Replace($s,
            '(    -- 3\. An unresolved MATERIAL reconciliation break covering this scope \(0018\)\.[\s\S]*?    PERFORM reconciliation\.assert_break_permitted\(\s*p_environment, p_account_id, p_instrument_id, p_venue_id,\s*p_is_risk_increasing, p_context\);)',
            '    -- MUTANT I: the reconciliation gate is not called', 1)
    }

# Restored and the schema rebuilt outside the per-mutation try/finally above,
# which has already put the original bytes back. The -Database is passed
# explicitly to match the guard at the top of this script.
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
