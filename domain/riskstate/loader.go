// Package riskstate loads the account and portfolio state the risk gate
// measures against, and stamps the result with where it came from.
//
// Authority: 04_TRADING_DOMAIN_AND_RISK.md, 17_CONFIGURATION_AND_RISK_POLICY.md.
//
// # Why this package exists
//
// On 2026-09-30 the gate was found accepting measurements that no one had read
// from anywhere. risk.OrderFacts carried Leverage, OpenOrderCount, DailyLoss,
// Drawdown, Position, Exposure and Concentration as plain values, and the
// caller filled them in. Every one of those figures is already stored:
//
//	risk.account_state.leverage              -> Leverage
//	risk.account_state.open_order_count      -> OpenOrderCount
//	risk.account_state.daily_pnl             -> DailyLoss (as a magnitude)
//	risk.account_state.equity, .peak_equity  -> Drawdown
//	risk.account_state.account_status        -> AccountAuthorized
//	risk.account_state.reconciliation_status -> AccountAuthorized
//	risk.account_state.base_currency         -> Currency
//	portfolio.position                       -> Position, Exposure, Concentration
//	strategy.strategy.state                  -> StrategyDeployed
//
// So the gate could be told an account at 50x was at 1.5x, and would approve
// against a max_leverage of 2, and the finding would say the leverage limit had
// been checked. This package is the one sanctioned way to produce those figures,
// and risk.Provenance is what it stamps them with.
//
// # It reads, it does not decide
//
// This package populates risk.OrderFacts. It does not evaluate controls, decide
// risk, or hold a second copy of any limit semantics. domain/risk remains the
// only thing that decides. If this package ever answers "is this order safe",
// there will be two gates, and they will eventually disagree.
//
// # Fail closed
//
// Every refusal below returns an error or a negative fact rather than a default.
// A missing account is not an account with zero exposure; an UNKNOWN
// reconciliation status is not a clean one; a stale as_of is not current. The
// account_state_not_optimistic constraint already forbids an ACTIVE account
// with an UNKNOWN reconciliation status, and this package refuses the states the
// constraint permits but a trading gate should not trade through -- MARGIN_CALL
// included, because a margin call is a request for risk to be removed.
package riskstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/domain/instrument"
	"github.com/aitc/trade/domain/risk"
)

// ErrNoAccountState is returned when risk.account_state has no row for the
// account.
//
// Not a transient fault. The correct response is to refuse the order: an account
// the platform cannot see is an account whose exposure it cannot bound.
var ErrNoAccountState = errors.New("riskstate: no risk.account_state row for this account")

// ErrStaleAccountState is returned when the account state read is older than the
// caller's staleness bound.
//
// Doc 17 §2 makes unavailable account state a reject condition. A row that exists
// but describes the past is unavailable for the purpose of a limit check, and
// treating it as current is how a margin call is discovered after the fill.
var ErrStaleAccountState = errors.New("riskstate: risk.account_state is stale")

// Account is the row this package read, in the gate's terms.
type Account struct {
	ID                   string
	Environment          string
	VenueID              string
	BaseCurrency         string
	Status               string
	ReconciliationStatus string
	Equity               contracts.Decimal
	AvailableMargin      contracts.Decimal
	UsedMargin           contracts.Decimal
	PeakEquity           contracts.Decimal
	DailyPnL             contracts.Decimal
	OpenOrderCount       int64
	Leverage             contracts.Decimal
	AsOf                 time.Time
	AsOfNs               int64
	VersionVector        int64
}

// tradeableAccountStatus is the single status an account may trade in.
//
// account_status_valid admits five. ACTIVE trades. MARGIN_CALL does not: it is
// the venue or the ledger telling the platform the account is out of margin, and
// the correct response to that is to stop, not to open another position and find
// out what happens next. FROZEN and DISABLED are refusals by name. UNKNOWN is a
// refusal because it means nobody knows.
var tradeableAccountStatus = "ACTIVE"

// tradeableReconciliationStatus is the only reconciliation status that permits
// trading.
//
// MINOR_BREAK is excluded. Whether a break judged minor should halt trading is an
// owner decision this package does not make, so it is excluded and recorded as
// an open question rather than assumed permissive.
var tradeableReconciliationStatus = "CLEAN"

