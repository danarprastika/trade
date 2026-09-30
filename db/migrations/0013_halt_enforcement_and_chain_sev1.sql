-- =============================================================================
-- 0013_halt_enforcement_and_chain_sev1.sql
-- G1/G3 - An active halt actually halts, and a broken audit chain stops
-- risk-increasing activity.
--
-- Authority:
--   09_TESTING_AND_RELEASE_EVIDENCE.md, mandatory financial invariant 7:
--       "Halt state blocks prohibited actions."
--   11_EXECUTION_GATES.md: "Waivers are prohibited for financial invariants,
--       authorization, live credential isolation, reconciliation, halt controls,
--       and recovery objectives."
--   17_CONFIGURATION_AND_RISK_POLICY.md: "Missing limits, ... or active halt
--       means reject. No default value may silently imply permission to trade."
--   22_AUDIT_INTEGRITY_AND_EVIDENCE.md section 3: "A chain break, missing
--       sequence, signature failure, or unexpected checkpoint is a SEV-1
--       security event. The system stops privileged mutations and
--       risk-increasing activity."
--
-- The gaps this closes
-- --------------------
-- 1. ops.halt was fully specified -- five levels, exact scope, emergency flag,
--    dual control on re-enable, version vector -- and NOTHING READ IT. There was
--    no guard function and no trigger anywhere in the schema that consulted it.
--    The two existing halt tests assert the halt table's own constraints
--    (re-enable needs two approvers, ranks are ordered) and say nothing about
--    whether a halt blocks anything. A halt system that halts nothing is worse
--    than no halt system, because it appears on the dashboard and in the runbook
--    as a working control.
--
-- 2. audit.partition_month.chain_state was declared with an OPEN/BROKEN/SEALED
--    state machine and a comment stating that BROKEN "stops privileged mutations
--    and risk-increasing activity". Nothing ever wrote BROKEN, and nothing read
--    it. The SEV-1 response required by section 3 was unimplemented.
--
-- 3. ops.halt_rank existed but was referenced only by its own definition and by
--    a test of the ranking function. The monotonicity claimed in the ops.halt
--    comment -- "Lower-level commands cannot clear higher-level halts" -- was
--    unenforced.
--
-- Risk direction
-- --------------
-- A halt blocks RISK-INCREASING activity and permits RISK-REDUCING activity.
-- That asymmetry is deliberate and load-bearing: a system that refuses to
-- flatten during a halt can turn a contained incident into an unbounded loss,
-- because the operator has no way to reduce exposure and the venue is still
-- moving. 17_..._POLICY.md scopes the rejection to "risk-increasing actions",
-- and the same order_type set that decides the risk-decision requirement
-- (order_risk_decision_present) is used here, so the two controls cannot
-- disagree about which orders are which.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- Scope matching.
--
-- A halt applies when, for every scope field it specifies, the order's value
-- matches. A NULL scope field on the halt means "all", so a SYSTEM_HALT with no
-- scope blocks everything in that environment and an ACCOUNT_HALT naming one
-- account blocks only that account.
--
-- The highest matching rank wins. When several halts match, the operator needs
-- to be told about the most severe one; the others remain independently active
-- and are cleared on their own terms.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ops.effective_halt(
    p_environment   common.environment,
    p_account_id    TEXT,
    p_instrument_id TEXT,
    p_market_class  common.market_class,
    p_venue_id      TEXT,
    p_strategy_id   TEXT
)
RETURNS TABLE (
    halt_id    TEXT,
    level      ops.halt_level,
    reason     TEXT,
    activated_at TIMESTAMPTZ,
    emergency  BOOLEAN,
    rank       INTEGER
)
LANGUAGE sql
STABLE
AS $$
    SELECT h.halt_id, h.level, h.reason, h.activated_at, h.emergency,
           ops.halt_rank(h.level)
      FROM ops.halt h
     WHERE h.state = 'ACTIVE'
       AND h.environment = p_environment
       AND (h.account_id    IS NULL OR h.account_id    = p_account_id)
       AND (h.instrument_id IS NULL OR h.instrument_id = p_instrument_id)
       AND (h.market_class  IS NULL OR h.market_class  = p_market_class)
       AND (h.venue_id      IS NULL OR h.venue_id      = p_venue_id)
       AND (h.strategy_id   IS NULL OR h.strategy_id   = p_strategy_id)
     ORDER BY ops.halt_rank(h.level) DESC, h.activated_at
     LIMIT 1;
