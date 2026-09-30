package dbtest_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/dbtest"
)

// Position reconciliation tests.
//
// Authority:
//   09_TESTING_AND_RELEASE_EVIDENCE.md invariant 5: "Portfolio positions
//       reconcile to validated fills."
//
// Before 0016 portfolio.position carried derived_from_fill_sequence and
// derived_from_ledger_sequence, both NOT NULL, and had NO TRIGGERS AT ALL. No
// function in the schema mentioned derived_from_fill. A position could be
// written with any quantity whatsoever.
//
// TestValidatedFillMustBeReconciled already proved the converse -- a fill cannot
// be marked validated without a reconciliation status -- which is necessary but
// is not what invariant 5 states. Nothing tied a position to the fills behind
// it.

// MustInsertFill records a fill. seq is the fill's position in the account's
// stream and must be supplied explicitly: it is the watermark a position
// derives from, so it is a fact the writer knows rather than a default.
func MustInsertFill(t *testing.T, ctx context.Context, tx *sql.Tx, order, ins, venue string,
	side string, quantity string, seq int, tradeID string, validated bool) {
	t.Helper()
	status := "PENDING"
	var validatedAt any
	if validated {
		status = "MATCHED"
		// validated_at is TIMESTAMPTZ, not a nanosecond integer. The fill's
		// source_timestamp_ns column is the nanosecond field; mixing the two
		// would be a silent type error rather than a compile-time one.
		validatedAt = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO oms.fill (fill_id, order_id, account_id, instrument_id, venue_id,
            side, quantity, price, fee_currency, venue_trade_id,
            source_timestamp, source_timestamp_ns, correlation_id, fill_sequence,
            validated, validated_at, reconciliation_status)
		VALUES ($1,$2,$3,$4,$5,$6::common.side,$7,100.50,'USD',$8,
                now(),$9,'cor',$10,$11,$12,$13)`,
		dbtest.CanonicalID("fil", seq*7+1), order, dbtest.CanonicalID("acc", 154), ins, venue,
		side, quantity, tradeID, dbtest.NowNs(), seq, validated, validatedAt, status); err != nil {
		t.Fatalf("insert fill seq %d: %v", seq, err)
	}
}

// MustInsertPosition writes a position. The deferred trigger will check it at
// commit, which is where the rejection surfaces -- so callers that expect a
// rejection must let the error surface, which the helper surfaces.
func MustInsertPosition(t *testing.T, ctx context.Context, tx *sql.Tx, ins, venue string,
	quantity string, watermark int) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO portfolio.position (position_id, account_id, environment, instrument_id,
            venue_id, quantity, average_entry_price, realized_pnl, unrealized_pnl,
            derived_from_fill_sequence, derived_from_ledger_sequence, built_at)
        VALUES ($1,$2,'paper',$3,$4,$5,100.50,0,0,$6,0,now())`,
		dbtest.CanonicalID("pos", watermark*13+1), dbtest.CanonicalID("acc", 154), ins, venue,
		quantity, watermark); err != nil {
		t.Fatalf("insert position: %v", err)
	}
}

// MustUpdatePosition rewrites an existing position's quantity and watermark.
//
// A position is one row per (account, environment, instrument, venue) --
// position_identity_idx is UNIQUE -- so a position is UPDATED as fills arrive
// rather than re-inserted. That is the real shape of the problem: successive
// rebuilds of the same position.
//
// The row is located by its identity key, not by a position_id derived from the
// new watermark. An earlier version derived the id from the watermark being
// written, so an update to a new watermark matched zero rows -- the UPDATE was
// a silent no-op, the deferred trigger never fired for it, and the test passed
// while exercising nothing. The affected-row count is therefore asserted.
func MustUpdatePosition(t *testing.T, ctx context.Context, tx *sql.Tx, ins, venue, quantity string, watermark int) {
	t.Helper()
	res := dbtest.MustExec(t, ctx, tx, `
        UPDATE portfolio.position
           SET quantity = $1, derived_from_fill_sequence = $2
         WHERE account_id = $3 AND environment = 'paper'
           AND instrument_id = $4 AND venue_id = $5`,
		quantity, watermark, dbtest.CanonicalID("acc", 154), ins, venue)
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("position update matched %d rows (err %v), want exactly 1; "+
			"an update that matches nothing leaves the position stale and passes vacuously",
			n, err)
	}
}

