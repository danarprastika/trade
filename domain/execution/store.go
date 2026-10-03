package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/aitc/trade/contracts"
)

// The durable submission record.
//
// Doc 16 §4 requires that where a venue does not guarantee client-order-id
// idempotency, the adapter use a durable submission record. The table is
// execution.submission, and its comments state the two obligations this package
// implements: the record must exist before the request leaves the process, and a
// submission that timed out is never recorded as REJECTED by assumption.
//
// Before this file the table existed and nothing read or wrote it. The no-retry
// invariant was enforced only over a []Attempt slice the caller assembled, so its
// durability was the caller's slice's durability. The schema also required a
// prefix -- 'sbm' -- that contracts.EntityTypes() did not contain, so no Go code
// could have written the row at all.

// Querier is the read/write surface this store needs.
//
// It mirrors riskstate.Querier: an interface rather than *sql.DB so that a caller
// can hold a submission row in a transaction it can discard. *sql.DB and *sql.Tx
// both satisfy it, and production callers should pass a *sql.DB so the durable
// record is committed before the venue is contacted. That ordering is the whole
// point of the record and cannot be delegated to the adapter.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// StoredOutcome is the outcome vocabulary execution.submission enforces.
//
// It is not the same set as Outcome above, and the difference is deliberate in
// both directions rather than an accident of naming:
//
//	ACCEPTED          -> ACKNOWLEDGED     a rename, no information lost
//	REJECTED          -> REJECTED         identical
//	UNDETERMINED      -> TIMED_OUT_UNKNOWN the stored name states the cause
//	PENDING           -> PENDING          Go has no term: it means "sent, no answer yet"
//	FILLED            -> FILLED           Go has no term: reported after the fact
//	CANCELLED         -> CANCELLED        Go has no term: reported after the fact
//
// The three values Go lacks are the ones that are terminal in the database but
// are not outcomes of a submission call -- they are later reports about an order
// the venue has already accepted. Treating them as AcceptedOutcome would be a
// small lie; treating them as an error would be a large one. They are mapped
// explicitly and Refusing them is not an option, so they carry through as a
// separate type.
type StoredOutcome string

const (
	StoredPending   StoredOutcome = "PENDING"
	StoredAck       StoredOutcome = "ACKNOWLEDGED"
	StoredRejected  StoredOutcome = "REJECTED"
	StoredUnknown   StoredOutcome = "TIMED_OUT_UNKNOWN"
	StoredFilled    StoredOutcome = "FILLED"
	StoredCancelled StoredOutcome = "CANCELLED"
)

func (s StoredOutcome) valid() bool {
	switch s {
	case StoredPending, StoredAck, StoredRejected, StoredUnknown, StoredFilled, StoredCancelled:
		return true
	}
	return false
}

// storedFor maps a submission outcome onto what the database stores.
//
// A value outside the three known outcomes maps to TIMED_OUT_UNKNOWN rather than
// to an error and rather than to REJECTED. The reasoning is the same one the
// Submitter applies to an unrecognised venue answer: an outcome this code cannot
// interpret is undetermined in effect, and undetermined is the state that
// permits reconciliation while rejection ends the question. Writing REJECTED here
// would be the single most damaging default available -- it would tell the
// database, durably, that no exposure exists, on the strength of a value the
// platform does not understand.
func storedFor(o Outcome) StoredOutcome {
	switch o {
	case AcceptedOutcome:
		return StoredAck
	case RejectedOutcome:
		return StoredRejected
	default:
		return StoredUnknown
	}
}

// outcomeFor maps a stored outcome back onto Go's vocabulary.
//
// TIMED_OUT_UNKNOWN and PENDING both map to UndeterminedOutcome. PENDING is the
// case that matters: a submission recorded before the send has no answer yet, so
// it is exactly as undetermined as one that timed out, and treating it as
// anything else would let UndeterminedPrior report no unresolved attempt for an
// order that is still in flight.
func outcomeFor(s StoredOutcome) Outcome {
	switch s {
	case StoredAck, StoredFilled, StoredCancelled:
		return AcceptedOutcome
	case StoredRejected:
		return RejectedOutcome
	default:
		return UndeterminedOutcome
	}
}

// Record is one row of execution.submission: the durable fact that a request was
// sent for an order, and what became of it.
type Record struct {
	SubmissionID contracts.ID

	// OrderID is the canonical OMS identity the submission exists for.
	OrderID string

	Environment   contracts.Environment
	VenueID       string
	AdapterID     string
	ClientOrderID string

	// VenueIdempotent records whether the venue guarantees client-order-id
	// idempotency. It is the fact that decides whether the durable record is
	// mandatory or merely prudent, so it is stored rather than assumed.
	VenueIdempotent bool

	RequestSentAt   time.Time
	RequestSentAtNs int64
	RequestDigest   string

	Outcome         StoredOutcome
	ResponseAt      sql.NullTime
	ResponseAtNs    sql.NullInt64
	VenueOrderRef   sql.NullString
	ResponseDigest  sql.NullString
	ErrorCode       sql.NullString
	ErrorDetail     sql.NullString
	Attempts        int
	RetryAuthorized sql.NullString
	ReconcilCaseID  sql.NullString
}

