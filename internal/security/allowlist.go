// Package security regroupe les mécanismes de sécurité transverses du serveur
// icloud-mcp : allowlist réseau, rédaction des secrets, audit des mutations.
// Aucun de ces mécanismes ne dépend d'iCloud ou de MCP, ce sont des feuilles
// réutilisables et testables isolément.
package security

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// ICloudBaseURL est la SEULE base réseau autorisée pour ce binaire au démarrage.
// Les shards de découverte (pXX-caldav.icloud.com) sont autorisés séparément
// par IsICloudHost, mais toujours revalidés (jamais fait confiance à l'aveugle
// à une réponse serveur).
const ICloudBaseURL = "https://caldav.icloud.com"

// shardHostRe matche les shards retournés par la découverte iCloud
// (ex. p46-caldav.icloud.com, p123-caldav.icloud.com).
var shardHostRe = regexp.MustCompile(`^p\d{1,3}-caldav\.icloud\.com$`)

// IsICloudHost autorise caldav.icloud.com et pXX-caldav.icloud.com, rien
// d'autre. La comparaison est sensible à la casse : url.URL.Hostname()
// retourne le host tel qu'il apparaît dans l'URL (non normalisé en
// minuscules par Go), donc une variante en majuscules est délibérément
// refusée plutôt que traitée comme équivalente.
func IsICloudHost(host string) bool {
	return host == "caldav.icloud.com" || shardHostRe.MatchString(host)
}

// portAllowed accepte le port vide (443 implicite) ou "443" explicite. iCloud
// renvoie l'URL de shard avec un port 443 EXPLICITE (ex.
// p120-caldav.icloud.com:443), le rejeter cassait tout appel de tool après la
// découverte. Tout autre port reste refusé (jamais légitime pour iCloud, signal
// de contournement possible).
func portAllowed(port string) bool {
	return port == "" || port == "443"
}

// AllowlistTransport est un http.RoundTripper qui REJETTE toute requête dont
// le scheme n'est pas https ou dont le host n'est pas autorisé par `allowed`.
// Un RoundTripper (plutôt qu'un DialContext) intercepte la requête avant
// toute résolution DNS, et couvre aussi chaque saut de redirection : le
// http.Client de la stdlib repasse chaque requête redirigée par le même
// Transport.
type AllowlistTransport struct {
	inner   http.RoundTripper
	allowed func(host string) bool
}

// NewAllowlistTransport construit un AllowlistTransport. `inner` transporte
// réellement les requêtes autorisées ; `allowed` décide du host.
func NewAllowlistTransport(inner http.RoundTripper, allowed func(string) bool) *AllowlistTransport {
	return &AllowlistTransport{inner: inner, allowed: allowed}
}

// RoundTrip implémente http.RoundTripper.
func (t *AllowlistTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("allowlist réseau : scheme %q refusé (https uniquement)", req.URL.Scheme)
	}
	// Seuls le port implicite (vide) et 443 sont acceptés : iCloud renvoie le
	// shard avec un port 443 explicite (ex. p120-caldav.icloud.com:443). Tout
	// autre port n'est jamais légitime pour iCloud et pourrait signaler une
	// tentative de contournement (service tiers sur un host par ailleurs
	// autorisé). Rejeté par défense en profondeur.
	if !portAllowed(req.URL.Port()) {
		return nil, fmt.Errorf("allowlist réseau : port %q refusé (443 uniquement)", req.URL.Port())
	}
	host := req.URL.Hostname()
	if !t.allowed(host) {
		return nil, fmt.Errorf("allowlist réseau : host %q refusé", host)
	}
	return t.inner.RoundTrip(req)
}

// NewICloudHTTPClient retourne le client HTTP de production : allowlist
// IsICloudHost, TLS vérifié (config par défaut, MinVersion TLS1.2, jamais
// InsecureSkipVerify), timeout borné, pool de connexions raisonnable. Un
// CheckRedirect additionnel revalide le host à chaque saut de redirection
// (défense en profondeur, redondant avec le RoundTripper mais sans coût).
func NewICloudHTTPClient(timeout time.Duration) *http.Client {
	inner := &http.Transport{
		MaxIdleConns:    10,
		MaxConnsPerHost: 10,
		IdleConnTimeout: 30 * time.Second,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	transport := NewAllowlistTransport(inner, IsICloudHost)
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != "https" || !IsICloudHost(req.URL.Hostname()) || !portAllowed(req.URL.Port()) {
				return fmt.Errorf("allowlist réseau : redirection refusée vers %q", req.URL.Host)
			}
			return nil
		},
	}
}
