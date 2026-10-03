// Package ledger posts double-entry postings to the ledger.
//
// The ledger is the system of record for money. Every entry is append-only, and a
// posting that is unbalanced is not a posting that is slightly wrong -- it is an
// assertion about balances that is false, permanently, in the one table that
// everything else is reconciled against. The constraints here therefore exist
// mostly to refuse.
package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/services/audit"
)

// Errors the poster returns. They are distinguishable so a caller can tell a
// refused posting from a failed one: a refusal is the ledger working, and a
// failure is a question about the database.
var (
	// ErrUnbalanced means the legs do not sum to zero on their own terms. It is
	// detected before the transaction opens, so nothing was written.
	ErrUnbalanced = errors.New("ledger: the legs do not balance")

	// ErrUnjournalled means an entry kind the database only permits inside a
	// journal was posted without one, or an unknown kind was used at all. The
	// database enforces this too (ledger.journal_must_balance refuses an
	// unjournalled internal leg at COMMIT); the poster refuses it earlier, with
	// the kind named, because a deferred constraint error arrives after the
	// caller has already decided what to do about a successful return.
	ErrUnjournalled = errors.New("ledger: this entry kind must be posted inside a balanced journal")

	// ErrReplayConflict means a source_digest already in the ledger maps to a
	// different entry than the one being posted. Because the digest is derived
	// from the full content of the leg, this is a genuine contradiction -- two
	// different postings claiming the same identity -- and is refused rather than
	// resolved in favour of either.
	ErrReplayConflict = errors.New("ledger: this source digest is already committed against a different entry")

	// ErrNoLegs means the posting carried no legs at all.
	ErrNoLegs = errors.New("ledger: a posting needs at least one leg")
)

// Direction is which way value moves for a given account.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

func (d Direction) valid() bool { return d == Debit || d == Credit }

// Kind is a ledger.entry_kind. The vocabulary mirrors the database enum, and the
// Go constants exist so a typo is a compile error rather than a runtime rejection
// naming a type the caller thought was valid.
type Kind string

const (
	KindCashDeposit             Kind = "CASH_DEPOSIT"
	KindCashWithdrawal          Kind = "CASH_WITHDRAWAL"
	KindCorrection              Kind = "CORRECTION"
	KindFee                     Kind = "FEE"
	KindFunding                 Kind = "FUNDING"
	KindInterest                Kind = "INTEREST"
	KindMarginChange            Kind = "MARGIN_CHANGE"
	KindRealizedPnL             Kind = "REALIZED_PNL"
	KindSettlement              Kind = "SETTLEMENT"
	KindTradeCash               Kind = "TRADE_CASH"
	KindTransfer                Kind = "TRANSFER"
	KindUnrealizedPnLAdjustment Kind = "UNREALIZED_PNL_ADJUSTMENT"
)

// knownKinds is the whole vocabulary. A kind outside it would be rejected by the
// enum cast at insert time, after the transaction opened.
var knownKinds = map[Kind]bool{
	KindCashDeposit: true, KindCashWithdrawal: true, KindCorrection: true,
	KindFee: true, KindFunding: true, KindInterest: true, KindMarginChange: true,
	KindRealizedPnL: true, KindSettlement: true, KindTradeCash: true,
	KindTransfer: true, KindUnrealizedPnLAdjustment: true,
}

// externalKinds are the entry kinds the database permits outside a journal.
//
// It mirrors the condition in ledger.journal_must_balance, which raises unless an
// unjournalled entry is one of these five. Keeping the two lists in agreement is
// the point: a caller assembling postings can ask which movements may stand alone
// instead of learning it by tripping a deferred constraint.
var externalKinds = map[Kind]bool{
	KindCashDeposit:    true,
	KindCashWithdrawal: true,
	KindFunding:        true,
	KindInterest:       true,
	KindMarginChange:   true,
}

// IsExternal reports whether a kind may exist without a journal header.
func (k Kind) IsExternal() bool { return externalKinds[k] }

// Known reports whether the kind is in the database vocabulary at all.
func (k Kind) Known() bool { return knownKinds[k] }

