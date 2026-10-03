package risk

import (
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
)

func dec(s string) contracts.Decimal { return contracts.MustParseDecimal(s) }

func dp(s string) *contracts.Decimal { d := dec(s); return &d }

func b(v bool) *bool { return &v }

// activePolicy mirrors a risk.policy row that satisfies every CHECK constraint,
// with limits generous enough that a passingFacts() order does not breach any.
func activePolicy() Policy {
	return Policy{
		Revision:             "pol-1",
		Environment:          "live",
		ScopeKind:            "ENVIRONMENT",
		ScopeValue:           "live",
		Status:               "ACTIVE",
		MaxOrderNotional:     dec("1000000"),
		MaxOrderQuantity:     dec("1000000"),
		MaxGrossExposure:     dec("5000000"),
		MaxNetExposure:       dec("4000000"),
		MaxLeverage:          dec("10"),
		MaxConcentration:     dec("0.5"),
		MaxOpenOrders:        100,
		MaxDailyLoss:         dec("50000"),
		MaxDrawdown:          dec("0.2"),
		MaxPriceDeviation:    dec("0.05"),
		MaxOrdersPerMinute:   60,
		MaxCancelsPerMinute:  60,
		LossWindow:           24 * time.Hour,
		LossSource:           "risk.account_state.daily_pnl",
		MaxMarketDataAge:     5 * time.Second,
		PermittedInstruments: []string{"i-1", "i-2"},
		PermittedVenues:      []string{"v-1"},
		PermittedMarkets:     []string{"CRYPTO"},
	}
}

// passingFacts is a fully-populated, passing fact set. Tests break exactly one
// thing at a time, so a failure names the control responsible.
func passingFacts() OrderFacts {
	return OrderFacts{
		Environment: "live", AccountID: "a-1", StrategyID: "s-1",
		InstrumentID: "i-1", MarketClass: "CRYPTO", VenueID: "v-1",
		OrderType: "MARKET", Amount: "1", Price: "500",
		AccountAuthorized:  b(true),
		StrategyDeployed:   b(true),
		InstrumentEligible: b(true),
		VenueAvailable:     b(true),
		PrecisionValid:     b(true),
		MarketDataFresh:    b(true),
		NoDuplicateOrder:   b(true),
		Notional:           dp("500"),
		Position:           dp("1000"),
		Exposure:           dp("2000"),
		Concentration:      dp("0.1"),
		Leverage:           dp("1.5"),
		DailyLoss:          dp("100"),
		Drawdown:           dp("0.05"),
		Currency:           "USD",
		OpenOrderCount:     3,
		OrdersLastMinute:   4,
		CancelsLastMinute:  2,
		// The measurements above are a stand-in for what domain/riskstate reads
		// from risk.account_state and portfolio.position. A test supplies the
		// claim directly, which is what the claim is: the production path earns
		// it by reading the rows. TestUnsourcedMeasurementsAreRefused covers the
		// case where nobody claims a source at all.
		Provenance: sourced(),
	}
}

// sourced is a provenance claim that is complete, for tests that need a
// measurement to be evaluated at all.
func sourced() Provenance {
	return Provenance{
		AccountStateAsOfNs: 1_700_000_000_000_000_000,
		PositionRowCount:   2,
		PolicyRevision:     "pol-1",
		LoadedAt:           time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC),
	}
}

func passingRequest() Request {
	return Request{
		Order:         passingFacts(),
		Policy:        activePolicy(),
		Halt:          HaltDecision{Evaluated: true, Blocked: false},
		CorrelationID: "corr-1",
		EvaluatedAt:   time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC),
	}
}

func findingFor(d Decision, c Control) (Finding, bool) {
	for _, f := range d.Findings {
		if f.Control == c {
			return f, true
		}
	}
	return Finding{}, false
}

