// Package reconcile compares the platform's own records against the venue's and
// opens a case for every difference it can establish.
//
// Doc 05, "Reconciliation": differences create cases with explicit severity, and a
// MATERIAL unresolved break BLOCKS affected risk-increasing scope. The table
// comments say the same: reconciliation runs continuously for live/paper adapters
// and at startup.
//
// THE LOAD-BEARING RULE
//
// A check that did not run is never reported as a check that found nothing.
//
// This is the whole design, and it is the failure mode an earlier draft of this
// package walked straight into. That draft asked the reader whether it could answer
// a question by looking for a method on it, found none, and concluded the check was
// inapplicable -- for every adapter, forever. The runs were COMPLETED. The counts
// were zero. Every report said reconciliation had found no differences, which is
// the most reassuring possible sentence and the one that must never be produced by
// a check that never looked.
//
// So a check here has exactly three outcomes, and they are not interchangeable:
//
//	COMPLETED  the check ran. It contributes to the counts. It may find nothing.
//	SKIPPED    the adapter has not certified the capability. A reason is recorded.
//	           It contributes NOTHING to the counts and does not imply agreement.
//	FAILED     the venue could not be asked. The run is FAILED. It contributes
//	           NOTHING to the counts and does not imply agreement.
//
// Only COMPLETED contributes. That is why the counts live on the check result rather
// than only on the run: a run total that summed across all three states would be a
// number nobody could interpret, because it would mix "we looked and agreed" with
// "we did not look".

package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aitc/trade/adapters/venue"
	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/services/audit"
)

// Status is one check's outcome.
type Status string

const (
	// Completed means the check ran against the venue.
	Completed Status = "COMPLETED"

	// Skipped means the adapter has not certified the capability this check needs.
	//
	// It is a fact about the DECLARATION, not about the venue's data, and it is
	// reported as its own state so it can never be summed into agreement.
	Skipped Status = "SKIPPED"

	// Failed means the check needed a capability, had it, and the venue could not
	// answer. An error is a failure to establish, never a clean result.
	Failed Status = "FAILED"
)

// Capability aliases venue.Capability so this package's checks read in doc 16
// section 4's vocabulary rather than importing the concrete type into every
// signature.
type Capability = venue.Capability

// The capability each check requires. Declared as data rather than implied by which
// reader method a check happens to call, because the declaration is what the skip
// decision is made from.
const (
	capOrders    = venue.CapabilityOrderLookup
	capFills     = venue.CapabilityFillRetrieval
	capBalances  = venue.CapabilityBalanceRetrieval
	capPositions = venue.CapabilityPositionRetrieval
)

// Severity is reconciliation.severity.
type Severity string

const (
	SevInfo     Severity = "INFO"
	SevLow      Severity = "LOW"
	SevMedium   Severity = "MEDIUM"
	SevHigh     Severity = "HIGH"
	SevMaterial Severity = "MATERIAL"
)

// severityOrder is the database's enum order, so escalation is a comparison rather
// than nine hand-written pairs.
var severityOrder = map[Severity]int{
	SevInfo: 0, SevLow: 1, SevMedium: 2, SevHigh: 3, SevMaterial: 4,
}

func (s Severity) known() bool { _, ok := severityOrder[s]; return ok }

// blocksRiskIncreasing reports whether an unresolved case at this severity stops
// risk-increasing activity.
//
// Only MATERIAL does. reconciliation.case's own comment says "A MATERIAL case blocks
// risk-increasing scope", and the partial index case_open_material_idx is defined
// `WHERE severity = 'MATERIAL'`. Promoting HIGH to blocking here would contradict the
// schema's own statement of which cases matter.
func (s Severity) blocksRiskIncreasing() bool { return s == SevMaterial }

// DifferenceKind is reconciliation.case.difference_kind.
type DifferenceKind string

const (
	KindOrderMissing  DifferenceKind = "ORDER_MISSING"
	KindOrderState    DifferenceKind = "ORDER_STATE"
	KindFillMissing   DifferenceKind = "FILL_MISSING"
	KindFillDuplicate DifferenceKind = "FILL_DUPLICATE"
	KindBalance       DifferenceKind = "BALANCE"
	KindPosition      DifferenceKind = "POSITION"
)

// Finding is one established difference, before it becomes a case.
type Finding struct {
	Kind     DifferenceKind
	Severity Severity

	// InternalReference identifies what differs, and is the identity a repeat
	// finding is matched on. Opening a case per pass would turn one difference into
	// one case per interval, so the count of open cases would measure how often
	// reconciliation ran rather than how much is actually wrong.
	InternalReference string
	ExternalReference string
	AccountID         string

	Expected   *contracts.Decimal
	Observed   *contracts.Decimal
	Difference *contracts.Decimal

	// Evidence is redacted: digests and references, never raw venue payloads.
	Evidence map[string]any
}

// CheckResult is one check's outcome, including what it did and did not establish.
type CheckResult struct {
	Name       string
	Capability Capability
	Status     Status

	// Reason is populated for SKipped and Failed, and empty for Completed.
	//
	// A skipped check with no recorded reason is indistinguishable from a bug, so
	// the reason is required for those two states rather than merely conventional.
	Reason string

	// Checked counts only what this check actually looked at, and is zero unless
	// Status is Completed. It is set by the check itself, which is the only place
	// that knows how many things it saw.
	Checked int

	Findings []Finding
}

// Completed reports whether the check ran. It is the only thing allowed to
// contribute to a count.
func (c CheckResult) Completed() bool { return c.Status == Completed }

