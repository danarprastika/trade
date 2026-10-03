package dbtest_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/services/dispatch"
)

// The outbox dispatcher: the reader half of doc 05's contract.
//
// Doc 05: "The dispatcher reads committed outbox rows using FOR UPDATE SKIP LOCKED,
// publishes the event, and marks delivery state. Duplicate publication is
// acceptable; consumers must be idempotent. Financial correctness never depends on
// exactly-once transport."
//
// The tests below assert three things that are easy to get wrong and expensive to
// get wrong. A committed submit's outbox row is actually delivered, which is what
// makes Command.Prepare load-bearing rather than decorative. A dispatcher that dies
// neither loses the event nor strands it. And an event the dispatcher cannot route
// is never quietly dropped.
//
// # Why every test here clears the outbox first
//
// The dispatcher claims every PENDING row, not the row a test happens to care
// about. That is correct behaviour and it makes these tests interdependent in a way
// that is easy to miss: other tests in this package commit outbox rows through
// Command.Prepare and never dispatch them, so the table accumulates rows across
// runs. Once residue outgrows the claim batch, a test's own row is no longer claimed
// in the first pass and the test fails for a reason that has nothing to do with the
// code.
//
// So each test clears delivery state before it runs. That is safe because delivery
// state is mutable by design -- it is the whole point of ops.outbox -- and because
// no other test asserts on a row it did not just create. A test that asserted
// delivery state for a row committed by some other test would be asserting on
// residue, which is the same defect in a different place.

// recordingPublisher captures what it was asked to publish and can be told to fail.
type recordingPublisher struct {
	// err is returned for every event. ErrUnroutable is passed through rather than
	// wrapped by the dispatcher, so a test can distinguish the two cases by
	// errors.Is.
	err error

	published []dispatch.Message
}

func (p *recordingPublisher) Publish(_ context.Context, m dispatch.Message) error {
	p.published = append(p.published, m)
	return p.err
}

// forEvent counts how many times this publisher was handed one specific event.
//
// Every assertion here is per-event rather than on the total. The dispatcher
// legitimately claims every pending row, so a total is a property of whatever
// residue the table happened to hold, and asserting on it would make these tests
// pass or fail for reasons unrelated to the code under test.
func (p *recordingPublisher) forEvent(id string) []dispatch.Message {
	var out []dispatch.Message
	for _, m := range p.published {
		if m.EventID.String() == id {
			out = append(out, m)
		}
	}
	return out
}

// newDispatcher builds a Dispatcher writing through db.
func newDispatcher(t *testing.T, db *sql.DB, pub dispatch.Publisher, name string) *dispatch.Dispatcher {
	t.Helper()
	d, err := dispatch.New(db, pub, name)
	if err != nil {
		t.Fatalf("dispatch.New(%q): %v", name, err)
	}
	return d
}

