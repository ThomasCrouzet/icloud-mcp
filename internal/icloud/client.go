package icloud

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	extcaldav "github.com/emersion/go-webdav/caldav"
)

// icalTimeLayout is the format of calendar-query time-range bounds
// (RFC 4791) and of iCalendar dates in UTC.
const icalTimeLayout = "20060102T150405Z"

// maxReportBodySize bounds how much of a REPORT response is read (defense
// in depth against an abnormally large response).
const maxReportBodySize = 32 << 20 // 32 MiB

// httpDoer is the minimal slice of an HTTP client used by the hand-rolled
// discovery, compatible with both *http.Client and the return value of
// webdav.HTTPClientWithBasicAuth (the webdav.HTTPClient interface), which
// declares the same single Do method.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client implements Service against iCloud via go-webdav/caldav, with a
// hand-rolled shard discovery (see discovery.go). go-webdav v0.7.0 loses
// the shard host in FindCalendarHomeSet (it only returns the path), so a
// hand-rolled discovery is needed to route subsequent requests to the
// right shard (pXX-caldav.icloud.com).
type Client struct {
	http    httpDoer
	baseURL string

	discoverOnce sync.Once
	discoverErr  error
	shardBase    string
	homeSetPath  string
	dav          *extcaldav.Client
	allowHost    func(string) bool
}

var _ Service = (*Client)(nil)

// NewClient builds a Client. authHTTP is an already configured HTTP client
// (network allowlist + Basic Auth in production). baseURL is
// security.ICloudBaseURL in production, the URL of an httptest.Server in
// tests. allowHost revalidates the discovered shard host (defense in depth).
func NewClient(authHTTP httpDoer, baseURL string, allowHost func(string) bool) *Client {
	return &Client{
		http:      authHTTP,
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		allowHost: allowHost,
	}
}

// Discover forces the iCloud shard discovery. Used at boot to validate the
// credentials before starting the MCP server. Idempotent: the CRUD methods
// also trigger it automatically via sync.Once.
func (c *Client) Discover(ctx context.Context) error {
	return c.discover(ctx)
}

// SearchEvents searches a calendar's events overlapping [start, end] and
// expands recurrences (RRULE + EXDATE + RECURRENCE-ID overrides) within
// that same range.
func (c *Client) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) ([]Event, error) {
	if err := c.discover(ctx); err != nil {
		return nil, err
	}

	// Time-range filter on VEVENT (the server only returns events, including
	// recurring ones, overlapping [start, end]).
	filterXML := `<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">` +
		`<C:time-range start="` + start.UTC().Format(icalTimeLayout) +
		`" end="` + end.UTC().Format(icalTimeLayout) + `"/>` +
		`</C:comp-filter></C:comp-filter></C:filter>`
	objs, err := c.reportCalendarQuery(ctx, calendarPath, filterXML)
	if err != nil {
		return nil, fmt.Errorf("searching events (calendar=%s): %w", calendarPath, err)
	}

	var events []Event
	for i := range objs {
		master, overrides, perr := parseCalendarObject(&objs[i])
		if perr != nil {
			slog.Warn("skipping unparseable calendar object", "path", objs[i].Path, "error", perr)
			continue
		}
		occs, eerr := ExpandOccurrences(*master, overrides, start, end, 0)
		if eerr != nil {
			slog.Warn("skipping recurrence expansion", "uid", master.UID, "error", eerr)
			continue
		}
		events = append(events, occs...)
	}
	return events, nil
}

// CreateEvent creates a new event in calendarPath.
func (c *Client) CreateEvent(ctx context.Context, calendarPath string, ne *NewEvent) (string, error) {
	if err := c.discover(ctx); err != nil {
		return "", err
	}
	uid, err := newUID()
	if err != nil {
		return "", err
	}
	cal := buildEventCalendar(uid, ne)
	path := strings.TrimSuffix(calendarPath, "/") + "/" + uid + ".ics"
	if _, err := c.dav.PutCalendarObject(ctx, path, cal); err != nil {
		return "", fmt.Errorf("creating event: %w", err)
	}
	return uid, nil
}

