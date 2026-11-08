package mcptools

import (
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestMutationAnnotationsAreConservative prevents risk hints from drifting
// when a mutation tool gains fields that clear or replace data. A tool is
// marked destructive when a repeated or partial call can remove or replace
// user data. It is marked idempotent only with a documented repeat-call
// postcondition.
func TestMutationAnnotationsAreConservative(t *testing.T) {
	defaultLoc := time.UTC
	tools := []mcp.Tool{
		newCreateEventTool(defaultLoc),
		newUpdateEventTool(defaultLoc),
		newDeleteEventTool(defaultLoc),
		newCreateContactTool(),
		newUpdateContactTool(),
		newDeleteContactTool(),
		newSetMessageFlagsTool(),
		newMoveMessageTool(),
		newTrashMessageTool(),
		newSendMessageTool(),
		newReplyMessageTool(),
	}
	want := map[string]struct {
		destructive bool
		idempotent  bool
	}{
		"create_event":      {false, false},
		"update_event":      {true, false},
		"delete_event":      {true, true},
		"create_contact":    {false, false},
		"update_contact":    {true, false},
		"delete_contact":    {true, true},
		"set_message_flags": {false, false},
		"move_message":      {true, false},
		"trash_message":     {true, false},
		"send_message":      {true, false},
		"reply_message":     {true, false},
	}
	if len(tools) != len(want) {
		t.Fatalf("mutation tool count = %d, want %d", len(tools), len(want))
	}
	for _, tool := range tools {
		expected, ok := want[tool.Name]
		if !ok {
			t.Fatalf("unreviewed mutation tool %q", tool.Name)
		}
		if tool.Annotations.ReadOnlyHint == nil || *tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must declare readOnlyHint=false", tool.Name)
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != expected.destructive {
			t.Errorf("%s destructiveHint = %v, want %v", tool.Name, tool.Annotations.DestructiveHint, expected.destructive)
		}
		if tool.Annotations.IdempotentHint == nil || *tool.Annotations.IdempotentHint != expected.idempotent {
			t.Errorf("%s idempotentHint = %v, want %v", tool.Name, tool.Annotations.IdempotentHint, expected.idempotent)
		}
	}
}