func TestAFullyConfiguredOrderIsApproved(t *testing.T) {
	dec := Evaluate(passingRequest())
	if !dec.Approved {
		t.Fatalf("a fully configured order was rejected: %s", dec.Summary())
	}
	if dec.PolicyRevision != "pol-1" {
		t.Errorf("decision does not carry the policy revision; doc 17 Â§4 requires it")
	}
	if dec.CorrelationID != "corr-1" {
		t.Errorf("decision does not carry the correlation ID; doc 17 Â§4 requires it")
	}
	if dec.EvaluatedAt.IsZero() {
		t.Error("decision carries no evaluation timestamp")
	}
}

// Doc 17 Â§4 requires the result to contain evaluated facts and failed controls.
// Recording only failures cannot show a control was consulted at all.
func TestEveryControlIsRecordedWhetherItPassedOrFailed(t *testing.T) {
	d := Evaluate(passingRequest())
	if len(d.Findings) != len(Controls()) {
		t.Fatalf("decision records %d findings, want one per control (%d)", len(d.Findings), len(Controls()))
	}
	for i, f := range d.Findings {
		if Controls()[i] != f.Control {
			t.Errorf("finding %d is %s, want %s in doc 04's order", i, f.Control, Controls()[i])
		}
		if f.Reason == "" {
			t.Errorf("%s has no reason; a finding with an empty reason cannot be reviewed", f.Control)
		}
	}
}

// Doc 04: "A single failed mandatory control rejects the order."
func TestOneFailedMandatoryControlRejectsTheWholeOrder(t *testing.T) {
	req := passingRequest()
	req.Order.InstrumentEligible = b(false)
	d := Evaluate(req)
	if d.Approved {
		t.Fatal("order approved despite a failed INSTRUMENT_ELIGIBILITY")
	}
	if len(d.Failed()) != 1 {
		t.Errorf("Failed() returns %d findings, want exactly 1", len(d.Failed()))
	}
	if d.Failed()[0].Control != CtrlInstrumentEligible {
		t.Errorf("Failed() names %s, want %s", d.Failed()[0].Control, CtrlInstrumentEligible)
	}
}

func TestEveryExternalFactControlCanIndependentlyReject(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*OrderFacts)
		want  Control
	}{
		{"unauthorized account", func(f *OrderFacts) { f.AccountAuthorized = b(false) }, CtrlAccountAuthorized},
		{"strategy not deployed", func(f *OrderFacts) { f.StrategyDeployed = b(false) }, CtrlStrategyDeployed},
		{"venue unavailable", func(f *OrderFacts) { f.VenueAvailable = b(false) }, CtrlVenueAvailable},
		{"bad precision", func(f *OrderFacts) { f.PrecisionValid = b(false) }, CtrlPricePrecision},
		{"stale market data", func(f *OrderFacts) { f.MarketDataFresh = b(false) }, CtrlMarketDataFreshness},
		{"duplicate order", func(f *OrderFacts) { f.NoDuplicateOrder = b(false) }, CtrlDuplicateOrder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := passingRequest()
			tc.apply(&req.Order)
			d := Evaluate(req)
			if d.Approved {
				t.Fatalf("order approved despite %s", tc.name)
			}
			if _, ok := findingFor(d, tc.want); !ok {
				t.Errorf("no failure reported for %s", tc.want)
			}
		})
	}
}

// The fail-closed core. With no policy, every limit control denies and says why.
func TestNoActivePolicyDeniesEveryLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     Policy
		wantSubstr string
	}{
		{"no policy at all", Policy{}, "no risk policy was loaded"},
		{"draft policy", func() Policy { p := activePolicy(); p.Status = "DRAFT"; return p }(), "not ACTIVE"},
		{"superseded policy", func() Policy { p := activePolicy(); p.Status = "SUPERSEDED"; return p }(), "not ACTIVE"},
		{"rejected policy", func() Policy { p := activePolicy(); p.Status = "REJECTED"; return p }(), "not ACTIVE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := passingRequest()
			req.Policy = tc.policy
			d := Evaluate(req)
			if d.Approved {
				t.Fatal("order approved with no ACTIVE policy; doc 17 Â§3 forbids a platform-supplied default")
			}
			for _, c := range Controls() {
				if !c.RequiresLimit() {
					continue
				}
				f, ok := findingFor(d, c)
				if !ok {
					t.Fatalf("no finding for %s", c)
				}
				if !strings.Contains(f.Reason, tc.wantSubstr) {
					t.Errorf("%s reason %q does not contain %q", c, f.Reason, tc.wantSubstr)
				}
			}
		})
	}
}

