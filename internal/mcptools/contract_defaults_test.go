package mcptools

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestContractPublishedDefaults(t *testing.T) {
	seed := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
	for name, tool := range seed.tools {
		t.Run(name, func(t *testing.T) {
			for _, property := range contractProperties(contractInputSchema(t, tool), nil) {
				value, exists := property.schema["default"]
				if !exists || len(property.path) != 1 {
					continue
				}
				key := property.path[0]
				t.Run(key, func(t *testing.T) {
					var results []map[string]any
					for _, explicit := range []bool{false, true} {
						f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
						args := f.arguments(name, key)
						delete(args, key)
						contractRepairAbsent(name, key, args)
						if explicit {
							args[key] = value
						}
						if name == "create_event" {
							args["client_uid"] = "default-created"
						}
						res := f.call(t, name, args)
						if res.IsError {
							t.Fatalf("published default rejected: %s", resultText(t, res))
						}
						contractValidateOutput(t, tool, res)
						f.verify(t, name, args, res)
						payload := contractPayload(t, res)
						if name == "send_message" || name == "reply_message" {
							delete(payload, "messageId")
						}
						results = append(results, payload)
					}
					contractSame(t, "absent versus published default", results[0], results[1])
					t.Logf("contract_default=%s", contractReplayJSON(t, map[string]any{"tool": name, "property": key, "default": value, "result": "pass"}))
				})
			}
		})
	}
}

func contractCounterCorpus(f *contractRuntime, name, key string) {
	if name == "get_message" && key == "allow_html_fallback" {
		f.mail.htmlOnly = true
	}
	if name == "get_message" && key == "max_body_bytes" {
		f.mail.body = contractSizedPlainBody(102401)
	}
	if name == "search_messages" && key == "limit" {
		f.mail.messageCount = 52
	}
	if name == "search_messages" && key == "before_uid" {
		f.mail.messageCount = 3
	}
	if name == "search_contacts" {
		if key == "include_groups" {
			f.contacts.objects[contractBookPath+"group.vcf"] = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:group\r\nFN:Synthetic Group\r\nN:Group;Synthetic;;;\r\nKIND:group\r\nEND:VCARD\r\n"
			return
		}
		if key == "limit" {
			for i := 0; i < 101; i++ {
				f.contacts.objects[fmt.Sprintf("%sp%03d.vcf", contractBookPath, i)] = fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:p%03d\r\nFN:Person %03d\r\nN:Person;;;;\r\nEND:VCARD\r\n", i, i)
			}
			return
		}
		f.contacts.objects[contractBookPath+"other.vcf"] = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:other\r\nFN:Other\r\nN:Other;;;;\r\nEMAIL:other@example.invalid\r\nTEL:9999\r\nEND:VCARD\r\n"
	}
	if name == "search_events" && key == "limit" {
		original := f.calendar.objects[contractCalendar+"event.ics"]
		for i := 0; i < 401; i++ {
			f.calendar.objects[fmt.Sprintf("%se%03d.ics", contractCalendar, i)] = strings.ReplaceAll(strings.ReplaceAll(original, "UID:event", fmt.Sprintf("UID:e%03d", i)), "RRULE:FREQ=DAILY;COUNT=2\r\n", "")
		}
		return
	}
	if key == "include_all_day_busy" || name == "search_events" && key == "all_day" {
		f.calendar.objects[contractCalendar+"all-day.ics"] = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Contract//EN\r\nBEGIN:VEVENT\r\nUID:all-day\r\nDTSTAMP:20260701T000000Z\r\nDTSTART;VALUE=DATE:20260701\r\nDTEND;VALUE=DATE:20260702\r\nSUMMARY:Synthetic all-day\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		return
	}
	if name == "search_events" && (key == "include_cancelled" || key == "busy_only" || key == "status" || key == "uid" || key == "query") {
		other := strings.ReplaceAll(f.calendar.objects[contractCalendar+"event.ics"], "UID:event", "UID:other")
		other = strings.ReplaceAll(other, "RRULE:FREQ=DAILY;COUNT=2\r\n", "")
		switch key {
		case "include_cancelled":
			other = strings.ReplaceAll(other, "STATUS:CONFIRMED", "STATUS:CANCELLED")
		case "busy_only":
			other = strings.ReplaceAll(other, "TRANSP:OPAQUE", "TRANSP:TRANSPARENT")
		case "status":
			other = strings.ReplaceAll(other, "STATUS:CONFIRMED", "STATUS:TENTATIVE")
		case "query":
			other = strings.ReplaceAll(other, "SUMMARY:Synthetic event", "SUMMARY:Other")
		}
		f.calendar.objects[contractCalendar+"other.ics"] = other
	}
}

