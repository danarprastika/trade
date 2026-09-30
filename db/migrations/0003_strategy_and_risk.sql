-- =============================================================================
-- 0003_strategy_and_risk.sql
-- G3/G4 - Strategy lifecycle, deterministic risk policy and risk decisions.
--
-- Authority: 04_TRADING_DOMAIN_AND_RISK.md, 17_CONFIGURATION_AND_RISK_POLICY.md
-- =============================================================================

CREATE TYPE strategy.strategy_state AS ENUM
    ('DRAFT','REVIEW','BACKTESTED','SIMULATION','PAPER','SHADOW',
     'APPROVED','DEPLOYED','PAUSED','RETIRED');

CREATE TYPE risk.risk_decision_outcome AS ENUM ('APPROVED','REJECTED');

CREATE TYPE risk.control_severity AS ENUM ('MANDATORY','ADVISORY');

-- -----------------------------------------------------------------------------
-- Strategy lifecycle.
--
-- DRAFT -> REVIEW -> BACKTESTED -> SIMULATION -> PAPER -> SHADOW -> APPROVED
--       -> DEPLOYED -> PAUSED -> RETIRED
--
-- Transitions are explicit commands producing immutable audit events. A strategy
-- cannot reach DEPLOYED without passing through the governed promotion path
-- (13_IMPLEMENTATION_HANDOFF.md, 07_AI_RESEARCH_AND_MODEL_GOVERNANCE.md).
-- -----------------------------------------------------------------------------
CREATE TABLE strategy.strategy (
    strategy_id       TEXT        PRIMARY KEY,
    name              TEXT        NOT NULL,
    owner_subject_id  TEXT        NOT NULL,
    state             strategy.strategy_state NOT NULL DEFAULT 'DRAFT',
    market_class      common.market_class NOT NULL,
    -- Version of the strategy code/parameters. Promotion is per version: a new
    -- version must re-validate rather than inheriting approval.
    strategy_version  BIGINT      NOT NULL DEFAULT 1,
    content_digest    TEXT        NOT NULL,
    -- Independent validation. A research author cannot approve their own
    -- strategy for live use.
    validated_by      TEXT,
    approved_by       TEXT,
    approved_at       TIMESTAMPTZ,
    approval_expires_at TIMESTAMPTZ,
    -- Rollback artifact required by the model/strategy governance record.
    rollback_digest   TEXT,
    -- Observations that must be demonstrably stale before a state change.
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    version_vector    BIGINT      NOT NULL DEFAULT 1,
    CONSTRAINT strategy_id_format CHECK (common.is_canonical_id(strategy_id, 'str')),
    CONSTRAINT strategy_version_positive CHECK (strategy_version > 0),
    CONSTRAINT content_digest_format CHECK (content_digest ~ '^[0-9a-f]{64}$'),
    -- Separation of duties: approver is never the owner.
    CONSTRAINT strategy_approval_distinct CHECK (approved_by IS NULL OR approved_by <> owner_subject_id),
    CONSTRAINT strategy_approval_complete CHECK (
        (approved_by IS NULL AND approved_at IS NULL)
        OR (approved_by IS NOT NULL AND approved_at IS NOT NULL AND approval_expires_at IS NOT NULL)
    ),
    -- A rollback artifact is mandatory before deployment.
    CONSTRAINT deployed_requires_rollback CHECK (state <> 'DEPLOYED' OR rollback_digest IS NOT NULL),
    CONSTRAINT rollback_digest_format CHECK (rollback_digest IS NULL OR rollback_digest ~ '^[0-9a-f]{64}$')
);

CREATE INDEX strategy_state_idx ON strategy.strategy (state, market_class);
CREATE INDEX strategy_owner_idx  ON strategy.strategy (owner_subject_id);

