package risk

import (
	"fmt"
	"strings"
	"time"

	"github.com/aitc/trade/contracts"
)

// OrderFacts is the order under evaluation, plus every external fact a control
// may consult.
//
// The shape is a flat struct rather than a chain of interfaces on purpose. Every
// control reads from one place, so a reviewer can see the complete input surface
// of the gate on a single screen, and no control can reach a fact that evaluation
// did not provide. A gate whose inputs are discovered through dependency
// injection is a gate whose inputs are not knowable without running it.
//
// Every external fact has the same polarity: true means the check PASSED. An
// earlier version named one field DuplicateOrder, where true meant the order WAS
// a duplicate -- the only field in the struct with inverted polarity, and
// therefore a field a caller could plausibly get backwards. Every other control
// denied on false and that one denied on true, so a caller who assumed uniformity
// passed every order through the duplicate check. It is now NoDuplicateOrder, and
// the name says which way the truth value points.
type OrderFacts struct {
	// Identity and scope.
	Environment  string
	AccountID    string
	StrategyID   string
	InstrumentID string
	MarketClass  string
	VenueID      string
	OrderType    string

	// Side is the direction of the order, and it is not derivable from anything
	// else in this struct.
	//
	// It was absent while INSTRUMENT_ELIGIBILITY existed as a caller-asserted
	// boolean, because nothing that read an instrument needed to know which way
	// the order went. Sourcing the fact removed that comfort: a SELL is short
	// exposure unless it is a closing action, so an instrument that cannot be
	// shorted can only refuse a SELL if the side is known. An absent or invalid
	// side is treated as a refusal rather than as a buy, because the one thing
	// this struct must never do is decide a side on the caller's behalf.
	Side string

	// Order terms as decimal strings, so that nothing on this path is ever an
	// IEEE-754 value. They are parsed with contracts.ParseDecimal.
	Amount string
	Price  string

	// RiskReducing is derived by the caller from contracts.OrderType
	// .RiskIncreasing() and carried here, because this package takes facts rather
	// than order terms and cannot classify an order it was not given.
	//
	// The classification has exactly one definition. It previously lived in
	// package oms as well, in a copy that enumerated six of common.order_type's
	// nine members and that nothing outside its own test consumed, while
	// domain/gate -- the actual risk boundary -- called the contracts
	// definition. That copy has been deleted; this comment points at the one that
	// decides, so the next reader does not add a fourth.
	RiskReducing bool

	// Facts consulted by the controls that answer from the environment rather
	// than from a limit. A nil pointer means "not established", which every
	// control treats as a failure. There is no "assume true" default anywhere in
	// this struct.
	AccountAuthorized  *bool
	StrategyDeployed   *bool
	InstrumentEligible *bool
	VenueAvailable     *bool
	PrecisionValid     *bool
	MarketDataFresh    *bool
	NoDuplicateOrder   *bool

	// Measurements compared against the policy columns named by ColumnFor.
	// A nil pointer means the figure could not be measured, which is also a
	// failure -- doc 17 §2 makes unavailable account state a reject condition.
	Notional      *contracts.Decimal
	Position      *contracts.Decimal
	Exposure      *contracts.Decimal
	Concentration *contracts.Decimal
	Leverage      *contracts.Decimal
	DailyLoss     *contracts.Decimal
	Drawdown      *contracts.Decimal

	// Currency is the denomination the measurements are in. risk.policy has no
	// per-limit currency column, so this must equal the account's
	// base_currency; see DerivedCurrency for why that is a recorded gap rather
	// than a solution.
	Currency string

	// Counts compared against the three INTEGER policy columns. These are
	// counts rather than decimals because risk.policy stores them as INTEGER,
	// and a count is not money.
	//
	// RATE_AND_THROTTLE_LIMIT reads these directly rather than accepting a
	// pre-computed WithinRateLimit boolean. An earlier version took that
	// boolean, which left max_open_orders, max_orders_per_minute and
	// max_cancels_per_minute as columns the engine never consulted -- a limit an
	// operator could set, believe was enforced, and find unreviewed in any
	// finding. Comparing the counts here is what makes those three columns real.
	//
	// The per-minute window is not a separate field because the column names
	// already state it: max_orders_per_minute and max_cancels_per_minute are
	// per-minute by name, and the engine takes the caller's counters over
	// whatever window the caller measured. max_open_orders is not rate-limited
	// at all and is compared against the standing open-order count.
	OpenOrderCount    int64
	OrdersLastMinute  int64
	CancelsLastMinute int64

	// Provenance is where the measurements above came from. The zero value
	// denies every limit control; see Provenance for why an unsourced figure
	// must not be compared against a limit.
	Provenance Provenance
}

