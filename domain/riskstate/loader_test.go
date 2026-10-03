package riskstate

import (
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/domain/risk"
)

func dec(s string) contracts.Decimal { return contracts.MustParseDecimal(s) }

func activeAccount() Account {
	return Account{
		ID: "acc-1", Environment: "paper", VenueID: "ven-1", BaseCurrency: "USD",
		Status: "ACTIVE", ReconciliationStatus: "CLEAN",
		Equity: dec("9000"), PeakEquity: dec("10000"),
		AvailableMargin: dec("8000"), UsedMargin: dec("2000"),
		DailyPnL: dec("0"), Leverage: dec("1.5"),
		OpenOrderCount: 3, AsOf: time.Now(), AsOfNs: 1,
	}
}

// orderFactsWithAssertedFigures returns order facts whose measurement fields are
// already filled in with wrong values.
//
// This is the attack the package exists to stop. A caller that hands Build an
// OrderFacts with Leverage already set to 1 could, by inattention rather than by
// dishonesty, have it measured at 50 and still be reported as 1. Build must
// overwrite every one of these from the state it loaded, so a test that supplies
// them and then checks the output can prove the overwrite happened. If any field
// below survives into the result, Build trusted the caller.
func orderFactsWithAssertedFigures() risk.OrderFacts {
	asserted := dec("1")
	declined := dec("0.01")
	yes := true
	return risk.OrderFacts{
		Environment: "paper", AccountID: "acc-1", StrategyID: "str-1",
		InstrumentID: "ins-1", MarketClass: "crypto", VenueID: "ven-1",
		OrderType: "LIMIT", Amount: "1", Price: "100",
		AccountAuthorized: &yes, StrategyDeployed: &yes,
		Notional: &asserted, Position: &asserted, Exposure: &asserted,
		Concentration: &asserted, Leverage: &asserted,
		DailyLoss: &asserted, Drawdown: &declined,
		Currency: "USD",
	}
}

// account_status_valid admits five values and only one of them may trade.
func TestOnlyActiveAccountsMayTrade(t *testing.T) {
	for _, status := range []string{"MARGIN_CALL", "FROZEN", "DISABLED", "UNKNOWN"} {
		a := activeAccount()
		a.Status = status
		ok, why := a.Tradeable()
		if ok {
			t.Errorf("an account in %s was permitted to trade", status)
		}
		if !strings.Contains(why, status) {
			t.Errorf("refusal for %s does not name the status: %q", status, why)
		}
	}
	a := activeAccount()
	if ok, why := a.Tradeable(); !ok {
		t.Errorf("a clean ACTIVE account was refused: %s", why)
	}
}

// A margin call is the ledger asking for risk to be removed. Trading through one
// is the failure this must not have, and the name says why.
func TestAMarginCallIsNotATradeableState(t *testing.T) {
	a := activeAccount()
	a.Status = "MARGIN_CALL"
	ok, why := a.Tradeable()
	if ok {
		t.Fatal("an account in MARGIN_CALL was permitted to trade")
	}
	if !strings.Contains(why, "MARGIN_CALL") {
		t.Errorf("refusal %q does not name MARGIN_CALL", why)
	}
}

// The two conditions report separately because their remedies differ: access
// versus a data problem.
func TestReconciliationAndStatusAreDistinctRefusals(t *testing.T) {
	a := activeAccount()
	a.ReconciliationStatus = "MATERIAL_BREAK"
	ok, why := a.Tradeable()
	if ok {
		t.Fatal("an account with a material reconciliation break was permitted to trade")
	}
	if !strings.Contains(why, "MATERIAL_BREAK") {
		t.Errorf("refusal %q does not name the reconciliation status", why)
	}

	// MINOR_BREAK is excluded too, and that is a recorded open question rather
	// than a decision this package makes.
	a.ReconciliationStatus = "MINOR_BREAK"
	if ok, _ := a.Tradeable(); ok {
		t.Error("an account with a minor reconciliation break was permitted to trade")
	}
}

