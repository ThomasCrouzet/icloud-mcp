package mcptools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func contractPathValue(args map[string]any, path []string) any {
	var node any = args
	for _, key := range path {
		if key == "*" {
			node = node.([]any)[0]
		} else {
			node = node.(map[string]any)[key]
		}
	}
	return node
}

func contractBoundValue(property contractProperty, keyword string, number json.Number, sample any) any {
	if keyword == "minimum" || keyword == "maximum" {
		return number
	}
	n, _ := strconv.Atoi(number.String())
	if keyword == "minItems" || keyword == "maxItems" {
		original := sample.([]any)
		out := make([]any, n)
		for i := range out {
			out[i] = original[0]
		}
		if property.path[0] == "flags" {
			all := []any{"seen", "flagged", "answered"}
			copy(out, all)
		}
		if property.path[0] == "to" || property.path[0] == "cc" || property.path[0] == "bcc" {
			for i := range out {
				out[i] = fmt.Sprintf("r%02d@example.invalid", i)
			}
		}
		return out
	}
	text := sample.(string)
	if len(text) == n {
		return text
	}
	key := property.path[len(property.path)-1]
	if n == 0 {
		return ""
	}
	if keyword == "minLength" {
		if n == 3 && property.path[len(property.path)-1] == "*" {
			return "a@b"
		}
		return strings.Repeat("x", n)
	}
	if key == "calendar" {
		return "/cal/" + strings.Repeat("x", n-6) + "/"
	}
	if key == "rrule" || key == "timezone" {
		return text + strings.Repeat(" ", n-len(text))
	}
	if key == "url" || len(property.path) > 1 && property.path[0] == "urls" && key == "value" {
		prefix := "https://example.invalid/"
		return prefix + strings.Repeat("x", n-len(prefix))
	}
	if len(property.path) > 1 && property.path[0] == "emails" && key == "value" {
		suffix := "@example.invalid"
		return strings.Repeat("x", n-len(suffix)) + suffix
	}
	if key == "phone" || len(property.path) > 1 && property.path[0] == "phones" && key == "value" {
		return strings.Repeat("1", n)
	}
	if key == "*" {
		suffix := "@example.invalid"
		return strings.Repeat("x", n-len(suffix)) + suffix
	}
	return strings.Repeat("x", n)
}

func contractBoundRuntime(t *testing.T, args map[string]any) *contractRuntime {
	t.Helper()
	return newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
		if path, ok := args["calendar"].(string); ok && strings.HasPrefix(path, "/cal/") {
			f.calendar.collection = path
			objects := map[string]string{}
			for _, object := range f.calendar.objects {
				uid := "event"
				if value, ok := args["uid"].(string); ok {
					object = strings.ReplaceAll(object, "UID:event\r\n", "UID:"+value+"\r\n")
					uid = value
				}
				objects[path+uid+".ics"] = object
			}
			f.calendar.objects = objects
		}
		if uid, ok := args["uid"].(string); ok {
			for path, object := range f.contacts.objects {
				f.contacts.objects[path] = strings.ReplaceAll(object, "UID:contact\r\n", "UID:"+uid+"\r\n")
			}
		}
		if value, ok := args["mailbox"].(string); ok {
			f.mail.source = value
		}
		if value, ok := args["destination"].(string); ok {
			f.mail.destination = value
		}
		if value, ok := args["uid_validity"]; ok {
			f.mail.validity, _ = strconv.ParseUint(fmt.Sprint(value), 10, 32)
		}
		addresses := []string{"reply@example.invalid", "recipient@example.invalid", "copy@example.invalid"}
		for _, field := range []string{"to", "cc", "bcc"} {
			if values, ok := args[field].([]any); ok {
				for _, address := range values {
					addresses = append(addresses, address.(string))
				}
			}
		}
		unique := make([]string, 0, len(addresses))
		seen := map[string]bool{}
		for _, address := range addresses {
			if !seen[address] {
				unique = append(unique, address)
				seen[address] = true
			}
		}
		f.mail.policy = strings.Join(unique, ",")
	})
}

