// Package audit is the write path for audit evidence.
//
// Authority: 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3.
//
// The database is the authority for record_hash. This package does not decide
// what a record's hash is and does not get to choose one: it computes the same
// hash with an independent implementation of the canonical form
// (contracts.CanonicalAuditPayload) and supplies it to audit.append_record as a
// cross-check. The database recomputes, compares, and rejects any disagreement.
//
// Why cross-check at all, if the database already computes the hash?
//
// Because the canonical form is specified once and implemented twice. A single
// implementation can be internally consistent and still wrong: reordered
// members, a NULL written as "", a timestamp in milliseconds, an encoder that
// escapes differently. dbtest/audit_canonical_test.go pins the two
// implementations against each other, and this service makes the comparison
// happen on every real append rather than only in tests. A divergence that
// appears after this code ships is caught by an append failing, not by an
// incident report.
package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aitc/trade/contracts"
)

// ErrHashMismatch is returned when the database's recomputed hash disagrees
// with the one this package supplied.
//
// It is a specification or implementation divergence, not a transient fault.
// The correct response is to stop and investigate, not to retry: retrying cannot
// make two implementations of a fixed form agree, and swallowing the error would
// let the database silently become the only implementation.
var ErrHashMismatch = errors.New("audit: record hash mismatch between the Go and database implementations")

// ErrChainMoved is returned when another writer advanced the partition between
// reading the chain head and appending, invalidating the previous_hash this
// package computed its hash from.
//
// This one IS transient, and Append retries it a bounded number of times.
var ErrChainMoved = errors.New("audit: partition chain advanced concurrently")

// maxAppendAttempts bounds the retry for ErrChainMoved.
//
// Three is enough because the window is a single round trip and contention on
// one partition is expected to be bursty rather than continuous. An unbounded
// retry would turn a stuck partition into an unbounded wait inside a database
// transaction.
const maxAppendAttempts = 3

// Record is the input to Append. It mirrors the columns of audit.record, minus
// the values the database owns: record_hash, previous_hash and the sequence
// relationship, which append_record re-derives under its advisory lock.
//
// PartitionKey is deliberately absent. The partition is the month of RecordedAt,
// and deriving it here rather than accepting it from the caller removes a way
// to be wrong: a partition key inconsistent with the record's timestamp would
// place the record in a chain whose ordering has nothing to do with when the
// event happened.
type Record struct {
	AuditID            string
	TenantOrOwnerScope string
	ActorID            string
	ActorType          contracts.ActorType
	Action             string
	TargetType         string
	TargetID           string
	Environment        contracts.Environment
	MarketScope        *contracts.MarketClass
	OccurredAt         time.Time
	RecordedAt         time.Time
	Reason             *string
	CorrelationID      string
	CausationID        *string
	PolicyVersion      string
	Result             contracts.AuditResult
	BeforeDigest       *string
	AfterDigest        *string
	Details            string
	SigningKeyID       string
}

// Appender writes audit records.
type Appender struct {
	db *sql.DB
}

// NewAppender returns an Appender writing through db.
//
// The Appender holds no transaction state. Each Append opens its own, so a
// caller that fails partway through does not leave a partition lock held.
func NewAppender(db *sql.DB) *Appender { return &Appender{db: db} }

// Append writes one audit record and returns the stored record hash.
//
// Sequence is not supplied. The appender reads the partition head to learn the
// next sequence and the previous hash, because both are needed to build the
// canonical payload. A caller that predicted them would be predicting a value
// the database owns.
func (a *Appender) Append(ctx context.Context, rec Record) (string, error) {
	var lastErr error
	for attempt := 0; attempt < maxAppendAttempts; attempt++ {
		hash, err := a.attempt(ctx, rec)
		switch {
		case err == nil:
			return hash, nil
		case errors.Is(err, ErrChainMoved):
			lastErr = err
			// Another writer won the race. Re-read the head and rebuild.
			continue
		default:
			return "", err
		}
	}
	return "", fmt.Errorf("audit: append %s gave up after %d attempts: %w",
		rec.AuditID, maxAppendAttempts, lastErr)
}

