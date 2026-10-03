// Package halt resolves which halt, if any, governs a proposed trading action.
//
// Doc 04 sets the hierarchy: SYSTEM_HALT > VENUE_HALT > MARKET_HALT >
// STRATEGY_HALT > ACCOUNT_HALT, and states that "a higher-level halt cannot be
// bypassed by a lower-level enable command". This package implements the
// resolution, including the part that is easy to get wrong: a halt is scoped,
// and a halt whose scope does not cover the action does not apply to it.
//
// The relationship to SQL mirrors package oms. This package is authoritative;
// ops.effective_halt and ops.halt_rank in migrations 0007 and 0013 are the
// backstop. The parity test compares the rank function, the level enum and the
// scope-matching predicate against the database.
package halt

import (
	"fmt"
	"sort"
	"time"
)

// Level is a halt level. The names and the rank order come from the
// ops.halt_level enum and ops.halt_rank in migration 0007.
//
// Rank is not severity in the abstract -- it is precedence. A SYSTEM_HALT outranks
// a VENUE_HALT because clearing a venue problem must not clear a system problem.
type Level string

const (
	LevelAccount  Level = "ACCOUNT_HALT"
	LevelStrategy Level = "STRATEGY_HALT"
	LevelMarket   Level = "MARKET_HALT"
	LevelVenue    Level = "VENUE_HALT"
	LevelSystem   Level = "SYSTEM_HALT"
)

// levels is ordered weakest to strongest. Index+1 is the rank, matching
// ops.halt_rank exactly.
var levels = []Level{LevelAccount, LevelStrategy, LevelMarket, LevelVenue, LevelSystem}

var levelSet = func() map[Level]struct{} {
	m := make(map[Level]struct{}, len(levels))
	for _, l := range levels {
		m[l] = struct{}{}
	}
	return m
}()

// Levels returns every halt level, weakest first. The result is a copy.
func Levels() []Level {
	out := make([]Level, len(levels))
	copy(out, levels)
	return out
}

// Known reports whether l is a declared halt level.
func (l Level) Known() bool {
	_, ok := levelSet[l]
	return ok
}

// Rank returns the precedence of l, weakest at 1.
//
// ops.halt_rank returns SYSTEM 5, VENUE 4, MARKET 3, STRATEGY 2, ACCOUNT 1.
// The parity test asserts this function returns the same value as the database
// for every level, because a disagreement here would mean the Go service and the
// backstop disagree about which halt outranks which -- and the loser of that
// disagreement is whichever one an incident is reading.
func (l Level) Rank() int {
	for i, lv := range levels {
		if lv == l {
			return i + 1
		}
	}
	return 0
}

// String makes Level printable in test failures without a cast at every call.
func (l Level) String() string { return string(l) }

// Scope is the trading scope a halt applies to.
//
// A nil field means "every value at this dimension", which is how a system-wide
// halt is expressed without a separate representation. The field names are
// deliberately the same as the parameters of ops.effective_halt so the two are
// readable side by side.
type Scope struct {
	Environment  string
	AccountID    string
	InstrumentID string
	MarketClass  string
	VenueID      string
	StrategyID   string
}

// Covers reports whether this scope governs an action in the given scope.
//
// A halt with a nil field matches any value; a halt with a set field matches
// only that value. This is the `h.account_id IS NULL OR h.account_id =
// p_account_id` shape from ops.effective_halt.
//
// One detail is load-bearing and easy to lose: the comparison is per field, not
// whole-scope equality. A MARKET_HALT scoped to equities must not block a crypto
// order just because the other four fields happen to match. Field-wise
// matching is what makes a halt binding rather than decorative, and it is also
// what makes it correct -- the alternative, requiring an exact whole-scope
// match, means a system-wide halt has to be written once per instrument.
func (s Scope) Covers(action Scope) bool {
	return matchesDimension(s.Environment, action.Environment) &&
		matchesDimension(s.AccountID, action.AccountID) &&
		matchesDimension(s.InstrumentID, action.InstrumentID) &&
		matchesDimension(s.MarketClass, action.MarketClass) &&
		matchesDimension(s.VenueID, action.VenueID) &&
		matchesDimension(s.StrategyID, action.StrategyID)
}

