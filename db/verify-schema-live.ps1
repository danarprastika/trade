# verify-schema-live.ps1 — compares the live database against the repository's schema.
#
# WHY THIS EXISTS
#
# db/migrate.ps1 verifies the digest of each migration FILE on disk against the ledger
# and reports file drift. It cannot see the opposite direction of divergence: a function
# belonging to an already-applied migration can be replaced in the running database, and
# every subsequent migrate.ps1 run reports "already applied, digest verified" while the
# database no longer matches the repository.
#
# This was measured, not hypothesised. During the 0024/0025 mutation proof,
# oms.guard_submitting_order_has_outbox was replaced with a bare RETURN NULL; the next
# full migrate.ps1 run reported "applied_now=0 total_files=25 failures=0 drift=0" and left
# the neutered function in place. The harness reported perfect agreement while the schema
# was wrong. Nothing in the repository would have caught it.
#
# WHAT IT CHECKS
#
# 1. Function bodies. For every function defined by a migration, this re-derives the body the
#    migration files say the database should contain and compares it against pg_proc.prosrc in
#    the live database. A function redefined by several migrations is compared against its LAST
#    definition, because that is the one applied last and therefore the one that is live.
#
# 2. Triggers. Every declared trigger is checked for existence, for the table it is attached
#    to, for the function it executes, and for being enabled. A missing, repointed, or
#    disabled trigger is reported. Partition-local copies of a parent trigger are excluded,
#    since they are a consequence of the partition layout rather than declared objects.
#
# 3. Indexes. Every declared index is checked for existence, for the table it sits on, for
#    uniqueness, for its key count, for its column names where both sides are unambiguous,
#    and for being valid and ready. Live indexes in repository schemas that no migration
#    declares are reported. Constraint-backed and partition-child indexes are excluded, for
#    reasons measured and recorded in the INDEXES section below.
#
# 4. Named table constraints. Every constraint the migrations name with an explicit
#    CONSTRAINT <name> clause is checked for existence on its declared table, for its type
#    (PRIMARY KEY / UNIQUE / FOREIGN KEY / CHECK / EXCLUDE), and for being validated.
#    A constraint that is NOT VALID in the live database while no migration declares it so
#    is reported, because such a constraint is not enforced for rows that already exist and
#    is otherwise invisible to a declaration-only review. Validation is compared against the
#    DECLARED state, not assumed: this repository deliberately contains one unvalidated
#    constraint (audit.record.audit_signing_key_required, migration 0023) which must NOT be
#    reported. See the CONSTRAINTS section for the full scope and its limits.
#
# SCOPE, STATED PLAINLY
#
# This covers function bodies, triggers, indexes, and named table constraints. It does NOT
# cover columns, enums, or tables.
#
# Specifically still invisible here, and each one is a real gap rather than a caveat:
#   * An UNNAMED constraint (the 46 PRIMARY KEYs and the inline REFERENCES foreign keys whose
#     names PostgreSQL generates). Not reported in either direction. This direction was
#     attempted and abandoned: it falsely accused oms_order_submitting_requires_outbox and
#     feed_observed_sequence_non_negative, both of which ARE declared. Reasoning recorded in
#     the CONSTRAINTS section.
#   * A CHECK constraint's EXPRESSION. Existence, type and validation are compared; the
#     expression text is not, because PostgreSQL's deparse rewrites the expression and a
#     text comparison reports phantom differences on a correct schema.
#   * A FOREIGN KEY's referenced table and columns, and the action taken on delete/update.
#   * An index PREDICATE (WHERE clause) that changed. Not compared: PostgreSQL's deparse of
#     "IN ('a','b')" is "= ANY (ARRAY['a','b'])" with enum type labels attached, which
#     produced 13 false mismatches on 30 correct predicates when it was tried.
#   * An index's SORT DIRECTION or NULLS ordering. Not compared: per-column pg_get_indexdef
#     omits DESC and NULLS FIRST/LAST entirely, which produced 25 false mismatches on 84
#     correct column lists when it was tried.
#   * The two expression-keyed indexes' column identities. Their key COUNT is compared; their
#     key EXPRESSIONS are not, because an expression cannot be compared to deparsed text
#     without reimplementing PostgreSQL's parser. The count is printed on every run.
#   * Columns, enums, and tables themselves.
#
# It is not a general schema-drift gate, and must not be reported as one.
#
# Every dimension here is mutation-proven: each was shown to DETECT a real divergence, not
# merely to report drift=0 on a healthy database. A gate that has only ever been run against
# a correct schema has been shown to do nothing. See the recorded proofs for functions,
# triggers (dropped, repointed, disabled, undeclared, name-collision, parse-incomplete),
# indexes (dropped, repointed, uniqueness lost, wrong columns, invalid, undeclared,
# parse-incomplete) and named constraints (dropped, type changed, silently unvalidated,
# parse-incomplete, plus a negative control proving the deliberate NOT VALID is not flagged).
#
# EXIT CODES
#
#   0  every compared function, trigger, index and named constraint matches
#   1  something differs, is missing, is undeclared, is disabled, is invalid, is
#      unexpectedly unvalidated, or a comparison could not be made (PARSE_INCOMPLETE)

[CmdletBinding()]
param(
    [string] $Container = 'aitc-pg17',
    [string] $Database  = 'aitc',
    [string] $User      = 'postgres'
)

$ErrorActionPreference = 'Stop'
$migrationsDir = Join-Path (Split-Path -Parent $PSScriptRoot) 'db/migrations'
$files = Get-ChildItem -Path $migrationsDir -Filter '*.sql' | Sort-Object Name

# name -> @{ File; Body }
$expected = [ordered]@{}
$dropped = [ordered]@{}
$declaredTotal = 0

# The header between the name and AS $tag$ is matched with a TEMPERED token rather than
# a [^$] character class. A previous version used [^$]*?, which forbids a '$' before the
# body delimiter -- and so silently failed to match any function whose SIGNATURE contains
# one. config.assert_no_embedded_secrets(p_document JSONB, p_path TEXT DEFAULT '$') in
# 0017 is exactly such a function, and the gate dropped it from its comparison without
# saying so: it reported "expected_functions=62" when the files declare 63, and reported
# drift=0 while that function was live holding the corrupted body described in
# verified.nonAsciiEncodingDefect. A gate that under-reports what it looked at is worse
# than no gate, because its green result is then read as coverage.
#
# The tempered token permits '$' in the header while refusing to run past the start of
# another CREATE OR REPLACE FUNCTION, which is what stops a lazy match from pairing one
# function's name with a later function's body.
#
# The dollar-quote tag is captured and closed with a backreference, so a $tag$ body is
# delimited correctly rather than being cut at the first '$$'.
$pattern = 'CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+([A-Za-z0-9_.]+)(?:(?!CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION)[\s\S])*?\bAS\s+\$(?<tag>[A-Za-z0-9_]*)\$(?<body>[\s\S]*?)\$\k<tag>\$'
$declarePattern = 'CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION'

# A function a LATER migration deliberately drops is not drift -- it is the schema doing
# what the repository says. ledger.assert_balanced(TEXT,TEXT) and
# ledger.assert_journal_balanced() were no-op stubs that 0008 removed in favour of
# ledger.verify_journal and ledger.journal_must_balance, so a verifier that only reads
# CREATE statements reports them as missing functions on a database that is entirely
# correct. Migration 0008 documents the reason at lines 23-24.
$dropPattern = 'DROP\s+FUNCTION\s+(?:IF\s+EXISTS\s+)?([A-Za-z0-9_.]+)'