func TestContractValueSensitiveDefaults(t *testing.T) {
	for _, tc := range []struct {
		tool, key        string
		value            any
		absent, explicit any
		args             map[string]any
		metric           string
	}{
		{"get_message", "allow_html_fallback", true, "html_only", "source-body", nil, "message-body"},
		{"get_message", "max_body_bytes", 204800, true, false, nil, "bodyOmitted"},
		{"search_contacts", "include_groups", true, 1, 2, nil, "contacts"},
		{"search_contacts", "query", "Synthetic", 2, 1, nil, "contacts"},
		{"search_contacts", "email", "reply", 2, 1, nil, "contacts"},
		{"search_contacts", "phone", "1234", 2, 1, nil, "contacts"},
		{"search_contacts", "limit", 1, "$default", 1, nil, "contacts"},
		{"search_contacts", "limit", 100, "$default", 100, nil, "contacts"},
		{"search_messages", "limit", 1, "$default", 1, nil, "messages"},
		{"search_messages", "limit", 50, "$default", 50, nil, "messages"},
		{"search_messages", "before_uid", 3, 3, 1, map[string]any{"uid_validity": 9}, "messages"},
		{"search_events", "limit", 1, "$default", 1, nil, "events"},
		{"search_events", "limit", 400, "$default", 400, nil, "events"},
		{"search_events", "all_day", true, 3, 1, nil, "events"},
		{"search_events", "include_cancelled", false, 3, 2, nil, "events"},
		{"search_events", "busy_only", true, 3, 2, nil, "events"},
		{"search_events", "query", "Synthetic", 3, 2, nil, "events"},
		{"search_events", "uid", "event", 3, 2, nil, "events"},
		{"search_events", "status", "TENTATIVE", 3, 1, nil, "events"},
		{"search_events", "expand_recurrence", false, 2, 1, nil, "events"},
		{"search_events", "compact", true, "Original notes", "", nil, "event-notes"},
		{"find_free_slots", "include_all_day_busy", false, "2026-07-02T00:00:00+00:00", "2026-07-01T09:00:00+00:00", nil, "first-slot"},
		{"find_free_slots", "days_of_week", "thu", "2026-07-01T09:00:00+00:00", "2026-07-02T00:00:00+00:00", nil, "first-slot"},
		{"find_free_slots", "buffer_before_minutes", 15, "2026-07-01T09:00:00+00:00", "2026-07-01T11:00:00+00:00", nil, "first-slot"},
		{"find_free_slots", "buffer_after_minutes", 15, "2026-07-01T11:00:00+00:00", "2026-07-01T11:15:00+00:00", map[string]any{"start": "2026-07-01T10:00:00Z"}, "first-slot"},
		{"find_free_slots", "working_hours_start", "13:00", "2026-07-01T09:00:00+00:00", "2026-07-01T13:00:00+00:00", nil, "first-slot"},
		{"find_free_slots", "timezone", "Europe/Paris", "2026-07-01T09:00:00+00:00", "2026-07-01T11:00:00+02:00", nil, "first-slot"},
		{"find_free_slots", "limit", 1, "$default", 1, map[string]any{"end": "2026-07-10T00:00:00Z"}, "slots"},
		{"find_free_slots", "limit", 200, "$default", 200, map[string]any{"end": "2026-07-10T00:00:00Z"}, "slots"},
	} {
		t.Run(tc.tool+"/"+tc.key, func(t *testing.T) {
			for _, explicit := range []bool{false, true} {
				t.Run(fmt.Sprint(explicit), func(t *testing.T) {
					f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) { contractCounterCorpus(f, tc.tool, tc.key) })
					args := contractMerge(f.arguments(tc.tool, ""), tc.args)
					if explicit {
						args[tc.key] = tc.value
					}
					res := f.call(t, tc.tool, args)
					if res.IsError {
						t.Fatalf("counterfactual dispatch: %s", resultText(t, res))
					}
					contractValidateOutput(t, f.tools[tc.tool], res)
					payload := contractPayload(t, res)
					var got any
					switch tc.metric {
					case "events", "contacts", "messages", "slots":
						got = len(payload[tc.metric].([]any))
					case "first-slot":
						got = payload["slots"].([]any)[0].(map[string]any)["start"]
					case "bodyOmitted":
						got = payload["bodyOmitted"] == true
					case "message-body":
						got = payload["body"]
						if got == nil {
							got = payload["bodyUnavailableReason"]
						}
					case "event-notes":
						got = payload["events"].([]any)[0].(map[string]any)["notes"]
						if got == nil {
							got = ""
						}
					}
					want := tc.absent
					if want == "$default" {
						want = contractInputSchema(t, f.tools[tc.tool])["properties"].(map[string]any)[tc.key].(map[string]any)["default"]
					}
					if explicit {
						want = tc.explicit
					}
					contractSame(t, "observable default/predicate effect", got, want)
					t.Logf("contract_counterfactual=%s", contractReplayJSON(t, map[string]any{"tool": tc.tool, "property": tc.key, "explicit": explicit, "observed": got, "expected": want, "counters": f.counts()}))
				})
			}
		})
	}
}