// Run is what one reconciliation pass did.
type Run struct {
	CheckRunID  contracts.ID
	VenueID     string
	AdapterID   string
	Environment contracts.Environment

	// Status is the database's vocabulary: COMPLETED, FAILED or ABORTED. A run with
	// any Failed check is FAILED and never COMPLETED-with-lower-counts, because a
	// reader of reconciliation.check_run.status has no other way to learn that part
	// of the check set did not run.
	Status string

	OrdersChecked    int
	FillsChecked     int
	BalancesChecked  int
	DifferencesFound int
	MaterialBreaks   int

	Checks []CheckResult

	// CaseErrors records every finding whose case could not be written.
	//
	// It is returned rather than swallowed. An earlier draft set Status = FAILED and
	// moved on, which meant a difference could be established, reported as a count,
	// and then silently have no case behind it -- the exact state this package exists
	// to prevent, and invisible in every number on the run.
	CaseErrors []error

	// Cases holds every case this pass opened or escalated, in order.
	Cases []contracts.ID
}

// Option configures a Runner.
type Option func(*Runner)

// WithAccount names the account whose books this runner reconciles.
//
// It is required rather than discovered, and that requirement came from a test
// failure rather than from foresight. An earlier version read the account back with
// `SELECT account_id FROM oms."order" WHERE venue_id = $1 LIMIT 1`, which is
// deterministic only while a venue serves exactly one account. The moment it served
// two, which account reconciliation compared became a function of row order, and a
// balance check silently comparing the wrong books is indistinguishable from one
// that found no difference. Guessing is not available here: the alternative to
// naming the account is picking one arbitrarily.
func WithAccount(accountID string) Option {
	return func(r *Runner) { r.account = accountID }
}

// WithClock replaces the clock. Certification expiry and SLA deadlines are both
// comparisons against a point in time, so a test that cannot choose that point
// cannot test either.
func WithClock(now func() time.Time) Option {
	return func(r *Runner) {
		if now != nil {
			r.now = now
		}
	}
}

// WithOwner sets the accountable subject on every case opened.
//
// Doc 05 requires an owner and the schema enforces owner_subject_id NOT NULL. It is
// an option rather than a defaulted argument because a default would name a
// placeholder owner, and a case attributed to a placeholder is a case nobody is
// going to look at.
func WithOwner(owner string) Option {
	return func(r *Runner) { r.owner = owner }
}

// WithSLA sets the resolution deadline applied to newly opened cases.
//
// reconciliation.case enforces resolve_by > detected_at, so this must be positive.
// An unbounded MATERIAL break is exactly the silent risk the constraint exists to
// prevent.
func WithSLA(d time.Duration) Option {
	return func(r *Runner) {
		if d > 0 {
			r.sla = d
		}
	}
}

// WithPolicyVersion sets policy_version on every audit record.
//
// audit.record carries CHECK (length(policy_version) > 0). It is required rather
// than defaulted because a reconciliation audit record that cannot say which policy
// was in force cannot answer whether the difference it reports arose under a
// permitted configuration.
func WithPolicyVersion(v string) Option {
	return func(r *Runner) { r.policyVersion = v }
}

// WithSigningKey names the key that attests to this runner's audit records.
//
// It is required. audit.record.signing_key_id is NOT NULL, so an empty key stores
// without complaint, and nothing revises that column afterwards -- there is no
// UPDATE against it in any migration -- so the value written here is the value the
// record keeps for the life of the partition. A case is a claim that the platform
// and the venue disagree about money, and it is the record most likely to be read
// aloud later. It should not be the one that names nothing.
func WithSigningKey(v string) Option {
	return func(r *Runner) { r.signingKey = v }
}

// Runner performs reconciliation passes against one venue.
type Runner struct {
	db       *sql.DB
	gate     *venue.Gate
	reader   venue.Reader
	appender *audit.Appender

	adapterID string
	venueID   string
	account   string
	owner     string
	// producer names this runner as the actor on its audit records and as
	// ops.outbox.producer_id on the rows it writes. It is a fixed service
	// identity rather than an option: a reconciliation run is performed by the
	// service, not by a caller choosing who to appear as, so it is set once here
	// and not overridable. WithProducer used to expose this as an option and had
	// no call site, which is why it was removed.
	producer       string
	policyVersion  string
	signingKey     string
	sla            time.Duration
	maxOrderProbes int
	now            func() time.Time

	// lastRunID is the check run currently being written cases for. Every case
	// carries it as its correlation id, so a case and the counts that motivated it
	// are one evidence trail rather than two.
	lastRunID string
}

