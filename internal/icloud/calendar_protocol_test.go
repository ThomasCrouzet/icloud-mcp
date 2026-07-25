package icloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"golang.org/x/time/rate"
)

func TestProductionRetryWrapperNeverReplaysMutations(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		for _, status := range []int{
			http.StatusTooManyRequests,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		} {
			t.Run(method+"_"+http.StatusText(status), func(t *testing.T) {
				inner := &statusDoer{statuses: []int{status, http.StatusNoContent}}
				wrapped := NewRetryClassifier(inner)
				req, err := http.NewRequest(method, "https://caldav.icloud.com/home/cal/event.ics", strings.NewReader("event"))
				if err != nil {
					t.Fatal(err)
				}
				_, err = wrapped.Do(req)
				want := CodeOutcomeUnknown
				if status == http.StatusTooManyRequests {
					want = CodeRateLimited
				}
				requireICloudCode(t, err, want)
				if inner.calls != 1 {
					t.Fatalf("calls = %d, want 1", inner.calls)
				}
			})
		}
	}
}

func TestProductionRetryWrapperMutationTransportIsOutcomeUnknown(t *testing.T) {
	var calls int
	wrapped := NewRetryClassifier(calendarDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("transport failed after dispatch")
	}))
	req, err := http.NewRequest(http.MethodPut, "https://caldav.icloud.com/home/cal/event.ics", strings.NewReader("event"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = wrapped.Do(req)
	requireICloudCode(t, err, CodeOutcomeUnknown)
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestCalendarMutationNilResponseIsOutcomeUnknown(t *testing.T) {
	client := initializedSecurityClient(calendarDoerFunc(func(*http.Request) (*http.Response, error) {
		return nil, nil
	}))
	_, err := client.doCalendarRequest(context.Background(), http.MethodPut,
		"https://caldav.icloud.com/home/cal/event.ics", nil, []byte("event"), "/home/cal/")
	requireICloudCode(t, err, CodeOutcomeUnknown)
}

func TestProductionRetryWrapperLeavesDefinitiveMutationStatusToOperation(t *testing.T) {
	newCalendar := func() *ical.Calendar {
		start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		return buildEventCalendar("uid-1", &NewEvent{Title: "Event", StartTime: start, EndTime: start.Add(time.Hour)})
	}

	t.Run("create 412 is conflict", func(t *testing.T) {
		inner := &statusDoer{statuses: []int{http.StatusPreconditionFailed}}
		client := initializedSecurityClient(NewRetryClassifier(inner))
		err := client.putCalendarObjectIfMatch(context.Background(), "/home/cal/", "/home/cal/uid-1.ics", "", newCalendar(), "*")
		requireICloudCode(t, err, CodeConflict)
		if inner.calls != 1 {
			t.Fatalf("calls = %d, want 1", inner.calls)
		}
	})

	t.Run("update 412 is concurrent modification", func(t *testing.T) {
		inner := &statusDoer{statuses: []int{http.StatusPreconditionFailed}}
		client := initializedSecurityClient(NewRetryClassifier(inner))
		err := client.putCalendarObjectIfMatch(context.Background(), "/home/cal/", "/home/cal/uid-1.ics", `"v1"`, newCalendar())
		requireICloudCode(t, err, CodeConcurrentModification)
		if inner.calls != 1 {
			t.Fatalf("calls = %d, want 1", inner.calls)
		}
	})

	t.Run("delete 412 is concurrent modification", func(t *testing.T) {
		inner := &statusDoer{statuses: []int{http.StatusPreconditionFailed}}
		client := initializedSecurityClient(NewRetryClassifier(inner))
		err := client.deleteCalendarObjectIfMatch(context.Background(), "/home/cal/", "/home/cal/uid-1.ics", `"v1"`)
		requireICloudCode(t, err, CodeConcurrentModification)
		if inner.calls != 1 {
			t.Fatalf("calls = %d, want 1", inner.calls)
		}
	})

	t.Run("series delete 404 is intentional success", func(t *testing.T) {
		inner := &statusDoer{statuses: []int{http.StatusNotFound}}
		client := initializedSecurityClient(NewRetryClassifier(inner))
		if err := client.deleteCalendarObjectIfMatch(context.Background(), "/home/cal/", "/home/cal/uid-1.ics", `"v1"`); err != nil {
			t.Fatal(err)
		}
		if inner.calls != 1 {
			t.Fatalf("calls = %d, want 1", inner.calls)
		}
	})
}

func TestCreateOutcomeUnknownIncludesSafeReconciliation(t *testing.T) {
	client := initializedSecurityClient(NewRetryClassifier(calendarDoerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	})))
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	_, err := client.CreateEvent(context.Background(), "/home/cal/", &NewEvent{
		Title: "Event", StartTime: start, EndTime: start.Add(time.Hour),
	})
	typed := AsICloudError(err)
	if typed == nil || typed.Code != CodeOutcomeUnknown {
		t.Fatalf("error = %v, want outcome_unknown", err)
	}
	if typed.Details["uid"] == "" || typed.Details["reconciliation"] == "" {
		t.Fatalf("missing reconciliation details: %#v", typed.Details)
	}
}

