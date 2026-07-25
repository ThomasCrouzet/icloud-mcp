package icloud

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	extcaldav "github.com/emersion/go-webdav/caldav"
)

// maxEventsPerCalendarSearch bounds aggregate materialization before MCP-level
// filtering and the smaller public result cap are applied.
const maxEventsPerCalendarSearch = 10000

// icalTimeLayout is the format of calendar-query time-range bounds
// (RFC 4791) and of iCalendar dates in UTC.
const icalTimeLayout = "20060102T150405Z"

// maxReportBodySize bounds how much of a REPORT response is read (defense
// in depth against an abnormally large response).
const maxReportBodySize = 32 << 20 // 32 MiB

// uidLookupWindow is the half-range used by findEventByUID when the direct
// GET on <uid>.ics fails (imported events whose filename differs from the
// UID). A +/-10-year window keeps the REPORT tractable under the 25s tool timeout while
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

	discoverMu   sync.Mutex
	discovered   bool
	discovering  bool
	discoverWait chan struct{}
	shardBase    string
	homeSetPath  string
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
		return SearchResult{}, fmt.Errorf("searching events: %w", err)
	}

	var events []Event
	var truncated bool
	remainingWork := maxRecurrenceSearchWork
	for i := range objs {
		master, overrides, perr := parseCalendarObject(&objs[i])
		if perr != nil {
			if ie := AsICloudError(perr); ie != nil && (ie.Code == CodePayloadTooLarge || ie.Code == CodeProtocolError) {
				return SearchResult{}, ie
			}
			return SearchResult{}, NewError(CodeProtocolError, 0, "Calendar event data is malformed", nil)
		}
		// Propagate REPORT getetag onto every returned row so agents can
		// conditional-update without an extra get_event round-trip.
		master.ETag = objs[i].ETag
		if !expand {
			// The server selects recurring resources when any occurrence is in
			// range, even though the master's DTSTART can be years outside it.
			// Keep those masters; only locally filter over-selected one-offs.
			if master.Recurrence != "" || eventOverlaps(*master, start, end) {
				if len(events) >= maxEventsPerCalendarSearch {
					return SearchResult{}, NewError(CodePayloadTooLarge, 0, "Calendar search exceeded its event limit", nil)
				}
				events = append(events, *master)
			}
			continue
		}
		occs, t, eerr := expandOccurrencesContext(ctx, *master, overrides, start, end, 0, &remainingWork)
		if eerr != nil {
			if ie := AsICloudError(eerr); ie != nil {
				return SearchResult{}, ie
			}
			return SearchResult{}, NewError(CodeProtocolError, 0, "Calendar recurrence data is malformed", nil)
		}
		if t {
			truncated = true
		}
		for j := range occs {
			occs[j].ETag = objs[i].ETag
		}
		if len(occs) > maxEventsPerCalendarSearch-len(events) {
			return SearchResult{}, NewError(CodePayloadTooLarge, 0, "Calendar search exceeded its event limit", nil)
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
		return nil, err
	}
	master, overrides, perr := parseCalendarObject(obj)
	if perr != nil {
		if AsICloudError(perr) != nil {
			return nil, perr
		}
		return nil, NewError(CodeProtocolError, 0, "Calendar event data is malformed", nil)
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
	if len(ne.ExDates) > maxRemoteExDates {
		return "", NewValidationError(fmt.Sprintf("at most %d recurrence exceptions allowed", maxRemoteExDates))
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
		if err := validateRRULEForStartContext(ctx, ne.Recurrence, ne.StartTime); err != nil {
			return "", err
		}
		if loc := resolveWriteLocation(ne); loc != nil && loc != time.UTC && !recurringTimezoneStable(loc, ne.StartTime) {
			return "", NewValidationError("timezone does not have a stable annual transition rule for recurring writes")
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
		existing, gerr := c.getCalendarObject(ctx, calendarPath, path)
		if gerr == nil && existing != nil {
			return "", NewError(CodeConflict, http.StatusConflict, "event already exists for client UID; not overwriting", nil)
		}
		if gerr != nil {
			if ie := AsICloudError(gerr); ie == nil || ie.Code != CodeNotFound {
				return "", gerr
			}
		}
	}
	cal := buildEventCalendar(uid, ne)
	if err := c.putCalendarObjectIfMatch(ctx, calendarPath, path, "", cal, "*"); err != nil {
		err = withOutcomeReconciliation(err, uid,
			"Call get_event with this UID. If it exists, do not create it again; if it is not found, retry with the same client UID.")
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
		if err := applyFieldUpdate(vevent, up); err != nil {
			return err
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
	}

	if err := incrementSequence(vevent); err != nil {
		return err
	}
	vevent.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())

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
	if err := c.putCalendarObjectIfMatch(ctx, calendarPath, found.Path, etag, found.Data); err != nil {
		err = withOutcomeReconciliation(err, uid,
			"Call get_event with this UID and compare the intended fields before deciding whether to retry the update.")
		return fmt.Errorf("updating event: %w", err)
	}
	return nil
}

func applyFieldUpdate(vevent *ical.Event, up *EventUpdate) error {
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
		if err := setEventDateProp(vevent, ical.PropDateTimeStart, *up.StartTime); err != nil {
			return err
		}
	}
	if up.EndTime != nil {
		if err := setEventDateProp(vevent, ical.PropDateTimeEnd, *up.EndTime); err != nil {
			return err
		}
		// DTEND and DURATION are mutually exclusive. An explicit end replaces
		// any duration-based representation.
		vevent.Props.Del(ical.PropDuration)
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
	return nil
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
		recProp, err := setRecurrenceInstantProp(ical.PropRecurrenceID, master, recID)
		if err != nil {
			return err
		}
		override.Props.Set(recProp)
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
		if err := setEventDateProp(ov, ical.PropDateTimeStart, start); err != nil {
			return err
		}
		if err := setEventDateProp(ov, ical.PropDateTimeEnd, end); err != nil {
			return err
		}
		// DTEND and DURATION are mutually exclusive (RFC 5545).
		ov.Props.Del(ical.PropDuration)
		cal.Children = append(cal.Children, override)
	}
	ov := &ical.Event{Component: override}
	prevDur := masterEventDuration(master)
	preserveOwnDuration := false
	if !created {
		if sp := ov.Props.Get(ical.PropDateTimeStart); sp != nil {
			if ep := ov.Props.Get(ical.PropDateTimeEnd); ep != nil {
				if st, serr := sp.DateTime(time.UTC); serr == nil {
					if en, eerr := ep.DateTime(time.UTC); eerr == nil && en.After(st) {
						prevDur = en.Sub(st)
					}
				}
			} else if dp := ov.Props.Get(ical.PropDuration); dp != nil {
				if duration, derr := parseICalDuration(dp.Value); derr == nil && duration > 0 {
					prevDur = duration
					preserveOwnDuration = true
				}
			}
		}
	}
	if err := applyFieldUpdate(ov, up); err != nil {
		return err
	}
	if up.StartTime != nil && up.EndTime == nil && !preserveOwnDuration {
		if err := setEventDateProp(ov, ical.PropDateTimeEnd, up.StartTime.Add(prevDur)); err != nil {
			return err
		}
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
func (c *Client) putCalendarObjectIfMatch(ctx context.Context, calendarPath, path, etag string, cal *ical.Calendar, ifNoneMatch ...string) error {
	if err := c.discover(ctx); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return fmt.Errorf("encoding event for update: %w", err)
	}
	target, err := resolvePathOnBase(c.shardBase, path)
	if err != nil {
		return fmt.Errorf("invalid event URL: %w", err)
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "text/calendar; charset=utf-8")
	if etag != "" {
		parsed, err := parseStrongETag(etag)
		if err != nil {
			return NewError(CodeProtocolError, 0, "Calendar mutation has an invalid ETag", nil)
		}
		headers.Set("If-Match", parsed)
	}
	noneMatch := ""
	if len(ifNoneMatch) > 0 {
		noneMatch = ifNoneMatch[0]
	}
	if noneMatch != "" {
		headers.Set("If-None-Match", noneMatch)
	}

	result, err := c.doCalendarRequest(ctx, http.MethodPut, target, headers, buf.Bytes(), calendarPath)
	if err != nil {
		// With the retry/classify doer, err is already a typed *Error
		// (e.g. concurrent_modification). With a plain test doer err is a
		// transport error. Either way, propagate as-is.
		return err
	}
	resp := result.response
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusPreconditionFailed:
		if noneMatch != "" && etag == "" {
			return NewError(CodeConflict, resp.StatusCode, "event already exists; not overwriting", nil)
		}
		return classifyStatus(resp.StatusCode)
	case resp.StatusCode/100 == 2:
		return nil
	default:
		return classifyStatus(resp.StatusCode)
	}
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
		return DeleteResult{}, err
	}

	master, masterErr := findMasterVEvent(obj.Data)
	if masterErr != nil {
		return DeleteResult{}, masterErr
	}
	if scope == ScopeOccurrence && master.Props.Get(ical.PropRecurrenceRule) == nil {
		return DeleteResult{}, NewValidationError("scope=occurrence requires a recurring master (RRULE)")
	}

	title := ""
	if p := master.Props.Get(ical.PropSummary); p != nil {
		title = p.Value
	}

	result := DeleteResult{
		Title:       title,
		UID:         uid,
		Scope:       string(scope),
		WouldMutate: true,
	}

	if opts != nil && opts.DryRun {
		etag := obj.ETag
		if opts.IfMatchETag != "" {
			etag = opts.IfMatchETag
		}
		if etag == "" {
			return DeleteResult{}, NewError(CodeConcurrentModification, http.StatusPreconditionFailed,
				"etag unavailable for conditional delete; re-read with get_event and pass etag", nil)
		}
		if _, err := parseStrongETag(etag); err != nil {
			return DeleteResult{}, NewValidationError("etag must be a strong entity-tag")
		}
		result.DryRun = true
		return result, nil
	}

	if scope == ScopeOccurrence {
		if err := c.deleteOccurrence(ctx, calendarPath, obj, *opts.RecurrenceID, opts.IfMatchETag); err != nil {
			err = withOutcomeReconciliation(err, uid,
				"Call get_event with this UID and inspect the occurrence. If it is absent, do not repeat the delete; otherwise compare the current ETag before retrying.")
			return DeleteResult{}, fmt.Errorf("deleting occurrence: %w", err)
		}
		return result, nil
	}

	etag := obj.ETag
	if opts != nil && opts.IfMatchETag != "" {
		etag = opts.IfMatchETag
	}
	if err := c.deleteCalendarObjectIfMatch(ctx, calendarPath, obj.Path, etag); err != nil {
		err = withOutcomeReconciliation(err, uid,
			"Call get_event with this UID. A not_found result confirms deletion; if it still exists, compare its ETag before deciding whether to retry.")
		return DeleteResult{}, fmt.Errorf("deleting event: %w", err)
	}
	return result, nil
}

