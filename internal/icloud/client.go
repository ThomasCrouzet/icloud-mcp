package icloud

import (
	"bytes"
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

// uidLookupWindow is the half-range used by findEventByUID when the direct
// GET on <uid>.ics fails (imported events whose filename differs from the
// UID). ±10 years keeps the REPORT tractable under the 25s tool timeout while
// covering ordinary calendar content. Events entirely outside this window
// are reported as not found on the fallback path.
const uidLookupWindow = 10 * 365 * 24 * time.Hour

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

	discoverMu  sync.Mutex
	discovered  bool
	shardBase   string
	homeSetPath string
	dav         *extcaldav.Client
	allowHost   func(string) bool
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
// credentials before starting the MCP server. Idempotent after success:
// CRUD methods also trigger it; failures are retried on the next call.
func (c *Client) Discover(ctx context.Context) error {
	return c.discover(ctx)
}

// SearchEvents searches a calendar's events overlapping [start, end].
// When opts is nil or opts.ExpandRecurrence is true, recurrences are expanded
// (RRULE + EXDATE + RECURRENCE-ID). When ExpandRecurrence is false, only
// master VEVENTs from the server time-range are returned.
func (c *Client) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time, opts *SearchOptions) (SearchResult, error) {
	if err := ValidateRange(start, end); err != nil {
		return SearchResult{}, err
	}
	if err := c.discover(ctx); err != nil {
		return SearchResult{}, err
	}
	if err := c.validateAgentCalendarPath(calendarPath); err != nil {
		return SearchResult{}, err
	}
	expand := true
	if opts != nil {
		expand = opts.ExpandRecurrence
	}

	// Time-range filter on VEVENT (the server only returns events, including
	// recurring ones, overlapping [start, end]).
	filterXML := `<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">` +
		`<C:time-range start="` + start.UTC().Format(icalTimeLayout) +
		`" end="` + end.UTC().Format(icalTimeLayout) + `"/>` +
		`</C:comp-filter></C:comp-filter></C:filter>`
	objs, err := c.reportCalendarQuery(ctx, calendarPath, filterXML)
	if err != nil {
		return SearchResult{}, fmt.Errorf("searching events (calendar=%s): %w", calendarPath, err)
	}

	var events []Event
	var truncated bool
	for i := range objs {
		master, overrides, perr := parseCalendarObject(&objs[i])
		if perr != nil {
			slog.Warn("skipping unparseable calendar object", "path", objs[i].Path, "error", perr)
			continue
		}
		// Propagate REPORT getetag onto every returned row so agents can
		// conditional-update without an extra get_event round-trip.
		master.ETag = objs[i].ETag
		if !expand {
			// Masters only: still apply overlap so zero-length/out-of-range
			// masters are not returned when the server over-selects.
			if eventOverlaps(*master, start, end) {
				events = append(events, *master)
			}
			continue
		}
		occs, t, eerr := ExpandOccurrences(*master, overrides, start, end, 0)
		if eerr != nil {
			slog.Warn("skipping recurrence expansion", "uid", master.UID, "error", eerr)
			continue
		}
		if t {
			truncated = true
		}
		for j := range occs {
			occs[j].ETag = objs[i].ETag
		}
		events = append(events, occs...)
	}
	return SearchResult{Events: events, TruncatedByExpansion: truncated}, nil
}

// GetEvent returns a single event by UID (master + metadata). Path is never
// exposed on the returned detail's JSON (Event.Path has json:"-").
func (c *Client) GetEvent(ctx context.Context, calendarPath, uid string) (*EventDetail, error) {
	if err := ValidateUID(uid); err != nil {
		return nil, err
	}
	if err := c.discover(ctx); err != nil {
		return nil, err
	}
	if err := c.validateAgentCalendarPath(calendarPath); err != nil {
		return nil, err
	}
	obj, err := c.findEventByUID(ctx, calendarPath, uid)
	if err != nil {
		if strings.Contains(err.Error(), "event not found") {
			return nil, NewError(CodeNotFound, 404, err.Error(), nil)
		}
		return nil, err
	}
	master, overrides, perr := parseCalendarObject(obj)
	if perr != nil {
		return nil, perr
	}
	master.ETag = obj.ETag
	detail := &EventDetail{
		Event:         *master,
		IsRecurring:   master.Recurrence != "",
		OverrideCount: len(overrides),
		Alarms:        parseAlarms(obj.Data),
	}
	if len(overrides) > 0 {
		detail.Overrides = make([]OccurrenceRef, 0, len(overrides))
		for _, o := range overrides {
			detail.Overrides = append(detail.Overrides, OccurrenceRef{
				RecurrenceID: o.RecurrenceID,
				StartTime:    o.StartTime,
				EndTime:      o.EndTime,
				Title:        o.Title,
				IsOverride:   true,
			})
		}
	}
	// Never leak internal path to callers that serialize EventDetail by hand.
	detail.Path = ""
	return detail, nil
}

