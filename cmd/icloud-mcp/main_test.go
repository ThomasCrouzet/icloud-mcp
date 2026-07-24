package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestVersionNonEmpty(t *testing.T) {
	if version == "" {
		t.Fatal("version must be non-empty (overridden at release via -ldflags)")
	}
}

func TestTimeoutMiddleware(t *testing.T) {
	mw := timeoutMiddleware(30 * time.Millisecond)
	slow := mw(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
			return mcp.NewToolResultText("late"), nil
		}
	})
	_, err := slow(context.Background(), mcp.CallToolRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestTimeoutMiddlewarePasses(t *testing.T) {
	mw := timeoutMiddleware(time.Second)
	h := mw(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("expected deadline on context")
		}
		return mcp.NewToolResultText("ok"), nil
	})
	res, err := h(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("expected result content")
	}
}
