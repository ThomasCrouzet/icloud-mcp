package icloud

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	davXMLNamespace        = "DAV:"
	maxReportXMLDepth      = 32
	maxReportXMLTokens     = 262144
	maxReportResponses     = 4096
	maxReportPropstats     = 16384
	maxReportPropertyCount = 32768
)

// encoding/xml structures for the PROPFIND responses (207 Multi-Status) of
// the hand-rolled discovery and of list_calendars. Namespace note:
// encoding/xml matches an unqualified tag regardless of namespace; the
// non-DAV props (CalDAV, Apple) are qualified to remove any ambiguity with
// same-named properties.
type msMultistatus struct {
	XMLName   xml.Name     `xml:"DAV: multistatus"`
	Responses []msResponse `xml:"response"`
}

type msResponse struct {
	Href      string       `xml:"href"`
	Propstats []msPropstat `xml:"propstat"`
}

type msPropstat struct {
	Status string `xml:"status"`
	Prop   msProp `xml:"prop"`
}

type msProp struct {
	CurrentUserPrincipal *msHref         `xml:"current-user-principal"`
	CalendarHomeSet      *msHref         `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
	DisplayName          string          `xml:"displayname"`
	CalendarDescription  string          `xml:"urn:ietf:params:xml:ns:caldav calendar-description"`
	CalendarColor        string          `xml:"http://apple.com/ns/ical/ calendar-color"`
	ResourceType         *msResourceType `xml:"resourcetype"`
	SupportedComps       *msSupportedSet `xml:"urn:ietf:params:xml:ns:caldav supported-calendar-component-set"`
	CalendarData         string          `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
	GetETags             []string        `xml:"getetag"`
}

type msHref struct {
	Href string `xml:"href"`
}

type msResourceType struct {
	Calendar       *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
	ScheduleInbox  *struct{} `xml:"urn:ietf:params:xml:ns:caldav schedule-inbox"`
	ScheduleOutbox *struct{} `xml:"urn:ietf:params:xml:ns:caldav schedule-outbox"`
}

type msSupportedSet struct {
	Comps []struct {
		Name string `xml:"name,attr"`
	} `xml:"urn:ietf:params:xml:ns:caldav comp"`
}

func decodeReportMultiStatus(data []byte, status int) ([]msResponse, error) {
	if err := validateReportMultiStatus(data, status); err != nil {
		return nil, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	responses := make([]msResponse, 0)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return responses, nil
		}
		if err != nil {
			return nil, malformedReportError(status)
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 && value.Name.Space == davXMLNamespace && value.Name.Local == "response" {
				var response msResponse
				if err := decoder.DecodeElement(&response, &value); err != nil {
					return nil, malformedReportError(status)
				}
				responses = append(responses, response)
				depth--
			}
		case xml.EndElement:
			depth--
		}
	}
}

// validateReportMultiStatus caps the aggregate XML shape before any response
// is decoded. The second pass then materializes one response at a time instead
// of unmarshalling the complete multistatus tree.
func validateReportMultiStatus(data []byte, status int) error {
	return validateDAVMultiStatus(data, status, "REPORT")
}

func validatePropfindMultiStatus(data []byte, status int) error {
	return validateDAVMultiStatus(data, status, "PROPFIND")
}

func validateDAVMultiStatus(data []byte, status int, operation string) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	tokens := 0
	responses := 0
	propstats := 0
	properties := 0
	propDepth := 0
	rootSeen := false
	rootClosed := false

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if !rootSeen || !rootClosed || depth != 0 {
				return malformedDAVError(status, operation)
			}
			return nil
		}
		if err != nil {
			return malformedDAVError(status, operation)
		}
		tokens++
		if tokens > maxReportXMLTokens {
			return davLimitError(status, operation, "token")
		}

		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if depth > maxReportXMLDepth {
				return davLimitError(status, operation, "depth")
			}
			if rootClosed {
				return malformedDAVError(status, operation)
			}
			if !rootSeen {
				if value.Name.Space != davXMLNamespace || value.Name.Local != "multistatus" {
					return malformedDAVError(status, operation)
				}
				rootSeen = true
				continue
			}
			if value.Name.Space == davXMLNamespace && value.Name.Local == "response" {
				responses++
				if responses > maxReportResponses {
					return davLimitError(status, operation, "object")
				}
			}
			if value.Name.Space == davXMLNamespace && value.Name.Local == "propstat" {
				propstats++
				if propstats > maxReportPropstats {
					return davLimitError(status, operation, "propstat")
				}
			}
			if propDepth != 0 && depth == propDepth+1 {
				properties++
				if properties > maxReportPropertyCount {
					return davLimitError(status, operation, "property")
				}
			}
			if value.Name.Space == davXMLNamespace && value.Name.Local == "prop" && propDepth == 0 {
				propDepth = depth
			}
		case xml.EndElement:
			if propDepth == depth && value.Name.Space == davXMLNamespace && value.Name.Local == "prop" {
				propDepth = 0
			}
			if depth == 1 && value.Name.Space == davXMLNamespace && value.Name.Local == "multistatus" {
				rootClosed = true
			}
			depth--
		case xml.CharData:
			if (!rootSeen || rootClosed) && len(bytes.TrimSpace(value)) != 0 {
				return malformedDAVError(status, operation)
			}
		}
	}
}

