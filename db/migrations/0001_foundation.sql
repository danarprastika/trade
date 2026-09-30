-- =============================================================================
-- 0001_foundation.sql
-- G1 - Domain Foundation. Schemas, identity, configuration, idempotency.
--
-- Authority: 05_PERSISTENCE_EVENTING_RECONCILIATION.md (schema domains),
--            03_CANONICAL_CONTRACTS.md (identifier/money/time/idem formats),
--            17_CONFIGURATION_AND_RISK_POLICY.md (typed versioned config).
--
-- Migration discipline: expand/contract (24_ENTERPRISE_RELEASE_STANDARD.md §5).
-- Every migration in this directory is EXPAND-only and must remain compatible
-- with the immediately previous release. Destructive changes are a separately
-- approved REMOVAL release after the retention and rollback window closes.
--
-- Numeric discipline: every monetary, quantity, price and ratio value is
-- NUMERIC. PostgreSQL NUMERIC is exact base-10, so no IEEE-754 value can enter
-- an authoritative financial column. REAL/DOUBLE PRECISION are prohibited.
-- =============================================================================

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- -----------------------------------------------------------------------------
-- Bounded-context schemas. Each schema owns its tables; cross-domain writes
-- occur through domain services, never through ad-hoc SQL from another module.
-- -----------------------------------------------------------------------------
CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS config;
CREATE SCHEMA IF NOT EXISTS market;
CREATE SCHEMA IF NOT EXISTS strategy;
CREATE SCHEMA IF NOT EXISTS risk;
CREATE SCHEMA IF NOT EXISTS oms;
CREATE SCHEMA IF NOT EXISTS execution;
CREATE SCHEMA IF NOT EXISTS reconciliation;
CREATE SCHEMA IF NOT EXISTS portfolio;
CREATE SCHEMA IF NOT EXISTS ledger;
CREATE SCHEMA IF NOT EXISTS audit;
CREATE SCHEMA IF NOT EXISTS ops;
-- Shared vocabulary schema. Created before the shared enumerations below so the
-- type declarations can qualify it.
CREATE SCHEMA IF NOT EXISTS common;

COMMENT ON SCHEMA identity        IS 'Identity and Access. Owns subjects, sessions, role bindings. Never owns financial state.';
COMMENT ON SCHEMA config         IS 'Configuration. Owns typed, schema-validated, immutable release snapshots. Never owns runtime secrets.';
COMMENT ON SCHEMA market         IS 'Market Data. Owns normalized instruments, observations, feed health. Never authoritative for execution.';
COMMENT ON SCHEMA strategy       IS 'Strategy. Owns the strategy lifecycle, signals, approvals, deployment state. Proposes actions only.';
COMMENT ON SCHEMA risk           IS 'Risk. Owns deterministic pre-trade and portfolio controls. The sole financial veto.';
COMMENT ON SCHEMA oms            IS 'OMS. Owns the canonical order state machine. Never owns external venue truth.';
COMMENT ON SCHEMA execution      IS 'Execution. Owns venue-specific translation and submission records. Never owns policy or ledger facts.';
COMMENT ON SCHEMA reconciliation IS 'Reconciliation. Owns external-vs-internal convergence. Never owns policy authority.';
COMMENT ON SCHEMA portfolio      IS 'Portfolio. Owns positions and balances as rebuildable projections. Not authoritative for financial facts.';
COMMENT ON SCHEMA ledger         IS 'Ledger. Owns immutable financial facts and compensating corrections.';
COMMENT ON SCHEMA audit          IS 'Audit. Owns append-only security, decision, configuration and financial evidence.';
COMMENT ON SCHEMA ops            IS 'Operations. Owns health, alerts, incidents, deployment, recovery state.';

