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

// Applying a venue's answer to a submission.
//
// Before this existed, a successful submission was a one-way door: Prepare advanced
// the order to SUBMITTING and wrote a PENDING submission, and nothing ever read the
// venue's answer. The order stayed in SUBMITTING, the submission stayed PENDING,
// and UndeterminedPrior reads PENDING as undetermined -- so the next submit attempt
// for that order was refused as an unresolved prior attempt. One accepted order
// wedged itself permanently.
//
// The assertions here are on the stored result rather than the returned value,
// because a method that reports a state it did not commit is worse than one that
// returns nothing.

// answeredOrder returns an order that has been prepared for submission: SUBMITTING,
// with one PENDING submission.
func answeredOrder(t *testing.T, ctx context.Context, db *sql.DB,
	env contracts.Environment) (orderID string, prepared execution.Prepared) {

	t.Helper()
	orderID, corr := readyOrder(t, ctx, db, env)

	// An order that cleared the risk gate has the policy revision that admitted it
	// recorded against it. Doc 22 requires policy_version on the audit record, and
	// ApplyVenueAnswer takes it from the order's own history rather than from the
	// adapter, which observed an outcome but did not originate the order. readyOrder
	// seeds an empty revision array to keep the submit tests focused, so it is filled
	// in here -- the same thing unknownOrder does for Resolve.
	if _, err := db.ExecContext(ctx,
		`UPDATE oms."order" SET risk_policy_revisions = ARRAY[$2] WHERE order_id = $1`,
		orderID, dbtest.CanonicalID("pol", int(seq()))); err != nil {
		t.Fatalf("recording the risk policy revision: %v", err)
	}

	p, err := newCommand(t, db).Prepare(ctx, submitIntent(t, orderID, corr, env))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return orderID, p
}

// answer builds a venue answer for a prepared submission.
func answer(t *testing.T, orderID string, prepared execution.Prepared,
	outcome execution.Outcome) execution.VenueAnswer {

	t.Helper()
	return execution.VenueAnswer{
		OrderID:        orderID,
		SubmissionID:   prepared.SubmissionID,
		Outcome:        outcome,
		ResponseDigest: execution.RequestDigest([]byte(`{"venue":"ack"}`)),
		ActorID:        "svc-venue-adapter",
		ObservedAt:     time.Now().UTC().Truncate(time.Microsecond),
	}
}

// orderState reads where an order is.
func orderState(t *testing.T, db *sql.DB, orderID string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(context.Background(),
		`SELECT state::text FROM oms."order" WHERE order_id = $1`, orderID).Scan(&state); err != nil {
		t.Fatalf("reading the order state: %v", err)
	}
	return state
}

// submissionOutcome reads what a submission was recorded as.
func submissionOutcome(t *testing.T, db *sql.DB, submissionID contracts.ID) string {
	t.Helper()
	var outcome string
	if err := db.QueryRowContext(context.Background(),
		`SELECT outcome::text FROM execution.submission WHERE submission_id = $1`,
		submissionID.String()).Scan(&outcome); err != nil {
		t.Fatalf("reading the submission outcome: %v", err)
	}
	return outcome
}

// count runs a counting query.
func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	return n
}

