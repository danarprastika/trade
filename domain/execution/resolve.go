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

// The sanctioned exit from UNKNOWN.
//
// Doc 16 §4: "A timeout after a possible submission yields OMS UNKNOWN; no
// exposure-increasing retry occurs until the venue state is resolved or an
// authorized recovery procedure explicitly determines the safe action."
//
// So resolution is not optional bookkeeping after a timeout -- it is the only thing
// that can make the order whole again. It was the last behaviour living solely in
// the retired Submitter, which had no durable path: it wrote a transition through a
// Recorder and kept the attempt history in a slice the caller supplied. The rules
// are the same here and the evidence is now committed.
//
// Resolving is a READ of venue state and is kept separate from submitting for the
// reason doc 16 implies: conflating them is how a reconciliation becomes an
// accidental second order. Command.Resolve cannot place an order, and the only
// thing it writes is the outcome the venue reports plus the transition that
// records it.

// Resolver is the read side of the venue boundary.
//
// It is an interface rather than a function so the distinction from submission is
// structural: a type that can only resolve cannot be passed where a type that can
// submit is expected, and a caller cannot accidentally reuse a submit-capable
// adapter as a resolver without the compiler noticing.
type Resolver interface {
	Resolve(ctx context.Context, orderID, clientOrderID string) (Outcome, string, error)
}

// ErrNothingToResolve means the order has no undetermined attempt.
//
// Resolving an order whose outcome is already known would be a second opinion
// rather than a reconciliation, and it would overwrite a determinate record with
// whatever the venue happens to say now. Refusing is the only reading that cannot
// lose information.
var ErrNothingToResolve = errors.New("execution: this order has no undetermined submission attempt, " +
	"so there is nothing to resolve; resolving a known outcome would overwrite it with a later answer")

// Resolved is what the venue reported and what became of the order.
type Resolved struct {
	// OrderID is the order that was resolved.
	OrderID string

	// SubmissionID is the submission whose outcome was rewritten.
	SubmissionID contracts.ID

	// State is where the order now is. UNKNOWN means the venue still could not
	// say, which is a legitimate answer and not a failure to handle.
	State oms.State

	// Outcome is what the venue reported, mapped onto Go's vocabulary.
	Outcome Outcome

	// VenueOrderID is the venue's own reference, empty when it gave none.
	VenueOrderID string

	// Attempts is the durable history after the rewrite.
	Attempts []Attempt
}

// Resolve determines an UNKNOWN order's outcome from the venue's own record.
//
// The order of operations mirrors Prepare and is deliberate:
//
//  1. read the durable history and refuse unless something is undetermined
//  2. ask the venue -- the only network call, and it happens after step 1
//  3. commit the outcome and the transition together
//
// An outcome this code cannot interpret leaves the order in UNKNOWN. Adopting an
// unknown answer would end the order's life at exactly the moment the platform
// knows least, which is the failure oms.State.Terminal's comment warns about for
// UNKNOWN specifically.
func (c *Command) Resolve(ctx context.Context, orderID, actorID string, resolver Resolver) (Resolved, error) {
	if orderID == "" {
		return Resolved{}, errors.New("execution: no order identity")
	}
	// Not defaulted to a component name. Doc 22 requires an actor on every record
	// and doc 21 forbids service identities impersonating humans, so who resolved
	// an order is evidence, not a constant. A caller that cannot say is a caller
	// that should not be resolving.
	if actorID == "" {
		return Resolved{}, errors.New("execution: resolving needs an actor identity; a resolution with " +
			"no accountable actor cannot be audited")
	}
	if resolver == nil {
		// Not a defaulted argument. Without a resolver the outcome stays
		// undetermined by construction, and the order would stay UNKNOWN forever
		// with no error to explain why.
		return Resolved{}, errors.New("execution: an UNKNOWN order needs a resolver; without one the " +
			"outcome stays undetermined by construction")
	}

	// Step 1: the durable precondition.
	history, err := c.store.History(ctx, orderID)
	if err != nil {
		return Resolved{}, err
	}
	unresolved := UndeterminedPrior(history)
	if unresolved == nil {
		return Resolved{}, fmt.Errorf("%w: order %s", ErrNothingToResolve, orderID)
	}

	// Step 2: the read. This is the only network call in this method.
	outcome, venueOrderID, err := resolver.Resolve(ctx, orderID, unresolved.ClientOrderID)
	if err != nil {
		// Still unknown. UNKNOWN is not a state to be escalated out of; it is a
		// state to be resolved later, possibly by a different process. The
		// refusal is returned rather than swallowed so a caller cannot read it
		// as a rejection.
		return Resolved{
			OrderID:  orderID,
			State:    oms.StateUnknown,
			Attempts: history,
		}, ErrOutcomeUndetermined
	}

	switch outcome {
	case AcceptedOutcome, RejectedOutcome:
	default:
		// An answer the platform cannot interpret is undetermined in effect. The
		// venue has responded, but not in a way that responds to the question.
		return Resolved{
				OrderID:  orderID,
				State:    oms.StateUnknown,
				Attempts: history,
			}, fmt.Errorf("%w: the venue reported outcome %q, which is not one of ACCEPTED or REJECTED; "+
				"an unrecognised answer cannot resolve an order", ErrOutcomeUndetermined, outcome)
	}

	// Step 3: the commit. The submission record and the OMS transition move
	// together, so a reader never sees the order resolved while the submission
	// still says the outcome is unknown, or the reverse.
	resolvedSubID, resolvedState, err := c.commitResolution(ctx, orderID, actorID, outcome, venueOrderID)
	if err != nil {
		return Resolved{}, err
	}

	rewritten, err := c.store.History(ctx, orderID)
	if err != nil {
		return Resolved{}, err
	}

	return Resolved{
		OrderID:      orderID,
		SubmissionID: resolvedSubID,
		// The state that committed, not the one this function meant to commit.
		// Returning an intended value would let a caller act on a transition the
		// database never accepted.
		State:        resolvedState,
		Outcome:      outcome,
		VenueOrderID: venueOrderID,
		Attempts:     rewritten,
	}, nil
}

