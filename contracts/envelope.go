package contracts

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SchemaVersion identifies the canonical contract version of a payload. It is
// carried on every command and event so that a consumer can reject an
// unsupported version instead of guessing at its meaning
// (03_CANONICAL_CONTRACTS.md, "Compatibility").
type SchemaVersion string

// CurrentSchemaVersion is the version emitted by this implementation. It is
// advanced only through a coordinated migration with dual-read/dual-write
// behaviour.
const CurrentSchemaVersion SchemaVersion = "1.0.0"

// supportedSchemaVersions is the closed set this build accepts. A version
// outside the set is unsupported, not silently mapped.
var supportedSchemaVersions = map[SchemaVersion]struct{}{
	"1.0.0": {},
}

// SupportedSchemaVersion reports whether this build can process the version.
func (v SchemaVersion) SupportedSchemaVersion() bool {
	_, ok := supportedSchemaVersions[v]
	return ok
}

// Validate rejects an unknown schema version.
func (v SchemaVersion) Validate() error {
	if !v.SupportedSchemaVersion() {
		return fmt.Errorf("%w: unsupported schema version %q (supported: %v)",
			ErrInvalidIdentifier, string(v), CanonicalSchemaVersions())
	}
	return nil
}

// CanonicalSchemaVersions returns the accepted set in stable order.
func CanonicalSchemaVersions() []SchemaVersion {
	return []SchemaVersion{"1.0.0"}
}

// ---------------------------------------------------------------------------
// Command envelope
// ---------------------------------------------------------------------------

// CommandStatus is the outcome classification of a command response
// (15_API_AND_INTEGRATION_CONTRACTS.md §3).
type CommandStatus string

const (
	// CommandAccepted means the platform durably accepted the command. It does
	// not mean a venue accepted, filled or acknowledged anything.
	CommandAccepted CommandStatus = "accepted"
	// CommandRejected means the command will not take effect.
	CommandRejected CommandStatus = "rejected"
	// CommandOutcomeUnknown means a timeout prevented proving whether a
	// downstream side effect occurred. The client must query canonical state
	// and must not retry with a new idempotency key.
	CommandOutcomeUnknown CommandStatus = "outcome_unknown"
)

// CommandEnvelope is the canonical wrapper for every externally initiated
// mutating request. Field names and requiredness are normative
// (03_CANONICAL_CONTRACTS.md, "Command envelope").
type CommandEnvelope struct {
	CommandID     ID            `json:"command_id"`
	CommandType   string        `json:"command_type"`
	SchemaVersion SchemaVersion `json:"schema_version"`
	CorrelationID string        `json:"correlation_id"`
	CausationID   string        `json:"causation_id"`
	ActorID       string        `json:"actor_id"`
	ActorType     ActorType     `json:"actor_type"`
	Environment   Environment   `json:"environment"`
	RequestedAt   Timestamp     `json:"requested_at"`
	// IdempotencyKey scopes replay suppression to actor + endpoint + environment.
	IdempotencyKey string `json:"idempotency_key"`
	// Payload is the command-specific body. It is carried as raw JSON so that
	// the envelope can be persisted and replayed without decoding a payload
	// type the dispatcher does not own.
	Payload json.RawMessage `json:"payload"`
}

// MaxCommandPayloadBytes is the binding command payload limit
// (23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §7). Routes may set a lower
// bound; a higher bound is not permitted.
const MaxCommandPayloadBytes = 64 * 1024

// MaxRequestBodyBytes is the binding total request body limit.
const MaxRequestBodyBytes = 1024 * 1024

