// Package mcptools defines the 5 MCP tools exposed by the server and their
// handlers. All input validation and audit logging live here (protocol
// layer); network access lives in internal/icloud.
package mcptools

import (
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// errResult builds an error CallToolResult, always routing the message
// through the Redactor. EVERY error returned by a tool goes through this
// helper; it is one of the 3 redaction insertion points (see
// internal/security).
func errResult(red *security.Redactor, context string, err error) *mcp.CallToolResult {
	return mcp.NewToolResultError(red.Redact(fmt.Sprintf("%s: %v", context, err)))
}

// writeJSON serializes payload as indented JSON and builds a success
// CallToolResult. A serialization failure (unlikely internal case) is
// itself routed through errResult, for consistency: every tool error goes
// through redaction.
func writeJSON(red *security.Redactor, payload any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return errResult(red, "formatting response", err)
	}
	return mcp.NewToolResultText(string(b))
}