// A venue acceptance moves the order out of SUBMITTING and settles the submission.
//
// It also asserts the accounting an accepted answer must produce: the journal, the
// audit record and the outbox row all exist and are tied to this order. Doc 05 says
// a committed state mutation always has an outbox row, and no trigger enforces it.
func TestAnAcceptedVenueAnswerAcknowledgesTheOrderAndSettlesTheSubmission(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared := answeredOrder(t, ctx, db, contracts.EnvPaper)

	res, err := newCommand(t, db).ApplyVenueAnswer(ctx,
		answer(t, orderID, prepared, execution.AcceptedOutcome))
	if err != nil {
		t.Fatalf("ApplyVenueAnswer: %v", err)
	}
	if res.State != oms.StateAcknowledged {
		t.Errorf("State = %s, want %s", res.State, oms.StateAcknowledged)
	}
	if got := orderState(t, db, orderID); got != string(oms.StateAcknowledged) {
		t.Errorf("stored order state = %s, want ACKNOWLEDGED", got)
	}
	if got := submissionOutcome(t, db, prepared.SubmissionID); got != "ACKNOWLEDGED" {
		t.Errorf("stored submission outcome = %s, want ACKNOWLEDGED", got)
	}

	// The journal event, with the right kind and the risk decision the rule requires.
	var kind string
	var riskID sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT event_kind::text, risk_decision_id FROM oms.order_event
		 WHERE order_id = $1 ORDER BY sequence DESC LIMIT 1`, orderID).
		Scan(&kind, &riskID); err != nil {
		t.Fatalf("reading the journal: %v", err)
	}
	if kind != string(oms.EventAcknowledged) {
		t.Errorf("event kind = %s, want ACKNOWLEDGED", kind)
	}
	if !riskID.Valid {
		t.Error("the ACKNOWLEDGED event carries no risk_decision_id; SUBMITTING -> ACKNOWLEDGED " +
			"requires a durable risk decision and the order row must already carry it")
	}

	if n := count(t, db, `SELECT count(*) FROM audit.record
		WHERE target_type = 'ORDER' AND target_id = $1 AND action = 'ORDER_SUBMIT_ANSWERED'`, orderID); n != 1 {
		t.Errorf("ORDER_SUBMIT_ANSWERED audit records = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM ops.outbox
		WHERE aggregate_id = $1 AND event_type = 'ORDER_SUBMIT_ANSWERED'`, orderID); n != 1 {
		t.Errorf("ORDER_SUBMIT_ANSWERED outbox rows = %d, want 1", n)
	}
}

// The fail-closed case, and the single most important assertion in this file.
//
// A timeout is not a rejection. If this recorded REJECTED it would tell the
// database, durably, that no exposure exists, on the strength of a venue that never
// answered -- and the order would then be resolvable to nothing, because a REJECTED
// order is terminal. UNKNOWN is the honest state, and it is the only one from which
// Command.Resolve can later produce a determinate record.
func TestAnUndeterminedVenueAnswerBecomesUnknownAndNeverRejected(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared := answeredOrder(t, ctx, db, contracts.EnvPaper)

	res, err := newCommand(t, db).ApplyVenueAnswer(ctx,
		answer(t, orderID, prepared, execution.UndeterminedOutcome))
	if err != nil {
		t.Fatalf("ApplyVenueAnswer: %v", err)
	}
	if res.State != oms.StateUnknown {
		t.Errorf("State = %s, want UNKNOWN", res.State)
	}
	if got := orderState(t, db, orderID); got != string(oms.StateUnknown) {
		t.Errorf("stored order state = %s, want UNKNOWN", got)
	}

	// Both the OMS state and the stored outcome. storedFor maps anything that is not
	// explicitly Accepted or Rejected to TIMED_OUT_UNKNOWN, so this is where a
	// future edit that reordered that switch would show up.
	if got := submissionOutcome(t, db, prepared.SubmissionID); got != "TIMED_OUT_UNKNOWN" {
		t.Errorf("stored submission outcome = %s, want TIMED_OUT_UNKNOWN; a timeout recorded as "+
			"REJECTED would tell the database no exposure exists", got)
	}

	var kind string
	if err := db.QueryRowContext(ctx, `
		SELECT event_kind::text FROM oms.order_event
		 WHERE order_id = $1 ORDER BY sequence DESC LIMIT 1`, orderID).Scan(&kind); err != nil {
		t.Fatalf("reading the journal: %v", err)
	}
	if kind != string(oms.EventUnknownOutcome) {
		t.Errorf("event kind = %s, want UNKNOWN_OUTCOME", kind)
	}

	// And the reason text must not read as a refusal. An operator reading UNKNOWN
	// alongside "the venue rejected this" has been actively misled.
	var reason string
	if err := db.QueryRowContext(ctx, `
		SELECT reason FROM oms.order_event WHERE order_id = $1 ORDER BY sequence DESC LIMIT 1`,
		orderID).Scan(&reason); err != nil {
		t.Fatalf("reading the event reason: %v", err)
	}
	if reason == "" {
		t.Error("the UNKNOWN event carries no reason")
	}
}

