package reconcile

// Recording a finding as a case.
//
// The obligation doc 05 states is that a difference creates a CASE, with explicit
// severity, and that an unresolved MATERIAL break blocks affected risk-increasing
// scope. A finding held in memory and reported to a log is not a case: it has no
// owner, no deadline, no evidence and nothing to resolve, so the next pass would
// rediscover it and the count of genuine problems would be indistinguishable from
// the count of passes.
//
// What this file deliberately does NOT do is open a new case per pass. One
// difference is one case. Repeat findings attach to the existing one and escalate
// it in place, because:
//
//   - case_open_material_idx is `WHERE status IN ('OPEN','INVESTIGATING','REOPENED')
//     AND severity = 'MATERIAL'`. Every duplicate case would land in that index and
//     the index that exists to find the blocking break would become a list of the
//     same break, repeated once per reconciliation interval.
//   - an operator's queue is a work list. Twenty rows for one balance disagreement
//     is twenty units of apparent work, and the twenty-first is not more urgent than
//     the first.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/services/audit"
)

// record opens a case for a finding, or escalates the existing one, and returns its
// id.
//
// The whole thing is one transaction: the case row, its audit record and its outbox
// row either all exist or none do. A case with no audit record is a difference
// nobody can later account for, and a case with no outbox row is one nothing
// downstream will hear about -- and doc 05 requires an outbox row for every committed
// state mutation.
func (r *Runner) record(ctx context.Context, env contracts.Environment,
	f Finding, now time.Time) (contracts.ID, error) {

	if !f.Severity.known() {
		return contracts.ID{}, fmt.Errorf("reconcile: severity %q is not one of INFO, LOW, MEDIUM, "+
			"HIGH or MATERIAL", f.Severity)
	}
	if f.InternalReference == "" {
		// The internal reference is the finding's identity. Without one the
		// de-duplication below has nothing to match on and every pass would open a
		// case, which is the failure mode this file exists to prevent.
		return contracts.ID{}, fmt.Errorf("reconcile: a finding of kind %s has no internal reference, "+
			"so it cannot be matched against the case it duplicates", f.Kind)
	}
	if f.Difference != nil && f.Difference.Sign() < 0 {
		// reconciliation.case_difference_non_negative refuses a negative value, and
		// the column exists because a negative "difference" is a sign error rather
		// than a small difference.
		return contracts.ID{}, fmt.Errorf("reconcile: a finding of kind %s carries a negative "+
			"difference of %s", f.Kind, f.Difference)
	}

	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return contracts.ID{}, fmt.Errorf("reconcile: begin the case transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	evidence, err := json.Marshal(f.Evidence)
	if err != nil {
		return contracts.ID{}, fmt.Errorf("reconcile: encoding the case evidence failed: %w", err)
	}
	blocked, err := json.Marshal(r.blockedScope(env, f))
	if err != nil {
		return contracts.ID{}, fmt.Errorf("reconcile: encoding the blocked scope failed: %w", err)
	}

	// An existing unresolved case for the same difference. FOR UPDATE, because two
	// passes running concurrently would both find nothing here and both insert.
	var (
		existingID     string
		existingSev    string
		existingStatus string
	)
	row := tx.QueryRowContext(ctx, `
		SELECT case_id, severity::text, status::text
		  FROM reconciliation.case
		 WHERE environment = $1::common.environment
		   AND venue_id = $2
		   AND difference_kind = $3
		   AND internal_reference = $4
		   AND status IN ('OPEN','INVESTIGATING','REOPENED')
		 ORDER BY detected_at
		 LIMIT 1
		 FOR UPDATE`, string(env), r.venueID, string(f.Kind), f.InternalReference)

	scanErr := row.Scan(&existingID, &existingSev, &existingStatus)
	found := scanErr == nil
	if scanErr != nil && !isNoRows(scanErr) {
		return contracts.ID{}, fmt.Errorf("reconcile: looking for an existing case failed: %w", scanErr)
	}

	// The check run is the correlation. Every case this pass opens points at the run
	// that found it, so the cases and the counts that motivated them are one trail
	// rather than two.
	correlationID := r.lastRunID
	nowNs := now.UnixNano()

	var (
		caseID    contracts.ID
		escalated bool
		eventType = "RECONCILIATION_CASE_OPENED"
	)

	if found {
		parsed, err := contracts.ParseID(existingID)
		if err != nil {
			return contracts.ID{}, fmt.Errorf("reconcile: stored case id %s does not parse: %w", existingID, err)
		}
		caseID = parsed

		existingOrder, ok := severityOrder[Severity(existingSev)]
		if !ok {
			// A stored severity this code cannot interpret. Refusing is the only safe
			// reading: an unrecognised severity might be one that blocks, and
			// treating it as the lowest would silently unblock a break.
			return contracts.ID{}, fmt.Errorf("reconcile: case %s carries severity %q, which is not in "+
				"the severity vocabulary", existingID, existingSev)
		}
		if severityOrder[f.Severity] > existingOrder {
			escalated = true
			eventType = "RECONCILIATION_CASE_ESCALATED"
			if _, err := tx.ExecContext(ctx, `
				UPDATE reconciliation.case
				   SET severity = $2::reconciliation.severity,
				       blocked_scope = $3::jsonb,
				       evidence = $4::jsonb,
				       observed_value = $5,
				       difference_value = $6,
				       escalation_level = escalation_level + 1,
				       version_vector = version_vector + 1
				 WHERE case_id = $1`,
				caseID.String(), string(f.Severity), string(blocked), string(evidence),
				decArg(f.Observed), decArg(f.Difference)); err != nil {
				return contracts.ID{}, fmt.Errorf("reconcile: escalating case %s failed: %w", caseID, err)
			}
		} else if _, err := tx.ExecContext(ctx, `
			UPDATE reconciliation.case
			   SET evidence = $2::jsonb,
			       observed_value = $3,
			       difference_value = $4,
			       version_vector = version_vector + 1
			 WHERE case_id = $1`,
			caseID.String(), string(evidence), decArg(f.Observed), decArg(f.Difference)); err != nil {
			return contracts.ID{}, fmt.Errorf("reconcile: refreshing case %s evidence failed: %w", caseID, err)
		}
	} else {
		minted, err := contracts.NewID(contracts.EntityReconcilCase)
		if err != nil {
			return contracts.ID{}, fmt.Errorf("reconcile: minting a case id failed: %w", err)
		}
		caseID = minted
		// resolve_by > detected_at is enforced by case_has_sla. An unbounded case
		// cannot be aged, and an unbounded MATERIAL break is the silent risk the
		// constraint exists to prevent.
		resolveBy := now.Add(r.sla)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO reconciliation.case (
				case_id, environment, venue_id, account_id, difference_kind, severity,
				status, blocked_scope, internal_reference, external_reference,
				evidence, expected_value, observed_value, difference_value,
				owner_subject_id, detected_at, resolve_by, correlation_id)
			VALUES ($1, $2::common.environment, $3, $4, $5, $6::reconciliation.severity,
			        'OPEN', $7::jsonb, $8, $9,
			        $10::jsonb, $11, $12, $13,
			        $14, common.ns_to_timestamptz($15), common.ns_to_timestamptz($16), $17)`,
			caseID.String(), string(env), r.venueID, nullIfEmpty(f.AccountID),
			string(f.Kind), string(f.Severity), string(blocked),
			f.InternalReference, nullIfEmpty(f.ExternalReference), string(evidence),
			decArg(f.Expected), decArg(f.Observed), decArg(f.Difference),
			r.owner, nowNs, resolveBy.UnixNano(), correlationID); err != nil {
			return contracts.ID{}, fmt.Errorf("reconcile: opening a case failed: %w", err)
		}
	}

	// The audit record. Doc 22 requires policy_version on every audit record and doc
	// 21 forbids a service identity impersonating a human, so ActorType is WORKLOAD
	// and the actor is the runner's own identity.
	auditID, err := contracts.NewID(contracts.EntityAudit)
	if err != nil {
		return contracts.ID{}, fmt.Errorf("reconcile: minting an audit id failed: %w", err)
	}
	reason := string(f.Kind) + " at " + f.InternalReference
	details, err := json.Marshal(map[string]any{
		"case_id":                caseID.String(),
		"check_run_id":           correlationID,
		"difference_kind":        string(f.Kind),
		"severity":               string(f.Severity),
		"internal_reference":     f.InternalReference,
		"external_reference":     f.ExternalReference,
		"escalated":              escalated,
		"blocks_risk_increasing": f.Severity.blocksRiskIncreasing(),
	})
	if err != nil {
		return contracts.ID{}, fmt.Errorf("reconcile: encoding audit details failed: %w", err)
	}
	if _, err := r.appender.AppendIn(ctx, tx, audit.Record{
		AuditID:            auditID.String(),
		TenantOrOwnerScope: "trading",
		ActorID:            r.producer,
		ActorType:          contracts.ActorWorkload,
		Action:             eventType,
		TargetType:         "RECONCILIATION_CASE",
		TargetID:           caseID.String(),
		Environment:        env,
		OccurredAt:         now,
		RecordedAt:         now,
		Reason:             &reason,
		CorrelationID:      correlationID,
		PolicyVersion:      r.policyVersion,
		Result:             contracts.AuditResultSuccess,
		Details:            string(details),
		SigningKeyID:       r.signingKey,
	}); err != nil {
		return contracts.ID{}, fmt.Errorf("reconcile: the case was not audited, so the transaction "+
			"is abandoned: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE reconciliation.case SET audit_id = $2 WHERE case_id = $1`,
		caseID.String(), auditID.String()); err != nil {
		return contracts.ID{}, fmt.Errorf("reconcile: binding the audit record to the case failed: %w", err)
	}

	// The outbox row. A MATERIAL break that blocks risk-increasing scope is
	// something other parts of the platform must be able to react to, and doc 05
	// requires the row for every committed mutation regardless.
	if err := r.enqueue(ctx, tx, eventType, caseID, f, correlationID, env, now); err != nil {
		return contracts.ID{}, err
	}

	if err := tx.Commit(); err != nil {
		return contracts.ID{}, fmt.Errorf("reconcile: the case did not commit: %w", err)
	}
	committed = true
	return caseID, nil
}

// blockedScope describes what an unresolved case at this severity stops.
//
// Only a MATERIAL case blocks. This is recorded in the row itself rather than left
// to be re-derived by whatever reads the case, because "is this blocking?" is
// precisely the question an answering system should not have to re-decide.
func (r *Runner) blockedScope(env contracts.Environment, f Finding) map[string]any {
	scope := map[string]any{
		"blocks_risk_increasing": f.Severity.blocksRiskIncreasing(),
		"environment":            string(env),
		"venue_id":               r.venueID,
		"account_id":             f.AccountID,
	}
	return scope
}

// enqueue writes the outbox row for a case, in the caller's transaction.
func (r *Runner) enqueue(ctx context.Context, tx *sql.Tx, eventType string,
	caseID contracts.ID, f Finding, correlationID string, env contracts.Environment,
	now time.Time) error {

	evtID, err := contracts.NewID(contracts.EntityEvent)
	if err != nil {
		return fmt.Errorf("reconcile: minting an outbox event id failed: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"case_id":                caseID.String(),
		"difference_kind":        string(f.Kind),
		"severity":               string(f.Severity),
		"internal_reference":     f.InternalReference,
		"blocks_risk_increasing": f.Severity.blocksRiskIncreasing(),
	})
	if err != nil {
		return fmt.Errorf("reconcile: encoding the outbox payload failed: %w", err)
	}
	ns := now.UnixNano()
	// The sequence advances per aggregate.
	//
	// ops.outbox carries UNIQUE (aggregate_type, aggregate_id, sequence), and a
	// repeated finding attaches to the case it already has rather than opening a new
	// one -- so a case legitimately has several outbox rows: one when it opened, one
	// when it escalated, and one whenever its evidence is refreshed. Writing
	// sequence = 1 every time collided with outbox_aggregate_sequence_unique on the
	// second pass, which meant the evidence refresh silently never committed.
	//
	// Reading max(sequence)+1 inside the transaction is the same shape
	// domain/execution uses for an order's own event sequence, and it keeps a case's
	// outbox stream ordered in step with what happened to that case.
	var seq int64
	if err := tx.QueryRowContext(ctx, `
		SELECT coalesce(max(sequence), 0) + 1
		  FROM ops.outbox
		 WHERE aggregate_type = 'RECONCILIATION_CASE' AND aggregate_id = $1`,
		caseID.String()).Scan(&seq); err != nil {
		return fmt.Errorf("reconcile: reading the case's outbox sequence failed: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ops.outbox (
			event_id, event_type, schema_version, aggregate_type, aggregate_id,
			sequence, correlation_id, causation_id, producer_id, environment,
			occurred_at, occurred_at_ns, recorded_at, payload, dispatch_state,
			attempt_count, max_attempts)
		VALUES ($1,$2,'1.0.0','RECONCILIATION_CASE',$3,
		        $4,$5,$6,$7,$8::common.environment,
		        common.ns_to_timestamptz($9),$9,common.ns_to_timestamptz($9),$10::jsonb,
		        'PENDING',0,5)`,
		evtID.String(), eventType, caseID.String(), seq,
		correlationID, caseID.String(),
		r.producer, string(env), ns, string(payload)); err != nil {
		return fmt.Errorf("reconcile: inserting the outbox row failed; a committed case must "+
			"always have one (doc 05): %w", err)
	}
	return nil
}

// decArg converts a decimal for a nullable numeric column.
func decArg(d *contracts.Decimal) any {
	if d == nil {
		return nil
	}
	return *d
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isNoRows reports whether the lookup found nothing.
//
// errors.Is rather than == : the driver may wrap sql.ErrNoRows, and a comparison
// that silently stopped matching would turn "no existing case" into a hard error on
// every finding, which reads as a broken runner rather than as a bug in this helper.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
