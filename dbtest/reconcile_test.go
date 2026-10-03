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
	"github.com/aitc/trade/services/audit"
	"github.com/aitc/trade/services/reconcile"
)

// Reconciliation.
//
// The rule every test here serves is the one in the package comment: a check that
// did not run is never reported as a check that found nothing.
//
// It is worth being blunt about why this needs its own tests rather than being
// taken on trust. The failure is silent, it is the most reassuring failure
// available, and it is invisible to every other kind of assertion. A runner that
// skipped all four checks and reported COMPLETED with zero counts would produce
// exactly the report an operator wants to see. There is no difference between that
// runner and a working one, anywhere in the output, unless something checks the
// per-check status rather than the run status. So most of what follows asserts on
// CheckResult.Status and CheckResult.Checked, not on Run.Status.

// fakeReader is a venue.Reader whose every answer is scripted.
//
// There is no concrete venue adapter in this repository and none may be invented as
// though it were real, so reconciliation's input is a double. What is NOT a double
// is everything downstream of it: the certification gate, the comparison, the
// grading, the case rows, the audit chain and the outbox are all the real thing
// against the live schema. The tests are therefore evidence about the control
// plane's behaviour given a venue's answers, and not evidence that any venue
// answers this way.
type fakeReader struct {
	orders    map[string]venue.VenueOrder
	orderErr  error
	fills     []venue.VenueFill
	fillsErr  error
	balances  []venue.VenueBalance
	balErr    error
	positions []venue.VenuePosition
	posErr    error

	// asked records which capabilities were actually exercised, so a test can prove
	// a skipped check never touched the venue.
	asked map[string]int
}

func newFakeReader() *fakeReader {
	return &fakeReader{orders: map[string]venue.VenueOrder{}, asked: map[string]int{}}
}

func (f *fakeReader) LookupOrder(ctx context.Context, clientOrderID string) (venue.VenueOrder, error) {
	f.asked["orders"]++
	if f.orderErr != nil {
		return venue.VenueOrder{}, f.orderErr
	}
	o, ok := f.orders[clientOrderID]
	if !ok {
		// A venue that knows nothing about an order returns an empty record rather
		// than an error, which is what checkOrders reads as ORDER_MISSING.
		return venue.VenueOrder{}, nil
	}
	return o, nil
}

func (f *fakeReader) Fills(ctx context.Context, since time.Time) ([]venue.VenueFill, error) {
	f.asked["fills"]++
	return f.fills, f.fillsErr
}

func (f *fakeReader) Balances(ctx context.Context) ([]venue.VenueBalance, error) {
	f.asked["balances"]++
	return f.balances, f.balErr
}

func (f *fakeReader) Positions(ctx context.Context) ([]venue.VenuePosition, error) {
	f.asked["positions"]++
	return f.positions, f.posErr
}

func (f *fakeReader) Health(ctx context.Context) error { return nil }

// reconcileVenue is the venue the shared fixtures use. readyOrder stamps this value
// onto oms.order, and the runner's account lookup and order probe both filter on it,
// so a runner configured with anything else would compare the wrong books and report
// agreement about the wrong account.
const reconcileVenue = "ven-1"

// capabilitiesThatDriveNoCheck are doc 16 capabilities that no reconciliation check
// consults.
//
// They exist in these tests because LoadCertification refuses an adapter with NO
// certification rows in an environment at all -- correctly, since an adapter nobody
// certified must not look usable. So "an adapter certified for nothing reconciliation
// needs" has to be expressed as an adapter certified for something else, which is
// also the more honest shape of the problem: what matters is not that an adapter is
// unknown but that this particular capability was never certified for it.
var capabilitiesThatDriveNoCheck = []venue.Capability{
	venue.CapabilityDiscovery, venue.CapabilityAuthenticate,
	venue.CapabilityRateLimit, venue.CapabilityHealth,
}

// reconcileGate builds a real venue.Gate for adapterID with the given certified
// capabilities.
//
// It goes through the same declaredGate helper the submission gate tests use, so
// what is exercised here is a genuine declaration paired with genuine persisted
// certification rows rather than a stub that answers yes. Passing no capabilities
// certifies the ones that drive no check, which leaves every check SKIPPED.
func reconcileGate(t *testing.T, db *sql.DB, adapterID string,
	caps ...venue.Capability) *venue.Gate {

	t.Helper()
	ctx := context.Background()
	if len(caps) == 0 {
		caps = capabilitiesThatDriveNoCheck
	}
	certify(t, ctx, db, adapterID, caps, int(seq()))
	g, err := declaredGate(t, db, adapterID, caps...).GateFor(ctx, adapterID, contracts.EnvPaper)
	if err != nil {
		t.Fatalf("building the gate for %s: %v", adapterID, err)
	}
	return g
}

// certify writes certification rows for an adapter's capabilities.
func certify(t *testing.T, ctx context.Context, db *sql.DB, adapterID string,
	caps []venue.Capability, seed int) {

	t.Helper()
	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		for i, c := range caps {
			seedCapability(t, ctx, tx, adapterID, contracts.EnvPaper, c,
				venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), seed+i)
		}
	})
}

// newReconciler builds a Runner over a real gate, the double reader and the live
// database.
// newReconciler builds a Runner over a real gate, the double reader and the live
// database.
//
// The account is minted here and exposed through r.Account(), so a test that needs
// to seed portfolio.balance can do so against the very account the runner will
// compare -- rather than the runner discovering one, which is what made the first
// version of these tests nondeterministic.
func newReconciler(t *testing.T, db *sql.DB, gate *venue.Gate, reader venue.Reader,
	opts ...reconcile.Option) *reconcile.Runner {

	t.Helper()
	opts = append([]reconcile.Option{
		reconcile.WithAccount(dbtest.CanonicalID("acc", int(seq()))),
		reconcile.WithOwner("ops-reconciliation"),
		reconcile.WithPolicyVersion(dbtest.CanonicalID("pol", int(seq()))),
		reconcile.WithSigningKey(TestSigningKeyID),
		reconcile.WithClock(func() time.Time { return gateNow }),
		reconcile.WithSLA(2 * time.Hour),
	}, opts...)
	r, err := reconcile.NewRunner(db, gate, reader, audit.NewAppender(db),
		gate.Registry().AdapterID(), reconcileVenue, opts...)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	clearCases(t, db)
	return r
}

