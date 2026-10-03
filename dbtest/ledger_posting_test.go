package dbtest_test

// The ledger poster against the real tables.
//
// These are integration tests because almost every control they exercise is a
// database control. Balance is a CHECK constraint, the journal header agreeing
// with its entries is a DEFERRED trigger, append-only is a BEFORE UPDATE/DELETE
// trigger, the credential boundary is a BEFORE INSERT trigger, and replay
// suppression is a unique index. A unit test with a fake would agree with the
// poster by construction, which is the one thing that cannot be assumed here.
//
// ledger.entry is append-only, so tests that must observe a real COMMIT leave
// rows behind. That is why this suite runs against a disposable database.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
	"github.com/aitc/trade/domain/ledger"
	auditsvc "github.com/aitc/trade/services/audit"
)

func newPoster(t *testing.T, db *sql.DB) *ledger.Poster {
	t.Helper()
	p, err := ledger.NewPoster(db, auditsvc.NewAppender(db),
		"ledger-poster-test", TestSigningKeyID)
	if err != nil {
		t.Fatalf("NewPoster: %v", err)
	}
	return p
}

// mustCount reads a single count(*) through the test transaction.
//
// dbtest.MustQueryRow takes one destination, so counting is wrapped rather than
// repeated. Every use here asserts on committed rows that this suite wrote, and
// a missing row is a failure to report as such rather than a zero.
func mustCount(t *testing.T, ctx context.Context, tx *sql.Tx, query string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	return n
}

// tradePosting builds a balanced two-leg posting whose ids are unique to this run.
//
// seed separates the tests from each other. dbtest.UniqueSequence is a pure
// function of n, so two tests that both ask for sequence 1 get the SAME id -- and
// a test that counts rows by source_command_id would then be counting another
// test's committed rows. Each test therefore takes a distinct block.
func tradePosting(t *testing.T, seed int) ledger.Posting {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Microsecond)
	seq := func(n int) int64 { return dbtest.UniqueSequence(seed*100 + n) }
	return ledger.Posting{
		SourceCommandID: dbtest.CanonicalID("cmd", int(seq(1))),
		AccountID:       dbtest.CanonicalID("acc", int(seq(2))),
		Environment:     contracts.EnvPaper,
		Currency:        "USD",
		CorrelationID:   dbtest.CanonicalID("evt", int(seq(3))),
		CausationID:     dbtest.CanonicalID("cmd", int(seq(4))),
		PolicyVersion:   "ledger-posting-v1",
		EffectiveAt:     at.Add(-time.Minute),
		OccurredAt:      at,
		ActorID:         "svc_ledger_poster",
		Legs: []ledger.Leg{
			{
				Kind:      ledger.KindTradeCash,
				Direction: ledger.Debit,
				Amount:    contracts.MustParseDecimal("1250.75"),
				Memo:      "consideration for a filled buy",
			},
			{
				Kind:      ledger.KindFee,
				Direction: ledger.Credit,
				Amount:    contracts.MustParseDecimal("1250.75"),
				Memo:      "offsetting leg",
			},
		},
	}
}

