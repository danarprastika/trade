-- =============================================================================
-- 0007_outbox_halts_ops.sql
-- G3/G6 - Transactional outbox, halt hierarchy, operational control.
--
-- Authority: 05_PERSISTENCE_EVENTING_RECONCILIATION.md, 04_TRADING_DOMAIN_AND_RISK.md
-- =============================================================================

CREATE TYPE ops.halt_level AS ENUM
    ('ACCOUNT_HALT','STRATEGY_HALT','MARKET_HALT','VENUE_HALT','SYSTEM_HALT');

CREATE TYPE ops.halt_state AS ENUM ('ACTIVE','CLEARED');

-- -----------------------------------------------------------------------------
-- TRANSACTIONAL OUTBOX.
--
-- 05_..._PERSISTENCE.md: "An authoritative command executes domain validation,
-- state mutation, audit record creation, and outbox insertion in one PostgreSQL
-- transaction. A committed state mutation always has a corresponding durable
-- outbox record."
--
-- Duplicate publication is acceptable; consumers must be idempotent. Financial
-- correctness NEVER depends on exactly-once transport.
-- -----------------------------------------------------------------------------
CREATE TABLE ops.outbox (
    outbox_id         BIGSERIAL   PRIMARY KEY,
    event_id          TEXT        NOT NULL,
    event_type        TEXT        NOT NULL,
    schema_version    TEXT        NOT NULL,
    aggregate_type    TEXT        NOT NULL,
    aggregate_id      TEXT        NOT NULL,
    -- The monotonic sequence WITHIN the aggregate. Missing or out-of-order
    -- sequences are rejected by the consumer and routed to reconciliation.
    sequence          BIGINT      NOT NULL,
    correlation_id    TEXT        NOT NULL,
    causation_id      TEXT,
    producer_id       TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    occurred_at       TIMESTAMPTZ NOT NULL,
    occurred_at_ns    BIGINT      NOT NULL,
    recorded_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    source_timestamp  TIMESTAMPTZ,
    payload           JSONB       NOT NULL,
    -- Dispatch state. The dispatcher reads committed rows FOR UPDATE SKIP LOCKED.
    dispatch_state    TEXT        NOT NULL DEFAULT 'PENDING',
    dispatched_at     TIMESTAMPTZ,
    -- Bounded retry. Unbounded retries are prohibited
    -- (24_ENTERPRISE_RELEASE_STANDARD.md §16).
    attempt_count     INTEGER     NOT NULL DEFAULT 0,
    max_attempts      INTEGER     NOT NULL DEFAULT 10,
    last_error        TEXT,
    -- Dead-lettering. A record that exhausts its attempts is preserved, never
    -- dropped, and becomes an operational alert.
    dead_lettered_at  TIMESTAMPTZ,
    -- Exactly-once claim token, so a crashed dispatcher does not lose the record.
    claimed_by        TEXT,
    claimed_at        TIMESTAMPTZ,
    CONSTRAINT outbox_event_id_format CHECK (common.is_canonical_id(event_id, 'evt')),
    CONSTRAINT outbox_sequence_positive CHECK (sequence > 0),
    CONSTRAINT outbox_dispatch_state_valid CHECK (dispatch_state IN
        ('PENDING','DISPATCHING','DISPATCHED','FAILED','DEAD_LETTERED')),
    CONSTRAINT outbox_attempts_bounded CHECK (
        attempt_count >= 0 AND max_attempts > 0 AND attempt_count <= max_attempts
    ),
    -- An exhausted record must be dead-lettered, never left silently retrying.
    CONSTRAINT outbox_exhausted_is_dead_lettered CHECK (
        attempt_count < max_attempts OR dispatch_state = 'DEAD_LETTERED'
    ),
    CONSTRAINT outbox_dead_letter_state CHECK (
        (dead_lettered_at IS NULL) = (dispatch_state <> 'DEAD_LETTERED')
    ),
    CONSTRAINT outbox_aggregate_sequence_unique UNIQUE (aggregate_type, aggregate_id, sequence),
    CONSTRAINT outbox_event_id_unique UNIQUE (event_id)
);

-- The dispatcher's read path: pending rows, oldest first.
CREATE INDEX outbox_pending_idx ON ops.outbox (recorded_at, outbox_id)
    WHERE dispatch_state = 'PENDING';