func contractSizedPlainBody(size int) string {
	var b strings.Builder
	for b.Len()+82 <= size {
		b.WriteString(strings.Repeat("x", 80) + "\r\n")
	}
	b.WriteString(strings.Repeat("x", size-b.Len()))
	return b.String()
}

func TestContractWorkingHoursEndCounterfactual(t *testing.T) {
	for _, zone := range []string{"UTC", "Europe/Paris", "America/New_York"} {
		t.Run(zone, func(t *testing.T) {
			location, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, time.July, 1, 9, 0, 0, 0, location)
			cutoff := time.Date(2026, time.July, 1, 17, 0, 0, 0, location)
			rangeEnd := time.Date(2026, time.July, 1, 20, 0, 0, 0, location)
			for _, explicit := range []bool{false, true} {
				t.Run(fmt.Sprint(explicit), func(t *testing.T) {
					f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
					args := contractMerge(f.arguments("find_free_slots", ""), map[string]any{"start": start.Format(time.RFC3339), "end": rangeEnd.Format(time.RFC3339), "timezone": zone, "working_hours_start": "09:00", "limit": 200})
					if explicit {
						args["working_hours_end"] = "17:00"
					}
					res := f.call(t, "find_free_slots", args)
					if res.IsError {
						t.Fatal(resultText(t, res))
					}
					contractValidateOutput(t, f.tools["find_free_slots"], res)
					slots := contractPayload(t, res)["slots"].([]any)
					if len(slots) == 0 {
						t.Fatal("working_hours_end counterfactual requires nonempty actual slots")
					}
					var latest time.Time
					for _, item := range slots {
						end, err := time.Parse(time.RFC3339, item.(map[string]any)["end"].(string))
						if err != nil {
							t.Fatal(err)
						}
						if end.After(latest) {
							latest = end
						}
					}
					want := rangeEnd
					if explicit {
						f.verifyProperty(t, "find_free_slots", []string{"working_hours_end"}, args, res)
						want = cutoff
					} else if !latest.After(cutoff) {
						t.Fatal("absent-cutoff dataset cannot expose an ignored working_hours_end")
					}
					if !latest.Equal(want) {
						t.Fatalf("latest local slot end %s != %s", latest.In(location).Format(time.RFC3339), want.Format(time.RFC3339))
					}
					t.Logf("contract_working_end=%s", contractReplayJSON(t, map[string]any{"tool": "find_free_slots", "property": "working_hours_end", "timezone": zone, "explicit": explicit, "slots": len(slots), "latest_local_end": latest.In(location).Format(time.RFC3339), "requested_local_cutoff": cutoff.Format(time.RFC3339)}))
				})
			}
		})
	}
}

func TestContractDecodedBodyExactMaximum(t *testing.T) {
	for _, size := range []int{204800, 204801} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) { f.mail.body = contractSizedPlainBody(size) })
			args := f.arguments("get_message", "")
			args["max_body_bytes"] = 204800
			res := f.call(t, "get_message", args)
			if res.IsError {
				t.Fatal(resultText(t, res))
			}
			contractValidateOutput(t, f.tools["get_message"], res)
			payload := contractPayload(t, res)
			if size == 204800 {
				body, ok := payload["body"].(string)
				if !ok || len(body) != size || body != f.mail.body {
					t.Fatal("exact maximum decoded body was altered or omitted")
				}
			} else if payload["bodyOmitted"] != true || payload["body"] != nil {
				t.Fatal("oversized decoded body was returned")
			}
		})
	}
}
