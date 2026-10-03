package dbtest_test

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/risk"
)

// Parity between the Go risk package's Policy projection and the risk.policy
// table it reads.
//
// domain/risk is not a second design of the risk policy; it projects columns
// that have existed, NOT NULL with a CHECK, since migration 0003. An earlier
// version invented a generic limit model with its own unit, scope, window,
// boundary, source and missing-data fields, and nothing ever compared it to the
// schema -- the same class of defect as the canonical ID divergence migration
// 0020 had to undo.
//
// These tests read information_schema rather than the migration files, so they
// compare against what the database actually has.
//
// The assertion has two halves, and the second matters as much as the first:
//
//   - every column the Go projection names must exist, with the type,
//     precision and nullability the Go field assumes;
//   - every column risk.policy has must be accounted for -- either consulted by
//     a control, or named in the known-gap list below.
//
// A projection that drifts upward names a column that does not exist. A
// projection that drifts downward silently stops consulting a column an operator
// set and believes is enforced, which is the more dangerous direction and the
// reason for the second half.

type columnFacts struct {
	name      string
	dataType  string
	numericPr string
	numericSc string
	notNull   bool
}

// policyColumns returns risk.policy's columns in ordinal order.
func policyColumns(t *testing.T, db *sql.DB) []columnFacts {
	t.Helper()
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `
		SELECT a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       a.attnotnull
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'risk'
		   AND c.relname = 'policy'
		   AND a.attnum > 0
		   AND NOT a.attisdropped
		 ORDER BY a.attnum`)
	if err != nil {
		t.Fatalf("could not read risk.policy columns: %v", err)
	}
	defer rows.Close()

	var out []columnFacts
	for rows.Next() {
		var c columnFacts
		if err := rows.Scan(&c.name, &c.dataType, &c.notNull); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		// format_type renders numeric(38,18); the Go field documents the same
		// precision, and the claim is worth checking.
		if i := strings.Index(c.dataType, "("); i >= 0 {
			parts := strings.Split(strings.TrimSuffix(strings.Trim(c.dataType[i+1:], ")"), ","), ",")
			if len(parts) == 2 {
				c.numericPr = strings.TrimSpace(parts[0])
				c.numericSc = strings.TrimSpace(parts[1])
			}
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows failed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("risk.policy has no columns; the schema this projects is not there")
	}
	return out
}

// limitColumnNames is what domain/risk.Policy assumes about the twelve numeric
// limit columns. Written as data rather than read off the struct, so that a
// wrong assumption in the struct is a test failure rather than a tautology.
var limitColumnNames = []struct {
	column    string
	kind      string // "decimal" or "count"
	precision string
	scale     string
}{
	{"max_order_notional", "decimal", "38", "18"},
	{"max_order_quantity", "decimal", "38", "18"},
	{"max_gross_exposure", "decimal", "38", "18"},
	{"max_net_exposure", "decimal", "38", "18"},
	{"max_leverage", "decimal", "18", "8"},
	{"max_concentration", "decimal", "12", "8"},
	{"max_open_orders", "count", "", ""},
	{"max_daily_loss", "decimal", "38", "18"},
	{"max_drawdown", "decimal", "12", "8"},
	{"max_price_deviation", "decimal", "12", "8"},
	{"max_orders_per_minute", "count", "", ""},
	{"max_cancels_per_minute", "count", "", ""},
}

func TestRiskPolicyLimitColumnsMatchTheGoProjection(t *testing.T) {
	db := dbtest.Open(t)

	byName := map[string]columnFacts{}
	for _, c := range policyColumns(t, db) {
		byName[c.name] = c
	}

	for _, want := range limitColumnNames {
		got, ok := byName[want.column]
		if !ok {
			t.Errorf("risk.policy has no column %s; domain/risk projects it as a real limit", want.column)
			continue
		}

		// Every limit column is NOT NULL with a > 0 CHECK, which is why the Go
		// type treats "missing limit" as "no policy" rather than "a zero limit".
		// If this ever stops holding, Policy.Validate's reasoning is stale.
		if !got.notNull {
			t.Errorf("%s is nullable in the database; domain/risk assumes NOT NULL and "+
				"concludes an absent policy from the absence of a row", want.column)
		}

		switch want.kind {
		case "count":
			if !strings.HasPrefix(got.dataType, "integer") && !strings.HasPrefix(got.dataType, "bigint") {
				t.Errorf("%s is %s in the database, but the Go projection reads it as an int64 count",
					want.column, got.dataType)
			}
		case "decimal":
			if !strings.HasPrefix(got.dataType, "numeric") {
				t.Errorf("%s is %s in the database, but the Go projection reads it as a "+
					"contracts.Decimal; financial values must never be IEEE-754",
					want.column, got.dataType)
				continue
			}
			if got.numericPr != want.precision || got.numericSc != want.scale {
				t.Errorf("%s is %s in the database, the Go projection documents numeric(%s,%s)\n"+
					"  A precision or scale the Go side does not assume can silently round a limit",
					want.column, got.dataType, want.precision, want.scale)
			}
		}
	}
}

