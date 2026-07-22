// Package mcptools defines the MCP tools exposed by the server and their
// handlers. All input validation and audit logging live here (protocol
// layer); network access lives in internal/icloud.
package mcptools

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// datetimeParamDescription builds the mcp.Description text for a start/end
// tool parameter, naming the actually configured default timezone so the
// schema never nudges the calling agent toward the wrong thing.
//
// Deliberately does NOT lead with a "...Z" example: an earlier version of
// this description did, and the calling agent was observed echoing a stated
// local hour straight back with a "Z" suffix (i.e. literal UTC) instead of
// converting it, shifting real events by the local UTC offset once iCloud
// rendered them. Leading with the no-offset local-time example steers
// towards the form that removes that conversion step entirely.
func datetimeParamDescription(label string, defaultLoc *time.Location) string {
	tz := defaultLocationName(defaultLoc)
	return fmt.Sprintf(
		"%s. Prefer a local wall-clock time with NO offset (e.g. 2026-07-01T14:00:00 for 2pm) "+
			"matching what the user said: it is interpreted as %s (ICLOUD_MCP_DEFAULT_TZ), DST-aware, "+
			"with no conversion needed on your part. Do NOT append Z or compute an offset yourself "+
			"unless the user explicitly means a different, specific timezone (e.g. UTC or another city) "+
			"in which case use full RFC3339 with that explicit offset (e.g. 2026-07-01T14:00:00+02:00, or "+
			"...Z only if UTC is truly what is meant).",
		label, tz,
	)
}

// toolErrorPayload is the machine-readable shape of MCP tool errors.
type toolErrorPayload struct {
	Code       string            `json:"code,omitempty"`
	Message    string            `json:"message"`
	Retryable  bool              `json:"retryable,omitempty"`
	RetryAfter int               `json:"retry_after_seconds,omitempty"`
	Details    map[string]string `json:"details,omitempty"`
}

// errResult builds an error CallToolResult, always routing the message
// through the Redactor. EVERY error returned by a tool goes through this
// helper. When err wraps a classified *icloud.Error, the payload is JSON
// with a stable "code" field so agents can match without parsing English text.
// Raw HTTP/XML bodies are never included.
func errResult(red *security.Redactor, context string, err error) *mcp.CallToolResult {
	msg := red.Redact(fmt.Sprintf("%s: %v", context, err))
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
			payload.Details = ie.Details
		}
	} else if context == "validation" || context == "validation error" {
		payload.Code = string(icloud.CodeValidation)
	}
	b, mErr := json.Marshal(payload)
	if mErr != nil {
		return mcp.NewToolResultError(msg)
	}
	return mcp.NewToolResultError(string(b))
}

// writeJSON serializes payload as indented JSON and builds a success
// CallToolResult. A serialization failure is itself routed through errResult.
func writeJSON(red *security.Redactor, payload any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return errResult(red, "formatting response", err)
	}
	return mcp.NewToolResultText(string(b))
}
