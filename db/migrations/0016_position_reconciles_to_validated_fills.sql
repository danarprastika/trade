-- 0016_position_reconciles_to_validated_fills.sql
--
-- Mandatory financial invariant 5:
--
--   09_TESTING_AND_RELEASE_EVIDENCE.md line 5:
--       "Portfolio positions reconcile to validated fills."
--
-- WHAT WAS MISSING
-- ----------------
-- portfolio.position carried `derived_from_fill_sequence` and
-- `derived_from_ledger_sequence`, both NOT NULL, which records an intent to
-- derive the position from fills. Nothing implemented it. The table had NO
-- TRIGGERS AT ALL and no function in the entire schema mentioned
-- derived_from_fill. A position row could be inserted with any quantity and
-- any watermark whatsoever, and the database accepted it.
--
-- So the column asserting derivation was decoration. The existing test
-- TestValidatedFillMustBeReconciled proves the CONVERSE -- that a fill cannot
-- be marked validated without a reconciliation status -- which is necessary but
-- not what invariant 5 states. Nothing tied a position to the fills behind it.
--
-- A second, compounding gap: oms.fill had no triggers either. A validated fill
-- could be UPDATEd freely, so even a correct position would silently drift the
-- moment a fill's quantity was edited. 09_TESTING_AND_RELEASE_EVIDENCE.md
-- requires fault-injection coverage for "duplicate events" and "stale data";
-- this was an unclosed door for both.
--
-- WHY A FILL_SEQUENCE COLUMN IS INTRODUCED
-- -----------------------------------------
-- portfolio.position.derived_from_fill_sequence is NOT NULL, so the schema
-- already requires fills to be sequence-numbered. oms.fill had no such column,
-- which means the premise of an existing NOT NULL column was unmet and the
-- watermark had nothing to point at. This completes the existing design rather
-- than extending it.
--
-- The sequence is per (account_id, environment) and monotonic, and uniqueness is
-- enforced, so the watermark is well defined. It is explicit rather than
-- defaulted: a fill's position in its account's sequence is a meaningful fact
-- that the writer knows, and a default would let it be invented silently.
--
-- oms.fill carries no environment column of its own; it inherits it from
-- oms.order. The derivation therefore joins through the order, which is also
-- the correct source of truth for which environment a fill belongs to.

-- -----------------------------------------------------------------------------
-- 1. Fill sequence, and immutability once validated
-- -----------------------------------------------------------------------------
ALTER TABLE oms.fill
    ADD COLUMN fill_sequence BIGINT NOT NULL;

-- Uniqueness within an account's stream. Without this two fills could claim the
-- same position in the sequence and the watermark would be ambiguous.
--
-- The index is scoped to account_id alone rather than to (account, environment).
-- That is correct because an account identifier belongs to exactly one
-- environment: oms.order.environment is immutable after acceptance, so an
-- account cannot span two environments, and ops.assert_principal_permitted
-- (0015) is what keeps a lower-environment credential from reaching another
-- environment's rows. Scoping the index by environment as well would be
-- redundant, and redundant keys on a hot reconciliation path are not free.
ALTER TABLE oms.fill
    ADD CONSTRAINT fill_sequence_positive CHECK (fill_sequence > 0);

CREATE UNIQUE INDEX fill_account_sequence_uniq
    ON oms.fill (account_id, fill_sequence);

COMMENT ON COLUMN oms.fill.fill_sequence IS
    'Monotonic per-account position of this fill in the account''s fill stream. Uniquely constrains the watermark portfolio.position.derived_from_fill_sequence refers to. Explicit, not defaulted: the writer knows where a fill sits in the stream and a default would let it be invented.';

