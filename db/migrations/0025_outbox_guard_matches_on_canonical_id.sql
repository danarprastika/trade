-- 0025_outbox_guard_matches_on_canonical_id.sql
--
-- A correction to 0024, not a new control.
--
-- 0024's guard matched the outbox row on (aggregate_type = 'ORDER', aggregate_id =
-- order_id). While testing it, a pre-existing fixture in this repository was found
-- writing aggregate_type = 'order' -- lower case -- where domain/execution writes
-- 'ORDER'. The column is unconstrained free text: nothing in any migration pins that
-- spelling, so the two spellings coexist today and a third writer could choose
-- either.
--
-- That makes the predicate in 0024 a spelling check dressed up as a structural one.
-- A guard that can be evaded by changing the case of a literal is not a fail-closed
-- guard, and the failure it would leave open is the exact failure 0024 exists to
-- close: an order committed in SUBMITTING claiming a send is owed that nothing will
-- ever read.
--
-- The fix is to match on aggregate_id alone, which is safe for a reason worth stating
-- rather than assuming. Canonical ids are globally unique and carry their aggregate
-- type as a prefix (ord_, evt_, rec_, ckp_, aud_, ...), so no outbox row belonging to
-- a reconciliation case, a checkpoint, an audit record or any other aggregate can
-- collide with an order id. The aggregate_type filter was therefore never excluding
-- anything; it was only excluding a differently-cased spelling of the same thing.
--
-- This is the second time in this repository that a control turned on a literal
-- rather than on structure -- the first was the fifteen-control gate enumeration,
-- pinned by count rather than against doc 04. The shape recurs: a test or a guard
-- that names a value is checking that value, not the fact.

BEGIN;

CREATE OR REPLACE FUNCTION oms.guard_submitting_order_has_outbox()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_has_outbox BOOLEAN;
BEGIN
    IF NEW.state IS DISTINCT FROM 'SUBMITTING'::oms.order_state THEN
        -- Not an order whose meaning is "a send is owed", so this trigger has no
        -- opinion. Checked first so the common case -- every order that is not being
        -- submitted -- costs one comparison.
        RETURN NULL;
    END IF;

    IF TG_OP = 'UPDATE' AND OLD.state IS NOT DISTINCT FROM NEW.state THEN
        -- Already SUBMITTING and staying there: a version_vector bump, a fill
        -- accounting update. The obligation was discharged when the order first became
        -- SUBMITTING, in an earlier transaction that committed its own outbox row.
        -- Re-checking would be checking a settled debt.
        RETURN NULL;
    END IF;

    -- aggregate_id alone, deliberately. See the migration header: canonical ids are
    -- globally unique and prefixed by aggregate type, so no other aggregate's outbox
    -- row can match an order id, and matching on it cannot be evaded by a change of
    -- case in an unpinned aggregate_type literal.
    SELECT EXISTS (
        SELECT 1
          FROM ops.outbox
         WHERE aggregate_id = NEW.order_id
    ) INTO v_has_outbox;

    IF NOT v_has_outbox THEN
        RAISE EXCEPTION
            'order % is committed in SUBMITTING with no ops.outbox row for it. Doc 05 requires '
            'every committed state mutation to have a durable outbox record: without one the order '
            'claims a send is owed that nothing will ever read, and it is indistinguishable from an '
            'order genuinely in flight at a venue that has not answered.',
            NEW.order_id
            USING ERRCODE = 'check_violation',
                  HINT = 'Insert the outbox row in the same transaction as the transition to SUBMITTING.';
    END IF;

    RETURN NULL;
END;
$$;

COMMENT ON FUNCTION oms.guard_submitting_order_has_outbox() IS
    'Enforces doc 05''s pairing for the transition that creates the obligation: an order that becomes '
    'SUBMITTING must have a durable ops.outbox row naming it in the same transaction. Deferred so the '
    'check runs at COMMIT. Matches on the canonical order id rather than on (aggregate_type, '
    'aggregate_id), because aggregate_type is unconstrained free text and this repository writes it '
    'both as ''ORDER'' and as ''order''.';

COMMIT;
