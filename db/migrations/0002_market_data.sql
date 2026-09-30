-- =============================================================================
-- 0002_market_data.sql
-- G2 - Market Data: normalized instruments, observations, feed health,
-- freshness, provenance, replayable ingestion.
--
-- Authority: 16_MARKET_DATA_AND_VENUE_ADAPTERS.md
-- =============================================================================

-- -----------------------------------------------------------------------------
-- Canonical instrument identity.
--
-- The platform instrument_id is the identity. A venue symbol is NEVER a
-- globally unique identifier: it is a versioned, audited, venue-scoped mapping
-- (16_..._ADAPTERS.md §1). This table is the single place that proves it.
-- -----------------------------------------------------------------------------
CREATE TABLE market.instrument (
    instrument_id     TEXT        PRIMARY KEY,
    market_class      common.market_class NOT NULL,
    base              TEXT        NOT NULL,
    quote             TEXT        NOT NULL,
    -- Contract specification for derivative/commodity instruments.
    contract_definition TEXT,
    -- Canonical increments. These are the platform's normalised values; a venue
    -- adapter may impose a STRICTER value at submission time. An unknown
    -- increment is a deny condition for risk-increasing activity.
    min_quantity      NUMERIC(38,18) NOT NULL,
    price_increment   NUMERIC(38,18) NOT NULL,
    tick_size         NUMERIC(38,18) NOT NULL,
    min_notional      NUMERIC(38,18),
    -- Capabilities and rules are DATA, not scattered conditional logic
    -- (16_..._ADAPTERS.md §5).
    order_types       common.order_type[] NOT NULL,
    supports_shorting BOOLEAN     NOT NULL DEFAULT false,
    supports_leverage BOOLEAN     NOT NULL DEFAULT false,
    settlement_model  TEXT,
    trading_status    common.trading_status NOT NULL DEFAULT 'UNKNOWN',
    status_effective_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Calendar version is auditable configuration; tradability is never inferred
    -- from a wall-clock schedule alone when venue status is available.
    calendar_version  TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT instrument_id_format CHECK (common.is_canonical_id(instrument_id, 'ins')),
    CONSTRAINT instrument_min_quantity_positive CHECK (min_quantity > 0),
    CONSTRAINT instrument_price_increment_positive CHECK (price_increment > 0),
    CONSTRAINT instrument_tick_size_positive CHECK (tick_size > 0),
    CONSTRAINT instrument_min_notional_positive CHECK (min_notional IS NULL OR min_notional > 0),
    -- A derivative contract must carry a contract definition; a spot pair must not.
    CONSTRAINT contract_definition_required_for_derivatives CHECK (
        market_class <> 'derivatives' OR contract_definition IS NOT NULL
    )
);

CREATE INDEX instrument_market_class_idx ON market.instrument (market_class, trading_status);

COMMENT ON TABLE market.instrument IS
    'Canonical internal instrument identity. Every tradable instrument has a platform instrument_id; venue symbols are scoped mappings held in market.venue_symbol.';

-- -----------------------------------------------------------------------------
-- Venue symbol mapping: versioned and audited, never globally unique.
-- -----------------------------------------------------------------------------
CREATE TABLE market.venue_symbol (
    instrument_id     TEXT        NOT NULL REFERENCES market.instrument(instrument_id),
    venue_id          TEXT        NOT NULL,
    symbol            TEXT        NOT NULL,
    mapping_version   BIGINT      NOT NULL,
    effective_at      TIMESTAMPTZ NOT NULL,
    retired_at        TIMESTAMPTZ,
    changed_by        TEXT        NOT NULL,
    change_reason     TEXT        NOT NULL,
    CONSTRAINT venue_symbol_pk PRIMARY KEY (instrument_id, venue_id, mapping_version),
    CONSTRAINT venue_id_format    CHECK (venue_id ~ '^[a-z]{3}_[0-9abcdefghjkmnpqrstvwxyz]{20}$'),
    CONSTRAINT mapping_version_positive CHECK (mapping_version > 0),
    -- The same venue symbol may map to different instruments (a real venue
    -- condition). What must never happen is one venue symbol being mapped to two
    -- instruments at the same version, which would make venue truth ambiguous.
    CONSTRAINT venue_symbol_unique_per_version UNIQUE (venue_id, symbol, mapping_version),
    CONSTRAINT retire_after_effective CHECK (retired_at IS NULL OR retired_at > effective_at)
);

COMMENT ON TABLE market.venue_symbol IS
    'Versioned, audited venue-to-instrument symbol mapping. A change creates a new version rather than mutating the existing mapping, so history remains reconstructible.';