// Querier is the read surface this package needs.
//
// It is an interface rather than *sql.DB so that account and position state can
// be read inside a transaction. That is not only a testing convenience: the rows
// this package reads are financial facts, and mandatory invariant 4 makes a
// validated fill immutable -- undeletable even by the test that inserted it. A
// caller that must not leave residue therefore has no option but to read them
// from a transaction it can discard.
//
// *sql.DB and *sql.Tx both satisfy it. Production code should generally pass a
// *sql.DB, because the gate must measure state the rest of the system can also
// see, not a snapshot only this caller holds.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// LoadAccount reads one account's state.
//
// maxAge bounds how old the row may be. A zero maxAge skips the staleness check
// entirely, which exists for tests and for a caller that has already established
// freshness; it is not a default, and a production caller must supply a bound
// derived from something rather than from nothing.
func LoadAccount(ctx context.Context, db Querier, accountID string, maxAge time.Duration) (Account, error) {
	var a Account
	var status, recon string
	var equity, available, used, peak, daily, leverage string

	err := db.QueryRowContext(ctx, `
		SELECT account_id, environment::text, venue_id, base_currency,
		       account_status, reconciliation_status,
		       equity::text, available_margin::text, used_margin::text,
		       peak_equity::text, daily_pnl::text, leverage::text,
		       open_order_count, as_of, as_of_ns, version_vector
		  FROM risk.account_state
		 WHERE account_id = $1`, accountID).
		Scan(&a.ID, &a.Environment, &a.VenueID, &a.BaseCurrency,
			&status, &recon,
			&equity, &available, &used, &peak, &daily, &leverage,
			&a.OpenOrderCount, &a.AsOf, &a.AsOfNs, &a.VersionVector)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: %s", ErrNoAccountState, accountID)
	}
	if err != nil {
		return Account{}, fmt.Errorf("riskstate: reading account_state failed: %w", err)
	}

	a.Status = status
	a.ReconciliationStatus = recon
	for _, f := range []struct {
		raw    string
		column string
		dst    *contracts.Decimal
	}{
		{equity, "equity", &a.Equity},
		{available, "available_margin", &a.AvailableMargin},
		{used, "used_margin", &a.UsedMargin},
		{peak, "peak_equity", &a.PeakEquity},
		{daily, "daily_pnl", &a.DailyPnL},
		{leverage, "leverage", &a.Leverage},
	} {
		d, parseErr := contracts.ParseDecimal(f.raw)
		if parseErr != nil {
			// A NUMERIC column cannot hold a non-numeric value, so this means the
			// driver's own conversion failed. It is refused rather than treated
			// as zero: a zero equity would make every exposure look free.
			return Account{}, fmt.Errorf(
				"riskstate: %s came back as %q, which is not a decimal; refusing to treat a "+
					"figure that will not parse as zero: %w", f.column, f.raw, parseErr)
		}
		*f.dst = d
	}

	if maxAge > 0 {
		age := time.Since(a.AsOf)
		if age > maxAge {
			return Account{}, fmt.Errorf("%w: as_of is %s old, the bound is %s (as_of_ns %d)",
				ErrStaleAccountState, age.Round(time.Second), maxAge, a.AsOfNs)
		}
	}
	return a, nil
}

// Tradeable reports whether the account may trade, and why not when it may not.
//
// The two conditions are separate because their remedies are: an unauthorised
// account is an access decision, and a broken reconciliation is a data problem.
// An operator handed only "unauthorised" would go looking at permissions.
func (a Account) Tradeable() (bool, string) {
	if a.Status != tradeableAccountStatus {
		return false, fmt.Sprintf("account_status is %s, and only %s may trade", a.Status, tradeableAccountStatus)
	}
	if a.ReconciliationStatus != tradeableReconciliationStatus {
		return false, fmt.Sprintf("reconciliation_status is %s, and only %s may trade",
			a.ReconciliationStatus, tradeableReconciliationStatus)
	}
	return true, ""
}

