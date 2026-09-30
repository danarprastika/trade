-- =============================================================================
-- 0008_structural_enforcement.sql
-- G1/G3 - Close the state machines and the append paths at the storage
-- boundary, and make an unbalanced internal journal uncommittable.
--
-- Authority: 01_SYSTEM_ARCHITECTURE.md §6/§8, 04_TRADING_DOMAIN_AND_RISK.md,
-- 05_PERSISTENCE_EVENTING_RECONCILIATION.md, 22_AUDIT_INTEGRITY_AND_EVIDENCE.md,
-- 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md.
--
-- Why this migration exists
-- -------------------------
-- 0003/0004/0005 DECLARED closed state machines and balanced journals but did
-- not ENFORCE them:
--
--   1. oms.state_transition_rule and strategy.state_transition_rule were
--      documentation. A direct UPDATE moved any order or strategy to any state.
--      A risk-increasing order reached 'SUBMITTING' with no risk decision.
--   2. oms.order_event was not tied to the order's actual state, so a journal
--      could claim a transition the order never made, or a transition in an
--      order that never happened.
--   3. oms.order.filled_quantity could be set to any value not covered by a
--      fill event, inflating position accounting.
--   4. ledger.assert_journal_balanced() was a no-op stub. Nothing prevented a
--      commit of an unbalanced internal journal, and ledger.assert_balanced()
--      summed a rolling one-day window, which is not a journal.
--   5. audit.append_record was documented as the ONLY write path, but a direct
--      INSERT into audit.record bypassed the advisory lock, the sequence check
--      and the chain head.
--   6. audit.record was partitioned to 2027-03 only. A record written after
--      2027-04-01 failed with "no partition of relation found", which would
--      have silently destroyed audit evidence at the worst moment.
--
-- Every control below is a trigger or a constraint, not application code. Each
-- one is exercised by a negative test in dbtest/invariants_test.go.
-- =============================================================================

-- =============================================================================
-- 1. OMS: the closed order state machine.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- 1a. A new order enters the machine at CREATED or RISK_PENDING only.
--
-- Without this, an order could be INSERTed directly into 'SUBMITTING' or
-- 'FILLED' and the transition rules would never see the move at all.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION oms.guard_order_entry_state()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.state NOT IN ('CREATED','RISK_PENDING') THEN
        RAISE EXCEPTION
            'a new OMS order must enter the state machine at CREATED or RISK_PENDING, not %; every later state is reached only through oms.state_transition_rule',
            NEW.state
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER oms_order_entry_state
    BEFORE INSERT ON oms.order
    FOR EACH ROW EXECUTE FUNCTION oms.guard_order_entry_state();

COMMENT ON FUNCTION oms.guard_order_entry_state IS
    'An order is created at CREATED/RISK_PENDING. Inserting a directly-terminal order would bypass the closed transition set, so it is structurally impossible.';

-- -----------------------------------------------------------------------------
-- 1b. Order terms are immutable; only execution progress may change.
--
-- side/quantity/price terms changing after acceptance would silently rewrite the
-- exposure a risk decision authorised. There is no cancel-and-replace inside an
-- order identity: a new order is a new command.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION oms.guard_order_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_event    oms.order_event%ROWTYPE;
    v_rule     oms.state_transition_rule%ROWTYPE;
    v_fill_sum NUMERIC(38,18);
    v_max_px   NUMERIC(38,18);
