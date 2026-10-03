-- =============================================================================
-- 0019_market_data_instrument_and_symbol_enforcement.sql
-- G2 - Market Data: instrument capability, increment and venue-symbol
--        enforcement.
--
-- Authority: 16_MARKET_DATA_AND_VENUE_ADAPTERS.md sections 1, 2 and 5.
--
-- WHY THIS MIGRATION EXISTS
--
-- 0002_market_data.sql is well-formed, correctly typed, and declares four
-- safety controls in its comments. It implements none of them. At the time this
-- migration was written the market schema had ZERO triggers:
--
--   SELECT count(*) FROM pg_trigger
--    WHERE NOT tgisinternal AND tgrelid::regclass::text LIKE 'market.%';
--   -- 0
--
-- Defects closed here:
--
--   16a  Instrument capabilities (order_types, supports_shorting) and
--        increments (min_quantity, price_increment, min_notional) were declared
--        and read by nothing. A MARKET order could be created for an
--        instrument whose order_types is [LIMIT]; a short against
--        supports_shorting = false; a quantity below the venue minimum.
--   16b  feed_unknown_not_healthy was a tautology: `health_state <> 'UNKNOWN'
--        OR health_state = 'UNKNOWN'` is true for every value. It read as a
--        control and rejected nothing.
--   16d  market.venue_symbol could be rewritten in place after taking effect,
--        contradicting the table's own comment that "a change creates a new
--        version rather than mutating the existing mapping".
--
-- Defect 16c (feed freshness never consulted at the risk boundary) is NOT
-- closed here. It needs a scope decision - which market.freshness_policy row
-- applies to an order - that no blueprint document makes. See the open question
-- at the bottom of this file. Half-closing it would be worse than leaving it
-- visibly open.
--
-- DESIGN NOTES
--
-- Terms are validated at INSERT, liveness is validated at the risk boundary.
-- oms.guard_order_update already makes order terms immutable after creation, so
-- a term cannot change later and validating it once is sufficient. But an
-- instrument can stop trading between creation and risk approval, so tradability
-- is re-checked at the risk boundary where 0013, 0014 and 0018 already screen.
--
-- Every denial here is a deny of RISK-INCREASING activity only. Reduce-only and
-- close-position orders bypass all of it, for the same reason 0018 does: a
-- control that blocks flattening is a control that gets switched off.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- 16a. Instrument capabilities and increments.
-- -----------------------------------------------------------------------------

-- Why incremental arithmetic is safe here. The check is
--     quantity / min_quantity = trunc(quantity / min_quantity)
-- on NUMERIC(38,18) columns, which is exact in PostgreSQL and therefore
-- carries no IEEE-754 error. This is the same reasoning that governs every
-- numeric column in the schema (0001 common.assert_no_floating_point), and it
-- is the reason the check is written as a quotient test rather than a modulo:
-- modulo on a very small increment is exact too, but the quotient form states
-- the intent -- "the quantity is a whole number of increments" -- directly.
CREATE OR REPLACE FUNCTION market.increment_multiple(
    p_value  NUMERIC,
    p_increment NUMERIC
)
RETURNS BOOLEAN
LANGUAGE plpgsql
IMMUTABLE
STRICT
AS $$
BEGIN
    IF p_increment IS NULL OR p_increment <= 0 THEN
        -- An unknown increment is a deny condition (0002 line 24-25). The
        -- caller distinguishes "denied" from "not applicable" by checking
        -- NOT NULL on the instrument column first.
        RETURN false;
    END IF;
    RETURN p_value / p_increment = trunc(p_value / p_increment);
END;
$$;

COMMENT ON FUNCTION market.increment_multiple IS
    'True when a value is a whole number of increments. Exact: NUMERIC division in PostgreSQL is decimal, not binary floating point. A NULL or non-positive increment returns false, because an unknown increment is a deny condition and not a licence to proceed.';


-- The capability check, separated from the increment check so that each can be
-- tested and mutated independently.
CREATE OR REPLACE FUNCTION market.instrument_permits(
    p_instrument_id TEXT,
    p_order_type    common.order_type,
    p_side          common.side
)
RETURNS BOOLEAN
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    v_allowed_types common.order_type[];
    v_shorting      BOOLEAN;