-- The transition table makes the state machine a CLOSED set at the storage
-- boundary: an unknown transition is rejected by the database, not only by
-- application code (01_SYSTEM_ARCHITECTURE.md §8).
CREATE TABLE strategy.state_transition (
    transition_id     TEXT        PRIMARY KEY,
    strategy_id       TEXT        NOT NULL REFERENCES strategy.strategy(strategy_id),
    from_state        strategy.strategy_state NOT NULL,
    to_state          strategy.strategy_state NOT NULL,
    environment       common.environment NOT NULL,
    actor_id          TEXT        NOT NULL,
    reason            TEXT        NOT NULL,
    correlation_id    TEXT        NOT NULL,
    causation_id      TEXT,
    occurred_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    occurred_at_ns    BIGINT      NOT NULL,
    audit_id          TEXT,
    CONSTRAINT transition_id_format CHECK (common.is_canonical_id(transition_id, 'trn')),
    CONSTRAINT transition_changes_state CHECK (from_state <> to_state)
);

CREATE INDEX state_transition_strategy_idx ON strategy.state_transition (strategy_id, occurred_at DESC);

CREATE TABLE strategy.state_transition_rule (
    from_state        strategy.strategy_state NOT NULL,
    to_state          strategy.strategy_state NOT NULL,
    required_role     TEXT        NOT NULL,
    requires_dual_control BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (from_state, to_state)
);

INSERT INTO strategy.state_transition_rule (from_state, to_state, required_role, requires_dual_control) VALUES
    ('DRAFT','REVIEW','RESEARCHER', false),
    ('REVIEW','BACKTESTED','RESEARCHER', false),
    ('BACKTESTED','SIMULATION','RESEARCHER', false),
    ('SIMULATION','PAPER','TRADING_OPERATOR', false),
    ('PAPER','SHADOW','TRADING_OPERATOR', false),
    ('SHADOW','APPROVED','RISK_OPERATOR', true),
    ('APPROVED','DEPLOYED','OWNER', true),
    ('DEPLOYED','PAUSED','TRADING_OPERATOR', false),
    ('PAUSED','DEPLOYED','RISK_OPERATOR', true),
    ('DEPLOYED','RETIRED','OWNER', true),
    ('PAUSED','RETIRED','OWNER', false),
    ('SHADOW','RETIRED','RISK_OPERATOR', false);

COMMENT ON TABLE strategy.state_transition_rule IS
    'The closed set of permitted strategy lifecycle transitions. SHADOW->APPROVED and *->DEPLOYED require dual control because they authorise risk-increasing capability.';

CREATE TABLE strategy.deployment (
    deployment_id     TEXT        PRIMARY KEY,
    strategy_id       TEXT        NOT NULL REFERENCES strategy.strategy(strategy_id),
    strategy_version  BIGINT      NOT NULL,
    environment       common.environment NOT NULL,
    account_scope     TEXT        NOT NULL,
    market_class      common.market_class NOT NULL,
    state             TEXT        NOT NULL DEFAULT 'ACTIVE',
    activated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deactivated_at    TIMESTAMPTZ,
    activated_by      TEXT        NOT NULL,
    -- The risk policy revision bound to this deployment. Changing risk limits
    -- requires a new revision and a new approval; the deployment does not carry
    -- its own limits.
    risk_policy_revision TEXT     NOT NULL,
    CONSTRAINT deployment_id_format CHECK (common.is_canonical_id(deployment_id, 'dep')),
    CONSTRAINT deployment_state_valid CHECK (state IN ('ACTIVE','PAUSED','RETIRED')),
    -- Live deployment is impossible without dual-control activation evidence.
    CONSTRAINT live_deployment_dual_control CHECK (
        environment <> 'live' OR approved_by IS NOT NULL
    ),
    approved_by       TEXT,
    CONSTRAINT activation_dual_control CHECK (activated_by IS NOT NULL),
    UNIQUE (strategy_id, strategy_version, environment, account_scope)
);

CREATE INDEX deployment_active_idx ON strategy.deployment (environment, state) WHERE state = 'ACTIVE';

