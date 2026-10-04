# Runs the three remaining migration mutation harnesses STRICTLY SEQUENTIALLY.
#
# They share one database and each drops every schema per mutation, so running two at once
# corrupts both -- which happened once in this session and left db/migrations/0019 mutated.
# One process, one harness at a time, no parallelism.
#
# This script exits 0 only when every harness exited 0 AND every post-run check passed.
# Each of those checks has to be able to fail, or it is decoration rather than evidence.
$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$scripts = @('mutate_0016.ps1', 'mutate_0017.ps1', 'mutate_0018.ps1')
$summary = @()

foreach ($s in $scripts) {
    Write-Output ""
    Write-Output "################ $s ################"
    $path = Join-Path $root "db\$s"
    & powershell -NoProfile -ExecutionPolicy Bypass -File $path 2>&1 |
        ForEach-Object { Write-Output $_ }
    $code = $LASTEXITCODE
    $summary += [pscustomobject]@{ Script = $s; Exit = $code }
    Write-Output "---- $s exit=$code ----"
}

Write-Output ""
Write-Output "################ SUMMARY ################"
foreach ($row in $summary) { Write-Output ("{0,-22} exit={1}" -f $row.Script, $row.Exit) }

# The repository and the database must be clean regardless of the outcomes above.
Write-Output ""
Write-Output "################ POST-RUN STATE ################"
$failedChecks = @()

$dirty = & git status --porcelain -- db/migrations
if ($dirty) {
    Write-Output "MIGRATION FILES DIRTY:"
    Write-Output $dirty
    $failedChecks += 'db/migrations does not match HEAD'
} else {
    Write-Output "db/migrations byte-identical to HEAD"
}

# .orig is the extension these harnesses actually create: mutate_0016.ps1, mutate_0017.ps1
# and mutate_0018.ps1 each set $backup = "$migration.orig". .mutant.bak belongs to
# mutate_domain.ps1, which this script does not run. Filtering on .mutant.bak alone meant
# this branch could never fire, so the script reported "no stray backups" after an
# interrupted run had left the .orig that is the only way to restore the file. Both
# extensions are gitignored, so the git status check above cannot stand in for this one.
$stray = Get-ChildItem -Path (Join-Path $root 'db') -Recurse -File -Include *.orig,*.mutant.bak -ErrorAction SilentlyContinue
if ($stray) {
    Write-Output "STRAY BACKUPS:"
    $stray | ForEach-Object { Write-Output "  $($_.FullName)" }
    $failedChecks += 'stray mutation backups are present'
} else {
    Write-Output "no stray backups"
}

# -Reset matters here. Each migration commits atomically with its ledger row, so a series
# left half-applied by a killed harness would otherwise just apply the remaining files and
# print applied_now=N failures=0 drift=0 -- byte-identical to a healthy rebuild. Only
# rebuilding from scratch can tell those two states apart.
$rebuild = & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $root 'db\migrate.ps1') -Container aitc-pg17 -Database aitc -Reset 2>&1
$rebuildCode = $LASTEXITCODE
$rebuild | ForEach-Object { Write-Output "  $_" }
if ($rebuildCode -ne 0) {
    Write-Output "REBUILD FAILED (exit=$rebuildCode)"
    $failedChecks += "migrate.ps1 -Reset failed (exit=$rebuildCode)"
}

Write-Output ""
if ($failedChecks.Count -gt 0 -or @($summary | Where-Object { $_.Exit -ne 0 }).Count -gt 0) {
    Write-Output "RUN FAILED:"
    foreach ($row in $summary) {
        if ($row.Exit -ne 0) { Write-Output "  - $($row.Script) exited $($row.Exit)" }
    }
    $failedChecks | ForEach-Object { Write-Output "  - $_" }
    exit 1
}
Write-Output "RUN CLEAN"
exit 0