// measurement returns the decimal figure a limit column is compared against.
func (f OrderFacts) measurement(column string) *contracts.Decimal {
	switch column {
	case "max_order_notional":
		return f.Notional
	case "max_gross_exposure":
		return f.Exposure
	case "max_net_exposure":
		return f.Position
	case "max_concentration":
		return f.Concentration
	case "max_leverage":
		return f.Leverage
	case "max_daily_loss":
		return f.DailyLoss
	case "max_drawdown":
		return f.Drawdown
	default:
		return nil
	}
}

// count returns the figure an INTEGER policy column is compared against.
func (f OrderFacts) count(column string) int64 {
	switch column {
	case "max_open_orders":
		return f.OpenOrderCount
	case "max_orders_per_minute":
		return f.OrdersLastMinute
	case "max_cancels_per_minute":
		return f.CancelsLastMinute
	default:
		return 0
	}
}

func (f OrderFacts) flag(c Control) *bool {
	switch c {
	case CtrlAccountAuthorized:
		return f.AccountAuthorized
	case CtrlStrategyDeployed:
		return f.StrategyDeployed
	case CtrlInstrumentEligible:
		return f.InstrumentEligible
	case CtrlVenueAvailable:
		return f.VenueAvailable
	case CtrlPricePrecision:
		return f.PrecisionValid
	case CtrlMarketDataFreshness:
		return f.MarketDataFresh
	case CtrlDuplicateOrder:
		return f.NoDuplicateOrder
	default:
		return nil
	}
}

// Provenance records where an order's measurements came from.
//
// This type exists because of a hole found on 2026-09-30. The gate takes every
// limit input as a value: leverage, open-order count, daily loss, drawdown,
// net and gross exposure, concentration. Every one of those is stored, in a
// table, in a column: risk.account_state.leverage, .open_order_count, .daily_pnl,
// .equity and .peak_equity, and portfolio.position. A caller could therefore
// hand the gate Leverage = 1.5 for an account the database records at 50x, and
// the gate would approve against a policy whose max_leverage is 2.
//
// A limit gate whose inputs are supplied by the caller is a gate that measures
// whatever the caller says. Provenance makes the source part of the input, and
// a measurement with no source denies rather than passing unexamined.
//
// # What this does and does not guarantee
//
// Provenance is a claim, not an attestation. A caller can construct a
// Provenance by hand, exactly as a test does. The control it buys is that
// "no source" is a refusal rather than a silent pass, and that the production
// path has exactly one way to make the claim honestly: domain/riskstate, which
// reads the rows and stamps what it read. An audit trail is what distinguishes
// an honest stamp from a forged one, and recording that the gate was invoked
// with sourced facts is part of the G3 evidence work, not part of this type.
type Provenance struct {
	// AccountStateAsOfNs is risk.account_state.as_of_ns for the row consulted.
	// Zero means no account state was read.
	AccountStateAsOfNs int64

	// PositionRowCount is how many portfolio.position rows were aggregated. A
	// negative value is refused by Established; zero is legitimate for an
	// account holding no positions, and is not the same as "not read".
	PositionRowCount int

	// PolicyRevision is the risk.policy the measurements were taken against.
	PolicyRevision string

	// LoadedAt is when the facts were read, used to detect a fact set assembled
	// from rows read at very different times.
	LoadedAt time.Time
}

// Established reports whether the measurements carry a source.
//
// The four conditions are all required. A zero as-of means nothing was read; a
// negative position count means the aggregate was not computed; an empty policy
// revision means the numbers were not taken against a policy; and a zero load
// time means the read itself was never recorded.
func (p Provenance) Established() bool {
	return p.AccountStateAsOfNs > 0 &&
		p.PositionRowCount >= 0 &&
		p.PolicyRevision != "" &&
		!p.LoadedAt.IsZero()
}

