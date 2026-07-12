package icloud

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	extcaldav "github.com/emersion/go-webdav/caldav"
)

// icalTimeLayout est le format des bornes time-range d'un calendar-query
// (RFC 4791) et des dates iCalendar en UTC.
const icalTimeLayout = "20060102T150405Z"

// maxReportBodySize borne la lecture d'une réponse REPORT (défense en
// profondeur contre une réponse anormalement volumineuse).
const maxReportBodySize = 32 << 20 // 32 Mio

// httpDoer est la portion minimale d'un client HTTP utilisée par la
// découverte maison, compatible *http.Client et le retour de
// webdav.HTTPClientWithBasicAuth (interface webdav.HTTPClient), qui déclare
// la même unique méthode Do.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client implémente Service contre iCloud via go-webdav/caldav, avec une
// découverte de shard maison (voir discovery.go). go-webdav v0.7.0 perd le
// host du shard dans FindCalendarHomeSet (ne retourne que le path), une
// découverte maison est nécessaire pour router les requêtes suivantes vers
// le bon shard (pXX-caldav.icloud.com).
type Client struct {
	http    httpDoer
	baseURL string

	discoverOnce sync.Once
	discoverErr  error
	shardBase    string
	homeSetPath  string
	dav          *extcaldav.Client
	allowHost    func(string) bool
}

var _ Service = (*Client)(nil)

// NewClient construit un Client. authHTTP est un client HTTP déjà configuré
// (allowlist réseau + Basic Auth en production). baseURL est
// security.ICloudBaseURL en production, l'URL d'un httptest.Server en test.
// allowHost revalide le host du shard découvert (défense en profondeur).
func NewClient(authHTTP httpDoer, baseURL string, allowHost func(string) bool) *Client {
	return &Client{
		http:      authHTTP,
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		allowHost: allowHost,
	}
}

// Discover force la découverte du shard iCloud. Utilisé au boot pour valider
// les credentials avant de démarrer le serveur MCP. Idempotent : les
// méthodes CRUD la déclenchent aussi automatiquement via sync.Once.
func (c *Client) Discover(ctx context.Context) error {
	return c.discover(ctx)
}

// SearchEvents recherche les événements d'un calendrier chevauchant
// [start, end] et développe les récurrences (RRULE + EXDATE + overrides
// RECURRENCE-ID) dans cette même plage.
func (c *Client) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) ([]Event, error) {
	if err := c.discover(ctx); err != nil {
		return nil, err
	}

	// Filtre time-range sur VEVENT (le serveur ne renvoie que les événements,
	// y compris récurrents, chevauchant [start, end]).
	filterXML := `<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">` +
		`<C:time-range start="` + start.UTC().Format(icalTimeLayout) +
		`" end="` + end.UTC().Format(icalTimeLayout) + `"/>` +
		`</C:comp-filter></C:comp-filter></C:filter>`
	objs, err := c.reportCalendarQuery(ctx, calendarPath, filterXML)
	if err != nil {
		return nil, fmt.Errorf("recherche d'événements (calendrier=%s) : %w", calendarPath, err)
	}

	var events []Event
	for i := range objs {
		master, overrides, perr := parseCalendarObject(&objs[i])
		if perr != nil {
			slog.Warn("objet calendrier ignoré (non parseable)", "path", objs[i].Path, "erreur", perr)
			continue
		}
		occs, eerr := ExpandOccurrences(*master, overrides, start, end, 0)
		if eerr != nil {
			slog.Warn("expansion de récurrence ignorée", "uid", master.UID, "erreur", eerr)
			continue
		}
		events = append(events, occs...)
	}
	return events, nil
}

// CreateEvent crée un nouvel événement dans calendarPath.
func (c *Client) CreateEvent(ctx context.Context, calendarPath string, ne *NewEvent) (string, error) {
	if err := c.discover(ctx); err != nil {
		return "", err
	}
	uid, err := newUID()
	if err != nil {
		return "", err
	}
	cal := buildEventCalendar(uid, ne)
	path := strings.TrimSuffix(calendarPath, "/") + "/" + uid + ".ics"
	if _, err := c.dav.PutCalendarObject(ctx, path, cal); err != nil {
		return "", fmt.Errorf("création de l'événement : %w", err)
	}
	return uid, nil
}