// NewRunner returns a Runner.
//
// gate is a venue.Gate rather than an adapter id, because the gate is the authority
// on what the adapter may be asked and this package must take its capabilities from
// the same source the rest of the control plane uses. Reading capabilities from the
// reader by duck-typing is what made the earlier draft skip every check forever.
func NewRunner(db *sql.DB, gate *venue.Gate, reader venue.Reader, appender *audit.Appender,
	adapterID, venueID string, opts ...Option) (*Runner, error) {

	if db == nil {
		return nil, errors.New("reconcile: a runner needs a database handle")
	}
	if gate == nil {
		return nil, errors.New("reconcile: a runner needs a certification gate; the capabilities " +
			"reconciliation may rely on are the certified ones, and nothing else states them")
	}
	if reader == nil {
		return nil, errors.New("reconcile: a runner needs a venue reader")
	}
	if appender == nil {
		return nil, errors.New("reconcile: a runner needs an audit appender; a difference this code " +
			"established and recorded nowhere is not evidence of anything")
	}
	if adapterID == "" {
		return nil, errors.New("reconcile: a runner needs an adapter id; certification is recorded per adapter")
	}
	if venueID == "" {
		return nil, errors.New("reconcile: a runner needs a venue id")
	}

	r := &Runner{
		db: db, gate: gate, reader: reader, appender: appender,
		adapterID: adapterID,
		venueID:   venueID,
		// Long enough that a difference noticed at the end of a shift is not already
		// overdue, short enough that a MATERIAL break cannot age out of relevance.
		sla:      4 * time.Hour,
		now:      func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
		owner:    "",
		producer: "svc-reconcile",
		// Set before the options run, so WithMaxOrderProbes overrides it and an
		// option that passes a non-positive value leaves the bound in place rather
		// than removing it.
		maxOrderProbes: defaultMaxOrderProbes,
	}
	for _, o := range opts {
		o(r)
	}
	if r.owner == "" {
		return nil, errors.New("reconcile: a runner needs an owner for the cases it opens; " +
			"reconciliation.case requires owner_subject_id, and a placeholder owner is a case " +
			"nobody is accountable for")
	}
	if r.policyVersion == "" {
		return nil, errors.New("reconcile: a runner needs a policy version; audit.record requires " +
			"one on every row and an empty value satisfies the column while naming nothing")
	}
	if r.signingKey == "" {
		// The same refusal domain/ledger and domain/execution make. Every case this
		// runner opens is a statement that the platform's books and the venue's
		// disagree, and it writes that statement to an audit record whose
		// signing_key_id is NOT NULL. An empty key stores without complaint and
		// leaves the loudest thing reconciliation produces unattributable: the one
		// record an operator will later be asked to defend.
		return nil, errors.New("reconcile: a runner needs a signing key id; audit.record." +
			"signing_key_id is NOT NULL, and a case recorded under an empty key names no key " +
			"that attests to the disagreement")
	}
	if r.account == "" {
		// Refused rather than read back from whatever order happens to be first. See
		// WithAccount: a runner that does not know whose books it is comparing cannot
		// be trusted to compare anybody's, and the failure would be silent.
		return nil, errors.New("reconcile: a runner needs the account whose books it reconciles; " +
			"reading it back from an arbitrary order would make the comparison a function of " +
			"row order for any venue serving more than one account")
	}
	return r, nil
}

// Account returns the account this runner reconciles.
func (r *Runner) Account() string { return r.account }

// Environment is the environment this runner reconciles. It is read from the gate
// rather than supplied, because the gate is bound to one environment and its
// certification set was loaded for exactly that one.
func (r *Runner) Environment() contracts.Environment { return r.gate.Environment() }

// permitted reports whether the adapter may be asked for cap.
//
// This is the only place a check decides whether it can run, and it consults the
// gate rather than the reader. A refusal is a SKIP with the reason recorded: the
// adapter has not certified the capability, so there is no evidence either way about
// the venue's data.
func (r *Runner) permitted(cap Capability) error {
	return r.gate.Permit(cap)
}

// Run performs one reconciliation pass.
//
// The check_run row is opened first and closed last, so a process that dies mid-pass
// leaves a RUNNING row rather than no evidence at all. check_run_running_idx exists
// to find exactly that.
func (r *Runner) Run(ctx context.Context) (*Run, error) {
	env := r.Environment()
	now := r.now()

	runID, err := contracts.NewID(contracts.EntityReconcilRun)
	if err != nil {
		return nil, fmt.Errorf("reconcile: minting a check run id failed: %w", err)
	}
	run := &Run{
		CheckRunID:  runID,
		VenueID:     r.venueID,
		AdapterID:   r.adapterID,
		Environment: env,
		Status:      "RUNNING",
	}

	ns := now.UnixNano()
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO reconciliation.check_run (
			check_run_id, environment, venue_id, started_at, started_at_ns, status)
		VALUES ($1, $2::common.environment, $3,
		        common.ns_to_timestamptz($4), $4, 'RUNNING')`,
		runID.String(), string(env), r.venueID, ns); err != nil {
		return nil, fmt.Errorf("reconcile: opening the check run failed: %w", err)
	}
	r.lastRunID = runID.String()

	// The checks are independent and each one owns its own outcome. They are run in
	// a fixed order rather than concurrently because each may open a case, and
	// serialising them keeps the case writes sequential and reviewable.
	checks := []CheckResult{
		r.checkOrders(ctx, env),
		r.checkFills(ctx, env),
		r.checkBalances(ctx, env),
		r.checkPositions(ctx, env),
	}
	run.Checks = checks

	for _, c := range checks {
		if !c.Completed() {
			// A skipped check is not a failure; a failed one makes the whole run
			// FAILED so that a reader of check_run.status cannot mistake a partial
			// pass for a clean one.
			if c.Status == Failed {
				run.Status = "FAILED"
			}
			continue
		}
		run.DifferencesFound += len(c.Findings)
		for _, f := range c.Findings {
			if f.Severity == SevMaterial {
				run.MaterialBreaks++
			}
		}
		switch c.Capability {
		case capOrders:
			run.OrdersChecked += c.Checked
		case capFills:
			run.FillsChecked += c.Checked
		case capBalances:
			run.BalancesChecked += c.Checked
		}
	}

	// Every finding becomes a case, in its own transaction, before the run closes.
	// A case that could fail to open would leave the run reporting a difference
	// that nothing tracks.
	for _, c := range checks {
		for _, f := range c.Findings {
			caseID, err := r.record(ctx, env, f, now)
			if err != nil {
				// The difference is real and the case could not be written. The run
				// is FAILED rather than COMPLETED, because a reported difference
				// with no case is the state this whole package exists to prevent.
				run.Status = "FAILED"
				run.CaseErrors = append(run.CaseErrors,
					fmt.Errorf("%s at %s: %w", f.Kind, f.InternalReference, err))
				continue
			}
			run.Cases = append(run.Cases, caseID)
		}
	}

	if run.Status == "RUNNING" {
		run.Status = "COMPLETED"
	}
	if err := r.close(ctx, run, now); err != nil {
		return run, err
	}
	return run, nil
}

// close writes the run's outcome and counts.
//
// check_run_material_recorded enforces material_breaks <= differences_found, and
// check_run_completion requires completed_at for every non-RUNNING status. Both are
// enforced by the schema, so the counts written here are the same ones Run reports.
func (r *Runner) close(ctx context.Context, run *Run, now time.Time) error {
	ns := now.UnixNano()
	var detail sql.NullString
	if run.Status == "FAILED" {
		failed := make([]string, 0, len(run.Checks))
		for _, c := range run.Checks {
			if c.Status == Failed {
				failed = append(failed, c.Name+": "+c.Reason)
			}
		}
		detail = sql.NullString{String: strings.Join(failed, "; "), Valid: true}
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE reconciliation.check_run
		   SET status = $2, completed_at = common.ns_to_timestamptz($3),
		       orders_checked = $4, fills_checked = $5, balances_checked = $6,
		       differences_found = $7, material_breaks = $8, error_detail = $9
		 WHERE check_run_id = $1`,
		run.CheckRunID.String(), run.Status, ns,
		run.OrdersChecked, run.FillsChecked, run.BalancesChecked,
		run.DifferencesFound, run.MaterialBreaks, detail)
	if err != nil {
		return fmt.Errorf("reconcile: closing the check run failed: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("reconcile: check run %s was not open; it cannot be closed", run.CheckRunID)
	}
	return nil
}

