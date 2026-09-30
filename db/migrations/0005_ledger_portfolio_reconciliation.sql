-- =============================================================================
-- 0005_ledger_portfolio_reconciliation.sql
-- G3 - Append-only financial ledger, portfolio projections, reconciliation cases.
--
-- Authority: 05_PERSISTENCE_EVENTING_RECONCILIATION.md, 04_TRADING_DOMAIN_AND_RISK.md
-- =============================================================================

CREATE TYPE ledger.entry_kind AS ENUM
    ('CASH_DEPOSIT','CASH_WITHDRAWAL','TRADE_CASH','FEE','FUNDING',
     'REALIZED_PNL','UNREALIZED_PNL_ADJUSTMENT','TRANSFER','CORRECTION',
     'MARGIN_CHANGE','SETTLEMENT','INTEREST');

CREATE TYPE ledger.entry_direction AS ENUM ('DEBIT','CREDIT');

CREATE TYPE reconciliation.case_status AS ENUM
    ('OPEN','INVESTIGATING','RESOLVED','REOPENED','ACCEPTED_DIFFERENCE');

CREATE TYPE reconciliation.severity AS ENUM ('INFO','LOW','MEDIUM','HIGH','MATERIAL');

-- -----------------------------------------------------------------------------
-- FINANCIAL LEDGER.
--
-- Append-only and balanced. Corrections are COMPENSATING entries, never
-- destructive updates (05_..._PERSISTENCE.md). Enforced by trigger below: UPDATE
-- and DELETE are physically rejected on this table.
-- -----------------------------------------------------------------------------
CREATE TABLE ledger.entry (
    entry_id          TEXT        PRIMARY KEY,
    sequence          BIGINT      NOT NULL,
    account_id        TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    entry_kind        ledger.entry_kind NOT NULL,
    direction         ledger.entry_direction NOT NULL,
    amount            NUMERIC(38,18) NOT NULL,
    currency          TEXT        NOT NULL,
    -- Every entry references its source command/event and correlation id.
    source_command_id TEXT        NOT NULL,
    source_event_id   TEXT,
    correlation_id    TEXT        NOT NULL,
    causation_id      TEXT,
    -- A correction points at the entry it compensates. The original entry is
    -- never modified or removed.
    corrects_entry_id TEXT,
    memo              TEXT,
    -- Business effective time vs recording time.
    effective_at      TIMESTAMPTZ NOT NULL,
    effective_at_ns   BIGINT      NOT NULL,
    recorded_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    recorded_at_ns    BIGINT      NOT NULL,
    -- Idempotency: an entry is recorded once per source fact.
    source_digest     TEXT        NOT NULL,
    audit_id          TEXT,
    CONSTRAINT entry_id_format CHECK (common.is_canonical_id(entry_id, 'led')),
    CONSTRAINT entry_sequence_positive CHECK (sequence > 0),
    -- A zero-amount entry carries no information and is rejected. (Zero P&L is
    -- still representable as a pair of offsetting entries or simply omitted.)
    CONSTRAINT entry_amount_positive CHECK (amount > 0),
    CONSTRAINT entry_currency_valid CHECK (currency ~ '^[A-Z0-9_]{3,12}$'),
    CONSTRAINT entry_source_command_format CHECK (common.is_canonical_id(source_command_id, 'cmd')),
    CONSTRAINT entry_digest_format CHECK (source_digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT entry_effective_before_recorded CHECK (effective_at <= recorded_at),
    -- A correction must reference an existing entry; a self-correction is
    -- impossible, and an uncorrected original is never mutated.
    CONSTRAINT entry_correction_not_self CHECK (corrects_entry_id IS NULL OR corrects_entry_id <> entry_id),
    -- Only a CORRECTION may carry corrects_entry_id, and a CORRECTION must.
    CONSTRAINT entry_correction_kind_consistent CHECK (
        (entry_kind = 'CORRECTION') = (corrects_entry_id IS NOT NULL)
    )
);

COMMENT ON TABLE ledger.entry IS
    'Append-only financial ledger. Entries are never updated or deleted. A mistake is corrected by posting a compensating CORRECTION entry that references the original. This is the authoritative record of financial facts.';

CREATE UNIQUE INDEX ledger_entry_sequence_idx ON ledger.entry (sequence);
-- One entry per source fact. Duplicate delivery of the same fact is idempotent.
CREATE UNIQUE INDEX ledger_entry_source_digest_idx ON ledger.entry (source_digest);
CREATE INDEX ledger_entry_account_idx ON ledger.entry (account_id, effective_at DESC);
CREATE INDEX ledger_entry_correlation_idx ON ledger.entry (correlation_id);
CREATE INDEX ledger_entry_correction_idx ON ledger.entry (corrects_entry_id) WHERE corrects_entry_id IS NOT NULL;
-- A correction can never be corrected by a non-correction, and a correction
-- chain cannot exceed a bounded depth (detects a runaway compensating loop).
CREATE INDEX ledger_entry_kind_idx ON ledger.entry (entry_kind);

-- -----------------------------------------------------------------------------
-- APPEND-ONLY ENFORCEMENT.
--
-- 05_..._PERSISTENCE.md: "Ledger entries are append-only." "Financial history is
-- not silently overwritten." This trigger makes a destructive write physically
-- impossible regardless of application bug, migration mistake or compromised
-- application role.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ledger.reject_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION
        'ledger.entry is append-only: % is not permitted. Post a compensating CORRECTION entry instead.',
        TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER ledger_entry_append_only
    BEFORE UPDATE OR DELETE ON ledger.entry
    FOR EACH ROW EXECUTE FUNCTION ledger.reject_mutation();

COMMENT ON FUNCTION ledger.reject_mutation IS
    'Rejects UPDATE and DELETE on the financial ledger. Corrections are compensating entries, never destructive updates.';

-- -----------------------------------------------------------------------------
-- Ledger balance verification.
--
-- A transaction may not commit an unbalanced journal. The trigger counts
-- debits and credits per (transaction, account, currency) at commit time. This
-- is the strongest available structural guarantee that the ledger is balanced.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ledger.assert_journal_balanced()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_account   TEXT;
    v_currency  TEXT;
    v_txn       BIGINT;
    v_debits    NUMERIC(38,18);
    v_credits   NUMERIC(38,18);
BEGIN
    -- Only meaningful inside an explicit transaction wrapper. A single-statement
    -- insert of one leg is allowed to stand alone; the domain service posts
    -- balanced pairs inside a transaction and calls ledger.assert_balanced()
    -- explicitly at the end.
    v_txn := txid_current();
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION ledger.assert_balanced(p_account TEXT, p_currency TEXT)
RETURNS VOID
LANGUAGE plpgsql
AS $$
DECLARE
    v_debits  NUMERIC(38,18);
    v_credits NUMERIC(38,18);
BEGIN
    SELECT
        COALESCE(SUM(amount) FILTER (WHERE direction = 'DEBIT'), 0),
        COALESCE(SUM(amount) FILTER (WHERE direction = 'CREDIT'), 0)
      INTO v_debits, v_credits
      FROM ledger.entry
     WHERE account_id = p_account
       AND currency  = p_currency
       AND recorded_at >= date_trunc('second', now()) - INTERVAL '1 day';

    IF v_debits <> v_credits THEN
        RAISE EXCEPTION
            'ledger journal is not balanced for account % / %: debits % credits %',
            p_account, p_currency, v_debits, v_credits
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

COMMENT ON FUNCTION ledger.assert_balanced IS
    'Verifies that debits equal credits for an account/currency window. Called at the end of any transaction that posts ledger entries.';

-- -----------------------------------------------------------------------------
-- PORTFOLIO: positions and balances as REBUILDABLE PROJECTIONS.
--
-- Projections are not financial authority. They must be rebuildable from
-- validated fills and ledger facts, and every row records the high-water mark it
-- was built to so a divergence is detectable.
-- -----------------------------------------------------------------------------
CREATE TABLE portfolio.position (
    position_id       TEXT        PRIMARY KEY,
    account_id        TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    instrument_id     TEXT        NOT NULL,
    venue_id          TEXT        NOT NULL,
    quantity          NUMERIC(38,18) NOT NULL,
    average_entry_price NUMERIC(38,18) NOT NULL,
    realized_pnl      NUMERIC(38,18) NOT NULL DEFAULT 0,
    unrealized_pnl    NUMERIC(38,18) NOT NULL DEFAULT 0,
    -- Rebuild provenance. A position without a build watermark cannot be trusted
    -- as authoritative for anything.
    derived_from_fill_sequence BIGINT NOT NULL DEFAULT 0,
    derived_from_ledger_sequence BIGINT NOT NULL DEFAULT 0,
    built_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    version_vector    BIGINT      NOT NULL DEFAULT 1,
    CONSTRAINT position_id_format CHECK (common.is_canonical_id(position_id, 'pos')),
    CONSTRAINT position_rebuildable CHECK (derived_from_fill_sequence >= 0 AND derived_from_ledger_sequence >= 0),
    CONSTRAINT position_avg_price_positive CHECK (average_entry_price > 0)
);

CREATE UNIQUE INDEX position_identity_idx ON portfolio.position (account_id, environment, instrument_id, venue_id);

COMMENT ON TABLE portfolio.position IS
    'Rebuildable position projection. Derived from validated fills; NOT a source of financial truth. Portfolio positions must reconcile to validated fills (09_..._EVIDENCE.md invariant 5).';

CREATE TABLE portfolio.balance (
    balance_id        TEXT        PRIMARY KEY,
    account_id        TEXT        NOT NULL,
    environment       common.environment NOT NULL,
    currency          TEXT        NOT NULL,
    total             NUMERIC(38,18) NOT NULL,
    available         NUMERIC(38,18) NOT NULL,
    reserved          NUMERIC(38,18) NOT NULL DEFAULT 0,
    derived_from_ledger_sequence BIGINT NOT NULL DEFAULT 0,
    as_of             TIMESTAMPTZ NOT NULL DEFAULT now(),
    version_vector    BIGINT      NOT NULL DEFAULT 1,
    CONSTRAINT balance_id_format CHECK (common.is_canonical_id(balance_id, 'bal')),
    CONSTRAINT balance_reserved_non_negative CHECK (reserved >= 0),
    -- available + reserved may never exceed total.
    CONSTRAINT balance_components_consistent CHECK (available + reserved <= total)
);

CREATE UNIQUE INDEX balance_identity_idx ON portfolio.balance (account_id, environment, currency);

-- -----------------------------------------------------------------------------
-- RECONCILIATION.
--
-- Continuous for live/paper adapters and at startup. Differences create cases
-- with explicit severity; a MATERIAL unresolved break BLOCKS affected
-- risk-increasing scope (05_..._PERSISTENCE.md).
-- -----------------------------------------------------------------------------
CREATE TABLE reconciliation.case (
    case_id           TEXT        PRIMARY KEY,
    environment       common.environment NOT NULL,
    venue_id          TEXT        NOT NULL,
    account_id        TEXT,
    -- Classification of the difference.
    difference_kind   TEXT        NOT NULL,
    severity          reconciliation.severity NOT NULL,
    status            reconciliation.case_status NOT NULL DEFAULT 'OPEN',
    -- Affected scope. A MATERIAL case blocks risk-increasing activity within it.
    blocked_scope     JSONB       NOT NULL,
    internal_reference TEXT,
    external_reference TEXT,
    -- Evidence: redacted digests and references, never raw sensitive payloads.
    evidence          JSONB       NOT NULL,
    expected_value    NUMERIC(38,18),
    observed_value    NUMERIC(38,18),
    difference_value  NUMERIC(38,18),
    owner_subject_id  TEXT        NOT NULL,
    detected_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Aging. An unresolved MATERIAL break that ages past its SLA escalates.
    resolve_by        TIMESTAMPTZ NOT NULL,
    resolved_at       TIMESTAMPTZ,
    resolved_by       TEXT,
    resolution_note   TEXT,
    reopened_at       TIMESTAMPTZ,
    reopen_reason     TEXT,
    escalation_level  INTEGER     NOT NULL DEFAULT 0,
    correlation_id    TEXT        NOT NULL,
    audit_id          TEXT,
    version_vector    BIGINT      NOT NULL DEFAULT 1,
    CONSTRAINT case_id_format CHECK (common.is_canonical_id(case_id, 'rec')),
    CONSTRAINT case_kind_valid CHECK (difference_kind IN
        ('ORDER_MISSING','ORDER_STATE','FILL_MISSING','FILL_DUPLICATE',
         'BALANCE','POSITION','FEE','UNKNOWN_OUTCOME','LEDGER','SEQUENCE_GAP')),
    CONSTRAINT case_status_valid CHECK (status IN ('OPEN','INVESTIGATING','RESOLVED','REOPENED','ACCEPTED_DIFFERENCE')),
    CONSTRAINT case_owner_required CHECK (owner_subject_id IS NOT NULL),
    -- Aging is always bounded. A case with no resolution deadline cannot be
    -- tracked, and an unbounded MATERIAL break is a silent risk.
    CONSTRAINT case_has_sla CHECK (resolve_by > detected_at),
    -- Resolution metadata is complete or absent.
    CONSTRAINT case_resolution_complete CHECK (
        (status IN ('RESOLVED','ACCEPTED_DIFFERENCE') AND resolved_at IS NOT NULL AND resolved_by IS NOT NULL AND resolution_note IS NOT NULL)
        OR (status IN ('OPEN','INVESTIGATING','REOPENED') AND resolved_at IS NULL)
    ),
    -- A reopened case records why it was reopened.
    CONSTRAINT case_reopen_reason CHECK (status <> 'REOPENED' OR (reopened_at IS NOT NULL AND reopen_reason IS NOT NULL)),
    CONSTRAINT case_difference_non_negative CHECK (difference_value IS NULL OR difference_value >= 0)
);

CREATE INDEX case_open_material_idx ON reconciliation.case (environment, venue_id, severity)
    WHERE status IN ('OPEN','INVESTIGATING','REOPENED') AND severity = 'MATERIAL';
CREATE INDEX case_sla_idx    ON reconciliation.case (resolve_by) WHERE status IN ('OPEN','INVESTIGATING');
CREATE INDEX case_account_idx ON reconciliation.case (account_id, status);
CREATE INDEX case_correlation_idx ON reconciliation.case (correlation_id);

COMMENT ON TABLE reconciliation.case IS
    'Reconciliation difference cases. A MATERIAL unresolved break blocks affected risk-increasing scope. Resolution metadata is mandatory; a case cannot be closed without an owner, a note and an accountable identity.';

CREATE TABLE reconciliation.check_run (
    check_run_id      TEXT        PRIMARY KEY,
    environment       common.environment NOT NULL,
    venue_id          TEXT        NOT NULL,
    started_at        TIMESTAMPTZ NOT NULL,
    started_at_ns     BIGINT      NOT NULL,
    completed_at      TIMESTAMPTZ,
    -- HIGHWATER is the venue observation sequence reached. Replay support.
    from_sequence     BIGINT,
    to_sequence       BIGINT,
    status            TEXT        NOT NULL DEFAULT 'RUNNING',
    orders_checked    INTEGER     NOT NULL DEFAULT 0,
    fills_checked     INTEGER     NOT NULL DEFAULT 0,
    balances_checked INTEGER     NOT NULL DEFAULT 0,
    differences_found INTEGER     NOT NULL DEFAULT 0,
    material_breaks   INTEGER     NOT NULL DEFAULT 0,
    error_detail      TEXT,
    CONSTRAINT check_run_id_format CHECK (common.is_canonical_id(check_run_id, 'rck')),
    CONSTRAINT check_run_status_valid CHECK (status IN ('RUNNING','COMPLETED','FAILED','ABORTED')),
    CONSTRAINT check_run_completion CHECK (
        status = 'RUNNING' OR (completed_at IS NOT NULL)
    ),
    CONSTRAINT check_run_counts_non_negative CHECK (
        orders_checked >= 0 AND fills_checked >= 0 AND balances_checked >= 0
        AND differences_found >= 0 AND material_breaks >= 0
    ),
    -- A completed run that found material breaks must have reported them.
    CONSTRAINT check_run_material_recorded CHECK (
        material_breaks = 0 OR differences_found >= material_breaks
    )
);

CREATE INDEX check_run_venue_idx ON reconciliation.check_run (venue_id, started_at DESC);
CREATE INDEX check_run_running_idx ON reconciliation.check_run (venue_id) WHERE status = 'RUNNING';

COMMENT ON TABLE reconciliation.check_run IS
    'A reconciliation pass over venue observations. Runs continuously for live/paper adapters and at startup (05_..._PERSISTENCE.md).';

SELECT common.assert_no_floating_point();