// An operator one approval away from a working gate must be told that, rather
// than being told no policy exists.
func TestADraftPolicyIsDistinguishedFromNoPolicy(t *testing.T) {
	req := passingRequest()
	req.Policy = activePolicy()
	req.Policy.Status = "DRAFT"
	f, _ := findingFor(Evaluate(req), CtrlNotionalLimit)
	if !strings.Contains(f.Reason, "pol-1") {
		t.Errorf("reason %q does not name the draft policy", f.Reason)
	}
}

// A non-limit control still runs when the policy is absent, so the operator sees
// every reason at once rather than fixing the policy and then discovering an
// unrelated authorization failure.
func TestNonLimitControlsStillRunWithoutAPolicy(t *testing.T) {
	req := passingRequest()
	req.Policy = Policy{}
	req.Order.VenueAvailable = b(false)
	d := Evaluate(req)
	f, ok := findingFor(d, CtrlVenueAvailable)
	if !ok {
		t.Fatal("no finding for VENUE_AVAILABILITY")
	}
	if f.Passed {
		t.Error("VENUE_AVAILABILITY passed while the policy is absent")
	}
}

func TestABreachIsReportedWithTheFiguresAndColumnInvolved(t *testing.T) {
	req := passingRequest()
	over := dec("2000000")
	req.Order.Notional = &over
	d := Evaluate(req)
	if d.Approved {
		t.Fatal("order approved over its notional limit")
	}
	f, _ := findingFor(d, CtrlNotionalLimit)
	for _, want := range []string{"2000000", "1000000", "max_order_notional"} {
		if !strings.Contains(f.Reason, want) {
			t.Errorf("reason %q does not include %s; a breach without its figures and column is not reviewable",
				f.Reason, want)
		}
	}
}

// The boundary is inclusive: an order for exactly the limit is permitted.
func TestBoundaryIsInclusiveAtTheExactLimit(t *testing.T) {
	if DerivationBoundary != BoundaryInclusive {
		t.Fatalf("DerivationBoundary is %s; risk.policy constrains limits to > 0 and the comparison "+
			"is <=, so the boundary must be inclusive", DerivationBoundary)
	}
	for _, tc := range []struct {
		column string
		ctrl   Control
		bound  string
		set    func(*OrderFacts, contracts.Decimal)
	}{
		{"max_order_notional", CtrlNotionalLimit, "1000000",
			func(f *OrderFacts, v contracts.Decimal) { f.Notional = &v }},
		{"max_gross_exposure", CtrlExposureLimit, "5000000",
			func(f *OrderFacts, v contracts.Decimal) { f.Exposure = &v }},
		{"max_net_exposure", CtrlPositionLimit, "4000000",
			func(f *OrderFacts, v contracts.Decimal) { f.Position = &v }},
		{"max_leverage", CtrlLeverageLimit, "10",
			func(f *OrderFacts, v contracts.Decimal) { f.Leverage = &v }},
		{"max_concentration", CtrlConcentrationLimit, "0.5",
			func(f *OrderFacts, v contracts.Decimal) { f.Concentration = &v }},
		{"max_daily_loss", CtrlLossLimit, "50000",
			func(f *OrderFacts, v contracts.Decimal) { f.DailyLoss = &v }},
		{"max_drawdown", CtrlLossLimit, "0.2",
			func(f *OrderFacts, v contracts.Decimal) { f.Drawdown = &v }},
	} {
		t.Run(tc.column, func(t *testing.T) {
			req := passingRequest()
			tc.set(&req.Order, dec(tc.bound))
			f, ok := findingFor(Evaluate(req), tc.ctrl)
			if !ok {
				t.Fatalf("no finding for %s", tc.ctrl)
			}
			if !f.Passed {
				t.Errorf("a measurement of exactly %s was refused as a breach of %s: %s",
					tc.bound, tc.column, f.Reason)
			}
		})
	}
}

