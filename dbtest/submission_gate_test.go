package dbtest_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/adapters/venue"
	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/execution"
	"github.com/aitc/trade/domain/oms"
	"github.com/aitc/trade/services/audit"
)

// The certification gate on the submission path.
//
// Everything asserted here was previously unassertable, because nothing in the
// submit path read a certification. Gate and LoadCertification were exercised only
// by tests that constructed them directly, so the rule from doc 16 section 4 --
// that an uncertified adapter may not be used -- had no enforcement point at all.
//
// Each test asserts two things, and the second is the one that matters. The first
// is that a refusal happens. The second is that the refusal happens BEFORE
// anything is written: the order stays RISK_APPROVED, no submission row exists, no
// outbox row exists. A gate checked after the commit would satisfy the first
// assertion and leave the exact harm the gate exists to prevent -- an order
// advanced to SUBMITTING with a durable PENDING submission presenting as in-flight
// when no venue was ever going to be asked.

// gateNow is the clock every gate in this file uses.
//
// Certification expiry is a comparison against a point in time, so a test that
// reads the wall clock cannot distinguish "expired yesterday" from "expires in an
// hour" without sleeping. The clock is fixed here instead.
var gateNow = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

// seedCapability writes one execution.adapter_capability row.
//
// It is not mustCertify. That helper hardcodes supported = true and an expiry a
// year out, which is correct for the parity tests it serves and useless here: half
// of what follows is about supported = false and about a certification that has
// already expired, and a helper that cannot express either would force those cases
// to be tested some other way or not at all.
func seedCapability(t *testing.T, ctx context.Context, tx *sql.Tx, adapterID string,
	env contracts.Environment, capability venue.Capability, state venue.CertificationState,
	supported bool, expiresAt time.Time, seed int) {

	t.Helper()
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO execution.adapter_capability (
			adapter_id, environment, venue_id, capability, supported,
			certification_state, certified_at, certification_expires_at,
			certified_by, content_digest)
		VALUES ($1, $2::common.environment, 'ven_test', $3, $4,
		        $5, $6, $7, 'certifier-1', $8)`,
		adapterID, string(env), string(capability), supported, string(state),
		gateNow.Add(-30*24*time.Hour), expiresAt, dbtest.UniqueDigest(seed))
}

// declaredGate returns a real venue.SubmissionGate over db, with adapterID declared
// for the given capabilities.
//
// The declaration is genuine -- it goes through Declaration.Declare, so it is
// validated and immutable -- so what these tests exercise is the real pairing of an
// in-process declaration with a persisted certification, not a stub.
func declaredGate(t *testing.T, db *sql.DB, adapterID string,
	supported ...venue.Capability) *venue.SubmissionGate {

	t.Helper()
	set := map[venue.Capability]bool{}
	for _, c := range supported {
		set[c] = true
	}
	decl := venue.Declaration{
		AdapterID:        adapterID,
		Vendor:           "test-venue",
		CertifiedAt:      gateNow.Add(-30 * 24 * time.Hour),
		ExpiresAt:        gateNow.Add(365 * 24 * time.Hour),
		Supported:        set,
		IdempotentSubmit: true,
		StateMap:         map[string]oms.State{"new": oms.StateAcknowledged},
	}
	// Declare first so a malformed declaration fails the test here rather than
	// surfacing later as a refusal that looks like a certification problem.
	if _, err := decl.Declare(); err != nil {
		t.Fatalf("declaring %s: %v", adapterID, err)
	}
	decls := venue.NewDeclarationSet()
	if _, err := decls.Declare(decl); err != nil {
		t.Fatalf("registering %s: %v", adapterID, err)
	}
	gate, err := venue.NewSubmissionGate(decls, db, func() time.Time { return gateNow })
	if err != nil {
		t.Fatalf("NewSubmissionGate: %v", err)
	}
	return gate
}

// gatedCommand builds a submit Command whose gate is the real one.
func gatedCommand(t *testing.T, db *sql.DB, gate execution.SubmissionGate) *execution.Command {
	t.Helper()
	st, err := execution.NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	c, err := execution.NewCommand(db, st, audit.NewAppender(db), gate, "svc-execution",
		TestSigningKeyID)
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	return c
}

// adapterID mints a distinct adapter id per test.
//
// Certification rows persist for the life of the database, so a shared id would let
// one test's certified row satisfy another test's request -- and a test that passes
// because of a neighbouring test's fixture is worse than no test.
func adapterID(t *testing.T, seed int) string {
	t.Helper()
	return dbtest.CanonicalID("ven", int(dbtest.UniqueSequence(seed)))
}

// intentFor is submitIntent with the adapter this test is about.
func intentFor(t *testing.T, orderID, corr string, env contracts.Environment, adapter string) execution.SubmitIntent {
	t.Helper()
	in := submitIntent(t, orderID, corr, env)
	in.AdapterID = adapter
	return in
}

// assertNothingPrepared asserts the refusal left no trace.
//
// This is the assertion that makes the test worth having. Without it, a gate that
// refused only after committing would pass every check above.
func assertNothingPrepared(t *testing.T, db *sql.DB, orderID string) {
	t.Helper()
	ctx := context.Background()

	var state string
	if err := db.QueryRowContext(ctx,
		`SELECT state::text FROM oms."order" WHERE order_id = $1`, orderID).Scan(&state); err != nil {
		t.Fatalf("reading the order state: %v", err)
	}
	if state != string(oms.StateRiskApproved) {
		t.Errorf("order state = %s, want %s; a refused submission must not advance the order, "+
			"because an order left in SUBMITTING with no send presents as one in flight",
			state, oms.StateRiskApproved)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM execution.submission WHERE order_id = $1`, orderID).Scan(&n); err != nil {
		t.Fatalf("counting submissions: %v", err)
	}
	if n != 0 {
		t.Errorf("submission rows = %d, want 0; a refused submission must not record a send that "+
			"was never authorised", n)
	}

	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ops.outbox WHERE aggregate_id = $1 AND event_type = 'ORDER_SUBMIT_REQUESTED'`,
		orderID).Scan(&n); err != nil {
		t.Fatalf("counting outbox rows: %v", err)
	}
	if n != 0 {
		t.Errorf("submit outbox rows = %d, want 0", n)
	}
}

// An adapter nobody certified is refused, and nothing is written.
func TestAnUncertifiedAdapterCannotHaveItsOrderPrepared(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 810)

	// Declared in this process, certified by nobody in the database. This is the
	// precise gap the gate closes: an in-process declaration asserting a capability
	// is not evidence that anyone tested the adapter behind it.
	gate := declaredGate(t, db, adapter, venue.CapabilitySubmit)

	_, err := gatedCommand(t, db, gate).Prepare(ctx,
		intentFor(t, orderID, corr, contracts.EnvPaper, adapter))
	if err == nil {
		t.Fatal("an adapter with no certification row had its order prepared")
	}
	if !errors.Is(err, venue.ErrNotCertified) {
		t.Errorf("the refusal did not report ErrNotCertified: %v", err)
	}
	assertNothingPrepared(t, db, orderID)
}

// A certification that has expired is refused exactly as though it never existed.
func TestAnExpiredCertificationRefusesSubmission(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 820)
	gate := declaredGate(t, db, adapter, venue.CapabilitySubmit)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		// Expired an hour before the gate's fixed clock.
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(-time.Hour), 821)
	})

	_, err := gatedCommand(t, db, gate).Prepare(ctx,
		intentFor(t, orderID, corr, contracts.EnvPaper, adapter))
	if err == nil {
		t.Fatal("an expired certification permitted a submission")
	}
	if !errors.Is(err, venue.ErrCertificationExpired) {
		t.Errorf("the refusal did not report ErrCertificationExpired: %v", err)
	}
	assertNothingPrepared(t, db, orderID)
}

// A paper certification is not a live certification, and the live environment has
// no row to read. This is the case where "absent evidence" and "permissive
// evidence" differ by everything.
func TestAPaperCertificationDoesNotPermitALiveSubmission(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 830)
	gate := declaredGate(t, db, adapter, venue.CapabilitySubmit)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		// The most permissive paper state available, so the refusal cannot be
		// attributed to the state -- only to the absence of a live row.
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.LiveEligible, true, gateNow.Add(365*24*time.Hour), 831)
	})

	_, err := gatedCommand(t, db, gate).Prepare(ctx,
		intentFor(t, orderID, corr, contracts.EnvLive, adapter))
	if err == nil {
		t.Fatal("a LIVE_ELIGIBLE paper certification permitted a live submission")
	}
	if !errors.Is(err, venue.ErrNotCertified) {
		t.Errorf("the refusal did not report ErrNotCertified: %v", err)
	}
	// The message must name the missing row, because "you are not certified" and
	// "you are certified in another environment" are different support tickets.
	if !strings.Contains(err.Error(), "live") {
		t.Errorf("the refusal does not mention the live environment: %v", err)
	}
	assertNothingPrepared(t, db, orderID)
}

// A revoked certification is a refusal, not a lesser certification.
func TestARevokedCertificationRefusesSubmission(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 840)
	gate := declaredGate(t, db, adapter, venue.CapabilitySubmit)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.Revoked, true, gateNow.Add(365*24*time.Hour), 841)
	})

	_, err := gatedCommand(t, db, gate).Prepare(ctx,
		intentFor(t, orderID, corr, contracts.EnvPaper, adapter))
	if err == nil {
		t.Fatal("a REVOKED certification permitted a submission")
	}
	if !errors.Is(err, venue.ErrNotCertified) {
		t.Errorf("the refusal did not report ErrNotCertified: %v", err)
	}
	assertNothingPrepared(t, db, orderID)
}

// Declaring SUBMIT and certifying something else is not certification of SUBMIT.
// An adapter certified only for HEALTH has no authority to place an order, and a
// gate that asked about "is this adapter certified" rather than "is SUBMIT
// certified" would let it through.
func TestCertifyingAnotherCapabilityDoesNotPermitSubmit(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 850)
	gate := declaredGate(t, db, adapter, venue.CapabilitySubmit)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilityHealth,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 851)
	})

	_, err := gatedCommand(t, db, gate).Prepare(ctx,
		intentFor(t, orderID, corr, contracts.EnvPaper, adapter))
	if err == nil {
		t.Fatal("an adapter certified only for HEALTH had an order prepared for submission")
	}
	if !errors.Is(err, venue.ErrNotCertified) {
		t.Errorf("the refusal did not report ErrNotCertified: %v", err)
	}
	if !strings.Contains(err.Error(), string(venue.CapabilitySubmit)) {
		t.Errorf("the refusal does not name the capability that was missing: %v", err)
	}
	assertNothingPrepared(t, db, orderID)
}

// An adapter nobody declared at all is refused before certification is even read.
// There is no state map, so nothing could translate the states it reports, and no
// declared capability set, so nothing could say what it is allowed to do.
func TestAnUndeclaredAdapterIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 860)

	// Certified in the database, and still refused, because this process holds no
	// declaration for it. Certification without a declaration is evidence about an
	// adapter that cannot actually be called.
	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 861)
	})

	decls := venue.NewDeclarationSet()
	gate, err := venue.NewSubmissionGate(decls, db, func() time.Time { return gateNow })
	if err != nil {
		t.Fatalf("NewSubmissionGate: %v", err)
	}
	_, err = gatedCommand(t, db, gate).Prepare(ctx,
		intentFor(t, orderID, corr, contracts.EnvPaper, adapter))
	if err == nil {
		t.Fatal("an adapter with no declaration had an order prepared for submission")
	}
	if !errors.Is(err, venue.ErrUnknownAdapter) {
		t.Errorf("the refusal did not report ErrUnknownAdapter: %v", err)
	}
	assertNothingPrepared(t, db, orderID)
}

// The positive case, and it is asserted rather than assumed.
//
// Every other test in this file proves a refusal. Without this one, the suite
// would still pass if the gate refused everything -- including adapters that are
// certified for exactly what they are doing -- and the failure would be a system
// that places no orders at all while looking correctly cautious.
func TestACertifiedAdapterIsPermittedAndTheOrderCommits(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 870)
	gate := declaredGate(t, db, adapter, venue.CapabilitySubmit)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 871)
	})

	prepared, err := gatedCommand(t, db, gate).Prepare(ctx,
		intentFor(t, orderID, corr, contracts.EnvPaper, adapter))
	if err != nil {
		t.Fatalf("a certified adapter was refused: %v", err)
	}
	if prepared.SubmissionID.String() == "" {
		t.Fatal("Prepare returned no submission id")
	}

	var state string
	if err := db.QueryRowContext(ctx,
		`SELECT state::text FROM oms."order" WHERE order_id = $1`, orderID).Scan(&state); err != nil {
		t.Fatalf("reading the order state: %v", err)
	}
	if state != string(oms.StateSubmitting) {
		t.Errorf("order state = %s, want %s after a permitted submit", state, oms.StateSubmitting)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM execution.submission WHERE order_id = $1`, orderID).Scan(&n); err != nil {
		t.Fatalf("counting submissions: %v", err)
	}
	if n != 1 {
		t.Errorf("submission rows = %d, want 1", n)
	}
}

