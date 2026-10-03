package riskpolicy

import (
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
	"time"
)

func sqlNull(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

// A Postgres interval is not a Go duration. "1 day" and "24:00:00" are the same
// interval and arrive as different strings; "1 mon" and "1 year" are intervals
// with no fixed length in seconds at all. A policy loss_window expressed in
// months has no duration, and guessing one would make the limit depend on the
// calendar -- so those are refused rather than approximated.
func TestParsePostgresInterval(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr string
	}{
		{in: "00:00:01", want: time.Second},
		{in: "00:05:00", want: 5 * time.Minute},
		{in: "01:00:00", want: time.Hour},
		{in: "1 day", want: 24 * time.Hour},
		{in: "2 days", want: 48 * time.Hour},
		{in: "1 day 01:30:00", want: 25*time.Hour + 30*time.Minute},
		{in: "23:59:59.5", want: 23*time.Hour + 59*time.Minute + 59*time.Second + 500*time.Millisecond},
		// Go duration forms, which Postgres also emits for some inputs.
		{in: "1h30m", want: 90 * time.Minute},
		{in: "24h0m0s", want: 24 * time.Hour},
		// Not a fixed length: refused.
		{in: "1 mon", wantErr: "not a fixed duration"},
		{in: "2 years", wantErr: "not a fixed duration"},
		// Malformed.
		{in: "not-an-interval", wantErr: "not a usable interval"},
		{in: "", wantErr: "interval is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parsePostgresInterval(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("%q parsed as %s, want an error containing %q", tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q failed to parse: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("%q parsed as %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// A null interval must be refused rather than becoming zero. Zero would fail
// Validate, but the message would then blame the wrong column -- the operator
// would be told a limit was out of range rather than told the row is incomplete.
func TestParseIntervalRefusesNullAndNonPositive(t *testing.T) {
	if _, err := parseInterval(sqlNull(""), "loss_window"); err == nil {
		t.Error("an empty loss_window was accepted")
	}
	if _, err := parseInterval(sqlNull("00:00:00"), "loss_window"); err == nil {
		t.Error("a zero loss_window was accepted")
	}
}

// The pgx stdlib driver hands a text[] over as its literal form, not as a Go
// []string. A loader that assumed otherwise would work for every non-array column
// and fail on the first permitted_* it read.
func TestTextArrayScan(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{name: "single", in: "{ins-1}", want: []string{"ins-1"}},
		{name: "several", in: "{a,b,c}", want: []string{"a", "b", "c"}},
		{name: "empty", in: "{}", want: nil},
		{name: "null", in: nil, want: nil},
		{name: "quoted comma", in: `{"a,b",c}`, want: []string{"a,b", "c"}},
		{name: "quoted brace", in: `{"a{b",c}`, want: []string{"a{b", "c"}},
		{name: "escaped quote", in: `{"a\"b"}`, want: []string{`a"b`}},
		{name: "spaces preserved", in: "{a, b}", want: []string{"a", " b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got pqStringArray
			if err := got.Scan(tc.in); err != nil {
				t.Fatalf("scan of %v failed: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("scanned %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("element %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A value that is not an array literal must be refused. Returning an empty slice
// would make an unreadable allowlist look like an empty one, and an empty one
// permits nothing -- so a malformed value and a genuinely empty list would
// produce the same, indistinguishable, decision.
func TestTextArrayScanRejectsNonArrays(t *testing.T) {
	for _, in := range []any{"ins-1", "[ins-1]", "ins-1}", 42, []byte("[a]")} {
		var got pqStringArray
		if err := got.Scan(in); err == nil {
			t.Errorf("scanning %v into a text array succeeded, want an error", in)
		}
	}
}

// Round trip through Value, so a test or a future write path using the same type
// cannot silently lose an element.
func TestTextArrayValueRoundTrip(t *testing.T) {
	for _, want := range [][]string{{"a"}, {"a", "b"}, {`a"b`}, {"a,b"}, {`a\b`}} {
		v, err := pqStringArray(want).Value()
		if err != nil {
			t.Fatalf("Value() on %v failed: %v", want, err)
		}
		var got pqStringArray
		if err := got.Scan(v.(driver.Value)); err != nil {
			t.Fatalf("re-reading %q failed: %v", v, err)
		}
		if len(got) != len(want) {
			t.Fatalf("round trip of %v produced %v", want, got)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("round trip of %v: element %d = %q", want, i, got[i])
			}
		}
	}
}

// The six scope kinds are fixed by policy_scope_kind_valid. Resolution over an
// unknown kind would be a silent miss, so ValidScopeKind is the gate.
func TestValidScopeKind(t *testing.T) {
	for _, k := range []string{"ACCOUNT", "STRATEGY", "INSTRUMENT", "VENUE", "MARKET", "ENVIRONMENT"} {
		if !ValidScopeKind(k) {
			t.Errorf("ValidScopeKind(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"", "account", "USER", "PORTFOLIO", "ENVIRONMENT "} {
		if ValidScopeKind(k) {
			t.Errorf("ValidScopeKind(%q) = true, want false", k)
		}
	}
}

// Precedence runs most specific to least, with ENVIRONMENT last because it is the
// only scope that is always present. A reordering here would silently change
// which policy governs an order, so the order is asserted rather than assumed.
func TestScopePrecedenceOrder(t *testing.T) {
	want := []string{"ACCOUNT", "STRATEGY", "INSTRUMENT", "VENUE", "MARKET", "ENVIRONMENT"}
	if len(scopePrecedence) != len(want) {
		t.Fatalf("scopePrecedence has %d kinds, want %d (policy_scope_kind_valid)",
			len(scopePrecedence), len(want))
	}
	for i, k := range want {
		if scopePrecedence[i] != k {
			t.Errorf("scopePrecedence[%d] = %s, want %s", i, scopePrecedence[i], k)
		}
	}
	// specificity must agree with position, since Resolve ranks by it.
	for i, k := range scopePrecedence {
		if got := specificity(k); got != i {
			t.Errorf("specificity(%s) = %d, want %d", k, got, i)
		}
	}
	if specificity("NONSENSE") != -1 {
		t.Error("specificity of an unknown kind did not rank last")
	}
}
