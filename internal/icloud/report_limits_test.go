package icloud

import (
	"net/http"
	"strings"
	"testing"
)

func TestReportMultiStatusStreamingLimits(t *testing.T) {
	t.Run("object cap plus one", func(t *testing.T) {
		var data strings.Builder
		data.WriteString(`<D:multistatus xmlns:D="DAV:">`)
		for range maxReportResponses + 1 {
			data.WriteString(`<D:response/>`)
		}
		data.WriteString(`</D:multistatus>`)
		_, err := decodeReportMultiStatus([]byte(data.String()), http.StatusMultiStatus)
		requireICloudCode(t, err, CodePayloadTooLarge)
	})

	t.Run("property cap plus one", func(t *testing.T) {
		var data strings.Builder
		data.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:X="urn:test"><D:response><D:propstat><D:prop>`)
		for range maxReportPropertyCount + 1 {
			data.WriteString(`<X:p/>`)
		}
		data.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
		_, err := decodeReportMultiStatus([]byte(data.String()), http.StatusMultiStatus)
		requireICloudCode(t, err, CodePayloadTooLarge)
	})

	t.Run("depth cap plus one", func(t *testing.T) {
		data := `<D:multistatus xmlns:D="DAV:" xmlns:X="urn:test">` + strings.Repeat(`<X:n>`, maxReportXMLDepth) + strings.Repeat(`</X:n>`, maxReportXMLDepth) + `</D:multistatus>`
		_, err := decodeReportMultiStatus([]byte(data), http.StatusMultiStatus)
		requireICloudCode(t, err, CodePayloadTooLarge)
	})
}

func TestReportMultiStatusDecodesResponsesIndividually(t *testing.T) {
	data := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		`<D:response><D:href>one.ics</D:href><D:propstat><D:prop><D:getetag>&quot;one&quot;</D:getetag><C:calendar-data>BEGIN:VCALENDAR&#13;&#10;END:VCALENDAR&#13;&#10;</C:calendar-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`<D:response><D:href>two.ics</D:href><D:propstat><D:prop><D:getetag>&quot;two&quot;</D:getetag></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response>` +
		`</D:multistatus>`
	responses, err := decodeReportMultiStatus([]byte(data), http.StatusMultiStatus)
	if err != nil {
		t.Fatal(err)
	}
	if len(responses) != 2 || responses[0].Href != "one.ics" || responses[1].Href != "two.ics" {
		t.Fatalf("responses = %#v", responses)
	}
	if prop := mergedOKProp(responses[0]); prop == nil || prop.CalendarData == "" {
		t.Fatalf("first response properties = %#v", prop)
	}
}