$$;

COMMENT ON FUNCTION ops.effective_halt IS
    'The highest-ranked ACTIVE halt matching a trading scope. A NULL scope field on the halt means "all values at that dimension". This is the function that makes a halt binding rather than decorative.';

-- -----------------------------------------------------------------------------
-- The gate.
--
-- One entry point, called from the OMS risk boundary. Keeping the decision in a
-- single function means the halt rule, the audit-chain rule and the
-- risk-direction rule cannot drift apart between call sites.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ops.assert_risk_increase_permitted(
    p_environment   common.environment,
    p_account_id    TEXT,
    p_instrument_id TEXT,
    p_market_class  common.market_class,
    p_venue_id      TEXT,
    p_strategy_id   TEXT,
    p_is_risk_increasing BOOLEAN,
    p_context       TEXT
)
RETURNS VOID
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    v_halt        RECORD;
    v_broken_part TEXT;
BEGIN
    -- Risk-reducing activity is never blocked. Flattening must remain possible
    -- during a halt; see the header.
    IF NOT p_is_risk_increasing THEN
        RETURN;
    END IF;

    -- A broken audit chain is checked FIRST and is not overridable by anything.
    -- Evidence that cannot be trusted means decisions cannot be evidenced, so
    -- risk-increasing activity stops until the chain is repaired -- which is
    -- exactly the SEV-1 response section 3 requires.
    SELECT partition_key INTO v_broken_part
      FROM audit.partition_month
     WHERE chain_state = 'BROKEN'
       AND period_end >= (now() AT TIME ZONE 'UTC')::date - interval '90 days'
     ORDER BY partition_key
     LIMIT 1;

    IF v_broken_part IS NOT NULL THEN
        RAISE EXCEPTION
            'risk-increasing activity refused (%): audit partition % has chain_state=BROKEN. A broken evidence chain is a SEV-1 security event (22_AUDIT_INTEGRITY_AND_EVIDENCE.md section 3); privileged mutations and risk-increasing activity stop until it is resolved.',
            p_context, v_broken_part
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT * INTO v_halt
      FROM ops.effective_halt(
               p_environment, p_account_id, p_instrument_id,
               p_market_class, p_venue_id, p_strategy_id);

    IF v_halt.halt_id IS NOT NULL THEN
        RAISE EXCEPTION
            'risk-increasing activity refused (%): % is active for account % instrument % venue % strategy % in % (reason: %). Reduce-only and close-position activity remains permitted so exposure can be flattened.',
            p_context, v_halt.level, p_account_id, p_instrument_id, p_venue_id,
            p_strategy_id, p_environment, v_halt.reason
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

COMMENT ON FUNCTION ops.assert_risk_increase_permitted IS
    'The single gate for risk-increasing activity. Refuses when an audit partition chain is BROKEN (SEV-1, checked first and not overridable) or when a matching ACTIVE halt exists. Risk-reducing activity always passes, so a halt cannot trap an operator in an open position. Called from the OMS risk boundary; all other callers should use this rather than reimplementing the rule.';

