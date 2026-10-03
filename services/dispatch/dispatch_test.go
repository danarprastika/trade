package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The database-independent surface of this package.
//
// Everything that decides which outbox rows exist is SQL, and it is tested in
// dbtest/dispatch_test.go against the live tables. That is the right place for it:
// the ceiling arithmetic in deliver is only meaningful if the schema's own
// outbox_exhausted_is_dead_lettered and outbox_attempts_bounded constraints agree
// with the Go, and a stub would agree with whatever the stub was told.
//
// What is left here is the code that runs before or instead of a query, and it had
// no coverage at all. truncate is the important one: it is the only function in the
// package that transforms text on its way into a column a human reads, and it
// makes a claim about UTF-8 that nothing was checking.
//
// Multi-byte test data is written with \u escapes rather than as literal characters.
// A literal here has to survive being read and rewritten by tools with differing
// opinions about encoding, and a test whose fixture silently became invalid UTF-8
// would be testing the wrong thing while still passing. Writing them as escapes
// makes the bytes under test explicit and the file pure ASCII.

// euro is U+20AC, three bytes in UTF-8 (E2 82 AC). Every multi-byte case below is
// built from it so the cut offsets are checkable by hand.
const euro = "\u20ac"

// TestTruncateIsTotalAndNeverSplitsARune states the invariant truncate exists to
// keep.
//
// A venue error body is very unlikely to be short ASCII, so the multi-byte cases
// are the ones that occur and the ASCII cases are the ones that are easy. The
// failure mode being excluded is specific: cutting mid-rune yields a string that
// is not valid UTF-8, and last_error and ops.event_dead_letter.error_detail are
// text columns that PostgreSQL will refuse rather than store corruptly. That
// refusal would arrive as a failed write inside the dead-letter transaction, which
// is the worst possible moment to discover a string-length bug -- the event being
// dead-lettered is one the platform has already decided it cannot deliver.
func TestTruncateIsTotalAndNeverSplitsARune(t *testing.T) {
	const suffix = " [truncated]"

	// "ab" + EURO SIGN + "cd" is 2 + 3 + 2 = 7 bytes, with the euro sign occupying
	// offsets 2, 3 and 4, so every cut below lands at a different depth of it.
	multibyte := "ab" + euro + "cd"
	ascii := "abcdef"

	cases := []struct {
		name string
		in   string
		max  int

		// want is spelled out rather than derived, because the point of the test is
		// the specific cut, not that the implementation agrees with itself.
		want string
	}{
		{
			// Below the bound: returned untouched. This is the case that must NOT
			// grow a suffix, because a stored error that fits has not been damaged.
			name: "under the limit is returned whole",
			in:   "venue timed out",
			max:  2000,
			want: "venue timed out",
		},
		{
			name: "exactly at the limit is returned whole",
			in:   ascii,
			max:  6,
			want: ascii,
		},
		{
			name: "one over the limit is cut at the bound",
			in:   ascii,
			max:  5,
			want: "abcde" + suffix,
		},
		{
			name: "an ascii cut needs no correction",
			in:   ascii,
			max:  3,
			want: "abc" + suffix,
		},
		{
			// max=4 lands inside the euro sign at offset 2. Walking back over the
			// trailing 0xAC and 0x82 continuation bytes stops at offset 2, so "ab"
			// survives and the euro sign is dropped whole rather than half kept.
			name: "a cut inside a multi-byte rune drops the whole rune",
			in:   multibyte,
			max:  4,
			want: "ab" + suffix,
		},
		{
			// max=5 is the offset just past the euro sign, so nothing needs dropping.
			name: "a cut one byte after a multi-byte rune keeps the rune",
			in:   multibyte,
			max:  5,
			want: "ab" + euro + suffix,
		},
		{
			// A bound smaller than the first rune leaves nothing. This is the case
			// that would panic on a naive cut-1 adjustment, and the loop condition
			// exists for it.
			name: "a bound smaller than the first rune retains nothing",
			in:   euro,
			max:  1,
			want: suffix,
		},
		{
			name: "a zero bound retains nothing",
			in:   multibyte,
			max:  0,
			want: suffix,
		},
		{
			name: "an empty string is returned whole",
			in:   "",
			max:  10,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.max)

			if got != tc.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncate(%q, %d) produced %q, which is not valid UTF-8; "+
					"PostgreSQL would refuse to store it in a text column",
					tc.in, tc.max, got)
			}
		})
	}
}

