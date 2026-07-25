package icloud

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	extcaldav "github.com/emersion/go-webdav/caldav"
)

type calendarDoerFunc func(*http.Request) (*http.Response, error)

func (f calendarDoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func initializedSecurityClient(doer httpDoer) *Client {
	return &Client{
		http:        doer,
		baseURL:     "https://caldav.icloud.com",
		discovered:  true,
		shardBase:   "https://caldav.icloud.com",
		homeSetPath: "/home/",
		allowHost:   func(host string) bool { return host == "caldav.icloud.com" },
	}
}

func calendarTestResponse(req *http.Request, status int, headers http.Header, body io.Reader) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	if body == nil {
		body = strings.NewReader("")
	}
	return &http.Response{
		StatusCode:    status,
		Header:        headers,
		Body:          io.NopCloser(body),
		ContentLength: -1,
		Request:       req,
	}
}

func requireICloudCode(t *testing.T, err error, code Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", code)
	}
	if typed := AsICloudError(err); typed == nil || typed.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestCalendarGETBodyIsBoundedBeforeICalDecode(t *testing.T) {
	body := bytes.Repeat([]byte("x"), maxGetBodySize+1)
	client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", ical.MIMEType)
		return calendarTestResponse(req, http.StatusOK, header, bytes.NewReader(body)), nil
	}))

	_, err := client.getCalendarObject(context.Background(), "/home/cal/", "/home/cal/event.ics")
	requireICloudCode(t, err, CodePayloadTooLarge)
	if strings.Contains(err.Error(), strings.Repeat("x", 32)) {
		t.Fatalf("GET overflow error included remote body data: %v", err)
	}
}

func TestCalendarGETParseFailuresAreSanitizedAtClientBoundary(t *testing.T) {
	const remoteSentinel = "REMOTE-SENTINEL-UID-AND-PROPERTY"
	t.Run("decoder", func(t *testing.T) {
		body := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:" + remoteSentinel + "\r\nBROKEN"
		client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
			header := make(http.Header)
			header.Set("Content-Type", ical.MIMEType)
			return calendarTestResponse(req, http.StatusOK, header, strings.NewReader(body)), nil
		}))
		_, err := client.getCalendarObject(context.Background(), "/home/cal/", "/home/cal/event.ics")
		requireICloudCode(t, err, CodeProtocolError)
		if strings.Contains(err.Error(), remoteSentinel) {
			t.Fatalf("decoder error leaked remote data: %v", err)
		}
	})

	t.Run("semantic property", func(t *testing.T) {
		body := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:uid-1\r\nSUMMARY:Safe\r\nDTSTART:" + remoteSentinel + "\r\nDTEND:20260701T110000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
			header := make(http.Header)
			header.Set("Content-Type", ical.MIMEType)
			header.Set("ETag", `"v1"`)
			return calendarTestResponse(req, http.StatusOK, header, strings.NewReader(body)), nil
		}))
		_, err := client.GetEvent(context.Background(), "/home/cal/", "uid-1")
		requireICloudCode(t, err, CodeProtocolError)
		if strings.Contains(err.Error(), remoteSentinel) || strings.Contains(err.Error(), "uid-1") {
			t.Fatalf("semantic parse error leaked remote data: %v", err)
		}
	})
}

func TestRemoteEventFieldLimits(t *testing.T) {
	tests := []struct {
		name  string
		prop  string
		limit int
	}{
		{"uid", ical.PropUID, MaxUIDLen},
		{"title", ical.PropSummary, MaxTitleLen},
		{"location", ical.PropLocation, MaxLocationLen},
		{"notes", ical.PropDescription, MaxNotesLen},
		{"url", ical.PropURL, MaxURLLen},
		{"rrule", ical.PropRecurrenceRule, 1024},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cal := ical.NewCalendar()
			event := ical.NewEvent()
			event.Props.SetText(ical.PropUID, "uid-1")
			prop := ical.NewProp(test.prop)
			prop.Value = strings.Repeat("x", test.limit+1)
			event.Props.Set(prop)
			cal.Children = append(cal.Children, event.Component)

			_, _, err := parseCalendarObject(&extcaldav.CalendarObject{Path: "/home/cal/event.ics", Data: cal})
			requireICloudCode(t, err, CodePayloadTooLarge)
			if strings.Contains(err.Error(), strings.Repeat("x", 16)) {
				t.Fatalf("field-limit error leaked remote value: %v", err)
			}
		})
	}
}

