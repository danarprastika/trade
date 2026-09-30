-- =============================================================================
-- 0012_checkpoint_key_binding.sql
-- G3/G9 - A checkpoint must be attributable to a real, currently-legitimate
-- signing key, and the signed body is defined exactly.
--
-- Authority: 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3 and §6 (acceptance tests):
--
--   "Every batch is closed with a signed checkpoint containing partition,
--    sequence range, first/last hash, count, timestamp, and signing key ID.
--    Checkpoints are signed by a managed KMS/HSM key."
--
--   "Acceptance tests must demonstrate tamper detection, deletion denial,
--    signature verification, duplicate delivery idempotency, checkpoint
--    recovery, key rotation, ..."
--
-- The gap this closes
-- ------------------
-- audit.signing_key exists, with a status machine (ACTIVE, RETIRING, RETIRED,
-- COMPROMISED), an algorithm, and a rotation deadline. Nothing referenced it.
-- audit.checkpoint.signing_key_id is TEXT NOT NULL with no foreign key, so:
--
--   * any string could be recorded as the signing key, including one that names
--     no registered key at all;
--   * a COMPROMISED key could sign a new checkpoint;
--   * a key past its rotation deadline could keep signing;
--   * signature is BYTEA NOT NULL, and the only check is octet_length > 0, so
--     decode('00','hex') -- a single null byte -- is an acceptable signature.
--
-- That last one is what the test fixtures were using. A one-byte "signature" is
-- a faithful record of a signing process that does not exist.
--
-- WHAT THIS MIGRATION DOES NOT DO
-- -------------------------------
-- It does NOT cryptographically verify the signature bytes, and no function here
-- claims to. Verification requires the public key or the KMS, and
-- audit.signing_key deliberately stores only a provider reference -- key material
-- never appears in this database. That is the correct design and it cannot be
-- worked around here.
--
-- What is installed instead is everything the database CAN decide: the exact
-- bytes a signature covers (audit.canonical_checkpoint_body), that the named key
-- exists, that it was legitimate to sign with at that moment, and that the
-- signature is a plausible size for the algorithm. Cryptographic verification
-- against the KMS/HSM remains outstanding, and the function that would do it is
-- named audit.verify_checkpoint_signature_crypto in the comment below so its
-- absence is greppable rather than merely absent.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- The signed body.
--
-- Flat JSON, fixed member order, no insignificant whitespace, for the same
-- reason as audit.canonical_payload: a form with no nested objects cannot have
-- its encoding depend on a serialiser's key ordering.
--
-- It includes first_hash, last_hash and expected_next_hash, so a signature
-- cannot be transplanted from one sealed range onto another. It deliberately
-- includes signing_key_id and algorithm, so a signature is bound to the key that
-- produced it.
--
-- The Go implementation is contracts.CanonicalCheckpointBody; the two are
-- compared byte-for-byte by dbtest/audit_checkpoint_body_test.go.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit.canonical_checkpoint_body(
    p_checkpoint_id    TEXT,
    p_partition_key    TEXT,
    p_first_sequence   BIGINT,
    p_last_sequence    BIGINT,
    p_record_count     BIGINT,
    p_first_hash       TEXT,
    p_last_hash        TEXT,
    p_expected_next_hash TEXT,
    p_signed_at_ns     BIGINT,
    p_signing_key_id   TEXT,
    p_algorithm        TEXT,
    p_canonical_schema_version TEXT
)
RETURNS TEXT
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT concat(
        '{',
        '"checkpoint_id":',   to_json(p_checkpoint_id),
        ',"partition_key":',   to_json(p_partition_key),
        ',"first_sequence":',  p_first_sequence::text,
        ',"last_sequence":',   p_last_sequence::text,
        ',"record_count":',    p_record_count::text,
        ',"first_hash":',      to_json(p_first_hash),
        ',"last_hash":',       to_json(p_last_hash),
        ',"expected_next_hash":',
            CASE WHEN p_expected_next_hash IS NULL THEN 'null' ELSE to_json(p_expected_next_hash) END,
        ',"signed_at_ns":',    p_signed_at_ns::text,
        ',"signing_key_id":',  to_json(p_signing_key_id),
        ',"algorithm":',       to_json(p_algorithm),
        ',"canonical_schema_version":', to_json(p_canonical_schema_version),
        '}'
    );
