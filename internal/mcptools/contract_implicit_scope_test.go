package mcptools

import (
	"strings"
	"testing"
)

func TestContractImplicitCalendarScopeRealBackends(t *testing.T) {
	const forbidden = "/cal/forbidden/"
	for _, name := range []string{"search_events", "find_free_slots"} {
		t.Run(name, func(t *testing.T) {
			for _, allowed := range []bool{false, true} {
				t.Run(map[bool]string{false: "none-permitted", true: "one-permitted"}[allowed], func(t *testing.T) {
					var previous string
					for _, busy := range []bool{false, true} {
						t.Run(map[bool]string{false: "denied-empty", true: "denied-busy"}[busy], func(t *testing.T) {
							scope := "calendar.read=/cal/not-listed/"
							if allowed {
								scope = "calendar.read=" + contractCalendar
							}
							f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true), scopes: scope}, func(f *contractRuntime) {
								f.calendar.extraCollections = []string{forbidden}
								if busy {
									f.calendar.objects[forbidden+"private.ics"] = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Contract//EN\r\nBEGIN:VEVENT\r\nUID:private\r\nDTSTAMP:20260701T000000Z\r\nDTSTART:20260701T090000Z\r\nDTEND:20260703T000000Z\r\nSUMMARY:private-busy\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
								}
							})
							result := f.call(t, name, f.arguments(name, ""))
							reports := 0
							for _, req := range f.calendar.requests {
								if req.Method == "REPORT" {
									reports++
									if req.Path != contractCalendar {
										t.Fatalf("queried forbidden calendar %s", req.Path)
									}
								}
							}
							want := 0
							if allowed {
								want = 1
								if result.IsError {
									t.Fatal(resultText(t, result))
								}
								contractValidateOutput(t, f.tools[name], result)
							} else if !result.IsError || contractPayload(t, result)["code"] != "authorization" {
								t.Fatal("no-permitted selection fabricated a success")
							}
							if reports != want || len(f.calendar.requests) != 3+want {
								t.Fatalf("selection backend counts reports=%d requests=%d", reports, len(f.calendar.requests))
							}
							text := resultText(t, result)
							if strings.Contains(text, forbidden) || strings.Contains(text, "private-busy") {
								t.Fatal("forbidden resource leaked")
							}
							if busy && text != previous {
								t.Fatal("denied busy data changed the authorized result")
							}
							previous = text
						})
					}
				})
			}
		})
	}
}
