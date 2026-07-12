// Commande icloud-mcp : serveur MCP stdio exposant le calendrier iCloud via
// CalDAV. Voir README.md à la racine du repo pour la spec produit et le
// modèle de menace.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/emersion/go-webdav"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/health"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// version est écrasée au build via -ldflags "-X main.version=...".
var version = "dev"

// toolTimeout borne l'exécution de chaque appel de tool MCP, strictement
// inférieur au timeout HTTP (30s) pour que le tool échoue proprement avant
// que la requête HTTP sous-jacente ne timeout de son côté.
const toolTimeout = 25 * time.Second

// discoveryTimeout borne la découverte iCloud effectuée au boot pour valider
// les credentials avant de démarrer le serveur MCP.
const discoveryTimeout = 20 * time.Second

func main() {
	healthAddr := flag.String("health", "", "adresse du healthcheck HTTP (ex. 127.0.0.1:8797), désactivé si vide")
	showVersion := flag.Bool("version", false, "affiche la version et quitte")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	// 1. Configuration : échec = os.Exit(1) AVANT tout accès réseau.
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("erreur de configuration : %v", err)
	}

	// 2. Redaction : TOUT stderr passe par le RedactingWriter à partir d'ici.
	// Le cahier des charges exige de rédiger « le password OU l'email »,
	// cfg.Email est donc inclus au même titre (défense en profondeur ;
	// n'affecte pas l'erreur de config au boot ci-dessus, levée AVANT la
	// création de ce Redactor).
	red := security.NewRedactor(
		cfg.Password,
		cfg.Email,
		base64.StdEncoding.EncodeToString([]byte(cfg.Email+":"+cfg.Password)), // header Authorization Basic
		url.QueryEscape(cfg.Password), // forme URL-encodée
	)
	stderr := security.NewRedactingWriter(os.Stderr, red)
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	// Le logger stdlib `log` par défaut (log.Fatalf/log.Printf, utilisé
	// avant ce point pour la config, et par toute dépendance appelant
	// log.Print* directement) écrit sur os.Stderr BRUT par défaut, pas
	// couvert par la redaction. Le rediriger explicitement vers le writer
	// rédigé pour qu'AUCUN chemin de log ne contourne la redaction.
	log.SetOutput(stderr)
	audit := security.NewAuditLogger(stderr)

	// 3. Client HTTP durci (allowlist réseau + TLS vérifié) + Basic Auth.
	httpClient := security.NewICloudHTTPClient(cfg.Timeout)
	authHTTP := webdav.HTTPClientWithBasicAuth(httpClient, cfg.Email, cfg.Password)

	// 4. Service iCloud + découverte au boot (valide les credentials avant
	// de démarrer le serveur MCP).
	ic := icloud.NewClient(authHTTP, security.ICloudBaseURL, security.IsICloudHost)
	discoverCtx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	err = ic.Discover(discoverCtx)
	cancel()
	if err != nil {
		slog.Error("échec de la découverte iCloud (vérifier ICLOUD_EMAIL et le mot de passe d'application)", "err", err)
		os.Exit(1)
	}
	svc := icloud.NewGuardedService(ic, 2, 500*time.Millisecond)

	// 5. Serveur MCP.
	s := server.NewMCPServer("icloud-mcp", version,
		server.WithToolCapabilities(false),
		// WithRecovery reste en filet de sécurité supplémentaire, mais c'est
		// mcptools.RecoverRedactMiddleware (enregistré plus bas, donc plus
		// proche du handler dans la pile) qui intercepte un panic en
		// premier et produit une réponse REDIGÉE, sinon WithRecovery seul
		// sérialiserait l'erreur brute (non rédigée) sur le canal JSON-RPC.
		server.WithRecovery(),
		server.WithInstructions("Serveur calendrier iCloud (CalDAV). Appeler list_calendars en premier pour obtenir les paths des calendriers."),
		server.WithToolHandlerMiddleware(timeoutMiddleware(toolTimeout)),
		server.WithToolHandlerMiddleware(mcptools.RecoverRedactMiddleware(red)),
	)
	mcptools.Register(s, mcptools.Deps{Service: svc, Audit: audit, Redactor: red}, cfg.ReadOnly)

	// 6. Healthcheck optionnel (off par défaut).
	if *healthAddr != "" {
		h, err := health.Start(*healthAddr)
		if err != nil {
			slog.Error("échec du démarrage du healthcheck", "err", err)
			os.Exit(1)
		}
		defer func() { _ = h.Close() }()
	}

	slog.Info("serveur démarré", "version", version, "readOnly", cfg.ReadOnly)

	// 7. Stdio : ServeStdio gère lui-même SIGTERM/SIGINT. Le logger
	// d'erreurs du transport DOIT être branché sur le writer redacté, sinon
	// mcp-go logge sur os.Stderr brut et contourne la redaction.
	errLogger := log.New(stderr, "", log.LstdFlags)
	if err := server.ServeStdio(s, server.WithErrorLogger(errLogger)); err != nil {
		slog.Error("serveur arrêté en erreur", "err", err)
		os.Exit(1)
	}
}

// timeoutMiddleware borne la durée d'exécution de chaque appel de tool.
func timeoutMiddleware(d time.Duration) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next(ctx, req)
		}
	}
}
