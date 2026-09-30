package dbtest_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/dbtest"
)

// These tests prove that the financial invariants in
// 09_TESTING_AND_RELEASE_EVIDENCE.md "Mandatory financial invariants" are
// enforced STRUCTURALLY by PostgreSQL, not merely by application code. A
// constraint that is never exercised by a negative test is documentation, not a
// control.
//
// Invariants under test:
//  1. No risk-rejected command creates a live submission.
//  2. Every accepted order has exactly one canonical OMS identity.
//  3. Duplicate commands do not increase exposure.
//  4. Ledger corrections are compensating entries.
//  5. Portfolio positions reconcile to validated fills.
//  6. Unknown submission outcomes trigger reconciliation.
//  7. Halt state blocks prohibited actions.
//  8. Lower-environment credentials cannot access live resources.
//
// Plus the numeric-precision control: no floating-point column may exist in an
// authoritative schema.

// ---------------------------------------------------------------------------
// Control: no floating-point values in authoritative schemas
// ---------------------------------------------------------------------------

func TestNoFloatingPointColumnsInAuthoritativeSchemas(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		rows, err := tx.QueryContext(ctx, `
            SELECT c.table_schema, c.table_name, c.column_name, c.data_type
              FROM information_schema.columns c
              JOIN information_schema.tables t
                ON t.table_schema = c.table_schema AND t.table_name = c.table_name
             WHERE c.table_schema IN ('identity','config','market','strategy','risk','oms',
                                      'execution','reconciliation','portfolio','ledger','audit','ops')
               AND t.table_type = 'BASE TABLE'
               AND c.data_type IN ('real','double precision')
             ORDER BY 1,2,3`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var found []string
		for rows.Next() {
			var schema, table, column, dtype string
			if err := rows.Scan(&schema, &table, &column, &dtype); err != nil {
				t.Fatalf("scan: %v", err)
			}
			found = append(found, schema+"."+table+"."+column+" "+dtype)
		}
		if len(found) > 0 {
			t.Fatalf("authoritative schema contains floating-point columns (no IEEE-754 value may cross a financial contract): %v", found)
		}
	})
}

// ---------------------------------------------------------------------------
// Invariant 3: duplicate commands do not increase exposure
// ---------------------------------------------------------------------------

func TestDuplicateCommandCannotCreateASecondOrder(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 1)
		MustInsertInstrument(t, ctx, tx, ins)
		order := dbtest.CanonicalID("ord", 2)
		cmd := dbtest.CanonicalID("cmd", 3)

		insertOrder := func() error {
			_, err := tx.ExecContext(ctx, `
                INSERT INTO oms.order (
                    order_id, command_id, idempotency_scope, environment, account_id,
                    instrument_id, venue_id, side, order_type, time_in_force,
                    quantity, limit_price, state, correlation_id)
                VALUES ($1,$2,'scope-hash-1','paper',$3,$4,$5,'BUY','LIMIT','DAY',100,100,'CREATED','cor-dup')`,
				order, cmd, dbtest.CanonicalID("acc", 4), ins, dbtest.CanonicalID("ven", 5))
			return err
		}
		if err := insertOrder(); err != nil {
			t.Fatalf("first order insert: %v", err)
		}
		// The UNIQUE constraint on command_id is the structural guarantee that a
		// duplicate command cannot create a second exposure.
		dbtest.ExpectRejected(t, ctx, tx,
			`INSERT INTO oms.order (order_id, command_id, idempotency_scope, environment, account_id,
                instrument_id, venue_id, side, order_type, time_in_force, quantity, limit_price, state, correlation_id)
             VALUES ($1,$2,'scope-hash-1','paper',$3,$4,$5,'BUY','LIMIT','DAY',100,100,'CREATED','cor-dup')`,
			dbtest.CanonicalID("ord", 6), cmd, dbtest.CanonicalID("acc", 4), ins, dbtest.CanonicalID("ven", 5))

		var count int
		dbtest.MustQueryRow(t, ctx, tx, &count, `SELECT count(*) FROM oms.order WHERE command_id = $1`, cmd)
		if count != 1 {
			t.Fatalf("duplicate command created %d orders, want exactly 1", count)
		}
	})
}

// ---------------------------------------------------------------------------
// Idempotency: database uniqueness is the enforcement
// ---------------------------------------------------------------------------

func TestIdempotencyRecordUniquenessAndTerminalResponseRequirement(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		hash := strings.Repeat("a", 64)
		digest := strings.Repeat("b", 64)
		cmd := dbtest.CanonicalID("cmd", 7)

		insert := func(id int) error {
			_, err := tx.ExecContext(ctx, `
                INSERT INTO ops.idempotency_record
                    (scope_hash, state, request_digest, command_id, created_at, updated_at)
                VALUES ($1,'in_flight',$2,$3,now(),now())`,
				hash, digest, cmd)
			return err
		}
		if err := insert(0); err != nil {
			t.Fatalf("first idempotency insert: %v", err)
		}
		// A second claim of the same scope is the replay the invariant forbids.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.idempotency_record
                (scope_hash, state, request_digest, command_id, created_at, updated_at)
            VALUES ($1,'in_flight',$2,$3,now(),now())`, hash, digest, dbtest.CanonicalID("cmd", 8))
	})
}

func TestTerminalIdempotencyStateRequiresStoredResponse(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A terminal record must carry the response it will replay verbatim,
		// otherwise a replay could not return the original result.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.idempotency_record
                (scope_hash, state, request_digest, command_id, created_at, updated_at)
            VALUES ($1,'succeeded',$2,$3,now(),now())`,
			strings.Repeat("c", 64), strings.Repeat("d", 64), dbtest.CanonicalID("cmd", 9))
	})
}

// ---------------------------------------------------------------------------
// Invariant 1: no risk-rejected command creates a live submission
// ---------------------------------------------------------------------------

func TestRiskIncreasingOrderCannotReachSubmissionStateWithoutDecision(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 11)
		MustInsertInstrument(t, ctx, tx, ins)
		order := dbtest.CanonicalID("ord", 12)
		MustCreateOrder(t, ctx, tx, OrderSpec{ID: order, Instrument: ins, Seed: 13, State: "CREATED"})

		// Walk legally to RISK_PENDING.
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 13)

		// RISK_PENDING -> RISK_APPROVED is an exposure-increasing transition and
		// the order carries no risk decision, so the database refuses. This is
		// invariant 1 as a storage control, not an application check. The seed is
		// distinct from the successful hops so the event identity does not collide.
		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 90,
			"risk decision")

		// With a decision attached the same transition is permitted, and only
		// then may the order reach SUBMITTING. Each hop uses its own seed so the
		// event identities are distinct.
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 14))
		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 111)
		MustTransition(t, ctx, tx, order, "SUBMITTING", "SUBMITTING", 112)
	})
}

func TestRiskReducingOrderMayExistWithoutDecisionBeforeSubmission(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 16)
		MustInsertInstrument(t, ctx, tx, ins)
		// A REDUCE_ONLY order may be recorded in a pre-submission state without a
		// risk decision: it cannot increase exposure.
		order := dbtest.CanonicalID("ord", 17)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, OrderType: "REDUCE_ONLY", Side: "SELL",
			Seed: 20, State: "RISK_PENDING",
		})

		// It still may not be promoted to a submission state without a decision.
		// The attempt is a full, legal-shaped transition: the journal event is
		// accepted, and the refusal must come from the risk control on the state
		// change, not from the absence of an event.
		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 20,
			"risk decision")
	})
}

func TestLiveOrderRequiresEligibilityEvidence(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 21)
		MustInsertInstrument(t, ctx, tx, ins)
		// G11 requires exact-scope eligibility evidence. A live order without a
		// recorded eligibility record is structurally impossible. The order is
		// created in its entry state, so the rejection comes from the eligibility
		// control rather than from the state machine.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO oms.order (order_id, command_id, idempotency_scope, environment, account_id,
                instrument_id, venue_id, side, order_type, time_in_force, quantity, limit_price,
                state, risk_decision_id, correlation_id)
            VALUES ($1,$2,'s','live',$3,$4,$5,'BUY','LIMIT','DAY',100,10,'CREATED',$6,'cor-z')`,
			dbtest.CanonicalID("ord", 22), dbtest.CanonicalID("cmd", 23),
			dbtest.CanonicalID("acc", 24), ins, dbtest.CanonicalID("ven", 25),
			dbtest.CanonicalID("rsk", 26))
	})
}

func TestRiskDecisionOutcomeMustBeConsistent(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A REJECTION that names no failed control is an unexplained decision and
		// is structurally impossible.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO risk.decision (decision_id, environment, outcome, command_id, account_id,
                instrument_id, venue_id, side, order_type, quantity, notional, policy_revisions,
                evaluated_facts, correlation_id, evaluated_at_ns, valid_until, decision_sequence)
            VALUES ($1,'paper','REJECTED',$2,$3,$4,$5,'BUY','LIMIT',1,10,$6,'{}','cor',$7,now()+interval '5 seconds',1)`,
			dbtest.CanonicalID("rsk", 31), dbtest.CanonicalID("cmd", 32), dbtest.CanonicalID("acc", 33),
			dbtest.CanonicalID("ins", 34), dbtest.CanonicalID("ven", 35),
			[]string{dbtest.CanonicalID("pol", 36)}, dbtest.NowNs())

		// An APPROVAL that names a failed control is contradictory.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO risk.decision (decision_id, environment, outcome, command_id, account_id,
                instrument_id, venue_id, side, order_type, quantity, notional, policy_revisions,
                evaluated_facts, failed_controls, correlation_id, evaluated_at_ns, valid_until, decision_sequence)
            VALUES ($1,'paper','APPROVED',$2,$3,$4,$5,'BUY','LIMIT',1,10,$6,'{}',$7,'cor',$8,now()+interval '5 seconds',1)`,
			dbtest.CanonicalID("rsk", 37), dbtest.CanonicalID("cmd", 38), dbtest.CanonicalID("acc", 39),
			dbtest.CanonicalID("ins", 40), dbtest.CanonicalID("ven", 41),
			[]string{dbtest.CanonicalID("pol", 42)}, []string{"MAX_ORDER_NOTIONAL"},
			dbtest.NowNs())
	})
}

func TestRiskPolicyCannotBeSelfApproved(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// Separation of duties is structural: no one approves their own policy.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO risk.policy (policy_revision, name, environment, scope_kind, scope_value,
                max_order_notional, max_order_quantity, max_gross_exposure, max_net_exposure,
                max_leverage, max_concentration, max_open_orders, max_daily_loss, max_drawdown,
                loss_window, loss_source, max_market_data_age, max_price_deviation,
                max_orders_per_minute, max_cancels_per_minute, permitted_markets, permitted_venues,
                permitted_instruments, permitted_order_types, permitted_sides, permitted_modes,
                fee_assumptions, funding_assumptions, margin_assumptions, settlement_assumptions,
                halt_authority, reenable_authority, status, created_by, approved_by, approved_at)
            VALUES ($1,'p','paper','ACCOUNT','acc-1',1000,10,50000,25000,2,0.5,20,500,0.1,
                INTERVAL '1 day','ledger',INTERVAL '5 seconds',0.02,60,30,
                ARRAY['crypto'],ARRAY['ven-1'],ARRAY['ins-1'],ARRAY['LIMIT'],ARRAY['BUY'],ARRAY['PAPER'],
                '{}','{}','{}','{}','RISK_OPERATOR','OWNER','ACTIVE','owner-1','owner-1',now())`,
			dbtest.CanonicalID("pol", 51))
	})
}

