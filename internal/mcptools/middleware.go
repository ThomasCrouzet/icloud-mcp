package mcptools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// RecoverRedactMiddleware intercepts panics from a tool handler and produces
// a REDACTED error CallToolResult, instead of letting the panic bubble up as
// (nil, err) to the JSON-RPC protocol channel.
//
// That channel (stdout) is NOT covered by the RedactingWriter, which only
// wraps stderr (slog logs + audit). Without this middleware, a panic
// carrying the password (e.g. an HTTP error that echoes the credentials in
// its message, see redaction_test.go) would leak the secret verbatim in the
// JSON-RPC response returned to the MCP caller: server.WithRecovery() does
// convert the panic into a Go error, but that error is then serialized as is
// (err.Error()) into the JSON-RPC message; NO redaction happens on that
// path.
//
// server.WithRecovery() stays in place as an extra safety net (defense in
// depth), but THIS middleware must intercept the panic FIRST to produce a
// redacted response: it must therefore be registered AFTER the other
// middlewares on the server.NewMCPServer side (see cmd/icloud-mcp/main.go),
// so that it sits closest to the handler in the call stack. The recover()
// closest to the panic wins during unwind, so the outer middlewares
// (including WithRecovery) never see anything propagate.
func RecoverRedactMiddleware(red *security.Redactor) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
			defer func() {
				if r := recover(); r != nil {
					result = errResult(red, "internal error", fmt.Errorf("%v", r))
					err = nil
				}
			}()
			return next(ctx, req)
		}
	}
}
