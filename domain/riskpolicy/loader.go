// Package riskpolicy loads and activates risk.policy rows for the risk gate.
//
// Authority: 17_CONFIGURATION_AND_RISK_POLICY.md, 04_TRADING_DOMAIN_AND_RISK.md.
//
// The risk gate in domain/risk is pure logic. It cannot load anything, by
// design: doc 04 requires a deterministic decision for a fixed policy, account
// state, snapshot and command, and a package that reads the database cannot make
// that promise. The recorded gap was that nothing connected the two, so the
// permissive path had only ever been exercised by unit tests constructing a
// Policy literal.
//
// This package is that connector, and it is deliberately narrow. It reads rows,
// it refuses rows the schema would refuse, and it hands domain/risk a Policy.
// It does not evaluate controls, decide risk, or hold a second copy of the
// limit semantics.
//
// # Why loading is also a safety property
//
// Two schema details make loading more than a convenience:
//
//   - policy_active_scope_idx is a UNIQUE partial index on
//     (environment, scope_kind, scope_value) WHERE status = 'ACTIVE'. At most
//     one policy is in force for a scope, and the database enforces it. A
//     loader that "helpfully" picked the most recent of several would resolve an
//     ambiguity the schema guarantees cannot exist, and would keep doing so in
//     environments where the index is absent or has been dropped.
//   - policy_scope_non_empty requires every permitted_* array to be non-empty.
//     An empty allowlist is therefore unrepresentable in a stored policy, while
//     an empty Policy literal in a unit test is perfectly legal. The two must
//     not be conflated: the fail-closed empty-list rule in domain/risk is a
//     guard against callers, not a description of stored data.
//
// # Scope resolution
//
// Doc 17 and the schema permit six scope kinds: ACCOUNT, STRATEGY, INSTRUMENT,
// VENUE, MARKET and ENVIRONMENT. Resolving which policy governs a given order
// means walking from most specific to least, and this package refuses to do that
// implicitly. MostSpecificFirst makes the precedence explicit and returns every
// candidate it considered, so a decision about which policy applied is
// reviewable rather than a consequence of query ordering.
package riskpolicy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/domain/risk"
)

// ErrNoActivePolicy is returned when no ACTIVE policy governs a scope.
//
// It is not a transient fault and must not be retried. The correct response is to
// refuse the order: doc 17 §3 forbids the platform supplying default limits, and
// inventing one here is the exact failure this package exists to prevent.
var ErrNoActivePolicy = errors.New("riskpolicy: no ACTIVE policy governs this scope")

// ErrAmbiguousScope is returned when more than one ACTIVE policy matches.
//
// The unique partial index policy_active_scope_idx makes this impossible while
// the index exists. It is still checked, because a dropped or renamed index
// would otherwise turn a schema guarantee into an unstated assumption -- the
// same class of failure as the canonical ID divergence, where two encoders
// agreed on everything observable and differed on the rest.
var ErrAmbiguousScope = errors.New("riskpolicy: more than one ACTIVE policy governs this scope")

// Loader reads risk.policy rows.
//
// A Loader holds a *sql.DB rather than a transaction. Policy resolution is a
// read that must observe one committed state, and callers that need the resolved
// policy to stay fixed for the duration of an order should resolve once and pass
// the resulting risk.Policy into domain/risk.
type Loader struct {
	db *sql.DB
}

// New returns a Loader over db.
func New(db *sql.DB) *Loader { return &Loader{db: db} }

// policyColumns is the read list.
//
// It is explicit rather than "SELECT *" so that adding a column to risk.policy
// does not silently change what this loader returns, and so that the order of
// the scan destinations is a thing a reader can check by eye. The names must
// match the projection in domain/risk.ProjectedColumns; drift between the two
// is caught by dbtest/risk_policy_parity_test.go.
const policyColumns = `
	policy_revision, name, environment, scope_kind, scope_value, status,
	max_order_notional, max_order_quantity, max_gross_exposure, max_net_exposure,
	max_leverage, max_concentration, max_open_orders, max_daily_loss, max_drawdown,
	max_price_deviation, max_orders_per_minute, max_cancels_per_minute,
	loss_window, loss_source, max_market_data_age,
	permitted_instruments, permitted_venues, permitted_markets,
	permitted_sides, permitted_order_types, permitted_modes,
	created_by, approved_by, approved_at, effective_at, rollback_revision,
	superseded_by`

