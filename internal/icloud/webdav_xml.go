package icloud

import (
	"encoding/xml"
	"strings"
)

// Structures encoding/xml pour les réponses PROPFIND (207 Multi-Status) de
// la découverte maison et de list_calendars. Note namespaces : encoding/xml
// matche un tag non qualifié quel que soit le namespace ; on qualifie les
// props non-DAV (CalDAV, Apple) pour lever toute ambiguïté avec d'éventuelles
// propriétés homonymes.
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

// isOKStatus retourne vrai si la ligne de statut d'un propstat indique un
// succès (ex. "HTTP/1.1 200 OK").
func isOKStatus(status string) bool {
	return strings.Contains(status, " 200 ") || strings.HasSuffix(strings.TrimSpace(status), "200 OK")
}

// mergedOKProp fusionne tous les propstat en succès (200) d'une response en
// un seul msProp, en pratique un serveur CalDAV renvoie un seul bloc 200
// regroupant toutes les propriétés trouvées et un bloc 404 pour les
// propriétés absentes, mais on fusionne par robustesse si plusieurs blocs
// 200 existent. Retourne nil si aucun propstat n'est en succès.
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

// principalFromMultistatus extrait le href current-user-principal de la
// réponse de l'étape 1 de la découverte.
func principalFromMultistatus(ms *msMultistatus) string {
	for _, r := range ms.Responses {
		prop := mergedOKProp(r)
		if prop != nil && prop.CurrentUserPrincipal != nil && prop.CurrentUserPrincipal.Href != "" {
			return prop.CurrentUserPrincipal.Href
		}
	}
	return ""
}

// homeSetFromMultistatus extrait le href calendar-home-set de la réponse de
// l'étape 2 de la découverte.
func homeSetFromMultistatus(ms *msMultistatus) string {
	for _, r := range ms.Responses {
		prop := mergedOKProp(r)
		if prop != nil && prop.CalendarHomeSet != nil && prop.CalendarHomeSet.Href != "" {
			return prop.CalendarHomeSet.Href
		}
	}
	return ""
}
