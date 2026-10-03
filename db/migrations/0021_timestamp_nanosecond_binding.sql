-- =============================================================================
-- 0021_timestamp_nanosecond_binding.sql
-- G1 - Domain Foundation: bind the nanosecond columns to the TIMESTAMPTZ columns
--        they claim to refine.
--
-- Authority: 25_PRODUCT_BLUEPRINT_DOMAIN_PROFILE.md (timestamp semantics),
--            01_SYSTEM_ARCHITECTURE.md section 8.
--
-- WHY THIS MIGRATION EXISTS
--
-- audit.record stores occurred_at / recorded_at as TIMESTAMPTZ and
-- occurred_at_ns / recorded_at_ns as BIGINT nanoseconds, and nothing connects
-- them. A writer can store an occurred_at of 2026-09-30T03:00:00Z beside an
-- occurred_at_ns that describes 2020-01-01T00:00:00Z, and both are accepted.
--
-- The two columns exist because the audit chain and the replayable ingestion
-- path need nanosecond precision that TIMESTAMPTZ does not carry, and because
-- a consumer that has only the ns value must be able to order events without
-- consulting a timestamp type. That is a reasonable reason to have both. It is
-- not a reason for the two to disagree.
--
-- WHY THE FIRST ATTEMPT AT THIS CHECK WAS REMOVED
--
-- An earlier consistency check was written and then deleted because it produced
-- false positives. The check was correct to be suspicious and wrong to be
-- removed. The cause is a property of PostgreSQL, not of the data:
--
--   SELECT '2026-09-30 03:00:00.123456789'::timestamptz;
--   -- 2026-09-30 03:00:00.123457     (rounded to microseconds)
--
-- TIMESTAMPTZ has microsecond resolution. Any ns value with a sub-microsecond
-- remainder is rounded on input, so exact equality with the ns column is not
-- merely sometimes false -- it is false for 999 of every 1000 nanosecond values.
-- The first check therefore failed against correct data, which is what makes a
-- failing check get deleted rather than fixed.
--
-- The correct contract is weaker and true: the TIMESTAMPTZ is the ns value
-- rounded to the nearest microsecond. That is what this migration asserts. The
-- full nanosecond value remains authoritative in the ns column, and the
-- TIMESTAMPTZ is the rounded projection of it -- not the other way round, and
-- not an independent second opinion.
-- =============================================================================

CREATE OR REPLACE FUNCTION common.ns_to_timestamptz(p_ns BIGINT)
RETURNS TIMESTAMPTZ
LANGUAGE sql
IMMUTABLE
STRICT
AS $$
    -- to_timestamp() is declared for double precision, so a numeric argument is
    -- cast. That cast is exact for the range that matters: seconds since the
    -- Unix epoch are around 1.8e9, and a double carries integers exactly to
    -- 2^53, so no precision is lost here. The sub-second part is added as an
    -- interval and never passes through floating point at all.
    --
    -- floor() rather than trunc() so that a pre-epoch ns value, whose remainder
    -- under PostgreSQL's modulo is negative, still yields a correct timestamp.
    SELECT to_timestamp(floor(p_ns::numeric / 1000000000))
         + round((p_ns % 1000000000)::numeric / 1000.0) * interval '1 microsecond';
$$;

COMMENT ON FUNCTION common.ns_to_timestamptz IS
    'Renders epoch nanoseconds as TIMESTAMPTZ, rounded to the nearest microsecond because that is the resolution TIMESTAMPTZ has. The inverse relationship is lossy by up to 500ns and that loss is deliberate and documented: the ns column is authoritative, this is the rounded projection of it. Exact for the sub-second component, which is added as an interval rather than computed in floating point.';


-- Returns BOOLEAN rather than VOID, and that is not a stylistic choice. A
-- function returning VOID yields a void value, which is never NULL, so the
-- natural-looking CHECK form
--
--     CHECK (assert_ns_bound(a_ns, a_ts, 'label') IS NULL)
--
-- is false for every row and rejects every insert. The first version of this
-- migration did exactly that, and the suite failed with a bare constraint
-- violation carrying no indication of the cause. The check is a predicate, so
-- the function is a predicate.
CREATE OR REPLACE FUNCTION common.ns_bound_to_timestamptz(
    p_ns    BIGINT,
    p_value TIMESTAMPTZ
)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT p_ns IS NULL
        OR p_value IS NULL
        OR common.ns_to_timestamptz(p_ns) IS NOT DISTINCT FROM p_value;
$$;

COMMENT ON FUNCTION common.ns_bound_to_timestamptz IS
    'True when a TIMESTAMPTZ is the microsecond-rounded projection of the nanosecond value beside it, or when either side is NULL. Returns a predicate, not VOID, so it can be used directly in a CHECK constraint: a VOID-returning function is never NULL, so the IS NULL formulation of a check is false for every row.';


