package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

type stubResolver struct {
	principal *memory.Principal
	err       error
}

func (s *stubResolver) Resolve(context.Context, string) (*memory.Principal, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.principal, nil
}

type stubIngester struct {
	got            memory.AppendInput
	gotConsistency string
	res            memory.IngestResult
	err            error
}

func (s *stubIngester) Ingest(_ context.Context, in memory.AppendInput,
	consistency string) (memory.IngestResult, error) {
	s.got, s.gotConsistency = in, consistency
	return s.res, s.err
}

type stubFinder struct {
	got memory.SearchRequest
	res memory.SearchResponse
	err error
}

func (s *stubFinder) Search(_ context.Context,
	req memory.SearchRequest) (memory.SearchResponse, error) {
	s.got = req
	return s.res, s.err
}

// stubConversations couvre le port Conversations. cc/ccErr contrôlent la
// réponse de Context, que handlePostConversation consulte désormais avant
// tout Declare: le zéro de la structure veut dire "aucune conversation de ce
// nom", le cas de création, qui est celui de la plupart des tests. declared
// enregistre les déclarations réellement passées, pour que les tests de refus
// puissent prouver qu'aucune écriture n'a eu lieu.
type stubConversations struct {
	err   error
	cc    memory.ConversationContext
	ccErr error

	declared []string
}

func (s *stubConversations) Context(context.Context, string) (memory.ConversationContext, error) {
	if s.ccErr != nil {
		return memory.ConversationContext{}, s.ccErr
	}
	if s.cc.WorkspaceID == "" {
		return memory.ConversationContext{}, memory.ErrNotFound
	}
	return s.cc, nil
}

func (s *stubConversations) Declare(_ context.Context, conversationID, workspaceID,
	scope string, _ []string) error {
	if s.err != nil {
		return s.err
	}
	s.declared = append(s.declared, conversationID+"/"+workspaceID+"/"+scope)
	return nil
}

// stubEditor couvre les routes PATCH et DELETE, implémentées en tâche 15.
// workspace/author/ownerErr contrôlent la réponse de MessageOwner, consultée
// par les handlers avant tout appel à EditMessage/DeleteMessage;
// editCalls/deleteCalls comptent les appels effectivement faits, pour que
// les tests puissent prouver qu'un refus n'a rien modifié.
type stubEditor struct {
	workspace   string
	author      string
	ownerErr    error
	res         memory.EditResult
	err         error
	editCalls   int
	deleteCalls int
}

func (s *stubEditor) MessageOwner(context.Context, uuid.UUID) (string, string, error) {
	return s.workspace, s.author, s.ownerErr
}
func (s *stubEditor) EditMessage(context.Context, uuid.UUID, string) (memory.EditResult, error) {
	s.editCalls++
	return s.res, s.err
}
func (s *stubEditor) DeleteMessage(context.Context, uuid.UUID) (memory.EditResult, error) {
	s.deleteCalls++
	return s.res, s.err
}

type stubOps struct{ err error }

func (s *stubOps) Health(context.Context) error { return s.err }
func (s *stubOps) Stats(context.Context) (memory.Stats, error) {
	return memory.Stats{QueueDepth: map[string]memory.QueueDepth{"embed": {}}}, nil
}

// blockingOps sert au test de coupure gracieuse: Health bloque jusqu'à ce que
// release soit fermé, ce qui simule une requête en vol pendant l'arrêt.
type blockingOps struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (o *blockingOps) Health(context.Context) error {
	o.once.Do(func() { close(o.started) })
	<-o.release
	return nil
}
func (o *blockingOps) Stats(context.Context) (memory.Stats, error) { return memory.Stats{}, nil }

func newTestServer(t *testing.T) (http.Handler, *stubResolver, *stubIngester, *stubFinder) {
	t.Helper()
	cfg := &config.Config{
		Service: config.Service{
			MaxRequestBytes: 1 << 20, ConsistencyDefault: "eventual",
		},
		Access: config.Access{DefaultScope: "participants"},
	}
	res := &stubResolver{principal: &memory.Principal{
		ClientID: uuid.New(), Label: "orchestrateur",
		WorkspaceID: "ws1", AllowedIdentities: []string{"agent:*", "user:paul"},
	}}
	ing := &stubIngester{res: memory.IngestResult{
		Message: memory.Message{
			MessageID: uuid.New(), ConversationID: "conv_1", SequenceNumber: 42,
		},
		Status: "searchable", GraphStatus: "pending",
	}}
	find := &stubFinder{}
	srv := NewServer(cfg, res, ing, &stubEditor{}, find,
		&stubConversations{}, &stubOps{})
	return srv.Handler(), res, ing, find
}

