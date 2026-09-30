package dbtest_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/aitc/trade/dbtest"
)

// Halt enforcement and audit-chain SEV-1 tests.
//
// Authority:
//   09_TESTING_AND_RELEASE_EVIDENCE.md, mandatory financial invariant 7:
//       "Halt state blocks prohibited actions."
//   11_EXECUTION_GATES.md: waivers are prohibited for halt controls.
//   17_CONFIGURATION_AND_RISK_POLICY.md: "active halt means reject".
//   22_AUDIT_INTEGRITY_AND_EVIDENCE.md section 3: a chain break is a SEV-1
//       event and "the system stops privileged mutations and risk-increasing
//       activity".
//
// This is invariant 7 as a storage control. Before 0013 the halt table was
// fully specified and nothing read it, so a halt did not halt; the two existing
// halt tests asserted only the halt table's own constraints and would have
// passed with the enforcement entirely removed.

// MustActivateHalt activates a halt for the given scope. A NULL scope field
// means "all values at that dimension", which is how a system-wide halt is
// expressed. seed makes each halt in a test distinct.
func MustActivateHalt(t *testing.T, ctx context.Context, tx *sql.Tx, level, environment string,
	account, instrument, market, venue, strategy, reason string, emergency bool, seed int) string {
	t.Helper()
	haltID := dbtest.CanonicalID("hlt", seed)
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO ops.halt (halt_id, level, state, environment, account_id, instrument_id,
            market_class, venue_id, strategy_id, reason, emergency, activated_by, activated_at_ns)
        VALUES ($1,$2::ops.halt_level,'ACTIVE',$3::common.environment,$4,$5,$6::common.market_class,
                $7,$8,$9,$10,'op-1',$11)`,
		haltID, level, environment, nullIfEmpty(account), nullIfEmpty(instrument),
		nullIfEmpty(market), nullIfEmpty(venue), nullIfEmpty(strategy), reason, emergency,
		dbtest.NowNs()); err != nil {
		t.Fatalf("activate %s: %v", level, err)
	}
	return haltID
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TestActiveHaltBlocksRiskIncreasingOrder is invariant 7. A halted system must
// refuse an order that increases exposure.
func TestActiveHaltBlocksRiskIncreasingOrder(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 701)
		MustInsertInstrument(t, ctx, tx, ins)
		order := dbtest.CanonicalID("ord", 702)
		acc := dbtest.CanonicalID("acc", 703)
		venue := dbtest.CanonicalID("ven", 704)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, Account: acc, Venue: venue,
			Seed: 705, State: "CREATED", Environment: "paper",
		})
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 706)
		// A decision is attached so that the ONLY thing refusing the transition
		// is the halt. Without this the risk-decision control would fire first
		// and the test would pass without ever exercising the halt gate.
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 707))

		MustActivateHalt(t, ctx, tx, "ACCOUNT_HALT", "paper", acc, "", "", "", "",
			"venue maintenance", false, 710)

		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 708,
			"risk-increasing activity refused")
	})
}

// TestHaltPermitsRiskReducingOrder is the other half, and it is the half that
// makes halting safe.
//
// A halt that also blocked flattening could turn a contained incident into an
// unbounded loss: the operator has no way to reduce exposure while the venue
// keeps moving. So a risk-reducing order must still advance during a halt.
//
// This test is the exact mirror of TestActiveHaltBlocksRiskIncreasingOrder --
// same instrument, account, venue, halt, and an attached risk decision in both,
// with OrderType the only difference. The risk decision is attached in both
// because a reduce-only order also cannot reach a submission state without one;
// omitting it here would make this test pass for the wrong reason, proving only
// that a missing risk decision is still enforced. Holding everything else
// constant is what makes the halt the only possible cause of the difference.
func TestHaltPermitsRiskReducingOrder(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 711)
		MustInsertInstrument(t, ctx, tx, ins)
		acc := dbtest.CanonicalID("acc", 713)
		venue := dbtest.CanonicalID("ven", 714)

		MustActivateHalt(t, ctx, tx, "ACCOUNT_HALT", "paper", acc, "", "", "", "",
			"venue maintenance", false, 710)

		order := dbtest.CanonicalID("ord", 712)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, Account: acc, Venue: venue, OrderType: "REDUCE_ONLY",
			Seed: 715, State: "CREATED", Environment: "paper",
		})
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 716)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 717))
		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 718)
		MustTransition(t, ctx, tx, order, "SUBMITTING", "SUBMITTING", 719)
	})
}

// TestHaltScopeDoesNotLeakToOtherAccounts proves the scope is honoured. A halt
// on one account must not freeze an unrelated account, or the blast radius of
// an incident would be the whole book.
func TestHaltScopeDoesNotLeakToOtherAccounts(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 721)
		MustInsertInstrument(t, ctx, tx, ins)
		halted := dbtest.CanonicalID("acc", 722)
		other := dbtest.CanonicalID("acc", 723)
		venue := dbtest.CanonicalID("ven", 724)

		MustActivateHalt(t, ctx, tx, "ACCOUNT_HALT", "paper", halted, "", "", "", "",
			"venue maintenance", false, 710)

		order := dbtest.CanonicalID("ord", 725)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, Account: other, Venue: venue,
			Seed: 726, State: "CREATED", Environment: "paper",
		})
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 727)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 728))
		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 729)
	})
}

// TestClearedHaltNoLongerBlocks proves the control is not a latch. A halt that
// cannot be lifted stops being a control and becomes an outage.
func TestClearedHaltNoLongerBlocks(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 731)
		MustInsertInstrument(t, ctx, tx, ins)
		acc := dbtest.CanonicalID("acc", 732)
		venue := dbtest.CanonicalID("ven", 733)
		haltID := MustActivateHalt(t, ctx, tx, "ACCOUNT_HALT", "paper", acc, "", "", "", "",
			"venue maintenance", false, 710)

		if _, err := tx.ExecContext(ctx, `
            UPDATE ops.halt SET state='CLEARED', cleared_by='op-1',
                cleared_approved_by='op-2', cleared_at=now(), clear_reason='recovered',
                health_verified=true, reconciliation_verified=true
             WHERE halt_id = $1`, haltID); err != nil {
			t.Fatalf("clear halt: %v", err)
		}

		order := dbtest.CanonicalID("ord", 734)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, Account: acc, Venue: venue,
			Seed: 735, State: "CREATED", Environment: "paper",
		})
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 736)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 737))
		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 738)
	})
}

// TestHaltPrecedencePreventsClearingALowerRank proves the monotonicity the
// ops.halt comment claims. Without it, a system-wide halt can be defeated by
// clearing the account-level halts first and declaring the system resolved.
func TestHaltPrecedencePreventsClearingALowerRank(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		acc := dbtest.CanonicalID("acc", 741)
		accountHalt := MustActivateHalt(t, ctx, tx, "ACCOUNT_HALT", "paper", acc, "", "", "", "",
			"account issue", false, 740)
		MustActivateHalt(t, ctx, tx, "SYSTEM_HALT", "paper", "", "", "", "", "",
			"market-wide failure", true, 741)

		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE ops.halt SET state='CLEARED', cleared_by='op-1',
                cleared_approved_by='op-2', cleared_at=now(), clear_reason='x',
                health_verified=true, reconciliation_verified=true
             WHERE halt_id = $1`, accountHalt)
		if !strings.Contains(err.Error(), "monotonic in severity") {
			t.Fatalf("a lower-ranked halt was cleared while a higher-ranked halt was ACTIVE: %v", err)
		}
	})
}