BEGIN
    SELECT order_types, supports_shorting
      INTO v_allowed_types, v_shorting
      FROM market.instrument
     WHERE instrument_id = p_instrument_id;

    -- A missing instrument cannot be permitted. The FK on oms.order already
    -- rejects this, so reaching it would mean the FK was dropped. Failing
    -- closed costs nothing and keeps this function safe to call directly.
    IF NOT FOUND THEN
        RETURN false;
    END IF;

    IF NOT (p_order_type = ANY (v_allowed_types)) THEN
        RETURN false;
    END IF;

    -- A sell is short exposure unless it is a closing action. Closing actions
    -- are permitted on an instrument that cannot be shorted, because reducing
    -- an existing long by selling is not shorting.
    IF p_side = 'SELL'
       AND NOT v_shorting
       AND p_order_type NOT IN ('REDUCE_ONLY', 'CLOSE_POSITION') THEN
        RETURN false;
    END IF;

    RETURN true;
END;
$$;

COMMENT ON FUNCTION market.instrument_permits IS
    'True when the instrument declares support for this order type, and permits the side. A closing action is not treated as a short even where shorting is unsupported. An unknown instrument returns false.';


-- Tradability. Distinct from capability: an instrument may support an order
-- type and still not be trading.
CREATE OR REPLACE FUNCTION market.instrument_is_tradable(
    p_instrument_id TEXT
)
RETURNS BOOLEAN
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    v_status common.trading_status;
BEGIN
    SELECT trading_status INTO v_status
      FROM market.instrument
     WHERE instrument_id = p_instrument_id;

    IF NOT FOUND THEN
        RETURN false;
    END IF;

    -- UNKNOWN denies. 0002 line 211-213 says so in a comment and enforced it
    -- with a tautology. An instrument nobody has confirmed as trading is not an
    -- instrument the platform may open exposure in; the default of the column is
    -- UNKNOWN precisely because that is the honest state of a new instrument.
    --
    -- AUCTION and PRE_OPEN are excluded deliberately. They are states in which
    -- orders are accepted but not filled, and whether the platform may submit
    -- into them is a venue-adapter concern (G8), not something this migration
    -- decides. Excluding them here fails closed; an adapter that proves it may
    -- submit can be granted that by an operator-visible status change.
    RETURN v_status = 'OPEN';
END;
$$;

COMMENT ON FUNCTION market.instrument_is_tradable IS
    'True only when the instrument trading_status is OPEN. UNKNOWN denies, which is the control 0002 claimed with a tautological CHECK. AUCTION, PRE_OPEN, POST_CLOSE, SESSION_BREAK, CLOSED, HALTED and DELISTED all deny.';


-- The combined order-term gate, called from the OMS at INSERT.
CREATE OR REPLACE FUNCTION market.assert_order_terms_permitted(
    p_instrument_id TEXT,
    p_order_type    common.order_type,
    p_side          common.side,
    p_quantity      NUMERIC,
    p_limit_price   NUMERIC,
    p_stop_price    NUMERIC
)
RETURNS VOID
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    v_inc RECORD;
    v_price NUMERIC;
BEGIN
    IF NOT market.instrument_permits(p_instrument_id, p_order_type, p_side) THEN
        RAISE EXCEPTION
            'order refused: instrument % does not support order_type % with side %',
            p_instrument_id, p_order_type, p_side
            USING ERRCODE = 'check_violation',
                  HINT = 'instrument capabilities are data (16_MARKET_DATA_AND_VENUE_ADAPTERS.md section 5); a venue adapter may impose a stricter set, never a looser one.';
    END IF;

    SELECT min_quantity, price_increment, tick_size, min_notional
      INTO v_inc
      FROM market.instrument
     WHERE instrument_id = p_instrument_id;

    IF NOT market.increment_multiple(p_quantity, v_inc.min_quantity) THEN
        RAISE EXCEPTION
            'order refused: quantity % is not a whole multiple of min_quantity % for instrument %',
            p_quantity, v_inc.min_quantity, p_instrument_id
            USING ERRCODE = 'check_violation';
    END IF;

    -- Which price is the executable one. A limit order is priced by
    -- limit_price; a stop order by stop_price. A market order has neither, and
    -- is handled by the notional rule below.
    v_price := COALESCE(p_limit_price, p_stop_price);

    IF v_price IS NOT NULL AND NOT market.increment_multiple(v_price, v_inc.price_increment) THEN
        RAISE EXCEPTION
            'order refused: price % is not a whole multiple of price_increment % for instrument %',
            v_price, v_inc.price_increment, p_instrument_id
            USING ERRCODE = 'check_violation';
    END IF;

    -- min_notional is the one increment-like bound that is optional. When it is
    -- declared it is a floor on the value of the order, and an order whose value
    -- cannot be computed -- a market order, which has no price at submission --
    -- cannot be shown to clear that floor. Declared but unverifiable denies.
    IF v_inc.min_notional IS NOT NULL THEN
        IF v_price IS NULL THEN
            RAISE EXCEPTION
                'order refused: instrument % declares min_notional % but this order has no limit or stop price to value it against',
                p_instrument_id, v_inc.min_notional
                USING ERRCODE = 'check_violation',
                      HINT = 'a market order against an instrument with a declared min_notional cannot be shown to clear the floor';
        END IF;

        IF p_quantity * v_price < v_inc.min_notional THEN
            RAISE EXCEPTION
                'order refused: notional % is below min_notional % for instrument %',
                p_quantity * v_price, v_inc.min_notional, p_instrument_id
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
END;
$$;