// cleanDeliveryState clears outbox and dead-letter rows so a test starts from a
// known queue.
//
// The outbox delete is narrower than "delete everything", and deliberately so. Every
// test in this file commits a real Prepare, which commits an order in SUBMITTING with
// its outbox row -- so by the time a later test runs, the database holds rows belonging
// to orders that are still in SUBMITTING. Migration 0026's
// ops_outbox_delete_preserves_submitting_order refuses to delete those, because that
// row is the durable record that a send is owed; deleting it would leave an order
// claiming an unfulfilled send that nothing will ever read.
//
// Rows that survive this are already DELIVERED, since the test that committed them is
// the test that dispatched them, and a dispatcher claims only PENDING rows. They
// therefore stay out of the way of the Claimed/Dispatched counts these tests assert.
func cleanDeliveryState(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `DELETE FROM ops.event_dead_letter`); err != nil {
		t.Fatalf("clearing dead letters: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		DELETE FROM ops.outbox o
		 WHERE NOT EXISTS (
		       SELECT 1 FROM oms."order" ord
		        WHERE ord.order_id = o.aggregate_id
		          AND ord.state = 'SUBMITTING')`); err != nil {
		t.Fatalf("clearing the outbox: %v", err)
	}
}

// outboxState reads a row's delivery state on a separate connection.
//
// A committed mutation cannot demonstrate its own durability from inside the
// transaction that made it, so every delivery assertion reads from outside.
func outboxState(t *testing.T, ctx context.Context, db *sql.DB, eventID string) (state string, attempts int, maxAttempts int, lastErr sql.NullString) {
	t.Helper()
	if err := db.QueryRowContext(ctx,
		`SELECT dispatch_state, attempt_count, max_attempts, last_error
		   FROM ops.outbox WHERE event_id = $1`, eventID).
		Scan(&state, &attempts, &maxAttempts, &lastErr); err != nil {
		t.Fatalf("reading outbox state for %s: %v", eventID, err)
	}
	return state, attempts, maxAttempts, lastErr
}

// committedSubmit runs a real Command.Prepare and returns the outbox event id it
// committed.
//
// The row is built through Prepare rather than inserted directly because the claim
// under test is "a committed state mutation has a durable outbox record", not "a row
// can be inserted into ops.outbox". A dispatcher test that seeded its own rows would
// pass even if Prepare had stopped writing them, which is the failure this is here to
// catch.
func committedSubmit(t *testing.T, ctx context.Context, db *sql.DB) (orderID, eventID string) {
	t.Helper()
	orderID, _ = readyOrder(t, ctx, db, contracts.EnvPaper)
	corr := dbtest.CanonicalID("cmd", int(seq()))
	prepared, err := newCommand(t, db).Prepare(ctx, submitIntent(t, orderID, corr, contracts.EnvPaper))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return orderID, prepared.EventID.String()
}

// The headline claim: a committed submit's outbox row is claimed, published, and
// marked delivered, and the publisher is handed the order's own correlation id.
func TestACommittedSubmissionIsPublishedAndMarkedDelivered(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)
	orderID, eventID := committedSubmit(t, ctx, db)

	pub := &recordingPublisher{}
	res, err := newDispatcher(t, db, pub, "dispatcher-test-01").DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if res.Claimed != 1 || res.Dispatched != 1 {
		t.Fatalf("Result = %+v, want exactly 1 claimed and 1 dispatched", res)
	}

	sent := pub.forEvent(eventID)
	if len(sent) != 1 {
		t.Fatalf("the committed event was published %d times, want 1", len(sent))
	}
	got := sent[0]
	if got.EventType != "ORDER_SUBMIT_REQUESTED" {
		t.Fatalf("EventType = %s, want ORDER_SUBMIT_REQUESTED", got.EventType)
	}
	if got.AggregateType != "ORDER" || got.AggregateID != orderID {
		t.Fatalf("aggregate = %s/%s, want ORDER/%s", got.AggregateType, got.AggregateID, orderID)
	}
	if got.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1; a publisher asked to deliver the first attempt and one "+
			"asked to deliver the fifth are being asked different things", got.Attempt)
	}
	if got.MaxAttempts < 2 {
		t.Fatalf("MaxAttempts = %d, want room for a retry", got.MaxAttempts)
	}

	// The payload is the command Prepare committed, not a reconstruction.
	var payload struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("published payload is not valid JSON: %v", err)
	}
	if payload.OrderID != orderID {
		t.Fatalf("published payload names order %s, want %s", payload.OrderID, orderID)
	}

	state, attempts, _, lastErr := outboxState(t, ctx, db, eventID)
	if state != "DISPATCHED" {
		t.Fatalf("dispatch_state = %s, want DISPATCHED", state)
	}
	if attempts != 0 {
		t.Fatalf("attempt_count = %d, want 0; a delivered row spent no attempt, because an "+
			"attempt is a failure to deliver rather than an occasion of trying", attempts)
	}
	if lastErr.Valid {
		t.Fatalf("last_error = %q, want NULL on a delivered row", lastErr.String)
	}
}

// A failed publication is not a delivered event. The row goes back to PENDING with
// the reason recorded, one attempt spent, and is not dead-lettered while attempts
// remain.
func TestAFailedPublicationReturnsTheRowToPendingWithItsReason(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)
	_, eventID := committedSubmit(t, ctx, db)

	pub := &recordingPublisher{err: errors.New("venue unreachable")}
	d := newDispatcher(t, db, pub, "dispatcher-test-01")

	res, err := d.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if res.Failed != 1 || res.DeadLettered != 0 {
		t.Fatalf("Result = %+v, want 1 failed and 0 dead-lettered", res)
	}

	state, attempts, _, lastErr := outboxState(t, ctx, db, eventID)
	if state != "PENDING" {
		t.Fatalf("dispatch_state = %s, want PENDING; a failed publication must not look delivered", state)
	}
	if attempts != 1 {
		t.Fatalf("attempt_count = %d, want 1", attempts)
	}
	if !lastErr.Valid || lastErr.String != "venue unreachable" {
		t.Fatalf("last_error = %v, want the reason the event is not arriving", lastErr)
	}

	// The row is claimable again, so the failure is retryable rather than terminal,
	// and the retry is numbered as the second attempt.
	if _, err := d.DispatchOnce(ctx); err != nil {
		t.Fatalf("second DispatchOnce: %v", err)
	}
	sent := pub.forEvent(eventID)
	if len(sent) != 2 {
		t.Fatalf("the event was published %d times, want 2; a returned-to-PENDING row must be "+
			"claimable again", len(sent))
	}
	if sent[1].Attempt != 2 {
		t.Fatalf("second attempt numbered %d, want 2", sent[1].Attempt)
	}
}

// Attempts are bounded. A row that never publishes is dead-lettered once its
// attempts run out, with a reason recorded, rather than retried forever.
func TestAnExhaustedRowIsDeadLetteredWithItsReason(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)
	_, eventID := committedSubmit(t, ctx, db)

	_, _, maxAttempts, _ := outboxState(t, ctx, db, eventID)
	pub := &recordingPublisher{err: errors.New("venue unreachable")}
	d := newDispatcher(t, db, pub, "dispatcher-test-01")

	// Keep dispatching until the row stops being claimable. One extra pass proves
	// the dead-lettered row is not picked up again.
	for i := 0; i < maxAttempts+2; i++ {
		if _, err := d.DispatchOnce(ctx); err != nil {
			t.Fatalf("DispatchOnce pass %d: %v", i+1, err)
		}
	}

	if got := len(pub.forEvent(eventID)); got != maxAttempts {
		t.Fatalf("published %d times, want exactly max_attempts (%d); the row must not be "+
			"retried past its own ceiling", got, maxAttempts)
	}

	state, attempts, _, lastErr := outboxState(t, ctx, db, eventID)
	if state != "DEAD_LETTERED" {
		t.Fatalf("dispatch_state = %s, want DEAD_LETTERED", state)
	}
	if attempts != maxAttempts {
		t.Fatalf("attempt_count = %d, want %d", attempts, maxAttempts)
	}
	if !lastErr.Valid {
		t.Fatal("last_error is NULL on a dead-lettered row; the reason an event was given up on " +
			"is the only evidence a human has")
	}

	// The dead letter exists, is attributable to this dispatcher, and carries the
	// payload that could not be delivered.
	var code, group, dlPayload string
	var resolved sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT error_code, consumer_group, payload::text, resolved_at
		  FROM ops.event_dead_letter WHERE event_id = $1`, eventID).
		Scan(&code, &group, &dlPayload, &resolved); err != nil {
		t.Fatalf("reading the dead letter for %s: %v", eventID, err)
	}
	if code != "EXHAUSTED" {
		t.Fatalf("error_code = %s, want EXHAUSTED", code)
	}
	if group != "dispatcher-test-01" {
		t.Fatalf("consumer_group = %s, want the dispatcher's name so the event traces to a process", group)
	}
	if dlPayload == "" {
		t.Fatal("the dead letter carries no payload; it would stop being evidence once the " +
			"outbox row is cleaned up")
	}
	if resolved.Valid {
		t.Fatal("resolved_at is set on a dead letter nobody has resolved")
	}
}

