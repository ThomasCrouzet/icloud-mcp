package mcptools

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
)

const (
	contactsCursorVersion        = "c3"
	contactsCursorTTL            = 300 * time.Second
	contactsCursorMaxTokenLength = 4096
	contactsCursorMaxPayload     = 512
	maxContactsCursorEntries     = 128

	// maxContactsSnapshots bounds the retained cursor snapshots and
	// maxContactsSnapshotBytes bounds their combined summary bytes. Both caps
	// apply, so a wide contact book cannot grow the cache without limit. The
	// byte budget equals the existing 32 MiB REPORT budget, so one maximal
	// 2,000-card scan always fits and pagination never fails for a result set
	// that the scan budget already admitted.
	maxContactsSnapshots     = 4
	maxContactsSnapshotBytes = 32 << 20
)

type contactsCursorPayload struct {
	Exp   int64  `json:"e"`
	Query string `json:"q"`
	ID    string `json:"i"`
}

// contactsSnapshot is one immutable, process-local result set for a cursor
// walk. It is never written to disk, never logged, and never encoded into a
// token or an error message.
type contactsSnapshot struct {
	rows    []contacts.ContactSummary
	bytes   int
	created time.Time
}

type contactsCursorEntry struct {
	snapshot string
	position int
	expires  time.Time
}

// contactsCursorPage is a verified cursor position inside one snapshot.
// position is the index of the next row to emit.
type contactsCursorPage struct {
	snapshot string
	position int
}

type contactsCursorCodec struct {
	key [sha256.Size]byte
	now func() time.Time
	ttl time.Duration
	err error

	mu        sync.Mutex
	entries   map[string]contactsCursorEntry
	snapshots map[string]*contactsSnapshot
}

func newContactsCursorCodec() *contactsCursorCodec {
	codec := &contactsCursorCodec{
		now:       time.Now,
		ttl:       contactsCursorTTL,
		entries:   make(map[string]contactsCursorEntry),
		snapshots: make(map[string]*contactsSnapshot),
	}
	if _, err := rand.Read(codec.key[:]); err != nil {
		codec.err = err
	}
	return codec
}

var defaultContactsCursor = newContactsCursorCodec()

func (c *contactsCursorCodec) available() bool {
	return c != nil && c.err == nil
}

func (c *contactsCursorCodec) lifetime() time.Duration {
	if c != nil && c.ttl > 0 {
		return c.ttl
	}
	return contactsCursorTTL
}

func (c *contactsCursorCodec) currentTime() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

// storeSnapshot retains one immutable result set and returns its identifier and
// deadline. The caller must pass the complete bounded match set in the
// canonical order. The method refuses an empty, unordered, or oversized set, so
// the caller issues no token and reports truncation.
func (c *contactsCursorCodec) storeSnapshot(rows []contacts.ContactSummary) (string, time.Time, bool) {
	if !c.available() || len(rows) == 0 || !contactsRowsSorted(rows) {
		return "", time.Time{}, false
	}
	size := contactsSnapshotBytes(rows)
	if size <= 0 || size > maxContactsSnapshotBytes {
		return "", time.Time{}, false
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", time.Time{}, false
	}
	id := hex.EncodeToString(nonce[:])
	copied := make([]contacts.ContactSummary, len(rows))
	copy(copied, rows)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	created := c.currentTime()
	deadline := created.Add(c.lifetime())
	for len(c.snapshots) > 0 && (len(c.snapshots) >= maxContactsSnapshots ||
		c.snapshotBytesLocked()+size > maxContactsSnapshotBytes) {
		if !c.evictOldestSnapshotLocked() {
			break
		}
	}
	if c.snapshotBytesLocked()+size > maxContactsSnapshotBytes {
		return "", time.Time{}, false
	}
	c.snapshots[id] = &contactsSnapshot{rows: copied, bytes: size, created: created}
	return id, deadline, true
}