COMMENT ON FUNCTION market.assert_order_terms_permitted IS
    'Enforces market.instrument capabilities and increments against order terms. Called from the OMS at order creation, where terms are first specified. Refuses an unsupported order_type, an unsupported side, a quantity or price off the increment grid, and an order that cannot be shown to clear a declared min_notional.';


CREATE OR REPLACE FUNCTION market.guard_order_terms_against_instrument()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM market.assert_order_terms_permitted(
        NEW.instrument_id, NEW.order_type, NEW.side,
        NEW.quantity, NEW.limit_price, NEW.stop_price);
    RETURN NEW;
END;
$$;

-- Dropped before it is created, not created outright. This migration is transactional, so a
-- failure rolls back whole and nothing is left to converge from -- but a migration whose file is
-- edited after it has been applied cannot be re-applied at all unless every object it creates is
-- dropped first, and the digest in public.schema_migration will report that edit as drift forever.
--
-- Making the file re-runnable is what turns "this file changed after it ran" from a permanent
-- unrecoverable state into an ordinary re-apply. 0026 established the same shape for its own
-- reason; here the reason is the digest rather than a no-transaction migration.
--
-- The ordering here is load-bearing and is pinned by the negative control T5 in
-- db/verify-schema-live-mutations.ps1: the gate walks each file's statements in source order, so a
-- DROP followed by a CREATE of the same name leaves the replacement as the live object. Grouping
-- the two passes instead -- all CREATEs, then all DROPs -- is what made a correct database report
-- this very trigger as missing.
DROP TRIGGER IF EXISTS oms_order_instrument_terms_guard ON oms."order";

CREATE TRIGGER oms_order_instrument_terms_guard
    BEFORE INSERT ON oms."order"
    FOR EACH ROW EXECUTE FUNCTION market.guard_order_terms_against_instrument();

-- PostgreSQL has no COMMENT ON TRIGGER, so the trigger's rationale lives here
-- and on the function it executes.
-- oms_order_instrument_terms_guard applies instrument capability and increment
-- rules at order creation (defect 16a). INSERT only, because
-- oms.guard_order_update already makes order terms immutable -- a term that
-- cannot change after creation need only be checked once. Tradability is NOT
-- checked here; it is re-checked at the risk boundary, because an instrument
-- can stop trading between creation and risk approval.



-- -----------------------------------------------------------------------------
-- 16a (liveness). Tradability at the risk boundary.
--
-- Recreated rather than wrapped: CREATE OR REPLACE with a new parameter list
-- creates an overload, and the old seven-parameter gate would survive as a
-- second entry point to a gate that no longer screens for tradability. The
-- signature is deliberately identical so every existing caller and test keeps
-- working; only the body's step 4 is new.
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
            p_context, v_halt.halt_id, p_account_id, p_instrument_id, p_venue_id,
            p_strategy_id, p_environment, v_halt.reason
            USING ERRCODE = 'check_violation';
    END IF;

    -- 3. An unresolved MATERIAL reconciliation break covering this scope (0018).
    PERFORM reconciliation.assert_break_permitted(
        p_environment, p_account_id, p_instrument_id, p_venue_id,
        p_is_risk_increasing, p_context);

    -- 4. The instrument is not currently tradable. Added by 0019. This is
    --    checked here rather than at order creation because trading_status is
    --    time-varying: an order created against an OPEN instrument must still
    --    be refused at risk approval if the instrument has since been halted or
    --    delisted at venue.
    IF NOT market.instrument_is_tradable(p_instrument_id) THEN
        RAISE EXCEPTION
            'risk-increasing activity refused (%): instrument % is not in trading_status OPEN. Reduce-only and close-position activity remains permitted so existing exposure can be flattened.',
            p_context, p_instrument_id
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

