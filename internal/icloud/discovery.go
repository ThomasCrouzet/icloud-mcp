package icloud

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	extcaldav "github.com/emersion/go-webdav/caldav"

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

// discover runs the iCloud discovery sequence (2 PROPFINDs) only once per
// session (sync.Once) and fills shardBase/homeSetPath/dav. Every CRUD
// method calls discover(ctx) first; concurrent calls (stdio worker pool)
// share the same cached result.
func (c *Client) discover(ctx context.Context) error {
	c.discoverOnce.Do(func() {
		c.discoverErr = c.doDiscover(ctx)
	})
	return c.discoverErr
}

func (c *Client) doDiscover(ctx context.Context) error {
	// Step 1: current-user-principal on the main server.
	ms, err := c.propfind(ctx, c.baseURL+"/", "0", propfindPrincipalBody)
	if err != nil {
		return err
	}
	principal := principalFromMultistatus(ms)
	if principal == "" {
		return fmt.Errorf("iCloud discovery: current-user-principal not found in response")
	}

	principalURL, err := resolveRef(c.baseURL, principal)
	if err != nil {
		return fmt.Errorf("iCloud discovery: invalid principal URL: %w", err)
	}
	// MANDATORY revalidation of the principal, SYMMETRIC to the check already
	// applied to the home-set below (step 3): never blindly trust a server
	// response, even for an intermediate discovery step. Without this check,
	// a malicious current-user-principal was only caught downstream by the
	// production AllowlistTransport, never in the test paths (direct doer
	// without a hardened transport), and inconsistently with the home-set.
	pu, perr := url.Parse(principalURL)
	if perr != nil {
		return fmt.Errorf("iCloud discovery: invalid principal URL: %w", perr)
	}
	if err := c.validateDiscoveryURL(pu, "principal"); err != nil {
		return err
	}

	// Step 2: calendar-home-set on the principal.
	ms2, err := c.propfind(ctx, principalURL, "0", propfindHomeSetBody)
	if err != nil {
		return err
	}
	homeSetHref := homeSetFromMultistatus(ms2)
	if homeSetHref == "" {
		return fmt.Errorf("iCloud discovery: calendar-home-set not found in response")
	}

	// Step 3: shard resolution, MANDATORY revalidation of the host and port,
	// even though the production http.Client already has its own allowlist
	// (defense in depth: never blindly trust a server response to decide
	// where future requests will go).
	u, err := url.Parse(homeSetHref)
	if err != nil {
		return fmt.Errorf("iCloud discovery: invalid home-set: %w", err)
	}
	switch {
	case u.IsAbs():
		if err := c.validateDiscoveryURL(u, "home-set"); err != nil {
			return err
		}
		c.shardBase = u.Scheme + "://" + u.Host
		c.homeSetPath = u.Path
	default:
		// Relative href: test server or CalDAV server without shards.
		c.shardBase = c.baseURL
		c.homeSetPath = u.Path
	}

	dav, err := extcaldav.NewClient(c.http, c.shardBase)
	if err != nil {
		return fmt.Errorf("iCloud discovery: creating CalDAV client (shard=%s): %w", c.shardBase, err)
	}
	c.dav = dav
	return nil
}

// propfind executes a PROPFIND request and parses the 207 Multi-Status response.
func (c *Client) propfind(ctx context.Context, target, depth, body string) (*msMultistatus, error) {
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", target, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building PROPFIND request (%s): %w", target, err)
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("Depth", depth)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PROPFIND request to %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("iCloud authentication refused: check ICLOUD_EMAIL and the app-specific password")
	}
	if resp.StatusCode != 207 {
		return nil, fmt.Errorf("PROPFIND %s: unexpected HTTP status %d", target, resp.StatusCode)
	}

	// Defensive bound: an abnormally large response (buggy or hostile
	// server) must never be fully loaded into memory.
	// maxPropfindBodySize+1 makes overflow detectable (if exactly the limit
	// were read, there would be no way to know whether more data followed).
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPropfindBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("reading PROPFIND response (%s): %w", target, err)
	}
	if len(data) > maxPropfindBodySize {
		return nil, fmt.Errorf("PROPFIND response (%s) too large (> %d bytes)", target, maxPropfindBodySize)
	}
	var ms msMultistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("parsing PROPFIND response (%s): %w", target, err)
	}
	return &ms, nil
}

// validateDiscoveryURL enforces scheme https, allowHost, and (for production
// iCloud hostnames only) port empty-or-443. httptest fixtures bind random
// ports on non-iCloud hosts, so port is enforced only when the host is a
// real caldav.icloud.com / pXX-caldav.icloud.com name.
func (c *Client) validateDiscoveryURL(u *url.URL, kind string) error {
	if u.Scheme != "https" || !c.allowHost(u.Hostname()) {
		return fmt.Errorf("iCloud discovery: %s outside allowlist (%s)", kind, u.Hostname())
	}
	if security.IsICloudHost(u.Hostname()) && !security.PortAllowed(u.Port()) {
		return fmt.Errorf("iCloud discovery: %s port %q rejected (443 only)", kind, u.Port())
	}
	return nil
}

// resolveRef resolves a reference (absolute or relative) against base.
func resolveRef(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}