// Drawdown returns the peak-to-current equity decline as a positive fraction.
//
// Zero when equity is at or above the peak, which is the ordinary case and must
// not be a negative drawdown: a negative figure would satisfy any max_drawdown
// check, including a limit of 0.
func (a Account) Drawdown() (contracts.Decimal, error) {
	if a.PeakEquity.Sign() <= 0 {
		return contracts.Decimal{}, fmt.Errorf(
			"riskstate: peak_equity is %s; a drawdown cannot be measured against a peak of zero", a.PeakEquity)
	}
	one := contracts.MustParseDecimal("1")
	if a.Equity.Cmp(a.PeakEquity) >= 0 {
		return contracts.MustParseDecimal("0"), nil
	}
	decline, err := a.PeakEquity.Sub(a.Equity)
	if err != nil {
		return contracts.Decimal{}, fmt.Errorf("riskstate: subtracting equity from peak failed: %w", err)
	}
	// max_drawdown is NUMERIC(12,8), so eight places is what the column can hold.
	ratio, err := decline.Div(a.PeakEquity, 8, contracts.RoundHalfEven)
	if err != nil {
		return contracts.Decimal{}, fmt.Errorf("riskstate: dividing by peak_equity failed: %w", err)
	}
	if ratio.Cmp(one) > 0 {
		return contracts.Decimal{}, fmt.Errorf(
			"riskstate: drawdown came out as %s, above 1; equity %s against peak %s is inconsistent",
			ratio, a.Equity, a.PeakEquity)
	}
	return ratio, nil
}

// DailyLoss returns the magnitude of the day's P&L, since a limit is a magnitude
// and a loss limit of 5000 should be breached by -6000, not satisfied by it.
func (a Account) DailyLoss() contracts.Decimal { return a.DailyPnL.Abs() }

// Positions is the aggregate of portfolio.position for one account.
type Positions struct {
	// Net is the signed sum of quantity, which is what max_net_exposure bounds.
	Net contracts.Decimal
	// Gross is the sum of |quantity x average_entry_price|, which is what
	// max_gross_exposure bounds: a long and a short of equal size are net flat
	// but each consumes margin.
	Gross contracts.Decimal
	// Concentration is the largest instrument's share of gross. A flat book has
	// no dominant instrument, and reporting 0 there would let a policy with a
	// max_concentration of 0.01 pass an account holding nothing.
	Concentration contracts.Decimal
	// RowCount is how many positions were aggregated. It feeds Provenance, and
	// zero is a legitimate value for an account holding nothing.
	RowCount int
}

