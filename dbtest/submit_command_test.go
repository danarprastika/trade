package dbtest_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/execution"
	"github.com/aitc/trade/services/audit"
)

// The submit command's transaction boundary.
//
// Doc 05 requires domain validation, state mutation, audit record creation and
// outbox insertion in one transaction, and states that a committed state mutation
// ALWAYS has a durable outbox record. Nothing in the schema enforces that second
// sentence -- there is no trigger on ops.outbox or oms."order" requiring the
// pairing -- so an omission here would be silent and permanent: the order would
// be SUBMITTING with nothing committed to send, and nothing would ever complain.
//
// These tests therefore assert the pairing in both directions, and assert that a
// failure at any step leaves nothing behind.

// readyOrder returns an order at RISK_APPROVED, which is the only state from which
// a submit may proceed, plus its correlation id.
// seq hands out distinct values for dbtest.CanonicalID and dbtest.UniqueSequence.
//
// Those two helpers derive their value from a per-run nonce plus the n passed in,
// so two call sites passing the same n collide -- and a collided primary key fails
// the second test with a duplicate-key error that says nothing about the code
// under test. Every id in this file comes from here instead.
//
// The counter also counts from seqBase rather than from 1, and that is a separate
// concern from uniqueness within this file.
//
// readyOrder COMMITs its rows. The ledger and journal tables are append-only, so a
// test that needs to observe a real commit cannot delete them afterwards, and the
// ids it claimed survive into every file that runs later. Most other files here
// hardcode their literals instead (ins 11, acc 3, and so on) and rely on those
// inserts being ROLLED BACK, which leaves nothing behind to collide with.
//
// That made the usable range a property of FILE ORDER. Any file sorting before
// this one that committed a seq()-derived id claimed a literal that a later file
// still meant to use, and the later file failed on a duplicate key while testing
// code it never touched. dispatch_test.go sorts under 'd', ahead of
// gate_composition_test.go ('g') and invariants_test.go ('i'), so adding it broke
// 13 of their tests and nothing of its own. Counting from a band above every
// literal in the package makes the two ranges disjoint by construction, so a new
// file's position in the alphabet stops being load-bearing.
//
// seqBase clears the largest literal used anywhere in this package (970500) and
// the 900000+ band that risk_gate_integration_test.go reserves. It cannot collide
// with dbtest.UniqueSequence either, which already returns values at or above 1<<31.
const seqBase = 1_000_000

var seqCounter atomic.Int64

func seq() int64 { return seqCounter.Add(1) + seqBase - 1 }

func readyOrder(t *testing.T, ctx context.Context, db *sql.DB, env contracts.Environment) (orderID, correlationID string) {
	t.Helper()
	instrument := dbtest.CanonicalID("ins", int(seq()))
	if _, err := db.ExecContext(ctx, `
		INSERT INTO market.instrument (
			instrument_id, market_class, base, quote, min_quantity,
			price_increment, tick_size, order_types, trading_status)
		VALUES ($1,'crypto','BTC','USD',0.00000001,0.01,0.01,
		        ARRAY['MARKET','LIMIT']::common.order_type[],'OPEN')`, instrument); err != nil {
		t.Fatalf("insert instrument: %v", err)
	}

	orderID = dbtest.CanonicalID("ord", int(seq()))
	correlationID = dbtest.CanonicalID("cmd", int(seq()))
	riskDecisionID := dbtest.CanonicalID("rsk", int(seq()))

	if _, err := db.ExecContext(ctx, `
		INSERT INTO oms."order" (
			order_id, command_id, idempotency_scope, environment, account_id,
			strategy_id, instrument_id, venue_id, side, order_type, time_in_force,
			quantity, filled_quantity, average_fill_price, limit_price, stop_price,
			state, version_vector, risk_decision_id, risk_policy_revisions, correlation_id)
		VALUES ($1,$2,'ready',$3::common.environment,$4,
		        'str-1',$5,'ven-1','BUY','LIMIT','GTC',
		        1,0,NULL,100,NULL,
		        'RISK_PENDING',1,$6,'{}',$7)`,
		orderID, correlationID, string(env),
		dbtest.CanonicalID("acc", int(seq())),
		instrument, riskDecisionID, dbtest.CanonicalID("sbm", int(seq()))); err != nil {
		t.Fatalf("insert order: %v", err)
	}

	// Journal RISK_PENDING -> RISK_APPROVED. The state is unreachable by update
	// alone, which is the control that makes "only an approved order may be
	// submitted" checkable rather than aspirational.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO oms.order_event (
			order_event_id, order_id, sequence, from_state, to_state, event_kind,
			filled_quantity_delta, price, actor_id, correlation_id, occurred_at, occurred_at_ns)
		VALUES ($1,$2,1,'RISK_PENDING','RISK_APPROVED','RISK_APPROVED',
		        0,NULL,'fixture',$3,now(),$4)`,
		dbtest.CanonicalID("oe1", int(seq())), orderID,
		correlationID, int64(1)); err != nil {
		t.Fatalf("journal approval: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE oms."order" SET state='RISK_APPROVED', version_vector=version_vector+1
		  WHERE order_id=$1`, orderID); err != nil {
		t.Fatalf("advance order: %v", err)
	}
	return orderID, correlationID
}

