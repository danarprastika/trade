// Package dbtest provides the integration-test harness for the authoritative
// PostgreSQL schema.
//
// These tests exist because the blueprint makes several financial invariants
// STRUCTURAL rather than procedural: the ledger is append-only, the audit chain
// is continuous, an order cannot reach a submission state without a risk
// decision, and a rejected risk decision must name a failed control. A CHECK
// constraint that is never exercised by a negative test is documentation, not a
// control (09_TESTING_AND_RELEASE_EVIDENCE.md, 24_ENTERPRISE_RELEASE_STANDARD.md §13).
//
// Tests skip (not fail) when AITC_TEST_DATABASE_URL is unset, so the unit suite
// stays runnable without a database. The database suite MUST run before any gate
// is claimed PASS.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// EnvTestDatabaseURL names the environment variable holding the connection
// string for the integration database.
const EnvTestDatabaseURL = "AITC_TEST_DATABASE_URL"

// TestDatabaseURL returns the configured integration database URL, or the empty
// string when the database suite should be skipped.
func TestDatabaseURL() string { return os.Getenv(EnvTestDatabaseURL) }

// Open returns a connection to the integration database, or skips the test.
//
// The database must be an ephemeral, disposable instance. Tests drop and
// recreate schemas, so pointing this at anything shared is unsafe.
func Open(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv(EnvTestDatabaseURL)
	if url == "" {
		t.Skipf("%s is not set; skipping database integration test", EnvTestDatabaseURL)
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping database: %v", err)
	}
	return db
}

// Resettable runs fn inside a transaction that is rolled back afterwards, so
// each test starts from a clean state without cross-test interference.
// Committed runs fn in a transaction that is COMMITTED, rather than rolled back
// like Resettable, and truncates the trading tables afterwards so the committed
// rows cannot leak into another test.
//
// It exists because one of the two invariant 5 controls is only reachable across
// a transaction boundary. A deferred trigger on portfolio.position fires when a
// position row is written; it cannot observe a fill that lands in a later
// transaction with no position write at all. The fill-side trigger exists to
// close exactly that gap, and a test that builds its whole scenario inside one
// transaction never exercises it -- the position-side trigger quietly covers
// for it, and the fill-side trigger could be deleted without any test going
// red. Committing the setup is the only way to make that control load-bearing.
func Committed(t *testing.T, db *sql.DB, fn func(ctx context.Context, tx *sql.Tx)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatalf("begin committed: %v", err)
	}
	fn(ctx, tx)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanCancel()
		// audit.record is deliberately NOT truncated.
		//
		// audit.partition_month stores the head of each monthly chain (last
		// sequence and last hash), and that bookkeeping is the anchor the next
		// append chains onto. Truncating the rows while leaving the head intact
		// produces a chain that begins at a record which no longer exists, and
		// every subsequent verification correctly refuses it.
		//
		// That is the audit design working as intended -- a chain that has lost
		// its history must not verify -- but it is a corruption for a test
		// helper to introduce. An earlier version of this helper truncated
		// audit.record, which made the audit service tests pass on the first run
		// after a reset and fail on every run after that, depending only on
		// whether this helper had already executed. Committed tests do not create
		// audit records, so leaving the chain alone is both correct and sufficient.
		if _, err := db.ExecContext(cleanCtx, `TRUNCATE
			portfolio.position, portfolio.balance,
			oms.fill, oms.order_event, oms."order",
			market.trade, market.quote, market.candle, market.venue_symbol, market.instrument,
			identity.session, identity.subject,
			ops.outbox, ops.event_consumer_offset, ops.event_dead_letter
			RESTART IDENTITY CASCADE`); err != nil {
			t.Errorf("truncate after committed test: %v", err)
		}
	})
}

func Resettable(t *testing.T, db *sql.DB, fn func(ctx context.Context, tx *sql.Tx)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	fn(ctx, tx)
	if err := tx.Rollback(); err != nil && !strings.Contains(err.Error(), "sql: transaction has already been committed") {
		// A deliberate constraint violation aborts the transaction; the rollback
		// error is expected in tests that assert a rejection.
		if !strings.Contains(err.Error(), "current transaction is aborted") {
			t.Logf("rollback: %v", err)
		}
	}
}

// MustExec runs an exec and fails the test on error.
func MustExec(t *testing.T, ctx context.Context, tx *sql.Tx, query string, args ...any) sql.Result {
	t.Helper()
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", trim(query), err)
	}
	return res
}

// MustQueryRow runs a query and scans a single value.
func MustQueryRow(t *testing.T, ctx context.Context, tx *sql.Tx, dest any, query string, args ...any) {
	t.Helper()
	if err := tx.QueryRowContext(ctx, query, args...).Scan(dest); err != nil {
		t.Fatalf("query %q: %v", trim(query), err)
	}
}

