package contracts

import (
	"math/big"
	"testing"
)

// TestDivStatesTheQuotientLiterally writes each expectation out rather than
// recomputing it, so a change to Div cannot quietly agree with a change to this
// table.
//
// The cases cover the property the defect violated: the quotient is a fact about
// the two VALUES and is independent of the scales at which each happens to be
// written. Several operands are deliberately the same numbers carried at
// different scales, because an implementation that folds the scale difference
// incorrectly agrees with itself on every one of them.
func TestDivStatesTheQuotientLiterally(t *testing.T) {
	cases := []struct{ value, divisor, want string }{
		// Equal scales.
		{"2", "1", "2"},
		{"0.7", "0.1", "7"},
		{"1.5", "0.5", "3"},
		// The divisor carries more places than the value.
		{"1", "0.1", "10"},
		{"5", "0.1", "50"},
		{"1", "0.01", "100"},
		{"10", "0.1", "100"},
		// The value carries more places than the divisor.
		{"0.70", "0.1", "7"},
		{"1.50", "0.25", "6"},
		// Equal values, unequal scales: the same answer as the rows above.
		{"0.7", "0.10", "7"},
		{"1.0", "0.100", "10"},
		// Neither value is a whole multiple, and the quotient rounds.
		{"1", "2", "0.5"},
		{"1", "3", "0.33333333"},
		{"100", "250", "0.4"},
		{"2", "7", "0.28571429"},
	}

	for _, c := range cases {
		got, err := MustParseDecimal(c.value).Div(MustParseDecimal(c.divisor), 8, RoundHalfEven)
		if err != nil {
			t.Errorf("Div(%s, %s) returned error %v", c.value, c.divisor, err)
			continue
		}
		// Compared numerically, not as strings: the quotient is returned at the
		// requested target scale, so 2/1 legitimately renders as 2.00000000 and a
		// string comparison would be asserting the rendering rather than the
		// value.
		if want := MustParseDecimal(c.want); got.Cmp(want) != 0 {
			t.Errorf("Div(%s, %s) = %s, want %s", c.value, c.divisor, got.String(), c.want)
		}
	}
}

// TestDivIsScaleInvariantInBothOperands states the invariance property directly:
// carrying either operand at a finer scale, without changing its value, must not
// change the quotient. This is the property the previous implementation failed,
// and stating it as a property rather than as a table means a future
// regression is caught even for scale combinations nobody thought to list.
func TestDivIsScaleInvariantInBothOperands(t *testing.T) {
	pairs := [][2]string{{"0.7", "0.1"}, {"1", "0.01"}, {"12.5", "0.05"}, {"100", "250"}}

	for _, p := range pairs {
		value := MustParseDecimal(p[0])
		divisor := MustParseDecimal(p[1])
		base, err := value.Div(divisor, 8, RoundHalfEven)
		if err != nil {
			t.Fatalf("Div(%s, %s) returned error %v", p[0], p[1], err)
		}

		for _, shift := range []int32{1, 6, 12} {
			finerValue := Decimal{
				coef:  new(big.Int).Mul(value.coeff(), pow10(shift)),
				scale: value.scale + shift,
			}
			finerDivisor := Decimal{
				coef:  new(big.Int).Mul(divisor.coeff(), pow10(shift)),
				scale: divisor.scale + shift,
			}

			got, err := finerValue.Div(finerDivisor, 8, RoundHalfEven)
			if err != nil {
				t.Fatalf("Div(rescaled %s, rescaled %s) returned error %v", p[0], p[1], err)
			}
			if got.Cmp(base) != 0 {
				t.Errorf("Div(%s, %s) = %s, but carrying both operands at scale +%d gives %s; "+
					"the quotient is a fact about the values, not their scales",
					p[0], p[1], base.String(), shift, got.String())
			}
		}
	}
}

// TestDivOfAWholeMultipleIsTheExactInteger covers the case the instrument
// grid depends on: an exact quotient must come back whole, with no rounding
// and no residue, however the two operands are scaled.
func TestDivOfAWholeMultipleIsTheExactInteger(t *testing.T) {
	cases := []struct{ value, divisor, want string }{
		{"1", "0.1", "10"},
		{"5", "0.1", "50"},
		{"0.70", "0.1", "7"},
		{"1.50", "0.25", "6"},
		{"12.5", "0.05", "250"},
	}

	for _, c := range cases {
		got, err := MustParseDecimal(c.value).Div(MustParseDecimal(c.divisor), 0, RoundHalfEven)
		if err != nil {
			t.Errorf("Div(%s, %s, 0) returned error %v", c.value, c.divisor, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("Div(%s, %s, 0) = %s, want %s", c.value, c.divisor, got.String(), c.want)
		}
	}
}
