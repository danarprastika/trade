// Package oms holds the canonical order state machine as pure domain logic.
//
// # Authority and relationship to SQL
//
// Doc 01 §3 makes the OMS authoritative for internal order state. This package
// is that authority. The trigger oms.guard_order_update in migration 0013 is a
// defence-in-depth backstop at the storage boundary: it exists so that a write
// that bypasses this package -- a bug, a migration, an operator with a psql
// session -- still cannot move an order to a state the machine forbids.
//
// Two implementations of one state machine is a defect waiting to happen, and
// this repository has already produced one: the canonical ID encoder existed in
// Go and in PostgreSQL, agreed on alphabet, length and entropy, was documented
// as "opaque so the encodings may differ by design", and produced different
// identifiers. Migration 0020 fixed that by making the two identical and pinning
// them together.
//
// The same treatment applies here. TransitionRule below is a transcription of
// oms.state_transition_rule, and TestTransitionRulesMatchTheDatabase reads the
// live table and compares it row for row. If a migration changes the SQL and
// this file is not updated, the parity test fails. That failure is the entire
// point: it is far better than two tables that disagree silently.
//
// The rules are data, not control flow, for the same reason. A closed set of
// permitted transitions is a fact about the machine, and a fact is easier to
// verify against another language than a function is.
package oms

import (
	"fmt"
	"sort"
)

// State is a state in the closed OMS order state machine.
//
// Doc 04 defines the lifecycle as
// CREATED -> RISK_PENDING -> RISK_APPROVED -> SUBMITTING -> ACKNOWLEDGED ->
// PARTIALLY_FILLED -> FILLED, with terminal alternatives CANCELLED, REJECTED,
// EXPIRED and UNKNOWN. Every name here comes from the oms.order_state enum in
// migration 0004; the parity test asserts the two agree.
type State string

const (
	StateCreated         State = "CREATED"
	StateRiskPending     State = "RISK_PENDING"
	StateRiskApproved    State = "RISK_APPROVED"
	StateSubmitting      State = "SUBMITTING"
	StateAcknowledged    State = "ACKNOWLEDGED"
	StatePartiallyFilled State = "PARTIALLY_FILLED"
	StateFilled          State = "FILLED"
	StateCancelled       State = "CANCELLED"
	StateRejected        State = "REJECTED"
	StateExpired         State = "EXPIRED"
	StateUnknown         State = "UNKNOWN"
)

var states = []State{
	StateCreated, StateRiskPending, StateRiskApproved, StateSubmitting,
	StateAcknowledged, StatePartiallyFilled, StateFilled, StateCancelled,
	StateRejected, StateExpired, StateUnknown,
}

var stateSet = func() map[State]struct{} {
	m := make(map[State]struct{}, len(states))
	for _, s := range states {
		m[s] = struct{}{}
	}
	return m
}()

// States returns every state in the machine, in declaration order.
//
// The slice is a copy. The package-level tables are shared and a caller that
// mutated one would silently redefine the state machine for every other caller.
func States() []State {
	out := make([]State, len(states))
	copy(out, states)
	return out
}

// Known reports whether s is a member of the closed state set.
//
// The state set is closed (doc 01 §8): an unknown state is not a future
// extension to be tolerated, it is a rejection. This is the Go-side equivalent
// of the enum in PostgreSQL, which likewise cannot hold a value outside its
// declaration.
func (s State) Known() bool {
	_, ok := stateSet[s]
	return ok
}

// Terminal reports whether s admits no further transition.
//
// UNKNOWN is deliberately not terminal. A submission timeout leaves the venue
// outcome undetermined, and doc 04 requires reconciliation before any retry that
// could duplicate exposure -- so the order must remain in the machine until the
// outcome is known. Treating UNKNOWN as terminal would be the comfortable
// choice and the dangerous one: it would end the order's life precisely when the
// platform knows least about what happened.
func (s State) Terminal() bool {
	switch s {
	case StateFilled, StateCancelled, StateRejected, StateExpired:
		return true
	default:
		return false
	}
}

// EventKind identifies the journal event that declares a transition.
type EventKind string

const (
	EventCreated         EventKind = "CREATED"
	EventRiskApproved    EventKind = "RISK_APPROVED"
	EventRiskRejected    EventKind = "RISK_REJECTED"
	EventSubmitting      EventKind = "SUBMITTING"
	EventSubmitFailed    EventKind = "SUBMIT_FAILED"
	EventAcknowledged    EventKind = "ACKNOWLEDGED"
	EventPartiallyFilled EventKind = "PARTIALLY_FILLED"
	EventFilled          EventKind = "FILLED"
	EventCancelRequested EventKind = "CANCEL_REQUESTED"
	EventCancelled       EventKind = "CANCELLED"
	EventRejectedByVenue EventKind = "REJECTED_BY_VENUE"
	EventExpired         EventKind = "EXPIRED"
	EventUnknownOutcome  EventKind = "UNKNOWN_OUTCOME"
	EventOutcomeResolved EventKind = "OUTCOME_RESOLVED"
)