func TestRemoteCalendarShapeLimits(t *testing.T) {
	t.Run("components", func(t *testing.T) {
		cal := ical.NewCalendar()
		for range maxRemoteComponents {
			cal.Children = append(cal.Children, ical.NewComponent(ical.CompTimezone))
		}
		requireICloudCode(t, validateRemoteCalendar(cal), CodePayloadTooLarge)
	})

	t.Run("properties", func(t *testing.T) {
		cal := ical.NewCalendar()
		cal.Props["X-LIMIT"] = make([]ical.Prop, maxRemotePropertiesPerComponent+1)
		requireICloudCode(t, validateRemoteCalendar(cal), CodePayloadTooLarge)
	})

	t.Run("overrides", func(t *testing.T) {
		cal := ical.NewCalendar()
		master := ical.NewEvent()
		master.Props.SetText(ical.PropUID, "uid-1")
		cal.Children = append(cal.Children, master.Component)
		for i := 0; i <= maxRemoteOverrides; i++ {
			override := ical.NewEvent()
			override.Props.SetText(ical.PropUID, "uid-1")
			override.Props.SetDateTime(ical.PropRecurrenceID, time.Unix(int64(i+1), 0).UTC())
			cal.Children = append(cal.Children, override.Component)
		}
		requireICloudCode(t, validateRemoteCalendar(cal), CodePayloadTooLarge)
	})
}

func TestExpandOccurrencesRejectsOversizedMasterBeforeCopy(t *testing.T) {
	master := Event{
		UID:        "uid-1",
		Title:      strings.Repeat("x", MaxTitleLen+1),
		StartTime:  time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
		Recurrence: "FREQ=MINUTELY;COUNT=2000",
	}
	events, _, err := ExpandOccurrences(master, nil, master.StartTime, master.StartTime.Add(48*time.Hour), 0)
	requireICloudCode(t, err, CodePayloadTooLarge)
	if len(events) != 0 {
		t.Fatalf("oversized master produced %d copied occurrences", len(events))
	}
}

func TestCalendarDAVRedirectsPreserveMethodBodyAndHeaders(t *testing.T) {
	statuses := []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}
	methods := []string{http.MethodGet, "PROPFIND", "REPORT", http.MethodPut, http.MethodDelete}
	for _, status := range statuses {
		for _, method := range methods {
			t.Run(strconv.Itoa(status)+"_"+method, func(t *testing.T) {
				var calls int
				doer := calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					body, err := io.ReadAll(req.Body)
					if err != nil {
						t.Fatal(err)
					}
					if req.Method != method || string(body) != "request-body" {
						t.Fatalf("hop %d request = %s %q, want %s request-body", calls, req.Method, body, method)
					}
					for name, want := range map[string]string{
						"Authorization": "Basic dGVzdDp0ZXN0",
						"If-Match":      `"etag-1"`,
						"If-None-Match": "*",
						"Depth":         "1",
					} {
						if got := req.Header.Get(name); got != want {
							t.Fatalf("hop %d header %s = %q, want %q", calls, name, got, want)
						}
					}
					if calls == 1 {
						header := make(http.Header)
						header.Set("Location", "/home/cal/redirected.ics")
						return calendarTestResponse(req, status, header, nil), nil
					}
					return calendarTestResponse(req, http.StatusNoContent, nil, nil), nil
				})
				client := initializedSecurityClient(doer)
				headers := make(http.Header)
				headers.Set("Authorization", "Basic dGVzdDp0ZXN0")
				headers.Set("If-Match", `"etag-1"`)
				headers.Set("If-None-Match", "*")
				headers.Set("Depth", "1")
				result, err := client.doCalendarRequest(context.Background(), method, "https://caldav.icloud.com/home/cal/original.ics", headers, []byte("request-body"), "/home/cal/")
				if method == http.MethodPut || method == http.MethodDelete {
					requireICloudCode(t, err, CodeOutcomeUnknown)
					if calls != 1 {
						t.Fatalf("mutation redirect dispatched %d requests, want 1", calls)
					}
					return
				}
				if err != nil {
					t.Fatalf("redirect failed: %v", err)
				}
				defer func() { _ = result.response.Body.Close() }()
				if calls != 2 || result.url.Path != "/home/cal/redirected.ics" {
					t.Fatalf("calls=%d final=%s", calls, result.url.Path)
				}
			})
		}
	}
}

