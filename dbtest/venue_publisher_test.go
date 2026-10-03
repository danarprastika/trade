package dbtest_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aitc/trade/adapters/venue"
	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/execution"
	"github.com/aitc/trade/domain/oms"
	"github.com/aitc/trade/services/dispatch"
)

// The send site.
//
// Prepare refuses an uncertified adapter, which closes the obvious hole. This file
// tests the other one: the certification can be withdrawn between Prepare's commit
// and the dispatcher's call, and an order prepared under a valid certificate must not
// reach the venue after that certificate is revoked.
//
// Every refusal test here asserts the SAME thing, and it is not "an error came
// back": it is that the writer was never called. An error returned after a send is
// the duplicate-order failure this whole path is arranged to avoid, so the count of
// calls is the assertion that matters.

// recordingWriter records every submission and returns a scripted answer.
//
// It is a test double standing in for a venue that does not exist yet. There is no
// concrete adapter in this repository, and inventing one that looked real would be
// worse than admitting the gap.
type recordingWriter struct {
	mu       sync.Mutex
	requests []venue.SubmitRequest
	result   venue.SubmitResult
	err      error
}

func (w *recordingWriter) Submit(_ context.Context, req venue.SubmitRequest) (venue.SubmitResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.requests = append(w.requests, req)
	return w.result, w.err
}

func (w *recordingWriter) Cancel(context.Context, string) (venue.SubmitResult, error) {
	return venue.SubmitResult{}, errors.New("the cancel path is not exercised by these tests")
}

func (w *recordingWriter) calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.requests)
}

func (w *recordingWriter) last() venue.SubmitRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.requests) == 0 {
		return venue.SubmitRequest{}
	}
	return w.requests[len(w.requests)-1]
}

// alwaysFailsApplier stands in for an OMS that cannot record the answer.
type alwaysFailsApplier struct{}

func (alwaysFailsApplier) ApplyVenueAnswer(context.Context, execution.VenueAnswer) (execution.Answered, error) {
	return execution.Answered{}, errors.New("the answer could not be recorded")
}

// sendableOrder returns a prepared submission whose instrument has an effective venue
// symbol mapping, plus the venue id to send to.
func sendableOrder(t *testing.T, ctx context.Context, db *sql.DB,
	env contracts.Environment) (orderID string, prepared execution.Prepared, venueID string) {

	t.Helper()
	orderID, prepared = answeredOrder(t, ctx, db, env)

	var instrument string
	if err := db.QueryRowContext(ctx,
		`SELECT instrument_id FROM oms."order" WHERE order_id = $1`, orderID).Scan(&instrument); err != nil {
		t.Fatalf("reading the order's instrument: %v", err)
	}
	// A canonical venue id, because market.venue_symbol enforces the
	// ^[a-z]{3}_[0-9a-z]{20}$ form. The order row's own venue_id is a bare fixture
	// string with no such constraint, and the publisher resolves the symbol from the
	// submission's venue rather than the order row's.
	venueID = dbtest.CanonicalID("ven", int(seq()))

	if _, err := db.ExecContext(ctx, `
		INSERT INTO market.venue_symbol (
			instrument_id, venue_id, symbol, mapping_version, effective_at,
			changed_by, change_reason)
		VALUES ($1, $2, 'BTC-USD', 1, $3, 'tester', 'fixture mapping')`,
		instrument, venueID, gateNow.Add(-365*24*time.Hour)); err != nil {
		t.Fatalf("inserting the venue symbol mapping: %v", err)
	}
	return orderID, prepared, venueID
}

// submitMessage builds the ORDER_SUBMIT_REQUESTED the dispatcher would hand over.
func submitMessage(t *testing.T, orderID string, prepared execution.Prepared,
	adapterID, venueID string, env contracts.Environment) dispatch.Message {

	t.Helper()
	body, err := json.Marshal(map[string]any{
		"submission_id":    prepared.SubmissionID.String(),
		"order_id":         orderID,
		"venue_id":         venueID,
		"adapter_id":       adapterID,
		"client_order_id":  "cid-" + orderID[4:],
		"venue_idempotent": false,
		"request_digest":   execution.RequestDigest([]byte(`{"side":"BUY","qty":"1"}`)),
	})
	if err != nil {
		t.Fatalf("encoding the payload: %v", err)
	}
	return dispatch.Message{
		EventID:       prepared.EventID,
		EventType:     venue.SubmitEventType,
		AggregateType: "ORDER",
		AggregateID:   orderID,
		Environment:   env,
		OccurredAt:    gateNow,
		Payload:       body,
		Attempt:       1,
	}
}

