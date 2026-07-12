package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// newReq construit une mcp.CallToolRequest avec les arguments fournis,
// équivalent minimal d'un appel réel via le transport stdio.
func newReq(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func testDeps(svc *icloud.MockService) Deps {
	return Deps{
		Service:  svc,
		Audit:    security.NewAuditLogger(&discardWriter{}),
		Redactor: security.NewRedactor("SENTINEL-PW-unused"),
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestListCalendarsHandler_HappyPath(t *testing.T) {
	svc := &icloud.MockService{Calendars: []icloud.Calendar{
		{Path: "/cal/home/", Name: "Maison", Color: "#FF2968FF"},
	}}
	handler := listCalendarsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(nil))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if res.IsError {
		t.Fatalf("résultat d'erreur inattendu : %+v", res)
	}

	var payload struct {
		Count     int               `json:"count"`
		Calendars []icloud.Calendar `json:"calendars"`
	}
	decodeResult(t, res, &payload)
	if payload.Count != 1 || len(payload.Calendars) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Calendars[0].Name != "Maison" {
		t.Errorf("Name = %q", payload.Calendars[0].Name)
	}
}

func TestListCalendarsHandler_ServiceError(t *testing.T) {
	svc := &icloud.MockService{ListErr: fmt.Errorf("panne réseau")}
	handler := listCalendarsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(nil))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if !res.IsError {
		t.Fatal("attendu : résultat d'erreur")
	}
	if !strings.Contains(resultText(t, res), "panne réseau") {
		t.Errorf("message d'erreur attendu contenant 'panne réseau' : %s", resultText(t, res))
	}
}

// --- Helpers partagés entre les fichiers de test de ce package -----------

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("résultat sans contenu")
	}
	tc, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("contenu non textuel : %#v", res.Content[0])
	}
	return tc.Text
}

func decodeResult(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	text := resultText(t, res)
	if err := json.Unmarshal([]byte(text), v); err != nil {
		t.Fatalf("décodage JSON de la réponse : %v\ncontenu : %s", err, text)
	}
}