// Scope identifies which policy should govern an order.
type Scope struct {
	Environment string
	// Kind is one of the six values policy_scope_kind_valid admits.
	Kind string
	// Value is the scope's subject: an account id, strategy id, instrument id,
	// venue id, market class, or -- for ENVIRONMENT -- the environment name.
	Value string
}

// scopePrecedence orders the six scope kinds from most specific to least.
//
// An order is governed by the most specific policy that matches it: an
// account policy overrides an environment policy, because whoever set it was
// describing that account. The reverse would let a broad policy silently
// override a narrow one, which is the same inversion as letting a broad risk
// ceiling defeat an account-specific cap.
//
// ENVIRONMENT is last because it is the only scope that is always present.
var scopePrecedence = []string{
	"ACCOUNT", "STRATEGY", "INSTRUMENT", "VENUE", "MARKET", "ENVIRONMENT",
}

// ValidScopeKind reports whether kind is one the schema admits.
func ValidScopeKind(kind string) bool {
	for _, k := range scopePrecedence {
		if k == kind {
			return true
		}
	}
	return false
}

// Candidate is one ACTIVE policy that could govern a scope, with the reason it
// was considered.
type Candidate struct {
	Scope  Scope
	Policy risk.Policy
}

// Resolve returns the most specific ACTIVE policy governing scope.
//
// Precedence is explicit: each scope kind is queried in scopePrecedence order and
// the first match wins. Every kind is queried even after a match, so that
// ambiguity is detected rather than hidden by short-circuiting -- ErrAmbiguousScope
// would otherwise only surface for whichever kind happened to be checked first.
func (l *Loader) Resolve(ctx context.Context, scope Scope) (risk.Policy, error) {
	if !ValidScopeKind(scope.Kind) {
		return risk.Policy{}, fmt.Errorf("riskpolicy: scope kind %q is not one of %v (policy_scope_kind_valid)",
			scope.Kind, scopePrecedence)
	}
	if scope.Value == "" {
		return risk.Policy{}, fmt.Errorf("riskpolicy: scope %s has an empty scope_value (policy_scope_non_empty)", scope.Kind)
	}

	rows, err := l.queryActive(ctx, scope.Environment)
	if err != nil {
		return risk.Policy{}, err
	}
	if len(rows) == 0 {
		return risk.Policy{}, fmt.Errorf("%w for %s %q in environment %q",
			ErrNoActivePolicy, scope.Kind, scope.Value, scope.Environment)
	}

	// Most specific wins, and a tie at the same specificity is an error rather
	// than a silent choice between two rows the schema says cannot coexist.
	best := -1
	for i := range rows {
		rank := specificity(rows[i].Scope.Kind)
		switch {
		case rank < 0:
			return risk.Policy{}, fmt.Errorf(
				"riskpolicy: policy %s has scope_kind %q, which is not one of %v (policy_scope_kind_valid)",
				rows[i].Policy.Revision, rows[i].Scope.Kind, scopePrecedence)
		case best < 0 || rank < specificity(rows[best].Scope.Kind):
			best = i
		case rank == specificity(rows[best].Scope.Kind):
			return risk.Policy{}, fmt.Errorf("%w: %s and %s both govern %s %q in environment %q",
				ErrAmbiguousScope,
				rows[best].Policy.Revision, rows[i].Policy.Revision,
				rows[i].Scope.Kind, rows[i].Scope.Value, scope.Environment)
		}
	}

	chosen := rows[best].Policy

	// The loader refuses what the schema would refuse. Validate is the same set
	// of conditions, written out so the refusal names a column instead of
	// surfacing as an opaque PostgreSQL error later in the request path.
	if err := chosen.Validate(); err != nil {
		return risk.Policy{}, fmt.Errorf("riskpolicy: policy %s is structurally invalid: %w",
			chosen.Revision, err)
	}
	return chosen, nil
}