// positionFixture builds the shared account/instrument/venue/order used by these
// tests. The account and instrument ids are pinned to the same seeds
// MustInsertFill uses so the fixtures line up.
func positionFixture(t *testing.T, ctx context.Context, tx *sql.Tx) (order, ins, venue string) {
	t.Helper()
	ins = dbtest.CanonicalID("ins", 151)
	MustInsertInstrument(t, ctx, tx, ins)
	order = MustOrderAt(t, ctx, tx, ins, 152, "ACKNOWLEDGED")
	venue = dbtest.CanonicalID("ven", 155)
	return order, ins, venue
}

// TestPositionMustEqualTheValidatedFills is invariant 5.
//
// The position is written with a quantity that no set of fills supports. The
// deferred trigger rejects it at commit, which is the correct time: a position
// built from several fills in one transaction is only correct once all of them
// exist.
func TestPositionMustEqualTheValidatedFills(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)

		// Quantity 999 is not supported by a single 10-lot validated BUY.
		MustInsertPosition(t, ctx, tx, ins, venue, "999", 1)

		// Either deferred control is an acceptable and correct refusal here, and
		// which one speaks first is PostgreSQL's deferred-queue ordering rather
		// than a property of this schema. Both name the same invariant 5 breach.
		dbtest.ExpectDeferredRejected(t, ctx, tx, "imply", "was not updated")
	})
}

// TestPositionMatchingTheValidatedFillsIsAccepted proves the control has no
// false positives, and that the sign convention is the ordinary one.
func TestPositionMatchingTheValidatedFillsIsAccepted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)
		MustInsertPosition(t, ctx, tx, ins, venue, "10", 1)

		// A SELL reduces the position, so the next watermark implies 4. The
		// position is UPDATED in place, because it is one row per identity.
		MustInsertFill(t, ctx, tx, order, ins, venue, "SELL", "6", 2, "t2", true)
		MustUpdatePosition(t, ctx, tx, ins, venue, "4", 2)
		// Deferred controls only fire at COMMIT, and Resettable rolls back, so the
		// checks must be forced explicitly or this test would pass vacuously.
		dbtest.SettleDeferred(t, ctx, tx)
	})
}

// TestUnvalidatedFillsDoNotContributeToAPosition proves the control keys on
// validation, not merely on the fill existing. A fill still in reconciliation is
// not yet a financial fact, and a position must not silently include it.
func TestUnvalidatedFillsDoNotContributeToAPosition(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)
		// Unvalidated at sequence 2: must be ignored entirely.
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "500", 2, "t2", false)

		// The watermark is the latest VALIDATED fill, 1, and the quantity is 10.
		MustInsertPosition(t, ctx, tx, ins, venue, "10", 1)
		// Deferred controls only fire at COMMIT, and Resettable rolls back, so the
		// checks must be forced explicitly or this test would pass vacuously.
		dbtest.SettleDeferred(t, ctx, tx)
	})
}

// TestPositionPinnedToAnOldFillSequenceIsRejected closes the subtler hole.
//
// It would be possible to satisfy the quantity check by pinning
// derived_from_fill_sequence to an older fill and ignoring everything since,
// which is exactly the silent drift invariant 5 exists to prevent. The
// watermark is therefore required to be the LATEST validated fill.
func TestPositionPinnedToAnOldFillSequenceIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "5", 2, "t2", true)

		// Quantity 10 matches sequence 1 exactly -- and is still wrong, because
		// the position would be ignoring the fill at sequence 2.
		MustInsertPosition(t, ctx, tx, ins, venue, "10", 1)

		dbtest.ExpectDeferredRejected(t, ctx, tx, "ignores later fills")
	})
}

