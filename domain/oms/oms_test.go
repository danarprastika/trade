package oms

import (
	"strings"
	"testing"

	"github.com/aitc/trade/contracts"
)

func TestStateSetIsClosed(t *testing.T) {
	for _, s := range States() {
		if !s.Known() {
			t.Errorf("%s is listed in States() but Known() says false", s)
		}
	}
	for _, s := range []State{"", "PENDING", "filled", "FILLED ", "SUBMITTINGX", "UNKNOWN_UNKNOWN"} {
		if s.Known() {
			t.Errorf("State(%q).Known() = true, want false: the state set must be closed", s)
		}
	}
}

func TestEventKindSetIsClosed(t *testing.T) {
	for _, e := range EventKinds() {
		if !e.Known() {
			t.Errorf("%s is listed in EventKinds() but Known() says false", e)
		}
	}
	for _, e := range []EventKind{"", "FILLED!", "filled", "UNKNOWN"} {
		if e.Known() {
			t.Errorf("EventKind(%q).Known() = true, want false", e)
		}
	}
}

// The machine must be a closed set in the sense that matters: no transition
// exists that was not declared. This walks the declared rules and checks each
// one's three components are themselves members of the closed vocabularies,
// which is the failure mode a hand-edited table produces.
func TestEveryDeclaredRuleUsesKnownStatesAndEvents(t *testing.T) {
	rules := TransitionRules()
	if len(rules) == 0 {
		t.Fatal("TransitionRules() is empty; the closed set cannot be empty")
	}
	for i, r := range rules {
		if !r.From.Known() {
			t.Errorf("rule %d: from-state %q is not in the closed set", i, r.From)
		}
		if !r.To.Known() {
			t.Errorf("rule %d: to-state %q is not in the closed set", i, r.To)
		}
		if !r.Via.Known() {
			t.Errorf("rule %d: event kind %q is not in the closed set", i, r.Via)
		}
	}
}

func TestTransitionRulesHaveNoDuplicates(t *testing.T) {
	seen := map[string]TransitionRule{}
	for _, r := range TransitionRules() {
		key := string(r.From) + "->" + string(r.To) + "/" + string(r.Via)
		if prev, dup := seen[key]; dup {
			t.Errorf("duplicate rule %s: first requires_risk=%v resolution=%v, second requires_risk=%v resolution=%v",
				key, prev.RequiresRiskDecision, prev.RequiresOutcomeResolution,
				r.RequiresRiskDecision, r.RequiresOutcomeResolution)
		}
		seen[key] = r
	}
}

// Every state must either be terminal or have somewhere to go. A state that is
// neither is a dead end the machine never intended, and an operator holding an
// order in it has no legal action.
func TestEveryNonTerminalStateHasALegalTarget(t *testing.T) {
	for _, s := range States() {
		targets := LegalTargets(s)
		if s.Terminal() {
			if len(targets) != 0 {
				t.Errorf("terminal state %s has %d outgoing transitions: %v", s, len(targets), targets)
			}
			continue
		}
		if len(targets) == 0 {
			t.Errorf("non-terminal state %s has no legal target; it is a dead end", s)
		}
	}
}