// Describe renders the provenance for a refusal message, naming what is missing
// rather than reporting a boolean.
func (p Provenance) Describe() string {
	var missing []string
	if p.AccountStateAsOfNs <= 0 {
		missing = append(missing, "no risk.account_state row was read")
	}
	if p.PositionRowCount < 0 {
		missing = append(missing, "portfolio.position was not aggregated")
	}
	if p.PolicyRevision == "" {
		missing = append(missing, "no policy revision was recorded")
	}
	if p.LoadedAt.IsZero() {
		missing = append(missing, "the read time was not recorded")
	}
	if len(missing) == 0 {
		return "measurements are sourced"
	}
	return "measurements are unsourced: " + strings.Join(missing, "; ")
}

// HaltDecision is the halt evaluation result folded into the risk gate.
//
// The risk package does not resolve halts itself: package halt owns that, and the
// two must not each hold a copy of the precedence rule. This type carries the
// already-made decision across the boundary.
type HaltDecision struct {
	// Evaluated reports whether the halt gate was consulted at all.
	//
	// A risk evaluation that never consulted the halt gate has not evaluated
	// the gate. This flag makes that distinguishable from "consulted, nothing in
	// force", which is the difference between a correct approval and an
	// unexamined one.
	Evaluated bool
	Blocked   bool
	Reason    string
}

// Request is one complete risk evaluation.
type Request struct {
	Order OrderFacts
	// Policy is the loaded risk.policy row. The caller must load it; this
	// package does not read the database, and an absent policy is not a policy
	// with permissive limits.
	Policy Policy
	// Halt is the already-resolved halt decision, produced by package halt.
	Halt          HaltDecision
	CorrelationID string
	EvaluatedAt   time.Time
}

// Evaluate runs every control in doc 04's gate and returns the decision.
//
// Every control is evaluated even after one fails, and every outcome is
// recorded. Two reasons, both load-bearing:
//
//   - Doc 17 §4 requires the result to contain "evaluated facts" and "failed
//     controls". A short-circuiting engine could not produce that.
//   - An operator who receives one failure and fixes it, only to be told about
//     the next one, learns less from each cycle than from the complete list.
func Evaluate(req Request) Decision {
	d := Decision{
		PolicyRevision: req.Policy.Revision,
		CorrelationID:  req.CorrelationID,
		EvaluatedAt:    req.EvaluatedAt,
		Approved:       true,
	}

	// A policy that is ACTIVE but structurally invalid must not be able to
	// approve anything. The check is done once here rather than per control so
	// that a single malformed column produces a single named reason, and
	// Policy.Validate is the same set of conditions the schema enforces.
	var policyErr error
	if req.Policy.Active() {
		policyErr = req.Policy.Validate()
	}

	for _, c := range Controls() {
		var f Finding
		switch {
		case c == CtrlInstrumentEligible || c == CtrlVenueAvailable:
			f = evaluateEligibility(req, c, policyErr)
		case c.RequiresLimit() && c == CtrlRateLimit:
			f = evaluateRate(req, policyErr)
		case c.RequiresLimit():
			f = evaluateLimit(req, c, policyErr)
		default:
			f = evaluateNonLimit(req, c)
		}
		if !f.Passed && f.Severity == Mandatory {
			d.Approved = false
		}
		d.Findings = append(d.Findings, f)
	}

	return d
}