// clearCases resolves the cases this file's runners open.
//
// This is not tidiness. reconciliation.case is the blocking control: an unresolved
// MATERIAL break refuses risk-increasing activity at its venue, which is enforced by
// a trigger on oms.order. The MATERIAL cases these tests deliberately open would
// therefore block every LATER test that advances an order at this venue -- including
// tests in other files, which is how TestANonMaterialBreakDoesNotBlockTrading came to
// fail for a reason that had nothing to do with what it asserts.
//
// That failure was the control working correctly and the fixture leaking. The
// committed-row-lifetime problem is the same one already recorded for the risk gate's
// committed policy fixture, and the answer is the same: a test that commits a row
// which outlives it must clean it up.
//
// The cleanup is scoped to owner_subject_id = ops-reconciliation so it cannot touch a
// case belonging to any other test.
func clearCases(t *testing.T, db *sql.DB) {
	t.Helper()
	resolve := func() {
		// A rollback is the wrong tool: the cases are committed, which is the whole
		// point of them.
		_, _ = db.ExecContext(context.Background(), `
			UPDATE reconciliation.case
			   SET status = 'RESOLVED',
			       resolved_at = now(),
			       resolved_by = 'dbtest-cleanup',
			       resolution_note = 'reconciliation runner test fixture, resolved on cleanup'
			 WHERE owner_subject_id = 'ops-reconciliation'
			   AND status IN ('OPEN','INVESTIGATING','REOPENED')`)
	}
	// Cleared on entry as well as on exit. A run that was interrupted -- a failing
	// assertion before the runner was even built, a killed process -- leaves
	// committed MATERIAL cases behind, and the next run would then fail for a reason
	// that has nothing to do with what it asserts. That is exactly how
	// TestANonMaterialBreakDoesNotBlockTrading came to fail.
	resolve()
	t.Cleanup(resolve)
}

// checkByName returns one check's result.
func checkByName(t *testing.T, run *reconcile.Run, name string) reconcile.CheckResult {
	t.Helper()
	for _, c := range run.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in the run; checks were %v", name, checkNames(run))
	return reconcile.CheckResult{}
}

func checkNames(run *reconcile.Run) []string {
	out := make([]string, 0, len(run.Checks))
	for _, c := range run.Checks {
		out = append(out, c.Name)
	}
	return out
}

// THE RULE, one half: an uncertified capability is SKIPPED with a reason, and the
// venue is never asked.
//
// The reader here returns data for every capability. If the runner consulted the
// reader instead of the declaration it would find four completed, agreeing checks
// and no cases -- the exact shape the earlier broken draft produced.
func TestAnUncertifiedCapabilityIsSkippedAndTheVenueIsNeverAsked(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1100)

	// Deliberately no certified capabilities at all.
	gate := reconcileGate(t, db, adapter)

	reader := newFakeReader()
	reader.balances = []venue.VenueBalance{
		{Currency: "USD", Total: contracts.MustParseDecimal("1000"),
			Available: contracts.MustParseDecimal("1000")},
	}
	reader.fills = []venue.VenueFill{
		{VenueFillRef: "vfill-1", Quantity: contracts.MustParseDecimal("1"),
			Price: contracts.MustParseDecimal("100")},
	}

	run, err := newReconciler(t, db, gate, reader).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, c := range run.Checks {
		if c.Status != reconcile.Skipped {
			t.Errorf("check %s: status = %s, want SKIPPED. A capability the adapter has not "+
				"certified cannot be evidence of agreement", c.Name, c.Status)
		}
		if c.Reason == "" {
			t.Errorf("check %s: SKIPPED with no recorded reason, which is indistinguishable "+
				"from a bug", c.Name)
		}
		if c.Checked != 0 {
			t.Errorf("check %s: Checked = %d, want 0. A skipped check contributes nothing "+
				"to any count", c.Name, c.Checked)
		}
		if len(c.Findings) != 0 {
			t.Errorf("check %s: reported %d findings without ever asking the venue",
				c.Name, len(c.Findings))
		}
	}
	for _, asked := range []string{"orders", "fills", "balances", "positions"} {
		if reader.asked[asked] != 0 {
			t.Errorf("the venue was asked for %s %d times by an adapter certified for nothing; "+
				"an uncertified adapter must never be called", asked, reader.asked[asked])
		}
	}
	if run.OrdersChecked != 0 || run.FillsChecked != 0 || run.BalancesChecked != 0 {
		t.Errorf("run counts = orders %d / fills %d / balances %d, want zero",
			run.OrdersChecked, run.FillsChecked, run.BalancesChecked)
	}
	if run.DifferencesFound != 0 {
		t.Errorf("DifferencesFound = %d, want 0; a check that did not run cannot find a "+
			"difference", run.DifferencesFound)
	}
	if n := count(t, db,
		`SELECT count(*) FROM reconciliation.case WHERE venue_id = $1 AND severity = 'MATERIAL'
		   AND status IN ('OPEN','INVESTIGATING','REOPENED')`, reconcileVenue); n != 0 {
		t.Errorf("opened %d MATERIAL cases from checks that never ran", n)
	}
}

