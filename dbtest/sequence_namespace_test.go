package dbtest_test

import (
	"testing"

	"github.com/aitc/trade/dbtest"
)

// The sequence namespace, which has to be disjoint from the literals.
//
// Defect 53. UniqueSequence exists so a test that must observe a real COMMIT of an
// append-only ledger row can do so twice against the same database. It was not
// unconditionally unique: the run multiplier came from a nanosecond clock, so
// (runNonce & 0x3fffffff) % 100000 landed on 0 about once in a hundred thousand
// runs. On such a run run*base vanishes and UniqueSequence(n) returned exactly n,
// so a caller asking for a unique sequence received 1, 2, 3 -- the same values
// other tests hardcode.
//
// The consequence was permanent rather than transient. ledger.entry is append-only,
// so a committed row on sequence 1 can never be removed, and it was observed here as
// a TRADE_CASH/FEE journal pair that made TestLedgerUpdateAndDeleteAreRejected and
// TestInternalLedgerEntryMustBelongToAJournal fail on ledger_entry_sequence_idx for
// every subsequent run against that database. The suite's own remedy -- reset the
// disposable database -- works, but a defect that appears once in a hundred thousand
// runs and then poisons the database forever is not something to detect by running
// the suite enough times.
//
// These tests set the nonce deliberately instead, so the degenerate case is exercised
// on every run rather than once in a hundred thousand.

// clockBase is a nonce of the shape time.Now().UTC().UnixNano() actually produces.
//
// Every fixture below is found by searching forward from here rather than hardcoded,
// because a hand-picked nonce has to be masked before it means anything: 1<<30 masks
// to 0 and therefore collapses, which is not obvious from the number and was wrong
// here once already.
const clockBase = int64(1_700_000_000_000_000_000)

// nonceWithMultiplier returns the first nonce at or after from whose run multiplier is
// want, so a fixture can name a multiplier rather than a magic constant.
func nonceWithMultiplier(t *testing.T, from, want int64) int64 {
	t.Helper()
	for c := from; c < from+2_000_000; c++ {
		if (c&0x3fffffff)%100000 == want {
			return c
		}
	}
	t.Fatalf("no nonce with run multiplier %d within two million of %d", want, from)
	return 0
}

// withRunNonce runs fn with the run nonce set to n, restoring the previous value
// afterwards.
//
// SetRunNonce is package-global state, so a test that left it changed would silently
// alter every canonical id and sequence in the rest of the suite. Restoring it in a
// defer is what keeps this test local.
func withRunNonce(t *testing.T, n int64, fn func()) {
	t.Helper()
	previous := dbtest.RunNonce()
	t.Cleanup(func() { dbtest.SetRunNonce(previous) })
	dbtest.SetRunNonce(n)
	fn()
}

// TestUniqueSequenceNeverReturnsALiteral is the regression test for defect 53.
//
// When the run multiplier collapses to zero these calls must still land above every
// sequence literal in the suite. Before the floor they returned 1, 2 and 3, which is
// what wrote a permanent duplicate onto an append-only table.
func TestUniqueSequenceNeverReturnsALiteral(t *testing.T) {
	const literalCeiling = 1000

	// Two nonces with a zero multiplier: the trivial one, and one of the shape a real
	// clock produces. A fix that only handled the small value would still fail in use.
	for _, nonce := range []int64{0, nonceWithMultiplier(t, clockBase, 0)} {
		if m := (nonce & 0x3fffffff) % 100000; m != 0 {
			t.Fatalf("fixture nonce %d has multiplier %d, so it does not exercise the "+
				"collapsed case this test exists for", nonce, m)
		}
		withRunNonce(t, nonce, func() {
			for n := 1; n <= 20; n++ {
				got := dbtest.UniqueSequence(n)
				if got <= literalCeiling {
					t.Fatalf("with run nonce %d (multiplier 0), UniqueSequence(%d) = %d, which "+
						"is inside the range other tests hardcode; ledger.entry.sequence is globally "+
						"unique and append-only, so this row could never be removed and would fail "+
						"an unrelated test on every later run", nonce, n, got)
				}
			}
		})
	}
}

// TestUniqueSequenceSeparatesRuns states the property that actually matters and that
// the collapsed-multiplier case must not have broken: two runs whose multipliers
// differ never share a sequence, including when one of them is the degenerate run.
//
// The boundary that matters is a zero multiplier next to its neighbour, because that
// is the adjacency the arithmetic is most likely to get wrong. So the neighbouring
// multipliers are 1 and 2, one step away from the degenerate case.
func TestUniqueSequenceSeparatesRuns(t *testing.T) {
	nonces := []int64{
		nonceWithMultiplier(t, clockBase, 0),
		nonceWithMultiplier(t, clockBase, 1),
		nonceWithMultiplier(t, clockBase, 2),
	}

	seen := make(map[int64]int64, len(nonces)*20)
	for _, nonce := range nonces {
		withRunNonce(t, nonce, func() {
			for n := 1; n <= 20; n++ {
				got := dbtest.UniqueSequence(n)
				if prior, taken := seen[got]; taken {
					t.Fatalf("run nonce %d asked for sequence %d and got %d, which run nonce %d "+
						"already produced", nonce, n, got, prior)
				}
				seen[got] = nonce
			}
		})
	}
}

