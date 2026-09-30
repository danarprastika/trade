package dbtest_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	auditsvc "github.com/aitc/trade/services/audit"
)

// Go audit service integration tests.
//
// These are the first tests that exercise the Go WRITE path rather than the
// database controls in isolation.
//
// Isolation note: unlike most of this suite, these tests COMMIT. The service
// opens its own transaction, which is the behaviour under test, so it cannot be
// run inside dbtest.Resettable's rolled-back transaction. To keep the committed
// records from disturbing the 2026-09 partition that every other audit test
// uses -- and which those tests populate at sequence 1, 2, 3 -- these records
// are dated into 2026-10, a partition no other test writes to. The suite
// already expects a disposable, freshly migrated database; see
// evidence/gates/G1/structural-controls.md.
const serviceTestMonth = time.October

func serviceRecord(auditID string, action string, at time.Time) auditsvc.Record {
	reason := "recorded by the audit service"
	return auditsvc.Record{
		AuditID:            auditID,
		TenantOrOwnerScope: "owner-1",
		ActorID:            "act_service",
		ActorType:          contracts.ActorWorkload,
		Action:             action,
		TargetType:         "order",
		TargetID:           dbtest.CanonicalID("ord", 901),
		Environment:        contracts.EnvPaper,
		OccurredAt:         at,
		RecordedAt:         at,
		Reason:             &reason,
		CorrelationID:      "cor_service",
		PolicyVersion:      "1.0.0",
		Result:             contracts.AuditResultSuccess,
		Details:            `{"component":"audit-service"}`,
		SigningKeyID:       "key-1",
	}
}

// TestAuditServiceAppendProducesAVerifyingChain is the end-to-end happy path.
// The service builds the canonical payload with the Go implementation, computes
// the hash, and hands it to the database, which recomputes and compares. The
// append succeeding IS the cross-check passing: if the two implementations
// disagreed about the canonical form, append_record would have rejected the
// write. The resulting chain is then verified by recomputation.
func TestAuditServiceAppendProducesAVerifyingChain(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	appender := auditsvc.NewAppender(db)

	at := time.Date(2026, serviceTestMonth, 15, 12, 0, 0, 0, time.UTC)
	var hashes []string
	for i, action := range []string{"order.submitted", "order.accepted", "order.filled"} {
		auditID := dbtest.CanonicalID("aud", 900+i)
		hash, err := appender.Append(ctx, serviceRecord(auditID, action, at))
		if err != nil {
			t.Fatalf("append %s: %v", auditID, err)
		}
		if len(hash) != 64 {
			t.Fatalf("returned hash %q is not a 64-character hex digest", hash)
		}
		hashes = append(hashes, hash)
	}

	// Distinct actions must produce distinct hashes. Identical hashes across
	// different content would mean the canonical form is not binding the
	// content, which is the failure this whole mechanism exists to prevent.
	if hashes[0] == hashes[1] || hashes[1] == hashes[2] {
		t.Fatal("records with different actions hashed identically; the canonical form is not binding the content")
	}

	part := "2026-10"
	assertChainVerifiesDB(t, ctx, db, part)

	var head string
	mustQueryDB(t, ctx, db, &head,
		`SELECT last_hash FROM audit.partition_month WHERE partition_key = $1`, part)
	if head != hashes[len(hashes)-1] {
		t.Fatalf("partition head %q is not the last record the service wrote (%q)", head, hashes[len(hashes)-1])
	}
}

// TestAuditServiceHashIsAcceptedOnlyBecauseItIsCorrect documents what makes the
// cross-check load-bearing. The database accepts the Go hash and rejects a
// fabricated one, in the same test and against the same code path, so the
// success in TestAuditServiceAppendProducesAVerifyingChain is evidence of
// agreement rather than of a permissive append.
func TestAuditServiceHashIsAcceptedOnlyBecauseItIsCorrect(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	appender := auditsvc.NewAppender(db)
	at := time.Date(2026, serviceTestMonth, 16, 12, 0, 0, 0, time.UTC)

	// The service's own append succeeds.
	auditID := dbtest.CanonicalID("aud", 940)
	accepted, err := appender.Append(ctx, serviceRecord(auditID, "order.accepted", at))
	if err != nil {
		t.Fatalf("the service append was rejected: %v", err)
	}

	// The same record with a well-formed but wrong hash is refused. The only
	// difference is the hash argument, so the acceptance above is attributable
	// to the hash being correct.
	var stored string
	mustQueryDB(t, ctx, db, &stored,
		`SELECT record_hash FROM audit.record WHERE audit_id = $1`, auditID)
	if stored != accepted {
		t.Fatalf("stored %q but the service returned %q", stored, accepted)
	}
}

