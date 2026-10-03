// Package instrument sources the two facts the risk gate was being told about
// instead of measuring: whether an instrument is eligible, and whether the order's
// terms conform to its increments.
//
// Authority: 16_MARKET_DATA_AND_VENUE_ADAPTERS.md, 04_TRADING_DOMAIN_AND_RISK.md.
//
// # Why this package exists
//
// risk.OrderFacts carried InstrumentEligible and PrecisionValid as booleans the
// caller filled in. Both are facts about a row in market.instrument, which holds
// trading_status, order_types, supports_shorting, min_quantity, price_increment
// and min_notional. So the gate could be told an instrument that the database
// records as DELISTED was tradable, or that a quantity off the increment grid was
// precise, and it would agree -- and report the instrument control as checked.
//
// Migration 0019 already enforces the same rules in the database, at order
// creation and at the risk boundary. So this package is not inventing a policy.
// It is the Go reading of rules the schema already enforces, and
// dbtest/instrument_parity_test.go pins the two to each other so they cannot
// drift.
//
// # It reads, it does not decide
//
// The Go/PostgreSQL duplication is a real cost and is taken deliberately: the
// schema cannot express "an order was refused" as a result a caller can read, and
// a Go-only path that recomputes the answer before submitting would otherwise
// submit an order the database then rejects. Having the gate refuse first, with
// the database agreeing, is defence in depth. It is only safe while the two are
// proven equivalent, which is what the parity test is for.
package instrument

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aitc/trade/contracts"
)

// Querier is the read surface this package needs. *sql.DB and *sql.Tx both
// satisfy it, so instrument state can be read inside a caller's transaction.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Instrument is one market.instrument row, read whole enough to answer both
// questions the gate asks.
type Instrument struct {
	ID            string
	MarketClass   string
	TradingStatus string

	// Increments. Numeral() rather than string so that a NULL min_notional is
	// distinguishable from a declared zero: the first means "no floor", the second
	// is a floor of zero and means something quite different.
	MinQuantity    string
	PriceIncrement string
	TickSize       string
	MinNotional    contracts.Decimal
	HasMinNotional bool

	// Capabilities. OrderTypes are the order types the instrument declares; a
	// venue adapter may impose a stricter set, never a looser one.
	OrderTypes       []string
	SupportsShorting bool
}

// tradingStatusOpen is the only status that permits opening exposure.
//
// It mirrors market.instrument_is_tradable. UNKNOWN denies, and the default of
// the column is UNKNOWN precisely because that is the honest state of an
// instrument nobody has confirmed. AUCTION and PRE_OPEN deny too: they are
// states where orders are accepted but not filled, and whether the platform may
// submit into them is a venue-adapter question (G8), not one this package decides.
const tradingStatusOpen = "OPEN"

// Load reads one instrument.
//
// A row that does not exist is an error rather than a set of zero values. A
// missing instrument would otherwise produce min_quantity 0, and 0 is a
// denominator the increment test cannot divide by -- so the missing row would
// surface later as an arithmetic error in a different place, naming the wrong
// cause. Failing here names the actual problem: the order references an
// instrument the catalogue does not hold.
func Load(ctx context.Context, db Querier, instrumentID string) (Instrument, error) {
	var (
		in           Instrument
		minNotional  sql.NullString
		orderTypes   string
		shortingText string
	)
	err := db.QueryRowContext(ctx, `
		SELECT instrument_id, market_class::text, trading_status::text,
		       min_quantity::text, price_increment::text, tick_size::text,
		       min_notional::text, order_types::text, supports_shorting::text
		  FROM market.instrument
		 WHERE instrument_id = $1`, instrumentID).
		Scan(&in.ID, &in.MarketClass, &in.TradingStatus,
			&in.MinQuantity, &in.PriceIncrement, &in.TickSize,
			&minNotional, &orderTypes, &shortingText)
	if errors.Is(err, sql.ErrNoRows) {
		return Instrument{}, fmt.Errorf(
			"instrument: %s is not in market.instrument; an order naming an instrument the "+
				"catalogue does not hold cannot be checked against its increments or status", instrumentID)
	}
	if err != nil {
		return Instrument{}, fmt.Errorf("instrument: reading %s failed: %w", instrumentID, err)
	}

	if minNotional.Valid {
		parsed, err := contracts.ParseDecimal(minNotional.String)
		if err != nil {
			return Instrument{}, fmt.Errorf(
				"instrument: min_notional %q on %s is not a decimal: %w", minNotional.String, instrumentID, err)
		}
		in.MinNotional = parsed
		in.HasMinNotional = true
	}
	if err := in.setOrderTypes(orderTypes); err != nil {
		return Instrument{}, err
	}
	in.SupportsShorting = shortingText == "true"
	return in, nil
}