func TestCalendarMutationInternalServerErrorIsOutcomeUnknown(t *testing.T) {
	client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		return calendarTestResponse(req, http.StatusInternalServerError, nil, nil), nil
	}))
	_, err := client.doCalendarRequest(context.Background(), http.MethodPut,
		"https://caldav.icloud.com/home/cal/event.ics", nil, []byte("event"), "/home/cal/")
	requireICloudCode(t, err, CodeOutcomeUnknown)
}

func TestCalendarDAVRedirectPolicyHandlesUnsafeHops(t *testing.T) {
	t.Run("303", func(t *testing.T) {
		var calls int
		client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			header := make(http.Header)
			header.Set("Location", "/home/cal/next.ics")
			return calendarTestResponse(req, http.StatusSeeOther, header, nil), nil
		}))
		_, err := client.doCalendarRequest(context.Background(), http.MethodPut, "https://caldav.icloud.com/home/cal/event.ics", nil, []byte("event"), "/home/cal/")
		requireICloudCode(t, err, CodeOutcomeUnknown)
		if calls != 1 {
			t.Fatalf("303 dispatched %d requests, want 1", calls)
		}
	})

	t.Run("hop cap", func(t *testing.T) {
		var calls int
		client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			header := make(http.Header)
			header.Set("Location", "/home/cal/next.ics")
			return calendarTestResponse(req, http.StatusMovedPermanently, header, nil), nil
		}))
		_, err := client.doCalendarRequest(context.Background(), "REPORT", "https://caldav.icloud.com/home/cal/", nil, []byte("query"), "/home/cal/")
		requireICloudCode(t, err, CodeProtocolError)
		if calls != maxCalendarRedirects+1 {
			t.Fatalf("requests = %d, want %d", calls, maxCalendarRedirects+1)
		}
	})

	for name, location := range map[string]string{
		"foreign host":       "https://evil.example/home/cal/event.ics",
		"other iCloud shard": "https://p12-caldav.icloud.com/home/cal/event.ics",
		"cross calendar":     "https://caldav.icloud.com/home/other/event.ics",
	} {
		t.Run(name, func(t *testing.T) {
			var calls int
			client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				header := make(http.Header)
				header.Set("Location", location)
				return calendarTestResponse(req, http.StatusPermanentRedirect, header, nil), nil
			}))
			client.allowHost = func(host string) bool {
				return host == "caldav.icloud.com" || host == "p12-caldav.icloud.com"
			}
			_, err := client.doCalendarRequest(context.Background(), http.MethodDelete, "https://caldav.icloud.com/home/cal/event.ics", nil, nil, "/home/cal/")
			requireICloudCode(t, err, CodeOutcomeUnknown)
			if calls != 1 {
				t.Fatalf("unsafe redirect dispatched %d requests, want 1", calls)
			}
		})
	}
}

func TestCalendarDAVAutomaticallyFollowedMutationIsOutcomeUnknown(t *testing.T) {
	client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		followed := req.Clone(req.Context())
		followed.Method = http.MethodGet
		followed.URL, _ = req.URL.Parse("/home/cal/redirected.ics")
		return calendarTestResponse(followed, http.StatusOK, nil, nil), nil
	}))

	_, err := client.doCalendarRequest(context.Background(), http.MethodPut, "https://caldav.icloud.com/home/cal/event.ics", nil, []byte("event"), "/home/cal/")
	requireICloudCode(t, err, CodeOutcomeUnknown)
}
