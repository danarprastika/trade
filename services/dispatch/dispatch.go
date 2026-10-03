// Package dispatch publishes committed outbox events and records what happened to
// them.
//
// It is the reader half of doc 05's outbox contract. The writer half is
// Command.Prepare, which commits an outbox row inside the same transaction as the
// state mutation it describes. This package is a separate process reading committed
// rows, which is what makes it safe for it to be unreliable: a transport that loses
// or repeats an event cannot corrupt a financial record, because it never writes one.
//
// # Exactly-once is not the goal
//
// Doc 05 is explicit: "Duplicate publication is acceptable; consumers must be
// idempotent. Financial correctness never depends on exactly-once transport."
//
// That sentence is the design constraint, and it cuts both ways. It means this
// package must never *claim* exactly-once semantics it cannot deliver, and it means
// the recovery paths are all permitted to redeliver rather than to guess:
//
//   - A row whose claim expires is returned to PENDING, not abandoned. A dispatcher
//     that dies mid-batch leaves rows in DISPATCHING forever otherwise, and the
//     alternative -- treating DISPATCHING as delivered -- is precisely the
//     at-most-once assumption doc 05 forbids.
//   - A crash between publishing and marking leaves the row PENDING, so it is
//     published again. The consumer's idempotency is what absorbs that.
//   - Delivery is marked only after the publisher returns, never before.
//
// # Failing closed
//
// An event the dispatcher cannot route is not an event it may drop. An unroutable
// event type is dead-lettered immediately rather than retried, because a routing
// mistake is a permanent condition and retrying it five times only delays the
// discovery. Every other failure increments the attempt count toward the row's
// max_attempts, and the database's own outbox_exhausted_is_dead_lettered constraint
// requires that an exhausted row be marked DEAD_LETTERED rather than left FAILED.
//
// A dead letter is written in the same transaction as the delivery state that
// caused it, so a DEAD_LETTERED row always has a recorded reason and never a reason
// with no row. ops.event_dead_letter carries resolved_at/resolved_by/resolution_note
// for a human to close it out, and its check constraint refuses a partial
// resolution.
package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aitc/trade/contracts"
)

// Message is one claimed outbox row, as handed to a Publisher.
//
// The attempt number travels with it because a publisher asked to deliver the
// fifth attempt is being asked something different from one asked to deliver the
// first, and a publisher that cannot tell the difference cannot decide whether a
// failure is worth another try.
type Message struct {
	EventID       contracts.ID
	EventType     string
	AggregateType string
	AggregateID   string

	// Sequence is the aggregate's own event sequence, so a consumer can detect a
	// gap in what it has seen.
	Sequence int64

	CorrelationID string
	CausationID   string
	ProducerID    string
	Environment   contracts.Environment
	OccurredAt    time.Time
	Payload       json.RawMessage

	// Attempt is 1-based: the number of the delivery being attempted now.
	Attempt int

	// MaxAttempts is the row's own ceiling, so a publisher is never asked to
	// retry beyond what the writer of the row committed to.
	MaxAttempts int

	// ClaimedBy is who holds the claim. It is set on the reclaim path, where it
	// identifies the dispatcher that died holding the row, and is empty on the
	// dispatch path because the publisher is that dispatcher.
	ClaimedBy string

	// OutboxID is the row's surrogate key. It is used to address the row on the
	// reclaim path, where the event id alone would require a second lookup to
	// re-resolve an id that is already in hand.
	OutboxID int64
}

// Publisher delivers one event to whatever consumes it.
//
// It must be idempotent by EventID. Doc 05 permits duplicate publication, so a
// publisher that cannot tolerate seeing the same EventID twice is not conforming,
// and a publisher that is not conforming makes the redelivery paths above unsafe.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// ErrUnroutable means no publisher is registered for an event type.
//
// It is separate from a publication failure because the two are not the same kind
// of problem. A publication failure is usually transient -- the venue was briefly
// unreachable -- and earns another attempt. An unroutable event type is a
// registration gap that no number of retries will fix, so retrying it would only
// delay the moment a human learns the gap exists.
var ErrUnroutable = errors.New("dispatch: no publisher is registered for this event type")

