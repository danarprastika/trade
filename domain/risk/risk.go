// Package risk implements the deterministic pre-trade risk gate.
//
// Doc 04 "Risk gate" enumerates fifteen controls that every risk-increasing order
// must pass. This package is that gate. Doc 17 §2 requires the platform to start
// everything disabled and to reject on missing limits; doc 17 §3 forbids the
// platform from supplying universal numeric trading limits.
//
// Those three statements combine into a constraint that shapes the whole
// package: the engine is complete, and the numbers are not. There is no default
// notional, no default position cap, no default drawdown. An order evaluated
// with no ACTIVE policy is denied every limit control, and the denial names the
// absence so an operator can act on it.
//
// This is the difference between failing closed and having no opinion. A default
// of "100k notional" would be a number the platform invented about somebody
// else's capital, which doc 17 §3 prohibits in terms. Failing closed produces a
// refusal that is correct today and correct tomorrow, and becomes permissive
// only when a human supplies a risk.policy row and approves it.
//
// # This is a projection of risk.policy, not a design of its own
//
// An earlier version of this package defined a generic Limit value carrying its
// own unit, aggregation scope, measurement window, boundary inclusivity, source
// of truth and missing-data behaviour. That was invented here and turned out to
// describe a different thing from the one the database already stores:
// risk.policy has been a fixed set of twelve NOT NULL limit columns since
// migration 0003, and nothing ever compared the two.
//
// That is the same shape as the canonical ID defect this repository has already
// had to undo once, where two implementations of one concept agreed on
// everything observable and differed in the rest. Policy below therefore names
// columns that exist, with their types, nullability and CHECK constraints, and
// represents nothing the schema does not carry. Three doc 17 §3 attributes have
// no column -- per-limit currency, boundary inclusivity and missing-data
// behaviour -- and are recorded as derived rules (DerivedCurrency,
// DerivationBoundary) rather than invented.
//
// dbtest/domain_parity_test.go asserts that every column named here exists in
// risk.policy with the expected type and nullability, so this file cannot drift
// away from the schema without a test failing.
package risk

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aitc/trade/contracts"
)

// Severity distinguishes controls that can refuse an order from controls that
// only advise. It mirrors the risk.control_severity enum in migration 0003.
//
// Doc 04: "A single failed mandatory control rejects the order." There is no
// scoring, no weighting, and no partial approval. That is deliberate -- a
// weighted score lets a severe failure be outvoted by several trivial ones, and
// a system whose safety depends on arithmetic the reader cannot check is not a
// safety system.
type Severity string

const (
	// Mandatory failures refuse the order.
	Mandatory Severity = "MANDATORY"
	// Advisory failures are recorded and reported but do not refuse.
	Advisory Severity = "ADVISORY"
)

// Control identifies one of the fifteen checks in doc 04's risk gate.
//
// The list is closed and the order is the order in the document, because a
// reader comparing this file to doc 04 should be able to do it by eye.
type Control string

const (
	// 1. Authorization.
	CtrlAccountAuthorized Control = "ACCOUNT_AND_ENVIRONMENT_AUTHORIZATION"
	CtrlStrategyDeployed  Control = "STRATEGY_DEPLOYMENT_STATUS"
	// 2. Eligibility.
	CtrlInstrumentEligible Control = "INSTRUMENT_ELIGIBILITY"
	CtrlVenueAvailable     Control = "VENUE_AVAILABILITY"
	// 3. Precision.
	CtrlPricePrecision Control = "PRICE_AND_QUANTITY_PRECISION"
	// 4. Limits.
	CtrlNotionalLimit      Control = "MAX_ORDER_NOTIONAL"
	CtrlPositionLimit      Control = "POSITION_LIMIT"
	CtrlExposureLimit      Control = "EXPOSURE_LIMIT"
	CtrlConcentrationLimit Control = "CONCENTRATION_LIMIT"
	CtrlLeverageLimit      Control = "LEVERAGE_LIMIT"
	CtrlLossLimit          Control = "LOSS_AND_DRAWDOWN_CONTROL"
	// 5. Market data.
	CtrlMarketDataFreshness Control = "MARKET_DATA_FRESHNESS"
	// 6. Duplication and rate.
	CtrlDuplicateOrder Control = "DUPLICATE_ORDER_DETECTION"
	CtrlRateLimit      Control = "RATE_AND_THROTTLE_LIMIT"
	// 7. Halt.
	CtrlHaltState Control = "KILL_AND_HALT_STATE"
)

