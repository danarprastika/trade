package dbtest_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/aitc/trade/dbtest"
)

// Tests for migration 0019: instrument capability, increment and venue-symbol
// enforcement (defects 16a, 16b, 16d).
//
// The pattern throughout is that these controls were previously absent
// entirely -- the market schema had zero triggers -- so a plain green test suite
// proved nothing about them. Each test here is written to fail if its control is
// deleted, and db/mutate_0019.ps1 proves that mechanically rather than by
// assertion.

// mustInsertConstrainedInstrument inserts an instrument with explicit
// capabilities and increments, so the constraint under test is stated in the
// test that exercises it rather than inherited from a shared helper.
func mustInsertConstrainedInstrument(t *testing.T, ctx context.Context, tx *sql.Tx, spec instrumentSpec) {
	t.Helper()
	types := spec.orderTypes
	if types == "" {
		types = "ARRAY['MARKET','LIMIT','IOC','REDUCE_ONLY','CLOSE_POSITION']::common.order_type[]"
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO market.instrument (instrument_id, market_class, base, quote,
            min_quantity, price_increment, tick_size, min_notional, order_types,
            supports_shorting, trading_status)
        VALUES ($1, $2, 'BTC', 'USDT', $3, $4, $4, $5, `+types+`, $6, $7)`,
		spec.id, spec.marketClass, spec.minQuantity, spec.priceIncrement,
		spec.minNotional, spec.supportsShorting, spec.tradingStatus); err != nil {
		t.Fatalf("insert constrained instrument %s: %v", spec.id, err)
	}
}

type instrumentSpec struct {
	id               string
	marketClass      string
	minQuantity      string
	priceIncrement   string
	minNotional      any
	orderTypes       string
	supportsShorting bool
	tradingStatus    string
}

func defaultInstrument(id string) instrumentSpec {
	return instrumentSpec{
		id: id, marketClass: "crypto",
		minQuantity: "0.00000001", priceIncrement: "0.01",
		supportsShorting: true, tradingStatus: "OPEN",
	}
}

// mustInsertOrderAt is MustCreateOrder with explicit economic terms, for the
// increment tests. MustCreateOrder hardcodes limit_price = 100.
func mustInsertOrderAt(t *testing.T, ctx context.Context, tx *sql.Tx, id, instrument, orderType, side, qty, price string, seed int) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO oms.order (order_id, command_id, idempotency_scope, environment, account_id,
            instrument_id, venue_id, side, order_type, time_in_force, quantity, limit_price,
            state, correlation_id)
        VALUES ($1,$2,'scope','paper','acc_1',$3,'ven_1',$4,$5,'DAY',$6,$7,'CREATED','cor')`,
		id, dbtest.CanonicalID("cmd", seed), instrument, side, orderType, qty, price); err != nil {
		t.Fatalf("insert order %s: %v", id, err)
	}
}

// -----------------------------------------------------------------------------
// 16a -- capabilities
// -----------------------------------------------------------------------------

// An order type the instrument does not declare is refused. Before 0019 the
// instrument's order_types array was read by nothing, so a MARKET order against
// an instrument declaring only LIMIT was accepted and would have been rejected
// by the venue adapter at best.
func TestAnOrderTypeTheInstrumentDoesNotSupportIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3000)
		mustInsertConstrainedInstrument(t, ctx, tx, defaultInstrument(ins))

		dbtest.ExpectRejectedBecause(t, ctx, tx, "does not support order_type",
			`INSERT INTO oms.order (order_id, command_id, idempotency_scope, environment, account_id,
                instrument_id, venue_id, side, order_type, time_in_force, quantity, limit_price,
                state, correlation_id)
             VALUES ($1,$2,'scope','paper','acc_1',$3,'ven_1','BUY','STOP','DAY',100,100,'CREATED','cor')`,
			dbtest.CanonicalID("ord", 3001), dbtest.CanonicalID("cmd", 3001), ins)
	})
}