// commitResolution writes the resolved outcome and the order's transition in one
// transaction.
func (c *Command) commitResolution(ctx context.Context, orderID, actorID string,
	outcome Outcome, venueOrderID string) (contracts.ID, oms.State, error) {

	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: begin resolution transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// The order must actually be UNKNOWN. Reading it rather than assuming means a
	// concurrent resolution is refused here instead of writing a transition the
	// state machine does not permit.
	//
	// The correlation id, environment and policy revisions come from the same row
	// rather than from the caller. Resolution is invoked by a reconciler that did
	// not originate the order, so anything it supplied would be a second, possibly
	// contradicting, account of the same command.
	var fromState, correlationID, environment, policyVersion string
	if err := tx.QueryRowContext(ctx, `
		SELECT state::text,
		       correlation_id,
		       environment::text,
		       coalesce(risk_policy_revisions[cardinality(risk_policy_revisions)], '')
		  FROM oms."order" WHERE order_id=$1 FOR UPDATE`, orderID).
		Scan(&fromState, &correlationID, &environment, &policyVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: order %s does not exist", orderID)
		}
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: reading the order failed: %w", err)
	}
	if fromState != string(oms.StateUnknown) {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: order %s is in %s, not UNKNOWN; only an order whose outcome "+
			"was never determined can be resolved from the venue record", orderID, fromState)
	}

	// Doc 22 requires policy_version on every audit record, and doc 21 makes it
	// part of the decision tuple. The order's own risk policy revisions are the
	// honest source: the last one is the policy this order is currently held under.
	//
	// An order with no recorded revision is refused rather than given a placeholder.
	// An order that reached the risk gate without recording the policy that admitted
	// it has an evidence gap, and resolving it would add another record to a chain
	// that already cannot explain how the order was admitted.
	if policyVersion == "" {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: order %s records no risk policy "+
			"revision, so there is no policy version to attribute the resolution to; an order that reached "+
			"the risk gate without recording the policy that admitted it cannot be audited", orderID)
	}

	to := oms.StateAcknowledged
	if outcome == RejectedOutcome {
		to = oms.StateRejected
	}

	seq, err := c.nextSequence(ctx, tx, orderID)
	if err != nil {
		return contracts.ID{}, oms.StateUnknown, err
	}
	eventID, err := contracts.NewID(contracts.EntityOrderEvent)
	if err != nil {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: minting an order event id failed: %w", err)
	}
	// Truncated to microseconds before anything derives a nanosecond count from it.
	//
	// The audit appender derives occurred_at_ns from this time directly, and
	// PostgreSQL stores timestamptz at microsecond resolution, so a nanosecond-precise
	// Go time produces an ns that disagrees with the timestamp it accompanies and
	// the database rejects the row with audit_record_occurred_at_ns_bound. Callers
	// of Prepare supply an already-truncated OccurredAt; Resolve mints its own, so
	// it truncates here rather than assuming the discipline it cannot check.
	now := time.Now().UTC().Truncate(time.Microsecond)
	ns := now.UnixNano()

	// The correlation id is the order's own, so the resolution joins the same
	// evidence trail as the submission it settles. It is deliberately not the venue
	// reference: that identifies the venue's record, not this platform's command.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO oms.order_event (
			order_event_id, order_id, sequence, from_state, to_state, event_kind,
			filled_quantity_delta, price, actor_id, reason, correlation_id,
			occurred_at, occurred_at_ns)
		VALUES ($1,$2,$3,$4::oms.order_state,$5::oms.order_state,$6::oms.order_event_kind,
		        0,NULL,$7,$8,$9,common.ns_to_timestamptz($10),$10)`,
		eventID.String(), orderID, seq, fromState, string(to),
		string(oms.EventOutcomeResolved),
		actorID, "resolved from the venue record", correlationID, ns); err != nil {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: journalling the resolution failed: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE oms."order"
		   SET state = $2::oms.order_state, version_vector = version_vector + 1
		 WHERE order_id = $1`, orderID, string(to)); err != nil {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: advancing the resolved order failed: %w", err)
	}

	// The submission row is rewritten in place rather than appended. It is the same
	// submission: what changed is what the venue has since said about it, and a
	// second row would present one send as two attempts -- which is precisely the
	// counting the no-retry rule depends on.
	var subID string
	if err := tx.QueryRowContext(ctx, `
		UPDATE execution.submission
		   SET outcome = $2,
		       response_received_at = common.ns_to_timestamptz($3),
		       venue_order_ref = COALESCE(NULLIF($4,''), venue_order_ref)
		 WHERE order_id = $1
		   AND outcome = 'TIMED_OUT_UNKNOWN'
		RETURNING submission_id`,
		orderID, string(storedFor(outcome)), ns, venueOrderID).Scan(&subID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The precondition read said something was undetermined, and this
			// UPDATE finds nothing. That means the row changed between the two --
			// a concurrent resolution -- and writing the transition anyway would
			// move an order on the strength of an outcome no longer outstanding.
			return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: order %s had no TIMED_OUT_UNKNOWN submission to resolve; "+
				"it was resolved concurrently", orderID)
		}
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: recording the resolved outcome failed: %w", err)
	}
	resolvedSubmission, err := contracts.ParseID(subID)
	if err != nil {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: stored submission id %s does not parse: %w", subID, err)
	}

	// The audit record, in this transaction rather than one of its own. A
	// resolution that moved the order without leaving an audit record would leave a
	// state change with no evidence -- the exact gap doc 22 exists to close. An
	// order whose outcome flips from undetermined to accepted or refused is a
	// change in whether the platform holds exposure, and that is not something to
	// record anywhere less durable than the order's own history.
	auditID, err := contracts.NewID(contracts.EntityAudit)
	if err != nil {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: minting an audit id failed: %w", err)
	}
	reason := "submission outcome resolved from the venue record"
	details, err := json.Marshal(map[string]any{
		"order_id":        orderID,
		"submission_id":   resolvedSubmission.String(),
		"resolved_to":     string(outcome),
		"prior_outcome":   string(storedFor(UndeterminedOutcome)),
		"venue_order_ref": venueOrderID,
	})
	if err != nil {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: encoding audit details failed: %w", err)
	}
	if _, err := c.appender.AppendIn(ctx, tx, audit.Record{
		AuditID:            auditID.String(),
		TenantOrOwnerScope: "trading",
		ActorID:            actorID,
		ActorType:          contracts.ActorWorkload,
		Action:             "ORDER_OUTCOME_RESOLVED",
		TargetType:         "ORDER",
		TargetID:           orderID,
		Environment:        contracts.Environment(environment),
		OccurredAt:         now,
		RecordedAt:         now,
		Reason:             &reason,
		CorrelationID:      correlationID,
		PolicyVersion:      policyVersion,
		Result:             contracts.AuditResultSuccess,
		Details:            string(details),
		SigningKeyID:       c.signingKey,
	}); err != nil {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: the resolution was not audited, so "+
			"the transaction is abandoned: %w", err)
	}

	// The outbox row. A resolution is a committed state mutation, and doc 05 says
	// that always has one. Without it the downstream consumers that learn about an
	// order's fate from the outbox would never hear that it stopped being
	// undetermined, and would keep treating it as in flight.
	if _, err := c.enqueue(ctx, tx, outboxEnvelope{
		EventType: "ORDER_OUTCOME_RESOLVED",
		Payload: map[string]any{
			"order_id":        orderID,
			"submission_id":   resolvedSubmission.String(),
			"resolved_to":     string(outcome),
			"venue_order_ref": venueOrderID,
			"resolved_by":     actorID,
		},
		AggregateID:   orderID,
		Sequence:      seq,
		CorrelationID: correlationID,
		CausationID:   resolvedSubmission.String(),
		Environment:   contracts.Environment(environment),
		OccurredAt:    now,
	}); err != nil {
		return contracts.ID{}, oms.StateUnknown, err
	}

	// Bind the audit record to the OMS event, closing the link between the state
	// machine and the evidence for it.
	if _, err := tx.ExecContext(ctx,
		`UPDATE oms.order_event SET audit_id=$1 WHERE order_event_id=$2`,
		auditID.String(), eventID.String()); err != nil {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: binding the audit record to the "+
			"order event failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return contracts.ID{}, oms.StateUnknown, fmt.Errorf("execution: the resolution did not commit: %w", err)
	}
	committed = true
	return resolvedSubmission, to, nil
}
