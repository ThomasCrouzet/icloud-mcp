package mcptools

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestContractRealConditionalStore(t *testing.T) {
	for _, tc := range []struct {
		name               string
		present, malformed bool
		stores             int
		code               string
	}{{"accepted", true, false, 1, ""}, {"missing-precondition", false, false, 0, "validation"}, {"malformed-returned-modseq", true, true, 1, "protocol_error"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
				f.mail.condstore = true
				f.mail.malformedStore = tc.malformed
				f.mail.modseq = 42
			})
			args := f.arguments("set_message_flags", "")
			if tc.present {
				args["expected_modseq"] = 42
			}
			result := f.call(t, "set_message_flags", args)
			payload := contractPayload(t, result)
			f.mail.mu.Lock()
			stores, commands := f.mail.stores, strings.Join(f.mail.commands, "\n")
			f.mail.mu.Unlock()
			if stores != tc.stores {
				t.Fatalf("real adapter STORE count %d != %d", stores, tc.stores)
			}
			if tc.stores == 1 && !strings.Contains(commands, "UNCHANGEDSINCE 42") {
				t.Fatal("exact expected_modseq was not sent on conditional STORE")
			}
			if tc.code != "" {
				if !result.IsError || payload["code"] != tc.code {
					t.Fatalf("unsafe conditional result: %v", payload)
				}
			} else if result.IsError || payload["conditionalUpdate"] != true {
				t.Fatalf("valid conditional STORE did not succeed: %v", payload)
			}
			t.Logf("contract_condstore_case=%s", contractReplayJSON(t, map[string]any{"case": tc.name, "result": result, "backend": f.counts(), "distinction": "real-adapter-conditional-STORE-not-unsupported-capability-fake"}))
		})
	}
}

func TestContractFullRangeConditionalStore(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		value       uint64
		malformed   bool
	}{
		{"maximum-malformed-return", "18446744073709551615", ^uint64(0), true},
		{"maximum-exponent-malformed-return", "184467440737095516150e-1", ^uint64(0), true},
		{"nearby-success", "18446744073709551614", ^uint64(0) - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
				f.mail.condstore = true
				f.mail.modseq = tc.value
				f.mail.malformedStore = tc.malformed
			})
			args := f.arguments("set_message_flags", "")
			args["expected_modseq"] = json.Number(tc.token)
			res := f.call(t, "set_message_flags", args)
			payload := contractPayload(t, res)
			f.mail.mu.Lock()
			stores, commands, smtp := f.mail.stores, strings.Join(f.mail.commands, "\n"), f.mail.smtpDials
			f.mail.mu.Unlock()
			if stores != 1 || smtp != 0 || !strings.Contains(commands, "UNCHANGEDSINCE "+strconv.FormatUint(tc.value, 10)+")") {
				t.Fatalf("exact conditional STORE missing or changed: %s", commands)
			}
			if tc.malformed {
				if !res.IsError || payload["code"] != "protocol_error" {
					t.Fatal("malformed MODSEQ falsely reported success")
				}
			} else if res.IsError || payload["conditionalUpdate"] != true {
				t.Fatalf("valid conditional STORE refused: %s", resultText(t, res))
			}
			t.Logf("contract_numeric_condstore=%s", contractReplayJSON(t, map[string]any{"case": tc.name, "expected_modseq": tc.token, "backend_value": strconv.FormatUint(tc.value, 10), "result": res, "counters": f.counts()}))
		})
	}
}