// sendPublisher wires a real gate, a real OMS command and a fake venue.
func sendPublisher(t *testing.T, db *sql.DB, adapter string,
	writer venue.Writer, answers venue.AnswerApplier) *venue.SubmitPublisher {

	t.Helper()
	pub, err := venue.NewSubmitPublisher(db, writer, declaredGate(t, db, adapter,
		venue.CapabilitySubmit), answers, "svc-venue-adapter")
	if err != nil {
		t.Fatalf("NewSubmitPublisher: %v", err)
	}
	return pub.WithClock(func() time.Time { return gateNow })
}

// A revoked adapter never reaches the venue.
//
// This is the case Prepare alone cannot catch. The order was prepared while the
// certification was valid; the certification was then withdrawn; the send must
// still be refused.
func TestARevokedAdapterNeverReachesTheVenue(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared, venueID := sendableOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 910)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 911)
	})

	writer := &recordingWriter{result: venue.SubmitResult{VenueOrderRef: "v-1", State: "new"}}
	pub := sendPublisher(t, db, adapter, writer, gatedCommand(t, db, declaredGate(t, db, adapter, venue.CapabilitySubmit)))

	// Withdraw the certification before the send, as a person would.
	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		if _, err := tx.ExecContext(ctx, `
			UPDATE execution.adapter_capability SET certification_state = 'REVOKED'
			 WHERE adapter_id = $1 AND environment = 'paper' AND capability = 'SUBMIT'`,
			adapter); err != nil {
			t.Fatalf("revoking: %v", err)
		}
	})

	if err := pub.Publish(ctx, submitMessage(t, orderID, prepared, adapter, venueID, contracts.EnvPaper)); err == nil {
		t.Fatal("a revoked adapter's order was submitted")
	} else if !errors.Is(err, venue.ErrNotCertified) {
		t.Errorf("the refusal did not report ErrNotCertified: %v", err)
	}
	if writer.calls() != 0 {
		t.Errorf("the venue was called %d times for a revoked adapter; it must be called zero times",
			writer.calls())
	}
	if got := orderState(t, db, orderID); got != string(oms.StateSubmitting) {
		t.Errorf("order state = %s, want SUBMITTING", got)
	}
}

// An expired certification is treated exactly as though it never existed.
func TestAnExpiredAdapterNeverReachesTheVenue(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared, venueID := sendableOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 920)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(-time.Hour), 921)
	})

	writer := &recordingWriter{result: venue.SubmitResult{VenueOrderRef: "v-1", State: "new"}}
	pub := sendPublisher(t, db, adapter, writer, gatedCommand(t, db, declaredGate(t, db, adapter, venue.CapabilitySubmit)))

	if err := pub.Publish(ctx, submitMessage(t, orderID, prepared, adapter, venueID, contracts.EnvPaper)); err == nil {
		t.Fatal("an expired adapter's order was submitted")
	} else if !errors.Is(err, venue.ErrCertificationExpired) {
		t.Errorf("the refusal did not report ErrCertificationExpired: %v", err)
	}
	if writer.calls() != 0 {
		t.Errorf("the venue was called %d times for an expired adapter", writer.calls())
	}
}

// A paper certification is not a live one, and the live environment has no row to
// read. The refusal is the absent row, not the permissive state it holds elsewhere.
func TestAPaperCertifiedAdapterNeverReachesLiveVenue(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared, venueID := sendableOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 930)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.LiveEligible, true, gateNow.Add(365*24*time.Hour), 931)
	})

	writer := &recordingWriter{result: venue.SubmitResult{VenueOrderRef: "v-1", State: "new"}}
	pub := sendPublisher(t, db, adapter, writer, gatedCommand(t, db, declaredGate(t, db, adapter, venue.CapabilitySubmit)))

	if err := pub.Publish(ctx,
		submitMessage(t, orderID, prepared, adapter, venueID, contracts.EnvLive)); err == nil {
		t.Fatal("a paper-certified adapter submitted to live")
	}
	if writer.calls() != 0 {
		t.Errorf("the venue was called %d times on the strength of a paper certificate", writer.calls())
	}
}

