package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const idempotencyProtocolSecret = "fixture-private-value"

// These protocol scenarios use a stateful service fixture, not a DAV transport.
// Each applied update changes the ETag, including repeated patches with identical text.
type idempotencyProtocolState struct {
	mu      sync.Mutex
	title   string
	calls   int
	writes  int
	mode    string
	started chan struct{}
	release <-chan struct{}
}

func (s *idempotencyProtocolState) apply(ctx context.Context, domain, title, etag string) error {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 && s.started != nil {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
			return idempotencyProtocolError(domain, "outcome_unknown")
		}
	}
	if call == 1 && s.mode == "rejected" {
		return idempotencyProtocolError(domain, "concurrent_modification")
	}
	s.mu.Lock()
	if etag != "" && etag != fmt.Sprintf(`"v%d"`, s.writes) {
		s.mu.Unlock()
		return idempotencyProtocolError(domain, "concurrent_modification")
	}
	s.title = title
	s.writes++
	s.mu.Unlock()
	switch s.mode {
	case "ambiguous":
		return idempotencyProtocolError(domain, "outcome_unknown")
	case "panic":
		panic(idempotencyProtocolSecret)
	case "unclassified":
		return fmt.Errorf("unexpected failure: %s", idempotencyProtocolSecret)
	}
	return nil
}

func idempotencyProtocolError(domain, code string) error {
	const reconciliation = "Read the resource before a new update."
	if domain == "calendar" {
		return fmt.Errorf("fixture: %w", &icloud.Error{
			Code: icloud.Code(code), Message: "fixture response " + idempotencyProtocolSecret,
			Details: map[string]string{"reconciliation": reconciliation},
		})
	}
	return fmt.Errorf("fixture: %w", &contacts.Error{
		Code: contacts.Code(code), Message: "fixture response " + idempotencyProtocolSecret,
		Reconciliation: reconciliation,
	})
}

func (s *idempotencyProtocolState) snapshot() (title, etag string, calls, writes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title, fmt.Sprintf(`"v%d"`, s.writes), s.calls, s.writes
}

type idempotencyProtocolCalendar struct {
	icloud.MockService
	state *idempotencyProtocolState
}

func (s *idempotencyProtocolCalendar) UpdateEvent(ctx context.Context, _, _ string, patch *icloud.EventUpdate) error {
	return s.state.apply(ctx, "calendar", *patch.Title, patch.IfMatchETag)
}

func (s *idempotencyProtocolCalendar) GetEvent(_ context.Context, _, uid string) (*icloud.EventDetail, error) {
	title, etag, _, _ := s.state.snapshot()
	return &icloud.EventDetail{Event: icloud.Event{
		UID: uid, Title: title, ETag: etag,
		StartTime: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
	}}, nil
}

type idempotencyProtocolContacts struct {
	fakeContactsService
	state *idempotencyProtocolState
}

func (s *idempotencyProtocolContacts) UpdateContact(ctx context.Context, input *contacts.UpdateContactInput) (contacts.UpdateResult, error) {
	err := s.state.apply(ctx, "contacts", *input.Patch.DisplayName, input.ETag)
	_, etag, _, _ := s.state.snapshot()
	result := contacts.UpdateResult{AddressBook: input.AddressBook, UID: input.UID, ETag: etag}
	if s.state.mode == "oversized" {
		result.Warning = strings.Repeat("x", contactsMaxResultBytes)
	}
	return result, err
}

func (s *idempotencyProtocolContacts) GetContact(_ context.Context, book, uid string) (*contacts.Contact, error) {
	title, etag, _, _ := s.state.snapshot()
	return &contacts.Contact{ContactSummary: contacts.ContactSummary{
		AddressBook: book, UID: uid, DisplayName: title, ETag: etag,
	}, Version: "3.0"}, nil
}

type idempotencyProtocolFixture struct {
	client *client.Client
	state  *idempotencyProtocolState
	domain string
	clock  atomic.Int64
}

func newIdempotencyProtocolFixture(t *testing.T, domain, mode string) *idempotencyProtocolFixture {
	t.Helper()
	f := &idempotencyProtocolFixture{domain: domain, state: &idempotencyProtocolState{title: "Before", mode: mode}}
	f.clock.Store(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC).UnixNano())
	previous := defaultIdempotency
	store := newIdempotencyStore()
	store.now = func() time.Time { return time.Unix(0, f.clock.Load()) }
	defaultIdempotency = store
	t.Cleanup(func() { defaultIdempotency = previous })
	red := security.NewRedactor(idempotencyProtocolSecret)
	s := server.NewMCPServer("idempotency-protocol", "test", server.WithToolCapabilities(false),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)))
	RegisterUnified(s, Deps{
		Service:         &idempotencyProtocolCalendar{state: f.state},
		ContactsService: &idempotencyProtocolContacts{state: f.state},
		Redactor:        red, Audit: security.NewAuditLogger(io.Discard), DefaultLocation: time.UTC,
	}, NewCapabilityPlan(false, true, false, false, false))
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	f.client = c
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := mcp.InitializeRequest{}
	request.Method = "initialize"
	request.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	request.Params.ClientInfo = mcp.Implementation{Name: "idempotency-protocol-client", Version: "test"}
	response, err := c.Initialize(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	logIdempotencyProtocol(t, "initialize", request, response)
	return f
}

