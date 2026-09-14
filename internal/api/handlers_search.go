package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// Valeurs de repli quand la configuration ne fournit aucun plafond positif
// pour ces trois champs. Même logique que defaultMaxRequestBytes: un zéro de
// configuration ne doit jamais se traduire par une absence de plafond.
const (
	defaultMaxCandidateLimit = 500
	defaultMaxResultLimit    = 50
	defaultMaxTokenBudget    = 20000
)

// maxKnownSubjects borne le nombre de sujets connus qu'un appelant peut
// fournir. Contrairement aux trois plafonds ci-dessus, ce n'est pas un
// levier de coût réglable mais une borne de précision, donc pas de champ de
// configuration: la liste part en paramètre d'un prédicat que le
// planificateur résout en filtre par ligne sur les entités du workspace, et
// une liste arbitrairement longue transforme la détection de graines en
// parcours coûteux dont le seul effet utile est de semer tout le graphe.
//
// Trente-deux est large: la section 8.2 de la spec voit les sujets connus
// comme les quelques personnes ou objets qu'une question mentionne, plus les
// participants de la conversation courante.
const maxKnownSubjects = 32

// restrictionError rend le message d'un 400 pour des restrictions de
// recherche ou de listage mal formées, vide si elles sont valides. Partagée
// par les deux routes pour qu'elles refusent exactement la même chose.
func restrictionError(convs []string, filter *memory.MetadataFilter) string {
	for _, c := range convs {
		if c == "" {
			return "conversation_ids must not contain empty values"
		}
	}
	if err := filter.Validate(); err != nil {
		return err.Error()
	}
	return ""
}

