-- 0014_halt_enforcement_and_chain_sev1 -> 0014_operational_mode_enforcement.sql
--
-- Operational mode enforcement: RECOVERY_HOLD, the kill switch, and the
-- restricted modes actually block risk-increasing activity.
--
-- WHY THIS MIGRATION EXISTS
-- ------------------------
-- 0007 declared on ops.system_state:
--
--   "RECOVERY_HOLD and the kill switch are each independently block risk-
--    increasing activity and cannot be disabled without dual control.
--    Recovery never automatically resumes risk-increasing operation."
--
-- None of that was implemented. The only constraints on the table were
-- row-shape CHECKs -- recovery_hold_consistent and kill_switch_consistent
-- merely require a reason and an actor to be present *while the flag is set*.
-- Nothing in any migration ever READ either column. So a recovered region
-- resumed risk-increasing operation exactly as an unrecovered one would, and
-- the kill switch was an inert bit.
--
-- That directly contradicts two blueprint requirements:
--   10_OPERATIONS_AND_DISCREASTE_RECOVERY.md line 41 -- after recovery the
--     system starts in RECOVERY_HOLD and "new risk-increasing commands,
--     strategy deployment, credential rotation, and live activation remain
--     blocked".
--   10_OPERATIONS_AND_DISCREASTE_RECOVERY.md, recovery decision table --
--     "Can a recovered region automatically resume risk-increasing behavior?"
--     Answer required: "No."
--   10_OPERATIONS_AND_DISCREASTE_RECOVERY.md line 41 of the objectives list --
--     "No restored environment may leave RECOVERY_HOLD without independent
--     verification."
--
-- 11_EXECUTION_GATES.md prohibits waivers for recovery objectives, halt
-- controls, and financial invariants. 09_TESTING_AND_RELEASE_EVIDENCE.md
-- fault-injection tests must cover credential rejection and database failover.
-- A silent recovery is the failure mode all three exist to prevent.
--
-- This is the same defect class as the halt table in 0013: a control that was
-- fully specified, commented as though implemented, and read by nothing.
--
-- A SECOND, OPPOSITE DEFECT
-- -------------------------
-- 0007 also asserted dual control with:
--
--   CONSTRAINT restricted_mode_dual_control CHECK (
--       (mode IN ('NORMAL','READ_ONLY')) OR mode_approved_by IS NOT NULL)
--
-- That requires a second approver to *ENTER* a restricted mode, and says
-- nothing about *leaving* one. It is backwards, and it is dangerous.
--
-- 06_SECURITY_AND_ACCESS_CONTROL.md line 23 states the principle for the
-- analogous control: "Emergency halt is always available to authorized halt
-- operators and does not require a second approver; re-enable does."
--
-- Requiring a second approver to engage RECOVERY_HOLD means that during the
-- incident the control exists for -- a region failover, a restore from an
-- untrusted backup -- the database may refuse to enter the safe state, because
-- a second operator is not yet awake. A control that is hard to engage and easy
-- to leave is worse than no control, because it is relied upon.
--
-- Even the "dual control" it did provide was nominal: mode_approved_by had only
-- to be non-null, and nothing prevented it being the same identity as
-- mode_changed_by. Compare ops.halt, which has a cleared_approved_by, and
-- identity.role_binding, which has a test asserting a distinct approver.
-- ops.system_state was the outlier in accepting a self-approval.
--
-- So this migration does three separable things:
--   1. Makes engaging a restricted mode cheap and leaving it expensive.
--   2. Makes the approvers provably distinct identities.
--   3. Makes the flags actually block risk-increasing activity.

-- -----------------------------------------------------------------------------
-- 1. Release evidence columns
-- -----------------------------------------------------------------------------
-- Leaving a restricted state must leave evidence, not just a boolean. These
-- record who released it, who independently verified the release, and when.
ALTER TABLE ops.system_state
    ADD COLUMN recovery_cleared_by         TEXT,
    ADD COLUMN recovery_cleared_approved_by TEXT,
    ADD COLUMN recovery_cleared_at         TIMESTAMPTZ,
    -- Deliberately separate from cleared_approved_by. "No restored environment
    -- may leave RECOVERY_HOLD without independent verification" -- verification
    -- is a different act from approval, and collapsing them lets the person who
    -- approved the resume be the person who checked the evidence.
    ADD COLUMN recovery_verified_by        TEXT,
    ADD COLUMN kill_switch_cleared_by        TEXT,
    ADD COLUMN kill_switch_cleared_approved_by TEXT,
    ADD COLUMN kill_switch_cleared_at        TIMESTAMPTZ;