-- -----------------------------------------------------------------------------
-- Wire it into the OMS risk boundary.
--
-- The gate is placed where the risk decision is already required: the
-- transition out of CREATED/RISK_PENDING, which is the point at which an order
-- stops being an intention and becomes possible exposure. Entry at CREATED is
-- deliberately NOT gated, so an operator can still record intent and the
-- evidence trail shows what was refused -- gating entry would make refusals
-- invisible, which is the opposite of what an audit system is for.
--
-- The existing guard is reproduced in full and extended. It is reproduced
-- rather than wrapped because a trigger function cannot call another and defer
-- to it without either duplicating the rules or trusting an ordering that
-- PostgreSQL does not promise; the rules are the control, so they are stated
-- once, here.
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
    v_market   common.market_class;
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
        IF OLD.state = 'UNKNOWN' AND v_event.event_kind <> 'OUTCOME_RESOLVED' THEN
            RAISE EXCEPTION
                'OMS order % may leave UNKNOWN only through an explicit OUTCOME_RESOLVED event, not %; an ambiguous venue outcome is never converted into a determinate state by assumption',
                OLD.order_id, v_event.event_kind
                USING ERRCODE = 'check_violation';
        END IF;

        -- 5. (0013) Halt and audit-chain gate, at the risk boundary.
        --
        -- Fires on the transition out of CREATED/RISK_PENDING, which is where
        -- an order stops being an intention and becomes possible exposure. The
        -- same order_type set the risk-decision constraint uses decides the
        -- direction, so the two controls cannot disagree.
        IF NEW.state NOT IN ('CREATED','RISK_PENDING') THEN
            -- oms.order has no market_class column; the market is a property of
            -- the instrument. It is looked up rather than passed as NULL,
            -- because a NULL market would fail to match a MARKET_HALT and the
            -- halt would silently not apply.
            SELECT i.market_class INTO v_market
              FROM market.instrument i
             WHERE i.instrument_id = NEW.instrument_id;

            -- Concatenation rather than format(): the context text is carried
            -- into a later RAISE, and a literal '%' in it would be read as a
            -- format specifier.
            PERFORM ops.assert_risk_increase_permitted(
                NEW.environment, NEW.account_id, NEW.instrument_id,
                v_market, NEW.venue_id, NEW.strategy_id,
                NEW.order_type NOT IN ('REDUCE_ONLY','CLOSE_POSITION'),
                'OMS order ' || OLD.order_id || ' ' || OLD.state::text || ' -> ' || NEW.state::text);
        END IF;
    END IF;

    -- 6. Execution progress must equal the validated fill events, always.
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

COMMENT ON FUNCTION oms.guard_order_update IS
    'Enforces the closed OMS state machine at the storage boundary: immutable terms, rule-backed state changes, mandatory risk decision for exposure-increasing transitions, explicit resolution for UNKNOWN, the 0013 halt/audit-chain gate at the risk boundary, and fill accounting that exactly matches the fill-event journal.';

-- -----------------------------------------------------------------------------
-- Halt precedence.
--
-- The ops.halt comment claims "Lower-level commands cannot clear higher-level
-- halts", and ops.halt_rank existed to express the ordering, but nothing used
-- it. What is enforced here is the part that is decidable from the data alone:
--
--   A halt cannot be cleared while a halt of strictly higher rank is still
--   ACTIVE in the same environment.
--
-- Without this, clearing a system-wide halt is trivially defeated by clearing
-- the account halt first and treating the system as resolved, or by a race
-- between two operators clearing overlapping scopes in the wrong order.
-- Authority-based enforcement -- an operator with account-level authority must
-- not clear a system halt -- needs the authorization model, which does not
-- exist yet, and is not claimed here.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ops.guard_halt_precedence()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_higher TEXT;
    v_rank   INTEGER;
BEGIN
    IF NEW.state = 'ACTIVE' OR OLD.state <> 'ACTIVE' THEN
        RETURN NEW;
    END IF;

    v_rank := ops.halt_rank(NEW.level);

    SELECT h.halt_id INTO v_higher
      FROM ops.halt h
     WHERE h.state = 'ACTIVE'
       AND h.environment = NEW.environment
       AND ops.halt_rank(h.level) > v_rank
       AND h.halt_id <> NEW.halt_id
     ORDER BY ops.halt_rank(h.level) DESC
     LIMIT 1;

    IF v_higher IS NOT NULL THEN
        RAISE EXCEPTION
            'halt % (%s, rank %) cannot be cleared while higher-ranked halt % is ACTIVE in the same environment. Halt clearance is monotonic in severity.',
            NEW.halt_id, NEW.level, v_rank, v_higher
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION ops.guard_halt_precedence IS
    'BEFORE UPDATE on ops.halt. A halt may not be cleared while a higher-ranked halt is still ACTIVE in the same environment. This is the data-decidable part of the monotonicity claim; authority-based precedence needs the authorization model, which does not exist.';

CREATE TRIGGER ops_halt_precedence
    BEFORE UPDATE ON ops.halt
    FOR EACH ROW EXECUTE FUNCTION ops.guard_halt_precedence();