// TestHighestRankHaltIsReported proves the operator is told about the most
// severe condition, which is the one that explains the incident.
func TestHighestRankHaltIsReported(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		acc := dbtest.CanonicalID("acc", 751)
		MustActivateHalt(t, ctx, tx, "ACCOUNT_HALT", "paper", acc, "", "", "", "",
			"account issue", false, 750)
		MustActivateHalt(t, ctx, tx, "VENUE_HALT", "paper", "", "", "", "", "",
			"venue down", true, 752)

		var level string
		dbtest.MustQueryRow(t, ctx, tx, &level, `
            SELECT level::text FROM ops.effective_halt('paper',$1,NULL,NULL,NULL,NULL)`, acc)
		if level != "VENUE_HALT" {
			t.Fatalf("effective_halt reported %s, want the highest matching rank VENUE_HALT", level)
		}
	})
}

// ---------------------------------------------------------------------------
// Audit chain SEV-1 (22_AUDIT_INTEGRITY_AND_EVIDENCE.md section 3)
// ---------------------------------------------------------------------------

// TestMarkChainBrokenRaisesAnEmergencySystemHalt proves the SEV-1 response
// exists at all. Before 0013 chain_state was never written and nothing read it,
// so a broken evidence chain had no effect on trading whatsoever.
func TestMarkChainBrokenRaisesAnEmergencySystemHalt(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)

		var haltID string
		dbtest.MustQueryRow(t, ctx, tx, &haltID, `
            SELECT audit.mark_chain_broken($1, 'signature verification failed', 'security-operator')`, part)
		if !dbtest.IsCanonicalID(t, ctx, tx, haltID, "hlt") {
			t.Fatalf("mark_chain_broken returned %q, which is not a canonical hlt identifier", haltID)
		}

		var state string
		dbtest.MustQueryRow(t, ctx, tx, &state,
			`SELECT chain_state FROM audit.partition_month WHERE partition_key = $1`, part)
		if state != "BROKEN" {
			t.Fatalf("chain_state = %s, want BROKEN", state)
		}

		var gotLevel, gotState string
		var gotEmergency bool
		if err := tx.QueryRowContext(ctx, `
            SELECT level::text, state::text, emergency FROM ops.halt WHERE halt_id = $1`, haltID).
			Scan(&gotLevel, &gotState, &gotEmergency); err != nil {
			t.Fatalf("read back the SEV-1 halt: %v", err)
		}
		if gotLevel != "SYSTEM_HALT" || gotState != "ACTIVE" || !gotEmergency {
			t.Fatalf("halt is %s/%s/emergency=%v, want SYSTEM_HALT/ACTIVE/emergency=true",
				gotLevel, gotState, gotEmergency)
		}
	})
}

