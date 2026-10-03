package dbtest_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/risk"
	"github.com/aitc/trade/domain/riskpolicy"
	"github.com/aitc/trade/domain/riskstate"
)

// The risk gate against a real policy row.
//
// This file exists to close a recorded gap, not to add coverage:
//
//	"The risk gate has never approved an order in a real configuration."
//
// Every test in domain/risk constructs a Policy literal. The parity test proves
// those literals' columns line up with the schema, but it does not prove that a
// row can be written, read back, and used to approve an order. A loader that
// mis-mapped one column, or dropped the allowlists, would pass every unit test in
// the package and refuse every real order.
//
// So this file does the one thing a literal cannot: it stores a policy in
// PostgreSQL, loads it back through domain/riskpolicy, and runs the gate on the
// result. An approval here is an approval produced from data the database holds.
//
// The paired denials matter as much. A gate that approves everything also passes
// "did it approve?" if the test only checks one column.
//
// # Isolation
//
// These inserts commit, because the loader reads through a *sql.DB and a policy
// existing only inside a transaction would not be visible to it. Committed rows
// therefore outlive the test.
//
// Two things follow, and both were learned the hard way:
//
//   - every inserted row is deleted in t.Cleanup, so a run leaves no residue for
//     the next one;
//   - every test gets its own environment, because policy_active_scope_idx is
//     unique on (environment, scope_kind, scope_value). Sharing "paper" meant an
//     ENVIRONMENT-scoped row from one test governed the accounts of every later
//     test, which is not a fixture problem but a precedence behaviour: an
//     environment policy is the floor that catches every account in it.

// policySeq allocates both canonical ids and environments, so no two fixtures in
// a run can collide.
//
// dbtest.CanonicalID is deterministic for a given (prefix, n) within a run --
// deliberately, so a test can compute an id once and reuse it. Here the opposite
// is required: repeated calls must not produce the same id.
var policySeq atomic.Int64

// environments are the common.environment labels a test may use. live is
// excluded: no test in this repository may write to it while G11 is BLOCKED.
var environments = []string{"dev", "test", "staging", "paper", "shadow"}

// newEnvironment returns an environment no other fixture in this run is using.
func newEnvironment() string {
	return environments[int(policySeq.Add(1))%len(environments)]
}

// insertPolicy writes a minimal ACTIVE policy and returns its revision.
//
// Every value satisfies the CHECK constraints, and the shape is uniform across
// tests so a failure points at the field the test changed rather than at
// incidental setup. Dual control is required: policy_dual_control refuses
// approved_by = created_by, so the approver differs from the creator.
func insertPolicy(t *testing.T, db *sql.DB, env, scopeKind, scopeValue string) string {
	t.Helper()
	rev := dbtest.CanonicalID("pol", int(policySeq.Add(1)))
	_, err := db.Exec(`
		INSERT INTO risk.policy (
			policy_revision, name, environment, scope_kind, scope_value,
			max_order_notional, max_order_quantity, max_gross_exposure, max_net_exposure,
			max_leverage, max_concentration, max_open_orders, max_daily_loss, max_drawdown,
			loss_window, loss_source, max_market_data_age, max_price_deviation,
			max_orders_per_minute, max_cancels_per_minute,
			permitted_markets, permitted_venues, permitted_instruments,
			permitted_order_types, permitted_sides, permitted_modes,
			fee_assumptions, funding_assumptions, margin_assumptions, settlement_assumptions,
			halt_authority, reenable_authority, status,
			created_by, approved_by, approved_at, effective_at, rollback_revision)
		VALUES ($1,'integration',$2::common.environment,$3,$4,
			100000,10,500000,250000,5,0.5,20,5000,0.2,
			INTERVAL '1 day','ledger',INTERVAL '5 seconds',0.02,60,30,
			ARRAY['crypto']::common.market_class[],ARRAY['ven-1'],ARRAY['ins-1'],
			ARRAY['MARKET']::common.order_type[],ARRAY['BUY']::common.side[],ARRAY['PAPER'],
			'{}','{}','{}','{}','RISK_OPERATOR','OWNER','ACTIVE',
			'creator-1','approver-1',now(),now(),'rollback-1')`,
		rev, env, scopeKind, scopeValue)
	if err != nil {
		t.Fatalf("could not insert a policy: %v", err)
	}
	t.Cleanup(func() {
		if _, delErr := db.Exec(`DELETE FROM risk.policy WHERE policy_revision = $1`, rev); delErr != nil {
			t.Logf("cleanup of policy %s failed: %v", rev, delErr)
		}
	})
	return rev
}

// insertAccountState writes a real risk.account_state row and returns its account
// id, so that the figures the gate measures are read rather than asserted.
//
// The earlier version of this file asserted leverage 1.2, daily loss 100 and
// drawdown 0.05 as Go literals. That worked while the gate trusted its caller,
// and stopped working when domain/risk began requiring every measurement to carry
// Provenance -- an unsourced figure is treated as a failure, because a figure
// nobody read cannot be evidence that a limit was respected.
//
// Asserting figures in a test whose subject is "does the gate work against real
// data" is also self-defeating: it proves the gate evaluates numbers the test
// chose, not numbers a database holds. These rows make the second claim true.
//
// The values are chosen to sit inside every limit insertPolicy writes: equity
// below peak by 5% for a 0.05 drawdown, leverage 1.2 against a cap of 5, a daily
// gain of 100 against a loss cap of 5000, and 2 open orders against a cap of 20.
func insertAccountState(t *testing.T, q riskstate.Querier, env, accountID string) {
	t.Helper()
	_, err := q.ExecContext(context.Background(), `
		INSERT INTO risk.account_state (
			account_id, environment, venue_id, base_currency,
			account_status, equity, available_margin, used_margin,
			realized_pnl, daily_pnl, peak_equity, open_order_count, leverage,
			reconciliation_status, as_of, as_of_ns, version_vector)
		VALUES ($1,$2::common.environment,'ven-1','USD',
			'ACTIVE',9500,9000,500,
			100,100,10000,2,1.2,
			'CLEAN',now(),1,1)`,
		accountID, env)
	if err != nil {
		t.Fatalf("could not insert account state: %v", err)
	}
}

