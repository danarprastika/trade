package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/domain/oms"
	"github.com/aitc/trade/services/audit"
)

// The submit command: one transaction, then the venue.
//
// Doc 05, "Transaction boundary": "An authoritative command executes domain
// validation, state mutation, audit record creation, and outbox insertion in one
// PostgreSQL transaction. A committed state mutation always has a corresponding
// durable outbox record."
//
// That sentence settles what is often left as a design question. The durable
// PENDING submission row, the OMS state move to SUBMITTING, the audit record and
// the outbox row are one unit. If any of them fails, none of them happened -- which
// is what makes the PENDING row trustworthy evidence that the order reached the
// venue, because it cannot exist without the transition that put it there.
//
// The venue call is NOT in that transaction. Doc 05 describes a command that
// mutates the platform's own state; a venue is a separate system, and holding a
// PostgreSQL transaction open across a network call to one is how a connection
// pool is exhausted. So the transaction commits, and only then does the
// dispatcher send. The PENDING row is the durable statement that a send is owed.
//
// Nothing here talks to a venue. This command produces the committed intent; the
// dispatcher (doc 05, "Outbox", reads committed rows FOR UPDATE SKIP LOCKED) is
// what performs the send.

// ErrSubmitNotDurable means the command's transaction did not commit, so the
// order was never sent and must not be treated as though it were.
//
// It is returned only after a rollback. A caller that receives it can retry the
// whole command; it must not assume a send is outstanding, because the record
// that would have proved one does not exist.
var ErrSubmitNotDurable = errors.New("execution: the submit intent did not commit; no request was sent " +
	"and the order was not advanced")

// SubmitIntent is the input to the command.
type SubmitIntent struct {
	OrderID       string
	CorrelationID string
	ActorID       string

	Environment contracts.Environment

	// RiskDecisionID must be present. Both the RISK_PENDING -> RISK_APPROVED and
	// RISK_APPROVED -> SUBMITTING rules require it, and the order row carries the
	// one from the risk gate. It is not invented here.
	RiskDecisionID string

	// PolicyVersion names the risk policy revision that authorised this submit.
	//
	// Required rather than defaulted because audit.record enforces
	// length(policy_version) > 0, and because an audit record that cannot say
	// which policy was in force cannot answer "was this permitted at the time".
	// The value comes from the order's resolved policy, not from this command.
	PolicyVersion string

	VenueID   string
	AdapterID string

	// ClientOrderID is the idempotency token for this attempt.
	ClientOrderID string

	// VenueIdempotent records what the adapter declared, not what the caller
	// believes. Where it is false the durable record is mandatory rather than
	// merely prudent (doc 16 §4).
	VenueIdempotent bool

	// ReconcilCaseID and RetryAuthorizedBy gate a second attempt. Both are
	// required together when this is not the first, and the database enforces the
	// same rule; the check here exists so the refusal names a reason.
	ReconcilCaseID  string
	RetryAuthorized string

	// RequestDigest is the redacted reference to the exact bytes to be sent. It is
	// required: a record that cannot say what was sent is not evidence.
	RequestDigest string

	OccurredAt time.Time
}

// Prepared is what the command committed. Everything the dispatcher needs, and
// nothing that would let it decide differently from the way the command did.
type Prepared struct {
	SubmissionID contracts.ID
	OrderEventID contracts.ID
	EventID      contracts.ID
	Attempts     int
}

// Command prepares a submission inside one transaction.
type Command struct {
	db       *sql.DB
	store    *Store
	appender *audit.Appender
	producer string
	gate     SubmissionGate
	// signingKey names the key in audit.record.signing_key_id. See NewCommand for
	// why it is required rather than defaulted.
	signingKey string
}

