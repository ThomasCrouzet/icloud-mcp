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

// shardHostRe matches the shards returned by iCloud discovery
// (e.g. p46-caldav.icloud.com, p123-caldav.icloud.com).
var shardHostRe = regexp.MustCompile(`^p\d{1,3}-caldav\.icloud\.com$`)

// IsICloudHost allows caldav.icloud.com and pXX-caldav.icloud.com, nothing
// else. The comparison is case-sensitive: url.URL.Hostname() returns the
// host as it appears in the URL (not lowercased by Go), so an uppercase
// variant is deliberately rejected rather than treated as equivalent.
func IsICloudHost(host string) bool {
	return host == "caldav.icloud.com" || shardHostRe.MatchString(host)
}

// portAllowed accepts an empty port (implicit 443) or an explicit "443".
// iCloud returns the shard URL with an EXPLICIT 443 port (e.g.
// p120-caldav.icloud.com:443); rejecting it broke every tool call after
// discovery. Any other port is still refused (never legitimate for iCloud,
// possible bypass signal).
func portAllowed(port string) bool {
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
	if !portAllowed(req.URL.Port()) {
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
// pool. An additional CheckRedirect revalidates the host on every redirect
// hop (defense in depth, redundant with the RoundTripper but at no cost).
func NewICloudHTTPClient(timeout time.Duration) *http.Client {
	inner := &http.Transport{
		MaxIdleConns:    10,
		MaxConnsPerHost: 10,
		IdleConnTimeout: 30 * time.Second,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	transport := NewAllowlistTransport(inner, IsICloudHost)
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != "https" || !IsICloudHost(req.URL.Hostname()) || !portAllowed(req.URL.Port()) {
				return fmt.Errorf("network allowlist: redirect to %q rejected", req.URL.Host)
			}
			return nil
		},
	}
}
