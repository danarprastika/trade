package dbtest_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/aitc/trade/dbtest"
)

// Workload identity / environment ceiling tests.
//
// Authority:
//   09_TESTING_AND_RELEASE_EVIDENCE.md invariant 8: "Lower-environment
//       credentials cannot access live resources."
//   01_SYSTEM_ARCHITECTURE.md section 4: "Each environment has separate
//       credentials... Live credentials are never available to lower
//       environments."
//   01_SYSTEM_ARCHITECTURE.md section 4: four hard trust zones; "Research
//       workloads cannot route to live control-plane data stores"; "The edge
//       cannot access databases."
//   06_SECURITY_AND_ACCESS_CONTROL.md: "Every service and worker has a unique
//       workload identity bound to its deployment and environment"; "Python
//       research workers have no live environment identity."
//
// SCOPE. This is DEFENCE IN DEPTH, not the primary control. The blueprint wants
// a separate database, secret store and network policy per environment; this
// repository has one database with an environment column. What these tests
// prove is narrower and still worth having: in a shared database, a credential
// issued for a lower environment cannot write a row in a higher one, and the
// refusal is a property of the database rather than of a service's own code.

// mustRegisterWorkload creates a database role and registers it as a workload.
//
// CREATE ROLE and GRANT are transactional in PostgreSQL, so both are rolled
// back with the surrounding Resettable transaction and leave no residue in the
// disposable test database. That is what makes it possible to test a real
// credential boundary rather than simulating one.
func mustRegisterWorkload(t *testing.T, ctx context.Context, tx *sql.Tx,
	role, zone, maxEnv string, seed int) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE ROLE %s NOLOGIN`, role)); err != nil {
		t.Fatalf("create role %s: %v", role, err)
	}
	// Grants are required so that a refusal provably comes from the guard and
	// not from a bare permission error. Without them a test asserting on the
	// message would pass for the wrong reason, since any INSERT would be
	// refused.
	dbtest.MustExec(t, ctx, tx, fmt.Sprintf(
		`GRANT USAGE ON SCHEMA common, oms, market, risk, ledger, ops, identity, audit TO %s`, role))
	dbtest.MustExec(t, ctx, tx, fmt.Sprintf(
		`GRANT ALL ON ALL TABLES IN SCHEMA common, oms, market, risk, ledger, ops, identity, audit TO %s`, role))
	dbtest.MustExec(t, ctx, tx, fmt.Sprintf(
		`GRANT ALL ON ALL SEQUENCES IN SCHEMA common, oms, market, risk, ledger, ops, identity, audit TO %s`, role))
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO ops.workload_identity
            (workload_id, principal, zone, max_environment, service_class,
             deployment, registered_by)
        VALUES ($1, $2, $3::ops.trust_zone, $4::common.environment, 'service',
                'test', 'op-1')`,
		dbtest.CanonicalID("wid", seed), role, zone, maxEnv); err != nil {
		t.Fatalf("register %s as %s/%s: %v", role, zone, maxEnv, err)
	}
}

// asRole switches the session to role and returns a function that restores it,
// so a refusal cannot silently leave the test running at a lower privilege than
// it started with.
func asRole(t *testing.T, ctx context.Context, tx *sql.Tx, role string) func() {
	t.Helper()
	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+role); err != nil {
		t.Fatalf("assume %s: %v", role, err)
	}
	return func() {
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE NONE"); err != nil {
			t.Fatalf("restore role: %v", err)
		}
	}
}

// TestLowerEnvironmentCredentialCannotReachLiveRows is mandatory invariant 8.
//
// A paper service credential is the realistic failure mode: in a shared
// database it is a valid credential with valid grants, and the only thing
// distinguishing it from a live service is which environment it was issued for.
// Before 0015 nothing in the database recorded or checked that, so a paper
// service could write live rows by naming a different environment.
func TestLowerEnvironmentCredentialCannotReachLiveRows(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		mustRegisterWorkload(t, ctx, tx, "aitc_paper_svc", "control", "paper", 900)
		restore := asRole(t, ctx, tx, "aitc_paper_svc")
		defer restore()

		// Its own environment is fine.
		if err := expectPermitted(t, ctx, tx, "paper"); err != nil {
			t.Fatalf("a paper credential was refused in its own environment: %v", err)
		}
		// Every environment above its ceiling is refused.
		for _, env := range []string{"shadow", "live"} {
			err := expectRefused(t, ctx, tx, env, "ceiling")
			if err != nil {
				t.Fatalf("a paper credential was not refused in %s: %v", env, err)
			}
		}
	})
}

