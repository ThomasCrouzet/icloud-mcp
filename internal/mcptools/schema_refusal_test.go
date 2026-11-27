package mcptools

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var probeNames atomic.Int64

// uniqueProbeName keeps each test run out of the process-wide schema cache.
func uniqueProbeName(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, probeNames.Add(1))
}

func schemaRefusalRequest(t *testing.T, name string, args map[string]any) *mcp.CallToolRequest {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	request := &mcp.CallToolRequest{}
	request.Params.Name = name
	request.Params.Arguments = args
	request.Params.RawArguments = raw
	return request
}

// TestSchemaRefusalCompilesEachSchemaOnce proves the compiled schema cache:
// repeated refusals for one tool compile its schema once.
func TestSchemaRefusalCompilesEachSchemaOnce(t *testing.T) {
	tool := &server.ServerTool{Tool: mcp.NewTool(uniqueProbeName("cache_probe"), mcp.WithNumber("count", mcp.Required()))}
	request := schemaRefusalRequest(t, tool.Tool.Name, map[string]any{"count": "x"})
	before := schemaCompilations.Load()
	for range 3 {
		if got, want := schemaRefusalMessage(tool, request), schemaRefusalSummary+": invalid value for count"; got != want {
			t.Fatalf("message = %q, want %q", got, want)
		}
	}
	if compiled := schemaCompilations.Load() - before; compiled != 1 {
		t.Fatalf("schema compiled %d times for 3 refusals, want 1", compiled)
	}
	// A changed schema under the same name gets its own entry.
	changed := &server.ServerTool{Tool: mcp.NewTool(tool.Tool.Name, mcp.WithNumber("count", mcp.Required()), mcp.WithString("extra"))}
	_ = schemaRefusalMessage(changed, request)
	if compiled := schemaCompilations.Load() - before; compiled != 2 {
		t.Fatalf("changed schema compiled %d times in total, want 2", compiled)
	}
}

// TestSchemaRefusalSkipsLargeArguments keeps the request-derived clauses and
// skips the second validation above the raw size bound.
func TestSchemaRefusalSkipsLargeArguments(t *testing.T) {
	tool := &server.ServerTool{Tool: mcp.NewTool(uniqueProbeName("size_probe"), mcp.WithString("text", mcp.Required()), mcp.WithNumber("count", mcp.Required()))}
	before := schemaCompilations.Load()
	large := schemaRefusalRequest(t, tool.Tool.Name, map[string]any{"count": strings.Repeat("x", maxSchemaRevalidationBytes), "other": 1})
	if got, want := schemaRefusalMessage(tool, large), schemaRefusalSummary+": missing required argument text; unknown argument"; got != want {
		t.Fatalf("large message = %q, want %q", got, want)
	}
	if compiled := schemaCompilations.Load() - before; compiled != 0 {
		t.Fatalf("large arguments compiled the schema %d times", compiled)
	}
	// Just below the bound, the second validation still names count.
	small := schemaRefusalRequest(t, tool.Tool.Name, map[string]any{"count": strings.Repeat("x", maxSchemaRevalidationBytes-64), "other": 1})
	if got, want := schemaRefusalMessage(tool, small), schemaRefusalSummary+": missing required argument text; invalid value for count; unknown argument"; got != want {
		t.Fatalf("small message = %q, want %q", got, want)
	}
}

// TestSchemaRefusalWithoutDeclaredArguments returns the bare summary when the
// tool schema cannot be read, instead of calling every key unknown.
func TestSchemaRefusalWithoutDeclaredArguments(t *testing.T) {
	request := schemaRefusalRequest(t, "missing_tool", map[string]any{"a": 1})
	unreadable := &server.ServerTool{Tool: mcp.NewToolWithRawSchema("raw_probe", "", json.RawMessage(`{"properties":`))}
	for name, tool := range map[string]*server.ServerTool{"nil tool": nil, "unreadable schema": unreadable} {
		if got := schemaRefusalMessage(tool, request); got != schemaRefusalSummary {
			t.Errorf("%s: message = %q, want %q", name, got, schemaRefusalSummary)
		}
	}
}

// TestSchemaRefusalCacheIsConcurrencySafe runs refusals in parallel. Run it
// with -race. Concurrent first calls can compile more than once, and all of
// them must give the same message.
func TestSchemaRefusalCacheIsConcurrencySafe(t *testing.T) {
	tool := &server.ServerTool{Tool: mcp.NewTool(uniqueProbeName("parallel_probe"), mcp.WithNumber("count", mcp.Required()))}
	request := schemaRefusalRequest(t, tool.Tool.Name, map[string]any{"count": "x"})
	want := schemaRefusalSummary + ": invalid value for count"
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for range 16 {
		wg.Go(func() {
			if got := schemaRefusalMessage(tool, request); got != want {
				errs <- got
			}
		})
	}
	wg.Wait()
	close(errs)
	for got := range errs {
		t.Errorf("parallel message = %q, want %q", got, want)
	}
}