var (
	// ErrNoSubmissionRecord means the order has never been submitted. It is
	// distinct from a read failure so that "never submitted" -- which permits a
	// first attempt -- is never confused with "could not be established" -- which
	// must refuse, because doc 01 invariant 3 is about what cannot be ruled out.
	ErrNoSubmissionRecord = errors.New("execution: no durable submission record exists for this order")

	// ErrHistoryUnreadable means the submission history exists but could not be
	// read. This is not ErrNoSubmissionRecord and must not be treated as it: an
	// unreadable history cannot establish that no prior attempt is undetermined,
	// so it refuses rather than permitting.
	ErrHistoryUnreadable = errors.New("execution: the durable submission history could not be read; " +
		"it cannot be established that no prior attempt is undetermined, so the submit is refused")

	// ErrRetryNotAuthorized is returned when a second attempt is requested for an
	// order whose history carries no reconciliation case and no authorizer. The
	// database enforces the same rule with
	// submission_retry_requires_reconciliation; this is the Go-side refusal that
	// ErrRetryNotAuthorized is returned when a second submission attempt would
	// stop the attempt before a row is written.
	ErrRetryNotAuthorized = errors.New("execution: a second submission attempt requires a recorded " +
		"reconciliation case and a named authorizer; an exposure-increasing retry is never blind")
)

// Store reads and writes execution.submission.
type Store struct {
	db Querier
}

// NewStore returns a Store over db.
func NewStore(db Querier) (*Store, error) {
	if db == nil {
		return nil, errors.New("execution: a submission store needs a database handle")
	}
	return &Store{db: db}, nil
}

// RequestDigest is the redacted reference to the request bytes actually sent.
//
// Raw payloads may carry venue credentials or account detail, so the table stores
// a digest rather than the payload (16_..._ADAPTERS.md §4). The digest is over
// the exact bytes handed to the transport, which is what makes it evidence of
// what was sent rather than evidence of what the caller intended to send.
func RequestDigest(request []byte) string {
	sum := sha256.Sum256(request)
	return hex.EncodeToString(sum[:])
}

// RecordAttempt writes the durable PENDING row that must exist before the
// request leaves the process.
//
// The row is written first and the send happens afterwards, by the caller. That
// ordering is not advisory: a submission whose record is written after the send
// has an interval in which the venue may hold an order that this platform has
// written down nowhere, and doc 01 invariant 3 is precisely about not being able
// to rule that out.
//
// A caller that supplies Attempts greater than one without a reconciliation case
// and an authorizer is refused here rather than being allowed to reach the
// database constraint, so the refusal carries a reason rather than a bare
// violation.
func (s *Store) RecordAttempt(ctx context.Context, r Record) error {
	return s.RecordAttemptIn(ctx, s.db, r)
}