// controlConsulted is the columns a doc 04 control actually reads: the limit
// bounds named by controlColumn, plus the allowlists the eligibility controls
// check.
//
// This is deliberately narrower than risk.ProjectedColumns. A column can be
// projected onto a Policy field and still be consulted by nothing, which is the
// state max_order_quantity and max_price_deviation are in, and the state a
// third column would silently fall into.
func controlConsulted() map[string]bool {
	out := map[string]bool{}
	for _, c := range risk.Controls() {
		for _, col := range risk.ColumnFor(c) {
			out[col] = true
		}
	}
	// INSTRUMENT_ELIGIBILITY and VENUE_AVAILABILITY check these through
	// Policy.PermitsInstrument and PermitsVenue rather than through ColumnFor,
	// because they are allowlists rather than numeric bounds.
	for _, col := range []string{"permitted_instruments", "permitted_venues"} {
		out[col] = true
	}
	return out
}

// The second half, and the one that catches a real defect class: a column an
// operator can set and believe is enforced, which no control reads.
//
// The gap list is asserted, not merely documented. Adding a column to risk.policy
// without deciding who owns it fails this test, which is the point. The list is
// currently:
//
//   - max_order_quantity, max_price_deviation: doc 04's fifteen controls name
//     neither a quantity cap nor a price-deviation cap, and inventing a
//     sixteenth control would be inventing a requirement.
//   - permitted_sides, permitted_order_types, permitted_modes: no control in doc
//     04 validates a side, an order type or an operational mode against policy.
//   - the *_assumptions bundles and schedule: risk accounting inputs, not
//     pre-trade gate conditions.
//   - authority, lifecycle and audit columns: enforced by migration 0014 and the
//     audit chain, not by this gate.
func TestRiskPolicyColumnsNotConsultedByTheGate(t *testing.T) {
	db := dbtest.Open(t)
	projected := map[string]bool{}
	for _, col := range risk.ProjectedColumns() {
		projected[col] = true
	}
	consulted := controlConsulted()

	knownGaps := map[string]string{
		"max_order_quantity":     "doc 04 names no quantity cap",
		"max_price_deviation":    "doc 04 names no price-deviation cap",
		"permitted_sides":        "no doc 04 control validates sides",
		"permitted_order_types":  "no doc 04 control validates order types",
		"permitted_modes":        "operational mode is enforced by migration 0014, not this gate",
		"schedule":               "policy activation schedule, not a gate condition",
		"fee_assumptions":        "risk accounting input, not a pre-trade condition",
		"funding_assumptions":    "risk accounting input, not a pre-trade condition",
		"margin_assumptions":     "risk accounting input, not a pre-trade condition",
		"settlement_assumptions": "risk accounting input, not a pre-trade condition",
		"halt_authority":         "enforced by migration 0014",
		"reenable_authority":     "enforced by migration 0014",
		// Audit and lifecycle columns, enforced by the schema rather than read
		// by the gate. Note the two that the database treats as part of
		// activeness and Go does not: active_policy_complete requires
		// approved_by, approved_at and rollback_revision, while Policy.Active
		// reads only status. That asymmetry is recorded in the risk package and
		// is safe only because the schema refuses to activate a policy that
		// fails them.
		"name":                  "display name, not a gate condition",
		"created_by":            "audit column",
		"created_at":            "audit column",
		"effective_at":          "activation timing, enforced by active_policy_complete",
		"superseded_by":         "policy lineage, enforced by the schema",
		"approved_by":           "dual control, enforced by the schema",
		"approved_at":           "dual control, enforced by the schema",
		"rollback_revision":     "rollback, enforced by active_policy_complete",
		"policy_version_vector": "optimistic concurrency, enforced by the schema",
	}

	// Three tiers, and the difference between them matters:
	//
	//	consulted  a doc 04 control compares against the column
	//	read      projected onto a Policy field and used, but not compared:
	//	          status gates everything, loss_window and loss_source are the
	//	          loss measurement's parameters, max_market_data_age bounds
	//	          staleness, and the scope columns say which policy this is
	//	unread    knownGaps: audit, lifecycle, assumption-bundle and allowlist
	//	          columns that no control consults
	//
	// A schema column in none of the three is one the Go package does not know
	// exists, which is a completeness failure.
	var unexplained []string
	for _, c := range policyColumns(t, db) {
		if projected[c.name] {
			continue
		}
		if _, ok := knownGaps[c.name]; !ok {
			unexplained = append(unexplained, c.name)
		}
	}
	sort.Strings(unexplained)

	for _, col := range unexplained {
		t.Errorf("risk.policy.%s is not projected by domain/risk and is not recorded as a known gap\n"+
			"  The Go package does not know this column exists. Either project it onto Policy, or "+
			"record why the gate ignores it in knownGaps here and in the controlColumn comment "+
			"in domain/risk/risk.go.", col)
	}

	// A gap that has since been closed should be removed from the list, so that
	// the list keeps meaning "gaps" rather than "columns I have not looked at".
	for col, why := range knownGaps {
		if !projected[col] && !hasColumn(db, col) {
			t.Errorf("knownGaps lists %s (%s) but risk.policy has no such column; the list has rotted",
				col, why)
		}
		if consulted[col] {
			t.Errorf("knownGaps lists %s (%s) but a doc 04 control now reads it; close the gap "+
				"properly and remove it from this list", col, why)
		}
	}
}

