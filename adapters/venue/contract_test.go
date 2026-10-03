package venue

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/domain/oms"
)

// declaration returns a valid declaration an individual test mutates.
//
// The supported set is the one a real venue adapter would claim: it can send and
// look up orders, and it cannot report balances or positions. That combination is
// deliberate -- it is the shape that makes "declared rather than simulated"
// testable, because there are capabilities in the same declaration that the
// adapter genuinely cannot answer.
func declaration() Declaration {
	return Declaration{
		AdapterID:   "adapter-test-1",
		Vendor:      "test-venue",
		CertifiedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt:   time.Date(2027, 9, 1, 0, 0, 0, 0, time.UTC),
		Supported: map[Capability]bool{
			CapabilityDiscovery:         true,
			CapabilityAuthenticate:      true,
			CapabilityInstrumentMapping: true,
			CapabilitySubmit:            true,
			CapabilityCancel:            true,
			CapabilityOrderLookup:       true,
			CapabilityHealth:            true,
		},
		IdempotentSubmit: true,
		StateMap: map[string]oms.State{
			"new":    oms.StateAcknowledged,
			"part":   oms.StatePartiallyFilled,
			"done":   oms.StateFilled,
			"dead":   oms.StateCancelled,
			"reject": oms.StateRejected,
			"expir":  oms.StateExpired,
		},
	}
}

// TestCapabilitiesAreDoc16sTwelveInOrder pins the Go vocabulary to the document.
//
// The list is in doc 16 section 4's order deliberately. An enumeration that does
// not follow the document reads as arbitrary, and the document is the thing the
// vocabulary has to be traceable to.
func TestCapabilitiesAreDoc16sTwelveInOrder(t *testing.T) {
	got := Capabilities()
	want := []Capability{
		CapabilityDiscovery, CapabilityAuthenticate, CapabilityInstrumentMapping,
		CapabilitySubmit, CapabilityCancel, CapabilityOrderLookup,
		CapabilityFillRetrieval, CapabilityBalanceRetrieval,
		CapabilityPositionRetrieval, CapabilityRateLimit, CapabilityHealth,
		CapabilityReconciliation,
	}
	if len(got) != len(want) {
		t.Fatalf("the contract has %d capabilities, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("capability %d = %s, want %s", i, got[i], want[i])
		}
	}
	if len(want) != 12 {
		t.Fatalf("the fixture itself lists %d capabilities; doc 16 section 4 names twelve", len(want))
	}
	for _, c := range got {
		if !c.Known() {
			t.Errorf("%s is in Capabilities() but not Known()", c)
		}
	}
	if Capability("TELEPORT").Known() {
		t.Error("an invented capability reported itself known")
	}
}

// TestDeclareRefusals covers the claims that would otherwise fail at the moment
// money was about to move.
func TestDeclareRefusals(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Declaration)
		contains string
	}{
		{
			name:     "no adapter id",
			mutate:   func(d *Declaration) { d.AdapterID = "" },
			contains: "adapter id",
		},
		{
			name:     "no vendor",
			mutate:   func(d *Declaration) { d.Vendor = "" },
			contains: "vendor",
		},
		{
			name:     "never certified",
			mutate:   func(d *Declaration) { d.CertifiedAt = time.Time{} },
			contains: "certification time",
		},
		{
			name: "expired on the day it was certified",
			mutate: func(d *Declaration) {
				d.ExpiresAt = d.CertifiedAt
			},
			contains: "not after",
		},
		{
			name: "a capability outside the vocabulary",
			mutate: func(d *Declaration) {
				d.Supported[Capability("HEDGE")] = true
			},
			contains: "not one of the twelve",
		},
		{
			name: "submit with no state map",
			mutate: func(d *Declaration) {
				d.StateMap = nil
			},
			contains: "declares no state map",
		},
		{
			name: "order lookup with no state map",
			mutate: func(d *Declaration) {
				delete(d.Supported, CapabilitySubmit)
				delete(d.Supported, CapabilityCancel)
				d.StateMap = nil
			},
			contains: "declares no state map",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := declaration()
			tc.mutate(&d)
			_, err := d.Declare()
			if err == nil {
				t.Fatalf("Declare accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("refusal %q does not explain the problem (%q)", err, tc.contains)
			}
		})
	}
}

func TestDeclareAcceptsTheBaseline(t *testing.T) {
	reg, err := declaration().Declare()
	if err != nil {
		t.Fatalf("the baseline declaration was refused: %v", err)
	}
	if reg.AdapterID() != "adapter-test-1" {
		t.Errorf("AdapterID = %s", reg.AdapterID())
	}
	if !reg.DeclaredState(CapabilitySubmit) {
		t.Error("the baseline does not declare Submit")
	}
	if reg.DeclaredState(CapabilityBalanceRetrieval) {
		t.Error("the baseline declares BalanceRetrieval, which it cannot answer")
	}
}

