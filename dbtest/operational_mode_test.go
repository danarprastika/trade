package dbtest_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/aitc/trade/dbtest"
)

// Operational mode enforcement tests.
//
// Authority:
//   10_OPERATIONS_AND_DISASTER_RECOVERY.md line 41: after recovery the system
//       starts in RECOVERY_HOLD; "read-only observation and reconciliation are
//       allowed; new risk-increasing commands, strategy deployment, credential
//       rotation, and live activation remain blocked".
//   10_OPERATIONS_AND_DISASTER_RECOVERY.md recovery decision table: "Can a
//       recovered region automatically resume risk-increasing behavior?" -- "No."
//   10_OPERATIONS_AND_DISASTER_RECOVERY.md objective 4: "No restored
//       environment may leave RECOVERY_HOLD without independent verification."
//   06_SECURITY_AND_ACCESS_CONTROL.md line 23: emergency controls "do not
//       require a second approver; re-enable does."
//
// Before 0014 ops.system_state carried recovery_hold and kill_switch_engaged and
// nothing in any migration read either column. 0007's own COMMENT ON TABLE
// asserted that they "each independently block risk-increasing activity and
// cannot be disabled without dual control"; neither half was implemented.

// MustSetOperationalMode moves an environment into a restricted mode.
//
// Engaging is deliberately unilateral: one accountable actor and a reason, no
// second approver. That is the corrected asymmetry, and requiring a second
// person to engage a safety control at 03:00 would make the control unavailable
// in exactly the incident it exists for.
func MustSetOperationalMode(t *testing.T, ctx context.Context, tx *sql.Tx, env, mode string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        UPDATE ops.system_state
           SET mode = $2, mode_changed_by = 'op-1', mode_change_reason = 'incident drill',
               mode_changed_at = now()
         WHERE environment = $1::common.environment`, env, mode); err != nil {
		t.Fatalf("enter %s on %s: %v", mode, env, err)
	}
}

func MustEngageRecoveryHold(t *testing.T, ctx context.Context, tx *sql.Tx, env, reason string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        UPDATE ops.system_state
           SET mode = 'RECOVERY_HOLD', recovery_hold = true, recovery_reason = $2,
               recovery_entered_at = now(), mode_changed_by = 'op-1',
               mode_change_reason = 'region failover', mode_changed_at = now()
         WHERE environment = $1::common.environment`, env, reason); err != nil {
		t.Fatalf("engage RECOVERY_HOLD on %s: %v", env, err)
	}
}

func MustEngageKillSwitch(t *testing.T, ctx context.Context, tx *sql.Tx, env, reason string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `
        UPDATE ops.system_state
           SET kill_switch_engaged = true, kill_switch_reason = $2,
               kill_switch_engaged_by = 'op-1', kill_switch_engaged_at = now()
         WHERE environment = $1::common.environment`, env, reason); err != nil {
		t.Fatalf("engage kill switch on %s: %v", env, err)
	}
}

// mustRiskIncreasingOrder builds an order walked legally to RISK_PENDING with a
// risk decision already attached.
//
// The decision is attached in every case so the ONLY thing that can refuse the
// final hop to RISK_APPROVED is the control under test. A test whose order
// lacks a decision passes for the wrong reason: the risk-decision control fires
// first and the operational-mode gate is never reached.
func mustRiskIncreasingOrder(t *testing.T, ctx context.Context, tx *sql.Tx, env string, seed int) string {
	t.Helper()
	ins := dbtest.CanonicalID("ins", seed)
	MustInsertInstrument(t, ctx, tx, ins)
	order := dbtest.CanonicalID("ord", seed+1)
	MustCreateOrder(t, ctx, tx, OrderSpec{
		ID: order, Instrument: ins, Account: dbtest.CanonicalID("acc", seed+2),
		Venue: dbtest.CanonicalID("ven", seed+3), Seed: seed + 4,
		State: "CREATED", Environment: env,
		// A live order needs recorded eligibility evidence to exist at all. The
		// constraint is evaluated on the INSERT row, so OrderSpec supplies it.
		Eligibility: dbtest.CanonicalID("elg", seed+7),
	})
	MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", seed+5)
	MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", seed+6))
	return order
}

