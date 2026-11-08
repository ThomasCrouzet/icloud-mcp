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
	"strings"
	"time"
)

const (
	contactsCursorVersion        = "c1"
	contactsCursorTTL            = 300 * time.Second
	contactsCursorMaxTokenLength = 4096
	contactsCursorMaxPayload     = 1536
)

type contactsSortKey struct {
	DisplayName string `json:"d"`
	UID         string `json:"u"`
	AddressBook string `json:"b"`
}

type contactsCursorPayload struct {
	Exp      int64           `json:"e"`
	Query    string          `json:"q"`
	Position contactsSortKey `json:"p"`
}

type contactsCursorCodec struct {
	key [sha256.Size]byte
	now func() time.Time
	ttl time.Duration
	err error
}

func newContactsCursorCodec() *contactsCursorCodec {
	codec := &contactsCursorCodec{now: time.Now, ttl: contactsCursorTTL}
	if _, err := rand.Read(codec.key[:]); err != nil {
		codec.err = err
	}
	return codec
}

func (c *contactsCursorCodec) available() bool {
	return c != nil && c.err == nil
}

func (c *contactsCursorCodec) mint(query, position string) (string, error) {
	if !c.available() {
		return "", fmt.Errorf("contact cursor is unavailable")
	}
	var key contactsSortKey
	if err := json.Unmarshal([]byte(position), &key); err != nil {
		return "", fmt.Errorf("contact cursor position is invalid")
	}
	payload := contactsCursorPayload{
		Exp:      c.now().Add(c.ttl).Unix(),
		Query:    query,
		Position: key,
	}
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
	return token, nil
}

func (c *contactsCursorCodec) verify(token, wantQuery string) (contactsSortKey, bool, error) {
	var position contactsSortKey
	if !c.available() {
		return position, false, nil
	}
	if token == "" || len(token) > contactsCursorMaxTokenLength {
		return position, false, nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != contactsCursorVersion {
		return position, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 || len(raw) > contactsCursorMaxPayload {
		return position, false, nil
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(mac) != sha256.Size {
		return position, false, nil
	}
	expected := hmac.New(sha256.New, c.key[:])
	_, _ = expected.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(expected.Sum(nil), mac) {
		return position, false, nil
	}
	var payload contactsCursorPayload
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return position, false, nil
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return position, false, nil
	}
	if payload.Exp <= 0 || c.now().Unix() >= payload.Exp || payload.Query != wantQuery {
		return position, false, nil
	}
	return payload.Position, true, nil
}

func contactsSearchQueryDigest(addressBook, query, email, phone string, includeGroups bool) string {
	encoded, _ := json.Marshal(struct {
		AddressBook   string `json:"addressBook"`
		Query         string `json:"query"`
		Email         string `json:"email"`
		Phone         string `json:"phone"`
		IncludeGroups bool   `json:"includeGroups"`
	}{addressBook, query, email, phone, includeGroups})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func encodeContactsSortKey(displayName, uid, addressBook string) string {
	encoded, _ := json.Marshal(contactsSortKey{
		DisplayName: strings.ToLower(displayName), UID: uid, AddressBook: addressBook,
	})
	return string(encoded)
}

var defaultContactsCursor = newContactsCursorCodec()

// contactsSortKeyAfter reports the stable total order used by
// searchScopedContacts: normalized display name, UID, then address book.
func contactsSortKeyAfter(displayName, uid, addressBook string, position contactsSortKey) bool {
	name := strings.ToLower(displayName)
	if name != position.DisplayName {
		return name > position.DisplayName
	}
	if uid != position.UID {
		return uid > position.UID
	}
	return addressBook > position.AddressBook
}