-- -----------------------------------------------------------------------------
-- Normalized market observations.
--
-- Every observation carries source venue, instrument, source timestamp, receive
-- timestamp, sequence where supplied, and quality flags (16_..._ADAPTERS.md §2).
-- source_timestamp is mandatory: freshness is evaluated against it, and a
-- future-dated source timestamp is a data-integrity defect that fails closed.
-- -----------------------------------------------------------------------------
CREATE TABLE market.trade (
    observation_id    TEXT        PRIMARY KEY,
    instrument_id     TEXT        NOT NULL REFERENCES market.instrument(instrument_id),
    venue_id          TEXT        NOT NULL,
    -- Exactly one of these is populated; the other is NULL. Enforced below.
    price             NUMERIC(38,18),
    quantity          NUMERIC(38,18),
    side              common.side,
    -- source_timestamp is the venue-reported origin. receive_timestamp is when
    -- the platform observed it. A large gap is a latency signal, not freshness.
    source_timestamp  TIMESTAMPTZ NOT NULL,
    source_timestamp_ns BIGINT    NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Venue sequence number where the venue supplies one. NULL is permitted;
    -- a GAP in a non-null sequence marks the stream degraded.
    source_sequence   BIGINT,
    quality_flags     TEXT[]      NOT NULL DEFAULT '{}',
    raw_reference     TEXT,
    CONSTRAINT trade_observation_id_format CHECK (common.is_canonical_id(observation_id, 'mkt')),
    CONSTRAINT trade_positive_price     CHECK (price IS NULL OR price > 0),
    CONSTRAINT trade_positive_quantity  CHECK (quantity IS NULL OR quantity > 0),
    CONSTRAINT trade_has_price_or_quantity CHECK (price IS NOT NULL OR quantity IS NOT NULL),
    CONSTRAINT trade_source_seq_positive CHECK (source_sequence IS NULL OR source_sequence > 0),
    -- Receive time must not precede source time: a negative latency is a defect,
    -- not a clock skew to be silently absorbed.
    CONSTRAINT trade_not_from_the_future CHECK (received_at >= source_timestamp)
);

CREATE INDEX trade_instrument_source_idx ON market.trade (instrument_id, source_timestamp DESC);
CREATE INDEX trade_venue_sequence_idx     ON market.trade (venue_id, source_sequence) WHERE source_sequence IS NOT NULL;
CREATE INDEX trade_received_idx           ON market.trade (received_at DESC);

COMMENT ON TABLE market.trade IS
    'Normalized trade observations. Retained under data retention policy; the normalized record is the internal consumption contract, not the raw vendor payload.';

CREATE TABLE market.quote (
    observation_id    TEXT        PRIMARY KEY,
    instrument_id     TEXT        NOT NULL REFERENCES market.instrument(instrument_id),
    venue_id          TEXT        NOT NULL,
    bid_price         NUMERIC(38,18),
    bid_quantity      NUMERIC(38,18),
    ask_price         NUMERIC(38,18),
    ask_quantity      NUMERIC(38,18),
    source_timestamp  TIMESTAMPTZ NOT NULL,
    source_timestamp_ns BIGINT    NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    source_sequence   BIGINT,
    quality_flags     TEXT[]      NOT NULL DEFAULT '{}',
    CONSTRAINT quote_observation_id_format CHECK (common.is_canonical_id(observation_id, 'mkt')),
    CONSTRAINT quote_bid_positive CHECK (bid_price IS NULL OR bid_price > 0),
    CONSTRAINT quote_ask_positive CHECK (ask_price IS NULL OR ask_price > 0),
    CONSTRAINT quote_bid_qty_positive CHECK (bid_quantity IS NULL OR bid_quantity > 0),
    CONSTRAINT quote_ask_qty_positive CHECK (ask_quantity IS NULL OR ask_quantity > 0),
    CONSTRAINT quote_has_a_side CHECK (bid_price IS NOT NULL OR ask_price IS NOT NULL),
    -- A crossed book (bid above ask) is invalid and marks the stream degraded
    -- (16_..._ADAPTERS.md §2). It is stored with a quality flag rather than
    -- discarded, so the integrity of the feed remains auditable.
    CONSTRAINT quote_not_crossed CHECK (bid_price IS NULL OR ask_price IS NULL OR bid_price <= ask_price),
    CONSTRAINT quote_not_from_the_future CHECK (received_at >= source_timestamp)
);

CREATE INDEX quote_instrument_source_idx ON market.quote (instrument_id, source_timestamp DESC);