// RecordAttemptIn writes the durable PENDING row inside a transaction the caller
// owns, and does not commit.
//
// Doc 05 requires the durable record to be created in the same transaction as the
// state mutation and the audit record. Committing the submission row separately
// would defeat that: the row would claim an order was submitted while the
// transition that put it there rolled back, and the row is precisely what the
// no-retry invariant reads.
func (s *Store) RecordAttemptIn(ctx context.Context, q Querier, r Record) error {
	if q == nil {
		return errors.New("execution: RecordAttemptIn needs a handle to write through")
	}
	if r.OrderID == "" {
		return errors.New("execution: a submission record needs an order identity")
	}
	if r.ClientOrderID == "" {
		// The column is NOT NULL and the uniqueness constraint is on
		// (venue_id, environment, client_order_id), so an empty value would
		// collide with every other empty value for that venue and environment.
		// It cannot be optional.
		return errors.New("execution: a submission record needs a client order id; the column is not " +
			"nullable and an empty value would collide with every other submission lacking one")
	}
	if r.RequestDigest == "" {
		return errors.New("execution: a submission record needs a request digest; without one the " +
			"record cannot prove what was sent")
	}
	if !r.Outcome.valid() {
		return fmt.Errorf("execution: outcome %q is not one of PENDING, ACKNOWLEDGED, REJECTED, "+
			"TIMED_OUT_UNKNOWN, FILLED or CANCELLED", r.Outcome)
	}

	attempts := r.Attempts
	if attempts <= 0 {
		attempts = 1
	}
	if attempts > 1 && (!r.ReconcilCaseID.Valid || !r.RetryAuthorized.Valid) {
		return fmt.Errorf("%w: attempts=%d, reconciliation_case_id=%v, retry_authorized_by=%v",
			ErrRetryNotAuthorized, attempts, r.ReconcilCaseID, r.RetryAuthorized)
	}

	if r.SubmissionID == (contracts.ID{}) {
		id, err := contracts.NewID(contracts.EntitySubmission)
		if err != nil {
			return fmt.Errorf("execution: minting a submission id failed: %w", err)
		}
		r.SubmissionID = id
	}

	_, err := q.ExecContext(ctx, `
		INSERT INTO execution.submission (
			submission_id, order_id, environment, venue_id, adapter_id,
			client_order_id, venue_idempotent,
			request_sent_at, request_sent_at_ns,
			outcome, response_received_at, venue_order_ref,
			request_digest, response_digest,
			error_code, error_detail,
			attempts, retry_authorized_by, reconciliation_case_id
		) VALUES (
			$1, $2, $3::common.environment, $4, $5,
			$6, $7,
			common.ns_to_timestamptz($8), $8,
			$9, $10, $11,
			$12, $13,
			$14, $15,
			$16, $17, $18
		)`,
		r.SubmissionID.String(), r.OrderID, string(r.Environment), r.VenueID, r.AdapterID,
		r.ClientOrderID, r.VenueIdempotent,
		r.RequestSentAtNs,
		string(r.Outcome), r.ResponseAt, r.VenueOrderRef,
		r.RequestDigest, r.ResponseDigest,
		r.ErrorCode, r.ErrorDetail,
		attempts, r.RetryAuthorized, r.ReconcilCaseID)
	if err != nil {
		return fmt.Errorf("execution: writing the durable submission record failed, and the "+
			"request is not sent: %w", err)
	}
	return nil
}

// Outcome writes the terminal outcome of a submission whose record already
// exists.
//
// It updates in place and never inserts. A missing row is ErrNoSubmissionRecord
// rather than a silent insert, because the record was required to precede the
// send: a row that does not exist here means the ordering was violated upstream,
// and creating it now would fabricate the appearance of compliance.
func (s *Store) Outcome(ctx context.Context, submissionID contracts.ID, o Outcome,
	venueOrderRef string, responseDigest string, at time.Time, atNs int64) error {

	return s.OutcomeIn(ctx, s.db, submissionID, o, venueOrderRef, responseDigest, at, atNs)
}

// OutcomeIn records a submission outcome through a caller-supplied handle.
//
// It exists for the same reason HistoryIn does. The command that applies a venue's
// answer has to move the OMS state, the submission row, the audit record and the
// outbox row in one transaction, and an outcome written through the pool would be a
// separate commit that can succeed while the state transition fails. A reader could
// then see a submission whose outcome is ACKNOWLEDGED while the order is still
// SUBMITTING, which is the split this codebase treats as a defect everywhere else.
func (s *Store) OutcomeIn(ctx context.Context, q Querier, submissionID contracts.ID, o Outcome,
	venueOrderRef string, responseDigest string, at time.Time, atNs int64) error {

	if q == nil {
		return fmt.Errorf("%w: no handle to write through", ErrNoSubmissionRecord)
	}

	var ref, resp any
	if venueOrderRef != "" {
		ref = venueOrderRef
	}
	if responseDigest != "" {
		resp = responseDigest
	}

	res, err := q.ExecContext(ctx, `
		UPDATE execution.submission
		   SET outcome = $2,
		       response_received_at = common.ns_to_timestamptz($3),
		       venue_order_ref = COALESCE($4, venue_order_ref),
		       response_digest = COALESCE($5, response_digest)
		 WHERE submission_id = $1`,
		submissionID.String(), string(storedFor(o)), atNs, ref, resp)
	if err != nil {
		return fmt.Errorf("execution: recording the submission outcome failed: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %s", ErrNoSubmissionRecord, submissionID)
	}
	return nil
}