// TestUniqueSequenceCollidesWithAnotherCoincidentalRun records the limitation the
// floor does NOT remove, so it is not mistaken for a solved problem.
//
// UniqueSequence derives its multiplier from a nanosecond clock. Two separate runs
// whose nonces share a masked multiplier therefore produce the same sequences --
// roughly once in a hundred thousand runs. The floor cannot fix that, because the
// multiplier is the only input that varies and there is nothing left to separate
// them with.
//
// What the floor changes is the severity, which is the part worth stating. Before it,
// such a coincidence returned the bare literals, collided with tests that hardcode
// sequence 1, and poisoned an append-only table permanently. Now the colliding pair
// stays out of the literal space, so the visible effect is a duplicate-key failure
// inside the two runs that actually share sequences -- an ordinary, self-diagnosing
// test failure that a reset clears, rather than an unrelated test failing forever.
func TestUniqueSequenceCollidesWithAnotherCoincidentalRun(t *testing.T) {
	first := nonceWithMultiplier(t, clockBase, 0)
	second := first + 1<<30 // masks to 1<<30, so also a zero multiplier

	withRunNonce(t, first, func() {
		a := dbtest.UniqueSequence(1)
		withRunNonce(t, second, func() {
			b := dbtest.UniqueSequence(1)
			if m := (second & 0x3fffffff) % 100000; m != 0 {
				t.Fatalf("fixture nonce %d has multiplier %d; this test needs two nonces that "+
					"collapse to zero", second, m)
			}
			if a != b {
				t.Fatalf("nonces %d and %d both have a zero multiplier, so UniqueSequence(1) "+
					"should have returned the same value for both: %d then %d", first, second, a, b)
			}
			if a <= 1000 {
				t.Fatalf("the coincidental runs produced sequence %d, which is inside the range "+
					"other tests hardcode; the floor is meant to prevent exactly this", a)
			}
		})
	})
}

// TestUniqueSequenceStaysPositiveAndOrdered checks the two properties a caller
// silently relies on.
//
// Positivity because the column is a sequence number and callers use it in ordering
// comparisons. Ordering because tests that request 1 then 2 and then 3 expect to
// receive them in that order, and a formula that reordered them would produce a
// ledger whose entries disagree with the order the test asked for.
//
// The zero multiplier is included because adding the floor is what could plausibly
// disturb either property.
func TestUniqueSequenceStaysPositiveAndOrdered(t *testing.T) {
	nonces := []int64{
		nonceWithMultiplier(t, clockBase, 0),
		nonceWithMultiplier(t, clockBase, 1),
		nonceWithMultiplier(t, clockBase, 99999),
	}

	for _, nonce := range nonces {
		withRunNonce(t, nonce, func() {
			previous := dbtest.UniqueSequence(0)
			for n := 1; n <= 20; n++ {
				got := dbtest.UniqueSequence(n)
				if got <= 0 {
					t.Fatalf("with run nonce %d, UniqueSequence(%d) = %d; ledger.entry.sequence "+
						"must be a positive number", nonce, n, got)
				}
				if got <= previous {
					t.Fatalf("with run nonce %d, UniqueSequence(%d) = %d, which is not greater "+
						"than the %d returned for %d; callers request sequences in order and "+
						"expect to receive them in order", nonce, n, got, previous, n-1)
				}
				previous = got
			}
		})
	}
}

// TestCanonicalIDIsUnaffectedByTheSequenceFloor states that the two namespace fixes
// are independent, and that canonical ids remain deterministic per run.
//
// This is worth one test because both fixes live in this package and both were made
// in the same sitting: if the sequence floor were ever folded into the canonical-id
// inputs, every committed canonical id in the database would become unreproducible
// at once. Two properties are asserted, in opposite directions -- stable within a
// run, different across runs -- because a change that broke determinism and a change
// that broke run-to-run distinctness look identical from either side alone.
func TestCanonicalIDIsUnaffectedByTheSequenceFloor(t *testing.T) {
	nonce := nonceWithMultiplier(t, clockBase, 0)

	withRunNonce(t, nonce, func() {
		first := dbtest.CanonicalID("led", 61)
		if first == "" {
			t.Fatal("CanonicalID returned an empty identifier")
		}
		// Deterministic within a run: committed ledger rows are addressed by this
		// value and cannot be re-minted, so a second call must reproduce it exactly.
		if again := dbtest.CanonicalID("led", 61); again != first {
			t.Fatalf("CanonicalID(%q, 61) is not deterministic within a run: %q then %q",
				"led", first, again)
		}

		// Distinct across runs, which is the whole reason the run nonce exists.
		withRunNonce(t, nonce+1, func() {
			if other := dbtest.CanonicalID("led", 61); other == first {
				t.Fatalf("CanonicalID(%q, 61) is %q under two different run nonces; the per-run "+
					"nonce is what keeps committed identifiers from colliding across runs",
					"led", other)
			}
		})
	})
}