$$;

COMMENT ON FUNCTION audit.canonical_checkpoint_body IS
    'The exact bytes a checkpoint signature covers. Flat, fixed member order, and bound to the sealed range (first/last/next hash) and to the signing key. Go: contracts.CanonicalCheckpointBody.';

-- Minimum plausible signature size, in bytes, for every algorithm the registry
-- permits. Ed25519 and raw P-256 are 64 bytes; DER-encoded P-256 is 70-72 and
-- RSA-PSS is at least 256 for the key sizes in use. A single null byte is not a
-- signature, whatever the algorithm claims to be.
--
-- This is a floor, not a check. It rejects placeholders; it cannot prove the
-- bytes are a signature. Only the KMS can do that.
CREATE OR REPLACE FUNCTION audit.minimum_signature_bytes(p_algorithm TEXT)
RETURNS INT
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT CASE p_algorithm
        WHEN 'ECDSA_P256_SHA256' THEN 64
        WHEN 'ED25519'            THEN 64
        WHEN 'RSA_PSS_SHA256'     THEN 64
        ELSE 64
    END;
$$;

COMMENT ON FUNCTION audit.minimum_signature_bytes IS
    'A floor on signature size, sized to reject placeholders. It is NOT a proof that the bytes are a signature; cryptographic verification is against the KMS/HSM.';

-- -----------------------------------------------------------------------------
-- Key binding.
--
-- Enforced on INSERT, so a checkpoint cannot be sealed with a key that was not
-- legitimate to sign with at the moment of sealing.
--
-- The check is against the key's status AT SIGNING TIME, not its status now.
-- A key that was ACTIVE when it signed a batch stays a valid signer for that
-- batch after it is rotated out; requiring the current status would invalidate
-- history on every rotation, which is the opposite of what "overlapping
-- verification keys during planned rotation" in the signing_key comment means.
-- A key that is COMPROMISED *now* is a retroactive alarm, and is reported by
-- audit.verify_checkpoint_key_binding rather than silently accepted.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit.guard_checkpoint_key_binding()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog, audit, common, public
AS $$
DECLARE
    v_algorithm   TEXT;
    v_status      TEXT;
    v_rotate_after TIMESTAMPTZ;
    v_retired_at  TIMESTAMPTZ;
    v_min_bytes   INT;
