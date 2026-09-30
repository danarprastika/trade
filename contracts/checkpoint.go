package contracts

import (
	"bytes"
	"strconv"
)

// Canonical audit checkpoint body.
//
// Authority: 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3 -- "Every batch is closed
// with a signed checkpoint containing partition, sequence range, first/last
// hash, count, timestamp, and signing key ID. Checkpoints are signed by a
// managed KMS/HSM key. Canonical serialization and schema version are fixed and
// tested."
//
// The signature covers exactly the bytes this function returns. Defining them
// in two places and requiring them to agree is the same discipline applied to
// the audit record payload in audit.go: the specification is fixed once, and two
// independent implementations must produce identical bytes or a test fails.
//
// If the two disagree, a correctly signed checkpoint will fail to verify
// against the stored body, or a body will be signed that does not describe what
// was committed. Both are the same class of failure -- the signature becomes
// unverifiable -- and both are caught here rather than during an audit.

// CheckpointCanonicalSchemaVersion is the version of the checkpoint body
// serialisation. It is inside the signed bytes, so changing the member order or
// encoding requires incrementing it, and old checkpoints keep their old meaning.
const CheckpointCanonicalSchemaVersion = "1.0.0"

// Checkpoint is the content a checkpoint signature covers.
type Checkpoint struct {
	CheckpointID           string
	PartitionKey           string
	FirstSequence          int64
	LastSequence           int64
	RecordCount            int64
	FirstHash              string
	LastHash               string
	ExpectedNextHash       *string
	SignedAtUnixNano       int64
	SigningKeyID           string
	Algorithm              string
	CanonicalSchemaVersion string
}

// CanonicalCheckpointBody returns the exact bytes a checkpoint signature covers.
//
// Flat JSON, fixed member order, no insignificant whitespace. It includes the
// sealed range (first_hash, last_hash, expected_next_hash), so a signature
// cannot be transplanted from one batch onto another, and it includes
// signing_key_id and algorithm, so a signature is bound to the key that produced
// it.
//
// Mirrors audit.canonical_checkpoint_body in
// db/migrations/0012_checkpoint_key_binding.sql.
func CanonicalCheckpointBody(c Checkpoint) string {
	var b bytes.Buffer
	b.WriteString(`{"checkpoint_id":`)
	b.WriteString(jsonString(c.CheckpointID))
	b.WriteString(`,"partition_key":`)
	b.WriteString(jsonString(c.PartitionKey))
	b.WriteString(`,"first_sequence":`)
	b.WriteString(strconv.FormatInt(c.FirstSequence, 10))
	b.WriteString(`,"last_sequence":`)
	b.WriteString(strconv.FormatInt(c.LastSequence, 10))
	b.WriteString(`,"record_count":`)
	b.WriteString(strconv.FormatInt(c.RecordCount, 10))
	b.WriteString(`,"first_hash":`)
	b.WriteString(jsonString(c.FirstHash))
	b.WriteString(`,"last_hash":`)
	b.WriteString(jsonString(c.LastHash))
	b.WriteString(`,"expected_next_hash":`)
	b.WriteString(jsonNullableString(c.ExpectedNextHash))
	b.WriteString(`,"signed_at_ns":`)
	b.WriteString(strconv.FormatInt(c.SignedAtUnixNano, 10))
	b.WriteString(`,"signing_key_id":`)
	b.WriteString(jsonString(c.SigningKeyID))
	b.WriteString(`,"algorithm":`)
	b.WriteString(jsonString(c.Algorithm))
	b.WriteString(`,"canonical_schema_version":`)
	b.WriteString(jsonString(c.CanonicalSchemaVersion))
	b.WriteString(`}`)
	return b.String()
}