// TestEveryEnvironmentHasARecordedOperationalMode guards the fail-closed design.
//
// ops.assert_operational_mode_permitted refuses risk-increasing activity for an
// environment with no ops.system_state row, on the grounds that an unrecorded
// operational mode is not an unrestricted one. That is only safe if every
// environment is seeded, so the seeding is asserted rather than assumed.
func TestEveryEnvironmentHasARecordedOperationalMode(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		var recorded, possible int
		dbtest.MustQueryRow(t, ctx, tx, &recorded,
			`SELECT count(*) FROM ops.system_state`)
		dbtest.MustQueryRow(t, ctx, tx, &possible,
			`SELECT count(*) FROM unnest(enum_range(NULL::common.environment))`)
		if recorded != possible {
			t.Fatalf("%d of %d environments have a recorded operational mode; "+
				"the remaining %d would fail closed on any risk-increasing activity",
				recorded, possible, possible-recorded)
		}
	})
}

// TestRiskIncreasingActivityFailsClosedForAnUnrecordedEnvironment proves the
// fail-closed branch is real rather than aspirational, by deleting the row and
// showing the refusal.
func TestRiskIncreasingActivityFailsClosedForAnUnrecordedEnvironment(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order := mustRiskIncreasingOrder(t, ctx, tx, "staging", 800)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM ops.system_state WHERE environment = 'staging'`); err != nil {
			t.Fatalf("delete staging mode: %v", err)
		}
		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 810,
			"no ops.system_state row")
	})
}

// TestRecoveryHoldBlocksRiskIncreasingActivity is the recovery requirement:
// a recovered region must not silently resume risk-increasing operation.
func TestRecoveryHoldBlocksRiskIncreasingActivity(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustEngageRecoveryHold(t, ctx, tx, "live", "region failover, restore under review")
		order := mustRiskIncreasingOrder(t, ctx, tx, "live", 811)
		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 820,
			"RECOVERY_HOLD active")
	})
}

// TestKillSwitchBlocksRiskIncreasingActivity covers the last-resort control.
// It is checked separately from RECOVERY_HOLD because they are independent:
// a kill switch during a venue outage must work even though no recovery occurred.
func TestKillSwitchBlocksRiskIncreasingActivity(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustEngageKillSwitch(t, ctx, tx, "paper", "venue rejecting acknowledgements")
		order := mustRiskIncreasingOrder(t, ctx, tx, "paper", 821)
		MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 830,
			"kill switch engaged")
	})
}

// TestRestrictedOperationalModesBlockRiskIncreasingActivity covers the
// remaining modes, and proves the gate reads mode rather than only the two
// boolean flags.
func TestRestrictedOperationalModesBlockRiskIncreasingActivity(t *testing.T) {
	for _, mode := range []string{"SAFE_HALT", "READ_ONLY", "CANCEL_ONLY"} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			db := dbtest.Open(t)
			dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
				MustSetOperationalMode(t, ctx, tx, "paper", mode)
				order := mustRiskIncreasingOrder(t, ctx, tx, "paper", 840)
				MustExpectTransitionRejected(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 850,
					"operational mode is "+mode)
			})
		})
	}
}

// TestOperationalModePermitsRiskReducingOrder is the mirror, and the reason the
// gate is not a trap.
//
// If a restricted mode also prevented an operator from flattening, RECOVERY_HOLD
// would convert a contained incident into unbounded loss with no remedy, and
// READ_ONLY would do the same during maintenance. A risk-reducing order must
// still reach submission.
func TestOperationalModePermitsRiskReducingOrder(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustEngageKillSwitch(t, ctx, tx, "paper", "venue outage")
		MustSetOperationalMode(t, ctx, tx, "paper", "READ_ONLY")

		ins := dbtest.CanonicalID("ins", 861)
		MustInsertInstrument(t, ctx, tx, ins)
		order := dbtest.CanonicalID("ord", 862)
		MustCreateOrder(t, ctx, tx, OrderSpec{
			ID: order, Instrument: ins, Account: dbtest.CanonicalID("acc", 863),
			Venue: dbtest.CanonicalID("ven", 864), OrderType: "CLOSE_POSITION",
			Seed: 865, State: "CREATED", Environment: "paper",
		})
		MustTransition(t, ctx, tx, order, "RISK_PENDING", "CREATED", 866)
		MustAttachRiskDecision(t, ctx, tx, order, dbtest.CanonicalID("rsk", 867))
		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 868)
		MustTransition(t, ctx, tx, order, "SUBMITTING", "SUBMITTING", 869)
	})
}

// TestNormalModeDoesNotBlock proves the gate has no false positives.
func TestNormalModeDoesNotBlock(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		order := mustRiskIncreasingOrder(t, ctx, tx, "paper", 871)
		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 880)
	})
}

// TestEngagingRecoveryHoldNeedsNoSecondApprover pins the corrected asymmetry.
//
// 0007 required mode_approved_by to ENGAGE a restricted mode, which meant the
// database could refuse to enter RECOVERY_HOLD during the failover that caused
// the hold. That inverts 06_SECURITY_AND_ACCESS_CONTROL.md line 23. This test
// exists to make the inversion unreversible without a deliberate change.
func TestEngagingRecoveryHoldNeedsNoSecondApprover(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// One accountable actor and a reason. No mode_approved_by.
		MustEngageRecoveryHold(t, ctx, tx, "live", "region failover")

		var approver *string
		dbtest.MustQueryRow(t, ctx, tx, &approver, `
            SELECT mode_approved_by FROM ops.system_state WHERE environment = 'live'`)
		if approver != nil {
			t.Fatalf("a second approver was recorded while engaging RECOVERY_HOLD (%s); "+
				"engaging a safety control must be unilateral", *approver)
		}
	})
}

// TestEngagingARestrictedModeRequiresAnAccountableActor keeps the loosening
// honest: cheap is not anonymous.
func TestEngagingARestrictedModeRequiresAnAccountableActor(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE ops.system_state SET mode = 'SAFE_HALT' WHERE environment = 'paper'`)
		if !strings.Contains(err.Error(), "requires mode_changed_by") {
			t.Fatalf("a restricted mode was entered with no accountable actor: %v", err)
		}
	})
}

