// Package contracts implements the canonical contract foundation defined by
// 03_CANONICAL_CONTRACTS.md. It is the single authoritative representation of
// identifiers, decimal money/quantity/price, UTC timestamps, the error
// taxonomy, command/event envelopes and idempotency keys.
//
// Authority boundary (01_SYSTEM_ARCHITECTURE.md §7, 25_..._PROFILE.md §4):
// this package is the only place where canonical representations are defined
// for the Go control plane. TypeScript and Python consumers MUST NOT redefine
// these structures; they are generated or validated from the schemas in
// contracts/schema against the fixtures in contracts/testdata.
//
// No IEEE-754 floating point value crosses a financial contract.
package contracts

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// EntityType is the typed prefix of a canonical identifier. The set is closed;
// an unknown prefix is an unsupported value and must never be silently mapped
// (03_CANONICAL_CONTRACTS.md, "Compatibility").
type EntityType string

const (
	EntityOrder        EntityType = "ord"
	EntityEvent        EntityType = "evt"
	EntityCommand      EntityType = "cmd"
	EntityStrategy     EntityType = "str"
	EntityModel        EntityType = "mdl"
	EntityRun          EntityType = "run"
	EntityPosition     EntityType = "pos"
	EntityLedgerEntry  EntityType = "led"
	EntityAccount      EntityType = "acc"
	EntityInstrument   EntityType = "ins"
	EntityVenue        EntityType = "ven"
	EntityFill         EntityType = "fil"
	EntityDataset      EntityType = "dst"
	EntityExperiment   EntityType = "exp"
	EntityArtifact     EntityType = "art"
	EntityAgent        EntityType = "agt"
	EntityApproval     EntityType = "apr"
	EntityReconcilCase EntityType = "rec"
	EntityHalt         EntityType = "hlt"
	EntityAudit        EntityType = "aud"
	EntityConfigRev    EntityType = "cfg"
	EntityPolicyRev    EntityType = "pol"
	EntitySession      EntityType = "ses"
	EntityIncident     EntityType = "inc"
	EntityFeatureFlag  EntityType = "flt"
	EntityRiskDecision EntityType = "rsk"
	EntityDataQuality  EntityType = "dqt"
	EntityEligibility  EntityType = "elg"
	EntityMarketData   EntityType = "mkt"
)

// knownEntities maps the closed entity-type set.
var knownEntities = map[EntityType]struct{}{
	EntityOrder: {}, EntityEvent: {}, EntityCommand: {}, EntityStrategy: {},
	EntityModel: {}, EntityRun: {}, EntityPosition: {}, EntityLedgerEntry: {},
	EntityAccount: {}, EntityInstrument: {}, EntityVenue: {}, EntityFill: {},
	EntityDataset: {}, EntityExperiment: {}, EntityArtifact: {}, EntityAgent: {},
	EntityApproval: {}, EntityReconcilCase: {}, EntityHalt: {}, EntityAudit: {},
	EntityConfigRev: {}, EntityPolicyRev: {}, EntitySession: {}, EntityIncident: {},
	EntityFeatureFlag: {}, EntityRiskDecision: {}, EntityDataQuality: {},
	EntityEligibility: {}, EntityMarketData: {},
}