// NewCommand returns a Command writing through db.
//
// producer identifies the component in ops.outbox.producer_id. It is supplied
// rather than defaulted because an outbox row that cannot be attributed to a
// producer cannot be traced to a release.
//
// gate is the certification gate every submission passes before anything is
// written. It is required; see ErrNoSubmissionGate for why a missing gate is a
// refusal rather than a default.
//
// signingKey names the key that attests to this component's audit records. It is
// required for the same reason domain/ledger.NewPoster requires one:
// audit.record.signing_key_id is NOT NULL, so an empty value satisfies the schema
// while naming no key at all, and a record that asserts a submission was prepared
// or resolved with nothing attesting to it cannot be defended later. That column
// reads as the batch-closing key, but nothing revises it after the append -- there
// is no UPDATE anywhere in the migrations -- so the only key that can ever appear
// there is the one supplied at write time. Leaving it empty is therefore not a
// deferred value; it is the final one.
func NewCommand(db *sql.DB, s *Store, a *audit.Appender, gate SubmissionGate,
	producer, signingKey string) (*Command, error) {

	if db == nil {
		return nil, errors.New("execution: a submit command needs a database handle")
	}
	if gate == nil {
		return nil, ErrNoSubmissionGate
	}
	if s == nil {
		return nil, errors.New("execution: a submit command needs a submission store")
	}
	if a == nil {
		// Not cosmetic. The audit appender is what supplies AppendIn, and without
		// it the transaction would carry a mutation with no audit record, which
		// is the condition doc 05's boundary exists to prevent.
		return nil, errors.New("execution: a submit command needs an audit appender; a mutation " +
			"committed without its audit record cannot be accounted for later")
	}
	if producer == "" {
		return nil, errors.New("execution: a submit command needs a producer identity for its outbox rows")
	}
	if signingKey == "" {
		return nil, ErrNoSigningKey
	}
	return &Command{
		db: db, store: s, appender: a, producer: producer, gate: gate, signingKey: signingKey,
	}, nil
}