// Doc 16 section 4: unsupported capabilities are declared rather than simulated.
// This is the test for that sentence.
func TestAnUndeclaredCapabilityIsRefusedByName(t *testing.T) {
	reg, err := declaration().Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	for _, c := range []Capability{
		CapabilityBalanceRetrieval, CapabilityPositionRetrieval,
		CapabilityFillRetrieval, CapabilityReconciliation, CapabilityRateLimit,
	} {
		err := reg.Require(c, contracts.EnvPaper, PaperCertified, time.Now())
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("calling %s returned %v, want a refusal naming it as unsupported", c, err)
		}
		if !strings.Contains(err.Error(), string(c)) {
			t.Errorf("the refusal for %s does not name the capability: %v", c, err)
		}
	}
}

// Certification standing is the second gate. Each pairing is written out in
// permits rather than ranked, so this table is the specification of which
// environments each state can reach.
func TestCertificationStandingGatesTheEnvironment(t *testing.T) {
	reg, err := declaration().Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	now := time.Now()

	cases := []struct {
		state   CertificationState
		env     contracts.Environment
		allowed bool
	}{
		{LiveEligible, contracts.EnvLive, true},
		{LiveEligible, contracts.EnvPaper, true},
		{PaperCertified, contracts.EnvPaper, true},
		{PaperCertified, contracts.EnvLive, false},
		{ShadowCertified, contracts.EnvPaper, true},
		{ShadowCertified, contracts.EnvStaging, true},
		{ShadowCertified, contracts.EnvLive, false},
		{Uncertified, contracts.EnvShadow, true},
		{Uncertified, contracts.EnvPaper, false},
		{Uncertified, contracts.EnvLive, false},
		{Suspended, contracts.EnvDev, false},
		{Suspended, contracts.EnvPaper, false},
		{Suspended, contracts.EnvLive, false},
		{Revoked, contracts.EnvDev, false},
		{Revoked, contracts.EnvShadow, false},
	}
	for _, tc := range cases {
		err := reg.Require(CapabilitySubmit, tc.env, tc.state, now)
		gotAllowed := err == nil
		if gotAllowed != tc.allowed {
			t.Errorf("Require(Submit, %s, %s) allowed = %v, want %v (%v)",
				tc.env, tc.state, gotAllowed, tc.allowed, err)
		}
	}
}

// An expired certification is a refusal, not a warning. Doc 16 section 6 ties
// certification to the venue API and the adapter's permissions, any of which can
// change without this build moving.
func TestAnExpiredCertificationIsRefused(t *testing.T) {
	d := declaration()
	d.ExpiresAt = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	reg, err := d.Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	before := reg.Require(CapabilitySubmit, contracts.EnvPaper, PaperCertified,
		time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC).Add(-time.Hour))
	if before != nil {
		t.Fatalf("the day before expiry the call was refused: %v", before)
	}
	after := reg.Require(CapabilitySubmit, contracts.EnvPaper, PaperCertified,
		time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC).Add(time.Hour))
	if !errors.Is(after, ErrCertificationExpired) {
		t.Fatalf("a call the day after expiry returned %v, want an expiry refusal", after)
	}
}

// Every available default for an unmapped venue state is a lie about an order's
// fate. ACKNOWLEDGED invents a live order, FILLED invents exposure, REJECTED
// invents a rejection. So there is no default.
func TestAnUnmappedVenueStateIsRefusedRatherThanGuessed(t *testing.T) {
	reg, err := declaration().Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := reg.MapState("some_state_this_venue_added_last_month"); !errors.Is(err, ErrUnmappedState) {
		t.Fatalf("an unmapped venue state returned %v, want a refusal to guess", err)
	}
	for venueState := range declaration().StateMap {
		got, err := reg.MapState(venueState)
		if err != nil {
			t.Errorf("declared state %q is unmapped: %v", venueState, err)
		}
		if !got.Known() {
			t.Errorf("venue state %q maps to %q, which is not an OMS state", venueState, got)
		}
	}
}

// A map that points at something outside the OMS vocabulary is caught rather than
// returned, because the caller would otherwise hold an order in a state the state
// machine has never heard of.
func TestAMapPointingOutsideTheOmsVocabularyIsRefused(t *testing.T) {
	d := declaration()
	d.StateMap["weird"] = oms.State("HALF_LIVED")
	reg, err := d.Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := reg.MapState("weird"); !errors.Is(err, ErrUnmappedState) {
		t.Fatalf("a mapping to an unknown OMS state returned %v, want a refusal", err)
	}
}

