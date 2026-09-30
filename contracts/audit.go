// Canonical audit record serialisation and hashing.
//
// Authority: 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3:
//
//	record_hash = SHA-256(canonical_json(record_without_record_hash_and_signature));
//	previous_hash references the prior record hash in that partition.
//	Canonical serialization and schema version are fixed and tested.
//
// A hash chain whose hashes are supplied by the party being audited is not
// tamper-evident, so the database is the authority for record_hash: see
// db/migrations/0009_audit_canonical_hash.sql, where audit.append_record computes
// the hash and rejects any caller-supplied value that disagrees. This file is the
// second, independent implementation of the same serialisation. The two are
// compared byte-for-byte by dbtest/audit_canonical_test.go, which calls
// audit.canonical_payload in the database and requires the strings to be equal.
//
// Two independent implementations that must agree on every byte is a stronger
// control than either one alone, so the divergence risk is caught at test time
// rather than during a post-incident chain walk.
package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

// AuditCanonicalSchemaVersion is the version of the canonical serialisation
// implemented by this file. It is part of the hashed payload, so changing the
// member order, the set of members, or the encoding rules REQUIRES incrementing
// this value; old records keep their old hash meaning forever.
const AuditCanonicalSchemaVersion = "1.0.0"

// AuditGenesisHash is the previous_hash of the first record in a partition. A
// distinct constant, not NULL and not empty, so that "first record" and "record
// whose predecessor is missing" are not the same value.
const AuditGenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// AuditResult is the closed result vocabulary of an audit record. It mirrors
// audit.result_value in db/migrations/0006_audit_integrity.sql.
type AuditResult string

const (
	AuditResultSuccess AuditResult = "SUCCESS"
	AuditResultFailure AuditResult = "FAILURE"
	AuditResultDenied  AuditResult = "DENIED"
	AuditResultUnknown AuditResult = "UNKNOWN"
)

// AuditRecord is the subset of an audit record that the hash covers.
//
// DetailsDigest, not Details, appears in the canonical payload. The free-form
// details object is hashed by the database and carried as its digest, which
// protects the content exactly as well while keeping the canonical form flat and
// therefore reproducible without depending on any implementation's key-ordering
// rules. The digest is part of the hash, so details are still covered.
type AuditRecord struct {
	AuditID                string
	PartitionKey           string
	Sequence               int64
	TenantOrOwnerScope     string
	ActorID                string
	ActorType              ActorType
	Action                 string
	TargetType             string
	TargetID               string
	Environment            Environment
	MarketScope            *MarketClass
	OccurredAtUnixNano     int64
	RecordedAtUnixNano     int64
	Reason                 *string
	CorrelationID          string
	CausationID            *string
	PolicyVersion          string
	Result                 AuditResult
	BeforeDigest           *string
	AfterDigest            *string
	DetailsDigest          string
	PreviousHash           string
	CanonicalSchemaVersion string
}

// jsonString encodes s the way PostgreSQL's to_json() encodes a text value.
//
// The encoder's HTML escaping is disabled deliberately. Go escapes <, > and &
// to <, > and & by default, which PostgreSQL does not. With escaping
// on, a record whose Reason contained an ampersand would hash differently in Go
// and in the database, and the cross-check in audit.append_record would reject
// legitimate records. The trailing newline Encoder writes is trimmed because the
// canonical form is defined to have no insignificant trailing whitespace.
func jsonString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// json.Encoder.Encode only fails when the writer fails, and a
		// bytes.Buffer never does. Panicking here would be unreachable.
		panic("contracts: encoding an audit string to JSON failed: " + err.Error())
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}

// jsonNullableString encodes s, or the JSON literal null when s is nil.
func jsonNullableString(s *string) string {
	if s == nil {
		return "null"
	}
	return jsonString(*s)
}

// jsonNullableMarketScope encodes m, or the JSON literal null when m is nil.
// market_scope is nullable because a non-market event (an authentication
// outcome, a configuration change) has no market scope, and representing that as
// the empty string would conflate "no market" with a malformed value.
func jsonNullableMarketScope(m *MarketClass) string {
	if m == nil {
		return "null"
	}
	return jsonString(string(*m))
}