// TestTruncateKeepsAPrefixOfItsInput states the other half of the contract: whatever
// survives is a prefix of the original and not a reconstruction of it.
//
// This is what makes a truncated last_error usable. A human reading a shortened
// error needs the surviving bytes to be the error's own opening, because that is
// where a venue names itself and the failure.
func TestTruncateKeepsAPrefixOfItsInput(t *testing.T) {
	// "venue returned an error" in Chinese, then the timeout tail.
	cjk := "venue \u8fd4\u56de\u4e86\u4e00\u4e2a\u9519\u8bef: timeout"

	inputs := []string{
		"",
		"a",
		"plain ascii venue rejection",
		cjk,
		strings.Repeat("\u00e9", 500),
		strings.Repeat(euro, 300),
		strings.Repeat("a", 1999) + strings.Repeat(euro, 10),
	}

	for _, in := range inputs {
		for _, max := range []int{0, 1, 2, 3, 7, 1999, 2000, 2001} {
			got := truncate(in, max)

			retained := strings.TrimSuffix(got, " [truncated]")
			if !strings.HasPrefix(in, retained) {
				t.Errorf("truncate(%.20q..., %d) retained %q, which is not a prefix of its input",
					in, max, retained)
			}
			if !utf8.ValidString(retained) {
				t.Errorf("truncate(%.20q..., %d) retained %q, which is not valid UTF-8",
					in, max, retained)
			}
		}
	}
}

// TestTruncateBoundsTheInputRatherThanTheResult records a property of truncate that
// looks like a defect and is not, so that it is changed deliberately if it is
// changed at all.
//
// The bound applies to the retained prefix; the marker is added on top, so the
// result can be up to len(marker) bytes longer than max. That is safe because
// last_error and error_detail are unbounded text columns, and tightening it would
// be wrong: a caller that passed max expecting a total-length guarantee would get
// silent corruption of the final bytes instead.
func TestTruncateBoundsTheInputRatherThanTheResult(t *testing.T) {
	const marker = " [truncated]"

	got := truncate("abcdefghij", 4)
	if want := "abcd" + marker; got != want {
		t.Fatalf("truncate(%q, %d) = %q, want %q", "abcdefghij", 4, got, want)
	}
	if len(got) <= 4 {
		t.Errorf("truncate bounded the result length to %d; the bound is documented to apply "+
			"to the retained prefix, and the marker is added on top", len(got))
	}
	if len(got) > 4+len(marker) {
		t.Errorf("truncate(%q, %d) = %q, which is %d bytes; the retained prefix plus the "+
			"marker should be at most %d", "abcdefghij", 4, got, len(got), 4+len(marker))
	}
}