// A rejection is recorded as a rejection, and moves to the terminal REJECTED state.
func TestARejectedVenueAnswerRejectsTheOrder(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared := answeredOrder(t, ctx, db, contracts.EnvPaper)

	if _, err := newCommand(t, db).ApplyVenueAnswer(ctx,
		answer(t, orderID, prepared, execution.RejectedOutcome)); err != nil {
		t.Fatalf("ApplyVenueAnswer: %v", err)
	}
	if got := orderState(t, db, orderID); got != string(oms.StateRejected) {
		t.Errorf("stored order state = %s, want REJECTED", got)
	}
	if got := submissionOutcome(t, db, prepared.SubmissionID); got != "REJECTED" {
		t.Errorf("stored submission outcome = %s, want REJECTED", got)
	}
	var kind string
	if err := db.QueryRowContext(ctx, `
		SELECT event_kind::text FROM oms.order_event
		 WHERE order_id = $1 ORDER BY sequence DESC LIMIT 1`, orderID).Scan(&kind); err != nil {
		t.Fatalf("reading the journal: %v", err)
	}
	if kind != string(oms.EventRejectedByVenue) {
		t.Errorf("event kind = %s, want REJECTED_BY_VENUE", kind)
	}
}

// A second answer is refused, and nothing is written.
//
// The submission row has already been rewritten by the first answer, so re-applying
// would overwrite a determinate record with whatever the venue says on a second
// observation -- and the state machine has no rule permitting it either.
func TestASecondVenueAnswerIsRefusedAndWritesNothing(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared := answeredOrder(t, ctx, db, contracts.EnvPaper)
	cmd := newCommand(t, db)

	if _, err := cmd.ApplyVenueAnswer(ctx,
		answer(t, orderID, prepared, execution.AcceptedOutcome)); err != nil {
		t.Fatalf("the first answer was refused: %v", err)
	}

	events := count(t, db, `SELECT count(*) FROM oms.order_event WHERE order_id = $1`, orderID)
	audits := count(t, db, `SELECT count(*) FROM audit.record WHERE target_id = $1`, orderID)
	outbox := count(t, db, `SELECT count(*) FROM ops.outbox WHERE aggregate_id = $1`, orderID)

	_, err := cmd.ApplyVenueAnswer(ctx,
		answer(t, orderID, prepared, execution.RejectedOutcome))
	if err == nil {
		t.Fatal("a second venue answer was accepted for an order that had already been answered")
	}
	if !errors.Is(err, execution.ErrAnswerNotApplicable) {
		t.Errorf("the refusal did not report ErrAnswerNotApplicable: %v", err)
	}

	if got := orderState(t, db, orderID); got != string(oms.StateAcknowledged) {
		t.Errorf("order state = %s; a refused second answer must not move it", got)
	}
	if got := submissionOutcome(t, db, prepared.SubmissionID); got != "ACKNOWLEDGED" {
		t.Errorf("submission outcome = %s; a refused second answer must not rewrite it", got)
	}
	if n := count(t, db, `SELECT count(*) FROM oms.order_event WHERE order_id = $1`, orderID); n != events {
		t.Errorf("order events = %d, want %d unchanged", n, events)
	}
	if n := count(t, db, `SELECT count(*) FROM audit.record WHERE target_id = $1`, orderID); n != audits {
		t.Errorf("audit records = %d, want %d unchanged", n, audits)
	}
	if n := count(t, db, `SELECT count(*) FROM ops.outbox WHERE aggregate_id = $1`, orderID); n != outbox {
		t.Errorf("outbox rows = %d, want %d unchanged", n, outbox)
	}
}

// An answer whose submission belongs to a DIFFERENT order is refused.
//
// This is a caller error rather than a race, and it is the one that would otherwise
// be invisible: the submission row is updated and the order row is moved, and if the
// two do not agree the platform has just acknowledged an order using evidence from
// somebody else's send.
func TestAVenueAnswerForAnotherOrdersSubmissionIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared := answeredOrder(t, ctx, db, contracts.EnvPaper)

	// A second, independent order with its own pending submission.
	otherID, otherPrepared := answeredOrder(t, ctx, db, contracts.EnvPaper)
	if otherID == orderID {
		t.Fatal("the two orders share an identity")
	}

	mismatched := answer(t, otherID, prepared, execution.AcceptedOutcome)
	if _, err := newCommand(t, db).ApplyVenueAnswer(ctx, mismatched); err == nil {
		t.Fatal("an answer moved an order using another order's submission")
	}

	// The order named by the submission must be untouched, and so must the order
	// whose id was passed.
	if got := orderState(t, db, orderID); got != string(oms.StateSubmitting) {
		t.Errorf("order %s state = %s, want SUBMITTING; a refused answer must not move it", orderID, got)
	}
	if got := orderState(t, db, otherID); got != string(oms.StateSubmitting) {
		t.Errorf("order %s state = %s, want SUBMITTING", otherID, got)
	}
	if got := submissionOutcome(t, db, otherPrepared.SubmissionID); got != "PENDING" {
		t.Errorf("the untouched submission is %s, want PENDING", got)
	}
}