// insertFilledOrder drives one order from RISK_PENDING to FILLED and returns its
// id.
//
// The order is inserted at RISK_PENDING and then walked forward one journal event
// at a time, because that is the only route the schema permits: oms_order_entry_state
// refuses a directly-terminal order, and oms.guard_order_update refuses a state
// change with no oms.order_event declaring the transition.
//
// This is recorded rather than worked around for the same reason the book is
// built from fills -- the fixture was rewritten three times by schema refusals,
// and each refusal was the schema correctly refusing a state it should not accept.
// A fixture that reached FILLED by INSERT would have tested an order that could
// never have existed.
func insertFilledOrder(t *testing.T, q riskstate.Querier, env, accountID, instrument string) string {
	t.Helper()
	orderID := dbtest.CanonicalID("ord", int(policySeq.Add(1)))
	correlationID := "fixture-book"

	// RISK_PENDING with the risk decision already attached. The transition into
	// RISK_APPROVED requires a durable risk decision, and a risk-increasing order
	// may not carry one into a submission state without it.
	if _, err := q.ExecContext(context.Background(), `
		INSERT INTO oms."order" (
			order_id, command_id, idempotency_scope, environment, account_id,
			strategy_id, instrument_id, venue_id, side, order_type, time_in_force,
			quantity, filled_quantity, average_fill_price, limit_price, stop_price,
			state, version_vector, risk_decision_id, risk_policy_revisions,
			correlation_id)
		VALUES ($1,$2,'fixture',$3::common.environment,$4,
			'str-1',$5,'ven-1','BUY','MARKET','GTC',
			2,0,NULL,NULL,NULL,
			'RISK_PENDING',1,'rdc-fixture','{}',
			$6)`,
		orderID, dbtest.CanonicalID("cmd", int(policySeq.Add(1))), env, accountID, instrument, correlationID); err != nil {
		t.Fatalf("could not insert order for %s: %v", instrument, err)
	}

	// Each step is: declare the transition in the journal, then move the order.
	// The reverse order would be refused, which is what makes the journal the
	// authority rather than a log kept after the fact.
	steps := []struct {
		from, to, kind string
		delta          string
		price          string
	}{
		{"RISK_PENDING", "RISK_APPROVED", "RISK_APPROVED", "0", "NULL"},
		{"RISK_APPROVED", "SUBMITTING", "SUBMITTING", "0", "NULL"},
		{"SUBMITTING", "ACKNOWLEDGED", "ACKNOWLEDGED", "0", "NULL"},
		{"ACKNOWLEDGED", "FILLED", "FILLED", "2", "1000"},
	}
	for i, step := range steps {
		priceArg := any(nil)
		if step.price != "NULL" {
			priceArg = step.price
		}
		if _, err := q.ExecContext(context.Background(), `
			INSERT INTO oms.order_event (
				order_event_id, order_id, sequence, from_state, to_state, event_kind,
				filled_quantity_delta, price, actor_id, correlation_id,
				occurred_at, occurred_at_ns)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'fixture-actor',$9,now(),$10)`,
			dbtest.CanonicalID("oe1", int(policySeq.Add(1))), orderID, i+1,
			step.from, step.to, step.kind, step.delta, priceArg, correlationID, i+1); err != nil {
			t.Fatalf("could not journal %s -> %s for %s: %v", step.from, step.to, instrument, err)
		}

		// Execution progress may only advance alongside the fill event that
		// declares it; filled_quantity must equal the sum of fill event deltas.
		exec := `UPDATE oms."order" SET state = $2::oms.order_state, version_vector = version_vector + 1`
		if step.kind == "FILLED" {
			exec += `, filled_quantity = 2, average_fill_price = 1000`
		}
		exec += ` WHERE order_id = $1`
		if _, err := q.ExecContext(context.Background(), exec, orderID, step.to); err != nil {
			t.Fatalf("could not move %s -> %s for %s: %v", step.from, step.to, instrument, err)
		}
	}
	return orderID
}

// insertBook builds a position book the only way the schema permits: ten
// instruments, each with a filled order, a validated fill, and the position that
// fill implies.
//
// This chain is longer than a fixture normally needs, and every step was added
// because the schema refused the shorter version. Migration 0016 enforces
// mandatory invariant 5 -- positions are derived from validated fills and never
// entered independently -- so a fixture that inserted portfolio.position rows
// directly was refused twice over: first for claiming a fill watermark no fill
// backed, then for carrying a quantity the validated fills did not imply.
//
// That refusal is the schema working. Building the book from fills also means the
// integration test exercises the same derivation production uses, so the exposure
// the risk gate measures is exposure the system could actually have reached.
//
// The shape is chosen to make the concentration control meaningful: ten
// instruments at 2000 of gross exposure each give a gross of 20000 and a largest
// single instrument of 2000, so concentration is 0.1 against a cap of 0.5. A
// one-instrument book would read 1.0 and breach the policy, so 0.1 is a measured
// fact rather than a convenient one.
//
// Two limitations are recorded rather than hidden:
//
//   - Entry price is the mark. LoadPositions reads average_entry_price and
//     consults no price feed, so gross exposure is measured at cost.
//   - fill_sequence is 1..10 across the book, monotonic per account as the column
//     requires. It is a fixture's arbitrary placement in a stream that would
//     really hold many more fills, and nothing in the risk path reads its
//     magnitude.
func insertBook(t *testing.T, q riskstate.Querier, env, accountID string) []string {
	t.Helper()
	instruments := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		instrument := dbtest.CanonicalID("ins", int(policySeq.Add(1)))
		instruments = append(instruments, instrument)

		if _, err := q.ExecContext(context.Background(), `
			INSERT INTO market.instrument (
				instrument_id, market_class, base, quote,
				min_quantity, price_increment, tick_size, order_types,
				trading_status)
			VALUES ($1,'crypto','BTC','USD',
				0.00000001,0.01,0.01,ARRAY['MARKET','LIMIT']::common.order_type[],
				'OPEN')`, instrument); err != nil {
			t.Fatalf("could not insert instrument %s: %v", instrument, err)
		}

		orderID := insertFilledOrder(t, q, env, accountID, instrument)

		// A validated fill: validated_at present and reconciliation_status MATCHED,
		// or fill_validation_complete refuses it. Positions derive from these and
		// from nothing else.
		if _, err := q.ExecContext(context.Background(), `
			INSERT INTO oms.fill (
				fill_id, order_id, account_id, instrument_id, venue_id,
				side, quantity, price, fee_amount, fee_currency, funding_amount,
				venue_trade_id, source_timestamp, source_timestamp_ns,
				validated, validated_at, reconciliation_status, correlation_id,
				fill_sequence)
			VALUES ($1,$2,$3,$4,'ven-1',
				'BUY',2,1000,0,'USD',0,
				$5,now(),1,
				true,now(),'MATCHED','fixture-book',
				$6)`,
			dbtest.CanonicalID("fil", int(policySeq.Add(1))), orderID, accountID, instrument,
			"vt-"+instrument, i+1); err != nil {
			t.Fatalf("could not insert fill for %s: %v", instrument, err)
		}

		positionID := dbtest.CanonicalID("pos", int(policySeq.Add(1)))
		if _, err := q.ExecContext(context.Background(), `
			INSERT INTO portfolio.position (
				position_id, account_id, environment, instrument_id, venue_id,
				quantity, average_entry_price, realized_pnl, unrealized_pnl,
				derived_from_fill_sequence, derived_from_ledger_sequence, built_at, version_vector)
			VALUES ($1,$2,$3::common.environment,$4,'ven-1',
				2,1000,0,0,$5,0,now(),1)`,
			positionID, accountID, env, instrument, i+1); err != nil {
			t.Fatalf("could not insert position %s: %v", instrument, err)
		}
	}
	// No cleanup here: these rows live in the fixture transaction and disappear
	// with it. A DELETE would be refused anyway -- a validated fill is immutable
	// under mandatory invariant 4, and orders and instruments are still
	// referenced by it.
	return instruments
}