// ExpectRejected asserts that a statement is rejected by the database.
//
// This is the workhorse of the negative-test suite: a control that is supposed
// to be structural is proven structural by demonstrating that the database
// refuses the forbidden write.
//
// The statement runs inside its own SAVEPOINT and the savepoint is always
// rolled back afterwards. Without this, a rejected statement leaves the
// surrounding transaction in the PostgreSQL "aborted" state, and EVERY later
// statement in the same test fails with 25P02. A test with two rejections would
// then "pass" for the wrong reason: the second assertion would be satisfied by
// the abort left behind by the first. That is precisely the kind of green test
// that hides a missing control, so the harness must not permit it.
func ExpectRejected(t *testing.T, ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	t.Helper()

	savepoint := fmt.Sprintf("dbtest_reject_%d", atomic.AddInt64(&savepointSeq, 1))
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		t.Fatalf("savepoint %s: %v", savepoint, err)
	}

	_, err := tx.ExecContext(ctx, query, args...)

	// Restore a usable transaction regardless of the outcome.
	if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); rbErr != nil {
		t.Fatalf("rollback to savepoint %s: %v", savepoint, rbErr)
	}
	if _, relErr := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); relErr != nil {
		t.Fatalf("release savepoint %s: %v", savepoint, relErr)
	}

	if err == nil {
		t.Fatalf("database ACCEPTED a write that must be structurally rejected: %s", trim(query))
	}
	return err
}

// savepointSeq makes each ExpectRejected savepoint name unique inside one
// transaction.
var savepointSeq int64

// runNonce makes generated identifiers unique per test-process run.
//
// The ledger is append-only by design, so a test that COMMITS a real journal
// cannot clean up after itself: ledger_entry_append_only refuses the DELETE.
// Rather than weakening that control for test convenience, the committed
// fixtures get fresh identities on every run and the suite declares that it
// expects a disposable, freshly migrated database.
var runNonce int64

// SetRunNonce is called from TestMain. It must be set before any CanonicalID
// call, because the generated identifier depends on it.
func SetRunNonce(n int64) { runNonce = n }

// RunNonce returns the current per-run identifier mix.
func RunNonce() int64 { return runNonce }

// CanonicalID returns a syntactically valid canonical identifier with the given
// prefix, for fixtures that must satisfy the storage-level format constraints.
//
// The 20 body characters are expanded from a 64-bit FNV-1a hash of
// (prefix, seed, run nonce) rather than from a simple offset. An earlier
// version indexed the alphabet with (i + seed) mod 32, which produced only 32
// distinct identifiers per prefix and therefore collided as soon as a suite
// generated more than a handful of them.
//
// The same (prefix, n) always yields the same identifier WITHIN one run, so a
// test can compute an id once and reuse it, while two runs cannot collide.
func CanonicalID(prefix string, n int) string {
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

	sum := fnv.New64a()
	_, _ = fmt.Fprintf(sum, "%s|%d|%d", prefix, n, runNonce)
	state := sum.Sum64()

	var b strings.Builder
	b.WriteString(prefix)
	b.WriteByte('_')
	for i := 0; i < 20; i++ {
		b.WriteByte(alphabet[state&31])
		// xorshift the 64-bit state so the next 5 bits are fresh.
		state = state>>5 | state<<59
	}
	return b.String()
}

// UniqueSequence returns a per-run unique positive sequence number.
//
// ledger.entry.sequence is globally UNIQUE and append-only, so a test that must
// observe a real COMMIT cannot reuse the same sequence number on a second run
// against the same database.
func UniqueSequence(n int) int64 {
	const base = 1 << 31
	run := int64(runNonce&0x3fffffff) % 100000
	return run*base + int64(n)
}

// UniqueDigest returns a per-run unique 64-character lowercase hex digest of
// the shape every *_digest / *_hash column requires.
//
// ledger.entry.source_digest is UNIQUE — that uniqueness IS the control making a
// duplicated source fact idempotent — so a committed fixture cannot reuse a
// literal digest across runs.
func UniqueDigest(n int) string {
	sum := fnv.New64a()
	_, _ = fmt.Fprintf(sum, "digest|%d|%d", n, runNonce)
	state := sum.Sum64()

	const hexdigits = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < 64; i++ {
		b.WriteByte(hexdigits[state&15])
		state = state>>4 | state<<60
	}
	return b.String()
}

// NowNs returns nanoseconds since the Unix epoch for test fixtures.
func NowNs() int64 { return time.Now().UTC().UnixNano() }