// UpdateEvent modifies the provided (non-nil) fields of an event located by
// UID. The master VEVENT is modified (any RECURRENCE-ID overrides are left
// unchanged). nil = field unchanged, pointer to an empty string = clear the
// field.
func (c *Client) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	if err := c.discover(ctx); err != nil {
		return err
	}
	found, err := c.findEventByUID(ctx, calendarPath, uid)
	if err != nil {
		return err
	}
	// findEventByUID returns the FULL object (direct GET on <uid>.ics, or a
	// time-range scan as fallback, never filtered calendar-data): VERSION/PRODID
	// and VTIMEZONE are preserved, so it can be modified and re-PUT as is.
	vevent, err := findMasterVEvent(found.Data)
	if err != nil {
		return err
	}

	if up.Title != nil {
		if *up.Title == "" {
			vevent.Props.Del(ical.PropSummary)
		} else {
			vevent.Props.SetText(ical.PropSummary, *up.Title)
		}
	}
	if up.Location != nil {
		if *up.Location == "" {
			vevent.Props.Del(ical.PropLocation)
		} else {
			vevent.Props.SetText(ical.PropLocation, *up.Location)
		}
	}
	if up.Notes != nil {
		if *up.Notes == "" {
			vevent.Props.Del(ical.PropDescription)
		} else {
			vevent.Props.SetText(ical.PropDescription, *up.Notes)
		}
	}
	if up.StartTime != nil {
		setEventDateProp(vevent, ical.PropDateTimeStart, *up.StartTime)
	}
	if up.EndTime != nil {
		setEventDateProp(vevent, ical.PropDateTimeEnd, *up.EndTime)
	}

	// Consistency validation after merging (needed when only one of the two
	// start/end bounds is provided: consistency can only be checked after
	// re-reading the existing event).
	startProp := vevent.Props.Get(ical.PropDateTimeStart)
	endProp := vevent.Props.Get(ical.PropDateTimeEnd)
	if startProp != nil && endProp != nil {
		newStart, sErr := startProp.DateTime(time.UTC)
		newEnd, eErr := endProp.DateTime(time.UTC)
		if sErr == nil && eErr == nil && !newEnd.After(newStart) {
			return fmt.Errorf("invalid update: end (%s) must be after start (%s)", newEnd.Format(time.RFC3339), newStart.Format(time.RFC3339))
		}
	}

	vevent.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	seq := 0
	if p := vevent.Props.Get(ical.PropSequence); p != nil {
		if n, serr := p.Int(); serr == nil {
			seq = n
		}
	}
	setSequence(vevent, seq+1)

	if _, err := c.dav.PutCalendarObject(ctx, found.Path, found.Data); err != nil {
		return fmt.Errorf("updating event (uid=%s): %w", uid, err)
	}
	return nil
}

// DeleteEvent deletes an event located by UID and returns its title (echo
// required by the spec so a human can confirm what is being deleted).
func (c *Client) DeleteEvent(ctx context.Context, calendarPath, uid string) (string, error) {
	if err := c.discover(ctx); err != nil {
		return "", err
	}
	obj, err := c.findEventByUID(ctx, calendarPath, uid)
	if err != nil {
		return "", err
	}

	title := ""
	if vevent, verr := findMasterVEvent(obj.Data); verr == nil {
		if p := vevent.Props.Get(ical.PropSummary); p != nil {
			title = p.Value
		}
	}

	if err := c.dav.RemoveAll(ctx, obj.Path); err != nil {
		return "", fmt.Errorf("deleting event (uid=%s): %w", uid, err)
	}
	return title, nil
}