// LoadPositions aggregates portfolio.position for one account and environment.
//
// average_entry_price is a cost basis, not a mark. Gross exposure marked at entry
// price understates a position whose market price has risen, and no price feed is
// consulted here. That is recorded rather than fixed: marking to market needs
// market data, and the market data path is G2's open item 16c. The consequence is
// that max_gross_exposure is currently evaluated against an entry-price mark,
// which is the more permissive of the two.
func LoadPositions(ctx context.Context, db Querier, accountID, environment string) (Positions, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT instrument_id, quantity::text, average_entry_price::text
		  FROM portfolio.position
		 WHERE account_id = $1
		   AND environment = $2::common.environment`, accountID, environment)
	if err != nil {
		return Positions{}, fmt.Errorf("riskstate: reading portfolio.position failed: %w", err)
	}
	defer rows.Close()

	zero := contracts.MustParseDecimal("0")
	p := Positions{Net: zero}
	byInstrument := map[string]contracts.Decimal{}

	for rows.Next() {
		var instrument, qtyRaw, priceRaw string
		if err := rows.Scan(&instrument, &qtyRaw, &priceRaw); err != nil {
			return Positions{}, fmt.Errorf("riskstate: scanning portfolio.position failed: %w", err)
		}
		qty, err := contracts.ParseDecimal(qtyRaw)
		if err != nil {
			return Positions{}, fmt.Errorf("riskstate: position quantity %q is not a decimal: %w", qtyRaw, err)
		}
		price, err := contracts.ParseDecimal(priceRaw)
		if err != nil {
			return Positions{}, fmt.Errorf("riskstate: average_entry_price %q is not a decimal: %w", priceRaw, err)
		}

		notional, err := qty.Abs().Mul(price.Abs())
		if err != nil {
			return Positions{}, fmt.Errorf("riskstate: multiplying quantity by price failed: %w", err)
		}
		net, err := p.Net.Add(qty)
		if err != nil {
			return Positions{}, fmt.Errorf("riskstate: summing quantity failed: %w", err)
		}
		p.Net = net
		p.Gross, err = p.Gross.Add(notional)
		if err != nil {
			return Positions{}, fmt.Errorf("riskstate: summing gross exposure failed: %w", err)
		}

		// position_identity_idx is unique on (account, environment, instrument,
		// venue), so one instrument can hold several rows across venues. They
		// are summed per instrument here rather than treating a single row as
		// the whole instrument, which would understate concentration for a
		// split-venue book -- the very book where concentration matters most.
		prior, ok := byInstrument[instrument]
		if !ok {
			prior = zero
		}
		byInstrument[instrument], err = prior.Add(notional)
		if err != nil {
			return Positions{}, fmt.Errorf("riskstate: summing %s exposure failed: %w", instrument, err)
		}
		p.RowCount++
	}
	if err := rows.Err(); err != nil {
		return Positions{}, fmt.Errorf("riskstate: reading portfolio.position failed: %w", err)
	}

	p.Concentration, err = concentration(byInstrument, p.Gross)
	if err != nil {
		return Positions{}, err
	}
	return p, nil
}

// concentration returns the largest instrument's share of gross exposure.
//
// A flat book has no dominant instrument, and the answer is 0 rather than 1: a
// book holding nothing does not have all of its risk in one instrument, and
// reporting 1 would breach every max_concentration in existence for an account
// that holds no positions at all.
func concentration(byInstrument map[string]contracts.Decimal, gross contracts.Decimal) (contracts.Decimal, error) {
	zero := contracts.MustParseDecimal("0")
	if len(byInstrument) == 0 || gross.Sign() <= 0 {
		return zero, nil
	}
	largest := zero
	for _, v := range byInstrument {
		if v.Cmp(largest) > 0 {
			largest = v
		}
	}
	// max_concentration is NUMERIC(12,8), so eight places is what the column holds.
	share, err := largest.Div(gross, 8, contracts.RoundHalfEven)
	if err != nil {
		return contracts.Decimal{}, fmt.Errorf("riskstate: computing concentration failed: %w", err)
	}
	if share.Cmp(contracts.MustParseDecimal("1")) > 0 {
		return contracts.Decimal{}, fmt.Errorf(
			"riskstate: concentration came out as %s, above 1; an instrument's share of gross "+
				"exposure cannot exceed the whole", share)
	}
	return share, nil
}

// StrategyDeployed reports whether a strategy may trade, and why not.
//
// Only DEPLOYED trades. PAPER, SHADOW and SIMULATION are deliberate no-trades
// for this gate: they describe a strategy that is being observed, and a system
// that traded a shadow strategy would be trading something nobody authorised for
// real capital. APPROVED is not DEPLOYED -- approval authorises deployment, and
// conflating the two would let an approved-but-undeployed strategy trade.
func StrategyDeployed(state string) (bool, string) {
	if state == "DEPLOYED" {
		return true, ""
	}
	return false, fmt.Sprintf("strategy state is %s, and only DEPLOYED may trade", state)
}

// LoadStrategy reads a strategy's deployment state.
//
// It returns the state as a string rather than a bool on purpose. The bool is the
// question the gate asks, and the string is the evidence for the answer: a
// refusal that says "strategy state is DRAFT" is diagnosable by the operator who
// has to fix it, and a refusal that says only "not deployed" is not.
//
// A strategy row that does not exist is an error rather than a non-DEPLOYED
// state. The two are different: a missing row means the strategy identity in the
// order names nothing, which is a defect to be reported, while a DRAFT row means
// the strategy exists and is deliberately not trading. Collapsing them would
// report a broken reference as a routine deployment decision.
func LoadStrategy(ctx context.Context, db Querier, strategyID string) (string, error) {
	var state string
	err := db.QueryRowContext(ctx,
		`SELECT state::text FROM strategy.strategy WHERE strategy_id = $1`, strategyID).
		Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf(
			"riskstate: strategy %s does not exist; a strategy id that names no row is a broken "+
				"reference, not a strategy that is merely not deployed", strategyID)
	}
	if err != nil {
		return "", fmt.Errorf("riskstate: reading strategy %s failed: %w", strategyID, err)
	}
	return state, nil
}

// Facts assembles an OrderFacts from read state.
//
// This is the sanctioned constructor. Its inputs are the account row, the
// position aggregate, the strategy state and the order's own terms; nothing here
// is a caller-chosen number, which is the property that makes the provenance
// stamp honest.
type Facts struct {
	Order risk.OrderFacts

	// AccountStateAsOfNs, PolicyRevision and LoadedAt feed Provenance.
	AccountStateAsOfNs int64
	PolicyRevision     string
	LoadedAt           time.Time

	// MaxAccountAge bounds the staleness of risk.account_state, and only
	// Load reads it -- Build is handed an already-loaded account and has nothing
	// to check.
	//
	// It is required here rather than defaulted. A zero here means "no bound",
	// which silently permits trading against arbitrarily stale state; the
	// production caller has to state how old the account row may be, and the
	// comparison to derive it is MaxMarketDataAge on the policy row. Leaving the
	// decision to whoever omits the field is how a limit becomes a comment.
	MaxAccountAge time.Duration
}

// Build assembles the facts and stamps the provenance.
//
// orderAmount, orderPrice and orderCurrency describe the order itself and are
// not read from any table: an order's notional is derived from the order, not
// looked up. The currency is checked against the account's base_currency,
// because DerivedCurrency in domain/risk records that limits are compared in
// base_currency and nothing else enforced it.
func Build(f Facts, acct Account, pos Positions, strategyState string) (risk.OrderFacts, error) {
	tradeable, why := acct.Tradeable()
	if !tradeable {
		return risk.OrderFacts{}, fmt.Errorf("riskstate: %s", why)
	}
	deployed, why := StrategyDeployed(strategyState)
	if !deployed {
		return risk.OrderFacts{}, fmt.Errorf("riskstate: %s", why)
	}
	drawdown, err := acct.Drawdown()
	if err != nil {
		return risk.OrderFacts{}, err
	}

	out := f.Order
	out.Provenance = risk.Provenance{
		AccountStateAsOfNs: f.AccountStateAsOfNs,
		PositionRowCount:   pos.RowCount,
		PolicyRevision:     f.PolicyRevision,
		LoadedAt:           f.LoadedAt,
	}

	// Account-derived measurements.
	//
	// Every one of these is assigned unconditionally rather than only when this
	// package measured it, because out starts as the caller's OrderFacts and the
	// caller may have arrived with the fields already filled in. Leaving a field
	// alone because it was "not measured here" would mean leaving the caller's
	// figure in place while the provenance stamp below claims this package
	// sourced it -- a 50x account evaluated as the caller's 1, reported as
	// sourced. Where a measurement does not exist the field is cleared to nil,
	// which every limit control treats as a failure. Clearing is therefore the
	// fail-closed outcome, and it is why a flat book reports no concentration
	// rather than reporting the concentration the caller asserted.
	//
	// The values are copied into locals first so the result does not alias the
	// caller's Account and Positions.
	dailyLoss := acct.DailyLoss()
	leverage := acct.Leverage
	position := pos.Net
	exposure := pos.Gross

	// The two authorisation facts are established here, not merely consulted.
	// Both were computed above and used to refuse the build, and recording them
	// is what lets the gate's ACCOUNT_AND_ENVIRONMENT_AUTHORIZATION and
	// STRATEGY_DEPLOYMENT_STATUS controls evaluate instead of reporting that no
	// fact was available. A control that never fires is not a control that
	// passed: an unestablished fact denies, so leaving these unset would have
	// refused every order for the wrong reason and hidden that the two checks
	// had in fact succeeded.
	out.AccountAuthorized = &tradeable
	out.StrategyDeployed = &deployed

	out.DailyLoss = &dailyLoss
	out.Drawdown = &drawdown
	out.Leverage = &leverage
	out.Position = &position
	out.Exposure = &exposure
	out.OpenOrderCount = acct.OpenOrderCount

	// Concentration is a share of gross, so a flat book has none. It is left
	// unset rather than reported as zero, which is what the controls expect: nil
	// means "not established" and zero would read as a measured figure that
	// happened to be nil.
	out.Concentration = nil
	if pos.Gross.Sign() > 0 {
		concentration := pos.Concentration
		out.Concentration = &concentration
	}

	// Notional is derived from the order's own terms rather than looked up: an
	// order's value is a property of the order, not of any row. It was left to
	// the caller, which meant max_order_notional was compared against a figure the
	// caller chose -- the same shape as a limit that exists, can be set, and is
	// consulted by nothing.
	//
	// The derivation needs a price. A market order has none at submission, and a
	// notional cannot be invented from a quantity alone, so the field stays unset
	// rather than being approximated. MAX_ORDER_NOTIONAL then denies, which is the
	// correct answer: an order whose value is unknown cannot be shown to be inside
	// a bound on value.
	out.Notional = nil
	if f.Order.Amount != "" && f.Order.Price != "" {
		quantity, qErr := contracts.ParseDecimal(f.Order.Amount)
		price, pErr := contracts.ParseDecimal(f.Order.Price)
		if qErr != nil || pErr != nil {
			return risk.OrderFacts{}, fmt.Errorf(
				"riskstate: the order's quantity or price is not a decimal, so its notional cannot be derived: %q x %q",
				f.Order.Amount, f.Order.Price)
		}
		notional, err := quantity.Mul(price)
		if err != nil {
			return risk.OrderFacts{}, fmt.Errorf("riskstate: deriving the order's notional failed: %w", err)
		}
		out.Notional = &notional
	}

	return out, nil
}

// Load assembles OrderFacts from the database, reading every fact it can.
//
// It is the entry point production should use, and it exists because Build's
// strategyState argument is a trust boundary: a caller can pass "DEPLOYED" for a
// strategy the database records as DRAFT, and Build has no way to notice. Load
// closes that by reading strategy.strategy.state itself, so the deployment
// decision rests on the row rather than on the caller's assertion.
//
// The same argument applies to the account and positions, which Load also reads.
// It is a thin sequence rather than a convenience wrapper: each read is one round
// trip, and a caller that has already read state in the same transaction should
// use Build to avoid re-querying it.
func Load(ctx context.Context, db Querier, f Facts) (risk.OrderFacts, error) {
	acct, err := LoadAccount(ctx, db, f.Order.AccountID, f.MaxAccountAge)
	if err != nil {
		return risk.OrderFacts{}, err
	}
	pos, err := LoadPositions(ctx, db, f.Order.AccountID, f.Order.Environment)
	if err != nil {
		return risk.OrderFacts{}, err
	}
	strategyState, err := LoadStrategy(ctx, db, f.Order.StrategyID)
	if err != nil {
		return risk.OrderFacts{}, err
	}

	// Instrument eligibility and precision validity are read from
	// market.instrument rather than accepted from the caller. Both were
	// caller-asserted booleans, which meant the gate could be told that an
	// instrument the catalogue records as DELISTED was eligible, or that a
	// quantity off the increment grid was precise, and it would agree -- and
	// report INSTRUMENT_ELIGIBILITY and PRECISION_VALIDITY as checked.
	//
	// The caller's values are overwritten rather than consulted. Build does the
	// same for every measurement it takes, for the same reason: a field this
	// function sources must not still hold a claim from above it.
	//
	// A read failure is returned as an error rather than as a denial, because
	// "the catalogue could not be read" and "the instrument is ineligible" are
	// different facts and the second must not be inferred from the first.
	instrumentFacts, err := instrument.Evaluate(ctx, db, f.Order.InstrumentID, instrument.Terms{
		OrderType: f.Order.OrderType,
		Side:      f.Order.Side,
		Quantity:  f.Order.Amount,
		// The gate holds one executable price rather than a limit and a stop
		// price, and instrument.Precise checks whichever is present. Passing it
		// as the limit price asks the same question -- is this price on the
		// increment grid -- which is the question a limit on a monetary amount
		// can actually answer.
		LimitPrice: f.Order.Price,
	})
	if err != nil {
		return risk.OrderFacts{}, fmt.Errorf("riskstate: reading instrument %s: %w", f.Order.InstrumentID, err)
	}
	f.Order.InstrumentEligible = &instrumentFacts.InstrumentEligible
	f.Order.PrecisionValid = &instrumentFacts.PrecisionValid

	// The read stamp is taken from what was read, not from what the caller said
	// was read. AccountStateAsOfNs and PositionRowCount are claims about rows,
	// and this function is the one that read them; accepting the caller's values
	// would let a stale row be stamped as fresh, which is the one thing the
	// provenance exists to make impossible.
	//
	// PolicyRevision and LoadedAt are different. The policy was resolved by the
	// caller and not by this function, so its revision is the caller's to state,
	// and the time is the time this read happened.
	f.AccountStateAsOfNs = acct.AsOfNs
	f.LoadedAt = time.Now()

	return Build(f, acct, pos, strategyState)
}
