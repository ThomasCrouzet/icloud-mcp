package mcptools

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestContractDispatchProfiles(t *testing.T) {
	universe := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
	for _, profile := range contractProfiles(t) {
		t.Run(profile.name, func(t *testing.T) {
			inventory := newContractRuntime(t, profile)
			names := make([]string, 0, len(universe.tools))
			for name := range universe.tools {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				t.Run(name, func(t *testing.T) {
					tool, enabled := inventory.tools[name]
					if !enabled {
						before := inventory.counts()
						result, err := contractCall(t, inventory.client, name, map[string]any{})
						if err == nil && (result == nil || !result.IsError) {
							t.Fatal("disabled tool dispatched")
						}
						if !reflect.DeepEqual(before, inventory.counts()) {
							t.Fatal("disabled tool accessed backend")
						}
						t.Logf("contract_profile_case=%s", contractReplayJSON(t, map[string]any{"profile": profile.name, "tool": name, "kind": "disabled", "result": result, "error": fmtContractError(err), "counters": inventory.counts()}))
						return
					}
					wantSchema := contractWireTool(t, universe.tools[name])
					actualSchema := contractWireTool(t, tool)
					for _, schema := range []string{"inputSchema", "outputSchema"} {
						if !reflect.DeepEqual(wantSchema[schema], actualSchema[schema]) {
							t.Fatalf("profile schema changed for %s; property cases must cover this variant", schema)
						}
					}
					f := newContractRuntime(t, profile)
					args := f.arguments(name, "")
					result := f.call(t, name, args)
					if result.IsError {
						t.Fatalf("enabled tool cannot dispatch in profile %s: %s", profile.name, resultText(t, result))
					}
					contractValidateOutput(t, tool, result)
					f.verify(t, name, args, result)
					t.Logf("contract_profile_case=%s", contractReplayJSON(t, map[string]any{"profile": profile.name, "tool": name, "kind": "valid", "result": result, "counters": f.counts()}))
				})
			}
		})
	}
}

func TestContractScopedDenialsRealBackends(t *testing.T) {
	for _, name := range []string{"search_events", "find_free_slots", "get_event", "create_event", "update_event", "delete_event", "get_contact", "create_contact", "update_contact", "delete_contact", "get_message", "search_messages", "set_message_flags", "move_message", "trash_message", "reply_message"} {
		t.Run(name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true), scopes: "calendar.read=/cal/denied/;calendar.write=/cal/denied/;calendar.delete=/cal/denied/;contacts.read=allowed-book;contacts.write=allowed-book;contacts.delete=allowed-book;mail.read=Other;mail.mutate=Other"}, func(f *contractRuntime) {
				if strings.Contains(name, "contact") {
					f.freshContacts = true
					f.contacts.extraCollections = []string{"/books/other/"}
					f.contacts.objects["/books/other/opaque.vcf"] = f.contacts.objects[contractBookPath+"opaque.vcf"]
				}
			})
			args := f.arguments(name, "")
			if strings.Contains(name, "contact") {
				books, err := f.contactTrace.ListAddressBooks(t.Context())
				if err != nil || len(books) != 2 {
					t.Fatalf("scope setup discovery: %v %v", books, err)
				}
				for _, book := range books {
					if book.Identifier != f.book {
						args["address_book"] = book.Identifier
					}
				}
				if args["address_book"] == f.book {
					t.Fatal("scope fixture did not select the discovered denied book")
				}
			}
			if name == "search_events" || name == "find_free_slots" {
				args["calendar"] = contractCalendar
			}
			before := f.counts()
			result := f.call(t, name, args)
			if !result.IsError {
				t.Fatal("forbidden resource accepted")
			}
			if strings.Contains(name, "contact") {
				code := "validation"
				if name == "get_contact" {
					code = "authorization"
				}
				if contractPayload(t, result)["code"] != code || !strings.Contains(resultText(t, result), "outside the configured resource scope") {
					t.Fatalf("configured scope refusal changed: %s", resultText(t, result))
				}
			}
			if !reflect.DeepEqual(before, f.counts()) {
				t.Fatal("explicit scope denial accessed backend")
			}
			t.Logf("contract_scope_case=%s", contractReplayJSON(t, map[string]any{"tool": name, "result": result, "counters": f.counts()}))
		})
	}
}
