-- 0026: close the hole 0024 left, and support the predicate 0025 introduced.
--
-- migrate: no-transaction
--
-- This migration opts out of the harness's transaction wrapper. The
-- reason is the index at the end: it is built CONCURRENTLY so that on a
-- deployment whose ops.outbox has grown, the build does not block writes
-- to the outbox for its duration. CREATE INDEX CONCURRENTLY cannot run
-- inside a transaction block -- PostgreSQL rejects it outright -- so the
-- migration that carries it cannot be wrapped in BEGIN/COMMIT.
--
-- The cost of that opt-out is stated rather than hidden: with no
-- transaction, a failure partway through leaves the earlier statements
-- applied. The migration is therefore written to be re-runnable, so a
-- re-run after a partial failure converges on the same end state rather
-- than sticking: the function is CREATE OR REPLACE, the trigger is
-- dropped before it is created, and the index is IF NOT EXISTS. Every
-- statement is additive -- no object is dropped that is not immediately
-- recreated identically -- so the EXPAND-only discipline holds.
--
-- WHY THIS MIGRATION EXISTS
--
-- 0024 established, at COMMIT, that an order cannot be committed in SUBMITTING without a
-- durable ops.outbox row naming it. That control is real, and it is enforced. It is also
-- incomplete, and the incompleteness is reachable by a statement that already exists in
-- this repository.
--
-- The trigger is on oms."order". It fires when an order enters SUBMITTING. Nothing about
-- it fires when a row is removed from ops.outbox. So:
--
--   1. a transaction commits an order to SUBMITTING together with its outbox row,
--   2. a later transaction deletes that outbox row,
--   3. any later update to the order that leaves state unchanged returns early from the
--      guard (0025 lines 46-52: TG_OP = 'UPDATE' AND OLD.state IS NOT DISTINCT FROM
--      NEW.state -> RETURN NULL), so the guard never re-checks.
--
-- The end state is exactly what 0024 exists to forbid: an order committed in SUBMITTING
-- with nothing durably committed that anything will read. It claims a send is owed, and
-- services/dispatch will never find it. The order is then indistinguishable from one
-- genuinely in flight at a venue that has not answered -- the precise condition the
-- no-blind-retry CHECK and ErrNoSubmissionGate exist to keep out of the system.
--
-- dbtest/dispatch_test.go:94 issues exactly `DELETE FROM ops.outbox`, so this is not a
-- hypothetical. 0024's own comment asserted "ops.outbox rows are never deleted -- nothing
-- in this repository or its migrations issues a DELETE against them". That assertion is
-- false, and a control justified by a false premise is a control with a known hole.
--
-- WHAT THIS ADDS
--
-- 1. A BEFORE DELETE trigger on ops.outbox that refuses to remove a row naming an order
--    that is currently in SUBMITTING. Deleting any other outbox row is unaffected: the
--    event log is still prunable for everything else, and this migration does not
--    attempt to make ops.outbox append-only in general. Only the pairing 0024 enforces is
--    protected, so the change is as narrow as the defect.
--
--    KNOWN LIMITATION, MEASURED RATHER THAN ASSUMED. This is a row-level trigger, and
--    PostgreSQL does not fire row-level triggers for TRUNCATE. Measured against a real
--    committed order in SUBMITTING with its row present:
--
--      DELETE FROM ops.outbox WHERE event_id = ...  -> REFUSED by this guard
--      DELETE FROM oms."order"     WHERE order_id =  -> REFUSED by FK order_event_order_id_fkey
--      TRUNCATE ops.outbox                          -> ALLOWED, and the protected row was lost
--
--    So the invariant is enforced against the application's write path, not against
--    privileged DDL. No code in domain/, services/ or adapters/ issues a TRUNCATE, and it
--    requires the TRUNCATE privilege. dbtest/dbtest.go's Committed helper does issue one,
--    but it truncates ops.outbox together with oms."order" in a single statement, so it
--    removes the order and its record together rather than orphaning an order whose record
--    has gone -- which is the state this guard exists to prevent. A BEFORE TRUNCATE
--    trigger that refused unconditionally would therefore break legitimate test
--    infrastructure while preventing nothing that the DELETE guard does not already
--    prevent for the application's own writes. Recorded as a residual rather than
--    papered over.
--
-- 2. An index on ops.outbox(aggregate_id).
--
--    0024 justified its lookup by outbox_aggregate_sequence_unique, whose leading columns
--    are exactly (aggregate_type, aggregate_id). 0025 then dropped the aggregate_type
--    predicate -- correctly, because aggregate_type is unconstrained free text that this
--    repository writes as both 'ORDER' and 'order' -- which leaves that index unable to
--    serve a lookup on aggregate_id alone. Measured before this migration:
--
--      EXPLAIN SELECT EXISTS (SELECT 1 FROM ops.outbox WHERE aggregate_id = '...');
--        ->  Seq Scan on outbox  Filter: (aggregate_id = '...')
--
--    Harmless while the table is empty and wrong as it grows. 0024's "No new index is
--    required" is stale text in an already-applied migration; this adds the index rather
--    than editing history.
--
-- EXPAND-ONLY: an additive index and an additive trigger. No existing row is read,
--    written, or constrained, and no object is dropped.

