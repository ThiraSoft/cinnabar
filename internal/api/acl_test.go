package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

type stubACL struct {
	uc      memory.UnitContext
	ucErr   error
	granted []string
	revoked []string
	entries []memory.ACLEntry
}

func (s *stubACL) UnitContext(context.Context, uuid.UUID) (memory.UnitContext, error) {
	return s.uc, s.ucErr
}
func (s *stubACL) Grant(_ context.Context, _ uuid.UUID, key, perm string) error {
	s.granted = append(s.granted, key+":"+perm)
	return nil
}
func (s *stubACL) Revoke(_ context.Context, _ uuid.UUID, key string) (int64, error) {
	s.revoked = append(s.revoked, key)
	return 1, nil
}
func (s *stubACL) List(context.Context, uuid.UUID) ([]memory.ACLEntry, error) {
	return s.entries, nil
}

// stubConvDeleter couvre ConversationDeleter. cc/ccErr contrôlent la réponse
// de Context, consultée par handleDeleteConversation avant tout appel à
// SoftDelete: le défaut (ws1, scope workspace) laisse passer les tests qui
// ne s'intéressent pas à cette vérification, ceux qui s'y intéressent la
// surchargent.
type stubConvDeleter struct {
	cc             memory.ConversationContext
	ccErr          error
	deleted        []string
	gotWorkspaceID string
	n              int64
}

func (s *stubConvDeleter) Context(_ context.Context, _ string) (memory.ConversationContext, error) {
	return s.cc, s.ccErr
}
func (s *stubConvDeleter) SoftDelete(_ context.Context, id, workspaceID string) (int64, error) {
	s.deleted = append(s.deleted, id)
	s.gotWorkspaceID = workspaceID
	return s.n, nil
}

func newACLServer(t *testing.T, uc memory.UnitContext) (http.Handler, *stubACL, *stubConvDeleter) {
	t.Helper()
	acl := &stubACL{uc: uc}
	del := &stubConvDeleter{
		n:  1,
		cc: memory.ConversationContext{WorkspaceID: "ws1", Scope: "workspace"},
	}
	srv := newFullTestServer(t, acl, del)
	return srv, acl, del
}

// --- POST /v1/memories/{id}/acl (partage) ---

func TestGrantACLRequiresReadAccessToTheUnit(t *testing.T) {
	// Le token de test peut incarner agent:* et user:paul. La conversation
	// source a user:paul pour participant, donc le partage est autorisé.
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", ConversationID: "conv_1",
		Scope: "participants", Participants: []string{"user:paul"},
	})

	unitID := uuid.New()
	rec := post(h, "/v1/memories/"+unitID.String()+"/acl", "tok",
		`{"principal_key":"agent:invite","permission":"read"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(acl.granted) != 1 || acl.granted[0] != "agent:invite:read" {
		t.Errorf("granted = %v", acl.granted)
	}
}

func TestGrantACLRefusedWhenCallerCannotReadTheUnit(t *testing.T) {
	// Aucun participant que le token puisse incarner: on ne partage pas ce
	// qu'on ne peut pas lire.
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", ConversationID: "conv_prive",
		Scope: "participants", Participants: []string{"user:alice"},
	})

	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "tok",
		`{"principal_key":"agent:invite"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(acl.granted) != 0 {
		t.Errorf("aucun partage ne doit avoir eu lieu: %v", acl.granted)
	}
}

func TestGrantACLAllowedOnWorkspaceScope(t *testing.T) {
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", ConversationID: "conv_pub",
		Scope: "workspace", Participants: []string{"user:alice"},
	})

	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "tok",
		`{"principal_key":"agent:invite"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: une unité de scope workspace est lisible", rec.Code)
	}
	if len(acl.granted) != 1 {
		t.Errorf("granted = %v", acl.granted)
	}
}

func TestGrantACLRefusedOnPrivateScopeWithoutACL(t *testing.T) {
	// Un scope private ou explicit ne s'ouvre jamais par la seule
	// participation: voir le commentaire de
	// sharableByParticipationOrWorkspaceScope. Ici l'appelant est bien
	// participant mais le scope est 'private', donc le refus doit tenir
	// quand même.
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", ConversationID: "conv_prive",
		Scope: "private", Participants: []string{"user:paul"},
	})

	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "tok",
		`{"principal_key":"agent:invite"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(acl.granted) != 0 {
		t.Errorf("aucun partage ne doit avoir eu lieu: %v", acl.granted)
	}
}

