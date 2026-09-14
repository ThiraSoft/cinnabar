package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// defaultMaxRequestBytes borne la taille du corps quand la configuration ne
// fournit aucune valeur positive. Un `max_request_bytes: 0` (config absente,
// faute de frappe, appel direct de NewServer sans passer par config.Load) ne
// doit jamais désactiver silencieusement la limite: fail-open sur un champ de
// configuration oublié serait pire qu'une valeur par défaut arbitraire.
const defaultMaxRequestBytes int64 = 1 << 20 // 1 MiB

type principalKey struct{}

func principalFrom(ctx context.Context) *memory.Principal {
	p, _ := ctx.Value(principalKey{}).(*memory.Principal)
	return p
}

// principalOrInternal rend le principal attaché par authenticate, ou répond
// 500 si absent. Ce cas n'arrive jamais en pratique: authenticate ne laisse
// jamais passer une requête sans poser de principal dans le contexte. Le
// garde-fou est là pour qu'un futur handler (tâche 15 notamment) qui
// oublierait de passer par authenticate échoue proprement plutôt que de
// paniquer sur un déréférencement nil.
func principalOrInternal(ctx context.Context, w http.ResponseWriter) (*memory.Principal, bool) {
	p := principalFrom(ctx)
	if p == nil {
		writeInternal(ctx, w, "missing principal", errors.New("no principal in request context"))
		return nil, false
	}
	return p, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// writeInternal journalise la cause et rend un message générique: un détail
// de schéma ou de DSN n'a rien à faire dans une réponse HTTP.
func writeInternal(ctx context.Context, w http.ResponseWriter, op string, err error) {
	slog.ErrorContext(ctx, "request failed", "operation", op, "error", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// authenticate résout le bearer token en principal. Un token absent, inconnu
// ou révoqué rend la même réponse, pour ne rien apprendre à l'appelant.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token := ""
		if after, ok := strings.CutPrefix(header, "Bearer "); ok {
			token = strings.TrimSpace(after)
		}
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}

		p, err := s.resolver.Resolve(r.Context(), token)
		if err != nil || p == nil {
			// Token absent, inconnu, révoqué ou base injoignable rendent la
			// même réponse: l'appelant n'apprend rien sur la cause.
			if err != nil {
				slog.WarnContext(r.Context(), "token resolution failed", "error", err)
			}
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}

		ctx := context.WithValue(r.Context(), principalKey{}, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// limitBody borne la taille des corps de requête. Un Content-Length annoncé
// au-delà de la limite est rejeté tout de suite en 413, sans lire un octet:
// MaxBytesReader seul ne suffit pas, parce qu'un client qui ment sur son
// Content-Length et envoie moins que promis ne serait jamais retenu par le
// lecteur, seulement par la lecture qu'en fait le décodeur JSON — trop tard
// pour distinguer "corps trop gros" de "JSON invalide".
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		max := s.cfg.Service.MaxRequestBytes
		if max <= 0 {
			max = defaultMaxRequestBytes
		}
		if r.ContentLength > max {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, max)
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logRequests journalise méthode, chemin, statut et durée. Jamais de corps,
// jamais de token.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.InfoContext(r.Context(), "http request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

// decodeJSON refuse les champs inconnus, ce qui attrape les fautes de frappe
// côté client au lieu de les ignorer silencieusement. Elle refuse aussi tout
// contenu qui suivrait la valeur JSON décodée: json.Decoder.Decode s'arrête
// dès qu'il a lu une valeur complète et ignore silencieusement la suite, ce
// qui accepterait aussi bien un corps `{...}{...}` qu'un corps dont la fin
// n'a jamais été comprise (le gros progressivement tronqué par
// MaxBytesReader se traduirait alors, une fois sur deux, par un décodage
// réussi sur un préfixe plausible plutôt que par une erreur).
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected content after JSON body")
		}
		return err
	}
	return nil
}
