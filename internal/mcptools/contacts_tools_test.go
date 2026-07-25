package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const contactsTestBook = "book-AAAAAAAAAAAAAAAAAAAAAA"

type fakeContactsService struct {
	mu sync.Mutex

	books        []contacts.AddressBook
	searchResult contacts.SearchResult
	contact      *contacts.Contact
	createResult contacts.CreateResult
	updateResult contacts.UpdateResult
	deleteResult contacts.DeleteResult

	listErr   error
	searchErr error
	getErr    error
	createErr error
	updateErr error
	deleteErr error

	listCalls   int
	searchCalls int
	getCalls    int
	createCalls int
	updateCalls int
	deleteCalls int

	lastSearch contacts.SearchOptions
	lastBook   string
	lastUID    string
	lastCreate *contacts.CreateContactInput
	lastUpdate *contacts.UpdateContactInput
	lastDelete *contacts.DeleteContactInput
}

func (f *fakeContactsService) ListAddressBooks(context.Context) ([]contacts.AddressBook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return append([]contacts.AddressBook(nil), f.books...), f.listErr
}

func (f *fakeContactsService) SearchContacts(_ context.Context, opts contacts.SearchOptions) (contacts.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls++
	f.lastSearch = opts
	result := f.searchResult
	result.Contacts = append([]contacts.ContactSummary(nil), f.searchResult.Contacts...)
	return result, f.searchErr
}

func (f *fakeContactsService) GetContact(_ context.Context, addressBook, uid string) (*contacts.Contact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	f.lastBook = addressBook
	f.lastUID = uid
	if f.contact == nil {
		return nil, f.getErr
	}
	copy := *f.contact
	return &copy, f.getErr
}

func (f *fakeContactsService) CreateContact(_ context.Context, input *contacts.CreateContactInput) (contacts.CreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	copy := *input
	copy.Emails = append([]contacts.TypedValue(nil), input.Emails...)
	copy.Phones = append([]contacts.TypedValue(nil), input.Phones...)
	copy.Addresses = append([]contacts.PostalAddress(nil), input.Addresses...)
	copy.URLs = append([]contacts.TypedValue(nil), input.URLs...)
	f.lastCreate = &copy
	return f.createResult, f.createErr
}

func (f *fakeContactsService) UpdateContact(_ context.Context, input *contacts.UpdateContactInput) (contacts.UpdateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	copy := *input
	f.lastUpdate = &copy
	return f.updateResult, f.updateErr
}

func (f *fakeContactsService) DeleteContact(_ context.Context, input *contacts.DeleteContactInput) (contacts.DeleteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	copy := *input
	f.lastDelete = &copy
	return f.deleteResult, f.deleteErr
}

func (f *fakeContactsService) snapshot() fakeContactsService {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeContactsService{
		listCalls: f.listCalls, searchCalls: f.searchCalls, getCalls: f.getCalls,
		createCalls: f.createCalls, updateCalls: f.updateCalls, deleteCalls: f.deleteCalls,
		lastSearch: f.lastSearch, lastBook: f.lastBook, lastUID: f.lastUID,
		lastCreate: f.lastCreate, lastUpdate: f.lastUpdate, lastDelete: f.lastDelete,
	}
}

func contactsTestDeps(service contacts.Service, auditBuffer *bytes.Buffer, secrets ...string) ContactsDeps {
	redactor := security.NewRedactor(secrets...)
	deps := ContactsDeps{Service: service, Redactor: redactor}
	if auditBuffer != nil {
		deps.Audit = security.NewAuditLogger(auditBuffer)
	}
	return deps
}

func contactsRequest(arguments any) mcp.CallToolRequest {
	request := mcp.CallToolRequest{}
	request.Params.Arguments = arguments
	return request
}

func contactsErrorPayloadFromResult(t *testing.T, result *mcp.CallToolResult) toolErrorPayload {
	t.Helper()
	if result == nil || !result.IsError {
		t.Fatalf("result = %#v, want tool error", result)
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(resultText(t, result)), &payload); err != nil {
		t.Fatalf("error result is not structured JSON: %v", err)
	}
	return payload
}

