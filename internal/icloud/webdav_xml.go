package icloud

import (
	"encoding/xml"
	"strings"
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

// isOKStatus reports whether a propstat status line indicates success
// (e.g. "HTTP/1.1 200 OK").
func isOKStatus(status string) bool {
	return strings.Contains(status, " 200 ") || strings.HasSuffix(strings.TrimSpace(status), "200 OK")
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