// seedSubmission writes a prior submission row for an order, committed.
//
// It inserts directly rather than producing the row through Prepare, so a test can
// set up history that the OMS state machine does not let a command create. That
// matters because the rules under test are about what the command does when it
// finds history it did not write itself.
func seedSubmission(t *testing.T, ctx context.Context, db *sql.DB, orderID, outcome string, attempts int) {
	t.Helper()
	ns := dbtest.NowNs()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO execution.submission (
			submission_id, order_id, environment, venue_id, adapter_id,
			client_order_id, venue_idempotent,
			request_sent_at, request_sent_at_ns, outcome, response_received_at,
			request_digest, attempts)
		VALUES ($1,$2,'paper','venue-alpha','adapter-test-01',
		        $3,false,common.ns_to_timestamptz($4),$4,$5,common.ns_to_timestamptz($4),
		        $6,$7)`,
		dbtest.CanonicalID("sbm", int(seq())), orderID, "seed-"+orderID[4:],
		ns, outcome, execution.RequestDigest([]byte("{}")), attempts); err != nil {
		t.Fatalf("seeding a %s submission: %v", outcome, err)
	}
}

func submitIntent(t *testing.T, orderID, correlationID string, env contracts.Environment) execution.SubmitIntent {
	t.Helper()
	return execution.SubmitIntent{
		OrderID:         orderID,
		CorrelationID:   correlationID,
		ActorID:         "svc-execution-test",
		Environment:     env,
		RiskDecisionID:  dbtest.CanonicalID("rsk", int(seq())),
		VenueID:         "venue-alpha",
		AdapterID:       "adapter-test-01",
		ClientOrderID:   "cid-" + orderID[4:],
		VenueIdempotent: false,
		PolicyVersion:   dbtest.CanonicalID("pol", int(seq())),
		RequestDigest:   execution.RequestDigest([]byte(`{"side":"BUY","qty":"1"}`)),
		OccurredAt:      time.Now().UTC().Truncate(time.Microsecond),
	}
}

// newCommand builds a Command with the certification gate released.
//
// It releases deliberately rather than seeding certification rows. The tests in
// this file are about the submit lifecycle -- what commits together, what a second
// attempt requires, what an undetermined outcome blocks -- and certification is not
// their subject. Standing up a real gate here would mean every failure in this file
// had two candidate causes: the thing under test, or a missing capability row. The
// gate is tested on its own terms in submission_gate_test.go, where a refusal is
// the thing being asserted.
func newCommand(t *testing.T, db *sql.DB) *execution.Command {
	t.Helper()
	st, err := execution.NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	c, err := execution.NewCommand(db, st, audit.NewAppender(db),
		execution.ReleaseGate{}, "svc-execution", TestSigningKeyID)
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	return c
}

// The claim: one commit produces a SUBMITTING order, a PENDING submission, an
// OMS event, an audit record and an outbox row. All four are asserted, and each
// is linked to the same correlation id, because a row that merely exists is not
// evidence that it belongs to this submission.
func TestTheSubmitCommandCommitsSubmissionTransitionAuditAndOutboxTogether(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)

	prepared, err := newCommand(t, db).Prepare(ctx, submitIntent(t, orderID, corr, contracts.EnvPaper))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if prepared.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1 for a first submission", prepared.Attempts)
	}

	// The order moved.
	var state string
	dbtest.MustQueryRowDB(t, ctx, db, &state,
		`SELECT state::text FROM oms."order" WHERE order_id=$1`, orderID)
	if state != "SUBMITTING" {
		t.Fatalf("order state = %s, want SUBMITTING", state)
	}

	// The durable submission record exists and is PENDING, because no send has
	// happened yet and the record must not claim otherwise.
	var subOutcome string
	var attempts int
	if err := db.QueryRowContext(ctx, `
		SELECT outcome, attempts FROM execution.submission WHERE order_id=$1`, orderID).
		Scan(&subOutcome, &attempts); err != nil {
		t.Fatalf("reading the submission row: %v", err)
	}
	if execution.StoredOutcome(subOutcome) != execution.StoredPending {
		t.Fatalf("submission outcome = %s, want PENDING; the record exists before the send and "+
			"must not assert an outcome that has not happened", subOutcome)
	}
	if attempts != 1 {
		t.Fatalf("submission attempts = %d, want 1", attempts)
	}

	// The OMS event exists and carries the audit id.
	var auditID sql.NullString
	dbtest.MustQueryRowDB(t, ctx, db, &auditID, `
		SELECT audit_id FROM oms.order_event
		 WHERE order_event_id=$1`, prepared.OrderEventID.String())
	if !auditID.Valid {
		t.Fatal("the SUBMITTING event carries no audit_id; the state machine and its evidence " +
			"are not linked")
	}

	// The audit record exists in the hash chain.
	var recorded int
	dbtest.MustQueryRowDB(t, ctx, db, &recorded,
		`SELECT count(*) FROM audit.record WHERE audit_id=$1`, auditID.String)
	if recorded != 1 {
		t.Fatalf("audit records for the submit = %d, want 1", recorded)
	}

	// The outbox row exists -- doc 05's "always" -- and is PENDING for dispatch.
	var dispatch string
	var agg string
	if err := db.QueryRowContext(ctx, `
		SELECT dispatch_state::text, aggregate_id FROM ops.outbox WHERE event_id=$1`,
		prepared.EventID.String()).Scan(&dispatch, &agg); err != nil {
		t.Fatalf("reading the outbox row: %v", err)
	}
	if dispatch != "PENDING" {
		t.Fatalf("outbox dispatch_state = %s, want PENDING", dispatch)
	}
	if agg != orderID {
		t.Fatalf("outbox aggregate_id = %s, want %s", agg, orderID)
	}
}

// Doc 05's "always" is the sentence with no trigger behind it, so it is asserted
// in both directions: every submission has an outbox row, and the outbox row
// carries the submission the dispatcher will act on.
func TestEveryCommittedSubmissionHasAnOutboxRowNamingIt(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)

	prepared, err := newCommand(t, db).Prepare(ctx, submitIntent(t, orderID, corr, contracts.EnvPaper))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	var payload []byte
	dbtest.MustQueryRowDB(t, ctx, db, &payload, `
		SELECT payload FROM ops.outbox WHERE event_id=$1`, prepared.EventID.String())

	if len(payload) == 0 {
		t.Fatal("the outbox row has an empty payload; the dispatcher would have nothing to send")
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decoding the outbox payload: %v", err)
	}
	if decoded["submission_id"] != prepared.SubmissionID.String() {
		t.Fatalf("outbox payload submission_id = %v, want %s",
			decoded["submission_id"], prepared.SubmissionID)
	}
	if decoded["order_id"] != orderID {
		t.Fatalf("outbox payload order_id = %v, want %s", decoded["order_id"], orderID)
	}
}

// The all-or-nothing property, forced at the LAST step.
//
// By this point the command has already written, inside its transaction: the OMS
// SUBMITTING event, the order's state change, the PENDING submission row and the
// audit record. A trigger makes the outbox insert raise, so the transaction fails
// with four successful writes outstanding. All four must be gone afterwards.
//
// The alternative used first -- taking the order out of RISK_APPROVED -- was not
// reachable: the database refuses an unexplained state change, which is the very
// control that makes "only an approved order may be submitted" worth stating. The
// failure had to move later in the sequence, where the earlier writes have
// actually happened.
func TestAFailedStepRollsBackEveryEarlierWrite(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)

	// The trigger is created outside any transaction, because the command opens
	// its own on the pool and would not see an uncommitted one. It is removed
	// again on cleanup.
	if _, err := db.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION test_block_outbox_insert() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
			RAISE EXCEPTION 'test: outbox insert blocked';
		END $$`); err != nil {
		t.Fatalf("creating the blocking function: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER test_block_outbox BEFORE INSERT ON ops.outbox
		FOR EACH ROW EXECUTE FUNCTION test_block_outbox_insert()`); err != nil {
		t.Fatalf("creating the blocking trigger: %v", err)
	}
	t.Cleanup(func() {
		dropCtx := context.Background()
		_, _ = db.ExecContext(dropCtx, `DROP TRIGGER IF EXISTS test_block_outbox ON ops.outbox`)
		_, _ = db.ExecContext(dropCtx, `DROP FUNCTION IF EXISTS test_block_outbox_insert()`)
	})

	in := submitIntent(t, orderID, corr, contracts.EnvPaper)
	if _, err := newCommand(t, db).Prepare(ctx, in); err == nil {
		t.Fatal("Prepare committed even though its outbox insert was blocked")
	}

	// Nothing may remain from the failed command. Each of these would be a false
	// statement about what happened: a submission row claiming an order is in
	// flight, an audit record for an event that never durably occurred, an OMS
	// event for a transition that did not happen.
	var n int
	dbtest.MustQueryRowDB(t, ctx, db, &n,
		`SELECT count(*) FROM execution.submission WHERE order_id=$1`, orderID)
	if n != 0 {
		t.Fatalf("submission rows left behind = %d, want 0", n)
	}
	dbtest.MustQueryRowDB(t, ctx, db, &n, `
		SELECT count(*) FROM oms.order_event WHERE order_id=$1 AND event_kind='SUBMITTING'`, orderID)
	if n != 0 {
		t.Fatalf("SUBMITTING events left behind = %d, want 0", n)
	}
	dbtest.MustQueryRowDB(t, ctx, db, &n, `
		SELECT count(*) FROM audit.record WHERE target_id=$1 AND action='ORDER_SUBMIT_PREPARED'`, orderID)
	if n != 0 {
		t.Fatalf("audit records left behind = %d, want 0", n)
	}
	dbtest.MustQueryRowDB(t, ctx, db, &n, `
		SELECT count(*) FROM ops.outbox WHERE aggregate_id=$1 AND event_type='ORDER_SUBMIT_REQUESTED'`, orderID)
	if n != 0 {
		t.Fatalf("outbox rows left behind = %d, want 0", n)
	}

	var state string
	dbtest.MustQueryRowDB(t, ctx, db, &state,
		`SELECT state::text FROM oms."order" WHERE order_id=$1`, orderID)
	if state != "RISK_APPROVED" {
		t.Fatalf("order state = %s, want RISK_APPROVED; a failed submit must not advance it", state)
	}
}

// The precondition is read from the transaction, not from a caller-supplied
// slice, and it is read BEFORE anything is written.
//
// The prior submission row is inserted directly rather than produced by a first
// Prepare, because the OMS state machine has no UNKNOWN -> SUBMITTING rule: once
// an order's outcome is undetermined its only exits are OUTCOME_RESOLVED to a
// terminal state. A second Prepare for the same order is therefore unreachable
// through legal transitions, which is a finding in its own right -- it means
// submission_retry_requires_reconciliation is defence in depth against a state
// machine that already forbids the retry it guards.
func TestAPreparedSubmitRefusesWhenDurableHistoryIsUndetermined(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)

	// A submission whose send may or may not have reached the venue.
	seedSubmission(t, ctx, db, orderID, "TIMED_OUT_UNKNOWN", 1)

	// Even with reconciliation and authorisation supplied -- the strongest case a
	// caller can make -- the undetermined attempt still blocks. This is the
	// ordering that matters: naming a reconciliation case is not the same as
	// having resolved anything, and the venue may still hold that order.
	in := submitIntent(t, orderID, corr, contracts.EnvPaper)
	in.RetryAuthorized = "operator-audit-01"
	in.ReconcilCaseID = dbtest.CanonicalID("rec", int(seq()))

	_, err := newCommand(t, db).Prepare(ctx, in)
	if err == nil {
		t.Fatal("Prepare submitted an order whose prior attempt is still undetermined")
	}
	if !errors.Is(err, execution.ErrResolutionRequired) {
		t.Fatalf("error = %v, want ErrResolutionRequired", err)
	}

	// The refusal wrote nothing.
	var n int
	dbtest.MustQueryRowDB(t, ctx, db, &n,
		`SELECT count(*) FROM execution.submission WHERE order_id=$1`, orderID)
	if n != 1 {
		t.Fatalf("submission rows = %d, want 1; the refused attempt must not have been recorded", n)
	}
	dbtest.MustQueryRowDB(t, ctx, db, &n, `
		SELECT count(*) FROM oms.order_event WHERE order_id=$1 AND event_kind='SUBMITTING'`, orderID)
	if n != 0 {
		t.Fatalf("SUBMITTING events = %d, want 0; the refusal precedes every write", n)
	}
	var state string
	dbtest.MustQueryRowDB(t, ctx, db, &state,
		`SELECT state::text FROM oms."order" WHERE order_id=$1`, orderID)
	if state != "RISK_APPROVED" {
		t.Fatalf("order state = %s, want RISK_APPROVED", state)
	}
}

// A second attempt with a determined prior outcome is still refused without
// reconciliation and an authorizer. This is the blind-retry rule, and it is
// enforced by the command rather than only by the table -- so that the refusal
// names a reason instead of surfacing a bare constraint violation.
func TestASecondAttemptWithoutReconciliationIsRefusedBeforeWriting(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)

	// A prior attempt the venue definitively refused: no exposure exists, so
	// nothing is undetermined and the order could in principle be sent again.
	seedSubmission(t, ctx, db, orderID, "REJECTED", 1)

	in := submitIntent(t, orderID, corr, contracts.EnvPaper)
	in.ClientOrderID = "cid2-" + orderID[4:]

	_, err := newCommand(t, db).Prepare(ctx, in)
	if err == nil {
		t.Fatal("Prepare permitted a second attempt with no reconciliation case and no authorizer")
	}
	if !errors.Is(err, execution.ErrRetryNotAuthorized) {
		t.Fatalf("error = %v, want ErrRetryNotAuthorized", err)
	}

	var n int
	dbtest.MustQueryRowDB(t, ctx, db, &n,
		`SELECT count(*) FROM execution.submission WHERE order_id=$1`, orderID)
	if n != 1 {
		t.Fatalf("submission rows = %d, want 1; the refused retry must leave no trace of having "+
			"been attempted", n)
	}
}

// With both supplied, the second attempt is permitted and recorded as attempt 2
// carrying the reconciliation and authorizer the constraint demands.
func TestASecondAttemptWithReconciliationIsRecordedAsAttemptTwo(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)

	seedSubmission(t, ctx, db, orderID, "REJECTED", 1)

	in := submitIntent(t, orderID, corr, contracts.EnvPaper)
	in.ClientOrderID = "cid2-" + orderID[4:]
	in.RetryAuthorized = "operator-audit-01"
	in.ReconcilCaseID = dbtest.CanonicalID("rec", int(seq()))

	prepared, err := newCommand(t, db).Prepare(ctx, in)
	if err != nil {
		t.Fatalf("Prepare with reconciliation: %v", err)
	}
	if prepared.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", prepared.Attempts)
	}

	var retryBy sql.NullString
	var caseID sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT retry_authorized_by, reconciliation_case_id
		  FROM execution.submission WHERE submission_id=$1`, prepared.SubmissionID.String()).
		Scan(&retryBy, &caseID); err != nil {
		t.Fatalf("reading the retry authorisation: %v", err)
	}
	if !retryBy.Valid || retryBy.String != "operator-audit-01" {
		t.Fatalf("retry_authorized_by = %v, want operator-audit-01", retryBy)
	}
	if !caseID.Valid || caseID.String != in.ReconcilCaseID {
		t.Fatalf("reconciliation_case_id = %v, want %s", caseID, in.ReconcilCaseID)
	}
}