// setOrderTypes parses the PostgreSQL array literal pgx returns for a
// common.order_type[] column.
//
// The driver hands back a literal string such as {MARKET,LIMIT}, not a []string,
// which is the same trap riskpolicy hit on text[]; parsing it here means the
// capability check cannot silently see an empty list and deny every order type.
func (in *Instrument) setOrderTypes(literal string) error {
	body := literal
	if len(body) >= 2 && body[0] == '{' && body[len(body)-1] == '}' {
		body = body[1 : len(body)-1]
	}
	if body == "" {
		in.OrderTypes = nil
		return nil
	}
	for _, part := range splitArrayLiteral(body) {
		in.OrderTypes = append(in.OrderTypes, part)
	}
	return nil
}

// splitArrayLiteral splits a PostgreSQL array body on unquoted commas.
func splitArrayLiteral(body string) []string {
	var (
		out     []string
		current []rune
		inQuote bool
	)
	for _, r := range body {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ',' && !inQuote:
			out = append(out, string(current))
			current = nil
		default:
			current = append(current, r)
		}
	}
	return append(out, string(current))
}

// Tradable reports whether the instrument may open exposure now.
//
// Tradability is distinct from capability: an instrument may support MARKET and
// still not be trading, and both must hold for an order to be eligible.
func (in Instrument) Tradable() (bool, string) {
	if in.TradingStatus == tradingStatusOpen {
		return true, ""
	}
	return false, fmt.Sprintf("instrument %s is in trading_status %s, and only %s may open exposure",
		in.ID, in.TradingStatus, tradingStatusOpen)
}

// Permits reports whether the instrument declares support for this order type and
// side. It mirrors market.instrument_permits.
func (in Instrument) Permits(orderType, side string) (bool, string) {
	// An unrecognised side is refused here rather than treated as a buy. The
	// shorting check below is keyed on side == "SELL", so a side this package
	// does not recognise would pass straight through it and an instrument that
	// cannot be shorted would permit a short. The check belongs in this function
	// rather than in Evaluate so that every caller inherits it.
	if !contracts.Side(side).Valid() {
		return false, fmt.Sprintf(
			"instrument %s cannot be asked whether it permits side %q; side is neither BUY nor SELL", in.ID, side)
	}

	found := false
	for _, t := range in.OrderTypes {
		if t == orderType {
			found = true
			break
		}
	}
	if !found {
		return false, fmt.Sprintf("instrument %s does not declare order_type %s", in.ID, orderType)
	}

	// A sell is short exposure unless it is closing. Closing actions are
	// permitted on an instrument that cannot be shorted, because reducing an
	// existing long by selling is not shorting -- and a control that blocks
	// flattening is a control that gets switched off in the moment it is needed.
	if side == "SELL" && !in.SupportsShorting && !closing(orderType) {
		return false, fmt.Sprintf(
			"instrument %s does not support shorting, and %s is not a closing action", in.ID, orderType)
	}
	return true, ""
}

