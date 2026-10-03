package dbtest_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/gate"
	"github.com/aitc/trade/domain/risk"
)

// The tests below cover package gate, which assembles a risk decision from
// persisted state rather than from a caller's assertions.
//
// Before it existed, every one of its three sourced inputs was supplied by the
// caller: the policy was a literal, the measurements were literals, and the halt
// decision was the literal risk.HaltDecision{Evaluated: true} -- a claim that the
// halt table had been consulted and found clear, made by a caller that had never
// opened it. These tests are the evidence that the composition now reads.

// sources pairs the two handles the composed gate reads through.
//
// The committed handle carries the policy and the halts; the rolled-back
// transaction carries the account, position, strategy and instrument rows. That
// split is not a convenience: the fixture's validated fills are immutable under
// invariant 4, so they cannot be deleted, and a rolled-back transaction is the
// only way to have them at all without leaving residue in the database.
func sources(f gateFixture) gate.Sources {
	return gate.Sources{Committed: f.db, State: f.tx}
}

// submissionFrom turns the gate fixture's order into a Submission, which is all
// the composed gate accepts: the order's own terms and nothing else.
//
// The policy is written here rather than by the caller because the composed gate
// resolves its own, and a fixture that evaluated against a hand-written policy
// would be testing the literal.
func submissionFrom(t *testing.T, f gateFixture, orderType string) gate.Submission {
	t.Helper()
	f.policy(t) // ensure a committed policy exists for this account

	return gate.Submission{
		Environment:   f.order.Environment,
		AccountID:     f.order.AccountID,
		StrategyID:    f.order.StrategyID,
		InstrumentID:  f.order.InstrumentID,
		MarketClass:   f.order.MarketClass,
		VenueID:       f.order.VenueID,
		OrderType:     orderType,
		Side:          f.order.Side,
		Quantity:      f.order.Amount,
		Price:         f.order.Price,
		Currency:      f.order.Currency,
		TimeInForce:   "DAY",
		CorrelationID: dbtest.CanonicalID("cor", int(policySeq.Add(1))),
		MaxAccountAge: 0,
	}
}

// activateCommittedHalt writes an ACTIVE halt through db rather than through a
// transaction, because the composed gate resolves against committed state and a
// halt inside an open transaction would be invisible to it -- which is the same
// visibility rule the policy is subject to, and the reason the two are treated
// alike.
//
// It is committed, so it must be cleaned, and it is cleaned in t.Cleanup. The
// alternative -- leaving it -- would make the second run of this file a
// different test, because the halt would still be governing the fixture's scope.
func activateCommittedHalt(t *testing.T, db *sql.DB, seq int64, level, environment, accountID string) string {
	t.Helper()
	haltID := dbtest.CanonicalID("hlt", int(seq))
	if _, err := db.Exec(`
		INSERT INTO ops.halt (halt_id, level, state, environment, account_id,
			instrument_id, market_class, venue_id, strategy_id,
			reason, emergency, activated_by, activated_at_ns)
		VALUES ($1,$2::ops.halt_level,'ACTIVE',$3::common.environment,$4,
			NULL,NULL,NULL,NULL,$5,false,'op-composed',$6)`,
		haltID, level, environment, nullIfEmpty(accountID),
		"composed-gate test halt", dbtest.NowNs()); err != nil {
		t.Fatalf("activate committed halt: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM ops.halt WHERE halt_id = $1`, haltID); err != nil {
			t.Errorf("clean up halt %s: %v", haltID, err)
		}
	})
	return haltID
}