-- audit.record. Applied to both columns, since both are read by the replay and
-- ordering paths that need the nanosecond precision.
--
-- These two are added NOT VALID, and this is deliberate. audit.record is
-- append-only and large in any real deployment, so validating the pair across
-- every historical row would scan the entire audit chain inside the migration
-- and take a lock on it for the duration. A deployment that has records written
-- before 0021 could also hold rows this check would reject, which would abort
-- the migration rather than report anything.
--
-- NOT VALID does not weaken the guarantee for anything written from here on:
-- the check is enforced against every new and updated row immediately, which is
-- what the suite in dbtest/timestamp_ns_binding_test.go asserts. What it declines
-- to do is certify rows this migration did not write. The trade is explicit
-- rather than accidental, and it matches how audit.record.audit_signing_key_required
-- was already added earlier in this schema.
--
-- An operator can validate the historical rows later, out of band, once they are
-- known to satisfy the projection:
--
--   ALTER TABLE audit.record VALIDATE CONSTRAINT audit_record_occurred_at_ns_bound;
--   ALTER TABLE audit.record VALIDATE CONSTRAINT audit_record_recorded_at_ns_bound;
ALTER TABLE audit.record
    ADD CONSTRAINT audit_record_occurred_at_ns_bound CHECK (
        common.ns_bound_to_timestamptz(occurred_at_ns, occurred_at)
    ) NOT VALID,
    ADD CONSTRAINT audit_record_recorded_at_ns_bound CHECK (
        common.ns_bound_to_timestamptz(recorded_at_ns, recorded_at)
    ) NOT VALID;

-- The same shape exists in the market observations, which is where replayable
-- ingestion actually reads source_timestamp_ns rather than source_timestamp.
-- Leaving these unbound would fix the audit chain and leave the market-data
-- replay path with the same unbound pair.
--
-- These four are added NOT VALID for exactly the reason the audit.record
-- pair above is: market.trade, market.quote, market.candle and
-- market.feed_health are append-only observation tables that are large in
-- any real deployment, so validating the pair across every historical row
-- would scan each table inside the migration and take a lock on it for the
-- duration. A deployment with observations written before 0021 could also
-- hold rows whose two representations disagree -- that disagreement is the
-- very defect this migration exists to close -- and a VALID constraint
-- would abort the migration on such a row rather than report anything.
--
-- NOT VALID does not weaken the guarantee for anything written from here
-- on: the check is enforced against every new and updated row immediately,
-- which is what the suite in dbtest/timestamp_ns_binding_test.go asserts.
-- What it declines to do is certify rows this migration did not write. The
-- trade is the same one already taken for audit.record, applied
-- consistently rather than to one table and not the others.
--
-- An operator can validate the historical rows later, out of band, once they
-- are known to satisfy the projection:
--
--   ALTER TABLE market.trade      VALIDATE CONSTRAINT trade_source_timestamp_ns_bound;
--   ALTER TABLE market.quote      VALIDATE CONSTRAINT quote_source_timestamp_ns_bound;
--   ALTER TABLE market.candle     VALIDATE CONSTRAINT candle_source_timestamp_ns_bound;
--   ALTER TABLE market.feed_health VALIDATE CONSTRAINT feed_last_source_timestamp_ns_bound;
ALTER TABLE market.trade
    ADD CONSTRAINT trade_source_timestamp_ns_bound CHECK (
        common.ns_bound_to_timestamptz(source_timestamp_ns, source_timestamp)
    ) NOT VALID;

ALTER TABLE market.quote
    ADD CONSTRAINT quote_source_timestamp_ns_bound CHECK (
        common.ns_bound_to_timestamptz(source_timestamp_ns, source_timestamp)
    ) NOT VALID;

ALTER TABLE market.candle
    ADD CONSTRAINT candle_source_timestamp_ns_bound CHECK (
        common.ns_bound_to_timestamptz(source_timestamp_ns, source_timestamp)
    ) NOT VALID;

-- market.feed_health's watermark pair is bound too, for the same reason: it is
-- the value a freshness decision is made against, so a disagreement between its
-- two representations is a disagreement about how fresh the feed is.
ALTER TABLE market.feed_health
    ADD CONSTRAINT feed_last_source_timestamp_ns_bound CHECK (
        common.ns_bound_to_timestamptz(last_source_timestamp_ns, last_source_timestamp)
    ) NOT VALID;

COMMENT ON CONSTRAINT audit_record_occurred_at_ns_bound ON audit.record IS
    'occurred_at is the microsecond-rounded projection of occurred_at_ns. TIMESTAMPTZ has microsecond resolution, so exact equality is impossible for most ns values; the ns column is authoritative (defect 19).';

COMMENT ON CONSTRAINT trade_source_timestamp_ns_bound ON market.trade IS
    'source_timestamp is the microsecond-rounded projection of source_timestamp_ns, which is what the replayable ingestion path orders on (defect 19).';
