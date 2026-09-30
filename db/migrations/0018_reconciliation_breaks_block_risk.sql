-- =============================================================================
-- 0018_reconciliation_breaks_block_risk.sql
-- G3 - Trading Core. Mandatory invariant: a material unresolved reconciliation
--      break blocks affected risk-increasing actions.
--
-- Authority: 05_PERSISTENCE_EVENTING_RECONCILIATION.md ("A material unresolved
--            break blocks affected risk-increasing actions"),
--            01_SYSTEM_ARCHITECTURE.md section 3 ("Reconciliation is
--            authoritative for resolving venue discrepancies"),
--            11_EXECUTION_GATES.md G11 ("no material reconciliation break ...
--            exists").
--
-- WHY THIS MIGRATION EXISTS
--
-- 0005_ledger_portfolio_reconciliation.sql created reconciliation.case and
-- described the control twice, in the table's own header comment and again on
-- the blocked_scope column:
--
--     -- with explicit severity; a MATERIAL unresolved break BLOCKS affected
--     -- risk-increasing scope
--
--     -- Affected scope. A MATERIAL case blocks risk-increasing activity within it.
--
-- Nothing implemented it. The reconciliation schema had zero triggers and zero
-- functions. blocked_scope is JSONB NOT NULL -- the system records which scope a
-- break covers and then never reads it.
--
-- The schema is otherwise careful: an SLA is mandatory, resolution metadata is
-- all-or-nothing, a reopened case must say why, a difference cannot be negative.
-- All of that governs how a break is RECORDED. None of it governs what a break
-- DOES, which is the part that is safety-relevant.
--
-- The consequence is the failure mode the blueprint is most explicit about. A
-- venue reports a fill the platform never received, or a balance that does not
-- reconcile, or a position that disagrees with the venue by an amount that
-- should stop trading. The engine opens a MATERIAL case, the case sits in the
-- table, and the OMS continues to raise new exposure on an account whose true
-- state is unknown. G11 requires that no material break exist before live
-- activation; a control that only records breaks cannot establish that.
--
-- WHAT THIS DOES
--
-- Reconciliation state is derived, not duplicated. The block is computed from
-- reconciliation.case at the moment risk-increasing activity is attempted, so it
-- cannot desynchronise from the case that causes it -- there is no second copy
-- of the truth to fall out of step, and clearing the case clears the block with
-- no separate administrative act. 01_SYSTEM_ARCHITECTURE.md section 3 makes
-- reconciliation authoritative for venue discrepancies, so the case table is
-- the right place to read from rather than a projection of it.
--
-- The check goes into ops.assert_risk_increase_permitted, which that function
-- already documents as "the single gate for risk-increasing activity" and which
-- 0013 describes as the point all other callers should use. A separate gate
-- would be a second door into the same building, and the second door is the one
-- that gets left open.
--
-- FAIL-CLOSED SHAPE OF blocked_scope
--
-- blocked_scope is JSONB and no blueprint document specifies its shape, so this
-- migration defines a minimal one and states the reasoning:
--
--     { "accounts":   ["acc_..."],   -- optional
--       "instruments":["ins_..."],   -- optional
--       "venues":     ["ven_..."] }  -- optional
--
-- Each list restricts that dimension; an absent or empty list is unrestricted on
-- that dimension. So {"venues": ["ven_x"]} blocks every account on instrument at
-- venue x, and {} blocks the whole environment.
--
-- The interpretation is deliberately one-sided. Anything this function cannot
-- confidently interpret -- a non-object, an unrecognised key set, a value of the
-- wrong JSON type -- is treated as covering EVERYTHING in the environment, and
-- therefore blocks. A scope this code does not understand must not be the reason
-- trading continues: under-blocking is the one failure direction that can create
-- exposure the operator believes is not there. Over-blocking a malformed scope
-- is an operational problem; under-blocking it is a financial one.
--
-- This asymmetry is a deliberate choice and the opposite trade is defensible in
-- isolation. It is chosen here because the alternative fails silently and the
-- cost of the failure is unmeasurable, while the cost of over-blocking is a case
-- an operator can see and fix.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- Does this case's blocked_scope cover this activity?
--
-- STABLE, not IMMUTABLE: it reads a table. It is a predicate over a case row and
-- its scope, with no writes and no time dependence.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION reconciliation.scope_covers(
    p_scope        JSONB,
    p_account_id   TEXT,
    p_instrument_id TEXT,
    p_venue_id     TEXT
)
RETURNS BOOLEAN
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    v_kind   TEXT;
    v_ids    JSONB;
    v_parsed JSONB;
BEGIN
    IF p_scope IS NULL OR jsonb_typeof(p_scope) = 'null' THEN
        -- A case with no scope covers everything. blocked_scope is NOT NULL, but
        -- a JSON null is a different thing from an absent value and is treated
        -- as the fail-closed case rather than as an error.
        RETURN true;
    END IF;

    IF jsonb_typeof(p_scope) <> 'object' THEN
        RETURN true;  -- uninterpretable -> covers everything
    END IF;

    -- Every recognised key must be an array of strings, or absent. A recognised
    -- key holding the wrong type makes the whole scope uninterpretable, because
    -- a partially-understood scope is exactly how under-blocking happens.
    FOR v_kind IN SELECT jsonb_object_keys(p_scope) LOOP
        IF v_kind NOT IN ('accounts', 'instruments', 'venues') THEN
            RETURN true;  -- unrecognised key -> covers everything
        END IF;
        v_ids := p_scope -> v_kind;
        IF v_ids IS NULL OR jsonb_typeof(v_ids) = 'null' THEN
            CONTINUE;  -- explicitly unrestricted on this dimension
        END IF;
        IF jsonb_typeof(v_ids) <> 'array' THEN
            RETURN true;  -- wrong type -> covers everything
        END IF;
        -- Every element must be a string. A number or object in an id list is a
        -- scope this function cannot compare against a canonical id.
        IF EXISTS (SELECT 1 FROM jsonb_array_elements(v_ids) AS e
                    WHERE jsonb_typeof(e) <> 'string') THEN
            RETURN true;
        END IF;
    END LOOP;

    -- Dimension checks. An absent or empty list is unrestricted.
    v_parsed := COALESCE(p_scope -> 'accounts', '[]'::jsonb);
    IF jsonb_array_length(v_parsed) > 0
       AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements_text(v_parsed) AS a
                        WHERE a = p_account_id) THEN
        RETURN false;
    END IF;

    v_parsed := COALESCE(p_scope -> 'instruments', '[]'::jsonb);
    IF jsonb_array_length(v_parsed) > 0
       AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements_text(v_parsed) AS a
                        WHERE a = p_instrument_id) THEN
        RETURN false;
    END IF;

    v_parsed := COALESCE(p_scope -> 'venues', '[]'::jsonb);
    IF jsonb_array_length(v_parsed) > 0
       AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements_text(v_parsed) AS a
                        WHERE a = p_venue_id) THEN
        RETURN false;
    END IF;

    RETURN true;