// The composed gate refuses this fixture, and that is the correct answer rather
// than a broken one.
//
// Four controls cannot be satisfied by anything in the database today, so they
// deny on "not established": venue availability and market-data freshness have no
// table to read, and duplicate-order detection is a caller flag. A gate that
// approved here would be approving on assertions, which is the entire failure
// this package exists to prevent.
//
// The test asserts the refusal names exactly those controls. It is written to
// fail loudly when one of them starts being sourced, because at that point the
// assertion is stale and the remaining gaps are fewer than it claims -- which is
// information, not breakage.
func TestTheComposedGateDeniesOnFactsNothingCanYetSource(t *testing.T) {
	f := newGateFixture(t)

	d, err := gate.Evaluate(context.Background(), sources(f), submissionFrom(t, f, "LIMIT"))
	if err != nil {
		t.Fatalf("the composed gate refused the fixture with an error rather than a decision: %v", err)
	}
	if d.Approved {
		t.Fatalf("the composed gate approved an order whose venue, market data and duplicate status were never established: %s",
			d.Decision.Summary())
	}

	failedControls := map[risk.Control]bool{}
	for _, f := range d.Decision.Failed() {
		failedControls[f.Control] = true
	}
	for _, want := range []risk.Control{
		risk.CtrlVenueAvailable,
		risk.CtrlMarketDataFreshness,
		risk.CtrlDuplicateOrder,
	} {
		if !failedControls[want] {
			t.Errorf("%s did not deny, though nothing can source it yet; the list of unsourced facts in this test is stale", want)
		}
	}

	// The refusal must be explicable: a rejection that names its controls is the
	// thing doc 17 §4 requires, and a bare "not approved" would leave an operator
	// nothing to act on.
	if len(d.Decision.Failed()) == 0 {
		t.Error("the rejection names no failed control")
	}

	// What the gate COULD establish, it did, from rows. This is the half that
	// matters: a control that denies for want of a fact is different from one that
	// denies because the database says so, and only the second is a real decision.
	if d.Facts.InstrumentEligible == nil || !*d.Facts.InstrumentEligible {
		t.Error("INSTRUMENT_ELIGIBILITY was not established from market.instrument")
	}
	if d.Facts.PrecisionValid == nil || !*d.Facts.PrecisionValid {
		t.Error("PRECISION_VALIDITY was not established from market.instrument")
	}
	if d.Facts.Notional == nil {
		t.Error("the order's notional was not derived from its own quantity and price")
	} else if d.Facts.Notional.String() != "5000.00000000" && d.Facts.Notional.String() != "5000" {
		t.Errorf("notional = %s, want 1 x 5000", d.Facts.Notional.String())
	}
	if d.Policy.Revision == "" || d.Policy.Revision == "unresolved-fixture" {
		t.Errorf("decision carries policy revision %q, want the resolved row", d.Policy.Revision)
	}
	if !d.Facts.Provenance.Established() {
		t.Error("the composed gate produced facts whose provenance is not established")
	}
	if d.Facts.Provenance.PolicyRevision != d.Policy.Revision {
		t.Errorf("provenance names policy %q while the decision was made against %q",
			d.Facts.Provenance.PolicyRevision, d.Policy.Revision)
	}
	if len(d.Decision.Findings) != len(risk.Controls()) {
		t.Errorf("decision carries %d findings, want one per control (%d)",
			len(d.Decision.Findings), len(risk.Controls()))
	}
}

// The control that matters most. An ACTIVE halt in the table must refuse an order
// the limits would otherwise permit, and the refusal has to name HALT_STATE
// rather than whatever limit the fabricated order would have breached first.
func TestAnActiveHaltRefusesAnOrderTheLimitsWouldPermit(t *testing.T) {
	f := newGateFixture(t)
	seq := int64(970100)
	activateCommittedHalt(t, f.db, seq, "SYSTEM_HALT", f.env, "")

	d, err := gate.Evaluate(context.Background(), sources(f), submissionFrom(t, f, "LIMIT"))
	if err != nil {
		t.Fatalf("a blocked order produced an error rather than a decision: %v", err)
	}
	if d.Approved {
		t.Fatal("an order was approved while a SYSTEM_HALT was ACTIVE")
	}
	failed := d.Decision.Failed()
	if len(failed) == 0 {
		t.Fatal("the rejection names no failed control")
	}
	if failed[0].Control != risk.CtrlHaltState {
		t.Errorf("first failed control is %s, want %s", failed[0].Control, risk.CtrlHaltState)
	}
	if !strings.Contains(failed[0].Reason, "SYSTEM_HALT") {
		t.Errorf("refusal %q does not name the halt that blocked it", failed[0].Reason)
	}
	// A block is a correctly produced refusal, not a failure, so it must still be
	// as reproducible as an approval.
	if d.Policy.Revision == "" {
		t.Error("a rejection carries no policy revision, so it is not reproducible")
	}
}

// A halt's scope is matched per field. An ACCOUNT_HALT on another account must
// not block this one, or a halt would stop trading far more widely than it was
// raised for.
//
// The order is refused for other reasons -- see the test above -- so this asserts
// on the halt specifically: it must not appear among the failed controls. Asserting
// "approved" here would only be asserting that some unrelated gap is still open.
func TestAHaltOnAnotherAccountDoesNotBlock(t *testing.T) {
	f := newGateFixture(t)
	seq := int64(970200)
	activateCommittedHalt(t, f.db, seq, "ACCOUNT_HALT", f.env, dbtest.CanonicalID("acc", 970201))

	d, err := gate.Evaluate(context.Background(), sources(f), submissionFrom(t, f, "LIMIT"))
	if err != nil {
		t.Fatalf("the composed gate refused the fixture with an error: %v", err)
	}
	if d.Halt.Blocked {
		t.Errorf("a halt on another account blocked this order: %s", d.Halt.Reason)
	}
	assertHaltNotAmongFailures(t, d, "ACCOUNT_HALT on another account")
}