func TestJustOverTheLimitAlwaysBreaches(t *testing.T) {
	req := passingRequest()
	over := dec("1000000.000000000000000001")
	req.Order.Notional = &over
	if Evaluate(req).Approved {
		t.Error("an order one unit over the limit was approved")
	}
}

// Limits are exact decimals. A float comparison would put 0.1+0.2 on the wrong
// side of a 0.3 bound.
func TestLimitComparisonsAreExactNotFloating(t *testing.T) {
	req := passingRequest()
	sum, err := dec("0.1").Add(dec("0.2")) // exactly 0.3 in decimal
	if err != nil {
		t.Fatalf("decimal addition failed: %v", err)
	}
	req.Policy.MaxConcentration = dec("0.3")
	req.Order.Concentration = &sum
	d := Evaluate(req)
	if !d.Approved {
		t.Errorf("0.1+0.2 == 0.3 was treated as a breach: %s", d.Summary())
	}
}

// An unmeasurable figure denies, and says so distinctly from an absent policy --
// different remedy, so it must not read the same.
func TestAnUnmeasurableFigureDeniesDistinctly(t *testing.T) {
	req := passingRequest()
	req.Order.Notional = nil
	d := Evaluate(req)
	if d.Approved {
		t.Fatal("order approved with an unmeasurable notional")
	}
	f, _ := findingFor(d, CtrlNotionalLimit)
	if !strings.Contains(f.Reason, "could not be measured") {
		t.Errorf("reason %q does not distinguish an unmeasurable figure from an absent policy", f.Reason)
	}
}

// The two-column controls must report which column failed.
func TestMultiColumnControlsNameBothColumns(t *testing.T) {
	if got := ColumnFor(CtrlLossLimit); len(got) != 2 {
		t.Errorf("LOSS_AND_DRAWDOWN_CONTROL reads %v, want two columns", got)
	}
	if got := ColumnFor(CtrlNotionalLimit); len(got) != 1 {
		t.Errorf("MAX_ORDER_NOTIONAL reads %v, want one column", got)
	}

	req := passingRequest()
	bad := dec("0.9") // above max_drawdown = 0.2
	req.Order.Drawdown = &bad
	d := Evaluate(req)
	if d.Approved {
		t.Fatal("order approved with a drawdown over the limit")
	}
	f, _ := findingFor(d, CtrlLossLimit)
	if !strings.Contains(f.Reason, "max_drawdown") {
		t.Errorf("reason %q does not name the breached column", f.Reason)
	}
}

// Every control either declares a policy column or declares none. A control
// claiming to take a limit but naming no column would evaluate to nothing.
func TestLimitControlsAllDeclareColumns(t *testing.T) {
	for _, c := range Controls() {
		cols := ColumnFor(c)
		if c.RequiresLimit() && len(cols) == 0 {
			t.Errorf("%s requires a limit but declares no policy column", c)
		}
		if !c.RequiresLimit() && len(cols) != 0 {
			t.Errorf("%s declares policy columns %v but takes no limit", c, cols)
		}
	}
}

// Refusing to let an operator reduce exposure during an incident converts a
// market problem into an unmanaged loss.
func TestRiskReducingActionsPassTheExposureCaps(t *testing.T) {
	req := passingRequest()
	req.Order.RiskReducing = true
	req.Order.OrderType = "CLOSE_POSITION"
	req.Policy = Policy{} // no policy at all
	d := Evaluate(req)
	if !d.Approved {
		t.Fatalf("a risk-reducing action was refused for lack of an active policy: %s", d.Summary())
	}
	f, ok := findingFor(d, CtrlNotionalLimit)
	if !ok {
		t.Fatal("no finding for MAX_ORDER_NOTIONAL")
	}
	if !strings.Contains(f.Reason, "not applied") {
		t.Errorf("reason %q does not explain the cap was skipped rather than checked", f.Reason)
	}
}

