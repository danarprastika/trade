-- 0015_workload_identity_environment_ceiling.sql
--
-- Credential/environment isolation: mandatory financial invariant 8.
--
--   09_TESTING_AND_RELEASE_EVIDENCE.md line 5, invariant 8:
--       "Lower-environment credentials cannot access live resources."
--   11_EXECUTION_GATES.md: waivers are PROHIBITED for "financial invariants,
--       authorization, live credential isolation, reconciliation, halt controls,
--       and recovery objectives."
--
-- WHAT WAS MISSING
-- ----------------
-- Nothing. There was no workload identity model, no credential-to-environment
-- binding, and no enforcement of any kind. `identity.role_binding` and
-- `identity.session` carry an `environment` column, but they describe what a
-- *human* is authorised to do, not what a *credential* is able to reach. The
-- table `ops.workload_identity` did not exist.
--
-- AN HONEST STATEMENT OF WHAT THIS DOES AND DOES NOT FIX
-- -----------------------------------------------------
-- 01_SYSTEM_ARCHITECTURE.md section 4 requires that each environment have
-- "separate credentials, database, encryption keys, deployment identity,
-- network policy, and configuration namespace", and that "Live credentials are
-- never available to lower environments."
--
-- This repository has ONE database containing live, paper, shadow, staging,
-- test and dev rows distinguished by an `environment` column. That is not the
-- architecture the blueprint specifies, and no in-database control can make it
-- so. The primary control for invariant 8 is architectural: separate databases,
-- separate secret stores, mTLS service identity, and network policy that make a
-- live credential simply absent from a paper deployment. Those are
-- infrastructure deliverables and are tracked in infra/, not here.
--
-- What this migration adds is DEFENCE IN DEPTH for the shared-database reality
-- that exists today, and it is worth having for one reason: in a single shared
-- database a paper service holding a valid credential can, today, read and
-- write live rows simply by naming `environment = 'live'` in its WHERE clause.
-- That is a routine mistake, not an attack, and it is exactly the class of
-- error that turns a paper rehearsal into a live trade.
--
-- So the control binds each database principal to a trust zone and an
-- environment CEILING, and refuses the write when the row's environment is
-- above what the credential was ever issued for. It fails closed.
--
-- Trust zones are 01_SYSTEM_ARCHITECTURE.md section 4's "four hard trust
-- zones": edge, control, research, recovery. Two of the four are expressible as
-- refusals, and both are enforced here:
--
--   01: "The edge cannot access databases."
--        -> a principal may not be REGISTERED with zone 'edge' at all. The edge
--           has no business holding a database credential, so the database
--           refuses to record one. This is a CHECK constraint, so the
--           prohibition is on the fact existing, not on a code path.
--
--   01/02: "Research workloads cannot route to live control-plane data
--   stores"; "Python ... has no production credentials and no direct write
--   path to authoritative financial tables."
--        -> a research principal may never hold a live ceiling, and no
--           principal in the research zone may write to the authoritative
--           financial tables in any environment. Python is research-only, so
--           this is the zone that matters.
--
-- 06_SECURITY_AND_ACCESS_CONTROL.md: "Python research workers have no live
-- environment identity" -- enforced as a CHECK.

-- -----------------------------------------------------------------------------
-- 1. Environment rank, mirroring contracts.Environment.Rank()
-- -----------------------------------------------------------------------------
-- The promotion ladder must be identical on both sides of the wire. Go holds it
-- in an unexported promotionOrder slice; this is its counterpart. A Go/Parity
-- test in contracts/ asserts the two agree, because a divergence here would be
-- silent: the Go service would consider an environment live while the database
-- considered it out of range, or the reverse, and neither would notice until an
-- incident.
CREATE OR REPLACE FUNCTION common.environment_rank(p_environment common.environment)
RETURNS INT
LANGUAGE sql
IMMUTABLE
STRICT
AS $$
    SELECT CASE p_environment::text
        WHEN 'dev'     THEN 1
        WHEN 'test'    THEN 2
        WHEN 'staging' THEN 3
        WHEN 'paper'   THEN 4
        WHEN 'shadow'  THEN 5
        WHEN 'live'    THEN 6
    END;
