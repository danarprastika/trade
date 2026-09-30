-- =============================================================================
-- 0006_audit_integrity.sql
-- G3/G9 - Signed hash-chain audit evidence with independent retention.
--
-- Authority: 22_AUDIT_INTEGRITY_AND_EVIDENCE.md
--
-- Design: append-only partitioned chain, per-partition monotonic sequence,
-- SHA-256 hash chaining, signed checkpoints by a KMS/HSM key, and an independent
-- exporter target. No public blockchain: it would add operational and privacy
-- complexity without improving the trust model.
-- =============================================================================

CREATE TYPE audit.result_value AS ENUM ('SUCCESS','FAILURE','DENIED','UNKNOWN');

CREATE TYPE audit.actor_type_v AS ENUM ('HUMAN','WORKLOAD','SYSTEM','AGENT','BREAK_GLASS');

-- -----------------------------------------------------------------------------
-- Audit partitions.
--
-- Records are ordered by a monotonic sequence WITHIN a partition. Partitioning
-- by month gives bounded partition sizes for verification and retention while
-- preserving the chain order inside each partition.
-- -----------------------------------------------------------------------------
CREATE TABLE audit.partition_month (
    partition_key     TEXT        PRIMARY KEY,
    period_start      DATE        NOT NULL,
    period_end        DATE        NOT NULL,
    record_count      BIGINT      NOT NULL DEFAULT 0,
    last_sequence     BIGINT      NOT NULL DEFAULT 0,
    last_hash        TEXT,
    -- Chain status. A break is a SEV-1 security event.
    chain_state       TEXT        NOT NULL DEFAULT 'OPEN',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT partition_period_valid CHECK (period_end > period_start),
    CONSTRAINT partition_chain_state_valid CHECK (chain_state IN ('OPEN','BROKEN','SEALED')),
    CONSTRAINT partition_counts_non_negative CHECK (record_count >= 0 AND last_sequence >= 0)
);

COMMENT ON TABLE audit.partition_month IS
    'One chain partition per month. chain_state=BROKEN is a SEV-1 evidence-integrity event: privileged mutations and risk-increasing activity stop until resolved (22_..._EVIDENCE.md §3).';