// SprintfID is a convenience wrapper used when a readable but unique id helps
// diagnose a failure.
// SettleDeferred forces every DEFERRABLE INITIALLY DEFERRED constraint and
// constraint trigger to be evaluated immediately, exactly as it would be at
// COMMIT, without ending the transaction.
//
// This exists because Resettable rolls back. A deferred trigger therefore never
// fires in any test that relies on Resettable, which means a test asserting that
// a deferred control REFUSES something will pass whether or not the control
// exists -- the refusal it is waiting for never happens, and the rollback
// discards the bad row quietly. That is the worst possible shape for a
// negative test: it is green, it is meaningless, and it silently hides a missing
// control. A mutation test is the only reliable way to notice, which is why the
// G1 evidence records one for every deferred control.
//
// Call this as the last statement of a test body that exercises a deferred
// control, so the checks actually run before the rollback.
func SettleDeferred(t *testing.T, ctx context.Context, tx *sql.Tx) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		t.Fatalf("settle deferred constraints: %v", err)
	}
}

// ExpectDeferredRejected asserts that a deferred control refuses to let the
// transaction settle, and that it refuses for one of the stated reasons.
//
// It is the deferred counterpart of ExpectRejected, and it exists for the same
// reason: without it a test that expects a deferred constraint to refuse
// something cannot express that expectation, because SettleDeferred treats the
// error as a test failure.
//
// want is a set rather than a single string because invariant 5 is enforced by
// two complementary deferred controls whose firing order is decided by
// PostgreSQL's deferred-queue ordering, not by this schema. Where more than one
// refusal is a correct answer the test names all of them, so that the assertion
// still pins the REASON without pinning which control happened to speak first.
func ExpectDeferredRejected(t *testing.T, ctx context.Context, tx *sql.Tx, want ...string) error {
	t.Helper()
	savepoint := fmt.Sprintf("dbtest_deferred_%d", atomic.AddInt64(&savepointSeq, 1))
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		t.Fatalf("savepoint %s: %v", savepoint, err)
	}
	_, err := tx.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`)
	if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); rbErr != nil {
		t.Fatalf("rollback to %s: %v", savepoint, rbErr)
	}
	if err == nil {
		t.Fatalf("the transaction settled, but a deferred control should have refused it (%s)", strings.Join(want, " or "))
	}
	for _, w := range want {
		if strings.Contains(err.Error(), w) {
			return err
		}
	}
	t.Fatalf("a deferred control refused the transaction, but for an unexpected reason; wanted one of %q, got: %v", want, err)
	return err
}

// ExpectRejectedBecause asserts that a write is refused AND that it is refused
// for the stated reason.
//
// ExpectRejected is enough when any refusal is the only acceptable outcome, but
// it is not enough when the test is the sole evidence that a particular control
// fired. A malformed query, a typo in a column name, or a missing function
// produces an error too, and a test that accepts any error will pass against
// any of them. That failure is silent: the test is green, the control is
// absent, and the suite reports confidence it has not earned.
//
// This helper exists because that exact substitution happened while building
// migration 0017. The secrets walker referenced jsonb_each columns that do not
// exist, every negative test for it went green against that unrelated error,
// and the control was never actually exercised.
func ExpectRejectedBecause(t *testing.T, ctx context.Context, tx *sql.Tx, want string, query string, args ...any) error {
	t.Helper()
	savepoint := fmt.Sprintf("dbtest_because_%d", atomic.AddInt64(&savepointSeq, 1))
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		t.Fatalf("savepoint %s: %v", savepoint, err)
	}
	_, err := tx.ExecContext(ctx, query, args...)
	if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); rbErr != nil {
		t.Fatalf("rollback to %s: %v", savepoint, rbErr)
	}
	if err == nil {
		t.Fatalf("the write was accepted, but it should have been refused (%s)", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("the write was refused, but for an unrelated reason; wanted %q, got: %v", want, err)
	}
	return err
}

func SprintfID(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// IsCanonicalID asks the database whether id is a well-formed canonical
// identifier with the given prefix.
//
// The question is deliberately not answered in Go. common.is_canonical_id is
// the single authority on the alphabet, the 20-character body and the prefix,
// and a second Go implementation could only ever agree with it until one of
// them changed.
func IsCanonicalID(t *testing.T, ctx context.Context, tx *sql.Tx, id, prefix string) bool {
	t.Helper()
	var ok bool
	MustQueryRow(t, ctx, tx, &ok, `SELECT common.is_canonical_id($1, $2)`, id, prefix)
	return ok
}

func trim(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if len(q) > 200 {
		return q[:200] + "..."
	}
	return q
}
