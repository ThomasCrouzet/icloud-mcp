package mcptools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-webdav"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// Test de redaction bout-en-bout (exigence de sécurité clé) : le mot de passe
// n'apparaît dans AUCUNE sortie, ni stderr, ni les réponses JSON des tools,
// même quand le serveur CalDAV distant renvoie des erreurs qui échoient les
// credentials dans leur corps (comportement go-webdav réel sur une réponse
// non-2xx text/plain : le corps est inclus dans l'erreur retournée).

const (
	redactionPrincipalPath = "/121234567/principal/"
	redactionHomeSetPath   = "/121234567/calendars/"
	redactionCalendarPath  = redactionHomeSetPath + "home/"
)

// redactionTestServer sert la découverte iCloud normalement, et peut être
// configuré pour faire échouer REPORT/PUT/DELETE avec un corps d'erreur
// contenant le password et le header Authorization brut (simulateur de
// serveur bogué / hostile echo).
type redactionTestServer struct {
	password    string
	authFail401 bool
	reportFail  bool
	putFail     bool
	deleteFail  bool
}

func (h *redactionTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "PROPFIND" && r.URL.Path == "/":
		if h.authFail401 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:">
  <response>
    <href>/</href>
    <propstat>
      <prop><current-user-principal><href>`+redactionPrincipalPath+`</href></current-user-principal></prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`)
	case r.Method == "PROPFIND" && r.URL.Path == redactionPrincipalPath:
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <response>
    <href>`+redactionPrincipalPath+`</href>
    <propstat>
      <prop><C:calendar-home-set><href>`+redactionHomeSetPath+`</href></C:calendar-home-set></prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`)
	case r.Method == "PROPFIND" && r.URL.Path == redactionHomeSetPath:
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <response>
    <href>`+redactionCalendarPath+`</href>
    <propstat>
      <prop>
        <resourcetype><collection/><C:calendar/></resourcetype>
        <displayname>Maison</displayname>
        <C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>
      </prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`)
	case r.Method == "REPORT":
		_, _ = io.ReadAll(r.Body)
		if h.reportFail {
			h.writeHostileError(w, r)
			return
		}
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:"></multistatus>`)
	case r.Method == http.MethodPut:
		_, _ = io.ReadAll(r.Body)
		if h.putFail {
			h.writeHostileError(w, r)
			return
		}
		w.Header().Set("ETag", `"etag-1"`)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodDelete:
		if h.deleteFail {
			h.writeHostileError(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// writeHostileError simule un serveur CalDAV bogué qui échoie les
// credentials reçus dans le corps de sa réponse d'erreur, SOUS TROIS FORMES
// (password brut, header Authorization brut = base64(email:password), et
// une forme url-encodée, ex. un système d'échoie via une query string de
// redirection), go-webdav (internal.Client.Do) inclut ce corps text/plain
// dans l'erreur Go retournée à l'appelant, c'est ce chemin de fuite que le
// test exerce (FIX-9 : les 3 formes doivent être rédigées, pas seulement le
// password brut).
func (h *redactionTestServer) writeHostileError(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = fmt.Fprintf(w, "erreur interne : Authorization reçu=%q, mot de passe brut=%q, forme url-encodee=%q",
		r.Header.Get("Authorization"), h.password, url.QueryEscape(h.password))
}

func writeRedactionXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	_, _ = io.WriteString(w, body)
}

// newRedactor reproduit exactement l'enregistrement fait dans main.go (§2.3
// du blueprint) : password brut, forme Basic base64, forme URL-encodée.
func newTestRedactor(email, password string) *security.Redactor {
	return security.NewRedactor(
		password,
		base64.StdEncoding.EncodeToString([]byte(email+":"+password)),
		url.QueryEscape(password),
	)
}

// TestNoPasswordLeak_DiscoveryAuthFailure, note FIX-10 : ce test est
// délibérément « vacuously true » quant à un vrai contrôle positif. La
// réponse 401 simulée a un corps vide, et surtout : discovery.go n'inclut
// JAMAIS le corps de la réponse dans son erreur pour un 401, le message est
// une constante fixe (`c.propfind`, cf. discovery.go), lue AVANT tout
// io.ReadAll du corps. Aucune credential ne peut donc fuiter par CE chemin
// précis, qu'il y ait redaction ou non : impossible d'y construire un
// contrôle positif honnête (un corps 401 échoyant le password ne
// prouverait rien, puisque notre code ne le lit jamais). La preuve RÉELLE
// que la redaction fonctionne sur un corps de réponse échoyant les
// credentials est apportée par TestNoPasswordLeak_HostileServerEchoesCredentials
// ci-dessous (REPORT/PUT/DELETE via go-webdav, qui LUI inclut le corps dans
// l'erreur retournée). Ce test-ci reste utile pour vérifier qu'un 401 ne
// fait planter aucun tool et ne fuite rien d'AUTRE que le corps (ex. la
// stack/contexte de l'erreur).
func TestNoPasswordLeak_DiscoveryAuthFailure(t *testing.T) {
	const email = "user@example.com"
	const password = "SENTINEL-PW-abc123-XYZ" // gitleaks:allow, sentinelle de test, pas un vrai secret

	h := &redactionTestServer{password: password, authFail401: true}
	srv := httptest.NewTLSServer(h)
	defer srv.Close()

	authHTTP := webdav.HTTPClientWithBasicAuth(srv.Client(), email, password)
	ic := icloud.NewClient(authHTTP, srv.URL, func(string) bool { return true })
	svc := icloud.NewGuardedService(ic, 0, time.Millisecond)

	red := newTestRedactor(email, password)
	var stderrBuf bytes.Buffer
	stderr := security.NewRedactingWriter(&stderrBuf, red)
	audit := security.NewAuditLogger(stderr)

	deps := Deps{Service: svc, Audit: audit, Redactor: red}
	s := server.NewMCPServer("test", "0.0.0", server.WithToolCapabilities(false))
	Register(s, deps, false)

	combined := callAllTools(t, s, []toolCall{
		{"list_calendars", nil},
		{"search_events", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z"}},
		{"create_event", map[string]any{"title": "x", "start": "2026-07-01T00:00:00Z", "end": "2026-07-01T01:00:00Z", "calendar": redactionCalendarPath}},
		{"update_event", map[string]any{"uid": "uid-1", "calendar": redactionCalendarPath, "title": "y"}},
		{"delete_event", map[string]any{"uid": "uid-1", "calendar": redactionCalendarPath}},
	}, true /* toutes doivent échouer (401) */)

	assertNoLeak(t, email, password, combined, stderrBuf.String())
}

func TestNoPasswordLeak_HostileServerEchoesCredentials(t *testing.T) {
	const email = "user@example.com"
	const password = "SENTINEL-PW-abc123-XYZ" // gitleaks:allow, sentinelle de test, pas un vrai secret

	h := &redactionTestServer{password: password, reportFail: true, putFail: true, deleteFail: true}
	srv := httptest.NewTLSServer(h)
	defer srv.Close()

	authHTTP := webdav.HTTPClientWithBasicAuth(srv.Client(), email, password)
	ic := icloud.NewClient(authHTTP, srv.URL, func(string) bool { return true })

	// Contrôle positif, FIX-9 : sans redaction, l'erreur brute DOIT
	// contenir les 3 formes du secret (brute, base64 Basic Auth,
	// url-encodée). On passe par CreateEvent (PUT via go-webdav, qui inclut le
	// corps de la réponse hostile dans son erreur, cf. writeHostileError).
	// NB : SearchEvents ne convient PLUS comme contrôle positif : sa requête
	// est un REPORT manuel (reportCalendarQuery) qui n'inclut jamais le corps
	// de la réponse dans l'erreur (« statut HTTP inattendu 500 »). La redaction
	// des chemins go-webdav (create/update/delete) reste vérifiée par
	// l'assertion combinée plus bas.
	rawStart := time.Now()
	_, rawErr := ic.CreateEvent(context.Background(), redactionCalendarPath, &icloud.NewEvent{
		Title:     "x",
		StartTime: rawStart,
		EndTime:   rawStart.Add(time.Hour),
	})
	if rawErr == nil {
		t.Fatalf("contrôle positif échoué : attendu une erreur brute non-nil")
	}
	rawBasicAuth := base64.StdEncoding.EncodeToString([]byte(email + ":" + password))
	rawURLEncoded := url.QueryEscape(password)
	for _, want := range []string{password, rawBasicAuth, rawURLEncoded} {
		if !strings.Contains(rawErr.Error(), want) {
			t.Fatalf("contrôle positif échoué : l'erreur brute (non rédigée) devrait contenir %q, obtenu : %v", want, rawErr)
		}
	}

	svc := icloud.NewGuardedService(ic, 0, time.Millisecond)
	red := newTestRedactor(email, password)
	var stderrBuf bytes.Buffer
	stderr := security.NewRedactingWriter(&stderrBuf, red)
	audit := security.NewAuditLogger(stderr)

	deps := Deps{Service: svc, Audit: audit, Redactor: red}
	s := server.NewMCPServer("test", "0.0.0", server.WithToolCapabilities(false))
	Register(s, deps, false)

	combined := callAllTools(t, s, []toolCall{
		{"list_calendars", nil}, // PROPFIND seule, succès attendu
		{"search_events", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": redactionCalendarPath}},
		{"create_event", map[string]any{"title": "x", "start": "2026-07-01T00:00:00Z", "end": "2026-07-01T01:00:00Z", "calendar": redactionCalendarPath}},
		{"update_event", map[string]any{"uid": "uid-1", "calendar": redactionCalendarPath, "title": "y"}},
		{"delete_event", map[string]any{"uid": "uid-1", "calendar": redactionCalendarPath}},
	}, false /* seul list_calendars doit réussir, les autres échouent */)

	assertNoLeak(t, email, password, combined, stderrBuf.String())

	// Un create_event RÉUSSI (aucun échec simulé) ne doit lui non plus
	// jamais exposer le password dans sa réponse de succès.
	h2 := &redactionTestServer{password: password}
	srv2 := httptest.NewTLSServer(h2)
	defer srv2.Close()
	authHTTP2 := webdav.HTTPClientWithBasicAuth(srv2.Client(), email, password)
	ic2 := icloud.NewClient(authHTTP2, srv2.URL, func(string) bool { return true })
	svc2 := icloud.NewGuardedService(ic2, 0, time.Millisecond)
	deps2 := Deps{Service: svc2, Audit: audit, Redactor: red}
	s2 := server.NewMCPServer("test2", "0.0.0", server.WithToolCapabilities(false))
	Register(s2, deps2, false)

	successCombined := callAllTools(t, s2, []toolCall{
		{"create_event", map[string]any{"title": "x", "start": "2026-07-01T00:00:00Z", "end": "2026-07-01T01:00:00Z", "calendar": redactionCalendarPath}},
	}, false)
	if !strings.Contains(successCombined, `"success": true`) {
		t.Errorf("le create_event de contrôle aurait dû réussir : %s", successCombined)
	}
	assertNoLeak(t, email, password, successCombined, stderrBuf.String())
}

type toolCall struct {
	name string
	args map[string]any
}

// callAllTools initialise un client MCP in-process contre s, appelle chaque
// tool de calls, et retourne la concaténation de tous les contenus textuels
// des réponses (succès ET erreur). allMustFail force une assertion que
// chaque appel est bien en erreur (utilisé pour le scénario 401).
func callAllTools(t *testing.T, s *server.MCPServer, calls []toolCall, allMustFail bool) string {
	t.Helper()
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient : %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start : %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "redaction-test", Version: "0.0.0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize : %v", err)
	}

	var combined strings.Builder
	for _, call := range calls {
		req := mcp.CallToolRequest{}
		req.Params.Name = call.name
		req.Params.Arguments = call.args
		res, err := c.CallTool(ctx, req)
		if err != nil {
			t.Fatalf("%s : erreur protocole inattendue : %v", call.name, err)
		}
		if allMustFail && !res.IsError {
			t.Errorf("%s : succès inattendu (devrait échouer)", call.name)
		}
		for _, content := range res.Content {
			if tc, ok := mcp.AsTextContent(content); ok {
				combined.WriteString(tc.Text)
				combined.WriteString("\n")
			}
		}
	}
	return combined.String()
}

// assertNoLeak vérifie l'absence des 3 formes du secret (FIX-9), password
// brut, base64(email:password) tel qu'échoyé par un header Authorization
// brut, et url.QueryEscape(password), dans toolResults ET capturedStderr.
func assertNoLeak(t *testing.T, email, password, toolResults, capturedStderr string) {
	t.Helper()
	forms := map[string]string{
		"password brut":                         password,
		"password base64 (Authorization Basic)": base64.StdEncoding.EncodeToString([]byte(email + ":" + password)),
		"password url-encodé (url.QueryEscape)": url.QueryEscape(password),
	}
	for label, form := range forms {
		if strings.Contains(toolResults, form) {
			t.Fatalf("%s apparaît dans une réponse de tool :\n%s", label, toolResults)
		}
		if strings.Contains(capturedStderr, form) {
			t.Fatalf("%s apparaît dans stderr :\n%s", label, capturedStderr)
		}
	}
}

// TestRecoverRedactMiddleware_PanicDoesNotLeakSecret, FIX-A. Le canal
// d'erreur JSON-RPC (déclenché par un panic dans un handler de tool)
// contourne le RedactingWriter de stderr : celui-ci ne couvre que stderr, pas
// la sérialisation des erreurs protocole sur stdout. server.WithRecovery()
// convertit certes un panic en erreur Go, mais cette erreur est ensuite
// sérialisée TELLE QUELLE (err.Error()) dans le message JSON-RPC, sans
// passer par la redaction. Ce test enregistre un tool bidon qui panique en
// portant le password, et vérifie :
//   - contrôle positif : SANS RecoverRedactMiddleware (juste WithRecovery,
//     configuration vulnérable), le password fuite bien dans l'erreur
//     protocole, sinon ce test ne prouverait rien ;
//   - AVEC RecoverRedactMiddleware, le password ne fuite JAMAIS : le panic
//     est absorbé et transformé en CallToolResult d'erreur rédigé.
func TestRecoverRedactMiddleware_PanicDoesNotLeakSecret(t *testing.T) {
	const email = "user@example.com"
	const password = "SENTINEL-PW-panic-abc123-XYZ" // gitleaks:allow, sentinelle de test, pas un vrai secret
	red := newTestRedactor(email, password)

	registerBoom := func(s *server.MCPServer) {
		s.AddTool(mcp.NewTool("boom"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			panic(fmt.Sprintf("erreur interne : Authorization reçu=%q, mot de passe brut=%q", "Basic xxx", password))
		})
	}

	callBoom := func(t *testing.T, s *server.MCPServer) (result string, callErr error) {
		t.Helper()
		c, err := client.NewInProcessClient(s)
		if err != nil {
			t.Fatalf("NewInProcessClient : %v", err)
		}
		defer func() { _ = c.Close() }()
		ctx := context.Background()
		if err := c.Start(ctx); err != nil {
			t.Fatalf("Start : %v", err)
		}
		initReq := mcp.InitializeRequest{}
		initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		initReq.Params.ClientInfo = mcp.Implementation{Name: "panic-test", Version: "0.0.0"}
		if _, err := c.Initialize(ctx, initReq); err != nil {
			t.Fatalf("Initialize : %v", err)
		}
		req := mcp.CallToolRequest{}
		req.Params.Name = "boom"
		res, err := c.CallTool(ctx, req)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, content := range res.Content {
			if tc, ok := mcp.AsTextContent(content); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String(), nil
	}

	t.Run("controle positif : sans RecoverRedactMiddleware, le panic fuite", func(t *testing.T) {
		vulnerable := server.NewMCPServer("vulnerable", "0.0.0",
			server.WithToolCapabilities(false),
			server.WithRecovery(),
		)
		registerBoom(vulnerable)
		text, callErr := callBoom(t, vulnerable)
		if callErr == nil {
			t.Fatalf("attendu : erreur protocole (panic non absorbé par un CallToolResult), obtenu résultat : %s", text)
		}
		if !strings.Contains(callErr.Error(), password) {
			t.Fatalf("contrôle positif échoué : l'erreur protocole (sans protection) devrait contenir le password sentinelle, obtenu : %v", callErr)
		}
	})

	t.Run("avec RecoverRedactMiddleware, aucune fuite", func(t *testing.T) {
		protected := server.NewMCPServer("protected", "0.0.0",
			server.WithToolCapabilities(false),
			server.WithRecovery(),
			server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)),
		)
		registerBoom(protected)
		text, callErr := callBoom(t, protected)
		if callErr != nil {
			t.Fatalf("erreur protocole inattendue (le panic aurait dû être absorbé en CallToolResult) : %v", callErr)
		}
		if strings.Contains(text, password) {
			t.Fatalf("le password apparaît dans la réponse du tool après panic :\n%s", text)
		}
	})
}
