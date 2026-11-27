package mcptools

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// contractAllOnPlan enables every capability token, so the plan registers
// every tool. A new tool behind a new token must also be added here.
func contractAllOnPlan() CapabilityPlan {
	return NewCapabilityPlanFromConfig(false, config.CapabilitySet{
		CalendarRead: true, CalendarWrite: true, CalendarDelete: true,
		ContactsRead: true, ContactsWrite: true, ContactsDelete: true,
		MailRead: true, MailMutate: true, MailTrash: true, MailSend: true, MailArchive: true,
		MailAttachmentsRead: true, MailAttachmentsSend: true, MailDrafts: true,
	})
}

// argumentErrorCase is one schema-valid argument that a handler must reject
// before any request. field must occur in the error message.
type argumentErrorCase struct {
	name  string
	field string
	args  map[string]any
}

// argumentErrorCases lists the handler-level argument rejections of each
// registered tool. A tool without handler-level argument checks has an empty
// list: the SDK input schema refuses all its arguments.
func argumentErrorCases() map[string][]argumentErrorCase {
	set := func(tool string, values map[string]any) map[string]any {
		return contractMerge(contractBusinessFixtures()[tool].base, values)
	}
	identity := map[string]any{"mailbox": "INBOX", "uid_validity": 9, "uid": 2}
	part := contractMerge(identity, map[string]any{"part_id": "2"})
	badRef := []any{contractMerge(part, map[string]any{"mailbox": "IN\x01BOX"})}
	stage := map[string]any{"filename": "notes.txt", "content_type": "text/plain", "data": "aGVsbG8="}
	draft := map[string]any{"to": []any{"recipient@example.invalid"}, "body": "Synthetic draft body"}
	return map[string][]argumentErrorCase{
		"icloud_capabilities":   nil,
		"calendar_capabilities": nil,
		"list_calendars":        nil,
		"list_address_books":    nil,
		"list_mailboxes":        nil,
		"create_event": {
			{"relative calendar", "calendar", set("create_event", map[string]any{"calendar": "cal/allowed/"})},
			{"title NUL", "title", set("create_event", map[string]any{"title": "a\x00b"})},
			{"location NUL", "location", set("create_event", map[string]any{"location": "a\x00b"})},
			{"notes NUL", "notes", set("create_event", map[string]any{"notes": "a\x00b"})},
			{"title CR", "title", set("create_event", map[string]any{"title": "a\rb"})},
			{"alarms list", "alarms_minutes", set("create_event", map[string]any{"alarms_minutes": "soon"})},
			{"rrule frequency", "rrule", set("create_event", map[string]any{"rrule": "FREQ=SOMETIMES"})},
			{"rrule prefix", "rrule", set("create_event", map[string]any{"rrule": "RRULE:FREQ=DAILY"})},
			{"start", "start", set("create_event", map[string]any{"start": "tomorrow"})},
			{"timezone", "timezone", set("create_event", map[string]any{"timezone": "Nowhere/Invalid"})},
			{"client uid", "client_uid", set("create_event", map[string]any{"client_uid": "a/b"})},
		},
		"update_event": {
			{"uid separator", "uid", set("update_event", map[string]any{"uid": "a/b"})},
			{"relative calendar", "calendar", set("update_event", map[string]any{"calendar": "cal/allowed/"})},
			{"title NUL", "title", set("update_event", map[string]any{"title": "a\x00b"})},
			{"location NUL", "location", set("update_event", map[string]any{"location": "a\x00b"})},
			{"notes NUL", "notes", set("update_event", map[string]any{"notes": "a\x00b"})},
			{"weak etag", "etag", set("update_event", map[string]any{"etag": `W/"v1"`})},
			{"status", "status", set("update_event", map[string]any{"status": "MAYBE"})},
			{"start", "start", set("update_event", map[string]any{"start": "tomorrow"})},
		},
		"delete_event": {
			{"uid separator", "uid", set("delete_event", map[string]any{"uid": "a/b"})},
			{"relative calendar", "calendar", set("delete_event", map[string]any{"calendar": "cal/allowed/"})},
			{"weak etag", "etag", set("delete_event", map[string]any{"etag": `W/"v1"`})},
			{"occurrence without id", "recurrence_id", set("delete_event", map[string]any{"scope": "occurrence"})},
		},
		"get_event": {
			{"uid separator", "uid", set("get_event", map[string]any{"uid": "a/b"})},
			{"relative calendar", "calendar", set("get_event", map[string]any{"calendar": "cal/allowed/"})},
		},
		"search_events": {
			{"relative calendar", "calendar", set("search_events", map[string]any{"calendar": "cal/allowed/"})},
			{"relative calendars item", "calendar", set("search_events", map[string]any{"calendars": contractCalendar + ",cal/other/"})},
			{"query NUL", "query", set("search_events", map[string]any{"calendar": contractCalendar, "query": "a\x00b"})},
			{"query NUL without selection", "query", set("search_events", map[string]any{"query": "a\x00b"})},
			{"uid without selection", "uid", set("search_events", map[string]any{"uid": "a/b"})},
			{"malformed cursor", "cursor", set("search_events", map[string]any{"calendar": contractCalendar, "cursor": "not-a-cursor"})},
			{"start", "start", set("search_events", map[string]any{"start": "tomorrow"})},
		},
		"find_free_slots": {
			{"relative calendar", "calendar", set("find_free_slots", map[string]any{"calendar": "cal/allowed/"})},
			{"empty calendars", "calendar", set("find_free_slots", map[string]any{"calendars": " , "})},
			{"working hours", "working_hours_start", set("find_free_slots", map[string]any{"calendar": contractCalendar, "working_hours_start": "nine"})},
			{"days of week", "day_of_week", set("find_free_slots", map[string]any{"calendar": contractCalendar, "days_of_week": "someday"})},
			{"working hours without selection", "working_hours_start", set("find_free_slots", map[string]any{"working_hours_start": "nine"})},
			{"working hours end without selection", "working_hours_end", set("find_free_slots", map[string]any{"working_hours_end": "25:00"})},
			{"days of week without selection", "day_of_week", set("find_free_slots", map[string]any{"days_of_week": "someday"})},
			{"duration longer than range", "duration", set("find_free_slots", map[string]any{"calendar": contractCalendar, "start": "2026-07-01T09:00:00Z", "end": "2026-07-01T09:30:00Z"})},
			{"duration longer than range without selection", "duration", set("find_free_slots", map[string]any{"start": "2026-07-01T09:00:00Z", "end": "2026-07-01T09:30:00Z"})},
			{"timezone", "timezone", set("find_free_slots", map[string]any{"timezone": "Nowhere/Invalid"})},
		},
		"validate_event": {
			{"start", "start", set("validate_event", map[string]any{"start": "tomorrow"})},
			{"alarms list", "alarms_minutes", set("validate_event", map[string]any{"alarms_minutes": "soon"})},
		},
		"get_contact": {
			{"uid control", "uid", set("get_contact", map[string]any{"uid": "a\x01b"})},
		},
		"create_contact": {
			{"birthday", "birthday", set("create_contact", map[string]any{"birthday": "2026-13-45"})},
		},
		"update_contact": {
			{"weak etag", "etag", set("update_contact", map[string]any{"etag": `W/"v1"`})},
		},
		"delete_contact": {
			{"weak etag", "etag", set("delete_contact", map[string]any{"etag": `W/"v1"`})},
		},
		"search_contacts": {
			{"malformed cursor", "cursor", set("search_contacts", map[string]any{"cursor": "not-a-cursor"})},
		},
		"get_message": {
			{"mailbox control", "mailbox", contractMerge(identity, map[string]any{"mailbox": "IN\x01BOX"})},
		},
		"search_messages": {
			{"since date", "since", set("search_messages", map[string]any{"since": "2026-13-45"})},
		},
		"set_message_flags": {
			{"mailbox control", "mailbox", set("set_message_flags", map[string]any{"mailbox": "IN\x01BOX"})},
		},
		"move_message": {
			{"same destination", "destination", set("move_message", map[string]any{"destination": "INBOX"})},
		},
		"trash_message": {
			{"mailbox control", "mailbox", set("trash_message", map[string]any{"mailbox": "IN\x01BOX"})},
		},
		"send_message": {
			{"recipient", "to", set("send_message", map[string]any{"to": []any{"not an address"}})},
			{"subject CR", "subject", set("send_message", map[string]any{"subject": "a\rb"})},
			{"attachment mailbox control", "mailbox", set("send_message", map[string]any{"attachments": badRef})},
		},
		"reply_message": {
			{"mailbox control", "mailbox", set("reply_message", map[string]any{"mailbox": "IN\x01BOX"})},
			{"body NUL", "body", set("reply_message", map[string]any{"body": "a\x00b"})},
			{"attachment mailbox control", "mailbox", set("reply_message", map[string]any{"attachments": badRef})},
		},
		"get_attachment": {
			{"mailbox control", "mailbox", contractMerge(part, map[string]any{"mailbox": "IN\x01BOX"})},
		},
		"stage_attachment": {
			{"bad base64", "data", contractMerge(stage, map[string]any{"data": "!!!!"})},
			{"filename path separator", "filename", contractMerge(stage, map[string]any{"filename": "a/b.txt"})},
			{"first chunk without filename", "filename", contractOmit(stage, "filename")},
		},
		"create_draft": {
			{"recipient", "to", contractMerge(draft, map[string]any{"to": []any{"not an address"}})},
			{"subject CR", "subject", contractMerge(draft, map[string]any{"subject": "a\rb"})},
			{"body NUL", "body", contractMerge(draft, map[string]any{"body": "a\x00b"})},
			{"attachment mailbox control", "mailbox", contractMerge(draft, map[string]any{"attachments": badRef})},
		},
	}
}