func logIdempotencyProtocol(t *testing.T, exchange string, request, response any) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"exchange": exchange, "request": request, "response": response})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
}

func (f *idempotencyProtocolFixture) update(key, title string) mcp.CallToolRequest {
	request := mcp.CallToolRequest{}
	request.Method = "tools/call"
	args := map[string]any{"uid": "resource-1", "idempotency_key": key}
	request.Params.Name = "update_event"
	if f.domain == "contacts" {
		request.Params.Name = "update_contact"
		args["address_book"], args["display_name"] = contactsTestBook, title
	} else {
		args["calendar"], args["title"] = "/cal/home/", title
	}
	request.Params.Arguments = args
	return request
}

func (f *idempotencyProtocolFixture) call(t *testing.T, ctx context.Context, exchange string, request mcp.CallToolRequest) *mcp.CallToolResult {
	t.Helper()
	response, err := f.client.CallTool(ctx, request)
	if err != nil {
		t.Fatalf("%s: %v", exchange, err)
	}
	logIdempotencyProtocol(t, exchange, request, response)
	if response == nil || len(response.Content) != 1 {
		t.Fatalf("%s: expected one text result, got %+v", exchange, response)
	}
	text := resultText(t, response)
	if len(text) > maxIdempotencyPayload || strings.Contains(text, idempotencyProtocolSecret) || !json.Valid([]byte(text)) {
		t.Fatalf("%s: response lost its size, redaction, or JSON guard", exchange)
	}
	return response
}