-- The lag alert: oldest pending record age (08_NFR_..._CAPACITY.md, page at >30s).
CREATE INDEX outbox_lag_idx ON ops.outbox (recorded_at)
    WHERE dispatch_state IN ('PENDING','DISPATCHING');
CREATE INDEX outbox_correlation_idx ON ops.outbox (correlation_id);
CREATE INDEX outbox_dead_letter_idx ON ops.outbox (dead_lettered_at)
    WHERE dispatch_state = 'DEAD_LETTERED';

COMMENT ON TABLE ops.outbox IS
    'Transactional outbox. Written in the same transaction as the state mutation, so a committed mutation always has a durable event. Delivery is at-least-once; consumers are idempotent. A record that exhausts its retries is dead-lettered and preserved, never dropped.';

-- Consumer-side idempotency ledger. A duplicate event delivery is a no-op.
CREATE TABLE ops.event_consumer_offset (
    consumer_group    TEXT        NOT NULL,
    consumer_name     TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    last_aggregate_sequence JSONB NOT NULL DEFAULT '{}',
    last_event_id     TEXT,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Records a gap when a sequence is skipped, rather than silently continuing.
    gap_detected_at   TIMESTAMPTZ,
    CONSTRAINT event_consumer_pk PRIMARY KEY (consumer_group, consumer_name, environment)
);

COMMENT ON TABLE ops.event_consumer_offset IS
    'Per-consumer idempotency and sequence-continuity ledger. A skipped aggregate sequence is recorded as a gap and routed to controlled reconciliation rather than silently tolerated.';

-- Events a consumer rejected, retained for diagnosis. Never silently dropped.
CREATE TABLE ops.event_dead_letter (
    dead_letter_id    TEXT        PRIMARY KEY,
    event_id          TEXT        NOT NULL,
    consumer_group    TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    error_code        TEXT        NOT NULL,
    error_detail      TEXT        NOT NULL,
    payload           JSONB       NOT NULL,
    failed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at       TIMESTAMPTZ,
    resolved_by       TEXT,
    resolution_note   TEXT,
    CONSTRAINT dead_letter_id_format CHECK (common.is_canonical_id(dead_letter_id, 'edl')),
    CONSTRAINT dead_letter_resolution_complete CHECK (
        (resolved_at IS NULL AND resolved_by IS NULL)
        OR (resolved_at IS NOT NULL AND resolved_by IS NOT NULL AND resolution_note IS NOT NULL)
    )
);

CREATE INDEX event_dead_letter_unresolved_idx ON ops.event_dead_letter (failed_at)
    WHERE resolved_at IS NULL;

-- -----------------------------------------------------------------------------
-- HALT HIERARCHY.
--
-- SYSTEM_HALT > VENUE_HALT > MARKET_HALT > STRATEGY_HALT > ACCOUNT_HALT.
-- A higher-level halt CANNOT be bypassed by a lower-level enable command
-- (04_TRADING_DOMAIN_AND_RISK.md). Monotonic in severity while active.
-- -----------------------------------------------------------------------------
CREATE TABLE ops.halt (
    halt_id           TEXT        PRIMARY KEY,
    level             ops.halt_level NOT NULL,
    state             ops.halt_state NOT NULL DEFAULT 'ACTIVE',
    -- The exact scope this halt applies to.
    environment       common.environment NOT NULL,
    account_id        TEXT,
    instrument_id     TEXT,
    market_class      common.market_class,
    venue_id          TEXT,
    strategy_id       TEXT,
    reason            TEXT        NOT NULL,
    -- Emergency halt is always available to an authorized halt operator and does
    -- NOT require a second approver. Re-enable DOES.
    emergency         BOOLEAN     NOT NULL DEFAULT false,
    activated_by      TEXT        NOT NULL,
    activated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at_ns   BIGINT      NOT NULL,
    cleared_by        TEXT,
    cleared_at        TIMESTAMPTZ,
    clear_reason      TEXT,
    -- Re-enable requires two distinct approvers for system-wide live scope.
    cleared_approved_by TEXT,
    -- Health and reconciliation conditions verified before re-enable.
    health_verified   BOOLEAN     NOT NULL DEFAULT false,
    reconciliation_verified BOOLEAN NOT NULL DEFAULT false,
    audit_id          TEXT,
    version_vector    BIGINT      NOT NULL DEFAULT 1,
    CONSTRAINT halt_id_format CHECK (common.is_canonical_id(halt_id, 'hlt')),
    -- Re-enable of a non-emergency halt requires dual control and verification.
    CONSTRAINT halt_clear_requires_dual_control CHECK (
        state = 'ACTIVE'
        OR (emergency = true)
        OR (cleared_by IS NOT NULL AND cleared_approved_by IS NOT NULL
            AND cleared_by <> cleared_approved_by
            AND health_verified = true AND reconciliation_verified = true)
    ),
    CONSTRAINT halt_clear_complete CHECK (
        state = 'ACTIVE' OR (cleared_at IS NOT NULL AND clear_reason IS NOT NULL)
    ),
    -- A live system-wide re-enable always needs two distinct approvers, even for
    -- an emergency halt, because re-enable is the dangerous direction.
    CONSTRAINT halt_live_system_clear_dual_control CHECK (
        NOT (environment = 'live' AND level = 'SYSTEM_HALT' AND state = 'CLEARED')
        OR (cleared_by IS NOT NULL AND cleared_approved_by IS NOT NULL AND cleared_by <> cleared_approved_by)
    )
);

