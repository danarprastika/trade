package dbtest_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
)

// Cross-language parity for the canonical identifier encoder.
//
// contracts.NewID and common.new_canonical_id mint every identifier in the
// system, from both sides of the language boundary. 0013 called the SQL function
// "the SQL twin of contracts.NewID" and it was not one: Go took the first 100
// bits of a 16-byte big-endian bit stream five at a time, SQL took the top five
// bits of each of 20 bytes. Both satisfied is_canonical_id, so every format test
// passed, and no test compared the two encoders. These are that comparison.
//
// The vectors are fixed bytes rather than generated IDs, because a parity test
// over random values can only prove the two agree sometimes. Fixed entropy
// proves they agree always, or fails on the first run.

// parityVectors are fixed 16-byte inputs chosen to exercise the bit layout
// rather than a single lucky case:
//
//	all zero           -- every character is the first of the alphabet
//	all one            -- every character is the last of the alphabet
//	0xFF..0x00         -- descending, catches a reversed bit order
//	0x00..0xFF         -- ascending
//	single bit set     -- a lone high bit and a lone low bit, the two places a
//	                     shift error hides
//	alternating        -- high nibble / low nibble, catches nibble-order errors
var parityVectors = []struct {
	name string
	data []byte
}{
	{"all zero", make([]byte, 16)},
	{"all one", repeatByte(0xff, 16)},
	{"descending", bytesOf(0xff, 0xff, 0xfe, 0xfd, 0xfb, 0xf7, 0xef, 0xdf,
		0xbf, 0x7f, 0x3f, 0x1f, 0x0f, 0x07, 0x03, 0x01)},
	{"ascending", bytesOf(0x00, 0x01, 0x03, 0x07, 0x0f, 0x1f, 0x3f, 0x7f,
		0xbf, 0xdf, 0xef, 0xf7, 0xfb, 0xfd, 0xfe, 0xff)},
	{"only high bit", bytesOf(0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)},
	{"only low bit", bytesOf(0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01)},
	{"alternating nibbles", bytesOf(0xf0, 0x0f, 0xf0, 0x0f, 0xf0, 0x0f, 0xf0, 0x0f,
		0xf0, 0x0f, 0xf0, 0x0f, 0xf0, 0x0f, 0xf0, 0x01)},
	{"walk", walkBytes()},
}

func bytesOf(bs ...byte) []byte { return bs }

// repeatByte returns n copies of b. Separate from bytesOf because bytesOf is
// variadic and a call like bytesOf(0xff, 16) silently produces a two-byte slice
// -- the first version of this file made exactly that mistake and the resulting
// "disagreement" was a defect in the vector, not in the encoders.
func repeatByte(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// walkBytes returns 0,1,2,...,15, which walks through the alphabet as a group
// advances and so would expose an off-by-one in the group width.
func walkBytes() []byte {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// TestTheGoAndSQLCanonicalEncodersAgree is the load-bearing test. Before 0020
// it would fail on every vector.
func TestTheGoAndSQLCanonicalEncodersAgree(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		for _, v := range parityVectors {
			t.Run(v.name, func(t *testing.T) {
				want, err := contracts.IDFromBytes(contracts.EntityOrder, v.data)
				if err != nil {
					t.Fatalf("IDFromBytes: %v", err)
				}

				var got string
				dbtest.MustQueryRow(t, ctx, tx, &got,
					`SELECT common.canonical_id_from_bytes('ord', $1::bytea)`, v.data)

				if want.String() != got {
					t.Fatalf("encoders disagree on %s.\n  Go:  %s\n  SQL: %s\n  entropy: %x",
						v.name, want, got, v.data)
				}
			})
		}
	})
}

// Every entity type is exercised, not just one. The prefix is the only
// difference between types, so this pins that the encoder never reaches across
// into it, and that no type has a special case.
func TestTheEncodersAgreeForEveryEntityType(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		entropy := walkBytes()
		for _, et := range contracts.EntityTypes() {
			want, err := contracts.IDFromBytes(et, entropy)
			if err != nil {
				t.Fatalf("IDFromBytes(%s): %v", et, err)
			}
			var got string
			dbtest.MustQueryRow(t, ctx, tx, &got,
				`SELECT common.canonical_id_from_bytes($1, $2::bytea)`, string(et), entropy)
			if want.String() != got {
				t.Fatalf("encoders disagree for entity type %s: Go %s, SQL %s", et, want, got)
			}
		}
	})
}

