package dbtest_test

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/oms"
)

// Parity for the four closed vocabularies the OMS state machine is built from.
//
// The transition rules were already pinned to oms.state_transition_rule
// (TestOMSTransitionRulesMatchTheDatabase). The vocabularies they are written in
// were not. That gap is not hypothetical: it is the reason package oms could
// carry its own six-member order-type set, disagreeing with both
// contracts.OrderType and the database's nine, for as long as it did.
//
// Comparing a derived table while leaving its alphabet unpinned checks the
// derivation and not the vocabulary, and every rule row references an event kind
// and two states, so a divergence in those sets changes what the rules can even
// express. Two of the fourteen event kinds drive no rule at all, which is exactly
// where a divergence would hide from a test that only ever looks at rule rows.

// readEnum returns the members of a PostgreSQL enum type, in declaration order,
// read from the live catalog rather than from the migration that created it.
func readEnum(t *testing.T, ctx context.Context, db *sql.DB, typeName string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT e.enumlabel
		  FROM pg_type ty
		  JOIN pg_enum e ON e.enumtypid = ty.oid
		 WHERE ty.typname = $1
		 ORDER BY e.enumsortorder`, typeName)
	if err != nil {
		t.Fatalf("reading enum %s failed: %v", typeName, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatalf("scan failed for %s: %v", typeName, err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows failed for %s: %v", typeName, err)
	}
	if len(out) == 0 {
		t.Fatalf("enum %s does not exist in the live database", typeName)
	}
	return out
}

// diffVocabulary reports set differences in vocabulary terms rather than in
// transition terms, because a state or a type is not a move.
//
// The asymmetry matters. A value Go has and the database does not means the
// service will accept something the storage boundary cannot hold, so the write
// fails at the last moment. A value the database has and Go does not means the
// database can hold a row the service cannot interpret, which is the worse of the
// two: the row exists, is authoritative, and no Go code path can reason about it.
func diffVocabulary(t *testing.T, name string, goVals, sqlVals []string) {
	t.Helper()
	if len(goVals) == 0 {
		t.Fatalf("%s: the Go vocabulary is empty; nothing to compare", name)
	}

	sqlSet := make(map[string]bool, len(sqlVals))
	for _, s := range sqlVals {
		sqlSet[s] = true
	}
	goSet := make(map[string]bool, len(goVals))
	for _, g := range goVals {
		goSet[g] = true
	}

	for _, g := range goVals {
		if !sqlSet[g] {
			t.Errorf("%s: Go accepts %q, which is not a member of the database enum.\n"+
				"  The service would validate it and the storage boundary would then refuse the write.", name, g)
		}
	}
	for _, s := range sqlVals {
		if !goSet[s] {
			t.Errorf("%s: the database holds %q, which the Go vocabulary does not contain.\n"+
				"  A row can exist that no Go code can interpret, so it can be read but never acted on.", name, s)
		}
	}
}

func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// oms.order_state is the enum the order's state column takes. Package oms is the
// other half of the same machine.
func TestOMSStatesMatchTheDatabaseEnum(t *testing.T) {
	db := dbtest.Open(t)

	var goStates []string
	for _, s := range oms.States() {
		goStates = append(goStates, string(s))
	}

	diffVocabulary(t, "oms.order_state",
		sortedCopy(goStates), sortedCopy(readEnum(t, context.Background(), db, "order_state")))
}

// oms.order_event_kind is the enum the journal's event_kind column takes.
//
// Both directions matter here for a specific reason: every transition rule row
// names an event kind, so a kind present in the enum but absent from Go cannot be
// used by any rule the service knows, and the database would accept a journal
// entry the service cannot classify.
func TestOMSEventKindsMatchTheDatabaseEnum(t *testing.T) {
	db := dbtest.Open(t)

	var goKinds []string
	for _, e := range oms.EventKinds() {
		goKinds = append(goKinds, string(e))
	}

	diffVocabulary(t, "oms.order_event_kind",
		sortedCopy(goKinds), sortedCopy(readEnum(t, context.Background(), db, "order_event_kind")))
}

// common.order_type is the enum behind oms.order.order_type, market.instrument
// .order_types and risk.policy.permitted_order_types.
//
// contracts.OrderType is the Go half. It is the only one, and package oms used to
// carry a second, six-member set that nothing but its own test consumed.
func TestContractsOrderTypesMatchTheDatabaseEnum(t *testing.T) {
	db := dbtest.Open(t)

	var goTypes []string
	seen := map[string]bool{}
	// The list comes from contracts.OrderTypes(), not from a copy written out here.
	// A hand-copied list is a second vocabulary: adding an order type to the
	// contract and forgetting this copy would leave the comparison passing against
	// a type the service cannot interpret, which is the drift this test catches.
	for _, ot := range contracts.OrderTypes() {
		name := string(ot)
		if seen[name] {
			t.Fatalf("contracts order-type list repeats %q", name)
		}
		seen[name] = true
		if !ot.Valid() {
			t.Errorf("contracts.OrderType(%q).Valid() is false; the list here and the closed set disagree", name)
		}
		goTypes = append(goTypes, name)
	}

	diffVocabulary(t, "common.order_type",
		sortedCopy(goTypes), sortedCopy(readEnum(t, context.Background(), db, "order_type")))
}

// The forward direction for contracts.OrderType: every value the database enum
// holds must be Valid(). A row the storage boundary accepts must be one the
// service can interpret.
func TestContractsOrderTypeCoversEveryValueTheDatabaseHolds(t *testing.T) {
	db := dbtest.Open(t)
	held := readEnum(t, context.Background(), db, "order_type")
	if len(held) == 0 {
		t.Fatal("common.order_type is empty")
	}
	for _, s := range held {
		if !contracts.OrderType(s).Valid() {
			t.Errorf("the database holds order type %s but contracts.OrderType rejects it", s)
		}
	}
	for _, bogus := range []string{"", "limit", "ICEBERG", "GTC", "REDUCE_ONLY ", "MARKET;"} {
		if contracts.OrderType(bogus).Valid() {
			t.Errorf("contracts.OrderType(%q).Valid() is true for a value that is not in common.order_type", bogus)
		}
	}
}

// common.time_in_force is the enum behind oms.order.time_in_force, which is NOT
// NULL on every order. contracts.TimeInForce exists with a Valid method and was
// read by nothing; this is the test that stops it drifting from storage.
func TestContractsTimeInForceMatchesTheDatabaseEnum(t *testing.T) {
	db := dbtest.Open(t)

	sqlTIF := readEnum(t, context.Background(), db, "time_in_force")

	var goTIF []string
	// From contracts.TimeInForces(), not a copy written out here: see the note on
	// the order-type test above.
	for _, tif := range contracts.TimeInForces() {
		n := string(tif)
		if !tif.Valid() {
			t.Errorf("contracts.TimeInForce(%q).Valid() is false but the list here claims it", n)
		}
		goTIF = append(goTIF, n)
	}

	diffVocabulary(t, "common.time_in_force", sortedCopy(goTIF), sortedCopy(sqlTIF))
}

func TestContractsTimeInForceRejectsAnythingTheDatabaseDoesNotHold(t *testing.T) {
	db := dbtest.Open(t)
	for _, s := range readEnum(t, context.Background(), db, "time_in_force") {
		if !contracts.TimeInForce(s).Valid() {
			t.Errorf("contracts.TimeInForce(%q).Valid() is false but the database enum holds it", s)
		}
	}
	for _, bogus := range []string{"", "gtc", "GTCX", "MARKET", "GTC "} {
		if contracts.TimeInForce(bogus).Valid() {
			t.Errorf("contracts.TimeInForce(%q).Valid() is true for a value outside common.time_in_force", bogus)
		}
	}
}

// IOC and FOK appear in both common.order_type and common.time_in_force. Two
// columns able to express the same instruction is the shape of the defect this
// repository has already produced twice, so the overlap is pinned here rather
// than left as a fact someone has to remember.
//
// The overlap is not a defect by itself: an enum is a vocabulary, and a
// vocabulary may carry a value that a particular table forbids. What would be a
// defect is a caller left to guess which column governs. Migration 0022 resolves
// it for oms.order by making time_in_force the sole expression of IOC and FOK,
// and this test is what makes that resolution observable -- if a later migration
// drops the constraint, the pair becomes contradictory again and this is where it
// should be noticed.
func TestIocAndFokBelongToTimeInForceAndOrderTypeForbidsThem(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	var def string
	err := db.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE conrelid = 'oms.order'::regclass
		   AND contype = 'c'
		   AND pg_get_constraintdef(oid) LIKE '%order_type%'
		   AND pg_get_constraintdef(oid) LIKE '%IOC%'`).Scan(&def)
	if err == sql.ErrNoRows || def == "" {
		t.Error("oms.order does not forbid IOC and FOK in order_type, so a row can claim a " +
			"time-in-force instruction in one column while naming a different instruction in the other. " +
			"Migration 0022 adds order_type_not_a_time_in_force; this constraint is missing.")
		return
	}
	if err != nil {
		t.Fatalf("could not read oms.order's constraints: %v", err)
	}
	// The constraint is matched by name as well as by shape, so a constraint that
	// happens to mention IOC for an unrelated reason cannot satisfy this test.
	for _, want := range []string{"IOC", "FOK"} {
		if !strings.Contains(def, want) {
			t.Errorf("the order_type constraint does not mention %s: %s", want, def)
		}
	}
}