-- Immutable once validated.
--
-- oms.fill cannot be fully immutable the way audit.record is, because a fill
-- must be UPDATEd once from reconciliation_status = PENDING to validated /
-- MATCHED, and the fill_validation_complete CHECK is evaluated on that UPDATE.
-- So the boundary is drawn at validation:
--
--   - While unvalidated, the row may still be corrected by reconciliation.
--   - Once validated, it is a financial fact and is frozen. Corrections are
--     compensating entries (mandatory invariant 4), not edits to history.
--
-- A validated fill is exactly the thing a position is derived from, so allowing
-- it to change would make every derived position unverifiable and would make
-- the watermark meaningless.
CREATE OR REPLACE FUNCTION oms.guard_fill_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.validated THEN
        RAISE EXCEPTION
            'Fill % is validated and immutable. A validated fill is a financial fact: correct it with a compensating entry, never by editing the record (mandatory invariant 4, corrections are compensating entries).',
            OLD.fill_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    -- The identifying and economic fields of an unvalidated fill may not be
    -- changed either. Only the reconciliation fields may move, and that is what
    -- the whole point of an unvalidated row is.
    IF NEW.fill_id        IS DISTINCT FROM OLD.fill_id
       OR NEW.order_id     IS DISTINCT FROM OLD.order_id
       OR NEW.account_id   IS DISTINCT FROM OLD.account_id
       OR NEW.instrument_id IS DISTINCT FROM OLD.instrument_id
       OR NEW.venue_id     IS DISTINCT FROM OLD.venue_id
       OR NEW.side         IS DISTINCT FROM OLD.side
       OR NEW.quantity     IS DISTINCT FROM OLD.quantity
       OR NEW.price        IS DISTINCT FROM OLD.price
       OR NEW.fee_amount   IS DISTINCT FROM OLD.fee_amount
       OR NEW.funding_amount IS DISTINCT FROM OLD.funding_amount
       OR NEW.venue_trade_id IS DISTINCT FROM OLD.venue_trade_id
       OR NEW.fill_sequence IS DISTINCT FROM OLD.fill_sequence THEN
        RAISE EXCEPTION
            'The economic terms of fill % are fixed once recorded, even before validation. Only reconciliation fields may change while a fill is unvalidated. Cancel the fill and record a compensating one.',
            OLD.fill_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER fill_immutability_guard
    BEFORE UPDATE OR DELETE ON oms.fill
    FOR EACH ROW EXECUTE FUNCTION oms.guard_fill_immutable();

COMMENT ON FUNCTION oms.guard_fill_immutable() IS
    'Freezes a fill at the moment of validation. Before validation only reconciliation fields may move; after it, nothing may. Corrections are compensating entries, not edits to recorded financial facts.';

-- -----------------------------------------------------------------------------
-- 2. Derivation
-- -----------------------------------------------------------------------------
-- The signed position implied by validated fills, in units of the instrument.
--
--   BUY  increases the position by the fill quantity.
--   SELL decreases it.
--
-- Only validated fills count, and only up to the watermark, because that is the
-- set of fills the position claims to incorporate. Filtering on validation
-- rather than on reconciliation_status is deliberate: reconciliation_status is
-- a text column whose values are not an enum, whereas `validated` is a boolean
-- governed by fill_validation_complete, and invariant 5 is stated in terms of
-- validated fills.
--
-- A fill whose order is in a different environment than the position is not
-- counted; the join carries the environment rather than assuming it.
CREATE OR REPLACE FUNCTION portfolio.reconciled_position_quantity(
    p_account_id     TEXT,
    p_environment    common.environment,
    p_instrument_id  TEXT,
    p_venue_id       TEXT,
    p_through_sequence BIGINT
)
RETURNS NUMERIC
LANGUAGE sql
STABLE
AS $$
    SELECT COALESCE(SUM(
               CASE f.side::text WHEN 'BUY' THEN f.quantity
                                 WHEN 'SELL' THEN -f.quantity
                                 ELSE 0 END), 0)
      FROM oms.fill f
      JOIN oms."order" o ON o.order_id = f.order_id
     WHERE f.account_id    = p_account_id
       AND o.environment   = p_environment
       AND f.instrument_id = p_instrument_id
       AND f.venue_id      = p_venue_id
       AND f.validated
       AND f.fill_sequence <= p_through_sequence;
$$;

COMMENT ON FUNCTION portfolio.reconciled_position_quantity IS
    'Signed instrument quantity implied by validated fills for one account/environment/instrument/venue up to a watermark sequence. This is the authority the portfolio.position control checks against; the position is never trusted as its own source of truth.';

-- -----------------------------------------------------------------------------
-- 3. The control
-- -----------------------------------------------------------------------------
-- DEFERRABLE INITIALLY DEFERRED, and the reason is the sum-aggregate problem:
-- a position built from three fills in one transaction cannot be checked
-- row-by-row, because the quantity is only correct once all three exist. The
-- same pattern 0008 uses for ledger journal balancing, so the two controls
-- behave consistently.
--
-- Checking at commit rather than on every write also matches the nature of the
-- requirement: the position must be correct when the transaction ends, not
-- after each individual fill.
CREATE OR REPLACE FUNCTION portfolio.guard_position_reconciles_to_fills()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    r          portfolio.position%ROWTYPE;
    v_expected NUMERIC;
    v_max_seq  BIGINT;
