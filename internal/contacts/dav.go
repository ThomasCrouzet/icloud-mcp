package contacts

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	maxRedirects    = 3
	maxReadAttempts = 3
	maxRateWait     = 2 * time.Second
)

// HTTPDoer is the injected transport surface used by Client.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type davResponse struct {
	Status int
	Header http.Header
	Body   []byte
	URL    *url.URL
}

func noRedirectDoer(doer HTTPDoer) HTTPDoer {
	client, ok := doer.(*http.Client)
	if !ok {
		return doer
	}
	clone := *client
	clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

func (c *Client) doDAV(ctx context.Context, method string, target *url.URL, headers http.Header, body []byte, bodyCap int64, selectedBook *url.URL) (*davResponse, error) {
	write := method == http.MethodPut || method == http.MethodDelete
	if c.http == nil {
		return nil, newError(CodeInternalError, 0, "Contacts HTTP doer is not configured")
	}
	if selectedBook != nil {
		if err := c.validateURL(selectedBook); err != nil {
			return nil, err
		}
	}
	current := cloneURL(target)
	redirects := 0
	retries := 0

	for {
		if err := c.validateURL(current); err != nil {
			return nil, err
		}
		if selectedBook != nil && !urlWithin(current, selectedBook, method == "REPORT") {
			return nil, newError(CodeProtocolError, 0, "Contacts request is outside its selected address book")
		}
		if err := c.waitAttempt(ctx, write); err != nil {
			return nil, err
		}
		if err := c.acquire(ctx); err != nil {
			return nil, err
		}

		result, next, err := func() (*davResponse, *url.URL, error) {
			defer c.release()

			var reader io.Reader
			if len(body) > 0 {
				reader = bytes.NewReader(body)
			}
			req, err := http.NewRequestWithContext(ctx, method, current.String(), reader)
			if err != nil {
				return nil, nil, newError(CodeInternalError, 0, "failed to build a Contacts request")
			}
			req.Header = headers.Clone()

			resp, doErr := c.http.Do(req)
			if doErr != nil {
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				if write {
					return nil, nil, mutationOutcomeUnknown(0)
				}
				if ctx.Err() != nil {
					return nil, nil, newError(CodeTimeout, 0, "the Contacts request timed out")
				}
				return nil, nil, &Error{Code: CodeUnavailable, Message: "iCloud Contacts is temporarily unavailable", Retryable: true}
			}
			if resp == nil {
				if write {
					return nil, nil, mutationOutcomeUnknown(0)
				}
				return nil, nil, newError(CodeProtocolError, 0, "Contacts HTTP doer returned no response")
			}

			responseURL := current
			if resp.Request != nil {
				if resp.Request.URL != nil {
					responseURL = resp.Request.URL
				}
				if resp.Request.URL == nil || responseURL.String() != current.String() || resp.Request.Method != method {
					if resp.Body != nil {
						_ = resp.Body.Close()
					}
					if write {
						return nil, nil, mutationOutcomeUnknown(0)
					}
					return nil, nil, newError(CodeProtocolError, 0, "the HTTP doer followed an automatic redirect")
				}
			}
			if err := c.validateURL(responseURL); err != nil {
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
				return nil, nil, err
			}
			if write && resp.StatusCode >= 300 && resp.StatusCode < 400 {
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
				return nil, nil, mutationOutcomeUnknown(resp.StatusCode)
			}

			if isRedirect(resp.StatusCode) {
				location := resp.Header.Get("Location")
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
				if redirects >= maxRedirects || location == "" {
					return nil, nil, newError(CodeProtocolError, resp.StatusCode, "Contacts redirect limit exceeded or Location was missing")
				}
				nextURL, err := resolveAndValidate(c, responseURL, location)
				if err != nil {
					return nil, nil, err
				}
				if selectedBook != nil && !urlWithin(nextURL, selectedBook, method == "REPORT") {
					return nil, nil, newError(CodeProtocolError, resp.StatusCode, "Contacts redirect is outside its selected address book")
				}
				return nil, nextURL, nil
			}
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
				return nil, nil, newError(CodeProtocolError, resp.StatusCode, "Contacts received an unsupported redirect status")
			}
			if write && isAmbiguousMutationStatus(resp.StatusCode) {
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
				return nil, nil, mutationOutcomeUnknown(resp.StatusCode)
			}

			readCap := bodyCap
			if readCap <= 0 {
				readCap = maxErrorBodyBytes
			}
			var data []byte
			var readErr error
			if resp.Body != nil {
				data, readErr = io.ReadAll(io.LimitReader(resp.Body, readCap+1))
				_ = resp.Body.Close()
			}
			if readErr != nil {
				if write {
					return &davResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), URL: cloneURL(responseURL)}, nil, nil
				}
				return nil, nil, newError(CodeUnavailable, 0, "failed to read the Contacts response")
			}
			if int64(len(data)) > readCap {
				if write && resp.StatusCode/100 == 2 {
					data = nil
				} else {
					return nil, nil, newError(CodePayloadTooLarge, resp.StatusCode, "the Contacts response exceeded its byte limit")
				}
			}
			return &davResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: data, URL: cloneURL(responseURL)}, nil, nil
		}()
		if err != nil {
			return nil, err
		}
		if next != nil {
			current = next
			redirects++
			continue
		}
		if !write && isRetryStatus(result.Status) && retries+1 < maxReadAttempts {
			retries++
			if err := waitRetry(ctx, retryAfter(result.Header, retries)); err != nil {
				return nil, err
			}
			continue
		}
		return result, nil
	}
}