// Prepare commits the submit intent. It does not contact a venue.
//
// The order of operations inside the transaction is not a preference. The history
// is read first, because a refusal must happen before anything is written; the OMS
// transition is journalled before the order row moves, because the journal is the
// authority and the row is a projection of it.
func (c *Command) Prepare(ctx context.Context, in SubmitIntent) (Prepared, error) {
	var out Prepared

	if err := validateIntent(in); err != nil {
		return Prepared{}, err
	}

	// 0. The certification gate, before the transaction opens.
	//
	//    Placement is load-bearing in two directions. It runs before BeginTx so a
	//    refusal costs nothing and writes nothing: no SUBMITTING transition, no
	//    PENDING submission row, no outbox row. And it runs here rather than only at
	//    the eventual venue call because Prepare is where the platform commits to
	//    owing a send. An order prepared for an uncertified adapter is not merely
	//    unsent -- it is committed in SUBMITTING with a durable pending submission,
	//    and it will present as an order in flight until someone notices no venue
	//    ever received it.
	//
	//    This is not sufficient on its own and does not pretend to be. Certification
	//    can be revoked between this commit and the dispatcher's send, and a gate
	//    consulted only here would let that order through. The send site must consult
	//    the same gate immediately before it calls the venue. That site does not
	//    exist yet -- no venue.Writer is wired into the dispatcher -- so until it
	//    does, this check closes the uncertified-adapter hole and leaves a narrower
	//    revocation window open.
	if err := c.gate.PermitSubmit(ctx, in.AdapterID, in.Environment); err != nil {
		return Prepared{}, fmt.Errorf("execution: order %s will not be prepared for submission to %s: %w",
			in.OrderID, in.VenueID, err)
	}

	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return Prepared{}, fmt.Errorf("execution: begin submit transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// 1. The durable precondition, read from the same transaction that will write.
	//
	//    Reading it here rather than trusting a caller-supplied slice is the whole
	//    reason this command exists. A caller can assemble []Attempt however it
	//    likes; it cannot hide a row from this query.
	history, err := c.store.HistoryIn(ctx, tx, in.OrderID)
	if err != nil {
		return Prepared{}, err
	}
	attempts := len(history) + 1
	if attempts > 1 && (in.ReconcilCaseID == "" || in.RetryAuthorized == "") {
		return Prepared{}, fmt.Errorf("%w: this order already has %d submission attempt(s), so a "+
			"second requires reconciliation_case_id and retry_authorized_by", ErrRetryNotAuthorized, len(history))
	}
	if unresolved := UndeterminedPrior(history); unresolved != nil {
		return Prepared{}, fmt.Errorf("%w: attempt %d for order %s is still undetermined",
			ErrResolutionRequired, unresolved.Index, in.OrderID)
	}

	// 2. Move the order to SUBMITTING, journalling first.
	eventID, err := contracts.NewID(contracts.EntityOrderEvent)
	if err != nil {
		return Prepared{}, fmt.Errorf("execution: minting an order event id failed: %w", err)
	}
	seq, err := c.nextSequence(ctx, tx, in.OrderID)
	if err != nil {
		return Prepared{}, err
	}
	if err := c.moveToSubmitting(ctx, tx, in, eventID, seq); err != nil {
		return Prepared{}, err
	}

	// 3. The durable submission record. Its existence is what the dispatcher will
	//    later act on and what proves an order may be in flight.
	subID, err := contracts.NewID(contracts.EntitySubmission)
	if err != nil {
		return Prepared{}, fmt.Errorf("execution: minting a submission id failed: %w", err)
	}
	ns := in.OccurredAt.UTC().UnixNano()
	if err := c.store.RecordAttemptIn(ctx, tx, Record{
		SubmissionID:    subID,
		OrderID:         in.OrderID,
		Environment:     in.Environment,
		VenueID:         in.VenueID,
		AdapterID:       in.AdapterID,
		ClientOrderID:   in.ClientOrderID,
		VenueIdempotent: in.VenueIdempotent,
		RequestSentAt:   in.OccurredAt.UTC(),
		RequestSentAtNs: ns,
		RequestDigest:   in.RequestDigest,
		Outcome:         StoredPending,
		Attempts:        attempts,
		RetryAuthorized: nullIfEmpty(in.RetryAuthorized),
		ReconcilCaseID:  nullIfEmpty(in.ReconcilCaseID),
	}); err != nil {
		return Prepared{}, err
	}

	// 4. The audit record, in the caller's transaction rather than one of its own.
	auditID, err := contracts.NewID(contracts.EntityAudit)
	if err != nil {
		return Prepared{}, fmt.Errorf("execution: minting an audit id failed: %w", err)
	}
	reason := "submission prepared for dispatch to " + in.VenueID
	details, err := json.Marshal(map[string]any{
		"order_id":        in.OrderID,
		"submission_id":   subID.String(),
		"client_order_id": in.ClientOrderID,
		"venue_id":        in.VenueID,
		"adapter_id":      in.AdapterID,
		"attempts":        attempts,
		"request_digest":  in.RequestDigest,
	})
	if err != nil {
		return Prepared{}, fmt.Errorf("execution: encoding audit details failed: %w", err)
	}
	if _, err := c.appender.AppendIn(ctx, tx, audit.Record{
		AuditID:            auditID.String(),
		TenantOrOwnerScope: "trading",
		ActorID:            in.ActorID,
		ActorType:          contracts.ActorWorkload,
		Action:             "ORDER_SUBMIT_PREPARED",
		TargetType:         "ORDER",
		TargetID:           in.OrderID,
		Environment:        in.Environment,
		OccurredAt:         in.OccurredAt.UTC(),
		RecordedAt:         in.OccurredAt.UTC(),
		Reason:             &reason,
		CorrelationID:      in.CorrelationID,
		PolicyVersion:      in.PolicyVersion,
		Result:             contracts.AuditResultSuccess,
		Details:            string(details),
		SigningKeyID:       c.signingKey,
	}); err != nil {
		return Prepared{}, fmt.Errorf("execution: the submit intent was not audited, so the "+
			"transaction is abandoned: %w", err)
	}

	// 5. The outbox row. Doc 05: a committed state mutation ALWAYS has one. There
	//    is no trigger that enforces this, so the omission would be silent.
	evtID, err := c.enqueue(ctx, tx, outboxEnvelope{
		EventType: "ORDER_SUBMIT_REQUESTED",
		Payload: map[string]any{
			"submission_id":    subID.String(),
			"order_id":         in.OrderID,
			"venue_id":         in.VenueID,
			"adapter_id":       in.AdapterID,
			"client_order_id":  in.ClientOrderID,
			"venue_idempotent": in.VenueIdempotent,
			"request_digest":   in.RequestDigest,
		},
		AggregateID:   in.OrderID,
		Sequence:      seq,
		CorrelationID: in.CorrelationID,
		CausationID:   subID.String(),
		Environment:   in.Environment,
		OccurredAt:    in.OccurredAt.UTC(),
	})
	if err != nil {
		return Prepared{}, err
	}

	// The audit record's identity is recorded on the OMS event, which closes the
	// link between the state machine and the evidence for it.
	if _, err := tx.ExecContext(ctx,
		`UPDATE oms.order_event SET audit_id = $1 WHERE order_event_id = $2`,
		auditID.String(), eventID.String()); err != nil {
		return Prepared{}, fmt.Errorf("execution: binding the audit record to the order event failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Prepared{}, fmt.Errorf("%w: commit failed: %v", ErrSubmitNotDurable, err)
	}
	committed = true

	out = Prepared{
		SubmissionID: subID,
		OrderEventID: eventID,
		EventID:      evtID,
		Attempts:     attempts,
	}
	return out, nil
}

// moveToSubmitting journals RISK_APPROVED -> SUBMITTING and moves the row.
//
// The journal write comes first. The reverse order would let the row advance
// without evidence, and the trigger that guards the initial state exists precisely
// because a state is meant to be reachable only through its event.
func (c *Command) moveToSubmitting(ctx context.Context, tx *sql.Tx, in SubmitIntent,
	eventID contracts.ID, seq int64) error {

	var fromState string
	if err := tx.QueryRowContext(ctx,
		`SELECT state::text FROM oms."order" WHERE order_id = $1 FOR UPDATE`, in.OrderID).
		Scan(&fromState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("execution: order %s does not exist, so there is nothing to submit", in.OrderID)
		}
		return fmt.Errorf("execution: reading the order failed: %w", err)
	}

	if fromState != string(oms.StateRiskApproved) {
		// Refusing rather than advancing. An order that is already SUBMITTING may
		// have a send in flight that nobody can see, and moving it again would
		// create a second one.
		return fmt.Errorf("execution: order %s is in %s, not %s; only an order the risk gate has "+
			"approved may be submitted", in.OrderID, fromState, oms.StateRiskApproved)
	}

	ns := in.OccurredAt.UTC().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO oms.order_event (
			order_event_id, order_id, sequence, from_state, to_state, event_kind,
			filled_quantity_delta, price, actor_id, reason, correlation_id,
			risk_decision_id, occurred_at, occurred_at_ns)
		VALUES ($1,$2,$3,$4::oms.order_state,$5::oms.order_state,$6::oms.order_event_kind,
		        0,NULL,$7,$8,$9,$10,common.ns_to_timestamptz($11),$11)`,
		eventID.String(), in.OrderID, seq, fromState,
		string(oms.StateSubmitting), string(oms.EventSubmitting),
		in.ActorID, "submitting to "+in.VenueID, in.CorrelationID,
		in.RiskDecisionID, ns); err != nil {
		return fmt.Errorf("execution: journalling the SUBMITTING transition failed: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE oms."order"
		   SET state = $2::oms.order_state, version_vector = version_vector + 1
		 WHERE order_id = $1`, in.OrderID, string(oms.StateSubmitting)); err != nil {
		return fmt.Errorf("execution: advancing the order to SUBMITTING failed: %w", err)
	}
	return nil
}

