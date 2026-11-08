package mcptools

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type contractRuntime struct {
	client             *client.Client
	calendar, contacts *contractDAVBackend
	mail               *contractMailBackend
	book               string
	tools              map[string]mcp.Tool
	freshContacts      bool
	contactTrace       *contractContactsTrace
}

func newContractRuntime(t *testing.T, p contractProfile, configure ...func(*contractRuntime)) *contractRuntime {
	t.Helper()
	f := &contractRuntime{calendar: newContractDAV(false), contacts: newContractDAV(true), mail: &contractMailBackend{}, tools: map[string]mcp.Tool{}}
	calendar := icloud.NewClient(f.calendar, "https://caldav.icloud.com", func(h string) bool { return h == "caldav.icloud.com" })
	people := contacts.NewClient(f.contacts, "https://contacts.icloud.com", func(h string) bool { return h == "contacts.icloud.com" })
	// Discover the opaque identifier through the real service. Setup traffic is
	// retained separately from the measured dispatch, including invalid cases.
	books, err := people.ListAddressBooks(t.Context())
	if err != nil || len(books) != 1 {
		t.Fatalf("synthetic book discovery: %v %v", books, err)
	}
	f.book = books[0].Identifier
	for _, option := range configure {
		option(f)
	}
	if f.freshContacts {
		people = contacts.NewClient(f.contacts, "https://contacts.icloud.com", func(h string) bool { return h == "contacts.icloud.com" })
	}
	f.contactTrace = &contractContactsTrace{Service: people}
	scopes := strings.ReplaceAll(p.scopes, "allowed-book", f.book)
	red := security.NewRedactor("synthetic-contract-secret")
	s := server.NewMCPServer("contract-dispatch", "test", server.WithToolCapabilities(false), server.WithInputSchemaValidation(), server.WithStrictInputSchemaDefault(), server.WithRecovery(), server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)))
	ConfigureMailNumberValidation(s)
	RegisterUnified(s, Deps{Service: calendar, ContactsService: f.contactTrace, MailService: f.mail.service(t), Redactor: red, Audit: security.NewAuditLogger(io.Discard), DefaultLocation: time.UTC, ResourceScopes: mustScopes(t, scopes)}, p.plan)
	f.client, err = client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.client.Close() })
	if err := f.client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "contract-dispatch", Version: "test"}
	if _, err := f.client.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	listed := contractListExact(t, f.client)
	for _, tool := range listed.Tools {
		f.tools[tool.Name] = tool
	}
	previous := defaultIdempotency
	defaultIdempotency = newIdempotencyStore()
	t.Cleanup(func() { defaultIdempotency = previous })
	return f
}

func (f *contractRuntime) counts() map[string]any {
	return map[string]any{"calendar": f.calendar.snapshot(), "contacts": f.contacts.snapshot(), "mail": f.mail.snapshot()}
}

