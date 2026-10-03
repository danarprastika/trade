package instrument

import (
	"strings"
	"testing"

	"github.com/aitc/trade/contracts"
)

func dec(s string) contracts.Decimal { return contracts.MustParseDecimal(s) }

// baseInstrument is a tradable, long-only instrument on a 0.1 lot and a 0.01
// price grid, with no declared notional floor.
func baseInstrument() Instrument {
	return Instrument{
		ID:             "ins_0123456789abcdefghjkmnpqrs",
		MarketClass:    "spot",
		TradingStatus:  tradingStatusOpen,
		MinQuantity:    "0.1",
		PriceIncrement: "0.01",
		TickSize:       "0.01",
		OrderTypes:     []string{"LIMIT", "MARKET", "REDUCE_ONLY", "CLOSE_POSITION"},
	}
}

// TestIncrementMultipleStatesTheGridRuleLiterally writes each expectation out
// instead of recomputing it, so that a change to the implementation cannot
// quietly agree with a change to this table. The rule is SQL's:
//
//	value / increment = trunc(value / increment)
func TestIncrementMultipleStatesTheGridRuleLiterally(t *testing.T) {
	cases := []struct {
		value, increment string
		want             bool
	}{
		// On the grid.
		{"2", "1", true},
		{"0.7", "0.1", true},
		{"1.5", "0.5", true},
		{"0", "1", true},
		{"-2", "1", true},
		{"12345678.9", "0.1", true},
		{"0.0001", "0.0001", true},
		// Off the grid.
		{"0.75", "0.1", false},
		{"1.4", "1", false},
		{"1", "3", false},
		{"0.0001", "0.001", false},
		{"-1.5", "1", false},
		{"2.0001", "0.1", false},
	}

	for _, c := range cases {
		got, err := IncrementMultiple(dec(c.value), dec(c.increment))
		if err != nil {
			t.Errorf("IncrementMultiple(%s, %s) returned error %v, want a verdict", c.value, c.increment, err)
			continue
		}
		if got != c.want {
			t.Errorf("IncrementMultiple(%s, %s) = %v, want %v", c.value, c.increment, got, c.want)
		}
	}
}

// TestAWholeMultipleRoundTripsAndARemainderNeverDoes states the soundness
// argument as a property rather than as a table, because that argument is what
// licenses the divide-and-multiply implementation: a quantity that reconstructs
// itself is necessarily a whole multiple, and a whole multiple always
// reconstructs itself.
func TestAWholeMultipleRoundTripsAndARemainderNeverDoes(t *testing.T) {
	increment := dec("0.25")
	for i := 0; i <= 40; i++ {
		whole, err := increment.Mul(dec(strconvItoa(i)))
		if err != nil {
			t.Fatalf("building whole multiple %d: %v", i, err)
		}

		got, err := IncrementMultiple(whole, increment)
		if err != nil {
			t.Fatalf("IncrementMultiple(%s, 0.25) returned error %v", whole.String(), err)
		}
		if !got {
			t.Fatalf("IncrementMultiple(%s, 0.25) = false, want true", whole.String())
		}

		// A hundredth below a whole multiple is off grid, and must not round-trip.
		difference, err := dec("0.01").Sub(whole)
		if err != nil {
			t.Fatalf("building off-grid neighbour of %s: %v", whole.String(), err)
		}
		offGrid := difference.Abs()
		got, err = IncrementMultiple(offGrid, increment)
		if err != nil {
			t.Fatalf("IncrementMultiple(%s, 0.25) returned error %v", offGrid.String(), err)
		}
		if got {
			t.Fatalf("IncrementMultiple(%s, 0.25) = true, want false", offGrid.String())
		}
	}
}

// TestAnUnknownIncrementIsRefusedRatherThanDivided covers the deny condition the
// schema's CHECK constraints normally make unreachable. A caller that bypassed
// them must still be refused, and must be refused with a reason naming the
// increment rather than with a division error.
func TestAnUnknownIncrementIsRefusedRatherThanDivided(t *testing.T) {
	for _, increment := range []string{"0", "-1", "0.000"} {
		got, err := IncrementMultiple(dec("1"), dec(increment))
		if got {
			t.Errorf("IncrementMultiple(1, %s) = true, want false", increment)
		}
		if err == nil {
			t.Errorf("IncrementMultiple(1, %s) returned no error, want a refusal naming the increment", increment)
			continue
		}
		if !strings.Contains(err.Error(), "not positive") {
			t.Errorf("IncrementMultiple(1, %s) error = %q, want it to say the increment is not positive", increment, err)
		}
	}
}

