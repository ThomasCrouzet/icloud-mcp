package icloud

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const completeReportEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTAMP:20260701T000000Z\r\nDTSTART:20260701T090000Z\r\nDTEND:20260701T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func completenessResponseXML(resources ...string) string {
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		strings.Join(resources, "") + `</D:multistatus>`
}

func reportResource(href, status, calendarData string) string {
	responseStatus := ""
	if status != "" {
		responseStatus = `<D:status>` + status + `</D:status>`
	}
	prop := `<D:getetag>&quot;e&quot;</D:getetag>`
	if calendarData != "" {
		prop += `<C:calendar-data>` + xmlEscapeText(calendarData) + `</C:calendar-data>`
	}
	return `<D:response><D:href>` + href + `</D:href>` + responseStatus +
		`<D:propstat><D:prop>` + prop + `</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
}

func xmlEscapeText(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "&#13;", "\n", "&#10;")
	return replacer.Replace(value)
}

func TestCalendarReportRejectsIncompleteResources(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "failed property status",
			body: completenessResponseXML(
				reportResource("/cal/a/one.ics", "", completeReportEvent),
				`<D:response><D:href>/cal/a/two.ics</D:href><D:propstat><D:prop><D:getetag>&quot;e&quot;</D:getetag></D:prop><D:status>HTTP/1.1 403 Forbidden</D:status></D:propstat></D:response>`,
			),
		},
		{
			name: "missing calendar data",
			body: completenessResponseXML(
				reportResource("/cal/a/one.ics", "", completeReportEvent),
				reportResource("/cal/a/two.ics", "", ""),
			),
		},
		{
			name: "response level status",
			body: completenessResponseXML(
				reportResource("/cal/a/one.ics", "", completeReportEvent),
				`<D:response><D:href>/cal/a/two.ics</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>`,
			),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			doer := calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
				return calendarTestResponse(req, http.StatusMultiStatus, nil, strings.NewReader(test.body)), nil
			})
			client := NewClient(doer, "https://caldav.icloud.com", func(host string) bool { return host == "p01-caldav.icloud.com" })
			client.shardBase = "https://p01-caldav.icloud.com"
			_, err := client.reportCalendarQuery(context.Background(), "/cal/a/", `<C:filter/>`)
			requireICloudCode(t, err, CodePartialFailure)
		})
	}
}

func TestCalendarReportAcceptsEmptyAndCompleteMultistatus(t *testing.T) {
	emptyDoer := calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		return calendarTestResponse(req, http.StatusMultiStatus, nil, strings.NewReader(completenessResponseXML())), nil
	})
	client := NewClient(emptyDoer, "https://caldav.icloud.com", func(host string) bool { return host == "p01-caldav.icloud.com" })
	client.shardBase = "https://p01-caldav.icloud.com"
	objects, err := client.reportCalendarQuery(context.Background(), "/cal/a/", `<C:filter/>`)
	if err != nil || len(objects) != 0 {
		t.Fatalf("empty multistatus = %#v, %v", objects, err)
	}

	completeDoer := calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		body := completenessResponseXML(
			reportResource("/cal/a/one.ics", "", completeReportEvent),
			reportResource("/cal/a/two.ics", "", strings.Replace(completeReportEvent, "UID:one", "UID:two", 1)),
		)
		return calendarTestResponse(req, http.StatusMultiStatus, nil, strings.NewReader(body)), nil
	})
	client = NewClient(completeDoer, "https://caldav.icloud.com", func(host string) bool { return host == "p01-caldav.icloud.com" })
	client.shardBase = "https://p01-caldav.icloud.com"
	objects, err = client.reportCalendarQuery(context.Background(), "/cal/a/", `<C:filter/>`)
	if err != nil || len(objects) != 2 {
		t.Fatalf("complete multistatus = %d objects, %v", len(objects), err)
	}
}