# =========================================================================================
# SQL COMMENT STRIPPING
#
# WHY: a comment that mentions DDL is indistinguishable from DDL to a regular expression,
# and it silently corrupts every count derived from these files. Measured, not assumed:
# db/migrations/0026_outbox_delete_guard_and_aggregate_id_index.sql mentions
# "CREATE INDEX" three times but declares one index -- the other two occurrences are inside
# comments. An index section that counted mentions on raw text would have declared 87,
# parsed 85, and failed closed with PARSE_INCOMPLETE forever on a correct repository, or,
# had it compared names, reported 2 phantom indexes as missing.
#
# WHY IT MUST BE LEXICAL, NOT A BLANK-OUT REGEX:
#
#   A naive `--.*` strip corrupts every function body that contains a comment. A function
#   body is a dollar-quoted string, but its CONTENTS are SQL, so a '--' line inside it is a
#   real comment in the database and is stored verbatim in pg_proc.prosrc. Blanking it
#   changes the body the function comparison compares -- correct migrations then read as
#   drift. Measured: a tag-only tokeniser blanked those lines and produced 31 phantom
#   function-drift reports on a database that was entirely correct.
#
#   The fix is to consume the ENTIRE dollar-quoted region as one token, closing on a
#   backreference to the opening tag. A regex scanner that merely MATCHES the opening
#   "$tag$" does not skip the body -- it resumes immediately after it -- which is why the
#   first attempt failed. Consuming the whole region means the scanner resumes after the
#   closing tag and can never treat a '--' inside a body as a comment.
#
#   Consequence, and it is the desired one: comments INSIDE a dollar-quoted body are left
#   untouched. That is not a miss. prosrc contains them, so the body must contain them.
#
# VERIFIED, not assumed: stripping changes zero of the 66 parsed function bodies
# (byte-identical hashes before and after) while reducing 0026's "CREATE INDEX" mentions
# from 3 to 1 -- the exact reduction this section exists to achieve.
#
# The tokeniser also handles single-quoted strings and double-quoted identifiers as atomic
# tokens, so a '--' inside a RAISE NOTICE message survives, and allows an empty dollar tag
# ($$), which is how nearly every function body in this repository is written.
# =========================================================================================
function Remove-SqlComments {
    param([string] $Text)
    $tokenRx = [regex]'(?s)--[^\n]*|/\*[\s\S]*?\*/|\$(?<t>[A-Za-z0-9_]*)\$[\s\S]*?\$\k<t>\$|''(?:[^'']|'''')*''|"(?:[^"]|"")*"'
    return $tokenRx.Replace($Text, {
        param($m)
        if ($m.Value.StartsWith('--') -or $m.Value.StartsWith('/*')) {
            # Blanked, not deleted: preserves newlines and therefore line offsets.
            return ($m.Value -replace '[^\n]', ' ')
        }
        return $m.Value
    })
}

# Read every migration once, with comments removed, and reuse that text for all three
# dimensions. Reading it raw three times is what let the 0026 comment reach the index
# parser in the first place.
$cleanFiles = @(
    foreach ($file in $files) {
        $raw = [System.IO.File]::ReadAllText($file.FullName)
        [pscustomobject]@{
            Name  = $file.Name
            Clean = (Remove-SqlComments $raw)
        }
    }
)

foreach ($file in $cleanFiles) {
    $text = $file.Clean
    $declaredTotal += ([regex]::Matches($text, $declarePattern)).Count
    foreach ($m in [regex]::Matches($text, $pattern)) {
        # Last definition wins: migrations are applied in filename order, so the final
        # definition in filename order is the definition that ends up live.
        $expected[$m.Groups[1].Value] = @{
            File = $file.Name
            Body = $m.Groups['body'].Value
        }
        $dropped.Remove($m.Groups[1].Value)
    }
    foreach ($m in [regex]::Matches($text, $dropPattern)) {
        # Only a drop AFTER the last create leaves the function absent. Processing in
        # filename order makes that ordering fall out of the loop, and re-creating a
        # previously dropped function clears the record above.
        $expected.Remove($m.Groups[1].Value)
        $dropped[$m.Groups[1].Value] = $file.Name
    }
}

# Every CREATE [OR REPLACE] FUNCTION in the files must have produced a parse. If this does
# not hold, the gate is comparing fewer things than the repository declares and its
# result must not be reported as coverage. This check exists because the under-parse it
# catches happened once already, silently.
#
# KNOWN LIMITATION, PROVEN BY TEST rather than assumed: a function whose body is written as
# a single-quoted string -- AS 'SELECT 1' rather than AS $$ ... $$ -- is valid PostgreSQL
# and is NOT parseable by the pattern above, so introducing one makes this check fail
# closed with PARSE_INCOMPLETE rather than silently skipping it. That is the intended
# direction of failure. No migration in this repository uses that form; all 74 definitions
# are dollar-quoted.
$parsedTotal = 0
foreach ($file in $cleanFiles) {
    $parsedTotal += ([regex]::Matches($file.Clean, $pattern)).Count
}
if ($parsedTotal -ne $declaredTotal) {
    Write-Output "PARSE_INCOMPLETE: the migration files declare $declaredTotal CREATE [OR REPLACE] FUNCTION statements but only $parsedTotal could be parsed."
    Write-Output 'The comparison below would silently omit the rest, so no verdict is issued.'
    exit 1
}

if ($expected.Count -eq 0) {
    Write-Output 'no function definitions found in the migration files'
    exit 1
}

function Invoke-Psql {
    param([string] $Sql)
    $out = & docker exec $Container psql -U $User -d $Database -tAq -c $Sql 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "psql failed: $($out -join ' ')"
    }
    return ($out | Out-String).Trim()
}

# md5 of the text with line endings normalised, because prosrc's newline convention is
# PostgreSQL's to choose and the file's is not. Comparing raw bytes would produce a false
# drift report on a schema that is in fact identical.
function Get-BodyHash {
    param([string] $Body)
    $normalised = $Body -replace "`r`n", "`n"
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($normalised)
    $md5 = [System.Security.Cryptography.MD5]::Create()
    try {
        return ([BitConverter]::ToString($md5.ComputeHash($bytes))).Replace('-', '').ToLower()
    } finally {
        $md5.Dispose()
    }
}

$drift = @()
$missing = @()
$checked = 0

foreach ($name in $expected.Keys) {
    $schema, $fname = $name.Split('.')
    # The hash is computed INSIDE the database. Fetching prosrc as text and hashing it
    # in PowerShell loses the difference between a body and the same body with its
    # trailing newline removed -- psql adds a line ending of its own and any trimming of
    # the result silently changes the hash, which reports every single function as
    # drifted when the schema is in fact identical. Comparing hashes keeps the value on
    # one side of the wire, where it belongs.
    #
    # Newlines are normalised on both sides: the file's line-ending convention is the
    # checkout's, not the database's, and a CRLF checkout must not read as drift.
    $live = Invoke-Psql @"
SELECT coalesce(md5(replace(p.prosrc, E'\r\n', E'\n')), '')
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = '$schema' AND p.proname = '$fname';
"@
    if ([string]::IsNullOrEmpty($live)) {
        $missing += $name
        continue
    }
    $checked++
    if ($live -ne (Get-BodyHash $expected[$name].Body)) {
        $drift += [pscustomobject]@{
            Function = $name
            Defined  = $expected[$name].File
        }
    }
}

Write-Output "compared_functions=$checked expected_functions=$($expected.Count) dropped_by_later_migration=$($dropped.Count) declared_by_files=$declaredTotal"

# A function that is live but defined by no migration is drift in the other direction,
# and nothing else in this script would notice it: the loop only visits functions the
# files declare. An object added live and never committed to the repository is precisely
# the sort of change the digest check cannot see.
#
# Restricted to the schemas the repository owns, and excluding anything owned by an
# extension, so that functions installed by pgcrypto or any other extension into public
# are not reported as repository drift.
$schemas = $expected.Keys |
    ForEach-Object { $_.Split('.')[0] } |
    Select-Object -Unique |
    Where-Object { $_ -ne 'public' }
if ($schemas.Count -gt 0) {
    $schemaList = ($schemas | ForEach-Object { "'$_'" }) -join ','
    $extra = Invoke-Psql @"
SELECT n.nspname || '.' || p.proname
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
  LEFT JOIN pg_depend d ON d.objid = p.oid AND d.deptype = 'e'
 WHERE n.nspname IN ($schemaList)
   AND d.objid IS NULL
   AND (n.nspname || '.' || p.proname) <> ALL (ARRAY[$(
        ($expected.Keys | ForEach-Object { "'$_'" }) -join ',')]::text[])
 ORDER BY 1;
"@
    if (-not [string]::IsNullOrEmpty($extra)) {
        $extras = @($extra -split "`n" | Where-Object { $_.Trim() })
    } else {
        $extras = @()
    }
} else {
    $extras = @()
}
Write-Output "---"

if ($missing.Count -gt 0) {
    Write-Output "MISSING_IN_DATABASE ($($missing.Count)) -- the migration files define these but the database has no such function:"
    $missing | ForEach-Object { Write-Output "  $_" }
}

if ($drift.Count -gt 0) {
    Write-Output "LIVE_DRIFT ($($drift.Count)) -- the database's function body differs from the repository:"
    $drift | ForEach-Object { Write-Output "  $($_.Function)  (defined in $($_.Defined))" }
}

if ($extras.Count -gt 0) {
    Write-Output "UNDECLARED_IN_DATABASE ($($extras.Count)) -- live functions that no migration defines:"
    $extras | ForEach-Object { Write-Output "  $($_.Trim())" }
}

# =========================================================================================
# TRIGGERS
#
# A trigger is a guard. Losing one does not raise an error on the next write -- it silently
# stops enforcing the rule it enforced, which is precisely the failure mode this script
# exists to catch. A dropped oms_order_submitting_requires_outbox, for example, leaves the
# database reporting healthy while the invariant it protected is no longer checked.
#
# MEASURED FACTS THIS SECTION DEPENDS ON, rather than assumptions:
#
#   * The migrations declare 27 triggers: 22 "CREATE TRIGGER" and 5 "CREATE CONSTRAINT
#     TRIGGER". Both forms must be matched. A first attempt used only CREATE\s+TRIGGER and
#     silently failed to parse the 5 constraint triggers, which would have reported them as
#     UNDECLARED_IN_DATABASE -- a false accusation against a correct database, and a gate
#     whose failure output cannot be trusted.
#
#   * Quoted table names occur: 0024 declares the trigger ON oms."order". A parser that
#     accepts only bare identifiers misses that trigger for the same reason.
#
#   * audit.record is partitioned into 15 partitions, and a trigger created on a partitioned
#     parent is REPLICATED onto every partition. The live database therefore holds 57
#     non-internal triggers, not 27: the 27 above, plus 30 partition-local copies of the two
#     audit.record triggers. Those copies are an automatic consequence of the parent's
#     existence, not separately declared objects, so they are excluded via relispartition.
#     Counting them would report 30 phantom differences on a perfectly correct database.
#
#   * Exactly one DROP TRIGGER exists -- ledger_entry_journal_balanced_stub, removed by 0008
#     along with the no-op function it called. That stub is never created by any migration in
#     this repository, so the drop removes nothing and 27 creates yield 27 live triggers. The
#     drop is processed anyway: if a future migration recreates that name, the ordering must
#     still be correct.
# =========================================================================================

# Statement-level match up to the terminating semicolon, then pull the three facts out of the
# statement. A fixed-width window is not used here: it silently truncates on long
# "UPDATE OF <column list>" headers, which is how the constraint triggers were first missed.
$triggerStatement = 'CREATE\s+(?:CONSTRAINT\s+)?TRIGGER\b[\s\S]*?;'
$triggerDeclare  = 'CREATE\s+(?:CONSTRAINT\s+)?TRIGGER\b'
# The leading \b keeps a bare-name form from matching the ON-clause of a DROP TRIGGER
# ... ON tbl form. Both wordings occur: 0026 issues "DROP TRIGGER IF EXISTS <name> ON
# ops.outbox" as part of making its own no-transaction migration re-runnable, and
# matching that ON clause as a trigger name would register "ops.outbox" as a trigger
# name that 0026 then "drops", which showed up as a spurious undeclared-trigger
# report naming the trigger 0026 had just created.
$triggerDrop     = '\bDROP\s+TRIGGER\s+(?:IF\s+EXISTS\s+)?([A-Za-z0-9_]+)'

# Ordered scan. Both alternatives run to their terminating semicolon so that the match positions
# follow the order the statements appear in, and so a CREATE ... TRIGGER header is never cut short
# by an earlier statement. $triggerDropAnchor only classifies a matched statement as a drop; the
# name itself still comes from $triggerDrop so the two can never drift.
$triggerOrdered      = '(?:CREATE\s+(?:CONSTRAINT\s+)?TRIGGER\b[\s\S]*?|\bDROP\s+TRIGGER\b[\s\S]*?);'
$triggerDropAnchor   = '^\s*DROP\s+TRIGGER\b'

# The ON clause is required to be schema-qualified. That is not a stylistic choice: it stops
# the pattern from matching a column literally named "on" inside an "UPDATE OF" list.
$triggerOn = '\bON\s+([A-Za-z0-9_]+(?:\."?[A-Za-z0-9_]+"?)?)'

# oms."order" and oms.order name the same relation; the quotes are quoting style, not part of
# the identity. Both sides are normalised so the comparison is about the object, not spelling.
function Normalise-Relation {
    param([string] $Name)
    return ($Name -replace '"', '').ToLower()
}

$expectedTriggers = @{}
$droppedTriggers  = [ordered]@{}
$triggerDeclaredTotal = 0
$triggerParsedTotal    = 0

foreach ($file in $cleanFiles) {
    $text = $file.Clean
    $triggerDeclaredTotal += ([regex]::Matches($text, $triggerDeclare)).Count
    # Statements are walked in file order, not "all CREATEs then all DROPs". A migration may drop a
    # trigger it is about to replace in the same file (0026 makes its own no-transaction migration
    # re-runnable that way), and the live database is left holding the replacement. Grouping the
    # two passes per file let the trailing DROP erase a trigger that was declared moments earlier,
    # which is what produced the false "declared but missing from the database" finding.
    foreach ($m in [regex]::Matches($text, $triggerOrdered)) {
        $stmt = $m.Value
        if ($stmt -match $triggerDropAnchor) {
            $dname = ([regex]::Match($stmt, $triggerDrop)).Groups[1].Value.ToLower()
            if ([string]::IsNullOrEmpty($dname)) {
                continue
            }
            $expectedTriggers.Remove($dname)
            $droppedTriggers[$dname] = $file.Name
            continue
        }
        $tname  = ([regex]::Match($stmt, 'CREATE\s+(?:CONSTRAINT\s+)?TRIGGER\s+([A-Za-z0-9_]+)').Groups[1].Value).ToLower()
        $ttable = [regex]::Match($stmt, $triggerOn).Groups[1].Value
        $tfunc  = [regex]::Match($stmt, 'EXECUTE\s+(?:FUNCTION|PROCEDURE)\s+([A-Za-z0-9_.]+)').Groups[1].Value
        if ([string]::IsNullOrEmpty($tname) -or [string]::IsNullOrEmpty($ttable) -or [string]::IsNullOrEmpty($tfunc)) {
            continue
        }
        $triggerParsedTotal++
        $expectedTriggers[$tname] = [pscustomobject]@{
            Name   = $tname
            Table  = (Normalise-Relation $ttable)
            Func   = $tfunc
            File   = $file.Name
        }
        [void] $droppedTriggers.Remove($tname)
    }
}

# Same fail-closed rule as functions: if a declared trigger did not parse, this section is
# comparing less than the repository declares and must not report a verdict.
if ($triggerParsedTotal -ne $triggerDeclaredTotal) {
    Write-Output "PARSE_INCOMPLETE: the migration files declare $triggerDeclaredTotal CREATE [CONSTRAINT] TRIGGER statements but only $triggerParsedTotal could be parsed."
    exit 1
}

$triggerDrift   = @()
$triggerMissing = @()
$triggerExtra   = @()
$triggerDisabled = @()
$triggerChecked  = 0

if ($expectedTriggers.Count -gt 0) {
    $liveTriggersRaw = Invoke-Psql @"
SELECT n.nspname || '.' || c.relname || '|' || t.tgname || '|' || pn.nspname || '.' || p.proname || '|' || t.tgenabled::text
  FROM pg_trigger t
  JOIN pg_class c ON c.oid = t.tgrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_proc p ON p.oid = t.tgfoid
  JOIN pg_namespace pn ON pn.oid = p.pronamespace
 WHERE NOT t.tgisinternal
   AND NOT c.relispartition
 ORDER BY 1;
"@
    $liveTriggerMap = @{}
    # name -> the tables carrying it. PostgreSQL makes a trigger name unique per TABLE, not
    # per schema, so a single name can legitimately be live on two relations at once.
    $liveTriggerTables = @{}
    if (-not [string]::IsNullOrEmpty($liveTriggersRaw)) {
        foreach ($line in ($liveTriggersRaw -split "`n")) {
            if ([string]::IsNullOrWhiteSpace($line)) { continue }
            $parts = $line.Trim() -split '\|'
            if ($parts.Count -lt 4) { continue }
            $table = Normalise-Relation $parts[0]
            $name  = $parts[1].ToLower()
            # Keyed by table AND name, deliberately.
            #
            # An earlier version keyed this map by trigger name alone. Because the query is
            # ORDER BY table, a second live trigger sharing a declared trigger's name on a
            # different table was written first and then silently OVERWRITTEN by the
            # declared one. Measured consequence: adding an undeclared trigger to oms.fill
            # named oms_order_state_machine left the gate reporting drift=0 and exit 0 while
            # the database carried an extra, unreviewed guard. That is the same
            # under-reporting defect this gate already committed once with a function its
            # pattern failed to parse, and it is why the key carries the table.
            $liveTriggerMap[$table + '|' + $name] = [pscustomobject]@{
                Table = $table
                Name  = $name
                Func  = $parts[2]
                Enabled = $parts[3]
            }
            if (-not $liveTriggerTables.ContainsKey($name)) {
                $liveTriggerTables[$name] = New-Object System.Collections.ArrayList
            }
            [void] $liveTriggerTables[$name].Add($table)
        }
    }

    foreach ($tname in $expectedTriggers.Keys) {
        $exp = $expectedTriggers[$tname]
        $key = $exp.Table + '|' + $tname
        if (-not $liveTriggerMap.ContainsKey($key)) {
            if ($liveTriggerTables.ContainsKey($tname)) {
                # The name exists but on a different relation. Reported as drift with the
                # actual table named, because "the guard is on the wrong table" and "the
                # guard is absent" call for different fixes.
                $triggerDrift += [pscustomobject]@{
                    Trigger = $tname; Field = 'table'
                    Expected = $exp.Table; Actual = ($liveTriggerTables[$tname] -join ',')
                    File = $exp.File
                }
                continue
            }
            $triggerMissing += $exp
            continue
        }
        $live = $liveTriggerMap[$key]
        $triggerChecked++
        if ($live.Func -ne $exp.Func) {
            $triggerDrift += [pscustomobject]@{
                Trigger = $tname; Field = 'function'
                Expected = $exp.Func; Actual = $live.Func; File = $exp.File
            }
        }
        # tgenabled is reported separately from drift because the failure is quieter than a
        # mismatch: 'D' (disabled) leaves the trigger present and every name matching, while
        # the rule it enforces stops being enforced.
        if ($live.Enabled -ne 'O') {
            $triggerDisabled += [pscustomobject]@{
                Trigger = $tname; Enabled = $live.Enabled; File = $exp.File
            }
        }
    }

    # Any live trigger whose (table, name) pair is not one the migrations declare. A name
    # that IS declared elsewhere still lands here, and that is the point: an undeclared
    # guard is unreviewed regardless of what it calls itself, and reporting it only when its
    # name is entirely unknown is what let the shadowed case pass silently.
    foreach ($key in $liveTriggerMap.Keys) {
        $sep = $key.LastIndexOf('|')
        $lTable = $key.Substring(0, $sep)
        $lName  = $key.Substring($sep + 1)
        $declaredHere = $expectedTriggers.ContainsKey($lName) -and $expectedTriggers[$lName].Table -eq $lTable
        if (-not $declaredHere) {
            $live = $liveTriggerMap[$key]
            $triggerExtra += [pscustomobject]@{
                Trigger = $lName; Table = $lTable; Func = $live.Func
                Shadows = ($expectedTriggers.ContainsKey($lName))
            }
        }
    }
}

Write-Output "compared_triggers=$triggerChecked expected_triggers=$($expectedTriggers.Count) dropped_by_later_migration=$($droppedTriggers.Count) declared_by_files=$triggerDeclaredTotal"

if ($triggerMissing.Count -gt 0) {
    Write-Output "MISSING_TRIGGER ($($triggerMissing.Count)) -- the migration files define these but the database has no such trigger:"
    $triggerMissing | ForEach-Object { Write-Output "  $($_.Name) on $($_.Table) -> $($_.Func)  (defined in $($_.File))" }
}

if ($triggerDrift.Count -gt 0) {
    Write-Output "TRIGGER_DRIFT ($($triggerDrift.Count)) -- the live trigger's $($triggerDrift[0].Field) differs from the repository:"
    $triggerDrift | ForEach-Object { Write-Output "  $($_.Trigger): expected $($_.Expected), live $($_.Actual)  (defined in $($_.File))" }
}

if ($triggerDisabled.Count -gt 0) {
    Write-Output "TRIGGER_DISABLED ($($triggerDisabled.Count)) -- the trigger exists but is not enabled (tgenabled <> 'O'):"
    $triggerDisabled | ForEach-Object { Write-Output "  $($_.Trigger) tgenabled=$($_.Enabled)  (defined in $($_.File))" }
}

if ($triggerExtra.Count -gt 0) {
    Write-Output "UNDECLARED_TRIGGER ($($triggerExtra.Count)) -- live triggers that no migration places where they are:"
    $triggerExtra | ForEach-Object {
        $note = if ($_.Shadows) { '  [shares a name with a declared trigger on another table]' } else { '' }
        Write-Output "  $($_.Trigger) on $($_.Table) -> $($_.Func)$note"
    }
}

# =========================================================================================
# INDEXES
#
# An index is not a guard the way a trigger is, but losing one is the same class of silent
# divergence: nothing errors, queries merely get slower, and a UNIQUE index that silently
# stops being unique removes a database-enforced invariant while the application still
# believes it holds. The digest check in migrate.ps1 cannot see either.
#
# MEASURED FACTS THIS SECTION DEPENDS ON, all established before the code was written:
#
#   * The migrations declare 85 CREATE [UNIQUE] INDEX statements; one index
#     (config_revision_active_idx) is dropped by 0017, so 84 are expected live.
#
#   * The live database holds 84 standalone non-constraint indexes in the 12 schemas this
#     repository owns (audit, config, execution, identity, ledger, market, oms, ops,
#     portfolio, reconciliation, risk, strategy), matching the expected set name for name:
#     0 missing, 0 undeclared, 0 table mismatches, 0 uniqueness mismatches.
#
#   * The live database ALSO holds 96 constraint-backed indexes and 105 indexes on partition
#     children. Both are excluded, deliberately and for different reasons:
#       - constraint-backed indexes back a PRIMARY KEY or UNIQUE constraint declared inside
#         CREATE TABLE/ALTER TABLE. They are not declared by CREATE INDEX, so counting them
#         would report 96 phantom undeclared indexes. Excluded via pg_constraint.conindid.
#       - an index on a partition is an automatic replica of its parent's index, exactly as
#         a partitioned table's triggers are replicas of the parent's triggers. Excluded via
#         relispartition, the same exclusion the trigger section uses. Counting them would
#         report 105 phantom differences.
#
#   * Index names are unique per SCHEMA, not globally. Measured: state_transition_rule_pkey
#     exists in both oms and strategy. The live map is therefore keyed by schema AND name,
#     repeating the lesson the trigger map already paid for: a map keyed by name alone
#     silently overwrites collisions and under-reports.
#
# WHAT IS COMPARED, AND WHY IT IS EXACTLY THIS MUCH
#
#   Compared: name, schema, table, uniqueness, key count, column names, and validity.
#
#   Deliberately NOT compared: the index predicate (WHERE clause) and sort direction.
#   Those were measured before being excluded, because the obvious implementation
#   -- compare the declared text against pg_get_indexdef -- produces FALSE DRIFT ON A
#   CORRECT DATABASE, 38 times over:
#     - pg_get_indexdef(idx, colno, true) omits DESC and NULLS FIRST/LAST entirely, so 25 of
#       84 column lists compared unequal;
#     - "IN ('a','b')" deparses to "= ANY (ARRAY['a', 'b'])", and the parser adds explicit
#       parentheses, so 13 of 30 predicates compared unequal;
#     - enum literals gain a type label: 'ACTIVE' deparses as 'ACTIVE'.session_state;
#     - redundant parens around a single expression differ after stripping.
#   A gate that reports 38 phantom differences on a correct schema is worse than no gate,
#   because its green result stops being believed. Key count and column NAMES are compared
#   instead, both of which measured 0 mismatches and catch the realistic failure -- an index
#   rebuilt on the wrong columns. Sort direction and predicate remain an acknowledged gap,
#   stated as such below rather than claimed as covered.
#
#   The two expression-keyed indexes (feature_flag_scope_key_idx, live_activation_single_active_idx)
#   cannot have their column names compared and are EXCLUDED FROM THE NAME CHECK, not
#   guessed at. Their key COUNT is still compared. The count of name-checked indexes is
#   printed so this gate never over-reports what it examined -- the defect it has already
#   committed once, when a function its parser could not match was silently omitted.
# =========================================================================================

# Statement runs to the terminating semicolon so a multi-line column list and a trailing
# WHERE clause are captured together.
$indexStatement = 'CREATE\s+(?<u>UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?"?(?<ident>[A-Za-z0-9_]+)"?\s+ON\s+(?<table>[A-Za-z0-9_]+(?:\."?[A-Za-z0-9_]+"?)?)\s*(?<cols>\([^;]*?\))(?<rest>[^;]*);'
      $indexDeclare  = 'CREATE\s+(?:UNIQUE\s+)?INDEX\b'
      $indexDrop     = 'DROP\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?(?:"?[A-Za-z0-9_]+"?\.)?"?(?<ident>[A-Za-z0-9_]+)"?'

# A declared key that is a bare column, optionally qualified by ordering. Anything carrying
# an operator, paren, cast or quote is an expression and is excluded from the name check.
$simpleIndexKey = '^(?<col>"?[A-Za-z_][A-Za-z0-9_]*"?)(?:\s+(?:ASC|DESC))?(?:\s+NULLS\s+(?:FIRST|LAST))?$'

# Split a declared column list on TOP-LEVEL commas only, so an expression key containing
# internal commas is not torn apart. Depth-tracked rather than a naive string split.
function Split-TopLevelCommas {
    param([string] $List)
    $inner = $List.Trim()
    if ($inner.StartsWith('(')) { $inner = $inner.Substring(1) }
    if ($inner.EndsWith(')')) { $inner = $inner.Substring(0, $inner.Length - 1) }
    $parts = New-Object System.Collections.ArrayList
    $depth = 0
    $buf = ''
    foreach ($ch in $inner.ToCharArray()) {
        if ($ch -eq '(') { $depth++ } elseif ($ch -eq ')') { $depth-- }
        if ($ch -eq ',' -and $depth -eq 0) { [void] $parts.Add($buf); $buf = '' } else { $buf += $ch }
    }
    if ($buf.Trim()) { [void] $parts.Add($buf) }
    return @($parts)
}

$expectedIndexes = @{}
$droppedIndexes  = [ordered]@{}
$indexDeclaredTotal = 0
$indexParsedTotal    = 0

foreach ($file in $cleanFiles) {
    $text = $file.Clean
    $indexDeclaredTotal += ([regex]::Matches($text, $indexDeclare)).Count

    # Creates first, then this file's drops. Across files, filename order applies, so both
    # "created then dropped in one file" and "dropped by a later file" fall out of the loop
    # with no special case.
    foreach ($m in [regex]::Matches($text, $indexStatement)) {
        $iname = $m.Groups['ident'].Value.ToLower()
        $itable = Normalise-Relation $m.Groups['table'].Value
        $ischema = ($itable -split '\.')[0]
        $ikey = $ischema + '|' + $iname
        $expectedIndexes[$ikey] = [pscustomobject]@{
            Name   = $iname
            Schema = $ischema
            Table  = $itable
            Unique = (-not [string]::IsNullOrWhiteSpace($m.Groups['u'].Value))
            Cols   = $m.Groups['cols'].Value
            File   = $file.Name
        }
        $indexParsedTotal++
        [void] $droppedIndexes.Remove($ikey)
    }
    foreach ($m in [regex]::Matches($text, $indexDrop)) {
        $dname = $m.Groups['ident'].Value.ToLower()
        # A drop may be schema-qualified or bare. Try the bare form first, then each declared
        # schema, so a bare DROP INDEX matches whichever schema declared that name.
        $removed = $false
        foreach ($k in @($expectedIndexes.Keys)) {
            if ($k.EndsWith('|' + $dname)) {
                $expectedIndexes.Remove($k)
                $droppedIndexes[$k] = $file.Name
                $removed = $true
                break
            }
        }
        if (-not $removed) { $droppedIndexes["?|$dname"] = $file.Name }
    }
}

# Same fail-closed rule as functions and triggers: a declared index that did not parse means
# this section compares less than the repository declares, so it must not report a verdict.
if ($indexParsedTotal -ne $indexDeclaredTotal) {
    Write-Output "PARSE_INCOMPLETE: the migration files declare $indexDeclaredTotal CREATE [UNIQUE] INDEX statements but only $indexParsedTotal could be parsed."
    Write-Output 'The index comparison below would silently omit the rest, so no verdict is issued.'
    exit 1
}

$indexDrift     = @()
$indexMissing   = @()
$indexExtra     = @()
$indexInvalid   = @()
$indexChecked   = 0
$indexNameChecked = 0
$indexNameSkipped = 0

if ($expectedIndexes.Count -gt 0) {
    # Only the schemas this repository's indexes actually live in. Scanning every
    # non-system schema would report another service's objects as repository drift.
    $indexSchemas = @($expectedIndexes.Values | ForEach-Object { $_.Schema } | Select-Object -Unique | Sort-Object)
    $indexSchemaList = ($indexSchemas | ForEach-Object { "'$_'" }) -join ','

    $liveIndexesRaw = Invoke-Psql @"
SELECT n.nspname || '|' || ic.relname || '|' || tn.nspname || '.' || tc.relname || '|' ||
       CASE WHEN i.indisunique THEN 'u' ELSE 'n' END || '|' ||
       CASE WHEN i.indisvalid AND i.indisready THEN '1' ELSE '0' END || '|' ||
       i.indnatts::text || '|' ||
       (SELECT coalesce(string_agg(coalesce(a.attname, '<expr>'), ' ;; ' ORDER BY k), '')
          FROM generate_series(1, i.indnatts) AS k
          LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[k - 1])
  FROM pg_index i
  JOIN pg_class ic ON ic.oid = i.indexrelid
  JOIN pg_class tc ON tc.oid = i.indrelid
  JOIN pg_namespace n ON n.oid = ic.relnamespace
  JOIN pg_namespace tn ON tn.oid = tc.relnamespace
 WHERE n.nspname IN ($indexSchemaList)
   AND NOT tc.relispartition
   AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = i.indexrelid)
 ORDER BY 1;
"@
    $liveIndexMap = @{}
    if (-not [string]::IsNullOrEmpty($liveIndexesRaw)) {
        foreach ($line in ($liveIndexesRaw -split "`n")) {
            if ([string]::IsNullOrWhiteSpace($line)) { continue }
            $p = $line.Trim() -split '\|'
            if ($p.Count -lt 7) { continue }
            $schema = $p[0].ToLower()
            $liveIndexMap[$schema + '|' + $p[1].ToLower()] = [pscustomobject]@{
                Schema   = $schema
                Name     = $p[1].ToLower()
                Table    = $p[2].ToLower()
                Unique   = ($p[3] -eq 'u')
                Valid    = ($p[4] -eq '1')
                KeyCount = [int] $p[5]
                Keys     = $p[6]
            }
        }
    }

    foreach ($key in $expectedIndexes.Keys) {
        $exp = $expectedIndexes[$key]
        if (-not $liveIndexMap.ContainsKey($key)) {
            $indexMissing += $exp
            continue
        }
        $live = $liveIndexMap[$key]
        $indexChecked++

        if ($live.Table -ne $exp.Table) {
            $indexDrift += [pscustomobject]@{
                Index = $exp.Name; Field = 'table'
                Expected = $exp.Table; Actual = $live.Table; File = $exp.File
            }
        }
        # A UNIQUE index that is no longer unique is an integrity failure, not a tuning one,
        # so it is reported as its own field rather than folded into a generic mismatch.
        if ($live.Unique -ne $exp.Unique) {
            $indexDrift += [pscustomobject]@{
                Index = $exp.Name; Field = 'uniqueness'
                Expected = $exp.Unique; Actual = $live.Unique; File = $exp.File
            }
        }

        $declaredKeys = @(Split-TopLevelCommas $exp.Cols)
        if ($declaredKeys.Count -ne $live.KeyCount) {
            $indexDrift += [pscustomobject]@{
                Index = $exp.Name; Field = 'key count'
                Expected = $declaredKeys.Count; Actual = $live.KeyCount; File = $exp.File
            }
        }

        # Column NAMES, only when both sides are unambiguous: every declared key a bare
        # identifier, and no live key an expression. Otherwise the index is counted as not
        # name-checked rather than compared on a guess.
        $declaredNames = @()
        $allSimple = $true
        foreach ($k in $declaredKeys) {
            $m = [regex]::Match($k.Trim(), $simpleIndexKey)
            if (-not $m.Success) { $allSimple = $false; break }
            $declaredNames += ($m.Groups['col'].Value -replace '"', '').ToLower()
        }
        if ($allSimple -and ($live.Keys -notmatch '<expr>')) {
            $indexNameChecked++
            $liveNames = @($live.Keys -split ' ;; ' | ForEach-Object { $_.Trim().ToLower() } | Where-Object { $_ })
            if (($declaredNames -join ' ;; ') -ne ($liveNames -join ' ;; ')) {
                $indexDrift += [pscustomobject]@{
                    Index = $exp.Name; Field = 'columns'
                    Expected = ($declaredNames -join ', '); Actual = $live.Keys; File = $exp.File
                }
            }
        } else {
            $indexNameSkipped++
        }

        # indisvalid=false is what a failed CREATE INDEX CONCURRENTLY leaves behind: the
        # index exists, is named correctly, and is silently ignored by the planner.
        if (-not $live.Valid) {
            $indexInvalid += [pscustomobject]@{ Index = $exp.Name; File = $exp.File }
        }
    }

    foreach ($key in $liveIndexMap.Keys) {
        if (-not $expectedIndexes.ContainsKey($key)) {
            $live = $liveIndexMap[$key]
            $indexExtra += [pscustomobject]@{ Index = $live.Name; Schema = $live.Schema; Table = $live.Table }
        }
    }
}

Write-Output "compared_indexes=$indexChecked expected_indexes=$($expectedIndexes.Count) dropped_by_later_migration=$($droppedIndexes.Count) declared_by_files=$indexDeclaredTotal"
Write-Output "index_column_names_compared=$indexNameChecked skipped_as_expression=$indexNameSkipped"

if ($indexMissing.Count -gt 0) {
    Write-Output "MISSING_INDEX ($($indexMissing.Count)) -- the migration files define these but the database has no such index:"
    $indexMissing | ForEach-Object { Write-Output "  $($_.Schema).$($_.Name) on $($_.Table)  (defined in $($_.File))" }
}

if ($indexDrift.Count -gt 0) {
    Write-Output "INDEX_DRIFT ($($indexDrift.Count)) -- the live index differs from the repository:"
    $indexDrift | ForEach-Object { Write-Output "  $($_.Index): $($_.Field) expected $($_.Expected), live $($_.Actual)  (defined in $($_.File))" }
}

if ($indexInvalid.Count -gt 0) {
    Write-Output "INDEX_INVALID ($($indexInvalid.Count)) -- the index exists but is not valid and ready, so the planner ignores it:"
    $indexInvalid | ForEach-Object { Write-Output "  $($_.Index)  (defined in $($_.File))" }
}

if ($indexExtra.Count -gt 0) {
    Write-Output "UNDECLARED_INDEX ($($indexExtra.Count)) -- live indexes in repository schemas that no migration declares:"
    # The property is Index, not Name. An earlier draft printed $_.Name here, which is null on
    # these objects, so every undeclared index was reported as "schema. on table" with its
    # name missing -- an accusation that is unreadable without the identifier it names. The
    # mutation harness caught it: zz_probe_idx was detected and counted correctly but the
    # line did not contain the name the harness looks for.
    $indexExtra | ForEach-Object { Write-Output "  $($_.Schema).$($_.Index) on $($_.Table)" }
}

# =========================================================================================
# TABLE CONSTRAINTS (named, declared in the migrations)
# =========================================================================================
#
# SCOPE, AND WHY IT IS DELIBERATELY NARROWER THAN THE INDEX SECTION
#
# Indexes have exactly one declaration form (CREATE INDEX), so a single statement regex covers
# them completely. Constraints do not. PostgreSQL accepts PRIMARY KEY / UNIQUE / CHECK both as
# table-level CONSTRAINT clauses and as bare column-level keywords, accepts FOREIGN KEY either as
# an explicit CONSTRAINT or as a bare column-level REFERENCES, and *generates* a name for every
# unnamed constraint. A complete declared-vs-live constraint comparison therefore requires a real
# SQL parser plus PostgreSQL's name-generation rules, and this repository does not have one.
#
# An attempt to get that with paren-depth tracking was measured before being written up and
# rejected. It matched only 49 of 65 CREATE TABLE statements in this repository, because a ')'
# inside a string literal (there are regex CHECK constraints containing ')') closes the body early
# and silently truncates the remaining constraint list -- 172 of 251 CHECK constraints, with no
# error. A constraint gate that miscounts is worse than no gate: it either misses real drift or
# accuses a correct database, and its green result stops being believed. So the section below
# does NOT parse CREATE TABLE bodies at all.
#
# It performs ONE linear pass over the cleaned text, tracking the current table as CREATE TABLE
# and ALTER TABLE are seen and attaching each CONSTRAINT <name> after it to that table. No paren
# matching, no nesting, no name generation -- so no silent truncation is possible. That is a
# strictly-verifiable subset: every constraint it reports as missing is provably named in the
# migrations, and it measured 0 missing and 0 type mismatches against the live database.
#
# COVERED
#   MISSING_CONSTRAINT     a named constraint declared in the migrations is absent live
#   CONSTRAINT_TYPE_DRIFT  it exists but as a different constraint type
#   CONSTRAINT_NOT_VALIDATED  it is NOT VALID and the migration does not declare it so
#
# NOT COVERED, STATED RATHER THAN CLAIMED
#   - Unnamed/anonymous constraints (the 46 PRIMARY KEYs and inline REFERENCES foreign keys whose
#     names PostgreSQL generates). Not reported in either direction. An earlier draft tried, and
#     would have falsely accused oms_order_submitting_requires_outbox and
#     feed_observed_sequence_non_negative, both of which ARE declared -- that measurement is why
#     this direction was dropped.
#   - CHECK expression text. PostgreSQL deparse adds parentheses and type casts, so a text
#     comparison reports phantom differences; see the index section for the same lesson.
#   - FOREIGN KEY referenced table/column targets.
#
# VALIDATION STATE IS COMPARED AGAINST THE DECLARED STATE, NOT ASSUMED
#
# migration 0023 deliberately adds audit.record.audit_signing_key_required as NOT VALID: 1294
# pre-existing audit rows violate it and no signing key can honestly be attributed to records
# written without one. Checking "is every declared constraint validated?" would have reported that
# deliberate design decision as drift. The comparison is therefore live-vs-DECLARED, and the
# NOT VALID detection is deliberately biased toward accepting the constraint -- when in doubt
# this section under-reports rather than raising a false accusation.
# =========================================================================================

# NOTE the alternation order. The DROP branch must precede the generic ALTER TABLE branch, or
# ALTER TABLE matches first, the drop is never seen, and a later migration's drop is reported as
# a MISSING_CONSTRAINT -- the same class of defect as mis-ordering the migration files.
$constraintRx =
    '(?s)(?<drop>ALTER\s+TABLE\s+(?:ONLY\s+)?"?[A-Za-z0-9_]+"?(?:\."?[A-Za-z0-9_]+"?)?\s+DROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?"?[A-Za-z0-9_]+"?)' +
    '|CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?<cqn>"?[A-Za-z0-9_]+"?(?:\."?[A-Za-z0-9_]+"?)?)' +
    '|ALTER\s+TABLE\s+(?:ONLY\s+)?(?<atqn>"?[A-Za-z0-9_]+"?(?:\."?[A-Za-z0-9_]+"?)?)' +
    # Match the TYPE KEYWORD directly rather than capturing the rest of the clause and
    # classifying it afterwards. Two reasons, both learned the hard way here.
    #
    # 1. Correctness. A multi-line constraint such as
    #      ADD CONSTRAINT x CHECK (
    #          a <> 'ACTIVE' OR (b IS NOT NULL AND c IS NULL)
    #      ),
    #    reaches '(' on its first line. A capture that stopped at a newline died there and
    #    silently dropped 84 of the 268 named constraints -- every multi-line one -- while
    #    still reporting drift=0. Letting a capture cross newlines fixed the count but made
    #    the gate slow enough to exceed a 600s budget, because a lazy `[\s\S]*?` has to be
    #    re-expanded from every candidate position.
    #
    # 2. Speed. The type keyword is always the token immediately after the name, so matching
    #    it in the pattern is a fixed, local match. It also excludes "CONSTRAINT TRIGGER"
    #    for free, because TRIGGER is not one of the five keywords.
    '|(?<!COMMENT\s+ON\s)CONSTRAINT\s+(?<cn>"?[A-Za-z0-9_]+"?)\s+(?<ckw>PRIMARY\s+KEY|UNIQUE|FOREIGN\s+KEY|CHECK|EXCLUDE)\b'

$constraintNotValidRx = '(?is)\bNOT\s+VALID\b'
$constraintCnRx       = [regex] $constraintRx
$constraintNotValid   = [regex] $constraintNotValidRx

# contype codes as PostgreSQL reports them.
function Get-ConstraintType {
    param([string] $Keyword)
    switch ($Keyword) {
        'PRIMARY KEY' { return 'p' }
        'UNIQUE'      { return 'u' }
        'FOREIGN KEY' { return 'f' }
        'CHECK'       { return 'c' }
        'EXCLUDE'     { return 'x' }
    }
    return $null
}

$expectedConstraints = [ordered]@{}
$droppedConstraints  = [ordered]@{}
$constraintDeclared  = 0
$constraintUnmapped  = 0

foreach ($file in $cleanFiles) {
    $text = $file.Clean
    $current = $null
    foreach ($m in $constraintCnRx.Matches($text)) {
        # A later migration that drops the constraint wins, regardless of order within the file.
        if ($m.Groups['drop'].Success) {
            $dq = ($m.Groups['drop'].Value -replace '"', '').ToLower()
            if ($dq -match 'ALTER\s+TABLE\s+(?:ONLY\s+)?(?<s>[A-Za-z0-9_]+)\.(?<t>[A-Za-z0-9_]+)\s+DROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?(?<n>[A-Za-z0-9_]+)') {
                $dkey = $Matches['s'] + '|' + $Matches['t'] + '|' + $Matches['n']
                [void] $expectedConstraints.Remove($dkey)
                $droppedConstraints[$dkey] = $file.Name
            }
            continue
        }
        if ($m.Groups['cqn'].Success -or $m.Groups['atqn'].Success) {
            $qn = if ($m.Groups['cqn'].Success) { $m.Groups['cqn'].Value } else { $m.Groups['atqn'].Value }
            $nt = (Normalise-Relation $qn) -split '\.'
            $current = @{ Schema = $nt[0]; Table = $nt[1] }
            continue
        }
        if (-not $m.Groups['cn'].Success) { continue }

        $cname = ($m.Groups['cn'].Value -replace '"', '').ToLower()
        # The keyword arrives as written in the file, so normalise internal whitespace before
        # mapping: "PRIMARY  KEY" and "PRIMARY KEY" are the same constraint type.
        $ctype = Get-ConstraintType (($m.Groups['ckw'].Value -replace '\s+', ' ').ToUpper().Trim())

        # "CREATE CONSTRAINT TRIGGER" names a trigger, not a table constraint, and the lookbehind
        # already removes COMMENT ON CONSTRAINT. Either way the pattern does not match, so nothing
        # reaches here that is not a real table constraint -- there is no silent-skip branch.
        if ($null -eq $ctype) { continue }
        $constraintDeclared++

        if ($null -eq $current) { $constraintUnmapped++; continue }

        $ckey = $current.Schema + '|' + $current.Table + '|' + $cname

        # Declared validation state: NOT VALID, if present, trails the constraint to the end of
        # the statement. The region deliberately runs to the terminating semicolon rather than
        # stopping at the next CONSTRAINT, so a region may cover a sibling constraint in the same
        # ALTER TABLE. That is the conservative direction: it can only cause this section to treat
        # a constraint as declared-NOT-VALID and therefore skip the check, never to raise a false
        # accusation. The bias is deliberate and matches the comment above.
        $regionEnd = $text.IndexOf(';', $m.Index)
        if ($regionEnd -lt 0) { $regionEnd = $text.Length }

        $expectedConstraints[$ckey] = [pscustomobject]@{
            Schema           = $current.Schema
            Table            = $current.Table
            Name             = $cname
            Type             = $ctype
            DeclaredNotValid = $constraintNotValid.IsMatch($text.Substring($m.Index, $regionEnd - $m.Index))
            File             = $file.Name
        }
        [void] $droppedConstraints.Remove($ckey)
    }
}

# Same fail-closed rule as functions, triggers and indexes: a recognised constraint declaration
# that could not be attributed to a table means this section compares less than the repository
# declares, so it must not issue a verdict.
if ($constraintUnmapped -gt 0) {
    Write-Output "PARSE_INCOMPLETE: $constraintUnmapped named table constraint(s) could not be attributed to a table."
    Write-Output 'The constraint comparison below would silently omit them, so no verdict is issued.'
    exit 1
}

$constraintMissing      = @()
$constraintTypeDrift    = @()
$constraintNotValidated = @()
$constraintChecked      = 0

if ($expectedConstraints.Count -gt 0) {
    $constraintSchemas = @($expectedConstraints.Values | ForEach-Object { $_.Schema } | Select-Object -Unique | Sort-Object)
    $constraintSchemaList = ($constraintSchemas | ForEach-Object { "'$_'" }) -join ','

    $liveConstraintsRaw = Invoke-Psql @"
SELECT n.nspname || '|' || t.relname || '|' || c.conname || '|' || c.contype::text || '|' || c.convalidated::int::text
  FROM pg_constraint c
  JOIN pg_class t      ON t.oid = c.conrelid
  JOIN pg_namespace n  ON n.oid = t.relnamespace
 WHERE n.nspname IN ($constraintSchemaList)
   AND NOT t.relispartition
"@

    $liveConstraintMap = @{}
    if (-not [string]::IsNullOrEmpty($liveConstraintsRaw)) {
    foreach ($line in ($liveConstraintsRaw -split "`n")) {
        # Trim: Invoke-Psql joins rows with CRLF on Windows, and an untrimmed final field makes
        # every constraint look NOT VALID. That fabricated 261 phantom findings before it was caught.
        $line = $line.Trim()
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        $p = $line -split '\|'
        if ($p.Count -lt 5) { continue }
        $liveConstraintMap[$p[0] + '|' + $p[1] + '|' + $p[2]] =
            [pscustomobject]@{ Type = $p[3]; Validated = ($p[4] -eq '1') }
    }
    }

    foreach ($key in $expectedConstraints.Keys) {
        $exp = $expectedConstraints[$key]
        $constraintChecked++
        if (-not $liveConstraintMap.ContainsKey($key)) {
            $constraintMissing += [pscustomobject]@{ Schema=$exp.Schema; Table=$exp.Table; Name=$exp.Name; File=$exp.File }
            continue
        }
        $live = $liveConstraintMap[$key]
        if ($live.Type -ne $exp.Type) {
            $constraintTypeDrift += [pscustomobject]@{ Name=$exp.Name; Table=$exp.Table; Expected=$exp.Type; Actual=$live.Type; File=$exp.File }
        }
        # Live is NOT VALID but the migration does not declare it so: the constraint is not
        # enforced for existing rows while looking identical in the repository.
        if ((-not $live.Validated) -and (-not $exp.DeclaredNotValid)) {
            $constraintNotValidated += [pscustomobject]@{ Name=$exp.Name; Table=$exp.Table; File=$exp.File }
        }
    }
}

Write-Output "compared_constraints=$constraintChecked expected_constraints=$($expectedConstraints.Count) dropped_by_later_migration=$($droppedConstraints.Count) declared_by_files=$constraintDeclared"

if ($constraintMissing.Count -gt 0) {
    Write-Output "MISSING_CONSTRAINT ($($constraintMissing.Count)) -- the migration files declare these but the database has no such constraint:"
    $constraintMissing | ForEach-Object { Write-Output "  $($_.Schema).$($_.Name) on $($_.Table)  (defined in $($_.File))" }
}

if ($constraintTypeDrift.Count -gt 0) {
    Write-Output "CONSTRAINT_TYPE_DRIFT ($($constraintTypeDrift.Count)) -- the live constraint exists but is a different type:"
    $constraintTypeDrift | ForEach-Object { Write-Output "  $($_.Name) on $($_.Table): expected $($_.Expected), live $($_.Actual)  (defined in $($_.File))" }
}

if ($constraintNotValidated.Count -gt 0) {
    Write-Output "CONSTRAINT_NOT_VALIDATED ($($constraintNotValidated.Count)) -- the live constraint is NOT VALID and no migration declares it so, so it is not enforced for existing rows:"
    $constraintNotValidated | ForEach-Object { Write-Output "  $($_.Name) on $($_.Table)  (defined in $($_.File))" }
}

if ($drift.Count -gt 0 -or $missing.Count -gt 0 -or $extras.Count -gt 0 -or
    $triggerDrift.Count -gt 0 -or $triggerMissing.Count -gt 0 -or
    $triggerExtra.Count -gt 0 -or $triggerDisabled.Count -gt 0 -or
    $indexDrift.Count -gt 0 -or $indexMissing.Count -gt 0 -or
    $indexExtra.Count -gt 0 -or $indexInvalid.Count -gt 0 -or
    $constraintMissing.Count -gt 0 -or $constraintTypeDrift.Count -gt 0 -or
    $constraintNotValidated.Count -gt 0) {
    exit 1
}

Write-Output 'drift=0 (live function bodies, triggers, indexes, and named constraints match the migration files)'
exit 0