// TestArgumentErrorsUseValidationCode proves the public contract for every
// handler-level argument rejection: code validation, not retryable, the field
// in the message, and no backend request.
func TestArgumentErrorsUseValidationCode(t *testing.T) {
	universe := newContractRuntime(t, contractProfile{plan: contractAllOnPlan()})
	if got, want := len(universe.tools), contractAllOnPlan().ToolCount(); got != want {
		t.Fatalf("all-on universe lists %d tools, want %d", got, want)
	}
	cases := argumentErrorCases()
	names := make([]string, 0, len(universe.tools))
	for name := range universe.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			toolCases, ok := cases[name]
			if !ok {
				t.Fatal("registered tool has no argument error cases")
			}
			for _, test := range toolCases {
				t.Run(test.name, func(t *testing.T) {
					f := newContractRuntime(t, contractProfile{plan: contractAllOnPlan()})
					args := contractClone(test.args)
					for key, value := range args {
						if text, ok := value.(string); ok && text == "$book" {
							args[key] = f.book
						}
					}
					before := f.counts()
					result := f.call(t, name, args)
					text := resultText(t, result)
					if !result.IsError {
						t.Fatalf("argument was accepted: %s", text)
					}
					if contractSchemaRefusal(text) {
						t.Fatalf("case must pass the input schema and reach the handler: %s", text)
					}
					payload := contractPayload(t, result)
					if payload["code"] != string(icloud.CodeValidation) {
						t.Fatalf("code = %v, want validation: %s", payload["code"], text)
					}
					if retryable, present := payload["retryable"]; present && retryable != false {
						t.Fatalf("validation error is retryable: %s", text)
					}
					message, _ := payload["message"].(string)
					if !strings.Contains(strings.ToLower(message), test.field) {
						t.Fatalf("message does not name %s: %q", test.field, message)
					}
					if !reflect.DeepEqual(before, f.counts()) {
						t.Fatalf("rejected argument reached a backend: before=%v after=%v", before, f.counts())
					}
				})
			}
		})
	}
	for name := range cases {
		if _, ok := universe.tools[name]; !ok {
			t.Errorf("argument error cases name an unregistered tool %s", name)
		}
	}
}