func TestStrongETagParserRFCEdgeCases(t *testing.T) {
	valid := []string{
		`""`,
		`"simple"`,
		`"comma,inside"`,
		`"opaque\"`,
		"\"obs-\x80\"",
		" \t\"ows\"\t ",
	}
	for _, value := range valid {
		got, err := parseStrongETag(value)
		if err != nil {
			t.Errorf("valid %q: %v", value, err)
			continue
		}
		if strings.Contains(value, "opaque") && got != value {
			t.Errorf("opaque tag changed: got %q, want %q", got, value)
		}
	}

	invalid := []string{
		"",
		"*",
		"bare",
		`W/"weak"`,
		`"one", "two"`,
		`"unterminated`,
		`"embedded"quote"`,
		"\"space inside\"",
		"\"tab\tinside\"",
		"\"delete\x7finside\"",
		"\"line\ninside\"",
	}
	for _, value := range invalid {
		if _, err := parseStrongETag(value); err == nil {
			t.Errorf("invalid %q was accepted", value)
		}
	}
}

func TestETagWireRoundTripPreservesBackslash(t *testing.T) {
	const tag = `"opaque\tag\"`
	var ifMatch string
	doer := calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodGet:
			header := make(http.Header)
			header.Set("Content-Type", ical.MIMEType)
			header.Set("ETag", tag)
			return calendarTestResponse(req, http.StatusOK, header, strings.NewReader(icsSimpleEvent)), nil
		case http.MethodDelete:
			ifMatch = req.Header.Get("If-Match")
			return calendarTestResponse(req, http.StatusNoContent, nil, nil), nil
		default:
			return calendarTestResponse(req, http.StatusMethodNotAllowed, nil, nil), nil
		}
	})
	client := initializedSecurityClient(doer)
	obj, err := client.getCalendarObject(context.Background(), "/home/cal/", "/home/cal/event.ics")
	if err != nil {
		t.Fatal(err)
	}
	if obj.ETag != tag {
		t.Fatalf("GET etag = %q, want exact %q", obj.ETag, tag)
	}
	if err := client.deleteCalendarObjectIfMatch(context.Background(), "/home/cal/", "/home/cal/event.ics", obj.ETag); err != nil {
		t.Fatal(err)
	}
	if ifMatch != tag {
		t.Fatalf("If-Match = %q, want exact %q", ifMatch, tag)
	}
}