// The intent is validated before a transaction is opened, so a malformed command
// cannot leave a lock held or a partial write behind.
func TestAnIncompleteIntentIsRefusedWithoutWriting(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	c := newCommand(t, db)

	cases := []struct {
		name    string
		mutate  func(*execution.SubmitIntent)
		wantAny string
	}{
		{"no client order id", func(i *execution.SubmitIntent) { i.ClientOrderID = "" }, "client order id"},
		{"no request digest", func(i *execution.SubmitIntent) { i.RequestDigest = "" }, "request digest"},
		{"no risk decision", func(i *execution.SubmitIntent) { i.RiskDecisionID = "" }, "risk decision"},
		{"no actor", func(i *execution.SubmitIntent) { i.ActorID = "" }, "actor"},
		{"no venue", func(i *execution.SubmitIntent) { i.VenueID = "" }, "venue"},
		{"no time", func(i *execution.SubmitIntent) { i.OccurredAt = time.Time{} }, "occurrence time"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := submitIntent(t, orderID, corr, contracts.EnvPaper)
			tc.mutate(&in)
			if _, err := c.Prepare(ctx, in); err == nil {
				t.Fatalf("Prepare accepted an intent missing %s", tc.wantAny)
			}
		})
	}

	var n int
	dbtest.MustQueryRowDB(t, ctx, db, &n,
		`SELECT count(*) FROM execution.submission WHERE order_id=$1`, orderID)
	if n != 0 {
		t.Fatalf("submission rows = %d, want 0 after only invalid intents", n)
	}
}