// CreateEvent creates a new event in calendarPath.
func (c *Client) CreateEvent(ctx context.Context, calendarPath string, ne *NewEvent) (string, error) {
	if ne == nil {
		return "", fmt.Errorf("event cannot be nil")
	}
	if err := ValidateTextField("title", ne.Title, MaxTitleLen); err != nil {
		return "", err
	}
	if ne.Title == "" {
		return "", fmt.Errorf("title cannot be empty")
	}
	if err := ValidateTextField("location", ne.Location, MaxLocationLen); err != nil {
		return "", err
	}
	if err := ValidateTextField("notes", ne.Notes, MaxNotesLen); err != nil {
		return "", err
	}
	if err := ValidateRange(ne.StartTime, ne.EndTime); err != nil {
		return "", err
	}
	if ne.Recurrence != "" {
		if err := ValidateRRULE(ne.Recurrence); err != nil {
			return "", err
		}
	}
	if ne.ClientUID != "" {
		if err := ValidateUID(ne.ClientUID); err != nil {
			return "", err
		}
	}
	status := strings.ToUpper(strings.TrimSpace(ne.Status))
	if !AllowedStatus[status] {
		return "", fmt.Errorf("invalid status %q", ne.Status)
	}
	transp := strings.ToUpper(strings.TrimSpace(ne.Transparency))
	if !AllowedTransparency[transp] {
		return "", fmt.Errorf("invalid transparency %q", ne.Transparency)
	}
	if ne.URL != "" {
		if err := validateEventURL(ne.URL); err != nil {
			return "", err
		}
	}
	if ne.Timezone != "" {
		if _, err := time.LoadLocation(ne.Timezone); err != nil {
			return "", fmt.Errorf("invalid timezone %q", ne.Timezone)
		}
	}
	if err := c.discover(ctx); err != nil {
		return "", err
	}
	if err := c.validateAgentCalendarPath(calendarPath); err != nil {
		return "", err
	}
	uid := ne.ClientUID
	if uid == "" {
		var err error
		uid, err = newUID()
		if err != nil {
			return "", err
		}
	}
	path := strings.TrimSuffix(calendarPath, "/") + "/" + uid + ".ics"
	// Fast path: if client-supplied UID already exists, refuse before encode.
	// The PUT below still sends If-None-Match: * so a concurrent create of
	// the same UID cannot silently overwrite (go-webdav PutCalendarObject
	// does not expose conditional headers).
	if ne.ClientUID != "" {
		if existing, gerr := c.dav.GetCalendarObject(ctx, path); gerr == nil && existing != nil {
			return "", NewError(CodeConflict, http.StatusConflict, "event already exists for client UID; not overwriting", nil)
		}
	}
	cal := buildEventCalendar(uid, ne)
	if err := c.putCalendarObjectIfMatch(ctx, path, "", cal, "*"); err != nil {
		return "", fmt.Errorf("creating event: %w", err)
	}
	return uid, nil
}