// outboxEnvelope is what an outbox row needs, expressed without reference to any
// one caller.
//
// It exists because Prepare and Resolve are different kinds of event -- a send and
// a reconciliation -- but carry the same obligation. Doc 05 says a committed state
// mutation ALWAYS has a durable outbox row, and no trigger enforces that, so the
// obligation has to be written down once in a statement both paths call. Two
// hand-written INSERTs would drift, and the drift would be silent.
type outboxEnvelope struct {
	// EventType names the event, e.g. ORDER_SUBMIT_REQUESTED.
	EventType string

	// Payload is the event body. It is encoded here rather than by the caller so
	// an encoding failure is part of the transaction that must not commit.
	Payload map[string]any

	// AggregateID is the order the event belongs to. The aggregate type is always
	// ORDER: every event this package emits is about one order's history.
	AggregateID string

	// Sequence is the order's own OMS event sequence, so the outbox stream for an
	// order is ordered the same way its state machine is. A per-aggregate counter
	// that disagreed with the OMS sequence would make the dispatcher's ordering and
	// the order's history two different stories about the same order.
	Sequence int64

	// CorrelationID and CausationID tie the row to the command that produced it.
	CorrelationID string
	CausationID   string

	Environment contracts.Environment
	OccurredAt  time.Time
}