// TestTradableIsExactlyTheOpenStatus enumerates the whole closed vocabulary,
// because the interesting half of this control is the statuses it denies and
// UNKNOWN is the default the column ships with.
func TestTradableIsExactlyTheOpenStatus(t *testing.T) {
	want := map[string]bool{
		"OPEN": true, "UNKNOWN": false, "AUCTION": false, "PRE_OPEN": false,
		"POST_CLOSE": false, "SESSION_BREAK": false, "CLOSED": false,
		"HALTED": false, "DELISTED": false,
	}
	for status, expected := range want {
		in := baseInstrument()
		in.TradingStatus = status
		got, reason := in.Tradable()
		if got != expected {
			t.Errorf("Tradable(%s) = %v, want %v", status, got, expected)
		}
		if !got && !strings.Contains(reason, status) {
			t.Errorf("Tradable(%s) reason %q does not name the status", status, reason)
		}
		if got && reason != "" {
			t.Errorf("Tradable(%s) returned a reason %q with a permissive verdict", status, reason)
		}
	}
}

// TestPermitsRequiresAnExplicitCapability covers every branch of
// market.instrument_permits: the type must be declared, a short side must be
// supported, and a closing action is not a short.
func TestPermitsRequiresAnExplicitCapability(t *testing.T) {
	cases := []struct {
		name      string
		types     []string
		shorting  bool
		orderType string
		side      string
		want      bool
	}{
		{"declared limit buy is permitted", []string{"LIMIT", "MARKET"}, false, "LIMIT", "BUY", true},
		{"undeclared stop is refused", []string{"LIMIT", "MARKET"}, false, "STOP", "BUY", false},
		{"an instrument declaring nothing permits nothing", nil, false, "LIMIT", "BUY", false},
		{"sell is short where shorting is unsupported", []string{"LIMIT"}, false, "LIMIT", "SELL", false},
		{"sell is permitted where shorting is supported", []string{"LIMIT"}, true, "LIMIT", "SELL", true},
		{"reduce only sell is not shorting", []string{"REDUCE_ONLY"}, false, "REDUCE_ONLY", "SELL", true},
		{"close position sell is not shorting", []string{"CLOSE_POSITION"}, false, "CLOSE_POSITION", "SELL", true},
		{"a declared market sell is short where shorting is unsupported", []string{"MARKET"}, false, "MARKET", "SELL", false},
		{"an undeclared closing action is still refused", []string{"LIMIT"}, false, "REDUCE_ONLY", "SELL", false},
		// An unrecognised side must not be read as a buy. The shorting check is
		// keyed on side == "SELL", so without this a typo would pass through it
		// and an instrument that cannot be shorted would permit a short.
		{"an absent side is refused", []string{"LIMIT"}, false, "LIMIT", "", false},
		{"a misspelled side is refused", []string{"LIMIT"}, false, "LIMIT", "Sell", false},
		{"the long form of a side is refused", []string{"LIMIT"}, false, "LIMIT", "SHORT", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := baseInstrument()
			in.OrderTypes = c.types
			in.SupportsShorting = c.shorting
			got, _ := in.Permits(c.orderType, c.side)
			if got != c.want {
				t.Errorf("Permits(%s, %s) = %v, want %v", c.orderType, c.side, got, c.want)
			}
		})
	}
}

