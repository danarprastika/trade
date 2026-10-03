package execution

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Doc 05 is the authority for the transaction boundary, so it is a test input
// rather than a comment.
//
// This exists because of how the boundary was nearly got wrong. The durable
// submission record has to be committed before the venue is contacted, which reads
// like a transaction-design decision, and it was recorded as one -- an "open owner
// decision" in the project ledger. Doc 05 settles it in a single sentence: an
// authoritative command executes domain validation, state mutation, audit record
// creation and outbox insertion in one transaction. A rule that is already written
// down should not be left to a future reader to rediscover, and an implementation
// that silently drifted from it would be caught by neither the schema nor any test.
//
// Each clause below is required to appear in the document. Removing one fails the
// build rather than quietly weakening the rule it was supposed to state.

const doc05 = "05_PERSISTENCE_EVENTING_RECONCILIATION.md"

// docText reads the specification, or fails. Skipping is not an option: a test that
// silently passes because it could not find its input is indistinguishable from a
// test that passed because the rule holds.
func docText(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "docs", doc05)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s failed, and this test cannot check the rule without it: %v", doc05, err)
	}
	return string(raw)
}

// TestDoc05StillStatesTheSingleTransactionBoundary pins the sentence the command's
// structure implements. The clauses are matched individually so the failure names
// which one was dropped.
func TestDoc05StillStatesTheSingleTransactionBoundary(t *testing.T) {
	text := docText(t)

	clauses := map[string]string{
		"domain validation": `domain validation`,
		"state mutation":    `state mutation`,
		"audit record":      `audit record creation`,
		"outbox insertion":  `outbox insertion`,
		"one transaction":   `in one PostgreSQL transaction`,
	}

	for name, pattern := range clauses {
		if !regexp.MustCompile(pattern).MatchString(text) {
			t.Errorf("doc 05 no longer states the %s clause of the transaction boundary "+
				"(pattern %q). domain/execution/command.go implements that boundary, so the "+
				"document and the implementation have diverged and one of them is wrong", name, pattern)
		}
	}
}

// The sentence that has no trigger behind it. Doc 05 says a committed state
// mutation ALWAYS has a durable outbox record, and nothing in the schema enforces
// the pairing -- there is no trigger on oms."order" or execution.submission
// requiring it. It holds only because Command.Prepare writes one, so the rule is
// asserted here rather than assumed.
func TestDoc05StillStatesThatACommittedMutationAlwaysHasAnOutboxRecord(t *testing.T) {
	text := docText(t)

	if !strings.Contains(text, "A committed state mutation always has a corresponding durable outbox record") {
		t.Error("doc 05 no longer states that a committed state mutation always has a " +
			"corresponding durable outbox record. No trigger enforces that pairing, so this " +
			"sentence is the only thing making the requirement binding")
	}
}

// The outbox is read by a separate dispatcher, which is why the venue call is not
// inside the transaction. Doc 05's FOR UPDATE SKIP LOCKED is the mechanism, and it
// is the reason a caller may hold no lock across a network call to a venue.
func TestDoc05StillStatesTheDispatcherReadsCommittedRowsSeparately(t *testing.T) {
	text := docText(t)

	if !strings.Contains(text, "FOR UPDATE SKIP LOCKED") {
		t.Error("doc 05 no longer states that the dispatcher reads committed outbox rows using " +
			"FOR UPDATE SKIP LOCKED. The submit command commits intent and lets a separate reader " +
			"send it, and this sentence is why that is safe")
	}

	// Duplicate publication being acceptable is what allows the separation: the
	// dispatcher may send the same row twice, so correctness cannot rest on the
	// transaction having sent it.
	if !strings.Contains(text, "Duplicate publication is acceptable") {
		t.Error("doc 05 no longer states that duplicate publication is acceptable. The submit " +
			"command defers the send to the dispatcher and relies on this to remain correct if " +
			"the same row is dispatched twice")
	}
}