// controls is the closed enumeration in doc 04's order.
var controls = []Control{
	CtrlAccountAuthorized,
	CtrlStrategyDeployed,
	CtrlInstrumentEligible,
	CtrlVenueAvailable,
	CtrlPricePrecision,
	CtrlNotionalLimit,
	CtrlPositionLimit,
	CtrlExposureLimit,
	CtrlConcentrationLimit,
	CtrlLeverageLimit,
	CtrlLossLimit,
	CtrlMarketDataFreshness,
	CtrlDuplicateOrder,
	CtrlRateLimit,
	CtrlHaltState,
}

var controlSet = func() map[Control]struct{} {
	m := make(map[Control]struct{}, len(controls))
	for _, c := range controls {
		m[c] = struct{}{}
	}
	return m
}()

// Controls returns every control in doc 04's order. The result is a copy.
func Controls() []Control {
	out := make([]Control, len(controls))
	copy(out, controls)
	return out
}

// Known reports whether c is a declared control.
func (c Control) Known() bool {
	_, ok := controlSet[c]
	return ok
}

// String makes Control printable in findings without a cast at every call.
func (c Control) String() string { return string(c) }

// RequiresLimit reports whether the control compares an order against a
// configured numeric limit, and therefore denies when none is configured.
//
// Seven of the fifteen controls take a number. Six compare a decimal exposure
// figure; RATE_AND_THROTTLE_LIMIT compares three integer counts. The other eight
// answer from the order and its environment, or from the halt table, and
// consult no policy column at all.
func (c Control) RequiresLimit() bool {
	switch c {
	case CtrlNotionalLimit, CtrlPositionLimit, CtrlExposureLimit,
		CtrlConcentrationLimit, CtrlLeverageLimit, CtrlLossLimit, CtrlRateLimit:
		return true
	default:
		return false
	}
}

// Policy is the Go projection of the risk.policy row.
//
// This type used to be a generic LimitSet: a slice of Limit values, each
// carrying its own unit, aggregation scope, measurement window, boundary
// inclusivity, source of truth and missing-data behaviour. That model was
// invented in this package and turned out to describe a different thing from the
// one the database already stores. risk.policy has been a fixed set of twelve
// NOT NULL limit columns since migration 0003, and nothing was ever compared
// against it -- which is the same shape as the canonical ID defect, where two
// implementations of one concept agreed on everything observable and differed in
// the rest.
//
// So this is a projection rather than a design. Every field below names a column
// that exists, with the column's type, its nullability and its CHECK. There is
// deliberately no free-form limit: a field the schema does not have cannot be
// represented here, because representing it would reintroduce the divergence.
//
// Three doc 17 §3 attributes have no column, and rather than invent them this
// type records them as derived rules:
//
//   - currency or unit          no column. Limits are compared in the denomination
//     of risk.account_state.base_currency. See DerivedCurrency.
//   - boundary inclusivity      no column. The schema's positive-limit CHECK
//     admits only values > 0, and an order is compared with "<=", so the
//     boundary is inclusive. See DerivationBoundary.
//   - behaviour on missing data no column. Every limit column is NOT NULL with a
//     > 0 CHECK, so a policy row cannot exist in a state where a limit is
//     absent. "Missing" is therefore unrepresentable rather than configured, and
//     an absent policy is handled by the caller as an absent policy.
type Policy struct {
	Revision    string
	Environment string
	ScopeKind   string
	ScopeValue  string
	Status      string

	// The twelve numeric limits. Types match the columns exactly; a NUMERIC
	// column is a contracts.Decimal and never a float.
	MaxOrderNotional    contracts.Decimal // NUMERIC(38,18) NOT NULL, > 0
	MaxOrderQuantity    contracts.Decimal // NUMERIC(38,18) NOT NULL, > 0
	MaxGrossExposure    contracts.Decimal // NUMERIC(38,18) NOT NULL, > 0
	MaxNetExposure      contracts.Decimal // NUMERIC(38,18) NOT NULL, > 0
	MaxLeverage         contracts.Decimal // NUMERIC(18,8)  NOT NULL, > 0
	MaxConcentration    contracts.Decimal // NUMERIC(12,8)  NOT NULL, > 0
	MaxOpenOrders       int64             // INTEGER      NOT NULL, > 0
	MaxDailyLoss        contracts.Decimal // NUMERIC(38,18) NOT NULL, > 0
	MaxDrawdown         contracts.Decimal // NUMERIC(12,8)  NOT NULL, <= 1
	MaxPriceDeviation   contracts.Decimal // NUMERIC(12,8)  NOT NULL, 0 <= x <= 1
	MaxOrdersPerMinute  int64             // INTEGER      NOT NULL, > 0
	MaxCancelsPerMinute int64             // INTEGER      NOT NULL, > 0

	// Loss measurement: the window and the system of record. These are the only
	// two doc 17 §3 attributes the schema carries as columns, and they apply to
	// the loss limits specifically rather than to every limit.
	LossWindow time.Duration
	LossSource string

	// MaxMarketDataAge bounds staleness for MARKET_DATA_FRESHNESS.
	MaxMarketDataAge time.Duration

	// The policy's own allowlists.
	//
	// risk.policy is not only twelve numbers. It also states, per policy, which
	// markets, venues, instruments, order types and sides are permitted at all.
	// That is a set of caps in the ordinary sense, and the gate below consults
	// the three that map onto controls doc 04 already enumerates:
	// permitted_markets and permitted_instruments for INSTRUMENT_ELIGIBILITY,
	// permitted_venues for VENUE_AVAILABILITY.
	//
	// An earlier version of this package read eligibility and availability
	// only as caller-supplied booleans, so these six array columns were never
	// consulted by anything. An operator who set permitted_instruments to a
	// short list got a policy that looked restrictive and behaved
	// unconditionally -- the same defect as a limit column that is set but
	// never read.
	//
	// permitted_sides, permitted_order_types and permitted_modes remain
	// unconsulted. Doc 04's fifteen controls include no side check, no
	// order-type check and no operational-mode check, and inventing one would
	// be inventing a control. They are recorded in
	// TestRiskPolicyColumnsNotConsultedByTheGate alongside the other gaps.
	PermittedInstruments []string
	PermittedVenues      []string
	PermittedMarkets     []string
}