// TestLeavingRecoveryHoldRequiresDualControl covers objective 4: no restored
// environment resumes without recorded, independent verification.
func TestLeavingRecoveryHoldRequiresDualControl(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustEngageRecoveryHold(t, ctx, tx, "live", "region failover")

		// Dropping the flag with no release evidence at all.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE ops.system_state SET recovery_hold = false WHERE environment = 'live'`)
		if !strings.Contains(err.Error(), "independent verification") {
			t.Fatalf("RECOVERY_HOLD was released with no verification evidence: %v", err)
		}

		// Evidence present, but the approver is the same person who released it.
		err = dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE ops.system_state
               SET recovery_hold = false, recovery_cleared_by = 'op-1',
                   recovery_cleared_approved_by = 'op-1', recovery_cleared_at = now(),
                   recovery_verified_by = 'op-3'
             WHERE environment = 'live'`)
		if !strings.Contains(err.Error(), "must differ from") {
			t.Fatalf("a self-approved release of RECOVERY_HOLD was accepted: %v", err)
		}

		// The verifier is one of the two who released it, so the verification is
		// not independent even though the approver differs.
		err = dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE ops.system_state
               SET recovery_hold = false, recovery_cleared_by = 'op-1',
                   recovery_cleared_approved_by = 'op-2', recovery_cleared_at = now(),
                   recovery_verified_by = 'op-2'
             WHERE environment = 'live'`)
		if !strings.Contains(err.Error(), "must be independent") {
			t.Fatalf("RECOVERY_HOLD was released with a non-independent verifier: %v", err)
		}
	})
}