func TestContractExactPublishedBoundaries(t *testing.T) {
	seed := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
	names := make([]string, 0, len(seed.tools))
	for name := range seed.tools {
		names = append(names, name)
	}
	sortContractNames(names)
	for _, name := range names {
		tool := seed.tools[name]
		t.Run(name, func(t *testing.T) {
			for _, property := range contractProperties(contractInputSchema(t, tool), nil) {
				for _, keyword := range []string{"minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems"} {
					raw, ok := property.schema[keyword].(json.Number)
					if !ok {
						continue
					}
					t.Run(strings.Join(property.path, "/")+"/exact-"+keyword, func(t *testing.T) {
						args := contractClone(seed.arguments(name, property.path[0]))
						sample := contractPathValue(args, property.path)
						value := contractBoundValue(property, keyword, raw, sample)
						contractPathSet(args, property.path, value, false)
						if keyword == "maxItems" && (property.path[0] == "cc" || property.path[0] == "bcc") {
							delete(args, "to")
						}
						f := contractBoundRuntime(t, args)
						if err := contractCompileSchema(t, contractInputSchema(t, tool)).Validate(contractValue(args)); err != nil {
							t.Fatalf("exact boundary fixture violates published schema: %v", err)
						}
						before := f.counts()
						res := f.call(t, name, args)
						resource := property.path[0] == "calendar" && keyword == "maxLength"
						semantic := resource || property.path[0] == "timezone" && keyword == "maxLength" || len(property.path) > 1 && property.path[len(property.path)-1] == "value" && keyword == "minLength" || property.path[0] == "cursor" && keyword == "maxLength"
						if semantic {
							text := resultText(t, res)
							payload := contractPayload(t, res)
							if strings.Contains(text, "input schema validation failed") || strings.Contains(text, "unsupported argument") {
								t.Fatalf("semantic refusal was actually schema/handler drift: %s", text)
							}
							if !res.IsError && payload["ok"] != false {
								t.Fatalf("expected precise business refusal at schema-valid semantic boundary: %s", text)
							}
							if resource && !strings.Contains(text, "path") {
								t.Fatalf("unexpected resource refusal: %s", text)
							}
							if f.calendar.mutations != 0 || f.contacts.mutations != 0 || f.mail.stores+f.mail.moves+f.mail.data != 0 {
								t.Fatal("business-invalid boundary mutated")
							}
							layer := "business-validation"
							if resource {
								layer = "resource-path-policy"
							}
							t.Logf("contract_bound=%s", contractReplayJSON(t, map[string]any{"tool": name, "property": strings.Join(property.path, "/"), "bound": keyword, "value": value, "layer": layer, "before": before, "after": f.counts(), "result": res}))
							return
						}
						if res.IsError {
							t.Fatalf("exact published boundary refused: %s", resultText(t, res))
						}
						contractValidateOutput(t, tool, res)
						payload := contractPayload(t, res)
						if payload["uid"] != nil && args["uid"] != nil {
							contractSame(t, "boundary UID identity", payload["uid"], args["uid"])
						}
						if payload["mailbox"] != nil {
							contractSame(t, "boundary mailbox", payload["mailbox"], args["mailbox"])
						}
						if payload["uidValidity"] != nil && args["uid_validity"] != nil {
							contractSame(t, "boundary UIDVALIDITY", payload["uidValidity"], args["uid_validity"])
						}
						if name == "validate_event" && payload["ok"] != true {
							t.Fatalf("valid boundary did not normalize: %v", payload)
						}
						t.Logf("contract_bound=%s", contractReplayJSON(t, map[string]any{"tool": name, "property": strings.Join(property.path, "/"), "bound": keyword, "value": value, "layer": "accepted-real-handler-service", "result": res, "after": f.counts()}))
					})
				}
			}
		})
	}
}

func sortContractNames(names []string) { sort.Strings(names) }
