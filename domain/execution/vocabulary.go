// Package execution drives a canonical order from RISK_APPROVED to a venue
// outcome, and is the only place in this repository permitted to do so.
//
// It exists because the rule that matters most here is a sequencing rule, and a
// sequencing rule cannot live in a state machine. Package oms knows which moves
// are legal; it cannot know whether this particular submit is a first attempt or
// a retry of one whose outcome was never determined. That distinction is the
// whole content of doc 16 §4 and doc 01 invariant 3:
//
//	A timeout after a possible submission yields OMS UNKNOWN; no
//	exposure-increasing retry occurs until the venue state is resolved or an
//	authorized recovery procedure explicitly determines the safe action.
//
// Nothing else can enforce it. The venue adapter cannot, because it is the thing
// whose timeout caused the problem. The risk gate cannot, because it evaluates
// an order that has not been submitted and knows nothing of prior attempts. The
// order row cannot, because a constraint sees one row and not the history of how
// many times it has already been sent.
//
// So the rule is enforced here, in one place, over the durable submission attempt
// history in execution.submission rather than over anything a caller supplied.
//
// # The two doors
//
// An order's life has exactly two durable commands, and both live here:
//
//	Command.Prepare   records the attempt, advances the order to SUBMITTING, and
//	                  commits the outbox row that owes the send -- all in one
//	                  transaction. It refuses if durable history holds an
//	                  undetermined attempt.
//	Command.Resolve   reads the venue's own record and settles the attempt.
//	                  It cannot place an order.
//
// Submission is outbox-driven: Prepare commits, and a dispatcher reads committed
// rows to make the call. The venue call therefore sits outside the command's
// transaction, which is what doc 05's transaction boundary requires -- and it is
// why a timeout is possible at all.
//
// An earlier version of this package also carried a Submitter that called the
// venue inline and kept the attempt history in a slice the caller passed. It was
// deleted rather than repaired, for three reasons: its history was whatever the
// caller felt like passing, so the no-retry rule could be bypassed by passing an
// empty slice; it wrote transitions through an interface and held no durable
// record, so an order could move with nothing committed to account for it; and it
// offered a second, weaker Resolve. Two implementations of a money-affecting
// lifecycle operation, one of them silently non-durable, is the failure this
// package exists to make impossible.
//
// # Fail closed
//
// Every error path in this package leaves the order in a state that cannot become
// exposure without a human or an explicit recovery procedure acting. In
// particular a submit whose outcome is unknown does not become REJECTED, does not
// become EXPIRED, and does not become eligible for resubmission -- it becomes
// UNKNOWN, which is the only honest description and which requires resolution
// before anything else may happen to it.
package execution

import "errors"

// Outcome is what a venue reported, or failed to report, about a submission.
//
// The three values are the whole of doc 16 §4's obligation. There is no
// "probably accepted" and no "assume rejected": an undetermined outcome is
// UndeterminedOutcome, and that is the safe direction because it keeps the order
// inside the machine rather than releasing it.
type Outcome string

const (
	// AcceptedOutcome means the venue acknowledged the order and holds it or has
	// begun working it.
	AcceptedOutcome Outcome = "ACCEPTED"

	// RejectedOutcome means the venue refused the order and holds no exposure
	// from it. This is the only submission outcome that returns the order's
	// capacity to the system without requiring reconciliation.
	RejectedOutcome Outcome = "REJECTED"

	// UndeterminedOutcome means the submission may or may not have reached the
	// venue. The order becomes UNKNOWN.
	UndeterminedOutcome Outcome = "UNDETERMINED"
)

// ErrOutcomeUndetermined is returned when a submission cannot be resolved and the
// order has therefore entered UNKNOWN.
//
// It is returned rather than swallowed so that a caller cannot mistake a
// timeout for a rejection. Every caller that treats it as an ordinary failure --
// by logging and moving on -- has just permitted the retry that doc 16 forbids.
var ErrOutcomeUndetermined = errors.New("execution: the venue outcome is undetermined and the order is in UNKNOWN")

// ErrResolutionRequired is returned when something is attempted on an order
// whose prior outcome was never determined.
//
// Doc 01 invariant 3: "No retry may transform an unknown external outcome into a
// second exposure-increasing command."
var ErrResolutionRequired = errors.New("execution: this order's previous submission outcome was never determined; " +
	"resolving the venue state or running an authorized recovery procedure is required before it may be submitted again")

// Attempt records one submission attempt and what became of it.
//
// This is the shape of a row in execution.submission. Store.History reads it from
// there rather than from anything a caller assembles, which is what makes the
// no-retry rule hold: a caller cannot hand in a history that omits an attempt.
type Attempt struct {
	// Index is the 1-based attempt number. It is part of the record rather than
	// derived so that the audit trail states which attempt produced a fill.
	Index int

	// ClientOrderID is the idempotency token sent to the venue, where the venue
	// supports it. Empty when the venue does not, which is itself a capability
	// difference the adapter must declare rather than simulate (doc 16 §4).
	ClientOrderID string

	// VenueOrderID is what the venue returned. Empty for an undetermined
	// outcome, which is the normal case for one and must not be invented.
	VenueOrderID string

	Outcome Outcome
}

// UndeterminedPrior returns the most recent attempt whose outcome was never
// determined, or nil if there is none.
//
// Only an attempt still carrying UndeterminedOutcome blocks. An attempt that was
// undetermined and has since been resolved by reconciliation does not block,
// which is the whole point of Resolve: resolving an order is what makes it
// submittable again.
//
// Every attempt is scanned rather than only the last. An undetermined attempt
// followed by a determinate one is unreachable through legal transitions -- the
// machine will not move a resolved order back -- so the case cannot arise, and
// the answer is the conservative one if it ever does: if any attempt anywhere in
// the history is undetermined, it is reported.
func UndeterminedPrior(prior []Attempt) *Attempt {
	for i := len(prior) - 1; i >= 0; i-- {
		if prior[i].Outcome == UndeterminedOutcome {
			return &prior[i]
		}
	}
	return nil
}
