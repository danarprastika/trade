-- =============================================================================
-- 0011_audit_details_digest.sql
-- G3/G9 - Name the details digest, and define it once.
--
-- 0009 inlined `encode(public.digest(COALESCE(details,'{}')::text,'sha256'),'hex')`
-- in three places: canonical_payload, and implicitly in verify_partition and
-- verify_checkpoints via compute_record_hash. Three copies of a hash
-- definition is three chances to spell it differently, so the expression is
-- given a name.
--
-- The Go audit service needs the same digest, and it must not re-derive it by
-- reimplementing PostgreSQL's jsonb text rendering. jsonb output is
-- deterministic but its rules -- keys ordered by length then bytewise, a space
-- after every ':' and ',', PostgreSQL's numeric formatting -- are a
-- PostgreSQL contract, not a portable one. Reimplementing it in Go would create
-- a second definition that is correct only until PostgreSQL changes a detail,
-- and a silent divergence there would surface as an unexplained hash mismatch
-- on every append.
--
-- So the digest is a database function, and the Go service asks for it. The
-- consequence is stated plainly: the cross-check covers the canonical FORM --
-- member order, encoding, NULL handling, the outer twenty members -- and the
-- one member whose canonicalisation is owned by the database is sourced from
-- the database. That is a deliberate narrowing, not an oversight, and it is
-- documented here so no reader assumes more coverage than exists.
-- =============================================================================

CREATE OR REPLACE FUNCTION audit.details_digest(p_details JSONB)
RETURNS TEXT
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT encode(
        public.digest(COALESCE(p_details, '{}'::jsonb)::text, 'sha256'),
        'hex');
$$;

COMMENT ON FUNCTION audit.details_digest IS
    'SHA-256 over the PostgreSQL jsonb text rendering of the details object. The single definition of the details_digest member of the canonical audit payload. Callers outside the database MUST use this function rather than reimplementing jsonb text rendering.';

-- canonical_payload is redefined to use the named function. The output is
-- byte-identical to the 0009 version, which the conformance test in
-- dbtest/audit_canonical_test.go verifies against contracts.CanonicalAuditPayload
-- rather than against a stored constant, so this refactor cannot silently
-- change the canonical form.
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
        ',"details_digest":',        to_json(audit.details_digest(p_details)),
        ',"previous_hash":',         to_json(p_previous_hash),
        ',"canonical_schema_version":', to_json(p_canonical_schema_version),
        '}'
    );
$$;

COMMENT ON FUNCTION audit.canonical_payload IS
    'The fixed canonical serialisation of an audit record, excluding record_hash and signature. Flat by construction, fixed member order, no insignificant whitespace, and the free-form details object is carried as its SHA-256 digest. Canonical schema version 1.0.0. The Go implementation is contracts.CanonicalAuditPayload; the two are compared byte-for-byte by dbtest/audit_canonical_test.go.';

SELECT common.assert_no_floating_point();
