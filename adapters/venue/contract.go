// Package venue is the contract every venue adapter implements, and the gate that
// decides whether a given adapter may be used at all.
//
// Doc 16 section 4 enumerates what an adapter must implement: capability
// discovery, authentication, instrument mapping, submit, cancel, order lookup,
// fill retrieval, balance/position retrieval, rate-limit handling, health status,
// and reconciliation. execution.adapter_capability enforces the same twelve names
// in a CHECK constraint, and TestCapabilitiesMatchTheDatabase pins the two
// together in both directions.
//
// The load-bearing rule in that section is the last sentence of the first
// paragraph: unsupported capabilities are DECLARED rather than simulated. An
// adapter that answers a balance query it cannot really answer is worse than one
// that refuses, because the caller cannot tell the difference. So the gate here
// refuses a call to any capability not declared supported, and refuses the whole
// adapter when its certification does not cover the environment being asked about.
package venue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/domain/oms"
)

// Capability is one of doc 16 section 4's twelve adapter obligations.
//
// The values are the strings execution.adapter_capability holds, not a separate
// spelling. Two vocabularies would be two things able to disagree.
type Capability string

const (
	CapabilityDiscovery         Capability = "CAPABILITY_DISCOVERY"
	CapabilityAuthenticate      Capability = "AUTHENTICATE"
	CapabilityInstrumentMapping Capability = "INSTRUMENT_MAPPING"
	CapabilitySubmit            Capability = "SUBMIT"
	CapabilityCancel            Capability = "CANCEL"
	CapabilityOrderLookup       Capability = "ORDER_LOOKUP"
	CapabilityFillRetrieval     Capability = "FILL_RETRIEVAL"
	CapabilityBalanceRetrieval  Capability = "BALANCE_RETRIEVAL"
	CapabilityPositionRetrieval Capability = "POSITION_RETRIEVAL"
	CapabilityRateLimit         Capability = "RATE_LIMIT"
	CapabilityHealth            Capability = "HEALTH"
	CapabilityReconciliation    Capability = "RECONCILIATION"
)

// capabilities is the closed set, in doc 16 section 4's order.
//
// Order is preserved because the order is the document's, and a capability list
// that does not follow it reads as an arbitrary enumeration.
var capabilities = []Capability{
	CapabilityDiscovery,
	CapabilityAuthenticate,
	CapabilityInstrumentMapping,
	CapabilitySubmit,
	CapabilityCancel,
	CapabilityOrderLookup,
	CapabilityFillRetrieval,
	CapabilityBalanceRetrieval,
	CapabilityPositionRetrieval,
	CapabilityRateLimit,
	CapabilityHealth,
	CapabilityReconciliation,
}

// Capabilities returns doc 16 section 4's twelve, in document order.
func Capabilities() []Capability {
	out := make([]Capability, len(capabilities))
	copy(out, capabilities)
	return out
}

// Known reports whether c is one of the twelve.
func (c Capability) Known() bool {
	for _, k := range capabilities {
		if k == c {
			return true
		}
	}
	return false
}

// CertificationState is the adapter's certification standing.
type CertificationState string

const (
	Uncertified     CertificationState = "UNCERTIFIED"
	PaperCertified  CertificationState = "PAPER_CERTIFIED"
	ShadowCertified CertificationState = "SHADOW_CERTIFIED"
	LiveEligible    CertificationState = "LIVE_ELIGIBLE"
	Suspended       CertificationState = "SUSPENDED"
	Revoked         CertificationState = "REVOKED"
)

// CertificationStates returns the whole certification vocabulary, in declaration
// order.
//
// Exported so the Go-to-schema parity test can compare this list against
// execution.adapter_capability.adapter_certification_valid without restating it.
// A hand-copied list in a test is a second copy of the vocabulary: adding a state
// here and forgetting the test copy leaves the test passing against a state the
// service cannot interpret.
func CertificationStates() []CertificationState {
	return []CertificationState{
		Uncertified, PaperCertified, ShadowCertified,
		LiveEligible, Suspended, Revoked,
	}
}

// knownCertification is the closed set execution.adapter_capability enforces.
//
// Derived from CertificationStates rather than written out again, so the map and
// the exported list cannot disagree. Iterating the list rather than hardcoding six
// entries is what makes the map correct when a state is added: a state absent from
// the map is unknown, which is the fail-closed answer, and the parity test then
// reports the disagreement instead of this map quietly accepting it.
var knownCertification = func() map[CertificationState]bool {
	m := make(map[CertificationState]bool, len(CertificationStates()))
	for _, c := range CertificationStates() {
		m[c] = true
	}
	return m
}()

