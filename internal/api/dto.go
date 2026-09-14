package api

import (
	"encoding/json"
	"time"
)

type postMessageRequest struct {
	WorkspaceID    string          `json:"workspace_id"`
	ConversationID string          `json:"conversation_id"`
	AuthorKey      string          `json:"author_key"`
	Role           string          `json:"role"`
	Content        string          `json:"content"`
	CreatedAt      *time.Time      `json:"created_at"`
	Metadata       json.RawMessage `json:"metadata"`
	Consistency    string          `json:"consistency"`
}

type postMessageResponse struct {
	MessageID      string   `json:"message_id"`
	ConversationID string   `json:"conversation_id"`
	SequenceNumber int64    `json:"sequence_number"`
	Status         string   `json:"status"`
	Replayed       bool     `json:"replayed"`
	MemoryUnitIDs  []string `json:"memory_unit_ids"`
	GraphStatus    string   `json:"graph_status"`
	IndexingError  string   `json:"indexing_error,omitempty"`
	// EmbedScheduled distingue une indexation seulement différée d'une
	// indexation perdue: sans lui, un "stored" ne dit pas lequel des deux
	// s'est produit.
	EmbedScheduled bool `json:"embed_scheduled"`
}

type patchMessageRequest struct {
	Content string `json:"content"`
}

type editResponse struct {
	MessageID          string   `json:"message_id"`
	Status             string   `json:"status"`
	DeactivatedAnchors []string `json:"deactivated_anchors"`
	GraphStatus        string   `json:"graph_status"`
}

type postConversationRequest struct {
	ConversationID string   `json:"conversation_id"`
	WorkspaceID    string   `json:"workspace_id"`
	Scope          string   `json:"scope"`
	Participants   []string `json:"participants"`
}

type recentMessageDTO struct {
	AuthorKey string `json:"author_key"`
	Role      string `json:"role"`
	Content   string `json:"content"`
}

type searchRequest struct {
	WorkspaceID         string             `json:"workspace_id"`
	RequesterKey        string             `json:"requester_key"`
	ConversationID      string             `json:"conversation_id"`
	Query               string             `json:"query"`
	RecentMessages      []recentMessageDTO `json:"recent_messages"`
	KnownSubjects       []string           `json:"known_subjects"`
	Strategies          []string           `json:"strategies"`
	CandidateLimit      int                `json:"candidate_limit"`
	ResultLimit         int                `json:"result_limit"`
	TokenBudget         int                `json:"token_budget"`
	ExcludeMessageIDs   []string           `json:"exclude_message_ids"`
	IncludeContextBlock bool               `json:"include_context_block"`
}

type memoryDTO struct {
	MemoryID         string             `json:"memory_id"`
	ConversationID   string             `json:"conversation_id"`
	AnchorMessageID  string             `json:"anchor_message_id"`
	SourceMessageIDs []string           `json:"source_message_ids"`
	OccurredAt       time.Time          `json:"occurred_at"`
	Content          string             `json:"content"`
	Participants     []string           `json:"participants"`
	Scores           map[string]float64 `json:"scores"`
	MatchedEntities  []string           `json:"matched_entities,omitempty"`
	AccessReason     string             `json:"access_reason"`
	SourceType       string             `json:"source_type"`
}

// graphFactDTO expose une relation du graphe telle que rendue par la
// recherche. Elle ne porte jamais le même sens qu'un memoryDTO: un fait est
// une dérivation, jamais un message réel, d'où sa clé JSON distincte
// (graph_facts) dans searchResponse.
type graphFactDTO struct {
	Subject          string   `json:"subject"`
	Predicate        string   `json:"predicate"`
	Object           string   `json:"object"`
	ObservedAt       string   `json:"observed_at"`
	ValidFrom        *string  `json:"valid_from,omitempty"`
	ValidUntil       *string  `json:"valid_until,omitempty"`
	Confidence       float64  `json:"confidence"`
	SourceMessageIDs []string `json:"source_message_ids"`
}

type searchDebugDTO struct {
	DenseCandidates     int               `json:"dense_candidates"`
	DenseDroppedByFloor int               `json:"dense_dropped_by_floor"`
	LexicalCandidates   int               `json:"lexical_candidates"`
	GraphCandidates     int               `json:"graph_candidates"`
	Fused               int               `json:"fused"`
	AfterMerge          int               `json:"after_merge"`
	DiscardedAsExcluded int               `json:"discarded_as_excluded"`
	DroppedByBudget     int               `json:"dropped_by_budget"`
	ACLFiltered         bool              `json:"acl_filtered"`
	StrategyErrors      map[string]string `json:"strategy_errors,omitempty"`
}

type searchResponse struct {
	QueryID      string          `json:"query_id"`
	Memories     []memoryDTO     `json:"memories"`
	GraphFacts   []graphFactDTO  `json:"graph_facts,omitempty"`
	ContextBlock string          `json:"context_block,omitempty"`
	Debug        *searchDebugDTO `json:"debug,omitempty"`
}

type grantACLRequest struct {
	PrincipalKey string `json:"principal_key"`
	Permission   string `json:"permission"`
}

type aclEntryDTO struct {
	PrincipalKey string `json:"principal_key"`
	Permission   string `json:"permission"`
}

type aclListResponse struct {
	MemoryUnitID string        `json:"memory_unit_id"`
	Entries      []aclEntryDTO `json:"entries"`
}

type aboutResponse struct {
	Service   string `json:"service"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

type errorResponse struct {
	Error string `json:"error"`
}