// ProjectedColumns returns every risk.policy column the Go projection reads,
// whether as a limit bound, an allowlist, or policy lifecycle state.
//
// It exists so the drift check has a single source of truth. An earlier version
// of dbtest's parity test hand-listed the columns it expected, and immediately
// disagreed with the package about seventeen of them -- not because either was
// wrong, but because two hand-maintained lists are two things to keep in step.
// This function is the Go side's list; the test compares it against
// information_schema and reports anything neither side explains.
func ProjectedColumns() []string {
	out := []string{
		// Policy lifecycle and scope, read by Policy.Active and Validate.
		"policy_revision",
		"environment",
		"scope_kind",
		"scope_value",
		"status",
		"loss_window",
		"loss_source",
		"max_market_data_age",
		// The twelve numeric limit columns.
		"max_order_notional",
		"max_order_quantity",
		"max_gross_exposure",
		"max_net_exposure",
		"max_leverage",
		"max_concentration",
		"max_open_orders",
		"max_daily_loss",
		"max_drawdown",
		"max_price_deviation",
		"max_orders_per_minute",
		"max_cancels_per_minute",
		// Allowlists the gate consults.
		"permitted_instruments",
		"permitted_venues",
		// Read by PermitsMarket, which no control calls yet. Recorded rather
		// than wired: doc 04 has no market-eligibility control, and INSTRUMENT_
		// ELIGIBILITY already has one.
		"permitted_markets",
	}
	sort.Strings(out)
	return out
}

// PermitsInstrument reports whether the policy allows this instrument.
//
// An empty list permits nothing. That is fail-closed rather than permissive,
// and it matches the schema: the column is NOT NULL, so an empty array is an
// explicit statement that nothing is permitted, not an absent statement.
func (p Policy) PermitsInstrument(id string) bool {
	return contains(p.PermittedInstruments, id)
}

// PermitsVenue reports whether the policy allows this venue.
func (p Policy) PermitsVenue(id string) bool {
	return contains(p.PermittedVenues, id)
}

