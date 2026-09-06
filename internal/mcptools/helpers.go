// Package mcptools defines the MCP tools and their handlers. This protocol
// layer validates input and writes audit logs. The Calendar, Contacts, and Mail
// domain packages provide network access.
package mcptools

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const maxCalendarResultBytes = 256 << 10

const (
	maxErrorMessageBytes = 64 << 10
	maxErrorDetailBytes  = 8 << 10
	maxErrorDetails      = 16
)

// datetimeParamDescription builds the description of a start or end parameter.
// It includes the configured default timezone.
//
// Start with a local-time example that has no "Z" suffix. Earlier text showed
// a leading "Z" example. One caller reused a local hour with "Z", which changed
// the intended local time to UTC. The current example removes that conversion
// step.
func datetimeParamDescription(label string, defaultLoc *time.Location) string {
	tz := defaultLocationName(defaultLoc)
	return fmt.Sprintf(
		"%s. Prefer local time without an offset (example: 2026-07-01T14:00:00); it is read as %s "+
			"(ICLOUD_MCP_DEFAULT_TZ), DST-aware. Do not append Z or calculate an offset unless the user "+
			"explicitly means another timezone; then use RFC3339 with its offset.",
		label, tz,
	)
}

// toolErrorPayload is the machine-readable shape of MCP tool errors.
type toolErrorPayload struct {
	Code           string            `json:"code,omitempty"`
	Message        string            `json:"message"`
	Retryable      bool              `json:"retryable,omitempty"`
	RetryAfter     int               `json:"retry_after_seconds,omitempty"`
	Reconciliation string            `json:"reconciliation,omitempty"`
	Details        map[string]string `json:"details,omitempty"`
}

// errResult builds an error CallToolResult and sends its message through the
// Redactor. All tool errors use this helper. A classified *icloud.Error adds a
// stable JSON code, so agents do not need to parse English text. The result
// never includes raw HTTP or XML bodies.
func errResult(red *security.Redactor, context string, err error) *mcp.CallToolResult {
	msg := boundedUTF8(redact(red, fmt.Sprintf("%s: %v", context, err)), maxErrorMessageBytes)
	payload := toolErrorPayload{Message: msg}
	if ie := icloud.AsICloudError(err); ie != nil {
		payload.Code = string(icloud.PublicCode(ie.Code))
		// Keep concurrent_modification visible under both names for agents.
		if ie.Code == icloud.CodeConcurrentModification {
			payload.Code = string(ie.Code)
		}
		payload.Retryable = ie.Retryable
		if ie.RetryAfter > 0 {
			sec := int(ie.RetryAfter.Seconds())
			if sec > 60 {
				sec = 60
			}
			payload.RetryAfter = sec
		}
		if len(ie.Details) > 0 {
			payload.Details = make(map[string]string, min(len(ie.Details), maxErrorDetails))
			for key, value := range ie.Details {
				if len(payload.Details) >= maxErrorDetails {
					break
				}
				key = boundedUTF8(redact(red, key), maxErrorDetailBytes)
				value = boundedUTF8(redact(red, value), maxErrorDetailBytes)
				payload.Details[key] = value
			}
			if ie.Code == icloud.CodeOutcomeUnknown {
				payload.Reconciliation = payload.Details["reconciliation"]
			}
		}
	} else if context == "validation" || context == "validation error" {
		payload.Code = string(icloud.CodeValidation)
	}
	b, mErr := json.Marshal(payload)
	if mErr != nil {
		return mcp.NewToolResultError(msg)
	}
	if len(b) > maxCalendarResultBytes {
		payload.Details = nil
		payload.Reconciliation = ""
		b, mErr = json.Marshal(payload)
		if mErr != nil || len(b) > maxCalendarResultBytes {
			return mcp.NewToolResultError("internal error: error response exceeded its byte limit")
		}
	}
	return mcp.NewToolResultError(string(b))
}

func calendarMutationErrorStatus(err error) string {
	if typed := icloud.AsICloudError(err); typed != nil && typed.Code == icloud.CodeOutcomeUnknown {
		return "outcome_unknown"
	}
	return "error"
}