// TestMarkChainBrokenRequiresAReason keeps the SEV-1 path from becoming a
// reflex. A chain break with no recorded cause cannot be triaged, and a control
// that is cheap to trigger and impossible to action is one that gets disabled.
func TestMarkChainBrokenRequiresAReason(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		err := dbtest.ExpectRejected(t, ctx, tx, `
            SELECT audit.mark_chain_broken($1, '   ', 'security-operator')`, part)
		if !strings.Contains(err.Error(), "requires a reason") {
			t.Fatalf("a chain break with no reason was accepted: %v", err)
		}
	})
}

// TestBrokenAuditChainBlocksRiskIncreasingActivity is the SEV-1 consequence.
// The chain check runs before the halt check and is not overridable, because
// evidence that cannot be trusted means decisions cannot be evidenced.
func TestBrokenAuditChainBlocksRiskIncreasingActivity(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		ins := dbtest.CanonicalID("ins", 761)
		MustInsertInstrument(t, ctx, tx, ins)
		order := dbtest.CanonicalID("ord", 762)
		acc := dbtest.CanonicalID("acc", 763)
		venue := dbtest.CanonicalID("ven", 764)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, Account: acc, Venue: venue,
			Seed: 765, State: "CREATED", Environment: "paper",
		})
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 766)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 767))

		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)
		dbtest.MustQueryRow(t, ctx, tx, new(string), `
            SELECT audit.mark_chain_broken($1, 'missing sequence', 'security-operator')`, part)

		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 768,
			"chain_state=BROKEN")
	})
}

// TestClearingTheHaltDoesNotRepairTheChain proves the two are independent.
// Re-enabling trading must not silently reopen a chain that was never repaired;
// chain repair is an evidence operation with its own sign-off.
func TestClearingTheHaltDoesNotRepairTheChain(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)
		var haltID string
		dbtest.MustQueryRow(t, ctx, tx, &haltID, `
            SELECT audit.mark_chain_broken($1, 'missing sequence', 'security-operator')`, part)

		// An emergency halt can be cleared without a second approver, but that
		// must not touch chain_state.
		if _, err := tx.ExecContext(ctx, `
            UPDATE ops.halt SET state='CLEARED', cleared_by='op-1', cleared_at=now(),
                clear_reason='trading resumed'
             WHERE halt_id = $1`, haltID); err != nil {
			t.Fatalf("clear halt: %v", err)
		}

		var state string
		dbtest.MustQueryRow(t, ctx, tx, &state,
			`SELECT chain_state FROM audit.partition_month WHERE partition_key = $1`, part)
		if state != "BROKEN" {
			t.Fatalf("clearing the halt changed chain_state to %s; chain repair must be a separate operation", state)
		}
	})
}

// TestNewCanonicalIdIsValidAndUnique covers the generator the SEV-1 path
// depends on. Nothing in the schema could mint a canonical identifier before
// 0013, so every writer had to supply one it could not verify until the write
// failed.
func TestNewCanonicalIdIsValidAndUnique(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		seen := map[string]bool{}
		for i := 0; i < 200; i++ {
			var id string
			dbtest.MustQueryRow(t, ctx, tx, &id, `SELECT common.new_canonical_id('hlt')`)
			if !dbtest.IsCanonicalID(t, ctx, tx, id, "hlt") {
				t.Fatalf("new_canonical_id produced %q, which common.is_canonical_id rejects", id)
			}
			if seen[id] {
				t.Fatalf("new_canonical_id produced %q twice in %d draws", id, i+1)
			}
			seen[id] = true
		}
	})
}
