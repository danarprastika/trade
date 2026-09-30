package dbtest_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/aitc/trade/dbtest"
)

// Reconciliation-break blocking tests.
//
// Authority:
//   05_PERSISTENCE_EVENTING_RECONCILIATION.md: "A material unresolved break
//       blocks affected risk-increasing actions."
//   11_EXECUTION_GATES.md G11: live activation requires that "no material
//       reconciliation break ... exists".
//   01_SYSTEM_ARCHITECTURE.md §3: reconciliation is authoritative for venue
//       discrepancies.
//
// Before migration 0018 the reconciliation schema had zero triggers and zero
// functions. It recorded breaks carefully -- mandatory SLA, all-or-nothing
// resolution metadata, a required reason on reopen -- and never made one DO
// anything. blocked_scope was written and never read. These tests are the
// evidence that it now does.

// MustOpenCase opens a reconciliation case. blockedScope is the JSONB scope that
// the case blocks; pass "{}" to block the whole environment.
func MustOpenCase(t *testing.T, ctx context.Context, tx *sql.Tx, seed int, environment, severity, status, kind, blockedScope string) string {
	t.Helper()
	caseID := dbtest.CanonicalID("rec", seed)
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO reconciliation.case (
            case_id, environment, venue_id, difference_kind, severity, status,
            blocked_scope, evidence, owner_subject_id, detected_at, resolve_by,
            resolved_at, resolved_by, resolution_note, correlation_id
        ) VALUES ($1,$2::common.environment,'ven_recon',$3,$4::reconciliation.severity,
                  $5::reconciliation.case_status,$6::jsonb,'{}'::jsonb,'sub_owner',
                  now(), now() + interval '1 hour',
                  CASE WHEN $5 IN ('RESOLVED','ACCEPTED_DIFFERENCE') THEN now() ELSE NULL END,
                  CASE WHEN $5 IN ('RESOLVED','ACCEPTED_DIFFERENCE') THEN 'sub_resolver' ELSE NULL END,
                  CASE WHEN $5 IN ('RESOLVED','ACCEPTED_DIFFERENCE') THEN 'accepted' ELSE NULL END,
                  'cor_recon')`,
		caseID, environment, kind, severity, status, blockedScope); err != nil {
		t.Fatalf("open case %s: %v", caseID, err)
	}
	return caseID
}

// TestAnOpenMaterialBreakBlocksRiskIncreasingOrder is the control 0005
// documented and never implemented.
//
// Without it, a venue reports a fill the platform never received, or a balance
// that does not reconcile, and the engine correctly opens a MATERIAL case --
// and the OMS goes on raising new exposure against a baseline nobody can vouch
// for. G11 requires that no material break exist before live activation, and a
// system that only records breaks cannot establish that.
func TestAnOpenMaterialBreakBlocksRiskIncreasingOrder(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, _, _, _ := riskIncreaseFixture(t, ctx, tx, 810, "paper", "CREATED")
		MustOpenCase(t, ctx, tx, 811, "paper", "MATERIAL", "OPEN", "BALANCE", "{}")

		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 812)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 813))

		// The only thing refusing this transition is the unresolved break. The
		// risk decision is present, so the state machine's own requirements are
		// satisfied and cannot be what stops it.
		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 814, "unresolved MATERIAL")
	})
}

// TestRiskReducingActivityIsNotBlockedByABreak is what makes the control safe to
// leave on.
//
// A control that blocks everything during an unreconciled break converts a data
// quality problem into an open position: the operator cannot flatten what the
// platform already holds. This is the same rule the halt system follows and for
// the same reason.
func TestRiskReducingActivityIsNotBlockedByABreak(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// The order is a REDUCE_ONLY order, which is how risk-reducing activity is
		// expressed. It is created that way rather than being turned into one:
		// order terms are immutable after acceptance, so an order cannot be
		// re-specified into a reducing action.
		ins := dbtest.CanonicalID("ins", 820)
		MustInsertInstrument(t, ctx, tx, ins)
		acc := dbtest.CanonicalID("acc", 821)
		venue := dbtest.CanonicalID("ven", 822)
		order := dbtest.CanonicalID("ord", 823)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, Account: acc, Venue: venue, OrderType: "REDUCE_ONLY",
			Seed: 824, State: "CREATED", Environment: "paper",
		})
		MustOpenCase(t, ctx, tx, 825, "paper", "MATERIAL", "OPEN", "POSITION", "{}")

		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 826)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 827))

		// A REDUCE_ONLY order is risk-reducing, so the break must not stop it
		// from being approved and used to flatten.
		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 828)
	})
}

// TestANonMaterialBreakDoesNotBlockTrading pins the threshold.
//
// Only MATERIAL stops trading. Blocking on every cosmetic rounding difference
// would make the control unusable within a day and it would be switched off,
// which is worse than not having it -- the blueprint would still claim the
// control exists.
func TestANonMaterialBreakDoesNotBlockTrading(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, _, _, _ := riskIncreaseFixture(t, ctx, tx, 830, "paper", "CREATED")
		MustOpenCase(t, ctx, tx, 831, "paper", "HIGH", "OPEN", "FEE", "{}")

		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 832)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 833))

		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 834)
	})
}

// TestAResolvedMaterialBreakNoLongerBlocks is the half that decides whether the
// control can be operated.
//
// The block is derived from the case at the moment activity is attempted, so
// resolving the case clears it. There is no separate administrative act, and
// therefore no way for the block to outlive the break that caused it.
func TestAResolvedMaterialBreakNoLongerBlocks(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, _, _, _ := riskIncreaseFixture(t, ctx, tx, 840, "paper", "CREATED")
		caseID := MustOpenCase(t, ctx, tx, 841, "paper", "MATERIAL", "OPEN", "BALANCE", "{}")

		dbtest.MustExec(t, ctx, tx, `
            UPDATE reconciliation.case
               SET status = 'RESOLVED', resolved_at = now(), resolved_by = 'sub_resolver',
                   resolution_note = 'venue corrected its report'
             WHERE case_id = $1`, caseID)

		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 842)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 843))

		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 844)
	})
}

// TestAnAcceptedDifferenceNoLongerBlocks covers the other terminal state. A
// break someone has signed off as legitimate is not blocking, and demanding a
// separate check to discover that is how a resolved case ends up stuck blocking
// trading forever.
func TestAnAcceptedDifferenceNoLongerBlocks(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, _, _, _ := riskIncreaseFixture(t, ctx, tx, 850, "paper", "CREATED")
		caseID := MustOpenCase(t, ctx, tx, 851, "paper", "MATERIAL", "ACCEPTED_DIFFERENCE", "BALANCE", "{}")

		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 852)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 853))

		var status string
		if err := tx.QueryRowContext(ctx,
			`SELECT status FROM reconciliation.case WHERE case_id = $1`, caseID).Scan(&status); err != nil {
			t.Fatalf("read case: %v", err)
		}
		if status != "ACCEPTED_DIFFERENCE" {
			t.Fatalf("fixture did not create the intended status, got %q", status)
		}
		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 855)
	})
}

// TestAReopenedBreakBlocksAgain closes the loop. A case that was resolved and
// then reopened is a break that came BACK, and trading must stop again.
func TestAReopenedBreakBlocksAgain(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, _, _, _ := riskIncreaseFixture(t, ctx, tx, 860, "paper", "CREATED")
		caseID := MustOpenCase(t, ctx, tx, 861, "paper", "MATERIAL", "OPEN", "BALANCE", "{}")
		dbtest.MustExec(t, ctx, tx, `
            UPDATE reconciliation.case
               SET status = 'RESOLVED', resolved_at = now(), resolved_by = 'sub_resolver',
                   resolution_note = 'fixed'
             WHERE case_id = $1`, caseID)
		dbtest.MustExec(t, ctx, tx, `
            UPDATE reconciliation.case
               SET status = 'REOPENED', resolved_at = NULL, reopened_at = now(),
                   reopen_reason = 'venue disagrees again'
             WHERE case_id = $1`, caseID)

		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 862)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 863))

		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 864, "unresolved MATERIAL")
	})
}

// TestABreakOnOneAccountDoesNotBlockAnother is the scope test, and it is the
// reason blocked_scope exists at all.
//
// A break affecting one account must not stop trading on every other account.
// Over-blocking is not a safe default here -- it is a denial of service on
// correct accounts, and an operator under pressure will be tempted to resolve
// cases they should not, or disable the control, to get trading moving again.
func TestABreakOnOneAccountDoesNotBlockAnother(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		blockedAcc := dbtest.CanonicalID("acc", 871)
		MustOpenCase(t, ctx, tx, 872, "paper", "MATERIAL", "OPEN", "BALANCE",
			`{"accounts":["`+blockedAcc+`"]}`)

		// A different account on the same instrument and venue.
		order, _, _, _ := riskIncreaseFixture(t, ctx, tx, 873, "paper", "CREATED")

		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 874)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 875))

		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 876)
	})
}

// TestABreakOnOneVenueDoesNotBlockAnotherVenue is the same property on the
// venue dimension.
func TestABreakOnOneVenueDoesNotBlockAnotherVenue(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustOpenCase(t, ctx, tx, 881, "paper", "MATERIAL", "OPEN", "BALANCE",
			`{"venues":["ven_broken"]}`)

		order, _, _, _ := riskIncreaseFixture(t, ctx, tx, 882, "paper", "CREATED")

		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 883)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 884))

		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 885)
	})
}

// TestABreakInOneEnvironmentDoesNotBlockAnother pins the environment dimension,
// which is what stops a paper break from halting shadow trading and vice versa.
func TestABreakInOneEnvironmentDoesNotBlockAnother(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustOpenCase(t, ctx, tx, 891, "paper", "MATERIAL", "OPEN", "BALANCE", "{}")

		order, _, _, _ := riskIncreaseFixture(t, ctx, tx, 892, "shadow", "CREATED")

		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 893)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 894))

		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 895)
	})
}

// TestAnUninterpretableScopeBlocksEverything is the fail-closed test, and it is
// the most important one in this file.
//
// blocked_scope is JSONB and no blueprint document specifies its shape, so this
// migration defines one. A scope this code cannot read must never be the reason
// trading continues: under-blocking creates exposure the operator believes is
// not there, and the cost of that failure is unmeasurable. Over-blocking a
// malformed scope is an operational problem somebody can see and fix.
func TestAnUninterpretableScopeBlocksEverything(t *testing.T) {
	scopes := map[string]string{
		"a number, not an object":      `12345`,
		"an unrecognised key":          `{"everything": true}`,
		"a recognised key, wrong type": `{"accounts": "acc_all"}`,
		"a list of non-strings":        `{"venues": [7, 8]}`,
		"an explicit JSON null":        `null`,
	}

	i := 900
	for name, scope := range scopes {
		t.Run(name, func(t *testing.T) {
			db := dbtest.Open(t)
			dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
				order, _, _, _ := riskIncreaseFixture(t, ctx, tx, i, "paper", "CREATED")
				i++
				MustOpenCase(t, ctx, tx, i, "paper", "MATERIAL", "OPEN", "BALANCE", scope)

				MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", i+1)
				MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", i+2))

				MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", i+3, "unresolved MATERIAL")
			})
		})
	}
}

// TestNoBreakAtAllPermitsRiskIncreasingOrder is the control-free baseline.
//
// If a test file only ever asserts refusals, it is also consistent with a
// database where nothing can trade. This is the test that says the gate is
// open when there is no reason to close it.
func TestNoBreakAtAllPermitsRiskIncreasingOrder(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order, _, _, _ := riskIncreaseFixture(t, ctx, tx, 950, "paper", "CREATED")

		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 951)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 952))

		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 953)
	})
}

// TestTheBreakGateItselfExemptsRiskReducingActivity tests the public contract of
// reconciliation.assert_break_permitted directly, rather than through an order.
//
// This exists because the exemption is unreachable by the obvious route.
// ops.assert_risk_increase_permitted returns early for risk-reducing activity
// before it ever calls the reconciliation gate, so the gate's own guard never
// fires when it is invoked the way production invokes it. Mutation testing
// confirmed this: removing the guard entirely left every test green, including
// the one that claims to cover risk-reducing activity.
//
// The function is public and documented as the reusable gate, so its contract
// has to hold on its own terms rather than only by way of a caller that happens
// to screen first.
func TestTheBreakGateItselfExemptsRiskReducingActivity(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustOpenCase(t, ctx, tx, 1000, "paper", "MATERIAL", "OPEN", "BALANCE", "{}")

		// Called directly with risk-reducing activity, against an open MATERIAL
		// break that covers the entire environment. It must return without
		// raising.
		dbtest.MustExec(t, ctx, tx, `
            SELECT reconciliation.assert_break_permitted(
                'paper'::common.environment, 'acc_any', 'ins_any', 'ven_any',
                false, 'direct call, risk-reducing')`)
	})
}

// TestTheBreakGateRefusesRiskIncreasingActivityDirectly is the positive half, so
// the test above cannot pass against a gate that has stopped working entirely.
func TestTheBreakGateRefusesRiskIncreasingActivityDirectly(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustOpenCase(t, ctx, tx, 1010, "paper", "MATERIAL", "OPEN", "BALANCE", "{}")

		dbtest.ExpectRejectedBecause(t, ctx, tx, "unresolved MATERIAL", `
            SELECT reconciliation.assert_break_permitted(
                'paper'::common.environment, 'acc_any', 'ins_any', 'ven_any',
                true, 'direct call, risk-increasing')`)
	})
}

// riskIncreaseFixture creates an order at CREATED that is ready to be moved to
// RISK_APPROVED, and returns the identifiers the caller needs to build a
// reconciliation case that does or does not cover it.
func riskIncreaseFixture(t *testing.T, ctx context.Context, tx *sql.Tx, seed int, environment, state string) (order, account, instrument, venue string) {
	t.Helper()
	instrument = dbtest.CanonicalID("ins", seed)
	MustInsertInstrument(t, ctx, tx, instrument)
	order = dbtest.CanonicalID("ord", seed+1)
	account = dbtest.CanonicalID("acc", seed+2)
	venue = dbtest.CanonicalID("ven", seed+3)
	MustCreateOrder(t, ctx, tx, OrderSpec{
		ID: order, Instrument: instrument, Account: account, Venue: venue,
		Seed: seed + 4, State: state, Environment: environment,
	})
	return order, account, instrument, venue
}
