package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// newReq builds a mcp.CallToolRequest with the given arguments, a minimal
// equivalent of a real call over the stdio transport.
func newReq(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func testDeps(svc *icloud.MockService) Deps {
	return Deps{
		Service:  svc,
		Audit:    security.NewAuditLogger(&discardWriter{}),
		Redactor: security.NewRedactor("SENTINEL-PW-unused"),
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestListCalendarsHandler_HappyPath(t *testing.T) {
	svc := &icloud.MockService{Calendars: []icloud.Calendar{
		{Path: "/cal/home/", Name: "Home", Color: "#FF2968FF"},
	}}
	handler := listCalendarsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(nil))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}

	var payload struct {
		Count     int               `json:"count"`
		Calendars []icloud.Calendar `json:"calendars"`
	}
	decodeResult(t, res, &payload)
	if payload.Count != 1 || len(payload.Calendars) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Calendars[0].Name != "Home" {
		t.Errorf("Name = %q", payload.Calendars[0].Name)
	}
}

func TestListCalendarsHandler_ServiceError(t *testing.T) {
	svc := &icloud.MockService{ListErr: fmt.Errorf("network failure")}
	handler := listCalendarsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(nil))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(resultText(t, res), "network failure") {
		t.Errorf("expected error message containing 'network failure': %s", resultText(t, res))
	}
}

// --- Helpers shared across this package's test files ----------------------

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	tc, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("non-textual content: %#v", res.Content[0])
	}
	return tc.Text
}

func decodeResult(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	text := resultText(t, res)
	if err := json.Unmarshal([]byte(text), v); err != nil {
		t.Fatalf("decoding JSON response: %v\ncontent: %s", err, text)
	}
}
