package venue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/domain/execution"
	"github.com/aitc/trade/domain/oms"
	"github.com/aitc/trade/services/dispatch"
)

// The send site.
//
// Prepare commits the intent and refuses an uncertified adapter, but that check is
// not sufficient on its own. Between Prepare's commit and the dispatcher's call the
// certification can be withdrawn -- by a person, because the venue changed its API,
// its permissions, or its contract (doc 16 §6). An order prepared at 09:00 under a
// valid LIVE_ELIGIBLE certificate and sent at 09:04 after it was revoked must not
// reach the venue. So the gate is consulted HERE, immediately before the call, and
// this is the check that actually closes that window. Prepare's is an early refusal
// that avoids creating durable intent for something unsendable; this one is
// authoritative.
//
// # Why a failure after the send is swallowed
//
// Once writer.Submit has been called, this method returns nil even if recording the
// answer fails. That is deliberate and it is the single most important line in the
// file.
//
// A non-nil return makes the dispatcher increment the attempt count and, up to
// max_attempts, call Publish again -- which calls Submit again. Against a venue that
// does not guarantee client-order-id idempotency that is a DUPLICATE ORDER. Doc 16 §4
// is explicit that the consequences must differ by idempotency, and that is why the
// durable submission record exists: the alternative to a blind retry is a submission
// left PENDING, which UndeterminedPrior reads as undetermined, which makes every
// later attempt for that order require reconciliation and an authorised retry. That
// is a wedged order needing a human, which is recoverable. A duplicate order is not.
//
// So the failure mode after a send is deliberately the safe one: undetermined, no
// retry, discoverable by reconciliation.

// ErrSubmitNotRoutable means an event this publisher does not handle arrived.
//
// It is dispatch.ErrUnroutable rather than a local error so the dispatcher
// dead-letters it immediately instead of retrying a routing gap five times. A
// publisher that cannot place an order is not the place to improvise one.
var ErrSubmitNotRoutable = dispatch.ErrUnroutable

// SubmitEventType is the outbox event this publisher consumes.
const SubmitEventType = "ORDER_SUBMIT_REQUESTED"

// AnswerApplier applies the venue's answer to the OMS.
//
// It is an interface rather than a *execution.Command so this package depends on the
// one behaviour it needs. A wider dependency would let the send site acquire the
// ability to prepare and resolve orders, and both of those are decisions a different
// component is supposed to be making.
type AnswerApplier interface {
	ApplyVenueAnswer(ctx context.Context, a execution.VenueAnswer) (execution.Answered, error)
}

// SubmitPublisher places orders on a venue.
//
// It implements dispatch.Publisher for exactly one event type. Anything else is
// unroutable rather than silently accepted, because a publisher that reports success
// for an event it did not deliver is worse than one that admits the gap.
type SubmitPublisher struct {
	db      *sql.DB
	writer  Writer
	gate    *SubmissionGate
	answers AnswerApplier

	// actorID is recorded on the answer's audit record. It is required: doc 22 wants
	// an actor on every record, and "the adapter observed it" is an answer, but an
	// unnamed one.
	actorID string

	now func() time.Time
}

