-- =============================================================================
-- 0004_oms_and_execution.sql
-- G3 - OMS canonical order state machine, venue submission records, unknown
-- outcome protocol.
--
-- Authority: 04_TRADING_DOMAIN_AND_RISK.md, 16_MARKET_DATA_AND_VENUE_ADAPTERS.md
-- =============================================================================

CREATE TYPE oms.order_state AS ENUM
    ('CREATED','RISK_PENDING','RISK_APPROVED','SUBMITTING','ACKNOWLEDGED',
     'PARTIALLY_FILLED','FILLED','CANCELLED','REJECTED','EXPIRED','UNKNOWN');

CREATE TYPE oms.order_event_kind AS ENUM
    ('CREATED','RISK_APPROVED','RISK_REJECTED','SUBMITTING','SUBMIT_FAILED',
     'ACKNOWLEDGED','PARTIALLY_FILLED','FILLED','CANCEL_REQUESTED','CANCELLED',
     'REJECTED_BY_VENUE','EXPIRED','UNKNOWN_OUTCOME','OUTCOME_RESOLVED');

-- -----------------------------------------------------------------------------
-- Canonical order.
--
-- Invariant: every accepted order has exactly one canonical OMS identity
-- (09_TESTING_AND_RELEASE_EVIDENCE.md invariant 2). The command that created it
-- is UNIQUE, so a duplicate command cannot create a second order identity.
-- -----------------------------------------------------------------------------
CREATE TABLE oms.order (
    order_id          TEXT        PRIMARY KEY,
    -- Durable command identity. UNIQUE is the structural guarantee that a
    -- duplicate command does not create a second exposure
    -- (09_..._EVIDENCE.md invariant 3).
    command_id        TEXT        NOT NULL UNIQUE,
    idempotency_scope TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    account_id        TEXT        NOT NULL,
    strategy_id       TEXT,
    strategy_version  BIGINT,
    instrument_id     TEXT        NOT NULL REFERENCES market.instrument(instrument_id),
    venue_id          TEXT        NOT NULL,
    side              common.side NOT NULL,
    order_type        common.order_type NOT NULL,
    time_in_force     common.time_in_force NOT NULL,
    quantity          NUMERIC(38,18) NOT NULL,
    -- Cumulative execution state. These are maintained by validated fills only.
    filled_quantity   NUMERIC(38,18) NOT NULL DEFAULT 0,
    average_fill_price NUMERIC(38,18),
    limit_price       NUMERIC(38,18),
    stop_price        NUMERIC(38,18),
    -- Canonical state. A venue adapter NEVER writes this column; it reports
    -- observations, and the OMS state machine decides
    -- (25_..._PROFILE.md §3.2).
    state             oms.order_state NOT NULL DEFAULT 'CREATED',
    -- Optimistic concurrency. Last-write-wins is prohibited for authoritative
    -- financial state (01_SYSTEM_ARCHITECTURE.md §8).
    version_vector    BIGINT      NOT NULL DEFAULT 1,
    -- The risk decision that authorised this order, if risk-increasing.
    risk_decision_id  TEXT,
    risk_policy_revisions TEXT[]  NOT NULL DEFAULT '{}',
    -- Eligibility snapshot at submission time.
    eligibility_record_id TEXT,
    correlation_id    TEXT        NOT NULL,
    causation_id      TEXT,
    -- Client order id, used for venue-side idempotency where supported.
    client_order_id   TEXT,
    -- Timestamp semantics.
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ,
    CONSTRAINT order_id_format     CHECK (common.is_canonical_id(order_id, 'ord')),
    CONSTRAINT order_command_format CHECK (common.is_canonical_id(command_id, 'cmd')),
    CONSTRAINT order_positive_quantity CHECK (quantity > 0),
    CONSTRAINT order_filled_within_quantity CHECK (
        filled_quantity >= 0 AND filled_quantity <= quantity
    ),
    CONSTRAINT order_limit_has_price  CHECK (order_type <> 'LIMIT' OR limit_price IS NOT NULL),
    CONSTRAINT order_limit_price_positive CHECK (limit_price IS NULL OR limit_price > 0),
    CONSTRAINT order_stop_price_positive  CHECK (stop_price IS NULL OR stop_price > 0),
    -- A filled order must have a fill price; a zero-filled order must not.
    CONSTRAINT order_filled_has_price CHECK (
        (state = 'FILLED' AND filled_quantity = quantity AND average_fill_price IS NOT NULL)
        OR state <> 'FILLED'
    ),
    -- A PARTIALLY_FILLED order must have a positive partial fill.
    CONSTRAINT order_partial_has_fill CHECK (
        state <> 'PARTIALLY_FILLED'
        OR (filled_quantity > 0 AND filled_quantity < quantity AND average_fill_price IS NOT NULL)
    ),
    -- A risk-increasing order REQUIRES a durable risk decision. This is
    -- invariant 1 enforced structurally: no risk decision, no risk-increasing
    -- order can reach a submission state.
    CONSTRAINT order_risk_decision_present CHECK (
        order_type NOT IN ('REDUCE_ONLY','CLOSE_POSITION')
        OR state IN ('CREATED','RISK_PENDING')
        OR risk_decision_id IS NOT NULL
    ),
    -- Live orders require recorded eligibility evidence.
    CONSTRAINT order_live_requires_eligibility CHECK (
        environment <> 'live' OR eligibility_record_id IS NOT NULL
    )
);