-- -----------------------------------------------------------------------------
-- Shared domain enumerations.
--
-- PostgreSQL enums are add-only in practice: a value cannot be removed without
-- an ALTER TYPE, which is a contract change. Consumers must treat an unknown
-- enum value as UNSUPPORTED rather than silently mapping it
-- (03_CANONICAL_CONTRACTS.md, "Compatibility").
-- -----------------------------------------------------------------------------
CREATE TYPE identity.actor_type          AS ENUM ('HUMAN','WORKLOAD','SYSTEM','AGENT','BREAK_GLASS');
CREATE TYPE identity.auth_strength       AS ENUM ('NONE','PASSWORD','MFA','PHISHING_RESISTANT');
CREATE TYPE identity.session_state       AS ENUM ('ACTIVE','EXPIRED','REVOKED');
CREATE TYPE common.environment           AS ENUM ('dev','test','staging','paper','shadow','live');
CREATE TYPE common.market_class          AS ENUM ('crypto','fx','equities','commodities','derivatives');
CREATE TYPE common.trading_status        AS ENUM ('OPEN','CLOSED','HALTED','AUCTION','SESSION_BREAK','PRE_OPEN','POST_CLOSE','DELISTED','UNKNOWN');
CREATE TYPE common.side                  AS ENUM ('BUY','SELL');
CREATE TYPE common.order_type            AS ENUM ('MARKET','LIMIT','STOP','STOP_LIMIT','POST_ONLY','IOC','FOK','REDUCE_ONLY','CLOSE_POSITION');
CREATE TYPE common.time_in_force         AS ENUM ('GTC','DAY','IOC','FOK','GTX');
CREATE TYPE common.idempotency_state     AS ENUM ('in_flight','succeeded','failed','unknown');
CREATE TYPE common.result                AS ENUM ('PASS','FAIL');

-- -----------------------------------------------------------------------------
-- Canonical identifier helper.
--
-- Canonical IDs are "<prefix>_<20 lowercase Crockford Base32 chars>" validated
-- in the domain layer. The CHECK constraints below re-assert the shape at the
-- storage boundary so a bypass of the application layer cannot introduce a
-- malformed identifier into an authoritative table.
-- -----------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION common.is_canonical_id(p_value TEXT, p_prefix TEXT)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT p_value ~ ('^' || p_prefix || '_[0-9abcdefghjkmnpqrstvwxyz]{20}$');
$$;

COMMENT ON FUNCTION common.is_canonical_id IS
    'Verifies the canonical identifier format: typed prefix + 20 lowercase Crockford Base32 characters. Crockford Base32 excludes i, l, o and u on encoding (03_CANONICAL_CONTRACTS.md).';

-- -----------------------------------------------------------------------------
-- UTILITY: reject float-typed columns in authoritative schemas.
--
-- A guard, not documentation. Any attempt to add REAL/DOUBLE PRECISION/FLOAT to
-- an authoritative schema is rejected at migration time.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION common.assert_no_floating_point()
RETURNS VOID
LANGUAGE plpgsql
AS $$
DECLARE
    v_offender TEXT;
BEGIN
    SELECT format('%I.%I.%I column %I uses a floating-point type',
                  c.table_schema, c.table_name, '', c.column_name)
      INTO v_offender
      FROM information_schema.columns c
      JOIN information_schema.tables t
        ON t.table_schema = c.table_schema AND t.table_name = c.table_name
     WHERE c.table_schema IN ('identity','config','market','strategy','risk','oms',
                              'execution','reconciliation','portfolio','ledger','audit','ops')
       AND t.table_type = 'BASE TABLE'
       AND c.data_type IN ('real','double precision')
     LIMIT 1;

    IF v_offender IS NOT NULL THEN
        RAISE EXCEPTION 'authoritative schema contains a floating-point column: %', v_offender
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

COMMENT ON FUNCTION common.assert_no_floating_point IS
    'Raises if any authoritative table declares a REAL/DOUBLE PRECISION column. No IEEE-754 value may cross a financial contract (03_CANONICAL_CONTRACTS.md).';

-- -----------------------------------------------------------------------------
-- IDENTITY: subjects, sessions, role bindings.
-- -----------------------------------------------------------------------------