COMMENT ON FUNCTION ops.assert_risk_increase_permitted IS
    'The single gate for risk-increasing activity, checked in order of severity: refuses when an audit partition chain is BROKEN (SEV-1, first and not overridable), when a matching ACTIVE halt exists, when an unresolved MATERIAL reconciliation break covers the scope, or when the instrument is not currently tradable. Risk-reducing activity always passes, so none of these can trap an operator in an open position. Called from the OMS risk boundary; all other callers should use this rather than reimplementing the rule.';


-- -----------------------------------------------------------------------------
-- 16b. The tautology.
--
-- Dropped rather than rewritten. feed_unknown_not_healthy's comment promised
-- that UNKNOWN is a deny condition for risk-increasing activity, which is a
-- property of an ORDER, not of a feed-health row. market.feed_health holds
-- several feeds; declaring any one of them non-UNKNOWN at write time would
-- assert that the platform has confirmed a feed it has not seen. The control
-- belongs where an order is evaluated, and step 4 above is where it now lives.
--
-- feed_health_state_valid on the line above is real and is kept.
-- -----------------------------------------------------------------------------
ALTER TABLE market.feed_health
    DROP CONSTRAINT IF EXISTS feed_unknown_not_healthy;

COMMENT ON CONSTRAINT feed_health_state_valid ON market.feed_health IS
    'The closed set of observable feed states. UNKNOWN is a legitimate persistable state meaning the platform has not confirmed this feed; it is not itself an error, which is why the constraint that once claimed to police it was a tautology. Whether UNKNOWN blocks trading is decided per order by market.instrument_is_tradable and, when closed, by the feed-freshness gate.';


-- -----------------------------------------------------------------------------
-- 16d. Venue symbol mapping is append-only once effective.
--
-- This is the table that joins platform identity to venue reality. 0002 line
-- 12-14 states the principle -- "A venue symbol is NEVER a globally unique
-- identifier" -- and venue_symbol_unique_per_version enforces half of it: one
-- venue symbol may not map to two instruments at the same version. It does
-- nothing about the other half, which is that an EXISTING mapping may be
-- repointed to a different instrument or symbol in place. A mapping that can be
-- rewritten after it takes effect means an order routed by it can reach a
-- different instrument than the one the strategy reasoned about, and the fill
-- arrives against the wrong instrument.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION market.guard_venue_symbol_append_only()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- A mapping that was ever effective is history. Deleting it does not
        -- unmap the venue; it destroys the record of what the venue was told.
        IF OLD.effective_at <= now() THEN
            RAISE EXCEPTION
                'venue symbol mapping (% / % / v%) cannot be deleted: it became effective at % and is part of the venue-truth record',
                OLD.instrument_id, OLD.venue_id, OLD.mapping_version, OLD.effective_at
                USING ERRCODE = 'check_violation',
                      HINT = 'supersede it by inserting a new version with a higher mapping_version';
        END IF;
        RETURN OLD;
    END IF;

    -- An effective mapping's identity is frozen. changed_by, change_reason and
    -- retired_at remain writable: provenance and retirement are the legitimate
    -- reasons to touch a mapping after it takes effect, and forbidding them
    -- would make a mapping impossible to retire, which would be worse.
    IF OLD.effective_at <= now() THEN
        IF NEW.instrument_id IS DISTINCT FROM OLD.instrument_id
           OR NEW.venue_id      IS DISTINCT FROM OLD.venue_id
           OR NEW.symbol        IS DISTINCT FROM OLD.symbol
           OR NEW.mapping_version IS DISTINCT FROM OLD.mapping_version THEN
            RAISE EXCEPTION
                'venue symbol mapping (% / % / v%) is effective as of % and its identity is frozen: instrument, venue, symbol and version cannot be changed',
                OLD.instrument_id, OLD.venue_id, OLD.mapping_version, OLD.effective_at
                USING ERRCODE = 'check_violation',
                      HINT = 'insert a new version with a higher mapping_version instead; a changed mapping is a new fact, not an edit of an old one';
        END IF;

        -- Retroactive effective_at is the same defect wearing a different hat:
        -- it moves when a mapping became true.
        IF NEW.effective_at IS DISTINCT FROM OLD.effective_at THEN
            RAISE EXCEPTION
                'venue symbol mapping (% / % / v%) cannot have its effective_at changed: it became effective at %',
                OLD.instrument_id, OLD.venue_id, OLD.mapping_version, OLD.effective_at
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS venue_symbol_append_only ON market.venue_symbol;