// storageScale is the fixed scale of ledger NUMERIC(38,18) columns.
const storageScale int32 = 18

// currencyPattern mirrors entry_currency_valid / journal_currency_valid.
var currencyPattern = regexp.MustCompile(`^[A-Z0-9_]{3,12}$`)

// Leg is one side of the posting.
type Leg struct {
	Kind      Kind
	Direction Direction
	// Amount is exact. It is a contracts.Decimal rather than a float, because
	// NUMERIC(38,18) cannot be faithfully represented in IEEE-754, and a ledger
	// that rounds quietly is not a ledger.
	Amount contracts.Decimal
	Memo   string

	// CorrectsEntryID, when set, makes this leg a correction of an earlier entry.
	// The database enforces that entry_kind = 'CORRECTION' exactly when
	// corrects_entry_id is set (entry_correction_kind_consistent); the poster
	// refuses the mismatch before the transaction opens.
	CorrectsEntryID string
}

// Posting is the intent to move value.
//
// Currency and AccountID are per-posting rather than per-leg because the journal
// header carries them and the deferred trigger requires every leg to agree with
// the header it claims.
type Posting struct {
	// SourceCommandID is the authoritative command that caused this posting. It is
	// required: a ledger movement with no cause cannot be traced to a decision,
	// and cannot be explained to an auditor.
	SourceCommandID string
	// SourceEventID optionally ties the posting to the OMS event that moved it.
	SourceEventID string

	AccountID   string
	Environment contracts.Environment
	Currency    string

	CorrelationID string
	CausationID   string

	// PolicyVersion is the version of the posting rules these legs were prepared
	// under. It is required because every audit partition row requires a
	// non-empty policy_version, and because a ledger entry recorded without the
	// rules version that produced it cannot be re-derived later. It is supplied
	// rather than defaulted for the same reason the OMS submit command does not
	// invent one: a placeholder would satisfy the column while asserting a version
	// nobody applied.
	PolicyVersion string

	// EffectiveAt is when the value moved. OccurredAt is when the ledger learned.
	// The database enforces effective_at <= recorded_at (entry_effective_before_recorded):
	// value cannot have been learned before it moved.
	EffectiveAt time.Time
	OccurredAt  time.Time

	// Legs is ordered, and the order is preserved in entry sequence.
	Legs []Leg

	// ActorID is who asked for this. A ledger entry with no actor cannot be
	// attributed to a human, a workload, or a service, and the audit record this
	// poster writes requires one.
	ActorID string
}

// Posted is the result of a committed posting.
type Posted struct {
	JournalID contracts.ID
	EntryIDs  []contracts.ID
	// AuditID is the audit record written in the same transaction.
	AuditID contracts.ID
	// EventID is the outbox row the dispatcher will publish.
	EventID contracts.ID

	DebitTotal  contracts.Decimal
	CreditTotal contracts.Decimal

	// Duplicate is true when this posting was already committed under the same
	// digests. Nothing was written and nothing needed to be. This is the normal
	// outcome of a retried command, not an error.
	Duplicate bool
}

// Poster writes postings.
type Poster struct {
	db       *sql.DB
	appender *audit.Appender
	producer string
	// signingKey names the key in audit.record.signing_key_id. It is required
	// rather than defaulted to empty because that column is NOT NULL: an entry
	// posted with an empty key would satisfy the schema while naming no key at
	// all, producing an audit record that asserts a movement without anything
	// attesting to it. That is the shape of evidence that cannot be defended later.
	signingKey string
}

// NewPoster returns a Poster writing through db.
//
// The audit appender is mandatory rather than optional. A ledger entry written
// without its audit record would be an entry whose origin is unrecorded, and the
// append-only trigger means that gap can never be closed afterwards.
func NewPoster(db *sql.DB, a *audit.Appender, producer, signingKey string) (*Poster, error) {
	if db == nil {
		return nil, errors.New("ledger: a poster needs a database handle")
	}
	if a == nil {
		return nil, errors.New("ledger: a poster needs an audit appender; an entry posted without " +
			"its audit record has no recorded cause and cannot be corrected later")
	}
	if producer == "" {
		return nil, errors.New("ledger: a poster needs a producer identity for its outbox rows")
	}
	if signingKey == "" {
		return nil, errors.New("ledger: a poster needs a signing key id; audit.record.signing_key_id " +
			"is NOT NULL, and an empty value would record a movement that nothing attests to")
	}
	return &Poster{db: db, appender: a, producer: producer, signingKey: signingKey}, nil
}