// THE RULE, the other half: a capability the adapter HAS, where the venue then
// cannot answer, is FAILED -- and the run is FAILED, so nobody can read it as clean.
func TestAVenueThatCannotBeAskedMakesTheRunFailedNotClean(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1110)

	caps := []venue.Capability{
		venue.CapabilityOrderLookup, venue.CapabilityFillRetrieval,
		venue.CapabilityBalanceRetrieval, venue.CapabilityPositionRetrieval,
	}
	gate := reconcileGate(t, db, adapter, caps...)

	reader := newFakeReader()
	reader.balErr = errors.New("the venue's balance endpoint returned 503")

	run, err := newReconciler(t, db, gate, reader).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	bal := checkByName(t, run, "balances")
	if bal.Status != reconcile.Failed {
		t.Errorf("balances: status = %s, want FAILED. A venue that cannot be asked has not "+
			"been checked", bal.Status)
	}
	if bal.Checked != 0 {
		t.Errorf("balances: Checked = %d, want 0", bal.Checked)
	}
	if !strings.Contains(bal.Reason, "503") {
		t.Errorf("balances: the recorded reason does not carry the cause: %q", bal.Reason)
	}
	if run.Status != "FAILED" {
		t.Errorf("run status = %s, want FAILED. A run that could not complete every check "+
			"must not present as COMPLETED", run.Status)
	}
	if run.BalancesChecked != 0 {
		t.Errorf("BalancesChecked = %d, want 0", run.BalancesChecked)
	}

	var stored string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM reconciliation.check_run WHERE check_run_id = $1`,
		run.CheckRunID.String()).Scan(&stored); err != nil {
		t.Fatalf("reading the check run: %v", err)
	}
	if stored != "FAILED" {
		t.Errorf("check_run.status = %s, want FAILED", stored)
	}
	var detail sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT error_detail FROM reconciliation.check_run WHERE check_run_id = $1`,
		run.CheckRunID.String()).Scan(&detail); err != nil {
		t.Fatalf("reading the check run detail: %v", err)
	}
	if !detail.Valid || detail.String == "" {
		t.Error("a FAILED check run recorded no error_detail, so the next reader cannot " +
			"tell what could not be checked")
	}
}

// Capabilities come from the certified declaration, not from what the reader can
// do. One capability certified, three not: exactly one check runs.
func TestExactlyTheCertifiedCapabilitiesRun(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1120)

	only := []venue.Capability{venue.CapabilityBalanceRetrieval}
	gate := reconcileGate(t, db, adapter, only...)

	// The reader can answer everything, and returns a balance that AGREES with
	// nothing locally, because there is no local balance row for this account.
	reader := newFakeReader()
	reader.balances = []venue.VenueBalance{
		{Currency: "USD", Total: contracts.MustParseDecimal("5"),
			Available: contracts.MustParseDecimal("5")},
	}
	reader.fills = []venue.VenueFill{{VenueFillRef: "v-1",
		Quantity: contracts.MustParseDecimal("1"), Price: contracts.MustParseDecimal("2")}}

	run, err := newReconciler(t, db, gate, reader).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := checkByName(t, run, "balances").Status; got != reconcile.Completed {
		t.Errorf("balances: status = %s, want COMPLETED. BALANCE_RETRIEVAL is certified", got)
	}
	for _, name := range []string{"orders", "fills", "positions"} {
		if got := checkByName(t, run, name).Status; got != reconcile.Skipped {
			t.Errorf("%s: status = %s, want SKIPPED; only BALANCE_RETRIEVAL is certified",
				name, got)
		}
	}
	// The one completed check ran and found a real difference: the venue holds a
	// balance for which this platform has no portfolio.balance row at all.
	if run.BalancesChecked != 1 {
		t.Errorf("BalancesChecked = %d, want 1", run.BalancesChecked)
	}
	if run.DifferencesFound != 1 {
		t.Errorf("DifferencesFound = %d, want 1", run.DifferencesFound)
	}
	if reader.asked["fills"] != 0 || reader.asked["positions"] != 0 || reader.asked["orders"] != 0 {
		t.Errorf("the venue was asked for an uncertified capability: %v", reader.asked)
	}
}

// SEVERITY, the inversion.
//
// A platform that believes it holds 1,000,000 and a venue that says zero is a
// complete disagreement about whether any exposure exists. Grading that by the size
// of the VENUE's figure -- which is what an earlier draft did -- calls it the
// mildest possible finding, which is exactly backwards.
func TestAVenueReportingZeroAgainstAPositiveLocalBalanceIsMaterial(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1130)

	caps := []venue.Capability{venue.CapabilityBalanceRetrieval}
	gate := reconcileGate(t, db, adapter, caps...)

	reader := newFakeReader()
	reader.balances = []venue.VenueBalance{
		{Currency: "USD", Total: contracts.MustParseDecimal("0"),
			Available: contracts.MustParseDecimal("0")},
	}

	r := newReconciler(t, db, gate, reader)
	seedBalance(t, ctx, db, r.Account(), "USD", "1000000", "1000000")

	run, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	bal := checkByName(t, run, "balances")
	if len(bal.Findings) == 0 {
		t.Fatal("no finding at all for a total disagreement about a million units")
	}
	for _, f := range bal.Findings {
		if f.Severity != reconcile.SevMaterial {
			t.Errorf("%s: severity = %s, want MATERIAL. The venue reports zero against a "+
				"positive local balance; grading this by the venue's zero inverts the severity",
				f.InternalReference, f.Severity)
		}
	}
	if run.MaterialBreaks != len(bal.Findings) {
		t.Errorf("MaterialBreaks = %d, want %d", run.MaterialBreaks, len(bal.Findings))
	}
}

