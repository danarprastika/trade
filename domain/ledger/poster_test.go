package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
)

// contains reports whether s has the substring want. Used so a table can assert
// on the part of a refusal a caller would act on.
func contains(s, want string) bool { return strings.Contains(s, want) }

// dec is a small helper so the tables below read as amounts rather than as parse
// boilerplate. It panics on a bad literal, which in a test is the correct
// outcome: a typo in the fixture should fail loudly, not become a case.
func dec(t *testing.T, s string) contracts.Decimal {
	t.Helper()
	d, err := contracts.ParseDecimal(s)
	if err != nil {
		t.Fatalf("fixture amount %q is not a decimal: %v", s, err)
	}
	return d
}

// basePosting is a valid posting that individual tests mutate. Keeping the valid
// baseline in one place means a new refusal can be added to the tables below
// without every case re-deriving a complete posting.
func basePosting() Posting {
	at := time.Now().UTC()
	return Posting{
		SourceCommandID: "cmd_00000000000000000001",
		AccountID:       "acc_00000000000000000001",
		Environment:     contracts.EnvLive,
		Currency:        "USD",
		CorrelationID:   "corr-1",
		PolicyVersion:   "ledger-posting-v1",
		EffectiveAt:     at.Add(-time.Minute),
		OccurredAt:      at,
		ActorID:         "svc_ledger",
		Legs: []Leg{
			{Kind: KindTradeCash, Direction: Debit, Amount: contracts.MustParseDecimal("1000")},
			{Kind: KindTradeCash, Direction: Credit, Amount: contracts.MustParseDecimal("1000")},
		},
	}
}

func TestBalanceAcceptsAnExactlyBalancedPosting(t *testing.T) {
	sum, err := balance(basePosting().Legs)
	if err != nil {
		t.Fatalf("a balanced posting was refused: %v", err)
	}
	if got := sum.debit.String(); got != "1000" {
		t.Errorf("debit total = %s, want 1000", got)
	}
}

// TestBalanceIsExact is the reason Amount is a contracts.Decimal.
//
// In IEEE-754, 0.1 + 0.2 != 0.3, so a float-based checker would either reject a
// correct posting or pass an incorrect one depending on how it rounded. Here the
// three decimals must sum exactly.
func TestBalanceIsExact(t *testing.T) {
	legs := []Leg{
		{Kind: KindFee, Direction: Debit, Amount: dec(t, "0.1")},
		{Kind: KindFee, Direction: Credit, Amount: dec(t, "0.2")},
		{Kind: KindFee, Direction: Credit, Amount: dec(t, "0.3")},
	}
	// 0.1 + 0.2 vs 0.3 on one side: still unbalanced, and must be refused for the
	// right reason rather than passing a float comparison.
	if _, err := balance(legs); !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("debits 0.1 against credits 0.5 was not refused as unbalanced: %v", err)
	}

	balanced := []Leg{
		{Kind: KindFee, Direction: Debit, Amount: dec(t, "0.1")},
		{Kind: KindFee, Direction: Credit, Amount: dec(t, "0.2")},
		{Kind: KindFee, Direction: Debit, Amount: dec(t, "0.2")},
		{Kind: KindFee, Direction: Credit, Amount: dec(t, "0.3")},
		{Kind: KindFee, Direction: Debit, Amount: dec(t, "0.3")},
		{Kind: KindFee, Direction: Credit, Amount: dec(t, "0.1")},
	}
	if _, err := balance(balanced); err != nil {
		t.Fatalf("a posting that sums exactly was refused: %v", err)
	}
}

func TestBalanceRefusals(t *testing.T) {
	cases := []struct {
		name string
		legs []Leg
		want error
		// contains is a substring the message must carry, so the caller learns which
		// leg broke it rather than just that something did.
		contains string
	}{
		{
			name: "no legs",
			legs: nil,
			want: ErrNoLegs,
		},
		{
			name:     "a single debit is not a balanced posting",
			legs:     []Leg{{Kind: KindFee, Direction: Debit, Amount: dec(t, "100")}},
			want:     ErrUnbalanced,
			contains: "debits 100, credits 0",
		},
		{
			name: "debits and credits disagree",
			legs: []Leg{
				{Kind: KindFee, Direction: Debit, Amount: dec(t, "100")},
				{Kind: KindFee, Direction: Credit, Amount: dec(t, "99.99")},
			},
			want:     ErrUnbalanced,
			contains: "debits 100, credits 99.99",
		},
		{
			name: "an unknown direction is refused",
			legs: []Leg{
				{Kind: KindFee, Direction: "LEFT", Amount: dec(t, "100")},
			},
			want:     nil,
			contains: "direction \"LEFT\"",
		},
		{
			name: "a zero amount is refused",
			legs: []Leg{
				{Kind: KindFee, Direction: Debit, Amount: dec(t, "0")},
				{Kind: KindFee, Direction: Credit, Amount: dec(t, "0")},
			},
			want:     nil,
			contains: "positive amount",
		},
		{
			name: "a negative amount is refused rather than treated as the opposite direction",
			legs: []Leg{
				{Kind: KindFee, Direction: Debit, Amount: dec(t, "-100")},
				{Kind: KindFee, Direction: Credit, Amount: dec(t, "100")},
			},
			want:     nil,
			contains: "positive amount",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := balance(tc.legs)
			if err == nil {
				t.Fatalf("expected a refusal, got none")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.want)
			}
			if tc.contains != "" && !contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err, tc.contains)
			}
		})
	}
}

