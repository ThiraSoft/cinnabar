package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

var validRoles = map[string]bool{
	"user": true, "assistant": true, "system": true, "tool": true,
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	var req postMessageRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	// Les champs dont Authorize a besoin sont vérifiés présents avant de
	// l'appeler: un serveur peut y trouver une identité vide de deux façons
	// bien différentes — un client qui a un bug de sérialisation et a
	// simplement oublié le champ, ou un client qui vise délibérément un
	// workspace ou une identité qui n'est pas la sienne. Les confondre sous
	// un même 403 dirait au premier qu'il n'a pas le droit de faire quelque
	// chose qu'il aurait pu faire, pour une faute qui n'a rien à voir avec
	// l'autorisation. DisallowUnknownFields a déjà fermé, plus haut, le
	// canal qu'une réponse 400 différenciée aurait pu ouvrir à un
	// prober: distinguer 400 (champ absent) de 403 (présent mais refusé) ne
	// lui apprend donc rien qu'il ne savait déjà avoir omis.
	if req.WorkspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	if req.AuthorKey == "" {
		writeError(w, http.StatusBadRequest, "author_key is required")
		return
	}

	// L'autorisation passe ensuite, avant toute autre validation du
	// payload: un appelant qui n'a pas le droit d'écrire dans ce workspace
	// ou sous cette identité ne doit pas apprendre, via un 400 différent,
	// quoi que ce soit sur la forme attendue du reste du corps.
	p, ok := principalOrInternal(r.Context(), w)
	if !ok {
		return
	}
	if err := p.Authorize(req.WorkspaceID, req.AuthorKey); err != nil {
		if req.WorkspaceID != p.WorkspaceID {
			slog.WarnContext(r.Context(), "cross-tenant write attempt",
				"operation", "post_message", "client_id", p.ClientID)
		}
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	switch {
	case req.ConversationID == "":
		writeError(w, http.StatusBadRequest, "conversation_id is required")
		return
	case strings.TrimSpace(req.Content) == "":
		writeError(w, http.StatusBadRequest, "content is required")
		return
	case !validRoles[req.Role]:
		writeError(w, http.StatusBadRequest, "role must be user, assistant, system or tool")
		return
	}
	switch req.Consistency {
	case "", "eventual", "searchable":
	default:
		writeError(w, http.StatusBadRequest, "consistency must be eventual or searchable")
		return
	}

	in := memory.AppendInput{
		WorkspaceID:    req.WorkspaceID,
		ConversationID: req.ConversationID,
		DefaultScope:   s.cfg.Access.DefaultScope,
		AuthorKey:      req.AuthorKey,
		Role:           req.Role,
		Content:        req.Content,
		Metadata:       req.Metadata,
		RequestID:      strings.TrimSpace(r.Header.Get("Idempotency-Key")),
	}
	if req.CreatedAt != nil {
		in.CreatedAt = *req.CreatedAt
	}

	res, err := s.ingester.Ingest(r.Context(), in, req.Consistency)
	if err != nil {
		// conversation_id est un TEXT libre, indexé seul: une conversation
		// déjà créée sous un autre workspace ne peut être détectée qu'ici,
		// une fois en base. C'est la faute de l'appelant (une tentative
		// cross-tenant ou une collision d'identifiant), jamais une panne du
		// service: pas de 500.
		//
		// 400, et pas le 403 que cette route rendait: partout ailleurs, une
		// ressource existante dans un autre workspace rend 404 pour ne rien
		// confirmer, et un 403 ici ne pourrait vouloir dire qu'une chose,
		// "ce conversation_id existe chez quelqu'un d'autre", sur un espace
		// d'identifiants que le client choisit lui-même. Un prober maîtrise
		// son identité et son workspace: la seule information qu'il tirait
		// de ce code, c'était l'existence d'une conversation ailleurs. Sur
		// une route qui crée la ressource, il n'y a rien à confirmer ni à
		// nier: l'identifiant est inutilisable pour cet appelant, point. Le
		// message ne distingue donc pas les deux cas, et ne reprend aucun
		// détail de MessageRepo.Append.
		if errors.Is(err, memory.ErrWorkspaceMismatch) {
			slog.WarnContext(r.Context(), "cross-tenant write attempt",
				"operation", "post_message", "client_id", p.ClientID)
			writeError(w, http.StatusBadRequest,
				"conversation_id is not usable by this caller")
			return
		}
		writeInternal(r.Context(), w, "ingest", err)
		return
	}

	out := postMessageResponse{
		MessageID:      res.Message.MessageID.String(),
		ConversationID: res.Message.ConversationID,
		SequenceNumber: res.Message.SequenceNumber,
		Status:         res.Status,
		Replayed:       res.Replayed,
		GraphStatus:    res.GraphStatus,
		IndexingError:  res.IndexingError,
		EmbedScheduled: res.EmbedScheduled,
		MemoryUnitIDs:  []string{},
	}
	for _, id := range res.UnitIDs {
		out.MemoryUnitIDs = append(out.MemoryUnitIDs, id.String())
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePostConversation(w http.ResponseWriter, r *http.Request) {
	var req postConversationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	if req.WorkspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}

	// Comme pour handlePostMessage, l'appartenance au workspace et le
	// périmètre d'identités du token sont vérifiés avant toute validation
	// de forme (conversation_id, scope), pour ne jamais masquer un refus
	// d'autorisation derrière un 400. AuthorizeWorkspace applique exactement
	// la même règle qu'Authorize (y compris son refus d'un workspace_id
	// vide), plutôt qu'une comparaison réécrite à la main sur ce seul
	// chemin.
	p, ok := principalOrInternal(r.Context(), w)
	if !ok {
		return
	}
	if err := p.AuthorizeWorkspace(req.WorkspaceID); err != nil {
		if req.WorkspaceID != p.WorkspaceID {
			slog.WarnContext(r.Context(), "cross-tenant conversation declare attempt",
				"operation", "post_conversation", "client_id", p.ClientID)
		}
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	// Chaque participant déclaré doit rester dans le périmètre du token.
	for _, part := range req.Participants {
		if !memory.MatchIdentity(p.AllowedIdentities, part) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
	}

	if req.ConversationID == "" {
		writeError(w, http.StatusBadRequest, "conversation_id is required")
		return
	}
	if req.Scope == "" {
		req.Scope = s.cfg.Access.DefaultScope
	}
	if !config.ValidScopes[req.Scope] {
		writeError(w, http.StatusBadRequest, "unknown scope")
		return
	}

	// Une conversation qui existe déjà est une ressource dont l'appelant
	// n'est pas forcément le propriétaire: Declare y écrase le scope
	// (ON CONFLICT DO UPDATE) et y ajoute des participants. On la charge
	// donc avant, exactement comme handleDeleteConversation le fait avant
	// toute suppression, et on lui applique la même règle d'accès.
	//
	// Trois cas. Elle n'existe pas: création libre, les vérifications
	// ci-dessus suffisent. Elle existe dans un autre workspace: 404 comme
	// partout ailleurs pour une ressource étrangère (conversation_id est un
	// TEXT libre choisi par le client, donc devinable depuis n'importe quel
	// workspace: un 403 confirmerait son existence). Elle existe dans ce
	// workspace: il faut la même force d'autorisation que pour la
	// supprimer, puisque basculer une conversation 'private' en 'workspace'
	// ouvre tous ses souvenirs à tout le workspace, et l'inverse les cache
	// à ceux qui les lisaient légitimement.
	//
	// La séquence d'amorçage documentée dans le README (déclarer en
	// 'participants', partager, puis figer en 'explicit') continue de
	// passer: le déclarant y est participant, donc canDeleteConversation
	// l'admet.
	cc, err := s.convs.Context(r.Context(), req.ConversationID)
	switch {
	case errors.Is(err, memory.ErrNotFound):
		// Création: rien à autoriser de plus.
	case err != nil:
		writeInternal(r.Context(), w, "conversation context", err)
		return
	default:
		if err := p.AuthorizeWorkspace(cc.WorkspaceID); err != nil {
			slog.WarnContext(r.Context(), "cross-tenant declare attempt",
				"operation", "post_conversation", "client_id", p.ClientID)
			writeError(w, http.StatusNotFound, "conversation not found")
			return
		}
		if !canDeleteConversation(p, cc) {
			slog.WarnContext(r.Context(), "identity-scope declare attempt",
				"operation", "post_conversation", "client_id", p.ClientID)
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
	}

	if err := s.convs.Declare(r.Context(), req.ConversationID, req.WorkspaceID,
		req.Scope, req.Participants); err != nil {
		// Deux situations tombent ici, et le DO UPDATE de Declare les
		// rattrape toutes les deux. Soit la conversation a été créée dans
		// un autre workspace entre le Context ci-dessus et ce Declare.
		// Soit elle est soft-deleted, auquel cas Context l'a déjà déclarée
		// introuvable et le switch est passé par la branche "création":
		// c'est le prédicat deleted_at du DO UPDATE qui empêche alors de
		// ressusciter la ligne. 404 dans les deux cas, comme partout
		// ailleurs pour une ressource qu'on n'a pas à voir.
		if errors.Is(err, memory.ErrWorkspaceMismatch) {
			slog.WarnContext(r.Context(), "cross-tenant declare attempt",
				"operation", "post_conversation", "client_id", p.ClientID)
			writeError(w, http.StatusNotFound, "conversation not found")
			return
		}
		writeInternal(r.Context(), w, "declare conversation", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"conversation_id": req.ConversationID, "scope": req.Scope,
	})
}

// handlePatchMessage édite un message existant.
//
// L'appartenance au workspace et le périmètre d'identité sont vérifiés avant
// toute autre chose, y compris avant de décoder le corps: contrairement à un
// compromis envisagé un temps, un aller-retour de plus vaut mieux qu'une
// écriture dans un workspace ou sous une identité qui n'est pas celle de
// l'appelant. Un message d'un autre workspace rend 404, comme un message
// inexistant, jamais 403: dire "ça existe mais ce n'est pas à vous"
// confirmerait l'existence d'une ressource dans le workspace d'un autre,
// pour un identifiant que l'appelant ne pourrait sinon que deviner. Un
// message d'un auteur que le token ne peut pas usurper, dans le workspace de
// l'appelant, rend en revanche 403: l'appelant sait déjà que la ressource
// existe puisqu'elle est dans son propre workspace, rien ne se voit donc
// disclosé de plus.
func (s *Server) handlePatchMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.messageIDParam(w, r)
	if !ok {
		return
	}
	p, ok := principalOrInternal(r.Context(), w)
	if !ok {
		return
	}

	ws, author, err := s.editor.MessageOwner(r.Context(), id)
	if errors.Is(err, memory.ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if err != nil {
		writeInternal(r.Context(), w, "message owner", err)
		return
	}
	if err := p.AuthorizeWorkspace(ws); err != nil {
		slog.WarnContext(r.Context(), "cross-tenant edit attempt",
			"operation", "patch_message", "client_id", p.ClientID)
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if !memory.MatchIdentity(p.AllowedIdentities, author) {
		slog.WarnContext(r.Context(), "identity-scope edit attempt",
			"operation", "patch_message", "client_id", p.ClientID)
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req patchMessageRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}

	res, err := s.editor.EditMessage(r.Context(), id, req.Content)
	if errors.Is(err, memory.ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if err != nil {
		writeInternal(r.Context(), w, "edit message", err)
		return
	}
	writeJSON(w, http.StatusOK, toEditResponse("reindexing", res))
}

// handleDeleteMessage supprime (soft delete) un message existant. Même ordre
// de vérification que handlePatchMessage: voir son commentaire.
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.messageIDParam(w, r)
	if !ok {
		return
	}
	p, ok := principalOrInternal(r.Context(), w)
	if !ok {
		return
	}

	ws, author, err := s.editor.MessageOwner(r.Context(), id)
	if errors.Is(err, memory.ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if err != nil {
		writeInternal(r.Context(), w, "message owner", err)
		return
	}
	if err := p.AuthorizeWorkspace(ws); err != nil {
		slog.WarnContext(r.Context(), "cross-tenant delete attempt",
			"operation", "delete_message", "client_id", p.ClientID)
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if !memory.MatchIdentity(p.AllowedIdentities, author) {
		slog.WarnContext(r.Context(), "identity-scope delete attempt",
			"operation", "delete_message", "client_id", p.ClientID)
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	res, err := s.editor.DeleteMessage(r.Context(), id)
	if errors.Is(err, memory.ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if err != nil {
		writeInternal(r.Context(), w, "delete message", err)
		return
	}
	writeJSON(w, http.StatusOK, toEditResponse("deleted", res))
}

// messageIDParam décode le paramètre de chemin message_id en uuid.UUID,
// partagé par les deux handlers ci-dessus.
func (s *Server) messageIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("message_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "message_id must be a uuid")
		return uuid.Nil, false
	}
	return id, true
}

func toEditResponse(status string, res memory.EditResult) editResponse {
	out := editResponse{
		MessageID: res.Message.MessageID.String(), Status: status,
		GraphStatus: res.GraphStatus, DeactivatedAnchors: []string{},
	}
	for _, a := range res.DeactivatedAnchors {
		out.DeactivatedAnchors = append(out.DeactivatedAnchors, a.String())
	}
	return out
}