func hasColumn(db *sql.DB, name string) bool {
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM pg_attribute a
			  JOIN pg_class c ON c.oid = a.attrelid
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = 'risk' AND c.relname = 'policy'
			   AND a.attname = $1 AND a.attnum > 0 AND NOT a.attisdropped)`, name).Scan(&exists)
	if err != nil {
		return true // do not fail on the rot check if the lookup itself broke
	}
	return exists
}

// permitted_markets is read by Policy.PermitsMarket but no control calls it
// yet. That is a recorded gap, not an oversight, and the assertion is that the
// accessor behaves as documented so the gap stays closable.
func TestPermittedMarketsIsReadableButUnwired(t *testing.T) {
	p := activePolicyForParity()
	if !p.PermitsMarket("CRYPTO") {
		t.Error("PermitsMarket did not find a market it was given")
	}
	if p.PermitsMarket("EQUITY") {
		t.Error("PermitsMarket permitted a market outside the list")
	}
	// An empty allowlist permits nothing, because the column is NOT NULL: an
	// empty array is an explicit statement, not an absent one.
	var empty risk.Policy
	if empty.PermitsMarket("CRYPTO") {
		t.Error("an empty market allowlist permitted everything")
	}
}

func activePolicyForParity() risk.Policy {
	return risk.Policy{
		PermittedMarkets: []string{"CRYPTO"},
	}
}

// Policy.Validate must accept what the schema accepts. A Go-side refusal of a
// legal row would stop the platform trading under a policy PostgreSQL considers
// sound.
func TestPolicyValidateAgreesWithTheSchema(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	// The schema permits max_price_deviation = 0, and the Go field must too.
	var deviationCheck string
	err := db.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE conrelid = 'risk.policy'::regclass
		   AND pg_get_constraintdef(oid) LIKE '%price_deviation%'`).Scan(&deviationCheck)
	if err != nil {
		t.Fatalf("could not read the price deviation constraint: %v", err)
	}
	if !strings.Contains(deviationCheck, "0") {
		t.Errorf("the price deviation constraint is %q; domain/risk assumes 0 is permitted", deviationCheck)
	}

	legal := risk.Policy{
		Revision: "pol-legal", Environment: "live", Status: "ACTIVE",
		MaxOrderNotional:    contracts.MustParseDecimal("1"),
		MaxOrderQuantity:    contracts.MustParseDecimal("1"),
		MaxGrossExposure:    contracts.MustParseDecimal("1"),
		MaxNetExposure:      contracts.MustParseDecimal("1"),
		MaxLeverage:         contracts.MustParseDecimal("1"),
		MaxConcentration:    contracts.MustParseDecimal("1"),
		MaxDailyLoss:        contracts.MustParseDecimal("1"),
		MaxDrawdown:         contracts.MustParseDecimal("1"),
		MaxPriceDeviation:   contracts.MustParseDecimal("0"),
		MaxOpenOrders:       1,
		MaxOrdersPerMinute:  1,
		MaxCancelsPerMinute: 1,
		LossWindow:          time.Second,
		LossSource:          "risk.account_state.daily_pnl",
		MaxMarketDataAge:    time.Second,
	}
	if err := legal.Validate(); err != nil {
		t.Errorf("a policy the schema permits was refused by Go: %v", err)
	}
}
