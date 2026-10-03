// Package strategy holds the strategy lifecycle as pure domain logic.
//
// The relationship to SQL mirrors package oms: this package is authoritative
// (doc 01 §7, the Strategy module owns strategy lifecycle), the trigger in
// migration 0008 is the backstop at the storage boundary, and
// TestTransitionRulesMatchTheDatabase pins the two closed sets together.
//
// What is different from the OMS machine is that strategy transitions are not
// merely permitted or refused -- they are permitted only to particular actors,
// and some of them require two of them. That is the whole content of the
// governance model, so it is modelled as data rather than buried in a switch.
package strategy

import (
	"fmt"
	"sort"
)

// State is a state in the closed strategy lifecycle.
//
// Doc 04: DRAFT -> REVIEW -> BACKTESTED -> SIMULATION -> PAPER -> SHADOW ->
// APPROVED -> DEPLOYED -> PAUSED -> RETIRED. The names come from the
// strategy.strategy_state enum in migration 0003.
type State string

const (
	StateDraft      State = "DRAFT"
	StateReview     State = "REVIEW"
	StateBacktested State = "BACKTESTED"
	StateSimulation State = "SIMULATION"
	StatePaper      State = "PAPER"
	StateShadow     State = "SHADOW"
	StateApproved   State = "APPROVED"
	StateDeployed   State = "DEPLOYED"
	StatePaused     State = "PAUSED"
	StateRetired    State = "RETIRED"
)

var states = []State{
	StateDraft, StateReview, StateBacktested, StateSimulation, StatePaper,
	StateShadow, StateApproved, StateDeployed, StatePaused, StateRetired,
}

var stateSet = func() map[State]struct{} {
	m := make(map[State]struct{}, len(states))
	for _, s := range states {
		m[s] = struct{}{}
	}
	return m
}()

// States returns every lifecycle state in declaration order. The result is a copy.
func States() []State {
	out := make([]State, len(states))
	copy(out, states)
	return out
}

// Known reports whether s is in the closed lifecycle set.
func (s State) Known() bool {
	_, ok := stateSet[s]
	return ok
}

// Terminal reports whether s admits no further transition.
func (s State) Terminal() bool { return s == StateRetired }

// ActorRole is the authority required to perform a transition.
//
// These mirror the strategy.actor_role enum in migration 0003. The ordering
// below is not a ranking of trust -- OWNER is not "more trusted" than
// RISK_OPERATOR, it is a different authority. An OWNER is accountable for
// capital; a RISK_OPERATOR is accountable for controls. Conflating them is how
// segregation of duties gets lost.
type ActorRole string

const (
	RoleResearcher   ActorRole = "RESEARCHER"
	RoleTradingOp    ActorRole = "TRADING_OPERATOR"
	RoleRiskOperator ActorRole = "RISK_OPERATOR"
	RoleOwner        ActorRole = "OWNER"
)

var actorRoles = []ActorRole{RoleResearcher, RoleTradingOp, RoleRiskOperator, RoleOwner}

var actorRoleSet = func() map[ActorRole]struct{} {
	m := make(map[ActorRole]struct{}, len(actorRoles))
	for _, r := range actorRoles {
		m[r] = struct{}{}
	}
	return m
}()

// ActorRoles returns every role in declaration order. The result is a copy.
func ActorRoles() []ActorRole {
	out := make([]ActorRole, len(actorRoles))
	copy(out, actorRoles)
	return out
}

// Known reports whether r is a recognised role.
func (r ActorRole) Known() bool {
	_, ok := actorRoleSet[r]
	return ok
}

// RiskCapable reports whether this role may authorise risk-increasing capability.
//
// A strategy reaching DEPLOYED is a strategy permitted to originate orders that
// create exposure. Only RISK_OPERATOR and OWNER can do that, and never a
// RESEARCHER -- doc 01 §10 invariant 1 forbids AI or model output from
// authorising its own execution, and doc 07 makes the research author unable to
// approve their own strategy.
func (r ActorRole) RiskCapable() bool {
	return r == RoleRiskOperator || r == RoleOwner
}

// TransitionRule is one permitted lifecycle move, with the authority it demands.
type TransitionRule struct {
	From State
	To   State

	// RequiredRole is the role that may perform the transition. The rule names
	// one role, not a set: permitting "any of these" would make it impossible to
	// answer the question an operator actually asks, which is "who may do this",
	// rather than "who might do this".
	RequiredRole ActorRole

	// RequiresDualControl marks a transition that must be authorised by two
	// distinct actors.
	//
	// Four of the twelve rules carry it, and they are exactly the transitions
	// that authorise risk: SHADOW->APPROVED, APPROVED->DEPLOYED, PAUSED->DEPLOYED
	// and DEPLOYED->RETIRED. Dual control here is not ceremony; doc 11 G11
	// requires dual control for live activation, and a lifecycle that let one
	// person promote a strategy into order-originating capability would route
	// around that requirement entirely.
	RequiresDualControl bool
}