// DefaultBatch is how many rows a DispatchOnce claims when the caller does not
// choose.
//
// Small on purpose. Each claimed row holds a row lock for the duration of its own
// publication, so a large batch lengthens lock hold time and makes a slow publisher
// look like a stuck one to ReclaimStale. Liveness here is cheap to tune; a
// long-held lock is not.
const DefaultBatch = 16

// DefaultLease is how long a claim is honoured before ReclaimStale reclaims it.
const DefaultLease = 30 * time.Second

// Dispatcher claims committed outbox rows, publishes them, and records the result.
type Dispatcher struct {
	db *sql.DB
	// pub is the destination. It is required rather than defaulted: a dispatcher
	// with no publisher would mark rows DISPATCHED without delivering anything,
	// which is the worst failure available here.
	pub Publisher

	// name is this dispatcher's identity. It becomes ops.outbox.claimed_by and the
	// consumer_group on any dead letter, so an undelivered event can be traced to
	// the process that owned it.
	name string

	batch int
	lease time.Duration

	// now is injectable so lease expiry is testable without sleeping.
	now func() time.Time
}

// Result reports what one DispatchOnce did.
//
// Dispatched, Failed and DeadLettered are counted separately because they mean
// different things to whoever is watching: DISPATCHED is progress, FAILED is
// "still trying", and DEAD_LETTERED needs a human.
type Result struct {
	// Claimed is how many rows were taken for delivery this pass.
	Claimed int

	// Dispatched, Failed and DeadLettered are subsets of Claimed, so
	// Dispatched+Failed+DeadLettered <= Claimed. A claimed row is in none of them
	// only if publication is still in flight, which cannot happen: DispatchOnce
	// marks every row it claims before returning.
	Dispatched   int
	Failed       int
	DeadLettered int

	// Reclaimed is how many stale claims were returned to PENDING this pass.
	Reclaimed int
}

// New returns a Dispatcher writing through db.
//
// name identifies this dispatcher in ops.outbox.claimed_by and in dead letters, so
// it must be stable and must not be shared with another live dispatcher: two
// processes claiming under one name make claimed_by useless for diagnosing which
// one lost the row.
func New(db *sql.DB, pub Publisher, name string) (*Dispatcher, error) {
	if db == nil {
		return nil, errors.New("dispatch: a dispatcher needs a database handle")
	}
	if pub == nil {
		// Refused rather than defaulted. A dispatcher with no publisher is the one
		// component that can mark financial events delivered without delivering
		// them, so this is the check that most needs to be here.
		return nil, errors.New("dispatch: a dispatcher needs a publisher; without one it would " +
			"mark committed events delivered without delivering them")
	}
	if name == "" {
		return nil, errors.New("dispatch: a dispatcher needs a name to claim rows under; " +
			"claimed_by is how an undelivered event is traced to the process that owned it")
	}
	return &Dispatcher{
		db:    db,
		pub:   pub,
		name:  name,
		batch: DefaultBatch,
		lease: DefaultLease,
		now:   func() time.Time { return time.Now().UTC() },
	}, nil
}

// WithBatch sets the claim size and returns d, for chaining in a constructor.
func (d *Dispatcher) WithBatch(n int) *Dispatcher {
	if n > 0 {
		d.batch = n
	}
	return d
}

// WithLease sets the claim lease and returns d, for chaining in a constructor.
func (d *Dispatcher) WithLease(l time.Duration) *Dispatcher {
	if l > 0 {
		d.lease = l
	}
	return d
}

// withClock replaces the clock. Unexported because it is a test seam, and named as
// such so that a reader does not mistake it for a supported option.
func (d *Dispatcher) withClock(f func() time.Time) *Dispatcher {
	d.now = f
	return d
}

// DispatchOnce claims one batch, publishes it, and records every outcome.
//
// Rows are claimed in one short transaction and published outside it. Holding a
// row lock across a network call would serialise every other dispatcher behind the
// slowest venue, and the lock buys nothing: the row is already marked DISPATCHING
// with a lease, so a crash here is recoverable by ReclaimStale without holding a
// lock across the failure.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (Result, error) {
	claimed, err := d.claim(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(claimed) == 0 {
		return Result{}, nil
	}

	var res Result
	res.Claimed = len(claimed)
	for _, m := range claimed {
		switch err := d.deliver(ctx, m); {
		case err == nil:
			res.Dispatched++
		case errors.Is(err, errDeadLettered):
			res.DeadLettered++
		default:
			// A publication failure is expected and is not an error of this pass.
			// It is recorded on the row and counted; returning it would tell the
			// caller the batch failed when in fact most of it may have succeeded,
			// and a caller that retried the whole batch would duplicate deliveries
			// for rows that already went out.
			res.Failed++
		}
	}
	return res, nil
}

