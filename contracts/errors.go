package contracts

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// ErrorCode is the closed canonical error taxonomy (03_CANONICAL_CONTRACTS.md).
// The set is closed: adding a member is a breaking contract change requiring a
// new schema version.
type ErrorCode string

const (
	CodeValidation            ErrorCode = "VALIDATION_ERROR"
	CodeAuthentication        ErrorCode = "AUTHENTICATION_ERROR"
	CodeAuthorization         ErrorCode = "AUTHORIZATION_ERROR"
	CodeConflict              ErrorCode = "CONFLICT"
	CodeRiskRejected          ErrorCode = "RISK_REJECTED"
	CodeRateLimited           ErrorCode = "RATE_LIMITED"
	CodeDependencyUnavailable ErrorCode = "DEPENDENCY_UNAVAILABLE"
	CodeTimeoutUnknown        ErrorCode = "TIMEOUT_UNKNOWN"
	CodeDataStale             ErrorCode = "DATA_STALE"
	CodeReconciliationReq     ErrorCode = "RECONCILIATION_REQUIRED"
	CodeInternal              ErrorCode = "INTERNAL_ERROR"
)

var allCodes = []ErrorCode{
	CodeValidation, CodeAuthentication, CodeAuthorization, CodeConflict,
	CodeRiskRejected, CodeRateLimited, CodeDependencyUnavailable,
	CodeTimeoutUnknown, CodeDataStale, CodeReconciliationReq, CodeInternal,
}

var codeSet = func() map[ErrorCode]struct{} {
	m := make(map[ErrorCode]struct{}, len(allCodes))
	for _, c := range allCodes {
		m[c] = struct{}{}
	}
	return m
}()

// Sentinel errors for errors.Is matching. Each taxonomy member has exactly one
// canonical sentinel, so an operator can match on the class of failure without
// depending on the message text.
var (
	ErrValidation             = &Error{Code: CodeValidation, Message: "validation failed"}
	ErrAuthentication         = &Error{Code: CodeAuthentication, Message: "authentication failed"}
	ErrAuthorization          = &Error{Code: CodeAuthorization, Message: "authorization failed"}
	ErrConflict               = &Error{Code: CodeConflict, Message: "conflict"}
	ErrRiskRejected           = &Error{Code: CodeRiskRejected, Message: "risk rejected"}
	ErrRateLimited            = &Error{Code: CodeRateLimited, Message: "rate limited"}
	ErrDependencyUnavailable  = &Error{Code: CodeDependencyUnavailable, Message: "dependency unavailable"}
	ErrTimeoutUnknown         = &Error{Code: CodeTimeoutUnknown, Message: "outcome unknown"}
	ErrDataStale              = &Error{Code: CodeDataStale, Message: "data stale or degraded"}
	ErrReconciliationRequired = &Error{Code: CodeReconciliationReq, Message: "reconciliation required"}
	ErrInternal               = &Error{Code: CodeInternal, Message: "internal error"}
)

// Sentinels returns the canonical sentinel for a taxonomy member.
func (c ErrorCode) Sentinel() *Error {
	switch c {
	case CodeValidation:
		return ErrValidation
	case CodeAuthentication:
		return ErrAuthentication
	case CodeAuthorization:
		return ErrAuthorization
	case CodeConflict:
		return ErrConflict
	case CodeRiskRejected:
		return ErrRiskRejected
	case CodeRateLimited:
		return ErrRateLimited
	case CodeDependencyUnavailable:
		return ErrDependencyUnavailable
	case CodeTimeoutUnknown:
		return ErrTimeoutUnknown
	case CodeDataStale:
		return ErrDataStale
	case CodeReconciliationReq:
		return ErrReconciliationRequired
	case CodeInternal:
		return ErrInternal
	}
	return nil
}

// Known reports whether the code is a member of the closed taxonomy.
func (c ErrorCode) Known() bool { _, ok := codeSet[c]; return ok }