$$;

COMMENT ON FUNCTION common.environment_rank IS
    'Promotion position on the environment ladder, mirroring contracts.Environment.Rank() in Go: dev(1) < test(2) < staging(3) < paper(4) < shadow(5) < live(6).';

-- -----------------------------------------------------------------------------
-- 2. Trust zones
-- -----------------------------------------------------------------------------
CREATE TYPE ops.trust_zone AS ENUM ('edge','control','research','recovery');

COMMENT ON TYPE ops.trust_zone IS
    'The four hard trust zones of 01_SYSTEM_ARCHITECTURE.md section 4. The edge is listed for completeness but may never be granted to a principal in this database, because the edge has no database access by design.';

-- -----------------------------------------------------------------------------
-- 3. Workload identity
-- -----------------------------------------------------------------------------
CREATE TABLE ops.workload_identity (
    workload_id       TEXT PRIMARY KEY,
    -- principal is the database credential: the PostgreSQL role the workload
    -- actually connects as. This is the real-world "credential", which is why
    -- the control is keyed on it rather than on an application-supplied string.
    -- An application that lies about who it is does not change session_user.
    principal         TEXT NOT NULL UNIQUE,
    zone              ops.trust_zone NOT NULL,
    -- max_environment is the CEILING: the most sensitive environment this
    -- credential was ever issued for. A paper service has a 'paper' ceiling and
    -- therefore cannot write a live row; a live service has a 'live' ceiling.
    max_environment   common.environment NOT NULL,
    state             TEXT NOT NULL DEFAULT 'ACTIVE',
    -- service_class mirrors 06_SECURITY_AND_ACCESS_CONTROL.md's separation of
    -- service, worker and human principals. Recorded for triage; the zone is
    -- what is enforced.
    service_class     TEXT,
    deployment        TEXT,
    registered_by     TEXT NOT NULL,
    registered_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_by        TEXT,
    revoked_at        TIMESTAMPTZ,
    revocation_reason TEXT,
    version_vector    BIGINT NOT NULL DEFAULT 1,

    CONSTRAINT workload_id_format CHECK (common.is_canonical_id(workload_id, 'wid')),
    CONSTRAINT workload_principal_present CHECK (btrim(principal) <> ''),
    CONSTRAINT workload_state_valid CHECK (state IN ('ACTIVE','REVOKED')),

    -- The edge cannot access databases. A principal may not be registered in
    -- the edge zone, so the prohibition is on the row existing rather than on
    -- any particular code path declining to use it.
    CONSTRAINT edge_has_no_database_credential CHECK (zone <> 'edge'),

    -- 06_SECURITY_AND_ACCESS_CONTROL.md: "Python research workers have no live
    -- environment identity." Written as an implication, not an equivalence: the
    -- research zone may never hold a live ceiling, but a control-plane workload
    -- with a non-live ceiling is perfectly normal.
    CONSTRAINT research_has_no_live_identity CHECK (
        NOT (zone = 'research' AND max_environment = 'live')
    ),

    -- A live ceiling requires a control or recovery zone. Combined with the
    -- research constraint this means only control-plane and recovery workloads
    -- can ever hold a live credential, which is the intent of the zone model.
    CONSTRAINT live_ceiling_requires_control_or_recovery CHECK (
        (max_environment <> 'live') OR zone IN ('control','recovery')
    ),

    -- A revoked credential must say who revoked it and why, and must not remain
    -- registered as active. Credential rotation is required at most every 90
    -- days (06_SECURITY_AND_ACCESS_CONTROL.md) and immediately on suspected
    -- exposure; a revocation with no reason cannot be triaged.
    CONSTRAINT revocation_is_evidenced CHECK (
        (state = 'ACTIVE')
        OR (revoked_by IS NOT NULL AND revoked_at IS NOT NULL AND revocation_reason IS NOT NULL)
    )
);