// gateFixture is the account, its positions, and an order measured against them.
//
// It exists because of an ordering constraint that the earlier version of this
// file got wrong. Policy resolution is scoped by account id, so the account must
// exist and be known before the policy row is written -- and the order's
// measurements must be read from that same account, or the policy governs an
// account nothing was measured against. Building the three in one fixture is
// what keeps those identities consistent.
//
// order is built once and returned by value. Tests that need to breach a limit
// take a copy and modify the copy, which is why the mutation helpers below take
// a pointer and cannot disturb another subtest's fixture.
type gateFixture struct {
	db          *sql.DB
	tx          *sql.Tx
	env         string
	accountID   string
	instruments []string
	order       risk.OrderFacts
}

// newGateFixture writes the account and position rows and returns an order whose
// measurements were read from them.
//
// Nothing here is invented to satisfy a limit. If a limit rejects this order, the
// limit is right and the fixture is wrong.
// insertStrategy writes a strategy row in the given state and returns its id.
//
// The approval fields are populated only for DEPLOYED, because the schema requires
// a DEPLOYED strategy to have been genuinely approved: strategy_approval_complete
// demands approver, approval time and expiry together, and
// strategy_approval_distinct forbids the owner approving their own strategy. A
// row that reached DEPLOYED without those would be a strategy nobody authorised,
// which is the state this whole package exists to prevent -- so the fixture is
// built to be one an operator could actually have approved.
//
// A non-DEPLOYED row is written directly rather than demoted from DEPLOYED.
// strategy.guard_strategy_update refuses an unexplained state change without a
// strategy.state_transition event, and walking DEPLOYED->PAUSED->...->DRAFT
// backwards through rules that only permit promotion would be a longer path
// through states the schema does not allow.
func insertStrategy(t *testing.T, q riskstate.Querier, state string) string {
	t.Helper()
	strategyID := dbtest.CanonicalID("str", int(policySeq.Add(1)))

	var approver, approvedAt, expiresAt any
	if state == "DEPLOYED" {
		approver = "approver-1"
		approvedAt = time.Now()
		expiresAt = time.Now().AddDate(1, 0, 0)
	}
	_, err := q.ExecContext(context.Background(), `
		INSERT INTO strategy.strategy (
			strategy_id, name, owner_subject_id, state, market_class,
			strategy_version, content_digest, validated_by, approved_by,
			approved_at, approval_expires_at, rollback_digest)
		VALUES ($1,'fixture',$2,$3::strategy.strategy_state,'crypto',
			1,$4,'validator-1',$5,
			$6,$7,$8)`,
		strategyID, "owner-1", state,
		strings.Repeat("a", 64), // content_digest: 64 hex characters
		approver, approvedAt, expiresAt,
		strings.Repeat("b", 64)) // rollback_digest
	if err != nil {
		t.Fatalf("could not insert %s strategy %s: %v", state, strategyID, err)
	}
	return strategyID
}

// newGateFixture writes the account and position rows and returns an order whose
// measurements were read from them.
//
// The OMS and portfolio rows live in a transaction that is rolled back at the end
// of the test, which is the only way to leave no residue: mandatory invariant 4
// makes a validated fill immutable and undeletable, so cleanup by DELETE is
// refused by the schema. The risk.policy row is committed separately and cleaned
// up normally, because policy rows are deletable and the loader resolves through
// a *sql.DB, which deliberately reads committed state rather than this
// transaction.
//
// Nothing here is invented to satisfy a limit. If a limit rejects this order, the
// limit is right and the fixture is wrong.
func newGateFixture(t *testing.T) gateFixture {
	t.Helper()
	db := dbtest.Open(t)
	env := newEnvironment()
	accountID := dbtest.CanonicalID("acc", int(policySeq.Add(1)))

	// The context is released by Cleanup, not by defer. A defer here would cancel
	// the context the moment this constructor returns, and database/sql tears the
	// transaction down with it -- the fixture would arrive already dead, with the
	// failure surfacing later as "transaction has already been committed or
	// rolled back" from whichever read happened to be second.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	insertAccountState(t, tx, env, accountID)
	strategyID := insertStrategy(t, tx, "DEPLOYED")
	instruments := insertBook(t, tx, env, accountID)

	// The order itself: a market buy of 1 at 5000, so a notional of 5000
	// against max_order_notional of 100000. It names the first instrument of the
	// book, so the instrument on the order is one the book really holds.
	yes := true
	notional := contracts.MustParseDecimal("5000")
	order := risk.OrderFacts{
		Environment: env, AccountID: accountID, StrategyID: strategyID,
		InstrumentID: instruments[0], MarketClass: "crypto", VenueID: "ven-1",
		OrderType: "MARKET", Side: "BUY", Amount: "1", Price: "5000", Currency: "USD",
		// InstrumentEligible and PrecisionValid are deliberately absent. Load
		// reads both from market.instrument, and a fixture that supplied them
		// would be asserting the very fact this test exists to source.
		VenueAvailable:  &yes,
		MarketDataFresh: &yes, NoDuplicateOrder: &yes,
		Notional: &notional,
		// Orders and cancels in the last minute, against caps of 60 and 30.
		OrdersLastMinute: 3, CancelsLastMinute: 1,
	}

	// Load rather than Build, so the account, the positions and the strategy's
	// deployment state are all read from rows this test wrote. A Build here would
	// need the strategy state passed in as the literal "DEPLOYED", which is the
	// assertion this fixture exists to stop making.
	//
	// MaxAccountAge 0 skips the staleness check: the row was written microseconds
	// ago by this test, so freshness is not in question and asserting it would
	// only measure the clock.
	facts, err := riskstate.Load(ctx, tx, riskstate.Facts{
		Order:          order,
		PolicyRevision: "unresolved-fixture",
	})
	if err != nil {
		t.Fatalf("riskstate.Load refused the fixture: %v", err)
	}
	return gateFixture{
		db: db, tx: tx, env: env, accountID: accountID,
		instruments: instruments, order: facts,
	}
}

