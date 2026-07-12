package mcptools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// RecoverRedactMiddleware intercepte les panics d'un handler de tool et
// produit un CallToolResult d'erreur REDACTÉ, au lieu de laisser le panic
// remonter en (nil, err) jusqu'au canal protocole JSON-RPC.
//
// Ce canal (stdout) N'EST PAS couvert par le RedactingWriter, celui-ci ne
// wrappe que stderr (logs slog + audit). Sans ce middleware, un panic
// porteur du mot de passe (ex. erreur HTTP qui échoie les credentials dans
// son message, cf. redaction_test.go) fuiterait le secret verbatim dans la
// réponse JSON-RPC renvoyée à l'appelant MCP : server.WithRecovery()
// convertit certes le panic en erreur Go, mais cette erreur est ensuite
// sérialisée telle quelle (err.Error()) dans le message JSON-RPC,
// AUCUNE redaction n'intervient sur ce chemin.
//
// server.WithRecovery() reste en place comme filet de sécurité
// supplémentaire (défense en profondeur), mais CE middleware doit
// intercepter le panic EN PREMIER pour produire une réponse rédigée : il
// doit donc être enregistré APRÈS les autres middlewares côté
// server.NewMCPServer (voir cmd/icloud-mcp/main.go), de sorte à être le
// plus proche du handler dans la pile d'appels, le recover() le plus proche
// du panic gagne lors du unwind, les middlewares plus externes (dont
// WithRecovery) ne voient alors plus rien remonter.
func RecoverRedactMiddleware(red *security.Redactor) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
			defer func() {
				if r := recover(); r != nil {
					result = errResult(red, "erreur interne", fmt.Errorf("%v", r))
					err = nil
				}
			}()
			return next(ctx, req)
		}
	}
}
