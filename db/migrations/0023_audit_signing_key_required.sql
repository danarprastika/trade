-- 0023_audit_signing_key_required.sql
--
-- audit.record.signing_key_id is NOT NULL, and that is not the same as required.
--
-- The column has carried a comment since 0006 saying it names "the key used for the
-- checkpoint that will close this record's batch". Read that way, the value is
-- unknowable at append time and '' looks like a reasonable placeholder to fill in
-- later. It is never filled in later: there is no UPDATE against this column in any
-- migration in this repository, and audit.record is append-only besides. So the
-- placeholder was the final value, and 81% of the records in the test database
-- (1298 of 1594) were storing it.
--
-- What made this worse than an ordinary missing field is that the empty string
-- satisfies NOT NULL, so nothing failed. Every one of those records asserts
-- something happened -- an order prepared, an outcome resolved, a reconciliation
-- case opened -- while naming nothing that attests to it. domain/ledger already
-- knew this and refused an empty key in NewPoster. domain/execution and
-- services/reconcile did not, and wrote '' on every submission, every resolution
-- and every venue answer.
--
-- This migration closes the hole at the level where it cannot be reopened by a
-- future producer that forgets to check: the database. Both Go paths now refuse an
-- empty key at construction, and the CHECK below refuses the write.
--
-- Two deliberate choices:
--
-- 1. NOT VALID. 24_ENTERPRISE_RELEASE_STANDARD.md §5 requires every migration to be
--    EXPAND-only. A validated CHECK cannot be added while 1298 historical rows
--    violate it, and dropping them to make the constraint apply would be a
--    CONTRACT change -- and worse, it would delete the only record that those
--    operations happened. NOT VALID enforces every new and updated row from here
--    forward while leaving history in place, which is the correct trade: the
--    constraint is what stops the next record, and rewriting the last 1298 is not
--    this migration's authority.
--
-- 2. The historical rows are NOT backfilled, and no migration should backfill them.
--    There is no key that can be honestly attributed to a record written before one
--    was captured. Inventing a value would manufacture the appearance of
--    attestation for events that have none, which is a worse defect than the empty
--    column and much harder to discover. The 1298 rows stay unattributable, and
--    that is recorded here as a known, permanent historical gap rather than
--    papered over.
--
-- The column comment is corrected to say what the column actually holds. Leaving the
-- batch-closing description in place is what produced the empty writes in the first
-- place: it described a value the code is not able to supply and never did.

BEGIN;

ALTER TABLE audit.record
    ADD CONSTRAINT audit_signing_key_required
    CHECK (length(btrim(signing_key_id)) > 0) NOT VALID;

COMMENT ON COLUMN audit.record.signing_key_id IS
    'Key attesting to this record. Required and non-empty: supplied by the writing '
    'component at append time and never revised afterwards, so an empty value is '
    'not a placeholder but the record''s permanent, unattributable final state. '
    'The previous comment described a batch-closing key written at checkpoint time; '
    'no such back-fill exists, which is how 1298 records came to name no key.';

COMMENT ON CONSTRAINT audit_signing_key_required ON audit.record IS
    'Enforced for new and updated rows from migration 0023. NOT VALID is deliberate: '
    '1294 pre-existing rows violate it and are preserved. They are not backfilled, '
    'because no key can be honestly attributed to a record that was written without '
    'one, and fabricating a value would create false attestation. Backfill is a '
    'separate owner decision if those records ever need to be defensible.';

COMMIT;