// The positive case. A certified adapter is called exactly once, with the order's own
// terms and the audited venue symbol, and the order leaves SUBMITTING.
func TestACertifiedAdapterIsSentAndTheOrderIsAcknowledged(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared, venueID := sendableOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 940)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 941)
	})

	writer := &recordingWriter{result: venue.SubmitResult{
		VenueOrderRef: "venue-ord-1",
		State:         "new", // the fixture StateMap's key for ACKNOWLEDGED
	}}
	pub := sendPublisher(t, db, adapter, writer, gatedCommand(t, db, declaredGate(t, db, adapter, venue.CapabilitySubmit)))

	if err := pub.Publish(ctx,
		submitMessage(t, orderID, prepared, adapter, venueID, contracts.EnvPaper)); err != nil {
		t.Fatalf("a certified adapter was refused: %v", err)
	}
	if writer.calls() != 1 {
		t.Fatalf("the venue was called %d times, want exactly 1", writer.calls())
	}
	if got := writer.last().VenueSymbol; got != "BTC-USD" {
		t.Errorf("sent symbol = %q, want BTC-USD from the audited mapping", got)
	}
	if got := writer.last().Side; got != contracts.Side("BUY") {
		t.Errorf("sent side = %s, want BUY", got)
	}
	// Compared as a value, not as text. quantity is NUMERIC(38,18) on the way out,
	// so it reads back as 1.000000000000000000 and an equality check against "1"
	// would be asserting the rendering rather than the amount.
	if got := writer.last().Quantity; !got.Equal(contracts.MustParseDecimal("1")) {
		t.Errorf("sent quantity = %s, want 1", got)
	}
	if got := orderState(t, db, orderID); got != string(oms.StateAcknowledged) {
		t.Errorf("order state = %s, want ACKNOWLEDGED", got)
	}
}

// A timeout is UNKNOWN, never REJECTED.
//
// The venue took the request and did not answer. Recording that as a rejection would
// tell the database no exposure exists; recording it as UNKNOWN keeps the order
// resolvable by Command.Resolve.
func TestATimedOutSubmissionBecomesUnknownAndNeverRejected(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared, venueID := sendableOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 950)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 951)
	})

	writer := &recordingWriter{result: venue.SubmitResult{TimedOut: true}}
	pub := sendPublisher(t, db, adapter, writer, gatedCommand(t, db, declaredGate(t, db, adapter, venue.CapabilitySubmit)))

	if err := pub.Publish(ctx,
		submitMessage(t, orderID, prepared, adapter, venueID, contracts.EnvPaper)); err != nil {
		t.Fatalf("a timeout was reported as a publish failure: %v", err)
	}
	if got := orderState(t, db, orderID); got != string(oms.StateUnknown) {
		t.Errorf("order state = %s, want UNKNOWN; a timeout is not a rejection", got)
	}
	if got := submissionOutcome(t, db, prepared.SubmissionID); got != "TIMED_OUT_UNKNOWN" {
		t.Errorf("submission outcome = %s, want TIMED_OUT_UNKNOWN", got)
	}
}

// A transport error after the request may have been sent is also UNKNOWN. The
// platform cannot tell a rejected-on-arrival request from one that was placed.
func TestATransportErrorBecomesUnknown(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared, venueID := sendableOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 960)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 961)
	})

	writer := &recordingWriter{err: errors.New("connection reset by peer")}
	pub := sendPublisher(t, db, adapter, writer, gatedCommand(t, db, declaredGate(t, db, adapter, venue.CapabilitySubmit)))

	if err := pub.Publish(ctx,
		submitMessage(t, orderID, prepared, adapter, venueID, contracts.EnvPaper)); err != nil {
		t.Fatalf("a transport error was reported as a publish failure: %v", err)
	}
	if got := orderState(t, db, orderID); got != string(oms.StateUnknown) {
		t.Errorf("order state = %s, want UNKNOWN", got)
	}
}