// The registry is immutable once built, so the gate cannot be satisfied by a
// capability added after certification.
func TestDeclaredCapabilitiesAreSortedAndDoNotAlias(t *testing.T) {
	reg, err := declaration().Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	got := reg.DeclaredCapabilities()
	if len(got) != 7 {
		t.Fatalf("declared %d capabilities, want 7", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("declared capabilities are not sorted: %s before %s", got[i-1], got[i])
		}
	}
	// Mutating the returned slice must not affect the registry.
	got[0] = Capability("TAMPERED")
	if reg.DeclaredCapabilities()[0] == Capability("TAMPERED") {
		t.Error("the registry handed out its own slice; a caller can widen it")
	}

	all := Capabilities()
	all[0] = Capability("TAMPERED")
	if Capabilities()[0] == Capability("TAMPERED") {
		t.Error("Capabilities() handed out the package's own slice")
	}
}

// IdempotentSubmit is the field that decides whether a timeout may be retried, so
// it has to be read from the declaration rather than assumed.
func TestIdempotentSubmitIsReadFromTheDeclaration(t *testing.T) {
	d := declaration()
	d.IdempotentSubmit = false
	reg, err := d.Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if reg.IdempotentSubmit() {
		t.Error("the registry reported idempotent submission for an adapter that declared none")
	}
	if !declaration().IdempotentSubmit {
		t.Fatal("the fixture lost its idempotency flag")
	}
}

// Binding a gate to an environment is not sufficient on its own: the gate must
// also compare the row's own environment against its own, because the state on
// the row is otherwise trusted across environments. LIVE_ELIGIBLE in paper is the
// exact combination that would slip through.
func TestAGateRefusesACertificationFromAnotherEnvironment(t *testing.T) {
	reg, err := declaration().Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	now := time.Now()

	// A set read from paper, carrying the most permissive state there is.
	paper := map[Capability]Certification{
		CapabilitySubmit: {
			AdapterID:   reg.AdapterID(),
			Environment: contracts.EnvPaper,
			Capability:  CapabilitySubmit,
			Supported:   true,
			State:       LiveEligible,
		},
	}

	if err := NewGate(reg, paper, contracts.EnvPaper, now).Permit(CapabilitySubmit); err != nil {
		t.Fatalf("a LIVE_ELIGIBLE certification in paper does not permit a paper call: %v", err)
	}
	if err := NewGate(reg, paper, contracts.EnvLive, now).Permit(CapabilitySubmit); err == nil {
		t.Error("a gate bound to live permitted a call on the strength of a paper row")
	}
}

// A certification set whose rows disagree with the gate's environment is refused
// even when the state itself would permit it, because a gate cannot tell a caller
// it was handed the wrong map.
func TestAGateWithNoCertificationRowDefaultsToRefusing(t *testing.T) {
	reg, err := declaration().Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	// An empty set: Permit's missing-row branch. The state used for the
	// declaration check defaults to Revoked, which refuses, so the refusal names
	// the absent evidence rather than a standing the row never had.
	gate := NewGate(reg, map[Capability]Certification{}, contracts.EnvPaper, time.Now())
	if err = gate.Permit(CapabilitySubmit); err == nil {
		t.Fatal("a gate with no certifications permitted a call")
	}
	if !strings.Contains(err.Error(), "no certification row") {
		t.Errorf("the refusal does not name the absent evidence: %v", err)
	}
	if got := gate.AuthorizedCapabilities(); len(got) != 0 {
		t.Errorf("a gate with no certifications authorizes %v", got)
	}
}

// A row that records supported = false refuses even though the declaration claims
// the capability. The persisted evidence is the authority.
func TestARowThatRecordsUnsupportedRefuses(t *testing.T) {
	reg, err := declaration().Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	certs := map[Capability]Certification{
		CapabilitySubmit: {
			AdapterID:   reg.AdapterID(),
			Environment: contracts.EnvPaper,
			Capability:  CapabilitySubmit,
			Supported:   false,
			State:       LiveEligible,
		},
	}
	if err := NewGate(reg, certs, contracts.EnvPaper, time.Now()).Permit(CapabilitySubmit); err == nil {
		t.Error("a certification row recording supported = false permitted the call")
	}
}

func TestCertificationVocabularyIsClosed(t *testing.T) {
	for _, s := range []CertificationState{
		Uncertified, PaperCertified, ShadowCertified,
		LiveEligible, Suspended, Revoked,
	} {
		if !IsCertifiable(s) {
			t.Errorf("%s is not in the certification vocabulary", s)
		}
	}
	if IsCertifiable(CertificationState("APPROVED")) {
		t.Error("an invented certification state was accepted")
	}
}