// maxOrderProbes bounds how many orders one pass will ask the venue about.
//
// The loop below issues one venue round trip per order in the probe set, and the
// probe set is every order this platform has not established the outcome of. An
// outage at the venue leaves orders in SUBMITTING, so the probe set grows exactly
// when the venue is least able to answer, and an unbounded loop then spends the
// pass -- and the venue's rate limit -- catching up on a backlog. The cost of a
// pass should be bounded by the pass, not by the size of the backlog.
//
// ORDER BY o.order_id makes the bound deterministic rather than arbitrary: given
// the same backlog, every pass reconciles the same prefix, so an order cannot be
// starved indefinitely by a changing set ahead of it. It is not a complete
// fairness guarantee -- an order that sorts late can still wait many passes -- but
// it is deterministic, and a random or unordered selection would not be.
//
// The bound is reported rather than silent. A pass that stops early says so in
// its reason and sets Skipped rather than Completed, because a truncated check
// that reports Completed is the same shape of defect as a partial read reported
// as a complete one.
const defaultMaxOrderProbes = 500

// WithMaxOrderProbes overrides how many orders one pass will ask the venue about.
// A non-positive value is ignored, so the bound can never be removed by
// accident; a bound of zero would make the check silently report nothing.
func WithMaxOrderProbes(n int) Option {
	return func(r *Runner) {
		if n > 0 {
			r.maxOrderProbes = n
		}
	}
}

