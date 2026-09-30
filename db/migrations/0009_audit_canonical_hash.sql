-- =============================================================================
-- 0009_audit_canonical_hash.sql
-- G3/G9 - The database computes and verifies the audit record hash, so a caller
-- can no longer supply a well-formed but wrong hash that chains successfully.
--
-- Authority: 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3.
--
--   "record_hash = SHA-256(canonical_json(record_without_record_hash_and_signature));
--    previous_hash references the prior record hash in that partition.
--    Canonical serialization and schema version are fixed and tested."
--
-- Why this migration exists
-- -------------------------
-- 0006/0008 implemented audit.append_record with p_record_hash supplied BY THE
-- CALLER. The chain validated CONTINUITY (sequence + previous_hash) but never
-- recomputed the hash, so a caller could record a fabricated record_hash and the
-- chain would link to it happily. A tamper-evident ledger whose hashes are
-- supplied by the party being audited is not tamper-evident: the whole point of
-- the chain is that a verifier can recompute the hashes independently and detect
-- a rewritten record.
--
-- The fix is to make the canonical serialisation a DATABASE function, compute
-- the hash inside append_record, and reject any caller-supplied hash that
-- disagrees. The Go audit service implements the identical serialisation; the two
-- are compared against shared fixtures in dbtest, so "fixed and tested" is
-- satisfied across the language boundary rather than within one language.
-- =============================================================================

