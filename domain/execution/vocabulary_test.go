package execution_test

import (
	"testing"

	"github.com/aitc/trade/domain/execution"
)

// UndeterminedPrior is the predicate the no-retry rule turns on, and it is pure, so
// it is tested here rather than only through the command that calls it.
//
// The case that matters most is the fourth: an undetermined attempt followed by a
// determinate one is unreachable through legal transitions, so a scan that stopped
// at the last attempt and one that scanned the whole history would agree on every
// reachable input. Scanning is still the right answer, because the unreachable
// case is exactly the one where a history assembled by hand or restored from a
// backup can disagree with the machine.
func TestUndeterminedPriorFindsOnlyStillUndeterminedAttempts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prior []execution.Attempt
		want  bool
	}{
		{"no history", nil, false},
		{"all determinate", []execution.Attempt{
			{Outcome: execution.AcceptedOutcome}, {Outcome: execution.RejectedOutcome}}, false},
		{"one undetermined", []execution.Attempt{
			{Outcome: execution.RejectedOutcome}, {Outcome: execution.UndeterminedOutcome}}, true},
		{"several undetermined", []execution.Attempt{
			{Outcome: execution.UndeterminedOutcome}, {Outcome: execution.UndeterminedOutcome}}, true},
		// An undetermined attempt followed by a determinate one is unreachable:
		// the retry that would produce the second attempt is itself refused while
		// the first is undetermined. The answer for a state the machine cannot
		// reach is the conservative one -- if any attempt was never determined,
		// the venue may still hold that order, so a further send is refused.
		{"an undetermined attempt anywhere blocks", []execution.Attempt{
			{Outcome: execution.UndeterminedOutcome}, {Outcome: execution.RejectedOutcome}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := execution.UndeterminedPrior(tc.prior) != nil
			if got != tc.want {
				t.Errorf("UndeterminedPrior != nil = %v, want %v", got, tc.want)
			}
		})
	}
}

// No retry may become a second exposure-increasing command. Every attempt in the
// history of an order that reaches a venue must be accounted for.
//
// Attempt numbering is a property of the durable rows rather than of anything in
// memory, so it is asserted against execution.submission by
// TestASecondAttemptWithReconciliationIsRecordedAsAttemptTwo rather than here.