// The same order against a capable instrument is accepted, so the test above is
// not passing merely because order creation is broken.
func TestASupportedOrderTypeIsAccepted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3010)
		mustInsertConstrainedInstrument(t, ctx, tx, defaultInstrument(ins))
		mustInsertOrderAt(t, ctx, tx, dbtest.CanonicalID("ord", 3011), ins,
			"LIMIT", "BUY", "100", "100.00", 3011)
	})
}

func TestAShortAgainstANonShortableInstrumentIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3020)
		spec := defaultInstrument(ins)
		spec.supportsShorting = false
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "does not support order_type",
			`INSERT INTO oms.order (order_id, command_id, idempotency_scope, environment, account_id,
                instrument_id, venue_id, side, order_type, time_in_force, quantity, limit_price,
                state, correlation_id)
             VALUES ($1,$2,'scope','paper','acc_1',$3,'ven_1','SELL','LIMIT','DAY',100,100,'CREATED','cor')`,
			dbtest.CanonicalID("ord", 3021), dbtest.CanonicalID("cmd", 3021), ins)
	})
}

// Selling to close an existing long is not shorting. This is the distinction
// that makes the shorting control safe to have: a control that blocked closing
// activity on a non-shortable instrument would trap open exposure, and would
// then be switched off.
func TestClosingActivityIsNotTreatedAsAShort(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3030)
		spec := defaultInstrument(ins)
		spec.supportsShorting = false
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		for _, ot := range []string{"CLOSE_POSITION", "REDUCE_ONLY"} {
			mustInsertOrderAt(t, ctx, tx, dbtest.CanonicalID("ord", 3031+len(ot)), ins,
				ot, "SELL", "100", "100.00", 3031+len(ot))
		}
	})
}

// -----------------------------------------------------------------------------
// 16a -- increments
// -----------------------------------------------------------------------------

func TestAQuantityOffTheIncrementGridIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3040)
		spec := defaultInstrument(ins)
		spec.minQuantity = "0.5"
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		// 0.7 is not a whole number of 0.5 increments.
		dbtest.ExpectRejectedBecause(t, ctx, tx, "not a whole multiple of min_quantity", `
            SELECT market.assert_order_terms_permitted($1,'LIMIT','BUY',0.7,100,NULL)`, ins)
	})
}

func TestAWholeNumberOfIncrementsIsAccepted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3050)
		spec := defaultInstrument(ins)
		spec.minQuantity = "0.5"
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		dbtest.MustExec(t, ctx, tx,
			`SELECT market.assert_order_terms_permitted($1,'LIMIT','BUY',1.5,100,NULL)`, ins)
	})
}

func TestAPriceOffTheTickGridIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3060)
		spec := defaultInstrument(ins)
		spec.priceIncrement = "0.25"
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		// 100.10 is not a multiple of 0.25.
		dbtest.ExpectRejectedBecause(t, ctx, tx, "not a whole multiple of price_increment", `
            SELECT market.assert_order_terms_permitted($1,'LIMIT','BUY',1,100.10,NULL)`, ins)
	})
}

func TestAnOrderBelowMinNotionalIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3070)
		spec := defaultInstrument(ins)
		spec.minNotional = "1000"
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		// 2 * 100 = 200, below the 1000 floor.
		dbtest.ExpectRejectedBecause(t, ctx, tx, "below min_notional", `
            SELECT market.assert_order_terms_permitted($1,'LIMIT','BUY',2,100,NULL)`, ins)
	})
}

func TestAnOrderAtOrAboveMinNotionalIsAccepted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3080)
		spec := defaultInstrument(ins)
		spec.minNotional = "1000"
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		dbtest.MustExec(t, ctx, tx,
			`SELECT market.assert_order_terms_permitted($1,'LIMIT','BUY',10,100,NULL)`, ins)
	})
}

// A market order against an instrument with a declared min_notional cannot be
// shown to clear the floor, because it has no price at submission. It is
// refused rather than admitted on the assumption that it will probably be big
// enough.
func TestAMarketOrderThatCannotBeValuedAgainstMinNotionalIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3090)
		spec := defaultInstrument(ins)
		spec.minNotional = "1000"
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "no limit or stop price to value it against", `
            SELECT market.assert_order_terms_permitted($1,'MARKET','BUY',1,NULL,NULL)`, ins)
	})
}