// checkOrders asks the venue about every order whose outcome this platform has not
// established.
//
// The set is the one that matters: orders the platform believes are SUBMITTING or
// UNKNOWN. An order the venue and the platform both call ACKNOWLEDGED needs no
// reconciliation, and asking about it on every pass would turn the venue into a
// polling load proportional to all history.
func (r *Runner) checkOrders(ctx context.Context, env contracts.Environment) CheckResult {
	res := CheckResult{Name: "orders", Capability: capOrders}

	if err := r.permitted(capOrders); err != nil {
		res.Status = Skipped
		res.Reason = err.Error()
		return res
	}

	// One row more than the bound, so a page that comes back exactly bound+1 long
	// is proof that more remain rather than an inference from hitting the bound.
	// That distinction is the whole point: a set that ends exactly at the bound
	// would be indistinguishable from an overflowing one, and the pass would
	// either claim to be complete when it was not, or claim to be truncated when
	// it was not.
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT o.order_id, s.client_order_id, o.state::text
		  FROM oms."order" o
		  JOIN execution.submission s ON s.order_id = o.order_id
		 WHERE o.venue_id = $1
		   AND o.environment = $2::common.environment
		   AND o.state IN ('SUBMITTING','UNKNOWN')
		 ORDER BY o.order_id
		 LIMIT $3`, r.venueID, string(env), r.maxOrderProbes+1)
	if err != nil {
		res.Status = Failed
		res.Reason = fmt.Sprintf("reading the orders needing reconciliation failed: %v", err)
		return res
	}
	type probe struct{ orderID, clientOrderID, state string }
	var probes []probe
	for rows.Next() {
		var p probe
		if err := rows.Scan(&p.orderID, &p.clientOrderID, &p.state); err != nil {
			rows.Close()
			res.Status = Failed
			res.Reason = fmt.Sprintf("reading the orders needing reconciliation failed: %v", err)
			return res
		}
		probes = append(probes, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		res.Status = Failed
		res.Reason = fmt.Sprintf("reading the orders needing reconciliation failed: %v", err)
		return res
	}
	rows.Close()

	// A full page means there may be more, because the query asked for one more row
	// than the bound allows: a page shorter than that is the whole set, and a page
	// exactly that long is a set that continues past it.
	truncated := len(probes) > r.maxOrderProbes
	if truncated {
		probes = probes[:r.maxOrderProbes]
		res.Reason = fmt.Sprintf(
			"stopped after %d orders: at least one more order is awaiting reconciliation, so this pass did not examine the whole set. The remainder is left for the next pass rather than reported as examined.",
			r.maxOrderProbes)
		res.Status = Skipped
	}

	for _, p := range probes {
		vo, err := r.reader.LookupOrder(ctx, p.clientOrderID)
		if err != nil {
			// The venue could not answer for this order. That is a failure to
			// establish, so the whole check fails rather than counting what it read
			// before the error and reporting a partial result as complete.
			res.Status = Failed
			res.Reason = fmt.Sprintf("the venue could not be asked about order %s: %v", p.orderID, err)
			return res
		}
		res.Checked++

		if vo.VenueOrderRef == "" && vo.State == "" {
			res.Findings = append(res.Findings, Finding{
				Kind:              KindOrderMissing,
				Severity:          SevHigh,
				InternalReference: p.orderID,
				ExternalReference: p.clientOrderID,
				Evidence: map[string]any{
					"local_state":     p.state,
					"venue_reported":  "nothing",
					"client_order_id": p.clientOrderID,
				},
			})
			continue
		}

		// The venue's state is translated through the Registry that declared the
		// adapter's map. There is no package-level translation and no default: an
		// unmappable state is an UNKNOWN_OUTCOME finding rather than a guess, for
		// the same reason execution.Store never defaults an uninterpretable outcome
		// to REJECTED.
		mapped, err := r.gate.Registry().MapState(vo.State)
		switch {
		case err != nil:
			res.Findings = append(res.Findings, Finding{
				Kind:              KindOrderState,
				Severity:          SevMaterial,
				InternalReference: p.orderID,
				ExternalReference: vo.VenueOrderRef,
				Evidence: map[string]any{
					"local_state":   p.state,
					"venue_state":   vo.State,
					"mapping_error": err.Error(),
					"explanation":   "the adapter declares no mapping for this venue state, so the platform cannot say what happened to the order",
				},
			})
		case string(mapped) != p.state:
			res.Findings = append(res.Findings, Finding{
				Kind:              KindOrderState,
				Severity:          SevMedium,
				InternalReference: p.orderID,
				ExternalReference: vo.VenueOrderRef,
				Evidence: map[string]any{
					"local_state":  p.state,
					"venue_state":  vo.State,
					"mapped_state": string(mapped),
					"observed_at":  vo.ObservedAt.UTC().Format(time.RFC3339Nano),
				},
			})
		}
	}

	// Skipped, not Completed, when the probe set overflowed: the reason set above
	// explains what was left for the next pass. Every return path inside the loop
	// has already overwritten Status with Failed, which is correct -- a venue that
	// cannot be asked is a worse outcome than a bounded pass, and Failed is what
	// stops the caller reading these findings as a complete picture.
	if !truncated {
		res.Status = Completed
	}
	return res
}

// checkFills asks the venue for its executions and looks for the ones this platform
// never recorded, plus the ones it recorded twice.
func (r *Runner) checkFills(ctx context.Context, env contracts.Environment) CheckResult {
	res := CheckResult{Name: "fills", Capability: capFills}

	if err := r.permitted(capFills); err != nil {
		res.Status = Skipped
		res.Reason = err.Error()
		return res
	}

	since := r.now().Add(-24 * time.Hour)
	fills, err := r.reader.Fills(ctx, since)
	if err != nil {
		res.Status = Failed
		res.Reason = fmt.Sprintf("the venue's fills could not be retrieved: %v", err)
		return res
	}

	// A venue that reports one trade twice is reporting it twice. The platform
	// stores it once (fill_venue_trade_unique), so the duplicate is invisible in
	// oms.fill and only the delivery shows it.
	seen := map[string]int{}
	for _, f := range fills {
		seen[f.VenueFillRef]++
	}
	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	// One query for every reference rather than one per reference. The venue
	// reports all of these in one call, so a per-reference query made the cost
	// of a reconciliation scale with the number of executions the account had in
	// the window, which is the one thing a reconciliation's cost must not do.
	recorded := make(map[string]int, len(refs))
	if len(refs) > 0 {
		rows, err := r.db.QueryContext(ctx, `
			SELECT venue_trade_id, count(*)
			  FROM oms.fill
			 WHERE venue_id = $1 AND venue_trade_id = ANY($2)
			 GROUP BY venue_trade_id`,
			r.venueID, refs)
		if err != nil {
			res.Status = Failed
			res.Reason = fmt.Sprintf("reading local fills failed: %v", err)
			return res
		}
		for rows.Next() {
			var ref string
			var n int
			if err := rows.Scan(&ref, &n); err != nil {
				_ = rows.Close()
				res.Status = Failed
				res.Reason = fmt.Sprintf("reading local fills failed: %v", err)
				return res
			}
			recorded[ref] = n
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			res.Status = Failed
			res.Reason = fmt.Sprintf("reading local fills failed: %v", err)
			return res
		}
		_ = rows.Close()
	}

	for _, ref := range refs {
		res.Checked++
		if n := seen[ref]; n > 1 {
			res.Findings = append(res.Findings, Finding{
				Kind:              KindFillDuplicate,
				Severity:          SevHigh,
				InternalReference: ref,
				Evidence: map[string]any{
					"deliveries": n,
					"explanation": "the venue reported one venue_trade_id more than once; " +
						"fill_venue_trade_unique stores it once, so the local count cannot show this",
				},
			})
			continue
		}
		if recorded[ref] == 0 {
			res.Findings = append(res.Findings, Finding{
				Kind:              KindFillMissing,
				Severity:          SevMaterial,
				InternalReference: ref,
				Evidence: map[string]any{
					"explanation": "the venue reports an execution this platform has no oms.fill row " +
						"for; portfolio.position derives from validated fills, so a missing fill is a " +
						"position that is silently wrong",
				},
			})
		}
	}

	res.Status = Completed
	return res
}

// checkBalances compares the venue's balances with portfolio.balance.
//
// portfolio.balance is a PROJECTION, not a source of truth (its own comment says
// so), so a disagreement is a defect in the platform or the venue and reconciliation
// cannot tell which. That is why it is reported rather than corrected: picking a
// winner would be picking a financial truth from two disagreeing accounts.
func (r *Runner) checkBalances(ctx context.Context, env contracts.Environment) CheckResult {
	res := CheckResult{Name: "balances", Capability: capBalances}

	if err := r.permitted(capBalances); err != nil {
		res.Status = Skipped
		res.Reason = err.Error()
		return res
	}

	venueBalances, err := r.reader.Balances(ctx)
	if err != nil {
		res.Status = Failed
		res.Reason = fmt.Sprintf("the venue's balances could not be retrieved: %v", err)
		return res
	}

	// Read once rather than per balance: the account this venue's books belong to is
	// the same for every currency, and a lookup per row would both be wasteful and
	// open a window in which two balances were compared against two accounts.
	accountID := r.account

	// One query for every currency rather than one per currency, for the same
	// reason as the fills and positions checks above: the venue returns all of its
	// balances in a single call, so the query count must not scale with them.
	//
	// balance_identity_idx is UNIQUE over (account_id, environment, currency), so
	// one grouped read cannot collapse two rows into one currency and silently
	// convert a disagreement into agreement. Both figures are selected as ::text
	// because the comparison below is exact decimal arithmetic.
	type localBalance struct{ total, available sql.NullString }
	balances := make(map[string]localBalance, len(venueBalances))
	if len(venueBalances) > 0 {
		rows, err := r.db.QueryContext(ctx, `
			SELECT currency, total::text, available::text
			  FROM portfolio.balance
			 WHERE account_id = $1 AND environment = $2::common.environment
			   AND currency = ANY($3)`,
			accountID, string(env), venueCurrencies(venueBalances))
		if err != nil {
			res.Status = Failed
			res.Reason = fmt.Sprintf("reading local balances failed: %v", err)
			return res
		}
		for rows.Next() {
			var currency string
			var b localBalance
			if err := rows.Scan(&currency, &b.total, &b.available); err != nil {
				_ = rows.Close()
				res.Status = Failed
				res.Reason = fmt.Sprintf("reading local balances failed: %v", err)
				return res
			}
			balances[currency] = b
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			res.Status = Failed
			res.Reason = fmt.Sprintf("reading local balances failed: %v", err)
			return res
		}
		_ = rows.Close()
	}

	for _, vb := range venueBalances {
		res.Checked++
		local, ok := balances[vb.Currency]
		if !ok {
			res.Findings = append(res.Findings, Finding{
				Kind:              KindBalance,
				Severity:          SevMaterial,
				InternalReference: vb.Currency,
				AccountID:         accountID,
				Expected:          zeroDecimal(),
				Observed:          &vb.Total,
				Evidence: map[string]any{
					"explanation": "the venue holds a balance for which this platform holds no " +
						"portfolio.balance row at all",
				},
			})
			continue
		}
		localTotal, err := decimalFrom(local.total)
		if err != nil {
			res.Status = Failed
			res.Reason = fmt.Sprintf("the local total for %s could not be read exactly: %v",
				vb.Currency, err)
			return res
		}
		localAvailable, err := decimalFrom(local.available)
		if err != nil {
			res.Status = Failed
			res.Reason = fmt.Sprintf("the local available balance for %s could not be read "+
				"exactly: %v", vb.Currency, err)
			return res
		}

		// One case per currency, not per field.
		//
		// total and available are two figures about the same fact: what this
		// platform holds in a currency. A single missed fill usually breaks both at
		// once, and keying the finding on currency/field therefore turned one broken
		// USD balance into two rows in case_open_material_idx and two items on the
		// operator's list -- while fixing the actual cause closes both. The identity
		// of the difference is the currency; which fields disagree is evidence
		// attached to it, and it is preserved in full so nothing is hidden by the
		// grouping.
		type fieldDiff struct {
			field        string
			local, venue contracts.Decimal
			difference   contracts.Decimal
			severity     Severity
		}
		var diffs []fieldDiff
		for _, cmp := range []struct {
			field        string
			local, venue contracts.Decimal
		}{
			{"total", localTotal, vb.Total},
			{"available", localAvailable, vb.Available},
		} {
			if cmp.local.Equal(cmp.venue) {
				continue
			}
			diff, err := cmp.local.Sub(cmp.venue)
			if err != nil {
				// The two figures could not be subtracted at all. That is a failure
				// to establish the size of the difference, and it is reported as a
				// failed check rather than as a small difference, because the whole
				// purpose of grading is that a small difference and an
				// uncomputable one must not be confused.
				res.Status = Failed
				res.Reason = fmt.Sprintf("the local and venue %s for %s could not be "+
					"subtracted exactly: %v", cmp.field, vb.Currency, err)
				return res
			}
			diffs = append(diffs, fieldDiff{
				field:      cmp.field,
				local:      cmp.local,
				venue:      cmp.venue,
				difference: diff.Abs(),
				severity:   gradeAmount(cmp.local, cmp.venue),
			})
		}
		if len(diffs) == 0 {
			continue
		}

		// The case records the widest gap and the highest severity, so that a
		// break graded from one field is never understated because another field
		// happened to be closer.
		//
		// The severity comparison goes through severityOrder rather than through
		// the string. Severity is declared `type Severity string`, so `>` on two of
		// them orders them ALPHABETICALLY: HIGH < INFO < LOW < MATERIAL < MEDIUM.
		// Comparing the strings directly therefore lets a later MEDIUM field
		// overwrite an earlier MATERIAL one, which understates the break and, because
		// only MATERIAL blocks risk-increasing activity, silently unblocks trading
		// against an unknown balance. severityOrder exists to make that comparison
		// total and correct; case.go already uses it for the same decision.
		worst := 0
		highest := SevInfo
		for i, d := range diffs {
			if diffs[worst].difference.Cmp(d.difference) < 0 {
				worst = i
			}
			if severityOrder[d.severity] > severityOrder[highest] {
				highest = d.severity
			}
		}
		fields := make([]map[string]any, 0, len(diffs))
		for _, d := range diffs {
			fields = append(fields, map[string]any{
				"field":      d.field,
				"local":      d.local.String(),
				"venue":      d.venue.String(),
				"difference": d.difference.String(),
				"severity":   string(d.severity),
			})
		}
		res.Findings = append(res.Findings, Finding{
			Kind:              KindBalance,
			Severity:          highest,
			InternalReference: vb.Currency,
			AccountID:         accountID,
			Expected:          &diffs[worst].local,
			Observed:          &diffs[worst].venue,
			Difference:        &diffs[worst].difference,
			Evidence: map[string]any{
				"currency": vb.Currency,
				"fields":   fields,
				"explanation": "portfolio.balance is a projection derived from the ledger; a disagreement " +
					"with the venue is a defect on one side or the other and is reported, never corrected",
			},
		})
	}

	res.Status = Completed
	return res
}

// checkPositions compares the venue's positions with portfolio.position.
func (r *Runner) checkPositions(ctx context.Context, env contracts.Environment) CheckResult {
	res := CheckResult{Name: "positions", Capability: capPositions}

	if err := r.permitted(capPositions); err != nil {
		res.Status = Skipped
		res.Reason = err.Error()
		return res
	}

	venuePositions, err := r.reader.Positions(ctx)
	if err != nil {
		res.Status = Failed
		res.Reason = fmt.Sprintf("the venue's positions could not be retrieved: %v", err)
		return res
	}

	accountID := r.account

	// One query for the whole window rather than one per instrument. The venue
	// returns every position in a single call, so querying per instrument made
	// the cost of a reconciliation scale with the size of the book.
	//
	// position_identity_idx is UNIQUE over (account_id, environment,
	// instrument_id, venue_id), so a single grouped read cannot collapse two
	// distinct rows into one key and quietly turn a discrepancy into agreement.
	// The read returns quantity::text because the comparison below is exact
	// decimal arithmetic and must never pass through a float.
	local := make(map[string]sql.NullString, len(venuePositions))
	if len(venuePositions) > 0 {
		rows, err := r.db.QueryContext(ctx, `
			SELECT instrument_id, quantity::text
			  FROM portfolio.position
			 WHERE account_id = $1 AND environment = $2::common.environment
			   AND venue_id = $3 AND instrument_id = ANY($4)`,
			accountID, string(env), r.venueID, venueInstrumentIDs(venuePositions))
		if err != nil {
			res.Status = Failed
			res.Reason = fmt.Sprintf("reading local positions failed: %v", err)
			return res
		}
		for rows.Next() {
			var instrumentID string
			var quantity sql.NullString
			if err := rows.Scan(&instrumentID, &quantity); err != nil {
				_ = rows.Close()
				res.Status = Failed
				res.Reason = fmt.Sprintf("reading local positions failed: %v", err)
				return res
			}
			local[instrumentID] = quantity
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			res.Status = Failed
			res.Reason = fmt.Sprintf("reading local positions failed: %v", err)
			return res
		}
		_ = rows.Close()
	}

	for _, vp := range venuePositions {
		res.Checked++
		localText := local[vp.InstrumentID]
		if !localText.Valid {
			res.Findings = append(res.Findings, Finding{
				Kind:              KindPosition,
				Severity:          SevMaterial,
				InternalReference: vp.InstrumentID,
				AccountID:         accountID,
				Observed:          &vp.Quantity,
				Evidence: map[string]any{
					"explanation": "the venue holds a position for which this platform holds no " +
						"portfolio.position row; the platform would not know of exposure it has",
				},
			})
			continue
		}
		local, err := decimalFrom(localText)
		if err != nil {
			res.Status = Failed
			res.Reason = fmt.Sprintf("the local position for %s could not be read exactly: %v",
				vp.InstrumentID, err)
			return res
		}
		if local.Equal(vp.Quantity) {
			continue
		}
		diff, err := local.Sub(vp.Quantity)
		if err != nil {
			res.Status = Failed
			res.Reason = fmt.Sprintf("the local and venue positions for %s could not be "+
				"subtracted exactly: %v", vp.InstrumentID, err)
			return res
		}
		abs := diff.Abs()
		res.Findings = append(res.Findings, Finding{
			Kind:              KindPosition,
			Severity:          gradeAmount(local, vp.Quantity),
			InternalReference: vp.InstrumentID,
			AccountID:         accountID,
			Expected:          &local,
			Observed:          &vp.Quantity,
			Difference:        &abs,
			Evidence: map[string]any{
				"as_of": vp.AsOf.UTC().Format(time.RFC3339Nano),
				"explanation": "portfolio.position derives from validated fills and is a projection; " +
					"a disagreement is reported rather than corrected",
			},
		})
	}

	res.Status = Completed
	return res
}

// gradeAmount classifies a numeric disagreement.
//
// # THE INVERSION THIS EXISTS TO PREVENT
//
// An earlier draft graded by the size of the VENUE's figure. That reads correctly
// and is exactly backwards: it reported LOW whenever the venue said zero, which is
// the single most important thing a balance check can discover. A platform that
// believes it holds 1,000,000 and a venue that says 0 is a complete disagreement
// about whether any exposure exists at all, and grading it by the venue's zero
// called it the mildest possible finding.
//
// So severity is graded from the LOCAL figure -- what this platform believes it
// holds -- and the three cases are:
//
//	both zero            no difference; not a finding at all
//	local zero, venue not the platform has no record of exposure that exists. This
//	                     is MATERIAL: untracked exposure is the worst outcome, because
//	                     nothing in this platform would ever constrain it.
//	local non-zero       graded by the fraction of the local figure that disagrees,
//	                     and a total disagreement escalates to MATERIAL.
func gradeAmount(local, venue contracts.Decimal) Severity {
	if local.IsZero() {
		if venue.IsZero() {
			// Not reachable for a caller that only grades differences, but naming it
			// keeps the rule total rather than leaving an unstated default.
			return SevInfo
		}
		return SevMaterial
	}
	diff, err := local.Sub(venue)
	if err != nil {
		// The magnitude could not be computed, which is a failure to establish it.
		// Refusing to grade it LOW is the whole point of this function, so an
		// uncomputable ratio is the more serious answer rather than the least.
		return SevMaterial
	}
	abs := diff.Abs()
	ratio, err := abs.Div(local, 4, contracts.RoundHalfEven)
	if err != nil {
		return SevMaterial
	}
	switch {
	case abs.Equal(local):
		// Total disagreement: the venue says none of what the platform holds.
		return SevMaterial
	case ratio.Cmp(contracts.MustParseDecimal("0.5")) >= 0:
		return SevHigh
	default:
		return SevMedium
	}
}

// decimalFrom reads a NUMERIC column that was selected as text.
//
// pgx hands NUMERIC back through database/sql as a string, and contracts.Decimal has
// no sql.Scanner, so scanning one into the other fails at runtime with "unsupported
// Scan, storing driver.Value type string into type *contracts.Decimal". That failure
// arrived as a FAILED check with a finding count of zero, which is the worst shape a
// bug in a reader can have: the reason named a driver error rather than a missing
// balance, and the run reported FAILED rather than anything about money.
//
// The column is therefore selected as ::text and parsed here. This follows the
// decision already taken in this codebase for risk.policy's text[] columns, which
// are likewise read and converted at the read site rather than by teaching the core
// contract type about a driver. Parsing NUMERIC(38,18) text is exact -- that is the
// whole reason PostgreSQL does not send it as a float.
func decimalFrom(ns sql.NullString) (contracts.Decimal, error) {
	if !ns.Valid {
		// A NULL where a figure is required is not zero. Reading it as zero would
		// report agreement between a missing balance and an empty one.
		return contracts.Decimal{}, errors.New("the stored figure is NULL, which is not the " +
			"same as zero")
	}
	d, err := contracts.ParseDecimal(ns.String)
	if err != nil {
		return contracts.Decimal{}, fmt.Errorf("stored figure %q does not parse: %w", ns.String, err)
	}
	return d, nil
}

// venueCurrencies collects the currencies to read in one query.
//
// Duplicates are dropped so the array parameter is sized by distinct currency
// rather than by the number of rows the venue reported.
func venueCurrencies(balances []venue.VenueBalance) []string {
	seen := make(map[string]struct{}, len(balances))
	currencies := make([]string, 0, len(balances))
	for _, b := range balances {
		if _, dup := seen[b.Currency]; dup {
			continue
		}
		seen[b.Currency] = struct{}{}
		currencies = append(currencies, b.Currency)
	}
	return currencies
}

// venueInstrumentIDs collects the instruments to read in one query.
//
// Duplicates are dropped so the array parameter stays as small as the number of
// distinct instruments rather than the number of rows the venue reported. The
// caller still iterates the full slice; this only sizes the read.
func venueInstrumentIDs(positions []venue.VenuePosition) []string {
	seen := make(map[string]struct{}, len(positions))
	ids := make([]string, 0, len(positions))
	for _, p := range positions {
		if _, dup := seen[p.InstrumentID]; dup {
			continue
		}
		seen[p.InstrumentID] = struct{}{}
		ids = append(ids, p.InstrumentID)
	}
	return ids
}

// zeroDecimal is the platform's figure for "nothing held", as opposed to an absent
// row. The distinction matters in checkBalances: a missing portfolio.balance row is
// its own finding, and grading it against a zero would report agreement where the
// truth is that this platform holds no record of the account at all.
func zeroDecimal() *contracts.Decimal {
	d := contracts.MustParseDecimal("0")
	return &d
}