// Post commits the posting.
//
// Everything happens in one transaction: the journal header, every entry, the
// audit record, and the outbox row. Doc 05 requires this. A ledger mutation
// committed without its audit record would assert a movement the evidence trail
// does not corroborate, and the append-only constraints mean the assertion can
// never be withdrawn afterwards.
func (p *Poster) Post(ctx context.Context, in Posting) (Posted, error) {
	sum, err := balance(in.Legs)
	if err != nil {
		return Posted{}, err
	}
	if err := validate(in); err != nil {
		return Posted{}, err
	}

	// Timestamps are truncated to microseconds because that is the resolution of
	// PostgreSQL's timestamptz. Keeping more digits in the Go value and fewer in
	// the column would make the nanosecond columns disagree with the timestamps
	// they are supposed to describe.
	effective := in.EffectiveAt.UTC().Truncate(time.Microsecond)
	recorded := in.OccurredAt.UTC().Truncate(time.Microsecond)

	// Digests depend only on the posting's content, so the replay check below and
	// the inserts inside the transaction agree on identity without coordinating.
	digests := legDigests(in, effective)

	prior, err := p.replay(ctx, in, digests)
	if err != nil {
		return Posted{}, err
	}
	if prior != nil {
		return *prior, nil
	}

	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return Posted{}, fmt.Errorf("ledger: begin posting transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	journalID, err := contracts.NewID(contracts.EntityJournal)
	if err != nil {
		return Posted{}, fmt.Errorf("ledger: minting a journal id failed: %w", err)
	}
	auditID, err := contracts.NewID(contracts.EntityAudit)
	if err != nil {
		return Posted{}, fmt.Errorf("ledger: minting an audit id failed: %w", err)
	}
	entryIDs := make([]contracts.ID, 0, len(in.Legs))
	for range in.Legs {
		id, err := contracts.NewID(contracts.EntityLedgerEntry)
		if err != nil {
			return Posted{}, fmt.Errorf("ledger: minting an entry id failed: %w", err)
		}
		entryIDs = append(entryIDs, id)
	}

	// 1. The header. Its totals come from the same contracts.Decimal values the
	//    legs carry, so the header cannot disagree with the entries it claims.
	//    The deferred trigger re-checks both at COMMIT.
	//
	//    entry_count counts entry rows, and the header is not one of them:
	//    journal_must_balance counts ledger.entry for this journal and compares it
	//    against this column. A header that counted itself would be off by one and
	//    the journal would be refused at COMMIT.
	if err := insertJournal(ctx, tx, journalID, in, auditID, sum, len(in.Legs), recorded); err != nil {
		return Posted{}, err
	}

	// 2. The legs, in order, at consecutive sequences.
	seq, err := nextSequence(ctx, tx)
	if err != nil {
		return Posted{}, err
	}
	for i, leg := range in.Legs {
		if err := insertEntry(ctx, tx, entryLeg{
			EntryID:       entryIDs[i],
			Sequence:      seq + int64(i),
			JournalID:     journalID,
			AuditID:       auditID,
			Posting:       in,
			Leg:           leg,
			SourceDigest:  digests[i],
			EffectiveAt:   effective,
			EffectiveAtNs: effective.UnixNano(),
			RecordedAt:    recorded,
			RecordedAtNs:  recorded.UnixNano(),
		}); err != nil {
			return Posted{}, err
		}
	}

	// 3. The audit record.
	if err := p.recordAudit(ctx, tx, journalID, auditID, in, sum, len(in.Legs)); err != nil {
		return Posted{}, err
	}

	// 4. The outbox row.
	evtID, err := p.enqueue(ctx, tx, journalID, in, sum, len(in.Legs), recorded)
	if err != nil {
		return Posted{}, err
	}

	if err := tx.Commit(); err != nil {
		return Posted{}, fmt.Errorf("ledger: commit posting: %w", err)
	}
	committed = true

	return Posted{
		JournalID:   journalID,
		EntryIDs:    entryIDs,
		AuditID:     auditID,
		EventID:     evtID,
		DebitTotal:  sum.debit,
		CreditTotal: sum.credit,
	}, nil
}

// totals is the exact debit and credit sum of a posting's legs.
type totals struct {
	debit  contracts.Decimal
	credit contracts.Decimal
}

// balance sums the legs with contracts.Decimal, so the check is exact.
//
// This runs before the transaction opens. The database also enforces balance
// (journal_balanced, and journal_must_balance at COMMIT), but that error arrives
// after the caller has committed to reporting this posting, and it names neither
// the amounts nor the leg that broke it.
func balance(legs []Leg) (totals, error) {
	if len(legs) == 0 {
		return totals{}, ErrNoLegs
	}
	var sum totals
	for i, leg := range legs {
		if !leg.Direction.valid() {
			return totals{}, fmt.Errorf("ledger: leg %d has direction %q, which is neither DEBIT nor CREDIT",
				i, leg.Direction)
		}
		if leg.Amount.Sign() <= 0 {
			// entry_amount_positive requires amount > 0. A zero or negative
			// "movement" is a direction without a magnitude, and NUMERIC would
			// accept it were the constraint absent.
			return totals{}, fmt.Errorf("ledger: leg %d has amount %s; a leg must carry a positive "+
				"amount and express direction on the direction column", i, leg.Amount)
		}
		var err error
		switch leg.Direction {
		case Debit:
			sum.debit, err = sum.debit.Add(leg.Amount)
		case Credit:
			sum.credit, err = sum.credit.Add(leg.Amount)
		}
		if err != nil {
			return totals{}, fmt.Errorf("ledger: summing leg %d overflowed: %w", i, err)
		}
	}
	if !sum.debit.Equal(sum.credit) {
		return totals{}, fmt.Errorf("%w: debits %s, credits %s",
			ErrUnbalanced, sum.debit, sum.credit)
	}
	if sum.debit.IsZero() {
		return totals{}, fmt.Errorf("%w: every leg is zero after summation", ErrUnbalanced)
	}
	return sum, nil
}

// validate refuses a posting the database would refuse, with better reasons, and
// nothing that would be accepted after silently changing the caller's numbers.
func validate(in Posting) error {
	if in.SourceCommandID == "" {
		return errors.New("ledger: a posting needs a source command id; an entry with no cause " +
			"cannot be traced to a decision and cannot be explained")
	}
	if in.AccountID == "" {
		return errors.New("ledger: a posting needs an account id; the column is not nullable")
	}
	if err := in.Environment.Validate(); err != nil {
		return fmt.Errorf("ledger: environment: %w", err)
	}
	if !currencyPattern.MatchString(in.Currency) {
		return fmt.Errorf("ledger: currency %q must match %s (journal_currency_valid)",
			in.Currency, currencyPattern)
	}
	if in.CorrelationID == "" {
		return errors.New("ledger: a posting needs a correlation id; without one the entries " +
			"cannot be tied to the decision that caused them")
	}
	if in.PolicyVersion == "" {
		return errors.New("ledger: a posting needs a policy version; every audit record requires one, " +
			"and an entry recorded without the rules version that produced it cannot be re-derived " +
			"later. It is not defaulted: a placeholder would satisfy the column while asserting a " +
			"version nobody applied")
	}
	if in.ActorID == "" {
		return errors.New("ledger: a posting needs an actor id; an unattributed ledger movement " +
			"cannot be audited to a person, workload, or service")
	}
	if in.EffectiveAt.IsZero() {
		return errors.New("ledger: a posting needs an effective_at; the column is not nullable")
	}
	if in.OccurredAt.IsZero() {
		return errors.New("ledger: a posting needs an occurred_at to record at; the column is not nullable")
	}
	if in.EffectiveAt.After(in.OccurredAt) {
		return fmt.Errorf("ledger: effective_at %s is after recorded_at %s; value cannot have been "+
			"learned before it moved (entry_effective_before_recorded)",
			in.EffectiveAt.UTC(), in.OccurredAt.UTC())
	}
	for i, leg := range in.Legs {
		if !leg.Kind.Known() {
			return fmt.Errorf("%w: leg %d has entry kind %q, which is not in the ledger "+
				"vocabulary", ErrUnjournalled, i, leg.Kind)
		}
		if leg.Amount.Scale() > storageScale {
			// NUMERIC(38,18) would round this to 18 places. Refusing is the only
			// honest option: writing it would post a different amount than the one
			// the caller asked for, and the audit record would carry the caller's
			// figure rather than the ledger's.
			return fmt.Errorf("ledger: leg %d has amount %s at scale %d, finer than the ledger's "+
				"NUMERIC(38,18); storing it would round the amount and post something other "+
				"than what was asked for", i, leg.Amount, leg.Amount.Scale())
		}
		isCorrection := leg.CorrectsEntryID != ""
		if isCorrection != (leg.Kind == KindCorrection) {
			want := "entry_kind = 'CORRECTION'"
			if !isCorrection {
				want = "no corrects_entry_id"
			}
			return fmt.Errorf("ledger: leg %d has corrects_entry_id %q, which requires %s "+
				"(entry_correction_kind_consistent)", i, leg.CorrectsEntryID, want)
		}
	}
	return nil
}

// legDigests derives one source_digest per leg.
//
// The digest covers the whole content of the leg -- command, account, currency,
// kind, direction, amount, effective instant, and the leg's index in the posting.
// Two consequences matter:
//
//   - Replaying the identical posting produces identical digests, and
//     ledger_entry_source_digest_idx is unique, so a replay is detectable rather
//     than a second posting.
//   - The digest is content-addressed, so a digest already in the ledger matching
//     a different entry is a real contradiction, which replay refuses instead of
//     silently resolving.
//
// The leg index is included because entry ids are minted per posting attempt.
// Without it, two orderings of the same legs could collide.
func legDigests(in Posting, effective time.Time) []string {
	digests := make([]string, 0, len(in.Legs))
	for i, leg := range in.Legs {
		// Unit separator between fields: it cannot occur in an id, a currency, or
		// an amount, so no combination of field values can produce the same
		// pre-image by shifting a boundary.
		pre := strings.Join([]string{
			in.SourceCommandID,
			in.AccountID,
			in.Currency,
			string(leg.Kind),
			string(leg.Direction),
			leg.Amount.String(),
			effective.Format(time.RFC3339Nano),
			fmt.Sprintf("%d", i),
		}, "\x1f")
		sum := sha256.Sum256([]byte(pre))
		digests = append(digests, hex.EncodeToString(sum[:]))
	}
	return digests
}

// replay returns the prior posting when every leg is already committed under
// these digests, nil when none are, and an error when only some are.
//
// A partial match is a failure rather than a resume. It means the ledger holds some
// legs of a posting whose others are absent, which the deferred constraint should
// have made impossible -- and if it is possible, no amount of writing here restores
// the invariant that the absent legs were never owed.
func (p *Poster) replay(ctx context.Context, in Posting, digests []string) (*Posted, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT source_digest, entry_id, journal_id, account_id, currency, entry_kind::text,
		       direction::text, amount::text
		  FROM ledger.entry
		 WHERE source_digest = ANY($1)`,
		digests)
	if err != nil {
		return nil, fmt.Errorf("ledger: checking for a prior posting failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byDigest := map[string]entrySnapshot{}
	for rows.Next() {
		var digest string
		var s entrySnapshot
		if err := rows.Scan(&digest, &s.EntryID, &s.JournalID, &s.AccountID, &s.Currency,
			&s.Kind, &s.Direction, &s.AmountText); err != nil {
			return nil, fmt.Errorf("ledger: scanning a prior entry failed: %w", err)
		}
		byDigest[digest] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: reading prior entries failed: %w", err)
	}
	switch {
	case len(byDigest) == 0:
		return nil, nil
	case len(byDigest) != len(digests):
		return nil, fmt.Errorf("%w: %d of %d legs are already committed under these digests; a "+
			"partially committed posting cannot be completed without asserting a journal the "+
			"ledger already rejected", ErrReplayConflict, len(byDigest), len(digests))
	}

	journalIDs := map[string]bool{}
	entryIDs := make([]contracts.ID, 0, len(digests))
	for i, digest := range digests {
		s := byDigest[digest]
		leg := in.Legs[i]
		stored, err := contracts.ParseDecimal(s.AmountText)
		if err != nil {
			return nil, fmt.Errorf("%w: committed entry %s holds amount %q, which is not a "+
				"decimal", ErrReplayConflict, s.EntryID, s.AmountText)
		}
		// Amounts are compared as decimals, not as text. The column is
		// NUMERIC(38,18), so a stored 1 comes back as 18 places; comparing strings
		// would report a conflict on every replay of a perfectly good posting.
		if s.AccountID != in.AccountID || s.Currency != in.Currency ||
			string(s.Kind) != string(leg.Kind) || string(s.Direction) != string(leg.Direction) ||
			!stored.Equal(leg.Amount) {
			return nil, fmt.Errorf("%w: digest %s is committed as account %s %s %s %s %s, not "+
				"as leg %d (%s %s %s %s)",
				ErrReplayConflict, digest, s.AccountID, s.Kind, s.Direction, s.AmountText, in.Currency,
				i, leg.Kind, leg.Direction, leg.Amount, in.Currency)
		}
		journalIDs[s.JournalID] = true
		id, err := contracts.ParseID(s.EntryID)
		if err != nil {
			return nil, fmt.Errorf("ledger: committed entry id %q is not canonical: %w", s.EntryID, err)
		}
		entryIDs = append(entryIDs, id)
	}
	if len(journalIDs) != 1 {
		return nil, fmt.Errorf("%w: this posting's legs are spread across %d journals; they were "+
			"committed as more than one posting", ErrReplayConflict, len(journalIDs))
	}

	var journalID string
	for id := range journalIDs {
		journalID = id
	}

	var debit, credit string
	if err := p.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'DEBIT'), 0)::text,
		       COALESCE(SUM(amount) FILTER (WHERE direction = 'CREDIT'), 0)::text
		  FROM ledger.entry WHERE journal_id = $1`,
		journalID).Scan(&debit, &credit); err != nil {
		return nil, fmt.Errorf("ledger: reading the prior journal totals failed: %w", err)
	}
	debitTotal, err := contracts.ParseDecimal(debit)
	if err != nil {
		return nil, fmt.Errorf("ledger: the committed journal holds an unreadable debit total %q: %w",
			debit, err)
	}
	creditTotal, err := contracts.ParseDecimal(credit)
	if err != nil {
		return nil, fmt.Errorf("ledger: the committed journal holds an unreadable credit total %q: %w",
			credit, err)
	}
	id, err := contracts.ParseID(journalID)
	if err != nil {
		return nil, fmt.Errorf("ledger: committed journal id %q is not canonical: %w", journalID, err)
	}
	return &Posted{
		JournalID:   id,
		EntryIDs:    entryIDs,
		DebitTotal:  debitTotal,
		CreditTotal: creditTotal,
		Duplicate:   true,
	}, nil
}

