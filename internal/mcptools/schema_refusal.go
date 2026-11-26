package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// The MCP SDK checks the input schema before any middleware or handler. It
// returns one plain text item that starts with one of these prefixes.
const (
	sdkSchemaRefusalPrefix    = "input schema validation failed: "
	sdkArgumentsRefusalPrefix = "invalid arguments: "
)

const schemaRefusalSummary = "arguments do not match the input schema"

// ConfigureSchemaRefusals rewrites each SDK input schema refusal into the
// project error payload: code validation, not retryable, and a fixed message.
// The message names only declared properties. It never repeats a value, an
// unknown key, or the SDK text. Handler results are JSON payloads and are
// never changed.
func ConfigureSchemaRefusals(s *server.MCPServer, red *security.Redactor) {
	if red == nil {
		panic("mcptools: schema refusals require a non-nil redactor")
	}
	hooks := s.GetHooks()
	if hooks == nil {
		hooks = &server.Hooks{}
		server.WithHooks(hooks)(s)
	}
	hooks.AddAfterCallTool(func(_ context.Context, _ any, request *mcp.CallToolRequest, result any) {
		res, ok := result.(*mcp.CallToolResult)
		if !ok || request == nil || !isSDKSchemaRefusal(res) {
			return
		}
		detail := strings.TrimPrefix(res.Content[0].(mcp.TextContent).Text, sdkSchemaRefusalPrefix)
		message := schemaRefusalMessage(s.GetTool(request.Params.Name), request, detail)
		*res = *errResult(red, "validation", errors.New(message))
	})
}

// isSDKSchemaRefusal accepts only the exact SDK refusal shape: an error with
// one plain text item, no structured content, and an SDK prefix.
func isSDKSchemaRefusal(res *mcp.CallToolResult) bool {
	if res == nil || !res.IsError || res.StructuredContent != nil || res.Meta != nil || len(res.Content) != 1 {
		return false
	}
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok || text.Annotations != nil || text.Meta != nil || text.Type != mcp.ContentTypeText {
		return false
	}
	return strings.HasPrefix(text.Text, sdkSchemaRefusalPrefix) || strings.HasPrefix(text.Text, sdkArgumentsRefusalPrefix)
}

// schemaRefusalMessage names the declared properties that failed. Missing and
// unknown keys come from the request itself. Other failed properties come from
// the first segment of each SDK location, kept only when the tool declares it.
func schemaRefusalMessage(tool *server.ServerTool, request *mcp.CallToolRequest, detail string) string {
	properties, required := declaredArguments(tool)
	args, object := request.Params.Arguments.(map[string]any)
	if request.Params.Arguments != nil && !object {
		return schemaRefusalSummary + ": arguments must be a JSON object"
	}
	missing := map[string]bool{}
	for _, name := range required {
		if _, present := args[name]; !present {
			missing[name] = true
		}
	}
	unknown := false
	for name := range args {
		if !properties[name] {
			unknown = true
		}
	}
	invalid := map[string]bool{}
	for _, part := range strings.Split(detail, "; ") {
		location, rule, _ := strings.Cut(part, ": ")
		if strings.Contains(rule, "additional properties") {
			unknown = true
		}
		if !strings.HasPrefix(location, "/") {
			continue
		}
		segment, _, _ := strings.Cut(location[1:], "/")
		segment = strings.NewReplacer("~1", "/", "~0", "~").Replace(segment)
		if properties[segment] && !missing[segment] {
			invalid[segment] = true
		}
	}
	var clauses []string
	if len(missing) > 0 {
		clauses = append(clauses, "missing required argument "+sortedNames(missing))
	}
	if len(invalid) > 0 {
		clauses = append(clauses, "invalid value for "+sortedNames(invalid))
	}
	if unknown {
		clauses = append(clauses, "unknown argument")
	}
	if len(clauses) == 0 {
		return schemaRefusalSummary
	}
	return schemaRefusalSummary + ": " + strings.Join(clauses, "; ")
}

// declaredArguments returns the top-level property names and the required
// names of a tool input schema.
func declaredArguments(tool *server.ServerTool) (map[string]bool, []string) {
	properties := map[string]bool{}
	if tool == nil {
		return properties, nil
	}
	if len(tool.Tool.RawInputSchema) > 0 {
		var raw struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if json.Unmarshal(tool.Tool.RawInputSchema, &raw) != nil {
			return properties, nil
		}
		for name := range raw.Properties {
			properties[name] = true
		}
		return properties, raw.Required
	}
	for name := range tool.Tool.InputSchema.Properties {
		properties[name] = true
	}
	return properties, tool.Tool.InputSchema.Required
}

func sortedNames(set map[string]bool) string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
