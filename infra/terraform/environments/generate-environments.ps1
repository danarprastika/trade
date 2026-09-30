# Generates the test/staging/paper/shadow environment root modules by templating
# the (hand-maintained) dev root module.
#
# The `live` environment is NOT generated: it differs structurally (venue egress,
# live-eligibility evidence inputs, G11 gate) and is maintained as a distinct file.
#
# NOTE ON ENCODING: this script reads and writes UTF-8 explicitly. PowerShell 5.1
# defaults to ANSI when reading a .ps1 without a BOM, which would corrupt the
# section-sign characters in the blueprint citations.
#
# Run:  powershell -NoProfile -ExecutionPolicy Bypass -File .\generate-environments.ps1

$ErrorActionPreference = "Stop"

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$devDir = Join-Path $here "dev"
$utf8 = New-Object System.Text.UTF8Encoding($false)

# dev/main.tf, variables.tf and outputs.tf are the canonical templates. Only
# whole-word `dev` tokens are substituted; the blueprints' section-sign
# characters are preserved verbatim.
$targets = @("test", "staging", "paper", "shadow")

$devMain    = [System.IO.File]::ReadAllText((Join-Path $devDir "main.tf"), $utf8)
$devVars    = [System.IO.File]::ReadAllText((Join-Path $devDir "variables.tf"), $utf8)
$devOutputs = [System.IO.File]::ReadAllText((Join-Path $devDir "outputs.tf"), $utf8)

foreach ($n in $targets) {
  $dir = Join-Path $here $n
  if (-not (Test-Path -LiteralPath $dir)) {
    New-Item -ItemType Directory -Path $dir | Out-Null
  }

  $main = [System.Text.RegularExpressions.Regex]::Replace($devMain, '\bdev\b', $n)
  $vars = [System.Text.RegularExpressions.Regex]::Replace($devVars, '\bdev\b', $n)
  $outs = [System.Text.RegularExpressions.Regex]::Replace($devOutputs, '\bdev\b', $n)

  [System.IO.File]::WriteAllText((Join-Path $dir "main.tf"), $main, $utf8)
  [System.IO.File]::WriteAllText((Join-Path $dir "variables.tf"), $vars, $utf8)
  [System.IO.File]::WriteAllText((Join-Path $dir "outputs.tf"), $outs, $utf8)

  Write-Output "generated: $n"
}