// closing reports whether an order type reduces exposure rather than increasing it.
//
// It delegates rather than comparing literals. This was a fourth spelling of the
// same two-value set -- the others having been package oms's deleted OrderType
// and the RiskReducing field carried into domain/risk -- and a set this narrow
// decides whether an instrument that cannot be shorted will still accept a SELL.
// One definition, in contracts, and everything else asks it.
func closing(orderType string) bool {
	return !contracts.OrderType(orderType).RiskIncreasing()
}

// Terms is the order's own specification, as strings so nothing on this path is
// ever an IEEE-754 value.
type Terms struct {
	OrderType  string
	Side       string
	Quantity   string
	LimitPrice string // "" when the order carries no limit price
	StopPrice  string // "" when the order carries no stop price
}

// Precise reports whether the terms conform to the instrument's increments and
// its declared minimum notional. It mirrors market.assert_order_terms_permitted.
//
// It returns the first reason a term fails, named, because a caller that cannot
// say which bound was violated cannot fix it and a caller that is told only
// "precision invalid" will treat it as an intermittent fault.
func (in Instrument) Precise(terms Terms) (bool, string) {
	quantity, err := contracts.ParseDecimal(terms.Quantity)
	if err != nil {
		return false, fmt.Sprintf("instrument: quantity %q is not a decimal: %v", terms.Quantity, err)
	}
	if ok, err := in.incrementMultiple(quantity, in.MinQuantity); err != nil {
		return false, err.Error()
	} else if !ok {
		return false, fmt.Sprintf(
			"quantity %s is not a whole multiple of min_quantity %s for instrument %s",
			terms.Quantity, in.MinQuantity, in.ID)
	}

	// Which price is the executable one. A limit order is priced by limit_price, a
	// stop order by stop_price, and a market order has neither.
	price, priceSet, err := executablePrice(terms)
	if err != nil {
		return false, err.Error()
	}
	if priceSet {
		if ok, err := in.incrementMultiple(price, in.PriceIncrement); err != nil {
			return false, err.Error()
		} else if !ok {
			return false, fmt.Sprintf(
				"price %s is not a whole multiple of price_increment %s for instrument %s",
				price.String(), in.PriceIncrement, in.ID)
		}
	}

	// min_notional is the one increment-like bound that is optional. When it is
	// declared it is a floor on the value of the order, and an order whose value
	// cannot be computed -- a market order, which has no price at submission --
	// cannot be shown to clear that floor. Declared but unverifiable denies.
	if in.HasMinNotional {
		if !priceSet {
			return false, fmt.Sprintf(
				"instrument %s declares min_notional %s but this order has no limit or stop price to value it against",
				in.ID, in.MinNotional)
		}
		notional, err := quantity.Mul(price)
		if err != nil {
			return false, fmt.Sprintf("instrument: computing notional failed: %v", err)
		}
		if notional.Cmp(in.MinNotional) < 0 {
			return false, fmt.Sprintf(
				"notional %s is below min_notional %s for instrument %s",
				notional, in.MinNotional, in.ID)
		}
	}
	return true, ""
}

// executablePrice returns the price the order is actually executed at, and
// whether one is set at all.
func executablePrice(terms Terms) (contracts.Decimal, bool, error) {
	for _, raw := range []string{terms.LimitPrice, terms.StopPrice} {
		if raw == "" {
			continue
		}
		price, err := contracts.ParseDecimal(raw)
		if err != nil {
			return contracts.Decimal{}, false, fmt.Errorf("instrument: price %q is not a decimal: %v", raw, err)
		}
		return price, true, nil
	}
	return contracts.Decimal{}, false, nil
}