func TestContactsRegisterCountsSchemasAndAnnotations(t *testing.T) {
	readNames := []string{"list_address_books", "search_contacts", "get_contact"}
	writeNames := []string{"create_contact", "update_contact", "delete_contact"}
	for _, test := range []struct {
		name        string
		allowWrites bool
		wantCount   int
	}{
		{name: "read only", allowWrites: false, wantCount: 3},
		{name: "read write", allowWrites: true, wantCount: 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			mcpServer := server.NewMCPServer("contacts-test", "0", server.WithToolCapabilities(false))
			names := RegisterContacts(mcpServer, contactsTestDeps(&fakeContactsService{}, nil), test.allowWrites)
			tools := mcpServer.ListTools()
			if len(names) != test.wantCount || len(tools) != test.wantCount {
				t.Fatalf("registered names/tools = %d/%d, want %d", len(names), len(tools), test.wantCount)
			}
			for _, name := range readNames {
				if tools[name] == nil {
					t.Errorf("missing read tool %q", name)
				}
			}
			for _, name := range writeNames {
				if got := tools[name] != nil; got != test.allowWrites {
					t.Errorf("tool %q present = %v, want %v", name, got, test.allowWrites)
				}
			}
			if tools["validate_contact"] != nil {
				t.Fatal("validate_contact must not be registered")
			}
		})
	}

	tools := map[string]mcp.Tool{
		"list_address_books": newListAddressBooksTool(),
		"search_contacts":    newSearchContactsTool(),
		"get_contact":        newGetContactTool(),
		"create_contact":     newCreateContactTool(),
		"update_contact":     newUpdateContactTool(),
		"delete_contact":     newDeleteContactTool(),
	}
	wantRequired := map[string][]string{
		"list_address_books": {},
		"search_contacts":    {},
		"get_contact":        {"address_book", "uid"},
		"create_contact":     {"address_book"},
		"update_contact":     {"address_book", "uid"},
		"delete_contact":     {"address_book", "uid"},
	}
	wantIdempotent := map[string]bool{
		"list_address_books": true,
		"search_contacts":    true,
		"get_contact":        true,
		"create_contact":     false,
		"update_contact":     true,
		"delete_contact":     true,
	}
	for name, tool := range tools {
		if !strings.Contains(strings.ToLower(tool.Description), "untrusted") {
			t.Errorf("%s description does not warn about untrusted content", name)
		}
		if tool.InputSchema.AdditionalProperties != false {
			t.Errorf("%s additionalProperties = %#v, want false", name, tool.InputSchema.AdditionalProperties)
		}
		if strings.Join(tool.InputSchema.Required, ",") != strings.Join(wantRequired[name], ",") {
			t.Errorf("%s required = %v, want %v", name, tool.InputSchema.Required, wantRequired[name])
		}
		readOnly := strings.HasPrefix(name, "list_") || strings.HasPrefix(name, "search_") || strings.HasPrefix(name, "get_")
		if tool.Annotations.ReadOnlyHint == nil || *tool.Annotations.ReadOnlyHint != readOnly {
			t.Errorf("%s readOnlyHint is incorrect", name)
		}
		wantDestructive := name == "delete_contact"
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != wantDestructive {
			t.Errorf("%s destructiveHint is incorrect", name)
		}
		if tool.Annotations.IdempotentHint == nil || *tool.Annotations.IdempotentHint != wantIdempotent[name] {
			t.Errorf("%s idempotentHint is incorrect", name)
		}
	}

	limit := tools["search_contacts"].InputSchema.Properties["limit"].(map[string]any)
	if limit["type"] != "integer" || limit["default"] != contactsDefaultSearchLimit || limit["maximum"] != contactsMaxSearchLimit {
		t.Errorf("search limit schema = %#v", limit)
	}
	emails := tools["create_contact"].InputSchema.Properties["emails"].(map[string]any)
	if emails["maxItems"] != contactsMaxEmails {
		t.Errorf("create emails maxItems = %#v", emails["maxItems"])
	}
	updateProperties := tools["update_contact"].InputSchema.Properties
	if _, exists := updateProperties["patch"]; exists {
		t.Fatal("update_contact still exposes nested patch")
	}
	for _, field := range []string{"display_name", "name", "organization", "title", "nickname", "birthday", "notes", "emails", "phones", "addresses", "urls"} {
		if _, exists := updateProperties[field]; !exists {
			t.Errorf("update_contact missing top-level editable field %q", field)
		}
	}
	name := updateProperties["name"].(map[string]any)
	if name["additionalProperties"] != false {
		t.Errorf("update name additionalProperties = %#v, want false", name["additionalProperties"])
	}
}

