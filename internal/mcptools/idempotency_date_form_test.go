package mcptools

import (
	"encoding/json"
	"testing"
)

// TestUpdateEventIdempotencyBindsDateValueForm covers FIND-3. A DATE-only input
// and a midnight DATE-TIME input parse to the same instant, but they are
// different update parameters: one keeps the DATE form and the other requires a
// timed form. The idempotency fingerprint must separate them, so a reused key
// returns conflict instead of replaying the first response.
func TestUpdateEventIdempotencyBindsDateValueForm(t *testing.T) {
	f := newIdempotencyProtocolFixture(t, "calendar", "success")

	dateOnly := f.update("shape-key", "After")
	dateOnly.GetArguments()["start"] = "2026-07-13"
	first := f.call(t, t.Context(), "date-only", dateOnly)
	if first.IsError {
		t.Fatalf("first keyed update failed: %s", resultText(t, first))
	}

	timed := f.update("shape-key", "After")
	timed.GetArguments()["start"] = "2026-07-13T00:00:00Z"
	second := f.call(t, t.Context(), "same-key-timed-form", timed)
	if !second.IsError {
		t.Fatalf("a different value form reused the cached outcome: %s", resultText(t, second))
	}
	assertIdempotencyProtocolError(t, second, "conflict")

	// The original request still returns its saved result.
	assertIdempotencyProtocolSame(t, first, f.call(t, t.Context(), "same-key-same-form", dateOnly))
	_, _, calls, writes := f.state.snapshot()
	if calls != 1 || writes != 1 {
		t.Fatalf("calls/writes = %d/%d, want 1/1", calls, writes)
	}
}

// TestUpdateEventIdempotencyBindsDateOnlyFlags proves each bound's flag
// participates independently, so an end-only form change also conflicts.
func TestUpdateEventIdempotencyBindsDateOnlyFlags(t *testing.T) {
	f := newIdempotencyProtocolFixture(t, "calendar", "success")

	base := f.update("end-shape-key", "After")
	base.GetArguments()["end"] = "2026-07-13"
	if res := f.call(t, t.Context(), "date-end", base); res.IsError {
		t.Fatalf("first keyed update failed: %s", resultText(t, res))
	}

	changed := f.update("end-shape-key", "After")
	changed.GetArguments()["end"] = "2026-07-13T00:00:00Z"
	res := f.call(t, t.Context(), "same-key-timed-end", changed)
	if !res.IsError {
		t.Fatalf("a different end value form reused the cached outcome: %s", resultText(t, res))
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != "conflict" {
		t.Fatalf("code = %q, want conflict: %s", payload.Code, resultText(t, res))
	}
}
