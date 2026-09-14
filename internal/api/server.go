// Package api expose le service en HTTP.
package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/version"
)

type Ingester interface {
	Ingest(ctx context.Context, in memory.AppendInput, consistency string) (memory.IngestResult, error)
}

type Finder interface {
	Search(ctx context.Context, req memory.SearchRequest) (memory.SearchResponse, error)
}

type Resolver interface {
	Resolve(ctx context.Context, token string) (*memory.Principal, error)
}

// Conversations porte la déclaration explicite d'une conversation
// (POST /v1/conversations). Context est consultée avant tout Declare, comme
// ConversationDeleter.Context l'est avant tout SoftDelete et pour la même
// raison: Declare fait un ON CONFLICT DO UPDATE sur le scope et insère des
// participants, donc c'est une écriture sur une ressource qui peut déjà
// exister, et rien dans le corps de la requête ne dit à qui elle appartient.
// Sans cette lecture préalable, un conversation_id deviné suffisait à
// réécrire le scope de la conversation d'un voisin (ou d'un autre
// workspace), donc à la rendre lisible par tout son workspace.
type Conversations interface {
	Context(ctx context.Context, conversationID string) (memory.ConversationContext, error)
	Declare(ctx context.Context, conversationID, workspaceID, scope string, participants []string) error
}

// Ops est le port du domaine vers les routes d'exploitation (/health,
// /debug/stats). Stats rend memory.Stats, pas un map[string]any: voir son
// commentaire dans internal/memory pour l'engagement que ce type porte.
type Ops interface {
	Health(ctx context.Context) error
	Stats(ctx context.Context) (memory.Stats, error)
}

// Editor porte l'édition et la suppression. MessageOwner est consultée avant
// toute modification: les handlers PATCH/DELETE vérifient l'appartenance au
// workspace et le périmètre d'identité avant d'appeler EditMessage ou
// DeleteMessage, jamais après, pour qu'un refus n'ait jamais laissé une
// écriture dans un workspace ou sous une identité qui n'est pas celle de
// l'appelant.
type Editor interface {
	MessageOwner(ctx context.Context, messageID uuid.UUID) (workspaceID, authorKey string, err error)
	EditMessage(ctx context.Context, messageID uuid.UUID, content string) (memory.EditResult, error)
	DeleteMessage(ctx context.Context, messageID uuid.UUID) (memory.EditResult, error)
}

// ACLStore porte l'écriture et la lecture des ACL explicites d'une unité de
// mémoire. UnitContext rend memory.UnitContext, pas un type du paquet
// postgres: internal/api ne doit jamais importer internal/store/postgres
// dans un fichier non-test (voir le commentaire de memory.UnitContext), donc
// tout type qui traverse cette frontière est déclaré dans le domaine.
type ACLStore interface {
	UnitContext(ctx context.Context, unitID uuid.UUID) (memory.UnitContext, error)
	Grant(ctx context.Context, unitID uuid.UUID, principalKey, permission string) error
	Revoke(ctx context.Context, unitID uuid.UUID, principalKey string) (int64, error)
	List(ctx context.Context, unitID uuid.UUID) ([]memory.ACLEntry, error)
}

// ConversationDeleter porte la lecture du contexte d'accès d'une
// conversation et sa suppression douce. Context est consultée avant tout
// SoftDelete: handleDeleteConversation applique la même règle d'accès que
// le partage (canDeleteConversation) avant de rien supprimer, jamais après.
// workspaceID passé à SoftDelete est celui du principal appelant, jamais
// celui du corps de la requête.
type ConversationDeleter interface {
	Context(ctx context.Context, conversationID string) (memory.ConversationContext, error)
	SoftDelete(ctx context.Context, conversationID, workspaceID string) (int64, error)
}

type Server struct {
	cfg         *config.Config
	resolver    Resolver
	ingester    Ingester
	editor      Editor
	finder      Finder
	convs       Conversations
	ops         Ops
	acl         ACLStore
	convDeleter ConversationDeleter
	http        *http.Server

	// mu protège listener, posé par ListenAndServe et lu par Addr depuis un
	// autre goroutine (typiquement un test qui attend que le serveur écoute
	// avant d'ouvrir des connexions).
	mu       sync.Mutex
	listener net.Listener
}