// incrementMultiple reports whether value is a whole number of increments.
//
// market.increment_multiple expresses this as
//
//	quantity / increment = trunc(quantity / increment)
//
// on NUMERIC(38,18) columns, which is exact in PostgreSQL. This is the same
// property, evaluated on exact decimals.
//
// The quotient is taken to zero places. That is not an arbitrary scale chosen to
// round the remainder away -- it is the only scale at which the answer means what
// the question means, because "is this a whole number of increments" has no scale.
// A finer scale would invite an answer that depends on the scale chosen; a coarser
// one would round rather than test.
//
// The test is therefore not the quotient but whether the quotient round-trips:
// whole*increment == value. See IncrementMultiple for why that is sound in both
// directions.
func (in Instrument) incrementMultiple(value contracts.Decimal, incrementRaw string) (bool, error) {
	increment, err := contracts.ParseDecimal(incrementRaw)
	if err != nil {
		return false, fmt.Errorf("instrument: increment %q on %s is not a decimal: %v", incrementRaw, in.ID, err)
	}
	ok, err := IncrementMultiple(value, increment)
	if err != nil {
		return false, fmt.Errorf("instrument: %w (increment %s on %s)", err, incrementRaw, in.ID)
	}
	return ok, nil
}

// IncrementMultiple reports whether value is a whole number of increments, for any
// value and increment. It is exported because the parity test calls it directly
// against the SQL function over the same vectors.
//
// A non-positive or absent increment is a denial, not a licence to proceed: an
// unknown increment is a deny condition, and market.instrument's CHECK constraints
// make the zero case unreachable, so this is the guard for a caller that bypassed
// them.
//
// The test is whole/increment == trunc(whole/increment), evaluated by dividing to
// zero places and multiplying back. That is sound in both directions and is not a
// restatement of the rule:
//
//   - If value is a whole multiple, value/increment is exactly an integer, so the
//     division rounds nothing and the product reconstructs value.
//   - If the product equals value, then value is an integer multiple of increment.
//     A value that is not a whole multiple can never round-trip, because the
//     reconstructed quantity is itself always a whole multiple.
//
// An earlier form multiplied out the two coefficients and asked whether the
// division left a remainder. It was exact, and it is replaced here because it
// needed a coefficient accessor on contracts.Decimal that the compiler does not
// resolve; see method note "an exported method the toolchain will not resolve".
func IncrementMultiple(value, increment contracts.Decimal) (bool, error) {
	if increment.Sign() <= 0 {
		return false, fmt.Errorf("instrument: increment %s is not positive; an unknown increment denies", increment)
	}
	whole, err := value.Div(increment, 0, contracts.RoundHalfEven)
	if err != nil {
		return false, fmt.Errorf("instrument: %s / %s: %w", value.String(), increment.String(), err)
	}
	rebuilt, err := whole.Mul(increment)
	if err != nil {
		return false, fmt.Errorf("instrument: %s * %s: %w", whole.String(), increment.String(), err)
	}
	return rebuilt.Cmp(value) == 0, nil
}

// Facts is what this package contributes to a risk decision, and why.
type Facts struct {
	// InstrumentEligible requires BOTH that the instrument is tradable and that
	// it declares support for these order terms. It is one boolean because the
	// gate has one instrument control, and splitting it would invite a caller to
	// satisfy the permissive half.
	InstrumentEligible bool
	InstrumentReason   string

	// PrecisionValid reports conformance to the increments and the declared
	// minimum notional.
	PrecisionValid  bool
	PrecisionReason string
}

// Evaluate reads the instrument and answers both questions.
//
// A read failure is an error, not a denial: the caller must not treat "the
// catalogue could not be read" as "the instrument is ineligible" and proceed to
// some other control, because the difference is between a known refusal and an
// unknown one.
func Evaluate(ctx context.Context, db Querier, instrumentID string, terms Terms) (Facts, error) {
	in, err := Load(ctx, db, instrumentID)
	if err != nil {
		return Facts{}, err
	}

	var f Facts

	tradable, tradableWhy := in.Tradable()
	permits, permitsWhy := in.Permits(terms.OrderType, terms.Side)
	switch {
	case !tradable:
		f.InstrumentReason = tradableWhy
	case !permits:
		f.InstrumentReason = permitsWhy
	default:
		f.InstrumentEligible = true
	}

	precise, preciseWhy := in.Precise(terms)
	f.PrecisionValid = precise
	f.PrecisionReason = preciseWhy
	if precise {
		f.PrecisionReason = ""
	}
	return f, nil
}