// One currency, two fields, two grades -- and the case must record the WORSE of them.
//
// severityOrder exists because Severity is a string type and the alphabet does not
// agree with the risk order: "MEDIUM" sorts after "MATERIAL". Aggregating the per-field
// grades with `>` therefore walked INFO -> MATERIAL -> MEDIUM and wrote the case as
// MEDIUM, and because only MATERIAL blocks risk-increasing activity, a complete
// disagreement about a balance silently stopped blocking new orders.
//
// This is the ordering that the alphabetical comparison gets wrong, stated as a test.
func TestABalanceCaseRecordsItsWorstFieldNotItsLastOne(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1250)

	caps := []venue.Capability{venue.CapabilityBalanceRetrieval}
	gate := reconcileGate(t, db, adapter, caps...)

	reader := newFakeReader()
	reader.balances = []venue.VenueBalance{
		// total disagrees completely (MATERIAL); available disagrees by 20% (MEDIUM).
		// total is graded first, so a string comparison ends on the MEDIUM.
		{Currency: "USD", Total: contracts.MustParseDecimal("0"),
			Available: contracts.MustParseDecimal("800")},
	}

	r := newReconciler(t, db, gate, reader)
	seedBalance(t, ctx, db, r.Account(), "USD", "1000", "1000")

	run, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	bal := checkByName(t, run, "balances")
	if len(bal.Findings) != 1 {
		t.Fatalf("findings = %d, want 1 (one currency, one case)", len(bal.Findings))
	}
	f := bal.Findings[0]
	if f.Severity != reconcile.SevMaterial {
		t.Errorf("severity = %s, want MATERIAL. The total field disagrees completely, so the "+
			"case must be graded on the worst field rather than the last one compared; "+
			"only MATERIAL blocks risk-increasing activity",
			f.Severity)
	}
	if run.MaterialBreaks != 1 {
		t.Errorf("MaterialBreaks = %d, want 1. A balance break the venue reports as zero "+
			"against a positive local holding must block risk-increasing activity",
			run.MaterialBreaks)
	}
}

// The other direction: the venue holds exposure the platform has no record of. That
// is untracked exposure, which is the worst outcome because nothing in this
// platform would ever constrain it.
func TestExposureTheVenueHoldsAndThePlatformDoesNotIsMaterial(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1140)

	caps := []venue.Capability{venue.CapabilityBalanceRetrieval}
	gate := reconcileGate(t, db, adapter, caps...)

	// No local balance row is seeded: the platform holds no record of this account's
	// balance at all, which is a different defect from holding a wrong one.
	reader := newFakeReader()
	reader.balances = []venue.VenueBalance{
		{Currency: "GBP", Total: contracts.MustParseDecimal("42"),
			Available: contracts.MustParseDecimal("42")},
	}

	run, err := newReconciler(t, db, gate, reader).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	bal := checkByName(t, run, "balances")
	if len(bal.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(bal.Findings))
	}
	if got := bal.Findings[0].Severity; got != reconcile.SevMaterial {
		t.Errorf("severity = %s, want MATERIAL for exposure the platform has no record of", got)
	}
	if got := bal.Findings[0].Kind; got != reconcile.KindBalance {
		t.Errorf("kind = %s, want BALANCE", got)
	}
}

// A small relative disagreement on a large balance is a MEDIUM, not a MATERIAL. The
// grading has to be capable of saying something milder or it cannot be trusted when
// it says something severe.
func TestASmallRelativeDisagreementIsNotEscalatedToMaterial(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1150)

	caps := []venue.Capability{venue.CapabilityBalanceRetrieval}
	gate := reconcileGate(t, db, adapter, caps...)

	reader := newFakeReader()
	// One unit out of a million: a 0.0001 fraction of the local figure.
	reader.balances = []venue.VenueBalance{
		{Currency: "USD", Total: contracts.MustParseDecimal("1000000"),
			Available: contracts.MustParseDecimal("999999")},
	}

	r := newReconciler(t, db, gate, reader)
	seedBalance(t, ctx, db, r.Account(), "USD", "1000000", "1000000")

	run, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	bal := checkByName(t, run, "balances")
	if len(bal.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(bal.Findings))
	}
	if got := bal.Findings[0]; got.Severity == reconcile.SevMaterial {
		t.Errorf("a one-unit disagreement on a million was graded MATERIAL, which makes "+
			"MATERIAL meaningless: %s", got.Severity)
	}
}

// One difference is one case. Two passes over the same disagreement must not
// double the operator's work list, and must not pile up in case_open_material_idx,
// which exists to find the blocking break.
func TestARepeatedDifferenceReusesItsCaseRatherThanOpeningAnother(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1160)

	caps := []venue.Capability{venue.CapabilityBalanceRetrieval}
	gate := reconcileGate(t, db, adapter, caps...)

	reader := newFakeReader()
	reader.balances = []venue.VenueBalance{
		{Currency: "USD", Total: contracts.MustParseDecimal("0"),
			Available: contracts.MustParseDecimal("0")},
	}
	r := newReconciler(t, db, gate, reader)
	seedBalance(t, ctx, db, r.Account(), "USD", "1000", "1000")

	first, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	second, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if len(first.Cases) == 0 {
		bal := checkByName(t, first, "balances")
		var rows int
		_ = db.QueryRowContext(ctx,
			`SELECT count(*) FROM portfolio.balance WHERE account_id = $1`,
			r.Account()).Scan(&rows)
		t.Fatalf("the first pass opened no case for a MATERIAL difference; case errors: %v\n"+
			"balances check: status=%s checked=%d findings=%d reason=%q\n"+
			"balance rows for %s: %d", first.CaseErrors,
			bal.Status, bal.Checked, len(bal.Findings), bal.Reason, r.Account(), rows)
	}
	if len(first.Cases) != len(second.Cases) {
		t.Errorf("first pass opened %d cases and the second opened %d; a repeated "+
			"difference must reuse its case", len(first.Cases), len(second.Cases))
	}
	if first.Cases[0] != second.Cases[0] {
		t.Errorf("the second pass opened case %s where the first opened %s; the same "+
			"difference must attach to the same case", second.Cases[0], first.Cases[0])
	}
	if n := count(t, db,
		`SELECT count(*) FROM reconciliation.case
		  WHERE venue_id = $1 AND difference_kind = 'BALANCE'
		    AND status IN ('OPEN','INVESTIGATING','REOPENED')`, reconcileVenue); n != 1 {
		t.Errorf("open BALANCE cases = %d after two passes over one difference, want 1", n)
	}
	// Two passes, two check runs: the run history is a real record even though the
	// case is not duplicated.
	if n := count(t, db,
		`SELECT count(*) FROM reconciliation.check_run WHERE venue_id = $1`,
		reconcileVenue); n < 2 {
		t.Errorf("check runs = %d, want at least 2; each pass opens its own run", n)
	}
}