// Doc 01 §8 requires unknown transitions to be rejected, not absorbed. This
// checks a spread of plausible-but-illegal moves, each of which a careless
// implementation would allow.
func TestIllegalTransitionsAreRejected(t *testing.T) {
	cases := []struct {
		name       string
		from, to   State
		via        EventKind
		wantSubstr string
	}{
		{"no forward from terminal", StateFilled, StatePartiallyFilled, EventPartiallyFilled, "no such rule"},
		{"cancelled is terminal", StateCancelled, StateSubmitting, EventSubmitting, "no such rule"},
		{"rejected is terminal", StateRejected, StateCreated, EventCreated, "no such rule"},
		{"expired is terminal", StateExpired, StateAcknowledged, EventAcknowledged, "no such rule"},
		{"cannot skip risk", StateCreated, StateSubmitting, EventSubmitting, "no such rule"},
		{"cannot skip to filled", StateRiskApproved, StateFilled, EventFilled, "no such rule"},
		{"wrong event for the move", StateRiskPending, StateRiskApproved, EventCancelled, "no such rule"},
		{"no backward to pending", StateRiskApproved, StateRiskPending, EventCreated, "no such rule"},
		{"deployed-like state does not exist", "DEPLOYED", StateFilled, EventFilled, "unknown source state"},
		{"unknown target state", StateCreated, "PENDING", EventCreated, "unknown target state"},
		{"unknown event", StateCreated, StateRiskPending, "PENDING", "unknown event kind"},
		{"unknown leaving unknown", StateUnknown, StateFilled, "FILLED", "no such rule"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(TransitionRequest{From: tc.from, To: tc.to, Via: tc.via, RiskDecisionID: "rd-1"})
			if err == nil {
				t.Fatalf("Validate(%s -> %s via %s) = nil, want rejection containing %q",
					tc.from, tc.to, tc.via, tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// Doc 04: UNKNOWN is mandatory when a submission timeout prevents determining
// the venue outcome, and the system reconciles before any retry that could
// duplicate exposure. So UNKNOWN must not be a dead end, and it must not be
// escapable by assumption.
func TestUnknownIsNeitherTerminalNorEscapableByAssumption(t *testing.T) {
	if StateUnknown.Terminal() {
		t.Fatal("StateUnknown.Terminal() = true; an undetermined venue outcome must keep the order " +
			"in the machine until it is resolved, not end its life")
	}

	targets := LegalTargets(StateUnknown)
	if len(targets) == 0 {
		t.Fatal("UNKNOWN has no legal target; the order would be stuck with no way to resolve it")
	}

	// Every escape from UNKNOWN must be an explicit resolution.
	for _, to := range targets {
		for _, e := range EventKinds() {
			rule, ok := Rule(StateUnknown, to, e)
			if !ok {
				continue
			}
			if e != EventOutcomeResolved {
				t.Errorf("UNKNOWN -> %s is permitted via %s; only OUTCOME_RESOLVED may leave UNKNOWN", to, e)
			}
			if !rule.RequiresOutcomeResolution {
				t.Errorf("UNKNOWN -> %s via %s does not carry requires_outcome_resolution", to, e)
			}
		}
	}
}

// Doc 09 invariant 1: no risk-increasing command creates a live submission
// without a risk decision.
//
// The expected set is written out literally rather than derived from
// TransitionRules(). An earlier version of this test collected the rules whose
// RequiresRiskDecision was true and asserted on those, which meant a mutation
// clearing the flag on RISK_APPROVED -> SUBMITTING removed the rule from the
// collection and the test passed having asserted nothing. It was caught only
// because the Go/SQL parity test noticed the divergence.
//
// A test that derives its expectations from the thing under test verifies that
// the thing is self-consistent, which is not the same as verifying it is
// correct. The literal list below is the specification; the table is the
// implementation; the two are compared.
func TestRiskIncreasingTransitionsRequireADecision(t *testing.T) {
	want := []struct {
		from State
		to   State
		via  EventKind
	}{
		// The order becomes approved to trade.
		{StateRiskPending, StateRiskApproved, EventRiskApproved},
		// The order is submitted to the venue: this is the exposure.
		{StateRiskApproved, StateSubmitting, EventSubmitting},
		// The venue has the order and is working it.
		{StateSubmitting, StateAcknowledged, EventAcknowledged},
	}

	for _, w := range want {
		rule, ok := Rule(w.from, w.to, w.via)
		if !ok {
			t.Errorf("%s -> %s via %s is not in the closed set at all", w.from, w.to, w.via)
			continue
		}
		if !rule.RequiresRiskDecision {
			t.Errorf("%s -> %s via %s does not require a risk decision; doc 09 invariant 1 "+
				"forbids a risk-increasing command reaching submission without one", w.from, w.to, w.via)
			continue
		}

		// Without a decision attached, the move must be refused.
		err := Validate(TransitionRequest{From: w.from, To: w.to, Via: w.via})
		if err == nil {
			t.Errorf("Validate(%s -> %s via %s) = nil without a risk decision, want rejection",
				w.from, w.to, w.via)
			continue
		}
		if !strings.Contains(err.Error(), "risk decision") {
			t.Errorf("%s -> %s via %s: error %q does not mention the missing risk decision",
				w.from, w.to, w.via, err.Error())
		}

		// With one attached it must pass, or the control would be
		// unconditional and the machine unusable.
		if err := Validate(TransitionRequest{
			From: w.from, To: w.to, Via: w.via, RiskDecisionID: "rd-1",
		}); err != nil {
			t.Errorf("%s -> %s via %s rejected despite carrying a risk decision: %v",
				w.from, w.to, w.via, err)
		}
	}

	// And no transition outside that set may claim to require a decision, or
	// the machine would demand paperwork from paths that have no exposure.
	for _, r := range TransitionRules() {
		if !r.RequiresRiskDecision {
			continue
		}
		known := false
		for _, w := range want {
			if r.From == w.from && r.To == w.to && r.Via == w.via {
				known = true
			}
		}
		if !known {
			t.Errorf("%s -> %s via %s requires a risk decision but is not one of the three "+
				"exposure-creating transitions", r.From, r.To, r.Via)
		}
	}
}

// A rejected order must never be able to proceed. REJECTED is terminal, so this
// also guards the terminal-state rule.
func TestRejectedOrderCannotProceed(t *testing.T) {
	if targets := LegalTargets(StateRejected); len(targets) != 0 {
		t.Fatalf("REJECTED has outgoing transitions %v; a rejected order must be finished", targets)
	}
}

// Doc 09 invariant 3: duplicate commands do not increase exposure. The state
// machine cannot see a duplicate command, so the guarantee lives in idempotency
// and the entry rules; this test pins the part of it that IS in the machine --
// an order that reached REJECTED or CANCELLED cannot be resurrected.
func TestTerminalStatesCannotBeResurrected(t *testing.T) {
	terminal := []State{StateFilled, StateCancelled, StateRejected, StateExpired}
	for _, s := range terminal {
		for _, e := range EventKinds() {
			for _, to := range States() {
				if Permits(s, to, e) {
					t.Errorf("%s -> %s via %s is permitted from a terminal state", s, to, e)
				}
			}
		}
	}
}

// The state machine must not contain a cycle that returns to an earlier
// progress state, because that is a reopen. The only permitted self-transition
// is PARTIALLY_FILLED -> PARTIALLY_FILLED, which is the order accumulating
// fills. A self-transition on any other state is a resubmission in disguise.
func TestSelfTransitionsAreOnlyPartialFills(t *testing.T) {
	for _, r := range TransitionRules() {
		if r.From == r.To && r.From != StatePartiallyFilled {
			t.Errorf("%s -> %s via %s is a self-transition; only PARTIALLY_FILLED may self-transition", r.From, r.To, r.Via)
		}
	}
	if !Permits(StatePartiallyFilled, StatePartiallyFilled, EventPartiallyFilled) {
		t.Error("PARTIALLY_FILLED must be able to accumulate additional partial fills")
	}
}

// Every state must be reachable from an entry state, or part of the declared
// closed set is decoration.
func TestEveryStateIsReachableFromAnEntryState(t *testing.T) {
	reachable := map[State]bool{}
	queue := append([]State{}, EntryStates()...)
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		if reachable[s] {
			continue
		}
		reachable[s] = true
		queue = append(queue, LegalTargets(s)...)
	}

	for _, s := range States() {
		if !reachable[s] {
			t.Errorf("state %s is not reachable from any entry state; it is unreachable decoration", s)
		}
	}
}

// An order inserted already exposed would have skipped the machine entirely.
func TestEntryStatesAreOnlyTheTwoSafeOnes(t *testing.T) {
	entry := EntryStates()
	if len(entry) != 2 {
		t.Fatalf("EntryStates() = %v, want exactly 2 states", entry)
	}
	if entry[0] != StateCreated || entry[1] != StateRiskPending {
		t.Errorf("EntryStates() = %v, want [CREATED RISK_PENDING]", entry)
	}
	for _, s := range States() {
		switch s {
		case StateCreated, StateRiskPending:
		default:
			if Permits(s, s, EventCreated) && s != StateCreated {
				t.Errorf("%s accepts the CREATED event; only entry states may", s)
			}
		}
	}
}

// The risk-reducing set is exactly REDUCE_ONLY and CLOSE_POSITION, and it
// decides whether a halt blocks an action. A false positive refuses to let an
// operator flatten a position during an incident.
//
// This used to test a package-local OrderType that carried six of the nine
// values in common.order_type. The test passed, and would have kept passing, with
// POST_ONLY, IOC and FOK simply absent -- an enumeration that checks only the
// members it happens to mention. It now runs against contracts.OrderType and
// names all nine, so a tenth value in the database fails the shape of this test
// rather than passing unnoticed.
func TestRiskReducingClassificationIsExactlyTheTwoOrderTypes(t *testing.T) {
	reducing := []string{"REDUCE_ONLY", "CLOSE_POSITION"}
	increasing := []string{"MARKET", "LIMIT", "STOP", "STOP_LIMIT", "POST_ONLY", "IOC", "FOK"}

	if len(reducing)+len(increasing) != 9 {
		t.Fatalf("this test names %d order types; common.order_type has 9",
			len(reducing)+len(increasing))
	}

	for _, name := range reducing {
		o := contracts.OrderType(name)
		if !o.Valid() {
			t.Errorf("contracts.OrderType(%q).Valid() is false", name)
		}
		if o.RiskIncreasing() {
			t.Errorf("%s is risk-increasing; it is the risk-reducing set's own member", name)
		}
	}
	for _, name := range increasing {
		o := contracts.OrderType(name)
		if !o.Valid() {
			t.Errorf("contracts.OrderType(%q).Valid() is false", name)
		}
		if !o.RiskIncreasing() {
			t.Errorf("%s is not risk-increasing; only REDUCE_ONLY and CLOSE_POSITION are not", name)
		}
	}

	// An unknown value is risk-increasing, because that is the direction that
	// applies the full control set. The alternative -- treating an unrecognised
	// type as risk-reducing -- would let an invented order type bypass both the
	// risk gate and a halt.
	for _, bogus := range []string{"", "limit", "ICEBERG", "reduce_only", "REDUCE_ONLY "} {
		o := contracts.OrderType(bogus)
		if o.Valid() {
			t.Errorf("contracts.OrderType(%q) is Valid; the closed set is upper case and exact", bogus)
		}
		if !o.RiskIncreasing() {
			t.Errorf("contracts.OrderType(%q) is not risk-increasing; an unknown type must take "+
				"the full control set rather than the reduced one", bogus)
		}
	}
}

// IOC and FOK are risk-increasing order types that oms.order refuses to hold in
// its order_type column, because time_in_force expresses them. The two facts are
// independent and both matter: the order is risk-increasing whichever column
// carries the instruction, and migration 0022's constraint is what stops the
// instruction being carried twice.
func TestIocAndFokAreRiskIncreasingEvenThoughOrderTypeForbidsThem(t *testing.T) {
	for _, name := range []string{"IOC", "FOK"} {
		if !contracts.OrderType(name).RiskIncreasing() {
			t.Errorf("%s must be risk-increasing", name)
		}
	}
	if !contracts.TimeInForce("IOC").Valid() || !contracts.TimeInForce("FOK").Valid() {
		t.Error("IOC and FOK must be expressible through time_in_force; that is the column oms.order uses for them")
	}
}

// A caller that mutates the returned slice must not be able to redefine the
// machine for every other caller.
func TestTransitionRulesReturnsACopy(t *testing.T) {
	first := TransitionRules()
	if len(first) == 0 {
		t.Fatal("no rules to copy")
	}
	original := TransitionRules()

	first[0] = TransitionRule{From: StateFilled, To: StateCreated, Via: EventCreated}
	first[0].RequiresRiskDecision = true

	after := TransitionRules()
	for i := range after {
		if after[i] != original[i] {
			t.Fatalf("mutating the returned slice changed the shared table at index %d: got %+v, want %+v",
				i, after[i], original[i])
		}
	}
}

func TestValidationErrorNamesTheRequest(t *testing.T) {
	err := Validate(TransitionRequest{From: StateFilled, To: StateCreated, Via: EventCreated})
	if err == nil {
		t.Fatal("expected rejection")
	}
	msg := err.Error()
	for _, want := range []string{"FILLED", "CREATED"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q; an error that omits the states involved is "+
				"close to useless during an incident", msg, want)
		}
	}
}