// TestCalendarSelectionFailureIsNotValidation keeps a failed calendar listing
// out of the validation code. The listing runs after the argument checks.
func TestCalendarSelectionFailureIsNotValidation(t *testing.T) {
	listing := errors.New("synthetic listing failure")
	for _, test := range []struct {
		name string
		call func(*icloud.MockService) (string, error)
	}{
		{"search_events", func(svc *icloud.MockService) (string, error) {
			res, err := searchEventsHandler(testDeps(svc))(t.Context(), newReq(map[string]any{
				"start": "2026-07-01T09:00:00Z", "end": "2026-07-03T00:00:00Z",
			}))
			if err != nil {
				return "", err
			}
			return resultText(t, res), nil
		}},
		{"find_free_slots", func(svc *icloud.MockService) (string, error) {
			res, err := findFreeSlotsHandler(testDeps(svc))(t.Context(), newReq(map[string]any{
				"start": "2026-07-01T09:00:00Z", "end": "2026-07-03T00:00:00Z", "duration_minutes": 60,
			}))
			if err != nil {
				return "", err
			}
			return resultText(t, res), nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := &icloud.MockService{ListErr: listing}
			text, err := test.call(svc)
			if err != nil {
				t.Fatal(err)
			}
			if svc.ListCallCount != 1 {
				t.Fatalf("listing calls = %d, want 1", svc.ListCallCount)
			}
			if strings.Contains(text, `"code":"validation"`) {
				t.Fatalf("listing failure was reported as validation: %s", text)
			}
		})
	}
}