// evaluateEligibility is evaluateNonLimit for the two controls that also carry a
// policy allowlist.
//
// The external fact and the policy are independent sources and both must hold.
// A venue being up says nothing about whether this policy may trade it, and a
// policy permitting a venue says nothing about whether that venue is reachable.
// The controls answer "may and can"; they are not substitutes for one another.
func evaluateEligibility(req Request, c Control, policyErr error) Finding {
	// The allowlist is a configured restriction like a limit, so a risk-reducing
	// action is not measured against it. Refusing a reduction because no
	// allowlist was configured is the failure mode this package must not have.
	//
	// The exemption covers the policy half only. The external fact is still
	// required: a risk-reducing action against an unreachable venue or an
	// ineligible instrument is not an order this package can bless. The reason
	// below may only claim the external fact is satisfied once it has been.
	if req.Order.RiskReducing {
		f := evaluateNonLimit(req, c)
		if !f.Passed {
			return f
		}
		return Finding{
			Control: c, Severity: Mandatory, Passed: true,
			Reason: fmt.Sprintf(
				"%s: the external fact is satisfied; the policy allowlist was not applied because "+
					"this is a risk-reducing action", c),
		}
	}

	// The external fact first: it is the cheaper of the two and does not depend
	// on any configuration.
	f := evaluateNonLimit(req, c)
	if !f.Passed {
		return f
	}

	if !req.Policy.Active() {
		f.Passed = false
		f.Reason = fmt.Sprintf(
			"%s is satisfied by the environment, but no ACTIVE policy states which venues or "+
				"instruments are permitted, so the policy half of the control is unestablished", c)
		return f
	}
	if policyErr != nil {
		f.Passed = false
		f.Reason = fmt.Sprintf("%s is satisfied by the environment, but the policy is invalid: %v",
			c, policyErr)
		return f
	}

	column, subject, permitted := c, "", false
	switch c {
	case CtrlInstrumentEligible:
		column, subject, permitted = "permitted_instruments", req.Order.InstrumentID,
			req.Policy.PermitsInstrument(req.Order.InstrumentID)
	case CtrlVenueAvailable:
		column, subject, permitted = "permitted_venues", req.Order.VenueID,
			req.Policy.PermitsVenue(req.Order.VenueID)
	}

	if !permitted {
		f.Passed = false
		f.Reason = fmt.Sprintf("%s is reachable, but policy %s does not list %s in %s",
			c, req.Policy.Revision, subject, column)
		return f
	}

	f.Reason = fmt.Sprintf("%s is satisfied and permitted by policy %s %s",
		c, req.Policy.Revision, column)
	return f
}

func evaluateNonLimit(req Request, c Control) Finding {
	if c == CtrlHaltState {
		return evaluateHalt(req)
	}
	p := req.Order.flag(c)
	if p == nil {
		return Finding{
			Control: c, Severity: Mandatory, Passed: false,
			Reason: fmt.Sprintf("%s was not evaluated; an unestablished fact is not permission", c),
		}
	}
	if !*p {
		return Finding{
			Control: c, Severity: Mandatory, Passed: false,
			Reason: fmt.Sprintf("%s is not satisfied", c),
		}
	}
	return Finding{
		Control: c, Severity: Mandatory, Passed: true,
		Reason: fmt.Sprintf("%s is satisfied", c),
	}
}

// notApplicable reports the finding for a limit control that a risk-reducing
// action is not measured against.
//
// This must be consulted BEFORE the policy state, and not after it. A
// risk-reducing action during a market incident frequently arrives when no
// policy is loaded, and refusing to reduce is the failure mode that turns a
// market problem into an unmanaged loss. An earlier version checked policy state
// first and denied the reduction for want of a policy, which is backwards.
func notApplicable(c Control) Finding {
	return Finding{
		Control: c, Severity: Mandatory, Passed: true,
		Reason: fmt.Sprintf(
			"%s not applied: risk-reducing action, which doc 17 §4 routes through a "+
				"separate audited safe-reduction policy that is not yet defined", c),
	}
}

// evaluateLimit compares the order's measurements against the policy columns the
// control reads.
//
// Four failure modes are reported separately, because they have different
// remedies and an operator who cannot tell them apart will guess:
//
//	risk-reducing -> the cap is skipped, and says so rather than claiming a pass
//	policy absent  -> an owner must approve and activate one
//	policy invalid -> a column is malformed; the schema would refuse the row
//	not measurable -> the system of record is unavailable
//	over the limit -> the order itself is too large
//
// All of them but the first refuse the order. None substitutes a default.
func evaluateLimit(req Request, c Control, policyErr error) Finding {
	fail := func(reason string) Finding {
		return Finding{Control: c, Severity: Mandatory, Passed: false, Reason: reason}
	}

	if req.Order.RiskReducing {
		return notApplicable(c)
	}

	if err := policyState(req.Policy); err != "" {
		return fail(err)
	}

	// The source check sits between the policy check and the comparison, and
	// that position is deliberate. With no ACTIVE policy nothing is compared at
	// all, so the order carries no safety consequence and the operator gets the
	// more actionable message: "no policy is in force" rather than "your
	// numbers have no source". Once a policy IS in force, an unsourced
	// measurement must never reach a comparison, because comparing one produces
	// a number that looks like a measurement and is not -- Leverage 1.5 asserted
	// by a caller for an account the database records at 50x passes a
	// max_leverage of 2, and the finding reports that the leverage limit was
	// checked.
	if !req.Order.Provenance.Established() {
		return fail(c.String() + " cannot be evaluated: " + req.Order.Provenance.Describe() +
			"; the figure would be whatever the caller asserted, and an asserted figure is not a measurement")
	}
	if policyErr != nil {
		return fail(policyErr.Error())
	}

	columns := ColumnFor(c)
	if len(columns) == 0 {
		return fail(fmt.Sprintf("%s declares no policy column; the control and the schema disagree", c))
	}

	for _, column := range columns {
		bound, ok := req.Policy.Value(c, column)
		if !ok {
			return fail(fmt.Sprintf("%s: %s is not a column this control reads", c, column))
		}

		measured := req.Order.measurement(column)
		if measured == nil {
			return fail(fmt.Sprintf(
				"%s: %s could not be measured; missing data is a deny condition "+
					"(17_CONFIGURATION_AND_RISK_POLICY.md §2)", c, column))
		}

		if measured.Cmp(bound) > 0 {
			return fail(fmt.Sprintf(
				"%s: measured %s against %s = %s, %s bound is exceeded",
				c, measured.String(), column, bound.String(), DerivationBoundary))
		}
	}

	return Finding{
		Control: c, Severity: Mandatory, Passed: true,
		Reason: fmt.Sprintf("%s: within %v (limit %s, boundary %s, missing data denies)",
			c, columns, req.Policy.Revision, DerivationBoundary),
	}
}