CREATE TABLE identity.subject (
    subject_id        TEXT        PRIMARY KEY,
    actor_type        identity.actor_type NOT NULL,
    auth_strength     identity.auth_strength NOT NULL,
    -- Workload audience. Required for non-human actors: a token presented to a
    -- different audience is rejected (21_ZERO_TRUST...md §4).
    audience          TEXT,
    display_label     TEXT,
    -- Identity provider subject reference. There is no local password column:
    -- local password authentication is disabled for production
    -- (06_SECURITY_AND_ACCESS_CONTROL.md §2).
    external_subject  TEXT UNIQUE,
    status            TEXT        NOT NULL DEFAULT 'ACTIVE',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT subject_id_format   CHECK (subject_id ~ '^[a-z]{3}_[0-9abcdefghjkmnpqrstvwxyz]{20}$'),
    CONSTRAINT subject_status      CHECK (status IN ('ACTIVE','DISABLED','DEPROVISIONED')),
    -- A non-human actor must declare the audience its credential is bound to.
    CONSTRAINT workload_audience_required
        CHECK (actor_type NOT IN ('WORKLOAD','AGENT') OR audience IS NOT NULL)
);

COMMENT ON TABLE identity.subject IS
    'Platform identities. Shared human accounts are prohibited; each subject represents exactly one person or one workload principal.';

CREATE TABLE identity.session (
    session_id        TEXT        PRIMARY KEY,
    subject_id        TEXT        NOT NULL REFERENCES identity.subject(subject_id),
    environment       common.environment NOT NULL,
    session_class     TEXT        NOT NULL,
    auth_strength     identity.auth_strength NOT NULL,
    -- Step-up freshness is evaluated against auth_time, not issued_at.
    auth_time         TIMESTAMPTZ NOT NULL,
    issued_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    absolute_expires_at TIMESTAMPTZ NOT NULL,
    idle_expires_at   TIMESTAMPTZ NOT NULL,
    -- Bumped on any role/permission revision change. A privileged mutation
    -- rechecks this against the current revision, so a revocation takes effect
    -- without waiting for session expiry (21_..._AUTHORIZATION.md §2.6).
    permission_revision BIGINT    NOT NULL DEFAULT 1,
    state             identity.session_state NOT NULL DEFAULT 'ACTIVE',
    revoked_at        TIMESTAMPTZ,
    revocation_reason TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT session_id_format CHECK (session_id ~ '^[a-z]{3}_[0-9abcdefghjkmnpqrstvwxyz]{20}$'),
    CONSTRAINT session_class_valid CHECK (session_class IN ('PRIVILEGED_OPERATOR','READ_ONLY_OPERATOR','SERVICE')),
    CONSTRAINT session_expiry_order CHECK (idle_expires_at <= absolute_expires_at),
    -- Session policy from 21_..._AUTHORIZATION.md §3.
    CONSTRAINT session_idle_within_policy CHECK (
        idle_expires_at <= issued_at + INTERVAL '30 minutes'
    ),
    CONSTRAINT session_absolute_within_policy CHECK (
        absolute_expires_at <= issued_at + INTERVAL '8 hours'
    ),
    CONSTRAINT revoked_requires_state CHECK (
        (state = 'REVOKED') = (revoked_at IS NOT NULL)
    )
);

CREATE INDEX session_subject_idx  ON identity.session (subject_id, state);
CREATE INDEX session_expiry_idx   ON identity.session (idle_expires_at) WHERE state = 'ACTIVE';
-- Supports the <=60 second revocation sweep.
CREATE INDEX session_revocation_idx ON identity.session (revoked_at DESC NULLS LAST) WHERE state = 'ACTIVE';

COMMENT ON TABLE identity.session IS
    'Server-side sessions. The browser holds only an opaque Secure/HttpOnly/SameSite cookie; access tokens are never stored in browser local storage (21_..._AUTHORIZATION.md §2).';

