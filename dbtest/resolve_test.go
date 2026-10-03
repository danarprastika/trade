package dbtest_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/execution"
	"github.com/aitc/trade/domain/oms"
)

// The durable exit from UNKNOWN.
//
// Doc 16 §4: "A timeout after a possible submission yields OMS UNKNOWN; no
// exposure-increasing retry occurs until the venue state is resolved or an
// authorized recovery procedure explicitly determines the safe action."
//
// Resolution is the only thing that makes such an order whole again, and it was
// the last behaviour that existed solely in the retired Submitter -- where it wrote
// a transition through an interface and kept the attempt history in a caller-supplied
// slice. These tests assert it against the live tables.

// reconcilerID is who resolved the order under test.
//
// It is a constant rather than a per-test id so that a test which forgot to supply
// an actor would fail on the actor check instead of passing quietly.
const reconcilerID = "svc-reconciliation-test"

// fakeResolver reports what the venue believes. It records the client order id it
// was handed, so a test can prove the resolver was asked about the right attempt.
type fakeResolver struct {
	outcome      execution.Outcome
	venueOrderID string
	err          error

	calls         int
	gotOrderID    string
	gotClientOurs string
}

func (f *fakeResolver) Resolve(_ context.Context, orderID, clientOrderID string) (execution.Outcome, string, error) {
	f.calls++
	f.gotOrderID = orderID
	f.gotClientOurs = clientOrderID
	return f.outcome, f.venueOrderID, f.err
}

// unknownOrder returns an order that is genuinely UNKNOWN: a submit was prepared,
// and its submission recorded a timeout. Both transitions are journalled, because
// the database refuses to reach either state any other way.
func unknownOrder(t *testing.T, ctx context.Context, db *sql.DB) (orderID, corr string) {
	t.Helper()
	orderID, corr = readyOrder(t, ctx, db, contracts.EnvPaper)
	// An order that cleared the risk gate has the policy revision that admitted it
	// recorded against it. Doc 22 requires policy_version on the audit record for a
	// resolution, and the only honest source for it is the order's own history.
	if _, err := db.ExecContext(ctx,
		`UPDATE oms."order" SET risk_policy_revisions = ARRAY[$2] WHERE order_id=$1`,
		orderID, dbtest.CanonicalID("pol", int(seq()))); err != nil {
		t.Fatalf("recording the risk policy revision: %v", err)
	}
	c := newCommand(t, db)

	prepared, err := c.Prepare(ctx, submitIntent(t, orderID, corr, contracts.EnvPaper))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	now := time.Now().UTC()
	if err := c.RecordOutcome(ctx, prepared.SubmissionID, execution.UndeterminedOutcome,
		"", "", now, now.UnixNano()); err != nil {
		t.Fatalf("recording the timeout: %v", err)
	}

	// SUBMITTING -> UNKNOWN, journalled then applied.
	var next int64
	if err := db.QueryRowContext(ctx,
		`SELECT coalesce(max(sequence),0)+1 FROM oms.order_event WHERE order_id=$1`, orderID).
		Scan(&next); err != nil {
		t.Fatalf("reading the next sequence: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO oms.order_event (
			order_event_id, order_id, sequence, from_state, to_state, event_kind,
			filled_quantity_delta, price, actor_id, reason, correlation_id,
			occurred_at, occurred_at_ns)
		VALUES ($1,$2,$3,'SUBMITTING','UNKNOWN','UNKNOWN_OUTCOME',
		        0,NULL,'dispatcher',$4,$5,now(),$6)`,
		dbtest.CanonicalID("oe1", int(seq())), orderID, next, "venue did not answer", corr, next); err != nil {
		t.Fatalf("journalling UNKNOWN: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE oms."order" SET state='UNKNOWN', version_vector=version_vector+1
		  WHERE order_id=$1`, orderID); err != nil {
		t.Fatalf("advancing to UNKNOWN: %v", err)
	}
	return orderID, corr
}