// The same gate, consulted twice, still refuses after the certification is
// withdrawn.
//
// The revocation window this file does not close is between Prepare committing and
// the dispatcher calling the venue. This test pins the behaviour that makes closing
// it possible: the gate reads certification per request, so a withdrawal takes
// effect immediately rather than at the next deploy.
func TestARevocationTakesEffectOnTheNextRequest(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 880)
	gate := declaredGate(t, db, adapter, venue.CapabilitySubmit)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 881)
	})
	if err := gate.PermitSubmit(ctx, adapter, contracts.EnvPaper); err != nil {
		t.Fatalf("the certified adapter was refused before revocation: %v", err)
	}

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		if _, err := tx.ExecContext(ctx, `
			UPDATE execution.adapter_capability
			   SET certification_state = 'REVOKED'
			 WHERE adapter_id = $1 AND environment = $2::common.environment AND capability = $3`,
			adapter, string(contracts.EnvPaper), string(venue.CapabilitySubmit)); err != nil {
			t.Fatalf("revoking: %v", err)
		}
	})

	if err := gate.PermitSubmit(ctx, adapter, contracts.EnvPaper); err == nil {
		t.Fatal("a gate built before the revocation still permitted the next request")
	}

	// And the order is still preparable-refused, not merely the gate.
	_, err := gatedCommand(t, db, gate).Prepare(ctx,
		intentFor(t, orderID, corr, contracts.EnvPaper, adapter))
	if err == nil {
		t.Fatal("a revoked adapter's order was prepared after revocation")
	}
	assertNothingPrepared(t, db, orderID)
}
