package mcptools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// Deterministic offline MCP benchmark scenarios. They reuse the accepted
// availability fixture and exercise the real handlers, validation, public
// result cap, materialization cap, cancellation, and error serialization.
const (
	mcpBenchmarkSeed       = "mcp-index-v1"
	mcpBenchmarkRangeStart = "2026-07-01T09:00:00Z"
	mcpBenchmarkRangeEnd   = "2026-07-01T14:00:00Z"
)

func mcpBenchmarkArgs(paths []string, extra map[string]any) map[string]any {
	args := map[string]any{
		"start":     mcpBenchmarkRangeStart,
		"end":       mcpBenchmarkRangeEnd,
		"calendars": strings.Join(paths, ","),
		"limit":     icloud.MaxResults,
	}
	for key, value := range extra {
		args[key] = value
	}
	return args
}

func mcpBenchmarkResultText(tb testing.TB, result *mcp.CallToolResult) string {
	tb.Helper()
	if result == nil || len(result.Content) == 0 {
		tb.Fatal("benchmark result has no content")
	}
	text, ok := mcp.AsTextContent(result.Content[0])
	if !ok {
		tb.Fatalf("benchmark result is not text: %#v", result.Content[0])
	}
	return text.Text
}

func mcpBenchmarkSearch(tb testing.TB, service icloud.Service, args map[string]any) (*mcp.CallToolResult, string) {
	tb.Helper()
	handler := searchEventsHandler(freeSlotsTestDeps(service))
	result, err := handler(context.Background(), newReq(args))
	if err != nil {
		tb.Fatalf("search_events handler: %v", err)
	}
	return result, mcpBenchmarkResultText(tb, result)
}

func mcpBenchmarkAssertSuccess(tb testing.TB, service icloud.Service, paths []string, wantTotal, wantCount int) {
	tb.Helper()
	result, text := mcpBenchmarkSearch(tb, service, mcpBenchmarkArgs(paths, nil))
	if result.IsError {
		tb.Fatalf("search_events error: %s", text)
	}
	var payload searchEventsResponse
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		tb.Fatalf("decode search_events: %v", err)
	}
	if payload.Total != wantTotal || payload.Count != wantCount || len(payload.Events) != wantCount {
		tb.Fatalf("search_events payload total=%d count=%d events=%d, want total=%d count=%d",
			payload.Total, payload.Count, len(payload.Events), wantTotal, wantCount)
	}
	if wantTotal > wantCount && !payload.Truncated {
		tb.Fatalf("overflow to the public cap did not report truncation: %+v", payload)
	}
}

func mcpBenchmarkAssertError(tb testing.TB, service icloud.Service, paths []string, wantCode string) string {
	tb.Helper()
	result, text := mcpBenchmarkSearch(tb, service, mcpBenchmarkArgs(paths, nil))
	if !result.IsError {
		tb.Fatalf("search_events unexpectedly succeeded: %s", text)
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		tb.Fatalf("decode search_events error: %v", err)
	}
	if wantCode != "" && payload.Code != wantCode {
		tb.Fatalf("error code = %q, want %q: %s", payload.Code, wantCode, text)
	}
	return text
}

func searchEventsBenchmarkFixture(tb testing.TB) (*availabilityBudgetService, []string) {
	tb.Helper()
	service, paths := availabilityBudgetFixture([]int{2500, 2500, 2500, 2500})
	if len(paths) != 4 {
		tb.Fatalf("calendar paths = %d, want 4", len(paths))
	}
	return service, paths
}

func TestMCPBenchmarkScenarios(t *testing.T) {
	t.Logf("fixture_seed=%s materialization_cap=%d", mcpBenchmarkSeed, icloud.MaxMultiSearchMaterialized)
	service, paths := searchEventsBenchmarkFixture(t)
	total := 2500 * len(paths)
	mcpBenchmarkAssertSuccess(t, service, paths, total, icloud.MaxResults)
	if calls := len(service.snapshot()); calls != len(paths) {
		t.Fatalf("success RPCs = %d, want %d", calls, len(paths))
	}

	overflow, overflowPaths := availabilityBudgetFixture([]int{2500, 2500, 2500, 2500, 1})
	if text := mcpBenchmarkAssertError(t, overflow, overflowPaths, "payload_too_large"); !strings.Contains(text, "narrow") {
		t.Fatalf("overflow error lacks guidance: %s", text)
	}
	if calls := len(overflow.snapshot()); calls != len(overflowPaths) {
		t.Fatalf("overflow RPCs = %d, want %d", calls, len(overflowPaths))
	}

	validation, validationPaths := searchEventsBenchmarkFixture(t)
	args := mcpBenchmarkArgs(validationPaths, nil)
	args["limit"] = icloud.MaxResults + 1
	handler := searchEventsHandler(freeSlotsTestDeps(validation))
	result, err := handler(context.Background(), newReq(args))
	if err != nil || !result.IsError {
		t.Fatalf("validation refusal result = %v, %v", result, err)
	}
	if calls := len(validation.snapshot()); calls != 0 {
		t.Fatalf("validation RPCs = %d, want 0", calls)
	}

	rateLimited, ratePaths := searchEventsBenchmarkFixture(t)
	rateLimited.read = func(context.Context, string) (icloud.SearchResult, error) {
		return icloud.SearchResult{}, icloud.NewError(icloud.CodeRateLimited, 429, "synthetic rate limit", nil)
	}
	mcpBenchmarkAssertError(t, rateLimited, ratePaths[:1], "rate_limited")

	canceled, cancelPaths := searchEventsBenchmarkFixture(t)
	canceled.read = func(ctx context.Context, _ string) (icloud.SearchResult, error) {
		<-ctx.Done()
		return icloud.SearchResult{}, ctx.Err()
	}
	cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel()
	cancelHandler := searchEventsHandler(freeSlotsTestDeps(canceled))
	cancelResult, err := cancelHandler(cancelCtx, newReq(mcpBenchmarkArgs(cancelPaths, nil)))
	if err != nil || !cancelResult.IsError {
		t.Fatalf("cancellation result = %v, %v", cancelResult, err)
	}
	if text := mcpBenchmarkResultText(t, cancelResult); !strings.Contains(text, "deadline") && !strings.Contains(text, "canceled") {
		t.Fatalf("cancellation text = %s", text)
	}
}