// CanonicalAuditPayload returns the canonical serialisation of an audit record
// excluding record_hash and signature, as required by §3.
//
// The form is a FLAT JSON object with a fixed member order and no insignificant
// whitespace. It is flat on purpose: every member is a JSON string, a JSON
// integer, or null, so there are no nested objects whose key order could depend
// on the serialiser. The member order below is part of the contract and is
// mirrored exactly by audit.canonical_payload in
// db/migrations/0009_audit_canonical_hash.sql.
func CanonicalAuditPayload(r AuditRecord) string {
	var b bytes.Buffer
	b.WriteString(`{"audit_id":`)
	b.WriteString(jsonString(r.AuditID))
	b.WriteString(`,"partition_key":`)
	b.WriteString(jsonString(r.PartitionKey))
	b.WriteString(`,"sequence":`)
	b.WriteString(strconv.FormatInt(r.Sequence, 10))
	b.WriteString(`,"tenant_or_owner_scope":`)
	b.WriteString(jsonString(r.TenantOrOwnerScope))
	b.WriteString(`,"actor_id":`)
	b.WriteString(jsonString(r.ActorID))
	b.WriteString(`,"actor_type":`)
	b.WriteString(jsonString(string(r.ActorType)))
	b.WriteString(`,"action":`)
	b.WriteString(jsonString(r.Action))
	b.WriteString(`,"target_type":`)
	b.WriteString(jsonString(r.TargetType))
	b.WriteString(`,"target_id":`)
	b.WriteString(jsonString(r.TargetID))
	b.WriteString(`,"environment":`)
	b.WriteString(jsonString(string(r.Environment)))
	b.WriteString(`,"market_scope":`)
	b.WriteString(jsonNullableMarketScope(r.MarketScope))
	b.WriteString(`,"occurred_at_ns":`)
	b.WriteString(strconv.FormatInt(r.OccurredAtUnixNano, 10))
	b.WriteString(`,"recorded_at_ns":`)
	b.WriteString(strconv.FormatInt(r.RecordedAtUnixNano, 10))
	b.WriteString(`,"reason":`)
	b.WriteString(jsonNullableString(r.Reason))
	b.WriteString(`,"correlation_id":`)
	b.WriteString(jsonString(r.CorrelationID))
	b.WriteString(`,"causation_id":`)
	b.WriteString(jsonNullableString(r.CausationID))
	b.WriteString(`,"policy_version":`)
	b.WriteString(jsonString(r.PolicyVersion))
	b.WriteString(`,"result":`)
	b.WriteString(jsonString(string(r.Result)))
	b.WriteString(`,"before_digest":`)
	b.WriteString(jsonNullableString(r.BeforeDigest))
	b.WriteString(`,"after_digest":`)
	b.WriteString(jsonNullableString(r.AfterDigest))
	b.WriteString(`,"details_digest":`)
	b.WriteString(jsonString(r.DetailsDigest))
	b.WriteString(`,"previous_hash":`)
	b.WriteString(jsonString(r.PreviousHash))
	b.WriteString(`,"canonical_schema_version":`)
	b.WriteString(jsonString(r.CanonicalSchemaVersion))
	b.WriteString(`}`)
	return b.String()
}

// AuditRecordHash returns the lowercase hex SHA-256 of the canonical payload.
//
// The parameter is named hash rather than digest to make the recursion
// impossible to misread at the call site: the record supplies the predecessor's
// hash through PreviousHash, and this function returns this record's hash.
func AuditRecordHash(r AuditRecord) (string, error) {
	if r.PreviousHash == "" {
		return "", fmt.Errorf("contracts: audit record %s: previous_hash is required and must be the genesis hash for the first record in a partition", r.AuditID)
	}
	if r.DetailsDigest == "" {
		return "", fmt.Errorf("contracts: audit record %s: details_digest is required", r.AuditID)
	}
	sum := sha256.Sum256([]byte(CanonicalAuditPayload(r)))
	return hex.EncodeToString(sum[:]), nil
}
