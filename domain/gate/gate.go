// Package gate assembles a complete risk decision from persisted state.
//
// It is the sanctioned entry point for deciding whether an order may proceed.
// Before it existed, assembling a risk.Request was test code: the policy came
// from riskpolicy, the facts from riskstate, and the halt decision from a
// literal, so nothing in the repository could produce a decision the way
// production would.
//
// # Why the assembly has to live somewhere
//
// risk.Evaluate is pure by design -- it reads no tables, which is what makes it
// exhaustively testable. The cost of that is that three of its inputs are facts
// about rows, and a pure function cannot insist on where they came from. A
// caller can hand it a permissive policy, a set of measurements it made up, and
// a halt decision stating that the halt table was consulted and found clear.
//
// Every one of those three was a real gap, in this order:
//
//   - The halt decision was a caller assertion, on the one control doc 25 makes
//     unwaivable. An asserted decision cannot distinguish "consulted, none
//     apply" from "never looked".
//   - Instrument eligibility and precision validity were caller booleans.
//   - Account, position and strategy facts were caller-supplied numbers.
//
// The first two are closed; the third was closed by riskstate. What is left is
// this package's job: make the sourcing the only way to get a decision, so that
// the next caller cannot reintroduce an assertion by accident.
//
// # What this package does not do
//
// It does not write. It does not persist a risk.decision, create an oms.order,
// append an audit record or talk to a venue. An approved decision here is a
// statement that the order is permitted, not a statement that it was placed, and
// the gap between those two is where OMS orchestration -- the next piece of G3 --
// will live.
package gate

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/domain/halt"
	"github.com/aitc/trade/domain/risk"
	"github.com/aitc/trade/domain/riskpolicy"
	"github.com/aitc/trade/domain/riskstate"
)

// Submission is one proposed order, described by its own terms.
//
// It carries no measurements. Every figure the gate compares against a limit is
// read from the database by riskstate, and every limit comes from a resolved
// risk.policy row, so a caller can state what it wants to trade and nothing about
// whether it is allowed to.
type Submission struct {
	Environment  string
	AccountID    string
	StrategyID   string
	InstrumentID string
	MarketClass  string
	VenueID      string
	OrderType    string
	Side         string

	// Quantity and Price are decimal strings, never float64. Nothing on this
	// path may hold an IEEE-754 value.
	Quantity string
	Price    string
	Currency string

	// TimeInForce is validated against contracts' closed set by NewOrderFacts.
	TimeInForce string

	CorrelationID string

	// MaxAccountAge bounds the staleness of risk.account_state and is required.
	// It has no default because a default here is a default staleness tolerance,
	// which is a limit nobody chose.
	MaxAccountAge time.Duration
}

// Decision is a risk decision plus the facts it was made from.
//
// The facts are returned rather than discarded because a caller that persists a
// risk.decision row has to store what was evaluated, and a caller that cannot
// reproduce the decision from its inputs cannot answer the question an operator
// will ask during an incident.
type Decision struct {
	risk.Decision

	// Facts is the order as the gate saw it, with every measurement sourced.
	Facts risk.OrderFacts

	// Policy is the resolved policy the decision was made against.
	Policy risk.Policy

	// Halt is the halt evaluation, including the reason when nothing applied.
	Halt halt.Decision
}