// A command with no audit appender cannot be constructed. The appender is what
// supplies AppendIn, and without it the transaction would carry a mutation with no
// audit record.
//
// A command with no certification gate cannot be constructed either, and that is
// the stricter of the two. A missing appender loses evidence; a missing gate loses
// the check that stops an uncertified adapter's order being committed as though a
// send were owed.
func TestACommandWithoutAnAuditAppenderIsRefused(t *testing.T) {
	db := dbtest.Open(t)
	st, err := execution.NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	appender := audit.NewAppender(db)
	if _, err := execution.NewCommand(db, st, nil, execution.ReleaseGate{}, "svc-execution",
		TestSigningKeyID); err == nil {
		t.Fatal("NewCommand accepted a nil audit appender; the transaction would commit a mutation " +
			"with no audit record")
	}
	if _, err := execution.NewCommand(db, st, appender, nil, "svc-execution",
		TestSigningKeyID); err == nil {
		t.Fatal("NewCommand accepted a nil submission gate; an uncertified adapter's order would be " +
			"committed to SUBMITTING with a durable pending submission and nothing would stop it")
	}
	if !errors.Is(mustNilGateError(t, db, st, appender), execution.ErrNoSubmissionGate) {
		t.Fatal("a nil gate did not report ErrNoSubmissionGate")
	}
	if _, err := execution.NewCommand(db, st, appender, execution.ReleaseGate{}, "",
		TestSigningKeyID); err == nil {
		t.Fatal("NewCommand accepted an empty producer identity; its outbox rows would be " +
			"unattributable")
	}
	// The empty signing key is the one that had been accepted in practice. It is
	// NOT NULL in the schema, so it stored silently, and this path was writing it
	// on every submission, every resolution and every venue answer. See
	// db/migrations/0023_audit_signing_key_required.sql for the 1298 historical
	// records that carried it.
	if _, err := execution.NewCommand(db, st, appender, execution.ReleaseGate{},
		"svc-execution", ""); !errors.Is(err, execution.ErrNoSigningKey) {
		t.Fatalf("NewCommand with an empty signing key returned %v, want ErrNoSigningKey; an audit "+
			"record naming no key is evidence nothing attests to", err)
	}
}