// CanonicalErrorCodes returns the taxonomy in stable order, for contract tests
// and for the published OpenAPI error-code enumeration.
func CanonicalErrorCodes() []ErrorCode {
	out := append([]ErrorCode(nil), allCodes...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// httpStatus is the binding HTTP mapping from 15_API_AND_INTEGRATION_CONTRACTS.md
// §3. It is part of the contract and is covered by API contract tests.
var httpStatus = map[ErrorCode]int{
	CodeValidation:            http.StatusBadRequest,          // 400
	CodeAuthentication:        http.StatusUnauthorized,        // 401
	CodeAuthorization:         http.StatusForbidden,           // 403
	CodeConflict:              http.StatusConflict,            // 409
	CodeRiskRejected:          http.StatusUnprocessableEntity, // 422
	CodeRateLimited:           http.StatusTooManyRequests,     // 429
	CodeDependencyUnavailable: http.StatusServiceUnavailable,  // 503
	CodeTimeoutUnknown:        http.StatusServiceUnavailable,  // 503
	CodeDataStale:             http.StatusUnprocessableEntity, // 422
	CodeReconciliationReq:     http.StatusUnprocessableEntity, // 422
	CodeInternal:              http.StatusInternalServerError, // 500
}

// HTTPStatus returns the binding HTTP status code for a taxonomy member.
func (c ErrorCode) HTTPStatus() int {
	if s, ok := httpStatus[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Retryable reports whether a caller may retry the same request with the same
// idempotency key. Only genuinely retryable conditions may be true: retrying a
// financial command whose outcome is unknown risks duplicate exposure
// (01_SYSTEM_ARCHITECTURE.md invariant 3).
func (c ErrorCode) Retryable() bool {
	switch c {
	case CodeRateLimited, CodeDependencyUnavailable:
		return true
	default:
		// TIMEOUT_UNKNOWN is deliberately NOT retryable with the same key:
		// the client must query canonical state instead.
		return false
	}
}

// Error is the canonical domain error. It carries the taxonomy code, a safe
// human-readable message, structured details that are explicitly safe for a
// client, and the correlation identifier for cross-system evidence lookup.
//
// Error payloads never carry secrets, credentials, raw tokens or internal
// topology (06_SECURITY_AND_ACCESS_CONTROL.md §6).
type Error struct {
	Code    ErrorCode
	Message string
	// Details are safe, structured, client-visible fields such as the list of
	// failed risk controls. It must never contain sensitive values.
	Details map[string]string
	// CorrelationID links the failure to audit records and traces.
	CorrelationID string
	// wrapped retains the internal cause for operator diagnostics. It is never
	// serialised to a client.
	wrapped error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	b.WriteString(": ")
	b.WriteString(e.Message)
	if e.CorrelationID != "" {
		b.WriteString(" (correlation_id=")
		b.WriteString(e.CorrelationID)
		b.WriteString(")")
	}
	if e.wrapped != nil {
		b.WriteString(": ")
		b.WriteString(e.wrapped.Error())
	}
	return b.String()
}

// Unwrap exposes the internal cause to errors.Is/As without serialising it.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.wrapped
}

// Is makes errors.Is match any *Error sharing the same taxonomy code, so that
// callers can test for a failure class without depending on message text.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || t == nil {
		return false
	}
	if e == nil {
		return false
	}
	return e.Code == t.Code
}

// HTTPStatus returns the binding HTTP status for this error.
func (e *Error) HTTPStatus() int {
	if e == nil {
		return http.StatusInternalServerError
	}
	return e.Code.HTTPStatus()
}

// Wire is the canonical error body. It matches the published OpenAPI error
// schema exactly: error_code, message, correlation_id, and safe details.
type ErrorWire struct {
	ErrorCode     ErrorCode         `json:"error_code"`
	Message       string            `json:"message"`
	CorrelationID string            `json:"correlation_id"`
	Details       map[string]string `json:"details,omitempty"`
	// Retryable is advisory only; it never authorises a blind retry of a
	// financial command.
	Retryable bool `json:"retryable"`
}

// Wire converts the error to its client-visible form. The internal cause is
// deliberately dropped so that internal topology cannot leak.
func (e *Error) Wire() ErrorWire {
	if e == nil {
		return ErrorWire{ErrorCode: CodeInternal, Message: "unclassified internal error"}
	}
	details := make(map[string]string, len(e.Details))
	for k, v := range e.Details {
		details[k] = v
	}
	return ErrorWire{
		ErrorCode:     e.Code,
		Message:       e.Message,
		CorrelationID: e.CorrelationID,
		Details:       details,
		Retryable:     e.Code.Retryable(),
	}
}

// WithDetail returns a copy with an additional safe detail field.
func (e *Error) WithDetail(k, v string) *Error {
	cp := e.clone()
	if cp.Details == nil {
		cp.Details = map[string]string{}
	}
	cp.Details[k] = v
	return cp
}

// WithCorrelation returns a copy carrying a correlation identifier.
func (e *Error) WithCorrelation(id string) *Error {
	cp := e.clone()
	cp.CorrelationID = id
	return cp
}

// WithCause attaches an internal cause that is never serialised to a client.
func (e *Error) WithCause(err error) *Error {
	cp := e.clone()
	cp.wrapped = err
	return cp
}

func (e *Error) clone() *Error {
	if e == nil {
		return NewError(CodeInternal, "unclassified internal error")
	}
	cp := &Error{
		Code:          e.Code,
		Message:       e.Message,
		CorrelationID: e.CorrelationID,
		wrapped:       e.wrapped,
	}
	if len(e.Details) > 0 {
		cp.Details = make(map[string]string, len(e.Details))
		for k, v := range e.Details {
			cp.Details[k] = v
		}
	}
	return cp
}

// NewError builds a canonical error.
func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Errorf builds a canonical error with a formatted message. The code must be a
// taxonomy member; a non-member is coerced to INTERNAL_ERROR and recorded as a
// defect rather than emitted as an unsupported value.
func Errorf(code ErrorCode, format string, args ...any) *Error {
	if !code.Known() {
		return &Error{
			Code:    CodeInternal,
			Message: fmt.Sprintf("non-taxonomy error code %q emitted; classified as INTERNAL_ERROR", code),
		}
	}
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Constructors for the conditions that recur across every bounded context.
// Centralising them keeps messages and codes consistent and makes the taxonomy
// exhaustively testable.

// ErrValidationf reports a domain/input validation failure (HTTP 400).
func ErrValidationf(format string, args ...any) *Error {
	return Errorf(CodeValidation, format, args...)
}

// ErrAuthenticationf reports an unauthenticated or invalid-credential failure.
func ErrAuthenticationf(format string, args ...any) *Error {
	return Errorf(CodeAuthentication, format, args...)
}

// ErrAuthorizationf reports an authenticated but unauthorized actor.
func ErrAuthorizationf(format string, args ...any) *Error {
	return Errorf(CodeAuthorization, format, args...)
}

// ErrConflictf reports a state, version or idempotency conflict (HTTP 409).
func ErrConflictf(format string, args ...any) *Error {
	return Errorf(CodeConflict, format, args...)
}

// ErrRiskRejectedf reports a deterministic Risk Engine veto (HTTP 422).
func ErrRiskRejectedf(format string, args ...any) *Error {
	return Errorf(CodeRiskRejected, format, args...)
}

// ErrRateLimitedf reports an applied rate limit (HTTP 429).
func ErrRateLimitedf(format string, args ...any) *Error {
	return Errorf(CodeRateLimited, format, args...)
}

// ErrDependencyUnavailablef reports an unavailable required dependency (503).
func ErrDependencyUnavailablef(format string, args ...any) *Error {
	return Errorf(CodeDependencyUnavailable, format, args...)
}

// ErrTimeoutUnknownf reports a timeout that prevents proving whether a
// downstream side effect occurred. This is deliberately distinct from a
// dependency outage: the command may have taken effect at the venue.
func ErrTimeoutUnknownf(format string, args ...any) *Error {
	return Errorf(CodeTimeoutUnknown, format, args...)
}

// ErrDataStalef reports stale or degraded required input (HTTP 422).
func ErrDataStalef(format string, args ...any) *Error {
	return Errorf(CodeDataStale, format, args...)
}

// ErrReconciliationRequiredf reports an unresolved material break blocking the
// requested scope (HTTP 422).
func ErrReconciliationRequiredf(format string, args ...any) *Error {
	return Errorf(CodeReconciliationReq, format, args...)
}

// ErrInternalf reports an unexpected internal condition (HTTP 500).
func ErrInternalf(format string, args ...any) *Error {
	return Errorf(CodeInternal, format, args...)
}

// InternalDetailf records an operator-facing diagnostic that must never reach a
// client. The returned error carries a fixed, opaque client message; the
// detailed text is retained only as the internal cause, reachable by operators
// through errors.Unwrap and written only to the internal structured log.
//
// Prefer this over ErrInternalf anywhere the format arguments could contain
// hostnames, addresses, table names, file paths, SQL, or vendor error strings.
// err.InternalDetailf("ledger write failed", "insert into ledger.entries on %s: %w", host, cause)
func InternalDetailf(clientMsg string, internalFormat string, args ...any) *Error {
	return &Error{
		Code:    CodeInternal,
		Message: clientMsg,
		wrapped: fmt.Errorf(internalFormat, args...),
	}
}

// AsError extracts a canonical *Error from an error chain, or nil.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// CodeOf returns the taxonomy code for an error, defaulting to INTERNAL_ERROR
// for a non-canonical error so that no unstructured failure escapes the taxonomy.
func CodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	if e := AsError(err); e != nil {
		return e.Code
	}
	return CodeInternal
}