CREATE INDEX workload_identity_zone_idx ON ops.workload_identity (zone);
CREATE INDEX workload_identity_env_idx  ON ops.workload_identity (max_environment);

COMMENT ON TABLE ops.workload_identity IS
    'Binds each database credential to a trust zone and an environment ceiling. This is DEFENCE IN DEPTH for mandatory invariant 8, not the primary control: 01_SYSTEM_ARCHITECTURE.md requires separate databases, secret stores and network policy per environment, and no in-database control substitutes for those. Guards against a paper service credential reaching live rows in a shared database.';

-- -----------------------------------------------------------------------------
-- 4. The gate
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ops.assert_principal_permitted(
    p_environment common.environment,
    p_context     TEXT
)
RETURNS VOID
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    v_principal TEXT;
    v_zone      ops.trust_zone;
    v_max_env   common.environment;
    v_state     TEXT;
    v_class     TEXT;
BEGIN
    -- BOTH session_user and current_user are checked, and both must pass.
    --
    -- session_user is the role that authenticated the connection and is fixed
    -- for its lifetime. current_user is the role currently in effect and CHANGES
    -- under SET ROLE. Checking only session_user would mean that any session
    -- holding a live-capable credential could SET ROLE to a lower or unregistered
    -- role and the guard would still see the live-capable identity -- the guard
    -- would be escapable by the exact mechanism it exists to constrain. Checking
    -- only current_user would be escapable by SET ROLE the other way.
    --
    -- Requiring both to be registered, active, out of the research zone and
    -- within their own ceiling means the effective authority is the more
    -- restrictive of the two, which is the only safe reading.
    FOREACH v_principal IN ARRAY ARRAY[session_user, current_user] LOOP
        SELECT zone, max_environment, state, service_class
          INTO v_zone, v_max_env, v_state, v_class
          FROM ops.workload_identity
         WHERE principal = v_principal;

        -- Fail closed. An unregistered credential can write authoritative
        -- financial rows, so "we have never heard of you" must be a refusal, not
        -- a default allow. The cost is that adding a service requires a
        -- registration step, which is the point: it makes the credential's
        -- environment scope a deliberate act.
        IF NOT FOUND THEN
            RAISE EXCEPTION
                'credential refused (%): principal % has no ops.workload_identity registration. Failing closed is deliberate; an unknown credential must not reach authoritative financial rows. Register the workload with a trust zone and an environment ceiling.',
                p_context, v_principal
                USING ERRCODE = 'insufficient_privilege';
        END IF;

        IF v_state <> 'ACTIVE' THEN
            RAISE EXCEPTION
                'credential refused (%): principal % is REVOKED. Revoked credentials are refused for every environment, including those below their former ceiling.',
                p_context, v_principal
                USING ERRCODE = 'insufficient_privilege';
        END IF;

        -- The research zone has no direct write path to authoritative financial
        -- tables, in any environment. Checked before the ceiling so a research
        -- workload is refused for the reason that actually applies, rather than
        -- incidentally because it happened to exceed a ceiling.
        IF v_zone = 'research' THEN
            RAISE EXCEPTION
                'credential refused (%): principal % is a research-zone workload. Research workloads have no direct write path to authoritative financial tables in any environment (02_POLYGLOT_ENGINEERING_STANDARD.md; 01_SYSTEM_ARCHITECTURE.md section 4). They produce content-addressed artifacts, which are promoted by a control-plane process, not written directly.',
                p_context, v_principal
                USING ERRCODE = 'insufficient_privilege';
        END IF;

        -- The environment ceiling: a lower-environment credential cannot reach a
        -- higher-environment row. This is invariant 8.
        IF common.environment_rank(p_environment) > common.environment_rank(v_max_env) THEN
            RAISE EXCEPTION
                'credential refused (%): principal % is a % workload with an environment ceiling of %, and may not act in %. Live credentials are never available to lower environments (01_SYSTEM_ARCHITECTURE.md section 4).',
                p_context, v_principal, v_zone, v_max_env, p_environment
                USING ERRCODE = 'insufficient_privilege';
        END IF;
    END LOOP;