CREATE INDEX order_state_idx        ON oms.order (environment, state, updated_at DESC);
CREATE INDEX order_account_idx      ON oms.order (account_id, created_at DESC);
CREATE INDEX order_strategy_idx     ON oms.order (strategy_id, strategy_version) WHERE strategy_id IS NOT NULL;
CREATE INDEX order_instrument_idx   ON oms.order (instrument_id, created_at DESC);
CREATE INDEX order_correlation_idx  ON oms.order (correlation_id);
-- Supports the reconciliation sweep for non-terminal orders.
CREATE INDEX order_open_idx         ON oms.order (venue_id, updated_at) WHERE state NOT IN ('FILLED','CANCELLED','REJECTED','EXPIRED');
-- Supports the UNKNOWN-order blocker required by G11.
CREATE INDEX order_unknown_idx      ON oms.order (venue_id, updated_at) WHERE state = 'UNKNOWN';

COMMENT ON TABLE oms.order IS
    'The canonical OMS order. One identity per accepted order, one creation per command. UNKNOWN is a mandatory state when a submission timeout prevents determination of the venue outcome (04_TRADING_DOMAIN_AND_RISK.md).';

-- -----------------------------------------------------------------------------
-- The closed OMS transition set, enforced by the database.
-- -----------------------------------------------------------------------------
CREATE TABLE oms.state_transition_rule (
    from_state        oms.order_state NOT NULL,
    to_state          oms.order_state NOT NULL,
    event_kind        oms.order_event_kind NOT NULL,
    -- A transition that increases exposure at the venue requires a prior risk
    -- decision. Enforced structurally below.
    requires_risk_decision BOOLEAN NOT NULL DEFAULT false,
    -- An UNKNOWN order may only leave UNKNOWN through an explicit resolution.
    requires_outcome_resolution BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (from_state, to_state, event_kind)
);