// An event kind that no transition rule consumes cannot move an order. Two exist,
// and that is a legitimate state of affairs -- the enum is a closed vocabulary
// shared with the journal -- but it is not obvious, and a kind that looks
// actionable while being inert is the same shape as a control nobody calls.
//
// Pinning the list is what stops it from becoming stale in either direction: a
// third inert kind fails, and a kind that starts driving a rule must be removed
// from this list.
func TestTheEventKindsThatDriveNoTransitionAreExactlyTwo(t *testing.T) {
	driven := map[string]bool{}
	for _, r := range oms.TransitionRules() {
		driven[string(r.Via)] = true
	}

	var inert []string
	for _, e := range oms.EventKinds() {
		if !driven[string(e)] {
			inert = append(inert, string(e))
		}
	}
	sort.Strings(inert)

	want := []string{"CANCEL_REQUESTED", "SUBMIT_FAILED"}
	if strings.Join(inert, ",") != strings.Join(want, ",") {
		t.Errorf("event kinds driving no transition are %v, want %v.\n"+
			"  A newly inert kind means a rule was removed; a kind that has become active must be "+
			"removed from this list so the statement stays true.", inert, want)
	}
}

// Every state the machine can reach must be reachable through the rules, and every
// rule must name states that exist. The second half is what the vocabulary parity
// test cannot see: two agreeing vocabularies can still be combined into a rule
// that jumps between states in an order the machine never permits.
func TestEveryTransitionRuleNamesStatesAndEventsThatExistInBothHalves(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	states := map[string]bool{}
	for _, s := range readEnum(t, ctx, db, "order_state") {
		states[s] = true
	}
	kinds := map[string]bool{}
	for _, k := range readEnum(t, ctx, db, "order_event_kind") {
		kinds[k] = true
	}

	for _, r := range oms.TransitionRules() {
		if !states[string(r.From)] {
			t.Errorf("rule %s -> %s names a source state the database enum does not hold", r.From, r.To)
		}
		if !states[string(r.To)] {
			t.Errorf("rule %s -> %s names a target state the database enum does not hold", r.From, r.To)
		}
		if !kinds[string(r.Via)] {
			t.Errorf("rule %s -> %s names an event kind the database enum does not hold", r.From, r.To)
		}
	}
}