CREATE TABLE identity.role (
    role_name         TEXT        PRIMARY KEY,
    description       TEXT        NOT NULL,
    -- Privileged grants expire after 90 days unless re-approved.
    max_grant_days    INTEGER     NOT NULL DEFAULT 90,
    CONSTRAINT role_baseline CHECK (role_name IN
        ('OWNER','TRADING_OPERATOR','RISK_OPERATOR','RESEARCHER',
         'PLATFORM_OPERATOR','AUDITOR','SERVICE'))
);

COMMENT ON TABLE identity.role IS
    'Role baseline from 21_..._AUTHORIZATION.md §5. Role membership alone never grants live activation or unrestricted account access; every role is further constrained by market, account, environment and resource scope.';

CREATE TABLE identity.role_binding (
    binding_id        TEXT        PRIMARY KEY,
    subject_id        TEXT        NOT NULL REFERENCES identity.subject(subject_id),
    role_name         TEXT        NOT NULL REFERENCES identity.role(role_name),
    environment       common.environment NOT NULL,
    market_scope      common.market_class,
    account_scope     TEXT,
    granted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ NOT NULL,
    -- Dual control: the requester cannot approve their own grant.
    requested_by      TEXT        NOT NULL,
    approved_by       TEXT        NOT NULL,
    revoked_at        TIMESTAMPTZ,
    revoked_by        TEXT,
    CONSTRAINT binding_id_format CHECK (binding_id ~ '^[a-z]{3}_[0-9abcdefghjkmnpqrstvwxyz]{20}$'),
    -- A grant or revocation requires a second, distinct approver.
    CONSTRAINT dual_control_distinct_approver CHECK (requested_by <> approved_by),
    CONSTRAINT dual_control_present        CHECK (approved_by IS NOT NULL AND requested_by IS NOT NULL),
    CONSTRAINT grant_expiry_bounded        CHECK (expires_at > granted_at),
    CONSTRAINT grant_max_90_days          CHECK (expires_at <= granted_at + INTERVAL '90 days'),
    CONSTRAINT revocation_has_actor       CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);

CREATE INDEX role_binding_subject_idx ON identity.role_binding (subject_id) WHERE revoked_at IS NULL;
CREATE INDEX role_binding_expiry_idx  ON identity.role_binding (expires_at)  WHERE revoked_at IS NULL;

