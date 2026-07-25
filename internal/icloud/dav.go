package icloud

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/emersion/go-ical"
	extcaldav "github.com/emersion/go-webdav/caldav"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const (
	maxCalendarRedirects = 3
	maxGetBodySize       = 8 << 20 // 8 MiB per calendar object
	maxETagBytes         = 512
)

type calendarHTTPResponse struct {
	response *http.Response
	url      *url.URL
}

// doCalendarRequest owns Calendar DAV redirects. Every hop is rebuilt from
// the original method, body, and headers, then revalidated before dispatch.
// scopePath is empty during discovery; after discovery it confines redirects
// to the selected calendar collection (or home-set for list operations).
func (c *Client) doCalendarRequest(ctx context.Context, method, target string, headers http.Header, body []byte, scopePath string) (*calendarHTTPResponse, error) {
	if c.http == nil {
		return nil, NewError(CodeInternal, 0, "Calendar HTTP client is not configured", nil)
	}
	current, err := url.Parse(target)
	if err != nil {
		return nil, NewError(CodeProtocolError, 0, "Calendar DAV target is invalid", nil)
	}

	redirects := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, calendarContextError(err)
		}
		if err := c.validateCalendarRequestURL(current, scopePath); err != nil {
			return nil, err
		}

		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, current.String(), reader)
		if err != nil {
			return nil, NewError(CodeInternal, 0, "failed to build a Calendar DAV request", nil)
		}
		req.Header = headers.Clone()

		resp, err := c.http.Do(req)
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if isCalendarMutationMethod(method) && AsICloudError(err) == nil {
				return nil, outcomeUnknownError(0)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, calendarContextError(err)
			}
			return nil, err
		}
		if resp == nil {
			if isCalendarMutationMethod(method) {
				return nil, outcomeUnknownError(0)
			}
			return nil, NewError(CodeProtocolError, 0, "Calendar HTTP client returned no response", nil)
		}
		if resp.Body == nil {
			resp.Body = http.NoBody
		}

		// A wrapped http.Client must not silently follow a redirect before this
		// policy sees it. This also prevents a redirected GET from being mistaken
		// for a successful PUT or DELETE.
		responseURL := current
		if resp.Request != nil {
			if resp.Request.URL == nil || resp.Request.URL.String() != current.String() || resp.Request.Method != method {
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
				if isCalendarMutationMethod(method) {
					return nil, outcomeUnknownError(0)
				}
				return nil, NewError(CodeProtocolError, 0, "the Calendar HTTP client followed an automatic redirect", nil)
			}
			responseURL = resp.Request.URL
		}
		if isCalendarMutationMethod(method) {
			switch resp.StatusCode {
			case http.StatusTooManyRequests:
				_ = resp.Body.Close()
				return nil, classifyStatus(resp.StatusCode)
			case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				_ = resp.Body.Close()
				return nil, outcomeUnknownError(resp.StatusCode)
			}
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				_ = resp.Body.Close()
				return nil, outcomeUnknownError(resp.StatusCode)
			}
		}

		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			location := resp.Header.Get("Location")
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			if redirects >= maxCalendarRedirects || location == "" || len(location) > 4096 {
				return nil, NewError(CodeProtocolError, resp.StatusCode, "Calendar redirect limit exceeded or Location was invalid", nil)
			}
			next, err := url.Parse(location)
			if err != nil {
				return nil, NewError(CodeProtocolError, resp.StatusCode, "Calendar redirect Location was invalid", nil)
			}
			resolved := current.ResolveReference(next)
			if err := c.validateCalendarRequestURL(resolved, scopePath); err != nil {
				return nil, err
			}
			current = resolved
			redirects++
			continue
		case http.StatusSeeOther:
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			return nil, NewError(CodeProtocolError, resp.StatusCode, "Calendar DAV does not allow 303 redirects", nil)
		default:
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
				return nil, NewError(CodeProtocolError, resp.StatusCode, "Calendar DAV received an unsupported redirect status", nil)
			}
			return &calendarHTTPResponse{response: resp, url: responseURL}, nil
		}
	}
}