// NewOrderFacts converts a Submission into the order description riskstate.Load
// takes.
//
// It is exported because the OMS needs to describe the same order when it writes
// the row, and building it twice is how the written order and the decided order
// drift apart.
//
// The identity fields are validated here rather than at each use. An empty
// environment or account produces a policy scope that resolves to nothing, and
// "no policy" is a rejection with a reason -- but a reason about a missing
// environment is much harder to act on than one that names the missing field.
func NewOrderFacts(in Submission) (risk.OrderFacts, error) {
	missing := ""
	switch {
	case in.Environment == "":
		missing = "environment"
	case in.AccountID == "":
		missing = "account_id"
	case in.InstrumentID == "":
		missing = "instrument_id"
	case in.VenueID == "":
		missing = "venue_id"
	case in.OrderType == "":
		missing = "order_type"
	case in.Quantity == "":
		missing = "quantity"
	}
	if missing != "" {
		return risk.OrderFacts{}, fmt.Errorf("gate: a submission carries no %s", missing)
	}
	if !contracts.OrderType(in.OrderType).Valid() {
		return risk.OrderFacts{}, fmt.Errorf(
			"gate: order_type %q is not one of contracts' closed set", in.OrderType)
	}
	if !contracts.Side(in.Side).Valid() {
		return risk.OrderFacts{}, fmt.Errorf("gate: side %q is neither BUY nor SELL", in.Side)
	}
	if _, err := contracts.ParseDecimal(in.Quantity); err != nil {
		return risk.OrderFacts{}, fmt.Errorf("gate: quantity %q is not a decimal: %w", in.Quantity, err)
	}
	if in.Price != "" {
		if _, err := contracts.ParseDecimal(in.Price); err != nil {
			return risk.OrderFacts{}, fmt.Errorf("gate: price %q is not a decimal: %w", in.Price, err)
		}
	}
	if in.TimeInForce != "" && !contracts.TimeInForce(in.TimeInForce).Valid() {
		return risk.OrderFacts{}, fmt.Errorf("gate: time_in_force %q is not one of contracts' closed set", in.TimeInForce)
	}

	return risk.OrderFacts{
		Environment:  in.Environment,
		AccountID:    in.AccountID,
		StrategyID:   in.StrategyID,
		InstrumentID: in.InstrumentID,
		MarketClass:  in.MarketClass,
		VenueID:      in.VenueID,
		OrderType:    in.OrderType,
		Side:         in.Side,
		Amount:       in.Quantity,
		Price:        in.Price,
		Currency:     in.Currency,
		// RiskReducing comes from the OMS's own classification rather than a
		// caller flag. Duplicating the definition here would be the same drift
		// domain/oms had to be corrected for once already: two places deciding
		// which orders are risk-reducing, disagreeing quietly on the third.
		RiskReducing: !contracts.OrderType(in.OrderType).RiskIncreasing(),
	}, nil
}

// Sources says where a decision's inputs are read from, and it is two handles
// rather than one because the two kinds of input have different visibility rules.
//
// Committed must be a *sql.DB. The policy and the halts are read through it,
// because neither is real until it is committed: a policy inside an open
// transaction is one that vanishes on rollback, and a halt inside an open
// transaction is not a halt. Reading either from a transaction would let a caller
// approve an order against a policy that will not exist, or trade through a halt
// that will not be there once the transaction ends.
//
// State may be a transaction. Account, position, strategy and instrument rows are
// read from it, and the OMS will want them read inside the transaction that is
// about to write the order, so that the decision and the write see one snapshot.
// That is also what makes this package testable against a fixture whose
// immutable rows cannot be deleted: a rolled-back transaction is the only way to
// satisfy a schema that refuses cleanup.
//
// State is riskstate's own interface rather than a local one. Two interfaces with
// the same methods are two things to keep in step, and the only method this
// package does not itself call is the one riskstate needs for its aggregates.
type Sources struct {
	Committed *sql.DB
	State     riskstate.Querier
}