// claim takes up to d.batch PENDING rows and marks them DISPATCHING.
//
// FOR UPDATE SKIP LOCKED is what lets several dispatchers run concurrently: a row
// another dispatcher holds is skipped rather than waited on, so a slow publisher
// delays its own rows and nobody else's.
func (d *Dispatcher) claim(ctx context.Context) ([]Message, error) {
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("dispatch: begin the claim transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.QueryContext(ctx, `
		SELECT outbox_id, event_id, event_type, aggregate_type, aggregate_id,
		       sequence, correlation_id, causation_id, producer_id, environment::text,
		       occurred_at, payload, attempt_count, max_attempts
		  FROM ops.outbox
		 WHERE dispatch_state = 'PENDING'
		 ORDER BY outbox_id
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, d.batch)
	if err != nil {
		return nil, fmt.Errorf("dispatch: reading pending outbox rows failed: %w", err)
	}

	var (
		out     []Message
		outboxN []int64
	)
	for rows.Next() {
		var (
			m         Message
			rowID     int64
			eventID   string
			env       string
			occurred  time.Time
			payload   []byte
			attempts  int
			maxTry    int
			causation sql.NullString
		)
		if err := rows.Scan(&rowID, &eventID, &m.EventType, &m.AggregateType,
			&m.AggregateID, &m.Sequence, &m.CorrelationID, &causation, &m.ProducerID,
			&env, &occurred, &payload, &attempts, &maxTry); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("dispatch: scanning an outbox row failed: %w", err)
		}
		id, err := contracts.ParseID(eventID)
		if err != nil {
			// The column has a canonical-id CHECK, so this is unreachable for any
			// row that exists. It is refused rather than skipped because skipping
			// would leave the row PENDING and the dispatcher looping on it forever.
			_ = rows.Close()
			return nil, fmt.Errorf("dispatch: outbox row carries event id %s, which does not parse: %w",
				eventID, err)
		}
		m.EventID = id
		m.Environment = contracts.Environment(env)
		m.OccurredAt = occurred.UTC()
		m.Payload = json.RawMessage(payload)
		m.Attempt = attempts + 1
		m.MaxAttempts = maxTry
		if causation.Valid {
			m.CausationID = causation.String
		}
		out = append(out, m)
		outboxN = append(outboxN, rowID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("dispatch: reading pending outbox rows failed: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("dispatch: closing the claim cursor failed: %w", err)
	}
	if len(out) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("dispatch: the empty claim did not commit: %w", err)
		}
		committed = true
		return nil, nil
	}

	now := d.now()
	for _, id := range outboxN {
		// attempt_count is NOT incremented here.
		//
		// Incrementing at claim time is the intuitive design -- the attempt is
		// spent whether or not the publish succeeds -- and the database forbids
		// it. outbox_exhausted_is_dead_lettered requires a row whose attempt_count
		// has reached max_attempts to be DEAD_LETTERED, so a claim that pushed
		// the count to the ceiling would have to make the row DEAD_LETTERED while
		// it was still in flight, and a row cannot be given up on before its last
		// attempt has been made. The count therefore moves on completion: a
		// success leaves it alone, and a failure spends one attempt.
		//
		// A claim that is abandoned by a crash is accounted for in ReclaimExpired
		// instead, which is where a lost attempt is actually observable.
		if _, err := tx.ExecContext(ctx, `
			UPDATE ops.outbox
			   SET dispatch_state = 'DISPATCHING',
			       claimed_by = $2,
			       claimed_at = $3
			 WHERE outbox_id = $1`, id, d.name, now); err != nil {
			return nil, fmt.Errorf("dispatch: claiming outbox row %d failed: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("dispatch: the claim did not commit: %w", err)
	}
	committed = true
	return out, nil
}

// errDeadLettered marks a message whose delivery this pass has given up on. It is
// returned alongside the nil-safe recording so DispatchOnce can count it without
// having to ask the database.
var errDeadLettered = errors.New("dispatch: delivery exhausted or unroutable; the row was dead-lettered")

// deliver publishes one message and records the outcome in its own transaction.
//
// The publish and the record are deliberately not atomic, because they cannot be.
// Document 05 permits a duplicate, so a crash between them costs a redelivery
// rather than a lost or corrupt record.
func (d *Dispatcher) deliver(ctx context.Context, m Message) error {
	pubErr := d.pub.Publish(ctx, m)

	switch {
	case pubErr == nil:
		_, err := d.db.ExecContext(ctx, `
			UPDATE ops.outbox
			   SET dispatch_state = 'DISPATCHED',
			       dispatched_at = $2,
			       last_error = NULL
			 WHERE outbox_id = (SELECT outbox_id FROM ops.outbox
			                        WHERE event_id = $1)`,
			m.EventID.String(), d.now())
		if err != nil {
			return fmt.Errorf("dispatch: marking %s dispatched failed, so it will be "+
				"published again: %w", m.EventID, err)
		}
		return nil

	case errors.Is(pubErr, ErrUnroutable):
		// Permanent. Dead-letter immediately: retrying a routing gap only delays
		// the moment someone learns about it.
		if err := d.deadLetter(ctx, m, "UNROUTABLE", pubErr.Error(), m.Attempt); err != nil {
			return err
		}
		return errDeadLettered

	default:
		// The attempt is spent now, not at claim time. A failure is the only
		// outcome that costs an attempt, which is what lets the schema keep a
		// claimed row strictly below its ceiling.
		next := m.Attempt // m.Attempt is attempt_count+1, i.e. this attempt's number.
		if next < m.MaxAttempts {
			// Back to PENDING so a later pass can claim it. last_error is kept
			// because the reason the last attempt failed is the only evidence of
			// why a financial event is not arriving.
			_, err := d.db.ExecContext(ctx, `
				UPDATE ops.outbox
				   SET dispatch_state = 'PENDING',
				       attempt_count = $2,
				       claimed_by = NULL,
				       claimed_at = NULL,
				       last_error = $3
				 WHERE outbox_id = (SELECT outbox_id FROM ops.outbox
				                        WHERE event_id = $1)`,
				m.EventID.String(), next, truncate(pubErr.Error(), 2000))
			if err != nil {
				return fmt.Errorf("dispatch: returning %s to PENDING after a failed attempt "+
					"failed, so it is now stuck in DISPATCHING: %w", m.EventID, err)
			}
			return fmt.Errorf("dispatch: publishing %s failed on attempt %d of %d: %w",
				m.EventID, m.Attempt, m.MaxAttempts, pubErr)
		}
		// The attempt being made is the last one, so the row is now exhausted. The
		// count has to be written as part of becoming DEAD_LETTERED: the schema
		// requires the two together and refuses either alone.
		if err := d.deadLetter(ctx, m, "EXHAUSTED", pubErr.Error(), next); err != nil {
			return err
		}
		return errDeadLettered
	}
}

// deadLetter marks a row DEAD_LETTERED and records why, in one transaction.
//
// Both writes are in the same transaction deliberately. A DEAD_LETTERED outbox row
// with no ops.event_dead_letter row is an event that has been given up on with no
// recorded reason and no index to find it by; the two are written together or
// neither is.
//
// The dispatch_state and dead_lettered_at updates are not optional bookkeeping
// either: outbox_dead_letter_state requires the timestamp to be present if and
// only if the state is DEAD_LETTERED, and outbox_exhausted_is_dead_lettered forbids
// leaving an exhausted row in any other state.
func (d *Dispatcher) deadLetter(ctx context.Context, m Message, code, detail string, attempts int) error {
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("dispatch: begin the dead-letter transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := d.writeDeadLetter(ctx, tx, m, code, detail, attempts); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dispatch: the dead letter for %s did not commit: %w", m.EventID, err)
	}
	committed = true
	return nil
}