// Only the exposure caps are skipped, not the safety controls.
func TestRiskReducingActionsStillRequireAuthorization(t *testing.T) {
	req := passingRequest()
	req.Order.RiskReducing = true
	req.Order.AccountAuthorized = b(false)
	if Evaluate(req).Approved {
		t.Fatal("an unauthorized risk-reducing action was approved")
	}
}

// Being exempt from the policy allowlist is not the same as being reachable.
//
// The allowlist is a configured restriction, so a risk-reducing action is
// rightly not measured against it. The external fact is a different kind of
// thing: an order sent to a venue this platform cannot reach, or for an
// instrument it does not recognise, is refused no matter how urgently it needs
// to be sent. Returning Passed with a reason claiming the external fact was
// satisfied without ever evaluating it made the decision report evidence that
// did not exist.
func TestRiskReducingActionsStillRequireAReachableVenue(t *testing.T) {
	req := passingRequest()
	req.Order.RiskReducing = true
	req.Order.OrderType = "CLOSE_POSITION"
	req.Policy = Policy{} // no policy, so only the external fact can refuse
	req.Order.VenueAvailable = b(false)

	d := Evaluate(req)
	if d.Approved {
		t.Fatal("a risk-reducing action was approved against an unreachable venue")
	}
	f, ok := findingFor(d, CtrlVenueAvailable)
	if !ok {
		t.Fatal("no finding for VENUE_AVAILABLE")
	}
	if strings.Contains(f.Reason, "external fact is satisfied") {
		t.Errorf("reason %q claims the external fact was satisfied, but it was not evaluated",
			f.Reason)
	}
}

// The mirror of the above: the exemption must still let a reachable venue pass,
// or the fix would have turned a reduction into something unorderable.
func TestRiskReducingActionOnAReachableVenueStillPasses(t *testing.T) {
	req := passingRequest()
	req.Order.RiskReducing = true
	req.Order.OrderType = "CLOSE_POSITION"
	req.Policy = Policy{} // no policy at all
	if !Evaluate(req).Approved {
		t.Fatalf("a risk-reducing action against a reachable venue was refused: %s",
			Evaluate(req).Summary())
	}
}

func TestAnUnevaluatedExternalFactDenies(t *testing.T) {
	fields := map[string]func(*OrderFacts){
		"AccountAuthorized":  func(f *OrderFacts) { f.AccountAuthorized = nil },
		"StrategyDeployed":   func(f *OrderFacts) { f.StrategyDeployed = nil },
		"InstrumentEligible": func(f *OrderFacts) { f.InstrumentEligible = nil },
		"VenueAvailable":     func(f *OrderFacts) { f.VenueAvailable = nil },
		"PrecisionValid":     func(f *OrderFacts) { f.PrecisionValid = nil },
		"MarketDataFresh":    func(f *OrderFacts) { f.MarketDataFresh = nil },
		"NoDuplicateOrder":   func(f *OrderFacts) { f.NoDuplicateOrder = nil },
	}
	for name, clear := range fields {
		t.Run(name, func(t *testing.T) {
			req := passingRequest()
			clear(&req.Order)
			if Evaluate(req).Approved {
				t.Errorf("order approved with %s unestablished", name)
			}
		})
	}
}

func TestAnUnevaluatedHaltGateRejects(t *testing.T) {
	req := passingRequest()
	req.Halt = HaltDecision{Evaluated: false}
	d := Evaluate(req)
	if d.Approved {
		t.Fatal("order approved without the halt gate being consulted")
	}
	f, _ := findingFor(d, CtrlHaltState)
	if !strings.Contains(f.Reason, "not consulted") {
		t.Errorf("reason %q does not distinguish an unexamined gate from a clear one", f.Reason)
	}
}

func TestAnActiveHaltRejects(t *testing.T) {
	req := passingRequest()
	req.Halt = HaltDecision{Evaluated: true, Blocked: true, Reason: "blocked by SYSTEM_HALT h-1"}
	d := Evaluate(req)
	if d.Approved {
		t.Fatal("order approved while a halt was in force")
	}
	f, _ := findingFor(d, CtrlHaltState)
	if !strings.Contains(f.Reason, "SYSTEM_HALT") {
		t.Errorf("reason %q does not carry the governing halt through from package halt", f.Reason)
	}
}