BEGIN
    -- 1. Immutable order terms.
    IF NEW.order_id        IS DISTINCT FROM OLD.order_id
       OR NEW.command_id   IS DISTINCT FROM OLD.command_id
       OR NEW.environment   IS DISTINCT FROM OLD.environment
       OR NEW.account_id    IS DISTINCT FROM OLD.account_id
       OR NEW.instrument_id IS DISTINCT FROM OLD.instrument_id
       OR NEW.venue_id      IS DISTINCT FROM OLD.venue_id
       OR NEW.side          IS DISTINCT FROM OLD.side
       OR NEW.order_type    IS DISTINCT FROM OLD.order_type
       OR NEW.time_in_force IS DISTINCT FROM OLD.time_in_force
       OR NEW.quantity      IS DISTINCT FROM OLD.quantity
       OR NEW.limit_price   IS DISTINCT FROM OLD.limit_price
       OR NEW.stop_price    IS DISTINCT FROM OLD.stop_price THEN
        RAISE EXCEPTION
            'OMS order terms are immutable after acceptance: order % cannot be re-specified. Cancel and replace with a new command instead.',
            OLD.order_id
            USING ERRCODE = 'check_violation';
    END IF;

    -- 2. State change: the transition must exist in the closed set AND must be
    --    backed by the journal event that declares it.
    IF NEW.state IS DISTINCT FROM OLD.state THEN
        SELECT * INTO v_event
          FROM oms.order_event e
         WHERE e.order_id   = OLD.order_id
           AND e.from_state = OLD.state
           AND e.to_state   = NEW.state
         ORDER BY e.sequence DESC
         LIMIT 1;

        IF NOT FOUND THEN
            RAISE EXCEPTION
                'OMS order % cannot move from % to % without a journal event in oms.order_event declaring that transition; an unexplained state change is forbidden',
                OLD.order_id, OLD.state, NEW.state
                USING ERRCODE = 'check_violation';
        END IF;

        SELECT * INTO v_rule
          FROM oms.state_transition_rule r
         WHERE r.from_state = OLD.state
           AND r.to_state   = NEW.state
           AND r.event_kind = v_event.event_kind;

        IF NOT FOUND THEN
            RAISE EXCEPTION
                'illegal OMS transition % -> % via %: no such rule exists in oms.state_transition_rule',
                OLD.state, NEW.state, v_event.event_kind
                USING ERRCODE = 'check_violation';
        END IF;

        -- 3. A transition that increases exposure at the venue requires the
        --    durable risk decision to already be attached to the order.
        IF v_rule.requires_risk_decision AND NEW.risk_decision_id IS NULL THEN
            RAISE EXCEPTION
                'OMS transition % -> % requires a durable risk decision; order % has no risk_decision_id (09_TESTING_AND_RELEASE_EVIDENCE.md invariant 1)',
                OLD.state, NEW.state, OLD.order_id
                USING ERRCODE = 'check_violation';
        END IF;

        -- 4. An UNKNOWN order leaves UNKNOWN only through an explicit,
        --    attributed resolution event, never by assumption.
        --    requires_outcome_resolution marks the UNKNOWN ambiguity FAMILY:
        --    it flags the SUBMITTING -> UNKNOWN entry, which is itself legal and
        --    expected. The rule set already contains no UNKNOWN row for any event
        --    kind other than OUTCOME_RESOLVED; this check states the rule
        --    directly so the invariant survives a future edit to the rule table.
        IF OLD.state = 'UNKNOWN' AND v_event.event_kind <> 'OUTCOME_RESOLVED' THEN
            RAISE EXCEPTION
                'OMS order % may leave UNKNOWN only through an explicit OUTCOME_RESOLVED event, not %; an ambiguous venue outcome is never converted into a determinate state by assumption',
                OLD.order_id, v_event.event_kind
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    -- 5. Execution progress must equal the validated fill events, always.
    --    This is an ABSOLUTE invariant, not a delta check, so it cannot be
    --    defeated by replaying or skipping events.
    IF NEW.filled_quantity      IS DISTINCT FROM OLD.filled_quantity
       OR NEW.average_fill_price IS DISTINCT FROM OLD.average_fill_price THEN

        SELECT COALESCE(SUM(filled_quantity_delta), 0) INTO v_fill_sum
          FROM oms.order_event
         WHERE order_id = OLD.order_id
           AND event_kind IN ('FILLED','PARTIALLY_FILLED');

        IF v_fill_sum <> NEW.filled_quantity THEN
            RAISE EXCEPTION
                'filled_quantity % for order % does not equal the sum of its fill events %; position accounting may only be advanced by validated fills',
                NEW.filled_quantity, OLD.order_id, v_fill_sum
                USING ERRCODE = 'check_violation';
        END IF;

        IF NEW.average_fill_price IS NULL OR NEW.average_fill_price <= 0 THEN
            RAISE EXCEPTION
                'order % reports a cumulative fill without a positive average_fill_price', OLD.order_id
                USING ERRCODE = 'check_violation';
        END IF;

        SELECT COALESCE(MAX(price), 0) INTO v_max_px
          FROM oms.order_event
         WHERE order_id = OLD.order_id
           AND event_kind IN ('FILLED','PARTIALLY_FILLED');

        IF NEW.average_fill_price > v_max_px THEN
            RAISE EXCEPTION
                'average_fill_price % for order % exceeds its highest fill price %',
                NEW.average_fill_price, OLD.order_id, v_max_px
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER oms_order_state_machine
    BEFORE UPDATE ON oms.order
    FOR EACH ROW EXECUTE FUNCTION oms.guard_order_update();

