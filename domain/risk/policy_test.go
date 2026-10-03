package risk

import (
	"strings"
	"testing"

	"github.com/aitc/trade/contracts"
)

func mustDec(s string) contracts.Decimal { return contracts.MustParseDecimal(s) }
func contractsZero() contracts.Decimal   { return mustDec("0") }
func contractsNeg() contracts.Decimal    { return mustDec("-1") }

func TestControlsAreClosed(t *testing.T) {
	for _, c := range Controls() {
		if !c.Known() {
			t.Errorf("%s is listed in Controls() but Known() says false", c)
		}
	}
	for _, c := range []Control{"", "notional", "MAX_ORDER_NOTIAL", "MAX_POSITION "} {
		if c.Known() {
			t.Errorf("Control(%q).Known() = true, want false", c)
		}
	}
}

// Doc 04 lists fifteen controls. The list is the contract, so its size and its
// content are both asserted rather than assumed.
func TestTheGateEvaluatesExactlyDoc04sFifteenControls(t *testing.T) {
	got := Controls()
	if len(got) != 15 {
		t.Fatalf("Controls() returns %d controls, want the 15 enumerated in 04_TRADING_DOMAIN_AND_RISK.md", len(got))
	}
	want := []Control{
		"ACCOUNT_AND_ENVIRONMENT_AUTHORIZATION", "STRATEGY_DEPLOYMENT_STATUS",
		"INSTRUMENT_ELIGIBILITY", "VENUE_AVAILABILITY", "PRICE_AND_QUANTITY_PRECISION",
		"MAX_ORDER_NOTIONAL", "POSITION_LIMIT", "EXPOSURE_LIMIT", "CONCENTRATION_LIMIT",
		"LEVERAGE_LIMIT", "LOSS_AND_DRAWDOWN_CONTROL", "MARKET_DATA_FRESHNESS",
		"DUPLICATE_ORDER_DETECTION", "RATE_AND_THROTTLE_LIMIT", "KILL_AND_HALT_STATE",
	}
	for i, w := range want {
		if got[i] != Control(w) {
			t.Errorf("Controls()[%d] = %s, want %s", i, got[i], w)
		}
	}
}

// Exactly these seven controls need a number. If that set drifts, a control
// would start failing closed for a reason that has nothing to do with
// measurement, or start measuring a number it does not own.
func TestExactlyTheseControlsRequireALimit(t *testing.T) {
	want := map[Control]bool{
		CtrlNotionalLimit: true, CtrlPositionLimit: true, CtrlExposureLimit: true,
		CtrlConcentrationLimit: true, CtrlLeverageLimit: true, CtrlLossLimit: true,
		CtrlRateLimit: true,
	}
	for _, c := range Controls() {
		if got := c.RequiresLimit(); got != want[c] {
			t.Errorf("%s.RequiresLimit() = %v, want %v", c, got, want[c])
		}
	}
}

// A policy row that no control reads is a limit an operator can set, believe is
// enforced, and find unmentioned in any finding. This pins the two columns that
// currently have no doc 04 control so the gap stays visible.
func TestColumnsWithNoDoc04Control(t *testing.T) {
	consulted := map[string]bool{}
	for _, c := range Controls() {
		for _, col := range ColumnFor(c) {
			consulted[col] = true
		}
	}

	all := []string{
		"max_order_notional", "max_order_quantity", "max_gross_exposure",
		"max_net_exposure", "max_leverage", "max_concentration", "max_open_orders",
		"max_daily_loss", "max_drawdown", "max_price_deviation",
		"max_orders_per_minute", "max_cancels_per_minute",
	}
	if len(all) != 12 {
		t.Fatalf("the twelve risk.policy limit columns are listed as %d here", len(all))
	}

	// Documented, deliberate, and asserted rather than incidental: doc 04 names
	// no quantity cap and no price-deviation cap.
	unconsulted := map[string]bool{}
	for _, col := range all {
		if !consulted[col] {
			unconsulted[col] = true
		}
	}
	for _, want := range []string{"max_order_quantity", "max_price_deviation"} {
		if !unconsulted[want] {
			t.Errorf("%s is now consulted; if a doc 04 control was given it, remove it from this test and "+
				"from the controlColumn comment", want)
		}
	}
	if len(unconsulted) != 2 {
		t.Errorf("%d columns have no doc 04 control, want exactly 2 (%v)", len(unconsulted), unconsulted)
	}
}