func TestContactsHandlersParseStrictParameters(t *testing.T) {
	fake := &fakeContactsService{searchResult: contacts.SearchResult{}}
	deps := contactsTestDeps(fake, nil)
	result, err := searchContactsHandler(deps)(context.Background(), contactsRequest(map[string]any{
		"address_book":   contactsTestBook,
		"query":          "Ada",
		"email":          "example.test",
		"phone":          "+33 1",
		"include_groups": true,
		"limit":          17,
	}))
	if err != nil || result.IsError {
		t.Fatalf("valid search returned err=%v result=%s", err, resultText(t, result))
	}
	snapshot := fake.snapshot()
	if snapshot.searchCalls != 1 || snapshot.lastSearch.AddressBook != contactsTestBook ||
		snapshot.lastSearch.Query != "Ada" || snapshot.lastSearch.Email != "example.test" ||
		snapshot.lastSearch.Phone != "+33 1" || !snapshot.lastSearch.IncludeGroups || snapshot.lastSearch.Limit != 17 {
		t.Fatalf("parsed search options = %#v, calls=%d", snapshot.lastSearch, snapshot.searchCalls)
	}

	invalid := []map[string]any{
		{"limit": "17"},
		{"limit": 1.5},
		{"include_groups": "true"},
		{"query": 123},
		{"unknown": true},
	}
	for _, arguments := range invalid {
		result, callErr := searchContactsHandler(deps)(context.Background(), contactsRequest(arguments))
		if callErr != nil {
			t.Fatalf("invalid arguments %v returned protocol error: %v", arguments, callErr)
		}
		payload := contactsErrorPayloadFromResult(t, result)
		if payload.Code != string(contacts.CodeValidation) {
			t.Errorf("arguments %v code = %q", arguments, payload.Code)
		}
	}
	if got := fake.snapshot().searchCalls; got != 1 {
		t.Fatalf("invalid searches reached service, calls = %d", got)
	}

	result, err = getContactHandler(deps)(context.Background(), contactsRequest(map[string]any{
		"address_book": contactsTestBook,
		"uid":          []string{"not", "a", "string"},
	}))
	if err != nil || !result.IsError || fake.snapshot().getCalls != 0 {
		t.Fatalf("strict get parsing failed: err=%v result=%#v calls=%d", err, result, fake.snapshot().getCalls)
	}
}