// nextSequence allocates the next ledger-wide entry sequence.
//
// The sequence is unique across the whole ledger (ledger_entry_sequence_idx), not
// per account, so concurrent postings for different accounts still collide. An
// advisory transaction lock serialises the read-and-increment; without it two
// postings could both read max+1 and one would fail on the index at COMMIT, long
// after the caller was told nothing.
func nextSequence(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock($1)`, sequenceLockKey); err != nil {
		return 0, fmt.Errorf("ledger: taking the entry sequence lock failed: %w", err)
	}
	var seq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(max(sequence), 0) + 1 FROM ledger.entry`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("ledger: reading the next entry sequence failed: %w", err)
	}
	return seq, nil
}

// sequenceLockKey is a fixed advisory-lock key for the ledger entry sequence. It
// must never be reused for a different resource.
const sequenceLockKey int64 = 0x1ed6e2_00000001

// entrySnapshot is one committed entry as read by the replay check.
type entrySnapshot struct {
	EntryID    string
	JournalID  string
	AccountID  string
	Currency   string
	Kind       Kind
	Direction  Direction
	AmountText string
}

// entryLeg is everything insertEntry needs about one leg.
type entryLeg struct {
	EntryID       contracts.ID
	Sequence      int64
	JournalID     contracts.ID
	AuditID       contracts.ID
	Posting       Posting
	Leg           Leg
	SourceDigest  string
	EffectiveAt   time.Time
	EffectiveAtNs int64
	RecordedAt    time.Time
	RecordedAtNs  int64
}