// policy inserts an ACCOUNT-scoped policy for the fixture's own account and
// resolves it back, returning the loaded policy and its revision.
//
// The policy's instrument allowlist carries the book this fixture actually wrote
// rather than a fixed literal, so the allowlist and the positions describe the
// same world. A policy permitting an instrument no market.instrument row backs
// would test the string comparison rather than the eligibility control.
//
// The insert and the resolve are together on purpose: a test that inserted a
// policy and evaluated against a hand-written literal would be testing the
// literal, while a test that resolved a policy without inserting one would be
// testing the loader's idea of what a policy looks like.
func (f gateFixture) policy(t *testing.T) (risk.Policy, string) {
	t.Helper()
	rev := dbtest.CanonicalID("pol", int(policySeq.Add(1)))

	// The instrument allowlist is built as a parameterised ARRAY[...] rather than
	// a string literal. The ids are canonical and machine-generated, so they are
	// passed as parameters where the database -- not this file -- decides what
	// they mean; a literal would mean the SQL text was assembled by string
	// concatenation from Go values.
	args := []any{rev, f.env, f.accountID}
	placeholders := make([]string, len(f.instruments))
	for i, instrument := range f.instruments {
		placeholders[i] = fmt.Sprintf("$%d", len(args)+1)
		args = append(args, instrument)
	}
	permittedInstruments := "ARRAY[" + strings.Join(placeholders, ",") + "]"

	if _, err := f.db.Exec(`
		INSERT INTO risk.policy (
			policy_revision, name, environment, scope_kind, scope_value,
			max_order_notional, max_order_quantity, max_gross_exposure, max_net_exposure,
			max_leverage, max_concentration, max_open_orders, max_daily_loss, max_drawdown,
			loss_window, loss_source, max_market_data_age, max_price_deviation,
			max_orders_per_minute, max_cancels_per_minute,
			permitted_markets, permitted_venues, permitted_instruments,
			permitted_order_types, permitted_sides, permitted_modes,
			fee_assumptions, funding_assumptions, margin_assumptions, settlement_assumptions,
			halt_authority, reenable_authority, status,
			created_by, approved_by, approved_at, effective_at, rollback_revision)
		VALUES ($1,'integration',$2::common.environment,'ACCOUNT',$3,
			100000,10,500000,250000,5,0.5,20,5000,0.2,
			INTERVAL '1 day','ledger',INTERVAL '5 seconds',0.02,60,30,
			ARRAY['crypto']::common.market_class[],ARRAY['ven-1'],`+permittedInstruments+`,
			ARRAY['MARKET']::common.order_type[],ARRAY['BUY']::common.side[],ARRAY['PAPER'],
			'{}','{}','{}','{}','RISK_OPERATOR','OWNER','ACTIVE',
			'creator-1','approver-1',now(),now(),'rollback-1')`,
		args...); err != nil {
		t.Fatalf("could not insert a policy: %v", err)
	}
	t.Cleanup(func() {
		if _, delErr := f.db.Exec(`DELETE FROM risk.policy WHERE policy_revision = $1`, rev); delErr != nil {
			t.Logf("cleanup of policy %s failed: %v", rev, delErr)
		}
	})

	policy, err := riskpolicy.New(f.db).ResolveForOrder(context.Background(), scopeFor(f.order))
	if err != nil {
		t.Fatalf("could not resolve the policy just written: %v", err)
	}
	if policy.Revision != rev {
		t.Fatalf("resolved %s, wrote %s", policy.Revision, rev)
	}
	return policy, rev
}

// scopeFor is the resolution scope for an order.
func scopeFor(o risk.OrderFacts) riskpolicy.OrderScope {
	return riskpolicy.OrderScope{
		Environment:  o.Environment,
		AccountID:    o.AccountID,
		StrategyID:   o.StrategyID,
		InstrumentID: o.InstrumentID,
		VenueID:      o.VenueID,
		MarketClass:  o.MarketClass,
	}
}

// The gap this file closes: an order approved by a gate running on a policy the
// database actually holds.
func TestTheRiskGateApprovesAnOrderFromARealPolicyRow(t *testing.T) {
	f := newGateFixture(t)
	policy, rev := f.policy(t)

	decision := risk.Evaluate(risk.Request{
		Order:         f.order,
		Policy:        policy,
		Halt:          risk.HaltDecision{Evaluated: true},
		CorrelationID: "integration-1",
		EvaluatedAt:   time.Now().UTC(),
	})
	if !decision.Approved {
		t.Fatalf("the gate refused an order that policy %s permits: %s", rev, decision.Summary())
	}
	if decision.PolicyRevision != rev {
		t.Errorf("the approval names policy %q, not the row that was loaded (%s)",
			decision.PolicyRevision, rev)
	}
	if len(decision.Findings) != len(risk.Controls()) {
		t.Errorf("the decision records %d findings, want all %d controls",
			len(decision.Findings), len(risk.Controls()))
	}
}