func TestCreateContactParsesStructuredFields(t *testing.T) {
	fake := &fakeContactsService{createResult: contacts.CreateResult{AddressBook: contactsTestBook, UID: "uid-created"}}
	deps := contactsTestDeps(fake, nil)
	result, err := createContactHandler(deps)(context.Background(), contactsRequest(map[string]any{
		"address_book": contactsTestBook,
		"name": map[string]any{
			"givenName":  "Ada",
			"familyName": "Lovelace",
		},
		"organization": "Analytical Engines",
		"birthday":     "1815-12-10",
		"emails":       []any{map[string]any{"type": "work", "value": "ada@example.test"}},
		"phones":       []any{map[string]any{"type": "cell", "value": "+44 123"}},
		"addresses": []any{map[string]any{
			"type": "home", "streetAddress": "1 Computing Way", "locality": "London",
		}},
		"urls":       []any{map[string]any{"value": "https://example.test/ada"}},
		"client_uid": "client-created",
	}))
	if err != nil || result.IsError {
		t.Fatalf("create returned err=%v result=%s", err, resultText(t, result))
	}
	input := fake.snapshot().lastCreate
	if input == nil || input.Name.GivenName != "Ada" || input.Name.FamilyName != "Lovelace" ||
		len(input.Emails) != 1 || input.Emails[0].Type != "work" ||
		len(input.Addresses) != 1 || input.Addresses[0].StreetAddress != "1 Computing Way" ||
		input.ClientUID != "client-created" {
		t.Fatalf("parsed create input = %#v", input)
	}
}

func TestUpdateContactPatchPreservesOmittedAndExplicitEmpty(t *testing.T) {
	fake := &fakeContactsService{updateResult: contacts.UpdateResult{AddressBook: contactsTestBook, UID: "uid-1"}}
	deps := contactsTestDeps(fake, nil)
	result, err := updateContactHandler(deps)(context.Background(), contactsRequest(map[string]any{
		"address_book": contactsTestBook,
		"uid":          "uid-1",
		"etag":         `"etag-1"`,
		"title":        "",
		"name":         map[string]any{},
		"emails":       []any{},
	}))
	if err != nil || result.IsError {
		t.Fatalf("update returned err=%v result=%s", err, resultText(t, result))
	}
	input := fake.snapshot().lastUpdate
	if input == nil {
		t.Fatal("service did not receive update")
	}
	patch := input.Patch
	if patch.Title == nil || *patch.Title != "" {
		t.Fatalf("explicit empty title was not preserved: %#v", patch.Title)
	}
	if patch.Emails == nil || len(*patch.Emails) != 0 {
		t.Fatalf("explicit empty emails were not preserved: %#v", patch.Emails)
	}
	if patch.Name == nil || !contactsNameEmpty(*patch.Name) {
		t.Fatalf("explicit empty name was not preserved: %#v", patch.Name)
	}
	if patch.DisplayName != nil || patch.Organization != nil || patch.Phones != nil || patch.Addresses != nil || patch.URLs != nil {
		t.Fatalf("omitted patch fields became present: %#v", patch)
	}

	result, err = updateContactHandler(deps)(context.Background(), contactsRequest(map[string]any{
		"address_book": contactsTestBook,
		"uid":          "uid-1",
		"emails":       nil,
	}))
	if err != nil {
		t.Fatal(err)
	}
	payload := contactsErrorPayloadFromResult(t, result)
	if payload.Code != string(contacts.CodeValidation) || fake.snapshot().updateCalls != 1 {
		t.Fatalf("null editable value was not rejected before service: payload=%#v calls=%d", payload, fake.snapshot().updateCalls)
	}

	result, err = updateContactHandler(deps)(context.Background(), contactsRequest(map[string]any{
		"address_book": contactsTestBook,
		"uid":          "uid-1",
		"etag":         `"etag-1"`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	payload = contactsErrorPayloadFromResult(t, result)
	if payload.Code != string(contacts.CodeValidation) || fake.snapshot().updateCalls != 1 {
		t.Fatalf("update without editable field reached service: payload=%#v calls=%d", payload, fake.snapshot().updateCalls)
	}
}

func TestContactsHandlersRejectMalformedETagsBeforeService(t *testing.T) {
	fake := &fakeContactsService{}
	deps := contactsTestDeps(fake, nil)
	invalid := []string{
		" ",
		"*",
		"bare",
		`W/"weak"`,
		`"one", "two"`,
		`"unterminated`,
		`"embedded"quote"`,
		"\"space inside\"",
		strings.Repeat("x", 513),
	}
	for _, etag := range invalid {
		result, err := updateContactHandler(deps)(context.Background(), contactsRequest(map[string]any{
			"address_book": contactsTestBook,
			"uid":          "uid-1",
			"etag":         etag,
			"title":        "changed",
		}))
		if err != nil {
			t.Fatalf("ETag %q returned protocol error: %v", etag, err)
		}
		payload := contactsErrorPayloadFromResult(t, result)
		if payload.Code != string(contacts.CodeValidation) {
			t.Errorf("ETag %q code = %q", etag, payload.Code)
		}
	}
	if calls := fake.snapshot().updateCalls; calls != 0 {
		t.Fatalf("malformed ETags reached service %d times", calls)
	}
}

func TestContactsErrorCodeMappingAndNoRawCause(t *testing.T) {
	codes := []contacts.Code{
		contacts.CodeValidation,
		contacts.CodeAuthentication,
		contacts.CodeAuthorization,
		contacts.CodeNotFound,
		contacts.CodeConflict,
		contacts.CodeConcurrentModification,
		contacts.CodeRateLimited,
		contacts.CodeTimeout,
		contacts.CodeUnavailable,
		contacts.CodePartialFailure,
		contacts.CodeProtocolError,
		contacts.CodePayloadTooLarge,
		contacts.CodeOutcomeUnknown,
		contacts.CodeInternalError,
	}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			result := contactsErrorResult(security.NewRedactor(), "contacts test", &contacts.Error{
				Code: code, Message: "sanitized failure", Retryable: code == contacts.CodeRateLimited,
				RetryAfter: 90 * time.Second,
			})
			payload := contactsErrorPayloadFromResult(t, result)
			if payload.Code != string(code) {
				t.Fatalf("code = %q, want %q", payload.Code, code)
			}
			if code == contacts.CodeRateLimited && (!payload.Retryable || payload.RetryAfter != 60) {
				t.Fatalf("retry metadata = %#v", payload)
			}
		})
	}

	const rawCause = "RAW-CONTACTS-CAUSE-SENTINEL"
	result := contactsErrorResult(security.NewRedactor(), "getting contact", fmt.Errorf("wrapped: %s", rawCause))
	payload := contactsErrorPayloadFromResult(t, result)
	if payload.Code != string(contacts.CodeInternalError) || strings.Contains(resultText(t, result), rawCause) {
		t.Fatalf("untyped error leaked raw cause: %#v", payload)
	}
}

