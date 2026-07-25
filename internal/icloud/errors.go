package icloud

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Default agent-facing retry delays when the remote did not supply a usable
// Retry-After header. Capped so agents never wait on a multi-minute stall.
const (
	defaultRateLimitRetryAfter   = 5 * time.Second
	defaultUnavailableRetryAfter = 2 * time.Second
	maxPublicRetryAfter          = 60 * time.Second
)

// Code is a stable, greppable error code surfaced to the MCP client. It is
// part of the public contract of the CalDAV client: an MCP host may match
// on it to adapt its behavior (e.g. re-read and retry on
// CodeConcurrentModification). Stable across releases.
type Code string

const (
	// CodeValidation: client-side input validation failed before any network call.
	CodeValidation Code = "validation"
	// CodeAuthenticationRefused: 401 (bad app-specific password, revoked
	// credentials). The user must regenerate/renew the app-specific password
	// on appleid.apple.com.
	CodeAuthenticationRefused Code = "authentication_refused"
	// CodeAuthentication is an alias used in structured MCP payloads (objective
	// vocabulary). Prefer matching either authentication or authentication_refused.
	CodeAuthentication Code = "authentication"
	// CodeForbidden / CodeAuthorization: 403.
	CodeForbidden     Code = "forbidden"
	CodeAuthorization Code = "authorization"
	// CodeNotFound: 404 (calendar or event no longer exists server-side).
	CodeNotFound Code = "not_found"
	// CodeConflict / CodeConcurrentModification: 409/412.
	CodeConflict               Code = "conflict"
	CodeConcurrentModification Code = "concurrent_modification"
	// CodeRateLimited: 429, after read retries or immediately for a mutation.
	CodeRateLimited Code = "rate_limited"
	// CodeTimeout: context deadline or client timeout.
	CodeTimeout Code = "timeout"
	// CodeServerUnavailable / CodeUnavailable: a definitive unavailable status.
	CodeServerUnavailable Code = "server_unavailable"
	CodeUnavailable       Code = "unavailable"
	// CodePartialFailure: multi-calendar operation succeeded partially.
	CodePartialFailure Code = "partial_failure"
	// CodeProtocolError: unexpected protocol / HTTP status not otherwise classified.
	CodeProtocolError Code = "protocol_error"
	// CodePayloadTooLarge: a bounded remote resource or serialized result
	// exceeded its security limit.
	CodePayloadTooLarge Code = "payload_too_large"
	// CodeOutcomeUnknown: a mutation was dispatched but its final outcome
	// cannot be determined safely. Callers must reconcile before retrying.
	CodeOutcomeUnknown Code = "outcome_unknown"
	// CodeHTTPError: legacy alias of protocol_error for unexpected HTTP status.
	CodeHTTPError Code = "http_error"
	// CodeInternal: unexpected internal error (never carries raw bodies).
	CodeInternal Code = "internal_error"
)

// Error is the typed CalDAV/MCP error returned for classified failures. Its
// Error() text starts with the stable Code so the code is visible even
// without structured access. Message never contains raw HTTP/XML bodies or
// credentials (callers must still run the Redactor on the way out).
type Error struct {
	Code       Code
	Status     int
	Message    string
	Retryable  bool
	RetryAfter time.Duration // zero = none; always capped by the HTTP retry layer
	Details    map[string]string
	Cause      error
}

// NewError builds a typed Error. A nil cause is fine.
func NewError(code Code, status int, message string, cause error) *Error {
	return &Error{Code: code, Status: status, Message: message, Cause: cause}
}