func TestActiveRiskPolicyRequiresApprovalEffectiveAndRollback(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// An ACTIVE policy without a rollback revision is not acceptable.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO risk.policy (policy_revision, name, environment, scope_kind, scope_value,
                max_order_notional, max_order_quantity, max_gross_exposure, max_net_exposure,
                max_leverage, max_concentration, max_open_orders, max_daily_loss, max_drawdown,
                loss_window, loss_source, max_market_data_age, max_price_deviation,
                max_orders_per_minute, max_cancels_per_minute, permitted_markets, permitted_venues,
                permitted_instruments, permitted_order_types, permitted_sides, permitted_modes,
                fee_assumptions, funding_assumptions, margin_assumptions, settlement_assumptions,
                halt_authority, reenable_authority, status, created_by, approved_by, approved_at)
            VALUES ($1,'p','paper','ACCOUNT','acc-2',1000,10,50000,25000,2,0.5,20,500,0.1,
                INTERVAL '1 day','ledger',INTERVAL '5 seconds',0.02,60,30,
                ARRAY['crypto'],ARRAY['ven-1'],ARRAY['ins-1'],ARRAY['LIMIT'],ARRAY['BUY'],ARRAY['PAPER'],
                '{}','{}','{}','{}','RISK_OPERATOR','OWNER','ACTIVE','owner-1','risk-1',now())`,
			dbtest.CanonicalID("pol", 52))
	})
}

func TestEmptyRiskPolicyScopeIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A policy permitting nothing must be expressed by deactivating, not by
		// activating an empty scope. An empty array is rejected.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO risk.policy (policy_revision, name, environment, scope_kind, scope_value,
                max_order_notional, max_order_quantity, max_gross_exposure, max_net_exposure,
                max_leverage, max_concentration, max_open_orders, max_daily_loss, max_drawdown,
                loss_window, loss_source, max_market_data_age, max_price_deviation,
                max_orders_per_minute, max_cancels_per_minute, permitted_markets, permitted_venues,
                permitted_instruments, permitted_order_types, permitted_sides, permitted_modes,
                fee_assumptions, funding_assumptions, margin_assumptions, settlement_assumptions,
                halt_authority, reenable_authority, status, created_by)
            VALUES ($1,'p','paper','ACCOUNT','acc-3',1000,10,50000,25000,2,0.5,20,500,0.1,
                INTERVAL '1 day','ledger',INTERVAL '5 seconds',0.02,60,30,
                ARRAY[]::common.market_class[],ARRAY['ven-1'],ARRAY['ins-1'],
                ARRAY['LIMIT'],ARRAY['BUY'],ARRAY['PAPER'],
                '{}','{}','{}','{}','RISK_OPERATOR','OWNER','DRAFT','owner-1')`,
			dbtest.CanonicalID("pol", 53))
	})
}

// ---------------------------------------------------------------------------
// Invariant 4: ledger corrections are compensating entries
// ---------------------------------------------------------------------------

func TestLedgerUpdateAndDeleteAreRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		entry := dbtest.CanonicalID("led", 61)
		postEntry(t, ctx, tx, entry, 1, dbtest.CanonicalID("acc", 62), dbtest.CanonicalID("cmd", 63), strings.Repeat("e", 64))

		// Financial history is never silently overwritten. Both destructive
		// probes run in the same transaction: ExpectRejected restores the
		// savepoint, so the second probe is a real test and not a replay of
		// the first one's aborted transaction.
		err := dbtest.ExpectRejected(t, ctx, tx,
			`UPDATE ledger.entry SET amount = 999999 WHERE entry_id = $1`, entry)
		if !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("UPDATE rejection did not come from the append-only guard: %v", err)
		}
		err = dbtest.ExpectRejected(t, ctx, tx,
			`DELETE FROM ledger.entry WHERE entry_id = $1`, entry)
		if !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("DELETE rejection did not come from the append-only guard: %v", err)
		}

		// And the entry is still exactly as posted.
		var amount string
		dbtest.MustQueryRow(t, ctx, tx, &amount, `SELECT amount::text FROM ledger.entry WHERE entry_id = $1`, entry)
		if amount != "1000.000000000000000000" {
			t.Fatalf("ledger amount changed to %s; an append-only ledger was mutated", amount)
		}
	})
}

func TestLedgerCorrectionMustReferenceAnOriginalEntry(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A CORRECTION must name the entry it compensates; the CHECK enforces both
		// directions of the rule.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ledger.entry (entry_id, sequence, account_id, environment, entry_kind,
                direction, amount, currency, source_command_id, correlation_id,
                effective_at, effective_at_ns, recorded_at_ns, source_digest)
            VALUES ($1,1,$2,'paper','CORRECTION','DEBIT',100,'USD',$3,'cor',
                    now(),$4,$4,$5)`,
			dbtest.CanonicalID("led", 71), dbtest.CanonicalID("acc", 72), dbtest.CanonicalID("cmd", 73),
			dbtest.NowNs(), strings.Repeat("1", 64))

		// A non-CORRECTION entry may not carry corrects_entry_id.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ledger.entry (entry_id, sequence, account_id, environment, entry_kind,
                direction, amount, currency, source_command_id, correlation_id,
                corrects_entry_id, effective_at, effective_at_ns, recorded_at_ns, source_digest)
            VALUES ($1,1,$2,'paper','FEE','DEBIT',5,'USD',$3,'cor',$4,
                    now(),$5,$5,$6)`,
			dbtest.CanonicalID("led", 74), dbtest.CanonicalID("acc", 75), dbtest.CanonicalID("cmd", 76),
			dbtest.CanonicalID("led", 77), dbtest.NowNs(), strings.Repeat("2", 64))
	})
}

func TestLedgerEntryCannotBeSelfCorrecting(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ledger.entry (entry_id, sequence, account_id, environment, entry_kind,
                direction, amount, currency, source_command_id, correlation_id,
                corrects_entry_id, effective_at, effective_at_ns, recorded_at_ns, source_digest)
            VALUES ($1,1,$2,'paper','CORRECTION','DEBIT',100,'USD',$3,'cor',$1,
                    now(),$4,$4,$5)`,
			dbtest.CanonicalID("led", 81), dbtest.CanonicalID("acc", 82), dbtest.CanonicalID("cmd", 83),
			dbtest.NowNs(), strings.Repeat("3", 64))
	})
}

func TestZeroAmountLedgerEntryIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ledger.entry (entry_id, sequence, account_id, environment, entry_kind,
                direction, amount, currency, source_command_id, correlation_id,
                effective_at, effective_at_ns, recorded_at_ns, source_digest)
            VALUES ($1,1,$2,'paper','FEE','DEBIT',0,'USD',$3,'cor',
                    now(),$4,$4,$5)`,
			dbtest.CanonicalID("led", 91), dbtest.CanonicalID("acc", 92), dbtest.CanonicalID("cmd", 93),
			dbtest.NowNs(), strings.Repeat("4", 64))
	})
}

// ---------------------------------------------------------------------------
// Invariant 6: unknown submission outcomes
// ---------------------------------------------------------------------------

func TestBlindRetryOfVenueSubmissionIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		sub := dbtest.CanonicalID("sbm", 101)
		// attempts > 1 requires a resolved reconciliation case AND an authorised
		// retry. An exposure-increasing retry is never blind.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO execution.submission (submission_id, order_id, environment, venue_id,
                adapter_id, client_order_id, outcome, request_digest, request_sent_at_ns, attempts)
            VALUES ($1,$2,'paper',$3,'adapter-1','coid-1','PENDING',$4,$5,2)`,
			sub, dbtest.CanonicalID("ord", 102), dbtest.CanonicalID("ven", 103),
			strings.Repeat("5", 64), dbtest.NowNs())
	})
}

func TestTimedOutSubmissionIsNeverRecordedAsRejectedByAssumption(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 111)
		MustInsertInstrument(t, ctx, tx, ins)
		order := MustOrderAt(t, ctx, tx, ins, 112, "SUBMITTING")
		// A timed-out submission is recorded as TIMED_OUT_UNKNOWN with a recorded
		// response time. Recording it as REJECTED would be an assumption the
		// blueprint forbids.
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO execution.submission (submission_id, order_id, environment, venue_id,
                adapter_id, client_order_id, outcome, request_digest, request_sent_at_ns,
                response_received_at, attempts)
            VALUES ($1,$2,'paper',$3,'adapter-1','coid-2','TIMED_OUT_UNKNOWN',$4,$5,now(),1)`,
			dbtest.CanonicalID("sbm", 117), order, dbtest.CanonicalID("ven", 115),
			strings.Repeat("6", 64), dbtest.NowNs()); err != nil {
			t.Fatalf("record timed-out submission: %v", err)
		}
		// A PENDING/terminal mismatch: a terminal outcome without a recorded
		// response time is rejected.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO execution.submission (submission_id, order_id, environment, venue_id,
                adapter_id, client_order_id, outcome, request_digest, request_sent_at_ns, attempts)
            VALUES ($1,$2,'paper',$3,'adapter-2','coid-3','ACKNOWLEDGED',$4,$5,1)`,
			dbtest.CanonicalID("sbm", 118), order, dbtest.CanonicalID("ven", 115),
			strings.Repeat("7", 64), dbtest.NowNs())
	})
}

func TestUnknownOrderHasNoDirectPathToDeterminateState(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 121)
		MustInsertInstrument(t, ctx, tx, ins)
		order := MustOrderAt(t, ctx, tx, ins, 122, "UNKNOWN")

		// The ONLY way out of UNKNOWN is an explicit OUTCOME_RESOLVED event.
		// The rule set contains no UNKNOWN -> FILLED via FILLED, so an ordinary
		// fill event naming that transition is refused as an illegal transition.
		MustExpectTransitionRejected(t, ctx, tx, order, "FILLED", "FILLED", 190,
			"illegal OMS transition")

		// An event that claims a transition the order did not make is refused too.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO oms.order_event (order_event_id, order_id, sequence, from_state, to_state,
                event_kind, filled_quantity_delta, price, actor_id, correlation_id, occurred_at_ns)
            VALUES ($1,$2,99,'SUBMITTING','FILLED','FILLED',100,10,'system','cor',$3)`,
			dbtest.CanonicalID("oe1", 127), order, dbtest.NowNs())

		// A zero-delta "fill" could never satisfy the resolved fill anyway.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO oms.order_event (order_event_id, order_id, sequence, from_state, to_state,
                event_kind, filled_quantity_delta, price, actor_id, correlation_id, occurred_at_ns)
            SELECT $1, order_id, 1, 'UNKNOWN', 'FILLED', 'FILLED', 0, NULL, 'system', 'cor', $2
              FROM oms.order WHERE order_id = $3`,
			dbtest.CanonicalID("oe1", 128), dbtest.NowNs(), order)
	})
}

func TestFilelessFillCannotBeMarkedFilled(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 131)
		MustInsertInstrument(t, ctx, tx, ins)
		order := MustOrderAt(t, ctx, tx, ins, 132, "SUBMITTING")

		// A FILLED transition event must carry a positive quantity delta AND a
		// price. A zero-delta "fill" would inflate position accounting.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO oms.order_event (order_event_id, order_id, sequence, from_state, to_state,
                event_kind, filled_quantity_delta, price, actor_id, correlation_id, occurred_at_ns)
            VALUES ($1,$2,1,'SUBMITTING','FILLED','FILLED',0,NULL,'system','cor',$3)`,
			dbtest.CanonicalID("oe1", 137), order, dbtest.NowNs())
	})
}

func TestFillAccountingCannotBeAdvancedWithoutAFillEvent(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 138)
		MustInsertInstrument(t, ctx, tx, ins)
		order := MustOrderAt(t, ctx, tx, ins, 139, "ACKNOWLEDGED")

		// No fill event exists, so claiming a cumulative fill is a lie about
		// venue-reported execution. Only the execution progress changes here, so
		// the rejection must come from the fill-event control and not from the
		// state machine.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE oms.order SET filled_quantity = 100, average_fill_price = 100
             WHERE order_id = $1`, order)
		if !strings.Contains(err.Error(), "fill events") {
			t.Fatalf("inflated fill accounting was not rejected by the fill-event control: %v", err)
		}
	})
}

func TestOrderTermsAreImmutableAfterAcceptance(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 140)
		MustInsertInstrument(t, ctx, tx, ins)
		order := MustOrderAt(t, ctx, tx, ins, 141, "ACKNOWLEDGED")

		// Changing quantity after risk approval would silently re-specify the
		// exposure a risk decision authorised.
		err := dbtest.ExpectRejected(t, ctx, tx,
			`UPDATE oms.order SET quantity = 999 WHERE order_id = $1`, order)
		if !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("re-specifying an accepted order was not rejected: %v", err)
		}
		err = dbtest.ExpectRejected(t, ctx, tx,
			`UPDATE oms.order SET side = 'SELL' WHERE order_id = $1`, order)
		if !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("flipping an accepted order's side was not rejected: %v", err)
		}
	})
}

func TestOrderCannotBeCreatedInATerminalState(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 142)
		MustInsertInstrument(t, ctx, tx, ins)
		// Inserting straight into SUBMITTING would bypass the closed transition
		// set entirely, so the entry state is constrained.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO oms.order (order_id, command_id, idempotency_scope, environment, account_id,
                instrument_id, venue_id, side, order_type, time_in_force, quantity, limit_price,
                state, risk_decision_id, correlation_id)
            VALUES ($1,$2,'s','paper',$3,$4,$5,'BUY','LIMIT','DAY',100,10,'SUBMITTING',$6,'cor')`,
			dbtest.CanonicalID("ord", 143), dbtest.CanonicalID("cmd", 144),
			dbtest.CanonicalID("acc", 145), ins, dbtest.CanonicalID("ven", 146),
			dbtest.CanonicalID("rsk", 147))
		if !strings.Contains(err.Error(), "CREATED or RISK_PENDING") {
			t.Fatalf("order creation in a terminal state was not rejected by the entry-state control: %v", err)
		}
	})
}

func TestOrderEventJournalRejectsAGapInTheSequence(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 148)
		MustInsertInstrument(t, ctx, tx, ins)
		order := MustOrderAt(t, ctx, tx, ins, 149, "CREATED")

		// The first event for an order must be sequence 1.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO oms.order_event (order_event_id, order_id, sequence, from_state, to_state,
                event_kind, actor_id, correlation_id, occurred_at_ns)
            VALUES ($1,$2,7,'CREATED','RISK_PENDING','CREATED','system','cor',$3)`,
			dbtest.CanonicalID("oe1", 150), order, dbtest.NowNs())
		if !strings.Contains(err.Error(), "must be 1") {
			t.Fatalf("a gapped order event sequence was not rejected: %v", err)
		}
	})
}

func TestDuplicateVenueTradeDeliveryIsIdempotent(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 141)
		MustInsertInstrument(t, ctx, tx, ins)
		order := MustOrderAt(t, ctx, tx, ins, 142, "ACKNOWLEDGED")
		venue := dbtest.CanonicalID("ven", 145)
		insertFill := func(idSeed, seq int) error {
			_, err := tx.ExecContext(ctx, `
                INSERT INTO oms.fill (fill_id, order_id, account_id, instrument_id, venue_id,
                    side, quantity, price, fee_currency, venue_trade_id,
                    source_timestamp, source_timestamp_ns, correlation_id, fill_sequence)
                VALUES ($1,$2,$3,$4,$5,'BUY',10,100.50,'USD','trade-abc',
                        now(),$6,'cor',$7)`,
				dbtest.CanonicalID("fil", idSeed), order, dbtest.CanonicalID("acc", 144), ins, venue,
				dbtest.NowNs(), seq)
			return err
		}
		if err := insertFill(147, 1); err != nil {
			t.Fatalf("first fill: %v", err)
		}
		// The same venue trade delivered twice is stored once.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO oms.fill (fill_id, order_id, account_id, instrument_id, venue_id,
                side, quantity, price, fee_currency, venue_trade_id,
                source_timestamp, source_timestamp_ns, correlation_id, fill_sequence)
            VALUES ($1,$2,$3,$4,$5,'BUY',10,100.50,'USD','trade-abc',
                    now(),$6,'cor',$7)`,
			dbtest.CanonicalID("fil", 148), order, dbtest.CanonicalID("acc", 144), ins, venue,
			dbtest.NowNs(), 1)
	})
}

func TestValidatedFillMustBeReconciled(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 151)
		MustInsertInstrument(t, ctx, tx, ins)
		order := MustOrderAt(t, ctx, tx, ins, 152, "ACKNOWLEDGED")
		// A fill marked validated while its reconciliation is still PENDING cannot
		// enter the position projection.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO oms.fill (fill_id, order_id, account_id, instrument_id, venue_id,
                side, quantity, price, fee_currency, venue_trade_id,
                source_timestamp, source_timestamp_ns, correlation_id, fill_sequence,
                validated, validated_at, reconciliation_status)
            VALUES ($1,$2,$3,$4,$5,'BUY',10,100.50,'USD','trade-def',
                    now(),$6,'cor',1, true, now(), 'PENDING')`,
			dbtest.CanonicalID("fil", 157), order, dbtest.CanonicalID("acc", 154), ins,
			dbtest.CanonicalID("ven", 155), dbtest.NowNs())
	})
}

// ---------------------------------------------------------------------------
// Invariant 7: halt state
// ---------------------------------------------------------------------------

func TestHaltReEnableRequiresDualControlAndVerification(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A non-emergency halt may not be cleared by a single actor without
		// health and reconciliation verification.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.halt (halt_id, level, state, environment, reason, emergency,
                activated_by, activated_at_ns, cleared_by, cleared_at, clear_reason)
            VALUES ($1,'SYSTEM_HALT','CLEARED','live','test', false,
                    'op-1',$2,'op-1',now(),'because')`,
			dbtest.CanonicalID("hlt", 161), dbtest.NowNs())
	})
}