func TestGETAndREPORTRejectWeakETags(t *testing.T) {
	t.Run("GET", func(t *testing.T) {
		client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
			header := make(http.Header)
			header.Set("Content-Type", ical.MIMEType)
			header.Set("ETag", `W/"weak"`)
			return calendarTestResponse(req, http.StatusOK, header, strings.NewReader(icsSimpleEvent)), nil
		}))
		_, err := client.getCalendarObject(context.Background(), "/home/cal/", "/home/cal/event.ics")
		requireICloudCode(t, err, CodeProtocolError)
	})

	t.Run("REPORT", func(t *testing.T) {
		body := reportResponseXMLWithETag("event.ics", icsSimpleEvent, `W/"weak"`)
		client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
			return calendarTestResponse(req, http.StatusMultiStatus, nil, strings.NewReader(body)), nil
		}))
		_, err := client.reportCalendarQuery(context.Background(), "/home/cal/", "")
		requireICloudCode(t, err, CodeProtocolError)
	})
}

func reportResponseXMLWithETag(href, data, etag string) string {
	return `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		reportResponseFragmentWithETag(href, data, etag) + `</multistatus>`
}

func TestREPORTResolvesRelativeHrefAgainstFinalURLAndPreservesEscaping(t *testing.T) {
	var calls int
	body := reportResponseXMLWithETag("event%2Fpart.ics", icsSimpleEvent, `"report\tag\"`)
	client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			header := make(http.Header)
			header.Set("Location", "/home/cal/final/")
			return calendarTestResponse(req, http.StatusTemporaryRedirect, header, nil), nil
		}
		return calendarTestResponse(req, http.StatusMultiStatus, nil, strings.NewReader(body)), nil
	}))

	objects, err := client.reportCalendarQuery(context.Background(), "/home/cal/", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 {
		t.Fatalf("objects = %d, want 1", len(objects))
	}
	if objects[0].Path != "/home/cal/final/event%2Fpart.ics" {
		t.Fatalf("path = %q", objects[0].Path)
	}
	if objects[0].ETag != `"report\tag\"` {
		t.Fatalf("etag = %q", objects[0].ETag)
	}
}

func TestResolveDAVHrefRejectsAuthorityAndURLMetadata(t *testing.T) {
	client := initializedSecurityClient(calendarDoerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	}))
	base, err := url.Parse("https://caldav.icloud.com/home/cal/")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := client.resolveDAVHref(base, "event%2Fpart.ics", "/home/cal/")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.EscapedPath() != "/home/cal/event%2Fpart.ics" {
		t.Fatalf("escaped path = %q", resolved.EscapedPath())
	}
	for _, href := range []string{
		"https://evil.example/home/cal/event.ics",
		"https://user@caldav.icloud.com/home/cal/event.ics",
		"event.ics?download=1",
		"event.ics#fragment",
		"/home/other/event.ics",
	} {
		if _, err := client.resolveDAVHref(base, href, "/home/cal/"); err == nil {
			t.Errorf("unsafe href accepted: %q", href)
		}
	}
}

func TestDiscoveryResolvesHrefsAgainstFinalResponseURLs(t *testing.T) {
	client := NewClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.EscapedPath() {
		case "/entry/":
			header := make(http.Header)
			header.Set("Location", "/dav/root/")
			return calendarTestResponse(req, http.StatusTemporaryRedirect, header, nil), nil
		case "/dav/root/":
			return calendarTestResponse(req, http.StatusMultiStatus, nil, strings.NewReader(principalResponseXML("principal/"))), nil
		case "/dav/root/principal/":
			header := make(http.Header)
			header.Set("Location", "/final/principal/")
			return calendarTestResponse(req, http.StatusTemporaryRedirect, header, nil), nil
		case "/final/principal/":
			return calendarTestResponse(req, http.StatusMultiStatus, nil, strings.NewReader(homeSetResponseXML("../calendars/%48ome/"))), nil
		default:
			t.Fatalf("unexpected discovery URL: %s", req.URL.String())
			return nil, errors.New("unexpected URL")
		}
	}), "https://caldav.icloud.com/entry", func(host string) bool { return host == "caldav.icloud.com" })

	if err := client.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.shardBase != "https://caldav.icloud.com" {
		t.Fatalf("shardBase = %q", client.shardBase)
	}
	if client.homeSetPath != "/final/calendars/%48ome/" {
		t.Fatalf("homeSetPath = %q", client.homeSetPath)
	}
}

