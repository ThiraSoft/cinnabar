package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// maxPrincipalKeyLength borne principal_key dans une requête de partage.
// Aucune limite n'existe côté schéma (colonne TEXT libre): en refuser une
// démesurée à la frontière HTTP coûte moins cher qu'un incident de
// production découvert plus tard.
const maxPrincipalKeyLength = 200

// sharableByParticipationOrWorkspaceScope applique la règle retenue pour
// cette tâche: on ne partage que ce qu'on peut lire. L'appelant doit
// pouvoir soit incarner au moins un participant de la conversation, soit
// bénéficier d'un scope 'workspace' (ouvert à tout le workspace). C'est
// exactement la même disjonction que ReadableCTE applique côté SQL pour les
// scopes 'workspace' et 'participants': un appelant qui ne passerait
// aucune des deux branches ne pourrait pas non plus lire la ressource par
// la recherche.
//
// Les scopes 'private' et 'explicit' ne passent par aucune des deux
// branches (voir le commentaire de ReadableCTE): ils ne s'ouvrent que par
// une ligne memory_unit_acl, jamais par la seule participation. C'est
// pourquoi cette fonction n'inspecte jamais memory_unit_acl elle-même: un
// participant d'une conversation 'private' n'a par construction aucun
// moyen de se prouver "lecteur" ici, exactement comme il n'en aurait aucun
// pour cette même conversation par la recherche. Le seul moyen légitime de
// rendre une conversation partageable au sens de cette fonction est de la
// déclarer 'participants' ou 'workspace' au départ (voir la section ACL du
// README pour la séquence d'amorçage complète: déclarer en 'participants',
// partager, puis figer en 'explicit' une fois les bons partages en place).
//
// N'inspecte jamais le workspace: ce prédicat-là est vérifié séparément par
// l'appelant via Principal.AuthorizeWorkspace, dont le refus doit rendre
// 404 (ressource d'un autre workspace) plutôt que le 403 que rendrait une
// disjonction qui mélangerait les deux (voir aclUnitContext et
// handleDeleteConversation).
func sharableByParticipationOrWorkspaceScope(p *memory.Principal, scope string, participants []string) bool {
	if scope == "workspace" {
		return true
	}
	if scope != "participants" {
		return false
	}
	return matchesAnyParticipant(p, participants)
}

func matchesAnyParticipant(p *memory.Principal, participants []string) bool {
	for _, part := range participants {
		if memory.MatchIdentity(p.AllowedIdentities, part) {
			return true
		}
	}
	return false
}

// canShare décide si l'appelant peut ajouter un principal à l'ACL d'une
// unité (partage: POST .../acl). Voir
// sharableByParticipationOrWorkspaceScope pour la règle elle-même.
func canShare(p *memory.Principal, uc memory.UnitContext) bool {
	return sharableByParticipationOrWorkspaceScope(p, uc.Scope, uc.Participants)
}

// canManage décide si l'appelant peut lister ou révoquer les ACL d'une
// unité (GET/DELETE .../acl). C'est canShare, plus la participation quel
// que soit le scope: canManage(p, uc) est vrai si canShare(p, uc) l'est, ou
// si l'appelant incarne un participant de la conversation même quand le
// scope est 'private'/'explicit' (les deux scopes où canShare refuse
// toujours, participation ou pas). Formulé ainsi, jamais comme la seule
// participation, pour que canManage reste structurellement au moins aussi
// permissif que canShare sur toute entrée: une conversation de scope
// 'workspace' laisse n'importe qui du workspace octroyer via canShare, donc
// canManage doit aussi le laisser lister et révoquer, participant enregistré
// ou pas, sous peine de recréer exactement le piège que cette asymétrie
// existe pour fermer, seulement déplacé d'un cran (un octroi que son propre
// auteur ne pourrait plus retirer). Voir
// TestListAndRevokeACLAllowedOnWorkspaceScopeWithoutParticipation.
//
// L'asymétrie elle-même reste délibérée: octroyer élargit l'accès et doit
// rester strict (canShare) : lister et révoquer ne peuvent jamais rien
// élargir, seulement retirer un accès ou dire à un participant qui peut
// déjà lire sa propre conversation, donc les rendre plus permissifs que
// canShare sur les scopes qu'il refuse ('private'/'explicit', par la
// participation) ne fuit rien de nouveau. Sans cette extension aux scopes
// refusés par canShare, une conversation déclarée 'participants', partagée
// à un tiers pendant qu'elle l'est, puis figée en 'private'/'explicit' par
// un Declare ultérieur, laisserait le bénéficiaire du partage continuer de
// lire tout en rendant la révocation impossible pour quiconque sauf par
// SQL direct: un accès qui ne peut plus être retiré par personne est pire
// qu'un accès permissif à retirer.
func canManage(p *memory.Principal, uc memory.UnitContext) bool {
	return canShare(p, uc) || matchesAnyParticipant(p, uc.Participants)
}

