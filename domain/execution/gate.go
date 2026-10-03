package execution

import (
	"context"
	"errors"

	"github.com/aitc/trade/contracts"
)

// The submission gate: the domain's side of doc 16 section 4's certification rule.
//
// This interface is declared here, in the package that needs it, rather than in
// adapters/venue where it is implemented. The direction matters. adapters/venue is
// the adapter layer; domain/execution is the control plane that decides whether an
// order may exist. If the control plane imported the adapter layer to learn what a
// gate looks like, then the domain's authority over a financial control would depend
// on a layer that exists to be substituted -- swapping an adapter would then change
// what the control plane is even capable of checking. Declaring the interface at
// the consumer keeps the dependency pointing inward, which is the only direction in
// which it is safe: adapters may be replaced, the rule may not.

// SubmissionGate reports whether a named adapter may submit an order in a named
// environment.
//
// It is parameterised by adapter and environment rather than bound to one of each
// because the submit command outlives any single adapter. A Command is a
// process-long singleton; the venues and environments it serves change while it
// runs. Binding the gate to one adapter at construction would mean a process
// serving two venues needed two gates, and the one holding the wrong adapter would
// be a live-refusal bug or, worse, a live-permit bug.
type SubmissionGate interface {
	PermitSubmit(ctx context.Context, adapterID string, env contracts.Environment) error
}

// ErrNoSubmissionGate means a submit command was constructed with no gate.
//
// It exists rather than defaulting, because the alternative is the failure this
// interface was added to close. Before it, the submit path consulted nothing: an
// uncertified adapter's orders were prepared and committed, an order was advanced
// to SUBMITTING, a PENDING submission row recorded a send that was owed, and the
// only thing standing between that and the venue was a certification record
// nothing read. If a missing gate means "permit", that whole path is back and the
// nil is invisible at the call site. So a Command cannot exist without one.
//
// The failure this prevents is specific and worth stating: an order prepared for an
// uncertified adapter is not merely un-sent. It is committed in SUBMITTING with a
// durable PENDING submission the platform believes is outstanding, and it will sit
// there looking like an order in flight until someone notices that no venue ever
// received it.
var ErrNoSubmissionGate = errors.New("execution: a submit command needs a submission gate; without one " +
	"an uncertified adapter's orders would be committed with a durable pending submission nobody " +
	"is authorised to send")

// ErrNoSigningKey means a component was constructed with no audit signing key.
//
// It is a sibling of ErrNoSubmissionGate rather than an ordinary argument check,
// because both answer the same question: is there anything here that can be held
// to account later? The gate answers who authorised the order; the signing key
// answers what attests to the record of it.
//
// audit.record.signing_key_id is NOT NULL, so an empty key does not fail loudly.
// It stores, it satisfies the schema, and it produces an audit record asserting
// that an order was prepared, resolved or had its outcome applied while naming
// nothing that vouches for it. Nothing revises that column afterwards -- the
// migrations contain no UPDATE against it, so the batch-closing key the column
// comment describes is never written back -- which means an empty value is not a
// placeholder awaiting completion. It is the final stored value, and the record
// stays unattributable for the life of the partition.
//
// domain/ledger already refused an empty key for exactly this reason. This
// sentinel brings the same refusal to the OMS path, which was writing the empty
// string on every submission, every resolution and every venue answer.
var ErrNoSigningKey = errors.New("execution: a submit command needs a signing key id; " +
	"audit.record.signing_key_id is NOT NULL, and an empty value stores successfully while naming " +
	"no key, leaving a record of the order that nothing attests to")

// releaseGate is the gate used where no adapter certification applies.
//
// It is exported under an unmistakable name rather than left nil so that a test, or
// a dev-mode caller, can reach the pre-gate behaviour deliberately and visibly
// instead of by omitting a dependency. It permits everything, which is why its name
// says what it does.
type ReleaseGate struct{}

// PermitSubmit permits unconditionally.
//
// It exists so that test fixtures and development entrypoints can construct a
// Command without standing up certification rows. It is deliberately not the zero
// value of anything and deliberately not reachable by accident: production wiring
// that reaches for this has opted out of the certification gate, which is a decision
// somebody should be making on purpose.
func (ReleaseGate) PermitSubmit(context.Context, string, contracts.Environment) error { return nil }