END;
$$;

COMMENT ON FUNCTION reconciliation.scope_covers IS
    'TRUE when a case''s blocked_scope covers the given account/instrument/venue. Scope shape: {"accounts":[],"instruments":[],"venues":[]}, each list optional and each absent list meaning unrestricted on that dimension. Uninterpretable scope -- null, non-object, unrecognised key, wrong value type -- covers EVERYTHING, so a scope this function cannot read can never be the reason trading continues.';

-- -----------------------------------------------------------------------------
-- The gate.
--
-- Called from ops.assert_risk_increase_permitted, after the audit-chain and
-- halt checks. A MATERIAL case is a reason to refuse, not a reason to halt: the
-- scope is narrower than a halt and the resolution path is ordinary case
-- management rather than an operational-mode transition requiring dual control.
-- Refusing here rather than raising a halt also means the block disappears the
-- moment the case is resolved, with nothing to clear.
--
-- Only MATERIAL blocks. A LOW or MEDIUM difference is recorded and tracked but
-- does not stop trading; blocking on every cosmetic rounding difference would
-- make the control unusable and would be switched off.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION reconciliation.assert_break_permitted(
    p_environment   common.environment,
    p_account_id    TEXT,
    p_instrument_id TEXT,
    p_venue_id      TEXT,
    p_is_risk_increasing BOOLEAN,
    p_context       TEXT
)
RETURNS VOID
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    v_case RECORD;
BEGIN
    -- Risk-reducing activity is never blocked. An unreconciled break must not
    -- prevent an operator from flattening exposure -- that would convert a data
    -- quality problem into an open position. This is the same rule the halt
    -- system follows, and for the same reason.
    IF NOT p_is_risk_increasing THEN
        RETURN;
    END IF;

    SELECT c.case_id, c.severity, c.difference_kind, c.detected_at, c.resolve_by
      INTO v_case
      FROM reconciliation.case c
     WHERE c.environment = p_environment
       AND c.severity = 'MATERIAL'
       -- RESOLVED and ACCEPTED_DIFFERENCE are the two terminal states. An
       -- ACCEPTED_DIFFERENCE is a break someone signed off as legitimate, so it
       -- must not keep blocking -- and demanding a second check to notice that
       -- is how a resolved case stays stuck blocking forever.
       AND c.status IN ('OPEN','INVESTIGATING','REOPENED')
       AND reconciliation.scope_covers(c.blocked_scope, p_account_id, p_instrument_id, p_venue_id)
     ORDER BY c.detected_at
     LIMIT 1;

    IF v_case.case_id IS NOT NULL THEN
        RAISE EXCEPTION
            'risk-increasing activity refused (%): reconciliation case % is an unresolved MATERIAL % break in % detected at % and due for resolution by %. Internal state for this scope is not known to match the venue, so new exposure would be taken against an unknown baseline (05_PERSISTENCE_EVENTING_RECONCILIATION.md). Risk-reducing activity remains permitted.',
            p_context, v_case.case_id, v_case.difference_kind, p_environment,
            v_case.detected_at, v_case.resolve_by
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