// NewSubmitPublisher returns a publisher sending through writer.
//
// db is used to load the order and its venue symbol, because the platform's
// instrument identity is not the venue's and the mapping between them is an audited,
// versioned record rather than something an adapter may improvise per call.
func NewSubmitPublisher(db *sql.DB, writer Writer, gate *SubmissionGate,
	answers AnswerApplier, actorID string) (*SubmitPublisher, error) {

	if db == nil {
		return nil, errors.New("venue: a submit publisher needs a database handle to read the order")
	}
	if writer == nil {
		return nil, errors.New("venue: a submit publisher needs a writer; without one it would " +
			"mark orders submitted without submitting them")
	}
	if gate == nil {
		return nil, errors.New("venue: a submit publisher needs the certification gate; this is the " +
			"only place a certification is checked close enough to the call to stop a withdrawal " +
			"landing between Prepare and the send")
	}
	if answers == nil {
		return nil, errors.New("venue: a submit publisher needs something to apply the venue's answer; " +
			"an order the venue acknowledged must leave SUBMITTING")
	}
	if actorID == "" {
		return nil, errors.New("venue: a submit publisher needs an actor identity for the answer it records")
	}
	return &SubmitPublisher{
		db: db, writer: writer, gate: gate, answers: answers, actorID: actorID,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// WithClock replaces the clock. Unexported: it is a test seam.
func (p *SubmitPublisher) WithClock(f func() time.Time) *SubmitPublisher {
	p.now = f
	return p
}

// SubmitPublisher must satisfy the dispatcher's contract.
var _ dispatch.Publisher = (*SubmitPublisher)(nil)

// submitPayload is the ORDER_SUBMIT_REQUESTED body written by execution.Prepare.
type submitPayload struct {
	SubmissionID    string `json:"submission_id"`
	OrderID         string `json:"order_id"`
	VenueID         string `json:"venue_id"`
	AdapterID       string `json:"adapter_id"`
	ClientOrderID   string `json:"client_order_id"`
	VenueIdempotent bool   `json:"venue_idempotent"`
	RequestDigest   string `json:"request_digest"`
}

// Publish submits one order, or refuses before contacting the venue.
func (p *SubmitPublisher) Publish(ctx context.Context, m dispatch.Message) error {
	if m.EventType != SubmitEventType {
		return fmt.Errorf("%w: %s is not an order submission", ErrSubmitNotRoutable, m.EventType)
	}

	var in submitPayload
	if err := json.Unmarshal(m.Payload, &in); err != nil {
		// Nothing has been sent. Returning the error is safe and correct: the
		// dispatcher may retry, and a retry cannot duplicate an order that was
		// never placed.
		return fmt.Errorf("venue: the %s payload is unreadable, so nothing was sent: %w", m.EventType, err)
	}
	submissionID, err := contracts.ParseID(in.SubmissionID)
	if err != nil {
		return fmt.Errorf("venue: the payload names submission %q, which does not parse: %w",
			in.SubmissionID, err)
	}
	if in.OrderID == "" || in.ClientOrderID == "" || in.AdapterID == "" {
		return fmt.Errorf("venue: the payload is missing an order id, client order id or adapter id; " +
			"nothing was sent")
	}

	// The gate, immediately before the call. Built once and both consulted and used
	// for the state mapping, so the declaration that authorises the send is the same
	// one that translates the venue's answer.
	gate, err := p.gate.GateFor(ctx, in.AdapterID, m.Environment)
	if err != nil {
		return fmt.Errorf("venue: %s may not submit in %s, and nothing was sent: %w",
			in.AdapterID, m.Environment, err)
	}
	if err := gate.Permit(CapabilitySubmit); err != nil {
		return fmt.Errorf("venue: %s may not submit in %s, and nothing was sent: %w",
			in.AdapterID, m.Environment, err)
	}

	req, err := p.buildRequest(ctx, in, m.Environment)
	if err != nil {
		// Still before the call, so a retry is safe.
		return err
	}

	// Everything above refused without sending. Everything below has sent.
	result, submitErr := p.writer.Submit(ctx, req)

	outcome := interpretResult(gate.Registry(), result, submitErr)

	// From here the venue may hold this order, so nothing below may return an error
	// that would provoke a retry.
	if _, err := p.answers.ApplyVenueAnswer(ctx, execution.VenueAnswer{
		OrderID:        in.OrderID,
		SubmissionID:   submissionID,
		Outcome:        outcome,
		VenueOrderRef:  result.VenueOrderRef,
		ResponseDigest: responseDigest(result, outcome),
		ActorID:        p.actorID,
		ObservedAt:     p.now().UTC().Truncate(time.Microsecond),
	}); err != nil {
		// Swallowed on purpose; see the file comment. The submission stays PENDING,
		// which is undetermined, which blocks an unauthorised retry and hands the
		// order to reconciliation. That is the recoverable outcome. Returning err
		// here would trade it for a duplicate order.
		_ = err
	}
	return nil
}

// buildRequest reads the order and its venue symbol.
//
// The order is read rather than taken from the payload because the payload carries a
// digest, not the order: doc 16 §4 stores a digest precisely because the raw payload
// may carry credentials. The digest proves what was intended; the row is the order.
func (p *SubmitPublisher) buildRequest(ctx context.Context, in submitPayload,
	env contracts.Environment) (SubmitRequest, error) {

	var (
		instrumentID, side, orderType, tif string
		quantityText, limitText            sql.NullString
	)
	if err := p.db.QueryRowContext(ctx, `
		SELECT instrument_id, side::text, order_type::text, time_in_force::text,
		       quantity::text, limit_price::text
		  FROM oms."order"
		 WHERE order_id = $1`, in.OrderID).
		Scan(&instrumentID, &side, &orderType, &tif, &quantityText, &limitText); err != nil {
		return SubmitRequest{}, fmt.Errorf("venue: order %s could not be read, so nothing was sent: %w",
			in.OrderID, err)
	}

	quantity, err := contracts.ParseDecimal(quantityText.String)
	if err != nil {
		return SubmitRequest{}, fmt.Errorf("venue: order %s has quantity %q, which is not a decimal, "+
			"so nothing was sent: %w", in.OrderID, quantityText.String, err)
	}
	var limit *contracts.Decimal
	if limitText.Valid {
		v, err := contracts.ParseDecimal(limitText.String)
		if err != nil {
			return SubmitRequest{}, fmt.Errorf("venue: order %s has limit price %q, which is not a "+
				"decimal, so nothing was sent: %w", in.OrderID, limitText.String, err)
		}
		limit = &v
	}

	// The symbol comes from the audited mapping, not from the instrument's own name.
	//
	// market.venue_symbol is versioned and append-only precisely because a mapping
	// that can be rewritten after taking effect would let an order reach a different
	// instrument than the strategy reasoned about. Reading it here, as the highest
	// version that is already effective and not retired, is what makes the mapping
	// authoritative rather than advisory.
	var symbol string
	if err := p.db.QueryRowContext(ctx, `
		SELECT symbol
		  FROM market.venue_symbol
		 WHERE instrument_id = $1 AND venue_id = $2
		   AND effective_at <= $3
		   AND (retired_at IS NULL OR retired_at > $3)
		 ORDER BY mapping_version DESC
		 LIMIT 1`, instrumentID, in.VenueID, p.now().UTC()).Scan(&symbol); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SubmitRequest{}, fmt.Errorf("venue: instrument %s has no effective %s symbol "+
				"mapping, so the order cannot be addressed and nothing was sent; an instrument with "+
				"no mapping is not translatable and guessing one is how an order reaches the wrong "+
				"instrument", instrumentID, in.VenueID)
		}
		return SubmitRequest{}, fmt.Errorf("venue: reading the symbol mapping for %s failed, so "+
			"nothing was sent: %w", instrumentID, err)
	}

	return SubmitRequest{
		ClientOrderID:  in.ClientOrderID,
		InstrumentID:   instrumentID,
		VenueSymbol:    symbol,
		Side:           contracts.Side(side),
		OrderType:      contracts.OrderType(orderType),
		TimeInForce:    contracts.TimeInForce(tif),
		Quantity:       quantity,
		LimitPrice:     limit,
		IdempotencyKey: in.ClientOrderID,
	}, nil
}