// NewServer construit le serveur et son *http.Server sous-jacent en un seul
// geste: le champ http est donc posé avant que quoi que ce soit ne puisse le
// lire ou l'écrire depuis une autre goroutine (ListenAndServe démarre
// toujours après le retour de NewServer, jamais avant), ce qui élimine la
// course qu'aurait ouverte un http construit paresseusement à l'intérieur de
// ListenAndServe pendant qu'une autre goroutine appelle Shutdown.
func NewServer(cfg *config.Config, res Resolver, ing Ingester, ed Editor,
	find Finder, convs Conversations, ops Ops) *Server {
	s := &Server{cfg: cfg, resolver: res, ingester: ing, editor: ed,
		finder: find, convs: convs, ops: ops}
	s.http = &http.Server{
		Addr:              cfg.Service.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: cfg.Service.ReadHeaderTimeout,
		ReadTimeout:       cfg.Service.ReadTimeout,
		WriteTimeout:      cfg.Service.WriteTimeout,
	}
	return s
}

// WithACL câble la gestion des ACL explicites et la suppression de
// conversation. Séparé du constructeur pour ne pas retoucher tous les
// appels existants à NewServer: les deux restent optionnels (nil laisse
// les routes correspondantes répondre 501, voir aclPreflight et
// handleDeleteConversation) plutôt qu'obligatoires pour tout appelant qui
// n'en a pas besoin (les tests des tâches précédentes, notamment).
func (s *Server) WithACL(acl ACLStore, del ConversationDeleter) *Server {
	s.acl, s.convDeleter = acl, del
	return s
}

func (s *Server) Handler() http.Handler {
	// Routes authentifiées. Le motif inclut la méthode, donc net/http rend un
	// 405 de lui-même sur une méthode non déclarée.
	authed := http.NewServeMux()
	authed.HandleFunc("POST /v1/messages", s.handlePostMessage)
	authed.HandleFunc("PATCH /v1/messages/{message_id}", s.handlePatchMessage)
	authed.HandleFunc("DELETE /v1/messages/{message_id}", s.handleDeleteMessage)
	authed.HandleFunc("POST /v1/conversations", s.handlePostConversation)
	authed.HandleFunc("DELETE /v1/conversations/{conversation_id}", s.handleDeleteConversation)
	authed.HandleFunc("POST /v1/memories/search", s.handleSearch)
	authed.HandleFunc("POST /v1/memories/{memory_unit_id}/acl", s.handleGrantACL)
	authed.HandleFunc("GET /v1/memories/{memory_unit_id}/acl", s.handleListACL)
	authed.HandleFunc("DELETE /v1/memories/{memory_unit_id}/acl/{principal_key}", s.handleRevokeACL)

	root := http.NewServeMux()
	root.Handle("/v1/", s.limitBody(s.authenticate(authed)))
	root.HandleFunc("GET /health", s.handleHealth)
	root.HandleFunc("GET /about", s.handleAbout)
	root.HandleFunc("GET /debug/stats", s.handleStats)

	return logRequests(root)
}

// ListenAndServe démarre l'écoute et bloque jusqu'à ce que le serveur
// s'arrête de lui-même ou que ctx soit annulé, selon ce qui arrive en
// premier. Une annulation de ctx déclenche elle-même un Shutdown gracieux,
// avec le délai configuré: c'est ctx qui pilote l'arrêt, pas un appel
// séparé à Shutdown qui entrerait en course avec la goroutine qui sert.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() { errCh <- s.http.Serve(ln) }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		if err := s.Shutdown(s.cfg.Service.ShutdownGrace); err != nil {
			return err
		}
		// Attendre que Serve ait rendu la main: l'appelant de ListenAndServe
		// ne doit pas croire l'arrêt terminé avant que la goroutine de
		// service n'ait vraiment fini.
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// Addr rend l'adresse effectivement écoutée depuis qu'un ListenAndServe a
// démarré, ou "" avant. Utile quand Service.Listen vaut ":0" (port choisi
// par l'OS), notamment en test.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Shutdown arrête le serveur en laissant finir les requêtes en vol. Sûre à
// appeler sur un Server nil ou construit sans passer par NewServer: les deux
// rendent nil sans effet plutôt que de paniquer.
func (s *Server) Shutdown(grace time.Duration) error {
	if s == nil || s.http == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	return s.http.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.ops.Health(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, aboutResponse{
		Service: "cinnabar", Version: version.Version,
		Commit: version.Commit, BuildDate: version.BuildDate,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.ops.Stats(r.Context())
	if err != nil {
		writeInternal(r.Context(), w, "stats", err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
