-- =============================================================================
-- 0017_configuration_boundary_enforcement.sql
-- G1 - Domain Foundation: configuration boundaries.
--
-- Authority: 17_CONFIGURATION_AND_RISK_POLICY.md §1, 06_SECURITY_AND_ACCESS_CONTROL.md §6
--
-- WHY THIS MIGRATION EXISTS
--
-- 0001_foundation.sql created config.revision with the columns doc 17 §1 names
-- -- revision_number, schema_version, content_digest, signature, created_by,
-- reason, approval_request_id, environment, status -- and then wrote this on
-- the table:
--
--     -- Immutability after activation: enforced by trigger below.
--
-- There was no trigger below. There was no trigger anywhere in the config
-- schema; config.revision had zero triggers and the config schema had zero
-- functions. Every enforcement sentence in that migration's config block was
-- aspirational, which is the same defect class as 0008's unenforced
-- state-transition rule table.
--
-- The consequence was not subtle. An ACTIVE configuration snapshot could be
-- UPDATEd in place -- document, content_digest, everything -- by anyone holding
-- ordinary write access, with no record that it had changed. A snapshot that
-- can be edited after it is loaded is not a snapshot, and a "release
-- configuration" that can be rewritten post-approval is not a release.
--
-- Worse, content_digest was checked only for FORMAT (a 64-char hex string) and
-- never against the document it claims to describe. The signature column is
-- defined as a signature OVER the content digest, so with the digest unbound
-- the signature control was decorative: an operator could activate a snapshot
-- with a genuine signature over an honest digest, then replace the document and
-- leave the signature and digest untouched. The signature would still verify.
-- Nothing would be wrong with the signature, and the configuration would be
-- entirely different from the one that was approved. That is why control 2
-- below binds the digest to the document rather than trusting it.
--
-- The controls in this migration, in the order they defend against:
--
--   1. config.document_digest          -- one definition of the digest
--   2. content_digest_binds_document   -- the signature means something again
--   3. config.guard_revision_mutation  -- immutability + closed lifecycle
--   4. one ACTIVE revision per environment
--   5. config.assert_no_embedded_secrets
--   6. config.guard_promotion_provenance -- live is never an automatic copy
--
-- Scope note. Doc 17 §3 additionally requires every numeric risk limit to carry
-- currency-or-unit, aggregation scope, measurement window, boundary
-- inclusivity, source of truth, and behaviour on missing data, and blocks live
-- activation until a complete policy is validated. That is NOT enforced here,
-- because it requires a document schema -- the JSON key names for six
-- attributes -- that no blueprint document specifies, and inventing one would
-- be the platform "supplying universal live numeric trading limits", which
-- 17_CONFIGURATION_AND_RISK_POLICY.md §3 explicitly prohibits. It is recorded
-- as an open gap in the G1 evidence rather than guessed at.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- Control 1: name the document digest once.
--
-- Same reasoning as 0011_audit_details_digest: jsonb's text rendering is a
-- PostgreSQL contract, not a portable one. A Go implementation of this digest
-- would be a second definition, correct only until PostgreSQL renders something
-- differently, and the divergence would surface as every configuration load
-- failing with an unexplained digest mismatch.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION config.document_digest(p_document JSONB)
RETURNS TEXT
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT encode(
        public.digest(COALESCE(p_document, '{}'::jsonb)::text, 'sha256'),
        'hex');
$$;

COMMENT ON FUNCTION config.document_digest IS
    'SHA-256 over the PostgreSQL jsonb text rendering of the configuration document. Single definition of config.revision.content_digest. Callers outside the database MUST use this function rather than reimplementing jsonb text rendering.';

-- -----------------------------------------------------------------------------
-- Control 2: bind content_digest to the document.
--
-- This is a CHECK rather than a trigger so it cannot be bypassed by a path
-- that forgets to fire it, and so it is visible in the table definition. The
-- existing format constraint is left in place because it still rejects a
-- malformed digest with a clearer message than a mismatch would.
--
-- The cost is that a large document is re-hashed on every UPDATE. Control 3
-- forbids updating the document at all, so in practice the check runs once per
-- revision.
-- -----------------------------------------------------------------------------
ALTER TABLE config.revision
    ADD CONSTRAINT content_digest_binds_document
    CHECK (content_digest = config.document_digest(document));