// interpretResult maps what the venue said onto Go's outcome vocabulary.
//
// The ordering of the cases is the whole content of this function, so it is spelled
// out rather than left to the reader:
//
//  1. A transport error is UNDETERMINED, never REJECTED. The platform cannot know
//     whether the request reached the venue, and a rejected order is terminal -- so
//     recording REJECTED here would tell the database no exposure exists on the
//     strength of a network hiccup.
//  2. A timeout is UNDETERMINED, for the same reason. SubmitResult.TimedOut is not a
//     transport error; doc 16 §4 names it explicitly.
//  3. A determinate venue state is mapped through the adapter's own declared StateMap,
//     so the venue's vocabulary is never compared against the OMS's.
//  4. An unmappable state is UNDETERMINED, not a guess. Registry.MapState refuses, and
//     the refusal is the answer.
func interpretResult(reg Registry, result SubmitResult, submitErr error) execution.Outcome {
	if submitErr != nil {
		return execution.UndeterminedOutcome
	}
	if result.TimedOut {
		return execution.UndeterminedOutcome
	}
	mapped, err := reg.MapState(result.State)
	if err != nil {
		// Refused by the adapter's own map. Undetermined is the honest reading: the
		// venue answered, in a vocabulary this adapter does not claim to understand.
		return execution.UndeterminedOutcome
	}
	switch mapped {
	case oms.StateAcknowledged, oms.StatePartiallyFilled, oms.StateFilled:
		return execution.AcceptedOutcome
	case oms.StateRejected, oms.StateExpired:
		return execution.RejectedOutcome
	default:
		return execution.UndeterminedOutcome
	}
}

// responseDigest is the redacted reference to what the venue answered with.
//
// It is a digest of the fields rather than the raw body for the same reason the
// request digest is: the raw payload may carry venue or account detail. It is never
// empty, because ApplyVenueAnswer refuses an answer that cannot say what came back.
func responseDigest(result SubmitResult, outcome execution.Outcome) string {
	body, err := json.Marshal(map[string]any{
		"venue_order_ref": result.VenueOrderRef,
		"venue_state":     result.State,
		"timed_out":       result.TimedOut,
		"interpreted_as":  string(outcome),
	})
	if err != nil {
		// A map of strings and a bool cannot fail to marshal, so this is
		// unreachable; returning the digest of the empty marker keeps the answer
		// recordable rather than refusing to record it.
		return execution.RequestDigest([]byte("unencodable-result"))
	}
	return execution.RequestDigest(body)
}