func davLimitError(status int, operation, kind string) error {
	return NewError(CodePayloadTooLarge, status, "Calendar "+operation+" XML exceeded its "+kind+" limit", nil)
}

func malformedReportError(status int) error {
	return malformedDAVError(status, "REPORT")
}

func malformedDAVError(status int, operation string) error {
	return NewError(CodeProtocolError, status, "Calendar "+operation+" response is malformed", nil)
}

// isOKStatus reports whether a propstat status line indicates success
// (e.g. "HTTP/1.1 200 OK").
func isOKStatus(status string) bool {
	code, ok := parseDAVStatus(status)
	return ok && code == http.StatusOK
}

func parseDAVStatus(status string) (int, bool) {
	status = strings.TrimSpace(status)
	if status == "" || strings.ContainsAny(status, "\r\n") {
		return 0, false
	}
	version, rest, ok := strings.Cut(status, " ")
	if !ok {
		return 0, false
	}
	if _, _, ok := http.ParseHTTPVersion(version); !ok {
		return 0, false
	}
	codeText, reason, ok := strings.Cut(rest, " ")
	if !ok || len(codeText) != 3 || reason == "" {
		return 0, false
	}
	for i := range len(codeText) {
		if codeText[i] < '0' || codeText[i] > '9' {
			return 0, false
		}
	}
	for _, r := range reason {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return 0, false
		}
	}
	code, err := strconv.Atoi(codeText)
	return code, err == nil
}

// mergedOKProp merges all successful (200) propstats of a response into a
// single msProp. In practice a CalDAV server returns a single 200 block
// grouping all found properties and a 404 block for the missing ones, but
// merging is done for robustness in case several 200 blocks exist. Returns
// nil if no propstat is successful.
func mergedOKProp(r msResponse) *msProp {
	var merged msProp
	found := false
	for _, ps := range r.Propstats {
		if !isOKStatus(ps.Status) {
			continue
		}
		found = true
		if ps.Prop.CurrentUserPrincipal != nil {
			merged.CurrentUserPrincipal = ps.Prop.CurrentUserPrincipal
		}
		if ps.Prop.CalendarHomeSet != nil {
			merged.CalendarHomeSet = ps.Prop.CalendarHomeSet
		}
		if ps.Prop.DisplayName != "" {
			merged.DisplayName = ps.Prop.DisplayName
		}
		if ps.Prop.CalendarDescription != "" {
			merged.CalendarDescription = ps.Prop.CalendarDescription
		}
		if ps.Prop.CalendarColor != "" {
			merged.CalendarColor = ps.Prop.CalendarColor
		}
		if ps.Prop.ResourceType != nil {
			merged.ResourceType = ps.Prop.ResourceType
		}
		if ps.Prop.SupportedComps != nil {
			merged.SupportedComps = ps.Prop.SupportedComps
		}
		if ps.Prop.CalendarData != "" {
			merged.CalendarData = ps.Prop.CalendarData
		}
		if len(ps.Prop.GetETags) > 0 {
			merged.GetETags = append(merged.GetETags, ps.Prop.GetETags...)
		}
	}
	if !found {
		return nil
	}
	return &merged
}

// principalFromMultistatus extracts the current-user-principal href from
// the discovery step 1 response.
func principalFromMultistatus(ms *msMultistatus) string {
	for _, r := range ms.Responses {
		prop := mergedOKProp(r)
		if prop != nil && prop.CurrentUserPrincipal != nil && prop.CurrentUserPrincipal.Href != "" {
			return prop.CurrentUserPrincipal.Href
		}
	}
	return ""
}

// homeSetFromMultistatus extracts the calendar-home-set href from the
// discovery step 2 response.
func homeSetFromMultistatus(ms *msMultistatus) string {
	for _, r := range ms.Responses {
		prop := mergedOKProp(r)
		if prop != nil && prop.CalendarHomeSet != nil && prop.CalendarHomeSet.Href != "" {
			return prop.CalendarHomeSet.Href
		}
	}
	return ""
}
