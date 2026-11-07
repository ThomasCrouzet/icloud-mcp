package mcptools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestContractUTF8TextByteBounds(t *testing.T) {
	seed := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
	for _, name := range []string{"create_event", "update_event", "validate_event", "create_contact", "update_contact", "send_message", "reply_message"} {
		tool := seed.tools[name]
		t.Run(name, func(t *testing.T) {
			for _, property := range contractProperties(contractInputSchema(t, tool), nil) {
				if len(property.path) != 1 {
					continue
				}
				key := property.path[0]
				if key != "title" && key != "location" && key != "notes" && key != "display_name" && key != "organization" && key != "nickname" && key != "subject" && key != "body" {
					continue
				}
				raw, ok := property.schema["maxLength"].(json.Number)
				if !ok {
					t.Fatal("text byte-bound fixture lost its published maximum")
				}
				n, _ := raw.Int64()
				for _, extra := range []int{0, 1} {
					t.Run(fmt.Sprintf("%s/bytes-plus-%d", key, extra), func(t *testing.T) {
						f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
						args := f.arguments(name, key)
						length := int(n) + extra
						args[key] = strings.Repeat("\u00e9", length/2) + strings.Repeat("x", length%2)
						if key == "body" {
							args[key] = "\u00e9" + strings.Repeat("x", length-2)
						}
						if err := contractCompileSchema(t, contractInputSchema(t, tool)).Validate(contractValue(args)); err != nil {
							t.Fatalf("UTF-8 byte-policy case was not schema-valid: %v", err)
						}
						before := f.counts()
						res := f.call(t, name, args)
						if extra == 0 {
							if res.IsError {
								t.Fatalf("exact UTF-8 byte bound rejected: %s", resultText(t, res))
							}
							contractValidateOutput(t, tool, res)
							f.verify(t, name, args, res)
							f.verifyProperty(t, name, property.path, args, res)
						} else {
							payload := contractPayload(t, res)
							if !res.IsError && payload["ok"] != false {
								t.Fatal("UTF-8 byte limit was not enforced")
							}
							if strings.Contains(resultText(t, res), "input schema validation failed") {
								t.Fatal("byte policy was misclassified as JSON character-length schema refusal")
							}
							if !reflect.DeepEqual(before, f.counts()) {
								t.Fatal("byte-invalid text reached a backend")
							}
						}
						t.Logf("contract_byte_boundary=%s", contractReplayJSON(t, map[string]any{"tool": name, "property": key, "bytes": length, "exact_maximum": extra == 0, "layer": "UTF-8-business-byte-policy"}))
					})
				}
			}
		})
	}
}

func TestContractEncodedMailResourceLimit(t *testing.T) {
	for _, name := range []string{"send_message", "reply_message"} {
		t.Run(name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
			args := f.arguments(name, "")
			args["body"] = strings.Repeat("\u00e9", 51200)
			if err := contractCompileSchema(t, contractInputSchema(t, f.tools[name])).Validate(contractValue(args)); err != nil {
				t.Fatal(err)
			}
			result := f.call(t, name, args)
			payload := contractPayload(t, result)
			if !result.IsError || payload["code"] != "payload_too_large" {
				t.Fatalf("encoded-message guard not observed: %s", resultText(t, result))
			}
			f.mail.mu.Lock()
			defer f.mail.mu.Unlock()
			wantIMAP := 0
			if name == "reply_message" {
				wantIMAP = 1
			}
			if f.mail.smtpDials != 0 || f.mail.data != 0 || f.mail.imapDials != wantIMAP {
				t.Fatal("encoded-message refusal accessed an unexpected backend")
			}
			t.Log("schema-valid/raw-byte-valid content exceeded the separate encoded wire budget; this is not exact-bound success")
		})
	}
}
