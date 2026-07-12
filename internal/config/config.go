// Package config gère la configuration du serveur icloud-mcp : lecture des
// variables d'environnement, résolution des secrets file://, validation au
// boot. Aucune dépendance externe (pas de godotenv, les env viennent de
// l'hôte MCP qui lance ce binaire en child-process stdio).
package config

import (
	"fmt"
	"net/mail"
	"os"
	"strings"
	"time"
)

// icloudTimeout est le timeout HTTP fixe pour toutes les requêtes CalDAV.
// Figé par la spec (pas de variable d'environnement dédiée).
const icloudTimeout = 30 * time.Second

// Config regroupe la configuration validée au démarrage.
type Config struct {
	Email      string        // ICLOUD_EMAIL (support file://)
	Password   string        // ICLOUD_PASSWORD (support file://), ne JAMAIS logger
	ReadOnly   bool          // ICLOUD_MCP_READ_ONLY=1
	HealthAddr string        // flag -health (ex. "127.0.0.1:8797"), "" = off
	Timeout    time.Duration // constante 30s
}

// Load lit la configuration depuis l'environnement, résout les éventuels
// préfixes file:// et valide le résultat.
func Load() (*Config, error) {
	email, err := loadCredential("ICLOUD_EMAIL")
	if err != nil {
		return nil, err
	}
	password, err := loadCredential("ICLOUD_PASSWORD")
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Email:    email,
		Password: password,
		ReadOnly: parseBool(os.Getenv("ICLOUD_MCP_READ_ONLY")),
		Timeout:  icloudTimeout,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate vérifie le format de l'email et la longueur minimale du mot de
// passe. Les messages d'erreur ne contiennent JAMAIS le mot de passe (ni un
// extrait), l'email est toléré dans le message de l'erreur de format.
func (c *Config) Validate() error {
	if _, err := mail.ParseAddress(c.Email); err != nil {
		return fmt.Errorf("ICLOUD_EMAIL invalide (%q) : %w", c.Email, err)
	}
	if len(c.Password) < 8 {
		return fmt.Errorf("ICLOUD_PASSWORD trop court (minimum 8 caractères) : utiliser un mot de passe d'application généré sur appleid.apple.com")
	}
	return nil
}

// loadCredential lit une variable d'environnement. Si sa valeur commence par
// "file://", le secret est lu depuis le fichier référencé (pattern Docker
// secrets), c'est la SEULE lecture disque autorisée du programme.
func loadCredential(envVar string) (string, error) {
	val := os.Getenv(envVar)
	if strings.HasPrefix(val, "file://") {
		path := strings.TrimPrefix(val, "file://")
		data, err := os.ReadFile(path) // #nosec G304 -- path contrôlé par l'opérateur via env
		if err != nil {
			return "", fmt.Errorf("lecture de %s depuis le fichier %s : %w", envVar, path, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return val, nil
}

// parseBool interprète "1" et "true" (insensible à la casse) comme vrai ;
// tout le reste (y compris absent) comme faux.
func parseBool(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true"
}
