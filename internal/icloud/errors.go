package icloud

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, greppable error code surfaced to the MCP client. It is
// part of the public contract of the CalDAV client: an MCP host may match
// on it to adapt its behavior (e.g. re-read and retry on
// CodeConcurrentModification). Stable across releases.
type Code string

const (
	// CodeAuthenticationRefused: 401 (bad app-specific password, revoked
	// credentials). The user must regenerate/renew the app-specific password
	// on appleid.apple.com.
	CodeAuthenticationRefused Code = "authentication_refused"
	// CodeForbidden: 403 (revoked app-specific password, or Apple quota
	// exceeded: an iCloud calendar holds at most 50,000 events).
	CodeForbidden Code = "forbidden"
	// CodeNotFound: 404 (calendar or event no longer exists server-side).
	CodeNotFound Code = "not_found"
	// CodeConcurrentModification: 412 Precondition Failed (If-Match/ETag
	// mismatch). Another client modified the event since it was read; the
	// caller must re-read and retry the update.
	CodeConcurrentModification Code = "concurrent_modification"
	// CodeRateLimited: 429 returned by iCloud after the HTTP-layer retry
	// budget was exhausted. Reduce the request rate.
	CodeRateLimited Code = "rate_limited"
	// CodeServerUnavailable: 5xx (502/503/504) returned by the CalDAV shard
	// after the HTTP-layer retry budget was exhausted. The shard is
	// temporarily unreachable.
	CodeServerUnavailable Code = "server_unavailable"
	// CodeHTTPError: any other unexpected HTTP status not covered above.
	CodeHTTPError Code = "http_error"
)

// Error is the typed CalDAV error returned by the client wrapper for every
// non-2xx HTTP response that could be classified. Its Error() text starts
// with the stable Code (e.g. "concurrent_modification: ..."), so the code is
// visible in the MCP error text even without structured access.
type Error struct {
	Code    Code
	Status  int
	Message string
	Cause   error
}

// NewError builds a typed Error. A nil cause is fine.
func NewError(code Code, status int, message string, cause error) *Error {
	return &Error{Code: code, Status: status, Message: message, Cause: cause}
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
	case status == http.StatusPreconditionFailed:
		return NewError(CodeConcurrentModification, status,
			"the event was modified by another client since it was read (ETag mismatch): re-read and retry the update", nil)
	case status == http.StatusTooManyRequests:
		return NewError(CodeRateLimited, status,
			"iCloud is rate limiting requests (HTTP 429) after retries: reduce the request rate", nil)
	case status >= 500:
		return NewError(CodeServerUnavailable, status,
			fmt.Sprintf("iCloud CalDAV shard is temporarily unavailable (HTTP %d) after retries", status), nil)
	default:
		return NewError(CodeHTTPError, status,
			fmt.Sprintf("iCloud returned an unexpected HTTP status (%d)", status), nil)
	}
}
