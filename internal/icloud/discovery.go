package icloud

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// maxPropfindBodySize bounds how much of a PROPFIND response is read,
// defense in depth against a buggy or hostile server that would return a
// pathologically large response body (an unbounded io.ReadAll would load
// everything into memory before even attempting the XML parsing).
const maxPropfindBodySize = 8 << 20 // 8 MiB

// propfindPrincipalBody requests current-user-principal on the main iCloud
// server (discovery step 1).
const propfindPrincipalBody = `<?xml version="1.0" encoding="UTF-8"?>
<A:propfind xmlns:A="DAV:">
  <A:prop>
    <A:current-user-principal/>
  </A:prop>
</A:propfind>`

// propfindHomeSetBody requests calendar-home-set on the principal
// (discovery step 2); the response contains the absolute URL of the shard.
const propfindHomeSetBody = `<?xml version="1.0" encoding="UTF-8"?>
<A:propfind xmlns:A="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <A:prop>
    <C:calendar-home-set/>
  </A:prop>
</A:propfind>`

type discoveryResult struct {
	shardBase   string
	homeSetPath string
}

type propfindResult struct {
	multistatus *msMultistatus
	url         *url.URL
}

// discover runs the iCloud discovery sequence and publishes a complete result
// atomically. Concurrent callers wait on a channel so their own contexts can
// cancel without waiting for the in-flight caller's network deadline.
func (c *Client) discover(ctx context.Context) error {
	for {
		c.discoverMu.Lock()
		if c.discovered {
			c.discoverMu.Unlock()
			return nil
		}
		if c.discovering {
			wait := c.discoverWait
			c.discoverMu.Unlock()
			select {
			case <-ctx.Done():
				return calendarContextError(ctx.Err())
			case <-wait:
				continue
			}
		}
		c.discovering = true
		c.discoverWait = make(chan struct{})
		wait := c.discoverWait
		c.discoverMu.Unlock()

		result, err := c.doDiscover(ctx)
		c.discoverMu.Lock()
		if err == nil {
			c.shardBase = result.shardBase
			c.homeSetPath = result.homeSetPath
			c.discovered = true
		}
		c.discovering = false
		close(wait)
		c.discoverMu.Unlock()
		return err
	}
}

func (c *Client) doDiscover(ctx context.Context) (discoveryResult, error) {
	// Step 1: current-user-principal on the main server.
	response, err := c.propfind(ctx, c.baseURL+"/", "0", propfindPrincipalBody)
	if err != nil {
		return discoveryResult{}, err
	}
	principal := principalFromMultistatus(response.multistatus)
	if principal == "" {
		return discoveryResult{}, fmt.Errorf("iCloud discovery: current-user-principal not found in response")
	}

	principalURL, err := c.resolveDiscoveryHref(response.url, principal, "principal")
	if err != nil {
		return discoveryResult{}, err
	}

	// Step 2: calendar-home-set on the principal.
	response2, err := c.propfind(ctx, principalURL.String(), "0", propfindHomeSetBody)
	if err != nil {
		return discoveryResult{}, err
	}
	homeSetHref := homeSetFromMultistatus(response2.multistatus)
	if homeSetHref == "" {
		return discoveryResult{}, fmt.Errorf("iCloud discovery: calendar-home-set not found in response")
	}

	// Step 3: resolve against the exact final principal response URL. The
	// home-set may legitimately move to an allowlisted iCloud shard.
	homeSetURL, err := c.resolveDiscoveryHref(response2.url, homeSetHref, "home-set")
	if err != nil {
		return discoveryResult{}, err
	}
	return discoveryResult{shardBase: davOrigin(homeSetURL), homeSetPath: homeSetURL.EscapedPath()}, nil
}