func (c *Client) validateCalendarRequestURL(target *url.URL, scopePath string) error {
	if target == nil || target.Scheme != "https" || target.Host == "" || target.Opaque != "" || target.User != nil || target.Fragment != "" || target.RawQuery != "" || target.ForceQuery {
		return NewError(CodeProtocolError, 0, "Calendar DAV URL failed scheme or authority validation", nil)
	}
	host := target.Hostname()
	if c.allowHost == nil || !c.allowHost(host) {
		return NewError(CodeProtocolError, 0, "Calendar DAV URL is outside the domain allowlist", nil)
	}
	if security.IsICloudHost(host) && !security.PortAllowed(target.Port()) {
		return NewError(CodeProtocolError, 0, "Calendar DAV URL uses a disallowed port", nil)
	}
	escapedPath := target.EscapedPath()
	if err := ValidateCalendarPath(escapedPath); err != nil {
		return NewError(CodeProtocolError, 0, "Calendar DAV URL path is invalid", nil)
	}
	if scopePath == "" {
		return nil
	}

	shard, err := url.Parse(c.shardBase)
	if err != nil || !sameDAVOrigin(target, shard) {
		return NewError(CodeProtocolError, 0, "Calendar DAV request left the discovered shard", nil)
	}
	if !pathUnderHomeSet(escapedPath, scopePath) {
		return NewError(CodeProtocolError, 0, "Calendar DAV request left the selected calendar collection", nil)
	}
	return nil
}

func sameDAVOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return ""
	}
	return port(a) == port(b)
}

func davOrigin(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// resolveDAVHref resolves a server-provided href against the exact URL of the
// response that carried it. It validates the absolute authority before
// returning an escaped path, preventing an absolute href from being reduced
// to a trusted-looking path before its origin is checked.
func (c *Client) resolveDAVHref(base *url.URL, href, scopePath string) (*url.URL, error) {
	if base == nil || href == "" || len(href) > 4096 {
		return nil, NewError(CodeProtocolError, 0, "Calendar DAV response contains an invalid href", nil)
	}
	ref, err := url.Parse(href)
	if err != nil || ref.Opaque != "" || ref.User != nil || ref.RawQuery != "" || ref.Fragment != "" || ref.ForceQuery || strings.ContainsAny(href, "?#") {
		return nil, NewError(CodeProtocolError, 0, "Calendar DAV response contains an invalid href", nil)
	}
	resolved := base.ResolveReference(ref)
	if !sameDAVOrigin(resolved, base) {
		return nil, NewError(CodeProtocolError, 0, "Calendar DAV href changed origin", nil)
	}
	if err := c.validateCalendarRequestURL(resolved, scopePath); err != nil {
		return nil, err
	}
	return resolved, nil
}

func readBoundedCalendarBody(body io.Reader, limit int64, message string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, NewError(CodeProtocolError, 0, "failed to read a Calendar DAV response", nil)
	}
	if int64(len(data)) > limit {
		return nil, NewError(CodePayloadTooLarge, 0, message, nil)
	}
	return data, nil
}

func decodeRemoteCalendar(reader io.Reader) (cal *ical.Calendar, err error) {
	defer func() {
		if recover() != nil {
			cal = nil
			err = NewError(CodeProtocolError, 0, "Calendar event data is malformed", nil)
		}
	}()
	data, readErr := io.ReadAll(io.LimitReader(reader, maxGetBodySize+1))
	if readErr != nil {
		return nil, NewError(CodeProtocolError, 0, "Calendar event data could not be read", nil)
	}
	if len(data) > maxGetBodySize {
		return nil, NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its byte limit", nil)
	}
	if preflightErr := validateRawICalendar(data); preflightErr != nil {
		return nil, preflightErr
	}
	cal, decodeErr := ical.NewDecoder(bytes.NewReader(data)).Decode()
	if decodeErr != nil {
		return nil, NewError(CodeProtocolError, 0, "Calendar event data is malformed", nil)
	}
	return cal, nil
}

type rawICalComponent struct {
	name       string
	properties int
}

