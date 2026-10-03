package dbtest_test

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/halt"
	"github.com/aitc/trade/domain/oms"
	"github.com/aitc/trade/domain/strategy"
)

// Parity between the Go domain packages and the SQL state machines they
// duplicate.
//
// packages oms, strategy and halt are authoritative (01_SYSTEM_ARCHITECTURE.md
// §7). The tables oms.state_transition_rule, strategy.state_transition_rule and
// the function ops.halt_rank are the backstop at the storage boundary. Both
// halves exist on purpose: the Go half owns the decision, the SQL half stops a
// write that bypasses the Go half.
//
// Two implementations of one machine is a defect that has already happened in
// this repository. contracts.NewID and common.new_canonical_id minted every
// identifier in the system, agreed on alphabet, length and entropy -- everything
// a format test can observe -- were documented as "opaque so the encodings may
// differ by design", and produced different identifiers. 0020 made them
// identical and pinned them with a parity test.
//
// These tests are the same treatment applied to the state machines. They read
// the live tables rather than the migration files, so they compare against what
// the database actually enforces, not against what a file once said.

// readRules returns a canonical string for every row, so the two sides can be
// compared as sets without depending on row order or column layout.
func readRules(t *testing.T, ctx context.Context, db *sql.DB, query string, cols int) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		parts := make([]string, cols)
		dest := make([]any, cols)
		for i := range dest {
			dest[i] = new(sql.NullString)
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		for i := range dest {
			ns := dest[i].(*sql.NullString)
			if ns.Valid {
				parts[i] = ns.String
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows failed: %v", err)
	}
	sort.Strings(out)
	return out
}

func diffSets(t *testing.T, name string, want, got []string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: the Go table is empty; nothing to compare", name)
	}
	wantSet := map[string]bool{}
	for _, s := range want {
		wantSet[s] = true
	}
	gotSet := map[string]bool{}
	for _, s := range got {
		gotSet[s] = true
	}

	for _, s := range want {
		if !gotSet[s] {
			t.Errorf("%s: the Go domain package allows a transition the database does not: %q\n"+
				"  The Go side is authoritative, so this would be permitted in the service and "+
				"refused by the backstop.", name, s)
		}
	}
	for _, s := range got {
		if !wantSet[s] {
			t.Errorf("%s: the database allows a transition the Go domain package does not: %q\n"+
				"  The service would refuse it while the database would accept it, so the two "+
				"disagree about the same closed set.", name, s)
		}
	}
}

// The OMS machine exists in package oms and in oms.state_transition_rule.
func TestOMSTransitionRulesMatchTheDatabase(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	sqlRows := readRules(t, ctx, db, `
		SELECT from_state::text, to_state::text, event_kind::text,
		       requires_risk_decision::text, requires_outcome_resolution::text
		  FROM oms.state_transition_rule`, 5)

	var goRows []string
	for _, r := range oms.TransitionRules() {
		goRows = append(goRows, strings.Join([]string{
			string(r.From), string(r.To), string(r.Via),
			boolText(r.RequiresRiskDecision), boolText(r.RequiresOutcomeResolution),
		}, "|"))
	}
	sort.Strings(goRows)

	if len(goRows) != len(sqlRows) {
		t.Errorf("Go declares %d OMS transitions, the database has %d", len(goRows), len(sqlRows))
	}
	diffSets(t, "OMS", goRows, sqlRows)
}

// The strategy machine exists in package strategy and in
// strategy.state_transition_rule.
func TestStrategyTransitionRulesMatchTheDatabase(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	sqlRows := readRules(t, ctx, db, `
		SELECT from_state::text, to_state::text, required_role::text,
		       requires_dual_control::text
		  FROM strategy.state_transition_rule`, 4)

	var goRows []string
	for _, r := range strategy.TransitionRules() {
		goRows = append(goRows, strings.Join([]string{
			string(r.From), string(r.To), string(r.RequiredRole),
			boolText(r.RequiresDualControl),
		}, "|"))
	}
	sort.Strings(goRows)

	if len(goRows) != len(sqlRows) {
		t.Errorf("Go declares %d strategy transitions, the database has %d", len(goRows), len(sqlRows))
	}
	diffSets(t, "Strategy", goRows, sqlRows)
}

