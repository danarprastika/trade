package dbtest_test

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/execution"
)

// The durable submission record against the real table.
//
// These are integration tests rather than unit tests because the behaviour under
// test is a disagreement between two vocabularies. Go's Outcome has three members
// and execution.submission enforces six, the names do not correspond, and the
// no-retry invariant is only correct if the mapping in both directions is right.
// A test double would agree with the mapping by construction, which is the one
// thing that needs proving is not the case.

// store returns a Store over the test transaction, so a submission row written
// here can be discarded and the fixture rows in oms.order are never touched.
func store(t *testing.T, tx *sql.Tx) *execution.Store {
	t.Helper()
	s, err := execution.NewStore(tx)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// submissionOrder inserts an order at SUBMITTING and returns its id.
//
// It cannot be inserted there directly. execution.submission requires an existing
// order, and oms.order refuses any state beyond RISK_PENDING on insert -- every
// later state is reachable only through oms.order_event. So the order enters at
// RISK_PENDING and is journalled forward, which is the same path production code
// takes and keeps the fixture honest.
//
// The instrument is inserted rather than referenced because a trigger refuses an
// order whose instrument does not list its order type and side. A submission
// record cannot exist without an order, so the whole fixture hangs off this.
func submissionOrder(t *testing.T, ctx context.Context, tx *sql.Tx, env contracts.Environment) string {
	t.Helper()
	orderID := dbtest.CanonicalID("ord", int(dbtest.UniqueSequence(1)))
	riskDecisionID := dbtest.CanonicalID("rsk", int(dbtest.UniqueSequence(4)))
	correlationID := dbtest.CanonicalID("sbm", int(dbtest.UniqueSequence(9)))
	instrument := dbtest.CanonicalID("ins", int(dbtest.UniqueSequence(3)))

	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO market.instrument (
			instrument_id, market_class, base, quote,
			min_quantity, price_increment, tick_size, order_types, trading_status)
		VALUES ($1,'crypto','BTC','USD',
			0.00000001,0.01,0.01,ARRAY['MARKET','LIMIT']::common.order_type[],
			'OPEN')`, instrument)

	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO oms."order" (
			order_id, command_id, idempotency_scope, environment, account_id,
			strategy_id, instrument_id, venue_id, side, order_type, time_in_force,
			quantity, filled_quantity, average_fill_price, limit_price, stop_price,
			state, version_vector, risk_decision_id,
			risk_policy_revisions, correlation_id)
		VALUES ($1, $2, 'fixture', $3::common.environment, $4,
			'str-1', $5, 'ven-1', 'BUY', 'LIMIT', 'GTC',
			1, 0, NULL, 100, NULL,
			'RISK_PENDING', 1, $6,
			'{}', $7)`,
		orderID, dbtest.CanonicalID("cmd", int(dbtest.UniqueSequence(8))), string(env),
		dbtest.CanonicalID("acc", int(dbtest.UniqueSequence(2))),
		instrument, riskDecisionID, correlationID)

	steps := []struct{ from, to, kind string }{
		{"RISK_PENDING", "RISK_APPROVED", "RISK_APPROVED"},
		{"RISK_APPROVED", "SUBMITTING", "SUBMITTING"},
	}
	for i, step := range steps {
		dbtest.MustExec(t, ctx, tx, `
			INSERT INTO oms.order_event (
				order_event_id, order_id, sequence, from_state, to_state, event_kind,
				filled_quantity_delta, price, actor_id, correlation_id,
				occurred_at, occurred_at_ns)
			VALUES ($1,$2,$3,$4,$5,$6,'0',NULL,'fixture-actor',$7,now(),$8)`,
			dbtest.CanonicalID("oe1", int(dbtest.UniqueSequence(10+i))), orderID, i+1,
			step.from, step.to, step.kind, correlationID, int64(i+1))

		dbtest.MustExec(t, ctx, tx, `
			UPDATE oms."order" SET state = $2::oms.order_state, version_vector = version_vector + 1
			 WHERE order_id = $1`, orderID, step.to)
	}
	return orderID
}