BEGIN
    SELECT algorithm, status, rotate_after, retired_at
      INTO v_algorithm, v_status, v_rotate_after, v_retired_at
      FROM audit.signing_key
     WHERE signing_key_id = NEW.signing_key_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION
            'checkpoint % names signing key %, which is not in the audit.signing_key registry. A checkpoint must be attributable to a registered key.',
            NEW.checkpoint_id, NEW.signing_key_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    IF v_status NOT IN ('ACTIVE', 'RETIRING') THEN
        RAISE EXCEPTION
            'checkpoint % is sealed with key %, whose status is %. Only ACTIVE or RETIRING keys may sign a new checkpoint.',
            NEW.checkpoint_id, NEW.signing_key_id, v_status
            USING ERRCODE = 'check_violation';
    END IF;

    IF v_retired_at IS NOT NULL THEN
        RAISE EXCEPTION
            'checkpoint % is sealed with key %, which was retired at %.',
            NEW.checkpoint_id, NEW.signing_key_id, v_retired_at
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.signed_at > v_rotate_after THEN
        RAISE EXCEPTION
            'checkpoint % is signed at %, but key % passed its rotation deadline of %. An overdue key may not sign new evidence.',
            NEW.checkpoint_id, NEW.signed_at, NEW.signing_key_id, v_rotate_after
            USING ERRCODE = 'check_violation';
    END IF;

    v_min_bytes := audit.minimum_signature_bytes(v_algorithm);

    IF octet_length(NEW.signature) < v_min_bytes THEN
        RAISE EXCEPTION
            'checkpoint % carries a %d-byte signature, below the %d-byte minimum for %s. A truncated or placeholder value is not a signature.',
            NEW.checkpoint_id, octet_length(NEW.signature), v_min_bytes, v_algorithm
            USING ERRCODE = 'check_violation';
    END IF;

    -- The checkpoint's own key_status column is a denormalised copy taken at
    -- signing time. It must not contradict the registry for that moment.
    IF NEW.key_status <> v_status THEN
        RAISE EXCEPTION
            'checkpoint % records key_status % but the registry says %. The two must agree as of signing.',
            NEW.checkpoint_id, NEW.key_status, v_status
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION audit.guard_checkpoint_key_binding IS
    'BEFORE INSERT on audit.checkpoint. Requires a registered signing key, a status legitimate at signing time (ACTIVE or RETIRING), a rotation deadline not yet passed, a signature of plausible size, and agreement between the checkpoint key_status and the registry. Cryptographic verification of the signature bytes is NOT performed here and cannot be: key material does not exist in this database.';

CREATE TRIGGER checkpoint_key_binding_guard
    BEFORE INSERT ON audit.checkpoint
    FOR EACH ROW EXECUTE FUNCTION audit.guard_checkpoint_key_binding();

-- -----------------------------------------------------------------------------
-- Key binding verification.
--
-- Named for what it checks. It verifies ATTRIBUTION and PLAUSIBILITY, not
-- cryptography, and the function name is deliberately not
-- "verify_signature" so that no reader, and no report citing this file, can
-- mistake it for the KMS-backed check that 22_..._EVIDENCE.md §6 requires.
--
-- The retrospective check matters most: a key marked COMPROMISED today must
-- surface every batch it signed, because those batches are exactly the ones
-- whose authenticity is now in doubt.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit.verify_checkpoint_key_binding(p_partition_key TEXT)
RETURNS TABLE (
    checkpoint_id   TEXT,
    signing_key_id  TEXT,
    key_registered  BOOLEAN,
    key_status_now  TEXT,
    signature_bytes INT,
    size_plausible  BOOLEAN,
    key_compromised BOOLEAN,
    ok              BOOLEAN
)
LANGUAGE sql
STABLE
AS $$
    SELECT c.checkpoint_id,
           c.signing_key_id,
           k.signing_key_id IS NOT NULL,
           COALESCE(k.status, 'UNKNOWN'),
           octet_length(c.signature),
           octet_length(c.signature) >= audit.minimum_signature_bytes(COALESCE(k.algorithm, 'ECDSA_P256_SHA256')),
           COALESCE(k.status, '') = 'COMPROMISED',
           k.signing_key_id IS NOT NULL
           AND octet_length(c.signature) >= audit.minimum_signature_bytes(COALESCE(k.algorithm, 'ECDSA_P256_SHA256'))
           AND COALESCE(k.status, '') NOT IN ('COMPROMISED', 'UNKNOWN')
      FROM audit.checkpoint c
      LEFT JOIN audit.signing_key k ON k.signing_key_id = c.signing_key_id
     WHERE c.partition_key = p_partition_key
     ORDER BY c.first_sequence;
$$;

COMMENT ON FUNCTION audit.verify_checkpoint_key_binding IS
    'Verifies that each checkpoint names a registered key whose signature is of plausible size, and flags any checkpoint signed by a key now marked COMPROMISED. This is NOT cryptographic signature verification; the pending KMS/HSM-backed function is audit.verify_checkpoint_signature_crypto (see 0012 header), which does not exist.';

SELECT common.assert_no_floating_point();