// EntityTypes returns the closed entity-type set in a stable, sorted order.
//
// It exists so that the published JSON Schema and this package can be required
// to declare the same set. The parity test in contracts/fixtures_test.go uses
// it, which is how a schema value with no counterpart here -- "cqt" was such a
// value -- is caught instead of sitting unnoticed in a published contract.
func EntityTypes() []EntityType {
	out := make([]EntityType, 0, len(knownEntities))
	for t := range knownEntities {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

var (
	// ErrInvalidIdentifier is returned when an identifier does not satisfy the
	// canonical format: typed prefix + lowercase Crockford Base32 payload.
	ErrInvalidIdentifier = errors.New("contracts: invalid canonical identifier")
	// ErrUnknownEntityType is returned for a prefix outside the closed set.
	ErrUnknownEntityType = errors.New("contracts: unknown entity type")
)

// crockfordLower is the lowercase Crockford Base32 alphabet. Crockford Base32
// excludes I, L, O and U to remove human transcription ambiguity; the canonical
// representation is lowercase (03_CANONICAL_CONTRACTS.md).
const crockfordLower = "0123456789abcdefghjkmnpqrstvwxyz"

// crockfordValue maps a character to its 5-bit value; also accepts the four
// excluded letters and normalises them to the visually similar canonical
// character on decode tolerance (decode only — encoding never emits them).
var crockfordValue = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(crockfordLower); i++ {
		t[crockfordLower[i]] = int8(i)
	}
	// Human-confusion tolerance on decode only.
	t['i'] = 1
	t['I'] = 1
	t['l'] = 1
	t['L'] = 1
	t['o'] = 0
	t['O'] = 0
	t['u'] = 28
	t['U'] = 28
	return t
}()

// IDLength is the number of Crockford Base32 payload characters (20 chars =
// 100 bits of entropy). Fixed length keeps identifiers non-enumerable and
// uniform across entity types while the prefix carries the type.
const IDLength = 20

// ID is a canonical typed identifier: "<prefix>_<20 lowercase Crockford chars>".
type ID struct {
	Entity EntityType
	// raw stores the original lowercased canonical string so that round-trip
	// serialisation is byte-stable (required for audit record hashing).
	raw string
}

// String returns the canonical textual form. The result is immutable.
func (i ID) String() string { return i.raw }

// IsZero reports whether the identifier is unset.
func (i ID) IsZero() bool { return i.raw == "" }

// Valid reports whether the identifier satisfies the canonical format.
func (i ID) Valid() bool { _, err := ParseID(i.raw); return err == nil }

// MarshalText implements encoding.TextMarshaler so identifiers serialise as
// bare strings rather than objects in every language binding.
func (i ID) MarshalText() ([]byte, error) {
	if !i.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidIdentifier, i.raw)
	}
	return []byte(i.raw), nil
}

// UnmarshalText implements encoding.TextUnmarshaler with full validation.
func (i *ID) UnmarshalText(b []byte) error {
	parsed, err := ParseID(string(b))
	if err != nil {
		return err
	}
	*i = parsed
	return nil
}

// NewID returns a new identifier of the given entity type backed by 100 bits of
// cryptographically secure randomness. It returns an error for an unknown type
// so that a typo in a type constant fails loudly rather than minting an
// identifier that no consumer can recognise.
func NewID(t EntityType) (ID, error) {
	if _, ok := knownEntities[t]; !ok {
		return ID{}, fmt.Errorf("%w: %q", ErrUnknownEntityType, string(t))
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ID{}, fmt.Errorf("contracts: entropy source failure: %w", err)
	}
	return ID{Entity: t, raw: string(t) + "_" + encodeCrockford(buf[:], IDLength)}, nil
}

// MustID is NewID for contexts where the caller cannot handle entropy failure.
// It panics rather than returning a zero value, because a zero identifier in an
// authoritative path is worse than an unavailable process.
func MustID(t EntityType) ID {
	id, err := NewID(t)
	if err != nil {
		panic(err)
	}
	return id
}

// ParseID validates and parses a canonical identifier string.
func ParseID(s string) (ID, error) {
	sep := strings.IndexByte(s, '_')
	if sep < 1 {
		return ID{}, fmt.Errorf("%w: %q has no prefix separator", ErrInvalidIdentifier, s)
	}
	prefix := EntityType(s[:sep])
	if _, ok := knownEntities[prefix]; !ok {
		return ID{}, fmt.Errorf("%w: %q", ErrUnknownEntityType, string(prefix))
	}
	if len(s) != sep+1+IDLength {
		return ID{}, fmt.Errorf("%w: %q has length %d, expected %d",
			ErrInvalidIdentifier, s, len(s), sep+1+IDLength)
	}
	payload := s[sep+1:]
	for i := 0; i < len(payload); i++ {
		if payload[i] >= 'A' && payload[i] <= 'Z' {
			return ID{}, fmt.Errorf("%w: %q payload must be lowercase", ErrInvalidIdentifier, s)
		}
		if crockfordValue[payload[i]] < 0 {
			return ID{}, fmt.Errorf("%w: %q payload is not Crockford Base32", ErrInvalidIdentifier, s)
		}
	}
	return ID{Entity: prefix, raw: s}, nil
}

// encodeCrockford renders the low-order big-endian bits of src as n lowercase
// Crockford Base32 characters. Bits above the requested width are discarded
// without bias because src is uniformly random and 100 bits divides 128 evenly.
func encodeCrockford(src []byte, n int) string {
	// Accumulate into a big-endian bit stream.
	bits := make([]byte, 0, len(src)*8)
	for _, b := range src {
		for i := 7; i >= 0; i-- {
			bits = append(bits, (b>>uint(i))&1)
		}
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		var v int8
		for j := 0; j < 5; j++ {
			v <<= 1
			if i*5+j < len(bits) {
				v |= int8(bits[i*5+j])
			}
		}
		out[i] = crockfordLower[v]
	}
	return string(out)
}