// TestLiveCredentialMayReachLowerEnvironments proves the ceiling is a ceiling
// and not a whitelist of live. A live service legitimately performs rehearsals
// against paper data, and refusing that would push operators toward separate
// credentials for everything, which is worse.
func TestLiveCredentialMayReachLowerEnvironments(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		mustRegisterWorkload(t, ctx, tx, "aitc_live_svc", "control", "live", 910)
		restore := asRole(t, ctx, tx, "aitc_live_svc")
		defer restore()

		for _, env := range []string{"dev", "test", "staging", "paper", "shadow", "live"} {
			if err := expectPermitted(t, ctx, tx, env); err != nil {
				t.Fatalf("a live credential was refused in %s: %v", env, err)
			}
		}
	})
}

// TestUnregisteredCredentialIsRefused proves the fail-closed branch. "We have
// never heard of this principal" must be a refusal; a default-allow would mean
// any new service could write authoritative financial rows simply by being
// deployed.
func TestUnregisteredCredentialIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		if _, err := tx.ExecContext(ctx, `CREATE ROLE aitc_stranger NOLOGIN`); err != nil {
			t.Fatalf("create role: %v", err)
		}
		dbtest.MustExec(t, ctx, tx, `GRANT USAGE ON SCHEMA common, oms, market, risk, ledger, ops TO aitc_stranger`)
		dbtest.MustExec(t, ctx, tx, `GRANT ALL ON ALL TABLES IN SCHEMA common, oms, market, risk, ledger, ops TO aitc_stranger`)
		restore := asRole(t, ctx, tx, "aitc_stranger")
		defer restore()

		for _, env := range []string{"paper", "live"} {
			if err := expectRefused(t, ctx, tx, env, "no ops.workload_identity registration"); err != nil {
				t.Fatalf("an unregistered credential was not refused in %s: %v", env, err)
			}
		}
	})
}

// TestRevokedCredentialIsRefusedEverywhere proves revocation is total. A revoked
// credential that still works in lower environments is only partly revoked, and
// "revoked" is a word an incident responder will trust absolutely.
func TestRevokedCredentialIsRefusedEverywhere(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		mustRegisterWorkload(t, ctx, tx, "aitc_revoked_svc", "control", "live", 920)
		if _, err := tx.ExecContext(ctx, `
            UPDATE ops.workload_identity
               SET state = 'REVOKED', revoked_by = 'op-1', revoked_at = now(),
                   revocation_reason = 'suspected exposure'
             WHERE principal = 'aitc_revoked_svc'`); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		restore := asRole(t, ctx, tx, "aitc_revoked_svc")
		defer restore()

		// Even in its own former environment.
		if err := expectRefused(t, ctx, tx, "live", "is REVOKED"); err != nil {
			t.Fatalf("a revoked credential was still usable: %v", err)
		}
	})
}

// TestResearchZoneHasNoWritePathToFinancialTables is 02_POLYGLOT: "Python is
// restricted to research... It has no production credentials and no direct write
// path to authoritative financial tables."
func TestResearchZoneHasNoWritePathToFinancialTables(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		mustRegisterWorkload(t, ctx, tx, "aitc_py_research", "research", "shadow", 930)
		restore := asRole(t, ctx, tx, "aitc_py_research")
		defer restore()

		// Refused in every environment, including ones below its ceiling. The
		// check runs before the ceiling so the error names the real reason.
		for _, env := range []string{"dev", "paper", "shadow", "live"} {
			if err := expectRefused(t, ctx, tx, env, "research-zone workload"); err != nil {
				t.Fatalf("a research workload was not refused in %s: %v", env, err)
			}
		}
	})
}

// TestEdgeZoneCannotBeRegistered encodes "The edge cannot access databases" as a
// property of the data rather than of any code path. There must be no edge
// principal in this database, so a future change that tries to give one to the
// edge is refused by the database.
func TestEdgeZoneCannotBeRegistered(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.workload_identity
                (workload_id, principal, zone, max_environment, registered_by)
            VALUES ($1, 'aitc_edge_ingress', 'edge', 'dev', 'op-1')`,
			dbtest.CanonicalID("wid", 940))
		if !strings.Contains(err.Error(), "edge_has_no_database_credential") {
			t.Fatalf("an edge principal was registered against the database: %v", err)
		}
	})
}

// TestResearchZoneCannotHoldALiveCeiling encodes 06_SECURITY_AND_ACCESS_CONTROL:
// "Python research workers have no live environment identity."
//
// Two independent constraints express this rule -- research_has_no_live_identity
// and live_ceiling_requires_control_or_recovery -- and PostgreSQL reports
// whichever it evaluates first, so the assertion accepts either. What matters is
// that the row cannot exist, not which of two correct guards caught it.
func TestResearchZoneCannotHoldALiveCeiling(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO ops.workload_identity
                (workload_id, principal, zone, max_environment, registered_by)
            VALUES ($1, 'aitc_py_live', 'research', 'live', 'op-1')`,
			dbtest.CanonicalID("wid", 941))
		if !strings.Contains(err.Error(), "research_has_no_live_identity") &&
			!strings.Contains(err.Error(), "live_ceiling_requires_control_or_recovery") {
			t.Fatalf("a research workload was registered with a live identity: %v", err)
		}
	})
}

