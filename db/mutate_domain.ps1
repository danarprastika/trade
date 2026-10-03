# Mutation-tests the domain packages.
#
# Each mutation disables one safety behaviour and re-runs the suite. A mutation
# that no test catches is a control that is present in the source and absent
# from the behaviour -- the fourth shape of that defect found so far, after
# defect 15 (a comment), the idle digest guard in 0017, and the uncalled
# assert_canonical_entropy in 0020.
#
# Mutations here target the domain layer, which is new code with no prior
# review. The Go/SQL parity tests cannot be mutated this way because they read
# the database rather than the Go source, so these mutations exercise the
# domain packages directly.
#
# Every mutation must be caught. A survivor means the suite is asserting
# something it does not actually check.

param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = Split-Path -Parent $PSScriptRoot
if (-not $root) { $root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path) }
$root = Split-Path -Parent $PSScriptRoot

function Invoke-Suite {
    param([string]$Label, [string[]]$ExtraArgs = @())
    Push-Location $root
    # A mutation that fails to compile writes to stderr, and under
    # $ErrorActionPreference = 'Stop' that aborts the whole run instead of
    # being reported as "caught by a compile failure". A compile failure is a
    # legitimate way for a mutation to be caught -- it proves the code depends
    # on what was changed -- so the suite must run to completion and report it.
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = & go test ./domain/... ./dbtest/... -count=1 2>&1 | Out-String
        $code = $LASTEXITCODE
        return @{ Code = $code; Out = $out }
    } finally {
        $ErrorActionPreference = $prev
        Pop-Location
    }
}

# Applies a mutation, runs the suite, restores, and reports.
function Invoke-Mutation {
    param(
        [string]$Label,
        [string]$File,
        [string]$Find,
        [string]$Replace,
        [string]$MustFailPattern
    )

    $path = Join-Path $root $File
    if (-not (Test-Path -LiteralPath $path)) { throw "missing mutation target: $File" }
    # Normalise to LF so a multi-line anchor written with "`n" always matches,
    # regardless of what line endings the file happens to carry.
    $original = [IO.File]::ReadAllText($path, [Text.Encoding]::UTF8).Replace("`r`n", "`n")
    $Find = $Find.Replace("`r`n", "`n")
    $Replace = $Replace.Replace("`r`n", "`n")

    if (-not $original.Contains($Find)) {
        Write-Host "=== MUTATION [$Label] : ANCHOR NOT FOUND in $File ===" -ForegroundColor Red
        Write-Host "    anchor: $Find" -ForegroundColor Red
        return @{ Label = $Label; Caught = $false; Reason = 'anchor-not-found' }
    }

    $backup = "$path.mutant.bak"
    Copy-Item -LiteralPath $path -Destination $backup -Force
    try {
        $mutated = $original.Replace($Find, $Replace)
        [IO.File]::WriteAllText($path, $mutated, (New-Object Text.UTF8Encoding($false)))

        $r = Invoke-Suite -Label $Label
        $failedBy = @()
        foreach ($m in [regex]::Matches($r.Out, '(?m)^\s*--- FAIL: (\w+)')) { $failedBy += $m.Groups[1].Value }
        $failedBy = @($failedBy | Sort-Object -Unique)

        if ($r.Code -eq 0) {
            Write-Host "=== MUTATION [$Label] : SURVIVED ===" -ForegroundColor Red
            Write-Host "    the suite passed with this behaviour disabled" -ForegroundColor Red
            return @{ Label = $Label; Caught = $false; Reason = 'survived'; Failures = @() }
        }

        if ($failedBy.Count -gt 0) {
            Write-Host "=== MUTATION [$Label] : caught by $($failedBy -join ', ') ===" -ForegroundColor Cyan
        } else {
            Write-Host "=== MUTATION [$Label] : caught (build/compile failure) ===" -ForegroundColor Cyan
        }

        if ($MustFailPattern -and ($r.Out -notmatch $MustFailPattern)) {
            # Caught by tests other than the one this mutation names is not proof: the mutation
            # may have broken something incidental. The SQL harness already hard-fails on the
            # equivalent over-broad case, and leaving this a warning let K and I report as caught
            # while no assertion capable of detecting their mutation had failed.
            $alsoFailed = if ($failedBy.Count) { $failedBy -join ', ' } else { 'no named test (build/compile failure)' }
            Write-Host "    WRONG REASON: nothing mentions '$MustFailPattern'; caught only by $alsoFailed" -ForegroundColor Red
            return @{ Label = $Label; Caught = $false; Reason = 'wrong-reason'; Failures = $failedBy }
        }
        return @{ Label = $Label; Caught = $true; Failures = $failedBy }
    } finally {
        Copy-Item -LiteralPath $backup -Destination $path -Force
        Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue
    }
}

