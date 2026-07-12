package icloud

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	extcaldav "github.com/emersion/go-webdav/caldav"
)

// maxPropfindBodySize borne la taille lue d'une réponse PROPFIND, défense
// en profondeur contre un serveur bogué ou hostile qui renverrait un corps
// de réponse pathologiquement volumineux (io.ReadAll non borné chargerait
// tout en mémoire avant même de tenter le parsing XML).
const maxPropfindBodySize = 8 << 20 // 8 Mio

// propfindPrincipalBody demande current-user-principal sur le serveur
// principal iCloud (étape 1 de la découverte).
const propfindPrincipalBody = `<?xml version="1.0" encoding="UTF-8"?>
<A:propfind xmlns:A="DAV:">
  <A:prop>
    <A:current-user-principal/>
  </A:prop>
</A:propfind>`

// propfindHomeSetBody demande calendar-home-set sur le principal (étape 2 de
// la découverte), la réponse contient l'URL absolue du shard.
const propfindHomeSetBody = `<?xml version="1.0" encoding="UTF-8"?>
<A:propfind xmlns:A="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <A:prop>
    <C:calendar-home-set/>
  </A:prop>
</A:propfind>`

// discover exécute la séquence de découverte iCloud (2 PROPFIND) une seule
// fois par session (sync.Once) et remplit shardBase/homeSetPath/dav. Toutes
// les méthodes CRUD appellent discover(ctx) en préambule ; les appels
// concurrents (pool de workers stdio) partagent le même résultat mis en cache.
func (c *Client) discover(ctx context.Context) error {
	c.discoverOnce.Do(func() {
		c.discoverErr = c.doDiscover(ctx)
	})
	return c.discoverErr
}

func (c *Client) doDiscover(ctx context.Context) error {
	// Étape 1 : current-user-principal sur le serveur principal.
	ms, err := c.propfind(ctx, c.baseURL+"/", "0", propfindPrincipalBody)
	if err != nil {
		return err
	}
	principal := principalFromMultistatus(ms)
	if principal == "" {
		return fmt.Errorf("découverte iCloud : current-user-principal introuvable dans la réponse")
	}

	principalURL, err := resolveRef(c.baseURL, principal)
	if err != nil {
		return fmt.Errorf("découverte iCloud : URL de principal invalide : %w", err)
	}
	// Revalidation OBLIGATOIRE du principal, SYMÉTRIQUE au contrôle déjà
	// appliqué au home-set ci-dessous (étape 3) : ne jamais faire confiance
	// à l'aveugle à une réponse serveur, même pour une étape intermédiaire
	// de la découverte, sans ce contrôle, un current-user-principal
	// malveillant n'était rattrapé qu'en aval par l'AllowlistTransport de
	// production, jamais dans les chemins de test (doer direct sans
	// transport durci) ni de façon homogène avec le home-set. Contrairement
	// au home-set (voir étape 3), le port n'est PAS revalidé ici : le
	// exiger casserait toute la suite de tests existante, qui simule le
	// serveur iCloud via httptest.Server (donc toujours sur un port
	// explicite), cf. FIX-1/FIX-2 dans FIXES_TODO.md pour le détail de
	// cette limitation assumée.
	pu, perr := url.Parse(principalURL)
	if perr != nil {
		return fmt.Errorf("découverte iCloud : URL de principal invalide : %w", perr)
	}
	if pu.Scheme != "https" || !c.allowHost(pu.Hostname()) {
		return fmt.Errorf("découverte iCloud : principal hors allowlist (%s)", pu.Hostname())
	}

	// Étape 2 : calendar-home-set sur le principal.
	ms2, err := c.propfind(ctx, principalURL, "0", propfindHomeSetBody)
	if err != nil {
		return err
	}
	homeSetHref := homeSetFromMultistatus(ms2)
	if homeSetHref == "" {
		return fmt.Errorf("découverte iCloud : calendar-home-set introuvable dans la réponse")
	}

	// Étape 3 : résolution du shard, revalidation OBLIGATOIRE du host,
	// même si le http.Client de production a déjà sa propre allowlist
	// (défense en profondeur : ne jamais faire confiance à l'aveugle à une
	// réponse serveur pour décider où partiront les futures requêtes).
	u, err := url.Parse(homeSetHref)
	if err != nil {
		return fmt.Errorf("découverte iCloud : home-set invalide : %w", err)
	}
	switch {
	case u.IsAbs():
		if u.Scheme != "https" || !c.allowHost(u.Hostname()) {
			return fmt.Errorf("découverte iCloud : home-set hors allowlist (%s)", u.Hostname())
		}
		c.shardBase = u.Scheme + "://" + u.Host
		c.homeSetPath = u.Path
	default:
		// href relatif : serveur de test ou serveur CalDAV sans shards.
		c.shardBase = c.baseURL
		c.homeSetPath = u.Path
	}

	dav, err := extcaldav.NewClient(c.http, c.shardBase)
	if err != nil {
		return fmt.Errorf("découverte iCloud : création du client CalDAV (shard=%s) : %w", c.shardBase, err)
	}
	c.dav = dav
	return nil
}

// propfind exécute une requête PROPFIND et parse la réponse 207 Multi-Status.
func (c *Client) propfind(ctx context.Context, target, depth, body string) (*msMultistatus, error) {
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", target, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("construction requête PROPFIND (%s) : %w", target, err)
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("Depth", depth)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requête PROPFIND vers %s : %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("authentification iCloud refusée : vérifier ICLOUD_EMAIL et le mot de passe d'application")
	}
	if resp.StatusCode != 207 {
		return nil, fmt.Errorf("PROPFIND %s : statut HTTP inattendu %d", target, resp.StatusCode)
	}

	// Borne défensive : une réponse anormalement volumineuse (serveur bogué
	// ou hostile) ne doit jamais être chargée intégralement en mémoire.
	// maxPropfindBodySize+1 permet de détecter le dépassement (si on lit
	// exactement la limite, on ne sait pas s'il y avait plus de données).
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPropfindBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("lecture réponse PROPFIND (%s) : %w", target, err)
	}
	if len(data) > maxPropfindBodySize {
		return nil, fmt.Errorf("réponse PROPFIND (%s) trop volumineuse (> %d octets)", target, maxPropfindBodySize)
	}
	var ms msMultistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("parsing réponse PROPFIND (%s) : %w", target, err)
	}
	return &ms, nil
}

// resolveRef résout une référence (absolue ou relative) par rapport à base.
func resolveRef(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}
