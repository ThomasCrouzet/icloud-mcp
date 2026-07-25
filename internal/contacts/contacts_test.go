package contacts

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (fn doerFunc) Do(req *http.Request) (*http.Response, error) { return fn(req) }

func httpResponse(req *http.Request, status int, body string, headers map[string]string) *http.Response {
	header := make(http.Header)
	for key, value := range headers {
		header.Set(key, value)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func allowContactsHost(host string) bool {
	if host == "contacts.icloud.com" {
		return true
	}
	matched, _ := regexp.MatchString(`^p[0-9]{1,3}-contacts[.]icloud[.]com$`, host)
	return matched
}

func principalXML(href string) string {
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href><D:propstat><D:prop><D:current-user-principal><D:href>` +
		xmlEscape(href) + `</D:href></D:current-user-principal></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
}

func homesXML(hrefs ...string) string {
	var values strings.Builder
	for _, href := range hrefs {
		values.WriteString(`<D:href>` + xmlEscape(href) + `</D:href>`)
	}
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/principal/</D:href><D:propstat><D:prop><C:addressbook-home-set>` +
		values.String() + `</C:addressbook-home-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
}

func booksXML(href string) string {
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>` + xmlEscape(href) +
		`</D:href><D:propstat><D:prop><D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>People</D:displayname><C:addressbook-description>Primary contacts</C:addressbook-description><C:supported-address-data><C:address-data-type content-type="text/vcard" version="3.0"/><C:address-data-type content-type="text/vcard" version="4.0"/></C:supported-address-data><C:max-resource-size>1048576</C:max-resource-size></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
}

func cardResponseXML(href, etag, card string) string {
	etagProperty := ""
	if etag != "" {
		etagProperty = `<D:getetag>` + xmlEscape(etag) + `</D:getetag>`
	}
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>` + xmlEscape(href) +
		`</D:href><D:propstat><D:prop>` + etagProperty + `<C:address-data>` + xmlEscape(card) +
		`</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
}

func multiCardResponseXML(cards map[string]string) string {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">`)
	for href, card := range cards {
		body.WriteString(`<D:response><D:href>` + xmlEscape(href) + `</D:href><D:propstat><D:prop><D:getetag>&quot;e&quot;</D:getetag><C:address-data>` + xmlEscape(card) + `</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}
	body.WriteString(`</D:multistatus>`)
	return body.String()
}

func v3Card(uid, name string, extra ...string) string {
	lines := []string{"BEGIN:VCARD", "VERSION:3.0", "UID:" + uid, "FN:" + name, "N:" + name + ";;;;"}
	lines = append(lines, extra...)
	lines = append(lines, "END:VCARD")
	return strings.Join(lines, "\r\n") + "\r\n"
}

func discoveryDoer(reportBody string) doerFunc {
	return func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == "PROPFIND" && req.URL.Path == "/":
			return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
		case req.Method == "PROPFIND" && req.URL.Path == "/principal/":
			return httpResponse(req, http.StatusMultiStatus, homesXML("/home/"), nil), nil
		case req.Method == "PROPFIND" && req.URL.Path == "/home/":
			return httpResponse(req, http.StatusMultiStatus, booksXML("/home/book/"), nil), nil
		case req.Method == "REPORT" && req.URL.Path == "/home/book/":
			return httpResponse(req, http.StatusMultiStatus, reportBody, nil), nil
		default:
			return httpResponse(req, http.StatusNotFound, "", nil), nil
		}
	}
}

func discoveredBook(t *testing.T, client *Client) AddressBook {
	t.Helper()
	books, err := client.ListAddressBooks(context.Background())
	if err != nil {
		t.Fatalf("ListAddressBooks() error = %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("ListAddressBooks() returned %d books, want 1", len(books))
	}
	return books[0]
}

func requireCode(t *testing.T, err error, code Code) {
	t.Helper()
	typed := AsError(err)
	if typed == nil || typed.Code != code {
		t.Fatalf("error = %v, want code %q", err, code)
	}
}

func TestDAVMultistatusStatusParsingIsExact(t *testing.T) {
	for _, status := range []string{"HTTP/1.1 200 OK", "HTTP/1.0 200 Success", " HTTP/1.1 200 OK "} {
		if !statusIsOK(status) {
			t.Errorf("valid DAV status %q was rejected", status)
		}
	}
	for _, status := range []string{
		"1200 OK",
		"HTTP/1.1 1200 OK",
		"HTTP/1.1 2000 OK",
		"HTTP/1.1 200OK",
		"HTTP/9.9 200 OK",
		"HTTP/1.1\n200 OK",
	} {
		if statusIsOK(status) {
			t.Errorf("malformed DAV status %q was accepted", status)
		}
	}

	card := v3Card("u", "User")
	body := strings.Replace(cardResponseXML("/home/book/u.vcf", `"etag"`, card), "HTTP/1.1 200 OK", "HTTP/1.1 1200 OK", 1)
	client := NewClient(discoveryDoer(body), "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	result, err := client.SearchContacts(context.Background(), SearchOptions{AddressBook: book.Identifier})
	if err != nil {
		t.Fatalf("SearchContacts() error = %v", err)
	}
	if len(result.Contacts) != 0 {
		t.Fatalf("malformed 1200 propstat returned contacts: %#v", result.Contacts)
	}
}

func TestDiscoveryPreservesAuthoritiesAndValidatesShardCallback(t *testing.T) {
	var mu sync.Mutex
	var validated []string
	allow := func(host string) bool {
		mu.Lock()
		validated = append(validated, host)
		mu.Unlock()
		return allowContactsHost(host)
	}
	var requests []string
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.String())
		switch req.URL.Path {
		case "/":
			return httpResponse(req, http.StatusMultiStatus, principalXML("https://p12-contacts.icloud.com/principal/"), nil), nil
		case "/principal/":
			return httpResponse(req, http.StatusMultiStatus, homesXML("https://p103-contacts.icloud.com:443/home/"), nil), nil
		case "/home/":
			return httpResponse(req, http.StatusMultiStatus, booksXML("contacts/"), nil), nil
		default:
			return httpResponse(req, http.StatusNotFound, "", nil), nil
		}
	})
	client := NewClient(doer, "https://contacts.icloud.com/", allow)
	books, err := client.ListAddressBooks(context.Background())
	if err != nil {
		t.Fatalf("ListAddressBooks() error = %v", err)
	}
	if len(books) != 1 || !strings.HasPrefix(books[0].Identifier, "book-") {
		t.Fatalf("books = %#v", books)
	}
	wantLast := "PROPFIND https://p103-contacts.icloud.com:443/home/"
	if requests[len(requests)-1] != wantLast {
		t.Fatalf("last request = %q, want %q", requests[len(requests)-1], wantLast)
	}
	mu.Lock()
	joined := strings.Join(validated, ",")
	mu.Unlock()
	if !strings.Contains(joined, "p12-contacts.icloud.com") || !strings.Contains(joined, "p103-contacts.icloud.com") {
		t.Fatalf("validated hosts = %q", joined)
	}
}

func TestDiscoveryRejectsUnsafeAuthoritiesBeforeSecondRequest(t *testing.T) {
	tests := []struct {
		name  string
		href  string
		allow func(string) bool
	}{
		{"http", "http://p12-contacts.icloud.com/principal/", allowContactsHost},
		{"non-standard port", "https://p12-contacts.icloud.com:444/principal/", allowContactsHost},
		{"uppercase host", "https://P12-contacts.icloud.com/principal/", allowContactsHost},
		{"foreign host", "https://example.com/principal/", allowContactsHost},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			doer := doerFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return httpResponse(req, http.StatusMultiStatus, principalXML(test.href), nil), nil
			})
			client := NewClient(doer, "https://contacts.icloud.com/", test.allow)
			_, err := client.ListAddressBooks(context.Background())
			requireCode(t, err, CodeProtocolError)
			if calls.Load() != 1 {
				t.Fatalf("HTTP calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestDiscoveryRejectsShardWhenCallbackDoes(t *testing.T) {
	var calls atomic.Int32
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return httpResponse(req, http.StatusMultiStatus, principalXML("https://p12-contacts.icloud.com/principal/"), nil), nil
	})
	client := NewClient(doer, "https://contacts.icloud.com/", func(host string) bool { return host == "contacts.icloud.com" })
	_, err := client.ListAddressBooks(context.Background())
	requireCode(t, err, CodeProtocolError)
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls.Load())
	}
}

func TestDiscoveryMergesHomesAndRejectsCrossHomeOrDuplicateBooks(t *testing.T) {
	t.Run("multiple homes", func(t *testing.T) {
		doer := doerFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/":
				return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
			case "/principal/":
				return httpResponse(req, http.StatusMultiStatus, homesXML("/home-a/", "/home-b/"), nil), nil
			case "/home-a/":
				return httpResponse(req, http.StatusMultiStatus, booksXML("/home-a/book/"), nil), nil
			case "/home-b/":
				return httpResponse(req, http.StatusMultiStatus, booksXML("/home-b/book/"), nil), nil
			default:
				return httpResponse(req, http.StatusNotFound, "", nil), nil
			}
		})
		client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
		books, err := client.ListAddressBooks(context.Background())
		if err != nil || len(books) != 2 || books[0].Identifier == books[1].Identifier {
			t.Fatalf("ListAddressBooks() = %#v, %v", books, err)
		}
	})

	t.Run("cross home", func(t *testing.T) {
		doer := doerFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/":
				return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
			case "/principal/":
				return httpResponse(req, http.StatusMultiStatus, homesXML("/home/"), nil), nil
			case "/home/":
				return httpResponse(req, http.StatusMultiStatus, booksXML("/other/book/"), nil), nil
			default:
				return httpResponse(req, http.StatusNotFound, "", nil), nil
			}
		})
		client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
		_, err := client.ListAddressBooks(context.Background())
		requireCode(t, err, CodeProtocolError)
	})

	t.Run("duplicate collection", func(t *testing.T) {
		doer := doerFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/":
				return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
			case "/principal/":
				return httpResponse(req, http.StatusMultiStatus, homesXML("/home/", "/home/nested/"), nil), nil
			case "/home/", "/home/nested/":
				return httpResponse(req, http.StatusMultiStatus, booksXML("/home/nested/book/"), nil), nil
			default:
				return httpResponse(req, http.StatusNotFound, "", nil), nil
			}
		})
		client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
		_, err := client.ListAddressBooks(context.Background())
		requireCode(t, err, CodeProtocolError)
	})
}

func TestDiscoveryFailureIsNotCached(t *testing.T) {
	var baseCalls atomic.Int32
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/":
			if baseCalls.Add(1) == 1 {
				return httpResponse(req, http.StatusMultiStatus, "not xml", nil), nil
			}
			return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
		case "/principal/":
			return httpResponse(req, http.StatusMultiStatus, homesXML("/home/"), nil), nil
		case "/home/":
			return httpResponse(req, http.StatusMultiStatus, booksXML("/home/book/"), nil), nil
		default:
			return httpResponse(req, http.StatusNotFound, "", nil), nil
		}
	})
	client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
	_, err := client.ListAddressBooks(context.Background())
	requireCode(t, err, CodeProtocolError)
	books, err := client.ListAddressBooks(context.Background())
	if err != nil || len(books) != 1 {
		t.Fatalf("second ListAddressBooks() = %#v, %v", books, err)
	}
	if baseCalls.Load() != 2 {
		t.Fatalf("base discovery calls = %d, want 2", baseCalls.Load())
	}
}

func TestConcurrentDiscoveryCachesOneSuccess(t *testing.T) {
	var calls atomic.Int32
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(time.Millisecond)
		switch req.URL.Path {
		case "/":
			return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
		case "/principal/":
			return httpResponse(req, http.StatusMultiStatus, homesXML("/home/"), nil), nil
		case "/home/":
			return httpResponse(req, http.StatusMultiStatus, booksXML("/home/book/"), nil), nil
		default:
			return httpResponse(req, http.StatusNotFound, "", nil), nil
		}
	})
	client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
	var wg sync.WaitGroup
	errors := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			books, err := client.ListAddressBooks(context.Background())
			if err != nil {
				errors <- err
				return
			}
			if len(books) != 1 {
				errors <- validationError("unexpected book count")
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Errorf("concurrent discovery error = %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("HTTP calls = %d, want one three-request discovery", calls.Load())
	}
}

func TestDiscoveryWaitIsCancellableAndFailedAttemptCanRetry(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var baseCalls atomic.Int32
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/":
			if baseCalls.Add(1) == 1 {
				close(started)
				<-release
				return httpResponse(req, http.StatusMultiStatus, "not xml", nil), nil
			}
			return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
		case "/principal/":
			return httpResponse(req, http.StatusMultiStatus, homesXML("/home/"), nil), nil
		case "/home/":
			return httpResponse(req, http.StatusMultiStatus, booksXML("/home/book/"), nil), nil
		default:
			return httpResponse(req, http.StatusNotFound, "", nil), nil
		}
	})
	client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.ListAddressBooks(context.Background())
		firstDone <- err
	}()
	<-started

	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	waitDone := make(chan error, 1)
	go func() {
		_, err := client.ListAddressBooks(waitCtx)
		waitDone <- err
	}()
	select {
	case err := <-waitDone:
		requireCode(t, err, CodeTimeout)
	case <-time.After(time.Second):
		close(release)
		t.Fatal("canceled discovery waiter remained blocked")
	}

	close(release)
	requireCode(t, <-firstDone, CodeProtocolError)
	books, err := client.ListAddressBooks(context.Background())
	if err != nil || len(books) != 1 {
		t.Fatalf("retry after failed discovery = %#v, %v", books, err)
	}
	if baseCalls.Load() != 2 {
		t.Fatalf("base discovery calls = %d, want 2", baseCalls.Load())
	}
}

func TestCanceledDiscoveryAttemptIsNotCached(t *testing.T) {
	started := make(chan struct{})
	var baseCalls atomic.Int32
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/":
			if baseCalls.Add(1) == 1 {
				close(started)
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
		case "/principal/":
			return httpResponse(req, http.StatusMultiStatus, homesXML("/home/"), nil), nil
		case "/home/":
			return httpResponse(req, http.StatusMultiStatus, booksXML("/home/book/"), nil), nil
		default:
			return httpResponse(req, http.StatusNotFound, "", nil), nil
		}
	})
	client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.ListAddressBooks(ctx)
		firstDone <- err
	}()
	<-started
	cancel()
	requireCode(t, <-firstDone, CodeTimeout)
	books, err := client.ListAddressBooks(context.Background())
	if err != nil || len(books) != 1 || baseCalls.Load() != 2 {
		t.Fatalf("retry after canceled discovery = %#v, %v, calls=%d", books, err, baseCalls.Load())
	}
}

func TestManualRedirectPreservesPROPFIND(t *testing.T) {
	var redirectedMethod string
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/":
			return httpResponse(req, http.StatusMovedPermanently, "", map[string]string{"Location": "/dav/"}), nil
		case "/dav/":
			redirectedMethod = req.Method
			return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
		case "/principal/":
			return httpResponse(req, http.StatusMultiStatus, homesXML("/home/"), nil), nil
		case "/home/":
			return httpResponse(req, http.StatusMultiStatus, booksXML("/home/book/"), nil), nil
		default:
			return httpResponse(req, http.StatusNotFound, "", nil), nil
		}
	})
	client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
	if _, err := client.ListAddressBooks(context.Background()); err != nil {
		t.Fatalf("ListAddressBooks() error = %v", err)
	}
	if redirectedMethod != "PROPFIND" {
		t.Fatalf("redirected method = %q, want PROPFIND", redirectedMethod)
	}
}

func TestReadRedirectsStayWithinSelectedOpaqueAddressBook(t *testing.T) {
	book, err := url.Parse("https://contacts.icloud.com/home/%62ook/")
	if err != nil {
		t.Fatal(err)
	}
	getTarget, err := url.Parse("https://contacts.icloud.com/home/%62ook/original.vcf")
	if err != nil {
		t.Fatal(err)
	}
	unsafeLocations := []string{
		"/home/other/redirected.vcf",
		"/home/%62ook/redirected.vcf?download=1",
		"/home/%62ook/redirected.vcf?",
		"/home/%62ook%2fother/redirected.vcf",
		"/home/%62ook/%2e%2e/other/redirected.vcf",
		"/home/%2562ook/%252e%252e/other/redirected.vcf",
		"/home/book/redirected.vcf",
	}
	for _, method := range []string{"REPORT", http.MethodGet} {
		for _, location := range unsafeLocations {
			t.Run(method+" "+location, func(t *testing.T) {
				var calls atomic.Int32
				client := NewClient(doerFunc(func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					return httpResponse(req, http.StatusTemporaryRedirect, "", map[string]string{"Location": location}), nil
				}), "https://contacts.icloud.com/", allowContactsHost)
				target := book
				if method == http.MethodGet {
					target = getTarget
				}
				_, err := client.doDAV(context.Background(), method, target, nil, nil, maxErrorBodyBytes, book)
				requireCode(t, err, CodeProtocolError)
				if calls.Load() != 1 {
					t.Fatalf("HTTP calls = %d, want redirect rejected before second dispatch", calls.Load())
				}
			})
		}
	}

	caseBook, _ := url.Parse("https://contacts.icloud.com/home/%6aook/")
	sameEscapedBook, _ := url.Parse("https://contacts.icloud.com/home/%6aook/contact.vcf")
	sameEscapedBookUpper, _ := url.Parse("https://contacts.icloud.com/home/%6Aook/contact.vcf")
	decodedAlias, _ := url.Parse("https://contacts.icloud.com/home/jook/contact.vcf")
	if !urlWithin(sameEscapedBookUpper, caseBook, false) {
		t.Fatal("equivalent escaped address-book path was rejected")
	}
	if urlWithin(decodedAlias, caseBook, false) || !urlWithin(sameEscapedBook, caseBook, false) {
		t.Fatal("opaque address-book path comparison accepted a decoded alias")
	}
}

func TestReadOperationsApplySelectedBookRedirectScope(t *testing.T) {
	t.Run("REPORT", func(t *testing.T) {
		base := discoveryDoer("")
		var reports atomic.Int32
		client := NewClient(doerFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == "REPORT" {
				reports.Add(1)
				return httpResponse(req, http.StatusTemporaryRedirect, "", map[string]string{"Location": "/home/other/"}), nil
			}
			return base(req)
		}), "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		_, err := client.SearchContacts(context.Background(), SearchOptions{AddressBook: book.Identifier})
		requireCode(t, err, CodeProtocolError)
		if reports.Load() != 1 {
			t.Fatalf("REPORT dispatches = %d, want 1", reports.Load())
		}
	})

	t.Run("GET", func(t *testing.T) {
		card := v3Card("uid", "User")
		base := discoveryDoer(cardResponseXML("/home/book/uid.vcf", `"report"`, card))
		var gets atomic.Int32
		client := NewClient(doerFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				gets.Add(1)
				return httpResponse(req, http.StatusTemporaryRedirect, "", map[string]string{"Location": "/home/other/uid.vcf"}), nil
			}
			return base(req)
		}), "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		_, err := client.GetContact(context.Background(), book.Identifier, "uid")
		requireCode(t, err, CodeProtocolError)
		if gets.Load() != 1 {
			t.Fatalf("GET dispatches = %d, want 1", gets.Load())
		}
	})
}

func TestMutationRedirectsStayWithinSelectedAddressBook(t *testing.T) {
	book, err := url.Parse("https://contacts.icloud.com/home/book/")
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse("https://contacts.icloud.com/home/book/original.vcf")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		method   string
		ifHeader string
		ifValue  string
	}{
		{name: "create", method: http.MethodPut, ifHeader: "If-None-Match", ifValue: "*"},
		{name: "update", method: http.MethodPut, ifHeader: "If-Match", ifValue: `"v1"`},
		{name: "delete", method: http.MethodDelete, ifHeader: "If-Match", ifValue: `"v1"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			doer := doerFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return httpResponse(req, http.StatusTemporaryRedirect, "", map[string]string{"Location": "/home/other/redirected.vcf"}), nil
			})
			client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
			header := make(http.Header)
			header.Set(test.ifHeader, test.ifValue)
			_, err := client.doDAV(context.Background(), test.method, target, header, []byte("card body"), maxErrorBodyBytes, book)
			requireCode(t, err, CodeOutcomeUnknown)
			if calls.Load() != 1 {
				t.Fatalf("HTTP calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestMutationSameBookRedirectIsNotReplayed(t *testing.T) {
	book, err := url.Parse("https://contacts.icloud.com/home/book/")
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse("https://contacts.icloud.com/home/book/original.vcf")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		method   string
		ifHeader string
		ifValue  string
		body     string
	}{
		{name: "create", method: http.MethodPut, ifHeader: "If-None-Match", ifValue: "*", body: "create body"},
		{name: "update", method: http.MethodPut, ifHeader: "If-Match", ifValue: `"v1"`, body: "update body"},
		{name: "delete", method: http.MethodDelete, ifHeader: "If-Match", ifValue: `"v1"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			doer := doerFunc(func(req *http.Request) (*http.Response, error) {
				call := calls.Add(1)
				if call == 1 {
					return httpResponse(req, http.StatusPermanentRedirect, "", map[string]string{"Location": "/home/book/redirected.vcf"}), nil
				}
				var body []byte
				if req.Body != nil {
					var readErr error
					body, readErr = io.ReadAll(req.Body)
					if readErr != nil {
						t.Fatalf("read redirected body: %v", readErr)
					}
				}
				if req.Method != test.method || req.URL.Path != "/home/book/redirected.vcf" || req.Header.Get(test.ifHeader) != test.ifValue || string(body) != test.body {
					t.Fatalf("redirected request = %s %s, %s=%q, body=%q", req.Method, req.URL.Path, test.ifHeader, req.Header.Get(test.ifHeader), body)
				}
				return httpResponse(req, http.StatusNoContent, "", nil), nil
			})
			client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
			header := make(http.Header)
			header.Set(test.ifHeader, test.ifValue)
			_, err := client.doDAV(context.Background(), test.method, target, header, []byte(test.body), maxErrorBodyBytes, book)
			requireCode(t, err, CodeOutcomeUnknown)
			if calls.Load() != 1 {
				t.Fatalf("HTTP calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestMutationSeeOtherRedirectIsOutcomeUnknown(t *testing.T) {
	book, err := url.Parse("https://contacts.icloud.com/home/book/")
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse("https://contacts.icloud.com/home/book/original.vcf")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client := NewClient(doerFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return httpResponse(req, http.StatusSeeOther, "", map[string]string{"Location": "/home/book/redirected.vcf"}), nil
	}), "https://contacts.icloud.com/", allowContactsHost)
	_, err = client.doDAV(context.Background(), http.MethodPut, target, nil, []byte("card body"), maxErrorBodyBytes, book)
	requireCode(t, err, CodeOutcomeUnknown)
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls.Load())
	}
}

func TestMutationAutomaticallyFollowedRedirectIsOutcomeUnknown(t *testing.T) {
	target, err := url.Parse("https://contacts.icloud.com/home/book/contact.vcf")
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(doerFunc(func(req *http.Request) (*http.Response, error) {
		followed := req.Clone(req.Context())
		followed.Method = http.MethodGet
		followed.URL = cloneURL(req.URL)
		return httpResponse(followed, http.StatusOK, "", nil), nil
	}), "https://contacts.icloud.com/", allowContactsHost)
	_, err = client.doDAV(context.Background(), http.MethodPut, target, nil, []byte("card"), maxErrorBodyBytes, nil)
	requireCode(t, err, CodeOutcomeUnknown)
}

func TestMutationAmbiguityIsOutcomeUnknownWithoutRetry(t *testing.T) {
	book, err := url.Parse("https://contacts.icloud.com/home/book/")
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse("https://contacts.icloud.com/home/book/contact.vcf")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		status    int
		transport bool
	}{
		{name: "transport", transport: true},
		{name: "internal server error", status: http.StatusInternalServerError},
		{name: "bad gateway", status: http.StatusBadGateway},
		{name: "service unavailable", status: http.StatusServiceUnavailable},
		{name: "gateway timeout", status: http.StatusGatewayTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			client := NewClient(doerFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if test.transport {
					return nil, fmt.Errorf("transport failed")
				}
				return httpResponse(req, test.status, "ignored", nil), nil
			}), "https://contacts.icloud.com/", allowContactsHost)
			_, err := client.doDAV(context.Background(), http.MethodPut, target, nil, []byte("body"), maxErrorBodyBytes, book)
			requireCode(t, err, CodeOutcomeUnknown)
			typed := AsError(err)
			if typed.Retryable || typed.Reconciliation == "" || typed.Status != test.status {
				t.Fatalf("outcome error = %#v", typed)
			}
			if calls.Load() != 1 {
				t.Fatalf("mutation attempts = %d, want 1", calls.Load())
			}
		})
	}
}

func TestGetContactUsesImportedHrefNotUIDFilename(t *testing.T) {
	card := v3Card("logical-uid", "Imported", "EMAIL;TYPE=HOME:imported@example.com")
	report := cardResponseXML("/home/book/server-generated-name.vcf", `"r1"`, card)
	var getPath string
	base := discoveryDoer(report)
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			getPath = req.URL.Path
			return httpResponse(req, http.StatusOK, card, map[string]string{"Content-Type": "text/vcard; charset=utf-8", "ETag": `"g1"`}), nil
		}
		return base(req)
	})
	client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	contact, err := client.GetContact(context.Background(), book.Identifier, "logical-uid")
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if contact.UID != "logical-uid" || getPath != "/home/book/server-generated-name.vcf" {
		t.Fatalf("contact UID/path = %q/%q", contact.UID, getPath)
	}
}

func TestGetContactAcceptsICloudTextPlainVCard(t *testing.T) {
	// Live iCloud CardDAV GETs return text/plain; charset=UTF-8 with a vCard body.
	card := v3Card("plain-uid", "Plain Contact", "EMAIL;TYPE=HOME:plain@example.com")
	report := cardResponseXML("/home/book/plain.vcf", `"p1"`, card)
	base := discoveryDoer(report)
	var sawContentType string
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			sawContentType = "text/plain; charset=UTF-8"
			return httpResponse(req, http.StatusOK, card, map[string]string{
				"Content-Type": "text/plain; charset=UTF-8",
				"ETag":         `"p1"`,
			}), nil
		}
		return base(req)
	})
	client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	contact, err := client.GetContact(context.Background(), book.Identifier, "plain-uid")
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if contact.UID != "plain-uid" || contact.DisplayName != "Plain Contact" {
		t.Fatalf("contact = %#v", contact)
	}
	if sawContentType != "text/plain; charset=UTF-8" {
		t.Fatalf("fixture Content-Type not exercised: %q", sawContentType)
	}
}

func TestAcceptableVCardContentType(t *testing.T) {
	for _, mediaType := range []string{"text/vcard", "TEXT/VCARD", "text/x-vcard", "text/directory", "text/plain"} {
		if !acceptableVCardContentType(mediaType) {
			t.Fatalf("acceptableVCardContentType(%q) = false", mediaType)
		}
	}
	for _, mediaType := range []string{"application/json", "text/html", "", "application/xml"} {
		if acceptableVCardContentType(mediaType) {
			t.Fatalf("acceptableVCardContentType(%q) = true", mediaType)
		}
	}
}

func TestSearchFiltersGroupsAndSorts(t *testing.T) {
	cards := map[string]string{
		"/home/book/alice.vcf": v3Card("a", "Alice Zephyr", "EMAIL;TYPE=HOME:Alice@Example.com", "TEL:+1 (555) 0100", "ORG:Beta"),
		"/home/book/bob.vcf":   v3Card("b", "Bob Alpha", "EMAIL:bob@example.net", "TEL:555-9999", "ORG:Acme Labs"),
		"/home/book/team.vcf":  v3Card("g", "Team", "KIND:group", "EMAIL:team@example.com"),
	}
	client := NewClient(discoveryDoer(multiCardResponseXML(cards)), "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	tests := []struct {
		name string
		opts SearchOptions
		want []string
	}{
		{"query name", SearchOptions{AddressBook: book.Identifier, Query: "ALICE"}, []string{"a"}},
		{"query organization", SearchOptions{AddressBook: book.Identifier, Query: "acme"}, []string{"b"}},
		{"email", SearchOptions{AddressBook: book.Identifier, Email: "example.net"}, []string{"b"}},
		{"normalized phone", SearchOptions{AddressBook: book.Identifier, Phone: "(555) 01"}, []string{"a"}},
		{"groups excluded", SearchOptions{AddressBook: book.Identifier}, []string{"a", "b"}},
		{"groups included", SearchOptions{AddressBook: book.Identifier, IncludeGroups: true}, []string{"a", "b", "g"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := client.SearchContacts(context.Background(), test.opts)
			if err != nil {
				t.Fatalf("SearchContacts() error = %v", err)
			}
			var got []string
			for _, contact := range result.Contacts {
				got = append(got, contact.UID)
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("UIDs = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSearchQueryUsesEscapedServerSideFiltersAndBoundedFullData(t *testing.T) {
	tests := []struct {
		name       string
		opts       SearchOptions
		want       []string
		notWant    []string
		matchCount int
	}{
		{
			name: "query anyof", opts: SearchOptions{Query: `A<&`},
			want:       []string{`<C:filter test="anyof">`, `name="FN"`, `name="N"`, `name="EMAIL"`, `name="TEL"`, `name="ORG"`, `>A&lt;&amp;</C:text-match>`},
			matchCount: 5,
		},
		{
			name: "email before phone", opts: SearchOptions{Email: `mail<&`, Phone: "+1 555"},
			want:       []string{`<C:filter><C:prop-filter name="EMAIL">`, `>mail&lt;&amp;</C:text-match>`},
			notWant:    []string{`name="TEL"`, `test="anyof"`},
			matchCount: 1,
		},
		{
			name: "phone", opts: SearchOptions{Phone: "+1 (555)"},
			want:    []string{`<C:filter><C:prop-filter name="VERSION"/></C:filter>`},
			notWant: []string{`name="TEL"`, `name="EMAIL"`, `<C:text-match`, `test="anyof"`},
		},
		{
			name: "email and normalized phone", opts: SearchOptions{Email: `mail<&`, Phone: "+1 (555)"},
			want:       []string{`<C:filter><C:prop-filter name="EMAIL">`, `>mail&lt;&amp;</C:text-match>`},
			notWant:    []string{`name="TEL"`, `test="anyof"`},
			matchCount: 1,
		},
		{
			name: "query and normalized phone", opts: SearchOptions{Query: "needle", Phone: "+1 (555)"},
			want:       []string{`<C:filter test="anyof">`, `name="FN"`, `name="TEL"`, `>needle</C:text-match>`},
			matchCount: 5,
		},
		{
			name: "version presence", opts: SearchOptions{},
			want:    []string{`<C:filter><C:prop-filter name="VERSION"/></C:filter>`},
			notWant: []string{`<C:text-match`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query := searchQuery(test.opts, maxCardsScanned+1)
			var document struct{}
			if err := xml.Unmarshal([]byte(query), &document); err != nil {
				t.Fatalf("query is not valid XML: %v\n%s", err, query)
			}
			for _, value := range append(test.want,
				`<D:prop><D:getetag/><C:address-data/></D:prop>`,
				`<C:limit><C:nresults>2001</C:nresults></C:limit>`,
			) {
				if !strings.Contains(query, value) {
					t.Errorf("query missing %q:\n%s", value, query)
				}
			}
			for _, value := range test.notWant {
				if strings.Contains(query, value) {
					t.Errorf("query unexpectedly contains %q:\n%s", value, query)
				}
			}
			if got := strings.Count(query, `<C:text-match`); got != test.matchCount {
				t.Errorf("text-match count = %d, want %d", got, test.matchCount)
			}
			if test.matchCount > 0 && (!strings.Contains(query, `collation="i;unicode-casemap"`) || !strings.Contains(query, `match-type="contains"`)) {
				t.Errorf("text-match attributes are missing:\n%s", query)
			}
		})
	}
}

func TestSearchScanAndResultCaps(t *testing.T) {
	scanResponse := func(count int) string {
		var body strings.Builder
		body.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">`)
		for i := range count {
			uid := fmt.Sprintf("u-%04d", i)
			card := v3Card(uid, "Person "+uid)
			body.WriteString(`<D:response><D:href>/home/book/` + uid + `.vcf</D:href><D:propstat><D:prop><C:address-data>` + xmlEscape(card) + `</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		}
		body.WriteString(`</D:multistatus>`)
		return body.String()
	}
	for _, test := range []struct {
		name          string
		count         int
		wantScanLimit bool
	}{
		{name: "exact cap reaches EOF", count: maxCardsScanned},
		{name: "cap plus one overflows", count: maxCardsScanned + 1, wantScanLimit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reportBody string
			base := discoveryDoer(scanResponse(test.count))
			client := NewClient(doerFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == "REPORT" {
					data, _ := io.ReadAll(req.Body)
					reportBody = string(data)
				}
				return base(req)
			}), "https://contacts.icloud.com/", allowContactsHost)
			book := discoveredBook(t, client)
			result, err := client.SearchContacts(context.Background(), SearchOptions{AddressBook: book.Identifier, Limit: 1})
			if err != nil {
				t.Fatalf("SearchContacts() error = %v", err)
			}
			if result.ScanLimitReached != test.wantScanLimit || !result.Truncated || len(result.Contacts) != 1 {
				t.Fatalf("result = %#v, want scanLimitReached=%v", result, test.wantScanLimit)
			}
			if !strings.Contains(reportBody, `<C:nresults>2001</C:nresults>`) {
				t.Fatalf("REPORT did not request cap+1 results:\n%s", reportBody)
			}
		})
	}

	largeCards := make(map[string]string)
	email := strings.Repeat("a", maxEmailBytes-len("@x.test")) + "@x.test"
	for i := range 100 {
		uid := fmt.Sprintf("large-%03d", i)
		extras := make([]string, 0, maxEmails)
		for range maxEmails {
			extras = append(extras, "EMAIL:"+email)
		}
		largeCards["/home/book/"+uid+".vcf"] = v3Card(uid, strings.Repeat("N", maxDisplayNameBytes), extras...)
	}
	largeClient := NewClient(discoveryDoer(multiCardResponseXML(largeCards)), "https://contacts.icloud.com/", allowContactsHost)
	largeBook := discoveredBook(t, largeClient)
	largeResult, err := largeClient.SearchContacts(context.Background(), SearchOptions{AddressBook: largeBook.Identifier, Limit: 100})
	if err != nil {
		t.Fatalf("large SearchContacts() error = %v", err)
	}
	encoded, _ := json.Marshal(largeResult)
	if len(encoded) > maxResultBytes || !largeResult.Truncated || len(largeResult.Contacts) >= 100 {
		t.Fatalf("result bytes/contacts/truncated = %d/%d/%v", len(encoded), len(largeResult.Contacts), largeResult.Truncated)
	}
}

func TestFieldAndCardCaps(t *testing.T) {
	validID := "book-AAAAAAAAAAAAAAAAAAAAAA"
	tests := []struct {
		name string
		err  error
	}{
		{"display name", validateCreate(&CreateContactInput{AddressBook: validID, DisplayName: strings.Repeat("x", maxDisplayNameBytes+1)})},
		{"notes", validateCreate(&CreateContactInput{AddressBook: validID, DisplayName: "x", Notes: strings.Repeat("x", maxNotesBytes+1)})},
		{"emails", validateCreate(&CreateContactInput{AddressBook: validID, DisplayName: "x", Emails: make([]TypedValue, maxEmails+1)})},
		{"phones", validateCreate(&CreateContactInput{AddressBook: validID, DisplayName: "x", Phones: make([]TypedValue, maxPhones+1)})},
		{"urls", validateCreate(&CreateContactInput{AddressBook: validID, DisplayName: "x", URLs: make([]TypedValue, maxURLs+1)})},
		{"addresses", validateCreate(&CreateContactInput{AddressBook: validID, DisplayName: "x", Addresses: make([]PostalAddress, maxAddresses+1)})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { requireCode(t, test.err, CodeValidation) })
	}

	prefix := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:u\r\nFN:n\r\nN:n;;;;\r\nX-PAD:"
	suffix := "\r\nEND:VCARD\r\n"
	exact := []byte(prefix + strings.Repeat("x", maxCardBytes-len(prefix)-len(suffix)) + suffix)
	if _, _, err := decodeCard(exact); err != nil {
		t.Fatalf("exact card cap rejected: %v", err)
	}
	_, _, err := decodeCard(append(exact, 'x'))
	requireCode(t, err, CodePayloadTooLarge)

	opts := SearchOptions{Query: strings.Repeat("q", maxQueryBytes+1)}
	requireCode(t, validateSearchOptions(&opts), CodeValidation)
}

func TestCardDetailHasPhotoWithoutBytes(t *testing.T) {
	withPhoto := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:photo-card\r\nFN:Photo\r\nN:Photo;;;;\r\nPHOTO;ENCODING=b;TYPE=JPEG:QUJD\r\nEND:VCARD\r\n"
	card, version, err := decodeCard([]byte(withPhoto))
	if err != nil {
		t.Fatal(err)
	}
	detail := cardDetail(card, version, "book-test", `"etag"`)
	if !detail.HasPhoto {
		t.Fatal("expected hasPhoto true")
	}
	encoded, _ := json.Marshal(detail)
	if strings.Contains(string(encoded), "QUJD") {
		t.Fatalf("PHOTO bytes leaked into JSON: %s", encoded)
	}
	without := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:plain\r\nFN:Plain\r\nN:Plain;;;;\r\nEND:VCARD\r\n"
	card2, version2, err := decodeCard([]byte(without))
	if err != nil {
		t.Fatal(err)
	}
	if cardDetail(card2, version2, "book-test", `"etag"`).HasPhoto {
		t.Fatal("expected hasPhoto false")
	}
}

func TestStrictVCardFramingAndLogicalLines(t *testing.T) {
	valid := "BEGIN:VCARD\r\n" +
		"VERSION:3.0\r\n" +
		"UID:strict-card\r\n" +
		"FN:Strict Card\r\n" +
		"N:Card;Strict;;;\r\n" +
		"CATEGORIES:Friends,Work\\,VIP\r\n" +
		"PHOTO;ENCODING=b;TYPE=JPEG:QUJD\r\n" +
		" REVGRw==\r\n" +
		"item1.X-CUSTOM;X-PARAM=\"one,two\":opaque\r\n" +
		"END:VCARD\r\n"
	if _, version, err := decodeCard([]byte(valid)); err != nil || version != "3.0" {
		t.Fatalf("valid folded card = version %q, %v", version, err)
	}
	tests := []struct {
		name string
		card string
	}{
		{name: "malformed ignored line", card: strings.Replace(valid, "FN:Strict Card\r\n", "FN:Strict Card\r\nMALFORMED\r\n", 1)},
		{name: "orphan fold", card: " continuation\r\n" + valid},
		{name: "unterminated parameter", card: strings.Replace(valid, `X-PARAM="one,two"`, `X-PARAM="one,two`, 1)},
		{name: "blank logical line", card: strings.Replace(valid, "UID:strict-card\r\n", "UID:strict-card\r\n\r\n", 1)},
		{name: "second card", card: valid + valid},
		{name: "trailing data", card: valid + "X-TRAILING:value\r\n"},
		{name: "missing final line ending", card: strings.TrimSuffix(valid, "\r\n")},
		{name: "duplicate UID", card: strings.Replace(valid, "UID:strict-card\r\n", "UID:strict-card\r\nUID:second\r\n", 1)},
		{name: "missing FN", card: strings.Replace(valid, "FN:Strict Card\r\n", "", 1)},
		{name: "malformed N", card: strings.Replace(valid, "N:Card;Strict;;;\r\n", "N:Card;Strict\r\n", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := decodeCard([]byte(test.card))
			requireCode(t, err, CodeProtocolError)
		})
	}
}

type requestRecord struct {
	method      string
	path        string
	escapedPath string
	requestURI  string
	header      http.Header
	body        string
}

type crudFixture struct {
	mu           sync.Mutex
	card         string
	href         string
	etag         string
	putStatus    int
	deleteStatus int
	bookHref     string
	records      []requestRecord
}

func (fixture *crudFixture) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.records = append(fixture.records, requestRecord{
		method: req.Method, path: req.URL.Path, escapedPath: req.URL.EscapedPath(),
		requestURI: req.URL.RequestURI(), header: req.Header.Clone(), body: string(body),
	})
	switch {
	case req.Method == "PROPFIND" && req.URL.Path == "/":
		return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
	case req.Method == "PROPFIND" && req.URL.Path == "/principal/":
		return httpResponse(req, http.StatusMultiStatus, homesXML("/home/"), nil), nil
	case req.Method == "PROPFIND" && req.URL.Path == "/home/":
		bookHref := fixture.bookHref
		if bookHref == "" {
			bookHref = "/home/book/"
		}
		return httpResponse(req, http.StatusMultiStatus, booksXML(bookHref), nil), nil
	case req.Method == "REPORT":
		if fixture.card == "" {
			return httpResponse(req, http.StatusMultiStatus, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"/>`, nil), nil
		}
		return httpResponse(req, http.StatusMultiStatus, cardResponseXML(fixture.href, fixture.etag, fixture.card), nil), nil
	case req.Method == http.MethodGet:
		if fixture.card == "" || req.URL.Path != fixture.href {
			return httpResponse(req, http.StatusNotFound, "", nil), nil
		}
		headers := map[string]string{"Content-Type": "text/vcard"}
		if fixture.etag != "" {
			headers["ETag"] = fixture.etag
		}
		return httpResponse(req, http.StatusOK, fixture.card, headers), nil
	case req.Method == http.MethodPut:
		status := fixture.putStatus
		if status == 0 {
			status = http.StatusNoContent
		}
		if status/100 == 2 {
			fixture.card = string(body)
			fixture.href = req.URL.Path
			if req.Header.Get("If-None-Match") == "*" {
				fixture.etag = `"created"`
			} else {
				fixture.etag = `"v2"`
			}
		}
		return httpResponse(req, status, "", nil), nil
	case req.Method == http.MethodDelete:
		status := fixture.deleteStatus
		if status == 0 {
			status = http.StatusNoContent
		}
		return httpResponse(req, status, "", nil), nil
	default:
		return httpResponse(req, http.StatusNotFound, "", nil), nil
	}
}

func (fixture *crudFixture) recordsFor(method string) []requestRecord {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	var records []requestRecord
	for _, record := range fixture.records {
		if record.method == method {
			records = append(records, record)
		}
	}
	return records
}

func TestV3CreateUpdateDeleteAndOpaquePreservation(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		fixture := &crudFixture{bookHref: "/home/%62ook"}
		client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		result, err := client.CreateContact(context.Background(), &CreateContactInput{
			AddressBook: book.Identifier,
			Name:        StructuredName{GivenName: "Ada", FamilyName: "Lovelace"},
			Emails:      []TypedValue{{Type: "work", Value: "ada@example.com"}},
		})
		if err != nil {
			t.Fatalf("CreateContact() error = %v", err)
		}
		if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(result.UID) {
			t.Fatalf("generated UID = %q", result.UID)
		}
		puts := fixture.recordsFor(http.MethodPut)
		if len(puts) != 1 || puts[0].header.Get("If-None-Match") != "*" {
			t.Fatalf("create PUTs = %#v", puts)
		}
		if !strings.HasPrefix(puts[0].escapedPath, "/home/%62ook/") || !strings.HasSuffix(puts[0].escapedPath, ".vcf") || puts[0].requestURI != puts[0].escapedPath {
			t.Fatalf("opaque create target = escaped %q request URI %q", puts[0].escapedPath, puts[0].requestURI)
		}
		for _, required := range []string{"VERSION:3.0", "PRODID:", "UID:" + result.UID, "FN:Ada Lovelace", "N:Lovelace;Ada;;;"} {
			if !strings.Contains(puts[0].body, required) {
				t.Errorf("create body missing %q:\n%s", required, puts[0].body)
			}
		}
		if result.ETag != `"created"` {
			t.Fatalf("create ETag = %q", result.ETag)
		}
	})

	t.Run("update preserves unknown and photo", func(t *testing.T) {
		original := v3Card("logical", "Before", "NOTE:old", "PHOTO;ENCODING=b;TYPE=JPEG:AAEC", "X-CUSTOM;X-PARAM=one:opaque")
		fixture := &crudFixture{card: original, href: "/home/book/imported-name.vcf", etag: `"v1,part"`}
		client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		note := "new note"
		result, err := client.UpdateContact(context.Background(), &UpdateContactInput{
			AddressBook: book.Identifier,
			UID:         "logical",
			Patch:       ContactPatch{Notes: &note},
		})
		if err != nil {
			t.Fatalf("UpdateContact() error = %v", err)
		}
		puts := fixture.recordsFor(http.MethodPut)
		if len(puts) != 1 || puts[0].path != "/home/book/imported-name.vcf" || puts[0].header.Get("If-Match") != `"v1,part"` {
			t.Fatalf("update PUT = %#v", puts)
		}
		for _, preserved := range []string{"PHOTO;ENCODING=b;TYPE=JPEG:AAEC", "X-CUSTOM;X-PARAM=one:opaque", "NOTE:new note"} {
			if !strings.Contains(puts[0].body, preserved) {
				t.Errorf("update body missing %q:\n%s", preserved, puts[0].body)
			}
		}
		if result.ETag != `"v2"` {
			t.Fatalf("update ETag = %q", result.ETag)
		}
	})

	t.Run("update changes only one raw logical property", func(t *testing.T) {
		original := "BEGIN:VCARD\r\n" +
			"VERSION:3.0\r\n" +
			"UID:lossless\r\n" +
			"FN:Lossless Contact\r\n" +
			"N:Contact;Lossless;;;\r\n" +
			"CATEGORIES:Family,Friends\\,Close\r\n" +
			"item1.EMAIL;TYPE=INTERNET;X-AB=\"one,two\":lossless@example.com\r\n" +
			"item1.X-ABLabel:_$!<Home>!$_\r\n" +
			"PHOTO;ENCODING=b;TYPE=JPEG:QUJD\r\n" +
			" REVGRw==\r\n" +
			"X-ODD;X-PARAM=\"a;b,c\":opaque\\,value\r\n" +
			"NOTE:old\r\n" +
			"END:VCARD\r\n"
		fixture := &crudFixture{card: original, href: "/home/book/lossless.vcf", etag: `"raw-v1"`}
		client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		note := "new note"
		_, err := client.UpdateContact(context.Background(), &UpdateContactInput{
			AddressBook: book.Identifier,
			UID:         "lossless",
			Patch:       ContactPatch{Notes: &note},
		})
		if err != nil {
			t.Fatalf("UpdateContact() error = %v", err)
		}
		puts := fixture.recordsFor(http.MethodPut)
		if len(puts) != 1 {
			t.Fatalf("PUT count = %d, want 1", len(puts))
		}
		want := strings.Replace(original, "NOTE:old\r\n", "NOTE:new note\r\n", 1)
		if puts[0].body != want {
			t.Fatalf("patched vCard changed untouched raw properties\ngot:\n%s\nwant:\n%s", puts[0].body, want)
		}
	})

	t.Run("delete and dry run", func(t *testing.T) {
		fixture := &crudFixture{card: v3Card("delete-me", "Delete Me"), href: "/home/book/random.vcf", etag: `"d1"`}
		client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		dry, err := client.DeleteContact(context.Background(), &DeleteContactInput{AddressBook: book.Identifier, UID: "delete-me", DryRun: true})
		if err != nil || !dry.DryRun || len(fixture.recordsFor(http.MethodDelete)) != 0 {
			t.Fatalf("dry delete = %#v, %v", dry, err)
		}
		result, err := client.DeleteContact(context.Background(), &DeleteContactInput{AddressBook: book.Identifier, UID: "delete-me", ETag: `"caller"`})
		if err != nil || !result.WouldDelete {
			t.Fatalf("DeleteContact() = %#v, %v", result, err)
		}
		deletes := fixture.recordsFor(http.MethodDelete)
		if len(deletes) != 1 || deletes[0].path != "/home/book/random.vcf" || deletes[0].header.Get("If-Match") != `"caller"` {
			t.Fatalf("DELETE = %#v", deletes)
		}
	})
}

func TestV4ReadAndUpdateRejected(t *testing.T) {
	card := "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:v4-contact\r\nFN:Version Four\r\nN:Four;Version;;;\r\nBDAY:--0203\r\nEND:VCARD\r\n"
	fixture := &crudFixture{card: card, href: "/home/book/v4-random.vcf", etag: `"v4"`}
	client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	detail, err := client.GetContact(context.Background(), book.Identifier, "v4-contact")
	if err != nil {
		t.Fatalf("GetContact(v4) error = %v", err)
	}
	if detail.Version != "4.0" || len(detail.UnsupportedFields) != 1 || detail.UnsupportedFields[0] != "birthday" {
		t.Fatalf("v4 detail = %#v", detail)
	}
	title := "No write"
	_, err = client.UpdateContact(context.Background(), &UpdateContactInput{AddressBook: book.Identifier, UID: "v4-contact", Patch: ContactPatch{Title: &title}})
	requireCode(t, err, CodeValidation)
	if len(fixture.recordsFor(http.MethodPut)) != 0 {
		t.Fatal("v4 update issued PUT")
	}
}

func TestStrongETagValidationAnd412Mappings(t *testing.T) {
	valid := []string{`""`, `"simple"`, `"opaque\tag"`, `"comma,inside"`, " \t\"ows\"\t ", "\"obs-\x80\""}
	for _, value := range valid {
		if got, err := parseStrongETag(value); err != nil || got == "" {
			t.Errorf("valid ETag %q = %q, %v", value, got, err)
		}
	}
	invalid := []string{
		" ",
		"*",
		"bare",
		`W/"weak"`,
		`"one", "two"`,
		`"unterminated`,
		`"embedded"quote"`,
		"\"space inside\"",
		"\"tab\tinside\"",
		"\"delete\x7finside\"",
		"\"line\ninside\"",
		`"` + strings.Repeat("x", maxETagBytes) + `"`,
	}
	for _, value := range invalid {
		if _, err := parseStrongETag(value); err == nil {
			t.Errorf("invalid ETag %q was accepted", value)
		}
	}

	t.Run("invalid caller tags rejected before network", func(t *testing.T) {
		for _, etag := range invalid {
			var calls atomic.Int32
			client := NewClient(doerFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return httpResponse(req, http.StatusInternalServerError, "", nil), nil
			}), "https://contacts.icloud.com/", allowContactsHost)
			title := "x"
			_, err := client.UpdateContact(context.Background(), &UpdateContactInput{AddressBook: "book-AAAAAAAAAAAAAAAAAAAAAA", UID: "u", ETag: etag, Patch: ContactPatch{Title: &title}})
			requireCode(t, err, CodeValidation)
			if calls.Load() != 0 {
				t.Fatalf("ETag %q caused %d HTTP calls, want 0", etag, calls.Load())
			}
		}
	})

	t.Run("weak server ETag fails closed", func(t *testing.T) {
		fixture := &crudFixture{card: v3Card("u", "User"), href: "/home/book/u-random.vcf", etag: `W/"v1"`}
		client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		title := "x"
		_, err := client.UpdateContact(context.Background(), &UpdateContactInput{AddressBook: book.Identifier, UID: "u", Patch: ContactPatch{Title: &title}})
		requireCode(t, err, CodeProtocolError)
		if len(fixture.recordsFor(http.MethodPut)) != 0 {
			t.Fatal("weak ETag update issued PUT")
		}
	})

	t.Run("duplicate REPORT ETags fail closed", func(t *testing.T) {
		card := v3Card("u", "User")
		body := cardResponseXML("/home/book/u.vcf", `"one"`, card)
		body = strings.Replace(body, `<D:getetag>&#34;one&#34;</D:getetag>`, `<D:getetag>&#34;one&#34;</D:getetag><D:getetag>&#34;two&#34;</D:getetag>`, 1)
		client := NewClient(discoveryDoer(body), "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		_, err := client.SearchContacts(context.Background(), SearchOptions{AddressBook: book.Identifier})
		requireCode(t, err, CodeProtocolError)
	})

	t.Run("duplicate GET ETag fields fail closed", func(t *testing.T) {
		card := v3Card("u", "User")
		base := discoveryDoer(cardResponseXML("/home/book/u.vcf", `"report"`, card))
		client := NewClient(doerFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				response := httpResponse(req, http.StatusOK, card, map[string]string{"Content-Type": "text/vcard"})
				response.Header.Add("ETag", `"one"`)
				response.Header.Add("ETag", `"two"`)
				return response, nil
			}
			return base(req)
		}), "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		_, err := client.GetContact(context.Background(), book.Identifier, "u")
		requireCode(t, err, CodeProtocolError)
	})

	t.Run("create 412 is conflict", func(t *testing.T) {
		fixture := &crudFixture{putStatus: http.StatusPreconditionFailed}
		client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		_, err := client.CreateContact(context.Background(), &CreateContactInput{AddressBook: book.Identifier, DisplayName: "Conflict"})
		requireCode(t, err, CodeConflict)
	})

	t.Run("update and delete 412 are concurrent modification", func(t *testing.T) {
		for _, method := range []string{"update", "delete"} {
			t.Run(method, func(t *testing.T) {
				fixture := &crudFixture{card: v3Card("u", "User"), href: "/home/book/random.vcf", etag: `"v1"`}
				if method == "update" {
					fixture.putStatus = http.StatusPreconditionFailed
				} else {
					fixture.deleteStatus = http.StatusPreconditionFailed
				}
				client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
				book := discoveredBook(t, client)
				var err error
				if method == "update" {
					title := "changed"
					_, err = client.UpdateContact(context.Background(), &UpdateContactInput{AddressBook: book.Identifier, UID: "u", Patch: ContactPatch{Title: &title}})
				} else {
					_, err = client.DeleteContact(context.Background(), &DeleteContactInput{AddressBook: book.Identifier, UID: "u"})
				}
				requireCode(t, err, CodeConcurrentModification)
			})
		}
	})
}