// insertJournal writes the header row.
func insertJournal(ctx context.Context, tx *sql.Tx, journalID contracts.ID, in Posting,
	auditID contracts.ID, sum totals, entryCount int, recorded time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ledger.journal (
			journal_id, source_command_id, environment, account_id, currency,
			entry_count, debit_total, credit_total, posted_at, posted_at_ns,
			correlation_id, audit_id)
		VALUES ($1, $2, $3::common.environment, $4, $5,
		        $6, $7, $8, common.ns_to_timestamptz($9), $9, $10, $11)`,
		journalID.String(), in.SourceCommandID, string(in.Environment), in.AccountID, in.Currency,
		entryCount, sum.debit.String(), sum.credit.String(), recorded.UnixNano(),
		in.CorrelationID, auditID.String()); err != nil {
		return fmt.Errorf("ledger: inserting the journal header failed: %w", err)
	}
	return nil
}

// insertEntry writes one leg.
func insertEntry(ctx context.Context, tx *sql.Tx, l entryLeg) error {
	var sourceEvent, causation, corrects, memo any
	if l.Posting.SourceEventID != "" {
		sourceEvent = l.Posting.SourceEventID
	}
	if l.Posting.CausationID != "" {
		causation = l.Posting.CausationID
	}
	if l.Leg.CorrectsEntryID != "" {
		corrects = l.Leg.CorrectsEntryID
	}
	if l.Leg.Memo != "" {
		memo = l.Leg.Memo
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ledger.entry (
			entry_id, sequence, account_id, environment, entry_kind, direction,
			amount, currency, source_command_id, source_event_id, correlation_id,
			causation_id, corrects_entry_id, memo, effective_at, effective_at_ns,
			recorded_at, recorded_at_ns, source_digest, audit_id, journal_id)
		VALUES ($1, $2, $3, $4::common.environment, $5::ledger.entry_kind, $6::ledger.entry_direction,
		        $7, $8, $9, $10, $11,
		        $12, $13, $14, common.ns_to_timestamptz($15), $15,
		        common.ns_to_timestamptz($16), $16, $17, $18, $19)`,
		l.EntryID.String(), l.Sequence, l.Posting.AccountID, string(l.Posting.Environment),
		string(l.Leg.Kind), string(l.Leg.Direction), l.Leg.Amount.String(), l.Posting.Currency,
		l.Posting.SourceCommandID, sourceEvent, l.Posting.CorrelationID,
		causation, corrects, memo, l.EffectiveAtNs,
		l.RecordedAtNs, l.SourceDigest, l.AuditID.String(), l.JournalID.String()); err != nil {
		return fmt.Errorf("ledger: inserting entry %s failed: %w", l.EntryID, err)
	}
	return nil
}

