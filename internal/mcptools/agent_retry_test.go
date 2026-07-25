package mcptools

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// agentRetryDecision models how a careful MCP agent should react to a tool error.
type agentRetryDecision string

const (
	agentRetry     agentRetryDecision = "retry"
	agentBackoff   agentRetryDecision = "backoff"
	agentAbort     agentRetryDecision = "abort"
	agentReconcile agentRetryDecision = "reconcile"
)

func decideAgentRetry(payload toolErrorPayload) agentRetryDecision {
	switch payload.Code {
	case string(icloud.CodeRateLimited), string(icloud.CodeUnavailable):
		if payload.Retryable {
			return agentBackoff
		}
		return agentAbort
	case string(icloud.CodeTimeout):
		if payload.Retryable {
			return agentRetry
		}
		return agentReconcile
	case string(icloud.CodeAuthentication), string(icloud.CodeAuthorization),
		string(icloud.CodeValidation), string(icloud.CodeNotFound),
		string(icloud.CodeConflict), string(icloud.CodeConcurrentModification):
		return agentAbort
	case string(icloud.CodeOutcomeUnknown):
		return agentReconcile
	default:
		if payload.Retryable {
			return agentRetry
		}
		return agentAbort
	}
}

func TestAgentRetrySimulation_StructuredCodes(t *testing.T) {
	red := security.NewRedactor()
	cases := []struct {
		name string
		err  error
		want agentRetryDecision
		code string
	}{
		{
			name: "rate_limited",
			err: &icloud.Error{
				Code: icloud.CodeRateLimited, Retryable: true, RetryAfter: 5 * time.Second,
				Message: "rate limited",
			},
			want: agentBackoff,
			code: "rate_limited",
		},
		{
			name: "auth",
			err:  icloud.NewError(icloud.CodeAuthenticationRefused, 401, "bad password", nil),
			want: agentAbort,
			code: "authentication",
		},
		{
			name: "conflict_412",
			err: &icloud.Error{
				Code: icloud.CodeConcurrentModification, Message: "etag mismatch",
			},
			want: agentAbort,
			code: "concurrent_modification",
		},
		{
			name: "timeout_read",
			err:  icloud.NewError(icloud.CodeTimeout, 0, "timed out", nil),
			want: agentReconcile,
			code: "timeout",
		},
		{
			name: "outcome_unknown",
			err: &icloud.Error{
				Code: icloud.CodeOutcomeUnknown, Message: "unknown",
				Details: map[string]string{"reconciliation": "re-read before retry"},
			},
			want: agentReconcile,
			code: "outcome_unknown",
		},
		{
			name: "unavailable",
			err: &icloud.Error{
				Code: icloud.CodeServerUnavailable, Retryable: true, RetryAfter: 2 * time.Second,
				Message: "shard down",
			},
			want: agentBackoff,
			code: "unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := errResult(red, "op", tc.err)
			payload := parseToolError(t, res)
			if payload.Code != tc.code {
				t.Fatalf("code = %q, want %q (payload=%+v)", payload.Code, tc.code, payload)
			}
			if got := decideAgentRetry(payload); got != tc.want {
				t.Fatalf("decision = %s, want %s (payload=%+v)", got, tc.want, payload)
			}
			if tc.code == "rate_limited" && payload.RetryAfter <= 0 {
				t.Fatalf("rate_limited must include retry_after_seconds, got %+v", payload)
			}
		})
	}
}

func parseToolError(t *testing.T, res *mcp.CallToolResult) toolErrorPayload {
	t.Helper()
	if res == nil || !res.IsError || len(res.Content) == 0 {
		t.Fatal("expected error result")
	}
	textContent, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content type %T", res.Content[0])
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(textContent.Text), &payload); err != nil {
		t.Fatalf("payload not JSON: %v\n%s", err, textContent.Text)
	}
	return payload
}

func TestIdempotencyStore_SameKeySameParams(t *testing.T) {
	store := newIdempotencyStore()
	hash, err := hashIdempotencyParams(map[string]string{"a": "1"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, ready := store.begin("k1", hash)
	if !ready {
		t.Fatal("expected ready")
	}
	store.complete("k1", hash, `{"success":true}`)
	payload, conflict, hit, ready := store.begin("k1", hash)
	if !hit || conflict || ready || payload != `{"success":true}` {
		t.Fatalf("begin hit = %q conflict=%v hit=%v ready=%v", payload, conflict, hit, ready)
	}
	other, err := hashIdempotencyParams(map[string]string{"a": "2"})
	if err != nil {
		t.Fatal(err)
	}
	_, conflict, hit, _ = store.begin("k1", other)
	if !hit || !conflict {
		t.Fatalf("expected conflict on different params")
	}
}

func TestErrResult_PropagatesRetryAfter(t *testing.T) {
	red := security.NewRedactor()
	res := errResult(red, "op", &icloud.Error{
		Code: icloud.CodeRateLimited, Retryable: true, RetryAfter: 12 * time.Second,
		Message: "slow down",
	})
	payload := parseToolError(t, res)
	if payload.RetryAfter != 12 {
		t.Fatalf("retry_after_seconds = %d, want 12", payload.RetryAfter)
	}
	if !payload.Retryable {
		t.Fatal("expected retryable")
	}
}