func TestValidateRefusals(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Posting)
		contains string
	}{
		{
			name:     "a posting with no cause",
			mutate:   func(p *Posting) { p.SourceCommandID = "" },
			contains: "source command id",
		},
		{
			name:     "a posting with no account",
			mutate:   func(p *Posting) { p.AccountID = "" },
			contains: "account id",
		},
		{
			name:     "a lowercase currency",
			mutate:   func(p *Posting) { p.Currency = "usd" },
			contains: "journal_currency_valid",
		},
		{
			name:     "a currency with a symbol in it",
			mutate:   func(p *Posting) { p.Currency = "US$" },
			contains: "journal_currency_valid",
		},
		{
			name:     "a posting with no correlation",
			mutate:   func(p *Posting) { p.CorrelationID = "" },
			contains: "correlation id",
		},
		{
			name:     "a posting with no actor",
			mutate:   func(p *Posting) { p.ActorID = "" },
			contains: "actor id",
		},
		{
			name:     "a posting with no policy version",
			mutate:   func(p *Posting) { p.PolicyVersion = "" },
			contains: "policy version",
		},
		{
			name:     "value learned before it moved",
			mutate:   func(p *Posting) { p.OccurredAt = p.EffectiveAt.Add(-time.Hour) },
			contains: "learned before it moved",
		},
		{
			name: "a correction kind with nothing to correct",
			mutate: func(p *Posting) {
				p.Legs[0].Kind = KindCorrection
			},
			contains: "entry_correction_kind_consistent",
		},
		{
			name: "a correction with no correction kind",
			mutate: func(p *Posting) {
				p.Legs[0].CorrectsEntryID = "led_00000000000000000009"
			},
			contains: "entry_correction_kind_consistent",
		},
		{
			name: "an entry kind outside the vocabulary",
			mutate: func(p *Posting) {
				p.Legs[0].Kind = Kind("PROFIT")
			},
			contains: "not in the ledger vocabulary",
		},
		{
			name: "an amount finer than the ledger can store",
			mutate: func(p *Posting) {
				// 19 decimal places. NUMERIC(38,18) would round this, so posting it
				// would move a different amount than the caller asked for.
				p.Legs[0].Amount = dec(t, "0.0000000000000000001")
				p.Legs[1].Amount = dec(t, "0.0000000000000000001")
			},
			contains: "NUMERIC(38,18)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := basePosting()
			tc.mutate(&in)
			err := validate(in)
			if err == nil {
				t.Fatalf("expected a refusal, got none")
			}
			if !contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err, tc.contains)
			}
		})
	}
}

func TestValidateAcceptsTheBaseline(t *testing.T) {
	if err := validate(basePosting()); err != nil {
		t.Fatalf("the baseline posting was refused: %v", err)
	}
}

// TestValidateAcceptsACorrectionPair covers the one asymmetry the correction
// constraint allows: a correction names an earlier entry, so both legs carry
// corrects_entry_id and both are CORRECTION.
func TestValidateAcceptsACorrectionPair(t *testing.T) {
	in := basePosting()
	original := "led_00000000000000000009"
	in.Legs = []Leg{
		{Kind: KindCorrection, Direction: Debit, Amount: dec(t, "1000"), CorrectsEntryID: original},
		{Kind: KindCorrection, Direction: Credit, Amount: dec(t, "1000"), CorrectsEntryID: original},
	}
	if err := validate(in); err != nil {
		t.Fatalf("a correction posting was refused: %v", err)
	}
}

func TestLegDigestsAreStableAndOrderSensitive(t *testing.T) {
	in := basePosting()
	at := in.EffectiveAt.UTC().Truncate(time.Microsecond)

	first := legDigests(in, at)
	second := legDigests(in, at)
	if len(first) != len(in.Legs) {
		t.Fatalf("got %d digests for %d legs", len(first), len(in.Legs))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("digest %d is not stable across calls: %s then %s", i, first[i], second[i])
		}
	}
	if first[0] == first[1] {
		t.Error("the two legs of a posting share a digest, so the unique index on " +
			"source_digest would refuse the second leg as a replay of the first")
	}
	for i, d := range first {
		if len(d) != 64 {
			t.Errorf("digest %d is %d characters, want 64 hex (entry_digest_format)", i, len(d))
		}
	}

	// Swapping the legs must change both digests: the legs are not
	// interchangeable, and a digest that ignored position would let a reordered
	// posting replay as the original. Only the legs move -- swapping directions as
	// well would restore the original posting exactly and assert nothing.
	swapped := basePosting()
	swapped.Legs[0], swapped.Legs[1] = swapped.Legs[1], swapped.Legs[0]
	reversed := legDigests(swapped, at)
	if reversed[0] == first[0] {
		t.Error("a posting with its legs reversed produced the first leg's original digest")
	}
}

// TestIsExternalMatchesTheDatabase covers the list the poster keeps in Go against
// the list ledger.journal_must_balance keeps in the function body. If they drift,
// the poster refuses a kind the database permits, or permits one it does not.
func TestIsExternalMatchesTheDatabase(t *testing.T) {
	all := []Kind{
		KindCashDeposit, KindCashWithdrawal, KindCorrection, KindFee, KindFunding,
		KindInterest, KindMarginChange, KindRealizedPnL, KindSettlement,
		KindTradeCash, KindTransfer, KindUnrealizedPnLAdjustment,
	}
	external := map[Kind]bool{}
	for _, k := range all {
		if k.IsExternal() {
			external[k] = true
		}
	}
	want := []Kind{KindCashDeposit, KindCashWithdrawal, KindFunding, KindInterest, KindMarginChange}
	if len(external) != len(want) {
		t.Errorf("%d kinds are treated as external, want %d", len(external), len(want))
	}
	for _, k := range want {
		if !external[k] {
			t.Errorf("%s is treated as internal, but journal_must_balance permits it unjournalled", k)
		}
	}
	for _, k := range all {
		if k.Known() == false {
			t.Errorf("%s is in the vocabulary list but not marked known", k)
		}
	}
}
