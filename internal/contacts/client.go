package contacts

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/time/rate"
)

// Client implements Service using only hand-rolled bounded CardDAV requests.
type Client struct {
	http      HTTPDoer
	baseURL   string
	allowHost func(string) bool

	discoverMu  sync.Mutex
	discovering chan struct{}
	state       *discoveryState

	readLimit  *rate.Limiter
	writeLimit *rate.Limiter
	semaphore  chan struct{}
}

// RateLimitStatus reports the live Contacts limiter state for health probes.
func (c *Client) RateLimitStatus() map[string]any {
	if c == nil {
		return nil
	}
	return map[string]any{
		"read":  map[string]any{"tokens": c.readLimit.Tokens(), "limit": float64(c.readLimit.Limit()), "burst": c.readLimit.Burst()},
		"write": map[string]any{"tokens": c.writeLimit.Tokens(), "limit": float64(c.writeLimit.Limit()), "burst": c.writeLimit.Burst()},
	}
}

type discoveryState struct {
	books []bookRecord
	byID  map[string]bookRecord
	homes []*url.URL
}

type bookRecord struct {
	public AddressBook
	url    *url.URL
}

var _ Service = (*Client)(nil)

// NewClient creates a lazy Contacts client. The injected doer must apply
// authentication and transport-level policy. When it is an *http.Client, it
// is cloned with automatic redirects disabled.
func NewClient(doer HTTPDoer, baseURL string, allowHost func(string) bool) *Client {
	readLimit, writeLimit := newLimiters()
	return &Client{
		http:       noRedirectDoer(doer),
		baseURL:    strings.TrimSpace(baseURL),
		allowHost:  allowHost,
		readLimit:  readLimit,
		writeLimit: writeLimit,
		semaphore:  make(chan struct{}, 4),
	}
}

// Discover forces lazy discovery. Only a complete validated success is cached.
func (c *Client) Discover(ctx context.Context) error {
	return c.discover(ctx)
}

func (c *Client) validateURL(target *url.URL) error {
	if target == nil || target.Scheme != "https" || target.Host == "" || target.Opaque != "" || target.User != nil || target.Fragment != "" || target.RawQuery != "" || target.ForceQuery {
		return newError(CodeProtocolError, 0, "Contacts URL failed scheme or authority validation")
	}
	host := target.Hostname()
	if host == "" || host != strings.ToLower(host) {
		return newError(CodeProtocolError, 0, "Contacts URL host must be lowercase")
	}
	if target.Port() != "" && target.Port() != "443" {
		return newError(CodeProtocolError, 0, "Contacts URL uses a disallowed port")
	}
	if c.allowHost == nil || !c.allowHost(host) {
		return newError(CodeProtocolError, 0, "Contacts URL host is outside the domain allowlist")
	}
	if _, ok := canonicalOpaquePath(target); !ok {
		return newError(CodeProtocolError, 0, "Contacts URL path is invalid")
	}
	return nil
}

func (c *Client) book(ctx context.Context, id string) (bookRecord, error) {
	if err := validateAddressBookID(id); err != nil {
		return bookRecord{}, err
	}
	if err := c.discover(ctx); err != nil {
		return bookRecord{}, err
	}
	record, ok := c.state.byID[id]
	if !ok {
		return bookRecord{}, validationError("address_book is not a discovered address book identifier")
	}
	return record, nil
}

func canonicalURL(target *url.URL) string {
	host := target.Hostname()
	authority := host
	if target.Port() != "" && target.Port() != "443" {
		authority += ":" + target.Port()
	}
	escapedPath, _ := canonicalOpaquePath(target)
	return "https://" + authority + escapedPath
}

func urlWithin(child, collection *url.URL, allowEqual bool) bool {
	if child == nil || collection == nil || child.Scheme != collection.Scheme || child.Hostname() != collection.Hostname() {
		return false
	}
	childPort := child.Port()
	if childPort == "" {
		childPort = "443"
	}
	collectionPort := collection.Port()
	if collectionPort == "" {
		collectionPort = "443"
	}
	if childPort != collectionPort {
		return false
	}
	basePath, baseOK := canonicalOpaquePath(collection)
	childPath, childOK := canonicalOpaquePath(child)
	if !baseOK || !childOK {
		return false
	}
	basePath = strings.TrimSuffix(basePath, "/")
	childPath = strings.TrimSuffix(childPath, "/")
	if allowEqual && childPath == basePath {
		return true
	}
	return basePath != "" && strings.HasPrefix(childPath, basePath+"/")
}

// canonicalOpaquePath validates path syntax while preserving escaped segments
// as opaque routing identifiers. It normalizes only percent-escape hex case;
// it never decodes paths for containment comparisons.
func canonicalOpaquePath(target *url.URL) (string, bool) {
	if target == nil {
		return "", false
	}
	escaped := target.EscapedPath()
	if escaped == "" || len(escaped) > 4096 || !strings.HasPrefix(escaped, "/") || strings.HasPrefix(escaped, "//") {
		return "", false
	}
	canonical := []byte(escaped)
	for index := 0; index < len(canonical); index++ {
		switch canonical[index] {
		case '%':
			if index+2 >= len(canonical) || !isHexByte(canonical[index+1]) || !isHexByte(canonical[index+2]) {
				return "", false
			}
			canonical[index+1] = upperHexByte(canonical[index+1])
			canonical[index+2] = upperHexByte(canonical[index+2])
			index += 2
		case '\\', '\x00', '\r', '\n':
			return "", false
		}
	}
	for _, segment := range strings.Split(string(canonical), "/") {
		if unsafeOpaquePathSegment(segment) {
			return "", false
		}
	}
	return string(canonical), true
}

func unsafeOpaquePathSegment(segment string) bool {
	decoded := segment
	for range len(segment) + 1 {
		if decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\\x00\r\n") {
			return true
		}
		next, changed := decodePercentEscapes(decoded)
		if !changed {
			return false
		}
		decoded = next
	}
	return true
}

func decodePercentEscapes(value string) (string, bool) {
	var decoded strings.Builder
	decoded.Grow(len(value))
	changed := false
	for index := 0; index < len(value); index++ {
		if value[index] == '%' && index+2 < len(value) && isHexByte(value[index+1]) && isHexByte(value[index+2]) {
			decoded.WriteByte(hexValue(value[index+1])<<4 | hexValue(value[index+2]))
			index += 2
			changed = true
			continue
		}
		decoded.WriteByte(value[index])
	}
	return decoded.String(), changed
}

func isHexByte(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func upperHexByte(value byte) byte {
	if value >= 'a' && value <= 'f' {
		return value - ('a' - 'A')
	}
	return value
}

func hexValue(value byte) byte {
	if value >= '0' && value <= '9' {
		return value - '0'
	}
	return upperHexByte(value) - 'A' + 10
}

func opaqueBookID(target *url.URL) string {
	sum := sha256.Sum256([]byte(canonicalURL(target)))
	return "book-" + base64.RawURLEncoding.EncodeToString(sum[:16])
}

func truncateUTF8(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func resultFits(value any) bool {
	data, err := json.Marshal(value)
	return err == nil && len(data) <= maxResultBytes
}

func sortedVersions(types []addressDataType) []string {
	seen := make(map[string]bool)
	for _, dataType := range types {
		if strings.EqualFold(dataType.ContentType, "text/vcard") && (dataType.Version == "3.0" || dataType.Version == "4.0") {
			seen[dataType.Version] = true
		}
	}
	versions := make([]string, 0, len(seen))
	for version := range seen {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	return versions
}