COMMENT ON FUNCTION oms.guard_order_update IS
    'Enforces the closed OMS state machine at the storage boundary: immutable terms, rule-backed state changes, mandatory risk decision for exposure-increasing transitions, explicit resolution for UNKNOWN, and fill accounting that exactly matches the fill-event journal.';

-- -----------------------------------------------------------------------------
-- 1c. The order event journal is a projection of reality, not a free-form log.
--
--   * the sequence must continue without a gap;
--   * from_state must equal the order's CURRENT state;
--   * the (from, to, kind) triple must exist in the closed rule set.
--
-- Write order is therefore: INSERT oms.order_event, then UPDATE oms.order.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION oms.guard_order_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_state oms.order_state;
    v_next  BIGINT;
BEGIN
    -- Lock the order so the event and the state change are serialised against
    -- any concurrent writer for the same order.
    SELECT state INTO v_state
      FROM oms.order
     WHERE order_id = NEW.order_id
     FOR UPDATE;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'OMS order % does not exist; an order event cannot precede the order', NEW.order_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    IF v_state <> NEW.from_state THEN
        RAISE EXCEPTION
            'OMS order event for order % claims a transition from % but the order is in %; the event journal may not contradict the order',
            NEW.order_id, NEW.from_state, v_state
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT COALESCE(MAX(sequence), 0) + 1 INTO v_next
      FROM oms.order_event
     WHERE order_id = NEW.order_id;

    IF NEW.sequence <> v_next THEN
        RAISE EXCEPTION
            'OMS order event sequence for order % must be % (a missing or out-of-order sequence is a reconciliation defect), got %',
            NEW.order_id, v_next, NEW.sequence
            USING ERRCODE = 'check_violation';
    END IF;

    PERFORM 1
      FROM oms.state_transition_rule r
     WHERE r.from_state = NEW.from_state
       AND r.to_state   = NEW.to_state
       AND r.event_kind = NEW.event_kind;

    IF NOT FOUND THEN
        RAISE EXCEPTION
            'illegal OMS transition % -> % via %: no such rule exists in oms.state_transition_rule',
            NEW.from_state, NEW.to_state, NEW.event_kind
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER oms_order_event_journal
    BEFORE INSERT ON oms.order_event
    FOR EACH ROW EXECUTE FUNCTION oms.guard_order_event();

-- =============================================================================
-- 2. Strategy: the closed lifecycle at the storage boundary.
-- =============================================================================

CREATE OR REPLACE FUNCTION strategy.guard_strategy_transition()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_rule  strategy.state_transition_rule%ROWTYPE;
    v_found BOOLEAN := false;
