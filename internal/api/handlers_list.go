package api

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// Bornes du listage. Pas de champ de configuration: une page de plus de cinq
// cents messages est un export, pas un listage.
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	var req listMessagesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if req.WorkspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	if req.RequesterKey == "" {
		writeError(w, http.StatusBadRequest, "requester_key is required")
		return
	}

	// Même ordre que handleSearch: l'autorisation passe avant la validation
	// du reste, pour qu'un 400 ne masque jamais un 403.
	p, ok := principalOrInternal(r.Context(), w)
	if !ok {
		return
	}
	if err := p.Authorize(req.WorkspaceID, req.RequesterKey); err != nil {
		if req.WorkspaceID != p.WorkspaceID {
			slog.WarnContext(r.Context(), "cross-tenant list attempt",
				"operation", "list_messages", "client_id", p.ClientID)
		}
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if s.lister == nil {
		writeError(w, http.StatusNotImplemented, "message listing is not configured")
		return
	}

	if msg := restrictionError(req.ConversationIDs, req.MetadataFilter); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	limit := req.Limit
	if limit == 0 {
		limit = defaultListLimit
	}
	if limit < 0 || limit > maxListLimit {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("limit must be between 1 and %d", maxListLimit))
		return
	}

	q := memory.ListQuery{CandidateQuery: memory.CandidateQuery{
		WorkspaceID: req.WorkspaceID, RequesterKey: req.RequesterKey,
		// Un de plus que la page, pour savoir s'il en reste une autre sans
		// second aller-retour.
		Limit:           limit + 1,
		ConversationIDs: req.ConversationIDs,
		MetadataFilter:  req.MetadataFilter,
	}}
	if req.Cursor != "" {
		at, id, err := decodeListCursor(req.Cursor)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		q.BeforeAt, q.BeforeID = &at, id
	}

	msgs, err := s.lister.ListMessages(r.Context(), q)
	if err != nil {
		writeInternal(r.Context(), w, "list_messages", err)
		return
	}

	out := listMessagesResponse{Messages: make([]listedMessageDTO, 0, min(len(msgs), limit))}
	if len(msgs) > limit {
		msgs = msgs[:limit]
		last := msgs[len(msgs)-1]
		out.NextCursor = encodeListCursor(last.CreatedAt, last.MessageID)
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, listedMessageDTO{
			MessageID:      m.MessageID.String(),
			ConversationID: m.ConversationID,
			SequenceNumber: m.SequenceNumber,
			AuthorKey:      m.AuthorKey,
			Role:           m.Role,
			Content:        m.Content,
			CreatedAt:      m.CreatedAt,
			Metadata:       m.Metadata,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// Le curseur est opaque pour le client: la date et l'identifiant du dernier
// message rendu. RFC3339Nano garde la microseconde de Postgres, sans quoi la
// comparaison du couple sauterait ou répéterait un message.
func encodeListCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeListCursor(c string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.UUID{}, err
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("cursor without separator")
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, uuid.UUID{}, err
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return time.Time{}, uuid.UUID{}, err
	}
	return t, u, nil
}