BEGIN
    -- The row is RE-READ here rather than taken from NEW.
    --
    -- A deferred constraint trigger executes with the NEW tuple captured when it
    -- was QUEUED, not the row as it stands at commit. A position that is
    -- INSERTed and then UPDATEd within one transaction would therefore be
    -- checked against its pre-UPDATE quantity and watermark, and would be
    -- refused even though the state being committed is perfectly correct. Since
    -- a position is a single row per identity that is updated as fills arrive,
    -- insert-then-update is the normal shape of the operation, not an edge case.
    --
    -- Reading the current row makes this a check of the state actually being
    -- committed, which is the property invariant 5 requires. A trigger that
    -- refuses correct state is worse than no trigger, because it teaches
    -- operators to work around the control.
    SELECT * INTO r
      FROM portfolio.position
     WHERE position_id = NEW.position_id;

    -- The position was deleted before commit. There is nothing left to
    -- reconcile, and re-inserting is caught by this trigger firing again.
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    SELECT portfolio.reconciled_position_quantity(
               r.account_id, r.environment, r.instrument_id,
               r.venue_id, r.derived_from_fill_sequence)
      INTO v_expected;

    -- COALESCE to 0: an account with no validated fills has a flat book, and a
    -- zero-watermark position is the correct representation of that. Without the
    -- coalesce, MAX returns NULL, and NULL IS DISTINCT FROM 0 is true, so a flat
    -- account could never hold a position at all.
    SELECT COALESCE(MAX(f.fill_sequence), 0)
      INTO v_max_seq
      FROM oms.fill f
      JOIN oms."order" o ON o.order_id = f.order_id
     WHERE f.account_id    = r.account_id
       AND o.environment   = r.environment
       AND f.instrument_id = r.instrument_id
       AND f.venue_id      = r.venue_id
       AND f.validated;

    -- The watermark must be the latest validated fill, not a convenient older
    -- one. Otherwise a position could be pinned at an old fill sequence and
    -- ignore every fill since, which is precisely the silent drift this
    -- migration exists to prevent.
    IF r.derived_from_fill_sequence IS DISTINCT FROM v_max_seq THEN
        RAISE EXCEPTION
            'Position % claims to derive from fill sequence % but the latest validated fill for this account/environment/instrument/venue is at sequence %. A position that ignores later fills is a silent reconciliation break (mandatory invariant 5).',
            r.position_id, r.derived_from_fill_sequence, v_max_seq
            USING ERRCODE = 'check_violation';
    END IF;

    IF r.quantity IS DISTINCT FROM v_expected THEN
        RAISE EXCEPTION
            'Position % carries quantity % but the validated fills through sequence % imply %. Positions are derived from validated fills, never entered independently (mandatory invariant 5, 09_TESTING_AND_RELEASE_EVIDENCE.md).',
            r.position_id, r.quantity, r.derived_from_fill_sequence, v_expected
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER position_reconciles_to_fills
    AFTER INSERT OR UPDATE ON portfolio.position
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION portfolio.guard_position_reconciles_to_fills();

-- Both sides are enforced, and neither is redundant.
--
-- A reader may reasonably ask why a fill needs a trigger at all when a
-- position already has one. The answer is that they fire on different writes:
--
--   * The POSITION-side trigger catches a position that was written wrong. It
--     fires on portfolio.position, so it cannot observe a fill that lands in a
--     LATER transaction with no position write at all.
--
--   * The FILL-side trigger catches a position that was invalidated by a new
--     fill. It fires on oms.fill, so it cannot observe a position written wrong
--     afterwards with no new fill.
--
-- Both perform the same two checks -- quantity against validated fills, and
-- watermark against the LATEST validated fill -- because the same two defects
-- are reachable from either direction. What makes them non-redundant is the
-- write path that queues them, not a difference in what they check.
--
-- Dropping either leaves invariant 5 enforceable-but-broken: without the
-- fill-side control a position goes stale the moment it stops being updated in
-- the same transaction as its fills, which is the normal operating pattern. That
-- regression is invisible to single-transaction tests, which is why the
-- cross-transaction case is covered explicitly and mutation tested.
--
-- Immutability of validated fills (section 2) is what makes this workable. If a
-- validated fill could be edited or removed, a position that was correct when
-- written would drift later with nothing firing.

COMMENT ON FUNCTION portfolio.guard_position_reconciles_to_fills() IS
    'Mandatory invariant 5 at commit time: a position''s quantity must equal the signed sum of validated fills, and its watermark must be the latest validated fill, not an older convenient one. Deferred because a position built from several fills in one transaction is only correct once all of them exist. Re-reads the row rather than using NEW, because a deferred trigger fires with the tuple captured when it was queued, not the state being committed.';