func matchesDimension(haltValue, actionValue string) bool {
	return haltValue == "" || haltValue == actionValue
}

// State is whether a halt is currently in force.
type State string

const (
	StateActive  State = "ACTIVE"
	StateCleared State = "CLEARED"
)

// Halt is one in-force or cleared halt.
type Halt struct {
	ID          string
	Level       Level
	Scope       Scope
	State       State
	Reason      string
	ActivatedAt time.Time

	// Emergency marks a halt the platform raised on itself -- the
	// audit-chain SEV-1 case in migration 0013.
	//
	// This is not a cosmetic flag. An emergency SYSTEM_HALT exists because
	// something is wrong that no operator has yet understood, and clearing it
	// on a human's say-so would restore a system whose audit evidence is known
	// to be untrustworthy. The field exists so the resolution logic can refuse
	// to treat a platform-raised halt as an ordinary one.
	Emergency bool
}

// Active reports whether h is currently in force.
func (h Halt) Active() bool { return h.State == StateActive }

// AppliesTo reports whether h is active and scoped to cover the action.
func (h Halt) AppliesTo(action Scope) bool {
	return h.Active() && h.Scope.Covers(action)
}

// Effective returns the halt that governs the action, if any.
//
// Selection is the highest-ranked active halt covering the scope, matching
// ops.effective_halt's `ORDER BY ops.halt_rank(h.level) DESC, h.activated_at`.
// Ties at equal rank are broken by activation time, earliest first, so that a
// specific instrument halt is reported ahead of a broad one covering the same
// class -- the more specific cause is the more useful thing to tell an operator
// during an incident.
//
// Returns (nil, false) when nothing applies. The bool is explicit rather than
// relying on a nil check alone, because a typed nil inside an interface is a
// classic source of a silent "no halt" that is actually a halt.
func Effective(action Scope, halts []Halt) (*Halt, bool) {
	var candidates []Halt
	for _, h := range halts {
		if h.AppliesTo(action) {
			candidates = append(candidates, h)
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Level.Rank() != candidates[j].Level.Rank() {
			return candidates[i].Level.Rank() > candidates[j].Level.Rank()
		}
		return candidates[i].ActivatedAt.Before(candidates[j].ActivatedAt)
	})
	best := candidates[0]
	return &best, true
}

// Decision is the outcome of evaluating one proposed action.
type Decision struct {
	// Blocked reports whether the action is refused.
	Blocked bool

	// Halt is the governing halt when Blocked, nil otherwise.
	Halt *Halt

	// Reason is a human-readable explanation, always populated.
	Reason string
}

// Evaluate decides whether a proposed action is blocked by any active halt.
//
// A risk-reducing action is not blocked by a halt. That asymmetry is the entire
// reason a halt is a safety control rather than a shutdown: during an incident
// an operator must still be able to flatten a position, and a system that
// refused to do so would convert a market problem into a loss.
//
// The reduction test is not derived from the action's size or side. It is
// delegated to the caller's classification, because only package oms can say
// whether a given order type is risk-reducing, and inventing a second
// definition here is how the two would drift.
func Evaluate(action Scope, riskReducing bool, halts []Halt) Decision {
	h, applies := Effective(action, halts)
	if !applies {
		return Decision{Blocked: false, Reason: "no active halt governs this scope"}
	}

	if riskReducing {
		return Decision{
			Blocked: false,
			Halt:    h,
			Reason: fmt.Sprintf(
				"%s is in force but does not block a risk-reducing action; "+
					"an operator must retain the ability to reduce exposure during an incident",
				h.Level),
		}
	}

	return Decision{
		Blocked: true,
		Halt:    h,
		Reason: fmt.Sprintf(
			"blocked by %s %s (%s): %s", h.Level, h.ID, h.Scope, h.Reason),
	}
}