// deleteOccurrence cancels a single occurrence by adding EXDATE to the master
// (and removing a matching RECURRENCE-ID override if present). It never
// deletes the series resource. EXDATE form matches master DTSTART.
func (c *Client) deleteOccurrence(ctx context.Context, calendarPath string, obj *extcaldav.CalendarObject, recID time.Time, ifMatch string) error {
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
	// Drop any override VEVENT whose RECURRENCE-ID matches.
	var kept []*ical.Component
	removedOverride := false
	for _, ch := range obj.Data.Children {
		if ch.Name != ical.CompEvent {
			kept = append(kept, ch)
			continue
		}
		if p := ch.Props.Get(ical.PropRecurrenceID); p != nil {
			if t, derr := p.DateTime(time.UTC); derr == nil && t.UTC().Unix() == recID.UTC().Unix() {
				removedOverride = true
				continue // drop override
			}
		}
		kept = append(kept, ch)
	}
	if already && !removedOverride {
		return nil
	}
	if !already {
		exProp, err := setRecurrenceInstantProp(ical.PropExceptionDates, vevent, recID)
		if err != nil {
			return err
		}
		vevent.Props.Add(exProp)
	}
	obj.Data.Children = kept
	if err := incrementSequence(vevent); err != nil {
		return err
	}
	vevent.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	etag := obj.ETag
	if ifMatch != "" {
		etag = ifMatch
	}
	if etag == "" {
		return NewError(CodeConcurrentModification, http.StatusPreconditionFailed,
			"etag unavailable for conditional update; re-read with get_event and pass etag", nil)
	}
	return c.putCalendarObjectIfMatch(ctx, calendarPath, obj.Path, etag, obj.Data)
}