// A venue state the adapter does not declare a mapping for is UNKNOWN, not a guess.
//
// The fixture StateMap maps only "new". A venue that invents a state the adapter has
// not seen must not be read as anything, and least of all as a rejection.
func TestAnUnmappedVenueStateBecomesUnknown(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared, venueID := sendableOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 970)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 971)
	})

	writer := &recordingWriter{result: venue.SubmitResult{
		VenueOrderRef: "venue-ord-2",
		State:         "SOME_STATE_ADDED_LAST_MONTH",
	}}
	pub := sendPublisher(t, db, adapter, writer, gatedCommand(t, db, declaredGate(t, db, adapter, venue.CapabilitySubmit)))

	if err := pub.Publish(ctx,
		submitMessage(t, orderID, prepared, adapter, venueID, contracts.EnvPaper)); err != nil {
		t.Fatalf("an unmapped venue state was reported as a publish failure: %v", err)
	}
	if writer.calls() != 1 {
		t.Fatalf("the venue was called %d times, want 1", writer.calls())
	}
	if got := orderState(t, db, orderID); got != string(oms.StateUnknown) {
		t.Errorf("order state = %s, want UNKNOWN; an unmapped state must not be guessed", got)
	}
}

// Once the venue has been called, a failure to record the answer must NOT be
// returned, because returning it makes the dispatcher call Submit again.
//
// This is the test that protects the duplicate order. A recording failure is far
// cheaper than a second live order, and the submission is left PENDING precisely so
// that the retry is blocked until a human reconciles it.
func TestAFailedRecordingAfterTheSendIsNotRetried(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared, venueID := sendableOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 980)

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 981)
	})

	writer := &recordingWriter{result: venue.SubmitResult{VenueOrderRef: "venue-ord-3", State: "new"}}
	pub := sendPublisher(t, db, adapter, writer, alwaysFailsApplier{})

	if err := pub.Publish(ctx,
		submitMessage(t, orderID, prepared, adapter, venueID, contracts.EnvPaper)); err != nil {
		t.Fatalf("Publish returned an error after the venue had already been called, which makes "+
			"the dispatcher retry and place a second order: %v", err)
	}
	if writer.calls() != 1 {
		t.Errorf("the venue was called %d times, want exactly 1", writer.calls())
	}
	// The order stays SUBMITTING and the submission stays PENDING, which is
	// undetermined. That is the recoverable state: a retry is refused until
	// reconciliation authorises one.
	if got := submissionOutcome(t, db, prepared.SubmissionID); got != "PENDING" {
		t.Errorf("submission outcome = %s, want PENDING; PENDING is what makes a second attempt "+
			"refuse until it is reconciled", got)
	}
}

// An instrument with no effective symbol mapping cannot be addressed, and guessing a
// symbol is how an order reaches the wrong instrument. Nothing is sent.
func TestAnUnmappedInstrumentIsNotSent(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared := answeredOrder(t, ctx, db, contracts.EnvPaper)
	adapter := adapterID(t, 990)
	venueID := dbtest.CanonicalID("ven", int(seq())) // no venue_symbol row for this

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		seedCapability(t, ctx, tx, adapter, contracts.EnvPaper, venue.CapabilitySubmit,
			venue.PaperCertified, true, gateNow.Add(365*24*time.Hour), 991)
	})

	writer := &recordingWriter{result: venue.SubmitResult{VenueOrderRef: "v-1", State: "new"}}
	pub := sendPublisher(t, db, adapter, writer, gatedCommand(t, db, declaredGate(t, db, adapter, venue.CapabilitySubmit)))

	if err := pub.Publish(ctx,
		submitMessage(t, orderID, prepared, adapter, venueID, contracts.EnvPaper)); err == nil {
		t.Fatal("an order with no venue symbol mapping was submitted")
	}
	if writer.calls() != 0 {
		t.Errorf("the venue was called %d times for an untranslatable instrument", writer.calls())
	}
	if got := orderState(t, db, orderID); got != string(oms.StateSubmitting) {
		t.Errorf("order state = %s, want SUBMITTING", got)
	}
}

// An event this publisher does not handle is unroutable, so the dispatcher
// dead-letters it instead of retrying a routing gap.
func TestANonSubmitEventIsUnroutable(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	adapter := adapterID(t, 995)
	writer := &recordingWriter{}
	pub := sendPublisher(t, db, adapter, writer, alwaysFailsApplier{})

	err := pub.Publish(ctx, dispatch.Message{
		EventType:   "ORDER_OUTCOME_RESOLVED",
		Environment: contracts.EnvPaper,
	})
	if err == nil {
		t.Fatal("a non-submit event was accepted by the submit publisher")
	}
	if !errors.Is(err, dispatch.ErrUnroutable) {
		t.Errorf("the refusal was not ErrUnroutable: %v", err)
	}
	if writer.calls() != 0 {
		t.Errorf("the venue was called for a non-submit event")
	}
}
