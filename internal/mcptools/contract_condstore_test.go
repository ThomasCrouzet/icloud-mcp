package mcptools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// The adapter observes the tagged status of the guarded STORE it sent, so a
// conditional flag mutation is provable on a CONDSTORE server. This contract
// keeps the #205 guarantee at the tool boundary: only the tagged OK plus the
// returned FETCH for that UID reports a conditional success. A tagged MODIFIED
// is concurrent_modification, and a dispatched but unverifiable STORE is
// outcome_unknown. Neither ever returns a success, and neither is retried.
//
// The non-CONDSTORE delta path (#208) is unchanged.
func TestContractRealConditionalStore(t *testing.T) {
	t.Run("proven success", func(t *testing.T) {
		f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
			f.mail.condstore = true
			f.mail.modseq = 42
			f.mail.flags = "\\Seen"
		})
		args := f.arguments("set_message_flags", "")
		args["expected_modseq"] = 42
		result := f.call(t, "set_message_flags", args)
		f.mail.mu.Lock()
		stores := f.mail.stores
		f.mail.mu.Unlock()
		if stores != 1 || result.IsError {
			t.Fatalf("proven conditional STORE: stores=%d error=%v", stores, result.IsError)
		}
		if !contractConditionalUpdate(t, result) {
			t.Fatal("proven conditional STORE did not report conditionalUpdate")
		}
	})

	t.Run("missing precondition", func(t *testing.T) {
		f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
			f.mail.condstore = true
			f.mail.modseq = 42
		})
		args := f.arguments("set_message_flags", "")
		result := f.call(t, "set_message_flags", args)
		f.mail.mu.Lock()
		stores := f.mail.stores
		f.mail.mu.Unlock()
		if stores != 0 {
			t.Fatalf("a conditional STORE without expected_modseq was dispatched %d time(s)", stores)
		}
		if !result.IsError || !contractErrorCode(t, result, "validation") {
			t.Fatal("a conditional STORE without expected_modseq did not fail as validation")
		}
	})

	t.Run("stale modseq", func(t *testing.T) {
		f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
			f.mail.condstore = true
			f.mail.modseq = 42
			f.mail.modifiedStore = true
		})
		args := f.arguments("set_message_flags", "")
		args["expected_modseq"] = 42
		result := f.call(t, "set_message_flags", args)
		f.mail.mu.Lock()
		stores := f.mail.stores
		f.mail.mu.Unlock()
		if stores != 1 {
			t.Fatalf("stale MODSEQ sent %d guarded STORE commands, want 1", stores)
		}
		if !result.IsError || !contractErrorCode(t, result, "concurrent_modification") {
			t.Fatal("a tagged MODIFIED did not report concurrent_modification")
		}
	})

	t.Run("unverifiable return", func(t *testing.T) {
		f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
			f.mail.condstore = true
			f.mail.modseq = 42
			f.mail.malformedStore = true
		})
		args := f.arguments("set_message_flags", "")
		args["expected_modseq"] = 42
		result := f.call(t, "set_message_flags", args)
		f.mail.mu.Lock()
		stores := f.mail.stores
		f.mail.mu.Unlock()
		if stores != 1 {
			t.Fatalf("unverifiable STORE sent %d guarded STORE commands, want 1", stores)
		}
		if !result.IsError || !contractErrorCode(t, result, "outcome_unknown") {
			t.Fatal("an unverifiable FETCH did not report outcome_unknown")
		}
	})
}

// TestContractNoUnconditionalFallbackOnCondStore proves that a CONDSTORE
// server never receives a delta STORE without UNCHANGEDSINCE, whatever the
// guarded outcome was.
func TestContractNoUnconditionalFallbackOnCondStore(t *testing.T) {
	for name, setup := range map[string]func(*contractRuntime){
		"stale":        func(f *contractRuntime) { f.mail.modifiedStore = true },
		"unverifiable": func(f *contractRuntime) { f.mail.malformedStore = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
				f.mail.condstore = true
				f.mail.modseq = 42
				setup(f)
			})
			args := f.arguments("set_message_flags", "")
			args["expected_modseq"] = 42
			f.call(t, "set_message_flags", args)
			f.mail.mu.Lock()
			commands := append([]string(nil), f.mail.commands...)
			f.mail.mu.Unlock()
			stores := 0
			for _, command := range commands {
				if !containsFold(command, " STORE ") {
					continue
				}
				stores++
				if !containsFold(command, "UNCHANGEDSINCE") {
					t.Fatalf("an unconditional STORE reached a CONDSTORE server: %q", command)
				}
			}
			if stores > 1 {
				t.Fatalf("the guarded STORE was replayed %d times", stores)
			}
		})
	}
}

// contractResultJSON returns the structured payload of one tool result.
func contractResultJSON(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	if result == nil || len(result.Content) == 0 {
		t.Fatalf("empty tool result: %#v", result)
	}
	text, ok := mcp.AsTextContent(result.Content[0])
	if !ok {
		t.Fatalf("non-text tool result: %#v", result.Content[0])
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
		t.Fatalf("decode tool payload: %v", err)
	}
	return payload
}

func contractConditionalUpdate(t *testing.T, result *mcp.CallToolResult) bool {
	t.Helper()
	update, ok := contractResultJSON(t, result)["conditionalUpdate"].(bool)
	return ok && update
}

func contractErrorCode(t *testing.T, result *mcp.CallToolResult, want string) bool {
	t.Helper()
	code, _ := contractResultJSON(t, result)["code"].(string)
	return code == want
}

func containsFold(value, substring string) bool {
	return strings.Contains(strings.ToUpper(value), strings.ToUpper(substring))
}

func TestContractFullRangeConditionalStore(t *testing.T) {
	for _, token := range []string{"18446744073709551615", "184467440737095516150e-1", "18446744073709551614"} {
		t.Run(token, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
				f.mail.condstore = true
				f.mail.modseq = 42
			})
			args := f.arguments("set_message_flags", "")
			args["expected_modseq"] = token
			result := f.call(t, "set_message_flags", args)
			f.mail.mu.Lock()
			stores := f.mail.stores
			f.mail.mu.Unlock()
			if stores != 0 || !result.IsError {
				t.Fatalf("an out-of-range MODSEQ was not refused: stores=%d error=%v", stores, result.IsError)
			}
		})
	}
}