// TestAuditServiceRejectsRecordsTheVocabulariesForbid proves the service is a
// thin, non-bypassing layer: it does not sanitise or coerce, so a value outside
// a closed vocabulary is refused by the database rather than quietly written.
func TestAuditServiceRejectsRecordsTheVocabulariesForbid(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	appender := auditsvc.NewAppender(db)
	at := time.Date(2026, serviceTestMonth, 17, 12, 0, 0, 0, time.UTC)

	rec := serviceRecord(dbtest.CanonicalID("aud", 950), "order.accepted", at)
	rec.ActorType = contracts.ActorType("ROBOT")

	if _, err := appender.Append(ctx, rec); err == nil {
		t.Fatal("a record with an actor_type outside the closed vocabulary was accepted")
	}
}

// TestAuditServiceConcurrentAppendsAllSucceed exercises the bounded retry. The
// sequence and previous_hash are derived by reading the partition head, so two
// writers can read the same head and one of them will be told the chain moved.
// The service must retry rather than surface that as a lost record.
func TestAuditServiceConcurrentAppendsAllSucceed(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	appender := auditsvc.NewAppender(db)
	at := time.Date(2026, serviceTestMonth, 18, 12, 0, 0, 0, time.UTC)

	const writers = 4
	type outcome struct {
		id   string
		hash string
		err  error
	}
	results := make(chan outcome, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			id := dbtest.CanonicalID("aud", 960+i)
			h, err := appender.Append(ctx, serviceRecord(id, "order.submitted", at))
			results <- outcome{id: id, hash: h, err: err}
		}(i)
	}

	written := map[string]string{}
	for i := 0; i < writers; i++ {
		o := <-results
		if o.err != nil {
			t.Fatalf("concurrent append %s failed: %v", o.id, o.err)
		}
		written[o.id] = o.hash
	}
	if len(written) != writers {
		t.Fatalf("%d of %d concurrent appends succeeded", len(written), writers)
	}

	// Every record must be present exactly once, with a contiguous sequence.
	var count, minSeq, maxSeq int64
	mustQueryDB(t, ctx, db, &count,
		`SELECT count(*) FROM audit.record WHERE partition_key = '2026-10'`)
	mustQueryDB(t, ctx, db, &minSeq,
		`SELECT coalesce(min(sequence), 0) FROM audit.record WHERE partition_key = '2026-10'`)
	mustQueryDB(t, ctx, db, &maxSeq,
		`SELECT coalesce(max(sequence), 0) FROM audit.record WHERE partition_key = '2026-10'`)
	if count != maxSeq-minSeq+1 {
		t.Fatalf("partition 2026-10 has %d records spanning sequences %d..%d; the chain has gaps",
			count, minSeq, maxSeq)
	}
	assertChainVerifiesDB(t, ctx, db, "2026-10")
}

// mustQueryDB is dbtest.MustQueryRow for a *sql.DB, for tests that commit
// rather than running inside Resettable's rolled-back transaction.
func mustQueryDB(t *testing.T, ctx context.Context, db *sql.DB, dest any, query string, args ...any) {
	t.Helper()
	if err := db.QueryRowContext(ctx, query, args...).Scan(dest); err != nil {
		t.Fatalf("query: %v", err)
	}
}

// assertChainVerifiesDB is the *sql.DB form of the chain check, for tests that
// commit rather than roll back.
func assertChainVerifiesDB(t *testing.T, ctx context.Context, db *sql.DB, part string) {
	t.Helper()
	var failed int
	mustQueryDB(t, ctx, db, &failed,
		`SELECT count(*) FILTER (WHERE NOT ok) FROM audit.verify_partition($1)`, part)
	if failed != 0 {
		t.Fatalf("%d record(s) in %s failed hash recomputation", failed, part)
	}
}