// TestNewRefusesADispatcherThatCannotDoItsJob covers the constructor's refusals.
//
// A nil publisher is the dangerous one and is also the one dbtest covers against a
// live database, because it is the failure that would mark committed financial
// events delivered without delivering them. A nil handle is here because it needs
// no database to exercise, and it belongs in the same test: the three refusals are
// one rule -- a dispatcher that cannot account for what it claims must not exist --
// and a constructor is the only place that rule can be enforced before a row is
// touched.
func TestNewRefusesADispatcherThatCannotDoItsJob(t *testing.T) {
	// A real handle, not a nil *sql.DB. A typed nil pointer is caught by the FIRST
	// check, so the later two cases would never be reached -- which is how a
	// table-driven case can appear to pass for the wrong reason.
	handle := new(sql.DB)

	cases := []struct {
		name   string
		db     *sql.DB
		pub    Publisher
		label  string
		wantIn string
	}{
		{
			name:   "no database handle",
			db:     nil,
			pub:    nopPublisher{},
			label:  "dispatcher-no-db",
			wantIn: "needs a database handle",
		},
		{
			name:   "no publisher",
			db:     handle,
			pub:    nil,
			label:  "dispatcher-no-pub",
			wantIn: "needs a publisher",
		},
		{
			name:   "no name",
			db:     handle,
			pub:    nopPublisher{},
			label:  "",
			wantIn: "needs a name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.db, tc.pub, tc.label)
			if err == nil {
				t.Fatalf("New accepted a dispatcher with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("New refused with %q, which does not say %q; the message is the only "+
					"thing an operator sees and it should name the missing piece", err, tc.wantIn)
			}
		})
	}
}

// TestNewAcceptsACompleteDispatcherWithDefaults pins the defaults a caller gets when
// it does not choose.
//
// The defaults are a liveness decision rather than a correctness one, so they are
// the kind of value that gets "tidied" without anyone recording why. A batch that
// grew would lengthen row-lock hold time and make a slow publisher look stuck to
// ReclaimExpired; a lease that shrank would reclaim rows from dispatchers that are
// merely slow.
func TestNewAcceptsACompleteDispatcherWithDefaults(t *testing.T) {
	d, err := New(new(sql.DB), nopPublisher{}, "dispatcher-defaults")
	if err != nil {
		t.Fatalf("New refused a complete dispatcher: %v", err)
	}

	if d.batch != DefaultBatch {
		t.Errorf("default batch = %d, want DefaultBatch (%d)", d.batch, DefaultBatch)
	}
	if d.lease != DefaultLease {
		t.Errorf("default lease = %v, want DefaultLease (%v)", d.lease, DefaultLease)
	}
	if d.name != "dispatcher-defaults" {
		t.Errorf("dispatcher name = %q, want the name it was given", d.name)
	}
	if d.now == nil {
		t.Error("New left the clock nil; every timestamp in this package would panic")
	}
	if DefaultBatch <= 0 {
		t.Errorf("DefaultBatch = %d; a non-positive claim limit would claim nothing and the "+
			"dispatcher would spin reporting success", DefaultBatch)
	}
	if DefaultLease <= 0 {
		t.Errorf("DefaultLease = %v; a non-positive lease makes every claim immediately "+
			"expired, so a row could be reclaimed while it is still being published", DefaultLease)
	}
}

// TestTheFluentSettersIgnoreValuesThatWouldDisableThem covers the guard clauses.
//
// WithBatch(0) and WithLease(0) are not neutral, they are disabling. A batch of zero
// claims nothing and the loop still reports success; a lease of zero makes a claim
// expired the instant it is written, so a slow publisher has its row reclaimed under
// it while it is still delivering. Ignoring the non-positive value keeps the last
// good one, which is the behaviour a caller who passes an unset variable wants even
// if it is not the behaviour they thought they asked for.
func TestTheFluentSettersIgnoreValuesThatWouldDisableThem(t *testing.T) {
	t.Run("batch", func(t *testing.T) {
		d, err := New(new(sql.DB), nopPublisher{}, "dispatcher-batch")
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		if got := d.WithBatch(64); got != d {
			t.Error("WithBatch did not return the receiver, so it cannot be chained")
		}
		if d.batch != 64 {
			t.Errorf("batch = %d, want 64", d.batch)
		}

		for _, bad := range []int{0, -1, -1000} {
			d.WithBatch(bad)
			if d.batch != 64 {
				t.Errorf("WithBatch(%d) set the batch to %d; a non-positive claim limit "+
					"claims nothing and the caller is told the pass succeeded", bad, d.batch)
			}
		}
	})

	t.Run("lease", func(t *testing.T) {
		d, err := New(new(sql.DB), nopPublisher{}, "dispatcher-lease")
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		if got := d.WithLease(2 * time.Minute); got != d {
			t.Error("WithLease did not return the receiver, so it cannot be chained")
		}
		if d.lease != 2*time.Minute {
			t.Errorf("lease = %v, want 2m", d.lease)
		}

		for _, bad := range []time.Duration{0, -time.Second, -time.Hour} {
			d.WithLease(bad)
			if d.lease != 2*time.Minute {
				t.Errorf("WithLease(%v) set the lease to %v; a non-positive lease expires a "+
					"claim the moment it is written, so a live publisher has its row "+
					"reclaimed underneath it", bad, d.lease)
			}
		}
	})
}