BEGIN
    IF NEW.state IS DISTINCT FROM OLD.state THEN
        SELECT EXISTS (
            SELECT 1 FROM strategy.state_transition t
             WHERE t.strategy_id = OLD.strategy_id
               AND t.from_state  = OLD.state
               AND t.to_state    = NEW.state
        ) INTO v_found;

        IF NOT v_found THEN
            RAISE EXCEPTION
                'strategy % cannot move from % to % without a recorded strategy.state_transition event; an unexplained strategy state change is forbidden',
                OLD.strategy_id, OLD.state, NEW.state
                USING ERRCODE = 'check_violation';
        END IF;

        SELECT * INTO v_rule
          FROM strategy.state_transition_rule r
         WHERE r.from_state = OLD.state
           AND r.to_state   = NEW.state;

        IF NOT FOUND THEN
            RAISE EXCEPTION
                'illegal strategy transition % -> %: no such rule exists in strategy.state_transition_rule',
                OLD.state, NEW.state
                USING ERRCODE = 'check_violation';
        END IF;

        -- Transitions that authorise risk-increasing capability
        -- (SHADOW->APPROVED, *->DEPLOYED) require a live, independently
        -- approved artifact. Dual control is enforced here rather than only by
        -- the required_role hint, because the hint alone is documentation.
        IF v_rule.requires_dual_control THEN
            IF NEW.approved_by IS NULL
               OR NEW.approved_at IS NULL
               OR NEW.approval_expires_at IS NULL THEN
                RAISE EXCEPTION
                    'strategy transition % -> % requires dual control: a recorded independent approval that has not expired is mandatory before %',
                    OLD.state, NEW.state, NEW.state
                    USING ERRCODE = 'check_violation';
            END IF;

            IF NEW.approval_expires_at <= now() THEN
                RAISE EXCEPTION
                    'strategy approval for % expired at %; it cannot authorise %',
                    OLD.strategy_id, NEW.approval_expires_at, NEW.state
                    USING ERRCODE = 'check_violation';
            END IF;
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER strategy_lifecycle_guard
    BEFORE UPDATE ON strategy.strategy
    FOR EACH ROW EXECUTE FUNCTION strategy.guard_strategy_transition();

COMMENT ON FUNCTION strategy.guard_strategy_transition IS
    'Enforces the closed strategy lifecycle: a state change needs a recorded transition event, a rule in the closed set, and — where the rule demands dual control — a live independent approval.';

-- =============================================================================
-- 3. Ledger: an unbalanced internal journal cannot commit.
--
-- Model: 05_..._PERSISTENCE.md requires entries to be "append-only and balanced".
-- A financial FACT has two shapes in this model:
--
--   * INTERNAL  - a movement the platform itself accounts for between its own
--                 accounts (trade cash, fee, realised P&L, transfer, correction,
--                 settlement). These MUST be posted inside a balanced journal:
--                 debits equal credits, in one currency, in one transaction.
--   * EXTERNAL  - a one-sided real-world flow (a deposit, a withdrawal, funding,
--                 interest, a margin change). There is no counter-account inside
--                 the platform, so a single entry is the complete record.
--
-- Mixing the two was previously possible in both directions: an internal entry
-- could be posted alone, and a journal could be posted with unequal legs.
-- =============================================================================

CREATE TABLE ledger.journal (
    journal_id       TEXT        PRIMARY KEY,
    source_command_id TEXT       NOT NULL,
    environment      common.environment NOT NULL,
    account_id       TEXT        NOT NULL,
    currency         TEXT        NOT NULL,
    entry_count      INTEGER     NOT NULL,
    debit_total      NUMERIC(38,18) NOT NULL,
    credit_total     NUMERIC(38,18) NOT NULL,
    posted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at_ns     BIGINT      NOT NULL,
    correlation_id   TEXT        NOT NULL,
    audit_id         TEXT,
    CONSTRAINT journal_id_format CHECK (common.is_canonical_id(journal_id, 'jnl')),
    CONSTRAINT journal_command_format CHECK (common.is_canonical_id(source_command_id, 'cmd')),
    CONSTRAINT journal_currency_valid CHECK (currency ~ '^[A-Z0-9_]{3,12}$'),
    CONSTRAINT journal_has_entries CHECK (entry_count > 0),
    -- The structural balance rule. A journal that does not net to zero is
    -- created unbalanced; there is no path to a committed unbalanced journal.
    CONSTRAINT journal_balanced CHECK (debit_total = credit_total),
    CONSTRAINT journal_totals_positive CHECK (debit_total > 0 AND credit_total > 0)
);