// writeDeadLetter records the dead letter and marks the row, inside a transaction
// the caller owns.
//
// The two writes are deliberately paired. A DEAD_LETTERED outbox row with no
// ops.event_dead_letter row is an event that has been given up on with no recorded
// reason and no index to find it by; a dead letter with no marked row is a record
// of a failure that did not happen. They commit together or neither does.
//
// The payload is copied rather than referenced so the dead letter is a
// self-contained record of what could not be delivered. A dead letter whose meaning
// depends on joining it back to a mutable outbox row stops being evidence the moment
// that row is cleaned up.
func (d *Dispatcher) writeDeadLetter(ctx context.Context, tx *sql.Tx, m Message, code, detail string, attempts int) error {
	var outboxID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT outbox_id FROM ops.outbox WHERE event_id = $1 FOR UPDATE`,
		m.EventID.String()).Scan(&outboxID); err != nil {
		return fmt.Errorf("dispatch: locating outbox row for %s to dead-letter it: %w", m.EventID, err)
	}

	dlID, err := contracts.NewID(contracts.EntityDeadLetter)
	if err != nil {
		return fmt.Errorf("dispatch: minting a dead-letter id failed: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ops.event_dead_letter (
			dead_letter_id, event_id, consumer_group, environment,
			error_code, error_detail, payload, failed_at)
		VALUES ($1,$2,$3,$4::common.environment,$5,$6,$7::jsonb,$8)`,
		dlID.String(), m.EventID.String(), d.name, string(m.Environment),
		code, truncate(detail, 2000), string(m.Payload), d.now()); err != nil {
		return fmt.Errorf("dispatch: writing the dead letter for %s failed: %w", m.EventID, err)
	}

	// attempt_count moves in the same statement that marks the row DEAD_LETTERED,
	// and not before. outbox_exhausted_is_dead_lettered is checked per row on every
	// write, so a statement that spent the final attempt and left the row PENDING
	// would be refused -- and so would one that marked the row DEAD_LETTERED while
	// leaving the count short. The two are a single fact.
	if _, err := tx.ExecContext(ctx, `
		UPDATE ops.outbox
		   SET dispatch_state = 'DEAD_LETTERED',
		       dead_lettered_at = $2,
		       attempt_count = $3,
		       last_error = $4
		 WHERE outbox_id = $1`, outboxID, d.now(), attempts, truncate(detail, 2000)); err != nil {
		return fmt.Errorf("dispatch: marking %s DEAD_LETTERED failed: %w", m.EventID, err)
	}
	return nil
}