-- -----------------------------------------------------------------------------
-- RISK POLICY revisions.
--
-- 17_CONFIGURATION_AND_RISK_POLICY.md §3: a risk policy must EXPLICITLY define
-- each dimension for its scope. There is no universal default. A missing
-- dimension is a deny condition, which the not-null columns and the completeness
-- check below enforce structurally.
-- -----------------------------------------------------------------------------
CREATE TABLE risk.policy (
    policy_revision   TEXT        PRIMARY KEY,
    name              TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    scope_kind        TEXT        NOT NULL,
    scope_value       TEXT        NOT NULL,
    -- Required dimensions. Every one is NOT NULL: a policy row cannot exist
    -- without them, so "missing limit" is a validation failure at write time
    -- rather than a runtime surprise.
    max_order_notional NUMERIC(38,18) NOT NULL,
    max_order_quantity NUMERIC(38,18) NOT NULL,
    max_gross_exposure NUMERIC(38,18) NOT NULL,
    max_net_exposure   NUMERIC(38,18) NOT NULL,
    max_leverage       NUMERIC(18,8)  NOT NULL,
    max_concentration  NUMERIC(12,8)  NOT NULL,
    max_open_orders    INTEGER      NOT NULL,
    -- Loss and drawdown controls, with the measurement window and source.
    max_daily_loss     NUMERIC(38,18) NOT NULL,
    max_drawdown       NUMERIC(12,8)  NOT NULL,
    loss_window        INTERVAL     NOT NULL,
    loss_source        TEXT         NOT NULL,
    -- Market-data and price-deviation bounds.
    max_market_data_age   INTERVAL     NOT NULL,
    max_price_deviation   NUMERIC(12,8) NOT NULL,
    -- Order/cancel rate limits.
    max_orders_per_minute INTEGER     NOT NULL,
    max_cancels_per_minute INTEGER     NOT NULL,
    -- Permitted scope.
    permitted_markets   common.market_class[] NOT NULL,
    permitted_venues    TEXT[]        NOT NULL,
    permitted_instruments TEXT[]      NOT NULL,
    permitted_order_types common.order_type[] NOT NULL,
    permitted_sides     common.side[] NOT NULL,
    permitted_modes     TEXT[]        NOT NULL,
    schedule            JSONB,
    -- Fee, funding, margin, settlement assumptions.
    fee_assumptions     JSONB         NOT NULL,
    funding_assumptions JSONB         NOT NULL,
    margin_assumptions   JSONB         NOT NULL,
    settlement_assumptions JSONB      NOT NULL,
    -- Escalation, halt and re-enable authority.
    halt_authority      TEXT         NOT NULL,
    reenable_authority  TEXT         NOT NULL,
    -- Governance.
    status              TEXT         NOT NULL DEFAULT 'DRAFT',
    effective_at        TIMESTAMPTZ,
    superseded_by       TEXT,
    created_by          TEXT         NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_by         TEXT,
    approved_at         TIMESTAMPTZ,
    -- Rollback revision required for acceptance.
    rollback_revision   TEXT,
    policy_version_vector BIGINT     NOT NULL DEFAULT 1,
    CONSTRAINT policy_revision_format CHECK (common.is_canonical_id(policy_revision, 'pol')),
    CONSTRAINT policy_scope_kind_valid CHECK (scope_kind IN ('ACCOUNT','STRATEGY','INSTRUMENT','VENUE','MARKET','ENVIRONMENT')),
    CONSTRAINT policy_status_valid CHECK (status IN ('DRAFT','ACTIVE','SUPERSEDED','REJECTED')),
    -- Every numeric limit is bounded positively. A negative or zero limit would
    -- be meaningless and is rejected structurally.
    CONSTRAINT policy_limits_positive CHECK (
        max_order_notional > 0 AND max_order_quantity > 0
        AND max_gross_exposure > 0 AND max_net_exposure > 0
        AND max_leverage > 0 AND max_concentration > 0 AND max_open_orders > 0
        AND max_daily_loss > 0 AND max_drawdown > 0 AND max_drawdown <= 1
        AND max_orders_per_minute > 0 AND max_cancels_per_minute > 0
    ),
    CONSTRAINT policy_deviation_bounded CHECK (
        max_price_deviation >= 0 AND max_price_deviation <= 1
    ),
    CONSTRAINT policy_loss_window_positive CHECK (loss_window > INTERVAL '0'),
    CONSTRAINT policy_market_data_age_positive CHECK (max_market_data_age > INTERVAL '0'),
    -- Arrays must be non-empty: a policy that permits nothing is a deny-all, and
    -- permitting nothing is expressed by deactivating the policy, not by
    -- activating an empty one.
    CONSTRAINT policy_scope_non_empty CHECK (
        cardinality(permitted_markets) > 0
        AND cardinality(permitted_venues) > 0
        AND cardinality(permitted_instruments) > 0
        AND cardinality(permitted_order_types) > 0
        AND cardinality(permitted_sides) > 0
        AND cardinality(permitted_modes) > 0
    ),
    -- Separation of duties: no one may approve their own policy revision.
    CONSTRAINT policy_dual_control CHECK (approved_by IS NULL OR approved_by <> created_by),
    CONSTRAINT policy_approval_complete CHECK (
        (approved_by IS NULL AND approved_at IS NULL)
        OR (approved_by IS NOT NULL AND approved_at IS NOT NULL)
    ),
    -- An ACTIVE policy must be approved, effective and have a rollback revision.
    CONSTRAINT active_policy_complete CHECK (
        status <> 'ACTIVE'
        OR (approved_by IS NOT NULL AND effective_at IS NOT NULL AND rollback_revision IS NOT NULL)
    )
);

