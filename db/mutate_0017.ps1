# Mutation test for migration 0017 (configuration boundaries).
#
# Same discipline as db/mutate_0016.ps1: a control that has never been shown to
# fail is not evidence of a control. Each mutation disables one specific
# behaviour and the suite must go red. A mutation that leaves the suite green
# means the behaviour was not load-bearing and the corresponding test is
# worthless.
#
# The database is rebuilt from the mutated file every time, so the mutated
# definition is the real one rather than a leftover from a previous run.

param(
    [string]$Container = 'aitc-pg17'
)

$ErrorActionPreference = 'Stop'
$migration = Join-Path $PSScriptRoot '..\db\migrations\0017_configuration_boundary_enforcement.sql'
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
    Write-Host "=== MUTATION $script:n: $Name ==="
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
        Write-Host "    migration failed to apply -- the mutant is not even valid SQL:"
        ($apply | Select-String -Pattern 'ERROR:' | Select-Object -First 1) | ForEach-Object { Write-Host "      $($_.Line)" }
        return $false
    }

    $out = (& go test ./dbtest/... -count=1 -run 'Config|Revision|Promotion|Secret|Active|Draft|Superseding|Innocent' 2>&1) -join "`n"
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

# A: the digest-to-document binding is dropped. Without it, an operator can
# sign an honest digest and then replace the document, leaving a valid
# signature over a configuration nobody approved.
$results['A'] = Invoke-Mutation -Name 'content digest no longer binds the document' `
    -Description 'content_digest and document can disagree again, so the release signature attests to nothing.' `
    -MustFailPattern 'Config' `
    -Mutate {
        param($s)
        # Replace the binding with a tautology. Written without nested quotes so
        # the PowerShell escaping cannot mangle the mutant into invalid SQL --
        # an earlier version of this mutation produced a syntax error, was
        # reported as "not load-bearing", and that report was meaningless.
        $s -replace 'content_digest = config\.document_digest\(document\)', 'content_digest = content_digest'
    }

# B: content immutability is dropped, so an ACTIVE snapshot can be rewritten.
$results['B'] = Invoke-Mutation -Name 'content immutability removed' `
    -Description 'An activated configuration can be edited in place, which is not a snapshot.' `
    -MustFailPattern 'Revision|Config' `
    -Mutate {
        param($s)
        $s -replace 'IF v_content_changed IS NOT NULL THEN', 'IF false THEN'
    }

# C: the lifecycle is no longer closed, so a revoked revision can be revived.
$results['C'] = Invoke-Mutation -Name 'lifecycle no longer closed' `
    -Description 'A REVOKED or SUPERSEDED revision can return to ACTIVE, laundering a revocation.' `
    -MustFailPattern 'Revision' `
    -Mutate {
        param($s)
        $s -replace 'IF NOT \(\r?\n            \(OLD\.status = ''DRAFT''    AND NEW\.status IN \(''ACTIVE'', ''REVOKED''\)\) OR\r?\n            \(OLD\.status = ''ACTIVE''   AND NEW\.status IN \(''SUPERSEDED'', ''REVOKED''\)\)\r?\n        \) THEN', 'IF false THEN'
    }

# D: DELETE of an activated revision is permitted, so release evidence can be
# removed. audit.record has an independent copy; this table does not.
$results['D'] = Invoke-Mutation -Name 'release evidence can be deleted' `
    -Description 'An ACTIVE configuration revision can be deleted, erasing the record of what was in force.' `
    -MustFailPattern 'Revision' `
    -Mutate {
        param($s)
        $s -replace "IF OLD\.status <> 'DRAFT' THEN", "IF false THEN"
    }

# E: more than one ACTIVE revision per environment is permitted again, which is
# the ambiguity that produces a last-write-wins reader.
$results['E'] = Invoke-Mutation -Name 'multiple ACTIVE revisions per environment permitted' `
    -Description 'The environment-unique index is replaced by the original per-number index, allowing several ACTIVE snapshots.' `
    -MustFailPattern 'Active|Superseding' `
    -Mutate {
        param($s)
        $s -replace 'ON config\.revision \(environment\)\r?\n    WHERE status = ''ACTIVE'';', "ON config.revision (environment, revision_number)`r`n    WHERE status = 'ACTIVE';"
    }

# F: the secrets walker is removed, so a configuration may embed a password.
$results['F'] = Invoke-Mutation -Name 'embedded secrets permitted' `
    -Description 'A configuration document may carry a plaintext credential again.' `
    -MustFailPattern 'Secret|Config' `
    -Mutate {
        param($s)
        # Neutralise the function, not the trigger. Renaming a CREATE TRIGGER does
        # not disable it -- the new name still fires -- so an earlier version of
        # this mutation renamed the trigger, the secrets control stayed fully
        # active, and the suite stayed green for the wrong reason.
        #
        # The pattern anchors on the function header rather than the PERFORM
        # statement, whose quoted '$.document' argument is awkward to pass
        # through PowerShell's own quoting intact.
        [regex]::Replace($s,
            '(CREATE OR REPLACE FUNCTION config\.guard_revision_secrets\(\)\s*RETURNS TRIGGER\s*LANGUAGE plpgsql\s*AS \$\$\s*BEGIN)',
            "`$1`r`n    RETURN NEW; -- MUTANT F", 1)
    }

# G: promotion into live no longer needs a second approver, so an automated
# pipeline with a single service identity can copy configuration into live.
$results['G'] = Invoke-Mutation -Name 'live promotion needs only one identity' `
    -Description 'A single-identity automated pipeline can copy configuration into live, which doc 17 §1 forbids.' `
    -MustFailPattern 'Promotion' `
    -Mutate {
        param($s)
        $s -replace "IF NEW\.environment = 'live' THEN", 'IF false THEN'
    }

# H: the promotion ladder is no longer monotonic.
$results['H'] = Invoke-Mutation -Name 'promotion ladder no longer monotonic' `
    -Description 'A staging configuration can be copied downward into dev, and a live one sideways.' `
    -MustFailPattern 'Promotion' `
    -Mutate {
        param($s)
        $s -replace 'IF common\.environment_rank\(v_source\.environment\) >= common\.environment_rank\(NEW\.environment\) THEN', 'IF false THEN'
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