func TestGrantACLValidatesPayload(t *testing.T) {
	h, _, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", Scope: "workspace", Participants: []string{"user:paul"},
	})

	cases := map[string]struct {
		path, body string
		want       int
	}{
		"unite non uuid": {"/v1/memories/pas-un-uuid/acl",
			`{"principal_key":"agent:x"}`, http.StatusBadRequest},
		"principal vide": {"/v1/memories/" + uuid.New().String() + "/acl",
			`{"principal_key":""}`, http.StatusBadRequest},
		"principal blanc": {"/v1/memories/" + uuid.New().String() + "/acl",
			`{"principal_key":"   "}`, http.StatusBadRequest},
		"permission inconnue": {"/v1/memories/" + uuid.New().String() + "/acl",
			`{"principal_key":"agent:x","permission":"write"}`, http.StatusBadRequest},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := post(h, c.path, "tok", c.body); rec.Code != c.want {
				t.Errorf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}

func TestGrantACLTrimsPrincipalKey(t *testing.T) {
	// Un principal_key rendu avec des espaces autour ne serait jamais
	// retrouvé par l'égalité exacte de la recherche (a.principal_key = $2):
	// le partage doit donc écrire, et rendre, la version tronquée.
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", Scope: "workspace", Participants: []string{"user:paul"},
	})

	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "tok",
		`{"principal_key":"  agent:invite  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(acl.granted) != 1 || acl.granted[0] != "agent:invite:read" {
		t.Errorf("granted = %v, want [agent:invite:read] (tronqué)", acl.granted)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["principal_key"] != "agent:invite" {
		t.Errorf("principal_key rendu = %q, want %q (tronqué)", out["principal_key"], "agent:invite")
	}
}

func TestGrantACLRejectsOversizedPrincipalKey(t *testing.T) {
	h, _, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", Scope: "workspace", Participants: []string{"user:paul"},
	})
	tooLong := `{"principal_key":"` + strings.Repeat("a", maxPrincipalKeyLength+1) + `"}`
	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "tok", tooLong)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestGrantACLNormalizesEmptyPermission(t *testing.T) {
	// permission omise: la réponse doit refléter ce qui a vraiment été
	// écrit ("read"), jamais une chaîne vide qui laisserait croire que rien
	// n'a été normalisé.
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", Scope: "workspace", Participants: []string{"user:paul"},
	})
	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "tok",
		`{"principal_key":"agent:invite"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(acl.granted) != 1 || acl.granted[0] != "agent:invite:read" {
		t.Errorf("granted = %v", acl.granted)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["permission"] != "read" {
		t.Errorf("permission rendue = %q, want read", out["permission"])
	}
}

// --- Le cas d'auth: workspace étranger rend 404, pas 403 ---

func TestGrantACLRefusedAcrossWorkspacesReturns404(t *testing.T) {
	// Un identifiant d'unité est un uuid v5 déterministe sur le message
	// ancre: un appelant qui en détiendrait un ne doit jamais apprendre,
	// via un 403, qu'il désigne une unité existante dans le workspace d'un
	// autre. 404, comme pour un message d'un autre workspace
	// (handlePatchMessage/handleDeleteMessage).
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws_du_voisin", ConversationID: "conv_1",
		Scope: "workspace", Participants: []string{"user:paul"},
	})

	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "tok",
		`{"principal_key":"agent:invite"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if len(acl.granted) != 0 {
		t.Errorf("granted = %v", acl.granted)
	}
}

func TestGrantACLUnknownUnitReturns404(t *testing.T) {
	acl := &stubACL{ucErr: memory.ErrNotFound}
	del := &stubConvDeleter{n: 1, cc: memory.ConversationContext{WorkspaceID: "ws1", Scope: "workspace"}}
	h := newFullTestServer(t, acl, del)

	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "tok",
		`{"principal_key":"agent:x"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestGrantACLRequiresToken(t *testing.T) {
	h, _, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", Scope: "workspace", Participants: []string{"user:paul"},
	})
	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "",
		`{"principal_key":"agent:x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestGrantACLRejectsUnknownToken(t *testing.T) {
	acl := &stubACL{uc: memory.UnitContext{
		WorkspaceID: "ws1", Scope: "workspace", Participants: []string{"user:paul"},
	}}
	del := &stubConvDeleter{n: 1, cc: memory.ConversationContext{WorkspaceID: "ws1", Scope: "workspace"}}
	h, res := newFullTestServerWithResolver(t, acl, del)
	res.err = context.DeadlineExceeded

	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "mauvais",
		`{"principal_key":"agent:x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// --- Les routes de gestion (GET/DELETE .../acl): permissives, quel que
// soit le scope, tant que l'appelant est participant ---

func TestListAndRevokeACL(t *testing.T) {
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", Scope: "workspace", Participants: []string{"user:paul"},
	})
	acl.entries = []memory.ACLEntry{{PrincipalKey: "agent:invite", Permission: "read"}}

	unitID := uuid.New().String()

	req := httptest.NewRequest(http.MethodGet, "/v1/memories/"+unitID+"/acl", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	var out struct {
		Entries []struct {
			PrincipalKey string `json:"principal_key"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 1 || out.Entries[0].PrincipalKey != "agent:invite" {
		t.Errorf("entries = %+v", out.Entries)
	}

	req2 := httptest.NewRequest(http.MethodDelete,
		"/v1/memories/"+unitID+"/acl/agent:invite", nil)
	req2.Header.Set("Authorization", "Bearer tok")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	if len(acl.revoked) != 1 || acl.revoked[0] != "agent:invite" {
		t.Errorf("revoked = %v", acl.revoked)
	}
}

func TestListACLRefusedForNonParticipant(t *testing.T) {
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", ConversationID: "conv_prive",
		Scope: "participants", Participants: []string{"user:alice"},
	})
	acl.entries = []memory.ACLEntry{{PrincipalKey: "agent:invite", Permission: "read"}}

	req := httptest.NewRequest(http.MethodGet,
		"/v1/memories/"+uuid.New().String()+"/acl", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestListAndRevokeACLAllowedOnWorkspaceScopeWithoutParticipation épingle la
// règle corrigée: canManage doit être canShare plus la participation, sur
// n'importe quel scope, jamais canShare moins la branche workspace. Une
// conversation de scope 'workspace' laisse n'importe qui du workspace
// octroyer (canShare); si canManage ne regardait que la participation, un
// appelant qui n'est pas un participant enregistré pourrait octroyer sans
// jamais pouvoir lister ni révoquer ce qu'il vient d'accorder, exactement
// le même piège (un accès que personne ne peut retirer) que celui fermé par
// l'asymétrie elle-même, seulement déplacé d'un cran.
func TestListAndRevokeACLAllowedOnWorkspaceScopeWithoutParticipation(t *testing.T) {
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", ConversationID: "conv_pub",
		// Scope 'workspace', mais aucun participant enregistré que le
		// token de test (agent:*, user:paul) puisse incarner.
		Scope: "workspace", Participants: []string{"user:alice"},
	})

	unitID := uuid.New().String()

	grantRec := post(h, "/v1/memories/"+unitID+"/acl", "tok",
		`{"principal_key":"agent:invite"}`)
	if grantRec.Code != http.StatusOK {
		t.Fatalf("grant status = %d, body = %s", grantRec.Code, grantRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/memories/"+unitID+"/acl", nil)
	listReq.Header.Set("Authorization", "Bearer tok")
	listRec := httptest.NewRecorder()
	h.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s: un accès octroyé via le scope workspace doit rester listable", listRec.Code, listRec.Body.String())
	}

	revokeReq := httptest.NewRequest(http.MethodDelete,
		"/v1/memories/"+unitID+"/acl/agent:invite", nil)
	revokeReq.Header.Set("Authorization", "Bearer tok")
	revokeRec := httptest.NewRecorder()
	h.ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body = %s: un accès octroyé via le scope workspace doit rester révocable", revokeRec.Code, revokeRec.Body.String())
	}
	if len(acl.revoked) != 1 || acl.revoked[0] != "agent:invite" {
		t.Errorf("revoked = %v", acl.revoked)
	}
}

// TestRevokeACLAllowedOnPrivateScopeByParticipant est la preuve directe que
// l'asymétrie canShare/canManage ferme le piège identifié en revue: une
// conversation déclarée 'participants', partagée, puis figée en 'private',
// laisserait sinon la révocation impossible pour son propre participant.
// canManage l'autorise malgré le scope 'private', là où canShare
// (TestGrantACLRefusedOnPrivateScopeWithoutACL) la refuserait.
func TestRevokeACLAllowedOnPrivateScopeByParticipant(t *testing.T) {
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", ConversationID: "conv_prive",
		Scope: "private", Participants: []string{"user:paul"},
	})

	req := httptest.NewRequest(http.MethodDelete,
		"/v1/memories/"+uuid.New().String()+"/acl/agent:invite", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s: la révocation doit rester possible malgré le scope private", rec.Code, rec.Body.String())
	}
	if len(acl.revoked) != 1 || acl.revoked[0] != "agent:invite" {
		t.Errorf("revoked = %v", acl.revoked)
	}
}

func TestListACLRefusedAcrossWorkspacesReturns404(t *testing.T) {
	h, _, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws_du_voisin", Scope: "workspace", Participants: []string{"user:paul"},
	})
	req := httptest.NewRequest(http.MethodGet,
		"/v1/memories/"+uuid.New().String()+"/acl", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRevokeACLTrimsPrincipalKey(t *testing.T) {
	// Même exigence que TestGrantACLTrimsPrincipalKey, côté révocation:
	// principal_key devrait désigner la même chose des deux côtés.
	h, acl, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", Scope: "workspace", Participants: []string{"user:paul"},
	})
	req := httptest.NewRequest(http.MethodDelete,
		"/v1/memories/"+uuid.New().String()+"/acl/%20agent:invite%20", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(acl.revoked) != 1 || acl.revoked[0] != "agent:invite" {
		t.Errorf("revoked = %v, want [agent:invite] (tronqué)", acl.revoked)
	}
}

func TestRevokeACLRejectsOversizedPrincipalKey(t *testing.T) {
	h, _, _ := newACLServer(t, memory.UnitContext{
		WorkspaceID: "ws1", Scope: "workspace", Participants: []string{"user:paul"},
	})
	tooLong := strings.Repeat("a", maxPrincipalKeyLength+1)
	req := httptest.NewRequest(http.MethodDelete,
		"/v1/memories/"+uuid.New().String()+"/acl/"+tooLong, nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Les deux chemins 501 (ACL non câblées: WithACL jamais appelé) ---
// newTestServer construit un serveur qui n'appelle jamais WithACL, exactement
// le cas d'un opérateur qui n'a pas encore câblé la gestion des ACL.

func TestGrantACLDisabledReturns501(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	rec := post(h, "/v1/memories/"+uuid.New().String()+"/acl", "tok",
		`{"principal_key":"agent:x"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

func TestListACLDisabledReturns501(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/memories/"+uuid.New().String()+"/acl", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

func TestDeleteConversationDisabledReturns501(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/v1/conversations/conv_1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

// --- DELETE /v1/conversations/{id} ---

func TestDeleteConversation(t *testing.T) {
	h, _, del := newACLServer(t, memory.UnitContext{})

	req := httptest.NewRequest(http.MethodDelete, "/v1/conversations/conv_1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(del.deleted) != 1 || del.deleted[0] != "conv_1" {
		t.Errorf("deleted = %v", del.deleted)
	}
	if del.gotWorkspaceID != "ws1" {
		t.Errorf("workspace_id transmis = %q, want ws1 (celui du principal, pas du chemin)",
			del.gotWorkspaceID)
	}
}

func TestDeleteConversationAllowedForParticipant(t *testing.T) {
	acl := &stubACL{}
	del := &stubConvDeleter{n: 1, cc: memory.ConversationContext{
		WorkspaceID: "ws1", Scope: "participants", Participants: []string{"user:paul"},
	}}
	h := newFullTestServer(t, acl, del)

	req := httptest.NewRequest(http.MethodDelete, "/v1/conversations/conv_1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(del.deleted) != 1 {
		t.Errorf("deleted = %v", del.deleted)
	}
}

// TestDeleteConversationRefusedForNonParticipant est la preuve directe du
// point important 2 de la revue: un token qui ne peut incarner aucun
// participant d'une conversation 'participants' ne doit pas pouvoir la
// purger, même dans son propre workspace.
func TestDeleteConversationRefusedForNonParticipant(t *testing.T) {
	acl := &stubACL{}
	del := &stubConvDeleter{n: 1, cc: memory.ConversationContext{
		WorkspaceID: "ws1", Scope: "participants", Participants: []string{"user:alice"},
	}}
	h := newFullTestServer(t, acl, del)

	req := httptest.NewRequest(http.MethodDelete, "/v1/conversations/conv_1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(del.deleted) != 0 {
		t.Errorf("aucune suppression ne doit avoir eu lieu: %v", del.deleted)
	}
}

func TestDeleteConversationRefusedOnPrivateScopeWithoutParticipation(t *testing.T) {
	acl := &stubACL{}
	del := &stubConvDeleter{n: 1, cc: memory.ConversationContext{
		WorkspaceID: "ws1", Scope: "private", Participants: []string{"user:alice"},
	}}
	h := newFullTestServer(t, acl, del)

	req := httptest.NewRequest(http.MethodDelete, "/v1/conversations/conv_1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestDeleteConversationRefusedAcrossWorkspaces(t *testing.T) {
	acl := &stubACL{}
	del := &stubConvDeleter{n: 1, cc: memory.ConversationContext{
		WorkspaceID: "ws_du_voisin", Scope: "workspace",
	}}
	h := newFullTestServer(t, acl, del)

	req := httptest.NewRequest(http.MethodDelete, "/v1/conversations/conv_1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if len(del.deleted) != 0 {
		t.Errorf("aucune suppression ne doit avoir eu lieu: %v", del.deleted)
	}
}

func TestDeleteConversationContextNotFound(t *testing.T) {
	acl := &stubACL{}
	del := &stubConvDeleter{n: 1, ccErr: memory.ErrNotFound}
	h := newFullTestServer(t, acl, del)

	req := httptest.NewRequest(http.MethodDelete, "/v1/conversations/conv_absente", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteConversationAlreadyGone(t *testing.T) {
	// Simule la fenêtre de course rare où Context confirme l'existence mais
	// SoftDelete découvre, juste après, qu'une suppression concurrente est
	// passée avant: voir le commentaire du n == 0 dans
	// handleDeleteConversation.
	h, _, del := newACLServer(t, memory.UnitContext{})
	del.n = 0

	req := httptest.NewRequest(http.MethodDelete, "/v1/conversations/conv_absente", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteConversationRequiresToken(t *testing.T) {
	h, _, _ := newACLServer(t, memory.UnitContext{})

	req := httptest.NewRequest(http.MethodDelete, "/v1/conversations/conv_1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