func TestContactsOutcomeUnknownIncludesBoundedRedactedReconciliation(t *testing.T) {
	const secret = "CONTACTS-RECONCILIATION-SECRET"
	result := contactsErrorResult(security.NewRedactor(secret), "updating contact", &contacts.Error{
		Code:           contacts.CodeOutcomeUnknown,
		Message:        "mutation outcome is unknown",
		Reconciliation: secret + strings.Repeat("\xc3\xa9", maxErrorDetailBytes),
	})
	payload := contactsErrorPayloadFromResult(t, result)
	if payload.Code != string(contacts.CodeOutcomeUnknown) || payload.Reconciliation == "" {
		t.Fatalf("outcome_unknown payload = %#v", payload)
	}
	if strings.Contains(payload.Reconciliation, secret) || len(payload.Reconciliation) > maxErrorDetailBytes {
		t.Fatalf("reconciliation was not redacted and bounded: %q", payload.Reconciliation)
	}
}

func TestContactsResultsAreRedacted(t *testing.T) {
	const secret = "CONTACTS-SECRET-SENTINEL"
	fake := &fakeContactsService{contact: &contacts.Contact{
		ContactSummary: contacts.ContactSummary{AddressBook: contactsTestBook, UID: "uid-1", DisplayName: "Name " + secret},
		Version:        "3.0",
		Notes:          "notes " + secret,
	}}
	deps := contactsTestDeps(fake, nil, secret)
	result, err := getContactHandler(deps)(context.Background(), contactsRequest(map[string]any{
		"address_book": contactsTestBook,
		"uid":          "uid-1",
	}))
	if err != nil || result.IsError {
		t.Fatalf("get returned err=%v result=%#v", err, result)
	}
	text := resultText(t, result)
	if strings.Contains(text, secret) || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("success result was not redacted: %s", text)
	}

	typed := &contacts.Error{Code: contacts.CodeUnavailable, Message: "remote said " + secret}
	errorResult := contactsErrorResult(deps.Redactor, "getting contact", typed)
	if strings.Contains(resultText(t, errorResult), secret) {
		t.Fatal("typed error message leaked configured secret")
	}
}