// Evaluate resolves the policy, sources the facts, consults the halts and runs
// the gate.
//
// The order of the three steps is the order of authority. A halt is checked
// before any limit, because a limit is a quantitative bound an operator chose
// and a halt is a statement that trading should not be happening at all; a
// system halted for an audit-chain break must not be told it breached a
// concentration limit. Limits are then evaluated against state read at that
// moment, and the halt is evaluated again afterwards, because the interval
// between reading the halts and reading the balances is a window in which a halt
// could have been raised.
func Evaluate(ctx context.Context, src Sources, in Submission) (Decision, error) {
	if src.Committed == nil {
		return Decision{}, fmt.Errorf("gate: no committed handle; the policy and the halts are committed state")
	}
	if src.State == nil {
		return Decision{}, fmt.Errorf("gate: no state handle; the account, position and strategy rows have to come from somewhere")
	}

	order, err := NewOrderFacts(in)
	if err != nil {
		return Decision{}, err
	}

	scope := riskpolicy.OrderScope{
		Environment:  in.Environment,
		AccountID:    in.AccountID,
		StrategyID:   in.StrategyID,
		InstrumentID: in.InstrumentID,
		VenueID:      in.VenueID,
		MarketClass:  in.MarketClass,
	}

	policy, err := riskpolicy.New(src.Committed).ResolveForOrder(ctx, scope)
	if err != nil {
		return Decision{}, fmt.Errorf("gate: resolving the policy: %w", err)
	}

	action := halt.Scope{
		Environment:  in.Environment,
		AccountID:    in.AccountID,
		InstrumentID: in.InstrumentID,
		MarketClass:  in.MarketClass,
		VenueID:      in.VenueID,
		StrategyID:   in.StrategyID,
	}
	riskReducing := !contracts.OrderType(in.OrderType).RiskIncreasing()

	haltDecision, err := halt.EvaluateAgainst(ctx, src.Committed, action, riskReducing)
	if err != nil {
		return Decision{}, fmt.Errorf("gate: consulting the halts: %w", err)
	}
	if haltDecision.Blocked {
		// Returned as a decision rather than an error. A blocked order is a
		// correctly produced refusal, and it has to be recordable as one: a
		// risk.decision row for a rejected command is exactly what doc 17 §4
		// requires, and an error here would leave no row to record.
		return Decision{
			Decision: risk.Decision{
				Approved: false,
				// The revision is carried even on this path so a refusal is as
				// reproducible as an approval.
				PolicyRevision: policy.Revision,
				Findings: []risk.Finding{{
					Control:  risk.CtrlHaltState,
					Severity: risk.Mandatory,
					Passed:   false,
					Reason:   haltDecision.Reason,
				}},
				CorrelationID: in.CorrelationID,
				EvaluatedAt:   time.Now(),
			},
			Facts:  order,
			Policy: policy,
			Halt:   haltDecision,
		}, nil
	}

	facts, err := riskstate.Load(ctx, src.State, riskstate.Facts{
		Order:          order,
		PolicyRevision: policy.Revision,
		MaxAccountAge:  in.MaxAccountAge,
	})
	if err != nil {
		return Decision{}, fmt.Errorf("gate: sourcing the account, position and strategy state: %w", err)
	}

	decision := risk.Evaluate(risk.Request{
		Order:         facts,
		Policy:        policy,
		Halt:          risk.HaltDecision{Evaluated: true, Blocked: false, Reason: haltDecision.Reason},
		CorrelationID: in.CorrelationID,
		EvaluatedAt:   time.Now(),
	})

	// The re-check. If a halt was raised while the balances were being read, the
	// limits do not matter: the answer is no. This is a second read rather than a
	// transaction-level snapshot because the honest question is whether a halt is
	// in force now, not whether one was in force when the first read happened.
	recheck, err := halt.EvaluateAgainst(ctx, src.Committed, action, riskReducing)
	if err != nil {
		return Decision{}, fmt.Errorf("gate: re-checking the halts: %w", err)
	}
	if recheck.Blocked {
		decision = risk.Decision{
			Approved:       false,
			PolicyRevision: policy.Revision,
			Findings: []risk.Finding{{
				Control:  risk.CtrlHaltState,
				Severity: risk.Mandatory,
				Passed:   false,
				Reason:   "raised while this decision was being made: " + recheck.Reason,
			}},
			CorrelationID: in.CorrelationID,
			EvaluatedAt:   time.Now(),
		}
		return Decision{Decision: decision, Facts: facts, Policy: policy, Halt: recheck}, nil
	}

	return Decision{Decision: decision, Facts: facts, Policy: policy, Halt: haltDecision}, nil
}