// A venue answer with no accountable observer, or no evidence of what came back, is
// refused before anything is written.
func TestAnUnattributableOrUnevidencedVenueAnswerIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared := answeredOrder(t, ctx, db, contracts.EnvPaper)
	cmd := newCommand(t, db)

	noActor := answer(t, orderID, prepared, execution.AcceptedOutcome)
	noActor.ActorID = ""
	if _, err := cmd.ApplyVenueAnswer(ctx, noActor); err == nil {
		t.Error("a venue answer with no actor was accepted")
	}

	noDigest := answer(t, orderID, prepared, execution.AcceptedOutcome)
	noDigest.ResponseDigest = ""
	if _, err := cmd.ApplyVenueAnswer(ctx, noDigest); err == nil {
		t.Error("a venue answer with no response digest was accepted")
	}

	noTime := answer(t, orderID, prepared, execution.AcceptedOutcome)
	noTime.ObservedAt = time.Time{}
	if _, err := cmd.ApplyVenueAnswer(ctx, noTime); err == nil {
		t.Error("a venue answer with no observation time was accepted")
	}

	// An outcome outside the vocabulary must be refused rather than defaulted. The
	// default would be UNDETERMINED, which is safe, but a caller reaching it by
	// accident rather than by decision is a bug this should surface.
	odd := answer(t, orderID, prepared, execution.AcceptedOutcome)
	odd.Outcome = execution.Outcome("PARTIALLY_FILLED")
	if _, err := cmd.ApplyVenueAnswer(ctx, odd); err == nil {
		t.Error("a venue answer with an unrecognised outcome was accepted")
	}

	// Nothing above may have written anything.
	if got := orderState(t, db, orderID); got != string(oms.StateSubmitting) {
		t.Errorf("order state = %s, want SUBMITTING", got)
	}
	if got := submissionOutcome(t, db, prepared.SubmissionID); got != "PENDING" {
		t.Errorf("submission outcome = %s, want PENDING", got)
	}
	if n := count(t, db, `SELECT count(*) FROM oms.order_event
		WHERE order_id = $1 AND event_kind IN ('ACKNOWLEDGED','REJECTED_BY_VENUE','UNKNOWN_OUTCOME')`,
		orderID); n != 0 {
		t.Errorf("answer events = %d, want 0; every refusal above must write nothing", n)
	}
}

// An UNKNOWN order produced by an undetermined answer can then be resolved.
//
// This is the reason UNKNOWN is worth entering rather than guessing at a rejection:
// it is the only state from which a determinate record can later be produced. Without
// it the platform either wedges the order in SUBMITTING forever or commits to a
// rejection the venue never made.
func TestAnUndeterminedAnswerLeavesTheOrderResolvable(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, prepared := answeredOrder(t, ctx, db, contracts.EnvPaper)
	cmd := newCommand(t, db)

	if _, err := cmd.ApplyVenueAnswer(ctx,
		answer(t, orderID, prepared, execution.UndeterminedOutcome)); err != nil {
		t.Fatalf("ApplyVenueAnswer: %v", err)
	}

	resolved, err := cmd.Resolve(ctx, orderID, "svc-reconciler", acceptingResolver{})
	if err != nil {
		t.Fatalf("an order left UNKNOWN by an undetermined answer could not be resolved: %v", err)
	}
	if resolved.State != oms.StateAcknowledged {
		t.Errorf("resolved state = %s, want ACKNOWLEDGED", resolved.State)
	}
	if got := orderState(t, db, orderID); got != string(oms.StateAcknowledged) {
		t.Errorf("stored order state = %s, want ACKNOWLEDGED", got)
	}
}

// acceptingResolver reports that the venue does hold the order.
type acceptingResolver struct{}

func (acceptingResolver) Resolve(context.Context, string, string) (execution.Outcome, string, error) {
	return execution.AcceptedOutcome, "venue-ord-1", nil
}