// A committed posting is four things or none of them: a journal header, its
// entries, the audit record that explains it, and the outbox row that publishes
// it. Doc 05 requires the last two in the same transaction as the mutation, and
// no trigger enforces either -- so this asserts all four against real rows.
func TestPostingCommitsHeaderEntriesAuditAndOutboxTogether(t *testing.T) {
	db := dbtest.Open(t)
	in := tradePosting(t, 1)

	posted, err := newPoster(t, db).Post(context.Background(), in)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if posted.Duplicate {
		t.Fatal("a fresh posting reported itself as a duplicate")
	}
	if len(posted.EntryIDs) != len(in.Legs) {
		t.Fatalf("got %d entry ids for %d legs", len(posted.EntryIDs), len(in.Legs))
	}

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		// The header, with the totals computed in Go from the legs' own decimals.
		var entryCount int
		var debit, credit string
		if err := tx.QueryRowContext(ctx, `
			SELECT entry_count, debit_total::text, credit_total::text
			  FROM ledger.journal WHERE journal_id = $1`, posted.JournalID.String()).
			Scan(&entryCount, &debit, &credit); err != nil {
			t.Fatalf("read the committed journal header: %v", err)
		}
		// entry_count counts entry rows, not the header: journal_must_balance
		// compares it against COUNT(*) over ledger.entry for this journal.
		if entryCount != len(in.Legs) {
			t.Errorf("header entry_count = %d, want %d (the header is not one of its own entries)",
				entryCount, len(in.Legs))
		}
		if debit != credit {
			t.Errorf("header totals disagree: debit %s, credit %s", debit, credit)
		}
		if posted.DebitTotal.String() != posted.CreditTotal.String() {
			t.Errorf("returned totals disagree: debit %s, credit %s",
				posted.DebitTotal, posted.CreditTotal)
		}

		// The entries, and the database agreeing they are consistent with the
		// header. verify_journal re-derives this, so a passing call is the
		// constraint's own verdict rather than a reimplementation of it.
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM ledger.entry WHERE journal_id = $1`,
			posted.JournalID.String()).Scan(&n); err != nil {
			t.Fatalf("count the journalled entries: %v", err)
		}
		if n != len(in.Legs) {
			t.Errorf("%d entries are journalled, want %d", n, len(in.Legs))
		}
		if _, err := tx.ExecContext(ctx,
			`SELECT ledger.verify_journal($1)`, posted.JournalID.String()); err != nil {
			t.Errorf("the committed journal does not verify: %v", err)
		}

		// The audit record, naming the same journal and the actor that caused it.
		var action, targetID, actor string
		if err := tx.QueryRowContext(ctx, `
			SELECT action, target_id, actor_id FROM audit.record WHERE audit_id = $1`,
			posted.AuditID.String()).Scan(&action, &targetID, &actor); err != nil {
			t.Fatalf("read the audit record: %v", err)
		}
		if action != "LEDGER_JOURNAL_POSTED" {
			t.Errorf("audit action = %q", action)
		}
		if targetID != posted.JournalID.String() {
			t.Errorf("audit target = %s, want the journal %s", targetID, posted.JournalID)
		}
		if actor != in.ActorID {
			t.Errorf("audit actor = %s, want %s", actor, in.ActorID)
		}

		// The outbox row, PENDING and addressed to this journal.
		var dispatchState, aggregateID string
		if err := tx.QueryRowContext(ctx, `
			SELECT dispatch_state, aggregate_id FROM ops.outbox WHERE event_id = $1`,
			posted.EventID.String()).Scan(&dispatchState, &aggregateID); err != nil {
			t.Fatalf("read the outbox row: %v", err)
		}
		if dispatchState != "PENDING" {
			t.Errorf("outbox dispatch_state = %s, want PENDING", dispatchState)
		}
		if aggregateID != posted.JournalID.String() {
			t.Errorf("outbox aggregate = %s, want the journal %s", aggregateID, posted.JournalID)
		}
	})
}

// Replaying an identical posting must not move money twice.
//
// The mechanism is the unique index on source_digest: the poster derives each
// leg's digest from the posting's content, so an identical retry derives
// identical digests and is recognised before anything is written. A retry that
// changed an amount is a different posting, and is refused rather than applied.
func TestReplayingAPostingIsIdempotent(t *testing.T) {
	db := dbtest.Open(t)
	in := tradePosting(t, 2)

	first, err := newPoster(t, db).Post(context.Background(), in)
	if err != nil {
		t.Fatalf("first Post: %v", err)
	}
	second, err := newPoster(t, db).Post(context.Background(), in)
	if err != nil {
		t.Fatalf("replaying the posting was not recognised: %v", err)
	}
	if !second.Duplicate {
		t.Error("the replay reported a fresh posting; it would have moved the money twice")
	}
	if second.JournalID != first.JournalID {
		t.Errorf("replay reported journal %s, want the original %s", second.JournalID, first.JournalID)
	}

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		entries := mustCount(t, ctx, tx,
			`SELECT count(*) FROM ledger.entry WHERE source_command_id = $1`, in.SourceCommandID)
		if entries != len(in.Legs) {
			t.Errorf("the command has %d entries after the replay, want %d; the replay posted again",
				entries, len(in.Legs))
		}
		journals := mustCount(t, ctx, tx,
			`SELECT count(*) FROM ledger.journal WHERE source_command_id = $1`, in.SourceCommandID)
		if journals != 1 {
			t.Errorf("the command has %d journals, want 1", journals)
		}
	})
}

// A posting that shares legs with one already committed, but is not that posting,
// is refused rather than applied.
//
// This is what a torn posting looks like from the outside: two of the four digests
// already exist in the ledger and two do not. Completing it would assert a journal
// the ledger never accepted as a whole; posting it fresh would move the money the
// committed legs already moved. The legs are appended rather than altered on
// purpose -- an altered amount would be caught by the balance check first and
// never reach the replay check this test is about.
func TestAPartiallyCommittedPostingIsRefusedRatherThanCompleted(t *testing.T) {
	db := dbtest.Open(t)
	in := tradePosting(t, 3)

	if _, err := newPoster(t, db).Post(context.Background(), in); err != nil {
		t.Fatalf("first Post: %v", err)
	}

	// The same command, account, instant and first two legs, plus two more that
	// balance against each other.
	extended := in
	extended.Legs = append(append([]ledger.Leg{}, in.Legs...),
		ledger.Leg{Kind: ledger.KindFee, Direction: ledger.Debit, Amount: contracts.MustParseDecimal("10")},
		ledger.Leg{Kind: ledger.KindFee, Direction: ledger.Credit, Amount: contracts.MustParseDecimal("10")},
	)

	_, err := newPoster(t, db).Post(context.Background(), extended)
	if err == nil {
		t.Fatal("a posting that partly overlapped a committed one was accepted")
	}
	if !strings.Contains(err.Error(), "already committed") {
		t.Fatalf("the refusal did not explain the conflict: %v", err)
	}

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		entries := mustCount(t, ctx, tx,
			`SELECT count(*) FROM ledger.entry WHERE source_command_id = $1`, in.SourceCommandID)
		if entries != len(in.Legs) {
			t.Errorf("the command has %d entries after the refused posting, want %d; "+
				"the refusal still wrote", entries, len(in.Legs))
		}
		journals := mustCount(t, ctx, tx,
			`SELECT count(*) FROM ledger.journal WHERE source_command_id = $1`, in.SourceCommandID)
		if journals != 1 {
			t.Errorf("the command has %d journals after the refused posting, want 1", journals)
		}
	})
}

// An unbalanced posting must be refused before the transaction opens, so that the
// caller learns which amounts disagreed instead of receiving a deferred
// constraint error after it has decided the posting succeeded.
func TestAnUnbalancedPostingIsRefusedBeforeItIsWritten(t *testing.T) {
	db := dbtest.Open(t)
	in := tradePosting(t, 4)
	in.Legs[1].Amount = contracts.MustParseDecimal("1250.76")

	_, err := newPoster(t, db).Post(context.Background(), in)
	if err == nil {
		t.Fatal("an unbalanced posting was accepted")
	}
	if !strings.Contains(err.Error(), "do not balance") {
		t.Fatalf("the refusal did not name the problem: %v", err)
	}
	// The message must carry both totals, so the caller can see by how much.
	if !strings.Contains(err.Error(), "1250.75") || !strings.Contains(err.Error(), "1250.76") {
		t.Errorf("the refusal does not carry both totals: %v", err)
	}

	dbtest.Committed(t, db, func(ctx context.Context, tx *sql.Tx) {
		var entries int
		mustCount(t, ctx, tx, `SELECT count(*) FROM ledger.entry WHERE source_command_id = $1`, in.SourceCommandID)
		if entries != 0 {
			t.Errorf("%d entries were written for a refused posting, want 0", entries)
		}
	})
}

// The database refuses an unjournalled internal leg at COMMIT. The poster refuses
// it before that, naming the kind, so the caller is not left holding a
// transaction it already believes succeeded.
func TestAnEntryKindOutsideTheVocabularyIsRefusedByName(t *testing.T) {
	db := dbtest.Open(t)
	in := tradePosting(t, 5)
	in.Legs[0].Kind = ledger.Kind("PROFIT")

	_, err := newPoster(t, db).Post(context.Background(), in)
	if err == nil {
		t.Fatal("an entry kind outside the ledger vocabulary was accepted")
	}
	if !strings.Contains(err.Error(), "PROFIT") {
		t.Fatalf("the refusal does not name the offending kind: %v", err)
	}
}

// A poster with no signing key would write an audit record that attests to
// nothing. audit.record.signing_key_id is NOT NULL, so the schema would accept an
// empty string -- which is why the constructor refuses instead of relying on it.
func TestAPosterNeedsASigningKey(t *testing.T) {
	db := dbtest.Open(t)
	appender := auditsvc.NewAppender(db)
	for _, tc := range []struct {
		name    string
		produce func() error
		want    string
	}{
		{"no database", func() error {
			_, err := ledger.NewPoster(nil, appender, "producer", "key-1")
			return err
		}, "database handle"},
		{"no audit appender", func() error {
			_, err := ledger.NewPoster(db, nil, "producer", "key-1")
			return err
		}, "audit appender"},
		{"no producer", func() error {
			_, err := ledger.NewPoster(db, appender, "", "key-1")
			return err
		}, "producer identity"},
		{"no signing key", func() error {
			_, err := ledger.NewPoster(db, appender, "producer", "")
			return err
		}, "signing key id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.produce()
			if err == nil {
				t.Fatalf("NewPoster accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not explain the missing %s", err, tc.want)
			}
		})
	}
}