// Every consulted column must resolve through Value or Count, or the control
// would compare a measurement against a zero-valued bound.
func TestEveryConsultedColumnResolves(t *testing.T) {
	p := activePolicy()
	for _, c := range Controls() {
		for _, col := range ColumnFor(c) {
			if _, ok := p.Value(c, col); ok {
				continue
			}
			if _, ok := p.Count(c, col); ok {
				continue
			}
			t.Errorf("%s reads %s but neither Policy.Value nor Policy.Count resolves it", c, col)
		}
	}
}

// A column must not resolve for a control that does not read it, or a caller
// iterating columns could compare a measurement against the wrong bound.
func TestColumnsResolveOnlyForTheirOwnControl(t *testing.T) {
	p := activePolicy()
	if _, ok := p.Value(CtrlExposureLimit, "max_net_exposure"); ok {
		t.Error("EXPOSURE_LIMIT resolved max_net_exposure, which is the position control's column")
	}
	if _, ok := p.Value(CtrlLossLimit, "max_order_notional"); ok {
		t.Error("LOSS_AND_DRAWDOWN_CONTROL resolved max_order_notional")
	}
	if _, ok := p.Count(CtrlNotionalLimit, "max_open_orders"); ok {
		t.Error("a decimal control resolved an integer count column")
	}
}

func TestACompletePolicyValidates(t *testing.T) {
	if err := activePolicy().Validate(); err != nil {
		t.Fatalf("a complete policy was rejected: %v", err)
	}
}

// Each mutation removes or breaks one column. Every one must be refused, naming
// the column, because a policy the database would reject must not reach the
// gate as though it were sound.
func TestPolicyValidationNamesTheOffendingColumn(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Policy)
		wantSubstr string
	}{
		{"no revision", func(p *Policy) { p.Revision = "" }, "revision"},
		{"zero notional", func(p *Policy) { p.MaxOrderNotional = contractsZero() }, "max_order_notional"},
		{"negative notional", func(p *Policy) { p.MaxOrderNotional = contractsNeg() }, "max_order_notional"},
		{"zero quantity", func(p *Policy) { p.MaxOrderQuantity = contractsZero() }, "max_order_quantity"},
		{"zero gross exposure", func(p *Policy) { p.MaxGrossExposure = contractsZero() }, "max_gross_exposure"},
		{"zero net exposure", func(p *Policy) { p.MaxNetExposure = contractsZero() }, "max_net_exposure"},
		{"zero leverage", func(p *Policy) { p.MaxLeverage = contractsZero() }, "max_leverage"},
		{"zero concentration", func(p *Policy) { p.MaxConcentration = contractsZero() }, "max_concentration"},
		{"zero daily loss", func(p *Policy) { p.MaxDailyLoss = contractsZero() }, "max_daily_loss"},
		{"drawdown above one", func(p *Policy) { p.MaxDrawdown = mustDec("1.5") }, "max_drawdown"},
		{"negative price deviation", func(p *Policy) { p.MaxPriceDeviation = contractsNeg() }, "max_price_deviation"},
		{"zero open orders", func(p *Policy) { p.MaxOpenOrders = 0 }, "max_open_orders"},
		{"zero orders per minute", func(p *Policy) { p.MaxOrdersPerMinute = 0 }, "max_orders_per_minute"},
		{"zero cancels per minute", func(p *Policy) { p.MaxCancelsPerMinute = 0 }, "max_cancels_per_minute"},
		{"no loss window", func(p *Policy) { p.LossWindow = 0 }, "loss_window"},
		{"no loss source", func(p *Policy) { p.LossSource = "" }, "loss_source"},
		{"no market data age", func(p *Policy) { p.MaxMarketDataAge = 0 }, "max_market_data_age"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := activePolicy()
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("a policy with %s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not name the column %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestActiveReflectsOnlyStatus(t *testing.T) {
	for _, s := range []string{"ACTIVE"} {
		p := activePolicy()
		p.Status = s
		if !p.Active() {
			t.Errorf("status %s reported inactive", s)
		}
	}
	for _, s := range []string{"", "DRAFT", "SUPERSEDED", "REJECTED", "active"} {
		p := activePolicy()
		p.Status = s
		if p.Active() {
			t.Errorf("status %q reported active", s)
		}
	}
}
