package halt

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC)

func TestLevelSetIsClosed(t *testing.T) {
	for _, l := range Levels() {
		if !l.Known() {
			t.Errorf("%s is listed in Levels() but Known() says false", l)
		}
	}
	for _, l := range []Level{"", "halt", "GLOBAL_HALT", "SYSTEM_HALT "} {
		if l.Known() {
			t.Errorf("Level(%q).Known() = true, want false", l)
		}
	}
}

// Doc 04: SYSTEM_HALT > VENUE_HALT > MARKET_HALT > STRATEGY_HALT > ACCOUNT_HALT,
// and ops.halt_rank assigns 5/4/3/2/1. The Go rank must equal the SQL rank or
// the two layers disagree about which halt outranks which.
func TestRankMatchesTheDeclaredPrecedence(t *testing.T) {
	want := map[Level]int{
		LevelSystem: 5, LevelVenue: 4, LevelMarket: 3,
		LevelStrategy: 2, LevelAccount: 1,
	}
	for l, w := range want {
		if got := l.Rank(); got != w {
			t.Errorf("%s.Rank() = %d, want %d (ops.halt_rank)", l, got, w)
		}
	}
}

func TestRankIsStrictlyOrderedByPrecedence(t *testing.T) {
	ordered := []Level{LevelAccount, LevelStrategy, LevelMarket, LevelVenue, LevelSystem}
	for i := 1; i < len(ordered); i++ {
		if ordered[i].Rank() <= ordered[i-1].Rank() {
			t.Errorf("%s (rank %d) does not outrank %s (rank %d)",
				ordered[i], ordered[i].Rank(), ordered[i-1], ordered[i-1].Rank())
		}
	}
}

func TestUnknownLevelHasNoRank(t *testing.T) {
	if Level("GLOBAL_HALT").Rank() != 0 {
		t.Error("an undeclared level must not receive a rank; rank 0 would compare below every real halt")
	}
}

// A nil field means "every value at this dimension". Without that, a
// system-wide halt would have to be written once per instrument.
func TestEmptyFieldMatchesEveryValue(t *testing.T) {
	broad := Scope{Environment: "live"}
	narrow := Scope{
		Environment: "live", AccountID: "a-1", InstrumentID: "i-1",
		MarketClass: "CRYPTO", VenueID: "v-1", StrategyID: "s-1",
	}
	if !broad.Covers(narrow) {
		t.Error("a halt with only Environment set must cover an action that sets all six dimensions")
	}
	if narrow.Covers(broad) {
		t.Error("a fully-scoped halt must not cover an action that names no account; " +
			"the match must be field-wise in both directions")
	}
}

// Per-field matching is what makes a halt binding without being useless. A
// whole-scope equality test would mean a system halt matching one instrument
// does not match another.
func TestScopeMatchingIsFieldWise(t *testing.T) {
	halt := Scope{Environment: "live", MarketClass: "CRYPTO"}
	cryptoOrder := Scope{Environment: "live", MarketClass: "CRYPTO", InstrumentID: "btc-usd", VenueID: "v-1"}
	fxOrder := Scope{Environment: "live", MarketClass: "FX", InstrumentID: "eur-usd", VenueID: "v-1"}
	otherEnv := Scope{Environment: "paper", MarketClass: "CRYPTO", InstrumentID: "btc-usd", VenueID: "v-1"}

	if !halt.Covers(cryptoOrder) {
		t.Error("a CRYPTO halt in live must cover a live crypto order")
	}
	if halt.Covers(fxOrder) {
		t.Error("a CRYPTO halt must not cover an FX order; field-wise matching is the whole point")
	}
	if halt.Covers(otherEnv) {
		t.Error("a live halt must not cover a paper order")
	}
}