var eventKinds = []EventKind{
	EventCreated, EventRiskApproved, EventRiskRejected, EventSubmitting,
	EventSubmitFailed, EventAcknowledged, EventPartiallyFilled, EventFilled,
	EventCancelRequested, EventCancelled, EventRejectedByVenue, EventExpired,
	EventUnknownOutcome, EventOutcomeResolved,
}

var eventKindSet = func() map[EventKind]struct{} {
	m := make(map[EventKind]struct{}, len(eventKinds))
	for _, e := range eventKinds {
		m[e] = struct{}{}
	}
	return m
}()

// EventKinds returns every event kind in the machine, in declaration order.
func EventKinds() []EventKind {
	out := make([]EventKind, len(eventKinds))
	copy(out, eventKinds)
	return out
}

// Known reports whether e is a member of the closed event set.
func (e EventKind) Known() bool {
	_, ok := eventKindSet[e]
	return ok
}

// TransitionRule is one permitted move in the closed set.
type TransitionRule struct {
	From State
	To   State
	Via  EventKind

	// RequiresRiskDecision marks a transition that turns an intention into
	// possible exposure at the venue. Doc 09 invariant 1: no risk-increasing
	// command may reach submission without a decision.
	RequiresRiskDecision bool

	// RequiresOutcomeResolution marks a transition out of UNKNOWN. Doc 01 §6:
	// an unknown external outcome is never converted into a determinate state
	// by assumption.
	RequiresOutcomeResolution bool
}

// transitionRules is the closed set, transcribed from the INSERT into
// oms.state_transition_rule in migration 0004.
//
// The transcription is exact and in the same order. TestTransitionRulesMatchTheDatabase
// reads the live table and fails on any difference, in either direction: a rule
// added in SQL but not here, or a rule here that SQL does not accept.
var transitionRules = []TransitionRule{
	{StateCreated, StateRiskPending, EventCreated, false, false},
	{StateRiskPending, StateRiskApproved, EventRiskApproved, true, false},
	{StateRiskPending, StateRejected, EventRiskRejected, false, false},
	{StateRiskPending, StateCancelled, EventCancelled, false, false},
	{StateRiskApproved, StateSubmitting, EventSubmitting, true, false},
	{StateRiskApproved, StateCancelled, EventCancelled, false, false},
	{StateSubmitting, StateAcknowledged, EventAcknowledged, true, false},
	{StateSubmitting, StateRejected, EventRejectedByVenue, false, false},
	{StateSubmitting, StateExpired, EventExpired, false, false},
	{StateSubmitting, StateUnknown, EventUnknownOutcome, false, true},
	{StateSubmitting, StateCancelled, EventCancelled, false, false},
	{StateAcknowledged, StatePartiallyFilled, EventPartiallyFilled, false, false},
	{StateAcknowledged, StateFilled, EventFilled, false, false},
	{StateAcknowledged, StateCancelled, EventCancelled, false, false},
	{StateAcknowledged, StateExpired, EventExpired, false, false},
	{StatePartiallyFilled, StatePartiallyFilled, EventPartiallyFilled, false, false},
	{StatePartiallyFilled, StateFilled, EventFilled, false, false},
	{StatePartiallyFilled, StateCancelled, EventCancelled, false, false},
	{StatePartiallyFilled, StateExpired, EventExpired, false, false},
	{StateUnknown, StateFilled, EventOutcomeResolved, false, true},
	{StateUnknown, StateRejected, EventOutcomeResolved, false, true},
	{StateUnknown, StateCancelled, EventOutcomeResolved, false, true},
	{StateUnknown, StateExpired, EventOutcomeResolved, false, true},
	{StateUnknown, StateAcknowledged, EventOutcomeResolved, false, true},
	{StateUnknown, StatePartiallyFilled, EventOutcomeResolved, false, true},
}

// TransitionRules returns the closed set of permitted transitions.
//
// The slice is a copy, for the same reason States returns a copy: the table is
// the machine, and handing out a mutable reference to it would let one caller
// redefine the machine for all of them.
func TransitionRules() []TransitionRule {
	out := make([]TransitionRule, len(transitionRules))
	copy(out, transitionRules)
	return out
}