// newFullTestServer câble un serveur complet, ACL et suppression de
// conversation comprises, avec le même token/principal de test que
// newTestServer (identités agent:* et user:paul, workspace ws1). Utilisé par
// les tests de acl_test.go, qui n'ont pas besoin des retours d'ingestion ou
// de recherche que newTestServer expose.
func newFullTestServer(t *testing.T, acl ACLStore, del ConversationDeleter) http.Handler {
	t.Helper()
	h, _ := newFullTestServerWithResolver(t, acl, del)
	return h
}

// newFullTestServerWithResolver est la variante de newFullTestServer qui
// rend aussi le stubResolver, pour les tests qui doivent simuler un token
// inconnu ou une résolution en échec.
func newFullTestServerWithResolver(t *testing.T, acl ACLStore,
	del ConversationDeleter) (http.Handler, *stubResolver) {
	t.Helper()
	cfg := &config.Config{
		Service: config.Service{
			MaxRequestBytes: 1 << 20, ConsistencyDefault: "eventual",
		},
		Access: config.Access{DefaultScope: "participants"},
	}
	res := &stubResolver{principal: &memory.Principal{
		ClientID: uuid.New(), Label: "orchestrateur",
		WorkspaceID: "ws1", AllowedIdentities: []string{"agent:*", "user:paul"},
	}}
	srv := NewServer(cfg, res, &stubIngester{}, &stubEditor{}, &stubFinder{},
		&stubConversations{}, &stubOps{})
	srv.WithACL(acl, del)
	return srv.Handler(), res
}