// Escalation happens IN PLACE: the same case, a higher severity, and a counter that
// shows it happened.
func TestASeverityIncreaseEscalatesTheExistingCaseInPlace(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1170)

	caps := []venue.Capability{venue.CapabilityBalanceRetrieval}
	gate := reconcileGate(t, db, adapter, caps...)

	reader := newFakeReader()
	// First: a 0.0001 disagreement, graded MEDIUM.
	reader.balances = []venue.VenueBalance{
		{Currency: "USD", Total: contracts.MustParseDecimal("1000000"),
			Available: contracts.MustParseDecimal("999999")},
	}
	r := newReconciler(t, db, gate, reader)
	seedBalance(t, ctx, db, r.Account(), "USD", "1000000", "1000000")

	first, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(first.Cases) == 0 {
		t.Fatalf("the first pass opened no case to escalate; case errors: %v", first.CaseErrors)
	}
	var firstSeverity string
	if err := db.QueryRowContext(ctx,
		`SELECT severity::text FROM reconciliation.case WHERE case_id = $1`,
		first.Cases[0].String()).Scan(&firstSeverity); err != nil {
		t.Fatalf("reading the first case: %v", err)
	}
	if firstSeverity == string(reconcile.SevMaterial) {
		t.Fatalf("the first pass already graded MATERIAL, so there is nothing to escalate " +
			"from; the fixture does not exercise escalation")
	}

	// Second: the venue now disagrees completely.
	reader.balances = []venue.VenueBalance{
		{Currency: "USD", Total: contracts.MustParseDecimal("0"),
			Available: contracts.MustParseDecimal("0")},
	}
	second, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if second.Cases[0] != first.Cases[0] {
		t.Fatalf("the escalation opened a new case %s rather than escalating %s",
			second.Cases[0], first.Cases[0])
	}
	var (
		severity string
		level    int
	)
	if err := db.QueryRowContext(ctx,
		`SELECT severity::text, escalation_level FROM reconciliation.case WHERE case_id = $1`,
		first.Cases[0].String()).Scan(&severity, &level); err != nil {
		t.Fatalf("reading the escalated case: %v", err)
	}
	if severity != string(reconcile.SevMaterial) {
		t.Errorf("case severity = %s, want MATERIAL after a complete disagreement", severity)
	}
	if level != 1 {
		t.Errorf("escalation_level = %d, want 1", level)
	}
	if n := count(t, db,
		`SELECT count(*) FROM reconciliation.case WHERE venue_id = $1
		   AND status IN ('OPEN','INVESTIGATING','REOPENED')`, reconcileVenue); n != 1 {
		t.Errorf("open cases = %d, want 1; an escalation must not fork the case", n)
	}
}

// Only MATERIAL blocks. reconciliation.case's own comment and case_open_material_idx
// both say so, and a HIGH case that blocked anyway would contradict the schema.
func TestOnlyAMaterialCaseBlocksRiskIncreasingScope(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1180)

	caps := []venue.Capability{venue.CapabilityBalanceRetrieval}
	gate := reconcileGate(t, db, adapter, caps...)

	reader := newFakeReader()
	reader.balances = []venue.VenueBalance{
		{Currency: "USD", Total: contracts.MustParseDecimal("0"),
			Available: contracts.MustParseDecimal("0")},
	}
	r := newReconciler(t, db, gate, reader)
	seedBalance(t, ctx, db, r.Account(), "USD", "1000000", "1000000")
	run, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(run.Cases) == 0 {
		t.Fatal("no case was opened")
	}

	var blocked bool
	if err := db.QueryRowContext(ctx,
		`SELECT (blocked_scope->>'blocks_risk_increasing')::boolean
		   FROM reconciliation.case WHERE case_id = $1`,
		run.Cases[0].String()).Scan(&blocked); err != nil {
		t.Fatalf("reading the blocked scope: %v", err)
	}
	if !blocked {
		t.Error("a MATERIAL case does not record that it blocks risk-increasing scope")
	}
	// The schema's own index is the final authority on which severities block.
	if n := count(t, db,
		`SELECT count(*) FROM reconciliation.case
		  WHERE case_id = $1 AND severity = 'MATERIAL'
		    AND status IN ('OPEN','INVESTIGATING','REOPENED')`, run.Cases[0].String()); n != 1 {
		t.Errorf("the case does not appear in the MATERIAL open index, which is what a "+
			"risk gate consults to decide whether to block: matched=%d", n)
	}
}