// propfind executes a PROPFIND request and parses the 207 Multi-Status response.
func (c *Client) propfind(ctx context.Context, target, depth, body string, scopePath ...string) (*propfindResult, error) {
	scope := ""
	if len(scopePath) > 0 {
		scope = scopePath[0]
	}
	headers := make(http.Header)
	headers.Set("Content-Type", `text/xml; charset="utf-8"`)
	headers.Set("Depth", depth)
	result, err := c.doCalendarRequest(ctx, "PROPFIND", target, headers, []byte(body), scope)
	if err != nil {
		return nil, fmt.Errorf("Calendar PROPFIND request failed: %w", err)
	}
	resp := result.response
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 207 {
		return nil, classifyStatus(resp.StatusCode)
	}

	// Defensive bound: an abnormally large response (buggy or hostile
	// server) must never be fully loaded into memory.
	// maxPropfindBodySize+1 makes overflow detectable (if exactly the limit
	// were read, there would be no way to know whether more data followed).
	data, err := readBoundedCalendarBody(resp.Body, maxPropfindBodySize, "Calendar PROPFIND response is too large")
	if err != nil {
		return nil, err
	}
	if err := validatePropfindMultiStatus(data, resp.StatusCode); err != nil {
		return nil, err
	}
	var ms msMultistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, NewError(CodeProtocolError, resp.StatusCode, "Calendar PROPFIND response is malformed", nil)
	}
	return &propfindResult{multistatus: &ms, url: result.url}, nil
}

// validateDiscoveryURL enforces scheme https, allowHost, and (for production
// iCloud hostnames only) port empty-or-443. httptest fixtures bind random
// ports on non-iCloud hosts, so port is enforced only when the host is a
// real caldav.icloud.com / pXX-caldav.icloud.com name.
func (c *Client) validateDiscoveryURL(u *url.URL, kind string) error {
	if u == nil || u.Scheme != "https" || u.Host == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || c.allowHost == nil || !c.allowHost(u.Hostname()) {
		host := ""
		if u != nil {
			host = u.Hostname()
		}
		return fmt.Errorf("iCloud discovery: %s outside allowlist (%s)", kind, host)
	}
	if security.IsICloudHost(u.Hostname()) && !security.PortAllowed(u.Port()) {
		return fmt.Errorf("iCloud discovery: %s port %q rejected (443 only)", kind, u.Port())
	}
	if err := ValidateCalendarPath(u.EscapedPath()); err != nil {
		return NewError(CodeProtocolError, 0, "iCloud discovery returned an invalid "+kind+" path", nil)
	}
	return nil
}

func (c *Client) resolveDiscoveryHref(base *url.URL, href, kind string) (*url.URL, error) {
	if base == nil || href == "" || len(href) > 4096 {
		return nil, NewError(CodeProtocolError, 0, "iCloud discovery returned an invalid "+kind+" href", nil)
	}
	ref, err := url.Parse(href)
	if err != nil || ref.Opaque != "" || ref.User != nil || ref.RawQuery != "" || ref.Fragment != "" || ref.ForceQuery || strings.ContainsAny(href, "?#") {
		return nil, NewError(CodeProtocolError, 0, "iCloud discovery returned an invalid "+kind+" href", nil)
	}
	resolved := base.ResolveReference(ref)
	if err := c.validateDiscoveryURL(resolved, kind); err != nil {
		return nil, err
	}
	return resolved, nil
}

// resolvePathOnBase resolves a path-absolute ref against base and requires
// the result to stay on base's host and port. Defense in depth if a path
// ever bypasses ValidateCalendarPath (scheme-relative //host rewrite).
func resolvePathOnBase(base, path string) (string, error) {
	if err := ValidateCalendarPath(path); err != nil {
		return "", err
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if b.Scheme != "https" || b.Host == "" || b.Opaque != "" || b.User != nil || b.RawQuery != "" || b.Fragment != "" {
		return "", fmt.Errorf("invalid Calendar base URL")
	}
	r, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	// Force path-only resolution: clear any accidental authority on ref.
	if r.Scheme != "" || r.Host != "" || r.User != nil || r.RawQuery != "" || r.Fragment != "" || r.Opaque != "" {
		return "", fmt.Errorf("calendar path must not include a host or scheme")
	}
	out := b.ResolveReference(r)
	if !sameDAVOrigin(out, b) {
		return "", fmt.Errorf("calendar path resolves outside base host")
	}
	return out.String(), nil
}