-- Replace the backwards CHECK.
--
-- Engagement now requires an accountable actor, which is the honest minimum:
-- somebody must be able to say who put the system into RECOVERY_HOLD. It does
-- not require a second person, because the whole point is that engaging must be
-- possible at 3am by one authorized operator.
ALTER TABLE ops.system_state
    DROP CONSTRAINT restricted_mode_dual_control;

ALTER TABLE ops.system_state
    ADD CONSTRAINT mode_engaged_with_accountable_actor CHECK (
        (mode IN ('NORMAL','READ_ONLY')) OR mode_changed_by IS NOT NULL);

-- Release consistency: the evidence must be complete the moment the flag drops.
-- These are row-shape invariants and are checked by the database even if the
-- transition trigger is ever bypassed.
ALTER TABLE ops.system_state
    ADD CONSTRAINT recovery_release_recorded CHECK (
        (recovery_hold = true) OR
        (recovery_cleared_by IS NULL AND recovery_cleared_approved_by IS NULL
         AND recovery_cleared_at IS NULL AND recovery_verified_by IS NULL)
        OR
        (recovery_cleared_by IS NOT NULL AND recovery_cleared_approved_by IS NOT NULL
         AND recovery_cleared_at IS NOT NULL AND recovery_verified_by IS NOT NULL)
    ),
    ADD CONSTRAINT kill_switch_release_recorded CHECK (
        (kill_switch_engaged = true) OR
        (kill_switch_cleared_by IS NULL AND kill_switch_cleared_approved_by IS NULL
         AND kill_switch_cleared_at IS NULL)
        OR
        (kill_switch_cleared_by IS NOT NULL AND kill_switch_cleared_approved_by IS NOT NULL
         AND kill_switch_cleared_at IS NOT NULL)
    );

COMMENT ON COLUMN ops.system_state.recovery_verified_by IS
    'Independent verifier of the release from RECOVERY_HOLD. Distinct from the approver by control, not by convention: 10_OPERATIONS_AND_DISCREASTE_RECOVERY.md requires independent verification before a restored environment resumes.';