// UpdateEvent modifie les champs fournis (non-nil) d'un événement localisé
// par UID. Le VEVENT maître est modifié (les éventuels overrides
// RECURRENCE-ID restent inchangés). nil = champ inchangé, pointeur vers
// chaîne vide = effacement du champ.
func (c *Client) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	if err := c.discover(ctx); err != nil {
		return err
	}
	found, err := c.findEventByUID(ctx, calendarPath, uid)
	if err != nil {
		return err
	}
	// findEventByUID renvoie l'objet COMPLET (GET direct sur <uid>.ics, ou scan
	// time-range en fallback, jamais un calendar-data filtré) : VERSION/PRODID
	// et VTIMEZONE sont préservés, on peut le modifier puis le re-PUT tel quel.
	vevent, err := findMasterVEvent(found.Data)
	if err != nil {
		return err
	}

	if up.Title != nil {
		if *up.Title == "" {
			vevent.Props.Del(ical.PropSummary)
		} else {
			vevent.Props.SetText(ical.PropSummary, *up.Title)
		}
	}
	if up.Location != nil {
		if *up.Location == "" {
			vevent.Props.Del(ical.PropLocation)
		} else {
			vevent.Props.SetText(ical.PropLocation, *up.Location)
		}
	}
	if up.Notes != nil {
		if *up.Notes == "" {
			vevent.Props.Del(ical.PropDescription)
		} else {
			vevent.Props.SetText(ical.PropDescription, *up.Notes)
		}
	}
	if up.StartTime != nil {
		setEventDateProp(vevent, ical.PropDateTimeStart, *up.StartTime)
	}
	if up.EndTime != nil {
		setEventDateProp(vevent, ical.PropDateTimeEnd, *up.EndTime)
	}

	// Validation de cohérence après fusion (nécessaire quand un seul des
	// deux bornes start/end est fourni : la cohérence ne peut être vérifiée
	// qu'après relecture de l'événement existant).
	startProp := vevent.Props.Get(ical.PropDateTimeStart)
	endProp := vevent.Props.Get(ical.PropDateTimeEnd)
	if startProp != nil && endProp != nil {
		newStart, sErr := startProp.DateTime(time.UTC)
		newEnd, eErr := endProp.DateTime(time.UTC)
		if sErr == nil && eErr == nil && !newEnd.After(newStart) {
			return fmt.Errorf("mise à jour invalide : la fin (%s) doit être après le début (%s)", newEnd.Format(time.RFC3339), newStart.Format(time.RFC3339))
		}
	}

	vevent.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	seq := 0
	if p := vevent.Props.Get(ical.PropSequence); p != nil {
		if n, serr := p.Int(); serr == nil {
			seq = n
		}
	}
	setSequence(vevent, seq+1)

	if _, err := c.dav.PutCalendarObject(ctx, found.Path, found.Data); err != nil {
		return fmt.Errorf("mise à jour de l'événement (uid=%s) : %w", uid, err)
	}
	return nil
}

// DeleteEvent supprime un événement localisé par UID et retourne son titre
// (écho exigé par la spec pour confirmation humaine avant suppression).
func (c *Client) DeleteEvent(ctx context.Context, calendarPath, uid string) (string, error) {
	if err := c.discover(ctx); err != nil {
		return "", err
	}
	obj, err := c.findEventByUID(ctx, calendarPath, uid)
	if err != nil {
		return "", err
	}

	title := ""
	if vevent, verr := findMasterVEvent(obj.Data); verr == nil {
		if p := vevent.Props.Get(ical.PropSummary); p != nil {
			title = p.Value
		}
	}

	if err := c.dav.RemoveAll(ctx, obj.Path); err != nil {
		return "", fmt.Errorf("suppression de l'événement (uid=%s) : %w", uid, err)
	}
	return title, nil
}