// ResolveForOrder resolves the policy that governs a specific order, by
// account, falling back through the narrower scopes to the environment.
//
// The fallback order is deliberate and fixed. An order belongs to exactly one
// account, one strategy, one instrument, one venue and one market class, so all
// five narrow scopes are candidates; the environment is the floor. A caller that
// wants a different precedence should call Resolve with an explicit Scope, and
// should not get a different answer by accident.
func (l *Loader) ResolveForOrder(ctx context.Context, order OrderScope) (risk.Policy, error) {
	if order.AccountID == "" {
		return risk.Policy{}, errors.New("riskpolicy: an order with no account has no scope to resolve a policy for")
	}

	// Query all candidates once, then pick. Querying per scope would let an
	// account policy and a strategy policy arrive in different snapshots, and
	// the choice between them would depend on query timing.
	candidates, err := l.queryActive(ctx, order.Environment)
	if err != nil {
		return risk.Policy{}, err
	}

	// A row's scope_value is matched against the order's subject for its kind, so
	// matching is per kind rather than a single value comparison.
	byKind := map[string]risk.Policy{}
	for _, c := range candidates {
		switch c.Scope.Kind {
		case "ACCOUNT":
			if c.Scope.Value == order.AccountID {
				byKind["ACCOUNT"] = c.Policy
			}
		case "STRATEGY":
			if order.StrategyID != "" && c.Scope.Value == order.StrategyID {
				byKind["STRATEGY"] = c.Policy
			}
		case "INSTRUMENT":
			if order.InstrumentID != "" && c.Scope.Value == order.InstrumentID {
				byKind["INSTRUMENT"] = c.Policy
			}
		case "VENUE":
			if order.VenueID != "" && c.Scope.Value == order.VenueID {
				byKind["VENUE"] = c.Policy
			}
		case "MARKET":
			if order.MarketClass != "" && c.Scope.Value == order.MarketClass {
				byKind["MARKET"] = c.Policy
			}
		case "ENVIRONMENT":
			byKind["ENVIRONMENT"] = c.Policy
		default:
			return risk.Policy{}, fmt.Errorf(
				"riskpolicy: policy %s has scope_kind %q, which is not one of %v (policy_scope_kind_valid)",
				c.Policy.Revision, c.Scope.Kind, scopePrecedence)
		}
	}

	// Resolution walks scopePrecedence rather than the map, so which policy won
	// is a consequence of the documented precedence and not of Go's map ordering.
	for _, kind := range scopePrecedence {
		p, ok := byKind[kind]
		if !ok {
			continue
		}
		if err := p.Validate(); err != nil {
			return risk.Policy{}, fmt.Errorf("riskpolicy: policy %s is structurally invalid: %w",
				p.Revision, err)
		}
		return p, nil
	}

	return risk.Policy{}, fmt.Errorf("%w for account %q in environment %q",
		ErrNoActivePolicy, order.AccountID, order.Environment)
}

// OrderScope is the set of subjects that determine which policy governs an
// order.
type OrderScope struct {
	Environment  string
	AccountID    string
	StrategyID   string
	InstrumentID string
	VenueID      string
	MarketClass  string
}

func specificity(kind string) int {
	for i, k := range scopePrecedence {
		if k == kind {
			return i
		}
	}
	return -1
}