-- There is no BEGIN/COMMIT in this file. The harness applies a
-- no-transaction migration exactly as written, and an explicit
-- transaction block would put the CONCURRENTLY index build back
-- inside a transaction, which is the one thing this migration must
-- not do.

-- CREATE OR REPLACE, not CREATE. Every function in this repository is defined that way so
-- a migration can be re-applied idempotently -- which is what makes a mutation harness
-- able to restore the schema afterwards. A plain CREATE here would fail on any second
-- application with "function already exists with same argument types", turning a
-- recoverable mistake into a stuck ledger entry that only a manual DROP clears.
CREATE OR REPLACE FUNCTION ops.guard_outbox_delete_preserves_submitting_order()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_order_id   TEXT;
    v_order_state oms.order_state;
BEGIN
    -- Only rows that could be satisfying the 0024 pairing are in scope. A row naming
    -- something that is not an order -- a fill, an audit event, a risk decision -- is
    -- prunable exactly as before.
    --
    -- "Is this an order id" is asked with common.is_canonical_id rather than a prefix
    -- pattern. oms."order".order_id is already constrained by
    -- CHECK (common.is_canonical_id(order_id, 'ord')), so this reuses the schema's own
    -- definition of a canonical order id and cannot drift from it. A literal like
    -- 'ord-' would also have been wrong: canonical ids join the prefix to the entropy
    -- with an underscore, so the correct literal is 'ord_'.
    IF NOT common.is_canonical_id(OLD.aggregate_id, 'ord') THEN
        RETURN OLD;
    END IF;

    SELECT o.order_id, o.state
      INTO v_order_id, v_order_state
      FROM oms."order" o
     WHERE o.order_id = OLD.aggregate_id;

    -- No such order: nothing is depending on this row.
    IF NOT FOUND THEN
        RETURN OLD;
    END IF;

    IF v_order_state = 'SUBMITTING' THEN
        RAISE EXCEPTION
            'refusing to delete outbox row %: order % is committed in SUBMITTING and this row is the durable record that a send is owed for it. Deleting it leaves an order that claims an unfulfilled send with nothing that anyone will read, which is the state migration 0024 exists to prevent. Resolve the order first, or prune the row only after the order has left SUBMITTING.',
            OLD.event_id, v_order_id
            USING ERRCODE = 'check_violation',
                  HINT = 'Resolve the order out of SUBMITTING before pruning its outbox row.';
    END IF;

    RETURN OLD;
END;
$$;

COMMENT ON FUNCTION ops.guard_outbox_delete_preserves_submitting_order() IS
    'Refuses to delete an ops.outbox row whose aggregate_id names an order still committed in SUBMITTING. The 0024 guard is a trigger on oms."order" and so never fires when the outbox row is removed instead, which would restore by deletion the exact state 0024 forbids. Narrow on purpose: only the SUBMITTING pairing is protected, and the event log remains prunable otherwise.';

-- Dropped before it is created rather than created outright. This migration runs
-- without a transaction (see the header), so a re-run after a partial failure must
-- not stop here on "trigger already exists"; dropping first makes the whole
-- migration converge on the same end state however many times it is applied.
DROP TRIGGER IF EXISTS ops_outbox_delete_preserves_submitting_order ON ops.outbox;

CREATE TRIGGER ops_outbox_delete_preserves_submitting_order
    BEFORE DELETE ON ops.outbox
    FOR EACH ROW
    EXECUTE FUNCTION ops.guard_outbox_delete_preserves_submitting_order();

COMMENT ON TRIGGER ops_outbox_delete_preserves_submitting_order ON ops.outbox IS
    'Keeps the durable record that migration 0024 requires present for as long as the order it belongs to is in SUBMITTING.';

-- Supports both this trigger's lookup and the 0025 guard's predicate, neither of which
-- the existing (aggregate_type, aggregate_id, sequence) index can serve.
--
-- CONCURRENTLY, and the reason this migration is no-transaction: on a deployment
-- whose outbox has grown, a plain CREATE INDEX takes a lock that blocks writes to
-- ops.outbox for the whole build. CONCURRENTLY builds without that lock. IF NOT
-- EXISTS makes the statement re-runnable, which the no-transaction path requires.
CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_aggregate_id_idx ON ops.outbox (aggregate_id);

-- Schema-qualified. CREATE INDEX infers the schema from its table, but COMMENT ON INDEX
-- resolves the name through search_path, and this database's search_path is
-- "$user", public -- ops is not in it. An unqualified COMMENT ON INDEX here fails with
-- 'relation "outbox_aggregate_id_idx" does not exist' immediately after the CREATE INDEX
-- that made it, which reads like the index failed to create. It did create; the comment
-- could not find it.
COMMENT ON INDEX ops.outbox_aggregate_id_idx IS
    'Serves the aggregate_id-only lookups introduced when 0025 dropped the aggregate_type predicate, and in ops.guard_outbox_delete_preserves_submitting_order. outbox_aggregate_sequence_unique cannot serve them because aggregate_type is no longer bound.';