// PermitsMarket reports whether the policy allows this market class.
func (p Policy) PermitsMarket(class string) bool {
	return contains(p.PermittedMarkets, class)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// DerivationBoundary records the inclusive/exclusive question that
// risk.policy does not answer with a column.
//
// The schema constrains every limit to > 0 and every drawdown to <= 1. An
// inclusive comparison ("measured <= limit") means the policy's own boundary
// value is a permitted value, which is the reading an operator expects when a
// limit is documented as a maximum. It is fixed here as a constant rather than
// made configurable, because a configurable boundary would be a second thing to
// keep in step with the schema and there is no column for it to be persisted in.
const DerivationBoundary = BoundaryInclusive

// Boundary states whether a limit's own value is permitted.
//
// Kept as a named type because it appears in finding text and in the boundary
// test, even though only DerivationBoundary is currently meaningful.
type Boundary string

const (
	BoundaryInclusive Boundary = "INCLUSIVE"
	BoundaryExclusive Boundary = "EXCLUSIVE"
)

// DerivedCurrency states the denomination limit comparisons happen in.
//
// risk.policy carries no per-limit currency column, and risk.account_state
// carries base_currency. Limits are therefore compared against measurements
// denominated in that account currency. This is a recorded gap rather than a
// solved problem: a policy scoped to one account in one currency is correct
// today, and a policy spanning accounts with different base currencies would
// need either a per-limit currency column or an explicit conversion source,
// neither of which exists.
const DerivedCurrency = "risk.account_state.base_currency"

// controlColumn maps each limit-requiring control to the policy columns it
// consults.
//
// The mapping is not one-to-one and the differences are informative:
//
//   - LOSS_AND_DRAWDOWN_CONTROL reads two columns, because a loss limit and a
//     drawdown limit are separate bounds with separate units.
//   - RATE_AND_THROTTLE_LIMIT reads three columns, because order rate, cancel
//     rate and the standing open-order count are throttled separately.
//   - The other five controls read one column each.
//
// The eight controls that take no numeric limit answer from the order, its
// environment or the halt table, and consult no policy column at all.
//
// Two of the twelve limit columns appear in no entry: max_order_quantity and
// max_price_deviation. Doc 04's gate enumerates fifteen controls and names
// neither a quantity cap nor a price-deviation cap, so there is no control here
// that could own them. Rather than fold a quantity cap into a control named for
// notional, or invent a sixteenth control that doc 04 does not have, the two are
// left unconsulted and TestColumnsWithNoDoc04Control asserts that they stay that
// way. That test is the point: it converts a silent gap between the schema and
// the specification into a recorded, reviewable one.
var controlColumn = map[Control][]string{
	CtrlNotionalLimit:      {"max_order_notional"},
	CtrlPositionLimit:      {"max_net_exposure"},
	CtrlExposureLimit:      {"max_gross_exposure"},
	CtrlConcentrationLimit: {"max_concentration"},
	CtrlLeverageLimit:      {"max_leverage"},
	CtrlLossLimit:          {"max_daily_loss", "max_drawdown"},
	CtrlRateLimit:          {"max_open_orders", "max_orders_per_minute", "max_cancels_per_minute"},
}

// ColumnFor returns the policy column names a control consults.
func ColumnFor(c Control) []string {
	out := controlColumn[c]
	if out == nil {
		return nil
	}
	cp := make([]string, len(out))
	copy(cp, out)
	return cp
}

// Value returns the decimal policy bound a control compares one of its columns
// against.
//
// The bool is false when the column is not one this control reads, which is how
// a caller iterating two columns for LOSS_AND_DRAWDOWN_CONTROL knows which
// measurement to compare against which bound.
func (p Policy) Value(c Control, column string) (contracts.Decimal, bool) {
	switch column {
	case "max_order_notional":
		return p.MaxOrderNotional, c == CtrlNotionalLimit
	case "max_gross_exposure":
		return p.MaxGrossExposure, c == CtrlExposureLimit
	case "max_net_exposure":
		return p.MaxNetExposure, c == CtrlPositionLimit
	case "max_leverage":
		return p.MaxLeverage, c == CtrlLeverageLimit
	case "max_concentration":
		return p.MaxConcentration, c == CtrlConcentrationLimit
	case "max_daily_loss":
		return p.MaxDailyLoss, c == CtrlLossLimit
	case "max_drawdown":
		return p.MaxDrawdown, c == CtrlLossLimit
	default:
		return contracts.Decimal{}, false
	}
}

// Count returns the integer policy bound for one of RATE_AND_THROTTLE_LIMIT's
// three columns.
//
// Counts are kept out of Value because a count is not money: routing them
// through contracts.Decimal would invite a caller to scale one as though it
// were.
func (p Policy) Count(c Control, column string) (int64, bool) {
	if c != CtrlRateLimit {
		return 0, false
	}
	switch column {
	case "max_open_orders":
		return p.MaxOpenOrders, true
	case "max_orders_per_minute":
		return p.MaxOrdersPerMinute, true
	case "max_cancels_per_minute":
		return p.MaxCancelsPerMinute, true
	default:
		return 0, false
	}
}

// Validate checks the invariants risk.policy enforces structurally, so a policy
// that the database would refuse is refused here first with a message naming the
// column.
//
// The database remains the authority -- these are the same conditions, written
// out so the service can report them rather than surfacing a CHECK violation as
// an opaque PostgreSQL error.
func (p Policy) Validate() error {
	if p.Revision == "" {
		return fmt.Errorf("risk: policy has no revision")
	}
	positives := []struct {
		name string
		v    contracts.Decimal
	}{
		{"max_order_notional", p.MaxOrderNotional},
		{"max_order_quantity", p.MaxOrderQuantity},
		{"max_gross_exposure", p.MaxGrossExposure},
		{"max_net_exposure", p.MaxNetExposure},
		{"max_leverage", p.MaxLeverage},
		{"max_concentration", p.MaxConcentration},
		{"max_daily_loss", p.MaxDailyLoss},
	}
	for _, f := range positives {
		if f.v.Sign() <= 0 {
			return fmt.Errorf("risk: %s must be greater than zero (risk.policy policy_limits_positive)", f.name)
		}
	}
	if p.MaxDrawdown.Sign() <= 0 || p.MaxDrawdown.Cmp(contracts.MustParseDecimal("1")) > 0 {
		return fmt.Errorf("risk: max_drawdown must be in (0, 1] (risk.policy policy_limits_positive)")
	}
	if p.MaxPriceDeviation.Sign() < 0 {
		return fmt.Errorf("risk: max_price_deviation must be in [0, 1] (risk.policy policy_deviation_bounded)")
	}
	counts := []struct {
		name string
		v    int64
	}{
		{"max_open_orders", p.MaxOpenOrders},
		{"max_orders_per_minute", p.MaxOrdersPerMinute},
		{"max_cancels_per_minute", p.MaxCancelsPerMinute},
	}
	for _, f := range counts {
		if f.v <= 0 {
			return fmt.Errorf("risk: %s must be greater than zero (risk.policy policy_limits_positive)", f.name)
		}
	}
	if p.LossWindow <= 0 {
		return fmt.Errorf("risk: loss_window must be positive (risk.policy policy_loss_window_positive)")
	}
	if p.LossSource == "" {
		return fmt.Errorf("risk: loss_source is NOT NULL in risk.policy and is the loss measurement's source of truth")
	}
	if p.MaxMarketDataAge <= 0 {
		return fmt.Errorf("risk: max_market_data_age must be positive (risk.policy policy_market_data_age_positive)")
	}
	return nil
}

// Active reports whether the policy is in force.
//
// Mirrors active_policy_complete: an ACTIVE row must be approved, effective and
// carry a rollback revision. The caller is responsible for having loaded a row
// satisfying that; this reports what was loaded.
func (p Policy) Active() bool { return p.Status == "ACTIVE" }

// Finding is one control's outcome.
type Finding struct {
	Control  Control
	Severity Severity
	Passed   bool
	// Reason is always populated, including on pass. A finding with an empty
	// reason cannot be reviewed after an incident.
	Reason string
}

// Decision is the outcome of one risk evaluation.
type Decision struct {
	Approved bool

	// PolicyRevision is the exact policy the decision was made against. Doc 17
	// §4 requires the result to carry it, and an approval without it is not
	// reproducible.
	PolicyRevision string

	// Findings lists every control evaluated, pass or fail, in doc 04's order.
	// Doc 17 §4 requires "evaluated facts" and "failed controls"; recording the
	// passes too is what lets a reviewer confirm a control was actually
	// consulted rather than skipped.
	Findings []Finding

	// CorrelationID ties the decision to the command and its audit trail.
	CorrelationID string

	EvaluatedAt time.Time
}

// Failed returns just the failed mandatory findings, in doc 04's order.
func (d Decision) Failed() []Finding {
	var out []Finding
	for _, f := range d.Findings {
		if !f.Passed && f.Severity == Mandatory {
			out = append(out, f)
		}
	}
	return out
}

// Summary renders the decision as one operator-readable line.
func (d Decision) Summary() string {
	if d.Approved {
		return fmt.Sprintf("APPROVED against policy %s", d.PolicyRevision)
	}
	parts := make([]string, 0, len(d.Findings))
	for _, f := range d.Failed() {
		parts = append(parts, fmt.Sprintf("%s: %s", f.Control, f.Reason))
	}
	return fmt.Sprintf("REJECTED against policy %s -- %s", d.PolicyRevision, strings.Join(parts, "; "))
}