COMMENT ON TABLE ledger.journal IS
    'A balanced double-entry posting. Every internal ledger entry belongs to exactly one journal, and a journal may not be created unless debits equal credits. Verified again at COMMIT by ledger.assert_journal_balanced().';

ALTER TABLE ledger.entry
    ADD COLUMN journal_id TEXT REFERENCES ledger.journal(journal_id);

CREATE INDEX ledger_entry_journal_idx ON ledger.entry (journal_id) WHERE journal_id IS NOT NULL;

COMMENT ON COLUMN ledger.entry.journal_id IS
    'The balanced journal this entry belongs to. Internal entry kinds REQUIRE a journal; external one-sided flows must NOT invent one.';

-- The previous window-based helper is replaced: summing "the last day for an
-- account/currency" is not a journal and silently accepted permanently
-- unbalanced history.
DROP FUNCTION IF EXISTS ledger.assert_balanced(TEXT, TEXT);

CREATE OR REPLACE FUNCTION ledger.verify_journal(p_journal_id TEXT)
RETURNS VOID
LANGUAGE plpgsql
AS $$
DECLARE
    v_header ledger.journal%ROWTYPE;
    v_count  INTEGER;
    v_debit  NUMERIC(38,18);
    v_credit NUMERIC(38,18);
BEGIN
    SELECT * INTO v_header FROM ledger.journal WHERE journal_id = p_journal_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger journal % does not exist', p_journal_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    SELECT count(*),
           COALESCE(SUM(amount) FILTER (WHERE direction = 'DEBIT'), 0),
           COALESCE(SUM(amount) FILTER (WHERE direction = 'CREDIT'), 0)
      INTO v_count, v_debit, v_credit
      FROM ledger.entry
     WHERE journal_id = p_journal_id;

    IF v_count <> v_header.entry_count
       OR v_debit <> v_header.debit_total
       OR v_credit <> v_header.credit_total THEN
        RAISE EXCEPTION
            'ledger journal % is not consistent with its entries: header says % entries / % debit / % credit, entries are % / % / %',
            p_journal_id, v_header.entry_count, v_header.debit_total, v_header.credit_total,
            v_count, v_debit, v_credit
            USING ERRCODE = 'check_violation';
    END IF;

    IF v_debit <> v_credit THEN
        RAISE EXCEPTION
            'ledger journal % is unbalanced: debits % credits %',
            p_journal_id, v_debit, v_credit
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

COMMENT ON FUNCTION ledger.verify_journal IS
    'Verifies that a journal header matches its entries and that debits equal credits. Callable explicitly by a domain service at the end of a posting transaction.';

-- The DEFERRED constraint trigger is the real control: it fires at COMMIT, so
-- the check cannot be bypassed by a domain service that simply forgets to call
-- a verification function.
CREATE OR REPLACE FUNCTION ledger.journal_must_balance()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_count  INTEGER;
    v_debit  NUMERIC(38,18);
    v_credit NUMERIC(38,18);
    v_header ledger.journal%ROWTYPE;