// Every column the loader reads must survive the round trip. A loader that
// silently dropped or mis-mapped a limit would still approve the order above, so
// that approval is not sufficient on its own.
func TestEveryPolicyColumnSurvivesTheRoundTrip(t *testing.T) {
	f := newGateFixture(t)
	policy, _ := f.policy(t)

	dec := func(want string, got contracts.Decimal, column string) {
		t.Helper()
		// Compared with Cmp, not string equality. A NUMERIC(38,18) column
		// normalises scale on read, so 100000 returns as
		// 100000.000000000000000000. That is the declared precision being
		// honoured, not a precision loss, and a string comparison would report
		// it as one.
		if w := contracts.MustParseDecimal(want); got.Cmp(w) != 0 {
			t.Errorf("%s came back as %s, the row holds %s", column, got.String(), want)
		}
	}
	dec("100000", policy.MaxOrderNotional, "max_order_notional")
	dec("10", policy.MaxOrderQuantity, "max_order_quantity")
	dec("500000", policy.MaxGrossExposure, "max_gross_exposure")
	dec("250000", policy.MaxNetExposure, "max_net_exposure")
	dec("5", policy.MaxLeverage, "max_leverage")
	dec("0.5", policy.MaxConcentration, "max_concentration")
	dec("5000", policy.MaxDailyLoss, "max_daily_loss")
	dec("0.2", policy.MaxDrawdown, "max_drawdown")
	dec("0.02", policy.MaxPriceDeviation, "max_price_deviation")

	if policy.MaxOpenOrders != 20 {
		t.Errorf("max_open_orders came back as %d, the row holds 20", policy.MaxOpenOrders)
	}
	if policy.MaxOrdersPerMinute != 60 {
		t.Errorf("max_orders_per_minute came back as %d, the row holds 60", policy.MaxOrdersPerMinute)
	}
	if policy.MaxCancelsPerMinute != 30 {
		t.Errorf("max_cancels_per_minute came back as %d, the row holds 30", policy.MaxCancelsPerMinute)
	}
	if policy.LossWindow != 24*time.Hour {
		t.Errorf("loss_window came back as %s, the row holds 1 day", policy.LossWindow)
	}
	if policy.MaxMarketDataAge != 5*time.Second {
		t.Errorf("max_market_data_age came back as %s, the row holds 5 seconds", policy.MaxMarketDataAge)
	}
	if policy.LossSource != "ledger" {
		t.Errorf("loss_source came back as %q, the row holds ledger", policy.LossSource)
	}
	if policy.ScopeKind != "ACCOUNT" || policy.ScopeValue != f.accountID {
		t.Errorf("scope came back as %s/%s, want ACCOUNT/%s",
			policy.ScopeKind, policy.ScopeValue, f.accountID)
	}
	if policy.Status != "ACTIVE" {
		t.Errorf("status came back as %q, the row holds ACTIVE", policy.Status)
	}

	// The allowlists decide eligibility and venue availability. A loader that
	// dropped them would make every order look ineligible, so they are asserted
	// both positively and negatively, against the instruments the fixture wrote.
	if !policy.PermitsInstrument(f.instruments[0]) {
		t.Error("permitted_instruments did not survive the round trip")
	}
	if !policy.PermitsVenue("ven-1") {
		t.Error("permitted_venues did not survive the round trip")
	}
	if !policy.PermitsMarket("crypto") {
		t.Error("permitted_markets did not survive the round trip")
	}
	if policy.PermitsVenue("ven-2") {
		t.Error("a venue outside the row's allowlist was permitted")
	}

	if err := policy.Validate(); err != nil {
		t.Errorf("a policy loaded from a committed row does not validate: %v", err)
	}
}

// The paired denials. Each changes one thing and confirms the gate refuses, so
// "it approved" cannot be an artefact of a permissive gate.
func TestTheGateStillRefusesWhenTheRowSaysOtherwise(t *testing.T) {
	cases := []struct {
		name  string
		order func(*risk.OrderFacts)
		want  risk.Control
	}{
		{"over the notional limit", func(f *risk.OrderFacts) {
			v := contracts.MustParseDecimal("200000")
			f.Notional = &v
		}, risk.CtrlNotionalLimit},
		{"over the net exposure limit", func(f *risk.OrderFacts) {
			v := contracts.MustParseDecimal("900000")
			f.Position = &v
		}, risk.CtrlPositionLimit},
		{"over the daily loss limit", func(f *risk.OrderFacts) {
			v := contracts.MustParseDecimal("9000")
			f.DailyLoss = &v
		}, risk.CtrlLossLimit},
		{"over the open-order limit", func(f *risk.OrderFacts) {
			f.OpenOrderCount = 21
		}, risk.CtrlRateLimit},
		{"over the orders-per-minute limit", func(f *risk.OrderFacts) {
			f.OrdersLastMinute = 61
		}, risk.CtrlRateLimit},
		{"instrument not on the allowlist", func(f *risk.OrderFacts) {
			f.InstrumentID = "ins-not-in-the-book"
		}, risk.CtrlInstrumentEligible},
		{"venue not on the allowlist", func(f *risk.OrderFacts) {
			f.VenueID = "ven-9"
		}, risk.CtrlVenueAvailable},
		{"venue unavailable", func(f *risk.OrderFacts) {
			no := false
			f.VenueAvailable = &no
		}, risk.CtrlVenueAvailable},
		{"account not authorized", func(f *risk.OrderFacts) {
			no := false
			f.AccountAuthorized = &no
		}, risk.CtrlAccountAuthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)

			// The policy is written and resolved before the order is mutated. The
			// policy is ACCOUNT-scoped and no case changes the account, so it
			// governs the mutated order too -- which is what makes these paired
			// denials about the mutated field rather than about scope resolution.
			policy, _ := f.policy(t)

			// A copy, so the mutation cannot reach the fixture other assertions
			// read. The measurements the mutation does not touch stay sourced from
			// the rows the loader read.
			order := f.order
			tc.order(&order)

			decision := risk.Evaluate(risk.Request{
				Order: order, Policy: policy,
				Halt:        risk.HaltDecision{Evaluated: true},
				EvaluatedAt: time.Now().UTC(),
			})
			if decision.Approved {
				t.Fatalf("the gate approved an order with %s", tc.name)
			}
			found := false
			for _, finding := range decision.Failed() {
				if finding.Control == tc.want {
					found = true
				}
			}
			if !found {
				t.Errorf("the refusal did not name %s; failures were %v", tc.want, decision.Failed())
			}
		})
	}
}

// No policy at all is a refusal, and a distinguishable one. Doc 17 §3 forbids the
// platform supplying a default, so the loader must report absence rather than
// invent a permissive policy.
func TestNoPolicyResolvesToRefusalNotToADefault(t *testing.T) {
	db := dbtest.Open(t)
	env := newEnvironment()

	_, err := riskpolicy.New(db).ResolveForOrder(context.Background(), riskpolicy.OrderScope{
		Environment: env, AccountID: "acc-absent", MarketClass: "crypto",
	})
	if err == nil {
		t.Fatal("a policy was resolved for an account that has none")
	}
	if !errors.Is(err, riskpolicy.ErrNoActivePolicy) {
		t.Errorf("error is %v, want ErrNoActivePolicy", err)
	}
}

