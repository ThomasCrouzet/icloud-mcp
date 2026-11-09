package mcptools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	msgmail "github.com/emersion/go-message/mail"
	"github.com/mark3labs/mcp-go/mcp"
)

func contractPayload(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.StructuredContent == nil {
		var payload map[string]any
		decoder := json.NewDecoder(strings.NewReader(resultText(t, res)))
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	return contractValue(res.StructuredContent).(map[string]any)
}

func contractSame(t *testing.T, label string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(contractValue(got), contractValue(want)) {
		t.Fatalf("%s: actual=%v expected=%v", label, got, want)
	}
}

func (f *contractRuntime) verifyValues(t *testing.T, name string, path []string, args map[string]any, res *mcp.CallToolResult) {
	t.Helper()
	key := path[0]
	payload := contractPayload(t, res)
	switch name {
	case "create_event", "update_event", "validate_event":
		var normalized map[string]any
		if name == "validate_event" {
			normalized = payload["normalized"].(map[string]any)
		} else {
			service := icloud.NewClient(f.calendar, "https://caldav.icloud.com", func(h string) bool { return h == "caldav.icloud.com" })
			uid := payload["uid"].(string)
			got, err := service.GetEvent(t.Context(), args["calendar"].(string), uid)
			if err != nil {
				t.Fatal(err)
			}
			normalized = contractValue(got).(map[string]any)
			if args["scope"] == "occurrence" {
				if got.OverrideCount != 1 || len(got.Overrides) != 1 || got.Overrides[0].Title != "Changed" {
					t.Fatalf("occurrence argument was dropped: %+v", got)
				}
				return
			}
			if key == "alarm_minutes_before" || key == "alarms_minutes" {
				want := []int{10}
				if key == "alarms_minutes" {
					want = []int{5, 15}
				}
				if len(got.Alarms) != len(want) {
					t.Fatalf("alarms lost: %+v", got.Alarms)
				}
				for i, n := range want {
					if got.Alarms[i].MinutesBefore != n {
						t.Fatalf("alarm offset lost: %+v", got.Alarms)
					}
				}
			}
			if key == "recurrence_exceptions" {
				result, err := service.SearchEvents(t.Context(), args["calendar"].(string), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC), nil)
				count := 0
				for _, event := range result.Events {
					if event.UID == uid {
						count++
					}
				}
				if err != nil || count != 1 {
					t.Fatalf("EXDATE not observable in real search: %+v %v", result, err)
				}
			}
		}
		if _, ok := args["title"]; ok {
			contractSame(t, "persisted title", normalized["title"], args["title"])
		}
		for _, field := range []string{"location", "notes", "status", "transparency", "url", "timezone"} {
			if v, ok := args[field]; ok {
				contractSame(t, "normalized/persisted "+field, normalized[field], v)
			}
		}
		for _, field := range []string{"start", "end"} {
			if raw, ok := args[field].(string); ok {
				var expected time.Time
				var err error
				if args["all_day"] == true {
					expected, err = time.Parse("2006-01-02", raw)
				} else {
					expected, err = time.Parse(time.RFC3339, raw)
				}
				if err != nil {
					t.Fatal(err)
				}
				layout := time.RFC3339
				if name == "validate_event" && args["all_day"] == true {
					layout = "2006-01-02"
				}
				actual, err := time.Parse(layout, normalized[field].(string))
				if err != nil || !actual.Equal(expected) {
					t.Fatalf("%s instant lost: %v != %v", field, actual, expected)
				}
			}
		}
		if key == "all_day" {
			actual, _ := normalized["allDay"].(bool)
			contractSame(t, "all-day value", actual, args["all_day"])
		}
		rrule, _ := normalized["recurrence"].(string)
		for field, token := range map[string]string{"recurrence_frequency": "FREQ=", "recurrence_interval": "INTERVAL=", "recurrence_count": "COUNT=", "recurrence_by_day": "BYDAY="} {
			if value, ok := args[field]; ok {
				expected := strings.ToUpper(fmt.Sprint(value))
				if !strings.Contains(rrule, token+expected) {
					t.Fatalf("structured recurrence dropped %s: %s", field, rrule)
				}
			}
		}
		if key == "rrule" && !strings.Contains(rrule, "COUNT=2") {
			t.Fatalf("RRULE dropped: %s", rrule)
		}
		if key == "recurrence_until" && !strings.Contains(rrule, "UNTIL=20260710T100000Z") {
			t.Fatalf("UNTIL dropped: %s", rrule)
		}
		if key == "client_uid" || key == "idempotency_key" && name != "update_event" {
			field := "uid"
			contractSame(t, "client-selected UID", normalized[field], args[key])
		}
		if name == "update_event" && key == "idempotency_key" {
			f.verifyIdempotency(t, name, args, res)
		}
	case "create_contact", "update_contact":
		service := contacts.NewClient(f.contacts, "https://contacts.icloud.com", func(h string) bool { return h == "contacts.icloud.com" })
		got, err := service.GetContact(t.Context(), f.book, payload["uid"].(string))
		if err != nil {
			t.Fatal(err)
		}
		actual := contractValue(got).(map[string]any)
		for _, field := range []string{"display_name", "name", "organization", "title", "nickname", "birthday", "notes", "emails", "phones", "addresses", "urls"} {
			if value, ok := args[field]; ok {
				target := field
				if field == "display_name" {
					target = "displayName"
				}
				contractSame(t, "persisted contact "+field, actual[target], value)
			}
		}
		if name == "create_contact" && (key == "client_uid" || key == "idempotency_key") {
			contractSame(t, "client-selected contact UID", got.UID, args[key])
		}
		if name == "update_contact" && key == "idempotency_key" {
			f.verifyIdempotency(t, name, args, res)
		}
	case "send_message", "reply_message":
		f.mail.mu.Lock()
		data := bytes.Clone(f.mail.payloads[0])
		recipients := append([]string(nil), f.mail.recipients...)
		archived := append([][]byte(nil), f.mail.archived...)
		f.mail.mu.Unlock()
		reader, err := msgmail.CreateReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(part.Body)
		if err != nil {
			t.Fatal(err)
		}
		wantedBody := strings.ReplaceAll(args["body"].(string), "\r\n", "\n")
		if !strings.HasSuffix(wantedBody, "\n") {
			wantedBody += "\n"
		}
		contractSame(t, "actual DATA body", strings.ReplaceAll(string(body), "\r\n", "\n"), wantedBody)
		if subject, ok := args["subject"].(string); ok {
			actual, err := reader.Header.Subject()
			if err != nil {
				t.Fatal(err)
			}
			contractSame(t, "actual subject", actual, subject)
		}
		if name == "reply_message" {
			want := 1
			if args["mode"] == "reply_all" {
				want = 2
			}
			if len(recipients) != want {
				t.Fatalf("reply mode dropped: %v", recipients)
			}
			contractSame(t, "actual In-Reply-To", reader.Header.Get("In-Reply-To"), "<source@example.invalid>")
		}
		for _, field := range []string{"to", "cc", "bcc"} {
			if values, ok := args[field].([]any); ok {
				for _, address := range values {
					found := false
					for _, r := range recipients {
						found = found || strings.Contains(r, address.(string))
					}
					if !found {
						t.Fatalf("recipient dropped: %s %v", field, address)
					}
				}
			}
		}
		if reader.Header.Get("Bcc") != "" {
			t.Fatal("BCC header disclosed")
		}
		if key == "archive_sent" && args[key] == true {
			if len(archived) != 1 {
				t.Fatal("archive_sent dropped")
			}
			// DotReader normalizes CRLF and the SMTP writer adds a terminal CRLF.
			// Preserve every content byte; account only for that DATA framing.
			canonical := strings.ReplaceAll(string(archived[0]), "\r\n", "\n")
			if !strings.HasSuffix(canonical, "\n") {
				canonical += "\n"
			}
			contractSame(t, "SMTP/APPEND content with explicit DATA framing", canonical, string(data))
		}
		if key == "archive_sent" && args[key] == false {
			f.mail.mu.Lock()
			if f.mail.appends != 0 || f.mail.imapDials != 0 {
				t.Fatal("archive_sent=false caused archive traffic")
			}
			f.mail.mu.Unlock()
		}
		if key == "idempotency_key" {
			f.verifyIdempotency(t, name, args, res)
		}
	case "get_message":
		contractSame(t, "read mailbox", payload["mailbox"], args["mailbox"])
		contractSame(t, "read UID", payload["uid"], args["uid"])
		contractSame(t, "read UIDVALIDITY", payload["uidValidity"], args["uid_validity"])
		if key == "max_body_bytes" && args[key] == float64(4) || key == "max_body_bytes" && args[key] == 4 {
			if payload["bodyOmitted"] != true {
				t.Fatal("max_body_bytes ignored")
			}
		}
	case "set_message_flags":
		f.mail.mu.Lock()
		commands := strings.Join(f.mail.commands, "\n")
		f.mail.mu.Unlock()
		operation := "+FLAGS.SILENT"
		if args["operation"] == "remove" {
			operation = "-FLAGS.SILENT"
		}
		if !strings.Contains(commands, operation) {
			t.Fatalf("delta operation dropped: %s", commands)
		}
		for _, flag := range args["flags"].([]any) {
			if !strings.Contains(strings.ToLower(commands), "\\"+flag.(string)) {
				t.Fatalf("flag dropped: %s", commands)
			}
		}
	case "move_message", "trash_message":
		expected := "Trash"
		if name == "move_message" {
			expected = args["destination"].(string)
		}
		contractSame(t, "actual destination", payload["destination"], expected)
	case "delete_event", "delete_contact":
		if key == "dry_run" {
			field := "dryRun"
			actual, _ := payload[field].(bool)
			contractSame(t, "dry-run result", actual, args[key])
		}
		if key == "scope" || key == "recurrence_id" {
			contractSame(t, "actual scope", payload["scope"], args["scope"])
			if args["scope"] == "occurrence" {
				f.calendar.mu.Lock()
				object := f.calendar.objects[contractCalendar+"event.ics"]
				f.calendar.mu.Unlock()
				if !strings.Contains(object, "EXDATE:20260702T100000Z") {
					t.Fatalf("occurrence deletion lost: %s", object)
				}
			}
		}
	case "search_messages":
		f.mail.mu.Lock()
		commands := strings.Join(f.mail.commands, "\n")
		f.mail.mu.Unlock()
		for field, token := range map[string]string{"query": "TEXT", "from": "FROM", "to": "TO", "subject": "SUBJECT", "since": "SINCE", "before": "BEFORE", "unseen_only": "UNSEEN", "flagged_only": "FLAGGED"} {
			value, present := args[field]
			if !present {
				continue
			}
			if value == false {
				if strings.Contains(commands, token) {
					t.Fatalf("false search predicate enabled %s: %s", field, commands)
				}
				continue
			}
			if !strings.Contains(commands, token) {
				t.Fatalf("search predicate dropped %s: %s", field, commands)
			}
			if text, ok := value.(string); ok {
				if field == "since" || field == "before" {
					date, err := time.Parse("2006-01-02", text)
					if err != nil {
						t.Fatal(err)
					}
					text = date.Format("2-Jan-2006")
				}
				if !strings.Contains(commands, text) {
					t.Fatalf("search value dropped %s: %s", field, commands)
				}
			}
		}
	case "search_contacts":
		observed, ok := f.contactTrace.lastSearch()
		if !ok {
			t.Fatal("search never reached the real Contacts service")
		}
		actual := map[string]any{"address_book": observed.AddressBook, "query": observed.Query, "email": observed.Email, "phone": observed.Phone, "include_groups": observed.IncludeGroups, "limit": observed.Limit}
		for field, value := range args {
			if field == "cursor" {
				continue
			}
			contractSame(t, "accepted Contacts search value "+field, actual[field], value)
		}
	case "search_events":
		if key == "limit" {
			contractSame(t, "returned page limit", payload["limit"], args["limit"])
		}
		if key == "offset" {
			contractSame(t, "returned page offset", payload["offset"], args["offset"])
		}
		if key == "expand_recurrence" {
			events := payload["events"].([]any)
			if args[key] == false && (len(events) != 1 || events[0].(map[string]any)["recurrence"] == nil) || args[key] == true && len(events) != 2 {
				t.Fatal("expand_recurrence=false ignored")
			}
		}
		if key == "compact" {
			for _, event := range payload["events"].([]any) {
				if args[key] == true && event.(map[string]any)["notes"] != nil || args[key] == false && event.(map[string]any)["notes"] != "Original notes" || event.(map[string]any)["location"] != "Room" {
					t.Fatal("compact ignored")
				}
			}
		}
	case "find_free_slots":
		slots := payload["slots"].([]any)
		if key == "working_hours_end" && len(slots) == 0 {
			t.Fatal("working_hours_end oracle requires nonempty actual slots")
		}
		for _, item := range slots {
			slot := item.(map[string]any)
			start, err := time.Parse(time.RFC3339, slot["start"].(string))
			if err != nil {
				t.Fatal(err)
			}
			end, err := time.Parse(time.RFC3339, slot["end"].(string))
			if err != nil {
				t.Fatal(err)
			}
			minutes := float64(60)
			if n, ok := args["duration_minutes"].(float64); ok {
				minutes = n
			}
			if end.Sub(start) != time.Duration(minutes)*time.Minute {
				t.Fatal("duration_minutes ignored")
			}
			if key == "timezone" && !strings.HasSuffix(slot["start"].(string), "+02:00") {
				t.Fatal("timezone ignored")
			}
			if key == "working_hours_end" {
				location := time.UTC
				if zone, ok := args["timezone"].(string); ok {
					location, err = time.LoadLocation(zone)
					if err != nil {
						t.Fatal(err)
					}
				}
				clock, err := time.Parse("15:04", args[key].(string))
				if err != nil {
					t.Fatal(err)
				}
				localStart := start.In(location)
				cutoff := time.Date(localStart.Year(), localStart.Month(), localStart.Day(), clock.Hour(), clock.Minute(), 0, 0, location)
				if end.After(cutoff) {
					t.Fatalf("working_hours_end=%s ignored: local slot end %s exceeds cutoff %s (%d actual slots)", args[key], end.In(location).Format(time.RFC3339), cutoff.Format(time.RFC3339), len(slots))
				}
			}
		}
		if key == "limit" {
			contractSame(t, "slot count cap", payload["count"], args["limit"])
		}
	}
	t.Logf("contract_value_oracle=%s", contractReplayJSON(t, map[string]any{"tool": name, "property": strings.Join(path, "/"), "result": "pass", "after": f.counts()}))
}

func (f *contractRuntime) verifyIdempotency(t *testing.T, name string, args map[string]any, first *mcp.CallToolResult) {
	t.Helper()
	beforeCalendar, beforeContacts := f.calendar.mutations, f.contacts.mutations
	f.mail.mu.Lock()
	beforeData := f.mail.data
	f.mail.mu.Unlock()
	second := f.call(t, name, args)
	if second.IsError {
		t.Fatal("identical keyed repeat rejected")
	}
	contractSame(t, "cached result", second.StructuredContent, first.StructuredContent)
	f.mail.mu.Lock()
	afterData := f.mail.data
	f.mail.mu.Unlock()
	if f.calendar.mutations != beforeCalendar || f.contacts.mutations != beforeContacts || beforeData != afterData {
		t.Fatal("idempotency key permitted duplicate mutation")
	}
}