// findEventByUID locates an event by UID. The .ics file name is NOT
// guaranteed to equal the UID for imported events (e.g. from another
// client): never guess a path, always search by UID.
func (c *Client) findEventByUID(ctx context.Context, calendarPath, uid string) (*extcaldav.CalendarObject, error) {
	// iCloud REJECTS calendar-query <prop-filter> (412 Precondition Failed,
	// observed 2026-07-12), so filtering by UID server-side is impossible. But
	// iCloud names its resources <UID>.ics (verified): first try a direct GET
	// on that path, which returns the FULL object (suitable for update/delete).
	directPath := strings.TrimSuffix(calendarPath, "/") + "/" + uid + ".ics"
	if obj, err := c.dav.GetCalendarObject(ctx, directPath); err == nil && calendarHasUID(obj.Data, uid) {
		return obj, nil
	}

	// Fallback: imported event whose file name != UID. The only server-side
	// filter iCloud accepts is time-range: scan a very wide window and filter
	// by UID client-side.
	wideStart := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	wideEnd := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	filterXML := `<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">` +
		`<C:time-range start="` + wideStart.Format(icalTimeLayout) +
		`" end="` + wideEnd.Format(icalTimeLayout) + `"/>` +
		`</C:comp-filter></C:comp-filter></C:filter>`
	objs, err := c.reportCalendarQuery(ctx, calendarPath, filterXML)
	if err != nil {
		return nil, fmt.Errorf("finding event (uid=%s): %w", uid, err)
	}
	for i := range objs {
		if calendarHasUID(objs[i].Data, uid) {
			return &objs[i], nil
		}
	}
	return nil, fmt.Errorf("event not found (uid=%s)", uid)
}

// calendarHasUID reports whether a VCALENDAR contains a VEVENT whose UID is uid.
func calendarHasUID(cal *ical.Calendar, uid string) bool {
	if cal == nil {
		return false
	}
	for _, ch := range cal.Children {
		if ch.Name != ical.CompEvent {
			continue
		}
		if p := ch.Props.Get(ical.PropUID); p != nil && p.Value == uid {
			return true
		}
	}
	return false
}

// reportCalendarQuery sends a REPORT calendar-query (Depth:1) requesting the
// FULL calendar-data (bare <C:calendar-data/>) with the provided filter,
// then decodes each object via go-ical.
//
// Hand-rolled request (not go-webdav QueryCalendar) because iCloud does NOT
// return component properties for a PARTIAL calendar-data retrieval (a
// nested <comp name="VEVENT"><allprop/></comp> yields empty VEVENTs;
// AllProps+AllComps on VCALENDAR yields zero sub-components), observed
// against the real iCloud on 2026-07-12. Only the bare <calendar-data/>
// works, and go-webdav's QueryCalendar always emits a <comp>.
func (c *Client) reportCalendarQuery(ctx context.Context, calendarPath, filterXML string) ([]extcaldav.CalendarObject, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		`<D:prop><C:calendar-data/></D:prop>` +
		filterXML +
		`</C:calendar-query>`

	target, err := resolveRef(c.shardBase, calendarPath)
	if err != nil {
		return nil, fmt.Errorf("invalid calendar URL (%s): %w", calendarPath, err)
	}
	req, err := http.NewRequestWithContext(ctx, "REPORT", target, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building REPORT request (%s): %w", calendarPath, err)
	}
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	req.Header.Set("Depth", "1")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("REPORT request to %s: %w", calendarPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("iCloud authentication refused: check ICLOUD_EMAIL and the app-specific password")
	}
	if resp.StatusCode != 207 {
		return nil, fmt.Errorf("REPORT %s: unexpected HTTP status %d", calendarPath, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReportBodySize))
	if err != nil {
		return nil, fmt.Errorf("reading REPORT response (%s): %w", calendarPath, err)
	}
	var ms msMultistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("parsing REPORT response (%s): %w", calendarPath, err)
	}

	var objs []extcaldav.CalendarObject
	for _, r := range ms.Responses {
		prop := mergedOKProp(r)
		if prop == nil || prop.CalendarData == "" {
			continue
		}
		cal, derr := ical.NewDecoder(strings.NewReader(prop.CalendarData)).Decode()
		if derr != nil {
			slog.Warn("undecodable calendar-data, skipping object", "href", r.Href, "error", derr)
			continue
		}
		objs = append(objs, extcaldav.CalendarObject{Path: hrefPath(r.Href), Data: cal})
	}
	return objs, nil
}

// hrefPath extracts the path from an href (absolute or relative); go-webdav
// GetCalendarObject/RemoveAll expect a path resolved against the shard
// endpoint, not an absolute URL.
func hrefPath(href string) string {
	if u, err := url.Parse(href); err == nil && u.Path != "" {
		return u.Path
	}
	return href
}
