package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Idempotency
//
// 03_CANONICAL_CONTRACTS.md, "Idempotency": every externally initiated mutating
// command requires an idempotency key scoped to actor, endpoint and
// environment. Replays return the original command result. Database uniqueness
// constraints enforce idempotency; application memory is never sufficient.
// ---------------------------------------------------------------------------

// IdempotencyScope is the composite uniqueness key of the durable idempotency
// record. It is enforced by a PostgreSQL unique constraint on
// idempotency_record(scope_hash).
type IdempotencyScope struct {
	// ActorID is the platform subject or workload principal.
	ActorID string
	// Endpoint is the logical command endpoint (method + route template), not
	// the full URL, so that a query-string change cannot bypass replay
	// suppression.
	Endpoint string
	// Environment is the deployment scope; a command is never idempotent across
	// environments.
	Environment Environment
	// Key is the client-supplied Idempotency-Key.
	Key string
}

// idempotencyKeyPattern bounds the client-supplied key: printable ASCII without
// whitespace, 8 to 128 characters. Restricting the character set keeps the
// scope hash stable and prevents a caller from smuggling control characters
// into the audit trail.
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:\-]{8,128}$`)

// IdempotencyState is the lifecycle of a durable idempotency record.
type IdempotencyState string

const (
	// IdempotencyInFlight means a first execution claimed the key and has not
	// yet durably recorded a terminal result. A concurrent replay observes this
	// state and receives CONFLICT rather than a duplicate execution.
	IdempotencyInFlight IdempotencyState = "in_flight"
	// IdempotencySucceeded means a terminal result is recorded and replays
	// return it verbatim.
	IdempotencySucceeded IdempotencyState = "succeeded"
	// IdempotencyFailed means a terminal rejection is recorded. Replays return
	// the same rejection so that an operator cannot retry into a different
	// outcome by resubmitting the same key.
	IdempotencyFailed IdempotencyState = "failed"
	// IdempotencyUnknown means the command's downstream outcome could not be
	// proven. A replay returns outcome_unknown and must not re-execute.
	IdempotencyUnknown IdempotencyState = "unknown"
)

// Terminal reports whether the state carries a durable terminal result.
func (s IdempotencyState) Terminal() bool {
	switch s {
	case IdempotencySucceeded, IdempotencyFailed, IdempotencyUnknown:
		return true
	}
	return false
}

// ValidateScope enforces the key format and scope completeness.
func (s IdempotencyScope) Validate() error {
	if strings.TrimSpace(s.ActorID) == "" {
		return fmt.Errorf("%w: idempotency scope requires actor_id", ErrInvalidIdentifier)
	}
	if strings.TrimSpace(s.Endpoint) == "" {
		return fmt.Errorf("%w: idempotency scope requires endpoint", ErrInvalidIdentifier)
	}
	if err := s.Environment.Validate(); err != nil {
		return err
	}
	if !idempotencyKeyPattern.MatchString(s.Key) {
		return fmt.Errorf("%w: idempotency key must match %s", ErrValidation, idempotencyKeyPattern.String())
	}
	return nil
}

// Hash returns the stable SHA-256 hex digest of the canonical scope string. The
// canonical form is length-prefixed field concatenation, so that no combination
// of field values can produce the same pre-image by shifting a separator
// (for example actor "a" + endpoint "bc" versus actor "ab" + endpoint "c").
func (s IdempotencyScope) Hash() string {
	var b strings.Builder
	writePart := func(v string) {
		fmt.Fprintf(&b, "%d:", len(v))
		b.WriteString(v)
		b.WriteByte('|')
	}
	writePart(s.ActorID)
	writePart(s.Endpoint)
	writePart(string(s.Environment))
	writePart(s.Key)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// IdempotencyRecord is the durable replay-suppression record. The RequestDigest
// binds the key to a specific request body: replaying the same key with a
// *different* body is a CONFLICT, not a silent return of the original result.
type IdempotencyRecord struct {
	ScopeHash string
	State     IdempotencyState
	// RequestDigest is SHA-256 over the canonical command payload.
	RequestDigest string
	// ResponseStatus is the stored HTTP status of the original result.
	ResponseStatus int
	// ResponseBody is the stored canonical response body, returned verbatim on
	// replay so that clients observe byte-identical behaviour.
	ResponseBody string
	// CommandID links the record to the durable command that executed it.
	CommandID ID
	CreatedAt Timestamp
	UpdatedAt Timestamp
}

// DigestRequest computes the stable request digest used to detect a key reused
// with a different body.
func DigestRequest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ReplayDecision is the outcome of an idempotency lookup against the durable
// record.
type ReplayDecision string

const (
	// ReplayExecute means no prior record exists; execute the command.
	ReplayExecute ReplayDecision = "execute"
	// ReplayReturnStored means a terminal result exists for the identical
	// request; return it verbatim.
	ReplayReturnStored ReplayDecision = "return_stored"
	// ReplayInFlight means another execution holds the key; reject with CONFLICT.
	ReplayInFlight ReplayDecision = "in_flight"
	// ReplayKeyReuseMismatch means the key was reused with a different request
	// body; reject with CONFLICT. Executing it would make the idempotency
	// guarantee meaningless.
	ReplayKeyReuseMismatch ReplayDecision = "key_reuse_mismatch"
)

// DecideReplay is the pure function that maps a durable record and the incoming
// request to a decision. It is exported and separately testable because the
// duplicate-command financial invariant depends entirely on it
// (09_TESTING_AND_RELEASE_EVIDENCE.md, invariant 3).
func DecideReplay(existing *IdempotencyRecord, scope IdempotencyScope, requestDigest string) (ReplayDecision, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if existing == nil {
		return ReplayExecute, nil
	}
	if existing.ScopeHash != scope.Hash() {
		// A lookup bug rather than a caller error: fail closed.
		return "", ErrConflictf("idempotency lookup returned a record for a different scope")
	}
	if existing.RequestDigest != requestDigest {
		return ReplayKeyReuseMismatch, nil
	}
	switch existing.State {
	case IdempotencySucceeded, IdempotencyFailed, IdempotencyUnknown:
		return ReplayReturnStored, nil
	case IdempotencyInFlight:
		return ReplayInFlight, nil
	default:
		return "", ErrInternalf("idempotency record has unsupported state %q", string(existing.State))
	}
}