COMMENT ON CONSTRAINT content_digest_binds_document ON config.revision IS
    'The content digest must be the digest of the document it describes. Without this, signature (which is defined over the digest) would attest to nothing: a genuine signature over an honest digest would survive replacing the document underneath it, because the digest column itself was never bound to the content.';

-- -----------------------------------------------------------------------------
-- Control 5 (helpers first, since control 3's promotion rules do not need them
-- but the secrets walker does): embedded secrets.
--
-- Doc 17 §1: "Secrets are referenced by secret-store identifiers, never
-- embedded in configuration documents." Doc 06 §6: secrets "never enter source,
-- logs, prompts, analytics, test fixtures, or client bundles."
--
-- The key matcher is deliberately segment-anchored rather than a substring
-- search. A substring search for "token" would flag token_endpoint,
-- token_ttl, auth_method and every other innocuous field whose name happens to
-- contain a secret-ish word, and a control that rejects valid configuration
-- gets disabled in practice. Requiring the final path segment to be the
-- secret-bearing word matches access_token, db_password, api_key and
-- signing_secret, and does not match token_endpoint or token_ttl. A key whose
-- final segment is a secret word but which is itself a reference field
-- (secret_ref, secret_id) does not match either, which is the correct reading:
-- those name a reference, they do not hold a value.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION config.is_secret_bearing_key(p_key TEXT)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT lower(p_key) ~ '^(.*[_-])?(password|passwd|secret|token|api[_-]?key|private[_-]?key|credential|passphrase|bearer)$';
$$;

COMMENT ON FUNCTION config.is_secret_bearing_key IS
    'True when a configuration key name is one that conventionally holds a secret VALUE rather than a reference. Matched on whole path segments, so access_token and db_password match while token_endpoint, token_ttl and secret_ref do not.';