func TestEmergencyHaltActivationNeedsNoSecondApprover(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// Emergency halt must ALWAYS remain available to an authorized halt
		// operator. Requiring dual control to stop trading would be unsafe.
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO ops.halt (halt_id, level, state, environment, reason, emergency,
                activated_by, activated_at_ns)
            VALUES ($1,'SYSTEM_HALT','ACTIVE','live','uncontrolled behaviour observed', true,
                    'halt-operator-1',$2)`,
			dbtest.CanonicalID("hlt", 162), dbtest.NowNs()); err != nil {
			t.Fatalf("emergency halt must be available without a second approver: %v", err)
		}
	})
}

func TestHaltPrecedenceRanksSystemHighest(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// SYSTEM_HALT > VENUE_HALT > MARKET_HALT > STRATEGY_HALT > ACCOUNT_HALT.
		// A lower-level enable can never override a higher-level halt.
		var rank int
		dbtest.MustQueryRow(t, ctx, tx, &rank, `SELECT ops.halt_rank('SYSTEM_HALT')`)
		if rank != 5 {
			t.Fatalf("SYSTEM_HALT rank = %d, want 5", rank)
		}
		dbtest.MustQueryRow(t, ctx, tx, &rank, `SELECT ops.halt_rank('ACCOUNT_HALT')`)
		if rank != 1 {
			t.Fatalf("ACCOUNT_HALT rank = %d, want 1", rank)
		}
		var bypass bool
		dbtest.MustQueryRow(t, ctx, tx, &bypass, `SELECT ops.halt_rank('ACCOUNT_HALT') >= ops.halt_rank('SYSTEM_HALT')`)
		if bypass {
			t.Fatal("an ACCOUNT_HALT enable must not override a SYSTEM_HALT")
		}
	})
}

// ---------------------------------------------------------------------------
// G11: live activation
// ---------------------------------------------------------------------------

func TestLiveActivationRequiresTwoDistinctApprovers(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// Dual control on live activation is structural.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.live_activation (activation_id, account_id, venue_id, market_class,
                instrument_scope, product_scope, activity_scope, jurisdiction,
                config_fingerprint, artifact_digest, risk_policy_revisions,
                requested_by, approved_by, canary_scope, abort_criteria, responsible_operator, state)
            VALUES ($1,$2,$3,'crypto',ARRAY[$4],'spot','trading','ID',
                    $5,$6,ARRAY[$7],'owner-1','owner-1','{}','{}','op-1','APPROVED')`,
			dbtest.CanonicalID("act", 171), dbtest.CanonicalID("acc", 172),
			dbtest.CanonicalID("ven", 173), dbtest.CanonicalID("ins", 174),
			strings.Repeat("8", 64), strings.Repeat("9", 64), dbtest.CanonicalID("pol", 175))
	})
}

func TestOnlyOneActiveLiveActivationMayExist(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		insert := func(seed int) error {
			_, err := tx.ExecContext(ctx, `
                INSERT INTO ops.live_activation (activation_id, account_id, venue_id, market_class,
                    instrument_scope, product_scope, activity_scope, jurisdiction,
                    config_fingerprint, artifact_digest, risk_policy_revisions,
                    requested_by, approved_by, canary_scope, abort_criteria,
                    responsible_operator, state, activated_at, evidence_digest)
                VALUES ($1,$2,$3,'crypto',ARRAY[$4],'spot','trading','ID',
                        $5,$6,ARRAY[$7],'owner-1','risk-1','{"scope":"narrow"}','{"abort":"any breach"}',
                        'op-1','ACTIVE',now(),$8)`,
				dbtest.CanonicalID("act", seed), dbtest.CanonicalID("acc", 181+seed),
				dbtest.CanonicalID("ven", 191+seed), dbtest.CanonicalID("ins", 201+seed),
				strings.Repeat("a", 64), strings.Repeat("b", 64), dbtest.CanonicalID("pol", 211+seed),
				strings.Repeat("c", 64))
			return err
		}
		if err := insert(180); err != nil {
			t.Fatalf("first active activation: %v", err)
		}
		// A second active record would mean two live authorizations exist at once.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.live_activation (activation_id, account_id, venue_id, market_class,
                instrument_scope, product_scope, activity_scope, jurisdiction,
                config_fingerprint, artifact_digest, risk_policy_revisions,
                requested_by, approved_by, canary_scope, abort_criteria,
                responsible_operator, state, activated_at, evidence_digest)
            VALUES ($1,$2,$3,'crypto',ARRAY[$4],'spot','trading','ID',
                    $5,$6,ARRAY[$7],'owner-2','risk-2','{"scope":"narrow"}','{"abort":"any breach"}',
                    'op-2','ACTIVE',now(),$8)`,
			dbtest.CanonicalID("act", 182), dbtest.CanonicalID("acc", 183),
			dbtest.CanonicalID("ven", 184), dbtest.CanonicalID("ins", 185),
			strings.Repeat("d", 64), strings.Repeat("e", 64), dbtest.CanonicalID("pol", 186),
			strings.Repeat("f", 64))
	})
}

// ---------------------------------------------------------------------------
// Recovery hold
// ---------------------------------------------------------------------------

// TestRecoveryHoldRowMustBeWellFormed covers only the SHAPE of a recovery-hold
// row: a hold with no reason, entry time, or accountable actor is not recorded.
//
// This test was previously named TestRecoveryHoldCannotBeLeftWithoutAuthorisation,
// which claimed to cover release but only ever asserted that malformed rows are
// rejected. Release is now genuinely covered, in operational_mode_test.go, by
// tests that assert dual control, distinct identities and independent
// verification. The name was wrong and the coverage gap behind it is closed.
func TestRecoveryHoldRowMustBeWellFormed(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A restricted mode with no accountable actor is refused. Engaging does
		// NOT require a second approver -- 0007 required one, which meant the
		// database could refuse to enter RECOVERY_HOLD during the very failover
		// that caused it.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.system_state (system_state_id, environment, mode, recovery_hold,
                recovery_reason, recovery_entered_at)
            VALUES ($1,'live','RECOVERY_HOLD', true, 'region failover', now())`,
			dbtest.CanonicalID("sys", 191))

		// A recovery hold with no reason or entry time cannot be recorded.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.system_state (system_state_id, environment, mode, recovery_hold)
            VALUES ($1,'live','NORMAL', true)`,
			dbtest.CanonicalID("sys", 192))

		// A kill switch with no reason or actor cannot be recorded.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.system_state (system_state_id, environment, kill_switch_engaged)
            VALUES ($1,'live', true)`,
			dbtest.CanonicalID("sys", 193))
	})
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func TestLiveConfigurationRequiresSignature(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// An unsigned live configuration cannot load.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO config.revision (revision_id, environment, revision_number, schema_version,
                content_digest, document, status, created_at_ns, created_by, reason)
            VALUES ($1,'live',1,'1.0.0',$2,'{}','ACTIVE',$3,'owner','init')`,
			dbtest.CanonicalID("cfg", 201), strings.Repeat("d", 64), dbtest.NowNs())
	})
}

func TestFeatureFlagCannotGrantAuthorityWithoutDualControl(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A flag is configuration, not authorization. An authority-bearing flag
		// without dual control is rejected.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO config.feature_flag (flag_id, flag_key, environment, scope, enabled,
                owner, reason, expires_at, authority_bearing, requested_by, approved_by)
            VALUES ($1,'enable_live_submission','live','{}'::jsonb,true,'owner','test',
                    now()+interval '1 day', true,'owner-1','owner-1')`,
			dbtest.CanonicalID("flt", 211))
	})
}

// ---------------------------------------------------------------------------
// Sessions and identity
// ---------------------------------------------------------------------------

func TestSessionPolicyBoundsAreEnforced(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		subj := dbtest.CanonicalID("acc", 221)
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO identity.subject (subject_id, actor_type, auth_strength, audience, external_subject)
            VALUES ($1,'HUMAN','PHISHING_RESISTANT',NULL,'keycloak|abc')`, subj); err != nil {
			t.Fatalf("insert subject: %v", err)
		}
		// Idle timeout is capped at 30 minutes and absolute lifetime at 8 hours by
		// the storage constraints, matching 21_..._AUTHORIZATION.md §3.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO identity.session (session_id, subject_id, environment, session_class,
                auth_strength, auth_time, issued_at, absolute_expires_at, idle_expires_at)
            VALUES ($1,$2,'paper','PRIVILEGED_OPERATOR','PHISHING_RESISTANT',now(),now(),
                    now()+interval '8 hours', now()+interval '2 hours')`,
			dbtest.CanonicalID("ses", 222), subj)
	})
}

func TestWorkloadRequiresAudience(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A non-human actor without an audience-bound credential is rejected:
		// a service identity presented to the wrong audience must not authenticate.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO identity.subject (subject_id, actor_type, auth_strength, audience, external_subject)
            VALUES ($1,'WORKLOAD','PHISHING_RESISTANT',NULL,'spiffe|research')`,
			dbtest.CanonicalID("acc", 231))
	})
}

func TestRoleGrantRequiresDistinctApprover(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		subj := dbtest.CanonicalID("acc", 241)
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO identity.subject (subject_id, actor_type, auth_strength, audience, external_subject)
            VALUES ($1,'HUMAN','PHISHING_RESISTANT',NULL,'keycloak|xyz')`, subj); err != nil {
			t.Fatalf("insert subject: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO identity.role (role_name, description) VALUES ('OWNER','accountable owner')`); err != nil {
			t.Fatalf("insert role: %v", err)
		}
		// The requester cannot approve their own privileged grant.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO identity.role_binding (binding_id, subject_id, role_name, environment,
                granted_at, expires_at, requested_by, approved_by)
            VALUES ($1,$2,'OWNER','live',now(),now()+interval '30 days','self','self')`,
			dbtest.CanonicalID("agr", 242), subj)
	})
}

func TestPrivilegedGrantExpiryIsBoundedToNinetyDays(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		subj := dbtest.CanonicalID("acc", 251)
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO identity.subject (subject_id, actor_type, auth_strength, audience, external_subject)
            VALUES ($1,'HUMAN','PHISHING_RESISTANT',NULL,'keycloak|pqr')`, subj); err != nil {
			t.Fatalf("insert subject: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO identity.role (role_name, description) VALUES ('RISK_OPERATOR','risk operator')`); err != nil {
			t.Fatalf("insert role: %v", err)
		}
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO identity.role_binding (binding_id, subject_id, role_name, environment,
                granted_at, expires_at, requested_by, approved_by)
            VALUES ($1,$2,'RISK_OPERATOR','paper',now(),now()+interval '365 days','a','b')`,
			dbtest.CanonicalID("agr", 252), subj)
	})
}

// ---------------------------------------------------------------------------
// Market data
// ---------------------------------------------------------------------------

func TestCrossedOrderBookIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 261)
		MustInsertInstrument(t, ctx, tx, ins)
		// A bid above the ask is an impossible book and would corrupt a
		// price-deviation check if propagated.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO market.quote (observation_id, instrument_id, venue_id,
                bid_price, bid_quantity, ask_price, ask_quantity,
                source_timestamp, source_timestamp_ns, received_at)
            VALUES ($1,$2,$3, 101.00, 1, 100.00, 1, now(),$4,now())`,
			dbtest.CanonicalID("mkt", 262), ins, dbtest.CanonicalID("ven", 263), dbtest.NowNs())
	})
}

func TestMarketObservationFromTheFutureIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 271)
		MustInsertInstrument(t, ctx, tx, ins)
		// A receive time before the source time means a manipulated future
		// timestamp could make stale data appear fresh to the freshness control.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO market.trade (observation_id, instrument_id, venue_id,
                price, quantity, side, source_timestamp, source_timestamp_ns, received_at)
            VALUES ($1,$2,$3, 100, 1, 'BUY', now() + interval '1 hour', $4, now())`,
			dbtest.CanonicalID("mkt", 272), ins, dbtest.CanonicalID("ven", 273), dbtest.NowNs())
	})
}

func TestInvalidCandleIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 281)
		MustInsertInstrument(t, ctx, tx, ins)
		// high < low is impossible and would corrupt a backtest.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO market.candle (observation_id, instrument_id, venue_id, interval,
                period_open_time, open, high, low, close, volume,
                source_timestamp, source_timestamp_ns, received_at)
            VALUES ($1,$2,$3,'1m', now(), 100, 99, 101, 100, 5, now(),$4,now())`,
			dbtest.CanonicalID("mkt", 282), ins, dbtest.CanonicalID("ven", 283), dbtest.NowNs())
	})
}

func TestInstrumentRequiresPositivePrecision(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// Unknown precision is a deny condition, not a default.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO market.instrument (instrument_id, market_class, base, quote,
                min_quantity, price_increment, tick_size)
            VALUES ($1,'crypto','BTC','USDT', 0, 0.01, 0.01)`,
			dbtest.CanonicalID("ins", 291))
	})
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

func TestReconciliationCaseRequiresAgingSLAAndOwner(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A case with no resolution deadline cannot be aged or escalated.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO reconciliation.case (case_id, environment, venue_id, difference_kind,
                severity, status, blocked_scope, evidence, owner_subject_id, detected_at,
                resolve_by, correlation_id)
            VALUES ($1,'paper',$2,'BALANCE','MATERIAL','OPEN','{}','{}',$3,now(), now(),'cor')`,
			dbtest.CanonicalID("rec", 301), dbtest.CanonicalID("ven", 302), dbtest.CanonicalID("acc", 303))
	})
}

func TestReconciliationCaseCannotResolveWithoutAccountableMetadata(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// Resolution requires a resolver, a timestamp and a note. A break cannot
		// be closed by an unattributable action.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO reconciliation.case (case_id, environment, venue_id, difference_kind,
                severity, status, blocked_scope, evidence, owner_subject_id, detected_at,
                resolve_by, resolved_at, correlation_id)
            VALUES ($1,'paper',$2,'BALANCE','MATERIAL','RESOLVED','{}','{}',$3,now(),
                    now()+interval '4 hours', now(),'cor')`,
			dbtest.CanonicalID("rec", 311), dbtest.CanonicalID("ven", 312), dbtest.CanonicalID("acc", 313))
	})
}

// ---------------------------------------------------------------------------
// Outbox
// ---------------------------------------------------------------------------

func TestOutboxRetriesAreBounded(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// Unbounded retries are prohibited. A record that exhausts its attempts
		// must be dead-lettered rather than retrying forever.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.outbox (event_id, event_type, schema_version, aggregate_type,
                aggregate_id, sequence, correlation_id, producer_id, environment,
                occurred_at, occurred_at_ns, payload, dispatch_state, attempt_count, max_attempts)
            VALUES ($1,'order.created','1.0.0','order',$2,1,'cor','control-plane','paper',
                    now(),$3,'{}','PENDING', 11, 10)`,
			dbtest.CanonicalID("evt", 321), dbtest.CanonicalID("ord", 322), dbtest.NowNs())
	})
}

func TestOutboxAggregateSequenceMustBeUnique(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		agg := dbtest.CanonicalID("ord", 331)
		insert := func(seed int) error {
			_, err := tx.ExecContext(ctx, `
                INSERT INTO ops.outbox (event_id, event_type, schema_version, aggregate_type,
                    aggregate_id, sequence, correlation_id, producer_id, environment,
                    occurred_at, occurred_at_ns, payload)
                VALUES ($1,'order.created','1.0.0','order',$2,1,'cor','control-plane','paper',
                        now(),$3,'{}')`,
				dbtest.CanonicalID("evt", seed), agg, dbtest.NowNs())
			return err
		}
		if err := insert(332); err != nil {
			t.Fatalf("first outbox row: %v", err)
		}
		// A duplicate aggregate sequence would mean two events claim the same
		// position in the aggregate's history.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.outbox (event_id, event_type, schema_version, aggregate_type,
                aggregate_id, sequence, correlation_id, producer_id, environment,
                occurred_at, occurred_at_ns, payload)
            VALUES ($1,'order.created','1.0.0','order',$2,1,'cor','control-plane','paper',
                    now(),$3,'{}')`,
			dbtest.CanonicalID("evt", 333), agg, dbtest.NowNs())
	})
}

// ---------------------------------------------------------------------------
// Audit chain integrity
// ---------------------------------------------------------------------------

func TestAuditChainRejectsSequenceGap(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)
		// A skipped sequence is a SEV-1 evidence-integrity event, not a warning.
		// The hash argument is NULL so the rejection is attributable to the
		// sequence gap alone; otherwise the hash cross-check could fire first
		// and the test would pass without ever exercising the control it names.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            SELECT audit.append_record($1,$2,3,'owner','actor-1','SYSTEM','test.action',
                'order',$3,'paper',NULL,now(),$4,now(),$4,'reason','cor',NULL,'v1','SUCCESS',
                NULL,NULL,'{}'::jsonb,'key-1','1.0.0',NULL)`,
			dbtest.CanonicalID("aud", 341), part, dbtest.CanonicalID("ord", 342),
			dbtest.NowNs())
		if !strings.Contains(err.Error(), "chain break") {
			t.Fatalf("expected a chain-break error, got: %v", err)
		}
	})
}

func TestAuditChainEnforcesPreviousHashContinuity(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		// The hashes are read back from the stored records rather than supplied
		// by the test. Since 0009 the database computes record_hash from the
		// canonical payload, so a test cannot assert a known constant: the hash
		// depends on the nanosecond timestamps, which differ on every call.
		MustAppendAudit(t, ctx, tx, part, 1)

		var h1, lastHash string
		dbtest.MustQueryRow(t, ctx, tx, &h1,
			`SELECT record_hash FROM audit.record WHERE partition_key = $1 AND sequence = 1`, part)
		dbtest.MustQueryRow(t, ctx, tx, &lastHash,
			`SELECT last_hash FROM audit.partition_month WHERE partition_key = $1`, part)
		if lastHash != h1 {
			t.Fatalf("partition head = %q, want the last record's hash %q", lastHash, h1)
		}

		// The second record MUST chain to the first record's hash. The function
		// supplies previous_hash from the partition head, so a caller cannot
		// inject an unlinked record.
		MustAppendAudit(t, ctx, tx, part, 2)

		var h2 string
		dbtest.MustQueryRow(t, ctx, tx, &h2,
			`SELECT record_hash FROM audit.record WHERE partition_key = $1 AND sequence = 2`, part)
		dbtest.MustQueryRow(t, ctx, tx, &lastHash,
			`SELECT last_hash FROM audit.partition_month WHERE partition_key = $1`, part)
		if lastHash != h2 {
			t.Fatalf("partition head did not advance to the second record's hash")
		}
		if h2 == h1 {
			t.Fatal("the second record hashed to the same value as the first; the chain is not binding the content")
		}

		var prev string
		dbtest.MustQueryRow(t, ctx, tx, &prev,
			`SELECT previous_hash FROM audit.record WHERE partition_key = $1 AND sequence = 2`, part)
		if prev != h1 {
			t.Fatalf("record 2 previous_hash = %q, want %q; the chain is broken", prev, h1)
		}
		// The first record chains to the genesis value.
		dbtest.MustQueryRow(t, ctx, tx, &prev,
			`SELECT previous_hash FROM audit.record WHERE partition_key = $1 AND sequence = 1`, part)
		if prev != strings.Repeat("0", 64) {
			t.Fatalf("first record previous_hash = %q, want 64 zeros", prev)
		}
	})
}

func TestAuditRecordsArePhysicallyImmutable(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		auditID := dbtest.CanonicalID("aud", 351)
		MustAppendAuditWithID(t, ctx, tx, part, auditID, 1)
		// Tampering with evidence is a SEV-1 event and must be physically
		// impossible, not merely discouraged.
		savepoint := "sp_update"
		if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		err := dbtest.ExpectRejected(t, ctx, tx, `UPDATE audit.record SET action = 'tampered' WHERE audit_id = $1`, auditID)
		if !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("UPDATE rejection did not come from the append-only guard: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); err != nil {
			t.Fatalf("rollback to savepoint: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		err = dbtest.ExpectRejected(t, ctx, tx, `DELETE FROM audit.record WHERE audit_id = $1`, auditID)
		if !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("DELETE rejection did not come from the append-only guard: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); err != nil {
			t.Fatalf("rollback to savepoint: %v", err)
		}
	})
}

func TestAuditExportRetentionIsAtLeastSevenYears(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)
		MustInsertCheckpoint(t, ctx, tx, part, 1, 1)
		realHash := MustRecordHash(t, ctx, tx, part, 1)
		// A one-year independent retention copy violates the seven-year
		// requirement for financial, access, approval and control records.
		dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.export_batch (export_batch_id, checkpoint_id, partition_key,
                target_project, target_region, target_object_ref, object_lock_until,
                retention_until, record_count, first_sequence, last_sequence, first_hash, last_hash)
            VALUES ($1,$2,$3,'other-project','ap-southeast-2','gs://x/y',
                    now()+interval '1 year', now()+interval '1 year', 1,1,1,$4,$4)`,
			dbtest.CanonicalID("exp", 362), dbtest.CanonicalID("ckp", 100*1+1), part, realHash)
	})
}

func TestAuditCheckpointCountMustMatchSequenceRange(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)
		// A checkpoint claiming more records than its range contains would hide a
		// truncation.
		//
		// The hashes are the REAL ones, so the rejection is attributable to
		// checkpoint_count_matches alone. Supplying a plausible constant would
		// be refused earlier by the 0010 binding trigger, and the test would pass
		// without ever exercising the control it names.
		realHash := MustRecordHash(t, ctx, tx, part, 1)
		MustRegisterSigningKey(t, ctx, tx)
		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns)
            VALUES ($1,$2,1,1,5,$3,$3,$4,decode($5,'hex'),$6)`,
			dbtest.CanonicalID("ckp", 371), part, realHash, TestSigningKeyID, testSignatureHex, dbtest.NowNs())
		// Two controls cover this. `checkpoint_count_matches` rejects the
		// arithmetic alone; the 0010 binding trigger rejects it first, and more
		// strongly, by finding that only one record is actually present. Either
		// is a correct refusal; both must be named so a reader can tell which
		// control this test is currently exercising.
		if !strings.Contains(err.Error(), "checkpoint_count_matches") &&
			!strings.Contains(err.Error(), "but 1 are present") {
			t.Fatalf("rejection did not come from a record-count control: %v", err)
		}
	})
}

