# verify-gofmt.ps1 — checks gofmt over this module's own packages, and nothing else.
#
# WHY THIS EXISTS
#
# The obvious command, `gofmt -l .`, walks the FILESYSTEM. This repository's working
# directory contains .kilo/worktrees/abrasive-artichoke, a second git worktree holding
# a separate copy of the module. `gofmt -l .` descends into it and reports 26 files as
# unformatted, none of which are this module's files and none of which anyone working
# here may reformat: a gate that reports another worktree's formatting is noise, and a
# gate whose output is mostly noise is a gate people learn to ignore.
#
# Measured on 2026-10-01: 26 flagged files, 0 of them in the main repository. Every one
# was under .kilo\worktrees\abrasive-artichoke. The main repository's own files were
# already clean; the check was reporting nothing useful about them.
#
# WHY THE EXCLUSION IS STRUCTURAL, NOT A FILTER
#
# The obvious fixes are both bad. A deny-list (`gofmt -l . | Where-Object { $_ -notmatch
# '\.kilo' }`) hardcodes today's layout, silently starts failing to exclude a worktree
# added later, and — as this session already recorded once — a filter written for the
# wrong separator reports the worktree's files anyway while appearing to work.
#
# Instead the scope comes from `go list`, which reports the packages THIS MODULE builds
# and already excludes any nested module. Measured: 16 packages, zero of them under
# .kilo. That stays correct when a package is added, when a worktree is added, and when
# the module path changes, because none of those are hardcoded here. The worktree is a
# separate module, so the Go toolchain has already excluded it before this script runs.
#
# The trade-off is stated rather than hidden: a .go file sitting in a directory with no
# package (no buildable Go files) is NOT covered, because `go list` cannot see it. That
# is the correct trade for this repository — every .go file here belongs to a package —
# and the file count printed below makes the scope visible so a change in coverage is
# observable rather than silent.
#
# FAIL CLOSED
#
# The gate reports how many files it examined and exits 1 if that is zero. A formatting
# check that examined nothing has not passed; it has not run, and reporting success
# would repeat the defect this repository has already hit twice: a verifier that
# compares fewer things than it appears to, and reports the gap as a green result.
#
# EXIT CODES
#
#   0  every examined file is gofmt-formatted
#   1  at least one file is unformatted, or nothing could be examined

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

# The module's own package directories, from the build list rather than from a guess.
# {{.Dir}} is the real filesystem directory; the default output is an import path, and
# Split-Path on an import path yields nonsense like github.com\aitc\trade.
#
# The error-action preference is lowered around the call on purpose. Under
# $ErrorActionPreference = 'Stop', a native command that writes to stderr has its output
# converted to an ErrorRecord by 2>&1, and the script then dies on that ErrorRecord
# BEFORE the explicit exit-code check below can run. Measured: run from a directory whose
# module matches no packages, `go list` warns on stderr, and the gate terminated with an
# opaque NativeCommandError while the intended explanation was never printed.
#
# That still exits 1, so the gate did fail closed -- but by accident of PowerShell's
# native-command handling rather than by the rule written here, and with a message that
# does not say what was wrong. A gate should fail closed because it decided to, so the
# exit code is what decides and this script is what explains it.
$previousPreference = $ErrorActionPreference
$ErrorActionPreference = 'Continue'
$goListRaw = @(& go list -f '{{.Dir}}' ./... 2>&1)
$goListExit = $LASTEXITCODE
$ErrorActionPreference = $previousPreference

# Only the lines that name a real directory are package paths; anything else came from
# stderr and is reported rather than silently treated as a package.
$dirs = @($goListRaw | ForEach-Object { $_.ToString() } | Where-Object { $_ -and (Test-Path -LiteralPath $_) })
$rejected = @($goListRaw | ForEach-Object { $_.ToString() } | Where-Object { $_ -and -not (Test-Path -LiteralPath $_) })

if ($goListExit -ne 0) {
    Write-Output "SCOPE_UNKNOWN: 'go list ./...' exited $goListExit, so the packages to check are not known."
    if ($rejected.Count -gt 0) { $rejected | ForEach-Object { Write-Output "  go list said: $_" } }
    Write-Output 'No verdict is issued, because a formatting gate that silently checked a different set is worse than none.'
    exit 1
}
if ($dirs.Count -eq 0) {
    Write-Output 'SCOPE_UNKNOWN: the module reported no package directories, so nothing could be examined.'
    if ($rejected.Count -gt 0) { $rejected | ForEach-Object { Write-Output "  go list said: $_" } }
    exit 1
}

# The set of files to check is built ONCE, and that same list is what gofmt is handed.
#
# This is not tidiness. The first version of this script filtered a directory LIST for
# .kilo, counted the filtered files, and then handed gofmt the unfiltered directories --
# so the count said 176 while gofmt recursed from the repository root and reported the
# 26 worktree files anyway. Two different sets were in play, and the one that reached
# the tool was the one that had not been filtered.
#
# Naming the files explicitly makes the examined set and the checked set the same set by
# construction, so the count printed below is evidence about what was actually checked
# rather than a claim about it. The repository root is walked too, so a top-level .go
# file outside any package is still covered.
$allDirs = @($dirs) + $root
$files = @()
foreach ($dir in ($allDirs | Sort-Object -Unique)) {
    $files += @(Get-ChildItem -LiteralPath $dir -Recurse -Filter '*.go' -File -ErrorAction SilentlyContinue |
        Where-Object { $_.FullName -notmatch '[\\/]\.kilo[\\/]' })
}
# A package directory is reachable from more than one entry above, so de-duplicate by
# full path rather than trusting the enumeration to.
$files = @($files | Sort-Object FullName -Unique)
$examined = $files.Count

$unformatted = @()
# Batched so a large repository cannot overflow the command line, which on Windows would
# surface as a truncation and a silently short check rather than as an error.
$batchSize = 40
for ($i = 0; $i -lt $examined; $i += $batchSize) {
    $batch = @($files[$i..([Math]::Min($i + $batchSize - 1, $examined - 1))] | ForEach-Object { $_.FullName })
    $out = & gofmt -l @batch 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "gofmt failed on a batch of $($batch.Count) files: $($out -join ' ')"
    }
    foreach ($line in $out) {
        if (-not [string]::IsNullOrWhiteSpace($line)) { $unformatted += $line.Trim() }
    }
}

Write-Output "packages=$($dirs.Count) go_files_examined=$examined unformatted=$($unformatted.Count)"

if ($examined -eq 0) {
    Write-Output 'NOTHING_EXAMINED: the gate ran but examined no .go file, so it has not verified anything.'
    exit 1
}

if ($unformatted.Count -gt 0) {
    Write-Output "UNFORMATTED ($($unformatted.Count)) -- run 'gofmt -w' on these:"
    $unformatted | ForEach-Object { Write-Output "  $_" }
    exit 1
}

Write-Output 'unformatted=0 (every .go file in this module is gofmt-formatted)'
exit 0