COMMENT ON FUNCTION reconciliation.assert_break_permitted IS
    'Refuses risk-increasing activity covered by an unresolved MATERIAL reconciliation case. Risk-reducing activity always passes, so an unreconciled break cannot prevent an operator from flattening exposure. The block is derived from reconciliation.case at the moment of the attempt, so it cannot desynchronise from the case that causes it and needs no separate clearing.';

-- -----------------------------------------------------------------------------
-- Wire it into the single gate.
--
-- ops.assert_risk_increase_permitted is reproduced in full and extended. It is
-- reproduced rather than wrapped for the reason 0013 gives: a trigger function
-- cannot delegate and defer to another without either duplicating the rules or
-- depending on an ordering PostgreSQL does not promise. The rules are the
-- control, so they are stated once, here.
--
-- ORDER OF CHECKS is deliberate and is the same order as 0013: the broken audit
-- chain first, because that is a SEV-1 security event and not overridable by
-- anything; then an active halt; then an unresolved material break. A caller
-- refused for several reasons at once is told about the most serious one, and
-- fixing it may reveal the next.
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
    IF NOT p_is_risk_increasing THEN
        RETURN;
    END IF;

    -- 1. Broken audit chain. SEV-1, checked first, not overridable.
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

    -- 2. An active halt at matching scope.
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

    -- 3. An unresolved MATERIAL reconciliation break covering this scope.
    --    Added by 0018. Before this, 0005 documented that a material break
    --    blocks affected risk-increasing scope and nothing implemented it.
    PERFORM reconciliation.assert_break_permitted(
        p_environment, p_account_id, p_instrument_id, p_venue_id,
        p_is_risk_increasing, p_context);
END;
$$;

COMMENT ON FUNCTION ops.assert_risk_increase_permitted IS
    'The single gate for risk-increasing activity, checked in order of severity: refuses when an audit partition chain is BROKEN (SEV-1, first and not overridable), when a matching ACTIVE halt exists, or when an unresolved MATERIAL reconciliation break covers the scope. Risk-reducing activity always passes, so neither a halt nor an unreconciled break can trap an operator in an open position. Called from the OMS risk boundary; all other callers should use this rather than reimplementing the rule.';