// TestRevocationMustBeEvidenced keeps revocation usable as evidence. A revoked
// credential with no reason cannot be triaged, and rotation is required at most
// every 90 days and immediately on suspected exposure.
func TestRevocationMustBeEvidenced(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		mustRegisterWorkload(t, ctx, tx, "aitc_rotate_svc", "control", "paper", 942)
		err := dbtest.ExpectRejected(t, ctx, tx, `
            UPDATE ops.workload_identity SET state = 'REVOKED' WHERE principal = 'aitc_rotate_svc'`)
		if !strings.Contains(err.Error(), "revocation_is_evidenced") {
			t.Fatalf("a credential was revoked with no reason or actor: %v", err)
		}
	})
}

// TestTheOwningPrincipalIsRegistered guards the bootstrap. If the role that
// applies migrations were unregistered, every subsequent migration would be
// refused -- loudly, by design. This asserts the registration exists so the
// failure is diagnosed as a missing bootstrap rather than as mysterious
// permission errors during a future migration.
func TestTheOwningPrincipalIsRegistered(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		var zone, maxEnv, state string
		if err := tx.QueryRowContext(ctx, `
            SELECT zone::text, max_environment::text, state
              FROM ops.workload_identity WHERE principal = session_user`).
			Scan(&zone, &maxEnv, &state); err != nil {
			t.Fatalf("the migration owner is not registered as a workload: %v", err)
		}
		if zone != "control" || maxEnv != "live" || state != "ACTIVE" {
			t.Fatalf("the migration owner is registered as %s/%s/%s, want control/live/ACTIVE",
				zone, maxEnv, state)
		}
	})
}

// TestPrincipalGuardIsAttachedToAuthoritativeTables proves the triggers exist
// on the tables that matter. A guard function that nothing calls is the exact
// defect class this work has been hunting, so its attachment is asserted rather
// than assumed.
func TestPrincipalGuardIsAttachedToAuthoritativeTables(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		for _, table := range []string{"oms.order", "ledger.entry", "ops.outbox"} {
			var n int
			dbtest.MustQueryRow(t, ctx, tx, &n, `
                SELECT count(*) FROM pg_trigger
                 WHERE tgname LIKE '%_principal_guard' AND tgrelid = $1::regclass`,
				table)
			if n == 0 {
				t.Fatalf("%s has no principal guard attached; ops.assert_principal_permitted "+
					"would exist but never run, which is the failure mode this migration was "+
					"written to eliminate", table)
			}
		}
	})
}

// TestEnvironmentRankMatchesGo is a cross-language parity test.
//
// common.environment_rank must agree with contracts.Environment.Rank() in Go.
// A divergence would be silent and dangerous: the Go service would consider an
// environment live while the database considered it out of range, or the
// reverse, and neither side would notice until an incident. The Go ladder is
// the authority; the expected values here are the ladder from
// 09_TESTING_AND_RELEASE_EVIDENCE.md, "Release strategy", as Go implements it.
//
// The values are spelled out literally rather than derived from a Go call
// because a test that computes both sides from the same source proves nothing.
func TestEnvironmentRankMatchesGo(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		want := map[string]int{
			"dev": 1, "test": 2, "staging": 3,
			"paper": 4, "shadow": 5, "live": 6,
		}

		rows, err := tx.QueryContext(ctx,
			`SELECT e::text, common.environment_rank(e)
			   FROM unnest(enum_range(NULL::common.environment)) AS e`)
		if err != nil {
			t.Fatalf("read environment ranks: %v", err)
		}
		defer rows.Close()

		seen := 0
		for rows.Next() {
			var env string
			var rank int
			if err := rows.Scan(&env, &rank); err != nil {
				t.Fatalf("scan: %v", err)
			}
			seen++
			if want[env] != rank {
				t.Fatalf("common.environment_rank(%s) = %d, want %d; the SQL ladder and "+
					"contracts.Environment.Rank() have diverged", env, rank, want[env])
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate: %v", err)
		}
		if seen != len(want) {
			t.Fatalf("read %d environments, want %d; the closed enum and the ladder disagree",
				seen, len(want))
		}
	})
}

func expectPermitted(t *testing.T, ctx context.Context, tx *sql.Tx, env string) error {
	t.Helper()
	if _, err := tx.ExecContext(ctx,
		`SELECT ops.assert_principal_permitted($1::common.environment, 'test')`, env); err != nil {
		return err
	}
	return nil
}

func expectRefused(t *testing.T, ctx context.Context, tx *sql.Tx, env, want string) error {
	t.Helper()
	err := dbtest.ExpectRejected(t, ctx, tx,
		`SELECT ops.assert_principal_permitted($1::common.environment, 'test')`, env)
	if err == nil {
		t.Fatalf("environment %s was permitted but should have been refused (%s)", env, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("environment %s was refused, but not by the expected control %q: %v", env, want, err)
	}
	return nil
}
