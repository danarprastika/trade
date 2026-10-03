-- =============================================================================
-- 0022_order_type_and_time_in_force.sql
--
-- Purpose: make time_in_force the sole expression of an immediate-or-cancel or
--          fill-or-kill instruction on oms.order.
--
-- Authority: 04_TRADING_DOMAIN_AND_RISK.md (order lifecycle),
--            03_CANONICAL_CONTRACTS.md (canonical order shape),
--            09_TESTING_AND_RELEASE_EVIDENCE.md invariant 1
--
-- Precedence: 25 > 23 > 03 > 04 > 21 > 22 > 24. This migration may only add a
--            control these documents require. It removes none.
-- =============================================================================
--
-- The defect
-- ----------
-- common.order_type and common.time_in_force both contain IOC and FOK. The same
-- instruction is therefore expressible in two columns of the same row, and
-- nothing prevents them from contradicting each other:
--
--     order_type = 'IOC', time_in_force = 'GTC'
--     order_type = 'FOK', time_in_force = 'DAY'
--
-- Both are accepted today. Neither means anything. A reader that consults
-- order_type gets "cancel whatever is unfilled immediately"; a reader that
-- consults time_in_force gets "good for the day". The two readings imply
-- opposite lifetimes for the same order, and which one governs is left to
-- whichever column the reader happened to open.
--
-- This is the shape of defect 23 and of the common.new_canonical_id divergence:
-- two representations of one fact that agree in testing and disagree in use. The
-- enum overlap itself is not the bug -- a shared vocabulary is legitimate, and
-- market.instrument.order_types and risk.policy.permitted_order_types may both
-- legitimately list IOC and FOK as venue capabilities. What is a bug is leaving
-- a caller to choose between two columns on the same row.
--
-- The resolution, and why it is not narrower
-- -------------------------------------------
-- Forbidding IOC and FOK in oms.order.order_type is enough, and it is chosen over
-- removing the values from the enum because removing them is destructive: both
-- instrument.order_types and policy.permitted_order_types are arrays of
-- common.order_type, so an enum value that any of those arrays holds cannot be
-- dropped without either rewriting live configuration or failing the migration on
-- it. A constraint on oms.order leaves those arrays untouched and gives the
-- storage boundary one unambiguous column to read.
--
-- What is deliberately NOT done here
-- ----------------------------------
-- Whether instrument.order_types and policy.permitted_order_types should continue
-- to list IOC and FOK is a modelling question this migration does not answer. The
-- constraint says oms.order does not accept them in order_type; it does not say an
-- instrument may not support them, which it may, because oms.order expresses them
-- through time_in_force. That is consistent, but it is a distinction a reader has to
-- be told, so it is recorded as an owner decision rather than left implicit.

BEGIN;

ALTER TABLE oms.order
    ADD CONSTRAINT order_type_not_a_time_in_force
    CHECK (order_type NOT IN ('IOC'::common.order_type, 'FOK'::common.order_type));

COMMENT ON CONSTRAINT order_type_not_a_time_in_force ON oms.order IS
    'IOC and FOK are time-in-force instructions. common.order_type still declares them '
    'because market.instrument.order_types and risk.policy.permitted_order_types are arrays '
    'of that enum and may list them as venue capabilities, but an oms.order row expresses '
    'them only through time_in_force. Two columns able to carry the same instruction is the '
    'ambiguity this constraint removes.';

-- The rule is about a column, so the comment has to be on the column too: someone
-- reading information_schema sees the enum and not the constraint.
COMMENT ON COLUMN oms.order.order_type IS
    'Venue order type. IOC and FOK are rejected here -- use time_in_force. See constraint '
    'order_type_not_a_time_in_force.';

COMMENT ON COLUMN oms.order.time_in_force IS
    'How long the order remains active. The sole expression of IOC/FOK semantics for this '
    'row; NOT NULL because an order with no stated lifetime is not an instruction.';

COMMIT;