INSERT INTO oms.state_transition_rule (from_state, to_state, event_kind, requires_risk_decision, requires_outcome_resolution) VALUES
    ('CREATED','RISK_PENDING','CREATED',              false, false),
    ('RISK_PENDING','RISK_APPROVED','RISK_APPROVED',  true,  false),
    ('RISK_PENDING','REJECTED','RISK_REJECTED',        false, false),
    ('RISK_PENDING','CANCELLED','CANCELLED',          false, false),
    ('RISK_APPROVED','SUBMITTING','SUBMITTING',        true,  false),
    ('RISK_APPROVED','CANCELLED','CANCELLED',          false, false),
    -- A submit timeout that may have reached the venue becomes UNKNOWN. From
    -- UNKNOWN the ONLY forward transitions are through an explicit resolution.
    ('SUBMITTING','ACKNOWLEDGED','ACKNOWLEDGED',      true,  false),
    ('SUBMITTING','REJECTED','REJECTED_BY_VENUE',      false, false),
    ('SUBMITTING','EXPIRED','EXPIRED',                false, false),
    ('SUBMITTING','UNKNOWN','UNKNOWN_OUTCOME',        false, true),
    ('SUBMITTING','CANCELLED','CANCELLED',            false, false),
    ('ACKNOWLEDGED','PARTIALLY_FILLED','PARTIALLY_FILLED', false, false),
    ('ACKNOWLEDGED','FILLED','FILLED',                false, false),
    ('ACKNOWLEDGED','CANCELLED','CANCELLED',          false, false),
    ('ACKNOWLEDGED','EXPIRED','EXPIRED',              false, false),
    ('PARTIALLY_FILLED','PARTIALLY_FILLED','PARTIALLY_FILLED', false, false),
    ('PARTIALLY_FILLED','FILLED','FILLED',            false, false),
    ('PARTIALLY_FILLED','CANCELLED','CANCELLED',      false, false),
    ('PARTIALLY_FILLED','EXPIRED','EXPIRED',          false, false),
    -- UNKNOWN resolves only to a determinate state via an explicit resolution
    -- event. It NEVER transitions directly to REJECTED or FILLED by assumption.
    ('UNKNOWN','FILLED','OUTCOME_RESOLVED',           false, true),
    ('UNKNOWN','REJECTED','OUTCOME_RESOLVED',         false, true),
    ('UNKNOWN','CANCELLED','OUTCOME_RESOLVED',        false, true),
    ('UNKNOWN','EXPIRED','OUTCOME_RESOLVED',          false, true),
    ('UNKNOWN','ACKNOWLEDGED','OUTCOME_RESOLVED',     false, true),
    ('UNKNOWN','PARTIALLY_FILLED','OUTCOME_RESOLVED', false, true);

COMMENT ON TABLE oms.state_transition_rule IS
    'The closed set of permitted OMS transitions. There is deliberately NO path from UNKNOWN that skips OUTCOME_RESOLVED: an ambiguous submission is never converted into a determinate state by assumption (01_SYSTEM_ARCHITECTURE.md §6).';

-- -----------------------------------------------------------------------------
-- Order state transition journal (immutable).
-- -----------------------------------------------------------------------------
CREATE TABLE oms.order_event (
    order_event_id    TEXT        PRIMARY KEY,
    order_id          TEXT        NOT NULL REFERENCES oms.order(order_id),
    -- Monotonic per order. Missing or out-of-order sequence numbers are rejected
    -- and routed to controlled reconciliation (01_SYSTEM_ARCHITECTURE.md §8).
    sequence          BIGINT      NOT NULL,
    from_state        oms.order_state NOT NULL,
    to_state          oms.order_state NOT NULL,
    event_kind        oms.order_event_kind NOT NULL,
    filled_quantity_delta NUMERIC(38,18) NOT NULL DEFAULT 0,
    price             NUMERIC(38,18),
    -- Venue observation that drove the transition, when applicable.
    venue_observation_ref TEXT,
    actor_id          TEXT        NOT NULL,
    reason            TEXT,
    correlation_id    TEXT        NOT NULL,
    causation_id      TEXT,
    risk_decision_id  TEXT,
    occurred_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    occurred_at_ns    BIGINT      NOT NULL,
    audit_id          TEXT,
    CONSTRAINT order_event_id_format CHECK (common.is_canonical_id(order_event_id, 'oe1')),
    CONSTRAINT order_event_sequence_positive CHECK (sequence > 0),
    CONSTRAINT order_event_changes_state CHECK (from_state <> to_state),
    CONSTRAINT order_event_fill_price CHECK (
        (event_kind IN ('FILLED','PARTIALLY_FILLED') AND price IS NOT NULL AND price > 0 AND filled_quantity_delta > 0)
        OR event_kind NOT IN ('FILLED','PARTIALLY_FILLED')
    ),
    CONSTRAINT order_event_unique_sequence UNIQUE (order_id, sequence)
);

CREATE INDEX order_event_order_idx ON oms.order_event (order_id, sequence DESC);
CREATE INDEX order_event_correlation_idx ON oms.order_event (correlation_id);

COMMENT ON TABLE oms.order_event IS
    'Immutable OMS transition journal. Reconstructing the order state from events must yield exactly the current oms.order.state; this is verified by an invariant test.';