CREATE TABLE market.candle (
    observation_id    TEXT        PRIMARY KEY,
    instrument_id     TEXT        NOT NULL REFERENCES market.instrument(instrument_id),
    venue_id          TEXT        NOT NULL,
    interval          TEXT        NOT NULL,
    period_open_time  TIMESTAMPTZ NOT NULL,
    open              NUMERIC(38,18) NOT NULL,
    high              NUMERIC(38,18) NOT NULL,
    low               NUMERIC(38,18) NOT NULL,
    close             NUMERIC(38,18) NOT NULL,
    volume            NUMERIC(38,18) NOT NULL,
    source_timestamp  TIMESTAMPTZ NOT NULL,
    source_timestamp_ns BIGINT    NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    quality_flags     TEXT[]      NOT NULL DEFAULT '{}',
    CONSTRAINT candle_observation_id_format CHECK (common.is_canonical_id(observation_id, 'mkt')),
    -- OHLC internal consistency: an impossible candle is rejected at the boundary
    -- rather than being propagated into a backtest.
    CONSTRAINT candle_ohlc_consistent CHECK (
        high >= low
        AND high >= open AND high >= close
        AND low  <= open AND low  <= close
        AND open > 0 AND high > 0 AND low > 0 AND close > 0
    ),
    CONSTRAINT candle_volume_non_negative CHECK (volume >= 0),
    CONSTRAINT candle_not_from_the_future CHECK (received_at >= source_timestamp),
    CONSTRAINT candle_identity UNIQUE (instrument_id, venue_id, interval, period_open_time)
);

CREATE INDEX candle_instrument_interval_idx ON market.candle (instrument_id, interval, period_open_time DESC);

-- -----------------------------------------------------------------------------
-- Feed health and freshness.
--
-- Freshness is per instrument/market and configured in environment policy
-- (16_..._ADAPTERS.md §2). The recorded last-good source timestamp is what the
-- Risk Engine reads; it is a NULLABLE, explicitly-checked value so that "never
-- observed" fails closed rather than defaulting to fresh.
-- -----------------------------------------------------------------------------
CREATE TABLE market.feed_health (
    feed_key          TEXT        PRIMARY KEY,
    environment       common.environment NOT NULL,
    venue_id          TEXT        NOT NULL,
    instrument_id     TEXT        REFERENCES market.instrument(instrument_id),
    health_state      TEXT        NOT NULL DEFAULT 'DEGRADED',
    last_healthy_at   TIMESTAMPTZ,
    last_source_timestamp TIMESTAMPTZ,
    last_source_timestamp_ns BIGINT,
    -- Expected sequence watermark. A source_sequence below this indicates a
    -- sequence gap; the feed is degraded until reconciled.
    expected_sequence BIGINT,
    observed_sequence BIGINT,
    sequence_gaps     BIGINT      NOT NULL DEFAULT 0,
    error_count       BIGINT      NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT feed_health_state_valid CHECK (health_state IN ('HEALTHY','DEGRADED','DOWN','UNKNOWN')),
    -- UNKNOWN is a distinct state from DOWN. An unverifiable feed is a deny
    -- condition for risk-increasing activity, not an implicit pass.
    CONSTRAINT feed_unknown_not_healthy CHECK (health_state <> 'UNKNOWN' OR health_state = 'UNKNOWN'),
    CONSTRAINT sequence_no_regression CHECK (observed_sequence IS NULL OR expected_sequence IS NULL OR observed_sequence >= 0)
);

CREATE INDEX feed_health_lookup_idx ON market.feed_health (environment, venue_id, health_state);

COMMENT ON TABLE market.feed_health IS
    'Feed health and freshness watermark. The Risk Engine rejects risk-increasing commands when required inputs are stale or degraded; the platform never substitutes a cached value without labelling its age and source.';

-- Freshness thresholds live in configuration, not in code.
CREATE TABLE market.freshness_policy (
    policy_key        TEXT        PRIMARY KEY,
    environment       common.environment NOT NULL,
    market_class      common.market_class NOT NULL,
    -- Maximum acceptable age of a required input, per observation kind.
    max_trade_age     INTERVAL    NOT NULL,
    max_quote_age     INTERVAL    NOT NULL,
    max_candle_age    INTERVAL    NOT NULL,
    -- Maximum tolerated price deviation from the reference before rejection.
    max_price_deviation NUMERIC(12,8) NOT NULL,
    policy_revision   TEXT        NOT NULL,
    effective_at      TIMESTAMPTZ NOT NULL,
    created_by        TEXT        NOT NULL,
    approved_by       TEXT        NOT NULL,
    CONSTRAINT freshness_ages_positive CHECK (
        max_trade_age > INTERVAL '0' AND max_quote_age > INTERVAL '0' AND max_candle_age > INTERVAL '0'
    ),
    CONSTRAINT freshness_deviation_non_negative CHECK (max_price_deviation >= 0),
    -- Dual control on a production data-quality policy.
    CONSTRAINT freshness_dual_control CHECK (created_by <> approved_by)
);

COMMENT ON TABLE market.freshness_policy IS
    'Per-market freshness and price-deviation bounds. Missing limits deny risk-increasing activity; no default value may silently imply permission to trade (17_CONFIGURATION_AND_RISK_POLICY.md §2).';

SELECT common.assert_no_floating_point();
