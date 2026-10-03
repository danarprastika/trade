package halt

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Querier is the read surface this package needs. It is declared here rather than
// imported from a sibling loader so that a caller holding a *sql.Tx, a *sql.DB or
// a fixture can all satisfy it without this package choosing a transaction
// boundary it does not own.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// activeHalts reads the ACTIVE halts recorded for one environment.
//
// Environment is the only dimension the query filters on, and that is deliberate.
// ops.halt.environment is NOT NULL, so every halt is environment-scoped and this
// cannot admit a halt from another environment. The remaining five dimensions are
// left to Effective, because their matching rule is per field rather than whole
// scope -- a MARKET_HALT on equities must not block a crypto order that happens
// to agree on the other five, and expressing that as SQL would duplicate a rule
// that already has one implementation and its own tests.
//
// Only ACTIVE rows are read. A cleared halt is not consulted at all rather than
// read and filtered, so a cleared row cannot be resurrected by a mistake in the
// state comparison.
const activeHalts = `
SELECT halt_id, level, state, environment, account_id, instrument_id,
       market_class, venue_id, strategy_id, reason, emergency, activated_at
  FROM ops.halt
 WHERE state = 'ACTIVE'
   AND environment = $1`

// LoadActive returns the active halts recorded for action's environment.
//
// An empty result is not an error: no active halt is the ordinary case, and
// callers distinguish it from a failure to read by handling the error, not by
// inspecting the slice.
func LoadActive(ctx context.Context, db Querier, action Scope) ([]Halt, error) {
	rows, err := db.QueryContext(ctx, activeHalts, action.Environment)
	if err != nil {
		return nil, fmt.Errorf("halt: reading active halts for %s: %w", action.Environment, err)
	}
	defer rows.Close()

	var out []Halt
	for rows.Next() {
		var (
			h                                    Halt
			level, state                         string
			environment                          string
			accountID, instrumentID, marketClass sql.NullString
			venueID, strategyID                  sql.NullString
			reason                               string
			emergency                            bool
			activatedAt                          time.Time
		)
		if err := rows.Scan(
			&h.ID, &level, &state, &environment,
			&accountID, &instrumentID, &marketClass,
			&venueID, &strategyID, &reason, &emergency, &activatedAt,
		); err != nil {
			return nil, fmt.Errorf("halt: scanning an active halt: %w", err)
		}

		h.Level = Level(level)
		if !h.Level.Known() {
			// An unrecognised level cannot be ranked, so it cannot be compared for
			// precedence. Skipping it would silently ignore the halt, and this
			// table's whole purpose is that nothing is silently ignored, so an
			// unknown level is a hard failure instead.
			return nil, fmt.Errorf(
				"halt: %s carries level %q, which this package does not know; "+
					"it cannot be ranked, so it cannot be safely ignored", h.ID, level)
		}

		h.State = State(state)
		h.Scope = Scope{
			Environment:  environment,
			AccountID:    accountID.String,
			InstrumentID: instrumentID.String,
			MarketClass:  marketClass.String,
			VenueID:      venueID.String,
			StrategyID:   strategyID.String,
		}
		h.Reason = reason
		h.Emergency = emergency
		h.ActivatedAt = activatedAt
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("halt: reading active halts for %s: %w", action.Environment, err)
	}
	return out, nil
}

// EvaluateAgainst reads ops.halt and decides whether the action is blocked.
//
// It is the sanctioned entry point for a caller holding a database handle, and
// it exists because risk.Request takes a HaltDecision that the caller could
// otherwise assert. An asserted decision is only as trustworthy as the caller:
// "consulted the halts, none apply" is indistinguishable from "never looked",
// and the blueprint makes a halt unwaivable, so the one control that must not be
// satisfiable by an assertion was the one satisfying an assertion.
//
// A read failure is an error rather than a clearance, because a halt table that
// could not be read has not been shown to be free of halts.
func EvaluateAgainst(ctx context.Context, db Querier, action Scope, riskReducing bool) (Decision, error) {
	halts, err := LoadActive(ctx, db, action)
	if err != nil {
		return Decision{}, err
	}
	return Evaluate(action, riskReducing, halts), nil
}
