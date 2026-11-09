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
	contactsCursorVersion        = "c2"
	contactsCursorTTL            = 300 * time.Second
	contactsCursorMaxTokenLength = 4096
	contactsCursorMaxPayload     = 512
	maxContactsCursorEntries     = 128
)

type contactsCursorPayload struct {
	Exp   int64  `json:"e"`
	Query string `json:"q"`
	ID    string `json:"i"`
}

type contactsCursorEntry struct {
	position string
	expires  time.Time
}

type contactsCursorCodec struct {
	key [sha256.Size]byte
	now func() time.Time
	ttl time.Duration
	err error

	mu      sync.Mutex
	entries map[string]contactsCursorEntry
}

func newContactsCursorCodec() *contactsCursorCodec {
	codec := &contactsCursorCodec{
		now:     time.Now,
		ttl:     contactsCursorTTL,
		entries: make(map[string]contactsCursorEntry),
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

func (c *contactsCursorCodec) mint(query, position string) (string, error) {
	if !c.available() {
		return "", fmt.Errorf("contact cursor is unavailable")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("contact cursor could not be generated")
	}
	id := hex.EncodeToString(nonce[:])
	expires := c.now().Add(c.ttl)
	payload := contactsCursorPayload{Exp: expires.Unix(), Query: query, ID: id}
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
	for len(c.entries) >= maxContactsCursorEntries {
		var oldestID string
		var oldest time.Time
		first := true
		for key, entry := range c.entries {
			if first || entry.expires.Before(oldest) {
				oldestID, oldest, first = key, entry.expires, false
			}
		}
		delete(c.entries, oldestID)
	}
	c.entries[id] = contactsCursorEntry{position: position, expires: expires}
	return token, nil
}

func (c *contactsCursorCodec) verify(token, wantQuery string) (string, bool) {
	if !c.available() || token == "" || len(token) > contactsCursorMaxTokenLength {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != contactsCursorVersion {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 || len(raw) > contactsCursorMaxPayload {
		return "", false
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(mac) != sha256.Size {
		return "", false
	}
	expected := hmac.New(sha256.New, c.key[:])
	_, _ = expected.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(expected.Sum(nil), mac) {
		return "", false
	}
	var payload contactsCursorPayload
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return "", false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", false
	}
	if payload.Exp <= 0 || c.now().Unix() >= payload.Exp || payload.Query != wantQuery {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	entry, ok := c.entries[payload.ID]
	if !ok || c.now().After(entry.expires) {
		delete(c.entries, payload.ID)
		return "", false
	}
	return entry.position, true
}

func (c *contactsCursorCodec) pruneLocked() {
	now := c.now()
	for key, entry := range c.entries {
		if now.After(entry.expires) {
			delete(c.entries, key)
		}
	}
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