// contractSchemaRefusal reports whether text is the project error payload for
// an input schema refusal.
func contractSchemaRefusal(text string) bool {
	return strings.Contains(text, `"code":"validation"`) && strings.Contains(text, schemaRefusalSummary)
}

// TestSchemaRefusalsUseValidationCode sends arguments that the input schema
// refuses before any handler. The refusal must use the project payload, name
// only declared properties, repeat no caller value or key, and send no request.
func TestSchemaRefusalsUseValidationCode(t *testing.T) {
	set := func(tool string, values map[string]any) map[string]any {
		return contractMerge(contractBusinessFixtures()[tool].base, values)
	}
	const summary = "validation: " + schemaRefusalSummary + ": "
	for _, test := range []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"wrong type", "create_event", set("create_event", map[string]any{"title": 123}), "invalid value for title"},
		{"wrong item type", "send_message", set("send_message", map[string]any{"to": []any{7}}), "invalid value for to"},
		{"pattern mismatch", "get_contact", map[string]any{"address_book": echoMarker, "uid": "contact"}, "invalid value for address_book"},
		{"date pattern", "search_messages", set("search_messages", map[string]any{"since": echoMarker + "1"}), "invalid value for since"},
		{"enum", "create_event", set("create_event", map[string]any{"status": echoMarker}), "invalid value for status"},
		{"bound", "find_free_slots", set("find_free_slots", map[string]any{"duration_minutes": 98765}), "invalid value for duration_minutes"},
		{"length", "search_events", set("search_events", map[string]any{"query": strings.Repeat(echoMarker, 30)}), "invalid value for query"},
		{"unknown key", "search_events", set("search_events", map[string]any{echoMarker: true}), "unknown argument"},
		{"unknown nested key", "create_contact", set("create_contact", map[string]any{"name": map[string]any{echoMarker: "x"}}), "invalid value for name; unknown argument"},
		{"missing required", "get_event", map[string]any{"calendar": contractCalendar}, "missing required argument uid"},
		{"missing all required", "search_events", map[string]any{}, "missing required argument end, start"},
		{"mixed", "create_event", map[string]any{"title": 5, "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z", echoMarker: 1}, "missing required argument calendar; invalid value for title; unknown argument"},
		{"several invalid", "set_message_flags", map[string]any{"mailbox": "INBOX", "uid_validity": 1.5, "uid": -1, "operation": "add", "flags": []any{echoMarker}}, "invalid value for flags, uid, uid_validity"},
		{"several types", "search_events", map[string]any{"start": 1, "end": []any{"x"}, "query": map[string]any{"a": 1}}, "invalid value for end, query, start"},
		{"crafted location separators", "get_contact", map[string]any{"address_book": echoMarker + "; /uid: boom; /calendar: x", "uid": "contact"}, "invalid value for address_book"},
		{"crafted declared name", "search_messages", map[string]any{"mailbox": "INBOX", "since": echoMarker + "; /mailbox: injected"}, "invalid value for since"},
		{"crafted unknown rule", "get_contact", map[string]any{"address_book": echoMarker + ": additional properties 'x' not allowed", "uid": "contact"}, "invalid value for address_book"},
		{"crafted unknown key", "get_contact", map[string]any{"address_book": "$book", "uid": "contact", echoMarker + "; /uid: boom": 1}, "unknown argument"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: contractAllOnPlan()})
			args := contractClone(test.args)
			for key, value := range args {
				if text, ok := value.(string); ok && text == "$book" {
					args[key] = f.book
				}
			}
			before := f.counts()
			result := f.call(t, test.tool, args)
			text := resultText(t, result)
			if !result.IsError || result.StructuredContent != nil || len(result.Content) != 1 {
				t.Fatalf("schema refusal shape: isError=%v structured=%v content=%d", result.IsError, result.StructuredContent, len(result.Content))
			}
			if !contractSchemaRefusal(text) {
				t.Fatalf("schema refusal is not the project payload: %s", text)
			}
			payload := contractPayload(t, result)
			if retryable, present := payload["retryable"]; present && retryable != false {
				t.Fatalf("schema refusal is retryable: %s", text)
			}
			if message, _ := payload["message"].(string); message != summary+test.want {
				t.Fatalf("message = %q, want %q", message, summary+test.want)
			}
			if strings.Contains(strings.ToUpper(text), echoMarker) || strings.Contains(text, "98765") {
				t.Fatalf("schema refusal repeats caller input: %s", text)
			}
			if !reflect.DeepEqual(before, f.counts()) {
				t.Fatalf("schema refusal reached a backend: before=%v after=%v", before, f.counts())
			}
		})
	}
}