func (c CertificationState) known() bool { return knownCertification[c] }

// permits reports whether this state authorises use in env.
//
// The mapping is deliberately not a ranking. Suspended and Revoked are not
// "less certified than LiveEligible"; they are refusals, and ranking them would
// make a future edit that reorders a list quietly change which credentials can
// move money. Each pairing is written out.
func (c CertificationState) permits(env contracts.Environment) bool {
	switch env {
	case contracts.EnvDev, contracts.EnvTest, contracts.EnvShadow:
		// Development and shadow may use anything that is not explicitly refused.
		// Shadow is where an adapter is first observed against real venue
		// behaviour, so requiring more than Uncertified there would mean the
		// evidence could never be collected.
		return c != Suspended && c != Revoked
	case contracts.EnvPaper:
		return c == PaperCertified || c == ShadowCertified || c == LiveEligible
	case contracts.EnvStaging:
		return c == ShadowCertified || c == LiveEligible
	case contracts.EnvLive:
		return c == LiveEligible
	default:
		return false
	}
}

// Refusal errors the gate returns.
var (
	// ErrUnsupported means the adapter declared the capability unsupported, or
	// never declared it at all. Doc 16 section 4 requires that an unsupported
	// capability be declared rather than simulated, so this is the declaration
	// working, not a malfunction.
	ErrUnsupported = errors.New("venue: the adapter does not support this capability")

	// ErrNotCertified means the adapter's certification does not authorise the
	// environment the caller asked about.
	ErrNotCertified = errors.New("venue: the adapter is not certified for this environment")

	// ErrCertificationExpired means the certification has passed its expiry.
	// Expired is treated exactly like never-certified rather than as a warning:
	// doc 16 section 6 ties certification to the adapter, venue API, permissions
	// and contract, any of which can change without the adapter's own code moving.
	ErrCertificationExpired = errors.New("venue: the adapter's certification has expired")

	// ErrUnmappedState means the venue reported an order state the adapter has no
	// mapping for.
	ErrUnmappedState = errors.New("venue: the venue reported an unmapped order state")
)

// Declaration is what an adapter says about itself: which capabilities it really
// implements, and what it is certified for.
type Declaration struct {
	AdapterID   string
	Vendor      string
	CertifiedAt time.Time
	ExpiresAt   time.Time

	// Supported is the set of capabilities the adapter really implements.
	// Anything absent is declared unsupported and will be refused, never
	// approximated.
	Supported map[Capability]bool

	// IdempotentSubmit says whether the venue guarantees that resubmitting the
	// same client order id cannot create a second order.
	//
	// This is the single most consequential field in the declaration. Doc 16
	// section 4 allows either behaviour but requires the consequences to differ:
	// where idempotency is guaranteed a retry is safe, and where it is not the
	// platform must fall back to a durable submission record and reconciliation.
	// An adapter that claimed idempotency without it would turn a timeout into a
	// duplicate order.
	IdempotentSubmit bool

	// StateMap translates venue order states into OMS states.
	//
	// It is required whenever OrderLookup or Submit is supported, because those
	// are the capabilities that surface a venue state to the platform. There is
	// deliberately no default and no fallback: see MapState.
	StateMap map[string]oms.State
}

// Declare validates the declaration itself.
//
// The checks here are the ones that would otherwise fail at the moment money was
// about to move. A declaration claiming a capability outside the vocabulary, an
// unknown certification state, a submission mapping with no state map, or an
// expiry before the certification, is refused at construction -- where the caller
// is still deciding whether to use this adapter -- rather than at call time.
func (d Declaration) Declare() (Registry, error) {
	if d.AdapterID == "" {
		return Registry{}, errors.New("venue: an adapter declaration needs an adapter id; a call " +
			"that cannot be attributed to a build cannot be certified")
	}
	if d.Vendor == "" {
		return Registry{}, errors.New("venue: an adapter declaration needs a vendor name; the " +
			"certification evidence is per vendor and venue, not per implementation")
	}
	if d.CertifiedAt.IsZero() {
		return Registry{}, errors.New("venue: an adapter declaration needs a certification time")
	}
	if !d.ExpiresAt.IsZero() && !d.ExpiresAt.After(d.CertifiedAt) {
		return Registry{}, fmt.Errorf("venue: %s expires at %s, which is not after it was "+
			"certified at %s", d.AdapterID, d.ExpiresAt, d.CertifiedAt)
	}
	for c := range d.Supported {
		if !c.Known() {
			return Registry{}, fmt.Errorf("venue: %s declares capability %q, which is not one of "+
				"the twelve in doc 16 section 4", d.AdapterID, c)
		}
	}
	if d.StateMap == nil && (d.Supported[CapabilityOrderLookup] || d.Supported[CapabilitySubmit]) {
		return Registry{}, fmt.Errorf("venue: %s supports submit or order lookup but declares no "+
			"state map; an unmapped venue state would have to be guessed, and guessing an order "+
			"state is how an order is recorded as live when the venue never took it", d.AdapterID)
	}
	return Registry{decl: d}, nil
}