// TestValidatedFillIsImmutable is the control that makes the derivation
// trustworthy. If a validated fill could be edited, a previously correct
// position would silently drift with no control ever firing.
func TestValidatedFillIsImmutable(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)
		MustInsertPosition(t, ctx, tx, ins, venue, "10", 1)

		err := dbtest.ExpectRejected(t, ctx, tx,
			`UPDATE oms.fill SET quantity = 9999 WHERE fill_id = $1`,
			dbtest.CanonicalID("fil", 1*7+1))
		if !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("a validated fill was edited: %v", err)
		}
		// Deferred controls only fire at COMMIT, and Resettable rolls back, so the
		// checks must be forced explicitly or this test would pass vacuously.
		dbtest.SettleDeferred(t, ctx, tx)
	})
}

// TestValidatedFillCannotBeDeleted keeps the same guarantee against deletion.
// Losing a validated fill would leave every derived position permanently
// unreconcilable with no record of what went missing.
func TestValidatedFillCannotBeDeleted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)

		err := dbtest.ExpectRejected(t, ctx, tx,
			`DELETE FROM oms.fill WHERE fill_id = $1`, dbtest.CanonicalID("fil", 1*7+1))
		if !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("a validated fill was deleted: %v", err)
		}
		// Deferred controls only fire at COMMIT, and Resettable rolls back, so the
		// checks must be forced explicitly or this test would pass vacuously.
		dbtest.SettleDeferred(t, ctx, tx)
	})
}

// TestUnvalidatedFillEconomicTermsCannotChange draws the boundary where it
// belongs. An unvalidated fill may still have its reconciliation status
// advanced, but its quantity or price may not be quietly rewritten -- that
// would let a corrected-in-place fill masquerade as the venue's original.
func TestUnvalidatedFillEconomicTermsCannotChange(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", false)

		err := dbtest.ExpectRejected(t, ctx, tx,
			`UPDATE oms.fill SET quantity = 9999 WHERE fill_id = $1`,
			dbtest.CanonicalID("fil", 1*7+1))
		if !strings.Contains(err.Error(), "fixed once recorded") {
			t.Fatalf("an unvalidated fill's quantity was rewritten: %v", err)
		}

		// The reconciliation transition itself remains possible, which is the
		// whole purpose of an unvalidated row.
		dbtest.MustExec(t, ctx, tx, `
            UPDATE oms.fill SET validated = true, validated_at = now(),
                reconciliation_status = 'MATCHED'
             WHERE fill_id = $1`, dbtest.CanonicalID("fil", 1*7+1))
		// Deferred controls only fire at COMMIT, and Resettable rolls back, so the
		// checks must be forced explicitly or this test would pass vacuously.
		dbtest.SettleDeferred(t, ctx, tx)
	})
}

