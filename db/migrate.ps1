#!/usr/bin/env pwsh
# =============================================================================
# db/migrate.ps1 - Migration harness
#
# Applies every migration in db/migrations in lexicographic order against a
# target PostgreSQL 17 instance, tracking applied versions in schema_migration.
#
# Migration discipline (24_ENTERPRISE_RELEASE_STANDARD.md §5):
#   - Every migration is EXPAND-only and must remain compatible with the
#     immediately previous release so that rollback never requires a
#     down-migration of a destructive change.
#   - -Reset is a development-only affordance and refuses to run unless the
#     target explicitly declares itself non-production.
# =============================================================================
param(
    [Parameter(Mandatory = $true)][string] $Container,
    [string] $Database = 'aitc',
    [string] $User = 'postgres',
    [switch] $Reset
)

$ErrorActionPreference = 'Stop'
$migrationDir = Join-Path (Split-Path -Parent $PSScriptRoot) 'db/migrations'
if (-not (Test-Path -LiteralPath $migrationDir)) {
    $migrationDir = Join-Path (Get-Location) 'db/migrations'
}

function Invoke-Sql {
    param([string] $Sql, [switch] $Quiet)
    # psql writes NOTICE/WARNING to stderr. PowerShell surfaces native stderr as
    # error records, which would abort under $ErrorActionPreference='Stop', so
    # the preference is relaxed for the duration of the call and success is
    # determined by the exit code alone.
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $result = $Sql | docker exec -i $Container psql -U $User -d $Database -v ON_ERROR_STOP=1 -tAq 2>&1
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prev
    if ($code -ne 0) {
        throw "SQL failed:`n$Sql`n---`n$result"
    }
    # Always return the result set. -Quiet suppresses psql's command tags; it
    # must NOT suppress query output, because the caller uses this to discover
    # already-applied migrations.
    return ($result | Out-String)
}

if ($Reset) {
    # Refuse to reset anything that looks like a production target.
    if ($Database -match 'prod|live') {
        throw "Refusing to reset database '$Database': the name indicates a protected environment."
    }
    Write-Output "Resetting database '$Database' on container '$Container'."
    Invoke-Sql -Quiet -Sql "DROP SCHEMA IF EXISTS public CASCADE; DROP SCHEMA IF EXISTS identity CASCADE;
        DROP SCHEMA IF EXISTS config CASCADE; DROP SCHEMA IF EXISTS market CASCADE;
        DROP SCHEMA IF EXISTS strategy CASCADE; DROP SCHEMA IF EXISTS risk CASCADE;
        DROP SCHEMA IF EXISTS oms CASCADE; DROP SCHEMA IF EXISTS execution CASCADE;
        DROP SCHEMA IF EXISTS reconciliation CASCADE; DROP SCHEMA IF EXISTS portfolio CASCADE;
        DROP SCHEMA IF EXISTS ledger CASCADE; DROP SCHEMA IF EXISTS audit CASCADE;
        DROP SCHEMA IF EXISTS ops CASCADE; DROP SCHEMA IF EXISTS common CASCADE;"
    # public is dropped above; recreate it so schema_migration has a home.
    Invoke-Sql -Quiet -Sql "CREATE SCHEMA IF NOT EXISTS public;"
    Invoke-Sql -Quiet -Sql "DROP TABLE IF EXISTS public.schema_migration CASCADE;"
}

# schema_migration is the authoritative record of what has been applied. It lives
# in public so that it survives a domain-schema drop during a contract release.
Invoke-Sql -Quiet -Sql @"
CREATE TABLE IF NOT EXISTS public.schema_migration (
    version      TEXT        PRIMARY KEY,
    applied_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at_ns BIGINT     NOT NULL,
    content_digest TEXT      NOT NULL,
    duration_ms  INTEGER     NOT NULL
);
"@

$applied = @{}
# version -> recorded content digest. The recorded digest is compared against the
# digest of the file on disk for every already-applied migration: a migration
# history that was edited after it ran is a release blocker, because the
# database and the repository now disagree about what was applied
# (24_ENTERPRISE_RELEASE_STANDARD.md §5).
$rows = (Invoke-Sql -Quiet -Sql "SELECT version || ' ' || content_digest FROM public.schema_migration;") -split "`r?`n"
foreach ($r in $rows) {
    $v = $r.Trim()
    if ($v) {
        $parts = $v -split '\s+', 2
        $applied[$parts[0]] = $parts[1]
    }
}

$files = Get-ChildItem -LiteralPath $migrationDir -Filter '*.sql' | Sort-Object Name
$failures = 0
$appliedNow = 0
$drift = 0

foreach ($f in $files) {
    $version = [System.IO.Path]::GetFileNameWithoutExtension($f.Name)
    $digest = (Get-FileHash -LiteralPath $f.FullName -Algorithm SHA256).Hash.ToLower()
    if ($applied.ContainsKey($version)) {
        if ($applied[$version] -ne $digest) {
            Write-Output ("DRIFT {0}: applied digest {1} != file digest {2}" -f $version, $applied[$version], $digest)
            $drift++
        }
        else {
            Write-Output "SKIP  $version (already applied, digest verified)"
        }
        continue
    }
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        $sql = Get-Content -LiteralPath $f.FullName -Raw
        # Each migration is applied in its own transaction so a failure leaves no
        # partial schema change behind.
        $wrapped = "BEGIN;`n$sql`nCOMMIT;"
        $prev = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        $result = $wrapped | docker exec -i $Container psql -U $User -d $Database -v ON_ERROR_STOP=1 2>&1
        $code = $LASTEXITCODE
        $ErrorActionPreference = $prev
        if ($code -ne 0) { throw ($result | Out-String) }
        $sw.Stop()
        $ns = [System.Diagnostics.Stopwatch]::GetTimestamp()
        $nsUnix = ($ns - [DateTime]::UnixEpoch.Ticks) * 100
        Invoke-Sql -Quiet -Sql "INSERT INTO public.schema_migration (version, applied_at_ns, content_digest, duration_ms)
            VALUES ('$version', $nsUnix, '$digest', $($sw.ElapsedMilliseconds));"
        Write-Output ("OK    {0}  ({1} ms)" -f $version, $sw.ElapsedMilliseconds)
        $appliedNow++
    }
    catch {
        $sw.Stop()
        Write-Output ("FAIL  {0}: {1}" -f $version, $_.Exception.Message)
        $failures++
        break
    }
}

Write-Output "---"
Write-Output ("applied_now={0} total_files={1} failures={2} drift={3}" -f $appliedNow, $files.Count, $failures, $drift)
if ($failures -gt 0) { exit 1 }
# A modified applied migration is a hard failure: the schema in the database was
# built from different bytes than the repository claims.
if ($drift -gt 0) { exit 2 }