// ReclaimExpired recovers rows whose dispatcher died mid-delivery.
//
// An abandoned claim costs the row an attempt, because a dispatcher that claimed a
// row and vanished has consumed that attempt whether or not it managed to publish,
// and a row that keeps being claimed by a process that keeps dying would otherwise
// cycle forever without ever reaching its ceiling.
//
// So each expired claim spends one attempt: rows with attempts left go back to
// PENDING to be claimed afresh, and a row whose last attempt was lost is
// dead-lettered immediately. That second case is not bookkeeping. A row cannot be
// re-claimed once its count reaches max_attempts -- outbox_exhausted_is_dead_lettered
// requires such a row to be DEAD_LETTERED -- so leaving it in DISPATCHING would
// strand a financial event in a state no query for pending work ever returns. It
// would be invisible rather than failed, which is the worst of the three.
//
// This is safe only because doc 05 permits duplicate publication: the worst outcome
// of a wrong reclaim is that an event is published twice, which a conforming
// consumer absorbs. The alternative -- treating an expired claim as delivered --
// would silently drop a financial event, which is the one outcome doc 05's rule
// exists to prevent.
func (d *Dispatcher) ReclaimExpired(ctx context.Context) (Result, error) {
	cutoff := d.now().Add(-d.lease)

	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return Result{}, fmt.Errorf("dispatch: begin the reclaim transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// The expired rows are read and acted on while still locked, so the read that
	// decided a row was abandoned and the write that acts on it cannot be
	// interleaved with another dispatcher's delivery of the same row.
	expired, err := d.selectExpiredClaims(ctx, tx, cutoff)
	if err != nil {
		return Result{}, err
	}

	for _, m := range expired {
		// m.Attempt is already the number of the attempt this claim was making
		// (selectExpiredClaims returns attempt_count+1), so it is the attempt being
		// spent here. Incrementing again would overshoot the row's ceiling and be
		// refused by outbox_attempts_bounded.
		spent := m.Attempt
		if spent < m.MaxAttempts {
			// Re-marked PENDING, never DISPATCHING, so the row is claimed afresh with
			// a fresh lease rather than resuming a claim nobody is holding.
			if _, err := tx.ExecContext(ctx, `
				UPDATE ops.outbox
				   SET dispatch_state = 'PENDING',
				       attempt_count = $2,
				       claimed_by = NULL,
				       claimed_at = NULL,
				       last_error = $3
				 WHERE outbox_id = $1`,
				m.OutboxID, spent, "the claim lease expired with no recorded delivery outcome"); err != nil {
				return Result{}, fmt.Errorf("dispatch: reclaiming outbox row %d failed: %w",
					m.OutboxID, err)
			}
			continue
		}
		detail := fmt.Sprintf("attempt %d of %d was claimed by %s and its lease expired with no "+
			"recorded outcome; the attempt is spent and the row has no attempts left",
			spent, m.MaxAttempts, m.ClaimedBy)
		if err := d.writeDeadLetter(ctx, tx, m, "LEASE_EXPIRED_EXHAUSTED", detail, spent); err != nil {
			return Result{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("dispatch: the reclaim did not commit: %w", err)
	}
	committed = true

	res := Result{Reclaimed: len(expired)}
	for _, m := range expired {
		if m.Attempt >= m.MaxAttempts {
			res.Reclaimed--
			res.DeadLettered++
		}
	}
	return res, nil
}

// DispatchReclaimed reclaims expired claims and then dispatches one batch, which is
// the usual shape of a dispatcher's loop: recover first, then make progress. A loop
// that only dispatched would leave crashed rows in DISPATCHING until someone
// noticed, and a loop that only reclaimed would never deliver anything.
func (d *Dispatcher) DispatchReclaimed(ctx context.Context) (Result, error) {
	res, err := d.ReclaimExpired(ctx)
	if err != nil {
		return Result{}, err
	}
	dispatched, err := d.DispatchOnce(ctx)
	dispatched.Reclaimed = res.Reclaimed
	dispatched.DeadLettered += res.DeadLettered
	return dispatched, err
}

// selectExpiredClaims reads the abandoned claims this pass will act on, holding
// their locks until the reclaim transaction ends.
func (d *Dispatcher) selectExpiredClaims(ctx context.Context, tx *sql.Tx, cutoff time.Time) ([]Message, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT outbox_id, event_id, event_type, aggregate_type, aggregate_id,
		       sequence, correlation_id, causation_id, producer_id, environment::text,
		       occurred_at, payload, attempt_count, max_attempts, claimed_by
		  FROM ops.outbox
		 WHERE dispatch_state = 'DISPATCHING'
		   AND claimed_at IS NOT NULL
		   AND claimed_at < $1
		 ORDER BY outbox_id
		 FOR UPDATE SKIP LOCKED`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("dispatch: reading expired claims failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Message
	for rows.Next() {
		var (
			m         Message
			eventID   string
			env       string
			occurred  time.Time
			payload   []byte
			attempts  int
			maxTry    int
			claimedBy sql.NullString
			causation sql.NullString
		)
		if err := rows.Scan(&m.OutboxID, &eventID, &m.EventType, &m.AggregateType,
			&m.AggregateID, &m.Sequence, &m.CorrelationID, &causation, &m.ProducerID,
			&env, &occurred, &payload, &attempts, &maxTry, &claimedBy); err != nil {
			return nil, fmt.Errorf("dispatch: scanning an expired claim failed: %w", err)
		}
		id, err := contracts.ParseID(eventID)
		if err != nil {
			return nil, fmt.Errorf("dispatch: outbox row carries event id %s, which does not parse: %w",
				eventID, err)
		}
		m.EventID = id
		m.Environment = contracts.Environment(env)
		m.OccurredAt = occurred.UTC()
		m.Payload = json.RawMessage(payload)
		// Attempt is the attempt this claim was making, so the caller can compute
		// the one it is about to spend.
		m.Attempt = attempts + 1
		m.MaxAttempts = maxTry
		m.ClaimedBy = claimedBy.String
		if causation.Valid {
			m.CausationID = causation.String
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dispatch: reading expired claims failed: %w", err)
	}
	return out, nil
}

// truncate bounds a stored error string.
//
// The length limit is not cosmetic: an error carrying a full HTTP response or a
// venue response body can be arbitrarily long, and last_error and error_detail are
// both free text in a table that will be read by a human deciding whether a
// financial event was lost.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Cut on a rune boundary. Truncating mid-rune produces a string that is not
	// valid UTF-8, which PostgreSQL will reject on a text column.
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + " [truncated]"
}