func mustNilGateError(t *testing.T, db *sql.DB, st *execution.Store, a *audit.Appender) error {
	t.Helper()
	_, err := execution.NewCommand(db, st, a, nil, "svc-execution", TestSigningKeyID)
	if err == nil {
		t.Fatal("NewCommand accepted a nil submission gate")
	}
	return err
}

// Every audit record this component writes must name a key that attests to it.
//
// The submission path is the one that matters most here. ORDER_SUBMIT_PREPARED is
// the record that says an order was committed to SUBMITTING with a durable PENDING
// submission, and for the whole life of this code it named no signing key -- because
// audit.record.signing_key_id is NOT NULL, so the empty string stored without
// complaint, and because nothing ever revises that column. The column's comment
// described a batch-closing key written at checkpoint time, which is why the
// placeholder looked harmless; no such back-fill exists.
//
// Prepare, Resolve and ApplyVenueAnswer are the three writers, and each passes
// c.signingKey. The constructor refusal is covered by
// TestACommandWithoutAnAuditAppenderIsRefused; this asserts the value actually
// reaches the row, because a constructor can be correct and the record still empty.
func TestTheSubmissionAuditRecordNamesTheConfiguredSigningKey(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)

	prepared, err := newCommand(t, db).Prepare(ctx, submitIntent(t, orderID, corr, contracts.EnvPaper))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.SubmissionID.String() == "" {
		t.Fatal("Prepare returned no submission id, so the audit record asserted below would be " +
			"about nothing")
	}

	var key string
	err = db.QueryRowContext(ctx, `
		SELECT signing_key_id
		  FROM audit.record
		 WHERE target_type = 'ORDER'
		   AND target_id = $1
		   AND action = 'ORDER_SUBMIT_PREPARED'`, orderID).Scan(&key)
	if err != nil {
		t.Fatalf("reading the submission's audit record: %v", err)
	}
	if key != TestSigningKeyID {
		t.Errorf("ORDER_SUBMIT_PREPARED for order %s names signing key %q, want %q. A record "+
			"asserting an order was sent, attributed to no key, cannot be defended later",
			orderID, key, TestSigningKeyID)
	}

	// The stronger form of the claim: no audit record this component wrote for
	// this order names an empty key. Querying by order rather than by one action
	// catches a second writer that was threaded and a second that was not --
	// execution.submission carries no audit_id, so the order is the only linkage
	// the schema actually offers.
	if n := count(t, db, `
		SELECT count(*) FROM audit.record
		 WHERE target_type = 'ORDER'
		   AND target_id = $1
		   AND btrim(signing_key_id) = ''`, orderID); n != 0 {
		t.Errorf("audit records naming no key for order %s = %d, want 0; the schema permits an "+
			"empty signing_key_id, so only this assertion would notice", orderID, n)
	}
	if n := count(t, db, `
		SELECT count(*) FROM audit.record
		 WHERE target_type = 'ORDER'
		   AND target_id = $1
		   AND action = 'ORDER_SUBMIT_PREPARED'
		   AND signing_key_id = $2`, orderID, TestSigningKeyID); n != 1 {
		t.Errorf("the submission's audit record naming %q = %d, want exactly 1",
			TestSigningKeyID, n)
	}
}