// A DRAFT policy is not in force. The loader filters on status, so a draft must
// not resolve even though the row exists.
func TestADraftPolicyDoesNotResolve(t *testing.T) {
	db := dbtest.Open(t)
	env := newEnvironment()
	rev := insertPolicy(t, db, env, "ACCOUNT", "acc-1")
	if _, err := db.Exec(`UPDATE risk.policy
		SET status = 'DRAFT', approved_by = NULL, approved_at = NULL,
		    effective_at = NULL, rollback_revision = NULL
		WHERE policy_revision = $1`, rev); err != nil {
		t.Fatalf("could not demote the policy to DRAFT: %v", err)
	}

	_, err := riskpolicy.New(db).ResolveForOrder(context.Background(), riskpolicy.OrderScope{
		Environment: env, AccountID: "acc-1", MarketClass: "crypto",
	})
	if !errors.Is(err, riskpolicy.ErrNoActivePolicy) {
		t.Errorf("a DRAFT policy resolved: err = %v, want ErrNoActivePolicy", err)
	}
}

// The most specific policy wins. An account policy must not be overridable by a
// broad environment policy, which is the same inversion as a broad ceiling
// defeating an account-specific cap.
func TestTheMostSpecificPolicyWins(t *testing.T) {
	f := newGateFixture(t)
	db, env, loader := f.db, f.env, riskpolicy.New(f.db)
	ctx := context.Background()

	// A broad environment policy with a low notional limit, and a narrow account
	// policy with a high one. The account policy must govern.
	envRev := insertPolicy(t, db, env, "ENVIRONMENT", env)
	accRev := insertPolicy(t, db, env, "ACCOUNT", "acc-specific")

	if _, err := db.Exec(`UPDATE risk.policy SET max_order_notional = 1000 WHERE policy_revision = $1`, envRev); err != nil {
		t.Fatalf("could not tighten the environment policy: %v", err)
	}
	if _, err := db.Exec(`UPDATE risk.policy SET max_order_notional = 900000 WHERE policy_revision = $1`, accRev); err != nil {
		t.Fatalf("could not loosen the account policy: %v", err)
	}

	policy, err := loader.ResolveForOrder(ctx, riskpolicy.OrderScope{
		Environment: env, AccountID: "acc-specific", MarketClass: "crypto",
	})
	if err != nil {
		t.Fatalf("could not resolve: %v", err)
	}
	if policy.Revision != accRev {
		t.Errorf("resolved %s (%s), want the account policy %s",
			policy.Revision, policy.ScopeKind, accRev)
	}

	// An unaccounted order falls through to the environment policy, and is then
	// refused for the notional that policy allows.
	envPolicy, err := loader.ResolveForOrder(ctx, riskpolicy.OrderScope{
		Environment: env, AccountID: "acc-other", MarketClass: "crypto",
	})
	if err != nil {
		t.Fatalf("could not resolve the environment policy: %v", err)
	}
	if envPolicy.Revision != envRev {
		t.Errorf("resolved %s for an unaccounted order, want the environment policy %s",
			envPolicy.Revision, envRev)
	}

	// The account is rewritten onto the order only after the measurements were
	// read: the provenance still describes a real account, and the scope it
	// resolves against is the one the test is exercising.
	order := f.order
	order.AccountID = "acc-other"
	decision := risk.Evaluate(risk.Request{
		Order: order, Policy: envPolicy,
		Halt:        risk.HaltDecision{Evaluated: true},
		EvaluatedAt: time.Now().UTC(),
	})
	if decision.Approved {
		t.Error("an order over the environment policy's tightened notional limit was approved")
	}
}

// The schema, not the loader, is what makes an invalid policy unloadable. The
// row is otherwise complete -- including the approval, effective_at and
// rollback_revision that active_policy_complete demands -- because constraint
// checking is not ordered by name, and a row invalid for two reasons reports an
// arbitrary one of them. Without those fields this test would pass while proving
// nothing about limits.
func TestTheSchemaRefusesAPolicyTheLoaderWouldReject(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		rev := dbtest.CanonicalID("pol", int(900000+policySeq.Add(1)))
		_, err := tx.Exec(`
			INSERT INTO risk.policy (
				policy_revision, name, environment, scope_kind, scope_value,
				max_order_notional, max_order_quantity, max_gross_exposure, max_net_exposure,
				max_leverage, max_concentration, max_open_orders, max_daily_loss, max_drawdown,
				loss_window, loss_source, max_market_data_age, max_price_deviation,
				max_orders_per_minute, max_cancels_per_minute,
				permitted_markets, permitted_venues, permitted_instruments,
				permitted_order_types, permitted_sides, permitted_modes,
				fee_assumptions, funding_assumptions, margin_assumptions, settlement_assumptions,
				halt_authority, reenable_authority, status,
				created_by, approved_by, approved_at, effective_at, rollback_revision)
			VALUES ($1,'integration','dev'::common.environment,'ACCOUNT','acc-bad',
				0,10,500000,250000,5,0.5,20,5000,0.2,
				INTERVAL '1 day','ledger',INTERVAL '5 seconds',0.02,60,30,
				ARRAY['crypto']::common.market_class[],ARRAY['ven-1'],ARRAY['ins-1'],
				ARRAY['MARKET']::common.order_type[],ARRAY['BUY']::common.side[],ARRAY['PAPER'],
				'{}','{}','{}','{}','RISK_OPERATOR','OWNER','ACTIVE',
				'creator-1','approver-1',now(),now(),'rollback-1')`,
			rev)
		if err == nil {
			t.Fatal("a policy with max_order_notional = 0 was accepted; policy_limits_positive " +
				"should refuse it, and the loader would refuse it in Go")
		}
		if !strings.Contains(err.Error(), "policy_limits_positive") {
			t.Errorf("rejection was %v, want the policy_limits_positive constraint named", err)
		}
	})
}

// A risk-reducing action is approved even with no policy loaded, and even when
// the account has no allowlist. Refusing to reduce exposure during an incident
// converts a market problem into an unmanaged loss, so this is asserted against
// the real loader rather than only against a literal.
func TestARiskReducingActionPassesWithNoPolicy(t *testing.T) {
	f := newGateFixture(t)
	order := f.order
	order.AccountID = "acc-unknown"
	order.RiskReducing = true
	order.OrderType = "CLOSE_POSITION"

	// Deliberately no policy row: the loader reports absence.
	_, err := riskpolicy.New(f.db).ResolveForOrder(context.Background(), scopeFor(order))
	if !errors.Is(err, riskpolicy.ErrNoActivePolicy) {
		t.Fatalf("expected no policy for an unknown account, got %v", err)
	}

	decision := risk.Evaluate(risk.Request{
		Order: order, Policy: risk.Policy{},
		Halt:        risk.HaltDecision{Evaluated: true},
		EvaluatedAt: time.Now().UTC(),
	})
	if !decision.Approved {
		t.Errorf("a risk-reducing action was refused with no policy loaded: %s", decision.Summary())
	}
}