-- -----------------------------------------------------------------------------
-- 2. Transition guard: engage freely, release with dual control
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ops.guard_system_state_transition()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    -- Engaging a restricted mode. One authorized actor, a reason, no second
    -- approver. 06_SECURITY_AND_ACCESS_CONTROL.md line 23.
    IF NEW.mode IS DISTINCT FROM OLD.mode AND NEW.mode NOT IN ('NORMAL','READ_ONLY') THEN
        IF NEW.mode_changed_by IS NULL OR btrim(NEW.mode_change_reason) IS NULL THEN
            RAISE EXCEPTION
                'Entering operational mode % requires mode_changed_by and mode_change_reason. Engaging a restricted mode deliberately needs no second approver, so the control is available at 03:00 by one authorized operator (06_SECURITY_AND_ACCESS_CONTROL.md section 4).',
                NEW.mode
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    -- Leaving a restricted mode. Dual control, and the approver must be a
    -- genuinely different identity.
    IF NEW.mode IS DISTINCT FROM OLD.mode
       AND OLD.mode NOT IN ('NORMAL','READ_ONLY')
       AND NEW.mode IN ('NORMAL','READ_ONLY') THEN
        IF NEW.mode_approved_by IS NULL THEN
            RAISE EXCEPTION
                'Leaving operational mode % requires a second approver in mode_approved_by. Engaging is unilateral by design; re-enabling is not (06_SECURITY_AND_ACCESS_CONTROL.md section 4).',
                OLD.mode
                USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.mode_approved_by = NEW.mode_changed_by THEN
            RAISE EXCEPTION
                'mode_approved_by (%) must be a distinct identity from mode_changed_by (%). Dual control requires two people; a self-approval is not dual control.',
                NEW.mode_approved_by, NEW.mode_changed_by
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    -- Releasing RECOVERY_HOLD. Dual control plus independent verification.
    IF OLD.recovery_hold AND NOT NEW.recovery_hold THEN
        IF NEW.recovery_cleared_by IS NULL
           OR NEW.recovery_cleared_approved_by IS NULL
           OR NEW.recovery_cleared_at IS NULL
           OR NEW.recovery_verified_by IS NULL THEN
            RAISE EXCEPTION
                'Leaving RECOVERY_HOLD on system state % requires recovery_cleared_by, recovery_cleared_approved_by, recovery_cleared_at and recovery_verified_by. A restored environment may not resume risk-increasing operation without recorded, independent verification (10_OPERATIONS_AND_DISCREASTE_RECOVERY.md).',
                OLD.system_state_id
                USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.recovery_cleared_approved_by = NEW.recovery_cleared_by THEN
            RAISE EXCEPTION
                'recovery_cleared_approved_by (%) must differ from recovery_cleared_by (%).',
                NEW.recovery_cleared_approved_by, NEW.recovery_cleared_by
                USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.recovery_verified_by IN (NEW.recovery_cleared_by, NEW.recovery_cleared_approved_by) THEN
            RAISE EXCEPTION
                'recovery_verified_by (%) must be independent of both recovery_cleared_by and recovery_cleared_approved_by; verification by a participant in the release is not verification.',
                NEW.recovery_verified_by
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    -- Releasing the kill switch. The last-resort control, so its release is
    -- dual-control as well; it is deliberately NOT required to be verified by a
    -- third party, because the kill switch is engaged during an active incident
    -- and requiring a third awake person to stand it down would make it a
    -- one-way door.
    IF OLD.kill_switch_engaged AND NOT NEW.kill_switch_engaged THEN
        IF NEW.kill_switch_cleared_by IS NULL
           OR NEW.kill_switch_cleared_approved_by IS NULL
           OR NEW.kill_switch_cleared_at IS NULL THEN
            RAISE EXCEPTION
                'Releasing the kill switch on system state % requires kill_switch_cleared_by, kill_switch_cleared_approved_by and kill_switch_cleared_at.',
                OLD.system_state_id
                USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.kill_switch_cleared_approved_by = NEW.kill_switch_cleared_by THEN
            RAISE EXCEPTION
                'kill_switch_cleared_approved_by (%) must differ from kill_switch_cleared_by (%).',
                NEW.kill_switch_cleared_approved_by, NEW.kill_switch_cleared_by
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER system_state_transition_guard
    BEFORE UPDATE ON ops.system_state
    FOR EACH ROW EXECUTE FUNCTION ops.guard_system_state_transition();

COMMENT ON FUNCTION ops.guard_system_state_transition() IS
    'Asymmetric by design: engaging a restricted operational mode needs one accountable actor so the control is always available during an incident, and leaving it needs dual control, distinct identities, and -- for RECOVERY_HOLD -- an independent verifier.';

-- -----------------------------------------------------------------------------
-- 3. Seed every environment
-- -----------------------------------------------------------------------------
-- assert_operational_mode_permitted fails CLOSED for risk-increasing activity
-- when an environment has no ops.system_state row. That is the correct posture
-- for a mandatory financial control: an unrecorded operational mode must not be
-- read as "unrestricted".
--
-- Failing closed only works if every environment is present, so all six are
-- seeded at NORMAL here. common.environment is a closed enum, so this INSERT
-- covers every value that can ever exist; the row count is asserted in the test
-- suite rather than trusted.
-- The identifier is 'sys_' plus 20 hex characters of md5. Hex digits are a
-- subset of the Crockford alphabet (0-9a-f are all members), so a 20-character
-- hex digest is already a well-formed canonical id under
-- common.is_canonical_id and needs no alphabet translation.
INSERT INTO ops.system_state (system_state_id, environment, mode)
SELECT 'sys_' || substr(md5('ops.system_state:' || e::text), 1, 20), e, 'NORMAL'
  FROM unnest(enum_range(NULL::common.environment)) AS e
 WHERE true
ON CONFLICT (environment) DO NOTHING;

-- -----------------------------------------------------------------------------
-- 4. The gate
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ops.assert_operational_mode_permitted(
    p_environment   common.environment,
    p_risk_increase BOOLEAN,
    p_context       TEXT
)
RETURNS VOID
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    v_mode     TEXT;
    v_recovery BOOLEAN;
    v_kill     BOOLEAN;
    v_reason   TEXT;
