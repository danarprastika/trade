-- =============================================================================
-- 0010_checkpoint_hash_binding.sql
-- G3/G9 - A checkpoint may only attest to records that actually exist and hash
-- to the values it claims.
--
-- Authority: 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3 and the audit.checkpoint
-- table comment in 0006_audit_integrity.sql:
--
--   -- Expected value of the chain head after the batch, so a verifier can
--   -- detect a TRUNCATED batch (a prefix removal that preserves internal
--   -- linkage).
--
-- The gap this closes
-- ------------------
-- audit.checkpoint enforced its own internal consistency and nothing else:
--
--   checkpoint_count_matches  CHECK (record_count = last_sequence - first_sequence + 1)
--   checkpoint_hashes_format  CHECK (first_hash ~ '^[0-9a-f]{64}$' AND ...)
--
-- Both are satisfied by any well-formed arithmetic and any 64 hex characters.
-- Neither looks at audit.record. A checkpoint could therefore be signed over a
-- range of sequences that was never written, or over real sequences while
-- naming hashes that belong to no record at all.
--
-- That defeats the purpose of the control. The checkpoint is the periodic,
-- signed commitment to a batch of evidence; it is what an auditor verifies
-- against, and it is what makes a sealed batch tamper-evident once the
-- application's own records have been tampered with. If the checkpoint's hashes
-- are not bound to the records, an operator who can reach both tables can
-- rewrite evidence AND re-attest to the rewritten evidence with a valid-looking
-- checkpoint. The chain would verify, the checkpoint would verify, and the
-- forgery would be undetectable without an independent copy of the signed
-- batch -- which is the only control left, and the only one that requires
-- external storage.
--
-- The binding is enforced by a BEFORE INSERT trigger rather than by a function
-- the application must remember to call. That is deliberate and matches
-- audit.reject_unauthorised_insert: the failure mode being closed is a caller
-- reaching the table directly, so the control has to sit on the table itself.
-- =============================================================================

CREATE OR REPLACE FUNCTION audit.guard_checkpoint_binding()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog, audit, common, public
AS $$
DECLARE
    v_actual_count  BIGINT;
    v_first_actual  TEXT;
    v_last_actual   TEXT;
    v_next_actual   TEXT;
    v_is_head       BOOLEAN;
BEGIN
    -- The records the checkpoint claims to close. FOR SHARE serialises against a
    -- concurrent append, and audit.record is append-only in any case, so these
    -- values cannot change while this statement runs.
    SELECT record_hash INTO v_first_actual
      FROM audit.record
     WHERE partition_key = NEW.partition_key AND sequence = NEW.first_sequence;

    SELECT record_hash INTO v_last_actual
      FROM audit.record
     WHERE partition_key = NEW.partition_key AND sequence = NEW.last_sequence;

    IF v_first_actual IS NULL THEN
        RAISE EXCEPTION
            'checkpoint % claims first_sequence % in partition %, but no such record exists. A checkpoint may not attest to evidence that was never written.',
            NEW.checkpoint_id, NEW.first_sequence, NEW.partition_key
            USING ERRCODE = 'check_violation';
    END IF;

    IF v_last_actual IS NULL THEN
        RAISE EXCEPTION
            'checkpoint % claims last_sequence % in partition %, but no such record exists. A checkpoint may not attest to evidence that was never written.',
            NEW.checkpoint_id, NEW.last_sequence, NEW.partition_key
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.first_hash <> v_first_actual THEN
        RAISE EXCEPTION
            'checkpoint % first_hash % does not match the record_hash % of sequence %. The checkpoint is not bound to the records it attests to.',
            NEW.checkpoint_id, NEW.first_hash, v_first_actual, NEW.first_sequence
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.last_hash <> v_last_actual THEN
        RAISE EXCEPTION
            'checkpoint % last_hash % does not match the record_hash % of sequence %. The checkpoint is not bound to the records it attests to.',
            NEW.checkpoint_id, NEW.last_hash, v_last_actual, NEW.last_sequence
            USING ERRCODE = 'check_violation';
    END IF;

    -- record_count must describe records that are PRESENT, not merely a range
    -- that exists arithmetically. A gap inside the sealed range would otherwise
    -- be counted as sealed evidence.
    SELECT count(*) INTO v_actual_count
      FROM audit.record
     WHERE partition_key = NEW.partition_key
       AND sequence BETWEEN NEW.first_sequence AND NEW.last_sequence;

    IF v_actual_count <> NEW.record_count THEN
        RAISE EXCEPTION
            'checkpoint % claims % record(s) in sequences %..%, but % are present. A sealed range must contain no gaps.',
            NEW.checkpoint_id, NEW.record_count, NEW.first_sequence,
            NEW.last_sequence, v_actual_count
            USING ERRCODE = 'check_violation';
    END IF;

    -- expected_next_hash is what detects a truncated batch: if the batch was
    -- sealed at a point that is not the current head, the successor's hash must
    -- be named, so removing the records that followed is detectable even though
    -- the sealed prefix still chains correctly.
    v_is_head := NEW.last_sequence = (
        SELECT last_sequence FROM audit.partition_month
         WHERE partition_key = NEW.partition_key);

    IF v_is_head THEN
        -- Sealing at the head: the next hash is by definition unknown, and
        -- supplying the head hash here would be a category error.
        IF NEW.expected_next_hash IS NOT NULL THEN
            RAISE EXCEPTION
                'checkpoint % seals the current head (sequence %), so expected_next_hash must be NULL; it is a commitment to a successor that does not exist yet.',
                NEW.checkpoint_id, NEW.last_sequence
                USING ERRCODE = 'check_violation';
        END IF;
    ELSE
        SELECT record_hash INTO v_next_actual
          FROM audit.record
         WHERE partition_key = NEW.partition_key
           AND sequence = NEW.last_sequence + 1;

        -- NULL must be tested explicitly, NOT by comparing it. In SQL,
        -- `NULL <> 'abc'` evaluates to NULL, not to TRUE, so an IF on that
        -- comparison does not fire. An earlier version of this function tested
        -- `v_next_actual IS NULL` and then compared, which meant a checkpoint
        -- that named NO successor was accepted: the comparison silently
        -- evaluated to NULL and the branch was skipped. The omission of
        -- expected_next_hash is precisely the truncated-batch case this is
        -- meant to close, so the test below now rejects it directly.
        IF NEW.expected_next_hash IS NULL THEN
            RAISE EXCEPTION
                'checkpoint % seals sequence % in partition % but names no expected_next_hash. Without it a truncated batch is indistinguishable from a complete one.',
                NEW.checkpoint_id, NEW.last_sequence, NEW.partition_key
                USING ERRCODE = 'check_violation';
        END IF;

        IF v_next_actual IS NULL THEN
            RAISE EXCEPTION
                'checkpoint % claims sequence % is not the head of partition %, but no record exists at sequence %. The partition head and the records disagree.',
                NEW.checkpoint_id, NEW.last_sequence, NEW.partition_key,
                NEW.last_sequence + 1
                USING ERRCODE = 'check_violation';
        END IF;

        IF NEW.expected_next_hash <> v_next_actual THEN
            RAISE EXCEPTION
                'checkpoint % expected_next_hash % does not match the record_hash % of sequence %. A truncated batch would not be detectable.',
                NEW.checkpoint_id, NEW.expected_next_hash, v_next_actual,
                NEW.last_sequence + 1
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION audit.guard_checkpoint_binding IS
    'BEFORE INSERT on audit.checkpoint. Requires first_hash/last_hash to equal the record_hash of the records at first_sequence/last_sequence, requires every sequence in the sealed range to be present, and binds expected_next_hash to the successor record. Without this a signed checkpoint can attest to records that do not exist or to hashes that belong to no record, which is the forgery path the chain exists to close.';