// UpdateEvent modifies the provided (non-nil) fields of an event located by
// UID. With scope=series (default) the master VEVENT is modified. With
// scope=occurrence a RECURRENCE-ID override is created/updated; the master
// RRULE is never removed. nil = field unchanged; pointer to empty string =
// clear the field (Title/Location/Notes only).
func (c *Client) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	if err := ValidateUID(uid); err != nil {
		return err
	}
	if up == nil {
		return fmt.Errorf("update cannot be nil")
	}
	if err := ValidateIfMatchETag(up.IfMatchETag); err != nil {
		return err
	}
	if up.Title != nil {
		if err := ValidateTextField("title", *up.Title, MaxTitleLen); err != nil {
			return err
		}
	}
	if up.Location != nil {
		if err := ValidateTextField("location", *up.Location, MaxLocationLen); err != nil {
			return err
		}
	}
	if up.Notes != nil {
		if err := ValidateTextField("notes", *up.Notes, MaxNotesLen); err != nil {
			return err
		}
	}
	// Reject invalid status/transparency/URL before any network I/O.
	if err := ValidateEventUpdateFields(up); err != nil {
		return err
	}
	NormalizeEventUpdateFields(up)
	scope := up.Scope
	if scope == "" {
		scope = ScopeSeries
	}
	if scope != ScopeSeries && scope != ScopeOccurrence {
		return NewValidationError("scope must be series or occurrence")
	}
	if scope == ScopeOccurrence {
		if up.RecurrenceID == nil || up.RecurrenceID.IsZero() {
			return NewValidationError("recurrence_id is required when scope=occurrence")
		}
	}
	if err := c.discover(ctx); err != nil {
		return err
	}
	if err := c.validateAgentCalendarPath(calendarPath); err != nil {
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

	if scope == ScopeOccurrence {
		if vevent.Props.Get(ical.PropRecurrenceRule) == nil {
			return NewValidationError("scope=occurrence requires a recurring master (RRULE)")
		}
		if err := applyOccurrenceUpdate(found.Data, vevent, *up.RecurrenceID, up); err != nil {
			return err
		}
	} else {
		applyFieldUpdate(vevent, up)
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
	}

	vevent.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	seq := 0
	if p := vevent.Props.Get(ical.PropSequence); p != nil {
		if n, serr := p.Int(); serr == nil {
			seq = n
		}
	}
	setSequence(vevent, seq+1)

	etag := found.ETag
	if up.IfMatchETag != "" {
		etag = up.IfMatchETag
	}
	// Fail closed without an ETag: unconditional PUT is last-writer-wins and
	// silently overwrites concurrent edits. Callers should pass etag from
	// get_event when the server omits it on lookup.
	if etag == "" {
		return NewError(CodeConcurrentModification, http.StatusPreconditionFailed,
			"etag unavailable for conditional update; re-read with get_event and pass etag", nil)
	}
	// Conditional PUT with If-Match. 412 is never auto-retried
	// (GuardedService does not retry UpdateEvent).
	if err := c.putCalendarObjectIfMatch(ctx, found.Path, etag, found.Data); err != nil {
		return fmt.Errorf("updating event (uid=%s): %w", uid, err)
	}
	return nil
}

func applyFieldUpdate(vevent *ical.Event, up *EventUpdate) {
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
	if up.Status != nil {
		s := strings.ToUpper(strings.TrimSpace(*up.Status))
		if s == "" {
			vevent.Props.Del(ical.PropStatus)
		} else {
			vevent.Props.SetText(ical.PropStatus, s)
		}
	}
	if up.Transparency != nil {
		s := strings.ToUpper(strings.TrimSpace(*up.Transparency))
		if s == "" {
			vevent.Props.Del(ical.PropTransparency)
		} else {
			vevent.Props.SetText(ical.PropTransparency, s)
		}
	}
	if up.URL != nil {
		if *up.URL == "" {
			vevent.Props.Del(ical.PropURL)
		} else {
			vevent.Props.SetText(ical.PropURL, *up.URL)
		}
	}
}

// occurrenceCopyProps are master properties safe to copy onto a new
// RECURRENCE-ID override. ATTENDEE/ORGANIZER are excluded: copying them can
// trigger invitation churn on scheduling-capable CalDAV servers. RRULE/EXDATE
// stay on the master only.
var occurrenceCopyProps = map[string]bool{
	ical.PropUID:           true,
	ical.PropSummary:       true,
	ical.PropDescription:   true,
	ical.PropLocation:      true,
	ical.PropDateTimeStart: true,
	ical.PropDateTimeEnd:   true,
	ical.PropDuration:      true,
	ical.PropStatus:        true,
	ical.PropTransparency:  true,
	ical.PropURL:           true,
	ical.PropClass:         true,
	ical.PropCategories:    true,
	ical.PropDateTimeStamp: true,
	ical.PropCreated:       true,
	ical.PropLastModified:  true,
	ical.PropSequence:      true,
}

