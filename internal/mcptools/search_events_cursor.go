package mcptools

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

const (
	searchCursorStartMarker             = "start"
	maxSearchCursorTokenLen             = 1024
	maxSearchCursorPayloadBytes         = 256
	maxSearchCursorEnvelopeBytes        = maxSearchCursorTokenLen + 64
	searchCursorVersion                 = "v1"
	searchCursorVersionNumber           = 1
	searchCursorTTL                     = 300 * time.Second
	searchCursorReasonMalformed         = "malformed"
	searchCursorReasonSignature         = "signature"
	searchCursorReasonExpired           = "expired"
	searchCursorReasonQueryMismatch     = "query_mismatch"
	searchCursorReasonStale             = "stale"
	searchCursorReasonSelectionRequired = "selection_required"
	searchCursorReasonOffsetNotAllowed  = "offset_not_allowed"
)

type searchCursorDigest [sha256.Size]byte

type searchCursorPayload struct {
	Exp      int64  `json:"exp"`
	Query    string `json:"q"`
	Position string `json:"p"`
}

type searchCursorCodec struct {
	key [sha256.Size]byte
	now func() time.Time
	ttl time.Duration
	err error

	mu    sync.Mutex
	scans map[searchCursorDigest]searchScanSnapshot
}

func (c *searchCursorCodec) lifetime() time.Duration {
	if c != nil && c.ttl > 0 {
		return c.ttl
	}
	return searchCursorTTL
}

// maxSearchScanSnapshots bounds process-local continuation snapshots. Each
// snapshot holds at most the existing 10,000-event materialization budget.
const maxSearchScanSnapshots = 4

// searchScanSnapshot is a short-lived, process-local result of one completed
// cursor scan. It is never written to disk or logs.
type searchScanSnapshot struct {
	rows                 []searchCursorRow
	warnings             []string
	truncatedByExpansion bool
	partialFailure       bool
	created              time.Time
}

func (c *searchCursorCodec) storeScan(query searchCursorDigest, snapshot searchScanSnapshot) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scans == nil {
		c.scans = make(map[searchCursorDigest]searchScanSnapshot, maxSearchScanSnapshots)
	}
	for key, existing := range c.scans {
		if c.currentTime().Sub(existing.created) >= c.lifetime() {
			delete(c.scans, key)
		}
	}
	for len(c.scans) >= maxSearchScanSnapshots {
		var oldestKey searchCursorDigest
		var oldest time.Time
		first := true
		for key, existing := range c.scans {
			if first || existing.created.Before(oldest) {
				oldestKey, oldest, first = key, existing.created, false
			}
		}
		delete(c.scans, oldestKey)
	}
	snapshot.created = c.currentTime()
	c.scans[query] = snapshot
}