// canDeleteConversation décide si l'appelant peut supprimer une
// conversation entière. Même règle que canShare (participation ou scope
// 'workspace'), volontairement stricte plutôt qu'alignée sur canManage:
// supprimer est bien plus destructeur que révoquer une seule ACL, donc
// c'est le partage, pas la gestion des ACL, qui est le bon gabarit ici.
func canDeleteConversation(p *memory.Principal, cc memory.ConversationContext) bool {
	return sharableByParticipationOrWorkspaceScope(p, cc.Scope, cc.Participants)
}

// aclUnitContext décode l'identifiant d'unité, résout le principal, charge
// le contexte de partage de l'unité et ferme le cas d'auth commun aux trois
// routes ACL: une unité d'un autre workspace rend 404, jamais 403, pour ne
// jamais confirmer à l'appelant qu'un identifiant qu'il ne fait que deviner
// (les uuid v5 de ce service sont déterministes sur le message ancre)
// désigne bien une unité existante ailleurs que dans son propre workspace
// — exactement la règle que handlePatchMessage/handleDeleteMessage
// appliquent déjà pour un message d'un autre workspace.
//
// N'applique PAS la décision finale (canShare ou canManage selon la
// route): c'est aux appelants de aclUnitContext de le faire, avec le
// contexte et le principal qu'elle rend, puisque la règle diffère entre
// partager (canShare, strict) et gérer (canManage, permissif).
//
// operation nomme l'appelant pour le message de log en cas de refus
// (jamais le token ni le contenu, comme les autres chemins de refus du
// paquet).
func (s *Server) aclUnitContext(w http.ResponseWriter, r *http.Request,
	operation string) (uuid.UUID, *memory.Principal, memory.UnitContext, bool) {

	if s.acl == nil {
		writeError(w, http.StatusNotImplemented, "acl management is not enabled")
		return uuid.Nil, nil, memory.UnitContext{}, false
	}
	unitID, err := uuid.Parse(r.PathValue("memory_unit_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "memory_unit_id must be a uuid")
		return uuid.Nil, nil, memory.UnitContext{}, false
	}
	p, ok := principalOrInternal(r.Context(), w)
	if !ok {
		return uuid.Nil, nil, memory.UnitContext{}, false
	}

	uc, err := s.acl.UnitContext(r.Context(), unitID)
	if errors.Is(err, memory.ErrNotFound) {
		writeError(w, http.StatusNotFound, "memory unit not found")
		return uuid.Nil, nil, memory.UnitContext{}, false
	}
	if err != nil {
		writeInternal(r.Context(), w, "unit context", err)
		return uuid.Nil, nil, memory.UnitContext{}, false
	}
	if err := p.AuthorizeWorkspace(uc.WorkspaceID); err != nil {
		slog.WarnContext(r.Context(), "cross-tenant acl attempt",
			"operation", operation, "client_id", p.ClientID)
		writeError(w, http.StatusNotFound, "memory unit not found")
		return uuid.Nil, nil, memory.UnitContext{}, false
	}
	return unitID, p, uc, true
}

func (s *Server) handleGrantACL(w http.ResponseWriter, r *http.Request) {
	// L'autorisation passe avant tout décodage du corps, comme les autres
	// routes d'écriture (handlePostMessage, handlePatchMessage, ...): un
	// appelant qui n'a pas le droit de partager cette unité ne doit pas
	// apprendre, via un 400 sur la forme de son corps, quoi que ce soit
	// sur ce que le service en attendrait.
	unitID, p, uc, ok := s.aclUnitContext(w, r, "grant_acl")
	if !ok {
		return
	}
	if !canShare(p, uc) {
		slog.WarnContext(r.Context(), "refused share attempt",
			"operation", "grant_acl", "client_id", p.ClientID)
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req grantACLRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	principalKey := strings.TrimSpace(req.PrincipalKey)
	if principalKey == "" {
		writeError(w, http.StatusBadRequest, "principal_key is required")
		return
	}
	if len(principalKey) > maxPrincipalKeyLength {
		writeError(w, http.StatusBadRequest, "principal_key is too long")
		return
	}
	// Une seule permission existe dans cette version. La refuser
	// explicitement vaut mieux que l'accepter et ne rien en faire.
	// Normalisée ici, pas seulement dans le repo: la réponse doit refléter
	// ce qui a vraiment été écrit, jamais une valeur par défaut que seul le
	// repo aurait appliquée en silence.
	permission := req.Permission
	if permission == "" {
		permission = "read"
	}
	if permission != "read" {
		writeError(w, http.StatusBadRequest, "permission must be read")
		return
	}

	if err := s.acl.Grant(r.Context(), unitID, principalKey, permission); err != nil {
		writeInternal(r.Context(), w, "grant acl", err)
		return
	}
	// Le principal bénéficiaire n'est volontairement pas contraint aux
	// motifs d'identité du token appelant: voir la section ACL du README.
	// Ce n'est pas un accès en blanc pour autant, borné qu'il est par le
	// fait qu'exercer ce partage exige, plus tard, un token à part
	// autorisé pour ce workspace et cette identité précise.
	writeJSON(w, http.StatusOK, map[string]string{
		"memory_unit_id": unitID.String(),
		"principal_key":  principalKey,
		"permission":     permission,
	})
}

func (s *Server) handleListACL(w http.ResponseWriter, r *http.Request) {
	unitID, p, uc, ok := s.aclUnitContext(w, r, "list_acl")
	if !ok {
		return
	}
	if !canManage(p, uc) {
		slog.WarnContext(r.Context(), "refused list attempt",
			"operation", "list_acl", "client_id", p.ClientID)
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	entries, err := s.acl.List(r.Context(), unitID)
	if err != nil {
		writeInternal(r.Context(), w, "list acl", err)
		return
	}
	out := aclListResponse{
		MemoryUnitID: unitID.String(), Entries: []aclEntryDTO{},
	}
	for _, e := range entries {
		out.Entries = append(out.Entries,
			aclEntryDTO{PrincipalKey: e.PrincipalKey, Permission: e.Permission})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRevokeACL(w http.ResponseWriter, r *http.Request) {
	unitID, p, uc, ok := s.aclUnitContext(w, r, "revoke_acl")
	if !ok {
		return
	}
	if !canManage(p, uc) {
		slog.WarnContext(r.Context(), "refused revoke attempt",
			"operation", "revoke_acl", "client_id", p.ClientID)
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	// Même normalisation que handleGrantACL: un principal_key devrait
	// désigner la même chose des deux côtés, tronqué et borné en longueur,
	// même si un décalage ici ne fait aujourd'hui que révoquer 0 ligne au
	// lieu de la bonne (aucun risque de sécurité), pour que les deux
	// chemins s'accordent sur ce qu'est un principal_key.
	principal := strings.TrimSpace(r.PathValue("principal_key"))
	if principal == "" {
		writeError(w, http.StatusBadRequest, "principal_key is required")
		return
	}
	if len(principal) > maxPrincipalKeyLength {
		writeError(w, http.StatusBadRequest, "principal_key is too long")
		return
	}
	n, err := s.acl.Revoke(r.Context(), unitID, principal)
	if err != nil {
		writeInternal(r.Context(), w, "revoke acl", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"memory_unit_id": unitID.String(),
		"principal_key":  principal,
		"removed":        n,
	})
}

// handleDeleteConversation supprime (soft delete) une conversation entière
// et toutes ses unités.
//
// Ferme les quatre cas d'auth avant tout travail, comme les autres routes:
// aucun token / token inconnu sont fermés par authenticate en amont; un
// workspace étranger rend 404 exactement comme un message d'un autre
// workspace (handlePatchMessage, handleDeleteMessage), pour ne jamais
// confirmer l'existence d'une ressource qui n'est pas dans le workspace de
// l'appelant; une identité hors des motifs du token rend 403
// (canDeleteConversation), pour qu'un token limité à une seule identité ne
// puisse pas purger une conversation entière dont il ne peut lire ou
// écrire aucun message.
func (s *Server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	if s.convDeleter == nil {
		writeError(w, http.StatusNotImplemented, "conversation deletion is not enabled")
		return
	}
	id := r.PathValue("conversation_id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "conversation_id is required")
		return
	}
	p, ok := principalOrInternal(r.Context(), w)
	if !ok {
		return
	}

	cc, err := s.convDeleter.Context(r.Context(), id)
	if errors.Is(err, memory.ErrNotFound) {
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	}
	if err != nil {
		writeInternal(r.Context(), w, "conversation context", err)
		return
	}
	if err := p.AuthorizeWorkspace(cc.WorkspaceID); err != nil {
		slog.WarnContext(r.Context(), "cross-tenant delete attempt",
			"operation", "delete_conversation", "client_id", p.ClientID)
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	}
	if !canDeleteConversation(p, cc) {
		slog.WarnContext(r.Context(), "identity-scope delete attempt",
			"operation", "delete_conversation", "client_id", p.ClientID)
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	n, err := s.convDeleter.SoftDelete(r.Context(), id, p.WorkspaceID)
	if err != nil {
		writeInternal(r.Context(), w, "delete conversation", err)
		return
	}
	if n == 0 {
		// N'arrive normalement pas: Context vient de confirmer que la
		// conversation existe et appartient à ce workspace. Ne peut se
		// produire que si une suppression concurrente l'a fait disparaître
		// entre les deux appels: 404, comme si elle n'avait jamais existé,
		// plutôt qu'une erreur serveur pour une simple course inoffensive.
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conversation_id": id, "status": "deleted",
	})
}
