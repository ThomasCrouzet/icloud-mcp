package mail

import (
	"errors"
	"fmt"
	"time"
)

// Code is a stable public error code suitable for MCP responses.
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

// Error is a sanitized Mail failure. It never includes server response text,
// protocol payloads, account identities, recipients, or credentials.
type Error struct {
	Code           Code          `json:"code"`
	Message        string        `json:"message"`
	Retryable      bool          `json:"retryable,omitempty"`
	RetryAfter     time.Duration `json:"-"`
	Reconciliation string        `json:"reconciliation,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func newError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func newRetryableError(code Code, message string) *Error {
	err := &Error{Code: code, Message: message, Retryable: true}
	switch code {
	case CodeRateLimited:
		err.RetryAfter = 5 * time.Second
	case CodeTimeout, CodeUnavailable:
		err.RetryAfter = 2 * time.Second
	}
	return err
}

func validationError(message string) *Error {
	return newError(CodeValidation, message)
}

// AsError returns the classified Mail error wrapping err, or nil.
func AsError(err error) *Error {
	var mailErr *Error
	if errors.As(err, &mailErr) {
		return mailErr
	}
	return nil
}
