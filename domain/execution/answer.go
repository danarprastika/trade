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

// Applying a venue's answer to a submission.
//
// Doc 16 §4: a submission outcome is what the venue reports, and doc 04 makes the
// OMS the authority on what the platform believes. Between them there was a hole.
// Prepare advanced the order to SUBMITTING and wrote a PENDING submission; Resolve
// settled an order already sitting in UNKNOWN; and nothing in between ever read the
// venue's answer off the wire. An order the venue accepted therefore stayed in
// SUBMITTING for the rest of its life, with a submission row that said PENDING
// forever -- which UndeterminedPrior reads as undetermined, so every later submit
// attempt for that order was refused as unresolved. A single successful submission
// wedged its own order permanently.
//
// This is the missing transition. It is deliberately NOT folded into Prepare: the
// venue call happens after Prepare commits, and its answer arrives later still, so
// no transaction can contain both. What this method does contain is everything
// else -- the journal, the state, the submission row, the audit record and the
// outbox row -- because those five are one statement about one observation, and
// splitting them is what produced the wedge.

// VenueAnswer is what a venue said about one submission.
type VenueAnswer struct {
	OrderID      string
	SubmissionID contracts.ID

	// Outcome is the answer in Go's vocabulary. UndeterminedOutcome is the
	// fail-closed member: it is the only honest recording of a timeout, and it must
	// never be smoothed into RejectedOutcome. See storedFor.
	Outcome Outcome

	// VenueOrderRef is the venue's own reference. It is recorded when present and
	// ignored when absent, because a venue that returns no reference has still
	// answered; refusing the whole answer over a missing identifier would discard a
	// determinate outcome to preserve a convenience field.
	VenueOrderRef string

	// ResponseDigest is the redacted reference to the bytes the venue answered with.
	// It is required: a record that cannot say what was received cannot prove what
	// was decided, and the whole value of this transition is that it is evidence
	// rather than bookkeeping.
	ResponseDigest string

	// ActorID is who observed the answer. Required rather than defaulted to the
	// adapter's name, for the reason Resolve requires one: doc 22 requires an actor
	// on every record, and an observation with no accountable observer is not
	// evidence of anything.
	ActorID string

	// ObservedAt is when the venue answered, not when this method ran. Truncated to
	// microseconds before anything derives a nanosecond count from it, because
	// PostgreSQL stores timestamptz at microsecond resolution and a nanosecond-precise
	// Go time produces an ns that disagrees with the timestamp it accompanies, which
	// audit_record_occurred_at_ns_bound rejects.
	ObservedAt time.Time
}

// Answered is what the commit actually did.
type Answered struct {
	OrderID      string
	SubmissionID contracts.ID

	// State is the state that committed, not the one this method intended to
	// commit. Returning an intended value would let a caller act on a transition
	// the database refused, which is the same reason Resolve returns the committed
	// state rather than the mapped one.
	State oms.State

	Outcome       Outcome
	VenueOrderRef string
}

// ErrAnswerNotApplicable means the order is not in a state that a venue answer can
// move.
//
// It is a refusal rather than a tolerated no-op because a second answer to an
// already-answered order is not harmless bookkeeping: the submission row has been
// rewritten, and re-applying would overwrite a determinate record with whatever
// the venue happens to say on a second observation.
var ErrAnswerNotApplicable = errors.New("execution: this order is not awaiting a venue answer, so the " +
	"answer cannot be applied; only a SUBMITTING order can move on the venue's first answer")