func (c *searchCursorCodec) loadScan(query searchCursorDigest) (searchScanSnapshot, bool) {
	if c == nil {
		return searchScanSnapshot{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot, ok := c.scans[query]
	if !ok {
		return searchScanSnapshot{}, false
	}
	if c.currentTime().Sub(snapshot.created) >= c.lifetime() {
		delete(c.scans, query)
		return searchScanSnapshot{}, false
	}
	return snapshot, true
}

func newSearchCursorCodec() *searchCursorCodec {
	codec := &searchCursorCodec{now: time.Now}
	if _, err := rand.Read(codec.key[:]); err != nil {
		codec.err = err
	}
	return codec
}

func (c *searchCursorCodec) available() bool {
	return c != nil && c.err == nil
}

func (c *searchCursorCodec) currentTime() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func searchCursorUnavailableError() error {
	return icloud.NewError(icloud.CodeInternal, 0, "cursor signing is unavailable", nil)
}

func searchCursorInternalError(message string) error {
	return icloud.NewError(icloud.CodeInternal, 0, message, nil)
}

func searchCursorValidationError(reason, message string) error {
	err := icloud.NewValidationError(message)
	err.Details = map[string]string{"cursor": reason}
	return err
}

func searchCursorMessage(reason string) string {
	switch reason {
	case searchCursorReasonMalformed:
		return "cursor is malformed; use start or the exact nextCursor value"
	case searchCursorReasonSignature:
		return "cursor signature is invalid"
	case searchCursorReasonExpired:
		return "cursor has expired; restart the search with cursor=start"
	case searchCursorReasonQueryMismatch:
		return "cursor does not match this query, calendar selection, or resource scope"
	case searchCursorReasonStale:
		return "cursor is stale; restart the search with cursor=start"
	case searchCursorReasonSelectionRequired:
		return "cursor requires a nonempty calendar or calendars selection"
	case searchCursorReasonOffsetNotAllowed:
		return "cursor requires offset 0"
	default:
		return "cursor is invalid"
	}
}

func readSearchCursorArg(req mcp.CallToolRequest) (bool, string, error) {
	value, ok := req.GetArguments()["cursor"]
	if !ok {
		return false, "", nil
	}
	text, ok := value.(string)
	if !ok {
		return true, "", searchCursorValidationError(searchCursorReasonMalformed, "cursor must be a string")
	}
	if text == "" || len(text) > maxSearchCursorTokenLen {
		return true, "", searchCursorValidationError(searchCursorReasonMalformed, searchCursorMessage(searchCursorReasonMalformed))
	}
	for i := 0; i < len(text); i++ {
		if text[i] > 0x7f {
			return true, "", searchCursorValidationError(searchCursorReasonMalformed, searchCursorMessage(searchCursorReasonMalformed))
		}
	}
	if text == searchCursorStartMarker {
		return true, "", nil
	}
	return true, text, nil
}

func hasExplicitSearchSelection(req mcp.CallToolRequest) bool {
	for _, name := range []string{"calendar", "calendars"} {
		for _, path := range strings.Split(req.GetString(name, ""), ",") {
			if strings.TrimSpace(path) != "" {
				return true
			}
		}
	}
	return false
}

func (c *searchCursorCodec) mint(query, position searchCursorDigest) (string, error) {
	if !c.available() {
		return "", searchCursorUnavailableError()
	}
	payload := searchCursorPayload{
		Exp:      c.currentTime().Add(searchCursorTTL).Unix(),
		Query:    hex.EncodeToString(query[:]),
		Position: hex.EncodeToString(position[:]),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", searchCursorInternalError("cursor payload could not be encoded")
	}
	if len(raw) > maxSearchCursorPayloadBytes {
		return "", searchCursorInternalError("cursor payload exceeded its bound")
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	signed := searchCursorVersion + "." + encoded
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write([]byte(signed))
	token := signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(token) > maxSearchCursorTokenLen {
		return "", searchCursorInternalError("cursor token exceeded its bound")
	}
	return token, nil
}

func (c *searchCursorCodec) verify(token string, wantQuery searchCursorDigest) (searchCursorDigest, string, error) {
	var position searchCursorDigest
	if !c.available() {
		return position, "", searchCursorUnavailableError()
	}
	if token == "" || len(token) > maxSearchCursorTokenLen {
		return position, searchCursorReasonMalformed, nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != searchCursorVersion || parts[1] == "" || parts[2] == "" {
		return position, searchCursorReasonMalformed, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 || len(raw) > maxSearchCursorPayloadBytes {
		return position, searchCursorReasonMalformed, nil
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(mac) != sha256.Size {
		return position, searchCursorReasonSignature, nil
	}
	expected := hmac.New(sha256.New, c.key[:])
	_, _ = expected.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(expected.Sum(nil), mac) {
		return position, searchCursorReasonSignature, nil
	}

	var payload searchCursorPayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return position, searchCursorReasonMalformed, nil
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return position, searchCursorReasonMalformed, nil
	}
	if payload.Exp <= 0 || !isSearchCursorHex(payload.Query) || !isSearchCursorHex(payload.Position) {
		return position, searchCursorReasonMalformed, nil
	}
	if c.currentTime().Unix() >= payload.Exp {
		return position, searchCursorReasonExpired, nil
	}
	if payload.Query != hex.EncodeToString(wantQuery[:]) {
		return position, searchCursorReasonQueryMismatch, nil
	}
	decoded, err := hex.DecodeString(payload.Position)
	if err != nil || len(decoded) != sha256.Size {
		return position, searchCursorReasonMalformed, nil
	}
	copy(position[:], decoded)
	return position, "", nil
}

func isSearchCursorHex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' {
			continue
		}
		return false
	}
	return true
}

type searchCursorQueryBinding struct {
	Version          int      `json:"v"`
	Start            string   `json:"start"`
	End              string   `json:"end"`
	Calendars        []string `json:"calendars"`
	Query            string   `json:"query"`
	UID              string   `json:"uid"`
	Status           string   `json:"status"`
	AllDayFilter     bool     `json:"allDayFilter"`
	AllDay           bool     `json:"allDay"`
	IncludeCancelled bool     `json:"includeCancelled"`
	BusyOnly         bool     `json:"busyOnly"`
	Compact          bool     `json:"compact"`
	ExpandRecurrence bool     `json:"expandRecurrence"`
	ScopeRestricted  bool     `json:"scopeRestricted"`
	Scope            []string `json:"scope"`
}

func searchCursorQueryDigest(start, end time.Time, calendars []string, query, uid, status string,
	allDayFilter, allDay, includeCancelled, busyOnly, compact, expandRecurrence bool,
	scope []string, scopeRestricted bool) searchCursorDigest {
	binding := searchCursorQueryBinding{
		Version:          searchCursorVersionNumber,
		Start:            start.UTC().Format(time.RFC3339Nano),
		End:              end.UTC().Format(time.RFC3339Nano),
		Calendars:        canonicalSearchCursorStrings(calendars),
		Query:            query,
		UID:              uid,
		Status:           status,
		AllDayFilter:     allDayFilter,
		AllDay:           allDay,
		IncludeCancelled: includeCancelled,
		BusyOnly:         busyOnly,
		Compact:          compact,
		ExpandRecurrence: expandRecurrence,
		ScopeRestricted:  scopeRestricted,
		Scope:            canonicalSearchCursorStrings(scope),
	}
	return hashSearchCursorJSON(binding)
}

func canonicalSearchCursorStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

type searchCursorRowIdentity struct {
	Start        string `json:"start"`
	End          string `json:"end"`
	RecurrenceID string `json:"recurrenceId"`
	Calendar     string `json:"calendar"`
	UID          string `json:"uid"`
	Title        string `json:"title"`
	Location     string `json:"location"`
	Notes        string `json:"notes"`
	AllDay       bool   `json:"allDay"`
	IsFloating   bool   `json:"isFloating"`
	DSTAdjusted  bool   `json:"dstAdjusted"`
	Recurrence   string `json:"recurrence"`
	Timezone     string `json:"timezone"`
	Status       string `json:"status"`
	Transparency string `json:"transparency"`
	URL          string `json:"url"`
	ETag         string `json:"etag"`
	Path         string `json:"path"`
	IsOverride   bool   `json:"isOverride"`
}

type searchCursorRow struct {
	event  icloud.Event
	digest searchCursorDigest
}

func newSearchCursorRow(event icloud.Event) searchCursorRow {
	recurrenceID := ""
	if !event.RecurrenceID.IsZero() {
		recurrenceID = event.RecurrenceID.UTC().Format(time.RFC3339Nano)
	}
	identity := searchCursorRowIdentity{
		Start:        event.StartTime.UTC().Format(time.RFC3339Nano),
		End:          event.EndTime.UTC().Format(time.RFC3339Nano),
		RecurrenceID: recurrenceID,
		Calendar:     event.Calendar,
		UID:          event.UID,
		Title:        event.Title,
		Location:     event.Location,
		Notes:        event.Notes,
		AllDay:       event.AllDay,
		IsFloating:   event.IsFloating,
		DSTAdjusted:  event.DSTAdjusted,
		Recurrence:   event.Recurrence,
		Timezone:     event.Timezone,
		Status:       event.Status,
		Transparency: event.Transp,
		URL:          event.URL,
		ETag:         event.ETag,
		Path:         event.Path,
		IsOverride:   event.IsOverride,
	}
	return searchCursorRow{event: event, digest: hashSearchCursorJSON(identity)}
}

func hashSearchCursorJSON(value any) searchCursorDigest {
	encoded, _ := json.Marshal(value)
	return sha256.Sum256(encoded)
}

func sortSearchCursorRows(rows []searchCursorRow) {
	sort.Slice(rows, func(i, j int) bool {
		return searchCursorRowLess(rows[i], rows[j])
	})
}

func searchCursorRowLess(a, b searchCursorRow) bool {
	if !a.event.StartTime.Equal(b.event.StartTime) {
		return a.event.StartTime.Before(b.event.StartTime)
	}
	if a.event.UID != b.event.UID {
		return a.event.UID < b.event.UID
	}
	if a.event.Calendar != b.event.Calendar {
		return a.event.Calendar < b.event.Calendar
	}
	if a.event.Title != b.event.Title {
		return a.event.Title < b.event.Title
	}
	if !a.event.RecurrenceID.Equal(b.event.RecurrenceID) {
		return a.event.RecurrenceID.Before(b.event.RecurrenceID)
	}
	if !a.event.EndTime.Equal(b.event.EndTime) {
		return a.event.EndTime.Before(b.event.EndTime)
	}
	if a.event.IsOverride != b.event.IsOverride {
		return !a.event.IsOverride
	}
	if a.event.Path != b.event.Path {
		return a.event.Path < b.event.Path
	}
	if a.event.ETag != b.event.ETag {
		return a.event.ETag < b.event.ETag
	}
	return bytes.Compare(a.digest[:], b.digest[:]) < 0
}

func collapseSearchCursorRows(rows []searchCursorRow) []searchCursorRow {
	if len(rows) < 2 {
		return rows
	}
	out := rows[:1]
	for i := 1; i < len(rows); i++ {
		if rows[i].digest == out[len(out)-1].digest {
			continue
		}
		out = append(out, rows[i])
	}
	return out
}

func findSearchCursorRow(rows []searchCursorRow, position searchCursorDigest) (int, bool) {
	for i := range rows {
		if rows[i].digest == position {
			return i, true
		}
	}
	return 0, false
}
