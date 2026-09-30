package dbtest_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
)

// Canonical checkpoint body conformance.
//
// 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3 requires the canonical serialisation to
// be "fixed and tested", and that checkpoints are "signed by a managed KMS/HSM
// key". A signature is only verifiable if both the signer and the verifier
// derive the same bytes, so the body is specified once and implemented twice:
// audit.canonical_checkpoint_body in SQL, contracts.CanonicalCheckpointBody in
// Go. They must agree byte-for-byte.
//
// This test is what makes "fixed and tested" true for the checkpoint body
// rather than merely asserted.

// checkpointBodyCase is one serialisation input. The cases cover the parts that
// are easy to get wrong: an absent successor (NULL, not "") and values that two
// different JSON encoders disagree about.
type checkpointBodyCase struct {
	name string
	body contracts.Checkpoint
}

func checkpointBodyCases() []checkpointBodyCase {
	successor := "3333333333333333333333333333333333333333333333333333333333333333"
	return []checkpointBodyCase{
		{
			name: "sealed before the head, successor named",
			body: contracts.Checkpoint{
				CheckpointID:           "ckp_01hq0000000000000000000001",
				PartitionKey:           "2026-09",
				FirstSequence:          1,
				LastSequence:           250,
				RecordCount:            250,
				FirstHash:              "1111111111111111111111111111111111111111111111111111111111111111",
				LastHash:               "2222222222222222222222222222222222222222222222222222222222222222",
				ExpectedNextHash:       &successor,
				SignedAtUnixNano:       1788000000123456789,
				SigningKeyID:           "key-1",
				Algorithm:              "ECDSA_P256_SHA256",
				CanonicalSchemaVersion: contracts.CheckpointCanonicalSchemaVersion,
			},
		},
		{
			// Sealing at the head is the common case, and it is the one where a
			// serializer is most tempted to write "" instead of null.
			name: "sealed at the head, successor absent",
			body: contracts.Checkpoint{
				CheckpointID:           "ckp_01hq0000000000000000000002",
				PartitionKey:           "2026-09",
				FirstSequence:          1,
				LastSequence:           250,
				RecordCount:            250,
				FirstHash:              "1111111111111111111111111111111111111111111111111111111111111111",
				LastHash:               "2222222222222222222222222222222222222222222222222222222222222222",
				ExpectedNextHash:       nil,
				SignedAtUnixNano:       1788000001123456789,
				SigningKeyID:           "key-1",
				Algorithm:              "ECDSA_P256_SHA256",
				CanonicalSchemaVersion: contracts.CheckpointCanonicalSchemaVersion,
			},
		},
		{
			// Characters that Go's JSON encoder escapes by default and
			// PostgreSQL's to_json does not. With escaping left on, the two
			// implementations would disagree on any key reference containing
			// one of them.
			name: "algorithm and key reference containing markup characters",
			body: contracts.Checkpoint{
				CheckpointID:           "ckp_01hq0000000000000000000003",
				PartitionKey:           "2026-09",
				FirstSequence:          0,
				LastSequence:           0,
				RecordCount:            1,
				FirstHash:              "4444444444444444444444444444444444444444444444444444444444444444",
				LastHash:               "5555555555555555555555555555555555555555555555555555555555555555",
				ExpectedNextHash:       nil,
				SignedAtUnixNano:       0,
				SigningKeyID:           "kms://eu-west-1/a&b<c>\"d\"",
				Algorithm:              "ED25519",
				CanonicalSchemaVersion: contracts.CheckpointCanonicalSchemaVersion,
			},
		},
	}
}

func TestCanonicalCheckpointBodyMatchesDatabaseImplementation(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		for _, tc := range checkpointBodyCases() {
			t.Run(tc.name, func(t *testing.T) {
				b := tc.body
				var sqlBody string
				dbtest.MustQueryRow(t, ctx, tx, &sqlBody, `
                    SELECT audit.canonical_checkpoint_body(
                             $1::text, $2::text, $3::bigint, $4::bigint, $5::bigint,
                             $6::text, $7::text, $8::text, $9::bigint, $10::text,
                             $11::text, $12::text)`,
					b.CheckpointID, b.PartitionKey, b.FirstSequence, b.LastSequence, b.RecordCount,
					b.FirstHash, b.LastHash, b.ExpectedNextHash, b.SignedAtUnixNano,
					b.SigningKeyID, b.Algorithm, b.CanonicalSchemaVersion)

				goBody := contracts.CanonicalCheckpointBody(b)
				if goBody != sqlBody {
					t.Fatalf("canonical checkpoint body differs between Go and SQL\n  go:  %s\n  sql: %s", goBody, sqlBody)
				}
			})
		}
	})
}

// TestCanonicalCheckpointBodyBindsTheSealedRange proves the body commits to the
// range it seals, which is what stops a signature being transplanted from one
// batch onto another. Every member that identifies the batch must change the
// body; a body that omitted the hashes would let a valid signature be
// presented for a different range.
func TestCanonicalCheckpointBodyBindsTheSealedRange(t *testing.T) {
	base := checkpointBodyCases()[0].body
	reference := contracts.CanonicalCheckpointBody(base)

	variants := map[string]func(b *contracts.Checkpoint){
		"first_hash":        func(b *contracts.Checkpoint) { b.FirstHash = "9" + b.FirstHash[1:] },
		"last_hash":         func(b *contracts.Checkpoint) { b.LastHash = "9" + b.LastHash[1:] },
		"first_sequence":    func(b *contracts.Checkpoint) { b.FirstSequence++ },
		"last_sequence":     func(b *contracts.Checkpoint) { b.LastSequence++ },
		"record_count":      func(b *contracts.Checkpoint) { b.RecordCount++ },
		"partition_key":     func(b *contracts.Checkpoint) { b.PartitionKey = "2026-10" },
		"signing_key_id":    func(b *contracts.Checkpoint) { b.SigningKeyID = "key-2" },
		"algorithm":         func(b *contracts.Checkpoint) { b.Algorithm = "ED25519" },
		"signed_at_ns":      func(b *contracts.Checkpoint) { b.SignedAtUnixNano++ },
		"checkpoint_id":     func(b *contracts.Checkpoint) { b.CheckpointID = "ckp_other" },
		"successor removed": func(b *contracts.Checkpoint) { b.ExpectedNextHash = nil },
	}
	for name, mutate := range variants {
		mutated := base
		mutate(&mutated)
		if contracts.CanonicalCheckpointBody(mutated) == reference {
			t.Errorf("changing %s did not change the signed body; a signature could be transplanted across it", name)
		}
	}
}

// TestSchemaVersionIsInsideTheSignedBody guards the upgrade path. If the
// schema version were not part of the body, a future change to the member order
// would silently change the meaning of every historical signature, and nothing
// would record which meaning a given signature was produced under.
func TestSchemaVersionIsInsideTheSignedBody(t *testing.T) {
	b := checkpointBodyCases()[0].body
	before := contracts.CanonicalCheckpointBody(b)
	b.CanonicalSchemaVersion = "2.0.0"
	after := contracts.CanonicalCheckpointBody(b)
	if before == after {
		t.Fatal("the canonical schema version is not part of the signed body")
	}
	if !strings.Contains(after, `"canonical_schema_version":"2.0.0"`) {
		t.Fatalf("the body does not carry the version verbatim: %s", after)
	}
}