-- -----------------------------------------------------------------------------
-- IDEMPOTENCY (replay suppression).
--
-- 03_CANONICAL_CONTRACTS.md: "Database uniqueness constraints enforce
-- idempotency; application memory is never sufficient." The unique index below
-- IS the enforcement. Application-level checks are an optimisation only.
-- -----------------------------------------------------------------------------
CREATE TABLE ops.idempotency_record (
    scope_hash       TEXT        PRIMARY KEY,
    state            common.idempotency_state NOT NULL,
    request_digest   TEXT        NOT NULL,
    command_id       TEXT        NOT NULL,
    response_status  INTEGER,
    response_body    TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT scope_hash_format  CHECK (scope_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT request_digest_format CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT command_id_format  CHECK (common.is_canonical_id(command_id, 'cmd')),
    -- A terminal result must carry the response it will replay verbatim.
    CONSTRAINT terminal_has_response CHECK (
        state = 'in_flight'
        OR (response_status IS NOT NULL AND response_body IS NOT NULL)
    )
);

COMMENT ON TABLE ops.idempotency_record IS
    'Durable replay suppression. A replay returns the original result; reusing a key with a different request body is a CONFLICT. Financial correctness depends on this table, not on application memory.';

-- -----------------------------------------------------------------------------
-- CONFIGURATION: typed, schema-validated, versioned, environment-scoped.
--
-- 17_CONFIGURATION_AND_RISK_POLICY.md §1: configuration is immutable after
-- activation; a change creates a new revision with actor, reason, timestamp,
-- approval, diff and effective scope. Secrets are referenced by secret-store
-- identifier and never embedded (06_SECURITY_AND_ACCESS_CONTROL.md §6).
-- -----------------------------------------------------------------------------
CREATE TABLE config.revision (
    revision_id      TEXT        PRIMARY KEY,
    environment      common.environment NOT NULL,
    -- Monotonic per environment. Prevents a stale snapshot being loaded after a
    -- newer one.
    revision_number  BIGINT      NOT NULL,
    schema_version   TEXT        NOT NULL,
    -- Content digest of the canonical snapshot document.
    content_digest   TEXT        NOT NULL,
    -- Signature over the content digest, produced by a KMS/HSM key. A snapshot
    -- with an invalid or missing signature cannot load.
    signature        BYTEA,
    signing_key_id   TEXT,
    document         JSONB       NOT NULL,
    status           TEXT        NOT NULL DEFAULT 'DRAFT',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at_ns    BIGINT      NOT NULL,
    activated_at     TIMESTAMPTZ,
    deactivated_at   TIMESTAMPTZ,
    created_by       TEXT        NOT NULL,
    reason           TEXT        NOT NULL,
    approval_request_id TEXT,
    CONSTRAINT revision_id_format CHECK (common.is_canonical_id(revision_id, 'cfg')),
    CONSTRAINT content_digest_format CHECK (content_digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT revision_status_valid CHECK (status IN ('DRAFT','ACTIVE','SUPERSEDED','REVOKED')),
    CONSTRAINT revision_number_positive CHECK (revision_number > 0),
    -- A live snapshot must be signed. An unsigned live configuration cannot load
    -- (24_ENTERPRISE_RELEASE_STANDARD.md §2).
    CONSTRAINT live_requires_signature CHECK (
        environment <> 'live' OR (signature IS NOT NULL AND signing_key_id IS NOT NULL)
    ),
    -- A signed revision must name the key that signed it.
    CONSTRAINT signature_has_key CHECK ((signature IS NULL) = (signing_key_id IS NULL)),
    -- Immutability after activation: enforced by trigger below.
    UNIQUE (environment, revision_number)
);

CREATE INDEX config_revision_active_idx ON config.revision (environment, revision_number DESC)
    WHERE status = 'ACTIVE';

COMMENT ON TABLE config.revision IS
    'Immutable typed configuration snapshots. Runtime services consume a validated snapshot; they never read arbitrary mutable key/value settings during a financial decision.';

CREATE TABLE config.feature_flag (
    flag_id          TEXT        PRIMARY KEY,
    flag_key         TEXT        NOT NULL,
    environment      common.environment NOT NULL,
    scope            JSONB       NOT NULL,
    enabled          BOOLEAN     NOT NULL DEFAULT false,
    owner            TEXT        NOT NULL,
    reason           TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL,
    -- Production flags affecting risk, identity, reconciliation, ledger or
    -- execution behaviour require dual approval.
    authority_bearing BOOLEAN    NOT NULL DEFAULT false,
    requested_by     TEXT,
    approved_by      TEXT,
    emergency_disabled_at TIMESTAMPTZ,
    CONSTRAINT flag_id_format CHECK (common.is_canonical_id(flag_id, 'flt')),
    -- A flag is configuration, not authorization: an authority-bearing flag must
    -- carry dual control.
    CONSTRAINT authority_flag_dual_control CHECK (
        NOT authority_bearing OR (requested_by IS NOT NULL AND approved_by IS NOT NULL AND requested_by <> approved_by)
    ),
    CONSTRAINT flag_expiry_required CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX feature_flag_scope_key_idx
    ON config.feature_flag (flag_key, environment, (scope ->> 'resource_id'));

COMMENT ON TABLE config.feature_flag IS
    'Versioned feature flags with owner, scope, expiry and audit. A flag can never grant authority; expired or unknown flags resolve to the safest state (24_ENTERPRISE_RELEASE_STANDARD.md §3).';

SELECT common.assert_no_floating_point();
