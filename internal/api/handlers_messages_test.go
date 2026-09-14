package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// newEditTestServer construit un serveur dont l'editor est un stubEditor
// contrôlable, pour les tests PATCH/DELETE /v1/messages/{message_id}. Le
// token de test ne peut agir que sous l'identité "user:paul": les tests qui
// veulent passer l'autorisation doivent donc fixer ed.author à "user:paul",
// et ceux qui veulent simuler un message d'un autre auteur y mettent autre
// chose.
func newEditTestServer(t *testing.T, principalWorkspace string) (http.Handler, *stubEditor) {
	t.Helper()
	cfg := &config.Config{
		Service: config.Service{MaxRequestBytes: 1 << 20},
		Access:  config.Access{DefaultScope: "participants"},
	}
	res := &stubResolver{principal: &memory.Principal{
		ClientID: uuid.New(), Label: "orchestrateur",
		WorkspaceID: principalWorkspace, AllowedIdentities: []string{"user:paul"},
	}}
	ed := &stubEditor{}
	srv := NewServer(cfg, res, &stubIngester{}, ed, &stubFinder{},
		&stubConversations{}, &stubOps{})
	return srv.Handler(), ed
}

func patch(h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func del(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPatchMessageHappyPath(t *testing.T) {
	id := uuid.New()
	anchor := uuid.New()
	h, ed := newEditTestServer(t, "ws1")
	ed.workspace, ed.author = "ws1", "user:paul"
	ed.res = memory.EditResult{
		Message:            memory.Message{MessageID: id, WorkspaceID: "ws1"},
		DeactivatedAnchors: []uuid.UUID{anchor},
		GraphStatus:        "pending",
	}

	rec := patch(h, "/v1/messages/"+id.String(), "tok", `{"content":"nouveau contenu"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "reindexing" {
		t.Errorf("status = %v, want reindexing", out["status"])
	}
	if out["graph_status"] != "pending" {
		t.Errorf("graph_status = %v", out["graph_status"])
	}
	anchors, _ := out["deactivated_anchors"].([]any)
	if len(anchors) != 1 || anchors[0] != anchor.String() {
		t.Errorf("deactivated_anchors = %v", out["deactivated_anchors"])
	}
	if ed.editCalls != 1 {
		t.Errorf("EditMessage appelé %d fois, want 1", ed.editCalls)
	}
}

func TestDeleteMessageHappyPath(t *testing.T) {
	id := uuid.New()
	h, ed := newEditTestServer(t, "ws1")
	ed.workspace, ed.author = "ws1", "user:paul"
	ed.res = memory.EditResult{
		Message:     memory.Message{MessageID: id, WorkspaceID: "ws1"},
		GraphStatus: "pending",
	}

	rec := del(h, "/v1/messages/"+id.String(), "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "deleted" {
		t.Errorf("status = %v, want deleted", out["status"])
	}
	if ed.deleteCalls != 1 {
		t.Errorf("DeleteMessage appelé %d fois, want 1", ed.deleteCalls)
	}
}

// TestPatchMessageForeignWorkspaceReturns404NotModified couvre la
// correction: un message d'un autre workspace rend 404, pas 403, et
// EditMessage n'est jamais appelé.
func TestPatchMessageForeignWorkspaceReturns404NotModified(t *testing.T) {
	id := uuid.New()
	h, ed := newEditTestServer(t, "ws1")
	ed.workspace = "ws2" // le message appartient à un autre workspace

	rec := patch(h, "/v1/messages/"+id.String(), "tok", `{"content":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ed.editCalls != 0 {
		t.Error("EditMessage ne doit pas être appelé sur un message d'un autre workspace")
	}
}

func TestDeleteMessageForeignWorkspaceReturns404NotModified(t *testing.T) {
	id := uuid.New()
	h, ed := newEditTestServer(t, "ws1")
	ed.workspace = "ws2"

	rec := del(h, "/v1/messages/"+id.String(), "tok")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ed.deleteCalls != 0 {
		t.Error("DeleteMessage ne doit pas être appelé sur un message d'un autre workspace")
	}
}

// TestPatchMessageForeignIdentityReturns403NotModified couvre l'escalade de
// privilège corrigée par la revue (Important 4): un message du propre
// workspace de l'appelant, mais écrit par un auteur que le token ne peut pas
// usurper, rend 403 (pas 404: l'appelant sait déjà que la ressource existe
// puisqu'elle est dans son propre workspace) et ne modifie rien.
func TestPatchMessageForeignIdentityReturns403NotModified(t *testing.T) {
	id := uuid.New()
	h, ed := newEditTestServer(t, "ws1")
	ed.workspace, ed.author = "ws1", "agent:cuisine" // hors du périmètre du token (user:paul)

	rec := patch(h, "/v1/messages/"+id.String(), "tok", `{"content":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if ed.editCalls != 0 {
		t.Error("EditMessage ne doit pas être appelé sur un message d'un auteur hors périmètre")
	}
}

func TestDeleteMessageForeignIdentityReturns403NotModified(t *testing.T) {
	id := uuid.New()
	h, ed := newEditTestServer(t, "ws1")
	ed.workspace, ed.author = "ws1", "agent:cuisine"

	rec := del(h, "/v1/messages/"+id.String(), "tok")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if ed.deleteCalls != 0 {
		t.Error("DeleteMessage ne doit pas être appelé sur un message d'un auteur hors périmètre")
	}
}

// TestPatchMessageNonexistentReturns404 couvre l'autre moitié de la
// correction: un identifiant qui n'existe pas rend aussi 404.
func TestPatchMessageNonexistentReturns404(t *testing.T) {
	id := uuid.New()
	h, ed := newEditTestServer(t, "ws1")
	ed.ownerErr = memory.ErrNotFound

	rec := patch(h, "/v1/messages/"+id.String(), "tok", `{"content":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ed.editCalls != 0 {
		t.Error("EditMessage ne doit pas être appelé sur un message inexistant")
	}
}

func TestDeleteMessageNonexistentReturns404(t *testing.T) {
	id := uuid.New()
	h, ed := newEditTestServer(t, "ws1")
	ed.ownerErr = memory.ErrNotFound

	rec := del(h, "/v1/messages/"+id.String(), "tok")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ed.deleteCalls != 0 {
		t.Error("DeleteMessage ne doit pas être appelé sur un message inexistant")
	}
}

func TestPatchMessageRejectsEmptyContent(t *testing.T) {
	id := uuid.New()
	h, ed := newEditTestServer(t, "ws1")
	ed.workspace, ed.author = "ws1", "user:paul"

	rec := patch(h, "/v1/messages/"+id.String(), "tok", `{"content":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if ed.editCalls != 0 {
		t.Error("EditMessage ne doit pas être appelé avec un contenu vide")
	}
}

func TestPatchMessageRejectsInvalidMessageID(t *testing.T) {
	h, _ := newEditTestServer(t, "ws1")
	rec := patch(h, "/v1/messages/pas-un-uuid", "tok", `{"content":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