func TestDiscoveryWaiterCanCancel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	client := NewClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/" {
			once.Do(func() {
				close(entered)
				<-release
			})
			return calendarTestResponse(req, http.StatusMultiStatus, nil, strings.NewReader(principalResponseXML(testPrincipalPath))), nil
		}
		return calendarTestResponse(req, http.StatusMultiStatus, nil, strings.NewReader(homeSetResponseXML("https://caldav.icloud.com/home/"))), nil
	}), "https://caldav.icloud.com", func(host string) bool { return host == "caldav.icloud.com" })

	first := make(chan error, 1)
	go func() { first <- client.Discover(context.Background()) }()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := client.Discover(ctx)
	requireICloudCode(t, err, CodeTimeout)
	if time.Since(started) > 250*time.Millisecond {
		t.Fatalf("canceled discovery waiter blocked too long")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("in-flight discovery failed: %v", err)
	}
	if err := client.Discover(context.Background()); err != nil {
		t.Fatalf("published discovery result unavailable: %v", err)
	}
}

func TestCalendarObjectIdentityValidation(t *testing.T) {
	valid := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:uid-a\r\nDTSTART:20260701T090000Z\r\nRRULE:FREQ=DAILY;COUNT=2\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:uid-a\r\nRECURRENCE-ID:20260702T090000Z\r\nDTSTART:20260702T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	calendar, err := decodeRemoteCalendar(strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCalendarObjectIdentity(calendar, "uid-a"); err != nil {
		t.Fatalf("valid object: %v", err)
	}

	invalid := []string{
		strings.Replace(valid, "UID:uid-a\r\nRECURRENCE-ID", "UID:uid-b\r\nRECURRENCE-ID", 1),
		strings.Replace(valid, "RECURRENCE-ID:20260702T090000Z\r\n", "", 1),
		strings.Replace(valid, "UID:uid-a\r\nDTSTART", "DTSTART", 1),
	}
	for _, data := range invalid {
		calendar, err := decodeRemoteCalendar(strings.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		err = validateCalendarObjectIdentity(calendar, "uid-a")
		requireICloudCode(t, err, CodeProtocolError)
		if strings.Contains(err.Error(), "uid-a") || strings.Contains(err.Error(), "uid-b") {
			t.Fatalf("identity error leaked UID: %v", err)
		}
	}
	if err := validateCalendarObjectIdentity(calendar, "different-requested-uid"); err == nil {
		t.Fatal("requested UID mismatch was accepted")
	}
}

func TestGetEventRejectsRequestedUIDMismatch(t *testing.T) {
	mock := newMockCalDAV(t)
	const requested = "requested-uid"
	const remote = "remote-uid"
	data := strings.Replace(icsSimpleEvent, "UID:uid-simple-1", "UID:"+remote, 1)
	mock.objects[requested] = mockObject{path: testHomeCalendar + requested + ".ics", ics: data}
	_, err := mock.client().GetEvent(context.Background(), testHomeCalendar, requested)
	requireICloudCode(t, err, CodeProtocolError)
	if strings.Contains(err.Error(), requested) || strings.Contains(err.Error(), remote) {
		t.Fatalf("UID mismatch error was not sanitized: %v", err)
	}
}

func TestSearchWithoutExpansionKeepsSelectedRecurringMaster(t *testing.T) {
	mock := newMockCalDAV(t)
	recurring := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:old-recurring\r\nDTSTART:20200101T090000Z\r\nDTEND:20200101T100000Z\r\nRRULE:FREQ=YEARLY\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	oneOff := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:old-one-off\r\nDTSTART:20200101T090000Z\r\nDTEND:20200101T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	mock.objects["old-recurring"] = mockObject{path: testHomeCalendar + "old-recurring.ics", ics: recurring}
	mock.objects["old-one-off"] = mockObject{path: testHomeCalendar + "old-one-off.ics", ics: oneOff}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	result, err := mock.client().SearchEvents(context.Background(), testHomeCalendar, start, start.Add(24*time.Hour), &SearchOptions{ExpandRecurrence: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || result.Events[0].UID != "old-recurring" {
		t.Fatalf("events = %+v, want only selected recurring master", result.Events)
	}
}

func TestFloatingDateFormsSurviveScopedWrites(t *testing.T) {
	calendar := ical.NewCalendar()
	calendar.Props.SetText(ical.PropVersion, "2.0")
	calendar.Props.SetText(ical.PropProductID, "-//test//EN")
	master := ical.NewEvent()
	master.Props.SetText(ical.PropUID, "floating-series")
	startProp := ical.NewProp(ical.PropDateTimeStart)
	startProp.Value = "20260701T090000"
	master.Props.Set(startProp)
	endProp := ical.NewProp(ical.PropDateTimeEnd)
	endProp.Value = "20260701T100000"
	master.Props.Set(endProp)
	master.Props.SetDateTime(ical.PropDateTimeStamp, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	rrule := ical.NewProp(ical.PropRecurrenceRule)
	rrule.Value = "FREQ=WEEKLY;COUNT=3"
	master.Props.Set(rrule)
	calendar.Children = append(calendar.Children, master.Component)

	newStart := time.Date(2026, 7, 8, 11, 0, 0, 0, time.UTC)
	if err := applyOccurrenceUpdate(calendar, master, time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC), &EventUpdate{StartTime: &newStart}); err != nil {
		t.Fatal(err)
	}
	exdate, err := setRecurrenceInstantProp(ical.PropExceptionDates, master, time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	master.Props.Add(exdate)
	if err := setEventDateProp(master, ical.PropDateTimeStart, time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	var encoded strings.Builder
	if err := ical.NewEncoder(&encoded).Encode(calendar); err != nil {
		t.Fatal(err)
	}
	body := encoded.String()
	for _, prefix := range []string{"DTSTART:", "DTEND:", "RECURRENCE-ID:", "EXDATE:"} {
		for _, line := range strings.Split(body, "\r\n") {
			if strings.HasPrefix(line, prefix) && (strings.Contains(line, "TZID=") || strings.HasSuffix(line, "Z")) {
				t.Fatalf("floating %s changed form: %q\n%s", prefix, line, body)
			}
		}
	}
	if !strings.Contains(body, "RECURRENCE-ID:20260708T090000") || !strings.Contains(body, "EXDATE:20260715T090000") {
		t.Fatalf("floating recurrence forms missing:\n%s", body)
	}
}

func TestRateLimiterContextCancellationIsTimeout(t *testing.T) {
	limiter := rate.NewLimiter(rate.Every(time.Hour), 1)
	if !limiter.Allow() {
		t.Fatal("failed to consume initial token")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitLimiter(ctx, limiter, "read")
	requireICloudCode(t, err, CodeTimeout)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation cause", err)
	}
}

func TestStrongETagHeaderRejectsMultipleFields(t *testing.T) {
	header := make(http.Header)
	header.Add("ETag", `"one"`)
	header.Add("ETag", `"two"`)
	if _, err := strongETagFromHeader(header); err == nil {
		t.Fatal("multiple ETag fields were accepted")
	}
}

func TestReadRetryStillRewindsREPORTBody(t *testing.T) {
	var calls int
	var bodies []string
	wrapped := NewRetryClassifier(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		bodies = append(bodies, string(data))
		status := http.StatusTooManyRequests
		if calls == 2 {
			status = http.StatusMultiStatus
		}
		header := make(http.Header)
		header.Set("Retry-After", "0")
		return calendarTestResponse(req, status, header, nil), nil
	}))
	req, err := http.NewRequest("REPORT", "https://caldav.icloud.com/home/cal/", strings.NewReader("query"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := wrapped.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls != 2 || len(bodies) != 2 || bodies[0] != "query" || bodies[1] != "query" {
		t.Fatalf("calls=%d bodies=%q", calls, bodies)
	}
}