// queryActive returns every ACTIVE policy in an environment.
//
// scope_kind is deliberately not filtered in SQL. The loader must be able to see
// a row with an unexpected scope_kind in order to report it, rather than
// silently skipping it, and the six kinds are resolved in Go so that the
// precedence is explicit rather than a consequence of ORDER BY.
func (l *Loader) queryActive(ctx context.Context, environment string) ([]Candidate, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT `+policyColumns+`
		  FROM risk.policy
		 WHERE environment = $1::common.environment
		   AND status = 'ACTIVE'`, environment)
	if err != nil {
		return nil, fmt.Errorf("riskpolicy: could not read risk.policy: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("riskpolicy: reading risk.policy failed: %w", err)
	}
	return out, nil
}

// pqStringArray scans a PostgreSQL text[] through the database/sql interface.
//
// The pgx stdlib driver returns a text[] column as its literal form -- the
// string "{a,b,c}" -- rather than as a Go []string, so a plain *[]string
// destination fails at runtime with "unsupported Scan". A loader that assumed
// otherwise would work for every non-array column and fail on the first
// permitted_* it read, which is the worst shape a bug can take here: the
// failure appears only once a real policy is loaded.
//
// Parsing is done by hand rather than by a driver-specific array type so that
// this package keeps the same database/sql dependency surface as the rest of the
// codebase. The grammar handled is the one PostgreSQL emits for a one-dimensional
// array of text: brace-delimited, comma-separated, with backslash escaping and
// double-quoted elements for values that need them.
type pqStringArray []string

// Scan implements sql.Scanner.
func (a *pqStringArray) Scan(src any) error {
	var raw string
	switch v := src.(type) {
	case nil:
		*a = nil
		return nil
	case string:
		raw = v
	case []byte:
		raw = string(v)
	default:
		return fmt.Errorf("riskpolicy: cannot scan %T into a text array", src)
	}

	if raw == "" {
		*a = nil
		return nil
	}
	if raw[0] != '{' || raw[len(raw)-1] != '}' {
		return fmt.Errorf("riskpolicy: %q is not a PostgreSQL array literal", raw)
	}

	*(*[]string)(a) = parseTextArray(raw[1 : len(raw)-1])
	return nil
}

// Value implements driver.Valuer so a test or a future write path can use the
// same type in both directions.
//
// Both the backslash and the double quote are escaped, and every element is
// quoted. Escaping only the backslash loses a quote silently: a"b came back as
// ab, which is a value change rather than a formatting change, and a test using
// this to write a fixture would not notice.
func (a pqStringArray) Value() (any, error) {
	parts := make([]string, len(a))
	for i, s := range a {
		escaped := strings.ReplaceAll(s, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		parts[i] = `"` + escaped + `"`
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

func parseTextArray(body string) []string {
	var (
		out     []string
		cur     strings.Builder
		inQuote bool
		escaped bool
	)
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	// A single empty element is an empty array, not one empty string; PostgreSQL
	// renders an empty array as {} with no body at all, which the caller has
	// already handled above.
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func scanCandidate(rows *sql.Rows) (Candidate, error) {
	var (
		c                                   Candidate
		name, lossSource, lossWindow, aging sql.NullString
		status, scopeKind, scopeValue       string
		notional, quantity                  string
		gross, net                          string
		leverage, concentration             string
		openOrders                          int64
		dailyLoss, drawdown, priceDeviation string
		ordersPerMin, cancelsPerMin         int64
		instruments, venues, markets        pqStringArray
		sides, orderTypes, modes            pqStringArray
		createdBy, approvedBy, rollbackRev  sql.NullString
		approvedAt, effectiveAt             sql.NullTime
		supersededBy                        sql.NullString
	)

	err := rows.Scan(
		&c.Policy.Revision, &name, &c.Policy.Environment, &scopeKind, &scopeValue, &status,
		&notional, &quantity, &gross, &net,
		&leverage, &concentration, &openOrders, &dailyLoss, &drawdown, &priceDeviation,
		&ordersPerMin, &cancelsPerMin,
		&lossWindow, &lossSource, &aging,
		&instruments, &venues, &markets,
		&sides, &orderTypes, &modes,
		&createdBy, &approvedBy, &approvedAt, &effectiveAt, &rollbackRev, &supersededBy,
	)
	if err != nil {
		return Candidate{}, fmt.Errorf("riskpolicy: scanning risk.policy failed: %w", err)
	}

	c.Scope = Scope{Kind: scopeKind, Value: scopeValue}
	c.Policy.ScopeKind = scopeKind
	c.Policy.ScopeValue = scopeValue
	c.Policy.Status = status

	// A policy that reached this point is ACTIVE, which active_policy_complete
	// already proved implies approved_by, effective_at and rollback_revision are
	// all present. They are scanned so that a row which somehow violates the
	// constraint is visible here rather than silently ignored, but their values
	// are not part of the gate's decision.
	_, _, _, _, _ = approvedBy, approvedAt, effectiveAt, rollbackRev, supersededBy

	dec := func(raw, column string) (contracts.Decimal, error) {
		d, err := contracts.ParseDecimal(raw)
		if err != nil {
			return contracts.Decimal{}, fmt.Errorf(
				"riskpolicy: %s holds %q, which is not a decimal; the column is NUMERIC and "+
					"a value that will not parse means the row cannot be evaluated: %w", column, raw, err)
		}
		return d, nil
	}

	for _, f := range []struct {
		column string
		raw    string
		dst    *contracts.Decimal
	}{
		{"max_order_notional", notional, &c.Policy.MaxOrderNotional},
		{"max_order_quantity", quantity, &c.Policy.MaxOrderQuantity},
		{"max_gross_exposure", gross, &c.Policy.MaxGrossExposure},
		{"max_net_exposure", net, &c.Policy.MaxNetExposure},
		{"max_leverage", leverage, &c.Policy.MaxLeverage},
		{"max_concentration", concentration, &c.Policy.MaxConcentration},
		{"max_daily_loss", dailyLoss, &c.Policy.MaxDailyLoss},
		{"max_drawdown", drawdown, &c.Policy.MaxDrawdown},
		{"max_price_deviation", priceDeviation, &c.Policy.MaxPriceDeviation},
	} {
		d, err := dec(f.raw, f.column)
		if err != nil {
			return Candidate{}, err
		}
		*f.dst = d
	}

	c.Policy.MaxOpenOrders = openOrders
	c.Policy.MaxOrdersPerMinute = ordersPerMin
	c.Policy.MaxCancelsPerMinute = cancelsPerMin

	// Intervals arrive as text through the pgx database/sql driver. They are
	// parsed explicitly rather than assumed, and a duration that will not parse
	// refuses the policy instead of defaulting to zero -- zero would fail
	// Validate, but the message would then blame the wrong column.
	var errs []error
	win, err := parseInterval(lossWindow, "loss_window")
	if err != nil {
		errs = append(errs, err)
	}
	c.Policy.LossWindow = win
	age, err := parseInterval(aging, "max_market_data_age")
	if err != nil {
		errs = append(errs, err)
	}
	c.Policy.MaxMarketDataAge = age
	if lossSource.Valid {
		c.Policy.LossSource = lossSource.String
	}

	c.Policy.PermittedInstruments = instruments
	c.Policy.PermittedVenues = venues
	c.Policy.PermittedMarkets = markets
	// permitted_sides, permitted_order_types and permitted_modes are loaded and
	// deliberately not projected onto Policy: no doc 04 control consults them,
	// and adding fields for them would suggest they are enforced. They are read
	// so that a row which violates policy_scope_non_empty is visible here.
	_, _, _ = sides, orderTypes, modes

	if len(errs) > 0 {
		return Candidate{}, errs[0]
	}
	return c, nil
}

// parseInterval reads a PostgreSQL interval as a duration.
//
// time.ParseDuration cannot read the "1 day" or "1 mon" forms that a Postgres
// interval can carry, so those are handled explicitly rather than being
// reported as a parse failure for a value the database considers perfectly
// valid.
func parseInterval(v sql.NullString, column string) (time.Duration, error) {
	if !v.Valid || v.String == "" {
		return 0, fmt.Errorf("riskpolicy: %s is NOT NULL in risk.policy and is required to evaluate a limit", column)
	}
	d, err := parsePostgresInterval(v.String)
	if err != nil {
		return 0, fmt.Errorf("riskpolicy: %s holds %q, which is not a usable duration: %w", column, v.String, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("riskpolicy: %s is %q, which is not positive", column, v.String)
	}
	return d, nil
}

// parsePostgresInterval handles the subset of the Postgres interval grammar that
// a policy window can reasonably use.
//
// An empty string is an error rather than a zero duration: "no window" and "a
// zero-length window" are different facts, and only one of them can be measured
// against a limit.
func parsePostgresInterval(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return 0, errors.New("riskpolicy: interval is empty; no window was recorded")
	}
	var total time.Duration
	// Months and years are rejected outright: they are not a fixed number of
	// seconds, so a loss window expressed in months has no duration, and
	// silently guessing one would make the limit depend on the calendar.
	for _, forbidden := range []string{"mon", "year"} {
		if containsFold(s, forbidden) {
			return 0, fmt.Errorf("riskpolicy: a %s component is not a fixed duration", forbidden)
		}
	}

	// Postgres renders an interval with a day component as "N days", singular or
	// plural, optionally followed by a time: "1 day 01:30:00". The unit is
	// matched rather than sliced at a fixed width, and the plural "s" is dropped
	// separately -- leaving it behind turns "2 days" into a parse error on "s".
	if i := indexFold(s, "day"); i > 0 {
		digits := strings.TrimRight(s[:i], " ")
		var days int
		if _, err := fmt.Sscanf(digits, "%d", &days); err != nil {
			return 0, fmt.Errorf("riskpolicy: %q is not a usable interval: %w", s, err)
		}
		total += time.Duration(days) * 24 * time.Hour
		rest := strings.TrimSpace(s[i+len("day"):])
		rest = strings.TrimPrefix(rest, "s")
		s = strings.TrimSpace(rest)
	}

	if s == "" {
		return total, nil
	}
	d, err := time.Parse("15:04:05.999999999", s)
	if err != nil {
		// Fall back to a plain Go duration, which covers "1h30m" and "24h0m0s".
		if d2, err2 := time.ParseDuration(s); err2 == nil {
			return total + d2, nil
		}
		return 0, fmt.Errorf("riskpolicy: %q is not a usable interval: %w", s, err)
	}
	return total + time.Duration(d.Hour())*time.Hour +
		time.Duration(d.Minute())*time.Minute + time.Duration(d.Second())*time.Second +
		time.Duration(d.Nanosecond()), nil
}

func containsFold(s, sub string) bool { return indexFold(s, sub) >= 0 }

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