func (f *idempotencyProtocolFixture) read(t *testing.T, title, etag string) {
	t.Helper()
	request := f.update("", "")
	args := request.GetArguments()
	delete(args, "idempotency_key")
	delete(args, "title")
	delete(args, "display_name")
	request.Params.Name = "get_event"
	field := "title"
	if f.domain == "contacts" {
		request.Params.Name, field = "get_contact", "displayName"
	}
	response := f.call(t, t.Context(), "reconcile", request)
	if response.IsError {
		t.Fatal("reconciliation read failed")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(resultText(t, response)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload[field] != title || payload["etag"] != etag {
		t.Fatalf("resource state = %v, want %q with ETag %q", payload, title, etag)
	}
}

func assertIdempotencyProtocolSame(t *testing.T, first, next *mcp.CallToolResult) {
	t.Helper()
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("cached result changed:\n%s\n%s", a, b)
	}
}

func assertIdempotencyProtocolError(t *testing.T, result *mcp.CallToolResult, code string) {
	t.Helper()
	payload := contactsErrorPayloadFromResult(t, result)
	if payload.Code != code {
		t.Fatalf("error code = %q, want %q", payload.Code, code)
	}
	if code == "outcome_unknown" && (payload.Retryable || payload.Reconciliation == "") {
		t.Fatalf("ambiguous outcome must require reconciliation: %+v", payload)
	}
}

func TestProtocolIdempotencySuccessAndConflict(t *testing.T) {
	for _, domain := range []string{"calendar", "contacts"} {
		t.Run(domain, func(t *testing.T) {
			f := newIdempotencyProtocolFixture(t, domain, "success")
			request := f.update("success-key", "After")
			first := f.call(t, t.Context(), "owner", request)
			if first.IsError {
				t.Fatal("owner failed")
			}
			assertIdempotencyProtocolSame(t, first, f.call(t, t.Context(), "same-key", request))
			assertIdempotencyProtocolError(t, f.call(t, t.Context(), "conflict", f.update("success-key", "Different")), "conflict")
			f.read(t, "After", `"v1"`)
			_, _, calls, writes := f.state.snapshot()
			if calls != 1 || writes != 1 {
				t.Fatalf("calls/writes = %d/%d, want 1/1", calls, writes)
			}
			f.clock.Add(int64(idempotencyTTL))
			if f.call(t, t.Context(), "expired-known-outcome", request).IsError {
				t.Fatal("known result did not expire")
			}
			f.read(t, "After", `"v2"`)
		})
	}
}

func TestProtocolIdempotencyAmbiguousDispatch(t *testing.T) {
	for _, domain := range []string{"calendar", "contacts"} {
		t.Run(domain, func(t *testing.T) {
			f := newIdempotencyProtocolFixture(t, domain, "ambiguous")
			request := f.update("unknown-key", "Applied")
			first := f.call(t, t.Context(), "lost-acknowledgement", request)
			assertIdempotencyProtocolError(t, first, "outcome_unknown")
			assertIdempotencyProtocolSame(t, first, f.call(t, t.Context(), "same-key", request))
			f.clock.Add(int64(24 * time.Hour))
			assertIdempotencyProtocolSame(t, first, f.call(t, t.Context(), "after-success-ttl", request))
			f.read(t, "Applied", `"v1"`)
			assertIdempotencyProtocolError(t, f.call(t, t.Context(), "conflict", f.update("unknown-key", "Different")), "conflict")
			_, _, calls, writes := f.state.snapshot()
			if calls != 1 || writes != 1 {
				t.Fatalf("ambiguous update replayed: calls/writes = %d/%d", calls, writes)
			}
			// A new key is an explicit new operation after the read, with its ETag.
			reconciled := f.update("reconciled-key", "Reconciled")
			reconciled.GetArguments()["etag"] = `"v1"`
			assertIdempotencyProtocolError(t, f.call(t, t.Context(), "new-key-after-read", reconciled), "outcome_unknown")
			f.read(t, "Reconciled", `"v2"`)
		})
	}
}

// Done signals when the duplicate is waiting. No wall-clock sleep orders the calls.
type idempotencyProtocolWaitContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *idempotencyProtocolWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func testIdempotencyProtocolWaiter(t *testing.T, advance time.Duration, cancelWaiter bool) {
	t.Helper()
	for _, domain := range []string{"calendar", "contacts"} {
		t.Run(domain, func(t *testing.T) {
			f := newIdempotencyProtocolFixture(t, domain, "success")
			release := make(chan struct{})
			f.state.started, f.state.release = make(chan struct{}), release
			var stop sync.Once
			unblock := func() { stop.Do(func() { close(release) }) }
			var wg sync.WaitGroup
			t.Cleanup(func() { unblock(); wg.Wait() })
			owner := make(chan *mcp.CallToolResult, 1)
			request := f.update("pending-key", "Owner")
			wg.Go(func() { owner <- f.call(t, t.Context(), "owner", request) })
			select {
			case <-f.state.started:
			case <-time.After(5 * time.Second):
				t.Fatal("owner did not reach the fixture")
			}
			f.clock.Add(int64(advance))
			assertIdempotencyProtocolError(t, f.call(t, t.Context(), "pending-conflict", f.update("pending-key", "Different")), "conflict")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waitCtx := &idempotencyProtocolWaitContext{Context: ctx, waiting: make(chan struct{})}
			waiter := make(chan *mcp.CallToolResult, 1)
			wg.Go(func() { waiter <- f.call(t, waitCtx, "waiter", request) })
			select {
			case <-waitCtx.waiting:
			case <-waiter:
				t.Fatal("duplicate dispatched instead of waiting for the owner")
			case <-time.After(5 * time.Second):
				t.Fatal("duplicate did not wait")
			}
			if cancelWaiter {
				cancel()
				select {
				case response := <-waiter:
					assertIdempotencyProtocolError(t, response, "timeout")
				case <-time.After(5 * time.Second):
					t.Fatal("cancelled waiter did not return")
				}
			}
			unblock()
			var first *mcp.CallToolResult
			select {
			case first = <-owner:
				if first.IsError {
					t.Fatal("waiter changed the owner's outcome")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("owner did not finish")
			}
			if !cancelWaiter {
				select {
				case response := <-waiter:
					assertIdempotencyProtocolSame(t, first, response)
				case <-time.After(5 * time.Second):
					t.Fatal("waiter did not receive the owner's outcome")
				}
			}
			assertIdempotencyProtocolSame(t, first, f.call(t, t.Context(), "same-key-after-owner", request))
			f.read(t, "Owner", `"v1"`)
			_, _, calls, writes := f.state.snapshot()
			if calls != 1 || writes != 1 {
				t.Fatalf("owner lost its claim: calls/writes = %d/%d", calls, writes)
			}
		})
	}
}

func TestProtocolIdempotencyCancelledWaiter(t *testing.T) {
	testIdempotencyProtocolWaiter(t, 0, true)
}

func TestProtocolIdempotencyPendingDoesNotExpire(t *testing.T) {
	testIdempotencyProtocolWaiter(t, idempotencyTTL+time.Second, true)
}

func TestProtocolIdempotencyConcurrentSuccess(t *testing.T) {
	testIdempotencyProtocolWaiter(t, 0, false)
}

func TestProtocolIdempotencyCancelledClaim(t *testing.T) {
	for _, domain := range []string{"calendar", "contacts"} {
		t.Run(domain, func(t *testing.T) {
			f := newIdempotencyProtocolFixture(t, domain, "success")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			request := f.update("cancelled-key", "After")
			assertIdempotencyProtocolError(t, f.call(t, ctx, "cancelled-before-claim", request), "timeout")
			f.read(t, "Before", `"v0"`)
			if f.call(t, t.Context(), "fresh-owner", request).IsError {
				t.Fatal("cancelled request reserved a key")
			}
			f.read(t, "After", `"v1"`)
		})
	}
}

func TestProtocolIdempotencyKnownFailure(t *testing.T) {
	for _, domain := range []string{"calendar", "contacts"} {
		t.Run(domain, func(t *testing.T) {
			f := newIdempotencyProtocolFixture(t, domain, "rejected")
			request := f.update("rejected-key", "After")
			first := f.call(t, t.Context(), "rejected-before-write", request)
			assertIdempotencyProtocolError(t, first, "concurrent_modification")
			assertIdempotencyProtocolSame(t, first, f.call(t, t.Context(), "cached-rejection", request))
			f.read(t, "Before", `"v0"`)
			f.clock.Add(int64(idempotencyTTL))
			if f.call(t, t.Context(), "expired-rejection", request).IsError {
				t.Fatal("definitive error did not expire")
			}
			f.read(t, "After", `"v1"`)
		})
	}
}

func TestProtocolIdempotencyPostDispatchFailures(t *testing.T) {
	for _, domain := range []string{"calendar", "contacts"} {
		for _, mode := range []string{"panic", "unclassified", "oversized"} {
			if domain == "calendar" && mode == "oversized" {
				continue
			}
			t.Run(domain+"/"+mode, func(t *testing.T) {
				f := newIdempotencyProtocolFixture(t, domain, mode)
				request := f.update("post-dispatch-key", "Applied")
				first := f.call(t, t.Context(), "post-dispatch-failure", request)
				if !first.IsError {
					t.Fatal("expected a guarded error result")
				}
				f.clock.Add(int64(24 * time.Hour))
				next := f.call(t, t.Context(), "retained-after-ttl", request)
				if mode == "panic" {
					assertIdempotencyProtocolError(t, next, "outcome_unknown")
				} else {
					assertIdempotencyProtocolSame(t, first, next)
				}
				if mode == "oversized" {
					assertIdempotencyProtocolError(t, next, "payload_too_large")
				}
				f.read(t, "Applied", `"v1"`)
				_, _, calls, writes := f.state.snapshot()
				if calls != 1 || writes != 1 {
					t.Fatalf("post-dispatch failure replayed: calls/writes = %d/%d", calls, writes)
				}
			})
		}
	}
}

func TestProtocolIdempotencyNamespaces(t *testing.T) {
	f := newIdempotencyProtocolFixture(t, "calendar", "success")
	if f.call(t, t.Context(), "calendar", f.update("shared-key", "Calendar")).IsError {
		t.Fatal("Calendar update failed")
	}
	f.domain = "contacts"
	response := f.call(t, t.Context(), "contacts", f.update("shared-key", "Contact"))
	if response.IsError || !strings.Contains(resultText(t, response), `"addressBook"`) {
		t.Fatal("Contacts key collided with Calendar")
	}
	f.read(t, "Contact", `"v2"`)
}

func TestProtocolIdempotencyCapacityRetainsUnknown(t *testing.T) {
	f := newIdempotencyProtocolFixture(t, "calendar", "ambiguous")
	var first *mcp.CallToolResult
	for i := 0; i < maxIdempotencyEntries; i++ {
		key := fmt.Sprintf("capacity-%04d", i)
		response := f.call(t, t.Context(), key, f.update(key, "Applied"))
		assertIdempotencyProtocolError(t, response, "outcome_unknown")
		if i == 0 {
			first = response
		}
	}
	f.clock.Add(int64(24 * time.Hour))
	denied := f.call(t, t.Context(), "capacity-denied", f.update("one-too-many", "Different"))
	assertIdempotencyProtocolError(t, denied, "conflict")
	if strings.Contains(resultText(t, denied), "without a key") {
		t.Fatal("capacity error advised bypassing the key")
	}
	assertIdempotencyProtocolSame(t, first, f.call(t, t.Context(), "retained-at-capacity", f.update("capacity-0000", "Applied")))
	f.read(t, "Applied", fmt.Sprintf(`"v%d"`, maxIdempotencyEntries))
	_, _, calls, writes := f.state.snapshot()
	if calls != maxIdempotencyEntries || writes != maxIdempotencyEntries {
		t.Fatalf("full cache dispatched another update: calls/writes = %d/%d", calls, writes)
	}
}