var transitionRules = []TransitionRule{
	{StateDraft, StateReview, RoleResearcher, false},
	{StateReview, StateBacktested, RoleResearcher, false},
	{StateBacktested, StateSimulation, RoleResearcher, false},
	{StateSimulation, StatePaper, RoleTradingOp, false},
	{StatePaper, StateShadow, RoleTradingOp, false},
	{StateShadow, StateApproved, RoleRiskOperator, true},
	{StateApproved, StateDeployed, RoleOwner, true},
	{StateDeployed, StatePaused, RoleTradingOp, false},
	{StatePaused, StateDeployed, RoleRiskOperator, true},
	{StateDeployed, StateRetired, RoleOwner, true},
	{StatePaused, StateRetired, RoleOwner, false},
	{StateShadow, StateRetired, RoleRiskOperator, false},
}

var ruleIndex = func() map[ruleKey]TransitionRule {
	m := make(map[ruleKey]TransitionRule, len(transitionRules))
	for _, r := range transitionRules {
		m[ruleKey{r.From, r.To}] = r
	}
	return m
}()

type ruleKey struct {
	from State
	to   State
}

// TransitionRules returns the closed set of permitted transitions. The result is a copy.
func TransitionRules() []TransitionRule {
	out := make([]TransitionRule, len(transitionRules))
	copy(out, transitionRules)
	return out
}

// Rule returns the rule permitting from -> to, if any.
//
// Keyed on the (From, To) pair, not on From alone. An earlier version keyed on
// From and assumed the lifecycle was a strict chain -- one successor per state.
// That assumption is false: DEPLOYED reaches both PAUSED and RETIRED, PAUSED
// reaches both DEPLOYED and RETIRED, and SHADOW reaches both APPROVED and
// RETIRED. Keyed on From, three of the twelve rules were silently discarded and
// the three transitions they governed reported "no such rule" instead of
// validating, which meant SHADOW->APPROVED, DEPLOYED->PAUSED and PAUSED->DEPLOYED
// were all unusable.
//
// The failure was quiet in the worst way: the table was fully populated, the
// package compiled, and the three missing rules were exactly the ones governing
// pause, resume and approval. TestEachStateHasAtMostOneSuccessor found it, and
// it is kept as a test even though it now passes, because it documents that the
// branch structure of this machine is not a chain and a future edit must not
// assume it is.
func Rule(from State, to State) (TransitionRule, bool) {
	r, ok := ruleIndex[ruleKey{from, to}]
	if !ok {
		return TransitionRule{}, false
	}
	return r, true
}

// Permits reports whether from -> to is in the closed set, ignoring authority.
func Permits(from State, to State) bool {
	_, ok := Rule(from, to)
	return ok
}

// LegalTargets returns every state reachable from s, sorted.
func LegalTargets(s State) []State {
	if !s.Known() {
		return nil
	}
	var out []State
	for _, r := range transitionRules {
		if r.From == s {
			out = append(out, r.To)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ValidationError reports a refused lifecycle transition.
type ValidationError struct {
	From   State
	To     State
	Role   ActorRole
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("strategy: %s (%s -> %s by %s)", e.Reason, e.From, e.To, e.Role)
}

// TransitionRequest is a proposed lifecycle move with the authority attached.
type TransitionRequest struct {
	From State
	To   State

	// Role is the role the initiator holds.
	Role ActorRole

	// Subject is the acting identity. Required for every transition.
	Subject string

	// SecondSubject is the co-approver. It is required for, and only for,
	// dual-control transitions.
	SecondSubject string
}

// Validate checks a transition against the closed set, the required role, and
// the dual-control requirement.
//
// The two dual-control checks are deliberately not one. A missing second
// subject and a second subject identical to the first are different failures:
// the first is an incomplete command, the second is an attempt to satisfy
// segregation of duties with one person wearing two hats. Collapsing them into
// "dual control not satisfied" would hide the second, which is the one that
// means someone is trying to get around the control.
func Validate(req TransitionRequest) error {
	if !req.From.Known() {
		return &ValidationError{req.From, req.To, req.Role, "unknown source state"}
	}
	if !req.To.Known() {
		return &ValidationError{req.From, req.To, req.Role, "unknown target state"}
	}
	if !req.Role.Known() {
		return &ValidationError{req.From, req.To, req.Role, "unknown actor role"}
	}

	rule, ok := Rule(req.From, req.To)
	if !ok {
		return &ValidationError{req.From, req.To, req.Role, "no such rule in the closed set"}
	}

	if req.Subject == "" {
		return &ValidationError{req.From, req.To, req.Role, "no acting identity recorded"}
	}

	if rule.RequiredRole != req.Role {
		return &ValidationError{
			req.From, req.To, req.Role,
			fmt.Sprintf("requires role %s", rule.RequiredRole),
		}
	}

	if rule.RequiresDualControl {
		if req.SecondSubject == "" {
			return &ValidationError{
				req.From, req.To, req.Role,
				"requires dual control: a second distinct authorised identity is required",
			}
		}
		if req.SecondSubject == req.Subject {
			return &ValidationError{
				req.From, req.To, req.Role,
				"dual control requires two distinct identities; the co-approver is the initiator",
			}
		}
	}

	return nil
}
