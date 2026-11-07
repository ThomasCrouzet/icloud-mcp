package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestContractExactUnsignedNormalization(t *testing.T) {
	for _, tc := range []struct {
		token, want string
		bits        int
	}{
		{"18446744073709551615", "18446744073709551615", 64},
		{"18446744073709551615.0", "18446744073709551615", 64},
		{"184467440737095516150e-1", "18446744073709551615", 64},
		{"1.8446744073709551615e19", "18446744073709551615", 64},
		{"9007199254740993", "9007199254740993", 64},
		{"1.0", "1", 64}, {"1e0", "1", 64}, {"10e-1", "1", 64}, {"0.01e2", "1", 64},
		{"100.00e-2", "1", 32}, {"4294967295.00", "4294967295", 32},
		{"0e99999999999999999999999999999", "0", 64},
		{"1e+0000000000000000000000000000", "1", 64},
		{"1" + strings.Repeat("0", 4096) + "e-4096", "1", 64},
		{"0." + strings.Repeat("0", 4095) + "1e4096", "1", 64},
		{"18446744073709551616", "", 64}, {"18446744073709551616.0", "", 64},
		{"4294967296", "", 32}, {"1.0000000000000000001", "", 64}, {"1.5", "", 64},
		{"-1", "", 64}, {"-0", "", 64}, {"1e-1000000000", "", 64}, {"1e1000000000", "", 64},
		{"1e" + strings.Repeat("9", 4096), "", 64}, {"1e-" + strings.Repeat("9", 4096), "", 64},
		{`"1"`, "", 64}, {"true", "", 64}, {"null", "", 64}, {"[]", "", 64}, {"{}", "", 64},
		{"01", "", 64}, {"+1", "", 64}, {"1.", "", 64}, {"1e", "", 64}, {"1 2", "", 64},
	} {
		t.Run(fmt.Sprintf("case-%d-%d", tc.bits, len(tc.token))+"/"+tc.token[:min(40, len(tc.token))], func(t *testing.T) {
			value, err := mailUnsigned(json.Number(tc.token), tc.bits)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("invalid number accepted as %d", value)
				}
				return
			}
			if err != nil || fmt.Sprint(value) != tc.want {
				t.Fatalf("exact number: got=%d err=%v want=%s", value, err, tc.want)
			}
		})
	}
}

func TestContractMailNumberHookScope(t *testing.T) {
	for _, tc := range []struct{ name, raw, canonical string }{
		{"set_message_flags", `{"uid":2,"expected_modseq":18446744073709551615,"unknown":3}`, "18446744073709551615"},
		{"set_message_flags", `{"expected_modseq":18446744073709551616}`, "18446744073709551616"},
		{"set_message_flags", `{"expected_modseq":184467440737095516150e-1}`, "18446744073709551615"},
		{"search_events", `{"expected_modseq":42}`, ""}, {"search_contacts", `{"expected_modseq":42}`, ""}, {"get_message", `{"expected_modseq":42}`, ""},
		{"set_message_flags", "", ""}, {"set_message_flags", `{"uid":2}`, ""}, {"set_message_flags", "{", ""},
		{"set_message_flags", `{"expected_modseq":42} {}`, ""}, {"set_message_flags", `{"expected_modseq":"42"}`, ""},
		{"set_message_flags", `{"expected_modseq":true}`, ""}, {"set_message_flags", `{"expected_modseq":null}`, ""},
		{"set_message_flags", `{"expected_modseq":[]}`, ""}, {"set_message_flags", `{"expected_modseq":{}}`, ""},
		{"set_message_flags", `{"expected_modseq":1.0000000000000000001}`, ""}, {"set_message_flags", `{"expected_modseq":1e-1000000000}`, ""},
	} {
		t.Run(tc.name+"/"+tc.raw, func(t *testing.T) {
			original := map[string]any{"uid": float64(2), "expected_modseq": float64(42), "unknown": float64(3)}
			req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: tc.name, Arguments: original, RawArguments: json.RawMessage(tc.raw)}}
			preserveMailModSeq(t.Context(), 1, &req)
			if string(req.Params.RawArguments) != tc.raw || req.GetArguments()["uid"] != float64(2) || req.GetArguments()["unknown"] != float64(3) || original["expected_modseq"] != float64(42) {
				t.Fatal("raw bytes, unrelated arguments or alias changed")
			}
			if tc.canonical != "" {
				if req.GetArguments()["expected_modseq"] != json.Number(tc.canonical) || reflect.ValueOf(req.GetArguments()).Pointer() == reflect.ValueOf(original).Pointer() {
					t.Fatal("request map not replaced exactly")
				}
			} else if reflect.ValueOf(req.GetArguments()).Pointer() != reflect.ValueOf(original).Pointer() {
				t.Fatal("out-of-scope map replaced")
			}
		})
	}
	preserveMailModSeq(t.Context(), 1, nil)
	for _, args := range []any{nil, []any{}, map[string]any{"uid": float64(2)}} {
		req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "set_message_flags", Arguments: args, RawArguments: json.RawMessage(`{"expected_modseq":42}`)}}
		preserveMailModSeq(t.Context(), 1, &req)
		if !reflect.DeepEqual(args, req.Params.Arguments) {
			t.Fatal("missing map/field changed")
		}
	}
	hooks := &server.Hooks{}
	calls := 0
	hooks.AddBeforeCallTool(func(_ context.Context, _ any, _ *mcp.CallToolRequest) { calls++ })
	s := server.NewMCPServer("hook-scope", "test", server.WithHooks(hooks))
	ConfigureMailNumberValidation(s)
	if s.GetHooks() != hooks || len(hooks.OnBeforeCallTool) != 2 {
		t.Fatal("existing hooks replaced")
	}
	for _, hook := range hooks.OnBeforeCallTool {
		hook(t.Context(), 1, &mcp.CallToolRequest{})
	}
	if calls != 1 {
		t.Fatal("existing hook lost")
	}
}