// Evaluation is deterministic for a fixed policy, account state, snapshot and
// command (doc 17 Â§4).
func TestEvaluationIsDeterministic(t *testing.T) {
	req := passingRequest()
	over := dec("2000000")
	req.Order.Notional = &over
	first := Evaluate(req).Summary()
	for i := 0; i < 5; i++ {
		if got := Evaluate(req).Summary(); got != first {
			t.Fatalf("run %d differed:\n got: %s\nwant: %s", i, got, first)
		}
	}
}

func TestEvaluationDoesNotShortCircuit(t *testing.T) {
	req := passingRequest()
	req.Policy = Policy{}
	d := Evaluate(req)
	// All seven limit controls, plus INSTRUMENT_ELIGIBILITY and
	// VENUE_AVAILABILITY, whose policy allowlists are also unestablished.
	if got := len(d.Failed()); got != 9 {
		t.Errorf("Failed() reports %d failures, want all 9 reported together", got)
	}
}

// A reachable venue that the policy does not list is still refused. The external
// fact and the policy are independent, and neither substitutes for the other.
func TestPolicyAllowlistsAreConsultedNotJustTheExternalFacts(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*Request)
		want  Control
	}{
		{"instrument not permitted", func(r *Request) {
			r.Order.InstrumentID = "i-999"
		}, CtrlInstrumentEligible},
		{"venue not permitted", func(r *Request) {
			r.Order.VenueID = "v-999"
		}, CtrlVenueAvailable},
		{"empty instrument allowlist", func(r *Request) {
			r.Policy.PermittedInstruments = nil
		}, CtrlInstrumentEligible},
		{"empty venue allowlist", func(r *Request) {
			r.Policy.PermittedVenues = nil
		}, CtrlVenueAvailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := passingRequest()
			tc.apply(&req)
			d := Evaluate(req)
			if d.Approved {
				t.Fatalf("order approved with %s", tc.name)
			}
			f, ok := findingFor(d, tc.want)
			if !ok {
				t.Fatalf("no finding for %s", tc.want)
			}
			if !strings.Contains(f.Reason, "permitted_") {
				t.Errorf("reason %q does not name the allowlist column", f.Reason)
			}
		})
	}
}

// Both halves must hold. A venue that is permitted but down is still refused.
func TestEligibilityNeedsBothTheExternalFactAndTheAllowlist(t *testing.T) {
	req := passingRequest()
	req.Order.VenueAvailable = b(false)
	if Evaluate(req).Approved {
		t.Error("order approved for a permitted but unavailable venue")
	}
	req = passingRequest()
	req.Order.VenueAvailable = b(true)
	req.Policy.PermittedVenues = []string{"other-venue"}
	if Evaluate(req).Approved {
		t.Error("order approved for an available but unpermitted venue")
	}
}

// The three INTEGER columns must actually be compared. An earlier version took
// a pre-computed WithinRateLimit boolean, which left max_open_orders,
// max_orders_per_minute and max_cancels_per_minute set-but-never-read.
func TestRateControlComparesEachCountColumnIndependently(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*OrderFacts)
		bound string
	}{
		{"open orders", func(f *OrderFacts) { f.OpenOrderCount = 101 }, "max_open_orders"},
		{"orders per minute", func(f *OrderFacts) { f.OrdersLastMinute = 61 }, "max_orders_per_minute"},
		{"cancels per minute", func(f *OrderFacts) { f.CancelsLastMinute = 61 }, "max_cancels_per_minute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := passingRequest()
			tc.set(&req.Order)
			d := Evaluate(req)
			if d.Approved {
				t.Fatalf("order approved over %s", tc.bound)
			}
			f, _ := findingFor(d, CtrlRateLimit)
			if !strings.Contains(f.Reason, tc.bound) {
				t.Errorf("reason %q does not name the breached column", f.Reason)
			}
		})
	}
}