// validateRawICalendar bounds recursive structure before go-ical decodes it.
// Folded physical lines are counted as one logical content line, matching the
// iCalendar grammar while preventing deeply nested components from reaching
// the recursive decoder.
func validateRawICalendar(data []byte) error {
	var stack []rawICalComponent
	logical := make([]byte, 0, 256)
	haveLogical := false
	logicalLines := 0
	physicalLines := 0
	components := 0
	properties := 0

	flush := func() error {
		logicalLines++
		if logicalLines > maxRemoteLogicalLines {
			return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its logical-line limit", nil)
		}
		colon := bytes.IndexByte(logical, ':')
		if colon <= 0 {
			return nil
		}
		head := logical[:colon]
		if semicolon := bytes.IndexByte(head, ';'); semicolon >= 0 {
			head = head[:semicolon]
		}
		value := logical[colon+1:]
		switch {
		case bytes.EqualFold(head, []byte("BEGIN")):
			name := string(value)
			if name == "" {
				return NewError(CodeProtocolError, 0, "Calendar event data is malformed", nil)
			}
			components++
			if components > maxRemoteComponents {
				return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its component limit", nil)
			}
			if len(stack) >= maxRemoteComponentDepth {
				return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its component-depth limit", nil)
			}
			stack = append(stack, rawICalComponent{name: name})
		case bytes.EqualFold(head, []byte("END")):
			if len(stack) == 0 || !strings.EqualFold(stack[len(stack)-1].name, string(value)) {
				return NewError(CodeProtocolError, 0, "Calendar event data is malformed", nil)
			}
			stack = stack[:len(stack)-1]
		default:
			properties++
			if properties > maxRemoteProperties {
				return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its property limit", nil)
			}
			if len(stack) > 0 {
				stack[len(stack)-1].properties++
				if stack[len(stack)-1].properties > maxRemotePropertiesPerComponent {
					return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its component property limit", nil)
				}
			}
		}
		return nil
	}

	for start := 0; start < len(data); {
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			end = len(data)
		} else {
			end += start
		}
		line := data[start:end]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		physicalLines++
		if physicalLines > maxRemotePhysicalLines {
			return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its physical-line limit", nil)
		}

		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if !haveLogical {
				return NewError(CodeProtocolError, 0, "Calendar event data is malformed", nil)
			}
			logical = append(logical, line[1:]...)
		} else {
			if haveLogical {
				if err := flush(); err != nil {
					return err
				}
			}
			logical = append(logical[:0], line...)
			haveLogical = true
		}
		if len(logical) > maxRemoteLogicalLineBytes {
			return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its logical-line byte limit", nil)
		}
		if end == len(data) {
			break
		}
		start = end + 1
	}
	if haveLogical {
		if err := flush(); err != nil {
			return err
		}
	}
	if len(stack) != 0 {
		return NewError(CodeProtocolError, 0, "Calendar event data is malformed", nil)
	}
	return nil
}

// getCalendarObject is the bounded replacement for go-webdav's streaming
// GetCalendarObject. The full body is capped with overflow detection before
// go-ical sees any bytes.
func (c *Client) getCalendarObject(ctx context.Context, calendarPath, objectPath string) (*extcaldav.CalendarObject, error) {
	target, err := resolvePathOnBase(c.shardBase, objectPath)
	if err != nil {
		return nil, NewError(CodeProtocolError, 0, "Calendar object path is invalid", nil)
	}
	headers := make(http.Header)
	headers.Set("Accept", ical.MIMEType)
	result, err := c.doCalendarRequest(ctx, http.MethodGet, target, headers, nil, calendarPath)
	if err != nil {
		return nil, err
	}
	resp := result.response
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, classifyStatus(resp.StatusCode)
	}
	if resp.ContentLength > maxGetBodySize {
		return nil, NewError(CodePayloadTooLarge, resp.StatusCode, "Calendar GET response exceeded its byte limit", nil)
	}
	data, err := readBoundedCalendarBody(resp.Body, maxGetBodySize, "Calendar GET response exceeded its byte limit")
	if err != nil {
		return nil, err
	}

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, ical.MIMEType) {
		return nil, NewError(CodeProtocolError, resp.StatusCode, "Calendar GET response has an invalid content type", nil)
	}
	cal, err := decodeRemoteCalendar(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if err := validateRemoteCalendar(cal); err != nil {
		return nil, err
	}
	if err := validateCalendarObjectIdentity(cal, ""); err != nil {
		return nil, err
	}

	obj := &extcaldav.CalendarObject{
		Path:          result.url.EscapedPath(),
		ContentLength: int64(len(data)),
		Data:          cal,
	}
	for _, rawETag := range resp.Header.Values("ETag") {
		if len(rawETag) > maxETagBytes {
			return nil, NewError(CodePayloadTooLarge, resp.StatusCode, "Calendar ETag exceeded its byte limit", nil)
		}
	}
	obj.ETag, err = strongETagFromHeader(resp.Header)
	if err != nil {
		return nil, NewError(CodeProtocolError, resp.StatusCode, "Calendar GET response has an invalid ETag", nil)
	}
	if modified := resp.Header.Get("Last-Modified"); modified != "" {
		obj.ModTime, err = http.ParseTime(modified)
		if err != nil {
			return nil, NewError(CodeProtocolError, resp.StatusCode, "Calendar GET response has invalid metadata", nil)
		}
	}
	return obj, nil
}
