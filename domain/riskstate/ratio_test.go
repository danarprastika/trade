package riskstate

import (
	"testing"

	"github.com/aitc/trade/contracts"
)

// dec is defined once for the package in loader_test.go.

// TestDrawdownStatesTheRatioLiterally pins the two risk measurements that are
// ratios, and pins them at operands whose scales DIFFER.
//
// Both figures are compared against a policy limit, so a quotient that is wrong
// by a power of ten is a corrupted limit comparison rather than a cosmetic
// error. The database suite passed with the arithmetic broken, because it
// supplied figures whose scales happened to agree; these cases are chosen so
// that the two operands cannot agree by accident.
func TestDrawdownStatesTheRatioLiterally(t *testing.T) {
	cases := []struct {
		name         string
		equity, peak string
		want         string
	}{
		// Equal scales: the case that happened to work.
		{"equal scales", "9000.00", "10000.00", "0.10000000"},
		// The equity carries more places than the peak. 999.50 / 10000 is
		// 0.09995, and no arrangement of a power of ten produces it.
		{"decline finer than peak", "9000.50", "10000", "0.09995000"},
		{"decline finer than peak, larger", "9999.99", "10000", "0.00000100"},
		// The peak carries more places than the equity: 1000.50 / 10000.50 is
		// 0.10004500 (2001/20001).
		{"peak finer than equity", "9000", "10000.50", "0.10004500"},
		// No decline at all is zero, not a tiny positive figure.
		{"equity at peak", "10000", "10000", "0"},
		{"equity above peak", "10500", "10000", "0"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Account{Equity: dec(c.equity), PeakEquity: dec(c.peak)}
			got, err := a.Drawdown()
			if err != nil {
				t.Fatalf("Drawdown() returned error %v", err)
			}
			if want := dec(c.want); got.Cmp(want) != 0 {
				t.Errorf("Drawdown() with equity %s against peak %s = %s, want %s",
					c.equity, c.peak, got.String(), c.want)
			}
		})
	}
}

// TestDrawdownRefusesInconsistentAccounts keeps the guard that catches a bad
// fixture rather than a good one: a peak of zero has no ratio, and a figure
// above 1 cannot be a proportion.
func TestDrawdownRefusesInconsistentAccounts(t *testing.T) {
	t.Run("a peak of zero has no ratio", func(t *testing.T) {
		a := Account{Equity: dec("100"), PeakEquity: dec("0")}
		if _, err := a.Drawdown(); err == nil {
			t.Error("Drawdown() against a peak of zero returned no error")
		}
	})

	t.Run("a negative peak is refused", func(t *testing.T) {
		a := Account{Equity: dec("100"), PeakEquity: dec("-5")}
		if _, err := a.Drawdown(); err == nil {
			t.Error("Drawdown() against a negative peak returned no error")
		}
	})
}

// TestConcentrationStatesTheShareLiterally pins the second ratio, and again at
// operands whose scales differ: a position carried to two places against a
// gross exposure carried to none.
func TestConcentrationStatesTheShareLiterally(t *testing.T) {
	cases := []struct {
		name  string
		book  map[string]contracts.Decimal
		gross string
		want  string
	}{
		{
			"equal scales",
			map[string]contracts.Decimal{"a": dec("2000")},
			"20000", "0.10000000",
		},
		{
			"positions finer than gross",
			map[string]contracts.Decimal{"a": dec("2000.00")},
			"20000", "0.10000000",
		},
		{
			"the largest position, not the first, sets the share",
			map[string]contracts.Decimal{"a": dec("500.00"), "b": dec("1500.00")},
			"2000", "0.75000000",
		},
		{
			"the whole book in one instrument",
			map[string]contracts.Decimal{"a": dec("20000.00")},
			"20000", "1.00000000",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := concentration(c.book, dec(c.gross))
			if err != nil {
				t.Fatalf("concentration() returned error %v", err)
			}
			if want := dec(c.want); got.Cmp(want) != 0 {
				t.Errorf("concentration() = %s, want %s", got.String(), c.want)
			}
		})
	}
}

// TestConcentrationOfAFlatBookIsZero states the deliberate answer, because 1
// would breach every max_concentration in existence for an account that holds
// nothing.
func TestConcentrationOfAFlatBookIsZero(t *testing.T) {
	if got, err := concentration(nil, dec("0")); err != nil || got.Sign() != 0 {
		t.Errorf("concentration(nil, 0) = (%s, %v), want (0, nil)", got.String(), err)
	}
	if got, err := concentration(map[string]contracts.Decimal{"a": dec("100")}, dec("0")); err != nil || got.Sign() != 0 {
		t.Errorf("concentration with zero gross = (%s, %v), want (0, nil)", got.String(), err)
	}
}