var ruleIndex = func() map[ruleKey]TransitionRule {
	m := make(map[ruleKey]TransitionRule, len(transitionRules))
	for _, r := range transitionRules {
		m[ruleKey{r.From, r.To, r.Via}] = r
	}
	return m
}()

type ruleKey struct {
	from State
	to   State
	via  EventKind
}

// Rule returns the rule permitting from -> to via via.
//
// The second result is false for every transition not in the closed set,
// including any involving an unknown state or event kind. There is no default
// and no wildcard: an unrecognised request is a rejection, because doc 01 §8
// requires unknown states and unknown transitions to be rejected and routed to
// reconciliation rather than absorbed.
func Rule(from State, to State, via EventKind) (TransitionRule, bool) {
	r, ok := ruleIndex[ruleKey{from, to, via}]
	return r, ok
}

// Permits reports whether from -> to via via is in the closed set.
func Permits(from State, to State, via EventKind) bool {
	_, ok := Rule(from, to, via)
	return ok
}

// LegalTargets returns every state reachable from s, sorted.
//
// Used by the operator interface and by tests that assert reachability without
// enumerating paths by hand. An empty result means the order is finished.
func LegalTargets(s State) []State {
	if !s.Known() {
		return nil
	}
	seen := map[State]struct{}{}
	var out []State
	for _, r := range transitionRules {
		if r.From == s {
			if _, dup := seen[r.To]; dup {
				continue
			}
			seen[r.To] = struct{}{}
			out = append(out, r.To)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ValidationError is returned when a state machine request is not permitted.
//
// It carries the request verbatim so that a caller can log exactly what was
// asked for. An error message that says "invalid transition" without the states
// involved is close to useless during an incident.
type ValidationError struct {
	From State
	To   State
	Via  EventKind
	// Reason distinguishes "no such rule" from "the rule exists but its
	// preconditions are unmet", because those have different remedies.
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("oms: %s (%s -> %s via %s)", e.Reason, e.From, e.To, e.Via)
}

// TransitionRequest is a proposed move with the evidence attached to it.
type TransitionRequest struct {
	From State
	To   State
	Via  EventKind

	// RiskDecisionID is the durable decision that authorised the order to
	// become possible exposure. Empty when the transition does not require one.
	RiskDecisionID string
}

// Validate checks a proposed transition against the closed set and against the
// preconditions the closed set attaches to it.
//
// The two failure modes are kept distinct on purpose. "No such rule" means the
// caller asked for something the machine has no concept of, which is a bug or a
// tamper. "Precondition unmet" means the move is legitimate but the evidence is
// missing -- no risk decision attached, or an UNKNOWN being resolved by
// assumption. They look identical to a caller that only checks for an error,
// and they should not: the first is a defect, the second is a halt.
func Validate(req TransitionRequest) error {
	if !req.From.Known() {
		return &ValidationError{req.From, req.To, req.Via, "unknown source state"}
	}
	if !req.To.Known() {
		return &ValidationError{req.From, req.To, req.Via, "unknown target state"}
	}
	if !req.Via.Known() {
		return &ValidationError{req.From, req.To, req.Via, "unknown event kind"}
	}

	rule, ok := Rule(req.From, req.To, req.Via)
	if !ok {
		return &ValidationError{req.From, req.To, req.Via, "no such rule in the closed set"}
	}

	if rule.RequiresRiskDecision && req.RiskDecisionID == "" {
		return &ValidationError{
			req.From, req.To, req.Via,
			"requires a durable risk decision and none is attached " +
				"(09_TESTING_AND_RELEASE_EVIDENCE.md invariant 1)",
		}
	}

	// Every rule out of UNKNOWN already carries EventOutcomeResolved as its
	// event kind, so this cannot currently fail. It is checked anyway because
	// it is the invariant that matters most and it costs one comparison.
	//
	// A future edit that added e.g. UNKNOWN->FILLED via FILLED would be caught
	// here rather than being permitted by the rule lookup, and the check is
	// what makes that edit safe to attempt.
	if req.From == StateUnknown && rule.RequiresOutcomeResolution && req.Via != EventOutcomeResolved {
		return &ValidationError{
			req.From, req.To, req.Via,
			"an UNKNOWN order leaves UNKNOWN only through an explicit OUTCOME_RESOLVED event; " +
				"an ambiguous venue outcome is never converted into a determinate state by assumption",
		}
	}

	return nil
}

// EntryStates returns the states a new order may be inserted in.
//
// Migration 0008's oms.guard_order_entry_state enforces the same set. An order
// that entered at any other state would have arrived already exposed, having
// skipped every check the machine exists to apply.
func EntryStates() []State {
	out := make([]State, 2)
	out[0] = StateCreated
	out[1] = StateRiskPending
	return out
}