func post(h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPostMessageHappyPath(t *testing.T) {
	h, _, ing, _ := newTestServer(t)

	rec := post(h, "/v1/messages", "tok", `{
		"workspace_id": "ws1",
		"conversation_id": "conv_8453",
		"author_key": "agent:86",
		"role": "user",
		"content": "blablabla",
		"consistency": "searchable"
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "searchable" {
		t.Errorf("status = %v", out["status"])
	}
	if out["sequence_number"].(float64) != 42 {
		t.Errorf("sequence_number = %v", out["sequence_number"])
	}
	if ing.gotConsistency != "searchable" {
		t.Errorf("consistency transmise = %q", ing.gotConsistency)
	}
	if ing.got.DefaultScope != "participants" {
		t.Errorf("le scope par défaut doit venir de la config: %q", ing.got.DefaultScope)
	}
	// author_key est libre côté format, seule la liste d'identités du token
	// décide. Le corps original de la spec utilisait "agent_86", qui ne
	// correspond à aucun motif du token de test (agent:*, user:paul) et
	// aurait échoué en 403: corrigé ici en "agent:86" pour correspondre au
	// motif agent:*. Voir TestPostMessageRejectsForeignIdentity pour le cas
	// où l'identité ne correspond vraiment à aucun motif.
}

func TestPostMessageRequiresToken(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/messages", "", `{"workspace_id":"ws1"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestPostMessageRejectsUnknownToken(t *testing.T) {
	h, res, _, _ := newTestServer(t)
	res.err = postgres.ErrNoClient

	rec := post(h, "/v1/messages", "mauvais", `{"workspace_id":"ws1"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "mauvais") {
		t.Error("la réponse ne doit jamais réfléchir le token")
	}
}

func TestPostMessageRejectsForeignWorkspace(t *testing.T) {
	h, _, _, _ := newTestServer(t)

	rec := post(h, "/v1/messages", "tok", `{
		"workspace_id": "ws_du_voisin",
		"conversation_id": "conv_1",
		"author_key": "user:paul",
		"role": "user",
		"content": "x"
	}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: un token ne doit pas écrire dans un autre workspace", rec.Code)
	}
}

func TestPostMessageRejectsForeignIdentity(t *testing.T) {
	h, _, _, _ := newTestServer(t)

	rec := post(h, "/v1/messages", "tok", `{
		"workspace_id": "ws1",
		"conversation_id": "conv_1",
		"author_key": "user:alice",
		"role": "user",
		"content": "x"
	}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: user:alice n'est pas dans les motifs du token", rec.Code)
	}
}

func TestPostMessageRequiresWorkspaceID(t *testing.T) {
	// Un workspace_id absent est une faute de forme, pas une tentative
	// cross-tenant: doit rendre 400, jamais le 403 qu'Authorize rendrait en
	// traitant l'absence comme un refus. Avant la restauration de ce
	// contrôle explicite, ce cas retombait sur Authorize (called with ""),
	// qui refuse tout workspace_id vide et rendait donc 403 à la place.
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/messages", "tok", `{
		"conversation_id": "conv_1",
		"author_key": "user:paul",
		"role": "user",
		"content": "x"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestPostMessageRequiresAuthorKey(t *testing.T) {
	// Même raisonnement que ci-dessus: MatchIdentity refuse une clé vide
	// inconditionnellement, donc Authorize seul rendrait 403 pour un
	// author_key simplement oublié.
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/messages", "tok", `{
		"workspace_id": "ws1",
		"conversation_id": "conv_1",
		"role": "user",
		"content": "x"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestPostMessageRejectsWorkspaceMismatchFromStore(t *testing.T) {
	// ErrWorkspaceMismatch remonte depuis MessageRepo.Append (via Ingest) quand
	// conversation_id désigne une conversation déjà rattachée à un autre
	// workspace. Le token de l'appelant porte pourtant bien son propre
	// workspace_id: la couche HTTP seule ne peut pas l'attraper, c'est la
	// conversation qui appartient à quelqu'un d'autre.
	//
	// 400, pas 403 ni 500. Pas 500 parce que c'est la faute de l'appelant.
	// Pas 403 non plus: partout ailleurs dans ce service, une ressource
	// existante dans un autre workspace rend 404 précisément pour ne rien
	// confirmer, et un 403 ici ne pourrait vouloir dire qu'une seule chose,
	// "ce conversation_id existe chez quelqu'un d'autre", sur un espace
	// d'identifiants que le client choisit lui-même. La route crée une
	// ressource: l'identifiant est inutilisable pour cet appelant, et c'est
	// tout ce qu'il a besoin de savoir.
	h, _, ing, _ := newTestServer(t)
	ing.err = memory.ErrWorkspaceMismatch

	rec := post(h, "/v1/messages", "tok", `{
		"workspace_id": "ws1",
		"conversation_id": "conv_shared",
		"author_key": "agent:86",
		"role": "user",
		"content": "x"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "conv_shared") ||
		strings.Contains(rec.Body.String(), "workspace") {
		t.Error("le détail de la conversation ou du workspace du voisin ne doit pas fuiter")
	}
}

// TestSearchRejectsUnknownStrategyWith400 : une faute de frappe dans
// strategies est une faute de l'appelant, pas une panne du service. Elle
// remontait en 500 parce que le handler versait toutes les erreurs de Search
// dans writeInternal, ce qui réveille l'astreinte pour un "densee".
func TestSearchRejectsUnknownStrategyWith400(t *testing.T) {
	h, _, _, find := newTestServer(t)
	find.err = fmt.Errorf("%w %q", memory.ErrUnknownStrategy, "densee")

	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id":"ws1","requester_key":"agent:86",
		"query":"tomates","strategies":["densee"]
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

func TestPostMessageValidatesPayload(t *testing.T) {
	h, _, _, _ := newTestServer(t)

	cases := map[string]string{
		"conversation absente": `{"workspace_id":"ws1","author_key":"user:paul","role":"user","content":"x"}`,
		"contenu vide":         `{"workspace_id":"ws1","conversation_id":"c","author_key":"user:paul","role":"user","content":""}`,
		"role inconnu":         `{"workspace_id":"ws1","conversation_id":"c","author_key":"user:paul","role":"robot","content":"x"}`,
		"consistency inconnue": `{"workspace_id":"ws1","conversation_id":"c","author_key":"user:paul","role":"user","content":"x","consistency":"vite"}`,
		"json invalide":        `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := post(h, "/v1/messages", "tok", body); rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestPostMessagePassesIdempotencyKey(t *testing.T) {
	h, _, ing, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"workspace_id":"ws1","conversation_id":"c","author_key":"user:paul",
		"role":"user","content":"x"
	}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Idempotency-Key", "evt-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ing.got.RequestID != "evt-123" {
		t.Errorf("RequestID = %q, want evt-123", ing.got.RequestID)
	}
}

func TestPostMessageRejectsTrailingContentAfterJSON(t *testing.T) {
	// json.Decoder.Decode s'arrête dès qu'il a lu une valeur JSON complète et
	// ignore silencieusement ce qui suit: un deuxième objet accolé
	// (contrebande de type `{...}{...}`) ne doit pas passer inaperçu.
	h, _, _, _ := newTestServer(t)
	body := `{"workspace_id":"ws1","conversation_id":"c","author_key":"user:paul","role":"user","content":"x"}{"trailing":"garbage"}`
	rec := post(h, "/v1/messages", "tok", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestPostMessageRejectsOversizedBody(t *testing.T) {
	cfg := &config.Config{
		Service: config.Service{MaxRequestBytes: 64, ConsistencyDefault: "eventual"},
		Access:  config.Access{DefaultScope: "participants"},
	}
	res := &stubResolver{principal: &memory.Principal{
		WorkspaceID: "ws1", AllowedIdentities: []string{"*"},
	}}
	srv := NewServer(cfg, res, &stubIngester{}, &stubEditor{}, &stubFinder{},
		&stubConversations{}, &stubOps{})

	big := `{"workspace_id":"ws1","content":"` + strings.Repeat("x", 500) + `"}`
	rec := post(srv.Handler(), "/v1/messages", "tok", big)

	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 413 ou 400", rec.Code)
	}
}

func TestSearchHappyPath(t *testing.T) {
	h, _, _, find := newTestServer(t)
	anchor := uuid.New()
	find.res = memory.SearchResponse{
		QueryID: "q1",
		Results: []memory.Result{{
			MemoryID: anchor.String(), ConversationID: "conv_1",
			AnchorMessageID: anchor, SourceMessageIDs: []uuid.UUID{anchor},
			Content: "user:paul : les tomates", SourceType: "original_messages",
			AccessReason: "conversation_participant",
			Scores:       map[string]float64{"final": 0.87, "dense": 0.82},
		}},
		ContextBlock: "<MEMORY_CONTEXT>...</MEMORY_CONTEXT>",
	}

	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id": "ws1",
		"requester_key": "agent:cuisine",
		"query": "les tomates de Paul",
		"result_limit": 3,
		"include_context_block": true
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		QueryID  string `json:"query_id"`
		Memories []struct {
			SourceMessageIDs []string           `json:"source_message_ids"`
			Scores           map[string]float64 `json:"scores"`
			SourceType       string             `json:"source_type"`
		} `json:"memories"`
		ContextBlock string `json:"context_block"`
		Debug        any    `json:"debug"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.QueryID != "q1" {
		t.Errorf("query_id = %q", out.QueryID)
	}
	if len(out.Memories) != 1 || len(out.Memories[0].SourceMessageIDs) != 1 {
		t.Errorf("memories = %+v", out.Memories)
	}
	if out.Memories[0].SourceType != "original_messages" {
		t.Errorf("source_type = %q", out.Memories[0].SourceType)
	}
	if out.ContextBlock == "" {
		t.Error("context_block demandé et absent")
	}
	if out.Debug != nil {
		t.Error("debug doit être absent quand le service ne le produit pas")
	}
	if find.got.ResultLimit != 3 {
		t.Errorf("result_limit transmis = %d", find.got.ResultLimit)
	}
	if strings.Contains(rec.Body.String(), "graph_facts") {
		t.Errorf("graph_facts ne doit pas apparaître sans fait: %s", rec.Body.String())
	}
}

func TestSearchExposesGraphFacts(t *testing.T) {
	h, _, _, find := newTestServer(t)
	observedAt := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	find.res = memory.SearchResponse{
		QueryID: "q1",
		GraphFacts: []memory.GraphFact{{
			Subject: "Tomate", Predicate: "has_observed_state", Object: "verte",
			ObservedAt: observedAt, Confidence: 0.9,
			Sources: []memory.FactSource{{MessageID: uuid.MustParse("7d1c2a4e-0000-4000-8000-000000000001"),
				ConversationID: "conv_paul", Metadata: json.RawMessage(`{"belief":"doute"}`)}},
		}},
	}

	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id": "ws1",
		"requester_key": "agent:cuisine",
		"query": "les tomates de Paul"
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		GraphFacts []struct {
			Subject    string `json:"subject"`
			Predicate  string `json:"predicate"`
			ObservedAt string `json:"observed_at"`
			Sources    []struct {
				MessageID      string          `json:"message_id"`
				ConversationID string          `json:"conversation_id"`
				Metadata       json.RawMessage `json:"metadata"`
			} `json:"sources"`
		} `json:"graph_facts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.GraphFacts) != 1 {
		t.Fatalf("graph_facts = %+v, want 1 entry", out.GraphFacts)
	}
	if out.GraphFacts[0].Subject != "Tomate" {
		t.Errorf("subject = %q", out.GraphFacts[0].Subject)
	}
	if src := out.GraphFacts[0].Sources; len(src) != 1 || src[0].ConversationID != "conv_paul" ||
		src[0].MessageID != "7d1c2a4e-0000-4000-8000-000000000001" || string(src[0].Metadata) != `{"belief":"doute"}` {
		t.Errorf("sources = %+v", src)
	}
	if out.GraphFacts[0].ObservedAt != observedAt.Format(time.RFC3339) {
		t.Errorf("observed_at = %q, want %q",
			out.GraphFacts[0].ObservedAt, observedAt.Format(time.RFC3339))
	}
}

// TestSearchDistinguishesAbsentFromPastValidUntil couvre le premier constat
// de la revue de la tâche 9: rien ne gardait le format des deux dates
// optionnelles ni, surtout, la différence entre une date absente et une date
// passée. Les deux disent l'inverse l'une de l'autre. Un valid_until absent
// veut dire que le fait est toujours vrai; un valid_until passé veut dire
// qu'il a cessé de l'être. Émettre un temps zéro formaté à la place d'une
// absence transformerait donc "toujours vrai" en "faux depuis l'an 1", et la
// mutation qui retire la garde sur le pointeur passait les 84 tests du
// paquet.
func TestSearchDistinguishesAbsentFromPastValidUntil(t *testing.T) {
	h, _, _, find := newTestServer(t)
	observedAt := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	validFrom := time.Date(2026, 6, 1, 8, 30, 0, 0, time.UTC)
	validUntil := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	find.res = memory.SearchResponse{
		QueryID: "q1",
		GraphFacts: []memory.GraphFact{
			{
				Subject: "Tomate", Predicate: "has_observed_state", Object: "verte",
				ObservedAt: observedAt, ValidFrom: &validFrom, ValidUntil: &validUntil,
			},
			{
				Subject: "Tomate", Predicate: "has_observed_state", Object: "rouge",
				ObservedAt: observedAt,
			},
		},
	}

	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id": "ws1",
		"requester_key": "agent:cuisine",
		"query": "les tomates de Paul"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Décodé dans une map pour distinguer une clé absente d'une clé présente
	// à la valeur nulle: une structure typée les confondrait toutes deux en
	// un pointeur nil, ce qui est exactement la distinction à garder.
	var out struct {
		GraphFacts []map[string]any `json:"graph_facts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.GraphFacts) != 2 {
		t.Fatalf("graph_facts = %+v, want 2 entries", out.GraphFacts)
	}

	ferme, ouvert := out.GraphFacts[0], out.GraphFacts[1]

	if got := ferme["valid_from"]; got != validFrom.Format(time.RFC3339) {
		t.Errorf("valid_from = %v, want %q", got, validFrom.Format(time.RFC3339))
	}
	if got := ferme["valid_until"]; got != validUntil.Format(time.RFC3339) {
		t.Errorf("valid_until = %v, want %q", got, validUntil.Format(time.RFC3339))
	}
	if _, present := ouvert["valid_until"]; present {
		t.Errorf("valid_until présent sur un fait toujours vrai: %v", ouvert["valid_until"])
	}
	if _, present := ouvert["valid_from"]; present {
		t.Errorf("valid_from présent sur un fait sans début connu: %v", ouvert["valid_from"])
	}
}

// TestSearchNeverPutsAFactAmongMemories couvre le second constat de la revue
// de la tâche 9: le critère 7 dit que le service rend des messages réels et
// jamais un résumé, et un fait est une dérivation. Rien ne l'empêchait de se
// glisser dans memories, où un appelant le prendrait pour une citation. La
// mutation qui injectait les faits dans les deux listes passait tous les
// TestSearch*.
func TestSearchNeverPutsAFactAmongMemories(t *testing.T) {
	h, _, _, find := newTestServer(t)
	find.res = memory.SearchResponse{
		QueryID: "q1",
		GraphFacts: []memory.GraphFact{{
			Subject: "Tomate", Predicate: "has_observed_state", Object: "verte",
			ObservedAt: time.Now().UTC(), Confidence: 0.9,
		}},
	}

	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id": "ws1",
		"requester_key": "agent:cuisine",
		"query": "les tomates de Paul"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Memories   []map[string]any `json:"memories"`
		GraphFacts []map[string]any `json:"graph_facts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// Le fait doit bien être passé quelque part, sinon le test ne prouve
	// rien: une réponse vide satisferait l'assertion suivante.
	if len(out.GraphFacts) != 1 {
		t.Fatalf("graph_facts = %+v, want 1 entry", out.GraphFacts)
	}
	if len(out.Memories) != 0 {
		t.Fatalf("memories = %+v, want aucune: la recherche n'a rendu aucun "+
			"message, seulement un fait", out.Memories)
	}
}

func TestSearchRejectsForeignRequesterKey(t *testing.T) {
	h, _, _, _ := newTestServer(t)

	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id": "ws1",
		"requester_key": "user:alice",
		"query": "x"
	}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestSearchRequiresRequesterKey(t *testing.T) {
	// Même raisonnement que TestPostMessageRequiresAuthorKey: un
	// requester_key oublié doit rendre 400, pas le 403 qu'Authorize
	// rendrait en traitant une identité vide comme un refus.
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id":"ws1","query":"x"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSearchRejectsForeignWorkspace(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id": "ws_du_voisin",
		"requester_key": "agent:cuisine",
		"query": "x"
	}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestSearchRejectsOversizedCandidateLimit(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id":"ws1","requester_key":"agent:cuisine","query":"x",
		"candidate_limit": 50000000
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSearchRejectsOversizedResultLimit(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id":"ws1","requester_key":"agent:cuisine","query":"x",
		"result_limit": 50000000
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSearchRejectsOversizedTokenBudget(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id":"ws1","requester_key":"agent:cuisine","query":"x",
		"token_budget": 50000000
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSearchRequiresQuery(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id":"ws1","requester_key":"agent:cuisine","query":"  "
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSearchInternalErrorIsNotLeaked(t *testing.T) {
	h, _, _, find := newTestServer(t)
	find.err = errors.New("postgres: relation \"memory_units\" does not exist")

	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id":"ws1","requester_key":"agent:cuisine","query":"x"
	}`)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "memory_units") {
		t.Error("un détail d'implémentation ne doit pas fuiter dans la réponse")
	}
}

func TestOpsRoutesNeedNoToken(t *testing.T) {
	h, _, _, _ := newTestServer(t)

	for _, path := range []string{"/health", "/about", "/debug/stats"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
	}
}

func TestHealthReportsUnavailable(t *testing.T) {
	cfg := &config.Config{Service: config.Service{MaxRequestBytes: 1 << 20}}
	srv := NewServer(cfg, &stubResolver{}, &stubIngester{}, &stubEditor{},
		&stubFinder{}, &stubConversations{}, &stubOps{err: errors.New("db down")})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	h, _, _, _ := newTestServer(t)

	rec := post(h, "/v1/nope", "tok", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("route inconnue: status = %d, want 404", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Errorf("méthode interdite: status = %d, want 405", rec2.Code)
	}
}

func TestPostConversationDeclaresScope(t *testing.T) {
	h, _, _, _ := newTestServer(t)

	rec := post(h, "/v1/conversations", "tok", `{
		"conversation_id": "conv_1",
		"workspace_id": "ws1",
		"scope": "workspace",
		"participants": ["user:paul", "agent:cuisine"]
	}`)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Errorf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestPostConversationRejectsUnknownScope(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/conversations", "tok", `{
		"conversation_id":"c","workspace_id":"ws1","scope":"top-secret"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestPostConversationRejectsForeignWorkspace(t *testing.T) {
	// Le workspace_id déclaré doit être vérifié contre le principal comme
	// pour toute autre route: sans ce contrôle un token pourrait déclarer
	// une conversation dans le workspace d'un autre.
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/conversations", "tok", `{
		"conversation_id":"c","workspace_id":"ws_du_voisin","scope":"workspace"
	}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestPostConversationRejectsForeignParticipant(t *testing.T) {
	// Chaque participant déclaré doit rester dans le périmètre d'identités
	// du token, sinon un client pourrait faire figurer n'importe quelle
	// identité comme participant d'une conversation.
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/conversations", "tok", `{
		"conversation_id":"c","workspace_id":"ws1","scope":"workspace",
		"participants":["user:paul","user:alice"]
	}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestPatchAndDeleteMessageRequireToken(t *testing.T) {
	// Les routes PATCH/DELETE restent authentifiées même si leur logique
	// métier n'arrive qu'en tâche 15: sans token, la requête ne doit jamais
	// atteindre le stub 501, elle doit s'arrêter à 401.
	h, _, _, _ := newTestServer(t)
	id := uuid.New()

	req := httptest.NewRequest(http.MethodPatch, "/v1/messages/"+id.String(),
		strings.NewReader(`{"content":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("PATCH sans token: status = %d, want 401", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodDelete, "/v1/messages/"+id.String(), nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("DELETE sans token: status = %d, want 401", rec2.Code)
	}
}

// TestListenAndServeGracefulShutdown couvre le chemin réseau réel (pas
// seulement Handler() via httptest): démarre le serveur sur un port
// aléatoire, laisse une requête s'installer en vol dans un handler qui
// bloque, annule le contexte pour déclencher l'arrêt, puis débloque le
// handler. ListenAndServe ne doit rendre la main qu'une fois la requête en
// vol terminée, et cette requête doit recevoir sa réponse normalement plutôt
// que de se faire couper.
func TestListenAndServeGracefulShutdown(t *testing.T) {
	ops := &blockingOps{started: make(chan struct{}), release: make(chan struct{})}
	cfg := &config.Config{Service: config.Service{
		Listen: "127.0.0.1:0", MaxRequestBytes: 1 << 20,
		ShutdownGrace: 5 * time.Second,
	}}
	srv := NewServer(cfg, &stubResolver{}, &stubIngester{}, &stubEditor{},
		&stubFinder{}, &stubConversations{}, ops)

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe(ctx) }()

	var addr string
	deadline := time.Now().Add(2 * time.Second)
	for addr == "" && time.Now().Before(deadline) {
		addr = srv.Addr()
		if addr == "" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if addr == "" {
		t.Fatal("le serveur n'a jamais commencé à écouter")
	}

	type result struct {
		status int
		err    error
	}
	reqDone := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/health")
		if err != nil {
			reqDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		reqDone <- result{status: resp.StatusCode}
	}()

	select {
	case <-ops.started:
	case <-time.After(2 * time.Second):
		t.Fatal("la requête en vol n'a jamais atteint le handler")
	}

	cancel()
	// Laisser l'arrêt gracieux démarrer avant de débloquer le handler:
	// c'est le coeur du test, la requête doit rester en vol pendant que
	// Shutdown est déjà en cours.
	time.Sleep(50 * time.Millisecond)
	close(ops.release)

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("ListenAndServe error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe n'a jamais rendu la main après l'annulation du contexte")
	}

	select {
	case res := <-reqDone:
		if res.err != nil {
			t.Fatalf("la requête en vol a échoué au lieu de se terminer proprement: %v", res.err)
		}
		if res.status != http.StatusOK {
			t.Errorf("status de la requête en vol = %d, want 200", res.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("la requête en vol n'a jamais reçu de réponse")
	}
}

// newConversationTestServer câble un serveur dont le port Conversations est
// contrôlé par le test, pour couvrir POST /v1/conversations sur une
// conversation qui existe déjà.
func newConversationTestServer(t *testing.T, convs *stubConversations) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Service: config.Service{
			MaxRequestBytes: 1 << 20, ConsistencyDefault: "eventual",
		},
		Access: config.Access{DefaultScope: "participants"},
	}
	res := &stubResolver{principal: &memory.Principal{
		ClientID: uuid.New(), Label: "orchestrateur",
		WorkspaceID: "ws1", AllowedIdentities: []string{"agent:*", "user:paul"},
	}}
	srv := NewServer(cfg, res, &stubIngester{}, &stubEditor{}, &stubFinder{},
		convs, &stubOps{})
	return srv.Handler()
}

// TestPostConversationOnForeignWorkspaceConversationIs404 ferme la forme
// cross-tenant du contournement: conversation_id est un TEXT libre choisi par
// le client, donc devinable depuis un autre workspace. Sans lecture préalable
// de la conversation, le DO UPDATE de Declare réécrivait le scope d'une
// conversation du voisin. 404 comme partout ailleurs pour une ressource d'un
// autre workspace, et surtout aucun Declare.
func TestPostConversationOnForeignWorkspaceConversationIs404(t *testing.T) {
	convs := &stubConversations{cc: memory.ConversationContext{
		WorkspaceID: "ws_du_voisin", Scope: "private",
		Participants: []string{"user:alice"},
	}}
	h := newConversationTestServer(t, convs)

	rec := post(h, "/v1/conversations", "tok", `{
		"conversation_id":"conv_alice","workspace_id":"ws1","scope":"workspace"
	}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if len(convs.declared) != 0 {
		t.Errorf("aucune déclaration ne doit avoir eu lieu: %v", convs.declared)
	}
}

// TestPostConversationRefusedWhenCallerCannotReachTheConversation ferme la
// forme intra-workspace: un scope 'private' ou 'explicit' dont l'appelant
// n'incarne aucun participant ne doit pas pouvoir basculer en 'workspace',
// ce qui rendrait tous ses souvenirs lisibles par n'importe qui du
// workspace. Même règle que la suppression (canDeleteConversation).
func TestPostConversationRefusedWhenCallerCannotReachTheConversation(t *testing.T) {
	convs := &stubConversations{cc: memory.ConversationContext{
		WorkspaceID: "ws1", Scope: "private",
		Participants: []string{"user:alice"},
	}}
	h := newConversationTestServer(t, convs)

	rec := post(h, "/v1/conversations", "tok", `{
		"conversation_id":"conv_alice_prive","workspace_id":"ws1","scope":"workspace"
	}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(convs.declared) != 0 {
		t.Errorf("aucune déclaration ne doit avoir eu lieu: %v", convs.declared)
	}
}

// TestPostConversationBootstrapSequenceStillWorks protège la séquence
// d'amorçage documentée dans le README: déclarer en 'participants', partager,
// puis figer en 'explicit'. Le déclarant y est participant, donc
// canDeleteConversation l'admet et le re-Declare doit passer.
func TestPostConversationBootstrapSequenceStillWorks(t *testing.T) {
	convs := &stubConversations{}
	h := newConversationTestServer(t, convs)

	rec := post(h, "/v1/conversations", "tok", `{
		"conversation_id":"conv_1","workspace_id":"ws1","scope":"participants",
		"participants":["user:paul"]
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("création: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// La conversation existe désormais, avec user:paul en participant, que le
	// token de test peut incarner.
	convs.cc = memory.ConversationContext{
		WorkspaceID: "ws1", Scope: "participants",
		Participants: []string{"user:paul"},
	}
	rec2 := post(h, "/v1/conversations", "tok", `{
		"conversation_id":"conv_1","workspace_id":"ws1","scope":"explicit"
	}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-déclaration: status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	want := []string{"conv_1/ws1/participants", "conv_1/ws1/explicit"}
	if len(convs.declared) != 2 ||
		convs.declared[0] != want[0] || convs.declared[1] != want[1] {
		t.Errorf("declared = %v, want %v", convs.declared, want)
	}
}

// TestSearchRejectsTooManyKnownSubjects: les sujets connus partent en
// paramètre d'un prédicat que le planificateur résout en filtre par ligne sur
// les entités du workspace, et rien ne bornait leur nombre. La revue de la
// tâche 8 l'a relevé en même temps qu'elle établissait qu'ils ne peuvent pas
// élargir l'accès: le risque est le coût et le bruit, pas la fuite.
func TestSearchRejectsTooManyKnownSubjects(t *testing.T) {
	h, _, _, _ := newTestServer(t)

	subjects := make([]string, maxKnownSubjects+1)
	for i := range subjects {
		subjects[i] = fmt.Sprintf(`"user:p%d"`, i)
	}
	body := fmt.Sprintf(`{
		"workspace_id": "ws1",
		"requester_key": "agent:cuisine",
		"query": "x",
		"known_subjects": [%s]
	}`, strings.Join(subjects, ","))

	rec := post(h, "/v1/memories/search", "tok", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}

	// Et la borne ne doit pas mordre juste en dessous.
	ok := subjects[:maxKnownSubjects]
	body = fmt.Sprintf(`{
		"workspace_id": "ws1",
		"requester_key": "agent:cuisine",
		"query": "x",
		"known_subjects": [%s]
	}`, strings.Join(ok, ","))
	if rec := post(h, "/v1/memories/search", "tok", body); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 à la borne exacte, body = %s",
			rec.Code, rec.Body.String())
	}
}