// Doc 05 states the pairing as an invariant and, until migration 0024, nothing
// enforced it: no trigger on oms."order" referenced ops.outbox, so the rule held
// only because Command.Prepare happens to write both rows. That is a statement about
// one function rather than about the schema, and the failure it leaves open is the
// one ErrNoSubmissionGate exists to prevent by other means.
//
// An order committed in SUBMITTING claims a send is owed. With no outbox row,
// nothing will ever read that claim: the dispatcher finds no row, and the order sits
// looking like one genuinely in flight at a venue that has not answered.
//
// The control is a DEFERRABLE INITIALLY DEFERRED constraint trigger, so it is
// evaluated at COMMIT rather than at the statement. That is not a detail of the
// implementation: Prepare writes the order row and the outbox row in one
// transaction, in that order, so an immediate trigger would refuse every correct
// submission. The test therefore settles the constraints rather than expecting the
// UPDATE itself to fail, which is what ExpectDeferredRejected exists for.
func TestAnOrderCannotReachSubmittingWithNoOutboxRow(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustInsertInstrument(t, ctx, tx, dbtest.CanonicalID("ins", 960))
		order := MustOrderAt(t, ctx, tx, dbtest.CanonicalID("ins", 960), 961, "RISK_APPROVED")

		// The transition to SUBMITTING is itself legal and is accepted. The control
		// is not a state-machine rule; it is a rule about what must accompany the
		// transition, so nothing refuses here.
		MustTransition(t, ctx, tx, order, "SUBMITTING", "SUBMITTING", 962)

		err := dbtest.ExpectDeferredRejected(t, ctx, tx, "no ops.outbox row")
		if err == nil {
			t.Fatal("an order was committed in SUBMITTING with no ops.outbox row for it. Doc 05 " +
				"requires every committed state mutation to have a durable outbox record, and " +
				"without one the order claims a send is owed that nothing will ever read")
		}
	})
}

