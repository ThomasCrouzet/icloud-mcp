package contacts

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Code is a stable public Contacts error code.
type Code string

const (
	CodeValidation             Code = "validation"
	CodeAuthentication         Code = "authentication"
	CodeAuthorization          Code = "authorization"
	CodeNotFound               Code = "not_found"
	CodeConflict               Code = "conflict"
	CodeConcurrentModification Code = "concurrent_modification"
	CodeRateLimited            Code = "rate_limited"
	CodeTimeout                Code = "timeout"
	CodeUnavailable            Code = "unavailable"
	CodePartialFailure         Code = "partial_failure"
	CodeProtocolError          Code = "protocol_error"
	CodePayloadTooLarge        Code = "payload_too_large"
	CodeOutcomeUnknown         Code = "outcome_unknown"
	CodeInternalError          Code = "internal_error"
)

// Error is a sanitized, typed CardDAV error. Message never includes a remote
// body, URL, contact value, account identity, or credential.
type Error struct {
	Code           Code
	Status         int
	Message        string
	Retryable      bool
	RetryAfter     time.Duration
	Reconciliation string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func newError(code Code, status int, message string) *Error {
	return &Error{Code: code, Status: status, Message: message}
}

func validationError(message string) *Error {
	return newError(CodeValidation, 0, message)
}

// AsError returns the classified Contacts error wrapped by err, if any.
func AsError(err error) *Error {
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	return nil
}

func classifyStatus(status int) *Error {
	switch {
	case status == http.StatusUnauthorized:
		return newError(CodeAuthentication, status, "iCloud Contacts authentication was rejected")
	case status == http.StatusForbidden:
		return newError(CodeAuthorization, status, "iCloud Contacts denied the operation")
	case status == http.StatusNotFound:
		return newError(CodeNotFound, status, "contact resource was not found")
	case status == http.StatusConflict:
		return newError(CodeConflict, status, "the operation conflicts with the current resource state")
	case status == http.StatusPreconditionFailed:
		return newError(CodeConcurrentModification, status, "the contact changed since it was read")
	case status == http.StatusRequestEntityTooLarge || status == http.StatusInsufficientStorage:
		return newError(CodePayloadTooLarge, status, "the contact exceeds the server resource limit")
	case status == http.StatusTooManyRequests:
		return &Error{Code: CodeRateLimited, Status: status, Message: "iCloud Contacts is rate limiting requests", Retryable: true}
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return &Error{Code: CodeTimeout, Status: status, Message: "the Contacts request timed out", Retryable: true}
	case status >= 500:
		return &Error{Code: CodeUnavailable, Status: status, Message: "iCloud Contacts is temporarily unavailable", Retryable: true}
	default:
		return newError(CodeProtocolError, status, "iCloud Contacts returned an unexpected HTTP status")
	}
}