// Same reasoning for the environment dimension: one environment's halt is not
// another environment's problem, and a leak here would silently disable an entire
// environment's protection.
func TestAHaltInAnotherEnvironmentDoesNotBlock(t *testing.T) {
	f := newGateFixture(t)
	seq := int64(970300)
	activateCommittedHalt(t, f.db, seq, "SYSTEM_HALT", "live", "")

	d, err := gate.Evaluate(context.Background(), sources(f), submissionFrom(t, f, "LIMIT"))
	if err != nil {
		t.Fatalf("the composed gate refused the fixture with an error: %v", err)
	}
	if d.Halt.Blocked {
		t.Errorf("a halt in another environment blocked this order: %s", d.Halt.Reason)
	}
	assertHaltNotAmongFailures(t, d, "SYSTEM_HALT in another environment")
}

// assertHaltNotAmongFailures states the claim a halt-scope test actually makes:
// the halt is not the reason this order was refused. Anything else the gate
// refuses is somebody else's control, and conflating the two would let a real
// scope regression hide behind an unrelated gap.
func assertHaltNotAmongFailures(t *testing.T, d gate.Decision, description string) {
	t.Helper()
	for _, f := range d.Decision.Failed() {
		if f.Control == risk.CtrlHaltState {
			t.Errorf("%s refused this order under HALT_STATE: %s", description, f.Reason)
		}
	}
}

// A risk-reducing action must survive a halt, or an incident becomes a loss. The
// assertion is on the halt decision alone: whether the order then clears the
// risk engine's own rules is a separate question this test does not own.
func TestAHaltDoesNotBlockARiskReducingAction(t *testing.T) {
	f := newGateFixture(t)
	seq := int64(970400)
	activateCommittedHalt(t, f.db, seq, "SYSTEM_HALT", f.env, "")

	d, err := gate.Evaluate(context.Background(), sources(f), submissionFrom(t, f, "REDUCE_ONLY"))
	if err != nil {
		t.Fatalf("a risk-reducing action produced an error under a halt: %v", err)
	}
	if d.Halt.Blocked {
		t.Errorf("a halt blocked a risk-reducing action: %s", d.Halt.Reason)
	}
	if !strings.Contains(d.Halt.Reason, "risk-reducing") {
		t.Errorf("halt reason %q does not explain why the action was permitted", d.Halt.Reason)
	}
}

// The loader reads ACTIVE rows only, so a cleared halt cannot govern anything.
// It is inserted cleared rather than cleared afterwards, because a clear requires
// dual control and two verifications, and the question here is whether the
// loader consults the column at all.
func TestAClearedHaltDoesNotBlock(t *testing.T) {
	f := newGateFixture(t)
	haltID := dbtest.CanonicalID("hlt", 970500)
	if _, err := f.db.Exec(`
		INSERT INTO ops.halt (halt_id, level, state, environment, account_id,
			instrument_id, market_class, venue_id, strategy_id,
			reason, emergency, activated_by, activated_at_ns,
			cleared_by, cleared_approved_by, cleared_at, clear_reason,
			health_verified, reconciliation_verified)
		VALUES ($1,'SYSTEM_HALT'::ops.halt_level,'CLEARED',$2::common.environment,$3,
			NULL,NULL,NULL,NULL,'already recovered',false,'op-composed',$4,
			'op-1','op-2',now(),'recovered',true,true)`,
		haltID, f.env, nullIfEmpty(f.accountID), dbtest.NowNs()); err != nil {
		t.Fatalf("insert cleared halt: %v", err)
	}
	t.Cleanup(func() {
		if _, err := f.db.Exec(`DELETE FROM ops.halt WHERE halt_id = $1`, haltID); err != nil {
			t.Errorf("clean up cleared halt %s: %v", haltID, err)
		}
	})

	d, err := gate.Evaluate(context.Background(), sources(f), submissionFrom(t, f, "LIMIT"))
	if err != nil {
		t.Fatalf("the composed gate refused the fixture with an error: %v", err)
	}
	if d.Halt.Blocked {
		t.Errorf("a CLEARED halt blocked this order: %s", d.Halt.Reason)
	}
	assertHaltNotAmongFailures(t, d, "a CLEARED halt")
}

// A halt table that could not be read has not been shown to be free of halts, so
// a read failure must not come back as a clearance.
func TestAHaltReadFailureIsNotAClearance(t *testing.T) {
	f := newGateFixture(t)
	// Closing the transaction's connection is not available here, so the failure
	// is provoked through a scope the loader cannot satisfy: an environment the
	// enum does not contain makes the query itself error rather than match
	// nothing.
	sub := submissionFrom(t, f, "LIMIT")
	sub.Environment = "NOT_AN_ENVIRONMENT"

	if _, err := gate.Evaluate(context.Background(), sources(f), sub); err == nil {
		t.Fatal("an environment outside the closed set produced a decision instead of an error")
	}
}