func (f *contractRuntime) call(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	before := f.counts()
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	req := mcp.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = name, args
	res, err := f.client.CallTool(ctx, req)
	t.Logf("contract_dispatch=%s", contractReplayJSON(t, map[string]any{"tool": name, "args": args, "before": before, "after": f.counts(), "result": res, "error": fmtContractError(err)}))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

type contractBusinessFixture struct{ base, values map[string]any }

func contractBusinessFixtures() map[string]contractBusinessFixture {
	identity := func() map[string]any { return map[string]any{"mailbox": "INBOX", "uid_validity": 9, "uid": 2} }
	book := map[string]any{"address_book": "$book", "uid": "contact"}
	when := map[string]any{"start": "2026-07-01T09:00:00Z", "end": "2026-07-03T00:00:00Z"}
	event := map[string]any{"title": "New synthetic", "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z", "calendar": contractCalendar}
	fields := map[string]any{"location": "New room", "notes": "New notes", "alarm_minutes_before": 10, "alarms_minutes": "5,15", "all_day": true, "rrule": "FREQ=DAILY;COUNT=2", "recurrence_frequency": "daily", "recurrence_interval": 2, "recurrence_count": 2, "recurrence_until": "2026-07-10T10:00:00Z", "recurrence_by_day": "WE,TH", "recurrence_exceptions": "2026-07-02T10:00:00Z", "status": "TENTATIVE", "transparency": "TRANSPARENT", "url": "https://example.invalid/event", "timezone": "Europe/Paris", "client_uid": "created", "idempotency_key": "alias-created"}
	contact := map[string]any{"display_name": "New synthetic", "name": map[string]any{"familyName": "Family", "givenName": "Given", "additionalName": "Middle", "honorificPrefix": "Dr", "honorificSuffix": "Jr"}, "organization": "Synthetic Org", "title": "Engineer", "nickname": "Nick", "birthday": "2000-01-02", "notes": "Synthetic notes", "emails": []any{map[string]any{"type": "work", "value": "reply@example.invalid"}}, "phones": []any{map[string]any{"type": "work", "value": "+331234"}}, "addresses": []any{map[string]any{"type": "work", "postOfficeBox": "PO", "extendedAddress": "Unit", "streetAddress": "Street", "locality": "City", "region": "Region", "postalCode": "12345", "country": "Country"}}, "urls": []any{map[string]any{"type": "work", "value": "https://example.invalid/contact"}}, "client_uid": "created", "idempotency_key": "alias-created"}
	return map[string]contractBusinessFixture{
		"icloud_capabilities": {map[string]any{}, nil}, "calendar_capabilities": {map[string]any{}, nil}, "list_calendars": {map[string]any{}, nil}, "list_address_books": {map[string]any{}, nil}, "list_mailboxes": {map[string]any{}, nil},
		"create_event": {event, fields}, "validate_event": {map[string]any{"title": "New synthetic", "start": event["start"], "end": event["end"]}, fields},
		"get_event":       {map[string]any{"calendar": contractCalendar, "uid": "event"}, nil},
		"update_event":    {map[string]any{"calendar": contractCalendar, "uid": "event", "title": "Changed"}, map[string]any{"location": "Changed room", "notes": "Changed notes", "start": "2026-07-01T09:00:00Z", "end": "2026-07-01T12:00:00Z", "status": "TENTATIVE", "transparency": "TRANSPARENT", "url": "https://example.invalid/changed", "etag": contractETag, "idempotency_key": "update-key", "scope": "occurrence", "recurrence_id": "2026-07-02T10:00:00Z"}},
		"delete_event":    {map[string]any{"calendar": contractCalendar, "uid": "event"}, map[string]any{"dry_run": true, "etag": contractETag, "scope": "occurrence", "recurrence_id": "2026-07-02T10:00:00Z"}},
		"search_events":   {when, map[string]any{"calendar": contractCalendar, "calendars": contractCalendar, "uid": "event", "query": "Synthetic", "limit": 1, "offset": 1, "all_day": false, "status": "CONFIRMED", "busy_only": true, "include_cancelled": true, "compact": true, "expand_recurrence": false, "cursor": "start"}},
		"find_free_slots": {map[string]any{"start": when["start"], "end": when["end"], "duration_minutes": 60}, map[string]any{"calendar": contractCalendar, "calendars": contractCalendar, "timezone": "Europe/Paris", "working_hours_start": "09:00", "working_hours_end": "17:00", "days_of_week": "wed,thu", "buffer_before_minutes": 15, "buffer_after_minutes": 15, "include_all_day_busy": false, "limit": 2}},
		"get_contact":     {book, nil}, "create_contact": {map[string]any{"address_book": "$book", "display_name": "New synthetic"}, contact},
		"update_contact":    {map[string]any{"address_book": "$book", "uid": "contact", "display_name": "Changed"}, contractMerge(contractOmit(contact, "client_uid"), map[string]any{"etag": contractETag, "idempotency_key": "update-key"})},
		"delete_contact":    {book, map[string]any{"etag": contractETag, "dry_run": true}},
		"search_contacts":   {map[string]any{}, map[string]any{"address_book": "$book", "query": "Synthetic", "email": "reply", "phone": "1234", "include_groups": true, "limit": 1}},
		"get_message":       {identity(), map[string]any{"max_body_bytes": 4, "allow_html_fallback": true}},
		"search_messages":   {map[string]any{"mailbox": "INBOX"}, map[string]any{"query": "Synthetic", "from": "reply", "to": "fixture", "subject": "Synthetic", "since": "2026-07-01", "before": "2026-07-03", "unseen_only": true, "flagged_only": true, "before_uid": 3, "uid_validity": 9, "limit": 1}},
		"set_message_flags": {contractMerge(identity(), map[string]any{"operation": "add", "flags": []any{"seen"}}), map[string]any{"expected_modseq": 42}},
		"move_message":      {contractMerge(identity(), map[string]any{"destination": "Archive"}), nil}, "trash_message": {identity(), nil},
		"send_message":  {map[string]any{"to": []any{"recipient@example.invalid"}, "body": "Synthetic outgoing body"}, map[string]any{"cc": []any{"copy@example.invalid"}, "bcc": []any{"reply@example.invalid"}, "subject": "Synthetic subject", "idempotency_key": "send-key", "archive_sent": true}},
		"reply_message": {contractMerge(identity(), map[string]any{"body": "Synthetic reply body"}), map[string]any{"mode": "reply_all", "idempotency_key": "reply-key"}},
	}
}

func contractClone(m map[string]any) map[string]any {
	var out map[string]any
	data, _ := json.Marshal(m)
	_ = json.Unmarshal(data, &out)
	return out
}
func contractMerge(a, b map[string]any) map[string]any {
	out := contractClone(a)
	for k, v := range b {
		out[k] = v
	}
	return out
}
func contractOmit(a map[string]any, keys ...string) map[string]any {
	out := contractClone(a)
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

func (f *contractRuntime) arguments(name, property string) map[string]any {
	spec := contractBusinessFixtures()[name]
	args := contractClone(spec.base)
	if property != "" {
		if value, ok := spec.values[property]; ok {
			args[property] = value
		}
	}
	if property == "scope" || property == "recurrence_id" {
		args["scope"] = "occurrence"
		args["recurrence_id"] = "2026-07-02T10:00:00Z"
	}
	if (name == "create_event" || name == "validate_event") && strings.HasPrefix(property, "recurrence_") {
		args["recurrence_frequency"] = "daily"
	}
	if property == "before_uid" {
		args["uid_validity"] = 9
	}
	if property == "all_day" && args["all_day"] == true {
		args["start"] = "2026-07-01"
		args["end"] = "2026-07-02"
	}
	if name == "search_events" && property == "cursor" {
		args["calendar"] = contractCalendar
		args["offset"] = 0
	}
	for key, value := range args {
		if text, ok := value.(string); ok && text == "$book" {
			args[key] = f.book
		}
	}
	return args
}

func TestContractRealDispatchAllTools(t *testing.T) {
	manifest, _ := contractManifestClient(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
	listed := contractListExact(t, manifest)
	specs := contractBusinessFixtures()
	for _, tool := range listed.Tools {
		t.Run(tool.Name, func(t *testing.T) {
			if _, ok := specs[tool.Name]; !ok {
				t.Fatal("registered tool lacks business-valid fixture")
			}
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
			args := f.arguments(tool.Name, "")
			res := f.call(t, tool.Name, args)
			if res.IsError {
				t.Fatalf("real valid dispatch failed: %s", resultText(t, res))
			}
			contractValidateOutput(t, tool, res)
			f.verify(t, tool.Name, args, res)
		})
	}
}

func (f *contractRuntime) verify(t *testing.T, name string, args map[string]any, res *mcp.CallToolResult) {
	t.Helper()
	f.mail.mu.Lock()
	defer f.mail.mu.Unlock()
	if len(f.mail.errors) != 0 {
		t.Fatalf("synthetic backend protocol error: %v", f.mail.errors)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatal(err)
	}
	switch name {
	case "validate_event":
		if payload["ok"] != true || payload["normalized"] == nil {
			t.Fatalf("not business-valid: %v", payload)
		}
	case "create_event", "update_event":
		if payload["success"] != true || f.calendar.mutations != 1 {
			t.Fatalf("no actual Calendar write: %v", payload)
		}
	case "delete_event":
		want := 1
		if args["dry_run"] == true {
			want = 0
		}
		if payload["success"] != true || f.calendar.mutations != want {
			t.Fatalf("Calendar delete effect: %v", payload)
		}
	case "create_contact", "update_contact":
		if payload["uid"] == nil || f.contacts.mutations != 1 {
			t.Fatalf("no actual Contacts write: %v", payload)
		}
	case "delete_contact":
		want := 1
		if args["dry_run"] == true {
			want = 0
		}
		if f.contacts.mutations != want {
			t.Fatalf("Contacts delete effect: %v", payload)
		}
	case "get_event":
		if payload["uid"] != "event" || payload["title"] != "Synthetic event" {
			t.Fatalf("actual event lost: %v", payload)
		}
	case "get_contact":
		if payload["uid"] != "contact" || payload["displayName"] != "Synthetic Person" {
			t.Fatalf("actual contact lost: %v", payload)
		}
	case "get_message":
		if payload["messageId"] != "<source@example.invalid>" {
			t.Fatalf("actual message identity lost: %v", payload)
		}
	case "set_message_flags":
		if f.mail.stores != 1 {
			t.Fatal("no actual delta STORE")
		}
	case "move_message", "trash_message":
		if f.mail.moves != 1 || payload["method"] != "move" {
			t.Fatalf("no actual UID MOVE: %v", payload)
		}
	case "send_message", "reply_message":
		if payload["status"] != "accepted" || f.mail.data != 1 || f.mail.tls != 1 {
			t.Fatalf("no verified-STARTTLS accepted DATA: %v", payload)
		}
	case "list_address_books":
		if payload["count"] != float64(1) {
			t.Fatalf("actual discovered books lost: %v", payload)
		}
	case "list_calendars", "list_mailboxes", "search_events", "search_contacts", "search_messages", "find_free_slots":
		if len(f.calendar.requests) == 0 && len(f.contacts.requests) <= 3 && f.mail.imapDials == 0 {
			t.Fatal("domain dispatch did not consult the real service backend")
		}
	}
}