// TestPreciseNamesTheFirstUnmetBound pins the order SQL checks in, because a
// refusal that names a later bound than the real one sends an operator to
// adjust the wrong term.
func TestPreciseNamesTheFirstUnmetBound(t *testing.T) {
	t.Run("the quantity grid is checked before the price grid", func(t *testing.T) {
		in := baseInstrument()
		ok, reason := in.Precise(Terms{OrderType: "LIMIT", Side: "BUY", Quantity: "0.75", LimitPrice: "100.005"})
		if ok {
			t.Fatal("off-grid quantity and price were permitted")
		}
		if !strings.Contains(reason, "min_quantity") {
			t.Errorf("reason %q names the price grid, want the quantity grid first", reason)
		}
	})

	t.Run("an off grid price is named as such", func(t *testing.T) {
		in := baseInstrument()
		ok, reason := in.Precise(Terms{OrderType: "LIMIT", Side: "BUY", Quantity: "1", LimitPrice: "100.005"})
		if ok {
			t.Fatal("off-grid price was permitted")
		}
		if !strings.Contains(reason, "price_increment") {
			t.Errorf("reason %q does not name the price increment", reason)
		}
	})

	t.Run("the stop price prices an order carrying no limit price", func(t *testing.T) {
		in := baseInstrument()
		if ok, reason := in.Precise(Terms{OrderType: "STOP", Side: "BUY", Quantity: "1", StopPrice: "100.00"}); !ok {
			t.Errorf("on-grid stop order refused: %s", reason)
		}
	})

	t.Run("a declared floor an order cannot be valued against denies", func(t *testing.T) {
		in := baseInstrument()
		in.HasMinNotional = true
		in.MinNotional = dec("50")
		ok, reason := in.Precise(Terms{OrderType: "MARKET", Side: "BUY", Quantity: "1"})
		if ok {
			t.Fatal("an unvaluable order cleared a declared floor")
		}
		if !strings.Contains(reason, "no limit or stop price") {
			t.Errorf("reason %q does not explain that the order cannot be valued", reason)
		}
	})

	t.Run("notional below a declared floor denies", func(t *testing.T) {
		in := baseInstrument()
		in.HasMinNotional = true
		in.MinNotional = dec("500")
		ok, reason := in.Precise(Terms{OrderType: "LIMIT", Side: "BUY", Quantity: "1", LimitPrice: "100.00"})
		if ok {
			t.Fatal("notional of 100 cleared a floor of 500")
		}
		if !strings.Contains(reason, "below min_notional") {
			t.Errorf("reason %q does not name the floor", reason)
		}
	})

	t.Run("notional exactly at the floor is permitted", func(t *testing.T) {
		in := baseInstrument()
		in.HasMinNotional = true
		in.MinNotional = dec("500")
		if ok, reason := in.Precise(Terms{OrderType: "LIMIT", Side: "BUY", Quantity: "5", LimitPrice: "100.00"}); !ok {
			t.Errorf("notional equal to the floor refused: %s", reason)
		}
	})

	t.Run("an order on the grid with no declared floor is permitted", func(t *testing.T) {
		in := baseInstrument()
		if ok, reason := in.Precise(Terms{OrderType: "LIMIT", Side: "BUY", Quantity: "1.5", LimitPrice: "100.00"}); !ok {
			t.Errorf("on-grid limit order refused: %s", reason)
		}
	})

	t.Run("a quantity that is not a decimal is refused before any arithmetic", func(t *testing.T) {
		in := baseInstrument()
		ok, reason := in.Precise(Terms{OrderType: "LIMIT", Side: "BUY", Quantity: "one", LimitPrice: "100.00"})
		if ok {
			t.Fatal("a non-decimal quantity was permitted")
		}
		if !strings.Contains(reason, "not a decimal") {
			t.Errorf("reason %q does not name the unparseable quantity", reason)
		}
	})

	t.Run("an unknown quantity increment denies", func(t *testing.T) {
		in := baseInstrument()
		in.MinQuantity = "0"
		ok, reason := in.Precise(Terms{OrderType: "LIMIT", Side: "BUY", Quantity: "1", LimitPrice: "100.00"})
		if ok {
			t.Fatal("an order was permitted against a zero increment")
		}
		if !strings.Contains(reason, "not positive") {
			t.Errorf("reason %q does not name the increment as unusable", reason)
		}
	})
}

// strconvItoa keeps the property test independent of strconv so that a reader
// comparing it against IncrementMultiple's arithmetic is comparing two things,
// not three.
func strconvItoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}