// A case is a committed state mutation, so doc 05 requires an outbox row and doc 22
// requires an audit record. Both, or the transaction did not happen.
func TestACaseCommitsItsAuditRecordAndOutboxRowTogether(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1190)

	caps := []venue.Capability{venue.CapabilityFillRetrieval}
	gate := reconcileGate(t, db, adapter, caps...)

	reader := newFakeReader()
	// A venue execution this platform has no oms.fill row for.
	reader.fills = []venue.VenueFill{{
		VenueFillRef: "vfill-missing-1",
		Quantity:     contracts.MustParseDecimal("2"),
		Price:        contracts.MustParseDecimal("50"),
		ExecutedAt:   gateNow,
	}}

	run, err := newReconciler(t, db, gate, reader).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(run.Cases) != 1 {
		t.Fatalf("cases = %d, want 1", len(run.Cases))
	}
	caseID := run.Cases[0].String()

	if n := count(t, db,
		`SELECT count(*) FROM reconciliation.case WHERE case_id = $1
		   AND audit_id IS NOT NULL`, caseID); n != 1 {
		t.Errorf("the case has no audit_id bound to it, or does not exist: matched=%d", n)
	}
	if n := count(t, db,
		`SELECT count(*) FROM audit.record WHERE target_type = 'RECONCILIATION_CASE'
		   AND target_id = $1`, caseID); n != 1 {
		t.Errorf("audit records for the case = %d, want exactly 1", n)
	}
	if n := count(t, db,
		`SELECT count(*) FROM ops.outbox WHERE aggregate_type = 'RECONCILIATION_CASE'
		   AND aggregate_id = $1 AND event_type = 'RECONCILIATION_CASE_OPENED'`, caseID); n != 1 {
		t.Errorf("outbox rows for the case = %d, want exactly 1. Doc 05 requires one for "+
			"every committed state mutation and no trigger enforces it", n)
	}
	// The case is bound to the run that found it, so the counts and the case are one
	// evidence trail.
	if n := count(t, db,
		`SELECT count(*) FROM reconciliation.case WHERE case_id = $1 AND correlation_id = $2`,
		caseID, run.CheckRunID.String()); n != 1 {
		t.Errorf("the case is not correlated to the check run that found it: matched=%d", n)
	}
}

// A venue state the adapter declares no mapping for is a MATERIAL finding, not a
// guess. Every available default would be a lie about an order's fate.
func TestAnUnmappableVenueOrderStateIsReportedNotGuessed(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1200)

	orderID, prepared := answeredOrder(t, ctx, db, contracts.EnvPaper)
	clientOrderID := "cid-" + orderID[4:]

	caps := []venue.Capability{venue.CapabilityOrderLookup}
	gate := reconcileGate(t, db, adapter, caps...)

	reader := newFakeReader()
	reader.orders[clientOrderID] = venue.VenueOrder{
		ClientOrderID: clientOrderID,
		VenueOrderRef: "vord-1",
		// The declared map holds only "new". This is a state the adapter has never
		// seen, which is precisely the case that must not be guessed.
		State:      "SOME_STATE_ADDED_LAST_MONTH",
		ObservedAt: gateNow,
	}

	run, err := newReconciler(t, db, gate, reader).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	orders := checkByName(t, run, "orders")
	if orders.Status != reconcile.Completed {
		t.Fatalf("orders: status = %s, want COMPLETED; the venue did answer", orders.Status)
	}
	if orders.Checked != 1 {
		t.Errorf("orders: Checked = %d, want 1", orders.Checked)
	}
	if len(orders.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(orders.Findings))
	}
	f := orders.Findings[0]
	if f.InternalReference != orderID {
		t.Errorf("internal reference = %s, want the order %s", f.InternalReference, orderID)
	}
	if f.Severity != reconcile.SevMaterial {
		t.Errorf("severity = %s, want MATERIAL. The platform cannot say what happened to "+
			"this order, and that is not a small difference", f.Severity)
	}
	ev, _ := f.Evidence["mapping_error"].(string)
	if ev == "" {
		t.Error("the finding records no mapping error, so an operator cannot tell an " +
			"unmappable state from a disagreement")
	}

	// The order is untouched. Reconciliation reports; it does not move an order.
	if got := orderState(t, db, orderID); got != "SUBMITTING" {
		t.Errorf("order state = %s; reconciliation must not change an order's state, "+
			"only report what the venue says", got)
	}
	if got := submissionOutcome(t, db, prepared.SubmissionID); got != "PENDING" {
		t.Errorf("submission outcome = %s, want PENDING; reconciliation must not settle a "+
			"submission, only report it", got)
	}
}

// The order probe is the orders whose outcome is not established. Asking about
// settled orders would make the venue's load proportional to all history.
func TestTheOrderProbeCoversOnlyOrdersWhoseOutcomeIsUnestablished(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1210)

	caps := []venue.Capability{venue.CapabilityOrderLookup}
	gate := reconcileGate(t, db, adapter, caps...)

	// One SUBMITTING order (prepared, PENDING) and one settled one.
	submittingID, _ := answeredOrder(t, ctx, db, contracts.EnvPaper)
	settledID, settled := answeredOrder(t, ctx, db, contracts.EnvPaper)
	if _, err := newCommand(t, db).ApplyVenueAnswer(ctx,
		answer(t, settledID, settled, execution.AcceptedOutcome)); err != nil {
		t.Fatalf("settling the second order: %v", err)
	}
	if got := orderState(t, db, settledID); got != "ACKNOWLEDGED" {
		t.Fatalf("the second order is %s, want ACKNOWLEDGED before the probe", got)
	}

	reader := newFakeReader()
	run, err := newReconciler(t, db, gate, reader).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	orders := checkByName(t, run, "orders")
	if orders.Checked != 1 {
		t.Errorf("orders: Checked = %d, want 1. Only the order still in SUBMITTING needs "+
			"reconciling; the settled one is not in flight", orders.Checked)
	}
	if reader.asked["orders"] != 1 {
		t.Errorf("the venue was asked about %d orders, want 1", reader.asked["orders"])
	}
	_ = submittingID
}