// writeJSON formats payload as indented JSON and returns a success
// CallToolResult. It sends the body through the Redactor to protect the MCP
// success channel, including secrets in Calendar text or echoed server data.
// It sends serialization failures through errResult.
func writeJSON(red *security.Redactor, payload any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return errResult(red, "formatting response", err)
	}
	text := redact(red, string(b))
	if len(text) > maxCalendarResultBytes {
		return errResult(red, "formatting response", icloud.NewError(
			icloud.CodePayloadTooLarge, 0, "serialized MCP result exceeded 256 KiB", nil,
		))
	}
	return mcp.NewToolResultText(text)
}

// writeCalendarJSON applies the Calendar-specific serialized result budget.
// Local capability/validation tools continue to use writeJSON directly.
func writeCalendarJSON(red *security.Redactor, payload any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return errResult(red, "formatting response", err)
	}
	return writeCalendarEncoded(red, b)
}

func writeCalendarEncoded(red *security.Redactor, encoded []byte) *mcp.CallToolResult {
	text := redact(red, string(encoded))
	if len(text) > maxCalendarResultBytes {
		return errResult(red, "formatting response", icloud.NewError(
			icloud.CodePayloadTooLarge, 0, "serialized Calendar result exceeded 256 KiB", nil,
		))
	}
	return mcp.NewToolResultText(text)
}

// calendarResultText extracts a deliverable success body for idempotency cache.
func calendarResultText(result *mcp.CallToolResult) (string, bool) {
	if result == nil || result.IsError || len(result.Content) == 0 {
		return "", false
	}
	text, ok := mcp.AsTextContent(result.Content[0])
	if !ok || text.Text == "" {
		return "", false
	}
	return text.Text, true
}

func logCalendarMutation(audit *security.AuditLogger, tool, calendarPath, uid, status string) {
	if audit == nil {
		return
	}
	// NUL makes the calendar/UID tuple unambiguous before AuditLogger hashes
	// it into the opaque, process-scoped resource token.
	audit.LogDomainMutation(tool, "calendar", "event", calendarPath+"\x00"+uid, status)
}

func redact(red *security.Redactor, value string) string {
	if red == nil {
		// Production RegisterUnified panics on a nil redactor. Passthrough is
		// only for tightly controlled unit tests of non-secret payloads.
		return value
	}
	return red.Redact(value)
}

func boundedUTF8(value string, limit int) string {
	if !utf8.ValidString(value) {
		value = string([]rune(value))
	}
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	const suffix = "...[truncated]"
	cut := limit - len(suffix)
	if cut <= 0 {
		return suffix[:limit]
	}
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + suffix
}

func optionalStringArg(req mcp.CallToolRequest, name, defaultValue string) (string, error) {
	value, ok := req.GetArguments()[name]
	if !ok {
		return defaultValue, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return text, nil
}

func optionalBoolArg(req mcp.CallToolRequest, name string, defaultValue bool) (bool, error) {
	value, ok := req.GetArguments()[name]
	if !ok {
		return defaultValue, nil
	}
	boolean, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return boolean, nil
}

func optionalIntArg(req mcp.CallToolRequest, name string, defaultValue int) (int, error) {
	value, ok := req.GetArguments()[name]
	if !ok {
		return defaultValue, nil
	}
	var number int64
	switch typed := value.(type) {
	case int:
		return typed, nil
	case int8:
		number = int64(typed)
	case int16:
		number = int64(typed)
	case int32:
		number = int64(typed)
	case int64:
		number = typed
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		number = int64(typed)
	case uint8:
		number = int64(typed)
	case uint16:
		number = int64(typed)
	case uint32:
		number = int64(typed)
	case uint64:
		if typed > math.MaxInt64 {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		number = int64(typed)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed < -1<<53 || typed > 1<<53 {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		number = int64(typed)
	case float32:
		asFloat := float64(typed)
		if math.IsNaN(asFloat) || math.IsInf(asFloat, 0) || math.Trunc(asFloat) != asFloat || asFloat < -1<<53 || asFloat > 1<<53 {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		number = int64(typed)
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		number = parsed
	default:
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	converted := int(number)
	if int64(converted) != number {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return converted, nil
}