// findEventByUID localise un événement par REPORT calendar-query filtré sur
// UID. Le nom de fichier .ics n'est PAS garanti égal à l'UID pour les
// événements importés (ex. depuis un autre client) : on ne devine jamais un
// path, on cherche toujours par UID.
func (c *Client) findEventByUID(ctx context.Context, calendarPath, uid string) (*extcaldav.CalendarObject, error) {
	// iCloud REJETTE les calendar-query <prop-filter> (412 Precondition Failed,
	// constaté 2026-07-12), impossible de filtrer par UID côté serveur. Mais
	// iCloud nomme ses ressources <UID>.ics (vérifié) : on tente d'abord un GET
	// direct sur ce path, qui renvoie l'objet COMPLET (adapté à update/delete).
	directPath := strings.TrimSuffix(calendarPath, "/") + "/" + uid + ".ics"
	if obj, err := c.dav.GetCalendarObject(ctx, directPath); err == nil && calendarHasUID(obj.Data, uid) {
		return obj, nil
	}

	// Fallback : événement importé dont le nom de fichier != UID. Le seul
	// filtre serveur accepté par iCloud est time-range : on balaie une fenêtre
	// très large et on filtre l'UID côté client.
	wideStart := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	wideEnd := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	filterXML := `<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">` +
		`<C:time-range start="` + wideStart.Format(icalTimeLayout) +
		`" end="` + wideEnd.Format(icalTimeLayout) + `"/>` +
		`</C:comp-filter></C:comp-filter></C:filter>`
	objs, err := c.reportCalendarQuery(ctx, calendarPath, filterXML)
	if err != nil {
		return nil, fmt.Errorf("recherche de l'événement (uid=%s) : %w", uid, err)
	}
	for i := range objs {
		if calendarHasUID(objs[i].Data, uid) {
			return &objs[i], nil
		}
	}
	return nil, fmt.Errorf("événement introuvable (uid=%s)", uid)
}

// calendarHasUID indique si un VCALENDAR contient un VEVENT dont l'UID est uid.
func calendarHasUID(cal *ical.Calendar, uid string) bool {
	if cal == nil {
		return false
	}
	for _, ch := range cal.Children {
		if ch.Name != ical.CompEvent {
			continue
		}
		if p := ch.Props.Get(ical.PropUID); p != nil && p.Value == uid {
			return true
		}
	}
	return false
}

// reportCalendarQuery envoie un REPORT calendar-query (Depth:1) demandant le
// calendar-data COMPLET (<C:calendar-data/> nu) avec le filtre fourni, puis
// décode chaque objet via go-ical.
//
// Requête manuelle (pas go-webdav QueryCalendar) car iCloud ne renvoie PAS les
// propriétés des composants pour une récupération PARTIELLE de calendar-data
// (un <comp name="VEVENT"><allprop/></comp> imbriqué renvoie des VEVENT vides ;
// AllProps+AllComps sur VCALENDAR renvoie zéro sous-composant), constaté
// contre le vrai iCloud le 2026-07-12. Seul le <calendar-data/> nu fonctionne,
// et QueryCalendar de go-webdav émet toujours un <comp>.
func (c *Client) reportCalendarQuery(ctx context.Context, calendarPath, filterXML string) ([]extcaldav.CalendarObject, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		`<D:prop><C:calendar-data/></D:prop>` +
		filterXML +
		`</C:calendar-query>`

	target, err := resolveRef(c.shardBase, calendarPath)
	if err != nil {
		return nil, fmt.Errorf("URL de calendrier invalide (%s) : %w", calendarPath, err)
	}
	req, err := http.NewRequestWithContext(ctx, "REPORT", target, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("construction REPORT (%s) : %w", calendarPath, err)
	}
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	req.Header.Set("Depth", "1")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("REPORT vers %s : %w", calendarPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("authentification iCloud refusée : vérifier ICLOUD_EMAIL et le mot de passe d'application")
	}
	if resp.StatusCode != 207 {
		return nil, fmt.Errorf("REPORT %s : statut HTTP inattendu %d", calendarPath, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReportBodySize))
	if err != nil {
		return nil, fmt.Errorf("lecture réponse REPORT (%s) : %w", calendarPath, err)
	}
	var ms msMultistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("parsing réponse REPORT (%s) : %w", calendarPath, err)
	}

	var objs []extcaldav.CalendarObject
	for _, r := range ms.Responses {
		prop := mergedOKProp(r)
		if prop == nil || prop.CalendarData == "" {
			continue
		}
		cal, derr := ical.NewDecoder(strings.NewReader(prop.CalendarData)).Decode()
		if derr != nil {
			slog.Warn("calendar-data non décodable, objet ignoré", "href", r.Href, "erreur", derr)
			continue
		}
		objs = append(objs, extcaldav.CalendarObject{Path: hrefPath(r.Href), Data: cal})
	}
	return objs, nil
}

// hrefPath extrait le path d'un href (absolu ou relatif), go-webdav
// GetCalendarObject/RemoveAll attendent un path résolu contre l'endpoint du
// shard, pas une URL absolue.
func hrefPath(href string) string {
	if u, err := url.Parse(href); err == nil && u.Path != "" {
		return u.Path
	}
	return href
}
