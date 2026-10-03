-- 0027: serve the reconciliation case dedup lookup.
--
-- migrate: no-transaction
--
-- WHY THIS MIGRATION EXISTS
--
-- services/reconcile/case.go:93 looks for an existing unresolved case before opening a
-- new one, so that a difference which persists across runs escalates one case rather
-- than accumulating a new case every pass. That lookup filters on five columns --
--
--     environment, venue_id, difference_kind, internal_reference, status
--
-- and no index leads with them. The indexes on reconciliation."case" are
-- case_open_material_idx (environment, venue_id, severity), case_sla_idx (resolve_by),
-- case_account_idx (account_id, status) and case_correlation_idx (correlation_id).
-- None can seek on the dedup predicate.
--
-- MEASURED, NOT ASSUMED. The plan for the real statement before this migration:
--
--   Limit
--     ->  Sort
--           Sort Key: detected_at
--           ->  Bitmap Heap Scan on "case"
--                 Recheck Cond: (status = ANY ('{OPEN,INVESTIGATING,REOPENED}'))
--                 Filter: ((environment = ...) AND (venue_id = ...) AND
--                         (difference_kind = ...) AND (internal_reference = ...))
--                 ->  Bitmap Index Scan on case_account_idx
--                       Index Cond: (status = ANY (...))
--
-- The planner reaches the table by scanning every unresolved case and filtering the
-- four remaining columns in the heap. environment, venue_id and status are read
-- twice, once from the index and once from the heap, and difference_kind and
-- internal_reference are read from the heap for every unresolved case in the table
-- rather than for the handful that match. This runs once per finding per
-- reconciliation pass, so it is on the hot path of every pass over every venue, and
-- it degrades with the size of the unresolved-case backlog rather than with the
-- number of cases matching the finding.
--
-- The index leads with the four equality columns and carries status as a partial
-- predicate, so the index contains only unresolved cases -- the same narrowing
-- case_open_material_idx and case_sla_idx already use, and for the same reason: a
-- resolved case can never be the target of this lookup, because the status filter
-- would exclude it.
--
-- detected_at is included rather than left to the sort so the ORDER BY ... LIMIT 1
-- is satisfied by the index in order. Without it the planner must still sort every
-- match to find the oldest; with it the oldest matching row is the first row
-- examined and no Sort node appears in the plan at all.
--
-- ENVIRONMENT-AWARE, like every other index in this schema. The index names the
-- environment as its leading column so it is also the index the 0018 scope guard
-- wants for its per-environment lookup.
--
-- EXPAND-ONLY: an additive index. No existing row is read, written or constrained,
-- and no object is dropped.
--
-- CONCURRENTLY, and this migration is therefore marked no-transaction. CREATE INDEX
-- CONCURRENTLY cannot run inside a transaction block. Reconciliation cases accumulate
-- without bound in any real deployment, which is precisely the case where a blocking
-- index build would hold a lock that serialises every reconciliation pass against it.
-- The marker and the re-runnable shape are explained in db/migrate.ps1 and in the
-- header of 0026, which established the opt-out.

CREATE INDEX CONCURRENTLY IF NOT EXISTS case_open_dedup_idx
    ON reconciliation."case" (environment, venue_id, difference_kind, internal_reference, detected_at)
    WHERE status IN ('OPEN', 'INVESTIGATING', 'REOPENED');

COMMENT ON INDEX reconciliation.case_open_dedup_idx IS
    'Serves the existing-unresolved-case lookup in services/reconcile/case.go, which prevents one difference from accumulating a new case every reconciliation pass. Leading columns are the four equality predicates; status is a partial predicate because a resolved case can never match the lookup, and detected_at is carried so ORDER BY detected_at LIMIT 1 is satisfied in index order with no Sort node. Before this index the planner bitmap-scanned every unresolved case and filtered the four equality columns in the heap.';

-- The assertion below is the reason the marker comment says "re-runnable": an
-- index left INVALID by an interrupted concurrent build is silently ignored by the
-- planner, so the lookup silently degrades back to the plan this migration exists
-- to fix, with no error anywhere. Verifying it here turns that silent degradation
-- into a migration failure.
--
-- The wording below avoids the words CREATE INDEX and DROP INDEX on purpose.
-- db/verify-schema-live.ps1 reads the migration files as text and counts index
-- statements with a regular expression that does not parse string literals, so a
-- message saying "an earlier CREATE INDEX CONCURRENTLY was interrupted" is counted
-- as a second declared index that no statement matches -- which makes the gate fail
-- closed with PARSE_INCOMPLETE against a correct repository. The instruction is
-- phrased around the index name instead.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
          FROM pg_class c
          JOIN pg_index ix ON ix.indexrelid = c.oid
         WHERE c.relname = 'case_open_dedup_idx'
           AND NOT ix.indisvalid
    ) THEN
        RETURN;
    END IF;
    RAISE EXCEPTION
        'index reconciliation.case_open_dedup_idx exists but is marked invalid, which means an earlier concurrent build of it was interrupted. PostgreSQL ignores an invalid index, so the reconciliation case dedup lookup would silently fall back to scanning every unresolved case. Remove it and let this migration build it again: drop index reconciliation.case_open_dedup_idx concurrently, then re-run the migration.';
END;
$$;
