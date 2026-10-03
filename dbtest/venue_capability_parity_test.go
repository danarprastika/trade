package dbtest_test

// The venue adapter contract against the live catalog.
//
// adapters/venue keeps two vocabularies in Go: the twelve capabilities of doc 16
// section 4, and the six certification states. execution.adapter_capability
// enforces both in CHECK constraints. Two spellings would be two things able to
// disagree, and the disagreement would be silent in the direction that matters --
// a capability the gate believes in but the certification table cannot record.
//
// So both directions are checked against the live catalog: every Go value must
// exist in the constraint, and every value in the constraint must exist in Go.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/adapters/venue"
	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/oms"
)

// mustConstraintDef reads a CHECK constraint's text off the live catalog.
//
// It is read rather than assumed because a constraint that no longer exists would
// otherwise fail this test as "parity broken", which points an investigator at
// the Go code instead of at the migration that dropped it.
//
// The lookup matches on conname, not on the definition text: pg_get_constraintdef
// returns the constraint's expression and does not contain its own name, so
// filtering on it finds nothing and reports "no rows" for a constraint that is
// plainly there.
func mustConstraintDef(t *testing.T, ctx context.Context, tx *sql.Tx, table, name string) string {
	t.Helper()
	var def string
	if err := tx.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE conrelid = $1::regclass
		   AND conname = $2`,
		table, name).Scan(&def); err != nil {
		t.Fatalf("read constraint %s on %s: %v", name, table, err)
	}
	return def
}

// quotedLiterals pulls the quoted string literals out of a CHECK constraint's
// right-hand side.
//
// The constraint is a membership test, so its text is the list. Extracting the
// literals rather than pattern-matching a hand-written copy is what makes this a
// parity test rather than a restatement of the schema.
func quotedLiterals(def string) []string {
	// Splitting on the quote puts the text before the first literal at index 0,
	// each literal at an odd index, and the separators at even indices.
	segments := strings.Split(def, "'")
	out := make([]string, 0, (len(segments)-1)/2)
	for i := 1; i+1 < len(segments); i += 2 {
		out = append(out, segments[i])
	}
	return out
}

// TestEveryCapabilityTheContractKnowsIsOneTheSchemaHolds is the direction that
// would silently disable a control: Go believes a capability exists and the
// certification table cannot record it, so certifying that adapter is impossible
// and the gate refuses everything for reasons that name the wrong thing.
func TestEveryCapabilityTheContractKnowsIsOneTheSchemaHolds(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		def := mustConstraintDef(t, ctx, tx, "execution.adapter_capability", "adapter_capability_valid")
		inSchema := map[string]bool{}
		for _, lit := range quotedLiterals(def) {
			inSchema[lit] = true
		}
		if len(inSchema) == 0 {
			t.Fatalf("no capability literals found in %q; the constraint was not read", def)
		}
		for _, c := range venue.Capabilities() {
			if !inSchema[string(c)] {
				t.Errorf("the contract has capability %s, which %s does not hold", c,
					"execution.adapter_capability")
			}
		}
		if got, want := len(venue.Capabilities()), len(inSchema); got != want {
			t.Errorf("the contract has %d capabilities, the constraint holds %d; they are not the same set",
				got, want)
		}
	})
}

// The reverse direction: a capability added to the schema without a Go constant
// would be recordable but un-honourable, and the gate would ignore it.
func TestEveryCapabilityTheSchemaHoldsIsOneTheContractKnows(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		def := mustConstraintDef(t, ctx, tx, "execution.adapter_capability", "adapter_capability_valid")
		inGo := map[string]bool{}
		for _, c := range venue.Capabilities() {
			inGo[string(c)] = true
		}
		for _, lit := range quotedLiterals(def) {
			if !inGo[lit] {
				t.Errorf("execution.adapter_capability holds capability %q, which adapters/venue "+
					"cannot name; a certified adapter could advertise it and no caller could ask for it",
					lit)
			}
		}
	})
}

// The certification vocabulary is the same arrangement, and the error it causes is
// worse: an unrecognised state read from the database would either be refused as
// corrupt or, worse, treated as permissive.
func TestTheCertificationVocabularyMatchesTheSchema(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		def := mustConstraintDef(t, ctx, tx, "execution.adapter_capability", "adapter_certification_valid")
		inSchema := map[string]bool{}
		for _, lit := range quotedLiterals(def) {
			inSchema[lit] = true
		}
		if len(inSchema) == 0 {
			t.Fatalf("no certification literals found in %q", def)
		}
		// The list comes from the contract, not from a copy written out here. A
		// hand-copied list is a second vocabulary: adding a state to the contract
		// and forgetting this copy would leave the comparison passing against a
		// state the service cannot interpret, which is the drift this test exists
		// to catch.
		states := venue.CertificationStates()
		for _, s := range states {
			if !inSchema[string(s)] {
				t.Errorf("the contract has certification state %s, which the schema does not hold", s)
			}
		}
		if got, want := len(states), len(inSchema); got != want {
			t.Errorf("the contract has %d certification states, the schema holds %d", got, want)
		}
	})
}

// The gate must read certification from the database rather than trust an
// in-process declaration, and must fail closed when nothing is recorded.
func TestTheGateRefusesAnAdapterNobodyHasCertified(t *testing.T) {
	db := dbtest.Open(t)
	reg, err := venue.Declaration{
		AdapterID:   dbtest.CanonicalID("ven", int(dbtest.UniqueSequence(700))),
		Vendor:      "test-venue",
		CertifiedAt: time.Now().UTC().Add(-24 * time.Hour),
		ExpiresAt:   time.Now().UTC().Add(365 * 24 * time.Hour),
		Supported: map[venue.Capability]bool{
			venue.CapabilitySubmit: true,
			venue.CapabilityHealth: true,
		},
		IdempotentSubmit: true,
		StateMap:         map[string]oms.State{"new": oms.StateAcknowledged},
	}.Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	ctx := context.Background()

	// No row at all.
	if _, err := venue.LoadCertification(ctx, db, reg.AdapterID(), contracts.EnvPaper); err == nil {
		t.Fatal("an adapter with no certification row was accepted; missing evidence must fail closed")
	} else if !strings.Contains(err.Error(), "no certification row") {
		t.Errorf("the refusal did not explain that nothing is recorded: %v", err)
	}

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		mustCertify(t, ctx, tx, reg.AdapterID(), contracts.EnvPaper,
			venue.CapabilitySubmit, venue.PaperCertified, 701)
		mustCertify(t, ctx, tx, reg.AdapterID(), contracts.EnvPaper,
			venue.CapabilityHealth, venue.PaperCertified, 702)
		certs, err := venue.LoadCertification(ctx, tx, reg.AdapterID(), contracts.EnvPaper)
		if err != nil {
			t.Fatalf("LoadCertification after certifying: %v", err)
		}
		if len(certs) != 2 {
			t.Fatalf("read %d certifications, want 2", len(certs))
		}

		gate := venue.NewGate(reg, certs, contracts.EnvPaper, time.Now().UTC())

		// A declared, certified capability is permitted.
		if err := gate.Permit(venue.CapabilitySubmit); err != nil {
			t.Errorf("a certified, declared capability was refused: %v", err)
		}
		// A capability neither declared nor certified is refused by name.
		if err := gate.Permit(venue.CapabilityBalanceRetrieval); err == nil {
			t.Error("an undeclared capability was permitted")
		}
		// The gate is bound to paper, so its whole report is a paper answer: the
		// two certified capabilities are permitted and the ten others are refused
		// by name. This is what an operator reads to find out why a call was
		// rejected, so it has to distinguish "declared but uncertified" from
		// "not implemented" rather than refusing everything identically.
		report := gate.Report()
		for _, permitted := range []string{"SUBMIT: permitted", "HEALTH: permitted"} {
			if !strings.Contains(report, permitted) {
				t.Errorf("the report does not record %q:\n%s", permitted, report)
			}
		}
		for _, refused := range []string{
			"SUBMIT", "BALANCE_RETRIEVAL", "RECONCILIATION", "RATE_LIMIT",
		} {
			if refused == "SUBMIT" {
				continue
			}
			if !strings.Contains(report, refused+": refused") {
				t.Errorf("the report does not record %s as refused:\n%s", refused, report)
			}
		}
		if got := gate.AuthorizedCapabilities(); len(got) != 2 {
			t.Errorf("the adapter has %d authorized capabilities, want 2: %v", len(got), got)
		}

		// Paper certification does not authorise live, and the gate cannot be
		// pointed at live: LoadCertification for live finds nothing, so there is
		// no gate to build. This is the combination that matters -- the
		// declaration is environment-independent and the certification is not.
		if _, err := venue.LoadCertification(ctx, tx, reg.AdapterID(), contracts.EnvLive); err == nil {
			t.Error("a live certification was built for an adapter certified only in paper")
		}
	})
}

// A certification row that exists for one environment says nothing about another.
// Reading paper certification while asking about live must not find it.
func TestCertificationDoesNotLeakAcrossEnvironments(t *testing.T) {
	db := dbtest.Open(t)
	adapterID := dbtest.CanonicalID("ven", int(dbtest.UniqueSequence(710)))
	reg, err := venue.Declaration{
		AdapterID:   adapterID,
		Vendor:      "test-venue",
		CertifiedAt: time.Now().UTC().Add(-time.Hour),
		Supported:   map[venue.Capability]bool{venue.CapabilitySubmit: true},
		StateMap:    map[string]oms.State{"new": oms.StateAcknowledged},
	}.Declare()
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		// The row records LIVE_ELIGIBLE, the most permissive state there is. It is
		// deliberately recorded in PAPER: a permissive state in the wrong
		// environment is the case that would matter if the gate ever read across.
		mustCertify(t, ctx, tx, adapterID, contracts.EnvPaper,
			venue.CapabilitySubmit, venue.LiveEligible, 711)

		paper, err := venue.LoadCertification(ctx, tx, adapterID, contracts.EnvPaper)
		if err != nil {
			t.Fatalf("paper certification was not readable: %v", err)
		}
		if len(paper) != 1 || paper[venue.CapabilitySubmit].State != venue.LiveEligible {
			t.Fatalf("the paper row did not read back as LIVE_ELIGIBLE: %+v", paper)
		}

		// A live gate cannot even be built from the live environment, because
		// there is no live row to build it from.
		if _, err := venue.LoadCertification(ctx, tx, adapterID, contracts.EnvLive); err == nil {
			t.Fatal("a live certification was found for an adapter certified only in paper")
		}

		gate := venue.NewGate(reg, paper, contracts.EnvPaper, time.Now().UTC())
		if gate.Environment() != contracts.EnvPaper {
			t.Errorf("the gate reports environment %s, want paper", gate.Environment())
		}
		if err := gate.Permit(venue.CapabilitySubmit); err != nil {
			t.Errorf("a LIVE_ELIGIBLE certification in paper does not permit a paper call: %v", err)
		}

		// Binding the same PAPER row to a LIVE gate is the mistake the binding
		// exists to make impossible, so the test asserts the two disagree rather
		// than asserting that one of them is correct.
		liveGate := venue.NewGate(reg, paper, contracts.EnvLive, time.Now().UTC())
		if err := liveGate.Permit(venue.CapabilitySubmit); err == nil {
			t.Error("a gate bound to live permitted a call on the strength of a paper row")
		}
	})
}

// mustCertify writes a certification row for one adapter and capability.
func mustCertify(t *testing.T, ctx context.Context, tx *sql.Tx, adapterID string,
	env contracts.Environment, capability venue.Capability, state venue.CertificationState,
	digestSeed int) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO execution.adapter_capability (
			adapter_id, environment, venue_id, capability, supported,
			certification_state, certified_at, certification_expires_at,
			certified_by, content_digest)
		VALUES ($1, $2::common.environment, 'ven_test', $3, true,
		        $4, $5, $6, 'certifier-1', $7)`,
		adapterID, string(env), string(capability), string(state),
		now, now.Add(365*24*time.Hour), dbtest.UniqueDigest(digestSeed))
}

// TestCapabilityNamesAreDistinct guards against a copy-paste collision in the
// constants, which would make two capabilities one value.
func TestCapabilityNamesAreDistinct(t *testing.T) {
	seen := map[string]venue.Capability{}
	for _, c := range venue.Capabilities() {
		if prev, dup := seen[string(c)]; dup {
			t.Errorf("%s and %s are both %q", prev, c, c)
		}
		seen[string(c)] = c
	}
	// Capabilities() is deliberately in doc 16 section 4's order rather than
	// sorted, so the enumeration reads as the document's. Only
	// DeclaredCapabilities is sorted, and
	// TestDeclaredCapabilitiesAreSortedAndDoNotAlias is what asserts that.
}