// -----------------------------------------------------------------------------
// 16a (liveness) -- tradability at the risk boundary
// -----------------------------------------------------------------------------

// The instrument must be tradable at the moment risk is granted, not merely
// when the order was created. This is why tradability is checked at the risk
// boundary rather than at INSERT: an instrument can be delisted between the two.
func TestAnInstrumentThatStoppedTradingIsRefusedAtRiskApproval(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3100)
		mustInsertConstrainedInstrument(t, ctx, tx, defaultInstrument(ins))

		order := dbtest.CanonicalID("ord", 3101)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, OrderType: "LIMIT",
			Seed: 3101, State: "CREATED", Environment: "paper",
		})
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 3102)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 3103))

		// The venue delists the instrument after the order was created.
		dbtest.MustExec(t, ctx, tx,
			`UPDATE market.instrument SET trading_status = 'DELISTED' WHERE instrument_id = $1`, ins)

		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 3104,
			"not in trading_status OPEN")
	})
}

// 16b. UNKNOWN denies. This is the control that the tautological
// feed_unknown_not_healthy CHECK claimed to provide. The tautology is gone, so
// the behaviour lives here, evaluated against a real order.
func TestAnInstrumentWithUnknownTradingStatusIsRefusedAtRiskApproval(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3110)
		spec := defaultInstrument(ins)
		spec.tradingStatus = "UNKNOWN"
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		order := dbtest.CanonicalID("ord", 3111)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, OrderType: "LIMIT",
			Seed: 3111, State: "CREATED", Environment: "paper",
		})
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 3112)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 3113))

		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 3114,
			"not in trading_status OPEN")
	})
}

// A halted instrument is the most likely non-OPEN state in practice, and is
// covered explicitly so the deny set is pinned rather than inferred.
func TestAHaltedInstrumentIsRefusedAtRiskApproval(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3120)
		spec := defaultInstrument(ins)
		spec.tradingStatus = "HALTED"
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		order := dbtest.CanonicalID("ord", 3121)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, OrderType: "LIMIT",
			Seed: 3121, State: "CREATED", Environment: "paper",
		})
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 3122)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 3123))

		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 3124,
			"not in trading_status OPEN")
	})
}

// Tradability is enforced as a closed set, not as a list of known-bad states,
// so a state added to the enum later denies by default. Every non-OPEN state is
// pinned individually here so that claim is tested rather than asserted.
func TestEveryNonOpenTradingStatusDenies(t *testing.T) {
	nonOpen := []string{"CLOSED", "HALTED", "AUCTION", "SESSION_BREAK",
		"PRE_OPEN", "POST_CLOSE", "DELISTED", "UNKNOWN"}

	for i, status := range nonOpen {
		t.Run(status, func(t *testing.T) {
			db := dbtest.Open(t)
			dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
				ins := dbtest.CanonicalID("ins", 3200+i)
				spec := defaultInstrument(ins)
				spec.tradingStatus = status
				mustInsertConstrainedInstrument(t, ctx, tx, spec)

				var tradable bool
				dbtest.MustQueryRow(t, ctx, tx, &tradable,
					`SELECT market.instrument_is_tradable($1)`, ins)
				if tradable {
					t.Fatalf("trading_status %s reported tradable; only OPEN may be tradable", status)
				}
			})
		})
	}
}

// Risk-reducing activity is not screened for tradability. An instrument that
// stopped trading is exactly the case where an operator must be able to
// flatten, and blocking it would convert a market event into an open position.
func TestTradabilityDoesNotBlockRiskReducingActivity(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 3210)
		spec := defaultInstrument(ins)
		spec.tradingStatus = "HALTED"
		mustInsertConstrainedInstrument(t, ctx, tx, spec)

		order := dbtest.CanonicalID("ord", 3211)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, OrderType: "REDUCE_ONLY",
			Seed: 3211, State: "CREATED", Environment: "paper",
		})
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 3212)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 3213))
		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 3214)
	})
}

// -----------------------------------------------------------------------------
// M1 -- the sequence watermark cannot move backwards
// -----------------------------------------------------------------------------