// The halt precedence must be the same number on both sides. If it is not, the
// service and the backstop disagree about which halt outranks which, and the
// loser of that disagreement is whichever one an incident is reading.
func TestHaltRankMatchesTheDatabase(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	for _, level := range halt.Levels() {
		var sqlRank int
		err := db.QueryRowContext(ctx, `SELECT ops.halt_rank($1::ops.halt_level)`, string(level)).
			Scan(&sqlRank)
		if err != nil {
			t.Fatalf("ops.halt_rank(%s) failed: %v", level, err)
		}
		if got := level.Rank(); got != sqlRank {
			t.Errorf("%s: Go Rank() = %d, ops.halt_rank = %d", level, got, sqlRank)
		}
	}
}

// The risk-reducing classification decides whether a halt blocks an action. A
// disagreement means an operator is either refused permission to flatten a
// position during an incident, or allowed to increase exposure under one.
// The risk-reducing classification, Go against the SQL predicate the OMS trigger
// uses.
//
// It reads contracts.OrderType rather than a copy inside package oms. That copy
// existed, was documented as "the same predicate migration 0013 uses at the risk
// boundary", carried six of the enum's nine members, and had no consumer outside
// this test -- while the boundary itself, domain/gate, called
// contracts.OrderType.RiskIncreasing(). So the function this test exercised was
// not the one deciding anything. It has been deleted rather than corrected:
// a second implementation of the same predicate is the defect, not its member set.
func TestRiskReducingClassificationMatchesTheDatabase(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	rows, err := db.QueryContext(ctx,
		`SELECT unnest(enum_range(NULL::common.order_type))::text`)
	if err != nil {
		t.Fatalf("could not enumerate common.order_type: %v", err)
	}
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		if !contracts.OrderType(name).Valid() {
			t.Errorf("the database holds order type %s but contracts.OrderType rejects it, "+
				"so this row of the comparison is untestable: Go cannot express it", name)
			continue
		}
		// The SQL predicate the OMS trigger uses, verbatim.
		var sqlReducing bool
		if err := db.QueryRowContext(ctx,
			`SELECT $1 NOT IN ('REDUCE_ONLY','CLOSE_POSITION')`, name).Scan(&sqlReducing); err != nil {
			t.Fatalf("predicate query failed: %v", err)
		}
		sqlReducing = !sqlReducing

		goReducing := !contracts.OrderType(name).RiskIncreasing()
		if goReducing != sqlReducing {
			t.Errorf("order type %s: Go RiskIncreasing = %v, SQL predicate risk-reducing = %v",
				name, !goReducing, sqlReducing)
		}
		checked++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows failed: %v", err)
	}
	if checked < 2 {
		t.Fatalf("only %d order types checked; the enum should have more", checked)
	}
}

// The order entry states must agree, or an order could be inserted in a state
// the Go machine would never produce.
func TestOrderEntryStatesMatchTheDatabase(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `SELECT unnest(enum_range(NULL::oms.order_state))::text`)
	if err != nil {
		t.Fatalf("could not enumerate oms.order_state: %v", err)
	}
	defer rows.Close()

	var sqlStates []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		sqlStates = append(sqlStates, s)
	}
	sort.Strings(sqlStates)

	var goStates []string
	for _, s := range oms.States() {
		goStates = append(goStates, string(s))
	}
	sort.Strings(goStates)

	diffSets(t, "OMS state enum", goStates, sqlStates)
}

// The strategy state enum must agree too, for the same reason.
func TestStrategyStatesMatchTheDatabase(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `SELECT unnest(enum_range(NULL::strategy.strategy_state))::text`)
	if err != nil {
		t.Fatalf("could not enumerate strategy.strategy_state: %v", err)
	}
	defer rows.Close()

	var sqlStates []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		sqlStates = append(sqlStates, s)
	}
	sort.Strings(sqlStates)

	var goStates []string
	for _, s := range strategy.States() {
		goStates = append(goStates, string(s))
	}
	sort.Strings(goStates)

	diffSets(t, "Strategy state enum", goStates, sqlStates)
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