// snapshotFor returns the retained rows of one snapshot and its deadline. It
// reports false for an unknown, evicted, or expired snapshot so the caller
// fails closed instead of rescanning or rebasing the token onto newer data.
func (c *contactsCursorCodec) snapshotFor(id string) ([]contacts.ContactSummary, time.Time, bool) {
	if !c.available() || id == "" {
		return nil, time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	snapshot, ok := c.snapshots[id]
	if !ok {
		return nil, time.Time{}, false
	}
	deadline := snapshot.created.Add(c.lifetime())
	if !c.currentTime().Before(deadline) {
		c.deleteSnapshotLocked(id)
		return nil, time.Time{}, false
	}
	return snapshot.rows, deadline, true
}

// mint issues one token for a position inside a retained snapshot. The token
// expires with its snapshot, so a chain of pages cannot extend the snapshot
// lifetime. The payload holds only a random entry identifier and the query
// digest, so decoding a token reveals no contact value.
func (c *contactsCursorCodec) mint(query, snapshot string, position int, deadline time.Time) (string, error) {
	if !c.available() {
		return "", fmt.Errorf("contact cursor is unavailable")
	}
	if snapshot == "" || position < 0 || !c.currentTime().Before(deadline) {
		return "", fmt.Errorf("contact cursor position is invalid")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("contact cursor could not be generated")
	}
	id := hex.EncodeToString(nonce[:])
	payload := contactsCursorPayload{Exp: deadline.Unix(), Query: query, ID: id}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > contactsCursorMaxPayload {
		return "", fmt.Errorf("contact cursor payload exceeded its bound")
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	signed := contactsCursorVersion + "." + encoded
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write([]byte(signed))
	token := signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(token) > contactsCursorMaxTokenLength {
		return "", fmt.Errorf("contact cursor token exceeded its bound")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	if _, ok := c.snapshots[snapshot]; !ok {
		return "", fmt.Errorf("contact cursor snapshot is unavailable")
	}
	for len(c.entries) >= maxContactsCursorEntries {
		if !c.evictOldestEntryLocked() {
			break
		}
	}
	c.entries[id] = contactsCursorEntry{snapshot: snapshot, position: position, expires: deadline}
	return token, nil
}

// verify returns the cursor position for a token bound to the current filters
// and read scope. Every rejection happens before any backend call.
func (c *contactsCursorCodec) verify(token, wantQuery string) (contactsCursorPage, bool) {
	if !c.available() || token == "" || len(token) > contactsCursorMaxTokenLength {
		return contactsCursorPage{}, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != contactsCursorVersion {
		return contactsCursorPage{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 || len(raw) > contactsCursorMaxPayload {
		return contactsCursorPage{}, false
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(mac) != sha256.Size {
		return contactsCursorPage{}, false
	}
	expected := hmac.New(sha256.New, c.key[:])
	_, _ = expected.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(expected.Sum(nil), mac) {
		return contactsCursorPage{}, false
	}
	var payload contactsCursorPayload
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return contactsCursorPage{}, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return contactsCursorPage{}, false
	}
	if payload.Exp <= 0 || c.currentTime().Unix() >= payload.Exp || payload.Query != wantQuery {
		return contactsCursorPage{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	entry, ok := c.entries[payload.ID]
	if !ok {
		return contactsCursorPage{}, false
	}
	if !c.currentTime().Before(entry.expires) {
		delete(c.entries, payload.ID)
		return contactsCursorPage{}, false
	}
	snapshot, ok := c.snapshots[entry.snapshot]
	if !ok || !c.currentTime().Before(snapshot.created.Add(c.lifetime())) {
		delete(c.entries, payload.ID)
		return contactsCursorPage{}, false
	}
	if entry.position > len(snapshot.rows) {
		delete(c.entries, payload.ID)
		return contactsCursorPage{}, false
	}
	return contactsCursorPage{snapshot: entry.snapshot, position: entry.position}, true
}

func (c *contactsCursorCodec) pruneLocked() {
	if c.entries == nil {
		c.entries = make(map[string]contactsCursorEntry)
	}
	if c.snapshots == nil {
		c.snapshots = make(map[string]*contactsSnapshot)
	}
	now := c.currentTime()
	for id, snapshot := range c.snapshots {
		if !now.Before(snapshot.created.Add(c.lifetime())) {
			delete(c.snapshots, id)
		}
	}
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, key)
			continue
		}
		if _, ok := c.snapshots[entry.snapshot]; !ok {
			delete(c.entries, key)
		}
	}
}

// evictOldestSnapshotLocked removes the oldest snapshot and every entry that
// points into it, so an evicted generation leaves no token that could silently
// advance inside newer results.
func (c *contactsCursorCodec) evictOldestSnapshotLocked() bool {
	var oldestID string
	var oldest time.Time
	first := true
	for id, snapshot := range c.snapshots {
		if first || snapshot.created.Before(oldest) {
			oldestID, oldest, first = id, snapshot.created, false
		}
	}
	if first {
		return false
	}
	c.deleteSnapshotLocked(oldestID)
	return true
}

func (c *contactsCursorCodec) deleteSnapshotLocked(id string) {
	delete(c.snapshots, id)
	for key, entry := range c.entries {
		if entry.snapshot == id {
			delete(c.entries, key)
		}
	}
}

func (c *contactsCursorCodec) evictOldestEntryLocked() bool {
	var oldestID string
	var oldest time.Time
	first := true
	for key, entry := range c.entries {
		if first || entry.expires.Before(oldest) {
			oldestID, oldest, first = key, entry.expires, false
		}
	}
	if first {
		return false
	}
	delete(c.entries, oldestID)
	return true
}

func (c *contactsCursorCodec) snapshotBytesLocked() int {
	total := 0
	for _, snapshot := range c.snapshots {
		total += snapshot.bytes
	}
	return total
}

// snapshotCount reports the retained snapshots. Tests use it to prove the
// retention cap.
func (c *contactsCursorCodec) snapshotCount() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	return len(c.snapshots)
}

// snapshotBytes reports the retained summary bytes. Tests use it to prove the
// byte cap.
func (c *contactsCursorCodec) snapshotBytes() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	return c.snapshotBytesLocked()
}

// entryCount reports the retained cursor entries. Tests use it to prove the
// entry cap.
func (c *contactsCursorCodec) entryCount() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	return len(c.entries)
}

// contactsSnapshotBytes estimates the retained size of one result set. It
// counts every contact value the snapshot keeps, so the budget tracks retained
// memory rather than a row count.
func contactsSnapshotBytes(rows []contacts.ContactSummary) int {
	total := 256
	for _, row := range rows {
		total += 256 + len(row.AddressBook) + len(row.UID) + len(row.ETag) +
			len(row.DisplayName) + len(row.Organization)
		for _, value := range row.Emails {
			total += 64 + len(value.Type) + len(value.Value)
		}
		for _, value := range row.Phones {
			total += 64 + len(value.Type) + len(value.Value)
		}
		for _, field := range row.TruncatedFields {
			total += 64 + len(field.Field)
		}
	}
	return total
}

// contactsRowsSorted verifies that a result set obeys the canonical total order
// that the service sorts with. Rows may share a sort key: the cursor position is
// a snapshot index, not a key, so equal keys cannot skip or repeat a row. A set
// that is out of order is refused, because later page boundaries would be
// meaningless.
func contactsRowsSorted(rows []contacts.ContactSummary) bool {
	for index := 1; index < len(rows); index++ {
		previous, ok := decodeContactsSortKey(encodeContactsSortKey(
			rows[index-1].DisplayName, rows[index-1].UID, rows[index-1].AddressBook))
		if !ok {
			return false
		}
		current := contactsSortKey{
			DisplayName: contacts.NormalizeSortName(rows[index].DisplayName),
			UID:         rows[index].UID,
			AddressBook: rows[index].AddressBook,
		}
		if contactsSortKeyAfter(previous.DisplayName, previous.UID, previous.AddressBook, current) {
			return false
		}
	}
	return true
}

func contactsSearchQueryDigest(addressBook, query, email, phone, scope string, includeGroups bool) string {
	encoded, _ := json.Marshal(struct {
		AddressBook   string `json:"addressBook"`
		Query         string `json:"query"`
		Email         string `json:"email"`
		Phone         string `json:"phone"`
		Scope         string `json:"scope"`
		IncludeGroups bool   `json:"includeGroups"`
	}{addressBook, query, email, phone, scope, includeGroups})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// contactsReadScopeBinding makes the effective contacts.read resources part of
// the cursor query identity. A scope change rejects old tokens before I/O.
func contactsReadScopeBinding(scopes config.ResourceScopes) string {
	values, restricted := scopes.Contacts(config.ResourceContactsRead)
	if !restricted {
		return "*"
	}
	canonical := append([]string(nil), values...)
	sort.Strings(canonical)
	return strings.Join(canonical, "\x00")
}

func encodeContactsSortKey(displayName, uid, addressBook string) string {
	encoded, _ := json.Marshal(contactsSortKey{
		DisplayName: contacts.NormalizeSortName(displayName), UID: uid, AddressBook: addressBook,
	})
	return string(encoded)
}

type contactsSortKey struct {
	DisplayName string `json:"d"`
	UID         string `json:"u"`
	AddressBook string `json:"b"`
}

func decodeContactsSortKey(position string) (contactsSortKey, bool) {
	var key contactsSortKey
	if err := json.Unmarshal([]byte(position), &key); err != nil {
		return contactsSortKey{}, false
	}
	return key, true
}

// contactsSortKeyAfter reports the stable total order used by
// searchScopedContacts: normalized display name, UID, then address book.
func contactsSortKeyAfter(displayName, uid, addressBook string, position contactsSortKey) bool {
	name := contacts.NormalizeSortName(displayName)
	if name != position.DisplayName {
		return name > position.DisplayName
	}
	if uid != position.UID {
		return uid > position.UID
	}
	return addressBook > position.AddressBook
}