func TestEffectiveReturnsNothingWhenNoHaltApplies(t *testing.T) {
	action := Scope{Environment: "live", AccountID: "a-1", MarketClass: "CRYPTO"}
	halts := []Halt{
		{ID: "h-1", Level: LevelAccount, State: StateActive, Scope: Scope{Environment: "live", AccountID: "a-2"}},
		{ID: "h-2", Level: LevelMarket, State: StateCleared, Scope: Scope{Environment: "live"}},
		{ID: "h-3", Level: LevelSystem, State: StateActive, Scope: Scope{Environment: "paper"}},
	}
	h, ok := Effective(action, halts)
	if ok || h != nil {
		t.Fatalf("Effective = (%v, %v), want (nil, false): none of these halts govern the action", h, ok)
	}
}

// Doc 17 §5: halt activation is monotonic in severity and a lower-level command
// cannot clear a higher-level halt.
func TestHighestRankingApplicableHaltWins(t *testing.T) {
	action := Scope{Environment: "live", AccountID: "a-1", MarketClass: "CRYPTO", VenueID: "v-1"}
	halts := []Halt{
		{ID: "h-account", Level: LevelAccount, State: StateActive, Reason: "account under review",
			Scope: Scope{Environment: "live", AccountID: "a-1"}},
		{ID: "h-venue", Level: LevelVenue, State: StateActive, Reason: "venue degraded",
			Scope: Scope{Environment: "live", VenueID: "v-1"}},
		{ID: "h-system", Level: LevelSystem, State: StateActive, Reason: "audit chain broken",
			Scope: Scope{Environment: "live"}},
	}
	h, ok := Effective(action, halts)
	if !ok {
		t.Fatal("Effective reported no halt, but three apply")
	}
	if h.Level != LevelSystem {
		t.Errorf("Effective returned %s (%s), want SYSTEM_HALT: the highest-ranked applicable halt governs",
			h.Level, h.ID)
	}
}

func TestClearedHaltsAreIgnored(t *testing.T) {
	action := Scope{Environment: "live"}
	h, ok := Effective(action, []Halt{
		{ID: "h-1", Level: LevelSystem, State: StateCleared, Scope: Scope{Environment: "live"}},
	})
	if ok {
		t.Fatalf("Effective returned the CLEARED halt %s; a cleared halt does not govern anything", h.ID)
	}
}

// A specific halt is more useful to an operator than a broad one of the same
// class, so equal ranks break toward the earliest activation.
func TestEqualRankBreaksTowardEarliestActivation(t *testing.T) {
	action := Scope{Environment: "live", MarketClass: "CRYPTO"}
	halts := []Halt{
		{ID: "h-late", Level: LevelMarket, State: StateActive, ActivatedAt: t0.Add(time.Hour),
			Scope: Scope{Environment: "live"}},
		{ID: "h-early", Level: LevelMarket, State: StateActive, ActivatedAt: t0,
			Scope: Scope{Environment: "live", MarketClass: "CRYPTO"}},
	}
	h, ok := Effective(action, halts)
	if !ok {
		t.Fatal("no halt selected")
	}
	if h.ID != "h-early" {
		t.Errorf("selected %s, want h-early: at equal rank the earliest activation wins", h.ID)
	}
}

// This is the property that makes a halt a safety control rather than a
// shutdown. Refusing to let an operator reduce exposure during an incident
// converts a market problem into an unmanaged loss.
func TestRiskReducingActionsAreNotBlocked(t *testing.T) {
	action := Scope{Environment: "live", AccountID: "a-1"}
	halts := []Halt{
		{ID: "h-1", Level: LevelSystem, State: StateActive, Emergency: true,
			Reason: "audit chain broken", Scope: Scope{Environment: "live"}},
	}
	d := Evaluate(action, true, halts)
	if d.Blocked {
		t.Errorf("a risk-reducing action was blocked by %s: %s", d.Halt.Level, d.Reason)
	}
	if d.Halt == nil {
		t.Error("the governing halt should still be reported even when it does not block, " +
			"so the operator can see why the action was treated differently")
	}
}