// The probe set is bounded, and hitting the bound is reported rather than passed
// off as a complete pass.
//
// checkOrders issues one venue round trip per order whose outcome is not
// established, so the cost of a pass scales with the backlog. A venue outage
// leaves orders in SUBMITTING, which means the backlog grows exactly when the
// venue is least able to answer -- so an unbounded loop spends the pass and the
// venue's rate limit catching up. Worse, a pass that examined a prefix and
// reported COMPLETED would be indistinguishable from a pass that examined
// everything, which is the same defect shape as a partial read reported as
// complete.
func TestAnUnboundedOrderBacklogIsBoundedAndReportedAsIncomplete(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1260)

	caps := []venue.Capability{venue.CapabilityOrderLookup}
	gate := reconcileGate(t, db, adapter, caps...)

	// A bound small enough to overflow cheaply. The bound is an option precisely
	// so this test does not have to seed 500 real orders to reach it.
	const bound = 3
	const overflow = 2
	total := bound + overflow
	for i := 0; i < total; i++ {
		answeredOrder(t, ctx, db, contracts.EnvPaper)
	}

	reader := newFakeReader()
	run, err := newReconciler(t, db, gate, reader,
		reconcile.WithMaxOrderProbes(bound)).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	orders := checkByName(t, run, "orders")

	if reader.asked["orders"] != bound {
		t.Errorf("the venue was asked about %d orders, want exactly the bound of %d; %d "+
			"orders were awaiting reconciliation, so the probe set must be bounded",
			reader.asked["orders"], bound, total)
	}
	if orders.Status == reconcile.Completed {
		t.Errorf("orders: status = COMPLETED, want %s. The probe set overflowed, so this pass "+
			"did not examine every order; reporting COMPLETED would present a truncated "+
			"check as a complete one", orders.Status)
	}
	if orders.Reason == "" {
		t.Error("orders: a truncated pass must say so in its reason; an operator cannot see " +
			"that orders were left unreconciled otherwise")
	}
}

// A fill the venue reports twice is invisible locally -- fill_venue_trade_unique
// stores it once -- so the delivery is the only place the duplicate can be seen.
func TestADuplicateVenueFillDeliveryIsFound(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1220)

	caps := []venue.Capability{venue.CapabilityFillRetrieval}
	gate := reconcileGate(t, db, adapter, caps...)

	fill := venue.VenueFill{
		VenueFillRef: "vfill-dup-1",
		Quantity:     contracts.MustParseDecimal("1"),
		Price:        contracts.MustParseDecimal("100"),
		ExecutedAt:   gateNow,
	}
	reader := newFakeReader()
	reader.fills = []venue.VenueFill{fill, fill}

	run, err := newReconciler(t, db, gate, reader).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	fills := checkByName(t, run, "fills")
	if fills.Checked != 1 {
		t.Errorf("fills: Checked = %d, want 1 (one distinct venue_trade_id)", fills.Checked)
	}
	if len(fills.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(fills.Findings))
	}
	if got := fills.Findings[0].Kind; got != reconcile.KindFillDuplicate {
		t.Errorf("kind = %s, want FILL_DUPLICATE", got)
	}
	if n, _ := fills.Findings[0].Evidence["deliveries"].(int); n != 2 {
		t.Errorf("evidence deliveries = %v, want 2", fills.Findings[0].Evidence["deliveries"])
	}
}

// The constructor refuses rather than defaulting, because every default here would
// silently produce a runner that cannot do its job.
func TestTheRunnerRefusesToBeBuiltWithoutTheThingsItNeeds(t *testing.T) {
	db := dbtest.Open(t)
	adapter := adapterID(t, 1230)
	gate := reconcileGate(t, db, adapter)
	reader := newFakeReader()
	appender := audit.NewAppender(db)

	// The last three are option omissions rather than argument omissions, so each
	// supplies every OTHER required option. Otherwise all three would fail on the
	// first missing one and assert nothing about the option actually under test.
	accOpt := reconcile.WithAccount("acc-" + adapter)
	ownOpt := reconcile.WithOwner("ops-1")
	polOpt := reconcile.WithPolicyVersion("pol-1")
	keyOpt := reconcile.WithSigningKey(TestSigningKeyID)
	// Every case below omits exactly one thing, so each must supply all the
	// others. Otherwise they would all fail on the first missing one and assert
	// nothing about the thing actually under test.
	base := []reconcile.Option{accOpt, ownOpt, polOpt, keyOpt}

	cases := []struct {
		name               string
		db                 *sql.DB
		gate               *venue.Gate
		reader             venue.Reader
		appender           *audit.Appender
		adapterID, venueID string
		opts               []reconcile.Option
		wantSubstring      string
	}{
		{"no database", nil, gate, reader, appender, adapter, reconcileVenue, base, "database handle"},
		{"no gate", db, nil, reader, appender, adapter, reconcileVenue, base, "certification gate"},
		{"no reader", db, gate, nil, appender, adapter, reconcileVenue, base, "venue reader"},
		{"no appender", db, gate, reader, nil, adapter, reconcileVenue, base, "audit appender"},
		{"no adapter", db, gate, reader, appender, "", reconcileVenue, base, "adapter id"},
		{"no venue", db, gate, reader, appender, adapter, "", base, "venue id"},
		{"no account", db, gate, reader, appender, adapter, reconcileVenue,
			[]reconcile.Option{ownOpt, polOpt, keyOpt}, "account"},
		{"no owner", db, gate, reader, appender, adapter, reconcileVenue,
			[]reconcile.Option{accOpt, polOpt, keyOpt}, "owner"},
		{"no policy version", db, gate, reader, appender, adapter, reconcileVenue,
			[]reconcile.Option{accOpt, ownOpt, keyOpt}, "policy version"},
		{"no signing key", db, gate, reader, appender, adapter, reconcileVenue,
			[]reconcile.Option{accOpt, ownOpt, polOpt}, "signing key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := reconcile.NewRunner(c.db, c.gate, c.reader, c.appender,
				c.adapterID, c.venueID, c.opts...)
			if err == nil {
				t.Fatalf("NewRunner accepted a runner with %s", c.name)
			}
			if !strings.Contains(err.Error(), c.wantSubstring) {
				t.Errorf("error %q does not mention %q", err, c.wantSubstring)
			}
		})
	}
}