func BenchmarkSearchEventsSerialization(b *testing.B) {
	service, paths := searchEventsBenchmarkFixture(b)
	total := 2500 * len(paths)
	mcpBenchmarkAssertSuccess(b, service, paths, total, icloud.MaxResults)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		mcpBenchmarkAssertSuccess(b, service, paths, total, icloud.MaxResults)
	}
	b.StopTimer()
	b.ReportMetric(float64(len(paths)), "rpc/op")
	b.ReportMetric(float64(calendarReadConcurrency), "worker-limit")
	b.ReportMetric(float64(total), "materialized-events")
	b.ReportMetric(float64(icloud.MaxResults), "result-cap")
}

func BenchmarkSearchEventsRefusals(b *testing.B) {
	b.Run("MaterializationOverflow", func(b *testing.B) {
		service, paths := availabilityBudgetFixture([]int{2500, 2500, 2500, 2500, 1})
		mcpBenchmarkAssertError(b, service, paths, "payload_too_large")
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			mcpBenchmarkAssertError(b, service, paths, "payload_too_large")
		}
		b.StopTimer()
		b.ReportMetric(float64(len(paths)), "rpc/op")
		b.ReportMetric(icloud.MaxMultiSearchMaterialized+1, "materialized-events")
	})
	b.Run("ValidationRefusal", func(b *testing.B) {
		service, paths := searchEventsBenchmarkFixture(b)
		args := mcpBenchmarkArgs(paths, nil)
		args["limit"] = icloud.MaxResults + 1
		handler := searchEventsHandler(freeSlotsTestDeps(service))
		run := func(tb testing.TB) {
			tb.Helper()
			result, err := handler(context.Background(), newReq(args))
			if err != nil || !result.IsError {
				tb.Fatalf("validation refusal result = %v, %v", result, err)
			}
		}
		run(b)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			run(b)
		}
		b.StopTimer()
		b.ReportMetric(0, "rpc/op")
	})
	b.Run("RateLimited", func(b *testing.B) {
		service, paths := searchEventsBenchmarkFixture(b)
		paths = paths[:1]
		service.read = func(context.Context, string) (icloud.SearchResult, error) {
			return icloud.SearchResult{}, icloud.NewError(icloud.CodeRateLimited, 429, "synthetic rate limit", nil)
		}
		mcpBenchmarkAssertError(b, service, paths, "rate_limited")
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			mcpBenchmarkAssertError(b, service, paths, "rate_limited")
		}
		b.StopTimer()
		b.ReportMetric(float64(len(paths)), "rpc/op")
		b.ReportMetric(1, "refused")
	})
	b.Run("Cancellation", func(b *testing.B) {
		service, paths := searchEventsBenchmarkFixture(b)
		service.read = func(ctx context.Context, _ string) (icloud.SearchResult, error) {
			<-ctx.Done()
			return icloud.SearchResult{}, ctx.Err()
		}
		handler := searchEventsHandler(freeSlotsTestDeps(service))
		run := func(tb testing.TB) {
			tb.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
			defer cancel()
			result, err := handler(ctx, newReq(mcpBenchmarkArgs(paths, nil)))
			if err != nil || !result.IsError {
				tb.Fatalf("cancellation result = %v, %v", result, err)
			}
		}
		run(b)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			run(b)
		}
		b.StopTimer()
		b.ReportMetric(float64(len(paths)), "rpc/op")
		b.ReportMetric(1, "canceled")
	})
}

func BenchmarkSearchEventsFanoutLatency(b *testing.B) {
	const syntheticLatency = time.Millisecond
	service, paths := searchEventsBenchmarkFixture(b)
	service.read = func(ctx context.Context, _ string) (icloud.SearchResult, error) {
		select {
		case <-ctx.Done():
			return icloud.SearchResult{}, ctx.Err()
		case <-time.After(syntheticLatency):
			return icloud.SearchResult{}, nil
		}
	}
	run := func(tb testing.TB) {
		tb.Helper()
		result, text := mcpBenchmarkSearch(tb, service, mcpBenchmarkArgs(paths, nil))
		if result.IsError {
			tb.Fatalf("latency fan-out error: %s", text)
		}
	}
	run(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		run(b)
	}
	b.StopTimer()
	if max := service.max.Load(); max < 2 || max > calendarReadConcurrency {
		b.Fatalf("maximum concurrent reads = %d, want 2..%d", max, calendarReadConcurrency)
	}
	b.ReportMetric(float64(len(paths)), "rpc/op")
	b.ReportMetric(float64(service.max.Load()), "max-concurrency")
	b.ReportMetric(float64(syntheticLatency/time.Millisecond), "injected-latency-ms")
}

func BenchmarkMCPToolSchema(b *testing.B) {
	tool := newSearchEventsTool(time.UTC)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		data, err := json.Marshal(tool)
		if err != nil {
			b.Fatal(err)
		}
		if len(data) == 0 || !strings.Contains(string(data), "search_events") {
			b.Fatalf("serialized schema is invalid")
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(len(tool.InputSchema.Properties)), "schema-properties")
}