-- -----------------------------------------------------------------------------
-- Fills. Positions are derived from validated fills only
-- (04_TRADING_DOMAIN_AND_RISK.md, "Portfolio integrity").
-- -----------------------------------------------------------------------------
CREATE TABLE oms.fill (
    fill_id           TEXT        PRIMARY KEY,
    order_id          TEXT        NOT NULL REFERENCES oms.order(order_id),
    account_id        TEXT        NOT NULL,
    instrument_id     TEXT        NOT NULL,
    venue_id          TEXT        NOT NULL,
    side              common.side NOT NULL,
    quantity          NUMERIC(38,18) NOT NULL,
    price             NUMERIC(38,18) NOT NULL,
    -- Explicit fees. A fill with no fee record is not a validated fill.
    fee_amount        NUMERIC(38,18) NOT NULL DEFAULT 0,
    fee_currency      TEXT        NOT NULL,
    funding_amount    NUMERIC(38,18) NOT NULL DEFAULT 0,
    -- Venue trade id, unique per venue. This is what makes a duplicate fill
    -- delivery idempotent.
    venue_trade_id    TEXT        NOT NULL,
    source_timestamp  TIMESTAMPTZ NOT NULL,
    source_timestamp_ns BIGINT    NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Validation: a fill enters the authoritative record only once validated.
    validated         BOOLEAN     NOT NULL DEFAULT false,
    validated_at      TIMESTAMPTZ,
    reconciliation_status TEXT    NOT NULL DEFAULT 'PENDING',
    correlation_id    TEXT        NOT NULL,
    ledger_entry_id   TEXT,
    CONSTRAINT fill_id_format      CHECK (common.is_canonical_id(fill_id, 'fil')),
    CONSTRAINT fill_positive_quantity CHECK (quantity > 0),
    CONSTRAINT fill_positive_price    CHECK (price > 0),
    CONSTRAINT fill_fee_non_negative  CHECK (fee_amount >= 0 AND funding_amount >= 0),
    -- A validated fill must carry its validation timestamp and a non-pending
    -- reconciliation status: positions derive from VALIDATED fills only.
    CONSTRAINT fill_validation_complete CHECK (
        (validated = false AND validated_at IS NULL)
        OR (validated = true AND validated_at IS NOT NULL
            AND reconciliation_status IN ('MATCHED','CORRECTED'))
    ),
    -- Duplicate delivery of the same venue trade is idempotent.
    CONSTRAINT fill_venue_trade_unique UNIQUE (venue_id, venue_trade_id)
);

CREATE INDEX fill_order_idx   ON oms.fill (order_id);
CREATE INDEX fill_account_idx ON oms.fill (account_id, source_timestamp DESC);
CREATE INDEX fill_pending_recon_idx ON oms.fill (reconciliation_status) WHERE reconciliation_status = 'PENDING';

COMMENT ON TABLE oms.fill IS
    'Validated fills. A venue trade delivered twice is stored once (unique on venue + venue_trade_id). Portfolio positions are derived from validated fills and must reconcile to them.';

-- -----------------------------------------------------------------------------
-- Execution: venue submission and the unknown-outcome protocol.
--
-- The submission record is durable BEFORE the request leaves the process. That
-- ordering is what makes an unknown outcome resolvable rather than lost
-- (16_..._ADAPTERS.md §4).
-- -----------------------------------------------------------------------------
CREATE TABLE execution.submission (
    submission_id     TEXT        PRIMARY KEY,
    order_id          TEXT        NOT NULL REFERENCES oms.order(order_id),
    environment       common.environment NOT NULL,
    venue_id          TEXT        NOT NULL,
    adapter_id        TEXT        NOT NULL,
    client_order_id   TEXT        NOT NULL,
    -- True when the venue guarantees client-order-id idempotency. When false the
    -- adapter MUST use a durable submission record and reconciliation-based
    -- uncertainty handling.
    venue_idempotent  BOOLEAN     NOT NULL DEFAULT false,
    request_sent_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    request_sent_at_ns BIGINT    NOT NULL,
    -- Terminal outcome of the venue interaction.
    outcome           TEXT        NOT NULL DEFAULT 'PENDING',
    response_received_at TIMESTAMPTZ,
    venue_order_ref   TEXT,
    -- Redacted reference to the raw request/response. Raw payloads may contain
    -- sensitive data and are retained under retention controls, never inline.
    request_digest    TEXT        NOT NULL,
    response_digest   TEXT,
    error_code        TEXT,
    error_detail      TEXT,
    attempts          INTEGER     NOT NULL DEFAULT 1,
    -- Set when a retry was performed. A retry of an exposure-increasing command
    -- is only permitted after reconciliation resolved the prior outcome.
    retry_authorized_by TEXT,
    reconciliation_case_id TEXT,
    CONSTRAINT submission_id_format CHECK (common.is_canonical_id(submission_id, 'sbm')),
    CONSTRAINT submission_outcome_valid CHECK (outcome IN
        ('PENDING','ACKNOWLEDGED','REJECTED','TIMED_OUT_UNKNOWN','CANCELLED','FILLED')),
    CONSTRAINT submission_digest_format CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    -- The durable record must exist before the request is sent. A submission
    -- that timed out is never recorded as REJECTED by assumption.
    CONSTRAINT submission_terminal_has_time CHECK (
        outcome = 'PENDING' OR response_received_at IS NOT NULL
    ),
    -- More than one attempt REQUIRES recorded reconciliation and authorisation:
    -- an exposure-increasing retry is never blind
    -- (01_SYSTEM_ARCHITECTURE.md invariant 3).
    CONSTRAINT submission_retry_requires_reconciliation CHECK (
        attempts <= 1
        OR (reconciliation_case_id IS NOT NULL AND retry_authorized_by IS NOT NULL)
    ),
    -- Client order id is unique per (venue, environment) so a duplicate submit
    -- cannot create a second venue order where the venue is idempotent.
    CONSTRAINT submission_client_order_unique UNIQUE (venue_id, environment, client_order_id)
);