// enqueue writes the outbox row the dispatcher will send from, and returns the
// event id that identifies it.
//
// The event id is minted here rather than by the caller, because the caller has to
// report it and must be reporting the one that was actually written. Minting it
// outside and passing it in was the earlier arrangement, and it meant the returned
// id named an event that was never inserted.
func (c *Command) enqueue(ctx context.Context, tx *sql.Tx, ev outboxEnvelope) (contracts.ID, error) {

	evtID, err := contracts.NewID(contracts.EntityEvent)
	if err != nil {
		return contracts.ID{}, fmt.Errorf("execution: minting an event id failed: %w", err)
	}
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return contracts.ID{}, fmt.Errorf("execution: encoding the outbox payload failed: %w", err)
	}
	ns := ev.OccurredAt.UTC().UnixNano()
	var causation any
	if ev.CausationID != "" {
		causation = ev.CausationID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ops.outbox (
			event_id, event_type, schema_version, aggregate_type, aggregate_id,
			sequence, correlation_id, causation_id, producer_id, environment,
			occurred_at, occurred_at_ns, recorded_at, payload, dispatch_state, attempt_count, max_attempts)
		VALUES ($1,$2,'1.0.0','ORDER',$3,
		        $4,$5,$6,$7,$8::common.environment,
		        common.ns_to_timestamptz($9),$9,common.ns_to_timestamptz($9),$10::jsonb,
		        'PENDING',0,5)`,
		evtID.String(), ev.EventType, ev.AggregateID, ev.Sequence,
		ev.CorrelationID, causation, c.producer, string(ev.Environment),
		ns, string(payload)); err != nil {
		return contracts.ID{}, fmt.Errorf("execution: inserting the outbox row failed; a committed "+
			"state mutation must always have one (doc 05): %w", err)
	}
	return evtID, nil
}

// RecordOutcome writes the terminal outcome of a submission Prepare created.
//
// It is separate from Prepare because it cannot be in the same transaction: the
// venue call happens after Prepare commits, and its answer arrives later still. A
// submit command that held its transaction open across the venue would defeat the
// boundary it exists to establish.
//
// UndeterminedOutcome here is the honest record of a timeout. It never maps to
// REJECTED -- see storedFor.
func (c *Command) RecordOutcome(ctx context.Context, submissionID contracts.ID, o Outcome,
	venueOrderRef, responseDigest string, at time.Time, atNs int64) error {
	return c.store.Outcome(ctx, submissionID, o, venueOrderRef, responseDigest, at, atNs)
}

// nextSequence reads the order's next event sequence inside the transaction.
func (c *Command) nextSequence(ctx context.Context, tx *sql.Tx, orderID string) (int64, error) {
	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT max(sequence) FROM oms.order_event WHERE order_id = $1`, orderID).
		Scan(&maxSeq); err != nil {
		return 0, fmt.Errorf("execution: reading the order event sequence failed: %w", err)
	}
	if !maxSeq.Valid {
		return 1, nil
	}
	return maxSeq.Int64 + 1, nil
}

func validateIntent(in SubmitIntent) error {
	if in.OrderID == "" {
		return errors.New("execution: a submit needs an order identity")
	}
	if in.ClientOrderID == "" {
		return errors.New("execution: a submit needs a client order id; the column is not nullable " +
			"and an empty value collides with every other submission lacking one")
	}
	if in.RequestDigest == "" {
		return errors.New("execution: a submit needs a request digest; without one the durable " +
			"record cannot prove what was sent")
	}
	if in.RiskDecisionID == "" {
		return errors.New("execution: a submit needs a risk decision id; the transition into " +
			"SUBMITTING requires one and it is not this command's to invent")
	}
	if in.PolicyVersion == "" {
		return errors.New("execution: a submit needs a policy version; audit.record requires one, and " +
			"an audit record that cannot name the policy in force cannot answer whether the submit was permitted")
	}
	if in.VenueID == "" {
		return errors.New("execution: a submit needs a venue")
	}
	if in.AdapterID == "" {
		// Not merely cosmetic. The certification gate is keyed by adapter, so an
		// intent naming no adapter is asking whether nobody's adapter may submit --
		// a question whose answer would have to be "yes" for the submit to proceed.
		return errors.New("execution: a submit needs an adapter id; certification is recorded per " +
			"adapter, so an unnamed adapter cannot be certified and cannot be checked")
	}
	if in.OccurredAt.IsZero() {
		return errors.New("execution: a submit needs an occurrence time; defaulting to the clock " +
			"would make the durable record's timestamps unreproducible")
	}
	if in.ActorID == "" {
		return errors.New("execution: a submit needs an actor; every audit record must attribute " +
			"the action to something")
	}
	return nil
}

func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