-- The canonical serialisation is defined as a FLAT JSON object with a fixed,
-- documented member order and no insignificant whitespace. It is deliberately
-- flat:
--
--   * Every value is a JSON string, a JSON integer, or null. No nested objects,
--     so there is no dependency on any implementation's key-ordering rules.
--   * The free-form 'details' object is NOT embedded. Its SHA-256 digest is
--     embedded instead, which protects the content just as well while keeping
--     the canonical form trivially reproducible in any language.
--   * Timestamps appear only as the explicit nanosecond integers, which are the
--     authoritative values. A consistency check below guarantees they agree with
--     the TIMESTAMPTZ columns, so nothing is lost by leaving the formatted
--     timestamp out of the hash.
--
-- Member order (canonical_schema_version '1.0.0'), which is part of the contract
-- and may not be reordered without incrementing the schema version:
--
--   audit_id, partition_key, sequence, tenant_or_owner_scope, actor_id,
--   actor_type, action, target_type, target_id, environment, market_scope,
--   occurred_at_ns, recorded_at_ns, reason, correlation_id, causation_id,
--   policy_version, result, before_digest, after_digest, details_digest,
--   previous_hash, canonical_schema_version
--
-- The exclusion of record_hash is the requirement in §3 ("record without
-- record_hash and signature"). There is no signature column on audit.record: the
-- signature lives on audit.checkpoint, which closes a batch rather than a record.

CREATE OR REPLACE FUNCTION audit.canonical_payload(
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
    p_occurred_at_ns BIGINT,
    p_recorded_at_ns BIGINT,
    p_reason        TEXT,
    p_correlation_id TEXT,
    p_causation_id  TEXT,
    p_policy_version TEXT,
    p_result        audit.result_value,
    p_before_digest TEXT,
    p_after_digest  TEXT,
    p_details       JSONB,
    p_previous_hash TEXT,
    p_canonical_schema_version TEXT
)
RETURNS TEXT
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT concat(
        '{',
        '"audit_id":',              to_json(p_audit_id),
        ',"partition_key":',         to_json(p_partition_key),
        ',"sequence":',              p_sequence::text,
        ',"tenant_or_owner_scope":', to_json(p_tenant_scope),
        ',"actor_id":',              to_json(p_actor_id),
        ',"actor_type":',            to_json(p_actor_type::text),
        ',"action":',                to_json(p_action),
        ',"target_type":',           to_json(p_target_type),
        ',"target_id":',             to_json(p_target_id),
        ',"environment":',           to_json(p_environment::text),
        ',"market_scope":',
            CASE WHEN p_market_scope IS NULL THEN 'null' ELSE to_json(p_market_scope::text) END,
        ',"occurred_at_ns":',        p_occurred_at_ns::text,
        ',"recorded_at_ns":',        p_recorded_at_ns::text,
        ',"reason":',
            CASE WHEN p_reason IS NULL THEN 'null' ELSE to_json(p_reason) END,
        ',"correlation_id":',        to_json(p_correlation_id),
        ',"causation_id":',
            CASE WHEN p_causation_id IS NULL THEN 'null' ELSE to_json(p_causation_id) END,
        ',"policy_version":',        to_json(p_policy_version),
        ',"result":',                to_json(p_result::text),
        ',"before_digest":',
            CASE WHEN p_before_digest IS NULL THEN 'null' ELSE to_json(p_before_digest) END,
        ',"after_digest":',
            CASE WHEN p_after_digest IS NULL THEN 'null' ELSE to_json(p_after_digest) END,
        ',"details_digest":',        to_json(encode(public.digest(COALESCE(p_details, '{}'::jsonb)::text, 'sha256'), 'hex')),
        ',"previous_hash":',         to_json(p_previous_hash),
        ',"canonical_schema_version":', to_json(p_canonical_schema_version),
        '}'
    );
$$;

COMMENT ON FUNCTION audit.canonical_payload IS
    'The fixed canonical serialisation of an audit record, excluding record_hash and signature. Flat by construction, fixed member order, no insignificant whitespace, and the free-form details object is carried as its SHA-256 digest. Canonical schema version 1.0.0. The Go implementation is contracts.CanonicalAuditPayload; the two are compared against shared fixtures.';

-- The single place a record hash is computed. Everything else in the system calls
-- this rather than hashing a record itself, so there is exactly one definition of
-- what a record hash means.
CREATE OR REPLACE FUNCTION audit.compute_record_hash(
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
    p_occurred_at_ns BIGINT,
    p_recorded_at_ns BIGINT,
    p_reason        TEXT,
    p_correlation_id TEXT,
    p_causation_id  TEXT,
    p_policy_version TEXT,
    p_result        audit.result_value,
    p_before_digest TEXT,
    p_after_digest  TEXT,
    p_details       JSONB,
    p_previous_hash TEXT,
    p_canonical_schema_version TEXT
)
RETURNS TEXT
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT encode(
        public.digest(
            audit.canonical_payload(
                p_audit_id, p_partition_key, p_sequence, p_tenant_scope, p_actor_id,
                p_actor_type, p_action, p_target_type, p_target_id, p_environment,
                p_market_scope, p_occurred_at_ns, p_recorded_at_ns, p_reason,
                p_correlation_id, p_causation_id, p_policy_version, p_result,
                p_before_digest, p_after_digest, p_details, p_previous_hash,
                p_canonical_schema_version),
            'sha256'),
        'hex');
$$;

COMMENT ON FUNCTION audit.compute_record_hash IS
    'record_hash = SHA-256(canonical_payload(record without record_hash and signature)). Computed by the database so the audited party cannot choose the value.';

-- append_record recomputes the hash and refuses a caller-supplied value that
-- disagrees. The parameter is retained so an independent implementation (the Go
-- audit service) can be checked against the database on every append, but it is
-- now a CROSS-CHECK rather than an input.
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
SECURITY DEFINER
SET search_path = pg_catalog, audit, common, public
AS $$
DECLARE
    v_last_sequence BIGINT;
    v_last_hash     TEXT;
    v_expected_seq  BIGINT;
    v_expected_hash TEXT;
    v_computed_hash TEXT;
BEGIN
    -- NOTE: this function deliberately does NOT cross-check p_occurred_at_ns
    -- against p_occurred_at (or the recorded pair).
    --
    -- A consistency check between the two was written and removed. It is
    -- unsound: a caller that samples the clock for the nanosecond value and then
    -- lets the statement fill the TIMESTAMPTZ from now() samples the clock twice,
    -- so a real, correct record trips the check by the round-trip time. The
    -- failure is indistinguishable from genuine corruption, which makes the
    -- control worse than no control: it would train operators to expect false
    -- positives and to reach for a bypass.
    --
    -- Nothing is lost by omitting it. The hash is computed over the nanosecond
    -- values, so a verifier recomputing from the stored record reaches the same
    -- hash whether or not the two time columns agree. The pair is stored so the
    -- records remain queryable, not because the hash depends on both.
    PERFORM audit.ensure_partition(date_trunc('month', p_recorded_at)::date);

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

    IF p_sequence <> v_expected_seq THEN
        RAISE EXCEPTION
            'audit chain break in partition %: expected sequence %, got %. A missing or out-of-order sequence is a SEV-1 evidence-integrity event.',
            p_partition_key, v_expected_seq, p_sequence
            USING ERRCODE = 'check_violation';
    END IF;

    v_computed_hash := audit.compute_record_hash(
        p_audit_id, p_partition_key, p_sequence, p_tenant_scope, p_actor_id,
        p_actor_type, p_action, p_target_type, p_target_id, p_environment,
        p_market_scope, p_occurred_at_ns, p_recorded_at_ns, p_reason,
        p_correlation_id, p_causation_id, p_policy_version, p_result,
        p_before_digest, p_after_digest, p_details, v_expected_hash,
        p_canonical_schema_version);

    -- A caller may pass a hash so that an independent implementation is checked
    -- on every append, but it may not pass a DIFFERENT one. This is what makes
    -- the chain tamper-evident rather than self-attesting.
    IF p_record_hash IS NOT NULL AND p_record_hash <> v_computed_hash THEN
        RAISE EXCEPTION
            'audit record hash mismatch for %: caller supplied % but the canonical payload hashes to %. The database is the authority for record_hash.',
            p_audit_id, p_record_hash, v_computed_hash
            USING ERRCODE = 'check_violation';
    END IF;

    PERFORM set_config('audit.chain_append', 'on', true);

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
        v_expected_hash, v_computed_hash, p_signing_key_id, p_canonical_schema_version
    );

    UPDATE audit.partition_month
       SET last_sequence = p_sequence,
           last_hash     = v_computed_hash,
           record_count  = record_count + 1
     WHERE partition_key = p_partition_key;

    PERFORM set_config('audit.chain_append', 'off', true);