// Registry is a validated adapter declaration.
//
// It is immutable: capabilities are declared once, at construction, and cannot be
// widened afterwards. A registry that could gain Submit at runtime would let the
// gate be satisfied by a capability that was never certified.
type Registry struct {
	decl Declaration
}

// AdapterID returns the declared adapter id.
func (r Registry) AdapterID() string { return r.decl.AdapterID }

// Vendor returns the declared vendor.
func (r Registry) Vendor() string { return r.decl.Vendor }

// DeclaredState reports whether the adapter claims a capability.
func (r Registry) DeclaredState(c Capability) bool { return r.decl.Supported[c] }

// IdempotentSubmit reports whether the venue guarantees submit idempotency.
func (r Registry) IdempotentSubmit() bool { return r.decl.IdempotentSubmit }

// DeclaredCapabilities returns the supported capabilities, sorted so the list is
// stable for comparison and logging.
func (r Registry) DeclaredCapabilities() []Capability {
	out := make([]Capability, 0, len(r.decl.Supported))
	for c, ok := range r.decl.Supported {
		if ok {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Require checks that a capability may be used in env right now.
//
// This is the gate every call goes through. It refuses on three independent
// grounds -- capability, certification standing, and expiry -- and says which one
// applied, because an operator responding to "not certified" and "certification
// expired" takes different actions.
func (r Registry) Require(c Capability, env contracts.Environment, state CertificationState, now time.Time) error {
	if !c.Known() {
		return fmt.Errorf("%w: %q is not one of the twelve capabilities", ErrUnsupported, c)
	}
	if !r.decl.Supported[c] {
		return fmt.Errorf("%w: %s does not implement %s; declared supported: %v",
			ErrUnsupported, r.decl.AdapterID, c, r.DeclaredCapabilities())
	}
	if !state.known() {
		return fmt.Errorf("%w: %s reports certification state %q, which is not in the "+
			"certification vocabulary", ErrNotCertified, r.decl.AdapterID, state)
	}
	if state == Revoked || state == Suspended {
		return fmt.Errorf("%w: %s is %s", ErrNotCertified, r.decl.AdapterID, state)
	}
	if !state.permits(env) {
		return fmt.Errorf("%w: %s is %s, which does not authorise %s", ErrNotCertified,
			r.decl.AdapterID, state, env)
	}
	if !r.decl.ExpiresAt.IsZero() && now.After(r.decl.ExpiresAt) {
		return fmt.Errorf("%w: %s expired at %s and it is now %s",
			ErrCertificationExpired, r.decl.AdapterID, r.decl.ExpiresAt, now)
	}
	return nil
}

// MapState translates a venue order state into the OMS vocabulary.
//
// It is exported because the only callers that need it are outside this package,
// and an unexported translator left reconciliation with no honest way to read a
// venue state. The two available workarounds were both worse than the gap: compare
// the venue's string against the platform's, which compares two vocabularies that
// are not the same and reports a difference on every live order; or publish a
// mutable package-level mapping, which lets anything in the process rewrite how
// every adapter's states are interpreted. Exporting the method keeps the
// translation on the Registry that declares it, which is the only object that
// knows which map applies.
//
// There is no default. An unrecognised venue state is refused, because every
// available default is a lie about an order's fate: ACKNOWLEDGED invents a live
// order, REJECTED invents a rejection the venue never made, and FILLED invents
// exposure. UNKNOWN is the only honest answer and it is not a mapping, it is the
// absence of one, so the caller is told to resolve rather than given a state.
//
// This is the same discipline as execution.Store's outcome mapping, where an
// uninterpretable value stores as TIMED_OUT_UNKNOWN rather than REJECTED.
func (r Registry) MapState(venueState string) (oms.State, error) {
	mapped, ok := r.decl.StateMap[venueState]
	if !ok {
		return "", fmt.Errorf("%w: %s reported %q; %s maps %v",
			ErrUnmappedState, r.decl.AdapterID, venueState, r.decl.AdapterID, r.decl.StateMap)
	}
	if !mapped.Known() {
		return "", fmt.Errorf("%w: %s maps %q to %q, which is not an OMS state",
			ErrUnmappedState, r.decl.AdapterID, venueState, mapped)
	}
	return mapped, nil
}

// VenueOrder is what an adapter learned about an order by asking the venue.
//
// It is the venue's answer, not the platform's belief. Everything the platform
// knows about an order that has left its control comes from a struct of this
// shape, and each field is something the venue asserted rather than something
// derived.
type VenueOrder struct {
	// ClientOrderID is the platform's own id, echoed back. It is the only field
	// that ties a venue record to a platform order without trusting the venue's
	// own reference, which is why an adapter that cannot echo it does not support
	// order lookup at all.
	ClientOrderID string
	VenueOrderRef string
	State         string
	Side          string
	Quantity      contracts.Decimal
	Filled        contracts.Decimal
	Price         *contracts.Decimal
	ObservedAt    time.Time
}

// VenueFill is one execution the venue reports.
type VenueFill struct {
	ClientOrderID string
	VenueOrderRef string
	VenueFillRef  string
	Quantity      contracts.Decimal
	Price         contracts.Decimal
	Fee           *contracts.Decimal
	FeeCurrency   string
	ExecutedAt    time.Time
	VenueSequence *int64
}

// VenueBalance is one currency balance the venue reports.
type VenueBalance struct {
	Currency  string
	Available contracts.Decimal
	Total     contracts.Decimal
	Held      contracts.Decimal
}

// VenuePosition is one position the venue reports.
type VenuePosition struct {
	InstrumentID string
	Quantity     contracts.Decimal
	AveragePrice *contracts.Decimal
	AsOf         time.Time
}

// SubmitRequest is what the platform asks a venue to do.
type SubmitRequest struct {
	ClientOrderID string
	InstrumentID  string
	VenueSymbol   string
	Side          contracts.Side
	OrderType     contracts.OrderType
	TimeInForce   contracts.TimeInForce
	Quantity      contracts.Decimal
	LimitPrice    *contracts.Decimal
	// IdempotencyKey is carried so an adapter whose venue supports client order
	// ids can use it, and one that does not can record that it could not.
	IdempotencyKey string
}

// SubmitResult is the venue's answer to a submit.
type SubmitResult struct {
	VenueOrderRef string
	// State is the venue's own state string, not an OMS state. The adapter maps it
	// through its declared StateMap; the caller never sees a raw venue state
	// without the mapping having happened first.
	State string
	// TimedOut is true when the request may or may not have reached the venue.
	//
	// It is not a transport error and is not treated as one. Doc 16 section 4 is
	// explicit that a timeout after a possible submission yields OMS UNKNOWN, and
	// the durable submission record plus reconciliation are what resolve it. An
	// adapter reporting a timeout as a failure would invite a retry, and a retry
	// against a non-idempotent venue is a duplicate order.
	TimedOut   bool
	ObservedAt time.Time
}

// Reader is the read surface reconciliation needs.
//
// Every method is separate rather than bundled so an adapter that cannot answer
// one of them is visibly incomplete rather than returning a zero value that looks
// like an answer.
type Reader interface {
	LookupOrder(ctx context.Context, clientOrderID string) (VenueOrder, error)
	Fills(ctx context.Context, since time.Time) ([]VenueFill, error)
	Balances(ctx context.Context) ([]VenueBalance, error)
	Positions(ctx context.Context) ([]VenuePosition, error)
	Health(ctx context.Context) error
}

// Writer is the surface that changes venue state.
type Writer interface {
	Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error)
	Cancel(ctx context.Context, clientOrderID string) (SubmitResult, error)
}

// Mapper is the surface that translates platform identity to venue identity.
type Mapper interface {
	VenueSymbol(instrumentID string) (string, error)
}

// Adapter is the full contract: a reader, a writer, and an identity mapper.
//
// It is one interface so that an adapter claiming to be an Adapter has visibly
// implemented all three, but the parts remain separately satisfiable so a
// read-only reconciliation-only adapter can be built from Reader alone.
type Adapter interface {
	Reader
	Writer
	Mapper
}

// Reason renders the declared capabilities as a single line, for a log or a
// startup banner. It is sorted so two runs of the same build produce the same
// string.
func (r Registry) Reason() string {
	caps := r.DeclaredCapabilities()
	names := make([]string, 0, len(caps))
	for _, c := range caps {
		names = append(names, string(c))
	}
	return r.decl.AdapterID + " (" + r.decl.Vendor + "): " + strings.Join(names, ",")
}
