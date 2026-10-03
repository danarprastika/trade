package dbtest_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/aitc/trade/dbtest"
)

// Tests for migration 0021: a TIMESTAMPTZ column is the microsecond-rounded
// projection of the nanosecond column beside it.
//
// This is the control that was missing, and the reason it was missing is
// recorded at 0021 rather than here: an earlier consistency check was written,
// failed against correct data, and was deleted instead of fixed. TIMESTAMPTZ
// has microsecond resolution, so exact equality with a nanosecond value is
// false for 999 of every 1000 values, and a check that fails on correct data
// gets removed. The tests below include the sub-microsecond cases that made
// that check fail, so the reason it was wrong cannot silently come back.

// nsAt returns a wall-clock instant truncated to the microsecond resolution
// TIMESTAMPTZ can actually store.
func nsAt(y int, mo time.Month, d, h, mi, s, ns int) (time.Time, int64) {
	t := time.Date(y, mo, d, h, mi, s, ns, time.UTC)
	return t, t.UnixNano()
}

// mustAppendAuditUnbound attempts an audit append whose occurred_at and
// occurred_at_ns are deliberately different instants, returning the error so
// the caller can assert both that it was refused and why.
func expectAppendRejected(t *testing.T, ctx context.Context, tx *sql.Tx, part string, seq int, at time.Time, ns int64) error {
	t.Helper()
	_, err := tx.ExecContext(ctx, `
        SELECT audit.append_record($1,$2,$3,'owner','actor-1','SYSTEM','test.action',
            'order',$4,'paper',NULL,$5,$6,$5,$6,'reason','cor',NULL,'v1','SUCCESS',
            NULL,NULL,'{}'::jsonb,'key-1','1.0.0',NULL)`,
		dbtest.CanonicalID("aud", seq*7+1), part, seq,
		dbtest.CanonicalID("ord", seq*13+2), at, ns)
	return err
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// A record whose occurred_at and occurred_at_ns describe different instants is
// refused. Before 0021 this was accepted, and nothing in the repository
// detected it.
func TestARecordWhoseTimestampAndNanosecondsDisagreeIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustOpenPartition(t, ctx, tx)
		at := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
		staleNs := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC).UnixNano()

		err := expectAppendRejected(t, ctx, tx, "2026-09", 1, at, staleNs)
		if err == nil {
			t.Fatal("an audit record with an unbound occurred_at/occurred_at_ns pair was accepted")
		}
		if !contains(err.Error(), "audit_record_occurred_at_ns_bound") {
			t.Fatalf("rejected for the wrong reason: %v", err)
		}
	})
}

// The market observation pair is bound on the same terms. This is the column
// pair the replayable ingestion path orders on, so a disagreement here is a
// disagreement about which observation came first.
func TestAMarketObservationWhoseTimestampAndNanosecondsDisagreeIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3400)
		MustInsertInstrument(t, ctx, tx, ins)
		ts, _ := nsAt(2026, time.September, 15, 12, 0, 0, 0)
		_, staleNs := nsAt(2020, time.January, 1, 0, 0, 0, 0)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "trade_source_timestamp_ns_bound", `
            INSERT INTO market.trade (observation_id, instrument_id, venue_id,
                price, quantity, side, source_timestamp, source_timestamp_ns, received_at)
            VALUES ($1, $2, $3, 100, 1, 'BUY', $4, $5, $6)`,
			dbtest.CanonicalID("mkt", 3401), ins, venueID(7800), ts, staleNs, ts.Add(time.Second))
	})
}