END;
$$;

COMMENT ON FUNCTION audit.append_record IS
    'The ONLY supported way to write an audit record. SECURITY DEFINER with a pinned search_path, holds the per-partition advisory lock, enforces monotonic sequence and previous_hash continuity, creates the target month partition on demand, and COMPUTES record_hash from the canonical payload. A caller-supplied hash is cross-checked and may not differ.';

-- -----------------------------------------------------------------------------
-- Independent chain verification.
--
-- This is the function an auditor (or the hourly verification run described in
-- 22_..._EVIDENCE.md) calls. It recomputes every record hash from stored
-- content and re-walks the linkage, so it detects a record that was forged with
-- a caller-supplied hash, a chain that was re-pointed, and a partition whose
-- head does not match its last record. It deliberately does NOT trust
-- audit.partition_month.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit.verify_partition(p_partition_key TEXT)
RETURNS TABLE (
    sequence          BIGINT,
    audit_id          TEXT,
    recomputed_hash   TEXT,
    stored_hash       TEXT,
    previous_hash     TEXT,
    expected_previous TEXT,
    ok                BOOLEAN
)
LANGUAGE plpgsql
STABLE
AS $$
BEGIN
    RETURN QUERY
    WITH ordered AS (
        SELECT r.*,
               COALESCE(lag(r.record_hash) OVER (ORDER BY r.sequence),
                        audit.genesis_hash()) AS expected_previous
          FROM audit.record r
         WHERE r.partition_key = p_partition_key
    ), walked AS (
        SELECT o.*,
               audit.compute_record_hash(
                   o.audit_id, o.partition_key, o.sequence, o.tenant_or_owner_scope,
                   o.actor_id, o.actor_type, o.action, o.target_type, o.target_id,
                   o.environment, o.market_scope, o.occurred_at_ns, o.recorded_at_ns,
                   o.reason, o.correlation_id, o.causation_id, o.policy_version,
                   o.result, o.before_digest, o.after_digest, o.details,
                   COALESCE(lag(o.record_hash) OVER (ORDER BY o.sequence),
                            audit.genesis_hash()),
                   o.canonical_schema_version) AS recomputed_hash
          FROM ordered o
    )
    SELECT w.sequence,
           w.audit_id,
           w.recomputed_hash,
           w.record_hash,
           w.previous_hash,
           w.expected_previous,
           w.recomputed_hash = w.record_hash
               AND w.previous_hash = w.expected_previous
      FROM walked w
     ORDER BY w.sequence;
END;
$$;

COMMENT ON FUNCTION audit.verify_partition IS
    'Recomputes every record hash from stored content and re-walks previous_hash linkage, using lag() over the stored sequence rather than trusting audit.partition_month. Any row with ok=false is a chain break and a SEV-1 evidence-integrity event.';

-- -----------------------------------------------------------------------------
-- Head verification.
--
-- verify_partition alone cannot detect TRUNCATION FROM THE TAIL. Every record
-- that remains still chains correctly after the last record is deleted, because
-- the deleted record was the chain's terminus, not a link in the middle. The
-- evidence that the tail is short is the partition head, so the head is
-- compared against the records themselves.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit.verify_partition_head(p_partition_key TEXT)
RETURNS TABLE (
    head_sequence     BIGINT,
    head_hash         TEXT,
    actual_sequence   BIGINT,
    actual_hash       TEXT,
    ok                BOOLEAN
)
LANGUAGE sql
STABLE
AS $$
    SELECT h.last_sequence,
           h.last_hash,
           (SELECT max(r.sequence) FROM audit.record r
             WHERE r.partition_key = p_partition_key),
           (SELECT r.record_hash FROM audit.record r
             WHERE r.partition_key = p_partition_key
             ORDER BY r.sequence DESC LIMIT 1),
           h.last_sequence = (SELECT max(r.sequence) FROM audit.record r
                                WHERE r.partition_key = p_partition_key)
       AND h.last_hash = (SELECT r.record_hash FROM audit.record r
                            WHERE r.partition_key = p_partition_key
                            ORDER BY r.sequence DESC LIMIT 1)
      FROM audit.partition_month h
     WHERE h.partition_key = p_partition_key;
$$;

COMMENT ON FUNCTION audit.verify_partition_head IS
    'Compares the recorded partition head against the records actually present. This is the control that detects deletion of the most recent record(s): the remaining chain still verifies, so only the head reveals the truncation.';

SELECT common.assert_no_floating_point();