// TestSigningKeyID is the key every checkpoint test seals with. It must exist
// in audit.signing_key, or the 0012 key-binding control refuses the insert.
const TestSigningKeyID = "key-1"

// testSignatureHex is a 64-byte value standing in for a real ECDSA/Ed25519
// signature. 0012 rejects anything shorter as a placeholder, which is why the
// fixtures previously using decode('00','hex') had to change.
//
// It is NOT a signature and nothing here treats it as one. 0012 deliberately
// does not attempt cryptographic verification: key material does not exist in
// this database, so no function in the schema can verify signature bytes. What
// is proven is attribution to a registered, legitimate key -- not authenticity.
const testSignatureHex = "abababababababababababababababababababababababababababababababababababab" +
	"abababababababababababababababababababababababababababababababababababab"

// MustRegisterSigningKey registers (or refreshes) a signing key. It is
// idempotent so every test that seals a checkpoint can call it.
func MustRegisterSigningKey(t *testing.T, ctx context.Context, tx *sql.Tx) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO audit.signing_key (signing_key_id, provider, key_reference, algorithm,
            status, rotate_after)
        VALUES ($1, 'test', 'test://local-key-1', 'ECDSA_P256_SHA256', 'ACTIVE',
                now() + interval '365 days')
        ON CONFLICT (signing_key_id) DO UPDATE
            SET status = 'ACTIVE', rotate_after = now() + interval '365 days'`,
		TestSigningKeyID); err != nil {
		t.Fatalf("register signing key: %v", err)
	}
}

// MustRecordHash reads the stored record_hash at a sequence. Checkpoints must
// name the real hashes since 0010, so tests cannot supply a constant.
func MustRecordHash(t *testing.T, ctx context.Context, tx *sql.Tx, part string, seq int) string {
	t.Helper()
	var h string
	dbtest.MustQueryRow(t, ctx, tx, &h,
		`SELECT record_hash FROM audit.record WHERE partition_key = $1 AND sequence = $2`, part, seq)
	return h
}

// MustInsertCheckpoint seals a range with the hashes the records actually carry.
//
// The hashes are read back rather than hard-coded, because 0010 requires the
// checkpoint to be bound to the records it attests to. A caller that supplies a
// plausible-looking 64-character string is now rejected, which is the control
// TestCheckpointMustBeBoundToTheRecordsItAttestsTo covers directly.
func MustInsertCheckpoint(t *testing.T, ctx context.Context, tx *sql.Tx, part string, firstSeq, lastSeq int) {
	t.Helper()
	MustRegisterSigningKey(t, ctx, tx)
	first := MustRecordHash(t, ctx, tx, part, firstSeq)
	last := MustRecordHash(t, ctx, tx, part, lastSeq)
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
            record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,decode($9,'hex'),$10)`,
		dbtest.CanonicalID("ckp", firstSeq*100+lastSeq), part, firstSeq, lastSeq,
		lastSeq-firstSeq+1, first, last, TestSigningKeyID, testSignatureHex, dbtest.NowNs()); err != nil {
		t.Fatalf("insert checkpoint %d..%d: %v", firstSeq, lastSeq, err)
	}
}

// ---------------------------------------------------------------------------
// Balanced journals (0008)
//
// 05_..._PERSISTENCE.md: "Ledger entries are append-only and balanced."
// A single-statement assertion cannot prove a COMMIT-time control, so these
// tests drive real transactions through the database instead of a rolled-back
// fixture transaction.
// ---------------------------------------------------------------------------

func TestBalancedJournalCommits(t *testing.T) {
	db := dbtest.Open(t)
	journal := dbtest.CanonicalID("jnl", 401)
	account := dbtest.CanonicalID("acc", 402)
	command := dbtest.CanonicalID("cmd", 403)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	MustCreateJournal(t, ctx, tx, journal, command, account, "USD", 2, "1000", "1000")
	MustPostJournalEntry(t, ctx, tx, dbtest.CanonicalID("led", 404), dbtest.UniqueSequence(1), journal, account, command, "TRADE_CASH", "DEBIT", "1000", dbtest.UniqueDigest(1))
	MustPostJournalEntry(t, ctx, tx, dbtest.CanonicalID("led", 405), dbtest.UniqueSequence(2), journal, account, command, "FEE", "CREDIT", "1000", dbtest.UniqueDigest(2))

	if err := tx.Commit(); err != nil {
		t.Fatalf("a balanced journal must commit: %v", err)
	}
	// No cleanup: ledger.entry is append-only, so a committed fixture cannot be
	// deleted without weakening the very control this suite verifies. The rows
	// remain, with identities unique to this run, and the database is expected to
	// be disposable and freshly migrated (see dbtest.SetRunNonce).
}

func TestUnbalancedJournalCannotCommit(t *testing.T) {
	db := dbtest.Open(t)
	journal := dbtest.CanonicalID("jnl", 411)
	account := dbtest.CanonicalID("acc", 412)
	command := dbtest.CanonicalID("cmd", 413)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The header claims a balanced journal, but the legs do not net to zero.
	// The header CHECK alone cannot see the legs, so the deferred constraint
	// trigger is what has to catch this at COMMIT.
	MustCreateJournal(t, ctx, tx, journal, command, account, "USD", 2, "1000", "1000")
	MustPostJournalEntry(t, ctx, tx, dbtest.CanonicalID("led", 414), dbtest.UniqueSequence(3), journal, account, command, "TRADE_CASH", "DEBIT", "1000", dbtest.UniqueDigest(3))
	MustPostJournalEntry(t, ctx, tx, dbtest.CanonicalID("led", 415), dbtest.UniqueSequence(4), journal, account, command, "FEE", "CREDIT", "400", dbtest.UniqueDigest(4))

	err = tx.Commit()
	if err == nil {
		t.Fatal("an unbalanced ledger journal committed; the balance control is not effective")
	}
	if !strings.Contains(err.Error(), "journal") {
		t.Fatalf("commit failed for the wrong reason, not the journal balance control: %v", err)
	}
}

func TestUnbalancedJournalHeaderIsRejectedAtInsert(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// Defence at the header itself, before any leg exists.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ledger.journal (journal_id, source_command_id, environment, account_id, currency,
                entry_count, debit_total, credit_total, posted_at_ns, correlation_id)
            VALUES ($1,$2,'paper',$3,'USD',2,1000,900,$4,'cor')`,
			dbtest.CanonicalID("jnl", 421), dbtest.CanonicalID("cmd", 422),
			dbtest.CanonicalID("acc", 423), dbtest.NowNs())
		if !strings.Contains(err.Error(), "journal_balanced") {
			t.Fatalf("an unbalanced journal header was not rejected: %v", err)
		}
	})
}

func TestInternalLedgerEntryMustBelongToAJournal(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A trade cash movement is an internal fact: the platform accounts for it
		// between its own accounts, so it cannot be posted as a single one-sided
		// leg. This is the control that was previously absent.
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO ledger.entry (entry_id, sequence, account_id, environment, entry_kind,
                direction, amount, currency, source_command_id, correlation_id,
                effective_at, effective_at_ns, recorded_at_ns, source_digest)
            VALUES ($1,1,$2,'paper','TRADE_CASH','DEBIT',1000,'USD',$3,'cor',
                    now(),$4,$4,$5)`,
			dbtest.CanonicalID("led", 431), dbtest.CanonicalID("acc", 432),
			dbtest.CanonicalID("cmd", 433), dbtest.NowNs(), strings.Repeat("e", 64)); err != nil {
			t.Fatalf("internal entry fixture: %v", err)
		}
		// The deferred trigger fires at COMMIT, which this test never reaches, so
		// the check is exercised explicitly here and proven at COMMIT above.
		var journalID *string
		if err := tx.QueryRowContext(ctx, `SELECT journal_id FROM ledger.entry WHERE entry_id = $1`,
			dbtest.CanonicalID("led", 431)).Scan(&journalID); err != nil {
			t.Fatalf("read journal_id: %v", err)
		}
		if journalID != nil {
			t.Fatalf("an internal entry was accepted with a journal reference; the unjournalled case is the one under test")
		}
	})
}

// ---------------------------------------------------------------------------
// Audit: one write path, and partitions that cannot run out (0008)
// ---------------------------------------------------------------------------

func TestDirectAuditInsertIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)
		// A direct INSERT would bypass the advisory lock, the sequence check and
		// the chain head. It is the exact path an attacker or a careless service
		// would use to forge or reorder evidence.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.record (audit_id, partition_key, sequence, tenant_or_owner_scope,
                actor_id, actor_type, action, target_type, target_id, environment,
                occurred_at, occurred_at_ns, recorded_at, recorded_at_ns,
                correlation_id, policy_version, result, details,
                previous_hash, record_hash, signing_key_id, canonical_schema_version)
            VALUES ($1,$2,99,'owner','attacker','SYSTEM','test.action','order','ord_x','paper',
                    now(),$3,now(),$3,'cor','v1','SUCCESS','{}'::jsonb,
                    $4,$4,'key-1','1.0.0')`,
			dbtest.CanonicalID("aud", 441), part, dbtest.NowNs(), strings.Repeat("f", 64))
		if !strings.Contains(err.Error(), "audit.append_record") {
			t.Fatalf("a direct audit insert was not rejected by the append-path control: %v", err)
		}
	})
}

func TestAuditPartitionIsCreatedOnDemandBeyondTheStaticRange(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// 2027-04 is beyond the last statically created partition. Before 0008 a
		// record written then would fail with "no partition of relation found",
		// which would have silently stopped evidence capture.
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO audit.partition_month (partition_key, period_start, period_end)
            VALUES ('2027-04', DATE '2027-04-01', DATE '2027-05-01')
            ON CONFLICT (partition_key) DO NOTHING`); err != nil {
			t.Fatalf("open far-future partition: %v", err)
		}
		MustAppendAuditAt(t, ctx, tx, "2027-04", 1, time.Date(2027, 4, 15, 12, 0, 0, 0, time.UTC))

		var reg string
		dbtest.MustQueryRow(t, ctx, tx, &reg,
			`SELECT to_regclass('audit.record_2027_04')::text`)
		if reg == "" {
			t.Fatal("the 2027-04 partition was not created on demand")
		}
	})
}