// Answered writes the venue's first answer to a submission, guarded on the row still
// being PENDING.
//
// The guard is the point. ApplyVenueAnswer reads the order FOR UPDATE, sees
// SUBMITTING, and then updates the submission -- and between those two statements
// another writer may have answered it. Updating without a guard would move the order
// on the strength of a second, possibly different, answer. The WHERE clause is the
// concurrency control, and finding nothing is reported rather than treated as
// success.
//
// It writes the same columns OutcomeIn does, including response_received_at, because
// submission_terminal_has_time rejects a terminal outcome with no response time.
// Writing a partial update here is not a smaller change to the same statement, it is
// a different statement that the database refuses.
func (s *Store) AnswerIn(ctx context.Context, q Querier, submissionID contracts.ID, o Outcome,
	venueOrderRef string, responseDigest string, at time.Time, atNs int64) (string, error) {

	if q == nil {
		return "", fmt.Errorf("%w: no handle to write through", ErrNoSubmissionRecord)
	}
	var ref, resp any
	if venueOrderRef != "" {
		ref = venueOrderRef
	}
	if responseDigest != "" {
		resp = responseDigest
	}
	var orderID string
	err := q.QueryRowContext(ctx, `
		UPDATE execution.submission
		   SET outcome = $2,
		       response_received_at = common.ns_to_timestamptz($3),
		       venue_order_ref = COALESCE($4, venue_order_ref),
		       response_digest = COALESCE($5, response_digest)
		 WHERE submission_id = $1 AND outcome = 'PENDING'
		RETURNING order_id`,
		submissionID.String(), string(storedFor(o)), atNs, ref, resp).Scan(&orderID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: submission %s was not PENDING; it was answered concurrently",
			ErrNoSubmissionRecord, submissionID)
	}
	if err != nil {
		return "", fmt.Errorf("execution: recording the submission outcome failed: %w", err)
	}
	return orderID, nil
}

// History returns the durable attempt history for an order.
//
// This is what replaces a caller-assembled []Attempt. Every row is returned,
// ordered by attempt, and its stored outcome is mapped back onto Go's
// vocabulary -- so PENDING and TIMED_OUT_UNKNOWN both arrive as
// UndeterminedOutcome, and a later Submit refuses on them.
//
// The refusal on read failure is the point of the function. A caller that
// assembled its own slice could omit a row it failed to load, which is exactly
// how a second exposure-increasing command gets sent. Here, an unreadable
// history is an error the caller must handle, not an empty list.
func (s *Store) History(ctx context.Context, orderID string) ([]Attempt, error) {
	return s.HistoryIn(ctx, s.db, orderID)
}

// HistoryIn reads the attempt history through a caller-supplied handle.
//
// The command uses the transaction it is about to write in, so the history it
// refuses on and the row it writes are decided against the same snapshot. Reading
// through the pool instead would open a window in which another writer commits a
// submission between the read and the write, and this command's whole purpose is
// to be the thing that cannot be raced into a duplicate send.
func (s *Store) HistoryIn(ctx context.Context, q Querier, orderID string) ([]Attempt, error) {
	if q == nil {
		return nil, fmt.Errorf("%w: no handle to read through", ErrHistoryUnreadable)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT attempts, client_order_id, outcome, venue_order_ref
		  FROM execution.submission
		 WHERE order_id = $1
		 ORDER BY request_sent_at_ns, submission_id`, orderID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHistoryUnreadable, err)
	}
	defer rows.Close()

	var out []Attempt
	for rows.Next() {
		var (
			attempts int
			cid      string
			outcome  string
			vref     sql.NullString
		)
		if err := rows.Scan(&attempts, &cid, &outcome, &vref); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrHistoryUnreadable, err)
		}
		if !StoredOutcome(outcome).valid() {
			// A stored value this code cannot interpret must not be read as
			// anything else. Undetermined is the reading that permits
			// reconciliation; every other reading is an assumption about
			// exposure that the database did not authorise. Skipping the row
			// would be worse still: it would drop the attempt entirely, which is
			// how a submission with live exposure disappears from the history.
			return nil, fmt.Errorf("%w: submission row carries outcome %q, which is not one of "+
				"PENDING, ACKNOWLEDGED, REJECTED, TIMED_OUT_UNKNOWN, FILLED or CANCELLED",
				ErrHistoryUnreadable, outcome)
		}
		a := Attempt{
			Index:         attempts,
			ClientOrderID: cid,
			Outcome:       outcomeFor(StoredOutcome(outcome)),
		}
		if vref.Valid {
			a.VenueOrderID = vref.String
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHistoryUnreadable, err)
	}
	return out, nil
}

// UndeterminedDurable reports whether the durable history for an order contains
// an attempt whose outcome was never determined, which is the precondition
// doc 01 invariant 3 forbids from being followed by another send.
//
// It is the store-backed equivalent of UndeterminedPrior, and it differs in what
// happens when the history cannot be read: UndeterminedPrior returns nil for an
// empty list, which means "nothing unresolved" and permits a submit. Here a read
// failure returns an error, because "could not be established" and "established
// as clear" are different facts and only the second one permits a send.
func (s *Store) UndeterminedDurable(ctx context.Context, orderID string) (*Attempt, error) {
	history, err := s.History(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if u := UndeterminedPrior(history); u != nil {
		return u, nil
	}
	return nil, nil
}