// The encoding must still be reversible in the sense that matters: every
// character of the output is in the alphabet, the length is 20, and the result
// parses back as the entity type it claims.
func TestAParityVectorStillRoundTripsThroughParse(t *testing.T) {
	id, err := contracts.IDFromBytes(contracts.EntityAccount, walkBytes())
	if err != nil {
		t.Fatalf("IDFromBytes: %v", err)
	}
	parsed, err := contracts.ParseID(id.String())
	if err != nil {
		t.Fatalf("ParseID(%s): %v", id, err)
	}
	if parsed.Entity != contracts.EntityAccount {
		t.Fatalf("parsed entity %s, want %s", parsed.Entity, contracts.EntityAccount)
	}
	if parsed.String() != id.String() {
		t.Fatalf("round trip changed the value: %s -> %s", id, parsed)
	}
}

// Entropy of the wrong width is refused rather than zero-padded. A caller that
// believes it supplied 128 bits and supplied 64 has a bug, and silently padding
// it would halve the entropy of every identifier it then mints.
func TestEntropyOfTheWrongWidthIsRefused(t *testing.T) {
	if _, err := contracts.IDFromBytes(contracts.EntityOrder, make([]byte, 8)); err == nil {
		t.Fatal("IDFromBytes accepted 8 bytes of entropy; want an error")
	}
	if _, err := contracts.IDFromBytes(contracts.EntityOrder, make([]byte, 20)); err == nil {
		t.Fatal("IDFromBytes accepted 20 bytes of entropy; want an error")
	}
	if _, err := contracts.IDFromBytes(contracts.EntityOrder, make([]byte, 16)); err != nil {
		t.Fatalf("IDFromBytes rejected 16 bytes of entropy: %v", err)
	}
}

// The SQL side enforces the same entropy-width contract as the Go side. This
// was mutation C: common.assert_canonical_entropy existed in the migration and
// nothing called it, so removing the guard left every test green. The guard is
// now invoked from inside canonical_id_from_bytes, and this test is what holds
// it there.
func TestTheSQLEncoderAlsoRefusesEntropyOfTheWrongWidth(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		for _, width := range []int{0, 8, 13, 15, 17, 20, 32} {
			// Each rejection aborts the surrounding transaction, so each has to
			// run inside its own savepoint. ExpectRejectedBecause does that;
			// a plain QueryRow in a loop would leave the transaction aborted
			// after the first failure and every later assertion would then be
			// testing PostgreSQL's error state rather than the encoder.
			dbtest.ExpectRejectedBecause(t, ctx, tx, "want exactly 16",
				`SELECT common.canonical_id_from_bytes('ord', $1)`,
				make([]byte, width))
		}

		var id string
		dbtest.MustQueryRow(t, ctx, tx, &id,
			`SELECT common.canonical_id_from_bytes('ord', $1)`, make([]byte, 16))
		if id == "" {
			t.Fatal("SQL encoder produced an empty identifier for 16 bytes of entropy")
		}
	})
}

// A fresh ID is still uniformly distributed, and still 100 bits. Parity fixes
// the mapping; it must not have introduced a way to mint colliding values.
func TestFreshIdentifiersRemainWellFormed(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		seen := make(map[string]bool, 256)
		for i := 0; i < 256; i++ {
			var id string
			dbtest.MustQueryRow(t, ctx, tx, &id,
				`SELECT common.new_canonical_id('ord')`)
			if _, err := contracts.ParseID(id); err != nil {
				t.Fatalf("SQL minted %s which Go cannot parse: %v", id, err)
			}
			if seen[id] {
				t.Fatalf("SQL minted a duplicate identifier %s after %d draws", id, i)
			}
			seen[id] = true
		}
	})
}