// An event the dispatcher cannot route is dead-lettered at once, not after burning
// every attempt. A routing gap is permanent; retrying it only delays the moment a
// human finds out.
func TestAnUnroutableEventIsDeadLetteredImmediately(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)
	_, eventID := committedSubmit(t, ctx, db)

	pub := &recordingPublisher{err: fmt.Errorf("ORDER_SUBMIT_REQUESTED: %w", dispatch.ErrUnroutable)}
	res, err := newDispatcher(t, db, pub, "dispatcher-test-01").DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if res.DeadLettered != 1 {
		t.Fatalf("Result = %+v, want 1 dead-lettered on the first pass", res)
	}

	state, attempts, _, _ := outboxState(t, ctx, db, eventID)
	if state != "DEAD_LETTERED" {
		t.Fatalf("dispatch_state = %s, want DEAD_LETTERED", state)
	}
	if attempts != 1 {
		t.Fatalf("attempt_count = %d, want 1; an unroutable event must not consume the whole ceiling", attempts)
	}

	var code string
	dbtest.MustQueryRowDB(t, ctx, db, &code,
		`SELECT error_code FROM ops.event_dead_letter WHERE event_id = $1`, eventID)
	if code != "UNROUTABLE" {
		t.Fatalf("error_code = %s, want UNROUTABLE, so a routing gap is distinguishable from a "+
			"venue that was merely down", code)
	}
}