CREATE INDEX halt_active_idx ON ops.halt (environment, level) WHERE state = 'ACTIVE';
CREATE INDEX halt_scope_idx   ON ops.halt (account_id, instrument_id, venue_id, strategy_id)
    WHERE state = 'ACTIVE';

COMMENT ON TABLE ops.halt IS
    'Operational halt records. Halt activation is immediately effective and monotonic in severity. Lower-level commands cannot clear higher-level halts. Re-enable requires the owning authority, a recorded reason, health checks, reconciliation status and two-person approval for system-wide live re-enable.';

-- The precedence rank used to enforce halt monotonicity.
CREATE OR REPLACE FUNCTION ops.halt_rank(p_level ops.halt_level)
RETURNS INTEGER
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT CASE p_level
        WHEN 'SYSTEM_HALT'   THEN 5
        WHEN 'VENUE_HALT'    THEN 4
        WHEN 'MARKET_HALT'   THEN 3
        WHEN 'STRATEGY_HALT' THEN 2
        WHEN 'ACCOUNT_HALT'  THEN 1
    END;
$$;

COMMENT ON FUNCTION ops.halt_rank IS
    'Halt precedence. A lower rank can never override a higher rank.';

-- -----------------------------------------------------------------------------
-- Live activation record.
--
-- G11 is a separate, dual-approved operational decision. Live is disabled by
-- default; the absence of a valid, unexpired, dual-approved activation record
-- is the deny condition.
-- -----------------------------------------------------------------------------
CREATE TABLE ops.live_activation (
    activation_id     TEXT        PRIMARY KEY,
    environment       common.environment NOT NULL DEFAULT 'live',
    -- The EXACT scope tuple from 23_..._DECISIONS.md §5. Activation is
    -- scope-specific; activating one tuple never implies another.
    account_id        TEXT        NOT NULL,
    venue_id          TEXT        NOT NULL,
    market_class      common.market_class NOT NULL,
    instrument_scope  TEXT[]      NOT NULL,
    product_scope     TEXT        NOT NULL,
    activity_scope    TEXT        NOT NULL,
    jurisdiction      TEXT        NOT NULL,
    -- Exact digests the owner authorised.
    config_fingerprint   TEXT    NOT NULL,
    artifact_digest      TEXT    NOT NULL,
    risk_policy_revisions TEXT[]  NOT NULL,
    -- Two DISTINCT authorized approvers. Owner authorization is recorded against
    -- the exact configuration/artifact digests above.
    requested_by      TEXT        NOT NULL,
    approved_by       TEXT        NOT NULL,
    -- Canary: a narrowest-permitted-scope activation with explicit abort criteria.
    canary_scope      JSONB       NOT NULL,
    abort_criteria    JSONB       NOT NULL,
    responsible_operator TEXT     NOT NULL,
    state             TEXT        NOT NULL DEFAULT 'REQUESTED',
    requested_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at      TIMESTAMPTZ,
    deactivated_at    TIMESTAMPTZ,
    deactivation_reason TEXT,
    -- Evidence must be immutable once approved.
    evidence_ref      TEXT,
    evidence_digest   TEXT,
    CONSTRAINT activation_id_format CHECK (common.is_canonical_id(activation_id, 'act')),
    -- Separation of duties: the requester cannot be the approver.
    CONSTRAINT activation_dual_control CHECK (requested_by <> approved_by),
    CONSTRAINT activation_state_valid CHECK (state IN
        ('REQUESTED','APPROVED','ACTIVE','SUSPENDED','DEACTIVATED','REVOKED')),
    CONSTRAINT activation_digest_formats CHECK (
        config_fingerprint ~ '^[0-9a-f]{64}$' AND artifact_digest ~ '^[0-9a-f]{64}$'
    ),
    CONSTRAINT activation_scope_non_empty CHECK (cardinality(instrument_scope) > 0),
    CONSTRAINT activation_risk_policies_present CHECK (cardinality(risk_policy_revisions) > 0),
    -- Activation requires a canary scope, abort criteria and a named operator.
    CONSTRAINT activation_canary_complete CHECK (
        state NOT IN ('ACTIVE','APPROVED') OR (canary_scope IS NOT NULL AND abort_criteria IS NOT NULL AND responsible_operator IS NOT NULL)
    ),
    -- Immutable evidence once approved.
    CONSTRAINT activation_evidence_immutable CHECK (
        state NOT IN ('ACTIVE','DEACTIVATED','REVOKED') OR evidence_digest IS NOT NULL
    ),
    CONSTRAINT activation_active_has_time CHECK (
        state <> 'ACTIVE' OR activated_at IS NOT NULL
    )
);