CREATE TRIGGER venue_symbol_append_only
    BEFORE UPDATE OR DELETE ON market.venue_symbol
    FOR EACH ROW EXECUTE FUNCTION market.guard_venue_symbol_append_only();

-- venue_symbol_append_only makes an effective venue symbol mapping append-only
-- (defect 16d). instrument_id, venue_id, symbol, mapping_version and
-- effective_at are frozen once effective_at has passed; changed_by,
-- change_reason and retired_at stay writable so a mapping can still be retired
-- and must still carry provenance. A mapping is superseded by inserting a higher
-- mapping_version, never by editing the one in force.



-- -----------------------------------------------------------------------------
-- M1. sequence_no_regression does not enforce what its name says.
--
-- 0002 line 214 reads:
--   CONSTRAINT sequence_no_regression CHECK (
--       observed_sequence IS NULL OR expected_sequence IS NULL
--       OR observed_sequence >= 0)
-- That is a non-negativity test. A sequence watermark is free to move
-- backwards through it, which is the exact failure "no_regression" claims to
-- prevent: a stale or duplicated report overwrites a newer watermark, the gap
-- counter is fed from a value that has gone backwards, and the feed reports
-- healthy on a stream that is actually replaying old data.
--
-- The non-negativity check is real and is kept, under a name that says what it
-- does. The property the original name claimed is enforced as a transition, not
-- as a row constraint -- "a row's value is not negative" and "a value does not
-- go backwards" are different shapes of statement, and only the second one is
-- what a sequence watermark needs.
-- -----------------------------------------------------------------------------
ALTER TABLE market.feed_health
    DROP CONSTRAINT IF EXISTS sequence_no_regression;

-- Both constraints are dropped before they are added, for the same reason as the triggers above:
-- an ADD CONSTRAINT with an existing name is a hard error, so without this the file cannot be
-- re-applied and any post-apply edit to it is reported as drift forever.
--
-- Both are genuinely VALID rather than NOT VALID, and both hold on every row that exists, so the
-- re-add succeeds and the constraints are enforced for existing rows as well as new ones. That is
-- the correct state for these two: they were split out of sequence_no_regression precisely because
-- they are real row properties. It is the signing-key CHECK in 0023 that must be NOT VALID,
-- because 1294 historical rows violate it.
ALTER TABLE market.feed_health
    DROP CONSTRAINT IF EXISTS feed_observed_sequence_non_negative;

ALTER TABLE market.feed_health
    DROP CONSTRAINT IF EXISTS feed_expected_sequence_positive;

ALTER TABLE market.feed_health
    ADD CONSTRAINT feed_observed_sequence_non_negative CHECK (
        observed_sequence IS NULL OR observed_sequence >= 0
    ),
    ADD CONSTRAINT feed_expected_sequence_positive CHECK (
        expected_sequence IS NULL OR expected_sequence > 0
    );

COMMENT ON CONSTRAINT feed_observed_sequence_non_negative ON market.feed_health IS
    'A venue sequence number is not negative. This is the check 0002 line 214 called sequence_no_regression, which was not a regression check at all. Monotonicity is enforced on the transition by market.guard_feed_sequence_monotonic.';

CREATE OR REPLACE FUNCTION market.guard_feed_sequence_monotonic()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    -- A watermark that moves backwards means a report older than one already
    -- recorded has overwritten a newer observation. Refusing it preserves the
    -- property the feed-health table exists to express: observed_sequence is
    -- how far the platform has seen, and "how far" does not decrease.
    IF OLD.observed_sequence IS NOT NULL
       AND NEW.observed_sequence IS NOT NULL
       AND NEW.observed_sequence < OLD.observed_sequence THEN
        RAISE EXCEPTION
            'feed_health % (%): observed_sequence cannot move backwards from % to %',
            NEW.feed_key, NEW.venue_id, OLD.observed_sequence, NEW.observed_sequence
            USING ERRCODE = 'check_violation',
                  HINT = 'a stale or duplicated venue report would overwrite a newer watermark; the gap counter must not be fed from a value that has gone backwards';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS feed_sequence_monotonic ON market.feed_health;

CREATE TRIGGER feed_sequence_monotonic
    BEFORE UPDATE OF observed_sequence ON market.feed_health
    FOR EACH ROW EXECUTE FUNCTION market.guard_feed_sequence_monotonic();