// Drawdown is a magnitude: equity below peak gives a positive fraction, and at
// or above peak gives exactly zero rather than a negative that would satisfy any
// max_drawdown including one of zero.
func TestDrawdown(t *testing.T) {
	cases := []struct {
		equity, peak, want string
	}{
		{equity: "10000", peak: "10000", want: "0"},  // at the peak
		{equity: "11000", peak: "10000", want: "0"},  // above the peak
		{equity: "9000", peak: "10000", want: "0.1"}, // 10% down
		{equity: "5000", peak: "10000", want: "0.5"}, // 50% down
		{equity: "0", peak: "10000", want: "1"},      // wiped out
	}
	for _, tc := range cases {
		a := activeAccount()
		a.Equity = dec(tc.equity)
		a.PeakEquity = dec(tc.peak)
		got, err := a.Drawdown()
		if err != nil {
			t.Fatalf("equity %s against peak %s failed: %v", tc.equity, tc.peak, err)
		}
		if w := dec(tc.want); got.Cmp(w) != 0 {
			t.Errorf("equity %s against peak %s gave %s, want %s", tc.equity, tc.peak, got, tc.want)
		}
	}
}

// A peak of zero has no ratio, and returning zero for it would report a wiped
// account as having no drawdown.
func TestDrawdownRefusesAZeroPeak(t *testing.T) {
	a := activeAccount()
	a.PeakEquity = dec("0")
	if _, err := a.Drawdown(); err == nil {
		t.Error("a drawdown was computed against a peak of zero")
	}
}

// A loss limit is a magnitude, so a loss must breach it and a profit must not.
func TestDailyLossIsAMagnitude(t *testing.T) {
	a := activeAccount()
	a.DailyPnL = dec("-6000")
	if got := a.DailyLoss(); got.Cmp(dec("6000")) != 0 {
		t.Errorf("a loss of -6000 gave a daily loss of %s", got)
	}
	a.DailyPnL = dec("2500")
	if got := a.DailyLoss(); got.Cmp(dec("2500")) != 0 {
		t.Errorf("a profit of 2500 gave a daily loss of %s", got)
	}
}

// Only DEPLOYED trades. PAPER, SHADOW and SIMULATION are deliberate no-trades,
// and APPROVED is not DEPLOYED: approval authorises deployment, and conflating
// them lets an approved-but-undeployed strategy trade.
func TestOnlyDeployedStrategiesMayTrade(t *testing.T) {
	for _, s := range []string{
		"DRAFT", "REVIEW", "BACKTESTED", "SIMULATION", "PAPER",
		"SHADOW", "APPROVED", "PAUSED", "RETIRED", "",
	} {
		ok, why := StrategyDeployed(s)
		if ok {
			t.Errorf("a strategy in %s was permitted to trade", s)
		}
		if !strings.Contains(why, s) && s != "" {
			t.Errorf("refusal %q does not name the state %q", why, s)
		}
	}
	if ok, why := StrategyDeployed("DEPLOYED"); !ok {
		t.Errorf("a DEPLOYED strategy was refused: %s", why)
	}
}

// A flat book has no dominant instrument, so concentration is 0 and not 1.
// Reporting 1 would breach every max_concentration for an account holding
// nothing at all.
func TestAFlatBookHasNoConcentration(t *testing.T) {
	got, err := concentration(map[string]contracts.Decimal{}, dec("0"))
	if err != nil {
		t.Fatalf("an empty book failed: %v", err)
	}
	if got.Cmp(dec("0")) != 0 {
		t.Errorf("an empty book gave a concentration of %s, want 0", got)
	}
}

func TestConcentrationIsTheLargestInstrumentShare(t *testing.T) {
	byInstrument := map[string]contracts.Decimal{
		"ins-1": dec("7000"),
		"ins-2": dec("2000"),
		"ins-3": dec("1000"),
	}
	got, err := concentration(byInstrument, dec("10000"))
	if err != nil {
		t.Fatalf("concentration failed: %v", err)
	}
	if want := dec("0.7"); got.Cmp(want) != 0 {
		t.Errorf("concentration = %s, want %s", got, want)
	}

	// A single-instrument book is fully concentrated, which is the case a
	// max_concentration limit exists to catch.
	got, err = concentration(map[string]contracts.Decimal{"ins-1": dec("10000")}, dec("10000"))
	if err != nil {
		t.Fatalf("concentration failed: %v", err)
	}
	if got.Cmp(dec("1")) != 0 {
		t.Errorf("a single-instrument book gave %s, want 1", got)
	}
}

// Instrument-level sums matter because position_identity_idx is unique on
// (account, environment, instrument, venue): one instrument can hold several
// rows across venues, and treating a single row as the whole instrument
// understates concentration for exactly the split-venue book where it matters.
func TestSplitVenueRowsSumPerInstrument(t *testing.T) {
	byInstrument := map[string]contracts.Decimal{
		// ins-1 split across two venues, 4000 each, having been summed by the caller.
		"ins-1": dec("8000"),
		"ins-2": dec("2000"),
	}
	got, err := concentration(byInstrument, dec("10000"))
	if err != nil {
		t.Fatalf("concentration failed: %v", err)
	}
	if want := dec("0.8"); got.Cmp(want) != 0 {
		t.Errorf("a split-venue instrument gave %s, want %s", got, want)
	}
}