// TestLeavingRecoveryHoldWithProperEvidenceSucceeds proves the control is not a
// latch. A recovery hold that cannot be lifted stops being a control and becomes
// an outage, and an operator who cannot verify their way out will find a way
// around the control instead.
func TestLeavingRecoveryHoldWithProperEvidenceSucceeds(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustEngageRecoveryHold(t, ctx, tx, "live", "region failover")
		if _, err := tx.ExecContext(ctx, `
            UPDATE ops.system_state
               SET recovery_hold = false, recovery_cleared_by = 'op-1',
                   recovery_cleared_approved_by = 'op-2', recovery_cleared_at = now(),
                   recovery_verified_by = 'op-3', mode = 'NORMAL', mode_changed_by = 'op-1',
                   mode_change_reason = 'verified restore', mode_approved_by = 'op-2'
             WHERE environment = 'live'`); err != nil {
			t.Fatalf("a properly evidenced release was refused: %v", err)
		}

		// And the environment genuinely trades again afterwards.
		order := mustRiskIncreasingOrder(t, ctx, tx, "live", 891)
		MustTransition(t, ctx, tx, order, "RISK_APPROVED", "RISK_APPROVED", 900)
	})
}

// TestSelfApprovalCannotReleaseARestrictedMode targets the nominal dual control
// 0007 provided. mode_approved_by only had to be non-null, so one person could
// both move and approve.
func TestSelfApprovalCannotReleaseARestrictedMode(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustSetOperationalMode(t, ctx, tx, "paper", "SAFE_HALT")
		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE ops.system_state SET mode = 'NORMAL', mode_changed_by = 'op-1',
                mode_change_reason = 'all clear', mode_approved_by = 'op-1'
             WHERE environment = 'paper'`)
		if !strings.Contains(err.Error(), "distinct identity") {
			t.Fatalf("a self-approved exit from SAFE_HALT was accepted: %v", err)
		}
	})
}

// TestLeavingARestrictedModeRequiresASecondApprover keeps release expensive
// where release is the dangerous direction.
func TestLeavingARestrictedModeRequiresASecondApprover(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustSetOperationalMode(t, ctx, tx, "paper", "SAFE_HALT")
		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE ops.system_state SET mode = 'NORMAL', mode_changed_by = 'op-1',
                mode_change_reason = 'all clear'
             WHERE environment = 'paper'`)
		if !strings.Contains(err.Error(), "second approver") {
			t.Fatalf("a restricted mode was left with no second approver: %v", err)
		}
	})
}

// TestReleasingTheKillSwitchRequiresDualControl covers the last-resort control.
// It is deliberately NOT required to have a third-party verifier, unlike
// RECOVERY_HOLD: the kill switch is engaged during a live incident, and
// requiring a third person to stand it down would make it a one-way door.
func TestReleasingTheKillSwitchRequiresDualControl(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustEngageKillSwitch(t, ctx, tx, "paper", "venue outage")

		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE ops.system_state SET kill_switch_engaged = false WHERE environment = 'paper'`)
		if !strings.Contains(err.Error(), "kill_switch_cleared_by") {
			t.Fatalf("the kill switch was released with no release evidence: %v", err)
		}

		err = dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE ops.system_state SET kill_switch_engaged = false,
                kill_switch_cleared_by = 'op-1', kill_switch_cleared_approved_by = 'op-1',
                kill_switch_cleared_at = now()
             WHERE environment = 'paper'`)
		if !strings.Contains(err.Error(), "must differ from") {
			t.Fatalf("the kill switch was released with a self-approval: %v", err)
		}

		if _, err := tx.ExecContext(ctx, `
            UPDATE ops.system_state SET kill_switch_engaged = false,
                kill_switch_cleared_by = 'op-1', kill_switch_cleared_approved_by = 'op-2',
                kill_switch_cleared_at = now()
             WHERE environment = 'paper'`); err != nil {
			t.Fatalf("a properly dual-controlled kill switch release was refused: %v", err)
		}
	})
}