// recordAudit writes the audit record for the posting.
func (p *Poster) recordAudit(ctx context.Context, tx *sql.Tx, journalID, auditID contracts.ID,
	in Posting, sum totals, legs int) error {
	reason := fmt.Sprintf("%d-entry balanced posting to account %s", legs, in.AccountID)
	details, err := json.Marshal(map[string]any{
		"journal_id":   journalID.String(),
		"account_id":   in.AccountID,
		"currency":     in.Currency,
		"debit_total":  sum.debit.String(),
		"credit_total": sum.credit.String(),
		"entry_count":  legs,
		"kinds":        kindsOf(in.Legs),
		"source_event": in.SourceEventID,
	})
	if err != nil {
		return fmt.Errorf("ledger: encoding audit details failed: %w", err)
	}
	if _, err := p.appender.AppendIn(ctx, tx, audit.Record{
		AuditID:            auditID.String(),
		TenantOrOwnerScope: "trading",
		ActorID:            in.ActorID,
		ActorType:          contracts.ActorWorkload,
		Action:             "LEDGER_JOURNAL_POSTED",
		TargetType:         "LEDGER_JOURNAL",
		TargetID:           journalID.String(),
		Environment:        in.Environment,
		OccurredAt:         in.OccurredAt.UTC(),
		RecordedAt:         in.OccurredAt.UTC(),
		Reason:             &reason,
		CorrelationID:      in.CorrelationID,
		CausationID:        nonEmpty(in.CausationID),
		PolicyVersion:      in.PolicyVersion,
		Result:             contracts.AuditResultSuccess,
		Details:            string(details),
		SigningKeyID:       p.signingKey,
	}); err != nil {
		return fmt.Errorf("ledger: the posting was not audited, so the transaction is abandoned: %w", err)
	}
	return nil
}

