-- 0024_submitting_order_requires_outbox.sql
--
-- Doc 05 states the transaction boundary as an invariant: "An authoritative command
-- executes domain validation, state mutation, audit record creation, and outbox
-- insertion in one PostgreSQL transaction. A committed state mutation always has a
-- corresponding durable outbox record."
--
-- Nothing enforced the second sentence. Not one trigger on oms."order" or
-- execution.submission referenced ops.outbox. The pairing held only because
-- domain/execution.Command.Prepare happens to write both rows, and a mutation proof
-- confirms that is load-bearing -- which is a statement about one function, not about
-- the schema.
--
-- The failure this leaves open is the same shape as the one ErrNoSubmissionGate was
-- added to close, and it is worth naming precisely. If a command advances an order to
-- SUBMITTING and forgets the outbox row, the order is committed in a state whose
-- entire meaning is "a send is owed", with nothing committed that anything will ever
-- read. It is indistinguishable, from outside, from an order genuinely in flight at a
-- venue that has not answered. It will sit there looking correct, and the dispatcher
-- will never mention it, because there is nothing for the dispatcher to find.
--
-- Why this trigger, and why deferred:
--
--   * DEFERRED INITIALLY DEFERRED is required, not stylistic. Prepare writes the order
--     row and the outbox row inside one transaction, in that order. An immediate
--     trigger would fire between them and refuse every correct submission. A deferred
--     constraint trigger fires at COMMIT, by which point both rows exist, so the check
--     can be about the transaction's outcome rather than its intermediate steps.
--
--   * It fires only on a transition INTO SUBMITTING. The obligation is created when an
--     order becomes SUBMITTING, and discharged in the same transaction. Requiring the
--     pairing again on every later update to an order that is already SUBMITTING would
--     be checking a debt already paid, and would couple this guard to unrelated future
--     edits of an in-flight order. ops.outbox rows are never deleted -- nothing in this
--     repository or its migrations issues a DELETE against them -- so the original row
--     cannot vanish and invalidate the order.
--
--   * The lookup is served by outbox_aggregate_sequence_unique, whose leading columns
--     are exactly (aggregate_type, aggregate_id). No new index is required.
--
-- Scope: this enforces the pairing for the transition doc 05's failure mode actually
-- describes. It is deliberately not extended to every committed mutation of every
-- table, because a guard that must be re-derived for each new aggregate is a guard that
-- will be forgotten for the next one -- the same reasoning that produced defect 42,
-- where a predicate was correct and had no consumer.

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
        -- opinion. Note this is checked before anything else so the common case --
        -- every order that is not being submitted -- costs one comparison.
        RETURN NULL;
    END IF;

    IF TG_OP = 'UPDATE' AND OLD.state IS NOT DISTINCT FROM NEW.state THEN
        -- Already SUBMITTING and staying there: a version_vector bump, a fill
        -- accounting update. The obligation was discharged when the order first
        -- became SUBMITTING, in an earlier transaction that committed its own outbox
        -- row. Re-checking would be checking a settled debt.
        RETURN NULL;
    END IF;

    SELECT EXISTS (
        SELECT 1
          FROM ops.outbox
         WHERE aggregate_type = 'ORDER'
           AND aggregate_id = NEW.order_id
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
    'SUBMITTING must have a durable ops.outbox row for it in the same transaction. Deferred so the '
    'check runs at COMMIT, when the order row and its outbox row both exist.';

CREATE CONSTRAINT TRIGGER oms_order_submitting_requires_outbox
    AFTER INSERT OR UPDATE ON oms.order
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION oms.guard_submitting_order_has_outbox();

COMMIT;