func pendingRecord(t *testing.T, orderID, venue, clientOrderID string, env contracts.Environment) execution.Record {
	t.Helper()
	now := time.Now().UTC()
	ns := now.UnixNano()
	return execution.Record{
		OrderID:         orderID,
		Environment:     env,
		VenueID:         venue,
		AdapterID:       "adapter-test-01",
		ClientOrderID:   clientOrderID,
		VenueIdempotent: false,
		RequestSentAt:   now,
		RequestSentAtNs: ns,
		RequestDigest:   execution.RequestDigest([]byte(`{"side":"BUY","qty":"1"}`)),
		Outcome:         execution.StoredPending,
		Attempts:        1,
	}
}

// The store's StoredOutcome vocabulary must equal what the live CHECK constraint
// allows, read from pg_constraint rather than from the migration.
//
// This is the assertion the whole mapping rests on. The two vocabularies have
// different cardinalities and non-corresponding names -- Go has three outcomes,
// the column has six, and UNDETERMINED is stored as TIMED_OUT_UNKNOWN -- so a
// mapping written against a Go constant alone would keep passing while the
// database disagreed, which is how an outcome could be written that no constraint
// accepts.
func TestStoredOutcomeVocabularyMatchesTheLiveCheckConstraint(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	// What the table actually accepts, parsed out of the constraint definition.
	var def string
	db.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		  FROM pg_constraint c
		  JOIN pg_class t ON t.oid = c.conrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace
		 WHERE n.nspname = 'execution'
		   AND t.relname = 'submission'
		   AND c.conname = 'submission_outcome_valid'`).Scan(&def)

	if def == "" {
		t.Fatal("submission_outcome_valid does not exist on execution.submission; the " +
			"vocabulary this test compares against has moved")
	}

	vocabRe := regexp.MustCompile(`ARRAY\[([^\]]*)\]`)
	sub := vocabRe.FindStringSubmatch(def)
	if len(sub) < 2 {
		t.Fatalf("submission_outcome_valid has no value list to compare against: %s", def)
	}
	var allowed []string
	for _, v := range strings.Split(sub[1], ",") {
		v = strings.TrimSpace(v)
		if i := strings.Index(v, "::"); i >= 0 {
			v = v[:i]
		}
		allowed = append(allowed, strings.Trim(strings.TrimSpace(v), "'"))
	}

	// Everything the store can write.
	storeSide := []execution.StoredOutcome{
		execution.StoredPending, execution.StoredAck, execution.StoredRejected,
		execution.StoredUnknown, execution.StoredFilled, execution.StoredCancelled,
	}

	allowedSet := map[string]bool{}
	for _, v := range allowed {
		allowedSet[v] = true
	}
	storeSet := map[execution.StoredOutcome]bool{}
	for _, v := range storeSide {
		storeSet[v] = true
	}

	var missing []string
	for _, v := range storeSide {
		if !allowedSet[string(v)] {
			missing = append(missing, string(v))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the store can write outcome(s) %v that execution.submission rejects; allowed: %v",
			missing, allowed)
	}

	// The reverse direction matters too: a value the constraint permits but the
	// store cannot name is a value a reader cannot interpret, and History refuses
	// rather than guessing. Every such value must be named by storedFor or
	// outcomeFor deliberately.
	for _, v := range allowed {
		if !storeSet[execution.StoredOutcome(v)] {
			t.Fatalf("execution.submission accepts outcome %q but the store has no constant for it; "+
				"History would refuse to read a row the database considers valid", v)
		}
	}
}

// A submission recorded before the send must exist as PENDING, and must carry the
// values that prove it was recorded rather than reconstructed.
func TestTheDurableRecordExistsBeforeTheSendAndReadsBackAsPending(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		orderID := submissionOrder(t, ctx, tx, contracts.EnvPaper)
		rec := pendingRecord(t, orderID, "venue-alpha", "cid-"+orderID[4:], contracts.EnvPaper)

		if err := store(t, tx).RecordAttempt(ctx, rec); err != nil {
			t.Fatalf("RecordAttempt: %v", err)
		}

		var (
			outcome, digest, cid string
			attempts             int
		)
		if err := tx.QueryRowContext(ctx, `
			SELECT outcome, request_digest, client_order_id, attempts
			  FROM execution.submission WHERE order_id = $1`, orderID).
			Scan(&outcome, &digest, &cid, &attempts); err != nil {
			t.Fatalf("reading back the submission row: %v", err)
		}
		if execution.StoredOutcome(outcome) != execution.StoredPending {
			t.Fatalf("outcome = %q, want PENDING; a record written before the send must not "+
				"claim any outcome", outcome)
		}
		if digest != rec.RequestDigest {
			t.Fatalf("request_digest = %q, want %q", digest, rec.RequestDigest)
		}
		if cid != rec.ClientOrderID {
			t.Fatalf("client_order_id = %q, want %q", cid, rec.ClientOrderID)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1", attempts)
		}

		// A PENDING record is undetermined, so the durable precondition must
		// report one. This is the case that a caller-assembled slice would have
		// to be told about, and that a slice omitting the row would get wrong.
		u, err := store(t, tx).UndeterminedDurable(ctx, orderID)
		if err != nil {
			t.Fatalf("UndeterminedDurable: %v", err)
		}
		if u == nil {
			t.Fatal("a freshly written PENDING submission reports no undetermined attempt; " +
				"that would permit an immediate second send while the first is in flight")
		}
	})
}

// The load-bearing case: a timed-out submission reads back as undetermined and
// therefore blocks. doc 16 §4 and the table's own comment forbid recording a
// timeout as REJECTED, and this is where that would have to happen.
func TestATimedOutSubmissionIsUndeterminedAndBlocksTheNextSend(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		orderID := submissionOrder(t, ctx, tx, contracts.EnvPaper)
		rec := pendingRecord(t, orderID, "venue-alpha", "cid-"+orderID[4:], contracts.EnvPaper)
		s := store(t, tx)
		if err := s.RecordAttempt(ctx, rec); err != nil {
			t.Fatalf("RecordAttempt: %v", err)
		}

		// The send happens. Nothing came back.
		//
		// The identifier is read back rather than minted here: RecordAttempt
		// mints its own when the caller supplies none, so a freshly generated ID
		// would name a row that does not exist and the update would match nothing.
		var stored string
		dbtest.MustQueryRow(t, ctx, tx, &stored,
			`SELECT submission_id FROM execution.submission WHERE order_id = $1`, orderID)
		subID, err := contracts.ParseID(stored)
		if err != nil {
			t.Fatalf("ParseID(%s): %v", stored, err)
		}
		if subID.Entity != contracts.EntitySubmission {
			t.Fatalf("stored submission id %s has entity %s, want sbm; this is the prefix that "+
				"was missing from contracts.EntityTypes() until this stage", stored, subID.Entity)
		}

		if err := s.Outcome(ctx, subID, execution.UndeterminedOutcome,
			"", "", time.Now().UTC(), time.Now().UTC().UnixNano()); err != nil {
			t.Fatalf("Outcome: %v", err)
		}

		var got string
		dbtest.MustQueryRow(t, ctx, tx, &got,
			`SELECT outcome FROM execution.submission WHERE submission_id = $1`, subID.String())
		if execution.StoredOutcome(got) != execution.StoredUnknown {
			t.Fatalf("outcome = %q, want TIMED_OUT_UNKNOWN. Recording a timeout as REJECTED tells "+
				"the database durably that no exposure exists, which is the one inference doc 16 "+
				"§4 forbids", got)
		}

		u, err := s.UndeterminedDurable(ctx, orderID)
		if err != nil {
			t.Fatalf("UndeterminedDurable: %v", err)
		}
		if u == nil {
			t.Fatal("a TIMED_OUT_UNKNOWN submission does not block; invariant 3 is not enforced " +
				"against durable state")
		}

		// And the Submitter, given the durable history, refuses.
		history, err := s.History(ctx, orderID)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if execution.UndeterminedPrior(history) == nil {
			t.Fatal("History returned no undetermined attempt for a timed-out submission")
		}
	})
}

// A resolved timeout does not block: that is the whole point of Resolve, and a
// test that only checked the blocking case would pass with resolution broken.
func TestAResolvedTimeoutStopsBlocking(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		orderID := submissionOrder(t, ctx, tx, contracts.EnvPaper)
		s := store(t, tx)
		if err := s.RecordAttempt(ctx, pendingRecord(t, orderID, "venue-alpha",
			"cid-"+orderID[4:], contracts.EnvPaper)); err != nil {
			t.Fatalf("RecordAttempt: %v", err)
		}
		var stored string
		dbtest.MustQueryRow(t, ctx, tx, &stored,
			`SELECT submission_id FROM execution.submission WHERE order_id = $1`, orderID)
		subID, err := contracts.ParseID(stored)
		if err != nil {
			t.Fatalf("ParseID: %v", err)
		}

		now := time.Now().UTC()
		if err := s.Outcome(ctx, subID, execution.UndeterminedOutcome, "", "", now, now.UnixNano()); err != nil {
			t.Fatalf("recording the timeout: %v", err)
		}
		if err := s.Outcome(ctx, subID, execution.AcceptedOutcome,
			"venue-ref-7781", execution.RequestDigest([]byte(`{"status":"ok"}`)), now, now.UnixNano()); err != nil {
			t.Fatalf("recording the resolution: %v", err)
		}

		u, err := s.UndeterminedDurable(ctx, orderID)
		if err != nil {
			t.Fatalf("UndeterminedDurable: %v", err)
		}
		if u != nil {
			t.Fatal("a submission resolved to ACCEPTED still blocks; a recovered order could never " +
				"be resubmitted")
		}

		var vref sql.NullString
		dbtest.MustQueryRow(t, ctx, tx, &vref, `
			SELECT venue_order_ref FROM execution.submission WHERE submission_id = $1`, subID.String())
		if !vref.Valid || vref.String != "venue-ref-7781" {
			t.Fatalf("venue_order_ref = %v, want venue-ref-7781", vref)
		}
	})
}

// A blind retry is refused in Go before it reaches the database, and refused by
// the database too. Both are asserted, because either alone is a weaker control:
// the Go check could be deleted, and the constraint alone produces a bare
// violation with no reason attached.
func TestABlindRetryIsRefusedByGoAndByTheDatabase(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		orderID := submissionOrder(t, ctx, tx, contracts.EnvPaper)
		s := store(t, tx)

		retry := pendingRecord(t, orderID, "venue-alpha", "cid2-"+orderID[4:], contracts.EnvPaper)
		retry.Attempts = 2
		retry.Outcome = execution.StoredPending

		err := s.RecordAttempt(ctx, retry)
		if err == nil {
			t.Fatal("RecordAttempt accepted attempts=2 with no reconciliation case and no " +
				"authorizer; that is a blind exposure-increasing retry")
		}
		if !errorsIs(err, execution.ErrRetryNotAuthorized) {
			t.Fatalf("error = %v, want ErrRetryNotAuthorized", err)
		}

		// The same insert written directly, so the database constraint is proven
		// independently of the Go check.
		dbtest.ExpectRejectedBecause(t, ctx, tx, "submission_retry_requires_reconciliation", `
			INSERT INTO execution.submission (
				submission_id, order_id, environment, venue_id, adapter_id,
				client_order_id, request_sent_at, request_sent_at_ns,
				request_digest, attempts)
			VALUES ($1, $2, 'paper', 'venue-alpha', 'adapter-test-01',
			        $3, common.ns_to_timestamptz($4), $4,
			        $5, 2)`,
			dbtest.CanonicalID("sbm", int(dbtest.UniqueSequence(5))), orderID,
			"cid3-"+orderID[4:], dbtest.NowNs(),
			execution.RequestDigest([]byte("{}")))

		// With both fields present the insert is permitted, so the constraint is
		// gating on reconciliation rather than on attempts alone.
		dbtest.MustExec(t, ctx, tx, `
			INSERT INTO execution.submission (
				submission_id, order_id, environment, venue_id, adapter_id,
				client_order_id, request_sent_at, request_sent_at_ns,
				request_digest, attempts, retry_authorized_by, reconciliation_case_id)
			VALUES ($1, $2, 'paper', 'venue-alpha', 'adapter-test-01',
			        $3, common.ns_to_timestamptz($4), $4,
			        $5, 2, $6, $7)`,
			dbtest.CanonicalID("sbm", int(dbtest.UniqueSequence(6))), orderID,
			"cid4-"+orderID[4:], dbtest.NowNs(),
			execution.RequestDigest([]byte("{}")),
			"operator-audit-01", dbtest.CanonicalID("rec", int(dbtest.UniqueSequence(7))))
	})
}

// A stored outcome this code cannot interpret refuses the read rather than being
// skipped or assumed.
//
// The guard is unreachable through ordinary writes, because
// submission_outcome_valid prevents such a row from existing. That is exactly why
// it needs a test: an unreachable branch is indistinguishable from a deleted one.
// The constraint is dropped inside the transaction, which Resettable rolls back,
// so the row can be forced into existence and the refusal proven.
func TestAnUninterpretableStoredOutcomeRefusesTheRead(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		dbtest.MustExec(t, ctx, tx,
			`ALTER TABLE execution.submission DROP CONSTRAINT submission_outcome_valid`)

		orderID := submissionOrder(t, ctx, tx, contracts.EnvPaper)
		dbtest.MustExec(t, ctx, tx, `
			INSERT INTO execution.submission (
				submission_id, order_id, environment, venue_id, adapter_id,
				client_order_id, request_sent_at, request_sent_at_ns,
				response_received_at, request_digest, attempts, outcome)
			VALUES ($1, $2, 'paper', 'venue-alpha', 'adapter-test-01',
			        $3, common.ns_to_timestamptz($4), $4,
			        common.ns_to_timestamptz($4), $5, 1, 'PARTIALLY_FILLED')`,
			dbtest.CanonicalID("sbm", int(dbtest.UniqueSequence(20))), orderID,
			"cid-unknown-"+orderID[4:], dbtest.NowNs(),
			execution.RequestDigest([]byte("{}")))

		_, err := store(t, tx).History(ctx, orderID)
		if err == nil {
			t.Fatal("History accepted outcome PARTIALLY_FILLED, which is not in the vocabulary; " +
				"skipping it would drop an attempt whose exposure is unaccounted for")
		}
		if !errorsIs(err, execution.ErrHistoryUnreadable) {
			t.Fatalf("error = %v, want ErrHistoryUnreadable", err)
		}
	})
}

// An unreadable history refuses. This is distinct from an empty one: an order
// that has never been submitted may be sent, an order whose history could not be
// read may not, and a caller-assembled slice cannot tell those apart.
func TestAnUnreadableHistoryRefusesRatherThanReportingNone(t *testing.T) {
	// Built outside Resettable because the handle under test is deliberately
	// broken; there is no transaction to discard.
	broken, err := sql.Open("pgx", dbtest.TestDatabaseURL())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := broken.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err := execution.NewStore(broken)
	if err != nil {
		t.Fatalf("NewStore over a closed handle: %v", err)
	}

	history, err := s.History(context.Background(), dbtest.CanonicalID("ord", 1))
	if err == nil {
		t.Fatalf("History on a closed handle returned no error and history %v; an unreadable "+
			"history must not look like an empty one", history)
	}
	if !errorsIs(err, execution.ErrHistoryUnreadable) {
		t.Fatalf("error = %v, want ErrHistoryUnreadable", err)
	}

	// The precondition has the same obligation, and returning (nil, nil) from it
	// would read as "nothing unresolved" and permit a send.
	u, err := s.UndeterminedDurable(context.Background(), dbtest.CanonicalID("ord", 1))
	if err == nil {
		t.Fatal("UndeterminedDurable on a closed handle returned no error; an unestablished " +
			"history is not an established-clear one")
	}
	if u != nil {
		t.Fatalf("UndeterminedDurable returned attempt %+v alongside error %v", u, err)
	}
}

// errorsIs is errors.Is, spelled out so the import list stays minimal and the
// intent at each call site is obvious.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