// The control is only load-bearing if the transaction that DOES the pairing is accepted,
// so the permissive case is asserted rather than assumed: a guard that refused every
// submission would pass the test above.
//
// This goes through Command.Prepare rather than through the fixture. An earlier version
// used MustOrderAt and then counted the outbox row that MustOrderAt had itself just
// written, which could only ever report one -- it asserted that the fixture had done what
// the fixture does, and said nothing about whether the guard would admit a real
// submission. Prepare is the production path, and its returning nil IS the permissive
// assertion: the guard is DEFERRED, so if it refused the pairing Prepare's COMMIT would
// fail and Prepare would return the error.
func TestAnOrderMayReachSubmittingWithItsOutboxRow(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	prepared, err := newCommand(t, db).Prepare(ctx, submitIntent(t, orderID, corr, contracts.EnvPaper))
	if err != nil {
		t.Fatalf("Prepare was refused for an order that paired with its outbox row: %v", err)
	}
	// Prepare commits, so this order and its outbox row outlive the test unless retired.
	t.Cleanup(func() { retireSubmittedOrder(t, ctx, db, orderID) })

	var state string
	if err := db.QueryRowContext(ctx,
		`SELECT state::text FROM oms."order" WHERE order_id = $1`, orderID).Scan(&state); err != nil {
		t.Fatalf("reading the order's state: %v", err)
	}
	if state != "SUBMITTING" {
		t.Errorf("order state = %q, want SUBMITTING -- the transition did not take effect", state)
	}

	var aggregateID string
	if err := db.QueryRowContext(ctx,
		`SELECT aggregate_id FROM ops.outbox WHERE event_id = $1`,
		prepared.EventID.String()).Scan(&aggregateID); err != nil {
		t.Fatalf("reading the prepared outbox row: %v", err)
	}
	if aggregateID != orderID {
		t.Errorf("outbox aggregate_id = %q, want the order id %q. The 0025 guard matches on "+
			"aggregate_id alone, so a row that names the order some other way would not "+
			"satisfy it", aggregateID, orderID)
	}
}

// The delete half of the pairing.
//
// Migration 0024 enforces the pairing when an order ENTERS SUBMITTING, and its trigger
// is on oms."order". Nothing about it fires when the outbox row is removed instead, so an
// order could be committed in SUBMITTING, have its outbox row deleted by a later
// transaction, and -- because every subsequent update leaves the state unchanged and so
// returns early from the guard -- never be checked again. The result is exactly the state
// 0024 exists to forbid, reached without ever committing a bad order row.
//
// 0024 justified itself by asserting that "ops.outbox rows are never deleted -- nothing in
// this repository or its migrations issues a DELETE against them". That was false:
// dbtest/dispatch_test.go did exactly that. Migration 0026 adds the missing control and
// this is the test that says the control is real.
//
// Both halves are asserted. A guard that refused every delete would also pass the first
// half, and making ops.outbox append-only would be the wrong fix for this defect: it is
// an event log, and pruning it is legitimate once the obligation it recorded is settled.
func TestTheOutboxRowOfASubmittingOrderCannotBeDeleted(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	orderID, corr := readyOrder(t, ctx, db, contracts.EnvPaper)
	prepared, err := newCommand(t, db).Prepare(ctx, submitIntent(t, orderID, corr, contracts.EnvPaper))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	eventID := prepared.EventID.String()
	// Committed, so the residue has to be retired; see retireSubmittedOrder.
	t.Cleanup(func() { retireSubmittedOrder(t, ctx, db, orderID) })

	// Committed, not rolled back. The hole this closes needs a SECOND transaction acting
	// on durable state, so the delete below must run outside the transaction that wrote
	// the row -- which is why this asserts through the pool rather than through the
	// transaction-based dbtest.ExpectRejected.
	_, err = db.ExecContext(ctx, `DELETE FROM ops.outbox WHERE event_id = $1`, eventID)
	if err == nil {
		t.Fatal("the outbox row naming an order in SUBMITTING was deleted. That row is the " +
			"durable record that a send is owed; without it the order claims an unfulfilled " +
			"send and the dispatcher will never find anything to read")
	}
	if !strings.Contains(err.Error(), "committed in SUBMITTING") {
		t.Errorf("the delete was refused, but not by the guard under test: %v", err)
	}
}