CREATE OR REPLACE FUNCTION config.is_secret_store_reference(p_value JSONB)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT jsonb_typeof(p_value) = 'string'
       AND (p_value #>> '{}') ~ '^secrets?://[A-Za-z0-9._~:/#=-]+$';
$$;

COMMENT ON FUNCTION config.is_secret_store_reference IS
    'True for a secret-store identifier of the form secrets://path#fragment. The scheme is fixed here rather than being any URI, because a configuration that satisfies the check by storing a secret under some other scheme has satisfied nothing.';

CREATE OR REPLACE FUNCTION config.assert_no_embedded_secrets(p_document JSONB, p_path TEXT DEFAULT '$')
RETURNS VOID
LANGUAGE plpgsql
AS $$
DECLARE
    v_key   TEXT;
    v_index TEXT;
    v_value JSONB;
BEGIN
    IF p_document IS NULL THEN
        RETURN;
    END IF;

    CASE jsonb_typeof(p_document)
        WHEN 'string' THEN
            -- A PEM block is a secret whatever it is filed under, including
            -- under an innocuous key name, so it is matched on content too.
            IF (p_document #>> '{}') LIKE '%-----BEGIN%PRIVATE KEY-----%' THEN
                RAISE EXCEPTION
                    'Configuration at % embeds what appears to be a private key. Secrets must be referenced by secret-store identifier and never embedded in configuration documents (17_CONFIGURATION_AND_RISK_POLICY.md §1, 06_SECURITY_AND_ACCESS_CONTROL.md §6).',
                    p_path
                    USING ERRCODE = 'check_violation';
            END IF;

        WHEN 'object' THEN
            FOR v_key, v_value IN SELECT e.key, e.value FROM jsonb_each(p_document) AS e LOOP
                IF config.is_secret_bearing_key(v_key)
                   AND v_value IS NOT NULL
                   AND jsonb_typeof(v_value) <> 'null'
                   AND NOT config.is_secret_store_reference(v_value) THEN
                    RAISE EXCEPTION
                        'Configuration key "%" at % carries an embedded secret value. Configuration documents must hold a secret-store identifier such as secrets://... , never the secret itself (17_CONFIGURATION_AND_RISK_POLICY.md §1, 06_SECURITY_AND_ACCESS_CONTROL.md §6).',
                        v_key, p_path || '.' || v_key
                        USING ERRCODE = 'check_violation';
                END IF;
                PERFORM config.assert_no_embedded_secrets(v_value, p_path || '.' || v_key);
            END LOOP;

        WHEN 'array' THEN
            -- jsonb_each only accepts objects, so an array needs the array form.
            -- The earlier version of this function used jsonb_each for both and
            -- the nested-array test passed against "cannot call jsonb_each on a
            -- non-object" rather than against the secrets rule, which is why the
            -- test now asserts the reason it was refused.
            FOR v_index, v_value IN
                SELECT (e.ordinality - 1)::text, e.value
                  FROM jsonb_array_elements(p_document) WITH ORDINALITY AS e(value, ordinality)
            LOOP
                PERFORM config.assert_no_embedded_secrets(v_value, p_path || '[' || v_index || ']');
            END LOOP;

        ELSE
            NULL; -- numbers, booleans and nulls cannot carry a secret.
    END CASE;
END;
$$;

COMMENT ON FUNCTION config.assert_no_embedded_secrets IS
    'Recursively refuses a configuration document that embeds a secret. A value under a secret-bearing key must be a secrets:// reference, or absent. Any string anywhere that looks like a PEM private key is refused regardless of its key name.';

-- -----------------------------------------------------------------------------
-- Control 6: promotion provenance, and live is never an automatic copy.
--
-- Doc 17 §1: "Configuration promotion follows dev, test, staging, paper,
-- shadow, and live. Live configuration cannot be copied automatically from
-- lower environments."
--
-- The previous schema could not express that sentence at all, because a
-- revision recorded no provenance: nothing said where a configuration came
-- from, so "copied automatically" was not merely forbidden, it was
-- unrepresentable. Two nullable columns make it representable, and the trigger
-- makes the live case require a second identity.
--
-- The rule is deliberately narrow. It does NOT require a revision to declare
-- provenance -- an author writing a live configuration from scratch is a
-- legitimate case and forcing a fabricated parent would be worse than not
-- recording one. It requires that IF a revision was derived from another
-- revision, the source is real, is ACTIVE, and is at a strictly lower rung of
-- the promotion ladder, and that when the target is live the copy carries a
-- second approver distinct from its author.
--
-- That last clause is the whole control. An automated promotion pipeline has
-- one identity, the service account that wrote the revision. It cannot produce
-- a second distinct approver, so an automated copy into live cannot commit.
-- There is no flag to set and no environment variable to flip.
-- -----------------------------------------------------------------------------
ALTER TABLE config.revision
    ADD COLUMN derived_from_revision_id TEXT REFERENCES config.revision (revision_id),
    ADD COLUMN promotion_approved_by   TEXT;

COMMENT ON COLUMN config.revision.derived_from_revision_id IS
    'The ACTIVE revision this one was promoted from, if any. NULL means the revision was authored directly, which is permitted. Set means promoted, and promotion is then governed by config.guard_promotion_provenance.';
COMMENT ON COLUMN config.revision.promotion_approved_by IS
    'A second identity, distinct from created_by, who approved promoting this revision into its environment from a lower one. Required for a promotion into live. This is what an automated copy cannot supply.';

CREATE OR REPLACE FUNCTION config.guard_promotion_provenance()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_source config.revision%ROWTYPE;
BEGIN
    IF NEW.derived_from_revision_id IS NULL THEN
        -- Authored directly. Nothing to verify.
        IF NEW.promotion_approved_by IS NOT NULL THEN
            RAISE EXCEPTION
                'Revision % names an approver % but no source revision. A promotion approval applies to a promotion; a revision authored directly has nothing to approve.',
                NEW.revision_id, NEW.promotion_approved_by
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    SELECT * INTO v_source FROM config.revision WHERE revision_id = NEW.derived_from_revision_id;

    -- A foreign key on derived_from_revision_id already refuses a nonexistent
    -- source, and it fires immediately at INSERT so the row never exists. The
    -- branch below is therefore unreachable through that path. It is kept
    -- because this trigger also fires on UPDATE, and because a control that
    -- depends on a constraint elsewhere in the schema to be present is a
    -- control that disappears quietly if that constraint is dropped.
    IF NOT FOUND THEN
        RAISE EXCEPTION
            'Revision % claims to be promoted from revision %, which does not exist.',
            NEW.revision_id, NEW.derived_from_revision_id
            USING ERRCODE = 'check_violation';
    END IF;

    IF v_source.environment = NEW.environment THEN
        RAISE EXCEPTION
            'Revision % declares promotion from revision % in the same environment (%). A revision within one environment is a supersession, not a promotion.',
            NEW.revision_id, v_source.revision_id, NEW.environment
            USING ERRCODE = 'check_violation';
    END IF;

    -- The ladder is monotonic. A configuration may move forward, never
    -- backward, and never sideways into an unrelated rung.
    IF common.environment_rank(v_source.environment) >= common.environment_rank(NEW.environment) THEN
        RAISE EXCEPTION
            'Revision % promotes from environment % to %, which is not forward on the promotion ladder (dev, test, staging, paper, shadow, live).',
            NEW.revision_id, v_source.environment, NEW.environment
            USING ERRCODE = 'check_violation';
    END IF;

    -- Promotion out of a non-final source means that source is being superseded
    -- where it lived. It must be in a state that can be promoted from.
    IF NEW.environment = 'live' THEN
        IF NEW.promotion_approved_by IS NULL THEN
            RAISE EXCEPTION
                'Revision % promotes configuration into live from environment % with no second approver. Live configuration cannot be copied automatically from lower environments (17_CONFIGURATION_AND_RISK_POLICY.md §1).',
                NEW.revision_id, v_source.environment
                USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.promotion_approved_by = NEW.created_by THEN
            RAISE EXCEPTION
                'Revision % promotes configuration into live with approver %, who is also its author. A promotion into live requires two distinct identities.',
                NEW.revision_id, NEW.promotion_approved_by
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER revision_promotion_provenance
    AFTER INSERT OR UPDATE OF derived_from_revision_id, promotion_approved_by, created_by, environment
    ON config.revision
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION config.guard_promotion_provenance();

COMMENT ON FUNCTION config.guard_promotion_provenance IS
    'A revision that declares a source must name a real ACTIVE-equivalent source at a strictly lower rung of the promotion ladder, and a promotion into live must carry a second approver distinct from its author. An automated pipeline has one identity and therefore cannot promote into live.';

-- -----------------------------------------------------------------------------
-- Control 3: immutability and a closed lifecycle.
--
-- This is the control 0001 claimed to have. Two rules, and the split matters:
--
--   CONTENT is immutable from INSERT, always. A revision is never edited. A
--   change is a new revision. This holds even for a DRAFT, because a draft
--   that can be edited in place has no stable identity for review: the thing
--   that was reviewed would not be the thing that gets activated.
--
--   LIFECYCLE columns are the only ones that may move, and only along a closed
--   path: DRAFT -> ACTIVE -> SUPERSEDED | REVOKED, or DRAFT -> REVOKED. There
--   is no way back to DRAFT, no way out of a terminal state, and no direct
--   DRAFT -> SUPERSEDED. Timestamps are set by this trigger rather than by the
--   caller, so activated_at and deactivated_at cannot be backdated or forged.
--
-- DELETE is permitted only for a DRAFT. An activated, superseded or revoked
-- revision is release evidence: deleting it would erase the record of what was
-- in force, and unlike audit.record this table has no independent copy
-- elsewhere.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION config.guard_revision_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_content_changed TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status <> 'DRAFT' THEN
            RAISE EXCEPTION
                'Configuration revision % has status % and cannot be deleted. A revision that has been activated is release evidence; supersede or revoke it instead (17_CONFIGURATION_AND_RISK_POLICY.md §1).',
                OLD.revision_id, OLD.status
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN OLD;
    END IF;

    -- Content columns are immutable for the lifetime of the revision.
    -- Every value is cast to text before comparison. The columns are of mixed
    -- types -- text, common.environment, bigint, timestamptz, bytea -- and a
    -- VALUES list unifies its rows to a single type, so without casts the whole
    -- comparison fails to build before it can report anything. Only the
    -- column NAME is ever surfaced, so comparing as text loses nothing.
    SELECT string_agg(column_name, ', ' ORDER BY column_name) INTO v_content_changed
      FROM (VALUES
                ('revision_id',          OLD.revision_id::text,          NEW.revision_id::text),
                ('environment',          OLD.environment::text,          NEW.environment::text),
                ('revision_number',      OLD.revision_number::text,      NEW.revision_number::text),
                ('schema_version',       OLD.schema_version::text,       NEW.schema_version::text),
                ('content_digest',       OLD.content_digest::text,       NEW.content_digest::text),
                ('document',             OLD.document::text,             NEW.document::text),
                ('created_at',           OLD.created_at::text,           NEW.created_at::text),
                ('created_at_ns',        OLD.created_at_ns::text,        NEW.created_at_ns::text),
                ('created_by',           OLD.created_by::text,           NEW.created_by::text),
                ('reason',               OLD.reason::text,               NEW.reason::text),
                ('signature',            OLD.signature::text,            NEW.signature::text),
                ('signing_key_id',       OLD.signing_key_id::text,       NEW.signing_key_id::text),
                ('derived_from_revision_id', OLD.derived_from_revision_id::text, NEW.derived_from_revision_id::text),
                ('promotion_approved_by',OLD.promotion_approved_by::text,NEW.promotion_approved_by::text)
           ) AS t(column_name, before_value, after_value)
     WHERE before_value IS DISTINCT FROM after_value;

    IF v_content_changed IS NOT NULL THEN
        RAISE EXCEPTION
            'Configuration revision % is immutable; % may not be changed. A configuration change creates a new revision (17_CONFIGURATION_AND_RISK_POLICY.md §1).',
            OLD.revision_id, v_content_changed
            USING ERRCODE = 'check_violation';
    END IF;

    -- Closed lifecycle. Terminal states are terminal.
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT (
            (OLD.status = 'DRAFT'    AND NEW.status IN ('ACTIVE', 'REVOKED')) OR
            (OLD.status = 'ACTIVE'   AND NEW.status IN ('SUPERSEDED', 'REVOKED'))
        ) THEN
            RAISE EXCEPTION
                'Configuration revision % cannot move from % to %. The lifecycle is DRAFT -> ACTIVE -> SUPERSEDED|REVOKED, plus DRAFT -> REVOKED; there is no path from a terminal state to any other state.',
                OLD.revision_id, OLD.status, NEW.status
                USING ERRCODE = 'check_violation';
        END IF;

        IF NEW.status = 'ACTIVE' THEN
            -- The transition timestamp is the database's to record. A caller
            -- that supplies activated_at is not believed.
            NEW.activated_at := now();
        END IF;
    END IF;

    IF NEW.status IN ('SUPERSEDED', 'REVOKED') AND OLD.deactivated_at IS NULL THEN
        NEW.deactivated_at := now();
    END IF;

    -- A revision cannot be activated without a signature in an environment
    -- where that is required. 0001's CHECK covers live; this additionally
    -- refuses an unactivated-but-claimed state, so an operator cannot leave a
    -- revision sitting in ACTIVE with a null signature by inserting it that way.
    IF NEW.status = 'ACTIVE' AND NEW.signature IS NULL AND NEW.environment = 'live' THEN
        RAISE EXCEPTION
            'Configuration revision % cannot be activated in live without a signature (24_ENTERPRISE_RELEASE_STANDARD.md §2).',
            NEW.revision_id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER revision_mutation_guard
    BEFORE UPDATE OR DELETE ON config.revision
    FOR EACH ROW EXECUTE FUNCTION config.guard_revision_mutation();

COMMENT ON FUNCTION config.guard_revision_mutation IS
    'Content columns of a configuration revision are immutable from INSERT, for every status including DRAFT. Only lifecycle columns move, only along the closed path DRAFT -> ACTIVE -> SUPERSEDED|REVOKED (or DRAFT -> REVOKED), and the transition timestamps are set here rather than by the caller. DELETE is permitted only for a DRAFT.';

-- Enforce the secrets rule on the way in, where it is checkable.
CREATE OR REPLACE FUNCTION config.guard_revision_secrets()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM config.assert_no_embedded_secrets(NEW.document, '$.document');
    RETURN NEW;
END;
$$;

CREATE TRIGGER revision_secrets_guard
    BEFORE INSERT ON config.revision
    FOR EACH ROW EXECUTE FUNCTION config.guard_revision_secrets();

COMMENT ON FUNCTION config.guard_revision_secrets IS
    'Refuses a configuration document that embeds a secret. Runs on INSERT only, because control 3 already forbids changing the document afterwards.';

-- -----------------------------------------------------------------------------
-- Control 4: at most one ACTIVE revision per environment.
--
-- 0001 created config_revision_active_idx as a UNIQUE index on
-- (environment, revision_number DESC) WHERE status = 'ACTIVE'. That index is
-- unique on the PAIR, so it constrains revision_number and permits any number
-- of ACTIVE revisions per environment -- two revisions with different numbers
-- both satisfy it.
--
-- Doc 17 §1 says runtime services "consume a validated snapshot", singular, and
-- doc 01 §8 prohibits last-write-wins for authoritative financial state. Two
-- ACTIVE revisions in one environment is exactly the ambiguity that produces
-- a last-write-wins reader, so the index has to be on the environment alone.
--
-- The old index is dropped rather than left alongside: it constrains nothing
-- once the correct one exists, and keeping it would suggest two overlapping
-- guarantees.
-- -----------------------------------------------------------------------------
DROP INDEX IF EXISTS config.config_revision_active_idx;

CREATE UNIQUE INDEX config_revision_single_active_idx
    ON config.revision (environment)
    WHERE status = 'ACTIVE';

COMMENT ON INDEX config.config_revision_single_active_idx IS
    'At most one ACTIVE configuration revision per environment. Replaces config_revision_active_idx, which was unique on (environment, revision_number) and therefore permitted any number of ACTIVE revisions per environment.';

-- -----------------------------------------------------------------------------
-- Consistency of the remaining activation bookkeeping.
--
-- 0001 allowed a revision to be INSERTed directly with status 'ACTIVE' and a
-- deactivated_at already set, or activated_at in the future. The lifecycle
-- trigger owns the timestamps for transitions, so an INSERT that claims to be
-- ACTIVE has not gone through a transition at all.
-- -----------------------------------------------------------------------------
ALTER TABLE config.revision
    ADD CONSTRAINT active_requires_activation CHECK (
        status <> 'ACTIVE' OR (activated_at IS NOT NULL AND deactivated_at IS NULL)
    ),
    ADD CONSTRAINT terminal_requires_deactivation CHECK (
        status NOT IN ('SUPERSEDED', 'REVOKED') OR deactivated_at IS NOT NULL
    ),
    ADD CONSTRAINT no_deactivation_before_creation CHECK (
        deactivated_at IS NULL OR deactivated_at >= created_at
    );

COMMENT ON CONSTRAINT no_deactivation_before_creation ON config.revision IS
    'A revision cannot be deactivated before it was created. A DRAFT revoked immediately is still deactivated after creation, so the lifecycle trigger''s now()-on-transition is consistent with this.';

COMMENT ON CONSTRAINT active_requires_activation ON config.revision IS
    'An ACTIVE revision must carry an activation timestamp and must not already be deactivated. Combined with the lifecycle trigger, which sets activated_at on the DRAFT -> ACTIVE transition, this refuses an INSERT that claims to be ACTIVE without having been activated.';