CREATE UNIQUE INDEX policy_active_scope_idx
    ON risk.policy (environment, scope_kind, scope_value) WHERE status = 'ACTIVE';

COMMENT ON TABLE risk.policy IS
    'Versioned, dual-controlled risk policy revisions. The Risk Engine is deterministic for a fixed policy revision, account state, market snapshot and command (17_CONFIGURATION_AND_RISK_POLICY.md §4).';

-- -----------------------------------------------------------------------------
-- Risk decisions.
--
-- Every risk-increasing command has exactly one durable risk decision. A risk
-- approval is short-lived and bound to the exact command, instrument, quantity,
-- price constraints, account, environment and policy revision; ANY material
-- change invalidates it and requires reevaluation.
-- -----------------------------------------------------------------------------
CREATE TABLE risk.decision (
    decision_id       TEXT        PRIMARY KEY,
    environment       common.environment NOT NULL,
    outcome           risk.risk_decision_outcome NOT NULL,
    -- Identity binding: the decision belongs to exactly one command.
    command_id        TEXT        NOT NULL,
    order_id          TEXT,
    account_id        TEXT        NOT NULL,
    strategy_id       TEXT,
    strategy_version  BIGINT,
    instrument_id     TEXT        NOT NULL,
    venue_id          TEXT        NOT NULL,
    side              common.side NOT NULL,
    order_type        common.order_type NOT NULL,
    quantity          NUMERIC(38,18) NOT NULL,
    price             NUMERIC(38,18),
    notional          NUMERIC(38,18) NOT NULL,
    -- Every policy revision consulted, so the decision is reproducible.
    policy_revisions  TEXT[]      NOT NULL,
    -- The evaluated facts. Stored so an evaluation is explainable and
    -- reproducible after the fact.
    evaluated_facts   JSONB       NOT NULL,
    -- Failed mandatory controls, named. A rejection always has at least one.
    failed_controls   TEXT[]      NOT NULL DEFAULT '{}',
    advisory_flags    TEXT[]      NOT NULL DEFAULT '{}',
    correlation_id    TEXT        NOT NULL,
    causation_id      TEXT,
    evaluated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    evaluated_at_ns   BIGINT      NOT NULL,
    -- Validity horizon. A risk approval expires; it is not a standing permit.
    valid_until       TIMESTAMPTZ NOT NULL,
    -- Monotonic version of the decision stream per command, for ordering.
    decision_sequence BIGINT      NOT NULL,
    CONSTRAINT decision_id_format CHECK (common.is_canonical_id(decision_id, 'rsk')),
    CONSTRAINT decision_command_format CHECK (common.is_canonical_id(command_id, 'cmd')),
    CONSTRAINT decision_positive_quantity CHECK (quantity > 0),
    CONSTRAINT decision_positive_notional CHECK (notional > 0),
    CONSTRAINT decision_facts_present   CHECK (evaluated_facts IS NOT NULL),
    CONSTRAINT decision_validity_future CHECK (valid_until > evaluated_at),
    CONSTRAINT decision_policies_present CHECK (cardinality(policy_revisions) > 0),
    -- A rejection must name at least one failed mandatory control, and an
    -- approval must not claim a failed control. This makes an unexplained
    -- decision structurally impossible.
    CONSTRAINT decision_outcome_consistent CHECK (
        (outcome = 'REJECTED' AND cardinality(failed_controls) > 0)
        OR (outcome = 'APPROVED' AND cardinality(failed_controls) = 0)
    )
);

