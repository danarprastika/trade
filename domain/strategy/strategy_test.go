package strategy

import (
	"strings"
	"testing"
)

func TestStateSetIsClosed(t *testing.T) {
	for _, s := range States() {
		if !s.Known() {
			t.Errorf("%s is listed in States() but Known() says false", s)
		}
	}
	for _, s := range []State{"", "DRAFTING", "draft", "DEPLOYED ", "ARCHIVED"} {
		if s.Known() {
			t.Errorf("State(%q).Known() = true, want false", s)
		}
	}
}

func TestActorRoleSetIsClosed(t *testing.T) {
	for _, r := range ActorRoles() {
		if !r.Known() {
			t.Errorf("%s is listed in ActorRoles() but Known() says false", r)
		}
	}
	for _, r := range []ActorRole{"", "ADMIN", "admin", "OWNER "} {
		if r.Known() {
			t.Errorf("ActorRole(%q).Known() = true, want false", r)
		}
	}
}

// This lifecycle is NOT a chain: DEPLOYED reaches both PAUSED and RETIRED,
// PAUSED reaches both DEPLOYED and RETIRED, SHADOW reaches both APPROVED and
// RETIRED. An earlier version of this file asserted the opposite and Rule was
// keyed on From alone, which silently discarded three of the twelve rules --
// exactly the ones governing pause, resume and approval.
//
// The property worth asserting is therefore that every declared rule is
// reachable through Rule and that no (From, To) pair is declared twice. A
// dropped rule is invisible; a duplicate is a contradiction.
func TestEveryDeclaredRuleIsReachableAndUnique(t *testing.T) {
	seen := map[[2]State]bool{}
	for i, r := range TransitionRules() {
		key := string(r.From) + "->" + string(r.To)
		pair := [2]State{r.From, r.To}
		if seen[pair] {
			t.Errorf("rule %d: %s is declared more than once", i, key)
		}
		seen[pair] = true

		got, ok := Rule(r.From, r.To)
		if !ok {
			t.Errorf("rule %d: %s is declared but Rule() cannot find it; a rule the lookup "+
				"cannot reach is a rule that does not exist", i, key)
			continue
		}
		if got != r {
			t.Errorf("rule %d: Rule(%s) returned %+v, but the table declares %+v", i, key, got, r)
		}
	}
}

// Multi-successor states are the ones a chain-shaped implementation gets wrong,
// so they are named explicitly rather than left to the general test above.
func TestMultiSuccessorStatesAreNotLosingRules(t *testing.T) {
	cases := []struct {
		from State
		want []State
	}{
		{StateDeployed, []State{StatePaused, StateRetired}},
		{StatePaused, []State{StateDeployed, StateRetired}},
		{StateShadow, []State{StateApproved, StateRetired}},
	}
	for _, tc := range cases {
		got := LegalTargets(tc.from)
		if len(got) != len(tc.want) {
			t.Errorf("%s: LegalTargets = %v, want %v", tc.from, got, tc.want)
			continue
		}
		for _, w := range tc.want {
			if !Permits(tc.from, w) {
				t.Errorf("%s -> %s is declared in the table but Permits reports false", tc.from, w)
			}
		}
	}
}

func TestEveryDeclaredRuleUsesKnownStatesAndRoles(t *testing.T) {
	for i, r := range TransitionRules() {
		if !r.From.Known() {
			t.Errorf("rule %d: from-state %q is not in the closed set", i, r.From)
		}
		if !r.To.Known() {
			t.Errorf("rule %d: to-state %q is not in the closed set", i, r.To)
		}
		if !r.RequiredRole.Known() {
			t.Errorf("rule %d: required role %q is not in the closed set", i, r.RequiredRole)
		}
	}
}

