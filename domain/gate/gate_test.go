package gate

import (
	"strings"
	"testing"
)

func submission() Submission {
	return Submission{
		Environment:   "paper",
		AccountID:     "acc_0123456789abcdefghjkmnpqrs",
		StrategyID:    "str_0123456789abcdefghjkmnpqrs",
		InstrumentID:  "ins_0123456789abcdefghjkmnpqrs",
		MarketClass:   "crypto",
		VenueID:       "ven-1",
		OrderType:     "LIMIT",
		Side:          "BUY",
		Quantity:      "1",
		Price:         "5000.00",
		Currency:      "USD",
		TimeInForce:   "DAY",
		CorrelationID: "cor_0123456789abcdefghjkmnpqrs",
	}
}

// TestASubmissionIsRefusedForMissingIdentityFields states which omissions are
// refused before any read happens. Each of these produces a policy scope that
// resolves to nothing or a scope that matches everything, and "no policy" is a
// rejection whose reason would name the wrong problem.
func TestASubmissionIsRefusedForMissingIdentityFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Submission)
		want   string
	}{
		{"no environment", func(s *Submission) { s.Environment = "" }, "environment"},
		{"no account", func(s *Submission) { s.AccountID = "" }, "account_id"},
		{"no instrument", func(s *Submission) { s.InstrumentID = "" }, "instrument_id"},
		{"no venue", func(s *Submission) { s.VenueID = "" }, "venue_id"},
		{"no order type", func(s *Submission) { s.OrderType = "" }, "order_type"},
		{"no quantity", func(s *Submission) { s.Quantity = "" }, "quantity"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := submission()
			c.mutate(&in)
			_, err := NewOrderFacts(in)
			if err == nil {
				t.Fatalf("a submission carrying no %s was accepted", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal %q does not name the missing field %s", err, c.want)
			}
		})
	}
}

// TestAClosedSetIsRefusedByName states that a value outside contracts' closed
// sets is an error rather than something passed through to be evaluated. An
// unrecognised order type would otherwise reach the instrument's capability
// check as a string no instrument declares, which denies for the wrong reason.
func TestAClosedSetIsRefusedByName(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Submission)
		want   string
	}{
		{"unknown order type", func(s *Submission) { s.OrderType = "ICEBERG" }, "order_type"},
		{"lowercase order type", func(s *Submission) { s.OrderType = "limit" }, "order_type"},
		{"absent side", func(s *Submission) { s.Side = "" }, "side"},
		{"misspelled side", func(s *Submission) { s.Side = "Buy" }, "side"},
		{"long-form side", func(s *Submission) { s.Side = "LONG" }, "side"},
		{"unknown time in force", func(s *Submission) { s.TimeInForce = "FOREVER" }, "time_in_force"},
		{"quantity that is not a decimal", func(s *Submission) { s.Quantity = "one" }, "quantity"},
		{"price that is not a decimal", func(s *Submission) { s.Price = "five thousand" }, "price"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := submission()
			c.mutate(&in)
			_, err := NewOrderFacts(in)
			if err == nil {
				t.Fatalf("a submission with an invalid %s was accepted", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal %q does not name the offending field %s", err, c.want)
			}
		})
	}
}

// TestRiskReducingIsDerivedFromTheOmsClassification pins the derivation rather
// than restating it. This package claims not to own the classification of a
// risk-reducing action; it claims to read the one definition that does. If a
// caller could supply the flag instead, two places would decide which orders may
// proceed through a halt, and they would disagree on the third order type.
func TestRiskReducingIsDerivedFromTheOmsClassification(t *testing.T) {
	cases := []struct {
		orderType string
		want      bool
	}{
		{"MARKET", false},
		{"LIMIT", false},
		{"STOP", false},
		{"STOP_LIMIT", false},
		{"POST_ONLY", false},
		{"IOC", false},
		{"FOK", false},
		{"REDUCE_ONLY", true},
		{"CLOSE_POSITION", true},
	}

	for _, c := range cases {
		in := submission()
		in.OrderType = c.orderType
		out, err := NewOrderFacts(in)
		if err != nil {
			t.Errorf("%s: %v", c.orderType, err)
			continue
		}
		if out.RiskReducing != c.want {
			t.Errorf("RiskReducing for %s = %v, want %v", c.orderType, out.RiskReducing, c.want)
		}
	}
}

// TestTheSubmissionCarriesNoMeasurements is a statement about what this package
// does not accept. Every figure the gate compares against a limit is read from
// the database, so a Submission that could carry one would be a way to assert it.
func TestTheSubmissionCarriesNoMeasurements(t *testing.T) {
	in := submission()
	out, err := NewOrderFacts(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Notional != nil {
		t.Error("NewOrderFacts carried a notional the caller supplied")
	}
	if out.Position != nil || out.Exposure != nil || out.Concentration != nil {
		t.Error("NewOrderFacts carried caller-supplied position measurements")
	}
	if out.Leverage != nil || out.Drawdown != nil || out.DailyLoss != nil {
		t.Error("NewOrderFacts carried caller-supplied account measurements")
	}
	if out.InstrumentEligible != nil || out.PrecisionValid != nil {
		t.Error("NewOrderFacts carried a caller verdict on the instrument")
	}
	if out.OpenOrderCount != 0 || out.OrdersLastMinute != 0 || out.CancelsLastMinute != 0 {
		t.Error("NewOrderFacts carried caller-supplied rate counters")
	}
}