// NewValidationError builds a non-retryable validation error.
func NewValidationError(message string) *Error {
	return &Error{Code: CodeValidation, Message: message}
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap implements errors.Wrapper so errors.As / errors.Is reach the cause.
func (e *Error) Unwrap() error { return e.Cause }

// AsICloudError returns the typed *Error wrapping err, or nil if err is not
// a classified CalDAV error.
func AsICloudError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// classifyStatus maps an HTTP status code to a typed Error. Used by the
// retry/classify doer and by the hand-rolled requests as a fallback when the
// doer is a plain test client (no classification).
func classifyStatus(status int) *Error {
	return classifyStatusWithRetryAfter(status, 0)
}

// classifyStatusWithRetryAfter is classifyStatus plus an optional Retry-After
// hint from the last HTTP response. Zero retryAfter keeps the code defaults.
func classifyStatusWithRetryAfter(status int, retryAfter time.Duration) *Error {
	err := classifyStatusBare(status)
	if err == nil {
		return nil
	}
	if retryAfter > 0 && (err.Code == CodeRateLimited || err.Code == CodeServerUnavailable || err.Code == CodeUnavailable) {
		err.RetryAfter = capPublicRetryAfter(retryAfter)
	}
	return err
}

func classifyStatusBare(status int) *Error {
	switch {
	case status == http.StatusUnauthorized:
		return NewError(CodeAuthenticationRefused, status,
			"iCloud authentication refused: check ICLOUD_EMAIL and the app-specific password", nil)
	case status == http.StatusForbidden:
		return NewError(CodeForbidden, status,
			"iCloud refused the request (forbidden): the app-specific password may have been revoked, or the calendar quota (50,000 events) is exceeded", nil)
	case status == http.StatusNotFound:
		return NewError(CodeNotFound, status,
			"iCloud resource not found: the calendar or event no longer exists", nil)
	case status == http.StatusConflict:
		return &Error{
			Code: CodeConflict, Status: status,
			Message: "the request conflicts with the current state of the resource",
		}
	case status == http.StatusPreconditionFailed:
		return &Error{
			Code: CodeConcurrentModification, Status: status,
			Message: "the event was modified by another client since it was read (ETag mismatch): re-read and retry the update",
		}
	case status == http.StatusLocked: // 423
		return &Error{
			Code: CodeConflict, Status: status,
			Message:   "the calendar resource is locked",
			Retryable: true,
		}
	case status == http.StatusTooManyRequests:
		return &Error{
			Code: CodeRateLimited, Status: status, Retryable: true,
			Message:    "iCloud is rate limiting requests (HTTP 429) after retries: reduce the request rate",
			RetryAfter: defaultRateLimitRetryAfter,
		}
	case status >= 500:
		return &Error{
			Code: CodeServerUnavailable, Status: status, Retryable: true,
			Message:    fmt.Sprintf("iCloud CalDAV shard is temporarily unavailable (HTTP %d) after retries", status),
			RetryAfter: defaultUnavailableRetryAfter,
		}
	default:
		return NewError(CodeProtocolError, status,
			fmt.Sprintf("iCloud returned an unexpected HTTP status (%d)", status), nil)
	}
}

func capPublicRetryAfter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > maxPublicRetryAfter {
		return maxPublicRetryAfter
	}
	return d
}

func outcomeUnknownError(status int) *Error {
	return &Error{
		Code:    CodeOutcomeUnknown,
		Status:  status,
		Message: "the Calendar mutation outcome is unknown",
		Details: map[string]string{
			"reconciliation": "Re-read the target event before retrying; do not repeat the mutation blindly.",
		},
	}
}

func withOutcomeReconciliation(err error, uid, reconciliation string) error {
	typed := AsICloudError(err)
	if typed == nil || typed.Code != CodeOutcomeUnknown {
		return err
	}
	details := make(map[string]string, len(typed.Details)+2)
	for key, value := range typed.Details {
		details[key] = value
	}
	if uid != "" {
		details["uid"] = uid
	}
	details["reconciliation"] = reconciliation
	clone := *typed
	clone.Details = details
	return &clone
}

// PublicCode maps an internal/legacy Code to the objective vocabulary where
// useful, while keeping concurrent_modification as the preferred conflict
// signal for agents that already match on it.
func PublicCode(c Code) Code {
	switch c {
	case CodeAuthenticationRefused:
		return CodeAuthentication
	case CodeForbidden:
		return CodeAuthorization
	case CodeServerUnavailable:
		return CodeUnavailable
	case CodeHTTPError:
		return CodeProtocolError
	default:
		return c
	}
}