func TestContactsMutationAuditStatusesContainNoPII(t *testing.T) {
	const (
		uidPII   = "private-contact-uid"
		namePII  = "Private Person"
		emailPII = "private.person@example.test"
	)
	var audit bytes.Buffer
	fake := &fakeContactsService{
		updateErr:    &contacts.Error{Code: contacts.CodeOutcomeUnknown, Message: "mutation may have applied"},
		deleteResult: contacts.DeleteResult{AddressBook: contactsTestBook, UID: uidPII, DryRun: true, WouldDelete: true},
	}
	deps := contactsTestDeps(fake, &audit)

	_, _ = createContactHandler(deps)(context.Background(), contactsRequest(map[string]any{
		"address_book": contactsTestBook,
		"client_uid":   uidPII,
		"emails":       []any{map[string]any{"value": emailPII}},
	}))
	_, _ = updateContactHandler(deps)(context.Background(), contactsRequest(map[string]any{
		"address_book": contactsTestBook,
		"uid":          uidPII,
		"display_name": namePII,
	}))
	_, _ = deleteContactHandler(deps)(context.Background(), contactsRequest(map[string]any{
		"address_book": contactsTestBook,
		"uid":          uidPII,
		"dry_run":      true,
	}))

	output := audit.String()
	for _, pii := range []string{uidPII, namePII, emailPII, contactsTestBook} {
		if strings.Contains(output, pii) {
			t.Fatalf("audit contains PII %q: %s", pii, output)
		}
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 3 {
		t.Fatalf("audit lines = %d, want 3: %s", len(lines), output)
	}
	wantStatus := []string{"denied", "outcome_unknown", "dry_run"}
	for index, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("audit line is not JSON: %v", err)
		}
		if record["domain"] != "contacts" || record["resourceType"] != "contact" || record["status"] != wantStatus[index] {
			t.Errorf("audit record %d = %#v", index, record)
		}
		if token, ok := record["resourceToken"].(string); !ok || token == "" {
			t.Errorf("audit record has no opaque resource token: %#v", record)
		}
	}
}

func TestContactsReadHandlersAreConcurrentSafe(t *testing.T) {
	fake := &fakeContactsService{
		searchResult: contacts.SearchResult{Contacts: []contacts.ContactSummary{}},
		contact: &contacts.Contact{
			ContactSummary: contacts.ContactSummary{AddressBook: contactsTestBook, UID: "uid-1", DisplayName: "Ada"},
			Version:        "3.0",
		},
	}
	deps := contactsTestDeps(fake, nil)
	var wait sync.WaitGroup
	errorsChannel := make(chan error, 64)
	for index := range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var result *mcp.CallToolResult
			var err error
			if index%2 == 0 {
				result, err = searchContactsHandler(deps)(context.Background(), contactsRequest(map[string]any{"limit": 10}))
			} else {
				result, err = getContactHandler(deps)(context.Background(), contactsRequest(map[string]any{"address_book": contactsTestBook, "uid": "uid-1"}))
			}
			if err != nil {
				errorsChannel <- err
			} else if result == nil || result.IsError {
				errorsChannel <- errors.New("unexpected tool error")
			}
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Error(err)
	}
	snapshot := fake.snapshot()
	if snapshot.searchCalls != 32 || snapshot.getCalls != 32 {
		t.Fatalf("concurrent calls = search %d, get %d", snapshot.searchCalls, snapshot.getCalls)
	}
}