func TestChildResourceURLPreservesOpaqueEscapedCollection(t *testing.T) {
	collection, err := url.Parse("https://contacts.icloud.com/home/%62ook")
	if err != nil {
		t.Fatal(err)
	}
	child := childResourceURL(collection, "contact.vcf")
	if got, want := child.EscapedPath(), "/home/%62ook/contact.vcf"; got != want {
		t.Fatalf("child escaped path = %q, want %q", got, want)
	}
	if !urlWithin(child, collection, false) {
		t.Fatal("opaque child was not contained by its collection")
	}
}

func TestDeleteDryRunDoesNotRequireServerStrongETag(t *testing.T) {
	fixture := &crudFixture{card: v3Card("u", "User"), href: "/home/book/u.vcf"}
	client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	result, err := client.DeleteContact(context.Background(), &DeleteContactInput{
		AddressBook: book.Identifier,
		UID:         "u",
		DryRun:      true,
	})
	if err != nil || !result.DryRun || !result.WouldDelete {
		t.Fatalf("DeleteContact(dry run) = %#v, %v", result, err)
	}
	if len(fixture.recordsFor(http.MethodDelete)) != 0 {
		t.Fatal("dry run issued DELETE")
	}

	_, err = client.DeleteContact(context.Background(), &DeleteContactInput{
		AddressBook: book.Identifier,
		UID:         "u",
	})
	requireCode(t, err, CodeProtocolError)

	weakFixture := &crudFixture{card: v3Card("u", "User"), href: "/home/book/u.vcf", etag: `W/"v1"`}
	weakClient := NewClient(weakFixture, "https://contacts.icloud.com/", allowContactsHost)
	weakBook := discoveredBook(t, weakClient)
	_, err = weakClient.DeleteContact(context.Background(), &DeleteContactInput{
		AddressBook: weakBook.Identifier,
		UID:         "u",
		DryRun:      true,
	})
	requireCode(t, err, CodeProtocolError)
	if len(weakFixture.recordsFor(http.MethodDelete)) != 0 {
		t.Fatal("invalid ETag dry run issued DELETE")
	}
}

