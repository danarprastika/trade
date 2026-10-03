package venue

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/aitc/trade/contracts"
)

// Certification is what execution.adapter_capability records about an adapter in
// a given environment.
//
// It is persisted rather than computed because certification is evidence: it was
// granted by a named person, at a time, against a specific build. An in-process
// declaration can assert that an adapter supports a capability; only this row can
// establish that anyone certified it for this environment.
type Certification struct {
	AdapterID     string
	Environment   contracts.Environment
	Capability    Capability
	Supported     bool
	State         CertificationState
	CertifiedAt   *time.Time
	ExpiresAt     *time.Time
	CertifiedBy   *string
	ContentDigest string
}

// CertificationQuerier is the read surface the gate needs.
//
// It is an interface so the same assertions run against a transaction and a
// pool. A gate that only works against a pool cannot be tested without
// committing.
type CertificationQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// LoadCertification reads every capability row for one adapter in one environment.
//
// It returns the full set rather than a single capability because the gate's job
// includes reporting what an adapter CAN do, and a per-capability lookup would
// make an operator discovering a misconfiguration need one query per mistake.
//
// A row that is absent is not a supported capability. Missing evidence fails
// closed, which is why this returns the set it found and lets the gate refuse,
// rather than returning a default that would let an uncertified adapter through.
func LoadCertification(ctx context.Context, q CertificationQuerier, adapterID string,
	env contracts.Environment) (map[Capability]Certification, error) {

	rows, err := q.QueryContext(ctx, `
		SELECT capability, supported, certification_state,
		       certified_at, certification_expires_at, certified_by, content_digest
		  FROM execution.adapter_capability
		 WHERE adapter_id = $1 AND environment = $2::common.environment`,
		adapterID, string(env))
	if err != nil {
		return nil, fmt.Errorf("venue: reading the certification of %s in %s failed: %w",
			adapterID, env, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[Capability]Certification, len(capabilities))
	for rows.Next() {
		var c Certification
		var capability string
		var state string
		if err := rows.Scan(&capability, &c.Supported, &state,
			&c.CertifiedAt, &c.ExpiresAt, &c.CertifiedBy, &c.ContentDigest); err != nil {
			return nil, fmt.Errorf("venue: scanning a certification row for %s failed: %w", adapterID, err)
		}
		c.AdapterID = adapterID
		c.Environment = env
		c.Capability = Capability(capability)
		c.State = CertificationState(state)

		// A capability outside doc 16's twelve cannot be honoured, and treating it
		// as supported would be a way for the row to assert something the contract
		// has no name for. It is refused rather than ignored.
		if !c.Capability.Known() {
			return nil, fmt.Errorf("venue: %s is certified for %q, which is not one of the "+
				"twelve capabilities in doc 16 section 4", adapterID, capability)
		}
		out[c.Capability] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("venue: reading certifications for %s failed: %w", adapterID, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s has no certification row in %s at all. Failing closed is "+
			"deliberate: an adapter nobody certified has not been shown to place an order "+
			"correctly, and 'we have no record of it' must not read as 'it is fine'",
			ErrNotCertified, adapterID, env)
	}
	return out, nil
}

// Gate authorises adapter calls against both what an adapter declares and what
// the database certifies.
//
// Both must agree. A declaration without certification is a capability nobody
// tested; certification without a declaration is evidence about an adapter this
// process cannot actually call. Requiring both means neither a forged
// in-process flag nor an orphaned certification row can authorise a venue call.
//
// A gate is BOUND to one environment. It does not take an environment per call,
// because the certification set it holds was read for exactly one environment and
// a caller asking about another would be asking a question the gate cannot answer
// from what it has. An earlier draft took the environment per call and read the
// certificate out of the map regardless of which environment it was loaded for,
// so a gate holding paper certifications recorded as LIVE_ELIGIBLE permitted a
// live call. Binding the environment makes that question unaskable.
type Gate struct {
	reg   Registry
	certs map[Capability]Certification
	env   contracts.Environment
	now   time.Time
}

// NewGate pairs a validated declaration with its persisted certification for one
// environment.
func NewGate(reg Registry, certs map[Capability]Certification, env contracts.Environment, now time.Time) *Gate {
	return &Gate{reg: reg, certs: certs, env: env, now: now}
}

// Environment returns the environment this gate authorises calls in.
func (g *Gate) Environment() contracts.Environment { return g.env }

// Registry returns the declaration behind the gate.
func (g *Gate) Registry() Registry { return g.reg }

// Permit reports whether c may be used, and says why not if it may not.
//
// The order is absence, then provenance, then standing, then the declaration.
// Absence first because a capability with no certification row has no standing to
// evaluate: asking about its state first answers a question that cannot be
// answered and reports a derived state rather than the missing record. "We hold no
// row for this capability" is both the true statement and the most actionable one,
// and nothing below this point is reachable for a capability with no evidence.
func (g *Gate) Permit(c Capability) error {
	cert, ok := g.certs[c]
	if !ok {
		return fmt.Errorf("%w: %s has no certification row for %s in %s. A declaration without "+
			"certification is a capability nobody tested",
			ErrNotCertified, g.reg.AdapterID(), c, g.env)
	}
	// The row must be about this gate's environment.
	//
	// Binding the gate to an environment is not sufficient on its own, which is
	// what this check exists to record. A gate holding a certification set loaded
	// for paper and bound to live would otherwise read the paper row's state --
	// LIVE_ELIGIBLE is the permissive one -- and permit a live call, because
	// nothing compared the row's own environment against the gate's. The gate
	// cannot tell a caller "that map is for another environment" from reading the
	// map alone, so it compares instead of assuming.
	if cert.Environment != g.env {
		return fmt.Errorf("%w: %s is certified for %s in %s, but this gate authorises %s. A "+
			"certification for one environment is not evidence about another",
			ErrNotCertified, g.reg.AdapterID(), c, cert.Environment, g.env)
	}
	// The declaration's own checks: vocabulary, declaration, certification state,
	// environment, expiry.
	if err := g.reg.Require(c, g.env, cert.State, g.now); err != nil {
		return err
	}
	if !cert.Supported {
		return fmt.Errorf("%w: the certification row for %s / %s / %s records supported = false",
			ErrUnsupported, g.reg.AdapterID(), g.env, c)
	}
	if cert.ExpiresAt != nil && g.now.After(*cert.ExpiresAt) {
		return fmt.Errorf("%w: the certification row for %s / %s / %s expired at %s",
			ErrCertificationExpired, g.reg.AdapterID(), g.env, c, *cert.ExpiresAt)
	}
	return nil
}

// AuthorizedCapabilities returns the capabilities this gate will permit,
// sorted. It exists so a startup banner reports what the process can actually do
// rather than what it was built to do.
func (g *Gate) AuthorizedCapabilities() []Capability {
	out := make([]Capability, 0, len(g.certs))
	for _, c := range Capabilities() {
		if g.Permit(c) == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Report renders the gate's decision for every capability, so an operator can see
// what is refused and why without calling each one.
//
// This exists because the alternative is a support ticket that turns into someone
// guessing at the gate. It reports the refusal reason per capability rather than
// a summary count, since the reasons are what differ.
func (g *Gate) Report() string {
	out := ""
	for _, c := range Capabilities() {
		if err := g.Permit(c); err != nil {
			out += fmt.Sprintf("%s: refused (%v)\n", c, err)
			continue
		}
		out += fmt.Sprintf("%s: permitted\n", c)
	}
	return out
}

// IsCertifiable reports whether a state could ever authorise something, which is
// a question about the vocabulary rather than about any one adapter.
func IsCertifiable(s CertificationState) bool { return s.known() }
