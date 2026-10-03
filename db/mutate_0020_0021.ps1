# Mutation test for migrations 0020 (canonical ID bit layout parity) and 0021
# (timestamp nanosecond binding).
#
# Both of these close gaps the G1 gate report recorded as PARTIAL criteria, so
# the claim being made is stronger than usual: "the Go and PostgreSQL encoders
# produce the same value", and "a TIMESTAMPTZ is bound to the nanosecond value
# beside it". A parity test can pass by both sides being wrong in the same way,
# and a binding check can pass by never firing, so each behaviour is removed in
# turn and the suite must go red.

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
    throw "mutate_0020_0021 rewrites and re-applies the whole migration series, which drops every schema. It refuses to run against '$Container'/'$Database': only the local disposable pair aitc-pg17/aitc is permitted. Point it at that container, or edit this guard deliberately if you have another disposable target."
}

$m20 = Join-Path $PSScriptRoot '..\db\migrations\0020_canonical_id_bit_layout_parity.sql'
$m21 = Join-Path $PSScriptRoot '..\db\migrations\0021_timestamp_nanosecond_binding.sql'
$env:AITC_TEST_DATABASE_URL = 'postgres://postgres:aitc_local_dev_only@localhost:55439/aitc?sslmode=disable'

$b20 = "$m20.orig"
$b21 = "$m21.orig"
Copy-Item -LiteralPath $m20 -Destination $b20 -Force
Copy-Item -LiteralPath $m21 -Destination $b21 -Force

$results = [ordered]@{}

function Invoke-Mutation {
    param(
        [string]$Name,
        [string]$Description,
        [string]$Target,          # '20' or '21'
        [scriptblock]$Mutate,
        [string]$MustFailPattern
    )

    Write-Host ""
    Write-Host "=== MUTATION [$Target] : $Name ==="
    Write-Host "    $Description"

    # Both files restored in a finally block so an exception thrown by the file
    # write or by PowerShell itself still leaves the original bytes in place. A
    # mutant that outlives its run makes the next migrate.ps1 report DRIFT against
    # a file nobody remembers editing.
    try {
        Copy-Item -LiteralPath $b20 -Destination $m20 -Force
        Copy-Item -LiteralPath $b21 -Destination $m21 -Force

        $path = if ($Target -eq '20') { $m20 } else { $m21 }
        $source = [System.IO.File]::ReadAllText($path, [Text.Encoding]::UTF8)
        $mutated = & $Mutate $source
        if ($mutated -eq $source) {
            Write-Host "    !! the mutation did not change the file -- the pattern did not match"
            return $false
        }
        [System.IO.File]::WriteAllText($path, $mutated, (New-Object Text.UTF8Encoding($false)))

        $apply = & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot '..\db\migrate.ps1') -Container $Container -Database $Database -Reset 2>&1
        if ($LASTEXITCODE -ne 0) {
            Write-Host "    migration failed to apply -- the mutant is not valid SQL:"
            ($apply | Select-String -Pattern 'ERROR:' | Select-Object -First 1) | ForEach-Object { Write-Host "      $($_.Line)" }
            return $false
        }

        $out = (& go test ./dbtest/... ./contracts/... -count=1 2>&1) -join "`n"
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
    } finally {
        Copy-Item -LiteralPath $b20 -Destination $m20 -Force
        Copy-Item -LiteralPath $b21 -Destination $m21 -Force
    }
}