// A dispatcher that dies mid-delivery must not lose the event and must not strand
// it. The lease is what distinguishes "in flight" from "abandoned", and reclaiming
// is safe only because doc 05 permits duplicates.
func TestAnExpiredClaimIsReclaimedAndRedelivered(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)
	_, eventID := committedSubmit(t, ctx, db)

	pub := &recordingPublisher{}
	if _, err := newDispatcher(t, db, pub, "dispatcher-crashed").DispatchOnce(ctx); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	stageAbandonedClaim(t, ctx, db, eventID, "dispatcher-crashed", 0)

	pub = &recordingPublisher{}
	reclaimer := newDispatcher(t, db, pub, "dispatcher-reclaimer").WithLease(time.Minute)

	res, err := reclaimer.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if res.Reclaimed != 1 || res.DeadLettered != 0 {
		t.Fatalf("Result = %+v, want 1 reclaimed and 0 dead-lettered", res)
	}

	state, attempts, _, lastErr := outboxState(t, ctx, db, eventID)
	if state != "PENDING" {
		t.Fatalf("dispatch_state = %s, want PENDING; a reclaimed row is claimable again, not "+
			"resuming a claim nobody holds", state)
	}
	if attempts != 1 {
		t.Fatalf("attempt_count = %d, want 1; a claim abandoned by a crash is a spent attempt", attempts)
	}
	if !lastErr.Valid {
		t.Fatal("last_error is NULL after a reclaim; the reason a delivery was abandoned is the " +
			"only evidence of what happened to the event")
	}

	// And it is delivered on the next pass, so the crash cost a redelivery rather
	// than the event.
	if _, err := reclaimer.DispatchOnce(ctx); err != nil {
		t.Fatalf("DispatchOnce after reclaim: %v", err)
	}
	if got := len(pub.forEvent(eventID)); got != 1 {
		t.Fatalf("the reclaimed event was published %d times, want 1; the crash must cost a "+
			"redelivery rather than the event", got)
	}
	if state, _, _, _ := outboxState(t, ctx, db, eventID); state != "DISPATCHED" {
		t.Fatalf("dispatch_state = %s, want DISPATCHED", state)
	}
}

// A claim that is still within its lease belongs to a live dispatcher and must not
// be taken from it. Two dispatchers racing for one row is exactly what SKIP LOCKED
// and the lease exist to prevent.
func TestAFreshClaimIsNotReclaimedFromALiveDispatcher(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)
	_, eventID := committedSubmit(t, ctx, db)

	// Claimed now, so the lease is nowhere near expiry.
	if _, err := db.ExecContext(ctx, `
		UPDATE ops.outbox
		   SET dispatch_state='DISPATCHING', claimed_by='dispatcher-live',
		       claimed_at=now(), dispatched_at=NULL
		 WHERE event_id=$1`, eventID); err != nil {
		t.Fatalf("staging a live claim: %v", err)
	}

	res, err := newDispatcher(t, db, &recordingPublisher{}, "dispatcher-other").ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if res.Reclaimed != 0 || res.DeadLettered != 0 {
		t.Fatalf("Result = %+v, want nothing touched; a claim inside its lease is a live "+
			"dispatcher's, not an abandoned one", res)
	}
	if state, _, _, _ := outboxState(t, ctx, db, eventID); state != "DISPATCHING" {
		t.Fatalf("dispatch_state = %s, want DISPATCHING", state)
	}
}

