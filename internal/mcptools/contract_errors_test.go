package mcptools

import (
	"strings"
	"testing"
)

func TestContractRealServiceErrors(t *testing.T) {
	seed := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
	for name := range seed.tools {
		t.Run(name, func(t *testing.T) {
			if name == "icloud_capabilities" || name == "calendar_capabilities" {
				f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
				f.refuseSDK(t, name, map[string]any{"undeclared_contract_key": true})
				return
			}
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
				switch {
				case strings.Contains(name, "contact") || name == "list_address_books":
					f.freshContacts = true
					f.contacts.failureStatus = 403
				case name == "send_message" || name == "reply_message":
					f.mail.rejectSMTPAuth = true
				case strings.Contains(name, "message") || name == "list_mailboxes":
					f.mail.rejectIMAP = true
				default:
					f.calendar.failureStatus = 403
				}
			})
			args := f.arguments(name, "")
			if name == "validate_event" {
				args["start"] = "not-a-date"
			}
			result := f.call(t, name, args)
			if !result.IsError {
				t.Fatalf("real service error was presented as success: %s", resultText(t, result))
			}
			payload := contractPayload(t, result)
			code, _ := payload["code"].(string)
			expected := "authorization"
			if strings.Contains(name, "message") || name == "list_mailboxes" {
				expected = "authentication"
			}
			if name == "validate_event" {
				expected = "validation"
			}
			if code != expected {
				t.Fatalf("public code %s != %s: %v", code, expected, payload)
			}
			if strings.Contains(resultText(t, result), "synthetic-contract-secret") {
				t.Fatal("secret leaked")
			}
			f.mail.mu.Lock()
			mutations := f.mail.data + f.mail.stores + f.mail.moves + f.mail.appends
			f.mail.mu.Unlock()
			if f.calendar.mutations != 0 || f.contacts.mutations != 0 || mutations != 0 {
				t.Fatal("refused service operation mutated")
			}
			t.Logf("contract_error_case=%s", contractReplayJSON(t, map[string]any{"tool": name, "code": code, "result": result, "counters": f.counts(), "output_schema": "success-shape-not-applied-to-IsError"}))
		})
	}
}