// TestSchemaRefusalDetectorKeepsOtherResults pins the exact SDK refusal shape.
// A handler never returns these other shapes, so no protocol case reaches them.
func TestSchemaRefusalDetectorKeepsOtherResults(t *testing.T) {
	sdk := mcp.NewToolResultError("input schema validation failed: /title: got number, want string")
	if !isSDKSchemaRefusal(sdk) {
		t.Fatal("SDK refusal was not detected")
	}
	payload := errResult(nil, "title parameter", errors.New("input schema validation failed: x"))
	notError := mcp.NewToolResultText("input schema validation failed: x")
	twoItems := mcp.NewToolResultError("invalid arguments: x")
	twoItems.Content = append(twoItems.Content, mcp.NewTextContent("second"))
	structured := mcp.NewToolResultError("invalid arguments: x")
	structured.StructuredContent = map[string]any{"code": "validation"}
	for name, result := range map[string]*mcp.CallToolResult{
		"handler payload": payload, "success": notError, "two items": twoItems, "structured": structured, "nil": nil,
	} {
		if isSDKSchemaRefusal(result) {
			t.Errorf("%s was treated as an SDK refusal", name)
		}
	}
}

// TestValidationMessagesNameArgumentAndRule pins the messages that must name
// the exact argument and the rule that failed.
func TestValidationMessagesNameArgumentAndRule(t *testing.T) {
	set := func(tool string, values map[string]any) map[string]any {
		return contractMerge(contractBusinessFixtures()[tool].base, values)
	}
	const uidRule = "UID must not contain '/', '\\', '%', or control characters"
	draft := map[string]any{"to": []any{"recipient@example.invalid"}, "body": "Synthetic draft body"}
	for _, test := range []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"get_event uid", "get_event", set("get_event", map[string]any{"uid": "a/b"}), "uid parameter: " + uidRule},
		{"search_events uid", "search_events", set("search_events", map[string]any{"uid": "a/b"}), "uid parameter: " + uidRule},
		{"delete_event uid", "delete_event", set("delete_event", map[string]any{"uid": "a\x01b"}), "uid parameter: " + uidRule},
		{"create_event client_uid", "create_event", set("create_event", map[string]any{"client_uid": "a/b"}), "client_uid: " + uidRule},
		{"uid traversal", "get_event", set("get_event", map[string]any{"uid": "a..b"}), "uid parameter: UID must not contain a directory traversal sequence ('..')"},
		{"mailbox", "get_message", map[string]any{"mailbox": "IN\x01BOX", "uid_validity": 9, "uid": 2}, "mailbox must be valid UTF-8 of at most 4096 bytes without control characters other than tab"},
		{"search value", "search_messages", set("search_messages", map[string]any{"query": "a\x01b"}), "query must be valid UTF-8 of at most 512 bytes without control characters"},
		{"to recipient", "send_message", set("send_message", map[string]any{"to": []any{"not an address"}}), "to must contain only plain addr-specs, for example name@example.com"},
		{"cc recipient", "send_message", set("send_message", map[string]any{"cc": []any{"not an address"}}), "cc must contain only plain addr-specs, for example name@example.com"},
		{"bcc recipient", "send_message", set("send_message", map[string]any{"bcc": []any{"not an address"}}), "bcc must contain only plain addr-specs, for example name@example.com"},
		{"draft cc recipient", "create_draft", contractMerge(draft, map[string]any{"cc": []any{"not an address"}}), "cc must contain only plain addr-specs, for example name@example.com"},
		{"subject", "send_message", set("send_message", map[string]any{"subject": "a\rb"}), "subject must be valid UTF-8 of at most 998 bytes without control characters"},
		{"draft subject", "create_draft", contractMerge(draft, map[string]any{"subject": "a\rb"}), "subject must be valid UTF-8 of at most 998 bytes without control characters"},
		{"body", "reply_message", set("reply_message", map[string]any{"body": "a\x00b"}), "body must be valid UTF-8 of at most 102400 bytes without NUL"},
		{"draft body", "create_draft", contractMerge(draft, map[string]any{"body": "a\x00b"}), "body must be valid UTF-8 of at most 102400 bytes without NUL"},
		{"send body", "send_message", set("send_message", map[string]any{"body": "a\x00b"}), "body must be valid UTF-8 of at most 102400 bytes without NUL"},
		{"working_hours_start form", "find_free_slots", set("find_free_slots", map[string]any{"working_hours_start": "nine"}), "working_hours_start must use HH:MM"},
		{"working_hours_end range", "find_free_slots", set("find_free_slots", map[string]any{"working_hours_end": "25:00"}), "working_hours_end must be between 00:00 and 24:00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: contractAllOnPlan()})
			before := f.counts()
			result := f.call(t, test.tool, test.args)
			payload := contractPayload(t, result)
			message, _ := payload["message"].(string)
			if !result.IsError || payload["code"] != string(icloud.CodeValidation) || !strings.Contains(message, test.want) {
				t.Fatalf("message = %q, want validation with %q", message, test.want)
			}
			if !reflect.DeepEqual(before, f.counts()) {
				t.Fatal("rejected argument reached a backend")
			}
		})
	}
}