// TestTwoFillsCannotClaimTheSameSequence keeps the watermark unambiguous.
func TestTwoFillsCannotClaimTheSameSequence(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", false)
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO oms.fill (fill_id, order_id, account_id, instrument_id, venue_id,
                side, quantity, price, fee_currency, venue_trade_id,
                source_timestamp, source_timestamp_ns, correlation_id, fill_sequence)
            VALUES ($1,$2,$3,$4,$5,'BUY',7,100.50,'USD','t-dup',
                    now(),$6,'cor',1)`,
			dbtest.CanonicalID("fil", 999), order, dbtest.CanonicalID("acc", 154), ins, venue,
			dbtest.NowNs())
	})
}

// TestNewValidatedFillLeavesAStalePositionUnreconciled is the write-side half
// of invariant 5.
//
// The position trigger only fires when a position row changes. A correct
// position can be invalidated from the other direction: a new validated fill
// arrives and no position row is touched. Without the fill-side trigger that is
// a silent reconciliation break, and the transaction would commit with a
// position that no longer matches its fills.
func TestNewValidatedFillLeavesAStalePositionUnreconciled(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)
		MustInsertPosition(t, ctx, tx, ins, venue, "10", 1)

		// A second validated fill lands and the position is NOT updated. At
		// commit the position is stale and the transaction must be refused.
		//
		// In practice the position-side watermark control is the one that fires
		// here, not the fill-side quantity control, and that is worth being
		// explicit about. The fill-side trigger recomputes the expected quantity
		// through the watermark the position CLAIMS, so a stale position pinned
		// at watermark 1 still reconciles to 10 and passes the fill-side check
		// even though validated fill 2 exists and is being ignored. Only the
		// position-side check, which compares the claimed watermark against the
		// latest validated fill, catches that shape.
		//
		// The two controls are therefore complementary rather than redundant, and
		// neither can be dropped: the fill-side one catches a position whose
		// quantity was never updated, and the position-side one catches a
		// position that was made to look self-consistent at an old watermark.
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "5", 2, "t2", true)
		dbtest.ExpectDeferredRejected(t, ctx, tx, "ignores later fills", "was not updated")
	})
}

// TestPositionUpdateIsRequiredBeforeCommit proves the accepted path still works
// once the position IS updated, so the previous test is refusing a real error
// rather than every fill after the first.
func TestPositionUpdateIsRequiredBeforeCommit(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)
		MustInsertPosition(t, ctx, tx, ins, venue, "10", 1)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "5", 2, "t2", true)
		MustUpdatePosition(t, ctx, tx, ins, venue, "15", 2)
		// Deferred controls only fire at COMMIT, and Resettable rolls back, so the
		// checks must be forced explicitly or this test would pass vacuously.
		dbtest.SettleDeferred(t, ctx, tx)
	})
}

// TestPositionIsRebuiltFromSeveralFillsInOneTransaction proves the deferred
// timing is right rather than merely convenient. Row-by-row checking cannot
// express a sum over rows that do not exist yet, which is why the trigger is
// DEFERRABLE INITIALLY DEFERRED and why it runs at commit.
func TestPositionIsRebuiltFromSeveralFillsInOneTransaction(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "7", 2, "t2", true)
		MustInsertFill(t, ctx, tx, order, ins, venue, "SELL", "4", 3, "t3", true)

		// 10 + 7 - 4 = 13 across three fills in a single transaction.
		MustInsertPosition(t, ctx, tx, ins, venue, "13", 3)
		// Deferred controls only fire at COMMIT, and Resettable rolls back, so the
		// checks must be forced explicitly or this test would pass vacuously.
		dbtest.SettleDeferred(t, ctx, tx)
	})
}

// TestZeroPositionWithNoFillsIsAccepted proves an empty book is representable.
// A flat account must be able to hold a zero position, and the sum over no
// fills is zero.
func TestZeroPositionWithNoFillsIsAccepted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		_, ins, venue := positionFixture(t, ctx, tx)
		MustInsertPosition(t, ctx, tx, ins, venue, "0", 0)
		// Deferred controls only fire at COMMIT, and Resettable rolls back, so the
		// checks must be forced explicitly or this test would pass vacuously.
		dbtest.SettleDeferred(t, ctx, tx)
	})
}

// TestACommittedPositionGoingStaleOnALaterFillIsRejected is the cross-transaction
// case, and the only one that exercises the fill-side control on its own.
//
// Every other test in this file builds its scenario inside a single transaction,
// so the position-side deferred trigger fires as well. That made those tests
// unable to distinguish the two controls: deleting the fill-side trigger
// outright left the suite green, which mutation testing confirmed before these
// tests existed.
//
// The distinction is not academic. A position committed in transaction N and
// invalidated by a validated fill in transaction N+1 involves no position write
// at all, so a trigger on portfolio.position has nothing to fire on. Only the
// fill-side control can observe it. Without it a position goes stale silently
// the moment it is no longer updated in the same transaction as its fills,
// which is the normal operating pattern rather than an edge case.
func TestACommittedPositionGoingStaleOnALaterFillIsRejected(t *testing.T) {
	db := dbtest.Open(t)

	// Transaction 1: a validated fill and a position that reconciles to it, both
	// committed. The position is correct as at this point.
	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)
		MustInsertPosition(t, ctx, tx, ins, venue, "10", 1)
	})

	// Transaction 2: a new validated fill arrives and no position row is
	// written. The position still claims 10 while the fills now imply 15.
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := readCommittedPosition(t, ctx, tx)

		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "5", 2, "t2", true)
		dbtest.ExpectDeferredRejected(t, ctx, tx, "ignores later fills", "was not updated", "imply")
	})
}

// TestACommittedPositionUpdatedForALaterFillIsAccepted is the other half, and it
// matters for the same reason. A control that only ever refuses is not a control,
// it is an outage: without this test the fill-side trigger could be "fixed" by
// refusing every validated fill, and no other test in the suite would notice.
func TestACommittedPositionUpdatedForALaterFillIsAccepted(t *testing.T) {
	db := dbtest.Open(t)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)
		MustInsertPosition(t, ctx, tx, ins, venue, "10", 1)
	})

	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := readCommittedPosition(t, ctx, tx)

		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "5", 2, "t2", true)
		dbtest.MustExec(t, ctx, tx, `
			UPDATE portfolio.position
			   SET quantity = 15, derived_from_fill_sequence = 2
			 WHERE instrument_id = $1 AND venue_id = $2`, ins, venue)
		dbtest.SettleDeferred(t, ctx, tx)
	})
}

// TestACorruptedCommittedPositionIsRejectedWithNoNewFill isolates the
// position-side control.
//
// Both invariant 5 controls now perform the same two checks, because the same
// two defects are reachable from either direction. That makes them redundant in
// WHAT they check and different only in WHEN they fire -- the fill-side control
// fires on oms.fill, the position-side control on portfolio.position. In any
// test that writes both a fill and a position in one transaction, whichever
// trigger is queued first satisfies the requirement and the other is never
// observed, so neither can be shown to be load-bearing.
//
// This scenario is the one only the position-side control can catch: the
// position is corrupted by a write that touches no fill at all. That is not
// hypothetical -- it is what a manual correction, a migration, or any other
// writer that assumes positions are ordinary mutable rows would produce. With
// the fill-side control as the only check, this corruption commits silently.
func TestACorruptedCommittedPositionIsRejectedWithNoNewFill(t *testing.T) {
	db := dbtest.Open(t)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, ins, venue := positionFixture(t, ctx, tx)
		MustInsertFill(t, ctx, tx, order, ins, venue, "BUY", "10", 1, "t1", true)
		MustInsertPosition(t, ctx, tx, ins, venue, "10", 1)
	})

	// No fill is written in this transaction, so the fill-side control has
	// nothing to fire on. The position is simply restated to a quantity no
	// validated fill supports.
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		_, ins, venue := readCommittedPosition(t, ctx, tx)

		dbtest.MustExec(t, ctx, tx, `
			UPDATE portfolio.position SET quantity = 999
			 WHERE instrument_id = $1 AND venue_id = $2`, ins, venue)

		dbtest.ExpectDeferredRejected(t, ctx, tx, "imply")
	})
}

// readCommittedPosition returns the identifiers of the fill and position
// committed by an earlier dbtest.Committed call, so a second transaction can
// build on state that Resettable's rollback would otherwise discard.
func readCommittedPosition(t *testing.T, ctx context.Context, tx *sql.Tx) (order, ins, venue string) {
	t.Helper()
	if err := tx.QueryRowContext(ctx, `
		SELECT o.order_id, f.instrument_id, f.venue_id
		  FROM oms.fill f
		  JOIN oms."order" o ON o.order_id = f.order_id
		 LIMIT 1`).Scan(&order, &ins, &venue); err != nil {
		t.Fatalf("read committed fill: %v", err)
	}
	return order, ins, venue
}