BEGIN
    IF NEW.journal_id IS NULL THEN
        IF NEW.entry_kind NOT IN ('CASH_DEPOSIT','CASH_WITHDRAWAL','FUNDING','INTEREST','MARGIN_CHANGE') THEN
            RAISE EXCEPTION
                'internal ledger entry kind % must be posted inside a balanced ledger.journal; entry % is an unjournalled % leg of % %',
                NEW.entry_kind, NEW.entry_id, NEW.direction, NEW.amount, NEW.currency
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NULL;
    END IF;

    SELECT * INTO v_header
      FROM ledger.journal
     WHERE journal_id = NEW.journal_id
       AND account_id  = NEW.account_id
       AND currency    = NEW.currency;

    IF NOT FOUND THEN
        RAISE EXCEPTION
            'ledger entry % claims journal % but no such journal exists for account % / %',
            NEW.entry_id, NEW.journal_id, NEW.account_id, NEW.currency
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    SELECT count(*),
           COALESCE(SUM(amount) FILTER (WHERE direction = 'DEBIT'), 0),
           COALESCE(SUM(amount) FILTER (WHERE direction = 'CREDIT'), 0)
      INTO v_count, v_debit, v_credit
      FROM ledger.entry
     WHERE journal_id = NEW.journal_id;

    IF v_count <> v_header.entry_count
       OR v_debit <> v_header.debit_total
       OR v_credit <> v_header.credit_total THEN
        RAISE EXCEPTION
            'ledger journal % does not match its entries at COMMIT: header %/%/%, entries %/%/%',
            NEW.journal_id, v_header.entry_count, v_header.debit_total, v_header.credit_total,
            v_count, v_debit, v_credit
            USING ERRCODE = 'check_violation';
    END IF;

    IF v_debit <> v_credit THEN
        RAISE EXCEPTION
            'an unbalanced ledger journal may not commit: journal % has debits % and credits %',
            NEW.journal_id, v_debit, v_credit
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_entry_journal_balanced
    AFTER INSERT ON ledger.entry
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger.journal_must_balance();

COMMENT ON FUNCTION ledger.journal_must_balance IS
    'Deferred to COMMIT so a domain service cannot post an unbalanced internal journal by simply omitting a verification call. External one-sided flows are permitted to stand alone; every other entry kind must belong to a balanced journal.';

-- The old no-op stub is removed: a trigger function that does nothing is worse
-- than no function, because a reader believes the invariant is enforced.
DROP TRIGGER IF EXISTS ledger_entry_journal_balanced_stub ON ledger.entry;
DROP FUNCTION IF EXISTS ledger.assert_journal_balanced();

-- =============================================================================
-- 4. Audit: one write path, and partitions that cannot run out.
-- =============================================================================

-- 4a. On-demand partition creation.
--
-- Static partitions to 2027-03 meant that the first record written in April 2027
-- would fail with "no partition of relation audit.record found". An audit system
-- that stops recording is worse than one that is down, because the absence of
-- evidence looks like the absence of events.
CREATE OR REPLACE FUNCTION audit.ensure_partition(p_month DATE)
RETURNS TEXT
LANGUAGE plpgsql
AS $$
DECLARE
    v_key   TEXT;
    v_start DATE;
    v_end   DATE;
    v_table TEXT;
BEGIN
    v_start := date_trunc('month', p_month)::date;
    v_end   := (v_start + INTERVAL '1 month')::date;
    v_key   := to_char(v_start, 'YYYY-MM');
    v_table := 'record_' || to_char(v_start, 'YYYY') || '_' || to_char(v_start, 'MM');

    -- Serialise creation for this month across concurrent writers.
    PERFORM pg_advisory_xact_lock(hashtextextended('audit.ensure_partition:' || v_key, 0));

    IF to_regclass(format('audit.%I', v_table)) IS NULL THEN
        EXECUTE format(
            'CREATE TABLE audit.%I PARTITION OF audit.record FOR VALUES FROM (%L) TO (%L)',
            v_table, v_start, v_end);
    END IF;

    INSERT INTO audit.partition_month (partition_key, period_start, period_end)
    VALUES (v_key, v_start, v_end)
    ON CONFLICT (partition_key) DO NOTHING;

    RETURN v_key;
END;
$$;

COMMENT ON FUNCTION audit.ensure_partition IS
    'Creates the monthly audit partition on demand so evidence capture never fails for want of a partition. Called by audit.append_record on every append.';