// The resolver is asked about the right attempt, the order advances, and the
// submission row is rewritten in place rather than appended. That last part is the
// one worth stating: a second row would present one send as two attempts, which is
// exactly the counting the no-retry rule depends on.
func TestResolvingAnUnknownOrderRewritesItsSubmissionInPlace(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := unknownOrder(t, ctx, db)

	r := &fakeResolver{outcome: execution.AcceptedOutcome, venueOrderID: "venue-ref-4417"}
	got, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID, r)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if r.calls != 1 {
		t.Fatalf("resolver called %d times, want exactly 1", r.calls)
	}
	if r.gotOrderID != orderID {
		t.Fatalf("resolver was asked about %s, want %s", r.gotOrderID, orderID)
	}
	if r.gotClientOurs == "" {
		t.Fatal("the resolver was given no client order id; the undetermined attempt's token is " +
			"the only thing that can identify the order at the venue")
	}
	if got.State != oms.StateAcknowledged {
		t.Fatalf("State = %s, want ACKNOWLEDGED", got.State)
	}
	if got.VenueOrderID != "venue-ref-4417" {
		t.Fatalf("VenueOrderID = %s, want venue-ref-4417", got.VenueOrderID)
	}

	var state string
	dbtest.MustQueryRowDB(t, ctx, db, &state,
		`SELECT state::text FROM oms."order" WHERE order_id=$1`, orderID)
	if state != "ACKNOWLEDGED" {
		t.Fatalf("order state = %s, want ACKNOWLEDGED", state)
	}

	// One row, not two, and its outcome is the resolved one.
	var n int
	dbtest.MustQueryRowDB(t, ctx, db, &n,
		`SELECT count(*) FROM execution.submission WHERE order_id=$1`, orderID)
	if n != 1 {
		t.Fatalf("submission rows = %d, want 1; resolution rewrites the attempt rather than "+
			"appending a second one, because two rows would read as two sends", n)
	}
	var outcome string
	var vref sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT outcome, venue_order_ref FROM execution.submission
		 WHERE submission_id=$1`, got.SubmissionID.String()).Scan(&outcome, &vref); err != nil {
		t.Fatalf("reading the resolved submission: %v", err)
	}
	if outcome != string(execution.StoredAck) {
		t.Fatalf("resolved outcome = %s, want ACKNOWLEDGED", outcome)
	}
	if !vref.Valid || vref.String != "venue-ref-4417" {
		t.Fatalf("venue_order_ref = %v, want venue-ref-4417", vref)
	}

	// The transition is journalled, so the state change has evidence.
	var via string
	dbtest.MustQueryRowDB(t, ctx, db, &via, `
		SELECT event_kind::text FROM oms.order_event
		 WHERE order_id=$1 AND to_state='ACKNOWLEDGED' AND from_state='UNKNOWN'`, orderID)
	if via != "OUTCOME_RESOLVED" {
		t.Fatalf("resolution event kind = %s, want OUTCOME_RESOLVED", via)
	}
}

// A venue refusal resolves to REJECTED, and the attempt stops blocking.
func TestResolvingToARejectionUnblocksTheOrder(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := unknownOrder(t, ctx, db)

	got, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID, &fakeResolver{outcome: execution.RejectedOutcome})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.State != oms.StateRejected {
		t.Fatalf("State = %s, want REJECTED", got.State)
	}

	// No longer undetermined, which is the whole point of resolving.
	st, err := execution.NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	u, err := st.UndeterminedDurable(ctx, orderID)
	if err != nil {
		t.Fatalf("UndeterminedDurable: %v", err)
	}
	if u != nil {
		t.Fatalf("a resolved submission still blocks as undetermined: %+v", u)
	}
}

// An order with nothing undetermined is refused. Resolving a known outcome would
// overwrite a determinate record with whatever the venue says now, and that is a
// second opinion rather than a reconciliation.
func TestResolvingAnOrderWithNoUndeterminedAttemptIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := readyOrder(t, ctx, db, contracts.EnvPaper)

	seedSubmission(t, ctx, db, orderID, "REJECTED", 1)

	r := &fakeResolver{outcome: execution.AcceptedOutcome}
	_, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID, r)
	if err == nil {
		t.Fatal("Resolve rewrote an outcome that was already determined")
	}
	if !errors.Is(err, execution.ErrNothingToResolve) {
		t.Fatalf("error = %v, want ErrNothingToResolve", err)
	}
	if r.calls != 0 {
		t.Fatalf("the venue was asked %d times; the precondition must be checked before the read", r.calls)
	}
}

// An answer the platform cannot interpret leaves the order in UNKNOWN. Adopting it
// would end the order's life at the moment the platform knows least.
func TestAnUnrecognisedResolutionLeavesTheOrderUnknown(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := unknownOrder(t, ctx, db)

	r := &fakeResolver{outcome: execution.Outcome("PARTIALLY_FILLED")}
	got, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID, r)
	if !errors.Is(err, execution.ErrOutcomeUndetermined) {
		t.Fatalf("error = %v, want ErrOutcomeUndetermined", err)
	}
	if got.State != oms.StateUnknown {
		t.Fatalf("State = %s, want UNKNOWN", got.State)
	}

	var state string
	dbtest.MustQueryRowDB(t, ctx, db, &state,
		`SELECT state::text FROM oms."order" WHERE order_id=$1`, orderID)
	if state != "UNKNOWN" {
		t.Fatalf("order state = %s, want UNKNOWN; an unrecognised answer must not resolve it", state)
	}
	var outcome string
	dbtest.MustQueryRowDB(t, ctx, db, &outcome, `
		SELECT outcome FROM execution.submission WHERE order_id=$1`, orderID)
	if outcome != string(execution.StoredUnknown) {
		t.Fatalf("submission outcome = %s, want TIMED_OUT_UNKNOWN; nothing was resolved", outcome)
	}
}

// A venue that cannot be asked leaves the order UNKNOWN, and the refusal is
// returned rather than swallowed. UNKNOWN is not a state to be escalated out of;
// it is a state to be resolved later, possibly by another process.
func TestAFailedReadLeavesTheOrderUnknown(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := unknownOrder(t, ctx, db)

	r := &fakeResolver{err: errors.New("dial tcp: i/o timeout")}
	got, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID, r)
	if !errors.Is(err, execution.ErrOutcomeUndetermined) {
		t.Fatalf("error = %v, want ErrOutcomeUndetermined", err)
	}
	if got.State != oms.StateUnknown {
		t.Fatalf("State = %s, want UNKNOWN", got.State)
	}

	var n int
	dbtest.MustQueryRowDB(t, ctx, db, &n,
		`SELECT count(*) FROM oms.order_event WHERE order_id=$1 AND event_kind='OUTCOME_RESOLVED'`, orderID)
	if n != 0 {
		t.Fatalf("OUTCOME_RESOLVED events = %d, want 0; a failed read must not journal a resolution", n)
	}
}

// Resolution is a read of venue state and must never become a second send. The
// resolver is the only thing consulted, and it has no submit capability: the type
// it satisfies declares Resolve and nothing else.
func TestResolvingCannotPlaceAnOrder(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := unknownOrder(t, ctx, db)

	before := map[string]int{}
	for _, table := range []string{"execution.submission", "oms.order_event"} {
		var n int
		dbtest.MustQueryRowDB(t, ctx, db, &n,
			`SELECT count(*) FROM `+table+` WHERE order_id=$1`, orderID)
		before[table] = n
	}

	if _, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID,
		&fakeResolver{outcome: execution.AcceptedOutcome, venueOrderID: "venue-ref-1"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// No new submission row. Resolution resolves; it does not resubmit.
	var n int
	dbtest.MustQueryRowDB(t, ctx, db, &n,
		`SELECT count(*) FROM execution.submission WHERE order_id=$1`, orderID)
	if n != before["execution.submission"] {
		t.Fatalf("submission rows went from %d to %d; resolution must never create a new send",
			before["execution.submission"], n)
	}
}

// Resolution is a committed state mutation, so doc 05 requires a durable outbox
// row and doc 22 requires an audit record. Both are asserted against the live
// tables, because neither is enforced by a trigger and an omission would be silent.
func TestResolvingLeavesAnAuditRecordAndAnOutboxRow(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := unknownOrder(t, ctx, db)

	before := outboxCount(t, ctx, db)

	if _, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID,
		&fakeResolver{outcome: execution.AcceptedOutcome, venueOrderID: "venue-ref-8812"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// The outbox row, tied to the resolution and not to the original submission.
	var n int
	dbtest.MustQueryRowDB(t, ctx, db, &n,
		`SELECT count(*) FROM ops.outbox WHERE aggregate_id=$1 AND event_type='ORDER_OUTCOME_RESOLVED'`,
		orderID)
	if n != 1 {
		t.Fatalf("ORDER_OUTCOME_RESOLVED outbox rows = %d, want 1; doc 05 requires one for every "+
			"committed state mutation and no trigger enforces it", n)
	}
	if after := outboxCount(t, ctx, db); after != before+1 {
		t.Fatalf("outbox rows went from %d to %d, want exactly one more", before, after)
	}

	var payloadVenue string
	dbtest.MustQueryRowDB(t, ctx, db, &payloadVenue, `
		SELECT payload->>'venue_order_ref' FROM ops.outbox
		 WHERE aggregate_id=$1 AND event_type='ORDER_OUTCOME_RESOLVED'`, orderID)
	if payloadVenue != "venue-ref-8812" {
		t.Fatalf("outbox payload venue_order_ref = %s, want venue-ref-8812", payloadVenue)
	}

	// The outbox row carries the order's correlation id, so the resolution lands in
	// the same trail as the submission it settles.
	var outboxCorr, orderCorr string
	dbtest.MustQueryRowDB(t, ctx, db, &outboxCorr, `
		SELECT correlation_id FROM ops.outbox
		 WHERE aggregate_id=$1 AND event_type='ORDER_OUTCOME_RESOLVED'`, orderID)
	dbtest.MustQueryRowDB(t, ctx, db, &orderCorr,
		`SELECT correlation_id FROM oms."order" WHERE order_id=$1`, orderID)
	if outboxCorr != orderCorr {
		t.Fatalf("outbox correlation id %s != order correlation id %s; a resolution must join the "+
			"same evidence trail as the submission it settles", outboxCorr, orderCorr)
	}

	// The audit record: real actor, real policy version, bound to the OMS event.
	var actor, policy string
	dbtest.MustQueryRowDB(t, ctx, db, &actor,
		`SELECT actor_id FROM audit.record WHERE action='ORDER_OUTCOME_RESOLVED' AND target_id=$1`, orderID)
	if actor != reconcilerID {
		t.Fatalf("audit actor_id = %s, want %s; a resolution with no accountable actor cannot be audited",
			actor, reconcilerID)
	}
	dbtest.MustQueryRowDB(t, ctx, db, &policy, `
		SELECT policy_version FROM audit.record WHERE action='ORDER_OUTCOME_RESOLVED' AND target_id=$1`, orderID)
	if policy == "" {
		t.Fatal("audit policy_version is empty; doc 22 requires it on every record")
	}

	// The OMS event is bound to the audit record, which closes the link between the
	// state machine and the evidence for it.
	var bound sql.NullString
	dbtest.MustQueryRowDB(t, ctx, db, &bound, `
		SELECT audit_id FROM oms.order_event
		 WHERE order_id=$1 AND event_kind='OUTCOME_RESOLVED'`, orderID)
	if !bound.Valid || bound.String == "" {
		t.Fatal("the OMS event has no audit_id; the state machine and its evidence are unlinked")
	}
}

// An order that reached the risk gate without recording the policy that admitted it
// cannot be audited, so it is refused rather than resolved with a placeholder.
func TestResolvingAnOrderWithNoPolicyRevisionIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := unknownOrder(t, ctx, db)

	if _, err := db.ExecContext(ctx,
		`UPDATE oms."order" SET risk_policy_revisions='{}' WHERE order_id=$1`, orderID); err != nil {
		t.Fatalf("clearing the policy revisions: %v", err)
	}

	_, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID,
		&fakeResolver{outcome: execution.AcceptedOutcome})
	if err == nil {
		t.Fatal("Resolve attributed a resolution to a policy version the order never recorded")
	}

	var state string
	dbtest.MustQueryRowDB(t, ctx, db, &state,
		`SELECT state::text FROM oms."order" WHERE order_id=$1`, orderID)
	if state != "UNKNOWN" {
		t.Fatalf("order state = %s, want UNKNOWN", state)
	}
	var n int
	dbtest.MustQueryRowDB(t, ctx, db, &n,
		`SELECT count(*) FROM audit.record WHERE target_id=$1 AND action='ORDER_OUTCOME_RESOLVED'`, orderID)
	if n != 0 {
		t.Fatalf("audit records = %d, want 0; a refused resolution must leave no evidence", n)
	}
}

// No actor, no resolution.
func TestResolvingWithoutAnActorIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := unknownOrder(t, ctx, db)

	r := &fakeResolver{outcome: execution.AcceptedOutcome}
	if _, err := newCommand(t, db).Resolve(ctx, orderID, "", r); err == nil {
		t.Fatal("Resolve accepted an empty actor identity")
	}
	if r.calls != 0 {
		t.Fatalf("the venue was asked %d times; identity must be established before the read", r.calls)
	}
}

func outboxCount(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	var n int
	dbtest.MustQueryRowDB(t, ctx, db, &n, `SELECT count(*) FROM ops.outbox`)
	return n
}

// No resolver, no resolution. Defaulting to "could not ask" instead of refusing
// would leave the order UNKNOWN forever with no error to explain why.
func TestResolvingWithoutAResolverIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := unknownOrder(t, ctx, db)

	if _, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID, nil); err == nil {
		t.Fatal("Resolve accepted a nil resolver")
	}
	if _, err := newCommand(t, db).Resolve(ctx, "", reconcilerID, &fakeResolver{}); err == nil {
		t.Fatal("Resolve accepted an empty order identity")
	}
}

// Resolution cannot be applied to an order that is not UNKNOWN, even if its
// history says something is undetermined. The two facts disagree, and the state
// machine is the authority on which is wrong.
func TestResolvingAnOrderThatIsNotUnknownIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, _ := readyOrder(t, ctx, db, contracts.EnvPaper)

	// History says undetermined; the order is still RISK_APPROVED.
	seedSubmission(t, ctx, db, orderID, "TIMED_OUT_UNKNOWN", 1)

	_, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID, &fakeResolver{outcome: execution.AcceptedOutcome})
	if err == nil {
		t.Fatal("Resolve advanced an order that is not in UNKNOWN")
	}

	var state string
	dbtest.MustQueryRowDB(t, ctx, db, &state,
		`SELECT state::text FROM oms."order" WHERE order_id=$1`, orderID)
	if state != "RISK_APPROVED" {
		t.Fatalf("order state = %s, want RISK_APPROVED", state)
	}
}
