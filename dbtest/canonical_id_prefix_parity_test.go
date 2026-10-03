package dbtest_test

import (
	"context"
	"database/sql"
	"regexp"
	"sort"
	"testing"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
)

// Parity between the prefixes the schema demands and the prefixes Go can mint.
//
// The existing encoder parity test in this package compares contracts.NewID
// against common.canonical_id_from_bytes over fixed entropy. That comparison is
// sound and stays, but it cannot see a missing prefix. common.canonical_id_from_bytes
// takes its prefix as TEXT and formats whatever it is given, so it agrees with
// Go about all 29 types Go had while the schema already required twelve more
// that Go had never heard of. Both encoders were right; they were answering a
// different question than the one that mattered.
//
// The consequence was concrete rather than cosmetic. execution.submission carries
//
//     submission_id  is_canonical_id(submission_id, 'sbm')
//
// and there was no way for any Go code to produce a value that satisfied it.
// A durable submission record could not be written from the control plane, so
// the OMS submission path could not be implemented without first changing the
// schema -- which is the wrong repair, because the schema is what the rest of
// the system is written against.
//
// This test reads the required prefixes out of the live catalog and compares
// them with contracts.EntityTypes() in both directions.

// prefixFromCall extracts the literal prefix argument of an is_canonical_id call.
// The pattern is anchored on the call rather than on the column so that a
// constraint naming any column is picked up without enumerating the columns here.
var prefixFromCall = regexp.MustCompile(`is_canonical_id\([^)]*'([a-z0-9_]+)'`)

// requiredPrefixes returns every canonical-ID prefix the live schema enforces,
// read from pg_constraint so that the answer is what is enforced rather than what
// a migration once said. A prefix introduced by a later migration appears here
// without this file being edited.
func requiredPrefixes(t *testing.T, ctx context.Context, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT c.conrelid::regclass::text, pg_get_constraintdef(c.oid)
		  FROM pg_constraint c
		 WHERE pg_get_constraintdef(c.oid) LIKE '%is_canonical_id%'`)
	if err != nil {
		t.Fatalf("reading canonical-ID constraints failed: %v", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var table, def string
		if err := rows.Scan(&table, &def); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		for _, m := range prefixFromCall.FindAllStringSubmatch(def, -1) {
			p := m[1]
			out[p] = append(out[p], table)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows failed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no canonical-ID constraints found in the live database; the query is wrong, " +
			"not the schema empty, and treating this as a pass would silently stop checking prefixes")
	}
	return out
}

// Every prefix the schema enforces must be one Go can mint. This is the
// direction that broke: a column requiring a prefix Go lacks cannot be written
// from Go at all, no matter how correct the rest of the path is.
func TestEverySchemaRequiredPrefixCanBeMintedByGo(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	required := requiredPrefixes(t, ctx, db)
	mintable := map[contracts.EntityType]bool{}
	for _, et := range contracts.EntityTypes() {
		mintable[et] = true
	}

	var missing []string
	for prefix := range required {
		if !mintable[contracts.EntityType(prefix)] {
			missing = append(missing, prefix)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		for _, p := range missing {
			t.Errorf("schema requires prefix %q on %v but contracts.EntityTypes() has no "+
				"EntityType for it, so no Go code can mint a valid value for that column",
				p, required[p])
		}
		t.Fatalf("%d prefix(es) required by the schema cannot be minted by Go", len(missing))
	}
}

// mintableOnly is the prefixes Go can mint that no constraint requires. They are
// reserved for entities whose tables do not exist yet -- agent, approval,
// artifact, dataset, data quality, eligibility, model, run, session, venue --
// so their absence from the schema is a fact about migration progress, not a
// defect. Pinning the set makes that explicit: adding a twelfth reserved type
// has to be a decision, and removing one that later gains a table fails loudly
// in the test above rather than leaving a type nothing can produce.
var mintableOnly = []string{
	"agt", "apr", "art", "dqt", "dst", "elg", "mdl", "run", "ses", "ven",
}

// The reserved set is checked so that the inventory in the comment above cannot
// drift away from the code. A new mintable type with no table is a decision to
// record here, not an omission.
func TestReservedPrefixesAreExactlyTheOnesWithNoTableYet(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()

	required := requiredPrefixes(t, ctx, db)
	mintable := map[contracts.EntityType]bool{}
	for _, et := range contracts.EntityTypes() {
		mintable[et] = true
	}

	var got []string
	for _, et := range contracts.EntityTypes() {
		if _, ok := required[string(et)]; !ok {
			got = append(got, string(et))
		}
	}

	if len(got) != len(mintableOnly) {
		t.Fatalf("reserved prefix set changed: %v have no constraint but %v are listed as "+
			"reserved. Update mintableOnly and its comment when that is intentional",
			got, mintableOnly)
	}
	for i := range got {
		if got[i] != mintableOnly[i] {
			t.Fatalf("reserved prefix set changed:\n  live: %v\n  test: %v\n"+
				"Update mintableOnly and its comment when that is intentional",
				got, mintableOnly)
		}
	}
}

// A prefix is only mintable if it also survives the schema's own validation and
// Go's own parse. EntityTypes() membership alone would pass a two-character or
// five-character prefix, which contracts.NewID accepts and which
// common.is_canonical_id -- anchored on '^<prefix>_[0-9...]{20}$' -- would
// either reject outright or, for a longer prefix, never be asked about.
//
// This is the check that would have caught a careless addition of the twelve.
func TestEveryMintedPrefixSatisfiesTheSchemasOwnValidator(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		entropy := make([]byte, 16)
		for i := range entropy {
			entropy[i] = byte(i)
		}
		for _, et := range contracts.EntityTypes() {
			id, err := contracts.IDFromBytes(et, entropy)
			if err != nil {
				t.Fatalf("Go cannot mint an id for %q: %v", et, err)
			}
			var ok bool
			dbtest.MustQueryRow(t, ctx, tx, &ok,
				`SELECT common.is_canonical_id($1, $2)`, id.String(), string(et))
			if !ok {
				t.Errorf("Go minted %q for prefix %q but common.is_canonical_id rejects it; "+
					"the two disagree about the shape of a canonical identifier", id, et)
			}
		}
	})
}