func (c *Client) waitAttempt(ctx context.Context, write bool) error {
	limiter := c.readLimit
	kind := "read"
	if write {
		limiter = c.writeLimit
		kind = "write"
	}
	reservation := limiter.Reserve()
	if !reservation.OK() {
		return newError(CodeRateLimited, http.StatusTooManyRequests, "Contacts "+kind+" rate limit exceeded")
	}
	delay := reservation.DelayFrom(time.Now())
	if delay > maxRateWait {
		reservation.Cancel()
		retryAfter := delay
		if retryAfter > 60*time.Second {
			retryAfter = 60 * time.Second
		}
		return &Error{Code: CodeRateLimited, Status: http.StatusTooManyRequests, Message: "Contacts " + kind + " rate limit exceeded", Retryable: true, RetryAfter: retryAfter}
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		reservation.Cancel()
		return newError(CodeTimeout, 0, "the Contacts request timed out while waiting for its rate limit")
	case <-timer.C:
		return nil
	}
}

func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.semaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return newError(CodeTimeout, 0, "the Contacts request timed out while waiting for network capacity")
	}
}

func (c *Client) release() { <-c.semaphore }

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func isRetryStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isAmbiguousMutationStatus(status int) bool {
	// Post-dispatch uncertainty: never mark these retryable without reconcile.
	// 429 is definitive (not applied) and stays outside this set.
	if status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout {
		return true
	}
	if status >= 500 {
		return true
	}
	return false
}

func mutationOutcomeUnknown(status int) *Error {
	const reconciliation = "For create, search for client_uid if one was supplied. For update or delete, re-read the contact by UID before deciding whether to retry."
	return &Error{
		Code:           CodeOutcomeUnknown,
		Status:         status,
		Message:        "the contact mutation outcome is unknown; reconcile the contact state before retrying",
		Reconciliation: reconciliation,
	}
}

func retryAfter(header http.Header, attempt int) time.Duration {
	const maxDelay = 2 * time.Second
	if value := strings.TrimSpace(header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil {
			delay := time.Duration(seconds) * time.Second
			if delay < 0 {
				delay = 0
			}
			if delay > maxDelay {
				delay = maxDelay
			}
			return delay
		}
		if at, err := http.ParseTime(value); err == nil {
			delay := time.Until(at)
			if delay < 0 {
				delay = 0
			}
			if delay > maxDelay {
				delay = maxDelay
			}
			return delay
		}
	}
	delay := 250 * time.Millisecond * time.Duration(1<<(attempt-1))
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return newError(CodeTimeout, 0, "the Contacts request timed out during retry backoff")
	case <-timer.C:
		return nil
	}
}

func resolveAndValidate(c *Client, base *url.URL, href string) (*url.URL, error) {
	href = strings.TrimSpace(href)
	if href == "" {
		return nil, newError(CodeProtocolError, 0, "iCloud Contacts returned an empty DAV href")
	}
	reference, err := url.Parse(href)
	if err != nil {
		return nil, newError(CodeProtocolError, 0, "iCloud Contacts returned an invalid DAV href")
	}
	resolved := base.ResolveReference(reference)
	if err := c.validateURL(resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

func cloneURL(in *url.URL) *url.URL {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func classifyDAVError(resp *davResponse, create bool) error {
	if resp.Status/100 == 2 {
		return nil
	}
	if resp.Status == http.StatusPreconditionFailed {
		if create {
			return newError(CodeConflict, resp.Status, "a contact resource already exists at the create target")
		}
		return newError(CodeConcurrentModification, resp.Status, "the contact changed since it was read")
	}
	preconditions, malformed := davPreconditions(resp.Body)
	for _, condition := range preconditions {
		switch condition {
		case "no-uid-conflict":
			return newError(CodeConflict, resp.Status, "the contact UID conflicts with an existing contact")
		case "valid-address-data":
			return newError(CodeValidation, resp.Status, "the server rejected the vCard data")
		case "max-resource-size":
			return newError(CodePayloadTooLarge, resp.Status, "the contact exceeds the server resource limit")
		}
	}
	if malformed || len(preconditions) > 0 {
		return newError(CodeProtocolError, resp.Status, "iCloud Contacts returned an unknown DAV precondition")
	}
	return classifyStatus(resp.Status)
}

func davPreconditions(body []byte) ([]string, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var conditions []string
	inError := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return conditions, false
		}
		if err != nil {
			return nil, true
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "error" {
				inError = true
				continue
			}
			if inError {
				conditions = append(conditions, value.Name.Local)
			}
		case xml.EndElement:
			if value.Name.Local == "error" {
				inError = false
			}
		}
	}
}

func newLimiters() (*rate.Limiter, *rate.Limiter) {
	return rate.NewLimiter(rate.Every(time.Minute/60), 10), rate.NewLimiter(rate.Every(time.Minute/20), 3)
}