func capOrDefault(configured, fallback int) int {
	if configured > 0 {
		return configured
	}
	return fallback
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	// Comme pour handlePostMessage: les champs dont Authorize a besoin sont
	// vérifiés présents avant de l'appeler, pour distinguer un champ omis
	// (400) d'un champ présent mais refusé (403).
	if req.WorkspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	if req.RequesterKey == "" {
		writeError(w, http.StatusBadRequest, "requester_key is required")
		return
	}

	// L'autorisation passe avant la validation du reste de la requête
	// (query non vide, plafonds, exclude_message_ids bien formés): un
	// requester_key hors du périmètre du token, ou un workspace_id
	// différent du sien, ne doit jamais être masqué par un 400 sur un autre
	// champ.
	p, ok := principalOrInternal(r.Context(), w)
	if !ok {
		return
	}
	if err := p.Authorize(req.WorkspaceID, req.RequesterKey); err != nil {
		if req.WorkspaceID != p.WorkspaceID {
			slog.WarnContext(r.Context(), "cross-tenant search attempt",
				"operation", "search", "client_id", p.ClientID)
		}
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}

	// candidate_limit, result_limit et token_budget sont des leviers de coût
	// fournis par l'appelant: plafonnés ici, à la frontière, plutôt que
	// transmis tels quels au planificateur de requêtes, sous peine qu'un
	// tenant en dégrade un autre sur un service partagé.
	if maxCandidate := capOrDefault(s.cfg.Service.MaxCandidateLimit, defaultMaxCandidateLimit); req.CandidateLimit > maxCandidate {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("candidate_limit must not exceed %d", maxCandidate))
		return
	}
	if maxResult := capOrDefault(s.cfg.Service.MaxResultLimit, defaultMaxResultLimit); req.ResultLimit > maxResult {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("result_limit must not exceed %d", maxResult))
		return
	}
	if maxBudget := capOrDefault(s.cfg.Service.MaxTokenBudget, defaultMaxTokenBudget); req.TokenBudget > maxBudget {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("token_budget must not exceed %d", maxBudget))
		return
	}

	if len(req.KnownSubjects) > maxKnownSubjects {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("known_subjects must not exceed %d entries", maxKnownSubjects))
		return
	}
	if msg := restrictionError(req.ConversationIDs, req.MetadataFilter); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	excluded := make([]uuid.UUID, 0, len(req.ExcludeMessageIDs))
	for _, raw := range req.ExcludeMessageIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "exclude_message_ids must be uuids")
			return
		}
		excluded = append(excluded, id)
	}

	recent := make([]memory.RecentMessage, 0, len(req.RecentMessages))
	for _, m := range req.RecentMessages {
		recent = append(recent, memory.RecentMessage{
			AuthorKey: m.AuthorKey, Role: m.Role, Content: m.Content,
		})
	}

	resp, err := s.finder.Search(r.Context(), memory.SearchRequest{
		WorkspaceID:         req.WorkspaceID,
		RequesterKey:        req.RequesterKey,
		ConversationID:      req.ConversationID,
		Query:               req.Query,
		RecentMessages:      recent,
		KnownSubjects:       req.KnownSubjects,
		Strategies:          req.Strategies,
		CandidateLimit:      req.CandidateLimit,
		ResultLimit:         req.ResultLimit,
		TokenBudget:         req.TokenBudget,
		ExcludeMessageIDs:   excluded,
		IncludeContextBlock: req.IncludeContextBlock,
		ConversationIDs:     req.ConversationIDs,
		MetadataFilter:      req.MetadataFilter,
	})
	if err != nil {
		// Un nom de stratégie inconnu est une faute de l'appelant (une faute
		// de frappe, typiquement), pas une panne: 400, jamais le 500 qui
		// réveillerait l'astreinte. Le message nomme les valeurs acceptées
		// plutôt que de reprendre celle qui a été refusée.
		if errors.Is(err, memory.ErrUnknownStrategy) {
			writeError(w, http.StatusBadRequest,
				"strategies must be dense, lexical or graph")
			return
		}
		writeInternal(r.Context(), w, "search", err)
		return
	}

	out := searchResponse{
		QueryID:      resp.QueryID,
		Memories:     make([]memoryDTO, 0, len(resp.Results)),
		ContextBlock: resp.ContextBlock,
	}
	for _, m := range resp.Results {
		dto := memoryDTO{
			MemoryID:        m.MemoryID,
			ConversationID:  m.ConversationID,
			AnchorMessageID: m.AnchorMessageID.String(),
			OccurredAt:      m.OccurredAt,
			Content:         m.Content,
			Participants:    m.Participants,
			Scores:          m.Scores,
			MatchedEntities: m.MatchedEntities,
			AccessReason:    m.AccessReason,
			SourceType:      m.SourceType,
			Metadata:        m.Metadata,
		}
		for _, id := range m.SourceMessageIDs {
			dto.SourceMessageIDs = append(dto.SourceMessageIDs, id.String())
		}
		out.Memories = append(out.Memories, dto)
	}
	for _, f := range resp.GraphFacts {
		dto := graphFactDTO{
			Subject:    f.Subject,
			Predicate:  f.Predicate,
			Object:     f.Object,
			ObservedAt: f.ObservedAt.UTC().Format(time.RFC3339),
			Confidence: f.Confidence,
		}
		if f.ValidFrom != nil {
			v := f.ValidFrom.UTC().Format(time.RFC3339)
			dto.ValidFrom = &v
		}
		if f.ValidUntil != nil {
			v := f.ValidUntil.UTC().Format(time.RFC3339)
			dto.ValidUntil = &v
		}
		for _, id := range f.SourceMessageIDs {
			dto.SourceMessageIDs = append(dto.SourceMessageIDs, id.String())
		}
		out.GraphFacts = append(out.GraphFacts, dto)
	}
	if resp.Debug != nil {
		out.Debug = &searchDebugDTO{
			DenseCandidates:     resp.Debug.DenseCandidates,
			DenseDroppedByFloor: resp.Debug.DenseDroppedByFloor,
			LexicalCandidates:   resp.Debug.LexicalCandidates,
			GraphCandidates:     resp.Debug.GraphCandidates,
			Fused:               resp.Debug.Fused,
			AfterMerge:          resp.Debug.AfterMerge,
			DiscardedAsExcluded: resp.Debug.DiscardedAsExcluded,
			DroppedByBudget:     resp.Debug.DroppedByBudget,
			ACLFiltered:         resp.Debug.ACLFiltered,
			StrategyErrors:      resp.Debug.StrategyErrors,
		}
	}
	writeJSON(w, http.StatusOK, out)
}
