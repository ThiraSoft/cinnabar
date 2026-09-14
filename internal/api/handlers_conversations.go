package api

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
)

// handleListConversations rend les conversations lisibles du demandeur dont
// l'identifiant commence par prefix, dans l'ordre des identifiants. Un client
// qui range ses conversations sous des identifiants structurés
// ("nine|player:ricardo") y retrouve celles d'un même propriétaire.
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	workspaceID, requester := q.Get("workspace_id"), q.Get("requester_key")
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	if requester == "" {
		writeError(w, http.StatusBadRequest, "requester_key is required")
		return
	}

	// Même ordre que handleListMessages: l'autorisation passe avant la
	// validation du reste, pour qu'un 400 ne masque jamais un 403.
	p, ok := principalOrInternal(r.Context(), w)
	if !ok {
		return
	}
	if err := p.Authorize(workspaceID, requester); err != nil {
		if workspaceID != p.WorkspaceID {
			slog.WarnContext(r.Context(), "cross-tenant list attempt",
				"operation", "list_conversations", "client_id", p.ClientID)
		}
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if s.convLister == nil {
		writeError(w, http.StatusNotImplemented, "conversation listing is not configured")
		return
	}

	limit := defaultListLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("limit must be between 1 and %d", maxListLimit))
			return
		}
		limit = n
	}
	after := ""
	if c := q.Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil || len(raw) == 0 {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		after = string(raw)
	}

	// Un de plus que la page, pour savoir s'il en reste une autre.
	convs, err := s.convLister.List(r.Context(), workspaceID, requester,
		q.Get("prefix"), after, limit+1)
	if err != nil {
		writeInternal(r.Context(), w, "list_conversations", err)
		return
	}
	out := listConversationsResponse{Conversations: make([]conversationDTO, 0, min(len(convs), limit))}
	if len(convs) > limit {
		convs = convs[:limit]
		out.NextCursor = base64.RawURLEncoding.EncodeToString(
			[]byte(convs[len(convs)-1].ConversationID))
	}
	for _, c := range convs {
		out.Conversations = append(out.Conversations, conversationDTO{
			ConversationID: c.ConversationID, Scope: c.Scope, UpdatedAt: c.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