// applyOccurrenceUpdate creates or replaces a RECURRENCE-ID override VEVENT
// for recID, applying field patches from up. The master RRULE is preserved.
// When start is set without end, DTEND is derived as start + duration so
// agents can move an occurrence start without sending end.
func applyOccurrenceUpdate(cal *ical.Calendar, master *ical.Event, recID time.Time, up *EventUpdate) error {
	var override *ical.Component
	for _, ch := range cal.Children {
		if ch.Name != ical.CompEvent {
			continue
		}
		if p := ch.Props.Get(ical.PropRecurrenceID); p != nil {
			if t, err := p.DateTime(time.UTC); err == nil && t.UTC().Unix() == recID.UTC().Unix() {
				override = ch
				break
			}
		}
	}
	created := false
	if override == nil {
		created = true
		override = ical.NewEvent().Component
		for name, props := range master.Props {
			if !occurrenceCopyProps[name] {
				continue
			}
			for _, p := range props {
				cp := p
				override.Props.Add(&cp)
			}
		}
		override.Props.Set(setRecurrenceInstantProp(ical.PropRecurrenceID, master, recID))
		dur := masterEventDuration(master)
		ov := &ical.Event{Component: override}
		start := recID
		if up.StartTime != nil {
			start = *up.StartTime
		}
		end := start.Add(dur)
		if up.EndTime != nil {
			end = *up.EndTime
		}
		setEventDateProp(ov, ical.PropDateTimeStart, start)
		setEventDateProp(ov, ical.PropDateTimeEnd, end)
		// DTEND and DURATION are mutually exclusive (RFC 5545).
		ov.Props.Del(ical.PropDuration)
		cal.Children = append(cal.Children, override)
	}
	ov := &ical.Event{Component: override}
	prevDur := masterEventDuration(master)
	if !created {
		if sp := ov.Props.Get(ical.PropDateTimeStart); sp != nil {
			if ep := ov.Props.Get(ical.PropDateTimeEnd); ep != nil {
				if st, serr := sp.DateTime(time.UTC); serr == nil {
					if en, eerr := ep.DateTime(time.UTC); eerr == nil && en.After(st) {
						prevDur = en.Sub(st)
					}
				}
			}
		}
	}
	applyFieldUpdate(ov, up)
	if up.StartTime != nil && up.EndTime == nil {
		setEventDateProp(ov, ical.PropDateTimeEnd, up.StartTime.Add(prevDur))
		ov.Props.Del(ical.PropDuration)
	}
	if sp := ov.Props.Get(ical.PropDateTimeStart); sp != nil {
		if ep := ov.Props.Get(ical.PropDateTimeEnd); ep != nil {
			newStart, sErr := sp.DateTime(time.UTC)
			newEnd, eErr := ep.DateTime(time.UTC)
			if sErr == nil && eErr == nil && !newEnd.After(newStart) {
				return fmt.Errorf("invalid update: end (%s) must be after start (%s)", newEnd.Format(time.RFC3339), newStart.Format(time.RFC3339))
			}
		}
	}
	return nil
}

