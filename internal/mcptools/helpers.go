// Package mcptools définit les 5 tools MCP exposés par le serveur et leurs
// handlers. Toute la validation d'entrée et la journalisation d'audit vivent
// ici (couche protocole) ; l'accès réseau vit dans internal/icloud.
package mcptools

import (
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// errResult construit un CallToolResult d'erreur en passant systématiquement
// le message par le Redactor. TOUTE erreur renvoyée par un tool transite par
// ce helper, c'est l'un des 3 points d'insertion de la redaction (voir
// internal/security).
func errResult(red *security.Redactor, context string, err error) *mcp.CallToolResult {
	return mcp.NewToolResultError(red.Redact(fmt.Sprintf("%s : %v", context, err)))
}

// writeJSON sérialise payload en JSON indenté et construit un
// CallToolResult de succès. Un échec de sérialisation (cas interne
// improbable) est lui-même redirigé vers errResult, par cohérence : toutes
// les erreurs de tools passent par la redaction.
func writeJSON(red *security.Redactor, payload any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return errResult(red, "formatage de la réponse", err)
	}
	return mcp.NewToolResultText(string(b))
}