-- -----------------------------------------------------------------------------
-- Canonical identifier generation.
--
-- The schema verifies canonical identifiers everywhere (common.is_canonical_id)
-- but nothing could MINT one, so every writer had to supply an id it could not
-- verify was well-formed until the write failed. A database function that
-- mints one removes that asymmetry, and gives database-side SEV-1 handling a
-- way to create the halt record it needs.
--
-- 20 characters carry 100 bits, the entropy 03_CANONICAL_CONTRACTS.md specifies.
-- Each character is derived from one random byte by get_byte(b, i) / 8, which
-- maps 256 byte values onto 32 characters with exactly 8 bytes per character.
-- That is uniform: no character is more likely than any other, which a modulo
-- reduction over 32 would also be, but the mapping is exact rather than
-- approximate and needs no bit manipulation to read.
--
-- The alphabet is 0123456789abcdefghjkmnpqrstvwxyz, matching the character class
-- in common.is_canonical_id -- Crockford on encode excludes i, l, o and u.
--
-- This is NOT the same bit layout as contracts.encodeCrockford in Go, and does
-- not need to be: identifiers are opaque and must only be unique, well-formed
-- and unpredictable. The alphabet, the length and the 100 bits of entropy all
-- match, which is what any consumer can observe.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION common.new_canonical_id(p_prefix TEXT)
RETURNS TEXT
LANGUAGE sql
VOLATILE
PARALLEL SAFE
AS $$
    SELECT p_prefix || '_' || (
        SELECT string_agg(
                   substr('0123456789abcdefghjkmnpqrstvwxyz',
                          (get_byte(b, i) / 8) + 1, 1),
                   '' ORDER BY i)
          FROM gen_random_bytes(20) AS b, generate_series(0, 19) AS g(i));
$$;

COMMENT ON FUNCTION common.new_canonical_id IS
    'Mints a canonical identifier: typed prefix plus 20 lowercase Crockford Base32 characters carrying 100 bits of entropy from gen_random_bytes. The SQL twin of contracts.NewID. Produces values accepted by common.is_canonical_id.';

-- -----------------------------------------------------------------------------
-- SEV-1: mark an audit chain broken.
--
-- This is the missing half of chain_state. It sets BROKEN and raises a
-- system-level halt in the same transaction, so the evidence-integrity event
-- and the trading restriction cannot be separated: there is no window in which
-- the chain is known broken and risk-increasing activity is still permitted.
--
-- Clearing the halt does NOT clear chain_state. Resolving a chain break is an
-- evidence operation with its own sign-off; the halt and the chain state are
-- deliberately independent so that re-enabling trading cannot silently reopen a
-- chain that was never repaired.
--
-- The halt is written at 'paper' scope rather than the partition's own
-- environment, because audit.partition_month carries no environment column and
-- a SEV-1 evidence break must not be scoped by inference. That is a real
-- limitation, recorded rather than papered over: until partition_month records
-- the environment these partitions serve, a break stops risk-increasing
-- activity in paper and live alike, which is the safe direction to be wrong in.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit.mark_chain_broken(
    p_partition_key TEXT,
    p_reason        TEXT,
    p_actor_id      TEXT
)
RETURNS TEXT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, audit, common, ops, public
AS $$
DECLARE
    v_halt_id TEXT;
BEGIN
    IF p_reason IS NULL OR btrim(p_reason) = '' THEN
        RAISE EXCEPTION 'mark_chain_broken requires a reason; a chain break without a recorded cause is not actionable'
            USING ERRCODE = 'check_violation';
    END IF;

    UPDATE audit.partition_month
       SET chain_state = 'BROKEN'
     WHERE partition_key = p_partition_key;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'audit partition % does not exist', p_partition_key
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    v_halt_id := common.new_canonical_id('hlt');

    INSERT INTO ops.halt (
        halt_id, level, state, environment, reason,
        emergency, activated_by, activated_at_ns
    ) VALUES (
        v_halt_id, 'SYSTEM_HALT', 'ACTIVE', 'paper',
        format('SEV-1 audit chain break in partition %s: %s', p_partition_key, p_reason),
        true, p_actor_id, (extract(epoch FROM now()) * 1000000000)::bigint
    );

    RETURN v_halt_id;
END;
$$;

COMMENT ON FUNCTION audit.mark_chain_broken IS
    'The SEV-1 response required by 22_..._EVIDENCE.md section 3. Sets chain_state=BROKEN and raises an emergency SYSTEM_HALT in the same transaction, so a known-broken evidence chain and permitted risk-increasing activity can never coexist. Clearing the halt does not clear chain_state: chain repair is a separate evidence operation with its own sign-off.';

SELECT common.assert_no_floating_point();