-- 4b. Reject every write path except audit.append_record.
--
-- The GUC is set by append_record inside the same transaction and is
-- transaction-local, so it cannot leak to another session. This is defence in
-- depth on top of grants: the deployment additionally grants the runtime role
-- EXECUTE on audit.append_record and no direct DML on audit.record.
CREATE OR REPLACE FUNCTION audit.reject_unauthorised_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_last_sequence BIGINT;
    v_last_hash     TEXT;
    v_expected_hash TEXT;
BEGIN
    -- Defence in depth, independent of the append-path token: even a write that
    -- somehow carried the token must genuinely continue this partition's chain.
    -- A forged row that did not advance the chain head would make the NEXT
    -- append_record fail with a sequence error, so it is detected immediately
    -- rather than being silently interleaved into the evidence.
    SELECT last_sequence, last_hash
      INTO v_last_sequence, v_last_hash
      FROM audit.partition_month
     WHERE partition_key = NEW.partition_key;

    IF v_last_sequence IS NULL THEN
        RAISE EXCEPTION
            'audit partition % does not exist; evidence may only be written into an open chain',
            NEW.partition_key
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    v_expected_hash := COALESCE(v_last_hash, audit.genesis_hash());

    IF NEW.sequence <> v_last_sequence + 1 OR NEW.previous_hash <> v_expected_hash THEN
        RAISE EXCEPTION
            'audit.record may only be written through audit.append_record; this row (sequence %, previous_hash %) does not continue the % chain head (expected sequence %, previous_hash %) and a chain break is a SEV-1 event',
            NEW.sequence, NEW.previous_hash, NEW.partition_key,
            v_last_sequence + 1, v_expected_hash
            USING ERRCODE = 'insufficient_privilege';
    END IF;

    -- The append-path token is transaction-local. append_record sets it
    -- immediately around its own INSERT and clears it again, so a direct write
    -- later in the same transaction carries no token.
    IF current_setting('audit.chain_append', true) IS DISTINCT FROM 'on' THEN
        RAISE EXCEPTION
            'audit.record may only be written through audit.append_record; a direct INSERT bypasses the per-partition advisory lock and the chain head update, so it is rejected (chain break is a SEV-1 event)'
            USING ERRCODE = 'insufficient_privilege';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER audit_record_append_path
    BEFORE INSERT ON audit.record
    FOR EACH ROW EXECUTE FUNCTION audit.reject_unauthorised_insert();

COMMENT ON FUNCTION audit.reject_unauthorised_insert IS
    'Blocks the direct-INSERT bypass of the audit chain. Only audit.append_record, which holds the partition advisory lock and validates sequence plus previous_hash, may create evidence.';

-- 4c. append_record: SECURITY DEFINER, pinned search_path, auto-partition.
CREATE OR REPLACE FUNCTION audit.append_record(
    p_audit_id      TEXT,
    p_partition_key TEXT,
    p_sequence      BIGINT,
    p_tenant_scope  TEXT,
    p_actor_id      TEXT,
    p_actor_type    audit.actor_type_v,
    p_action        TEXT,
    p_target_type   TEXT,
    p_target_id     TEXT,
    p_environment   common.environment,
    p_market_scope  common.market_class,
    p_occurred_at   TIMESTAMPTZ,
    p_occurred_at_ns BIGINT,
    p_recorded_at   TIMESTAMPTZ,
    p_recorded_at_ns BIGINT,
    p_reason        TEXT,
    p_correlation_id TEXT,
    p_causation_id  TEXT,
    p_policy_version TEXT,
    p_result        audit.result_value,
    p_before_digest TEXT,
    p_after_digest  TEXT,
    p_details       JSONB,
    p_signing_key_id TEXT,
    p_canonical_schema_version TEXT,
    p_record_hash   TEXT
)
RETURNS VOID
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, audit, common, public
AS $$
DECLARE
    v_last_sequence BIGINT;
    v_last_hash     TEXT;
    v_expected_seq  BIGINT;
    v_expected_hash TEXT;