// ApplyVenueAnswer records the venue's answer and moves the order, in one transaction.
//
// The order of operations matches Prepare and Resolve and is not a preference. The
// order is read FOR UPDATE before anything is written, so a refusal happens before
// any mutation; the journal is written before the row moves, because the journal is
// the authority and the row is a projection of it.
func (c *Command) ApplyVenueAnswer(ctx context.Context, a VenueAnswer) (Answered, error) {
	if err := validateAnswer(a); err != nil {
		return Answered{}, err
	}

	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return Answered{}, fmt.Errorf("execution: begin the venue answer transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// The order's own account of itself. Correlation id, environment, policy
	// revision and risk decision are read from the row rather than taken from the
	// caller: the caller is the venue adapter, which observed an outcome but did not
	// originate the order, so anything it supplied would be a second and possibly
	// contradicting account of the same command.
	var (
		fromState, correlationID, environment, policyVersion string
		riskDecisionID                                       sql.NullString
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT state::text, correlation_id, environment::text,
		       coalesce(risk_policy_revisions[cardinality(risk_policy_revisions)], ''),
		       risk_decision_id
		  FROM oms."order" WHERE order_id = $1 FOR UPDATE`, a.OrderID).
		Scan(&fromState, &correlationID, &environment, &policyVersion, &riskDecisionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Answered{}, fmt.Errorf("execution: order %s does not exist, so it has no answer to apply", a.OrderID)
		}
		return Answered{}, fmt.Errorf("execution: reading the order failed: %w", err)
	}

	if fromState != string(oms.StateSubmitting) {
		return Answered{}, fmt.Errorf("%w: order %s is in %s", ErrAnswerNotApplicable, a.OrderID, fromState)
	}

	// The same refusal Resolve makes, for the same reason. Doc 22 requires a policy
	// version on every audit record, and an order that reached the risk gate without
	// recording the policy that admitted it has an evidence gap that this transition
	// would add to rather than close.
	if policyVersion == "" {
		return Answered{}, fmt.Errorf("execution: order %s records no risk policy revision, so there is no "+
			"policy version to attribute the venue answer to", a.OrderID)
	}

	// oms.state_transition_rule marks SUBMITTING -> ACKNOWLEDGED as requiring a
	// durable risk decision, and the trigger refuses the move when the order row's
	// risk_decision_id is null. Refusing here, with this message, rather than
	// letting the trigger raise an opaque constraint error: the difference between
	// "this order carries no risk decision" and "some CHECK failed" is the
	// difference between a fixable record and an incident.
	if a.Outcome == AcceptedOutcome && !riskDecisionID.Valid {
		return Answered{}, fmt.Errorf("execution: order %s would move to ACKNOWLEDGED, which requires a "+
			"durable risk decision, but it carries no risk_decision_id "+
			"(09_TESTING_AND_RELEASE_EVIDENCE.md invariant 1)", a.OrderID)
	}

	to, kind := answerTransition(a.Outcome)

	seq, err := c.nextSequence(ctx, tx, a.OrderID)
	if err != nil {
		return Answered{}, err
	}
	eventID, err := contracts.NewID(contracts.EntityOrderEvent)
	if err != nil {
		return Answered{}, fmt.Errorf("execution: minting an order event id failed: %w", err)
	}

	now := a.ObservedAt.UTC().Truncate(time.Microsecond)
	ns := now.UnixNano()

	// The journal first. The reverse order would let the row advance with no event
	// declaring it, and the trigger that requires a backing event exists precisely
	// because a state is meant to be reachable only through its event.
	var riskArg any
	if riskDecisionID.Valid {
		riskArg = riskDecisionID.String
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO oms.order_event (
			order_event_id, order_id, sequence, from_state, to_state, event_kind,
			filled_quantity_delta, price, actor_id, reason, correlation_id,
			risk_decision_id, occurred_at, occurred_at_ns)
		VALUES ($1,$2,$3,$4::oms.order_state,$5::oms.order_state,$6::oms.order_event_kind,
		        0,NULL,$7,$8,$9,$10,common.ns_to_timestamptz($11),$11)`,
		eventID.String(), a.OrderID, seq, fromState, string(to), string(kind),
		a.ActorID, answerReason(a.Outcome, a.VenueOrderRef), correlationID, riskArg, ns); err != nil {
		return Answered{}, fmt.Errorf("execution: journalling the venue answer failed: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE oms."order"
		   SET state = $2::oms.order_state, version_vector = version_vector + 1
		 WHERE order_id = $1`, a.OrderID, string(to)); err != nil {
		return Answered{}, fmt.Errorf("execution: advancing the order from SUBMITTING failed: %w", err)
	}

	// The submission row moves in the same transaction, guarded on still being
	// PENDING. A missing row here means another writer answered first, and updating
	// anyway would move the order on a second answer.
	answeredOrderID, err := c.store.AnswerIn(ctx, tx, a.SubmissionID, a.Outcome,
		a.VenueOrderRef, a.ResponseDigest, now, ns)
	if err != nil {
		if errors.Is(err, ErrNoSubmissionRecord) {
			return Answered{}, fmt.Errorf("%w: %w", ErrAnswerNotApplicable, err)
		}
		return Answered{}, err
	}
	// The submission row and the order row must describe the same order. A mismatched
	// pair is not a race, it is a caller error, and applying an answer to the
	// submission while moving a different order is exactly the kind of split this
	// method exists to make impossible.
	if answeredOrderID != a.OrderID {
		return Answered{}, fmt.Errorf("execution: submission %s belongs to order %s, not %s",
			a.SubmissionID, answeredOrderID, a.OrderID)
	}

	auditID, err := contracts.NewID(contracts.EntityAudit)
	if err != nil {
		return Answered{}, fmt.Errorf("execution: minting an audit id failed: %w", err)
	}
	reason := answerReason(a.Outcome, a.VenueOrderRef)
	details, err := json.Marshal(map[string]any{
		"order_id":         a.OrderID,
		"submission_id":    a.SubmissionID.String(),
		"answered_to":      string(a.Outcome),
		"to_state":         string(to),
		"event_kind":       string(kind),
		"venue_order_ref":  a.VenueOrderRef,
		"response_digest":  a.ResponseDigest,
		"adapter_observed": true,
	})
	if err != nil {
		return Answered{}, fmt.Errorf("execution: encoding audit details failed: %w", err)
	}
	if _, err := c.appender.AppendIn(ctx, tx, audit.Record{
		AuditID:            auditID.String(),
		TenantOrOwnerScope: "trading",
		ActorID:            a.ActorID,
		ActorType:          contracts.ActorWorkload,
		Action:             "ORDER_SUBMIT_ANSWERED",
		TargetType:         "ORDER",
		TargetID:           a.OrderID,
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
		return Answered{}, fmt.Errorf("execution: the venue answer was not audited, so the "+
			"transaction is abandoned: %w", err)
	}

	if _, err := c.enqueue(ctx, tx, outboxEnvelope{
		EventType: "ORDER_SUBMIT_ANSWERED",
		Payload: map[string]any{
			"order_id":        a.OrderID,
			"submission_id":   a.SubmissionID.String(),
			"answered_to":     string(a.Outcome),
			"to_state":        string(to),
			"venue_order_ref": a.VenueOrderRef,
		},
		AggregateID:   a.OrderID,
		Sequence:      seq,
		CorrelationID: correlationID,
		CausationID:   a.SubmissionID.String(),
		Environment:   contracts.Environment(environment),
		OccurredAt:    now,
	}); err != nil {
		return Answered{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE oms.order_event SET audit_id = $1 WHERE order_event_id = $2`,
		auditID.String(), eventID.String()); err != nil {
		return Answered{}, fmt.Errorf("execution: binding the audit record to the order event failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Answered{}, fmt.Errorf("execution: the venue answer did not commit: %w", err)
	}
	committed = true

	return Answered{
		OrderID:       a.OrderID,
		SubmissionID:  a.SubmissionID,
		State:         to,
		Outcome:       a.Outcome,
		VenueOrderRef: a.VenueOrderRef,
	}, nil
}

// answerTransition maps an outcome onto the OMS state and event kind it may reach.
//
// Every member of Outcome appears, and there is a default as well — which is not the
// same as saying the default is unreachable. It IS unreachable today, because
// validateAnswer refuses any outcome outside the three before this is called. The
// default is written anyway, and deliberately returns UNKNOWN, because that is the
// direction that cannot lie: a fourth Outcome member added to the vocabulary in
// future would fall here, and an order would be marked as of unknown fate rather than
// moved to ACKNOWLEDGED or REJECTED on the strength of a case nobody wrote. Every
// available state is a claim about exposure, and UNKNOWN is the only one of them that
// claims nothing.
//
// UndeterminedOutcome goes to UNKNOWN, which is not a failure state — it is the
// accurate one, and it is the only state from which a later resolution can produce a
// determinate record.
func answerTransition(o Outcome) (oms.State, oms.EventKind) {
	switch o {
	case AcceptedOutcome:
		return oms.StateAcknowledged, oms.EventAcknowledged
	case RejectedOutcome:
		return oms.StateRejected, oms.EventRejectedByVenue
	default:
		return oms.StateUnknown, oms.EventUnknownOutcome
	}
}

// answerReason names what happened, in words an operator can act on.
//
// The UNKNOWN wording is deliberate and is the most important string in this file:
// the venue not answering is not the venue refusing, and an order sitting in UNKNOWN
// must never be mistaken for one the venue rejected.
func answerReason(o Outcome, venueOrderRef string) string {
	ref := venueOrderRef
	if ref == "" {
		ref = "the venue gave no order reference"
	} else {
		ref = "venue order " + ref
	}
	switch o {
	case AcceptedOutcome:
		return "the venue acknowledged the submission as " + ref
	case RejectedOutcome:
		return "the venue rejected the submission"
	default:
		return "the venue did not determine the submission's outcome (" + ref +
			"); this is NOT a rejection and the order is UNKNOWN until an explicit resolution"
	}
}

func validateAnswer(a VenueAnswer) error {
	if a.OrderID == "" {
		return errors.New("execution: a venue answer needs an order identity")
	}
	if a.SubmissionID.String() == "" {
		return errors.New("execution: a venue answer needs a submission id; without one the answer " +
			"cannot be tied to the attempt it describes")
	}
	// The vocabulary is closed here rather than defaulted downstream. An unrecognised
	// outcome is undetermined in effect, and the only honest way to record that is
	// UndeterminedOutcome -- which is what the caller must say explicitly, so that
	// choosing it is a decision rather than an accident.
	switch a.Outcome {
	case AcceptedOutcome, RejectedOutcome, UndeterminedOutcome:
	default:
		return fmt.Errorf("execution: outcome %q is not one of ACCEPTED, REJECTED or UNDETERMINED; an "+
			"unrecognised answer is recorded as UNDETERMINED, which is not the same as being rejected",
			a.Outcome)
	}
	if a.ResponseDigest == "" {
		return errors.New("execution: a venue answer needs a response digest; a record that cannot say " +
			"what the venue returned cannot prove what was decided")
	}
	if a.ActorID == "" {
		return errors.New("execution: a venue answer needs an actor; an observation with no accountable " +
			"observer is not evidence")
	}
	if a.ObservedAt.IsZero() {
		return errors.New("execution: a venue answer needs the time the venue answered; defaulting to " +
			"the clock would make the durable record's timestamps unreproducible")
	}
	return nil
}