func TestIllegalTransitionsAreRejected(t *testing.T) {
	cases := []struct {
		name       string
		from, to   State
		role       ActorRole
		wantSubstr string
	}{
		{"retired is terminal", StateRetired, StateDraft, RoleOwner, "no such rule"},
		{"cannot skip review", StateDraft, StateBacktested, RoleResearcher, "no such rule"},
		{"cannot jump to deployed", StateDraft, StateDeployed, RoleOwner, "no such rule"},
		{"cannot go backwards", StateBacktested, StateReview, RoleResearcher, "no such rule"},
		{"unknown source", "ARCHIVED", StateDraft, RoleOwner, "unknown source state"},
		{"unknown target", StateDraft, "ACTIVE", RoleOwner, "unknown target state"},
		{"unknown role", StateDraft, StateReview, "ADMIN", "unknown actor role"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(TransitionRequest{
				From: tc.from, To: tc.to, Role: tc.role, Subject: "u-1",
			})
			if err == nil {
				t.Fatalf("Validate(%s -> %s by %s) = nil, want rejection containing %q",
					tc.from, tc.to, tc.role, tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// The wrong role must be refused, and the message must name the right one --
// otherwise an operator has to read the rule table to find out who to ask.
func TestRequiredRoleIsEnforcedAndNamed(t *testing.T) {
	for _, r := range TransitionRules() {
		for _, other := range ActorRoles() {
			if other == r.RequiredRole {
				continue
			}
			err := Validate(TransitionRequest{From: r.From, To: r.To, Role: other, Subject: "u-1"})
			if err == nil {
				t.Errorf("%s -> %s accepted role %s, but requires %s", r.From, r.To, other, r.RequiredRole)
				continue
			}
			if !strings.Contains(err.Error(), string(r.RequiredRole)) {
				t.Errorf("%s -> %s by %s: error %q does not name the required role %s",
					r.From, r.To, other, err.Error(), r.RequiredRole)
			}
		}
	}
}

// A transition with no acting identity is unauditable. Doc 01 §8 requires every
// transition to declare an actor.
func TestTransitionRequiresAnActingIdentity(t *testing.T) {
	for _, r := range TransitionRules() {
		err := Validate(TransitionRequest{From: r.From, To: r.To, Role: r.RequiredRole, Subject: ""})
		if err == nil {
			t.Errorf("%s -> %s accepted with no acting identity", r.From, r.To)
			continue
		}
		if !strings.Contains(err.Error(), "acting identity") {
			t.Errorf("%s -> %s: error %q does not mention the missing identity", r.From, r.To, err.Error())
		}
	}
}

// Dual control is the control that matters most, so it gets the most scrutiny.
//
// Like the OMS risk-decision test, the expected set is written out literally.
// Collecting rules by their RequiresDualControl flag and asserting on the
// result verifies only that the table is self-consistent: clearing the flag
// would empty the collection and the test would pass having checked nothing.
func TestDualControlIsRequiredForRiskAuthorisingTransitions(t *testing.T) {
	dual := map[[2]State]bool{
		{StateShadow, StateApproved}:   true, // approves risk-increasing capability
		{StateApproved, StateDeployed}: true, // starts originating exposure
		{StatePaused, StateDeployed}:   true, // resumes originating exposure
		{StateDeployed, StateRetired}:  true, // unwinds a deployment under dual control
	}

	for pair := range dual {
		found := false
		for _, r := range TransitionRules() {
			if r.From == pair[0] && r.To == pair[1] {
				found = true
				if !r.RequiresDualControl {
					t.Errorf("%s -> %s does not require dual control", r.From, r.To)
				}
			}
		}
		if !found {
			t.Errorf("%s -> %s is missing from the closed set entirely", pair[0], pair[1])
		}
	}

	// Nothing else may claim dual control, or a routine promotion would acquire
	// a requirement nobody designed.
	for _, r := range TransitionRules() {
		if !r.RequiresDualControl {
			continue
		}
		if !dual[[2]State{r.From, r.To}] {
			t.Errorf("%s -> %s requires dual control but is not one of the four governed transitions",
				r.From, r.To)
		}
	}

	for pair := range dual {
		r, ok := Rule(pair[0], pair[1])
		if !ok {
			continue
		}
		t.Run(string(r.From)+"_to_"+string(r.To), func(t *testing.T) {
			// No co-approver.
			err := Validate(TransitionRequest{From: r.From, To: r.To, Role: r.RequiredRole, Subject: "u-1"})
			if err == nil {
				t.Fatal("accepted with no second approver")
			}
			if !strings.Contains(err.Error(), "second distinct") {
				t.Errorf("error %q does not identify the missing dual control", err.Error())
			}

			// Two distinct identities.
			if err := Validate(TransitionRequest{
				From: r.From, To: r.To, Role: r.RequiredRole,
				Subject: "u-1", SecondSubject: "u-2",
			}); err != nil {
				t.Errorf("rejected despite two distinct approvers: %v", err)
			}
		})
	}
}

// One person wearing two hats is a different failure from an incomplete
// command, and the message must say so. Collapsing them would hide an attempt
// to satisfy segregation of duties with a single identity.
func TestDualControlRejectsTheSameIdentityTwice(t *testing.T) {
	for _, r := range TransitionRules() {
		if !r.RequiresDualControl {
			continue
		}
		err := Validate(TransitionRequest{
			From: r.From, To: r.To, Role: r.RequiredRole,
			Subject: "u-1", SecondSubject: "u-1",
		})
		if err == nil {
			t.Errorf("%s -> %s accepted the same identity as both approvers", r.From, r.To)
			continue
		}
		if !strings.Contains(err.Error(), "two distinct identities") {
			t.Errorf("%s -> %s: error %q does not distinguish a repeated identity from a missing one",
				r.From, r.To, err.Error())
		}
	}
}

// A non-dual-control transition must not demand a second approver, or the
// lifecycle would be unusable.
func TestNonDualControlTransitionsDoNotRequireASecondApprover(t *testing.T) {
	for _, r := range TransitionRules() {
		if r.RequiresDualControl {
			continue
		}
		if err := Validate(TransitionRequest{
			From: r.From, To: r.To, Role: r.RequiredRole, Subject: "u-1",
		}); err != nil {
			t.Errorf("%s -> %s rejected with a single approver: %v", r.From, r.To, err)
		}
	}
}

// Doc 01 §10 invariant 1: no AI/model output can authorise its own execution,
// and doc 07 forbids a research author approving their own strategy. So the
// role that can approve and deploy must never be the role that writes.
func TestResearcherCanNeverAuthoriseRisk(t *testing.T) {
	for _, r := range TransitionRules() {
		if r.RequiredRole != RoleResearcher {
			continue
		}
		if !r.To.Known() {
			continue
		}
		if r.To == StateDeployed || r.To == StateApproved {
			t.Errorf("%s -> %s is performed by RESEARCHER; a research author must not be able "+
				"to approve or deploy a strategy for order-originating capability", r.From, r.To)
		}
		if r.RequiresDualControl {
			t.Errorf("%s -> %s requires dual control but is performed by RESEARCHER; "+
				"the second approver's authority is unstated, so the control is unverifiable",
				r.From, r.To)
		}
	}
	if RoleResearcher.RiskCapable() {
		t.Error("RoleResearcher.RiskCapable() = true; a researcher may not authorise risk")
	}
	if !RoleRiskOperator.RiskCapable() || !RoleOwner.RiskCapable() {
		t.Error("RISK_OPERATOR and OWNER must both be risk-capable")
	}
	if RoleTradingOp.RiskCapable() {
		t.Error("RoleTradingOp.RiskCapable() = true; a trading operator may not authorise risk")
	}
}

// A strategy reaching DEPLOYED can originate orders that create exposure, so
// every path into DEPLOYED must be dual-controlled.
func TestEveryPathIntoDeployedIsDualControlled(t *testing.T) {
	found := 0
	for _, r := range TransitionRules() {
		if r.To == StateDeployed {
			found++
			if !r.RequiresDualControl {
				t.Errorf("%s -> DEPLOYED does not require dual control", r.From)
			}
			if !r.RequiredRole.RiskCapable() {
				t.Errorf("%s -> DEPLOYED is performed by %s, which is not risk-capable",
					r.From, r.RequiredRole)
			}
		}
	}
	if found == 0 {
		t.Fatal("no transition leads to DEPLOYED")
	}
}

// Promotion is a one-way ratchet. Doc 04 lists the lifecycle as a sequence, and
// a strategy that can go from SHADOW back to PAPER would let an unvalidated
// strategy re-enter a governed environment.
func TestLifecycleIsAOneWayRatchet(t *testing.T) {
	rank := map[State]int{
		StateDraft: 0, StateReview: 1, StateBacktested: 2, StateSimulation: 3,
		StatePaper: 4, StateShadow: 5, StateApproved: 6, StateDeployed: 7,
	}
	for _, r := range TransitionRules() {
		fromRank, known := rank[r.From]
		if !known {
			continue // PAUSED and RETIRED are off the ratchet
		}
		toRank, known := rank[r.To]
		if !known {
			continue
		}
		if toRank <= fromRank {
			t.Errorf("%s -> %s moves backwards along the lifecycle; "+
				"promotion must be a one-way ratchet", r.From, r.To)
		}
	}
}

func TestEveryNonTerminalStateHasALegalTarget(t *testing.T) {
	for _, s := range States() {
		targets := LegalTargets(s)
		if s.Terminal() {
			if len(targets) != 0 {
				t.Errorf("terminal state %s has outgoing transitions: %v", s, targets)
			}
			continue
		}
		if len(targets) == 0 {
			t.Errorf("non-terminal state %s has no legal target; it is a dead end", s)
		}
	}
}

func TestEveryStateIsReachableFromDraft(t *testing.T) {
	reachable := map[State]bool{}
	queue := []State{StateDraft}
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
			t.Errorf("state %s is unreachable from DRAFT; it is unreachable decoration", s)
		}
	}
}

func TestTransitionRulesReturnsACopy(t *testing.T) {
	first := TransitionRules()
	original := TransitionRules()
	first[0] = TransitionRule{From: StateRetired, To: StateDraft, RequiredRole: RoleOwner}
	for i := range TransitionRules() {
		if TransitionRules()[i] != original[i] {
			t.Fatalf("mutating the returned slice changed the shared table at index %d", i)
		}
	}
}
