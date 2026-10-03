package halt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doc04Path is doc 04, which defines the halt hierarchy and its precedence.
const doc04Path = "../../docs/04_TRADING_DOMAIN_AND_RISK.md"

// readDoc04 returns the text of doc 04.
//
// A missing document fails the test rather than skipping it. Skipping would leave
// the hierarchy unpinned in exactly the environment where the specification is
// absent, and a skip reads as a pass in every summary it appears in.
func readDoc04(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(doc04Path))
	if err != nil {
		t.Fatalf("doc 04 defines the halt hierarchy and could not be read: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("doc 04 is empty")
	}
	return string(b)
}

// docChain reads the ordered levels out of doc 04's hierarchy line, which is
// written `SYSTEM_HALT > VENUE_HALT > MARKET_HALT > STRATEGY_HALT > ACCOUNT_HALT`.
//
// It is read from the document rather than transcribed so that a change to the
// specification is a test failure rather than a silent divergence.
func docChain(t *testing.T, doc string) []string {
	t.Helper()
	for _, raw := range strings.Split(doc, "\n") {
		// The chain is written inside a code span, so the backticks have to come
		// off before the line can be split on '>'.
		l := strings.TrimSpace(strings.ReplaceAll(raw, "`", ""))
		if !strings.Contains(l, "SYSTEM_HALT") || !strings.HasSuffix(l, "> ACCOUNT_HALT.") {
			continue
		}
		var out []string
		for _, p := range strings.Split(strings.TrimSuffix(l, "."), ">") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	t.Fatal("doc 04 has no 'SYSTEM_HALT > ... > ACCOUNT_HALT' chain; this test reads the " +
		"precedence from the document and must be taught a reworded chain")
	return nil
}

// Doc 04 says a higher-level halt cannot be bypassed by a lower-level enable
// command. That makes the order load-bearing rather than descriptive: an
// implementation whose order is reversed satisfies every membership check while
// inverting every precedence decision, and the visible symptom is an incident
// that clears a venue problem while a system problem is still active.
//
// The parity test already pins Rank() to ops.halt_rank in the database. This
// pins the database's order to the document, which closes the chain.
func TestTheHaltPrecedenceMatchesDoc04sChain(t *testing.T) {
	doc := readDoc04(t)
	want := docChain(t, doc)

	// Levels() is weakest first; the document's chain is strongest first.
	got := Levels()
	strongestFirst := make([]string, 0, len(got))
	for i := len(got) - 1; i >= 0; i-- {
		strongestFirst = append(strongestFirst, string(got[i]))
	}

	if len(want) != len(got) {
		t.Fatalf("doc 04 names %d halt levels (%s) but %d are implemented (%v).\n"+
			"A level doc 04 does not name has no authority behind it; a level doc 04 names and "+
			"the code omits is a bypass.",
			len(want), strings.Join(want, " > "), len(got), strongestFirst)
	}

	for i, level := range want {
		if strongestFirst[i] != level {
			t.Errorf("doc 04's chain is %q; the implementation ranks %v.\n"+
				"First divergence at position %d.", strings.Join(want, " > "), strongestFirst, i+1)
		}
	}
}

// Every level doc 04 names is one the code knows, and vice versa. This is the
// membership half of the check above, kept separate so that a level appearing in
// only one of the two is reported as exactly that rather than as a divergence at
// some position.
func TestEveryHaltLevelIsKnownAndNamed(t *testing.T) {
	doc := readDoc04(t)
	want := docChain(t, doc)

	inDoc := make(map[string]bool, len(want))
	for _, l := range want {
		inDoc[l] = true
		if !strings.Contains(doc, l) {
			t.Errorf("halt level %s was parsed from the chain but does not appear in doc 04", l)
		}
	}

	for _, l := range Levels() {
		if !inDoc[string(l)] {
			t.Errorf("halt level %s is implemented but doc 04's hierarchy does not name it", l)
		}
		if !l.Known() {
			t.Errorf("%s is in Levels() but Known() refuses it, so the two disagree", l)
		}
	}
	if got, want := Levels()[0].Rank(), 1; got != want {
		t.Errorf("the weakest level ranks %d, want %d; rank is index+1 and must match ops.halt_rank", got, want)
	}
	if got, want := LevelSystem.Rank(), len(Levels()); got != want {
		t.Errorf("SYSTEM_HALT ranks %d, want %d", got, want)
	}
}

// The chain is a strict order with no ties. Two levels sharing a rank would make
// which halt governs depend on the order rows happened to arrive in, which is
// not a property any incident can reason about.
func TestEveryHaltLevelHasADistinctRank(t *testing.T) {
	seen := make(map[int]Level, len(Levels()))
	for _, l := range Levels() {
		if prev, dup := seen[l.Rank()]; dup {
			t.Errorf("%s and %s share rank %d; precedence must be total", prev, l, l.Rank())
			continue
		}
		seen[l.Rank()] = l
	}
	if len(seen) != len(Levels()) {
		t.Errorf("%d ranks cover %d levels", len(seen), len(Levels()))
	}
}
