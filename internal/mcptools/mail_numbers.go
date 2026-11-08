package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ConfigureMailNumberValidation preserves the published uint64 range before the
// pinned SDK validates its separately decoded float64 argument map. Install once
// at startup, before serving requests. Other hooks and numeric fields stay intact.
func ConfigureMailNumberValidation(s *server.MCPServer) {
	hooks := s.GetHooks()
	if hooks == nil {
		hooks = &server.Hooks{}
		server.WithHooks(hooks)(s)
	}
	hooks.AddBeforeCallTool(preserveMailModSeq)
}

func preserveMailModSeq(_ context.Context, _ any, request *mcp.CallToolRequest) {
	if request == nil || request.Params.Name != "set_message_flags" || len(request.Params.RawArguments) == 0 {
		return
	}
	arguments, ok := request.Params.Arguments.(map[string]any)
	if !ok || arguments == nil {
		return
	}
	if _, present := arguments["expected_modseq"]; !present {
		return
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(request.Params.RawArguments, &raw) != nil {
		return
	}
	number, err := mailCanonicalUnsigned(string(raw["expected_modseq"]))
	if err != nil {
		return
	}
	clone := make(map[string]any, len(arguments))
	for key, value := range arguments {
		clone[key] = value
	}
	clone["expected_modseq"] = json.Number(number)
	request.Params.Arguments = clone
}

// mailCanonicalUnsigned accepts integral unsigned JSON spellings, not rounded
// floats. Exponent scanning saturates at a token-length bound. Only at most 20
// canonical digits reach the SDK rational validator, including uint64 overflow
// controls; the validator still owns the published minimum and maximum.
func mailCanonicalUnsigned(token string) (string, error) {
	token = strings.TrimSpace(token)
	invalid := func() (string, error) { return "", fmt.Errorf("not a bounded unsigned integer") }
	if len(token) == 0 || token[0] < '0' || token[0] > '9' || !json.Valid([]byte(token)) {
		return invalid()
	}
	mantissa, exponent := token, ""
	if i := strings.IndexAny(token, "eE"); i >= 0 {
		mantissa, exponent = token[:i], token[i+1:]
	}
	fractionDigits := 0
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		fractionDigits = len(mantissa) - i - 1
	}
	digits := strings.TrimLeft(strings.ReplaceAll(mantissa, ".", ""), "0")
	if digits == "" {
		return "0", nil
	}
	sign, power := 1, 0
	if len(exponent) > 0 && (exponent[0] == '+' || exponent[0] == '-') {
		if exponent[0] == '-' {
			sign = -1
		}
		exponent = exponent[1:]
	}
	// A larger exponent cannot cancel the mantissa's token-bounded scale.
	limit := len(token) + 20
	for _, digit := range exponent {
		if power < limit {
			power = min(limit, power*10+int(digit-'0'))
		}
	}
	trimmed := strings.TrimRight(digits, "0")
	scale := sign*power - fractionDigits + len(digits) - len(trimmed)
	if scale < 0 || len(trimmed) > 20 || scale > 20-len(trimmed) {
		return invalid()
	}
	return trimmed + strings.Repeat("0", scale), nil
}

func mailExactUnsigned(number json.Number, bits int) (uint64, error) {
	canonical, err := mailCanonicalUnsigned(string(number))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(canonical, 10, bits)
}