END;
$$;

COMMENT ON FUNCTION ops.assert_principal_permitted IS
    'Mandatory invariant 8 as a storage control: refuses a database credential acting in an environment above its registered ceiling, refuses research-zone writes to authoritative tables, and refuses unregistered or revoked credentials. Checks BOTH session_user and current_user, so the effective authority is the more restrictive of the authenticated role and the role currently assumed, and the guard cannot be escaped by SET ROLE.';

-- -----------------------------------------------------------------------------
-- 5. Attach to the authoritative financial tables
-- -----------------------------------------------------------------------------
-- BEFORE INSERT OR UPDATE, so the environment of a row is checked at the moment
-- it is written rather than at read time. Checking on read would be useless:
-- a live row that should never have been written cannot be un-written by
-- refusing to display it.
CREATE OR REPLACE FUNCTION ops.guard_principal_on_write()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM ops.assert_principal_permitted(NEW.environment,
        TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME || ' write');
    RETURN NEW;
END;
$$;

CREATE TRIGGER oms_order_principal_guard
    BEFORE INSERT OR UPDATE ON oms.order
    FOR EACH ROW EXECUTE FUNCTION ops.guard_principal_on_write();

CREATE TRIGGER ledger_entry_principal_guard
    BEFORE INSERT OR UPDATE ON ledger.entry
    FOR EACH ROW EXECUTE FUNCTION ops.guard_principal_on_write();

CREATE TRIGGER outbox_principal_guard
    BEFORE INSERT OR UPDATE ON ops.outbox
    FOR EACH ROW EXECUTE FUNCTION ops.guard_principal_on_write();

-- ops.idempotency_record is deliberately NOT guarded, and the reason is not
-- that it was overlooked.
--
-- It has no `environment` column because it does not need one. contracts.IdempotencyScope
-- includes Environment and states that "a command is never idempotent across
-- environments", so the environment is already mixed into scope_hash, which is
-- the table's uniqueness key. A paper record and a live record with the same
-- client Idempotency-Key are therefore distinct rows by construction.
--
-- Guarding this table would require adding a redundant column purely to have
-- something to compare, and a redundant column is a second source of truth for
-- a fact the primary key already fixes. If the two ever disagreed the guard
-- would be enforcing the wrong one.
--
-- ops.workload_identity itself is likewise deliberately not guarded. Guarding
-- the credential registry with the credential check would make it impossible to
-- register the first identity or to revoke a compromised one, which is the
-- exact window an attacker needs. Registration is instead an operator action
-- against a schema whose mutations are audited, and it is the reason a
-- compromised but still-ACTIVE credential is a residual risk recorded in the
-- G1 outstanding list rather than a solved problem.

-- -----------------------------------------------------------------------------
-- 6. Bootstrap
-- -----------------------------------------------------------------------------
-- The role that applies migrations is the control-plane owner and must be
-- registered, or the guards would refuse every subsequent migration. This is
-- registered explicitly and visibly rather than by a blanket exemption, so the
-- exemption is auditable: if migrations are ever run by a different role, the
-- failure mode is a loud refusal naming an unregistered principal, not a
-- silently unguarded database.
DO $$
DECLARE
    v_principal TEXT := session_user;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM ops.workload_identity WHERE principal = v_principal) THEN
        INSERT INTO ops.workload_identity
            (workload_id, principal, zone, max_environment, service_class,
             deployment, registered_by)
        VALUES ('wid_' || substr(md5('ops.workload_identity:' || v_principal), 1, 20),
                v_principal, 'control', 'live', 'migration_owner', 'local', 'bootstrap');
    END IF;
END;
$$;