func TestDeleteDryRunComparesCallerETag(t *testing.T) {
	fixture := &crudFixture{card: v3Card("u", "User"), href: "/home/book/u.vcf", etag: `"server"`}
	client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	_, err := client.DeleteContact(context.Background(), &DeleteContactInput{
		AddressBook: book.Identifier,
		UID:         "u",
		ETag:        `"caller"`,
		DryRun:      true,
	})
	requireCode(t, err, CodeConcurrentModification)
	if len(fixture.recordsFor(http.MethodDelete)) != 0 {
		t.Fatal("mismatched dry-run ETag issued DELETE")
	}
}

func TestAddressBookAndPerCardSafety(t *testing.T) {
	var calls atomic.Int32
	client := NewClient(doerFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return httpResponse(req, http.StatusInternalServerError, "", nil), nil
	}), "https://contacts.icloud.com/", allowContactsHost)
	_, err := client.GetContact(context.Background(), "https://contacts.icloud.com/home/book/", "uid")
	requireCode(t, err, CodeValidation)
	if calls.Load() != 0 {
		t.Fatalf("caller URL caused %d network calls", calls.Load())
	}

	oversized := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:u\r\nFN:n\r\nN:n;;;;\r\nNOTE:" + strings.Repeat("x", maxCardBytes) + "\r\nEND:VCARD\r\n"
	report := cardResponseXML("/home/book/u.vcf", `"e"`, oversized)
	largeClient := NewClient(discoveryDoer(report), "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, largeClient)
	_, err = largeClient.SearchContacts(context.Background(), SearchOptions{AddressBook: book.Identifier})
	requireCode(t, err, CodePayloadTooLarge)
}

