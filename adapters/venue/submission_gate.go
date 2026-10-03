package venue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/domain/execution"
)

// The submission gate: the production caller that nothing had.
//
// Gate, Require, and LoadCertification were all correct and all unexercised. This
// file is what makes them part of the order path rather than a library, by
// resolving an adapter's persisted certification for a request and refusing the
// submit when that certification does not authorise it.

// ErrUnknownAdapter means no declaration was registered for an adapter id.
//
// It is a refusal and not a lookup miss that falls through to a default. An
// undeclared adapter has no StateMap, so nothing could translate the states it
// reports, and it has no declared capability set, so nothing could say what it is
// allowed to do. Permitting it would mean permitting an adapter nobody described.
var ErrUnknownAdapter = errors.New("venue: no declaration is registered for this adapter")

// SubmissionGate adapts persisted certification to the control plane's submission
// gate.
//
// It holds a source of declarations and a database rather than one Gate, because a
// Gate is bound to a single adapter and environment and the control plane is
// neither. Building the Gate per request is also what keeps the certification
// fresh: a Gate captured at startup would keep permitting a capability whose row
// was revoked an hour ago, and a revocation that does not take effect until the
// next deploy is not a revocation.
type SubmissionGate struct {
	decls Declarations
	certs CertificationQuerier
	now   func() time.Time
}

// SubmissionGate must satisfy the interface the control plane asks for. The
// assertion is here so the contract is checked by the compiler at this package's
// build rather than at whichever wiring site happens to connect them.
var _ execution.SubmissionGate = (*SubmissionGate)(nil)

// NewSubmissionGate returns a gate resolving declarations from decls and
// certification from certs.
//
// now is supplied rather than read from the clock inside, because certification
// expiry is a comparison against a point in time and a test that cannot choose that
// point cannot test an expiry.
func NewSubmissionGate(decls Declarations, certs CertificationQuerier, now func() time.Time) (*SubmissionGate, error) {
	if decls == nil {
		return nil, errors.New("venue: a submission gate needs a source of adapter declarations")
	}
	if certs == nil {
		return nil, errors.New("venue: a submission gate needs a certification reader; an in-process " +
			"declaration can assert an adapter is supported, and only the persisted row can establish " +
			"that anyone certified it")
	}
	if now == nil {
		return nil, errors.New("venue: a submission gate needs a clock; certification expiry is " +
			"evaluated against it")
	}
	return &SubmissionGate{decls: decls, certs: certs, now: now}, nil
}

// PermitSubmit reports whether adapterID may submit in env.
//
// The order is declaration, then certification, then capability, and it is not
// rearranged for convenience. Each step refuses on its own evidence: an adapter
// nobody declared, an adapter whose certification does not cover this environment,
// and an adapter whose certification covers the environment but omits SUBMIT are
// three different misconfigurations and an operator fixes them three different ways.
func (g *SubmissionGate) PermitSubmit(ctx context.Context, adapterID string,
	env contracts.Environment) error {

	if adapterID == "" {
		return fmt.Errorf("%w: a submit named no adapter, and certification is recorded per adapter",
			ErrUnknownAdapter)
	}
	if err := env.Validate(); err != nil {
		return fmt.Errorf("venue: cannot check the certification of %s for an invalid environment: %w",
			adapterID, err)
	}
	gate, err := g.GateFor(ctx, adapterID, env)
	if err != nil {
		return err
	}
	// Ask the gate the actual question. Building it is not consulting it: GateFor
	// returning without error says the adapter is declared and has certification
	// rows in this environment, which is not the same as being permitted to submit.
	// It is exactly the distinction that separates a revoked or expired adapter from
	// a working one, and both of those have rows.
	if err := gate.Permit(CapabilitySubmit); err != nil {
		return fmt.Errorf("venue: %s may not submit in %s: %w", adapterID, env, err)
	}
	return nil
}

// GateFor builds the Gate for one adapter in one environment.
//
// It is exported so a startup banner can report what the process may actually do
// -- AuthorizedCapabilities and Report both hang off it -- rather than what it was
// built to be able to do.
func (g *SubmissionGate) GateFor(ctx context.Context, adapterID string,
	env contracts.Environment) (*Gate, error) {

	reg, err := g.decls.Registry(adapterID)
	if err != nil {
		return nil, err
	}
	certs, err := LoadCertification(ctx, g.certs, adapterID, env)
	if err != nil {
		return nil, err
	}
	return NewGate(reg, certs, env, g.now()), nil
}

// Declarations supplies the validated declaration for an adapter id.
type Declarations interface {
	Registry(adapterID string) (Registry, error)
}

// DeclarationSet is the set of adapters a process has described itself as.
//
// It is populated at startup from the adapter builds that were linked in, which is
// what makes "declared rather than simulated" enforceable: an adapter whose
// declaration was never written down has no entry, and no entry is a refusal.
type DeclarationSet struct {
	byID map[string]Registry
}

// NewDeclarationSet returns an empty set.
func NewDeclarationSet() *DeclarationSet {
	return &DeclarationSet{byID: map[string]Registry{}}
}

// Declare validates and registers a declaration.
//
// A duplicate id is refused rather than overwritten. Overwriting would let a
// later registration silently widen an adapter's capabilities, and a registry that
// can be widened at runtime is exactly what Gate's immutability comment rules out
// -- a capability that appears after certification was granted was never certified.
func (s *DeclarationSet) Declare(d Declaration) (Registry, error) {
	reg, err := d.Declare()
	if err != nil {
		return Registry{}, err
	}
	if s.byID == nil {
		s.byID = map[string]Registry{}
	}
	if existing, dup := s.byID[d.AdapterID]; dup {
		if !sameDeclaration(existing, reg) {
			return Registry{}, fmt.Errorf("venue: %s is already declared with a different "+
				"declaration; replacing a declaration would change an adapter's certified "+
				"capabilities without recertifying it", d.AdapterID)
		}
		return existing, nil
	}
	s.byID[d.AdapterID] = reg
	return reg, nil
}

// Registry returns the declaration for adapterID, or ErrUnknownAdapter.
func (s *DeclarationSet) Registry(adapterID string) (Registry, error) {
	reg, ok := s.byID[adapterID]
	if !ok {
		known := make([]string, 0, len(s.byID))
		for id := range s.byID {
			known = append(known, id)
		}
		sort.Strings(known)
		return Registry{}, fmt.Errorf("%w: %s; declared adapters: %v", ErrUnknownAdapter, adapterID, known)
	}
	return reg, nil
}

// sameDeclaration compares two registries by their declarations.
//
// Registry holds its declaration unexported and there is no exported equality, so
// this compares the fields Declare actually validated. It exists only to make
// re-registering an identical declaration idempotent, which is what lets an
// adapter be wired in from more than one place without a spurious conflict.
func sameDeclaration(a, b Registry) bool {
	if a.AdapterID() != b.AdapterID() || a.Vendor() != b.Vendor() {
		return false
	}
	if a.IdempotentSubmit() != b.IdempotentSubmit() {
		return false
	}
	if len(a.decl.StateMap) != len(b.decl.StateMap) {
		return false
	}
	for venueState, mapped := range a.decl.StateMap {
		if b.decl.StateMap[venueState] != mapped {
			return false
		}
	}
	as, bs := a.DeclaredCapabilities(), b.DeclaredCapabilities()
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return a.decl.CertifiedAt.Equal(b.decl.CertifiedAt) && a.decl.ExpiresAt.Equal(b.decl.ExpiresAt)
}