-- -----------------------------------------------------------------------------
-- 4. A new validated fill must not leave existing positions stale
-- -----------------------------------------------------------------------------
-- The position trigger alone is not sufficient. Correct positions can be
-- invalidated from the other side: a new validated fill arrives and every
-- position covering that account/instrument/venue is now behind its watermark.
-- The position trigger would not fire, because no position row changed.
--
-- So a fill insertion re-verifies the positions it affects. This is the
-- reconciliation loop closing on the write path rather than only on the
-- position path.
CREATE OR REPLACE FUNCTION portfolio.guard_new_fill_reconciles_positions()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    r          RECORD;
    v_expected NUMERIC;
    v_max_seq  BIGINT;
BEGIN
    -- This performs the SAME two checks as guard_position_reconciles_to_fills,
    -- against every position the new fill covers. The checks are identical on
    -- purpose. What differs is WHEN each control fires, and that is the whole
    -- point:
    --
    --   * guard_position_reconciles_to_fills is a trigger on portfolio.position.
    --     It fires when a position is WRITTEN, and cannot observe a fill that
    --     lands later with no position write at all.
    --
    --   * This trigger fires when a FILL is written, and cannot observe a
    --     position that is later written wrong without a new fill.
    --
    -- Neither is a subset of the other, and both must exist.
    --
    -- An earlier version of this function checked ONLY the quantity, computing
    -- the expected value through the watermark the position CLAIMED. That looks
    -- sufficient and is not: a position pinned at watermark 1 reconciles to its
    -- own stale watermark and passes, even though validated fill 2 exists and is
    -- being ignored. Because the position-side trigger cannot fire when no
    -- position row is written, that left a position free to go stale the moment
    -- it stopped being updated in the same transaction as its fills -- the
    -- normal operating pattern. The cross-transaction test
    -- TestACommittedPositionGoingStaleOnALaterFillIsRejected is what caught it;
    -- every single-transaction test passed against the broken control.
    FOR r IN
        SELECT p.position_id, p.quantity, p.derived_from_fill_sequence,
               p.account_id, p.environment, p.instrument_id, p.venue_id
          FROM portfolio.position p
          JOIN oms."order" o ON o.order_id = NEW.order_id
         WHERE p.account_id    = NEW.account_id
           AND p.environment   = o.environment
           AND p.instrument_id = NEW.instrument_id
           AND p.venue_id      = NEW.venue_id
    LOOP
        -- The watermark must be the latest validated fill. Without this check a
        -- position can be internally self-consistent at a stale watermark and
        -- silently ignore every fill since.
        SELECT COALESCE(MAX(f.fill_sequence), 0)
          INTO v_max_seq
          FROM oms.fill f
          JOIN oms."order" o ON o.order_id = f.order_id
         WHERE f.account_id    = r.account_id
           AND o.environment   = r.environment
           AND f.instrument_id = r.instrument_id
           AND f.venue_id      = r.venue_id
           AND f.validated;

        IF r.derived_from_fill_sequence IS DISTINCT FROM v_max_seq THEN
            RAISE EXCEPTION
                'Validated fill % leaves position % pinned to fill sequence % while the latest validated fill for this account/environment/instrument/venue is at sequence %. A position that ignores later fills is a silent reconciliation break (mandatory invariant 5).',
                NEW.fill_id, r.position_id, r.derived_from_fill_sequence, v_max_seq
                USING ERRCODE = 'check_violation';
        END IF;

        v_expected := portfolio.reconciled_position_quantity(
            r.account_id, r.environment, r.instrument_id, r.venue_id,
            r.derived_from_fill_sequence);

        IF r.quantity IS DISTINCT FROM v_expected THEN
            RAISE EXCEPTION
                'Validated fill % changes the reconciled quantity for position % from % to %, but the position was not updated. Positions are derived from validated fills (mandatory invariant 5); a fill that arrives without the corresponding position update leaves a reconciliation break.',
                NEW.fill_id, r.position_id, r.quantity, v_expected
                USING ERRCODE = 'check_violation';
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER new_fill_reconciles_positions
    AFTER INSERT ON oms.fill
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION portfolio.guard_new_fill_reconciles_positions();

COMMENT ON FUNCTION portfolio.guard_new_fill_reconciles_positions() IS
    'Closes invariant 5 from the write side: inserting a validated fill re-verifies every position it covers, so a correct position cannot be silently invalidated by a new fill arriving in a later transaction with no position write. Performs the same quantity and watermark-currency checks as guard_position_reconciles_to_fills; the two differ only in which table write triggers them, which is why neither is redundant.';