CREATE INDEX submission_order_idx   ON execution.submission (order_id);
CREATE INDEX submission_pending_idx ON execution.submission (venue_id, request_sent_at)
    WHERE outcome IN ('PENDING','TIMED_OUT_UNKNOWN');
CREATE INDEX submission_cid_idx     ON execution.submission (client_order_id);

COMMENT ON TABLE execution.submission IS
    'Durable venue submission record. Written before the request leaves the process. A timeout yields TIMED_OUT_UNKNOWN, which the OMS maps to UNKNOWN; reconciliation resolves it before any exposure-increasing retry.';

-- Venue adapter capability declaration. Unsupported capabilities are DECLARED,
-- not simulated (16_..._ADAPTERS.md §4).
CREATE TABLE execution.adapter_capability (
    adapter_id        TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    venue_id          TEXT        NOT NULL,
    capability        TEXT        NOT NULL,
    supported         BOOLEAN     NOT NULL,
    -- Rate/concurrency limits are venue-specific data, not generic defaults.
    max_requests_per_second NUMERIC(12,4),
    max_concurrency   INTEGER,
    precision_scale   INTEGER,
    -- Certification. Live eligibility additionally requires isolation, runbooks,
    -- venue-specific risk policy, owner approval and paper/shadow evidence.
    certification_state TEXT      NOT NULL DEFAULT 'UNCERTIFIED',
    certified_at      TIMESTAMPTZ,
    certification_expires_at TIMESTAMPTZ,
    certified_by      TEXT,
    -- Adapter certification expires after material adapter/venue API/permission/
    -- contract changes (16_..._ADAPTERS.md §6).
    content_digest    TEXT        NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (adapter_id, environment, capability),
    CONSTRAINT adapter_capability_valid CHECK (capability IN
        ('CAPABILITY_DISCOVERY','AUTHENTICATE','INSTRUMENT_MAPPING','SUBMIT','CANCEL',
         'ORDER_LOOKUP','FILL_RETRIEVAL','BALANCE_RETRIEVAL','POSITION_RETRIEVAL',
         'RATE_LIMIT','HEALTH','RECONCILIATION')),
    CONSTRAINT adapter_certification_valid CHECK (certification_state IN
        ('UNCERTIFIED','PAPER_CERTIFIED','SHADOW_CERTIFIED','LIVE_ELIGIBLE','SUSPENDED','REVOKED')),
    -- An unsupported capability may not declare rate/precision limits, because
    -- those describe behaviour that does not exist.
    CONSTRAINT unsupported_has_no_limits CHECK (
        supported OR (max_requests_per_second IS NULL AND max_concurrency IS NULL AND precision_scale IS NULL)
    ),
    CONSTRAINT adapter_digest_format CHECK (content_digest ~ '^[0-9a-f]{64}$')
);

COMMENT ON TABLE execution.adapter_capability IS
    'Declared venue adapter capabilities, rate limits and certification state. An adapter is not operational merely because its interface or configuration exists.';

SELECT common.assert_no_floating_point();