BEGIN
    -- The partition for the record's month always exists, created on demand.
    PERFORM audit.ensure_partition(date_trunc('month', p_recorded_at)::date);

    PERFORM pg_advisory_xact_lock(hashtextextended(p_partition_key, 0));

    SELECT last_sequence, last_hash
      INTO v_last_sequence, v_last_hash
      FROM audit.partition_month
     WHERE partition_key = p_partition_key
     FOR UPDATE;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'audit partition % does not exist', p_partition_key
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    v_expected_seq := v_last_sequence + 1;
    v_expected_hash := COALESCE(v_last_hash, audit.genesis_hash());

    IF p_sequence <> v_expected_seq THEN
        RAISE EXCEPTION
            'audit chain break in partition %: expected sequence %, got %. A missing or out-of-order sequence is a SEV-1 evidence-integrity event.',
            p_partition_key, v_expected_seq, p_sequence
            USING ERRCODE = 'check_violation';
    END IF;

    -- Authorise exactly this INSERT, then withdraw the authorisation. Scoping
    -- the token to the statement rather than to the transaction means a direct
    -- INSERT issued later by the same session is still refused.
    PERFORM set_config('audit.chain_append', 'on', true);

    INSERT INTO audit.record (
        audit_id, partition_key, sequence,
        tenant_or_owner_scope, actor_id, actor_type, action,
        target_type, target_id, environment, market_scope,
        occurred_at, occurred_at_ns, recorded_at, recorded_at_ns,
        reason, correlation_id, causation_id, policy_version, result,
        before_digest, after_digest, details,
        previous_hash, record_hash, signing_key_id, canonical_schema_version
    ) VALUES (
        p_audit_id, p_partition_key, p_sequence,
        p_tenant_scope, p_actor_id, p_actor_type, p_action,
        p_target_type, p_target_id, p_environment, p_market_scope,
        p_occurred_at, p_occurred_at_ns, p_recorded_at, p_recorded_at_ns,
        p_reason, p_correlation_id, p_causation_id, p_policy_version, p_result,
        p_before_digest, p_after_digest, COALESCE(p_details, '{}'::jsonb),
        v_expected_hash, p_record_hash, p_signing_key_id, p_canonical_schema_version
    );

    UPDATE audit.partition_month
       SET last_sequence = p_sequence,
           last_hash     = p_record_hash,
           record_count  = record_count + 1
     WHERE partition_key = p_partition_key;

    PERFORM set_config('audit.chain_append', 'off', true);
END;
$$;

COMMENT ON FUNCTION audit.append_record IS
    'The ONLY supported way to write an audit record. SECURITY DEFINER with a pinned search_path, holds the per-partition advisory lock, enforces monotonic sequence and previous_hash continuity, and creates the target month partition on demand.';

-- Defence in depth: no direct DML on audit evidence even if a grant is
-- misconfigured. PUBLIC holds no table privileges by default; the statements
-- make the intended posture explicit and are re-applied on every reset.
REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON audit.record FROM PUBLIC;

-- =============================================================================
-- 5. Migration drift detection helper.
--
-- public.schema_migration stores the SHA-256 of each applied file. This
-- function reports any applied migration whose recorded digest no longer
-- matches the digest the runner computed from the file on disk, which is how a
-- silently edited migration is detected instead of silently skipped.
-- =============================================================================

CREATE OR REPLACE FUNCTION public.migration_drift(p_expected jsonb)
RETURNS TABLE (version TEXT, recorded_digest TEXT, expected_digest TEXT)
LANGUAGE plpgsql
AS $$
BEGIN
    RETURN QUERY
    SELECT m.version, m.content_digest, (p_expected ->> m.version)
      FROM public.schema_migration m
     WHERE p_expected ? m.version
       AND m.content_digest IS DISTINCT FROM p_expected ->> m.version;
END;
$$;

COMMENT ON FUNCTION public.migration_drift IS
    'Compares recorded migration digests with the digests computed from the files on disk. Any row returned is a modified, already-applied migration, which is a release blocker (a migration history may never be rewritten).';

SELECT common.assert_no_floating_point();