// putCalendarObjectIfMatch encodes cal and PUTs it to path. When etag is
// non-empty, If-Match is set (update concurrency). When ifNoneMatch is
// non-empty (create uses "*"), If-None-Match is set so an existing resource
// yields 412 mapped to conflict rather than a silent overwrite. A 412 with
// only If-Match maps to concurrent_modification.
func (c *Client) putCalendarObjectIfMatch(ctx context.Context, path, etag string, cal *ical.Calendar, ifNoneMatch ...string) error {
	if err := c.discover(ctx); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return fmt.Errorf("encoding event for update: %w", err)
	}
	target, err := resolvePathOnBase(c.shardBase, path)
	if err != nil {
		return fmt.Errorf("invalid event URL (%s): %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, &buf)
	if err != nil {
		return fmt.Errorf("building PUT request (%s): %w", path, err)
	}
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	if etag != "" {
		// go-webdav's GetCalendarObject stores the ETag UNQUOTED (it calls
		// strconv.Unquote on the response header). RFC 7232 If-Match wants
		// the entity-tag in its quoted form ("v1"), or W/"v1" for a weak
		// validator, or "*". Re-quote a bare unquoted value.
		req.Header.Set("If-Match", normalizeIfMatch(etag))
	}
	noneMatch := ""
	if len(ifNoneMatch) > 0 {
		noneMatch = ifNoneMatch[0]
	}
	if noneMatch != "" {
		req.Header.Set("If-None-Match", noneMatch)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// With the retry/classify doer, err is already a typed *Error
		// (e.g. concurrent_modification). With a plain test doer err is a
		// transport error. Either way, propagate as-is.
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusPreconditionFailed:
		if noneMatch != "" && etag == "" {
			return NewError(CodeConflict, http.StatusConflict, "event already exists; not overwriting", nil)
		}
		return classifyStatus(resp.StatusCode)
	case resp.StatusCode/100 == 2:
		return nil
	default:
		return classifyStatus(resp.StatusCode)
	}
}

// normalizeIfMatch turns a bare, unquoted ETag (as stored by go-webdav after
// strconv.Unquote) into a valid RFC 7232 entity-tag for an If-Match header.
// Already-quoted values (including weak W/"...") are passed through.
// Client-supplied "*" is rejected earlier by ValidateIfMatchETag; this helper
// must not reintroduce last-writer-wins If-Match: *.
func normalizeIfMatch(etag string) string {
	if etag == "" {
		return etag
	}
	if strings.HasPrefix(etag, "W/") || strings.HasPrefix(etag, `"`) {
		return etag
	}
	return `"` + etag + `"`
}

// validateAgentCalendarPath requires a syntactically valid path under the
// discovered calendar-home-set (defense in depth against in-account path
// probing outside the principal's home collection).
func (c *Client) validateAgentCalendarPath(path string) error {
	if err := ValidateCalendarPath(path); err != nil {
		return err
	}
	if c.homeSetPath == "" {
		return fmt.Errorf("calendar home-set not discovered")
	}
	if !pathUnderHomeSet(path, c.homeSetPath) {
		return NewValidationError("calendar path is outside the discovered calendar home-set")
	}
	return nil
}

// pathUnderHomeSet reports whether path is the home-set itself or a resource
// under it (trailing-slash insensitive on the home-set prefix).
func pathUnderHomeSet(path, homeSet string) bool {
	home := strings.TrimSuffix(homeSet, "/")
	if home == "" {
		return false
	}
	p := strings.TrimSuffix(path, "/")
	if p == home {
		return true
	}
	return strings.HasPrefix(path, home+"/")
}

// DeleteEvent deletes an event located by UID (or a single occurrence when
// opts.Scope == ScopeOccurrence). Dry-run performs lookup only: no PUT/DELETE.
func (c *Client) DeleteEvent(ctx context.Context, calendarPath, uid string, opts *DeleteOptions) (DeleteResult, error) {
	if err := ValidateUID(uid); err != nil {
		return DeleteResult{}, err
	}
	if opts != nil {
		if err := ValidateIfMatchETag(opts.IfMatchETag); err != nil {
			return DeleteResult{}, err
		}
	}
	scope := ScopeSeries
	if opts != nil && opts.Scope != "" {
		scope = opts.Scope
	}
	if scope != ScopeSeries && scope != ScopeOccurrence {
		return DeleteResult{}, NewValidationError("scope must be series or occurrence")
	}
	if scope == ScopeOccurrence {
		if opts == nil || opts.RecurrenceID == nil || opts.RecurrenceID.IsZero() {
			return DeleteResult{}, NewValidationError("recurrence_id is required when scope=occurrence")
		}
	}
	if err := c.discover(ctx); err != nil {
		return DeleteResult{}, err
	}
	if err := c.validateAgentCalendarPath(calendarPath); err != nil {
		return DeleteResult{}, err
	}
	obj, err := c.findEventByUID(ctx, calendarPath, uid)
	if err != nil {
		if strings.Contains(err.Error(), "event not found") {
			return DeleteResult{}, NewError(CodeNotFound, 404, err.Error(), nil)
		}
		return DeleteResult{}, err
	}

	title := ""
	if vevent, verr := findMasterVEvent(obj.Data); verr == nil {
		if p := vevent.Props.Get(ical.PropSummary); p != nil {
			title = p.Value
		}
	}

	result := DeleteResult{
		Title:       title,
		UID:         uid,
		Scope:       string(scope),
		WouldMutate: true,
	}

	if opts != nil && opts.DryRun {
		result.DryRun = true
		return result, nil
	}

	if scope == ScopeOccurrence {
		if vevent, verr := findMasterVEvent(obj.Data); verr != nil {
			return DeleteResult{}, verr
		} else if vevent.Props.Get(ical.PropRecurrenceRule) == nil {
			return DeleteResult{}, NewValidationError("scope=occurrence requires a recurring master (RRULE)")
		}
		if err := c.deleteOccurrence(ctx, obj, *opts.RecurrenceID, opts.IfMatchETag); err != nil {
			return DeleteResult{}, fmt.Errorf("deleting occurrence (uid=%s): %w", uid, err)
		}
		return result, nil
	}

	etag := obj.ETag
	if opts != nil && opts.IfMatchETag != "" {
		etag = opts.IfMatchETag
	}
	if err := c.deleteCalendarObjectIfMatch(ctx, obj.Path, etag); err != nil {
		return DeleteResult{}, fmt.Errorf("deleting event (uid=%s): %w", uid, err)
	}
	return result, nil
}

// deleteOccurrence cancels a single occurrence by adding EXDATE to the master
// (and removing a matching RECURRENCE-ID override if present). It never
// deletes the series resource. EXDATE form matches master DTSTART.
func (c *Client) deleteOccurrence(ctx context.Context, obj *extcaldav.CalendarObject, recID time.Time, ifMatch string) error {
	vevent, err := findMasterVEvent(obj.Data)
	if err != nil {
		return err
	}
	// Dedupe: skip adding EXDATE when the instant is already excluded.
	already := false
	for _, p := range vevent.Props[ical.PropExceptionDates] {
		prop := p
		dates, derr := parseExDateProp(&prop)
		if derr != nil {
			continue
		}
		for _, d := range dates {
			if d.UTC().Unix() == recID.UTC().Unix() {
				already = true
				break
			}
		}
		if already {
			break
		}
	}
	if !already {
		vevent.Props.Add(setRecurrenceInstantProp(ical.PropExceptionDates, vevent, recID))
	}
	// Drop any override VEVENT whose RECURRENCE-ID matches.
	var kept []*ical.Component
	for _, ch := range obj.Data.Children {
		if ch.Name != ical.CompEvent {
			kept = append(kept, ch)
			continue
		}
		if p := ch.Props.Get(ical.PropRecurrenceID); p != nil {
			if t, derr := p.DateTime(time.UTC); derr == nil && t.UTC().Unix() == recID.UTC().Unix() {
				continue // drop override
			}
		}
		kept = append(kept, ch)
	}
	obj.Data.Children = kept
	vevent.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	seq := 0
	if p := vevent.Props.Get(ical.PropSequence); p != nil {
		if n, serr := p.Int(); serr == nil {
			seq = n
		}
	}
	setSequence(vevent, seq+1)
	etag := obj.ETag
	if ifMatch != "" {
		etag = ifMatch
	}
	if etag == "" {
		return NewError(CodeConcurrentModification, http.StatusPreconditionFailed,
			"etag unavailable for conditional update; re-read with get_event and pass etag", nil)
	}
	return c.putCalendarObjectIfMatch(ctx, obj.Path, etag, obj.Data)
}

// deleteCalendarObjectIfMatch DELETEs path with If-Match. Empty etag fails
// closed (same policy as update/occurrence): unconditional DELETE would be
// last-writer-wins against a concurrent edit. Callers pass etag from get_event
// when the lookup response omitted one.
func (c *Client) deleteCalendarObjectIfMatch(ctx context.Context, path, etag string) error {
	if err := c.discover(ctx); err != nil {
		return err
	}
	if etag == "" {
		return NewError(CodeConcurrentModification, http.StatusPreconditionFailed,
			"etag unavailable for conditional delete; re-read with get_event and pass etag", nil)
	}
	target, err := resolvePathOnBase(c.shardBase, path)
	if err != nil {
		return fmt.Errorf("invalid event URL (%s): %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return fmt.Errorf("building DELETE request (%s): %w", path, err)
	}
	req.Header.Set("If-Match", normalizeIfMatch(etag))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusPreconditionFailed:
		return classifyStatus(resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		// Idempotent: already gone.
		return nil
	case resp.StatusCode/100 == 2:
		return nil
	default:
		return classifyStatus(resp.StatusCode)
	}
}

// findEventByUID locates an event by UID. The .ics file name is NOT
// guaranteed to equal the UID for imported events (e.g. from another
// client): never guess a path, always search by UID.
//
// Always returns a full object suitable for re-PUT: direct GET on <uid>.ics
// when possible; otherwise REPORT discovers the href, then GET re-fetches
// the complete VCALENDAR (VERSION/PRODID/VTIMEZONE). REPORT calendar-data
// alone can omit components required by go-ical encode.
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
	// filter iCloud accepts is time-range: scan a bounded window around now
	// (uidLookupWindow) and filter by UID client-side. Full epoch scans
	// (1970-2100) were unbounded on large calendars and risked the 25s tool
	// timeout / 32 MiB REPORT cap.
	now := time.Now().UTC()
	wideStart := now.Add(-uidLookupWindow)
	wideEnd := now.Add(uidLookupWindow)
	filterXML := `<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">` +
		`<C:time-range start="` + wideStart.Format(icalTimeLayout) +
		`" end="` + wideEnd.Format(icalTimeLayout) + `"/>` +
		`</C:comp-filter></C:comp-filter></C:filter>`
	objs, err := c.reportCalendarQuery(ctx, calendarPath, filterXML)
	if err != nil {
		return nil, fmt.Errorf("finding event (uid=%s): %w", uid, err)
	}
	for i := range objs {
		if !calendarHasUID(objs[i].Data, uid) {
			continue
		}
		href := objs[i].Path
		if href == "" {
			return nil, fmt.Errorf("finding event (uid=%s): REPORT match has empty path", uid)
		}
		// Mandatory re-GET: do not re-PUT filtered REPORT calendar-data.
		obj, gerr := c.dav.GetCalendarObject(ctx, href)
		if gerr != nil {
			return nil, fmt.Errorf("re-fetching event after REPORT (uid=%s): %w", uid, gerr)
		}
		if !calendarHasUID(obj.Data, uid) {
			return nil, fmt.Errorf("event not found (uid=%s)", uid)
		}
		return obj, nil
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
// FULL calendar-data (bare <C:calendar-data/>) and getetag with the provided
// filter, then decodes each object via go-ical. getetag populates
// CalendarObject.ETag so UpdateEvent can send If-Match even when the object
// was located via this REPORT path (imported events, filename != UID).
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
		`<D:prop><D:getetag/><C:calendar-data/></D:prop>` +
		filterXML +
		`</C:calendar-query>`

	target, err := resolvePathOnBase(c.shardBase, calendarPath)
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

	// maxReportBodySize+1 makes overflow detectable (same pattern as PROPFIND).
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReportBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("reading REPORT response (%s): %w", calendarPath, err)
	}
	if len(data) > maxReportBodySize {
		return nil, fmt.Errorf("REPORT response (%s) too large (> %d bytes)", calendarPath, maxReportBodySize)
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
		// Strip strong ETag quotes the same way go-webdav does on GET so
		// normalizeIfMatch can re-quote consistently for If-Match. Weak
		// ETags (W/"...") are left intact for normalizeIfMatch.
		etag := strings.TrimSpace(prop.GetETag)
		if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
			etag = etag[1 : len(etag)-1]
		}
		objPath := hrefPath(r.Href)
		// Reject odd/foreign hrefs from a compromised or buggy multistatus
		// (must stay path-absolute under the discovered home-set).
		if err := ValidateCalendarPath(objPath); err != nil {
			slog.Warn("skipping REPORT href failing path validation", "href", r.Href)
			continue
		}
		if c.homeSetPath != "" && !pathUnderHomeSet(objPath, c.homeSetPath) {
			slog.Warn("skipping REPORT href outside home-set", "href", r.Href)
			continue
		}
		objs = append(objs, extcaldav.CalendarObject{Path: objPath, Data: cal, ETag: etag})
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