BEGIN
    -- Risk-reducing activity is never blocked by a restricted mode.
    --
    -- This is an interpretation, and it is deliberate. 10_OPERATIONS_AND_DISCREASTE_RECOVERY.md
    -- names what RECOVERY_HOLD blocks -- "new risk-increasing commands,
    -- strategy deployment, credential rotation, and live activation" -- and
    -- risk-reducing orders are not on that list. It is also the same reasoning
    -- that governs ops.halt: a mode that also prevented an operator from
    -- flattening a position would convert a contained incident into unbounded
    -- loss with no available remedy.
    --
    -- The instrument for freezing risk-reducing activity is an ACTIVE halt,
    -- which an authorized operator can raise for one account, one venue or the
    -- whole system, with a recorded reason. It is the right tool, and it is
    -- available when an operator genuinely wants nothing moving.
    IF NOT p_risk_increase THEN
        RETURN;
    END IF;

    SELECT mode, recovery_hold, kill_switch_engaged
      INTO v_mode, v_recovery, v_kill
      FROM ops.system_state
     WHERE environment = p_environment;

    IF NOT FOUND THEN
        RAISE EXCEPTION
            'risk-increasing activity refused (%): environment % has no ops.system_state row, so no operational mode is recorded. Failing closed is deliberate; an unrecorded mode is not an unrestricted mode. Seed the environment.',
            p_context, p_environment
            USING ERRCODE = 'check_violation';
    END IF;

    -- Precedence: the kill switch is the last-resort control and is reported
    -- first, because it is the condition that explains the most severe
    -- incident. RECOVERY_HOLD is next. Restricted modes are last.
    IF v_kill THEN
        v_reason := 'kill switch engaged: ' || COALESCE(
            (SELECT kill_switch_reason FROM ops.system_state WHERE environment = p_environment),
            'no reason recorded');
    ELSIF v_recovery THEN
        v_reason := 'RECOVERY_HOLD active: ' || COALESCE(
            (SELECT recovery_reason FROM ops.system_state WHERE environment = p_environment),
            'no reason recorded');
    ELSIF v_mode IN ('SAFE_HALT','READ_ONLY','CANCEL_ONLY','RECOVERY_HOLD') THEN
        v_reason := 'operational mode is ' || v_mode;
    ELSE
        v_reason := NULL;
    END IF;

    IF v_reason IS NOT NULL THEN
        RAISE EXCEPTION
            'risk-increasing activity refused (%): %',
            p_context, v_reason
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

COMMENT ON FUNCTION ops.assert_operational_mode_permitted IS
    'Blocks risk-increasing activity when the environment is in a restricted operational mode, is in RECOVERY_HOLD, or has the kill switch engaged. Risk-reducing activity is never blocked: preventing an operator from flattening is a loss multiplier, not a control. Fails closed when an environment has no recorded mode.';

-- -----------------------------------------------------------------------------
-- 5. Wire it to the OMS order state machine
-- -----------------------------------------------------------------------------
-- A separate trigger rather than another section inside oms.guard_order_update,
-- so the halt gate and the operational-mode gate are independent. A single
-- combined guard would let one failing check mask the other in the logs, and
-- the two controls answer different questions and are owned differently.
CREATE OR REPLACE FUNCTION ops.guard_operational_mode_on_order()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_market common.market_class;
BEGIN
    -- Fires at the same point as the halt gate: the transition out of
    -- CREATED/RISK_PENDING, where an order stops being an intention and becomes
    -- possible exposure.
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RETURN NEW;
    END IF;

    IF NEW.state IN ('CREATED','RISK_PENDING') THEN
        RETURN NEW;
    END IF;

    SELECT i.market_class INTO v_market
      FROM market.instrument i
     WHERE i.instrument_id = NEW.instrument_id;

    PERFORM ops.assert_operational_mode_permitted(
        NEW.environment,
        NEW.order_type NOT IN ('REDUCE_ONLY','CLOSE_POSITION'),
        'OMS order ' || NEW.order_id || ' ' || OLD.state::text || ' -> ' || NEW.state::text);

    RETURN NEW;
END;
$$;

CREATE TRIGGER order_operational_mode_guard
    BEFORE UPDATE OF state ON oms.order
    FOR EACH ROW EXECUTE FUNCTION ops.guard_operational_mode_on_order();

COMMENT ON FUNCTION ops.guard_operational_mode_on_order() IS
    'Applies the operational-mode gate at the OMS risk boundary. Independent of the halt gate in 0013 by design: the two answer different questions and neither should mask the other.';
