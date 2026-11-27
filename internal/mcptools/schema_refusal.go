package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

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
		message := schemaRefusalMessage(s.GetTool(request.Params.Name), request)
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

// schemaRefusalMessage names the declared arguments that failed. It never
// parses the SDK text, because that text embeds caller values. Missing and
// unknown keys come from the request keys. Invalid arguments come from the
// instance locations of a second validation of the same input against the
// same schema, kept only when the tool declares the first segment.
func schemaRefusalMessage(tool *server.ServerTool, request *mcp.CallToolRequest) string {
	properties, required := declaredArguments(tool)
	instance, err := schemaInstance(request.GetRawArguments())
	if err != nil {
		return schemaRefusalSummary
	}
	args, object := instance.(map[string]any)
	if !object {
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
	var verr *jsonschema.ValidationError
	if schema := compileInputSchema(tool); schema != nil && errors.As(schema.Validate(instance), &verr) {
		walkSchemaFailures(verr, func(location []string, failure jsonschema.ErrorKind) {
			if len(location) == 0 {
				return
			}
			if _, nested := failure.(*kind.AdditionalProperties); nested {
				unknown = true
			}
			if properties[location[0]] && !missing[location[0]] {
				invalid[location[0]] = true
			}
		})
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

// schemaInstance decodes the arguments as the SDK does. Absent or null
// arguments are an empty object, and numbers keep their exact form.
func schemaInstance(raw any) (any, error) {
	data, ok := raw.(json.RawMessage)
	if !ok {
		if raw == nil {
			return map[string]any{}, nil
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, err
		}
		data = encoded
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return map[string]any{}, nil
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(data))
}

// compileInputSchema compiles the tool input schema with the validator that
// the SDK uses. It returns nil when the schema cannot be compiled.
func compileInputSchema(tool *server.ServerTool) *jsonschema.Schema {
	if tool == nil {
		return nil
	}
	data := []byte(tool.Tool.RawInputSchema)
	if len(data) == 0 {
		encoded, err := json.Marshal(tool.Tool.InputSchema)
		if err != nil {
			return nil
		}
		data = encoded
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	const url = "mem:///icloud-mcp/input-schema.json"
	compiler := jsonschema.NewCompiler()
	if compiler.AddResource(url, document) != nil {
		return nil
	}
	schema, err := compiler.Compile(url)
	if err != nil {
		return nil
	}
	return schema
}

// walkSchemaFailures calls visit for each leaf failure.
func walkSchemaFailures(verr *jsonschema.ValidationError, visit func([]string, jsonschema.ErrorKind)) {
	if len(verr.Causes) == 0 {
		visit(verr.InstanceLocation, verr.ErrorKind)
		return
	}
	for _, cause := range verr.Causes {
		walkSchemaFailures(cause, visit)
	}
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