// A row whose last attempt was lost to a crash cannot be claimed again -- doing so
// would break outbox_attempts_bounded -- and must not be left in DISPATCHING, which
// no query for pending work ever returns. It is dead-lettered instead: failed, and
// visible, rather than invisible.
func TestALostFinalAttemptIsDeadLetteredRatherThanStranded(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)
	_, eventID := committedSubmit(t, ctx, db)

	// On its final attempt, with the dispatcher holding it long dead. The staging
	// leaves attempt_count one below the ceiling, which is the furthest an in-flight
	// row can legally be: outbox_exhausted_is_dead_lettered forbids a claimed row
	// from having spent its last attempt.
	_, _, maxAttempts, _ := outboxState(t, ctx, db, eventID)
	stageAbandonedClaim(t, ctx, db, eventID, "dispatcher-died", maxAttempts-1)

	pub := &recordingPublisher{}
	res, err := newDispatcher(t, db, pub, "dispatcher-reclaimer").WithLease(time.Minute).ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if res.DeadLettered != 1 {
		t.Fatalf("DeadLettered = %d, want 1", res.DeadLettered)
	}
	if res.Reclaimed != 0 {
		t.Fatalf("Reclaimed = %d, want 0; a row with no attempts left cannot be re-claimed", res.Reclaimed)
	}
	if len(pub.published) != 0 {
		t.Fatalf("published %d events, want 0; an exhausted row is dead-lettered, not tried again",
			len(pub.published))
	}

	if state, _, _, _ := outboxState(t, ctx, db, eventID); state != "DEAD_LETTERED" {
		t.Fatalf("dispatch_state = %s, want DEAD_LETTERED; leaving it DISPATCHING would make the "+
			"event invisible rather than failed", state)
	}

	var code, detail string
	if err := db.QueryRowContext(ctx, `
		SELECT error_code, error_detail FROM ops.event_dead_letter WHERE event_id = $1`,
		eventID).Scan(&code, &detail); err != nil {
		t.Fatalf("reading the dead letter for %s: %v", eventID, err)
	}
	if code != "LEASE_EXPIRED_EXHAUSTED" {
		t.Fatalf("error_code = %s, want LEASE_EXPIRED_EXHAUSTED so a lost attempt is "+
			"distinguishable from a refused publication", code)
	}
	if detail == "" {
		t.Fatal("error_detail is empty; the dead letter must say which dispatcher lost the row")
	}
}

// The two loops in one call: recover, then deliver. A dispatcher that only
// dispatched would strand crashed rows; one that only reclaimed would deliver
// nothing.
func TestDispatchReclaimedRecoversAndDelivers(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)
	_, eventID := committedSubmit(t, ctx, db)

	stageAbandonedClaim(t, ctx, db, eventID, "dispatcher-died", 0)

	pub := &recordingPublisher{}
	res, err := newDispatcher(t, db, pub, "dispatcher-loop").WithLease(time.Minute).DispatchReclaimed(ctx)
	if err != nil {
		t.Fatalf("DispatchReclaimed: %v", err)
	}
	if res.Reclaimed != 1 || res.Dispatched != 1 {
		t.Fatalf("Result = %+v, want 1 reclaimed and 1 dispatched in one pass", res)
	}
	if state, _, _, _ := outboxState(t, ctx, db, eventID); state != "DISPATCHED" {
		t.Fatalf("dispatch_state = %s, want DISPATCHED", state)
	}
}

// Nothing is claimable when there is nothing pending, and an empty pass is not an
// error. A dispatcher loop runs this constantly.
func TestDispatchingAnEmptyOutboxIsNotAnError(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)

	pub := &recordingPublisher{}
	res, err := newDispatcher(t, db, pub, "dispatcher-idle").DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("DispatchOnce on an empty outbox: %v", err)
	}
	if res.Claimed != 0 || len(pub.published) != 0 {
		t.Fatalf("Result = %+v with %d published, want nothing claimed and nothing published",
			res, len(pub.published))
	}
}