// deleteCalendarObjectIfMatch DELETEs path with If-Match. Empty etag fails
// closed (same policy as update/occurrence): unconditional DELETE would be
// last-writer-wins against a concurrent edit. Callers pass etag from get_event
// when the lookup response omitted one.
func (c *Client) deleteCalendarObjectIfMatch(ctx context.Context, calendarPath, path, etag string) error {
	if err := c.discover(ctx); err != nil {
		return err
	}
	if etag == "" {
		return NewError(CodeConcurrentModification, http.StatusPreconditionFailed,
			"etag unavailable for conditional delete; re-read with get_event and pass etag", nil)
	}
	target, err := resolvePathOnBase(c.shardBase, path)
	if err != nil {
		return fmt.Errorf("invalid event URL: %w", err)
	}
	parsed, err := parseStrongETag(etag)
	if err != nil {
		return NewError(CodeProtocolError, 0, "Calendar mutation has an invalid ETag", nil)
	}
	headers := make(http.Header)
	headers.Set("If-Match", parsed)
	result, err := c.doCalendarRequest(ctx, http.MethodDelete, target, headers, nil, calendarPath)
	if err != nil {
		return err
	}
	resp := result.response
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
	obj, directErr := c.getCalendarObject(ctx, calendarPath, directPath)
	if directErr == nil {
		if err := validateCalendarObjectIdentity(obj.Data, uid); err != nil {
			return nil, err
		}
		return obj, nil
	} else if ie := AsICloudError(directErr); ie == nil || ie.Code != CodeNotFound {
		return nil, directErr
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
		return nil, fmt.Errorf("finding event: %w", err)
	}
	for i := range objs {
		if !calendarHasUID(objs[i].Data, uid) {
			continue
		}
		href := objs[i].Path
		if href == "" {
			return nil, NewError(CodeProtocolError, 0, "REPORT match has empty path", nil)
		}
		// Mandatory re-GET: do not re-PUT filtered REPORT calendar-data.
		obj, gerr := c.getCalendarObject(ctx, calendarPath, href)
		if gerr != nil {
			return nil, fmt.Errorf("re-fetching event after REPORT: %w", gerr)
		}
		if err := validateCalendarObjectIdentity(obj.Data, uid); err != nil {
			return nil, err
		}
		return obj, nil
	}
	return nil, NewError(CodeNotFound, 404, "event not found", nil)
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
		return nil, fmt.Errorf("invalid calendar URL: %w", err)
	}
	headers := make(http.Header)
	headers.Set("Content-Type", `application/xml; charset="utf-8"`)
	headers.Set("Depth", "1")
	result, err := c.doCalendarRequest(ctx, "REPORT", target, headers, []byte(body), calendarPath)
	if err != nil {
		return nil, fmt.Errorf("REPORT request failed: %w", err)
	}
	resp := result.response
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 207 {
		return nil, classifyStatus(resp.StatusCode)
	}

	// maxReportBodySize+1 makes overflow detectable (same pattern as PROPFIND).
	data, err := readBoundedCalendarBody(resp.Body, maxReportBodySize, "Calendar REPORT response exceeded its byte limit")
	if err != nil {
		return nil, err
	}
	responses, err := decodeReportMultiStatus(data, resp.StatusCode)
	if err != nil {
		return nil, err
	}

	var objs []extcaldav.CalendarObject
	for _, r := range responses {
		prop := mergedOKProp(r)
		if prop == nil || prop.CalendarData == "" {
			continue
		}
		cal, derr := decodeRemoteCalendar(strings.NewReader(prop.CalendarData))
		if derr != nil {
			return nil, derr
		}
		if err := validateRemoteCalendar(cal); err != nil {
			return nil, err
		}
		if err := validateCalendarObjectIdentity(cal, ""); err != nil {
			return nil, err
		}
		etag, etagErr := strongETagFromResponse(r)
		if etagErr != nil {
			return nil, NewError(CodeProtocolError, resp.StatusCode, "Calendar REPORT response has an invalid ETag", nil)
		}
		if len(etag) > maxETagBytes {
			return nil, NewError(CodePayloadTooLarge, resp.StatusCode, "Calendar ETag exceeded its byte limit", nil)
		}
		resolved, hrefErr := c.resolveDAVHref(result.url, r.Href, calendarPath)
		if hrefErr != nil {
			return nil, hrefErr
		}
		objPath := resolved.EscapedPath()
		objs = append(objs, extcaldav.CalendarObject{Path: objPath, Data: cal, ETag: etag})
	}
	return objs, nil
}