// attempt performs one read-head-then-append cycle in its own transaction.
func (a *Appender) attempt(ctx context.Context, rec Record) (string, error) {
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("audit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stored, err := a.appendIn(ctx, tx, rec)
	if err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("audit: commit: %w", err)
	}
	return stored, nil
}

// AppendIn writes one audit record inside a transaction the caller owns, and
// returns the stored record hash without committing.
//
// Doc 05 requires domain validation, state mutation, audit record creation and
// outbox insertion to execute in one transaction. Append could not serve that
// requirement, because it opens its own: a submit command that called it would
// commit the audit record for a mutation that then failed to commit, producing
// evidence of an event that never durably happened. The chain is an append-only
// ledger, so such a record cannot be withdrawn afterwards -- the audit trail
// would assert something false and hold that assertion permanently.
//
// AppendIn exists for exactly that composition. It does not commit and does not
// retry. Both belong to the caller's transaction: retrying here would mean
// rolling the caller's work back, which this package has no authority to do.
//
// The partition head is read FOR UPDATE, so a caller running concurrent commands
// takes the same lock Append does and the chain advances one record at a time.
func (a *Appender) AppendIn(ctx context.Context, tx *sql.Tx, rec Record) (string, error) {
	if tx == nil {
		return "", errors.New("audit: AppendIn needs a transaction the caller owns")
	}
	return a.appendIn(ctx, tx, rec)
}