// evaluateRate is evaluateLimit for the three INTEGER columns.
//
// It is separate because counts cannot be compared through contracts.Decimal, and
// because an integer bound admits a count above it while a decimal bound admits
// a measurement above it by an arbitrarily small amount -- the same comparison
// discipline applies but the units do not.
func evaluateRate(req Request, policyErr error) Finding {
	c := CtrlRateLimit
	fail := func(reason string) Finding {
		return Finding{Control: c, Severity: Mandatory, Passed: false, Reason: reason}
	}

	if req.Order.RiskReducing {
		return notApplicable(c)
	}
	if err := policyState(req.Policy); err != "" {
		return fail(err)
	}
	if !req.Order.Provenance.Established() {
		return fail(c.String() + " cannot be evaluated: " + req.Order.Provenance.Describe() +
			"; the figure would be whatever the caller asserted, and an asserted figure is not a measurement")
	}
	if policyErr != nil {
		return fail(policyErr.Error())
	}

	for _, column := range ColumnFor(c) {
		bound, ok := req.Policy.Count(c, column)
		if !ok {
			return fail(fmt.Sprintf("%s: %s is not a column this control reads", c, column))
		}
		measured := req.Order.count(column)
		if measured > bound {
			return fail(fmt.Sprintf(
				"%s: %s is %d against %s = %d, %s bound is exceeded",
				c, column, measured, column, bound, DerivationBoundary))
		}
	}

	return Finding{
		Control: c, Severity: Mandatory, Passed: true,
		Reason: fmt.Sprintf("%s: within %v (limit %s, boundary %s, missing data denies)",
			c, ColumnFor(c), req.Policy.Revision, DerivationBoundary),
	}
}

// policyState explains why no limit is in force, or returns "" when one is.
//
// The two reasons are kept apart because their remedies differ: an operator who
// has no policy at all must have one drafted and approved, while an operator with
// a DRAFT policy is one approval away from a gate that works.
func policyState(p Policy) string {
	if p.Active() {
		return ""
	}
	if p.Revision == "" {
		return "no risk policy was loaded for this order; " +
			"an unestablished policy is not a permissive one " +
			"(17_CONFIGURATION_AND_RISK_POLICY.md §2)"
	}
	return fmt.Sprintf(
		"policy %s is %s, not ACTIVE; the platform supplies no default limits "+
			"(17_CONFIGURATION_AND_RISK_POLICY.md §3)", p.Revision, p.Status)
}

func evaluateHalt(req Request) Finding {
	if !req.Halt.Evaluated {
		return Finding{
			Control: CtrlHaltState, Severity: Mandatory, Passed: false,
			Reason: "the halt gate was not consulted; an unexamined halt state is not a clear one",
		}
	}
	if req.Halt.Blocked {
		return Finding{
			Control: CtrlHaltState, Severity: Mandatory, Passed: false,
			Reason: req.Halt.Reason,
		}
	}
	return Finding{
		Control: CtrlHaltState, Severity: Mandatory, Passed: true,
		Reason: "no active halt governs this scope" + optional(req.Halt.Reason),
	}
}

func optional(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}
