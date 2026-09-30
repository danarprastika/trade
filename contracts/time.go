package contracts

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// TimestampLayout is the canonical persisted timestamp layout: UTC RFC 3339
// with nanosecond precision (03_CANONICAL_CONTRACTS.md, "Time").
const TimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Timestamp is a UTC instant with nanosecond precision. It is the only clock
// type permitted in persisted state, event envelopes and audit records.
//
// The zero Timestamp is invalid: a missing timestamp in an authoritative record
// is a defect, not "the beginning of time", so IsZero is an explicit check that
// callers must make.
type Timestamp struct {
	t time.Time
}

// NewTimestamp normalises an instant to UTC at nanosecond precision.
func NewTimestamp(t time.Time) Timestamp {
	return Timestamp{t: t.UTC().Truncate(time.Nanosecond)}
}

// Now returns the current instant from the injected clock's perspective.
// Production code must pass a clock from internal/clock so that tests are
// deterministic and so that a clock failure is observable rather than silent.
func Now() Timestamp { return NewTimestamp(time.Now()) }

// TimestampAt builds a Timestamp from Unix nanoseconds since the epoch.
func TimestampAt(unixNano int64) Timestamp {
	return NewTimestamp(time.Unix(0, unixNano))
}

// Time returns the underlying UTC time.Time. Callers must not mutate it.
func (ts Timestamp) Time() time.Time { return ts.t }

// UnixNano returns nanoseconds since the epoch.
func (ts Timestamp) UnixNano() int64 { return ts.t.UnixNano() }

// IsZero reports an unset timestamp.
func (ts Timestamp) IsZero() bool { return ts.t.IsZero() }

// String renders the canonical UTC RFC 3339 nanosecond form.
func (ts Timestamp) String() string {
	if ts.t.IsZero() {
		return ""
	}
	return ts.t.UTC().Format(TimestampLayout)
}

// Compare orders two timestamps.
func (ts Timestamp) Compare(o Timestamp) int { return ts.t.Compare(o.t) }

// Before reports ts < o.
func (ts Timestamp) Before(o Timestamp) bool { return ts.t.Before(o.t) }

// After reports ts > o.
func (ts Timestamp) After(o Timestamp) bool { return ts.t.After(o.t) }

// Equal reports ts == o at nanosecond precision.
func (ts Timestamp) Equal(o Timestamp) bool { return ts.t.Equal(o.t) }

// Add returns ts + d.
func (ts Timestamp) Add(d time.Duration) Timestamp { return NewTimestamp(ts.t.Add(d)) }

// Sub returns ts - o as a duration.
func (ts Timestamp) Sub(o Timestamp) time.Duration { return ts.t.Sub(o.t) }

// Age returns the elapsed time since ts, evaluated against now. It returns an
// error for future timestamps because a future-dated source timestamp is a data
// integrity defect that must be surfaced, not silently clamped to zero age.
//
// Market-data freshness evaluation uses this function, so a manipulated future
// timestamp cannot be used to make stale data appear fresh.
func (ts Timestamp) Age(now Timestamp) (time.Duration, error) {
	d := now.t.Sub(ts.t)
	if d < 0 {
		return 0, fmt.Errorf("%w: timestamp %s is after reference %s",
			ErrFutureTimestamp, ts.String(), now.String())
	}
	return d, nil
}

var (
	// ErrInvalidTimestamp indicates a malformed or non-UTC timestamp literal.
	ErrInvalidTimestamp = errors.New("contracts: invalid timestamp")
	// ErrFutureTimestamp indicates a timestamp later than its reference instant.
	ErrFutureTimestamp = errors.New("contracts: future timestamp")
	// ErrMissingTimestamp indicates an absent required timestamp.
	ErrMissingTimestamp = errors.New("contracts: missing timestamp")
)

// ParseTimestamp parses a canonical UTC RFC 3339 timestamp.
//
// A non-UTC offset is accepted on parse and normalised to UTC, because
// external venue payloads legitimately carry offsets; the canonical *stored*
// and *transmitted* form is always UTC with a literal Z.
func ParseTimestamp(s string) (Timestamp, error) {
	if strings.TrimSpace(s) == "" {
		return Timestamp{}, ErrMissingTimestamp
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return Timestamp{}, fmt.Errorf("%w: %q", ErrInvalidTimestamp, s)
	}
	return NewTimestamp(t), nil
}

// MustParseTimestamp parses a static timestamp literal, panicking on failure.
func MustParseTimestamp(s string) Timestamp {
	ts, err := ParseTimestamp(s)
	if err != nil {
		panic(err)
	}
	return ts
}

// MarshalText emits the canonical UTC RFC 3339 nanosecond form.
func (ts Timestamp) MarshalText() ([]byte, error) {
	if ts.t.IsZero() {
		return nil, ErrMissingTimestamp
	}
	return []byte(ts.String()), nil
}

// UnmarshalText parses a canonical timestamp with full validation.
func (ts *Timestamp) UnmarshalText(b []byte) error {
	p, err := ParseTimestamp(string(b))
	if err != nil {
		return err
	}
	*ts = p
	return nil
}

// EventTimes carries the three timestamps required on every event envelope
// (03_CANONICAL_CONTRACTS.md, "Time").
type EventTimes struct {
	// OccurredAt is when the fact happened in the producing system.
	OccurredAt Timestamp
	// RecordedAt is when the platform durably recorded the fact.
	RecordedAt Timestamp
	// SourceTimestamp is the venue/provider-reported origin time, when the
	// source supplies one. It is optional because not every event originates
	// externally; market data always carries it.
	SourceTimestamp *Timestamp
}

// Validate enforces the required ordering and presence rules.
func (e EventTimes) Validate(requireSource bool) error {
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("%w: occurred_at", ErrMissingTimestamp)
	}
	if e.RecordedAt.IsZero() {
		return fmt.Errorf("%w: recorded_at", ErrMissingTimestamp)
	}
	if e.RecordedAt.Before(e.OccurredAt) {
		return fmt.Errorf("%w: recorded_at %s precedes occurred_at %s",
			ErrInvalidTimestamp, e.RecordedAt, e.OccurredAt)
	}
	if requireSource {
		if e.SourceTimestamp == nil || e.SourceTimestamp.IsZero() {
			return fmt.Errorf("%w: source_timestamp", ErrMissingTimestamp)
		}
	}
	return nil
}