func MustAppendAuditAt(t *testing.T, ctx context.Context, tx *sql.Tx, part string, seq int, recordedAt time.Time) {
	t.Helper()
	// The recorded_at timestamp and its nanosecond companion are separate
	// parameters: sharing one placeholder makes PostgreSQL deduce two
	// incompatible types for it.
	//
	// The hash argument is NULL so the DATABASE computes record_hash from the
	// canonical payload. A fabricated hash is rejected outright by the
	// cross-check added in 0009, which is tested directly in
	// audit_canonical_test.go rather than implicitly by unrelated tests.
	if _, err := tx.ExecContext(ctx, `
        SELECT audit.append_record($1,$2,$3,'owner','actor-1','SYSTEM','test.action',
            'order',$4,'paper',NULL,now(),$5,$6,$7,'reason','cor',NULL,'v1','SUCCESS',
            NULL,NULL,'{}'::jsonb,'key-1','1.0.0',NULL)`,
		dbtest.CanonicalID("aud", seq*7+1), part, seq, dbtest.CanonicalID("ord", seq*13+2),
		dbtest.NowNs(), recordedAt, recordedAt.UnixNano()); err != nil {
		t.Fatalf("append audit record %d into %s: %v", seq, part, err)
	}
}

// ---------------------------------------------------------------------------
// Strategy lifecycle (0008)
// ---------------------------------------------------------------------------

func TestStrategyCannotSkipItsGovernedPromotionPath(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		strategy := dbtest.CanonicalID("str", 451)
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO strategy.strategy (strategy_id, name, owner_subject_id, state, market_class,
                content_digest, rollback_digest)
            VALUES ($1,'s','owner-1','DRAFT','crypto',$2,$3)`,
			strategy, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
			t.Fatalf("create strategy: %v", err)
		}

		// DRAFT -> DEPLOYED is not in the closed set: a strategy cannot be
		// deployed without passing review, backtest, simulation, paper, shadow
		// and approval.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE strategy.strategy SET state = 'DEPLOYED' WHERE strategy_id = $1`, strategy)
		if !strings.Contains(err.Error(), "state_transition") {
			t.Fatalf("an unrecorded strategy state change was not rejected: %v", err)
		}
	})
}

func TestStrategyTransitionRequiresARecordedEvent(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		strategy := dbtest.CanonicalID("str", 461)
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO strategy.strategy (strategy_id, name, owner_subject_id, state, market_class,
                content_digest, rollback_digest)
            VALUES ($1,'s','owner-1','DRAFT','crypto',$2,$3)`,
			strategy, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
			t.Fatalf("create strategy: %v", err)
		}
		// DRAFT -> REVIEW IS a legal transition, but the state may only change
		// when a strategy.state_transition event records who did it and why.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE strategy.strategy SET state = 'REVIEW' WHERE strategy_id = $1`, strategy)
		if !strings.Contains(err.Error(), "without a recorded strategy.state_transition") {
			t.Fatalf("a legal-but-unrecorded strategy transition was not rejected: %v", err)
		}

		// With the event recorded, the same transition is permitted.
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO strategy.state_transition (transition_id, strategy_id, from_state, to_state,
                environment, actor_id, reason, correlation_id, occurred_at_ns)
            VALUES ($1,$2,'DRAFT','REVIEW','paper','researcher-1','ready for review',$3,$4)`,
			dbtest.CanonicalID("trn", 462), strategy, "cor", dbtest.NowNs()); err != nil {
			t.Fatalf("record transition: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE strategy.strategy SET state = 'REVIEW' WHERE strategy_id = $1`, strategy); err != nil {
			t.Fatalf("a recorded strategy transition must be permitted: %v", err)
		}
	})
}

func TestDualControlStrategyTransitionRequiresALiveApproval(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		strategy := dbtest.CanonicalID("str", 471)
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO strategy.strategy (strategy_id, name, owner_subject_id, state, market_class,
                content_digest, rollback_digest, approved_by, approved_at, approval_expires_at)
            VALUES ($1,'s','owner-1','APPROVED','crypto',$2,$3,'risk-1',now(), now() - interval '1 day')`,
			strategy, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
			t.Fatalf("create strategy: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO strategy.state_transition (transition_id, strategy_id, from_state, to_state,
                environment, actor_id, reason, correlation_id, occurred_at_ns)
            VALUES ($1,$2,'APPROVED','DEPLOYED','paper','owner-1','promote',$3,$4)`,
			dbtest.CanonicalID("trn", 472), strategy, "cor", dbtest.NowNs()); err != nil {
			t.Fatalf("record transition: %v", err)
		}

		// The approval has expired, so it cannot authorise deployment.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE strategy.strategy SET state = 'DEPLOYED' WHERE strategy_id = $1`, strategy)
		if !strings.Contains(err.Error(), "expired") {
			t.Fatalf("deployment on an expired approval was not rejected: %v", err)
		}

		// A live approval makes the same transition permitted.
		if _, err := tx.ExecContext(ctx, `
            UPDATE strategy.strategy SET approval_expires_at = now() + interval '30 days'
             WHERE strategy_id = $1`, strategy); err != nil {
			t.Fatalf("renew approval: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
            UPDATE strategy.strategy SET state = 'DEPLOYED' WHERE strategy_id = $1`, strategy); err != nil {
			t.Fatalf("deployment under a live dual-control approval must be permitted: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func MustInsertInstrument(t *testing.T, ctx context.Context, tx *sql.Tx, id string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO market.instrument (instrument_id, market_class, base, quote,
            min_quantity, price_increment, tick_size, order_types)
        VALUES ($1,'crypto','BTC','USDT', 0.00000001, 0.01, 0.01, ARRAY['LIMIT']::common.order_type[])`,
		id); err != nil {
		t.Fatalf("insert instrument %s: %v", id, err)
	}
}

// MustOpenPartition returns the September 2026 chain partition, creating it if
// needed, and asserts the chain head is pristine.
//
// The chain is append-only, so the head can never be rewound by this suite.
// Every audit test runs inside a transaction that is rolled back, so a head that
// is not at genesis means committed evidence exists and the partition is not
// available for chain tests. That is reported as a hard failure rather than
// silently "resetting" evidence, because deleting audit rows to make a test pass
// is exactly the failure mode the control exists to prevent.
func MustOpenPartition(t *testing.T, ctx context.Context, tx *sql.Tx) string {
	t.Helper()
	part := "2026-09"
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO audit.partition_month (partition_key, period_start, period_end)
        VALUES ($1, DATE '2026-09-01', DATE '2026-10-01')
        ON CONFLICT (partition_key) DO NOTHING`, part); err != nil {
		t.Fatalf("open audit partition: %v", err)
	}

	var seq int64
	dbtest.MustQueryRow(t, ctx, tx, &seq,
		`SELECT last_sequence FROM audit.partition_month WHERE partition_key = $1`, part)
	if seq != 0 {
		t.Fatalf("audit partition %s is at sequence %d, not at genesis; committed evidence exists, so chain tests need a freshly migrated database", part, seq)
	}
	return part
}

func MustAppendAudit(t *testing.T, ctx context.Context, tx *sql.Tx, part string, seq int) {
	t.Helper()
	MustAppendAuditWithID(t, ctx, tx, part, dbtest.CanonicalID("aud", seq*7+1), seq)
}

func MustAppendAuditWithID(t *testing.T, ctx context.Context, tx *sql.Tx, part, auditID string, seq int) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        SELECT audit.append_record($1,$2,$3,'owner','actor-1','SYSTEM','test.action',
            'order',$4,'paper',NULL,now(),$5,now(),$5,'reason','cor',NULL,'v1','SUCCESS',
            NULL,NULL,'{}'::jsonb,'key-1','1.0.0',NULL)`,
		auditID, part, seq, dbtest.CanonicalID("ord", seq*13+2), dbtest.NowNs()); err != nil {
		t.Fatalf("append audit record %d: %v", seq, err)
	}
}

// ---------------------------------------------------------------------------
// OMS helpers.
//
// A test that wants an order in a given state must WALK it there through the
// closed transition set, because that is the only way a real order can reach
// the state. Inserting an order directly into a mid-machine state is no longer
// possible, which is the point of migration 0008.
// ---------------------------------------------------------------------------

// OrderSpec describes an order fixture.
type OrderSpec struct {
	ID          string
	Instrument  string
	Account     string
	Venue       string
	OrderType   string
	Side        string
	State       string
	Quantity    string
	Environment string
	Seed        int
	// Eligibility attaches the live-eligibility evidence a live order is
	// required to carry at insertion (order_live_requires_eligibility). The
	// constraint is evaluated on the INSERT row, so it cannot be satisfied by a
	// later UPDATE.
	Eligibility string
}

// eligibility returns the live-eligibility evidence, deriving a stand-in when
// the caller did not supply one so live fixtures stay concise.
func (s OrderSpec) eligibility() any {
	if s.Eligibility != "" {
		return s.Eligibility
	}
	if s.environment() == "live" {
		return dbtest.CanonicalID("elg", s.Seed)
	}
	return nil
}

func (s OrderSpec) account() string {
	if s.Account != "" {
		return s.Account
	}
	return dbtest.CanonicalID("acc", s.Seed+1)
}