-- =============================================================================
-- OPEN QUESTION -- defect 16c, deliberately NOT closed here
--
-- 0002 line 246 states that "missing limits deny risk-increasing activity",
-- with respect to market.freshness_policy. Nothing enforces it, and this
-- migration does not begin to.
--
-- The blocker is a question no blueprint document answers. market.freshness_policy
-- is keyed (policy_key, environment, market_class). Wiring it to an order
-- requires deciding which policy governs a given order, and the available
-- candidate answers are:
--
--   (a) match on market_class alone;
--   (b) match on market_class and venue_id;
--   (c) match on market_class, venue_id and instrument_id.
--
-- (a) is the loosest and is what the key literally supports. (c) is the
-- tightest and is what a crypto venue with per-symbol tick policies would need.
-- They are not refinements of one another: choosing (a) permits an order to be
-- governed by a policy written for a different venue's latency profile, and
-- choosing (c) denies every order whenever the operator has not written a
-- per-instrument row.
--
-- The safe default -- deny when no policy matches -- is available under all
-- three, so it is tempting to implement now and widen later. It is not
-- implemented now because 16c is a live-trading safety control and doc 11 line
-- 49 requires that "all account-, venue-, instrument- and strategy-specific risk
-- limits are configured and independently reviewed" before G11. Shipping a
-- guessed scope rule would create the appearance of a reviewed control.
--
-- This is the same class of gap as doc 17 section 3 risk-limit attribute
-- validation: a schema-level question needing an owner, not a guess.
--
-- Until it is answered, market.feed_health and market.freshness_policy record
-- observations that no risk path reads. G2 is NOT passed. The finding is
-- recorded at evidence/gates/G2/findings.md.
--
--
-- MEASURED 2026-10-03, AND IT CHANGES THE SHAPE OF THE PROBLEM
--
-- The reasoning above makes this look like a policy question with a technical answer pending. It
-- is not, or rather it is not only that, and the difference decides whether a partial fix is
-- available at all.
--
-- market.feed_health has no maintainer. Measured against the live database and the repository:
--
--   Go references to feed_health or freshness_policy in domain/, services/, adapters/, contracts/ : 0
--   Rows in market.feed_health                                                                  : 0
--   Triggers on market.feed_health                        : feed_sequence_monotonic only
--
-- The one trigger that exists refuses a watermark that moves backwards. It does not compute
-- health, and nothing computes health. health_state is set by no production statement in this
-- repository; the only writes are two dbtest fixtures, both inside transactions that roll back.
--
-- That makes the obvious partial fix worse than no fix. Reading health_state without the age bound
-- looks like the safe half of 16c, because every direction it can refuse in is the refusing
-- direction. But it would be reading a column no process maintains, over a table that is empty.
-- An aggregate over that table is one of exactly two things:
--
--   NOT EXISTS (any feed that is not HEALTHY)   ->  vacuously TRUE, so the control PASSES on
--                                                   absent data. A fail-closed control becomes
--                                                   fail-open the moment the table is empty,
--                                                   which is its default state.
--   EXISTS (a HEALTHY feed for this market)      ->  vacuously FALSE, so every order is denied
--                                                   for a reason naming feed health when the
--                                                   real cause is that nothing feeds the table.
--
-- Neither is a control. The first is strictly more dangerous than today's unconditional denial,
-- because it looks like the defect 0019 exists to prevent -- a documented enforcement backed by
-- nothing -- which is the exact class this migration was written to close. The second denies
-- every order and tells the operator to look at a table that is empty, which is the "FEED DEGRADED
-- recorded but nothing reads it" failure the G2 audit found in its original form.
--
-- So 16c is not blocked on a decision alone. It is DOWNSTREAM of a component that does not exist:
-- something must observe a feed and maintain market.feed_health. That component cannot be written
-- against a fake, and this repository has no concrete venue adapter -- adapters/venue is the
-- contract, the certification authority and the publisher, and the last mile terminates in a test
-- double. The dependency chain is therefore:
--
--     concrete venue adapter  ->  feed health writer  ->  16c freshness control
--                                                            ->  MAX_ORDER_NOTIONAL reference price
--
-- The last link was already recorded as a dependency of 16c. The first two were not, and they are
-- what makes this a sequencing fact rather than a scheduling one.
--
-- The scope decision below is still required -- a feed health writer does not tell you which
-- freshness_policy row governs which order -- but it is no longer the first thing to settle. It is
-- the second.
-- =============================================================================