$mutations = @(
    @{
        Label  = 'A: strategy Rule keyed on From alone (the 3 dropped rules)'
        File   = 'domain/strategy/strategy.go'
        Find   = 'm[ruleKey{r.From, r.To}] = r'
        Replace= 'm[ruleKey{r.From, r.From}] = r'
        MustFail= 'EveryDeclaredRuleIsReachableAndUnique|MultiSuccessorStatesAreNotLosingRules|RequiredRoleIsEnforcedAndNamed|DualControl'
    },
    @{
        Label  = 'B: UNKNOWN escapable by assumption'
        File   = 'domain/oms/oms.go'
        Find   = '{StateUnknown, StateFilled, EventOutcomeResolved, false, true}'
        Replace= '{StateUnknown, StateFilled, EventFilled, false, false}'
        MustFail= 'UnknownIsNeitherTerminalNorEscapableByAssumption'
    },
    @{
        Label  = 'C: risk decision no longer required before submission'
        File   = 'domain/oms/oms.go'
        Find   = '{StateRiskApproved, StateSubmitting, EventSubmitting, true, false}'
        Replace= '{StateRiskApproved, StateSubmitting, EventSubmitting, false, false}'
        MustFail= 'RiskIncreasingTransitionsRequireADecision'
    },
    @{
        # Doc 04: "A single failed mandatory control rejects the order." This is
        # the headline invariant of the whole gate, and the mutation is a single
        # line so it compiles -- a mutation caught only by a compile error
        # proves the code mentions the change, not that any assertion depends
        # on it.
        Label  = 'D: a failed mandatory control no longer rejects'
        File   = 'domain/risk/evaluate.go'
        Find   = "`t`tif !f.Passed && f.Severity == Mandatory {"
        Replace= "`t`tif false {"
        MustFail= 'OneFailedMandatoryControlRejectsTheWholeOrder|EveryExternalFactControlCanIndependentlyReject'
    },
    @{
        Label  = 'K: an unmeasurable figure is treated as zero'
        File   = 'domain/risk/evaluate.go'
        Find   = "`tif measured == nil {"
        Replace= "`tif false {"
        MustFail= 'AnUnmeasurableFigureDeniesDistinctly'
    },
    @{
        Label  = 'E: halt precedence inverted (highest rank loses)'
        File   = 'domain/halt/halt.go'
        Find   = 'return candidates[i].Level.Rank() > candidates[j].Level.Rank()'
        Replace= 'return candidates[i].Level.Rank() < candidates[j].Level.Rank()'
        MustFail= 'HighestRankingApplicableHaltWins'
    },
    @{
        Label  = 'F: risk-reducing actions blocked by halts'
        File   = 'domain/halt/halt.go'
        Find   = 'if riskReducing {'
        Replace= 'if false {'
        MustFail= 'RiskReducingActionsAreNotBlocked'
    },
    @{
        Label  = 'G: dual control accepts one identity twice'
        File   = 'domain/strategy/strategy.go'
        Find   = 'if req.SecondSubject == req.Subject {'
        Replace= 'if false {'
        MustFail= 'DualControlRejectsTheSameIdentityTwice'
    },
    @{
        Label  = 'H: boundary inclusivity ignored (exact-limit order refused)'
        File   = 'domain/risk/evaluate.go'
        # The boundary is not decided by a switch over a Boundary value. risk.go fixes
        # DerivationBoundary = BoundaryInclusive and the inclusive semantics are baked into the
        # comparison itself, so the honest mutation turns ">" into ">=" and the exact-limit case
        # flips to refused. BoundaryExclusive is a declared-but-unused constant; mutating a
        # decision that no code consults would have passed while proving nothing.
        Find   = 'if measured.Cmp(bound) > 0 {'
        Replace= 'if measured.Cmp(bound) >= 0 {'
        MustFail= 'BoundaryIsInclusiveAtTheExactLimit'
    },
    @{
        Label  = 'I: an unevaluated halt gate treated as clear'
        File   = 'domain/risk/evaluate.go'
        Find   = 'if !req.Halt.Evaluated {'
        Replace= 'if false {'
        MustFail= 'AnUnevaluatedHaltGateRejects'
    },
    @{
        Label  = 'J: required role no longer enforced'
        File   = 'domain/strategy/strategy.go'
        Find   = 'if rule.RequiredRole != req.Role {'
        Replace= 'if false {'
        MustFail= 'RequiredRoleIsEnforcedAndNamed'
    }
)

Write-Host "mutating 11 domain behaviours"
Write-Host ""

$results = @()
foreach ($m in $mutations) {
    $results += Invoke-Mutation -Label $m.Label -File $m.File -Find $m.Find -Replace $m.Replace -MustFailPattern $m.MustFail
    Write-Host ""
}

# The working tree must be byte-identical to before the run.
$gofmt = & gofmt -l (Join-Path $root 'domain') 2>&1 | Out-String
if ($gofmt.Trim()) { Write-Host "WARNING: gofmt reports unformatted files after restore:`n$gofmt" -ForegroundColor Yellow }

$caught = @($results | Where-Object { $_.Caught })
$labels = @($caught | ForEach-Object { ($_.Label -split ':')[0].Trim() })

Write-Host "mutations caught: $($caught.Count)/$($results.Count)  [$($labels -join ', ')]"

$survivors = @($results | Where-Object { -not $_.Caught })
if ($survivors.Count -gt 0) {
    Write-Host ""
    foreach ($s in $survivors) {
        # Only a genuine survival may be called SURVIVED. 'wrong-reason' and 'anchor-not-found'
        # both mean the mutation WAS detected, just not by anything that proves what it claims.
        # Labelling those survivors tells an operator to go hunting for a missing test when the
        # defect is a stale pattern or a name that no longer exists.
        $word = if ($s.Reason -eq 'survived') { 'SURVIVED' } else { 'NOT PROVEN' }
        Write-Host "${word}: $($s.Label)  ($($s.Reason))" -ForegroundColor Red
    }
    exit 1
}
exit 0