// appendIn performs one read-head-then-append cycle against tx.
//
// It neither commits nor rolls back. Both belong to whoever opened tx, so that a
// caller composing an audit record with other writes gets all of them or none.
func (a *Appender) appendIn(ctx context.Context, tx *sql.Tx, rec Record) (string, error) {

	// Provision the partition before reading its head. append_record also calls
	// ensure_partition, but it does so only when the append reaches it, and the
	// head has to be read first to build the canonical payload. On a month with
	// no prior evidence there is no partition_month row to read, so the service
	// creates it here rather than failing with "no rows in result set".
	var ensured string
	if err := tx.QueryRowContext(ctx,
		`SELECT audit.ensure_partition(date_trunc('month', $1::timestamptz)::date)`,
		rec.RecordedAt).Scan(&ensured); err != nil {
		return "", fmt.Errorf("audit: ensure partition: %w", err)
	}

	// Read the chain head to derive the sequence and the previous_hash that the
	// canonical payload must commit to.
	var lastSequence int64
	var lastHash sql.NullString
	err := tx.QueryRowContext(ctx, `
        SELECT last_sequence, last_hash
          FROM audit.partition_month
         WHERE partition_key = $1
         FOR UPDATE`, ensured).Scan(&lastSequence, &lastHash)
	if err != nil {
		return "", fmt.Errorf("audit: read partition head: %w", err)
	}

	previousHash := contracts.AuditGenesisHash
	if lastHash.Valid && lastHash.String != "" {
		previousHash = lastHash.String
	}

	// The details digest is a database function, not something reimplemented
	// here. PostgreSQL's jsonb text rendering is a PostgreSQL contract; a Go
	// reimplementation would be a second definition that drifts silently. This
	// narrows the cross-check to the canonical FORM, which is where the
	// specification risk actually is.
	var detailsDigest string
	if err := tx.QueryRowContext(ctx,
		`SELECT audit.details_digest($1::jsonb)`, rec.Details).Scan(&detailsDigest); err != nil {
		return "", fmt.Errorf("audit: details digest: %w", err)
	}

	payload := contracts.AuditRecord{
		AuditID:                rec.AuditID,
		PartitionKey:           ensured,
		Sequence:               lastSequence + 1,
		TenantOrOwnerScope:     rec.TenantOrOwnerScope,
		ActorID:                rec.ActorID,
		ActorType:              rec.ActorType,
		Action:                 rec.Action,
		TargetType:             rec.TargetType,
		TargetID:               rec.TargetID,
		Environment:            rec.Environment,
		MarketScope:            rec.MarketScope,
		OccurredAtUnixNano:     rec.OccurredAt.UTC().UnixNano(),
		RecordedAtUnixNano:     rec.RecordedAt.UTC().UnixNano(),
		Reason:                 rec.Reason,
		CorrelationID:          rec.CorrelationID,
		CausationID:            rec.CausationID,
		PolicyVersion:          rec.PolicyVersion,
		Result:                 rec.Result,
		BeforeDigest:           rec.BeforeDigest,
		AfterDigest:            rec.AfterDigest,
		DetailsDigest:          detailsDigest,
		PreviousHash:           previousHash,
		CanonicalSchemaVersion: contracts.AuditCanonicalSchemaVersion,
	}

	supplied, err := contracts.AuditRecordHash(payload)
	if err != nil {
		return "", fmt.Errorf("audit: compute record hash: %w", err)
	}

	// append_record returns VOID, so the write and the read-back are two
	// statements in the same transaction. They cannot be one statement with
	// RETURNING.
	if _, err := tx.ExecContext(ctx, `
        SELECT audit.append_record($1::text, $2::text, $3::bigint, $4::text, $5::text,
            $6::audit.actor_type_v, $7::text, $8::text, $9::text,
            $10::common.environment, $11::common.market_class,
            $12::timestamptz, $13::bigint, $14::timestamptz, $15::bigint,
            $16::text, $17::text, $18::text, $19::text,
            $20::audit.result_value, $21::text, $22::text, $23::jsonb,
            $24::text, $25::text, $26::text)`,
		payload.AuditID, payload.PartitionKey, payload.Sequence, payload.TenantOrOwnerScope,
		payload.ActorID, string(payload.ActorType), payload.Action, payload.TargetType,
		payload.TargetID, string(payload.Environment), marketScopeArg(payload.MarketScope),
		rec.OccurredAt, payload.OccurredAtUnixNano, rec.RecordedAt, payload.RecordedAtUnixNano,
		payload.Reason, payload.CorrelationID, payload.CausationID, payload.PolicyVersion,
		string(payload.Result), payload.BeforeDigest, payload.AfterDigest, rec.Details,
		rec.SigningKeyID, payload.CanonicalSchemaVersion, supplied); err != nil {
		// The database's REJECTION is more informative than any error the driver
		// wraps around it, so the message is inspected before being wrapped. Both
		// branches below are normal operation, not exceptional internal states.
		switch {
		case strings.Contains(err.Error(), "record hash mismatch"):
			return "", fmt.Errorf("%w for %s", ErrHashMismatch, rec.AuditID)
		case strings.Contains(err.Error(), "chain break"):
			return "", fmt.Errorf("%w: %v", ErrChainMoved, err)
		default:
			return "", fmt.Errorf("audit: append %s: %w", rec.AuditID, err)
		}
	}

	var stored string
	if err := tx.QueryRowContext(ctx,
		`SELECT record_hash FROM audit.record WHERE audit_id = $1`,
		payload.AuditID).Scan(&stored); err != nil {
		return "", fmt.Errorf("audit: read back record %s: %w", payload.AuditID, err)
	}

	if stored != supplied {
		// append_record accepted the write, so the stored hash should equal what
		// was supplied. It cannot, because the database compares them, but the
		// check stays: it is the assertion that the control is wired up, and it
		// costs one comparison.
		return "", fmt.Errorf("%w: stored %s, supplied %s", ErrHashMismatch, stored, supplied)
	}

	return stored, nil
}

// marketScopeArg renders a nullable market class for a query argument.
func marketScopeArg(m *contracts.MarketClass) any {
	if m == nil {
		return nil
	}
	return string(*m)
}