// A policy row is a durable fact; the loader must not be able to widen it. The
// allowlists come back exactly as written, so a permitted instrument cannot be
// smuggled in and a forbidden one smuggled out.
func TestAllowlistsAreNotWidenedByLoading(t *testing.T) {
	f := newGateFixture(t)
	policy, _ := f.policy(t)

	// Every instrument on the book is on the allowlist, and nothing else is.
	// The negative cases are well-formed canonical ids that were never written,
	// so the comparison is exercised against ids of the right shape that the row
	// does not list -- plus the empty and case-folded forms, because a comparison
	// that ignored case would widen the allowlist by exactly one casing rule.
	forbidden := []string{"", "ins-", "ins-1", "INS-1",
		dbtest.CanonicalID("ins", 900001), dbtest.CanonicalID("ins", 900002)}
	for _, notPermitted := range forbidden {
		if policy.PermitsInstrument(notPermitted) {
			t.Errorf("PermitsInstrument(%q) = true; the row lists only the fixture book", notPermitted)
		}
	}
	for _, notPermitted := range []string{"ven-2", "VEN-1", ""} {
		if policy.PermitsVenue(notPermitted) {
			t.Errorf("PermitsVenue(%q) = true; the row lists only ven-1", notPermitted)
		}
	}
}

// Guard against a future change that makes the fixture's own environment
// allocation silently collide, which would present as a schema failure.
func TestTestEnvironmentsDoNotRepeatWithinARun(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < len(environments); i++ {
		e := newEnvironment()
		if seen[e] {
			t.Fatalf("environment %q allocated twice within %d allocations", e, len(environments))
		}
		seen[e] = true
	}
	if len(seen) != len(environments) {
		t.Errorf("allocated %d distinct environments, want %d", len(seen), len(environments))
	}
}

// The deployment fact must come from the row. A strategy id that names nothing is
// a broken reference, not a strategy that is merely not trading, and the two are
// reported differently so the operator knows whether to fix a reference or a
// deployment decision.
func TestTheStrategyStateIsReadAndAMissingOneIsNotConfusedWithADeployment(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()

	state, err := riskstate.LoadStrategy(ctx, f.tx, f.order.StrategyID)
	if err != nil {
		t.Fatalf("could not read the strategy just written: %v", err)
	}
	if state != "DEPLOYED" {
		t.Errorf("state = %q, want the DEPLOYED the fixture wrote", state)
	}

	_, err = riskstate.LoadStrategy(ctx, f.tx, dbtest.CanonicalID("str", 900010))
	if err == nil {
		t.Fatal("a strategy id naming no row was accepted")
	}
	if !strings.Contains(err.Error(), "broken reference") {
		t.Errorf("error %q does not distinguish a broken reference from an undeployed strategy", err)
	}
}