# 0020 A: the SQL encoder stops taking five bits at a time and reverts to the
# pre-0020 per-byte mapping. This is the exact divergence 0020 was written to
# remove, so it is the mutation that matters most for this file.
$results['A'] = Invoke-Mutation -Name 'SQL reverts to the per-byte encoding' `
    -Description 'The SQL encoder takes the top five bits of each byte instead of a continuous bit stream.' `
    -Target '20' -MustFailPattern 'EncodersAgree' `
    -Mutate {
        param($s)
        # Reverts the SQL encoder to the pre-0020 per-byte mapping, ignoring
        # p_bytes entirely. Valid SQL: generate_series is aliased with a column
        # name g(i) so i is addressable, matching 0013's original form.
        $s -replace '(?s)    WITH bits AS \(.*?      FROM generate_series\(0, 19\) AS c;', @'
    SELECT p_prefix || '_' || coalesce(string_agg(
               substr('0123456789abcdefghjkmnpqrstvwxyz',
                      (get_byte(b, i) / 8) + 1, 1),
               '' ORDER BY i), '')
      FROM gen_random_bytes(20) AS b, generate_series(0, 19) AS g(i);
'@
    }

# 0020 B: the bit order within a group is reversed. Both sides would still be
# valid Crockford Base32 of the right length, and the format tests would still
# pass; only the cross-language comparison can see it.
$results['B'] = Invoke-Mutation -Name 'bit order within a group reversed' `
    -Description 'Each character is assembled least-significant-bit first, so the SQL output is a different valid encoding.' `
    -Target '20' -MustFailPattern 'EncodersAgree' `
    -Mutate {
        param($s)
        $s -replace 'bits\.bit::int << \(4 - \(bits\.pos - c \* 5\)\)', '(bits.bit::int << (bits.pos - c * 5))'
    }

# 0020 C: entropy width is no longer enforced.
$results['C'] = Invoke-Mutation -Name 'canonical id from bytes accepts any entropy width' `
    -Description 'The encoder accepts short entropy and zero-pads it, halving the entropy of every identifier it mints.' `
    -Target '20' -MustFailPattern 'EntropyOfTheWrong|FreshIdentifiers' `
    -Mutate {
        param($s)
        $s -replace '    IF octet_length\(p_bytes\) <> 16 THEN', '    IF false THEN'
    }

# 0021 D: the rounding is replaced by truncation, which is the behaviour the
# deleted check was accidentally close to. A ns value of ...789 would then
# require a timestamp of ...000 and the correctly rounded pair would be refused.
$results['D'] = Invoke-Mutation -Name 'ns conversion truncates instead of rounding' `
    -Description 'A sub-microsecond nanosecond value no longer matches its correctly rounded timestamp.' `
    -Target '21' -MustFailPattern 'NsToTimestamptz|SubMicrosecond' `
    -Mutate {
        param($s)
        $s -replace 'round\(\(p_ns % 1000000000\)::numeric / 1000\.0\)', 'trunc((p_ns % 1000000000)::numeric / 1000.0)'
    }

# 0021 E: the market observation pair is no longer bound. Fixing only audit.record
# and calling the class closed would be the same mistake 0018 made, where the
# reconciliation gate existed but nothing called it.
$results['E'] = Invoke-Mutation -Name 'market observation timestamps are not bound' `
    -Description 'market.trade can store a source_timestamp that disagrees with its source_timestamp_ns.' `
    -Target '21' -MustFailPattern 'MarketObservation' `
    -Mutate {
        param($s)
        $s -replace 'common\.ns_bound_to_timestamptz\(source_timestamp_ns, source_timestamp\)', 'true'
    }

# 0021 F: the feed-health watermark is no longer bound. That value is what a
# freshness decision is made against, so a disagreement between its two
# representations is a disagreement about how fresh the feed is.
$results['F'] = Invoke-Mutation -Name 'feed freshness watermark is not bound' `
    -Description 'market.feed_health can store a last_source_timestamp that disagrees with its nanoseconds.' `
    -Target '21' -MustFailPattern 'FeedHealthWatermark' `
    -Mutate {
        param($s)
        $s -replace 'common\.ns_bound_to_timestamptz\(last_source_timestamp_ns, last_source_timestamp\)', 'true'
    }

Write-Host ""
Write-Host "=== RESTORED (original bytes back, schema rebuilt from them) ==="
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot '..\db\migrate.ps1') -Container $Container -Database $Database -Reset 2>&1 |
    Select-String -Pattern 'applied_now'

# The backups are removed only after the schema has been rebuilt from the
# restored files. Removing them earlier would leave no way back if the rebuild
# failed.
Remove-Item -LiteralPath $b20 -Force
Remove-Item -LiteralPath $b21 -Force

$caught = @($results.Keys | Where-Object { $results[$_] })
$missed = @($results.Keys | Where-Object { -not $results[$_] })
Write-Host ""
Write-Host "mutations caught: $($caught.Count)/$($results.Count)  [$($caught -join ', ')]"
if ($missed.Count -gt 0) {
    Write-Host "MUTATIONS SURVIVED (not load-bearing): $($missed -join ', ')"
    exit 1
}
exit 0