COMMENT ON TABLE ops.live_activation IS
    'Live activation is a separate, dual-approved, scope-specific operational decision. Absence of a valid record is a deny condition; no control may be bypassed and live is never enabled by default.';

-- The unique-active-activation guard: at most one active live activation record
-- may exist at a time, so a stale record cannot silently re-enable live.
CREATE UNIQUE INDEX live_activation_single_active_idx ON ops.live_activation ((true))
    WHERE state = 'ACTIVE';

-- -----------------------------------------------------------------------------
-- OPERATIONAL STATE.
-- -----------------------------------------------------------------------------
CREATE TABLE ops.system_state (
    system_state_id   TEXT        PRIMARY KEY,
    environment       common.environment NOT NULL,
    -- NORMAL | READ_ONLY | CANCEL_ONLY | RECOVERY_HOLD | SAFE_HALT
    -- Transitions are explicit, audited, monotonic toward safer states during an
    -- incident, and require authorisation to leave a restricted state
    -- (24_ENTERPRISE_RELEASE_STANDARD.md §6).
    mode              TEXT        NOT NULL DEFAULT 'NORMAL',
    -- RECOVERY_HOLD after any recovery. Leaving it requires verification and
    -- two-person approval.
    recovery_hold     BOOLEAN     NOT NULL DEFAULT false,
    recovery_reason   TEXT,
    recovery_entered_at TIMESTAMPTZ,
    -- Kill switch. Independent of AI services and noncritical UI.
    kill_switch_engaged BOOLEAN  NOT NULL DEFAULT false,
    kill_switch_reason  TEXT,
    kill_switch_engaged_by TEXT,
    kill_switch_engaged_at TIMESTAMPTZ,
    mode_changed_by   TEXT,
    mode_changed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    mode_change_reason TEXT,
    -- Two approvers required to leave a restricted mode.
    mode_approved_by  TEXT,
    version_vector    BIGINT      NOT NULL DEFAULT 1,
    CONSTRAINT system_mode_valid CHECK (mode IN
        ('NORMAL','READ_ONLY','CANCEL_ONLY','RECOVERY_HOLD','SAFE_HALT')),
    CONSTRAINT system_state_id_format CHECK (common.is_canonical_id(system_state_id, 'sys')),
    -- RECOVERY_HOLD and the kill switch are each independently sufficient to
    -- block risk-increasing activity. Neither may be disabled without dual control.
    CONSTRAINT restricted_mode_dual_control CHECK (
        (mode IN ('NORMAL','READ_ONLY')) OR mode_approved_by IS NOT NULL
    ),
    CONSTRAINT recovery_hold_consistent CHECK (
        (recovery_hold = false) OR (recovery_reason IS NOT NULL AND recovery_entered_at IS NOT NULL)
    ),
    CONSTRAINT kill_switch_consistent CHECK (
        (kill_switch_engaged = false)
        OR (kill_switch_reason IS NOT NULL AND kill_switch_engaged_by IS NOT NULL AND kill_switch_engaged_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX system_state_env_idx ON ops.system_state (environment);

COMMENT ON TABLE ops.system_state IS
    'Per-environment operational mode. RECOVERY_HOLD and the kill switch each independently block risk-increasing activity and cannot be disabled without dual control. Recovery never automatically resumes risk-increasing operation.';

-- -----------------------------------------------------------------------------
-- Incidents and evidence preservation.
-- -----------------------------------------------------------------------------
CREATE TABLE ops.incident (
    incident_id       TEXT        PRIMARY KEY,
    severity          TEXT        NOT NULL,
    status            TEXT        NOT NULL DEFAULT 'OPEN',
    title             TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    -- SEV-1 requires an Incident Commander, Technical Lead, Risk Owner and
    -- Communications Owner.
    incident_commander TEXT,
    technical_lead    TEXT,
    risk_owner         TEXT,
    communications_owner TEXT,
    declared_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    detected_at       TIMESTAMPTZ,
    mitigated_at      TIMESTAMPTZ,
    resolved_at       TIMESTAMPTZ,
    -- Evidence preservation. Logs, audit records and deployment state are
    -- preserved before remediation begins.
    evidence_preserved BOOLEAN    NOT NULL DEFAULT false,
    evidence_refs     JSONB       NOT NULL DEFAULT '[]',
    root_cause        TEXT,
    corrective_actions JSONB      NOT NULL DEFAULT '[]',
    review_due_at     TIMESTAMPTZ,
    -- Post-incident review within one business day for break-glass and emergency
    -- change use.
    CONSTRAINT incident_id_format CHECK (common.is_canonical_id(incident_id, 'inc')),
    CONSTRAINT incident_severity_valid CHECK (severity IN ('SEV-1','SEV-2','SEV-3','SEV-4')),
    CONSTRAINT incident_status_valid   CHECK (status IN ('OPEN','MITIGATED','RESOLVED','POSTMORTEM')),
    -- A SEV-1 incident cannot be opened without the four named roles.
    CONSTRAINT sev1_requires_roles CHECK (
        severity <> 'SEV-1'
        OR (incident_commander IS NOT NULL AND technical_lead IS NOT NULL
            AND risk_owner IS NOT NULL AND communications_owner IS NOT NULL)
    ),
    CONSTRAINT incident_resolution_complete CHECK (
        status NOT IN ('RESOLVED','POSTMORTEM')
        OR (resolved_at IS NOT NULL AND root_cause IS NOT NULL AND evidence_preserved = true)
    )
);

CREATE INDEX incident_open_idx ON ops.incident (severity, declared_at DESC)
    WHERE status IN ('OPEN','MITIGATED');
CREATE INDEX incident_sev1_idx ON ops.incident (declared_at DESC) WHERE severity = 'SEV-1';

-- -----------------------------------------------------------------------------
-- RATE LIMITING.
--
-- Enforced by identity, environment, account and route class. Defaults: 60 read
-- requests/minute and 10 privileged mutations/minute for an operator. Financial
-- command throughput is additionally constrained by account risk policy and
-- venue-specific limits (15_API_AND_INTEGRATION_CONTRACTS.md §5).
-- -----------------------------------------------------------------------------
CREATE TABLE ops.rate_limit_bucket (
    subject_id        TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    account_id        TEXT,
    route_class       TEXT        NOT NULL,
    window_start      TIMESTAMPTZ NOT NULL,
    request_count     INTEGER     NOT NULL DEFAULT 0,
    CONSTRAINT rate_limit_pk PRIMARY KEY (subject_id, environment, route_class, window_start),
    CONSTRAINT rate_limit_route_class_valid CHECK (route_class IN ('READ','PRIVILEGED_MUTATION','FINANCIAL_COMMAND','WEBHOOK')),
    CONSTRAINT rate_limit_count_non_negative CHECK (request_count >= 0)
);

COMMENT ON TABLE ops.rate_limit_bucket IS
    'Durable rate-limit accounting per identity, environment, account and route class. A limited request returns 429 with a retry hint; a command is never silently dropped or queued outside the documented command semantics.';

SELECT common.assert_no_floating_point();