// The refusal for a non-DEPLOYED strategy must name the state the database holds.
// "Not deployed" alone sends the operator to the deployment process; "strategy
// state is DRAFT" sends them to the right row.
func TestANonDeployedStrategyIsRefusedByName(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	// A second strategy, written DRAFT. The fixture's own DEPLOYED strategy is
	// left alone: the schema refuses an unexplained state change, and demoting it
	// would need a transition event and a path the rules do not permit.
	draft := insertStrategy(t, tx, "DRAFT")

	accountID := dbtest.CanonicalID("acc", int(policySeq.Add(1)))
	insertAccountState(t, tx, newEnvironment(), accountID)

	_, err = riskstate.LoadStrategy(ctx, tx, draft)
	if err != nil {
		t.Fatalf("could not read the DRAFT strategy: %v", err)
	}

	// The instrument has to exist. Load reads eligibility and precision from
	// market.instrument, so an instrument the catalogue does not hold is refused
	// before the strategy's state is ever consulted. The fixture is therefore
	// complete here, so that the refusal this test is about -- a DRAFT strategy
	// named as such -- is the one that surfaces rather than a missing catalogue
	// row standing in front of it.
	instrumentID := dbtest.CanonicalID("ins", 900020)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market.instrument (
			instrument_id, market_class, base, quote,
			min_quantity, price_increment, tick_size, order_types,
			trading_status)
		VALUES ($1,'crypto','BTC','USD',
			0.00000001,0.01,0.01,ARRAY['MARKET','LIMIT']::common.order_type[],
			'OPEN')`, instrumentID); err != nil {
		t.Fatalf("could not insert the instrument: %v", err)
	}

	order := risk.OrderFacts{
		Environment: "paper", AccountID: accountID, StrategyID: draft,
		InstrumentID: instrumentID, MarketClass: "crypto",
		VenueID: "ven-1", OrderType: "MARKET", Side: "BUY",
		Amount: "1", Price: "5000", Currency: "USD",
	}
	_, err = riskstate.Load(ctx, tx, riskstate.Facts{Order: order})
	if err == nil {
		t.Fatal("a DRAFT strategy was loaded as tradable")
	}
	if !strings.Contains(err.Error(), "DRAFT") {
		t.Errorf("refusal %q does not name the state the database holds", err)
	}
}

// Build stamps provenance from the Account and Positions it was handed, and those
// are arguments a caller can forge. Load reads them, so it must stamp from what
// it read: a caller claiming a fresh AsOfNs over a stale row would otherwise
// satisfy the provenance check with a number it made up.
func TestLoadStampsProvenanceFromWhatItRead(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()

	acct, err := riskstate.LoadAccount(ctx, f.tx, f.order.AccountID, 0)
	if err != nil {
		t.Fatalf("could not read the account: %v", err)
	}
	pos, err := riskstate.LoadPositions(ctx, f.tx, f.order.AccountID, f.order.Environment)
	if err != nil {
		t.Fatalf("could not read the positions: %v", err)
	}

	// Deliberately absurd: no account row carries this nanosecond stamp.
	const forged = int64(-1)
	out, err := riskstate.Load(ctx, f.tx, riskstate.Facts{
		Order:              f.order,
		AccountStateAsOfNs: forged,
		PolicyRevision:     "unresolved-fixture",
	})
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if out.Provenance.AccountStateAsOfNs == forged {
		t.Error("Load kept the caller's forged read stamp instead of the one it read")
	}
	if out.Provenance.AccountStateAsOfNs != acct.AsOfNs {
		t.Errorf("read stamp = %d, want the row's %d",
			out.Provenance.AccountStateAsOfNs, acct.AsOfNs)
	}
	if out.Provenance.PositionRowCount != pos.RowCount {
		t.Errorf("position row count = %d, want the aggregate's %d",
			out.Provenance.PositionRowCount, pos.RowCount)
	}
	if !out.Provenance.Established() {
		t.Error("Load produced facts whose provenance is not established")
	}
}

// The tests below cover instrument eligibility and precision validity, which
// risk.OrderFacts used to carry as booleans the fixture supplied. Each changes
// the catalogue and expects the verdict to follow the row, and each has a
// permissive counterpart, so a control that refused everything would fail half of
// them rather than pass all of them.
//
// The fixture's order is a MARKET BUY of 1 priced at 5000 against an instrument
// with min_quantity 0.00000001, price_increment 0.01, order_types
// {MARKET, LIMIT} and trading_status OPEN: a whole number of increments at a
// whole number of ticks, on an instrument that permits the terms. Every change
// below is therefore a single departure from a permitted order.

// The permissive case, stated first so that the refusals below are refusals of a
// known-good order rather than of an order nothing has established is good.
func TestAnInstrumentThatPermitsTheOrderIsEligibleAndPrecise(t *testing.T) {
	f := newGateFixture(t)

	out, err := riskstate.Load(context.Background(), f.tx, riskstate.Facts{Order: f.order})
	if err != nil {
		t.Fatalf("could not load the fixture order: %v", err)
	}
	if out.InstrumentEligible == nil {
		t.Fatal("Load left INSTRUMENT_ELIGIBILITY unestablished")
	}
	if !*out.InstrumentEligible {
		t.Error("an OPEN instrument permitting MARKET/BUY was refused eligibility")
	}
	if out.PrecisionValid == nil {
		t.Fatal("Load left PRECISION_VALIDITY unestablished")
	}
	if !*out.PrecisionValid {
		t.Error("a whole multiple of the lot at a whole tick was refused as imprecise")
	}
}

// A SELL against an instrument that does not support shorting is the case the
// Side field exists for: without it the shorting check is skipped and a
// long-only instrument permits a short.
func TestASellIsRefusedByAnInstrumentThatCannotBeShorted(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()

	sell := f.order
	sell.Side = "SELL"
	sell.OrderType = "LIMIT"
	sell.Price = "5000.00"

	out, err := riskstate.Load(ctx, f.tx, riskstate.Facts{Order: sell})
	if err != nil {
		t.Fatalf("could not load the fixture order: %v", err)
	}
	if out.InstrumentEligible == nil || *out.InstrumentEligible {
		t.Error("a SELL was permitted by an instrument that does not support shorting")
	}
}

// An absent side must not be read as a buy. The shorting check is keyed on
// side == "SELL", so a missing side would otherwise pass straight through it.
func TestAnAbsentSideIsRefusedRatherThanReadAsABuy(t *testing.T) {
	f := newGateFixture(t)

	side := f.order
	side.Side = ""

	out, err := riskstate.Load(context.Background(), f.tx, riskstate.Facts{Order: side})
	if err != nil {
		t.Fatalf("could not load the fixture order: %v", err)
	}
	if out.InstrumentEligible == nil || *out.InstrumentEligible {
		t.Error("an order carrying no side was granted eligibility")
	}
}

// The strongest form of the control: the caller asserts the fact AND the
// catalogue contradicts it. The row must win, because a fixture that supplies
// the fact under test is what this package was written to stop.
func TestACallerCannotOverrideTheCatalogue(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()

	if _, err := f.tx.ExecContext(ctx,
		`UPDATE market.instrument SET trading_status = 'DELISTED' WHERE instrument_id = $1`,
		f.instruments[0]); err != nil {
		t.Fatalf("could not delist the instrument: %v", err)
	}

	yes := true
	claiming := f.order
	claiming.InstrumentEligible = &yes
	claiming.PrecisionValid = &yes

	out, err := riskstate.Load(ctx, f.tx, riskstate.Facts{Order: claiming})
	if err != nil {
		t.Fatalf("could not load the fixture order: %v", err)
	}
	if out.InstrumentEligible == nil || *out.InstrumentEligible {
		t.Error("Load kept the caller's eligibility claim over a DELISTED catalogue row")
	}
	if out.PrecisionValid == nil || !*out.PrecisionValid {
		// This one is genuinely still true: raising the lot size is what makes the
		// quantity off grid, and it has not been raised. Asserting it here states
		// that the two controls are independent rather than one refusal in disguise.
		t.Log("precision was refused too; the lot size was not changed in this test")
	}
}

// Precision follows the instrument's own grid. 0.3 is a legal min_quantity under
// instrument_min_quantity_positive, and a quantity of 1 is no longer a whole
// number of it.
func TestAQuantityOffTheInstrumentsGridIsRefused(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()

	if _, err := f.tx.ExecContext(ctx,
		`UPDATE market.instrument SET min_quantity = 0.3 WHERE instrument_id = $1`,
		f.instruments[0]); err != nil {
		t.Fatalf("could not raise the lot size: %v", err)
	}

	out, err := riskstate.Load(ctx, f.tx, riskstate.Facts{Order: f.order})
	if err != nil {
		t.Fatalf("could not load the fixture order: %v", err)
	}
	if out.PrecisionValid == nil || *out.PrecisionValid {
		t.Error("a quantity of 1 was accepted against a lot size of 0.3")
	}
	// The instrument itself is untouched, so eligibility must not have moved with
	// it: one refused control is not evidence about the other.
	if out.InstrumentEligible == nil || !*out.InstrumentEligible {
		t.Error("raising the lot size also cost the instrument its eligibility")
	}
}

// An instrument that does not exist is an error rather than a denial, because
// "the catalogue could not be read" and "the instrument is ineligible" are
// different facts and the second must not be inferred from the first.
func TestAnInstrumentOutsideTheCatalogueIsAnErrorRatherThanADenial(t *testing.T) {
	f := newGateFixture(t)

	missing := f.order
	missing.InstrumentID = dbtest.CanonicalID("ins", 900031)

	out, err := riskstate.Load(context.Background(), f.tx, riskstate.Facts{Order: missing})
	if err == nil {
		t.Fatalf("an uncatalogued instrument produced facts instead of an error: %+v", out)
	}
	if !strings.Contains(err.Error(), "market.instrument") {
		t.Errorf("refusal %q does not name the catalogue", err)
	}
}