// The guard protects the pairing, not the table. Once the order has left SUBMITTING the
// row prunes normally, which is what stops this control from quietly becoming
// "ops.outbox is append-only".
func TestAnOutboxRowIsPrunableOnceItsOrderHasLeftSubmitting(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	// An order whose outcome was never established, and then resolved to a venue outcome.
	//
	// The order is moved out of SUBMITTING by the real lifecycle, not by writing the state
	// directly. A direct UPDATE is refused, and correctly so: the OMS requires a journal
	// event declaring every transition, and Resolve additionally refuses an order that is
	// still SUBMITTING because only an order whose outcome was never determined can be
	// resolved from the venue record. Both refusals are why this half uses the real path.
	orderID, _ := unknownOrder(t, ctx, db)
	r := &fakeResolver{outcome: execution.AcceptedOutcome, venueOrderID: "venue-ref-0026"}
	if _, err := newCommand(t, db).Resolve(ctx, orderID, reconcilerID, r); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	var eventID string
	if err := db.QueryRowContext(ctx,
		`SELECT event_id FROM ops.outbox WHERE aggregate_id = $1 ORDER BY occurred_at_ns DESC LIMIT 1`,
		orderID).Scan(&eventID); err != nil {
		t.Fatalf("reading the order's outbox rows: %v", err)
	}

	res, err := db.ExecContext(ctx, `DELETE FROM ops.outbox WHERE event_id = $1`, eventID)
	if err != nil {
		t.Fatalf("deleting the outbox row of an order no longer in SUBMITTING: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("deleted %d outbox rows, want 1 -- the guard protects the SUBMITTING pairing, not the table", n)
	}
}

// retireSubmittedOrder drives a committed SUBMITTING order to a terminal state and prunes
// its outbox rows.
//
// Two of these tests must COMMIT rather than roll back, because the delete-guard defect
// they cover required a second transaction acting on durable state. Committing leaves
// residue: an order sitting in SUBMITTING whose outbox row is still PENDING. The
// migration 0026 guard correctly refuses to let a test delete that row, so it survives
// cleanDeliveryState, and a later dispatcher test that calls DispatchOnce claims it and
// counts one more row than it expected.
//
// The residue is the tests' fault, not the guard's, so it is cleaned up here rather than
// by weakening either. The retirement goes through oms.state_transition_rule like every
// other transition: SUBMITTING -> REJECTED is legal only via REJECTED_BY_VENUE, and the
// journal row is written before the state change because oms.guard_order_update requires
// every transition to be declared. Writing the event kind by guess is what produced an
// earlier version of this helper that the rule table refused.
func retireSubmittedOrder(t *testing.T, ctx context.Context, db *sql.DB, orderID string) {
	t.Helper()
	var next int64
	if err := db.QueryRowContext(ctx,
		`SELECT coalesce(max(sequence),0)+1 FROM oms.order_event WHERE order_id=$1`, orderID).
		Scan(&next); err != nil {
		t.Errorf("reading the next journal sequence while retiring %s: %v", orderID, err)
		return
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO oms.order_event (
			order_event_id, order_id, sequence, from_state, to_state, event_kind,
			filled_quantity_delta, price, actor_id, reason, correlation_id,
			occurred_at, occurred_at_ns)
		VALUES ($1,$2,$3,'SUBMITTING','REJECTED','REJECTED_BY_VENUE',
		        0,NULL,'test-cleanup','retiring an order committed by a test',$4,now(),$5)`,
		dbtest.CanonicalID("oe1", int(seq())), orderID, next,
		dbtest.CanonicalID("cor", int(seq())), next); err != nil {
		t.Errorf("journalling the retirement of %s: %v", orderID, err)
		return
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE oms."order" SET state = 'REJECTED', updated_at = now() WHERE order_id = $1`,
		orderID); err != nil {
		t.Errorf("retiring %s: %v", orderID, err)
		return
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM ops.outbox WHERE aggregate_id = $1`, orderID); err != nil {
		t.Errorf("pruning the outbox rows of retired order %s: %v", orderID, err)
	}
}