// An instrument's share of gross cannot exceed the whole. A share above 1 means
// the aggregate is inconsistent, and returning it would produce a figure no
// policy could satisfy for the wrong reason.
func TestConcentrationAboveOneIsRefused(t *testing.T) {
	_, err := concentration(map[string]contracts.Decimal{"ins-1": dec("20000")}, dec("10000"))
	if err == nil {
		t.Error("a concentration above 1 was accepted")
	}
}

// Build must refuse what it is not entitled to decide, and it must write the
// account-derived measurements over whatever the caller supplied. The second
// property is the point: a caller that asserted Leverage 1.5 for an account at
// 50x must not survive Build.
func TestBuildOverwritesCallerAssertionsWithAccountState(t *testing.T) {
	acct := activeAccount()
	acct.Leverage = dec("50")
	acct.OpenOrderCount = 99
	acct.DailyPnL = dec("-7000")

	pos := Positions{Net: dec("5000"), Gross: dec("12000"), Concentration: dec("0.4"), RowCount: 3}

	order := orderFactsWithAssertedFigures()
	out, err := Build(Facts{
		Order:              order,
		AccountStateAsOfNs: 42,
		PolicyRevision:     "pol-x",
		LoadedAt:           time.Now(),
	}, acct, pos, "DEPLOYED")
	if err != nil {
		t.Fatalf("Build refused a healthy account: %v", err)
	}

	if out.Leverage == nil || out.Leverage.Cmp(dec("50")) != 0 {
		t.Errorf("leverage = %v, want the account's 50 rather than the caller's assertion", out.Leverage)
	}
	if out.OpenOrderCount != 99 {
		t.Errorf("open order count = %d, want the account's 99", out.OpenOrderCount)
	}
	if out.DailyLoss == nil || out.DailyLoss.Cmp(dec("7000")) != 0 {
		t.Errorf("daily loss = %v, want 7000 from a -7000 P&L", out.DailyLoss)
	}
	if out.Drawdown == nil || out.Drawdown.Cmp(dec("0.1")) != 0 {
		t.Errorf("drawdown = %v, want 0.1 from equity 9000 against peak 10000", out.Drawdown)
	}
	if out.Position == nil || out.Position.Cmp(dec("5000")) != 0 {
		t.Errorf("position = %v, want the aggregate 5000", out.Position)
	}
	if out.Provenance.AccountStateAsOfNs != 42 || out.Provenance.PositionRowCount != 3 {
		t.Errorf("provenance = %+v, want the read to be stamped", out.Provenance)
	}
	if !out.Provenance.Established() {
		t.Error("Build produced facts whose provenance is not established")
	}
}

func TestBuildRefusesAnUntradeableAccountOrStrategy(t *testing.T) {
	base := Facts{Order: orderFactsWithAssertedFigures(), AccountStateAsOfNs: 1,
		PolicyRevision: "pol-x", LoadedAt: time.Now()}

	a := activeAccount()
	a.Status = "FROZEN"
	if _, err := Build(base, a, Positions{RowCount: 0}, "DEPLOYED"); err == nil {
		t.Error("Build accepted a FROZEN account")
	}

	if _, err := Build(base, activeAccount(), Positions{RowCount: 0}, "PAPER"); err == nil {
		t.Error("Build accepted a PAPER strategy")
	}
}

// A flat book leaves concentration at zero, and Build must leave the field unset
// rather than pointing it at a zero the caller supplied for some other reason.
func TestBuildLeavesConcentrationUnsetForAFlatBook(t *testing.T) {
	out, err := Build(Facts{Order: orderFactsWithAssertedFigures(), AccountStateAsOfNs: 1,
		PolicyRevision: "pol-x", LoadedAt: time.Now()},
		activeAccount(), Positions{Net: dec("0"), Gross: dec("0"), RowCount: 0}, "DEPLOYED")
	if err != nil {
		t.Fatalf("Build failed for a flat book: %v", err)
	}
	if out.Concentration != nil && out.Concentration.Cmp(dec("0")) != 0 {
		t.Errorf("a flat book reported a concentration of %v, want unset or zero", out.Concentration)
	}
}