// Validate enforces every required field and the payload size limit.
func (c CommandEnvelope) Validate() error {
	if !c.CommandID.Valid() {
		return fmt.Errorf("%w: command_id", ErrInvalidIdentifier)
	}
	if c.CommandID.Entity != EntityCommand {
		return fmt.Errorf("%w: command_id prefix must be %q, got %q",
			ErrInvalidIdentifier, string(EntityCommand), string(c.CommandID.Entity))
	}
	if strings.TrimSpace(c.CommandType) == "" {
		return fmt.Errorf("%w: command_type is required", ErrInvalidIdentifier)
	}
	if err := c.SchemaVersion.Validate(); err != nil {
		return err
	}
	if err := validateTraceID("correlation_id", c.CorrelationID, true); err != nil {
		return err
	}
	if err := validateTraceID("causation_id", c.CausationID, false); err != nil {
		return err
	}
	if strings.TrimSpace(c.ActorID) == "" {
		return fmt.Errorf("%w: actor_id is required", ErrInvalidIdentifier)
	}
	if !c.ActorType.Known() {
		return fmt.Errorf("%w: unknown actor_type %q", ErrInvalidIdentifier, string(c.ActorType))
	}
	if err := c.Environment.Validate(); err != nil {
		return err
	}
	if c.RequestedAt.IsZero() {
		return fmt.Errorf("%w: requested_at", ErrMissingTimestamp)
	}
	if strings.TrimSpace(c.IdempotencyKey) == "" {
		return fmt.Errorf("%w: idempotency_key is required for every mutating command", ErrInvalidIdentifier)
	}
	if len(c.Payload) > MaxCommandPayloadBytes {
		return fmt.Errorf("%w: payload %d bytes exceeds the %d byte command limit",
			ErrValidation, len(c.Payload), MaxCommandPayloadBytes)
	}
	if !json.Valid(c.Payload) {
		return fmt.Errorf("%w: payload is not valid JSON", ErrValidation)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Event envelope
// ---------------------------------------------------------------------------

// EventEnvelope is the canonical wrapper for every durable domain event
// (03_CANONICAL_CONTRACTS.md, "Event envelope").
type EventEnvelope struct {
	EventID       ID            `json:"event_id"`
	EventType     string        `json:"event_type"`
	SchemaVersion SchemaVersion `json:"schema_version"`
	AggregateType string        `json:"aggregate_type"`
	AggregateID   string        `json:"aggregate_id"`
	CorrelationID string        `json:"correlation_id"`
	CausationID   string        `json:"causation_id"`
	ProducerID    string        `json:"producer_id"`
	OccurredAt    Timestamp     `json:"occurred_at"`
	RecordedAt    Timestamp     `json:"recorded_at"`
	Sequence      int64         `json:"sequence"`
	// SourceTimestamp is present when the event originates from an external
	// venue or provider.
	SourceTimestamp *Timestamp      `json:"source_timestamp,omitempty"`
	Payload         json.RawMessage `json:"payload"`
}

// Validate enforces every required field. A non-positive Sequence is rejected:
// a missing or invalid sequence number is routed to controlled reconciliation
// rather than accepted (01_SYSTEM_ARCHITECTURE.md §8).
func (e EventEnvelope) Validate(requireSource bool) error {
	if !e.EventID.Valid() {
		return fmt.Errorf("%w: event_id", ErrInvalidIdentifier)
	}
	if e.EventID.Entity != EntityEvent {
		return fmt.Errorf("%w: event_id prefix must be %q", ErrInvalidIdentifier, string(EntityEvent))
	}
	if strings.TrimSpace(e.EventType) == "" {
		return fmt.Errorf("%w: event_type is required", ErrInvalidIdentifier)
	}
	if err := e.SchemaVersion.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(e.AggregateType) == "" {
		return fmt.Errorf("%w: aggregate_type is required", ErrInvalidIdentifier)
	}
	if strings.TrimSpace(e.AggregateID) == "" {
		return fmt.Errorf("%w: aggregate_id is required", ErrInvalidIdentifier)
	}
	if err := validateTraceID("correlation_id", e.CorrelationID, true); err != nil {
		return err
	}
	if err := validateTraceID("causation_id", e.CausationID, false); err != nil {
		return err
	}
	if strings.TrimSpace(e.ProducerID) == "" {
		return fmt.Errorf("%w: producer_id is required", ErrInvalidIdentifier)
	}
	times := EventTimes{
		OccurredAt:      e.OccurredAt,
		RecordedAt:      e.RecordedAt,
		SourceTimestamp: e.SourceTimestamp,
	}
	if err := times.Validate(requireSource); err != nil {
		return err
	}
	if e.Sequence <= 0 {
		return fmt.Errorf("%w: sequence must be a positive monotonic value, got %d",
			ErrInvalidIdentifier, e.Sequence)
	}
	if !json.Valid(e.Payload) {
		return fmt.Errorf("%w: payload is not valid JSON", ErrValidation)
	}
	return nil
}

// TraceContext propagates correlation and causation across component
// boundaries (08_NFR_OBSERVABILITY_AND_CAPACITY.md, "Observability").
type TraceContext struct {
	CorrelationID string
	CausationID   string
}

// NewTrace derives a child trace from a parent command, using the parent's
// command identifier as the causation identifier.
func NewTrace(correlationID, commandID string) (TraceContext, error) {
	if strings.TrimSpace(correlationID) == "" {
		return TraceContext{}, fmt.Errorf("%w: correlation_id is required", ErrInvalidIdentifier)
	}
	if strings.TrimSpace(commandID) == "" {
		return TraceContext{}, fmt.Errorf("%w: causation_id is required", ErrInvalidIdentifier)
	}
	return TraceContext{CorrelationID: correlationID, CausationID: commandID}, nil
}

const maxTraceIDLen = 128

// validateTraceID enforces a bounded, printable identifier. Trace identifiers
// are attacker-controlled input and are recorded in audit records, so they are
// length-bounded and restricted to a safe character set to prevent log
// injection and unbounded storage growth in the audit chain.
func validateTraceID(field, v string, required bool) error {
	if strings.TrimSpace(v) == "" {
		if required {
			return fmt.Errorf("%w: %s is required", ErrInvalidIdentifier, field)
		}
		return nil
	}
	if len(v) > maxTraceIDLen {
		return fmt.Errorf("%w: %s exceeds %d characters", ErrValidation, field, maxTraceIDLen)
	}
	for i := 0; i < len(v); i++ {
		ch := v[i]
		ok := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == ':' || ch == '.'
		if !ok {
			return fmt.Errorf("%w: %s contains an unsupported character %q", ErrValidation, field, string(ch))
		}
	}
	return nil
}