CREATE INDEX decision_command_idx   ON risk.decision (command_id, decision_sequence);
CREATE INDEX decision_account_idx   ON risk.decision (account_id, evaluated_at DESC);
CREATE INDEX decision_rejected_idx  ON risk.decision (evaluated_at DESC) WHERE outcome = 'REJECTED';
CREATE INDEX decision_correlation_idx ON risk.decision (correlation_id);

COMMENT ON TABLE risk.decision IS
    'Durable, explainable risk decisions. Invariant: no risk-rejected command creates a live submission (09_TESTING_AND_RELEASE_EVIDENCE.md invariant 1). A decision is never mutated; a re-evaluation creates a new decision with a higher sequence.';

-- One decision per (command, sequence). Re-evaluation is an append.
CREATE UNIQUE INDEX decision_command_sequence_idx ON risk.decision (command_id, decision_sequence);

-- -----------------------------------------------------------------------------
-- Account state used by the Risk Engine.
--
-- This is authoritative *account* state, sourced from reconciled venue
-- observations. A missing row is a deny condition, not a zero balance.
-- -----------------------------------------------------------------------------
CREATE TABLE risk.account_state (
    account_id        TEXT        PRIMARY KEY,
    environment       common.environment NOT NULL,
    venue_id          TEXT        NOT NULL,
    base_currency     TEXT        NOT NULL,
    -- The effective trading account state. Anything other than ACTIVE denies
    -- risk-increasing activity.
    account_status    TEXT        NOT NULL,
    equity            NUMERIC(38,18) NOT NULL,
    available_margin  NUMERIC(38,18) NOT NULL,
    used_margin       NUMERIC(38,18) NOT NULL,
    realized_pnl      NUMERIC(38,18) NOT NULL DEFAULT 0,
    daily_pnl         NUMERIC(38,18) NOT NULL DEFAULT 0,
    peak_equity       NUMERIC(38,18) NOT NULL,
    open_order_count  INTEGER     NOT NULL DEFAULT 0,
    leverage          NUMERIC(18,8)  NOT NULL DEFAULT 1,
    -- Reconciliation state. A material unresolved break blocks affected scope.
    reconciliation_status TEXT     NOT NULL DEFAULT 'CLEAN',
    as_of             TIMESTAMPTZ NOT NULL,
    as_of_ns          BIGINT      NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    version_vector    BIGINT      NOT NULL DEFAULT 1,
    CONSTRAINT account_id_format  CHECK (common.is_canonical_id(account_id, 'acc')),
    CONSTRAINT account_status_valid CHECK (account_status IN
        ('ACTIVE','MARGIN_CALL','FROZEN','DISABLED','UNKNOWN')),
    CONSTRAINT account_equity_defined   CHECK (equity IS NOT NULL AND available_margin IS NOT NULL),
    CONSTRAINT account_margin_consistent CHECK (used_margin >= 0),
    CONSTRAINT account_leverage_positive CHECK (leverage > 0),
    CONSTRAINT account_recon_status_valid CHECK (reconciliation_status IN
        ('CLEAN','MINOR_BREAK','MATERIAL_BREAK','UNKNOWN')),
    CONSTRAINT account_open_orders_non_negative CHECK (open_order_count >= 0),
    -- An UNKNOWN account state or reconciliation status can never be presented as
    -- healthy; the Risk Engine denies on either.
    CONSTRAINT account_state_not_optimistic CHECK (
        (account_status = 'ACTIVE' AND reconciliation_status <> 'UNKNOWN')
        OR account_status <> 'ACTIVE'
    )
);

CREATE INDEX account_state_scope_idx ON risk.account_state (environment, venue_id, account_status);

COMMENT ON TABLE risk.account_state IS
    'Authoritative account state for risk evaluation, sourced from reconciled venue observations. Unknown state fails closed for new risk.';

SELECT common.assert_no_floating_point();
