package icloud

import (
	"context"
	"fmt"
	"strings"
)

// propfindCalendarsBody requests resourcetype/displayname/description/
// Apple color/supported components on the calendar-home-set (Depth: 1).
// go-webdav v0.7.0 (caldav.Calendar) does not expose the color: a
// hand-rolled PROPFIND is required for list_calendars.
const propfindCalendarsBody = `<?xml version="1.0" encoding="UTF-8"?>
<A:propfind xmlns:A="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:IC="http://apple.com/ns/ical/">
  <A:prop>
    <A:resourcetype/>
    <A:displayname/>
    <C:calendar-description/>
    <C:supported-calendar-component-set/>
    <IC:calendar-color/>
  </A:prop>
</A:propfind>`

// ListCalendars lists the account's calendars, filtering out technical
// collections (schedule-inbox/outbox, notifications) and VTODO-only
// collections (leftover Reminders).
func (c *Client) ListCalendars(ctx context.Context) ([]Calendar, error) {
	if err := c.discover(ctx); err != nil {
		return nil, err
	}

	target, err := resolvePathOnBase(c.shardBase, c.homeSetPath)
	if err != nil {
		return nil, NewError(CodeProtocolError, 0, "discovered Calendar home-set URL is invalid", nil)
	}
	response, err := c.propfind(ctx, target, "1", propfindCalendarsBody, c.homeSetPath)
	if err != nil {
		return nil, fmt.Errorf("listing calendars: %w", err)
	}

	var out []Calendar
	for _, r := range response.multistatus.Responses {
		prop := mergedOKProp(r)
		if prop == nil {
			continue
		}
		if prop.ResourceType == nil || prop.ResourceType.Calendar == nil {
			continue // not a calendar collection
		}
		if prop.ResourceType.ScheduleInbox != nil || prop.ResourceType.ScheduleOutbox != nil {
			continue
		}
		resolved, err := c.resolveDAVHref(response.url, r.Href, c.homeSetPath)
		if err != nil {
			return nil, err
		}
		path := resolved.EscapedPath()
		if strings.Contains(path, "/inbox") || strings.Contains(path, "/outbox") || strings.Contains(path, "/notification") {
			continue
		}
		if prop.SupportedComps != nil && !supportsVEvent(prop.SupportedComps) {
			continue // VTODO-only (Reminders) or another component set without VEVENT
		}
		out = append(out, Calendar{
			Path:        path,
			Name:        prop.DisplayName,
			Description: prop.CalendarDescription,
			Color:       prop.CalendarColor,
		})
	}
	return out, nil
}

func supportsVEvent(s *msSupportedSet) bool {
	if len(s.Comps) == 0 {
		// Property present but empty: do not filter overzealously.
		return true
	}
	for _, comp := range s.Comps {
		if comp.Name == "VEVENT" {
			return true
		}
	}
	return false
}
