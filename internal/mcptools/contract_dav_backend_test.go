package mcptools

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/emersion/go-vcard"
)

const contractCalendar = "/cal/allowed/"
const contractBookPath = "/books/main/"
const contractETag = `"v1"`

type contractDAVBackend struct {
	mu               sync.Mutex
	card             bool
	objects          map[string]string
	requests         []contractDAVRequest
	mutations        int
	collection       string
	failureStatus    int
	extraCollections []string
}

type contractDAVRequest struct{ Method, Path, Body, IfMatch, IfNoneMatch string }

func newContractDAV(card bool) *contractDAVBackend {
	b := &contractDAVBackend{card: card, objects: map[string]string{}, collection: contractCalendar}
	if card {
		b.collection = contractBookPath
	}
	if card {
		b.objects[contractBookPath+"opaque.vcf"] = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:contact\r\nFN:Synthetic Person\r\nN:Person;Synthetic;;;\r\nEMAIL:reply@example.invalid\r\nTEL:+331234\r\nNOTE:Original notes\r\nEND:VCARD\r\n"
	} else {
		b.objects[contractCalendar+"event.ics"] = strings.ReplaceAll("BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//Contract fixture//EN\nBEGIN:VEVENT\nUID:event\nDTSTAMP:20260701T000000Z\nDTSTART:20260701T100000Z\nDTEND:20260701T110000Z\nSUMMARY:Synthetic event\nLOCATION:Room\nDESCRIPTION:Original notes\nSTATUS:CONFIRMED\nTRANSP:OPAQUE\nRRULE:FREQ=DAILY;COUNT=2\nEND:VEVENT\nEND:VCALENDAR\n", "\n", "\r\n")
	}
	return b
}

func contractEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func contractMulti(rows string) string {
	return `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:A="urn:ietf:params:xml:ns:carddav">` + rows + `</D:multistatus>`
}
func contractRow(href, props string) string {
	return `<D:response><D:href>` + contractEscape(href) + `</D:href><D:propstat><D:prop>` + props + `</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
}

func (b *contractDAVBackend) Do(req *http.Request) (*http.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var data []byte
	if req.Body != nil {
		var err error
		data, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}
	body := string(data)
	path := req.URL.Path
	b.requests = append(b.requests, contractDAVRequest{req.Method, path, body, req.Header.Get("If-Match"), req.Header.Get("If-None-Match")})
	response := func(status int, body string) *http.Response {
		contentType := "application/xml"
		if req.Method == http.MethodGet {
			contentType = "text/calendar; charset=utf-8"
			if b.card {
				contentType = "text/vcard; charset=utf-8"
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}, "Etag": []string{contractETag}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
	}
	expectedHost := "caldav.icloud.com"
	if b.card {
		expectedHost = "contacts.icloud.com"
	}
	if req.URL.Scheme != "https" || req.URL.Host != expectedHost {
		return nil, fmt.Errorf("unexpected synthetic DAV authority")
	}
	if b.failureStatus != 0 {
		return response(b.failureStatus, ""), nil
	}
	switch req.Method {
	case "PROPFIND":
		switch path {
		case "/":
			return response(207, contractMulti(contractRow("/", `<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>`))), nil
		case "/principal/":
			prop := `<C:calendar-home-set><D:href>/cal/</D:href></C:calendar-home-set>`
			if b.card {
				prop = `<A:addressbook-home-set><D:href>/books/</D:href></A:addressbook-home-set>`
			}
			return response(207, contractMulti(contractRow(path, prop))), nil
		case "/cal/":
			props := `<D:resourcetype><D:collection/><C:calendar/></D:resourcetype><D:displayname>Synthetic</D:displayname><C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>`
			rows := contractRow(b.collection, props)
			for _, extra := range b.extraCollections {
				rows += contractRow(extra, props)
			}
			return response(207, contractMulti(rows)), nil
		case "/books/":
			props := `<D:resourcetype><D:collection/><A:addressbook/></D:resourcetype><D:displayname>Synthetic</D:displayname><A:supported-address-data><A:address-data-type content-type="text/vcard" version="3.0"/></A:supported-address-data>`
			rows := contractRow(contractBookPath, props)
			for _, extra := range b.extraCollections {
				rows += contractRow(extra, strings.ReplaceAll(props, "Synthetic", "Other"))
			}
			return response(207, contractMulti(rows)), nil
		}
	case "REPORT":
		collection := b.collection
		property := "C:calendar-data"
		if b.card {
			property = "A:address-data"
		}
		permitted := path == collection
		for _, extra := range b.extraCollections {
			permitted = permitted || path == extra
		}
		if !permitted {
			return response(404, ""), nil
		}
		keys := make([]string, 0, len(b.objects))
		for key := range b.objects {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var rows strings.Builder
		lookup := ""
		decoder := xml.NewDecoder(strings.NewReader(body))
		uidFilter := false
		for {
			token, err := decoder.Token()
			if err != nil {
				break
			}
			if start, ok := token.(xml.StartElement); ok {
				if start.Name.Local == "prop-filter" {
					uidFilter = false
					for _, a := range start.Attr {
						if a.Name.Local == "name" && a.Value == "UID" {
							uidFilter = true
						}
					}
				}
				if start.Name.Local == "text-match" && uidFilter {
					_ = decoder.DecodeElement(&lookup, &start)
				}
			}
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, path) {
				continue
			}
			if b.card && lookup != "" {
				card, err := vcard.NewDecoder(strings.NewReader(b.objects[key])).Decode()
				if err != nil {
					return nil, err
				}
				if card.Value(vcard.FieldUID) != lookup {
					continue
				}
			}
			rows.WriteString(contractRow(key, `<D:getetag>`+contractEscape(contractETag)+`</D:getetag><`+property+`>`+contractEscape(b.objects[key])+`</`+property+`>`))
		}
		return response(207, contractMulti(rows.String())), nil
	case http.MethodGet:
		if object, ok := b.objects[path]; ok {
			return response(200, object), nil
		}
		return response(404, ""), nil
	case http.MethodPut:
		_, exists := b.objects[path]
		if exists && req.Header.Get("If-Match") != contractETag || !exists && req.Header.Get("If-None-Match") != "*" {
			return response(412, ""), nil
		}
		b.mutations++
		b.objects[path] = body
		status := 201
		if exists {
			status = 204
		}
		return response(status, ""), nil
	case http.MethodDelete:
		if _, exists := b.objects[path]; !exists {
			return response(404, ""), nil
		}
		if req.Header.Get("If-Match") != contractETag {
			return response(412, ""), nil
		}
		b.mutations++
		delete(b.objects, path)
		return response(204, ""), nil
	}
	return nil, fmt.Errorf("unexpected synthetic DAV request %s %s", req.Method, path)
}

func (b *contractDAVBackend) snapshot() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{"requests": append([]contractDAVRequest(nil), b.requests...), "mutations": b.mutations}
}