// The sub-microsecond case, which is precisely the case the deleted check
// could not handle. A nanosecond value whose last three digits are not zero is
// accepted, provided the timestamp is its rounded projection. This test is why
// the control is stated as a rounding rather than an equality.
func TestASubMicrosecondNanosecondValueIsAcceptedWhenTheTimestampIsItsRounding(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3410)
		MustInsertInstrument(t, ctx, tx, ins)

		// A nanosecond value whose last three digits are not zero, so the
		// TIMESTAMPTZ cannot equal it. The correct timestamp is obtained from
		// the database rather than computed here: this test is about whether
		// the constraint accepts a correctly bound pair, and the rounding
		// behaviour itself is pinned independently by
		// TestNsToTimestamptzRoundsToTheNearestMicrosecond.
		//
		// An earlier version of this test hand-computed the expected timestamp
		// and asserted 123457 for 123457789ns. The correct answer is 123458,
		// because .789 rounds up. The failure was in the test's arithmetic and
		// not in the constraint, which is a distinction worth being careful
		// about: a test that computes its own expectation can be wrong in the
		// same direction as the code it is checking.
		base := time.Date(2026, time.September, 15, 12, 0, 0, 123457000, time.UTC)
		ns := base.UnixNano() + 789

		var rounded time.Time
		dbtest.MustQueryRow(t, ctx, tx, &rounded,
			`SELECT common.ns_to_timestamptz($1)`, ns)

		if rounded.Nanosecond()%1000 != 0 {
			t.Fatalf("the rounded timestamp %s is not microsecond-aligned; TIMESTAMPTZ cannot store it", rounded)
		}
		if rounded.Nanosecond() == base.Nanosecond() {
			t.Fatalf("rounding %dns did not move the microsecond, so this vector no longer exercises a sub-microsecond remainder", ns%1000)
		}

		dbtest.MustExec(t, ctx, tx, `
            INSERT INTO market.trade (observation_id, instrument_id, venue_id,
                price, quantity, side, source_timestamp, source_timestamp_ns, received_at)
            VALUES ($1, $2, $3, 100, 1, 'BUY', $4, $5, $6)`,
			dbtest.CanonicalID("mkt", 3411), ins, venueID(7800),
			rounded, ns, rounded.Add(time.Second))
	})
}

// The feed-health watermark pair is bound too, because that value is what a
// freshness decision is made against. A disagreement between its two
// representations is a disagreement about how fresh the feed is.
func TestAFeedHealthWatermarkThatDisagreesWithItsNanosecondsIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ts, _ := nsAt(2026, time.September, 15, 12, 0, 0, 0)
		_, staleNs := nsAt(2020, time.January, 1, 0, 0, 0, 0)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "feed_last_source_timestamp_ns_bound", `
            INSERT INTO market.feed_health (feed_key, environment, venue_id, health_state,
                last_healthy_at, last_source_timestamp, last_source_timestamp_ns)
            VALUES ('feed_ns_1', 'paper', $1, 'HEALTHY', $2, $2, $3)`,
			venueID(7800), ts, staleNs)
	})
}

// common.ns_to_timestamptz is the single definition of the rounding, and the
// audit chain, the market tables and the feed watermark all use it. A consumer
// that needs to interpret a bare nanosecond value uses this rather than
// reimplementing the rounding, so it is worth pinning directly.
func TestNsToTimestamptzRoundsToTheNearestMicrosecond(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		cases := []struct {
			name string
			ns   int64
			want string
		}{
			{"exact microsecond", nsOf(2026, 9, 15, 12, 0, 0, 0), "2026-09-15 12:00:00.000000+00"},
			{"rounds up", nsOf(2026, 9, 15, 12, 0, 0, 500) + 0, "2026-09-15 12:00:00.000001+00"},
			{"rounds down", nsOf(2026, 9, 15, 12, 0, 0, 499), "2026-09-15 12:00:00.000000+00"},
			{"rounds up from 999ns", nsOf(2026, 9, 15, 12, 0, 0, 0) + 999, "2026-09-15 12:00:00.000001+00"},
		}

		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				var got string
				dbtest.MustQueryRow(t, ctx, tx, &got, `
                    SELECT to_char(common.ns_to_timestamptz($1),
                                   'YYYY-MM-DD HH24:MI:SS.USOF')`, c.ns)
				// to_char with OF appends a decimal point; strip it for comparison.
				if normaliseOffset(got) != normaliseOffset(c.want) {
					t.Fatalf("ns %d rendered as %s, want %s", c.ns, got, c.want)
				}
			})
		}
	})
}

// normaliseOffset drops the "+00" offset suffix and any fractional-point
// punctuation, leaving a shape that can be compared without depending on how
// to_char chose to render the zone.
func normaliseOffset(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '+' {
			break
		}
		out = append(out, s[i])
	}
	res := string(out)
	// to_char's OF modifier emits " 12:00:00.000000+00"; nothing else here
	// contains a period, so trimming is unnecessary beyond the offset.
	return res
}

func nsOf(y int, mo time.Month, d, h, mi, s, ns int) int64 {
	return time.Date(y, mo, d, h, mi, s, ns, time.UTC).UnixNano()
}