// A publisher that fails on one event and succeeds on another must not take the
// successful one down with it. Batch failure is not a unit.
func TestOneFailedEventDoesNotFailTheRestOfTheBatch(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)
	_, failing := committedSubmit(t, ctx, db)
	_, succeeding := committedSubmit(t, ctx, db)

	// Fail only the first event committed, so the batch is genuinely mixed.
	pub := &selectivePublisher{failEvent: failing}
	res, err := newDispatcher(t, db, pub, "dispatcher-mixed").DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if res.Dispatched != 1 || res.Failed != 1 {
		t.Fatalf("Result = %+v, want 1 dispatched and 1 failed", res)
	}
	if s1, _, _, _ := outboxState(t, ctx, db, failing); s1 != "PENDING" {
		t.Fatalf("the failing event is %s, want PENDING", s1)
	}
	if s2, _, _, _ := outboxState(t, ctx, db, succeeding); s2 != "DISPATCHED" {
		t.Fatalf("the succeeding event is %s, want DISPATCHED; one bad event must not hold up "+
			"the rest of the batch", s2)
	}
}

// The dispatcher may not be constructible into a state where it would report events
// delivered without delivering them. A nil publisher is the dangerous case; an
// unnamed one makes claimed_by useless for diagnosing a lost row.
func TestADispatcherWithoutAPublisherOrNameIsRefused(t *testing.T) {
	db := dbtest.Open(t)

	if _, err := dispatch.New(db, nil, "dispatcher-test"); err == nil {
		t.Fatal("New accepted a nil publisher; that dispatcher would mark financial events " +
			"delivered without delivering them")
	}
	if _, err := dispatch.New(db, &recordingPublisher{}, ""); err == nil {
		t.Fatal("New accepted an empty name; claimed_by is how an undelivered event is traced " +
			"to the process that owned it")
	}
	if _, err := dispatch.New(nil, &recordingPublisher{}, "dispatcher-test"); err == nil {
		t.Fatal("New accepted a nil database handle")
	}
}

// Delivery state is written by whatever credential holds a connection, so the
// outbox must carry the principal guard. This is invariant 8 enforced at the point of
// delivery rather than only at the point of mutation, and it is inherited from the
// trigger the migration already installed.
func TestOutboxWritesAreSubjectToThePrincipalGuard(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	cleanDeliveryState(t, ctx, db)
	_, eventID := committedSubmit(t, ctx, db)

	var triggers int
	dbtest.MustQueryRowDB(t, ctx, db, &triggers, `
		SELECT count(*) FROM pg_trigger
		 WHERE tgrelid = 'ops.outbox'::regclass
		   AND NOT tgisinternal
		   AND pg_get_triggerdef(oid) ILIKE '%guard_principal_on_write%'`)
	if triggers == 0 {
		t.Fatal("ops.outbox has no principal guard, so delivery state is written by whatever " +
			"credential happens to hold a connection")
	}

	// The row a real commit produced is claimable, which is the state a prepared
	// submission must be in to be dispatchable at all.
	var pending int
	dbtest.MustQueryRowDB(t, ctx, db, &pending, `
		SELECT count(*) FROM ops.outbox WHERE event_id = $1 AND dispatch_state = 'PENDING'`, eventID)
	if pending != 1 {
		t.Fatalf("the committed row for %s is not claimable PENDING; a prepared submission "+
			"that cannot be dispatched is the failure this whole path exists to prevent", eventID)
	}
}

// selectivePublisher fails exactly one event and succeeds at the rest.
type selectivePublisher struct {
	failEvent string
	published []dispatch.Message
}

func (p *selectivePublisher) Publish(_ context.Context, m dispatch.Message) error {
	p.published = append(p.published, m)
	if m.EventID.String() == p.failEvent {
		return errors.New("venue refused this one")
	}
	return nil
}

// stageAbandonedClaim puts a row into the state a crashed dispatcher leaves behind:
// claimed, held by someone, and long past its lease.
func stageAbandonedClaim(t *testing.T, ctx context.Context, db *sql.DB, eventID, claimedBy string, attempts int) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		UPDATE ops.outbox
		   SET dispatch_state = 'DISPATCHING',
		       claimed_by = $2,
		       claimed_at = now() - interval '10 minutes',
		       attempt_count = $3,
		       dispatched_at = NULL
		 WHERE event_id = $1`, eventID, claimedBy, attempts); err != nil {
		t.Fatalf("staging an abandoned claim on %s: %v", eventID, err)
	}
}