func TestRiskIncreasingActionsAreBlocked(t *testing.T) {
	action := Scope{Environment: "live", AccountID: "a-1"}
	halts := []Halt{
		{ID: "h-1", Level: LevelSystem, State: StateActive,
			Reason: "audit chain broken", Scope: Scope{Environment: "live"}},
	}
	d := Evaluate(action, false, halts)
	if !d.Blocked {
		t.Fatal("a risk-increasing action was not blocked by an active SYSTEM_HALT")
	}
	if d.Halt == nil || d.Halt.ID != "h-1" {
		t.Errorf("decision does not name the governing halt: %+v", d.Halt)
	}
	if d.Reason == "" {
		t.Error("a block with no reason is not actionable during an incident")
	}
}

func TestNoHaltMeansNotBlocked(t *testing.T) {
	d := Evaluate(Scope{Environment: "live"}, false, nil)
	if d.Blocked {
		t.Errorf("blocked with no halts in force: %s", d.Reason)
	}
	if d.Reason == "" {
		t.Error("an allow decision must still carry a reason, so the audit record explains itself")
	}
}

// Every level must actually block, at every scope breadth. A level that was
// declared but never reached in a particular path is a level that does not work.
func TestEveryLevelBlocksWhenActive(t *testing.T) {
	action := Scope{Environment: "live", AccountID: "a-1", InstrumentID: "i-1",
		MarketClass: "CRYPTO", VenueID: "v-1", StrategyID: "s-1"}
	for _, l := range Levels() {
		t.Run(string(l), func(t *testing.T) {
			// Broadest scope: every field empty.
			broad := []Halt{{ID: "h", Level: l, State: StateActive, Scope: Scope{Environment: "live"}}}
			if d := Evaluate(action, false, broad); !d.Blocked {
				t.Errorf("%s with an environment-wide scope did not block: %s", l, d.Reason)
			}
			// Narrowest scope: every field set.
			narrow := []Halt{{ID: "h", Level: l, State: StateActive, Scope: action}}
			if d := Evaluate(action, false, narrow); !d.Blocked {
				t.Errorf("%s with an exactly-matching scope did not block: %s", l, d.Reason)
			}
		})
	}
}

// A halt carrying every dimension set must not also cover a different
// instrument. If it did, an ACCOUNT_HALT on one position would freeze the whole
// account, which is a different (and much larger) control than the one requested.
func TestFullyScopedHaltDoesNotLeakToOtherInstruments(t *testing.T) {
	halt := Scope{Environment: "live", AccountID: "a-1", InstrumentID: "i-1",
		MarketClass: "CRYPTO", VenueID: "v-1", StrategyID: "s-1"}
	other := Scope{Environment: "live", AccountID: "a-1", InstrumentID: "i-2",
		MarketClass: "CRYPTO", VenueID: "v-1", StrategyID: "s-1"}
	if halt.Covers(other) {
		t.Error("a fully-scoped halt covers a different instrument")
	}
}

func TestEffectiveIsStableAcrossRepeatedCalls(t *testing.T) {
	action := Scope{Environment: "live", VenueID: "v-1"}
	halts := []Halt{
		{ID: "h-low", Level: LevelAccount, State: StateActive, ActivatedAt: t0, Scope: Scope{Environment: "live"}},
		{ID: "h-high", Level: LevelVenue, State: StateActive, ActivatedAt: t0, Scope: Scope{Environment: "live", VenueID: "v-1"}},
	}
	first, _ := Effective(action, halts)
	for i := 0; i < 5; i++ {
		again, ok := Effective(action, halts)
		if !ok {
			t.Fatal("Effective stopped finding the halt on a repeated call")
		}
		if again.ID != first.ID {
			t.Fatalf("call %d returned %s, first call returned %s; resolution must be deterministic",
				i, again.ID, first.ID)
		}
	}
}

func TestLevelsReturnsACopy(t *testing.T) {
	first := Levels()
	first[0] = Level("GLOBAL_HALT")
	if Levels()[0] != LevelAccount {
		t.Error("mutating the returned slice changed the shared level table")
	}
}