// The runner reports the gate's environment rather than taking one, because the gate
// is bound to one environment and its certifications were loaded for exactly that one.
func TestTheRunnerTakesItsEnvironmentFromTheGate(t *testing.T) {
	db := dbtest.Open(t)
	adapter := adapterID(t, 1240)
	gate := reconcileGate(t, db, adapter)
	r, err := reconcile.NewRunner(db, gate, newFakeReader(), audit.NewAppender(db),
		adapter, reconcileVenue,
		reconcile.WithAccount("acc-"+adapter),
		reconcile.WithOwner("ops-1"), reconcile.WithPolicyVersion("pol-1"),
		reconcile.WithSigningKey(TestSigningKeyID))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if got := r.Environment(); got != contracts.EnvPaper {
		t.Errorf("Environment = %s, want paper (the gate's)", got)
	}
	if got := r.Account(); got != "acc-"+adapter {
		t.Errorf("Account = %q, want the account it was given", got)
	}
}

// seedBalance writes a portfolio.balance row.
//
// balance_components_consistent requires available + reserved <= total, so a fixture
// that writes total and available with reserved defaulted to zero is consistent by
// construction.
func seedBalance(t *testing.T, ctx context.Context, db *sql.DB, account, currency, total, available string) {
	t.Helper()
	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		dbtest.MustExec(t, ctx, tx, `
			INSERT INTO portfolio.balance (
				balance_id, account_id, environment, currency, total, available, reserved)
			VALUES ($1, $2, 'paper', $3, $4, $5, 0)`,
			dbtest.CanonicalID("bal", int(seq())), account, currency,
			contracts.MustParseDecimal(total), contracts.MustParseDecimal(available))
	})
}

// A case is the loudest thing reconciliation produces: a claim that the platform's
// books and the venue's disagree about money, with a severity, a blocked scope and
// an owner. It is also the record most likely to be read aloud later and asked who
// stands behind it.
//
// The answer used to be nobody. audit.record.signing_key_id is NOT NULL, so the
// empty string this path supplied satisfied the schema and named no key, and
// nothing ever revised it -- there is no UPDATE against that column anywhere in the
// migrations, so the value written at append time is the value the record keeps for
// the life of the partition. The comment on the column described a batch-closing key
// written at checkpoint time, which is why the placeholder looked reasonable; that
// back-fill does not exist. Migration 0023 corrects the comment and refuses the
// write, and this test holds the Go side to the same contract.
func TestTheCaseAuditRecordNamesTheConfiguredSigningKey(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 1191)

	caps := []venue.Capability{venue.CapabilityFillRetrieval}
	gate := reconcileGate(t, db, adapter, caps...)

	reader := newFakeReader()
	reader.fills = []venue.VenueFill{{
		VenueFillRef: "vfill-unattested-1",
		Quantity:     contracts.MustParseDecimal("2"),
		Price:        contracts.MustParseDecimal("50"),
		ExecutedAt:   gateNow,
	}}

	run, err := newReconciler(t, db, gate, reader).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(run.Cases) != 1 {
		t.Fatalf("cases = %d, want 1", len(run.Cases))
	}
	caseID := run.Cases[0].String()

	var key string
	err = db.QueryRowContext(ctx, `
		SELECT a.signing_key_id
		  FROM audit.record a
		  JOIN reconciliation.case c ON c.audit_id = a.audit_id
		 WHERE c.case_id = $1`, caseID).Scan(&key)
	if err != nil {
		t.Fatalf("reading the case's audit record: %v", err)
	}
	if key != TestSigningKeyID {
		t.Errorf("the audit record for case %s names signing key %q, want %q. A case asserting "+
			"the platform and the venue disagree, attributed to no key, is the one record "+
			"that cannot be defended later", caseID, key, TestSigningKeyID)
	}
}

// Migration 0023 is only worth anything if the database refuses the write, rather
// than the Go constructors happening to check. This is that proof, and it is why the
// constraint is in the schema and not only in three constructors: a fourth producer
// added later would not have to remember.
func TestTheDatabaseRefusesAnAuditRecordThatNamesNoKey(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	appender := audit.NewAppender(db)

	rec := serviceRecord(dbtest.CanonicalID("aud", int(seq())), "order.unattributed", gateNow)
	rec.SigningKeyID = ""
	_, err := appender.Append(ctx, rec)
	if err == nil {
		t.Fatal("an audit record naming no signing key was accepted. audit.record.signing_key_id is " +
			"NOT NULL but was never required to be non-empty, so this wrote successfully for the " +
			"whole life of the OMS path. See migration 0023")
	}
	if !strings.Contains(err.Error(), "audit_signing_key_required") {
		t.Logf("append was refused with: %v", err)
	}

	// The same record with a key is accepted, so the refusal above is the constraint
	// and not some unrelated failure of this fixture.
	rec.AuditID = dbtest.CanonicalID("aud", int(seq()))
	rec.SigningKeyID = TestSigningKeyID
	if _, err := appender.Append(ctx, rec); err != nil {
		t.Fatalf("the same record naming key %q was refused: %v", TestSigningKeyID, err)
	}
}