func TestUnknownBookStopsBeforeOperationAndSemaphoreCapsDAV(t *testing.T) {
	t.Run("unknown book", func(t *testing.T) {
		var reports atomic.Int32
		base := discoveryDoer(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"/>`)
		doer := doerFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == "REPORT" {
				reports.Add(1)
			}
			return base(req)
		})
		client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		unknown := book.Identifier[:len(book.Identifier)-1] + "A"
		if unknown == book.Identifier {
			unknown = book.Identifier[:len(book.Identifier)-1] + "B"
		}
		_, err := client.SearchContacts(context.Background(), SearchOptions{AddressBook: unknown})
		requireCode(t, err, CodeValidation)
		if reports.Load() != 0 {
			t.Fatalf("unknown book issued %d REPORT requests", reports.Load())
		}
	})

	t.Run("four DAV requests", func(t *testing.T) {
		gate := make(chan struct{})
		var active atomic.Int32
		var maximum atomic.Int32
		base := discoveryDoer("")
		doer := doerFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != "REPORT" {
				return base(req)
			}
			current := active.Add(1)
			for {
				old := maximum.Load()
				if current <= old || maximum.CompareAndSwap(old, current) {
					break
				}
			}
			<-gate
			active.Add(-1)
			return httpResponse(req, http.StatusMultiStatus, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"/>`, nil), nil
		})
		client := NewClient(doer, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		var wg sync.WaitGroup
		errors := make(chan error, 6)
		for range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := client.SearchContacts(context.Background(), SearchOptions{AddressBook: book.Identifier})
				errors <- err
			}()
		}
		deadline := time.After(2 * time.Second)
		for maximum.Load() < 4 {
			select {
			case <-deadline:
				close(gate)
				wg.Wait()
				t.Fatalf("maximum concurrent DAV requests = %d, want 4", maximum.Load())
			default:
				time.Sleep(time.Millisecond)
			}
		}
		close(gate)
		wg.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				t.Errorf("SearchContacts() error = %v", err)
			}
		}
		if maximum.Load() != 4 {
			t.Fatalf("maximum concurrent DAV requests = %d, want 4", maximum.Load())
		}
	})
}