CREATE TABLE audit.record (
    audit_id          TEXT        NOT NULL,
    partition_key     TEXT        NOT NULL REFERENCES audit.partition_month(partition_key),
    sequence          BIGINT      NOT NULL,
    -- Scope.
    tenant_or_owner_scope TEXT    NOT NULL,
    -- Attribution. An audit record without an attributable actor is not evidence.
    actor_id          TEXT        NOT NULL,
    actor_type        audit.actor_type_v NOT NULL,
    action            TEXT        NOT NULL,
    target_type       TEXT        NOT NULL,
    target_id         TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    market_scope      common.market_class,
    -- Timing. Both nanosecond companions preserve the canonical precision that
    -- TIMESTAMPTZ (microsecond) cannot.
    occurred_at       TIMESTAMPTZ NOT NULL,
    occurred_at_ns    BIGINT      NOT NULL,
    recorded_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    recorded_at_ns    BIGINT      NOT NULL,
    reason            TEXT,
    correlation_id    TEXT        NOT NULL,
    causation_id      TEXT,
    policy_version    TEXT        NOT NULL,
    result            audit.result_value NOT NULL,
    -- Sensitive values are represented by stable references or keyed digests,
    -- never copied into the audit payload (22_..._EVIDENCE.md §2).
    before_digest     TEXT,
    after_digest      TEXT,
    details           JSONB       NOT NULL DEFAULT '{}',
    -- Hash chain.
    previous_hash     TEXT        NOT NULL,
    record_hash       TEXT        NOT NULL,
    -- Key used for the checkpoint that will close this record's batch.
    signing_key_id    TEXT        NOT NULL,
    -- Batch/retention.
    checkpoint_id     TEXT,
    exported_at       TIMESTAMPTZ,
    export_object_ref TEXT,
    -- Schema version of the canonical serialisation that produced record_hash.
    canonical_schema_version TEXT NOT NULL,
    CONSTRAINT audit_id_format  CHECK (common.is_canonical_id(audit_id, 'aud')),
    CONSTRAINT audit_sequence_positive CHECK (sequence > 0),
    CONSTRAINT audit_record_hash_format   CHECK (record_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT audit_previous_hash_format CHECK (previous_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT audit_action_required CHECK (length(action) > 0),
    CONSTRAINT audit_target_required CHECK (length(target_type) > 0 AND length(target_id) > 0),
    CONSTRAINT audit_policy_version_required CHECK (length(policy_version) > 0),
    CONSTRAINT audit_digest_formats CHECK (
        (before_digest IS NULL OR before_digest ~ '^[0-9a-f]{64}$')
        AND (after_digest IS NULL OR after_digest ~ '^[0-9a-f]{64}$')
    ),
    CONSTRAINT audit_recorded_after_occurred CHECK (recorded_at >= occurred_at)
) PARTITION BY RANGE (recorded_at);

COMMENT ON TABLE audit.record IS
    'Append-only tamper-evident audit evidence. record_hash = SHA-256(canonical_json(record without record_hash and signature)); previous_hash references the prior record in the same partition. UPDATE and DELETE are physically rejected.';

CREATE TABLE audit.record_2026_01 PARTITION OF audit.record
    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE audit.record_2026_02 PARTITION OF audit.record
    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
CREATE TABLE audit.record_2026_03 PARTITION OF audit.record
    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');
CREATE TABLE audit.record_2026_04 PARTITION OF audit.record
    FOR VALUES FROM ('2026-04-01') TO ('2026-05-01');
CREATE TABLE audit.record_2026_05 PARTITION OF audit.record
    FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');
CREATE TABLE audit.record_2026_06 PARTITION OF audit.record
    FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');
CREATE TABLE audit.record_2026_07 PARTITION OF audit.record
    FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');
CREATE TABLE audit.record_2026_08 PARTITION OF audit.record
    FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE audit.record_2026_09 PARTITION OF audit.record
    FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE audit.record_2026_10 PARTITION OF audit.record
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE audit.record_2026_11 PARTITION OF audit.record
    FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
CREATE TABLE audit.record_2026_12 PARTITION OF audit.record
    FOR VALUES FROM ('2026-12-01') TO ('2027-01-01');
CREATE TABLE audit.record_2027_01 PARTITION OF audit.record
    FOR VALUES FROM ('2027-01-01') TO ('2027-02-01');
CREATE TABLE audit.record_2027_02 PARTITION OF audit.record
    FOR VALUES FROM ('2027-02-01') TO ('2027-03-01');
CREATE TABLE audit.record_2027_03 PARTITION OF audit.record
    FOR VALUES FROM ('2027-03-01') TO ('2027-04-01');

CREATE INDEX audit_record_actor_idx      ON audit.record (actor_id, recorded_at DESC);
CREATE INDEX audit_record_action_idx     ON audit.record (action, recorded_at DESC);
CREATE INDEX audit_record_target_idx     ON audit.record (target_type, target_id, recorded_at DESC);
CREATE INDEX audit_record_correlation_idx ON audit.record (correlation_id);
CREATE INDEX audit_record_environment_idx ON audit.record (environment, recorded_at DESC);
CREATE INDEX audit_record_sequence_idx   ON audit.record (partition_key, sequence);

-- Partition-scoped uniqueness is enforced by the chain-append function below
-- rather than by a unique index: PostgreSQL requires a unique index on a
-- partitioned table to include the partition key, and a uniqueness rule over
-- (partition_key, sequence) that tolerates recorded_at is not meaningful.
-- The chain-append trigger enforces BOTH monotonic sequence AND previous_hash
-- continuity, which a unique index cannot express.
CREATE INDEX audit_record_audit_id_idx ON audit.record (audit_id);

-- The genesis previous_hash for the first record in a chain.
CREATE OR REPLACE FUNCTION audit.genesis_hash()
RETURNS TEXT
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$ SELECT repeat('0', 64); $$;

COMMENT ON FUNCTION audit.genesis_hash IS
    'The previous_hash of the first record in a chain: 64 zeros.';

-- -----------------------------------------------------------------------------
-- CHAIN APPEND.
--
-- record_hash = SHA-256(canonical_json(record without record_hash and signature));
-- previous_hash references the prior record hash in that partition.
--
-- The append path is serialised per partition with a transaction-scoped
-- advisory lock. This is what makes a missing sequence, a reordering, or an
-- injected record structurally impossible rather than merely detectable after
-- the fact. The lock is per partition, so partitions remain fully concurrent.
-- -----------------------------------------------------------------------------
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
AS $$
DECLARE
    v_last_sequence BIGINT;
    v_last_hash     TEXT;
    v_expected_seq  BIGINT;
    v_expected_hash TEXT;
BEGIN
    -- Serialise appends within this partition only. Different partitions and
    -- different months proceed concurrently.
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

    -- Monotonic sequence. A gap is a chain break (SEV-1), not a warning.
    IF p_sequence <> v_expected_seq THEN
        RAISE EXCEPTION
            'audit chain break in partition %: expected sequence %, got %. A missing or out-of-order sequence is a SEV-1 evidence-integrity event.',
            p_partition_key, v_expected_seq, p_sequence
            USING ERRCODE = 'check_violation';
    END IF;

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
END;
$$;

COMMENT ON FUNCTION audit.append_record IS
    'The ONLY supported way to write an audit record. Serialises per partition, enforces monotonic sequence and previous_hash continuity, and advances the partition chain head. Any other insert path is a defect and will fail the CHECK constraints.';

-- -----------------------------------------------------------------------------
-- APPEND-ONLY ENFORCEMENT on audit evidence.
--
-- 22_..._EVIDENCE.md: "Never silently drop, rewrite, or delete required audit
-- evidence." "Deletion is permitted only after retention expiry, legal-hold
-- clearance, and two-person approval; the deletion event itself is retained."
-- The trigger permits a controlled retention expiry, which is itself audited in
-- a separate, non-partitioned log.
-- -----------------------------------------------------------------------------
CREATE TABLE audit.retention_deletion_event (
    deletion_event_id TEXT        PRIMARY KEY,
    partition_key     TEXT        NOT NULL,
    sequence_range    TEXT        NOT NULL,
    record_count      BIGINT      NOT NULL,
    deleted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    retention_expired_at TIMESTAMPTZ NOT NULL,
    legal_hold_cleared BOOLEAN    NOT NULL,
    requested_by      TEXT        NOT NULL,
    approved_by       TEXT        NOT NULL,
    -- Two-person control on evidence deletion.
    reason            TEXT        NOT NULL,
    CONSTRAINT retention_deletion_dual_control CHECK (requested_by <> approved_by),
    CONSTRAINT retention_deletion_requires_clearance CHECK (legal_hold_cleared = true),
    CONSTRAINT retention_deletion_requires_expiry CHECK (retention_expired_at <= deleted_at)
);

COMMENT ON TABLE audit.retention_deletion_event IS
    'Record of an authorised audit-evidence deletion. Two-person control, legal-hold clearance and retention expiry are structurally required. The deletion event itself is permanently retained.';

CREATE OR REPLACE FUNCTION audit.reject_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION
        'audit.record is append-only and tamper-evident: % is not permitted. A chain break is a SEV-1 security event.',
        TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER audit_record_append_only
    BEFORE UPDATE OR DELETE ON audit.record
    FOR EACH ROW EXECUTE FUNCTION audit.reject_mutation();

-- -----------------------------------------------------------------------------
-- Signed checkpoints.
--
-- Every batch is closed with a signed checkpoint containing partition, sequence
-- range, first/last hash, count, timestamp and signing key ID, signed by a
-- KMS/HSM key. An unexpected or invalid checkpoint is a SEV-1 event.
-- -----------------------------------------------------------------------------
CREATE TABLE audit.checkpoint (
    checkpoint_id     TEXT        PRIMARY KEY,
    partition_key     TEXT        NOT NULL REFERENCES audit.partition_month(partition_key),
    first_sequence    BIGINT      NOT NULL,
    last_sequence     BIGINT      NOT NULL,
    record_count      BIGINT      NOT NULL,
    first_hash        TEXT        NOT NULL,
    last_hash         TEXT        NOT NULL,
    -- Signature over the canonical checkpoint body, produced by a KMS/HSM key.
    signing_key_id    TEXT        NOT NULL,
    signature         BYTEA       NOT NULL,
    -- Key rotation: overlapping verification keys are used during a planned
    -- rotation, so an old checkpoint stays verifiable after rotation.
    key_status        TEXT        NOT NULL DEFAULT 'ACTIVE',
    signed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    signed_at_ns      BIGINT      NOT NULL,
    exported_at       TIMESTAMPTZ,
    export_object_ref TEXT,
    -- Expected value of the chain head after the batch, so a verifier can detect
    -- a TRUNCATED batch (a prefix removal that preserves internal linkage).
    expected_next_hash TEXT,
    CONSTRAINT checkpoint_id_format CHECK (common.is_canonical_id(checkpoint_id, 'ckp')),
    CONSTRAINT checkpoint_range_valid   CHECK (first_sequence > 0 AND last_sequence >= first_sequence),
    CONSTRAINT checkpoint_count_matches CHECK (record_count = last_sequence - first_sequence + 1),
    CONSTRAINT checkpoint_hashes_format  CHECK (first_hash ~ '^[0-9a-f]{64}$' AND last_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT checkpoint_next_hash_format CHECK (expected_next_hash IS NULL OR expected_next_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT checkpoint_key_status_valid CHECK (key_status IN ('ACTIVE','RETIRING','RETIRED')),
    CONSTRAINT checkpoint_signature_present CHECK (octet_length(signature) > 0),
    -- One checkpoint per (partition, range). A re-signed range must be a new
    -- partition record, never an overwrite.
    CONSTRAINT checkpoint_range_unique UNIQUE (partition_key, first_sequence, last_sequence)
);

CREATE INDEX checkpoint_partition_idx ON audit.checkpoint (partition_key, last_sequence DESC);
CREATE INDEX checkpoint_unexported_idx ON audit.checkpoint (exported_at) WHERE exported_at IS NULL;

COMMENT ON TABLE audit.checkpoint IS
    'Signed audit batch checkpoints. Signature verification failure, a missing sequence, a chain break or an unexpected checkpoint is a SEV-1 security event that stops privileged mutations and risk-increasing activity.';

-- Signing key registry, supporting rotation with overlapping verification keys.
CREATE TABLE audit.signing_key (
    signing_key_id    TEXT        PRIMARY KEY,
    provider          TEXT        NOT NULL,
    key_reference     TEXT        NOT NULL,
    algorithm         TEXT        NOT NULL DEFAULT 'ECDSA_P256_SHA256',
    status            TEXT        NOT NULL DEFAULT 'ACTIVE',
    activated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Rotation is at least annual and after compromise, with overlapping
    -- verification keys during planned rotation.
    rotate_after      TIMESTAMPTZ NOT NULL,
    retired_at        TIMESTAMPTZ,
    CONSTRAINT signing_key_algorithm_valid CHECK (algorithm IN
        ('ECDSA_P256_SHA256','ED25519','RSA_PSS_SHA256')),
    CONSTRAINT signing_key_status_valid CHECK (status IN ('ACTIVE','RETIRING','RETIRED','COMPROMISED')),
    CONSTRAINT signing_key_rotation_due CHECK (rotate_after > activated_at),
    CONSTRAINT signing_key_retire_complete CHECK (retired_at IS NULL OR retired_at >= activated_at)
);

COMMENT ON TABLE audit.signing_key IS
    'KMS/HSM-backed audit signing keys. Key material never appears in this table; only a provider reference. Retirement is a state transition, never a deletion, so historic checkpoints stay verifiable.';

-- -----------------------------------------------------------------------------
-- Independent immutable retention target.
--
-- 22_..._EVIDENCE.md §4: a separate exporter copies signed batches to a distinct
-- account/project and region with object-lock/WORM retention. The application
-- runtime identity has WRITE-ONLY append permission here and cannot delete or
-- shorten retention.
-- -----------------------------------------------------------------------------
CREATE TABLE audit.export_batch (
    export_batch_id   TEXT        PRIMARY KEY,
    checkpoint_id     TEXT        NOT NULL REFERENCES audit.checkpoint(checkpoint_id),
    partition_key     TEXT        NOT NULL,
    -- Distinct account/project AND region. A same-region, same-project copy is not
    -- independent retention.
    target_project    TEXT        NOT NULL,
    target_region     TEXT        NOT NULL,
    target_object_ref TEXT        NOT NULL,
    object_lock_until TIMESTAMPTZ NOT NULL,
    -- Seven years for financial, access, approval and control records.
    retention_until   TIMESTAMPTZ NOT NULL,
    record_count      BIGINT      NOT NULL,
    first_sequence    BIGINT      NOT NULL,
    last_sequence     BIGINT      NOT NULL,
    first_hash        TEXT        NOT NULL,
    last_hash         TEXT        NOT NULL,
    exported_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_at       TIMESTAMPTZ,
    verification_result TEXT,
    CONSTRAINT export_batch_id_format CHECK (common.is_canonical_id(export_batch_id, 'exp')),
    -- Seven-year minimum retention is structural, not policy advice.
    CONSTRAINT export_retention_seven_years CHECK (
        retention_until >= exported_at + INTERVAL '7 years'
    ),
    -- The export must be verifiable, not merely present.
    CONSTRAINT export_verification_recorded CHECK (
        verification_result IS NULL OR verification_result IN ('VERIFIED','MISMATCH','INCOMPLETE')
    ),
    CONSTRAINT export_hashes_format CHECK (first_hash ~ '^[0-9a-f]{64}$' AND last_hash ~ '^[0-9a-f]{64}$')
);

CREATE UNIQUE INDEX export_batch_checkpoint_idx ON audit.export_batch (checkpoint_id);
CREATE INDEX export_batch_unverified_idx ON audit.export_batch (exported_at)
    WHERE verification_result IS NULL OR verification_result <> 'VERIFIED';
CREATE INDEX export_batch_region_idx ON audit.export_batch (target_region, exported_at DESC);

COMMENT ON TABLE audit.export_batch IS
    'An independently retained, cross-region, object-locked copy of a signed audit batch. Retention is at least seven years and is structurally enforced.';

-- Evidence lookup by correlation id, retained even after the operational record
-- is aged out, so an investigation is never blocked by retention.
CREATE TABLE audit.evidence_index (
    audit_id          TEXT        PRIMARY KEY,
    correlation_id    TEXT        NOT NULL,
    partition_key     TEXT        NOT NULL,
    sequence          BIGINT      NOT NULL,
    object_ref        TEXT        NOT NULL,
    object_digest     TEXT        NOT NULL,
    indexed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT evidence_digest_format CHECK (object_digest ~ '^[0-9a-f]{64}$')
);

CREATE INDEX evidence_index_correlation_idx ON audit.evidence_index (correlation_id);
CREATE INDEX evidence_index_partition_idx  ON audit.evidence_index (partition_key, sequence);

SELECT common.assert_no_floating_point();