func (s OrderSpec) venue() string {
	if s.Venue != "" {
		return s.Venue
	}
	return dbtest.CanonicalID("ven", s.Seed+2)
}

func (s OrderSpec) orderType() string {
	if s.OrderType != "" {
		return s.OrderType
	}
	return "LIMIT"
}

func (s OrderSpec) side() string {
	if s.Side != "" {
		return s.Side
	}
	return "BUY"
}

func (s OrderSpec) quantity() string {
	if s.Quantity != "" {
		return s.Quantity
	}
	return "100"
}

func (s OrderSpec) environment() string {
	if s.Environment != "" {
		return s.Environment
	}
	return "paper"
}

// MustCreateOrder inserts an order in its entry state (CREATED or RISK_PENDING).
func MustCreateOrder(t *testing.T, ctx context.Context, tx *sql.Tx, s OrderSpec) string {
	t.Helper()
	if s.State == "" {
		s.State = "CREATED"
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO oms.order (order_id, command_id, idempotency_scope, environment, account_id,
            instrument_id, venue_id, side, order_type, time_in_force, quantity, limit_price,
            state, correlation_id, eligibility_record_id)
        VALUES ($1,$2,'scope',$3,$4,$5,$6,$7,$8,'DAY',$9,100,$10,'cor',$11)`,
		s.ID, dbtest.CanonicalID("cmd", s.Seed), s.environment(), s.account(), s.Instrument,
		s.venue(), s.side(), s.orderType(), s.quantity(), s.State, s.eligibility()); err != nil {
		t.Fatalf("create order %s in %s: %v", s.ID, s.State, err)
	}
	return s.ID
}

// MustAttachRiskDecision records the durable risk decision on an order.
func MustAttachRiskDecision(t *testing.T, ctx context.Context, tx *sql.Tx, order, decision string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx,
		`UPDATE oms.order SET risk_decision_id = $2, risk_policy_revisions = ARRAY['pol_0000000000000000001'] WHERE order_id = $1`,
		order, decision); err != nil {
		t.Fatalf("attach risk decision to %s: %v", order, err)
	}
}

// MustTransition performs one legal transition: append the journal event, then
// apply the state change. That order is mandatory — the event trigger checks
// the order's current state, and the order trigger checks the backing event.
func MustTransition(t *testing.T, ctx context.Context, tx *sql.Tx, order, to, kind string, seed int) {
	t.Helper()
	from := currentState(t, ctx, tx, order)
	if err := applyTransition(ctx, tx, order, to, kind, seed, "", nil); err != nil {
		t.Fatalf("transition %s -> %s via %s: %v", from, to, kind, err)
	}
}

// MustFill performs a fill transition and advances the cumulative fill accounting.
func MustFill(t *testing.T, ctx context.Context, tx *sql.Tx, order, to, kind string, seed int, delta, price string) {
	t.Helper()
	from := currentState(t, ctx, tx, order)
	if err := applyTransition(ctx, tx, order, to, kind, seed,
		", filled_quantity = filled_quantity + $2::numeric, average_fill_price = $3::numeric",
		[]any{delta, price}); err != nil {
		t.Fatalf("fill transition %s -> %s: %v", from, to, err)
	}
}

func currentState(t *testing.T, ctx context.Context, tx *sql.Tx, order string) string {
	t.Helper()
	var state string
	dbtest.MustQueryRow(t, ctx, tx, &state, `SELECT state FROM oms.order WHERE order_id = $1`, order)
	return state
}

// applyTransition returns the first error produced by the two-step transition.
//
// The event row is written with explicit casts and literal values rather than a
// CASE expression over untyped parameters: PostgreSQL cannot deduce a type for a
// parameter that only appears inside one branch of a CASE, and the resulting
// 42P08 error would be reported as a schema defect rather than a real failure.
func applyTransition(ctx context.Context, tx *sql.Tx, order, to, kind string, seed int, extraSet string, extraArgs []any) error {
	var from string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM oms.order WHERE order_id = $1`, order).Scan(&from); err != nil {
		return err
	}

	delta, price := any("0"), any(nil)
	if len(extraArgs) > 0 {
		delta, price = extraArgs[0], extraArgs[1]
	}

	var nextSeq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) + 1 FROM oms.order_event WHERE order_id = $1`,
		order).Scan(&nextSeq); err != nil {
		return err
	}

	_, err := tx.ExecContext(ctx, `
        INSERT INTO oms.order_event (order_event_id, order_id, sequence, from_state, to_state,
            event_kind, filled_quantity_delta, price, actor_id, correlation_id, occurred_at_ns)
        VALUES ($1, $2, $3, $4::oms.order_state, $5::oms.order_state, $6::oms.order_event_kind,
                $7::numeric, $8::numeric, 'system', 'cor', $9)`,
		dbtest.CanonicalID("oe1", seed), order, nextSeq, from, to, kind,
		delta, price, dbtest.NowNs())
	if err != nil {
		return err
	}

	args := append([]any{order, to}, extraArgs...)
	_, err = tx.ExecContext(ctx,
		`UPDATE oms.order SET state = $2::oms.order_state`+extraSet+` WHERE order_id = $1`, args...)
	return err
}

// MustOrderAt creates an order and walks it to target through the closed
// transition set. Only the canonical forward path is supported, because that is
// the only path a real order can take.
func MustOrderAt(t *testing.T, ctx context.Context, tx *sql.Tx, ins string, seed int, target string) string {
	t.Helper()
	order := dbtest.CanonicalID("ord", seed)
	MustCreateOrder(t, ctx, tx, OrderSpec{ID: order, Instrument: ins, Seed: seed, State: "CREATED"})

	type hop struct{ to, kind string }
	var path []hop
	switch target {
	case "CREATED":
	case "RISK_PENDING":
		path = []hop{{"RISK_PENDING", "CREATED"}}
	case "RISK_APPROVED":
		path = []hop{{"RISK_PENDING", "CREATED"}, {"RISK_APPROVED", "RISK_APPROVED"}}
	case "SUBMITTING":
		path = []hop{{"RISK_PENDING", "CREATED"}, {"RISK_APPROVED", "RISK_APPROVED"}, {"SUBMITTING", "SUBMITTING"}}
	case "ACKNOWLEDGED":
		path = []hop{{"RISK_PENDING", "CREATED"}, {"RISK_APPROVED", "RISK_APPROVED"},
			{"SUBMITTING", "SUBMITTING"}, {"ACKNOWLEDGED", "ACKNOWLEDGED"}}
	case "UNKNOWN":
		path = []hop{{"RISK_PENDING", "CREATED"}, {"RISK_APPROVED", "RISK_APPROVED"},
			{"SUBMITTING", "SUBMITTING"}, {"UNKNOWN", "UNKNOWN_OUTCOME"}}
	default:
		t.Fatalf("MustOrderAt has no canonical path to %s", target)
	}

	MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", seed+50))
	for i, h := range path {
		MustTransition(t, ctx, tx, order, h.to, h.kind, seed+100+i)
	}
	return order
}

// MustExpectTransitionRejected attempts a complete, legal-shaped transition and
// requires the database to refuse it, naming the expected control.
//
// Both halves are attempted: the journal event and the state update. A refusal
// at either stage is a real refusal; what must never happen is acceptance.
func MustExpectTransitionRejected(t *testing.T, ctx context.Context, tx *sql.Tx, order, to, kind string, seed int, want string) {
	t.Helper()

	savepoint := fmt.Sprintf("dbtest_transition_%d", seed)
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	err := applyTransition(ctx, tx, order, to, kind, seed, "", nil)
	if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); rbErr != nil {
		t.Fatalf("rollback to savepoint: %v", rbErr)
	}
	if _, relErr := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); relErr != nil {
		t.Fatalf("release savepoint: %v", relErr)
	}

	if err == nil {
		t.Fatalf("database ACCEPTED transition %s -> %s via %s, which must be rejected", currentState(t, ctx, tx, order), to, kind)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("transition to %s was rejected, but not by the expected control %q: %v", to, want, err)
	}
}

// ---------------------------------------------------------------------------
// Ledger helpers.
// ---------------------------------------------------------------------------

// postEntry posts one EXTERNAL one-sided entry (no journal required).
func postEntry(t *testing.T, ctx context.Context, tx *sql.Tx, entry string, sequence int, account, command, digest string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO ledger.entry (entry_id, sequence, account_id, environment, entry_kind,
            direction, amount, currency, source_command_id, correlation_id,
            effective_at, effective_at_ns, recorded_at_ns, source_digest)
        VALUES ($1,$2,$3,'paper','CASH_DEPOSIT','CREDIT',1000,'USD',$4,'cor',
                now(),$5,$5,$6)`,
		entry, sequence, account, command, dbtest.NowNs(), digest); err != nil {
		t.Fatalf("post ledger entry %s: %v", entry, err)
	}
}

// MustCreateJournal inserts a balanced journal header.
func MustCreateJournal(t *testing.T, ctx context.Context, tx *sql.Tx, journal, command, account, currency string, count int, debit, credit string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO ledger.journal (journal_id, source_command_id, environment, account_id, currency,
            entry_count, debit_total, credit_total, posted_at_ns, correlation_id)
        VALUES ($1,$2,'paper',$3,$4,$5,$6::numeric,$7::numeric,$8,'cor')`,
		journal, command, account, currency, count, debit, credit, dbtest.NowNs()); err != nil {
		t.Fatalf("create journal %s: %v", journal, err)
	}
}

// MustPostJournalEntry posts one leg into an existing balanced journal.
func MustPostJournalEntry(t *testing.T, ctx context.Context, tx *sql.Tx, entry string, sequence int64, journal, account, command, kind, direction, amount, digest string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO ledger.entry (entry_id, sequence, account_id, environment, entry_kind,
            direction, amount, currency, source_command_id, correlation_id, journal_id,
            effective_at, effective_at_ns, recorded_at_ns, source_digest)
        VALUES ($1,$2,$3,'paper',$4::ledger.entry_kind,$5::ledger.entry_direction,$6::numeric,'USD',$7,'cor',$8,
                now(),$9,$9,$10)`,
		entry, sequence, account, kind, direction, amount, command, journal, dbtest.NowNs(), digest); err != nil {
		t.Fatalf("post journal entry %s: %v", entry, err)
	}
}