func mustInsertFeedHealth(t *testing.T, ctx context.Context, tx *sql.Tx, key string, observed any) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO market.feed_health (feed_key, environment, venue_id,
            health_state, observed_sequence, expected_sequence)
        VALUES ($1, 'paper', $2, 'HEALTHY', $3, 1000)`, key, venueID(7800), observed); err != nil {
		t.Fatalf("insert feed health %s: %v", key, err)
	}
}

// A sequence watermark that moves backwards means a report older than one
// already recorded has overwritten a newer observation. The gap counter is fed
// from observed_sequence, so a rewind makes the feed report healthy on a stream
// that is replaying old data.
func TestASequenceWatermarkCannotMoveBackwards(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		mustInsertFeedHealth(t, ctx, tx, "feed_1", 500)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "cannot move backwards",
			`UPDATE market.feed_health SET observed_sequence = 400 WHERE feed_key = 'feed_1'`)
	})
}

func TestASequenceWatermarkMayAdvance(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		mustInsertFeedHealth(t, ctx, tx, "feed_2", 500)

		dbtest.MustExec(t, ctx, tx,
			`UPDATE market.feed_health SET observed_sequence = 900 WHERE feed_key = 'feed_2'`)
	})
}

// The first observation of a feed has nothing to regress from, so a NULL
// watermark may be set freely. Without this the control would refuse the very
// first update a feed ever receives.
func TestTheFirstWatermarkCanBeSetFromNull(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		mustInsertFeedHealth(t, ctx, tx, "feed_3", nil)

		dbtest.MustExec(t, ctx, tx,
			`UPDATE market.feed_health SET observed_sequence = 10 WHERE feed_key = 'feed_3'`)
	})
}

// -----------------------------------------------------------------------------
// 16d -- venue symbol mapping is append-only once effective
// -----------------------------------------------------------------------------

// venueID returns a venue identifier in the form market.venue_symbol requires:
// three lowercase letters, an underscore, then 20 Crockford characters. The
// rest of the suite uses short readable venue strings, which market.venue_symbol
// does not accept -- the constraint is real and predates this file.
func venueID(n int) string {
	return dbtest.CanonicalID("ven", n)
}

func mustInsertVenueSymbol(t *testing.T, ctx context.Context, tx *sql.Tx, instrument, venue, symbol, effective string, version int) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO market.venue_symbol (instrument_id, venue_id, symbol, mapping_version,
            effective_at, changed_by, change_reason)
        VALUES ($1,$2,$3,$4,$5,'op_1','initial mapping')`,
		instrument, venue, symbol, version, effective); err != nil {
		t.Fatalf("insert venue symbol %s/%s v%d: %v", venue, symbol, version, err)
	}
}

// Repointing an effective mapping at a different instrument is the failure this
// table exists to prevent: an order routed by the mutated mapping reaches a
// different instrument than the strategy reasoned about, and the fill arrives
// against the wrong instrument.
func TestAnEffectiveVenueSymbolCannotBeRepointedAtAnotherInstrument(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		a := dbtest.CanonicalID("ins", 3300)
		b := dbtest.CanonicalID("ins", 3301)
		MustInsertInstrument(t, ctx, tx, a)
		MustInsertInstrument(t, ctx, tx, b)

		mustInsertVenueSymbol(t, ctx, tx, a, venueID(7700), "BTCUSDT",
			time.Now().Add(-time.Hour).Format(time.RFC3339), 1)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "identity is frozen",
			`UPDATE market.venue_symbol SET instrument_id = $3
              WHERE instrument_id = $1 AND venue_id = $2 AND mapping_version = 1`,
			a, venueID(7700), b)
	})
}

func TestAnEffectiveVenueSymbolCannotBeRenamed(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		a := dbtest.CanonicalID("ins", 3310)
		MustInsertInstrument(t, ctx, tx, a)
		mustInsertVenueSymbol(t, ctx, tx, a, venueID(7700), "BTCUSDT",
			time.Now().Add(-time.Hour).Format(time.RFC3339), 1)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "identity is frozen",
			`UPDATE market.venue_symbol SET symbol = 'ETHUSDT'
              WHERE instrument_id = $1 AND venue_id = $2`, a, venueID(7700))
	})
}