// TestTheClockIsInjectable pins the seam the lease tests depend on.
//
// This exists so lease expiry can be tested without sleeping. It is only worth
// having if it actually takes effect, and a test seam that silently stopped being
// consulted would leave every lease assertion in dbtest passing against a real
// clock while proving nothing about the arithmetic.
func TestTheClockIsInjectable(t *testing.T) {
	d, err := New(new(sql.DB), nopPublisher{}, "dispatcher-clock")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fixed := time.Date(2026, time.March, 14, 9, 26, 53, 0, time.UTC)
	var calls int
	if got := d.withClock(func() time.Time { calls++; return fixed }); got != d {
		t.Error("withClock did not return the receiver, so it cannot be chained")
	}

	for i := 1; i <= 3; i++ {
		if got := d.now(); !got.Equal(fixed) {
			t.Fatalf("now() = %v on call %d, want the injected %v", got, i, fixed)
		}
	}
	if calls != 3 {
		t.Errorf("the injected clock was called %d times across 3 now() calls; the seam is "+
			"consulted lazily and must be called every time", calls)
	}
}

// TestErrUnroutableIsDistinguishableFromAPublicationFailure pins the sentinel's
// identity, because deliver branches on it.
//
// errors.Is rather than == is how deliver tests it, and that is deliberate: a
// publisher may wrap the sentinel with its own context ("no route for event type
// X") and still be telling the truth. A publisher that returns some other error
// must NOT match, or a transient venue failure would be dead-lettered as a routing
// gap and would stop consuming attempts.
func TestErrUnroutableIsDistinguishableFromAPublicationFailure(t *testing.T) {
	if !errors.Is(ErrUnroutable, ErrUnroutable) {
		t.Error("ErrUnroutable does not match itself")
	}

	wrapped := fmt.Errorf("venue-alpha has no publisher for ORDER_SUBMISSION_REQUESTED: %w",
		ErrUnroutable)
	if !errors.Is(wrapped, ErrUnroutable) {
		t.Error("a publisher cannot wrap ErrUnroutable with context; deliver branches on it " +
			"with errors.Is, so a wrapped sentinel would be retried as if it were transient")
	}

	// The near-miss, and the reason the assertion above is not tautological: quoting the
	// sentinel's text produces an error that reads identically and is not the sentinel.
	// A publisher that did that instead of wrapping would have its routing gap retried
	// against the ceiling, which is the behaviour ErrUnroutable exists to prevent.
	quoted := errors.New(ErrUnroutable.Error())
	if errors.Is(quoted, ErrUnroutable) {
		t.Error("an error quoting ErrUnroutable's text matched the sentinel; deliver would " +
			"dead-letter a transient failure as unroutable")
	}

	for _, transient := range []error{
		errors.New("connection reset by peer"),
		errors.New("context deadline exceeded"),
		nil,
	} {
		if errors.Is(transient, ErrUnroutable) {
			t.Errorf("%v was taken for an unroutable event; a transient failure that matched "+
				"the sentinel would be dead-lettered immediately instead of spending an attempt",
				transient)
		}
	}
}

type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, Message) error { return nil }
