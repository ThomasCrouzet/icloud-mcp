package mcptools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// RecoverRedactMiddleware catches panics from tool handlers and returns a
// redacted error CallToolResult.
//
// RedactingWriter protects stderr only. A panic can contain a secret and reach
// stdout through server.WithRecovery. This middleware redacts the panic before
// JSON-RPC serializes it. See redaction_test.go for the hostile error case.
//
// Register this middleware after the other server middlewares. Its recover
// call must be closest to the handler. Keep server.WithRecovery as a second
// safety layer. See cmd/icloud-mcp/main.go for the registration order.
func RecoverRedactMiddleware(red *security.Redactor) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
			defer func() {
				if r := recover(); r != nil {
					// Do not send panic text to stdout when the redactor is missing.
					if red == nil {
						result = mcp.NewToolResultError(`{"code":"internal_error","message":"internal error"}`)
					} else {
						result = errResult(red, "internal error", fmt.Errorf("%v", r))
					}
					err = nil
				}
			}()
			return next(ctx, req)
		}
	}
}