// Moving effective_at is the same defect as rewriting the symbol: it changes
// when a mapping became true, which is what makes history reconstructible.
func TestAnEffectiveVenueSymbolCannotMoveItsEffectiveAt(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		a := dbtest.CanonicalID("ins", 3320)
		MustInsertInstrument(t, ctx, tx, a)
		mustInsertVenueSymbol(t, ctx, tx, a, venueID(7700), "BTCUSDT",
			time.Now().Add(-time.Hour).Format(time.RFC3339), 1)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "cannot have its effective_at changed",
			`UPDATE market.venue_symbol SET effective_at = now() - interval '30 days'
              WHERE instrument_id = $1 AND venue_id = $2`, a, venueID(7700))
	})
}

func TestAnEffectiveVenueSymbolCannotBeDeleted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		a := dbtest.CanonicalID("ins", 3330)
		MustInsertInstrument(t, ctx, tx, a)
		mustInsertVenueSymbol(t, ctx, tx, a, venueID(7700), "BTCUSDT",
			time.Now().Add(-time.Hour).Format(time.RFC3339), 1)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "cannot be deleted",
			`DELETE FROM market.venue_symbol WHERE instrument_id = $1 AND venue_id = $2`,
			a, venueID(7700))
	})
}

// Retirement and provenance must stay writable. A mapping that cannot be retired
// is a mapping that stays in force forever, which is a worse failure than one
// that is edited -- and a control this strict would be disabled by the operator
// on day one.
func TestAnEffectiveVenueSymbolCanStillBeRetired(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		a := dbtest.CanonicalID("ins", 3340)
		MustInsertInstrument(t, ctx, tx, a)
		mustInsertVenueSymbol(t, ctx, tx, a, venueID(7700), "BTCUSDT",
			time.Now().Add(-time.Hour).Format(time.RFC3339), 1)

		dbtest.MustExec(t, ctx, tx, `
            UPDATE market.venue_symbol
               SET retired_at = now(), change_reason = 'venue delisted the symbol'
             WHERE instrument_id = $1 AND venue_id = $2`, a, venueID(7700))
	})
}

// A future-dated mapping is a scheduled change, not yet a fact, and may be
// corrected freely. Freezing it would make a mistyped scheduled change
// uncorrectable except by deletion, which the control also forbids.
func TestAFutureDatedVenueSymbolMappingCanBeCorrected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		a := dbtest.CanonicalID("ins", 3350)
		b := dbtest.CanonicalID("ins", 3351)
		MustInsertInstrument(t, ctx, tx, a)
		MustInsertInstrument(t, ctx, tx, b)

		mustInsertVenueSymbol(t, ctx, tx, a, venueID(7700), "BTCUSDT",
			time.Now().Add(48*time.Hour).Format(time.RFC3339), 1)

		dbtest.MustExec(t, ctx, tx, `
            UPDATE market.venue_symbol SET instrument_id = $3, symbol = 'BTCUSD'
             WHERE instrument_id = $1 AND venue_id = $2`, a, venueID(7700), b)
	})
}

// Supersession is how a mapping is legitimately changed. If this were refused
// the append-only control would make correction impossible and would have to be
// disabled.
func TestAVenueSymbolMappingCanBeSupersededByANewVersion(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		a := dbtest.CanonicalID("ins", 3360)
		MustInsertInstrument(t, ctx, tx, a)
		earlier := time.Now().Add(-time.Hour).Format(time.RFC3339)
		mustInsertVenueSymbol(t, ctx, tx, a, venueID(7700), "BTCUSDT", earlier, 1)

		// The new version starts when the old one ended.
		mustInsertVenueSymbol(t, ctx, tx, a, venueID(7700), "BTCUSDT2",
			time.Now().Add(-time.Minute).Format(time.RFC3339), 2)

		var versions int
		dbtest.MustQueryRow(t, ctx, tx, &versions, `
            SELECT count(*) FROM market.venue_symbol
             WHERE instrument_id = $1 AND venue_id = $2`, a, venueID(7700))
		if versions != 2 {
			t.Fatalf("expected 2 versions after supersession, got %d", versions)
		}
	})
}