CREATE TRIGGER checkpoint_binding_guard
    BEFORE INSERT ON audit.checkpoint
    FOR EACH ROW EXECUTE FUNCTION audit.guard_checkpoint_binding();

-- -----------------------------------------------------------------------------
-- Checkpoint verification, for the periodic verification run.
--
-- audit.verify_partition proves the records are internally consistent.
-- audit.verify_partition_head proves nothing has been removed from the tail.
-- This proves the SEALED CLAIMS are still true.
--
-- It RECOMPUTES each record's hash from its stored content rather than reading
-- the stored record_hash. That distinction is the whole point of the function.
-- A verifier that compares a checkpoint against the record's own stored hash is
-- tautological: rewriting a record's content does not change its stored
-- record_hash column, so such a verifier would report a checkpoint as valid
-- after the evidence it attests to had been rewritten. Recomputing is what
-- makes the signed checkpoint an INDEPENDENT commitment -- a claim about the
-- evidence that survives the evidence table being edited underneath it.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit.verify_checkpoints(p_partition_key TEXT)
RETURNS TABLE (
    checkpoint_id    TEXT,
    first_sequence   BIGINT,
    last_sequence    BIGINT,
    first_hash_ok    BOOLEAN,
    last_hash_ok     BOOLEAN,
    records_present  BOOLEAN,
    ok               BOOLEAN
)
LANGUAGE sql
STABLE
AS $$
    WITH ordered AS (
        SELECT r.*,
               COALESCE(lag(r.record_hash) OVER (ORDER BY r.sequence),
                        audit.genesis_hash()) AS expected_previous
          FROM audit.record r
         WHERE r.partition_key = p_partition_key
    ), recomputed AS (
        SELECT o.sequence,
               audit.compute_record_hash(
                   o.audit_id, o.partition_key, o.sequence, o.tenant_or_owner_scope,
                   o.actor_id, o.actor_type, o.action, o.target_type, o.target_id,
                   o.environment, o.market_scope, o.occurred_at_ns, o.recorded_at_ns,
                   o.reason, o.correlation_id, o.causation_id, o.policy_version,
                   o.result, o.before_digest, o.after_digest, o.details,
                   o.expected_previous, o.canonical_schema_version) AS content_hash
          FROM ordered o
    )
    SELECT c.checkpoint_id,
           c.first_sequence,
           c.last_sequence,
           c.first_hash = (SELECT x.content_hash FROM recomputed x
                            WHERE x.sequence = c.first_sequence),
           c.last_hash = (SELECT x.content_hash FROM recomputed x
                           WHERE x.sequence = c.last_sequence),
           (SELECT count(*) FROM recomputed x
             WHERE x.sequence BETWEEN c.first_sequence AND c.last_sequence) = c.record_count,
           c.first_hash = (SELECT x.content_hash FROM recomputed x
                            WHERE x.sequence = c.first_sequence)
           AND c.last_hash = (SELECT x.content_hash FROM recomputed x
                               WHERE x.sequence = c.last_sequence)
           AND (SELECT count(*) FROM recomputed x
                 WHERE x.sequence BETWEEN c.first_sequence AND c.last_sequence) = c.record_count
      FROM audit.checkpoint c
     WHERE c.partition_key = p_partition_key
     ORDER BY c.first_sequence;
$$;

COMMENT ON FUNCTION audit.verify_checkpoints IS
    'Re-checks every signed checkpoint against the records it names. A row with ok=false means a sealed claim no longer holds: evidence was rewritten, removed, or the checkpoint was never bound to its records in the first place.';

SELECT common.assert_no_floating_point();
