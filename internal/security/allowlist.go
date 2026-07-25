// Package security groups the cross-cutting security mechanisms of the
// icloud-mcp server: network allowlist, secret redaction, mutation audit.
// None of these mechanisms depend on iCloud or MCP; they are reusable leaf
// components, testable in isolation.
package security

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// ICloudBaseURL is the ONLY network base allowed for this binary at startup.
// Discovery shards (pXX-caldav.icloud.com) are allowed separately by
// IsICloudHost, but always revalidated (a server response is never trusted
// blindly).
const ICloudBaseURL = "https://caldav.icloud.com"

// ContactsBaseURL is the fixed CardDAV discovery entry point. Contacts uses a
// separate client and host predicate so Calendar credentials cannot cross into
// the Contacts transport, or vice versa.
const ContactsBaseURL = "https://contacts.icloud.com"

// shardHostRe matches the shards returned by iCloud discovery
// (e.g. p46-caldav.icloud.com, p123-caldav.icloud.com).
var shardHostRe = regexp.MustCompile(`^p\d{1,3}-caldav\.icloud\.com$`)

var contactsShardHostRe = regexp.MustCompile(`^p\d{1,3}-contacts\.icloud\.com$`)

// IsICloudHost allows caldav.icloud.com and pXX-caldav.icloud.com, nothing
// else. The comparison is case-sensitive: url.URL.Hostname() returns the
// host as it appears in the URL (not lowercased by Go), so an uppercase
// variant is deliberately rejected rather than treated as equivalent.
func IsICloudHost(host string) bool {
	return host == "caldav.icloud.com" || shardHostRe.MatchString(host)
}

// IsContactsHost allows contacts.icloud.com and its one-to-three-digit iCloud
// Contacts shards, nothing else. Host matching is deliberately case-sensitive.
func IsContactsHost(host string) bool {
	return host == "contacts.icloud.com" || contactsShardHostRe.MatchString(host)
}

// PortAllowed accepts an empty port (implicit 443) or an explicit "443".
// iCloud returns the shard URL with an EXPLICIT 443 port (e.g.
// p120-caldav.icloud.com:443); rejecting it broke every tool call after
// discovery. Any other port is still refused (never legitimate for iCloud,
// possible bypass signal). Exported so discovery can revalidate shard URLs
// with the same rule as the HTTP transport.
func PortAllowed(port string) bool {
	return port == "" || port == "443"
}

// AllowlistTransport is an http.RoundTripper that REJECTS any request whose
// scheme is not https or whose host is not authorized by `allowed`.
// A RoundTripper (rather than a DialContext) intercepts the request before
// any DNS resolution, and also covers every redirect hop: the stdlib
// http.Client sends each redirected request through the same Transport.
type AllowlistTransport struct {
	inner   http.RoundTripper
	allowed func(host string) bool
}

// NewAllowlistTransport builds an AllowlistTransport. `inner` actually
// transports the authorized requests; `allowed` decides on the host.
func NewAllowlistTransport(inner http.RoundTripper, allowed func(string) bool) *AllowlistTransport {
	return &AllowlistTransport{inner: inner, allowed: allowed}
}

// RoundTrip implements http.RoundTripper.
func (t *AllowlistTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("network allowlist: scheme %q rejected (https only)", req.URL.Scheme)
	}
	// Only the implicit port (empty) and 443 are accepted: iCloud returns the
	// shard with an explicit 443 port (e.g. p120-caldav.icloud.com:443). Any
	// other port is never legitimate for iCloud and could signal a bypass
	// attempt (third-party service on an otherwise authorized host).
	// Rejected as defense in depth.
	if !PortAllowed(req.URL.Port()) {
		return nil, fmt.Errorf("network allowlist: port %q rejected (443 only)", req.URL.Port())
	}
	host := req.URL.Hostname()
	if !t.allowed(host) {
		return nil, fmt.Errorf("network allowlist: host %q rejected", host)
	}
	return t.inner.RoundTrip(req)
}

// NewICloudHTTPClient returns the production HTTP client: IsICloudHost
// allowlist, verified TLS (default config, MinVersion TLS1.2, TLS
// verification always required), bounded timeout, reasonable connection
// pool. Automatic redirects are disabled so the Calendar DAV implementation
// can preserve methods and conditional headers while validating every hop.
func NewICloudHTTPClient(timeout time.Duration) *http.Client {
	transport := newDAVTransport(IsICloudHost)
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// NewContactsHTTPClient returns a Contacts-only HTTP client with a transport
// independent from every Calendar client. Automatic redirects are disabled so
// the CardDAV implementation can preserve methods and revalidate each hop.
func NewContactsHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: newDAVTransport(IsContactsHost),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newDAVTransport(allowed func(string) bool) *AllowlistTransport {
	inner := &http.Transport{
		// Explicit nil: never honor HTTP(S)_PROXY. The allowlist is the only
		// egress path; an env proxy would surprise operators and widen the
		// trust boundary to a local MITM.
		Proxy:           nil,
		MaxIdleConns:    10,
		MaxConnsPerHost: 10,
		IdleConnTimeout: 30 * time.Second,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return NewAllowlistTransport(inner, allowed)
}