func TestCountsExactlyAtTheBoundAreWithinIt(t *testing.T) {
	req := passingRequest()
	req.Order.OpenOrderCount = req.Policy.MaxOpenOrders
	req.Order.OrdersLastMinute = req.Policy.MaxOrdersPerMinute
	req.Order.CancelsLastMinute = req.Policy.MaxCancelsPerMinute
	if f, _ := findingFor(Evaluate(req), CtrlRateLimit); !f.Passed {
		t.Errorf("counts exactly at their bounds were refused: %s", f.Reason)
	}
}

func TestUnsourcedMeasurementsAreRefused(t *testing.T) {
	// The hole this closes: the gate takes leverage, open orders, loss, drawdown
	// and exposure as values, and every one of those is stored in
	// risk.account_state and portfolio.position. A caller asserting Leverage 1.5
	// for an account the database records at 50x used to pass a max_leverage of
	// 2, and the finding reported the limit as checked.
	//
	// Each case zeroes exactly one field of the provenance, so the refusal names
	// the field that was missing.
	cases := []struct {
		name   string
		break_ func(*Provenance)
		want   string
	}{
		{"no account state read", func(p *Provenance) { p.AccountStateAsOfNs = 0 }, "no risk.account_state row was read"},
		{"positions not aggregated", func(p *Provenance) { p.PositionRowCount = -1 }, "portfolio.position was not aggregated"},
		{"no policy revision", func(p *Provenance) { p.PolicyRevision = "" }, "no policy revision was recorded"},
		{"no read time", func(p *Provenance) { p.LoadedAt = time.Time{} }, "the read time was not recorded"},
		{"entirely absent", func(p *Provenance) { *p = Provenance{} }, "no risk.account_state row was read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := passingRequest()
			tc.break_(&req.Order.Provenance)
			d := Evaluate(req)
			if d.Approved {
				t.Fatalf("order approved with %s; an unsourced measurement was compared against a limit", tc.name)
			}
			f, ok := findingFor(d, CtrlLeverageLimit)
			if !ok {
				t.Fatal("no finding for LEVERAGE_LIMIT")
			}
			if !strings.Contains(f.Reason, tc.want) {
				t.Errorf("reason %q does not name the missing field %q", f.Reason, tc.want)
			}
			if !strings.Contains(f.Reason, "asserted figure is not a measurement") {
				t.Errorf("reason %q does not explain why an unsourced figure cannot be used", f.Reason)
			}
		})
	}
}

// Zero positions is a real account, not a missing aggregate, so it is sourced.
func TestAnAccountWithNoPositionsIsStillSourced(t *testing.T) {
	p := sourced()
	p.PositionRowCount = 0
	if !p.Established() {
		t.Error("a complete provenance with zero positions was treated as unsourced")
	}
}

// A risk-reducing action must still pass with no provenance: refusing to reduce
// exposure is the failure this package must not have, and it outranks a
// measurement rule.
func TestRiskReducingNeedsNoProvenance(t *testing.T) {
	req := passingRequest()
	req.Order.RiskReducing = true
	req.Order.Provenance = Provenance{}
	req.Policy = Policy{}
	if !Evaluate(req).Approved {
		t.Error("a risk-reducing action was refused for want of sourced measurements")
	}
}

// An unsourced measurement must be refused even when it would have passed. The
// whole point is that the number cannot be checked, not that it is out of range.
func TestAnUnsourcedFigureIsRefusedEvenWhenItIsWithinTheLimit(t *testing.T) {
	req := passingRequest()
	req.Order.Provenance = Provenance{}
	req.Order.Leverage = dp("0.001") // comfortably inside max_leverage of 10
	d := Evaluate(req)
	if d.Approved {
		t.Fatal("an unsourced but harmless-looking figure was accepted")
	}
	if _, ok := findingFor(d, CtrlLeverageLimit); !ok {
		t.Error("the refusal did not come from the leverage control")
	}
}

func TestSummaryNamesThePolicy(t *testing.T) {
	d := Evaluate(passingRequest())
	if !strings.Contains(d.Summary(), "pol-1") {
		t.Errorf("summary %q does not name the policy revision", d.Summary())
	}
}
