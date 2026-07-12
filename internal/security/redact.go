package security

import (
	"io"
	"strings"
)

// Redactor remplace chaque secret enregistré par "[REDACTED]" dans une chaîne.
type Redactor struct {
	secrets []string
}

// NewRedactor construit un Redactor à partir des secrets à masquer. Les
// chaînes vides ou trop courtes (< 4 caractères) sont ignorées : les
// remplacer partout produirait un bruit inexploitable (remplacer "" ou "ab"
// masquerait des passages sans rapport avec un secret).
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if len(s) < 4 {
			continue
		}
		r.secrets = append(r.secrets, s)
	}
	return r
}

// Redact remplace chaque secret enregistré par "[REDACTED]" dans s.
func (r *Redactor) Redact(s string) string {
	out := s
	for _, secret := range r.secrets {
		out = strings.ReplaceAll(out, secret, "[REDACTED]")
	}
	return out
}

// RedactingWriter wrappe un io.Writer (typiquement stderr) et redacte chaque
// Write avant de le transmettre. Limite assumée : un secret coupé entre deux
// appels à Write ne serait pas masqué ; en pratique slog et le logger
// d'erreurs du transport stdio émettent chaque enregistrement en un seul
// Write, donc le cas ne se présente pas pour nos usages.
type RedactingWriter struct {
	w io.Writer
	r *Redactor
}

// NewRedactingWriter construit un RedactingWriter.
func NewRedactingWriter(w io.Writer, r *Redactor) *RedactingWriter {
	return &RedactingWriter{w: w, r: r}
}

// Write implémente io.Writer. Il retourne len(p) en cas de succès (et non la
// longueur du texte rédigé, qui peut différer) : les appelants (slog,
// log.Logger) s'attendent à ce que Write consomme l'intégralité du buffer
// d'origine sans erreur d'écriture partielle.
func (rw *RedactingWriter) Write(p []byte) (int, error) {
	redacted := rw.r.Redact(string(p))
	if _, err := rw.w.Write([]byte(redacted)); err != nil {
		return 0, err
	}
	return len(p), nil
}
