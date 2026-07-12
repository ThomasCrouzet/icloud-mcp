package icloud

import (
	"context"
	"fmt"
	"strings"
)

// propfindCalendarsBody demande resourcetype/displayname/description/
// couleur Apple/composants supportés sur le calendar-home-set (Depth: 1).
// go-webdav v0.7.0 (caldav.Calendar) n'expose pas la couleur : PROPFIND
// maison obligatoire pour list_calendars.
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

// ListCalendars liste les calendriers du compte, en filtrant les collections
// techniques (schedule-inbox/outbox, notifications) et les collections
// VTODO-only (Reminders résiduels).
func (c *Client) ListCalendars(ctx context.Context) ([]Calendar, error) {
	if err := c.discover(ctx); err != nil {
		return nil, err
	}

	target := c.shardBase + c.homeSetPath
	ms, err := c.propfind(ctx, target, "1", propfindCalendarsBody)
	if err != nil {
		return nil, fmt.Errorf("liste des calendriers : %w", err)
	}

	var out []Calendar
	for _, r := range ms.Responses {
		prop := mergedOKProp(r)
		if prop == nil {
			continue
		}
		if prop.ResourceType == nil || prop.ResourceType.Calendar == nil {
			continue // pas une collection de type calendrier
		}
		if prop.ResourceType.ScheduleInbox != nil || prop.ResourceType.ScheduleOutbox != nil {
			continue
		}
		if strings.Contains(r.Href, "/inbox") || strings.Contains(r.Href, "/outbox") || strings.Contains(r.Href, "/notification") {
			continue
		}
		if prop.SupportedComps != nil && !supportsVEvent(prop.SupportedComps) {
			continue // VTODO-only (Reminders) ou autre composant sans VEVENT
		}
		out = append(out, Calendar{
			Path:        r.Href,
			Name:        prop.DisplayName,
			Description: prop.CalendarDescription,
			Color:       prop.CalendarColor,
		})
	}
	return out, nil
}

func supportsVEvent(s *msSupportedSet) bool {
	if len(s.Comps) == 0 {
		// Propriété présente mais vide : ne pas filtrer par excès de zèle.
		return true
	}
	for _, comp := range s.Comps {
		if comp.Name == "VEVENT" {
			return true
		}
	}
	return false
}