// enqueue writes the outbox row for the posting.
//
// The sequence is 1 because each journal is its own aggregate and receives exactly
// one LEDGER_JOURNAL_POSTED event; outbox_aggregate_sequence_unique is on
// (aggregate_type, aggregate_id, sequence), and the aggregate id is a freshly
// minted journal, so a fixed sequence cannot collide.
func (p *Poster) enqueue(ctx context.Context, tx *sql.Tx, journalID contracts.ID, in Posting,
	sum totals, legs int, recorded time.Time) (contracts.ID, error) {
	evtID, err := contracts.NewID(contracts.EntityEvent)
	if err != nil {
		return contracts.ID{}, fmt.Errorf("ledger: minting an event id failed: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"journal_id":   journalID.String(),
		"account_id":   in.AccountID,
		"currency":     in.Currency,
		"debit_total":  sum.debit.String(),
		"credit_total": sum.credit.String(),
		"entry_count":  legs,
		"command_id":   in.SourceCommandID,
	})
	if err != nil {
		return contracts.ID{}, fmt.Errorf("ledger: encoding the outbox payload failed: %w", err)
	}
	var causation any
	if in.CausationID != "" {
		causation = in.CausationID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ops.outbox (
			event_id, event_type, schema_version, aggregate_type, aggregate_id,
			sequence, correlation_id, causation_id, producer_id, environment,
			occurred_at, occurred_at_ns, recorded_at, payload, dispatch_state, attempt_count, max_attempts)
		VALUES ($1,'LEDGER_JOURNAL_POSTED','1.0.0','LEDGER_JOURNAL',$2,
		        1,$3,$4,$5,$6::common.environment,
		        common.ns_to_timestamptz($7),$7,common.ns_to_timestamptz($7),$8::jsonb,
		        'PENDING',0,5)`,
		evtID.String(), journalID.String(), in.CorrelationID, causation, p.producer,
		string(in.Environment), recorded.UnixNano(), string(payload)); err != nil {
		return contracts.ID{}, fmt.Errorf("ledger: inserting the outbox row failed; a committed "+
			"state mutation must always have one (doc 05): %w", err)
	}
	return evtID, nil
}

// kindsOf lists the entry kinds in a posting, for the audit record.
func kindsOf(legs []Leg) []string {
	kinds := make([]string, 0, len(legs))
	for _, leg := range legs {
		kinds = append(kinds, string(leg.Kind))
	}
	return kinds
}

// nonEmpty returns a pointer to s, or nil when s is empty, for nullable columns.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
