package mcptools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// TestErrResult_StructuredCodeForClassifiedError: a classified CalDAV error
// must surface its stable code as JSON so agents can branch on it without
// parsing the English message.
func TestErrResult_StructuredCodeForClassifiedError(t *testing.T) {
	red := testDeps(&icloud.MockService{}).Redactor
	err := icloud.NewError(icloud.CodeConcurrentModification, 412, "etag mismatch", nil)
	res := errResult(red, "updating event", err)
	if !res.IsError {
		t.Fatal("expected error result")
	}
	text := resultText(t, res)
	var payload toolErrorPayload
	if jerr := json.Unmarshal([]byte(text), &payload); jerr != nil {
		t.Fatalf("error payload is not JSON: %v\n%s", jerr, text)
	}
	if payload.Code != string(icloud.CodeConcurrentModification) {
		t.Errorf("code = %q, want %q", payload.Code, icloud.CodeConcurrentModification)
	}
	if !strings.Contains(payload.Message, "updating event") {
		t.Errorf("message = %q, want context prefix", payload.Message)
	}
}
