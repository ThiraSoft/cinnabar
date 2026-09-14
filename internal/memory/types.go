// Package memory porte le domaine du service de mémoire. Il ne dépend ni de
// SQL ni de HTTP, seulement des interfaces qu'il déclare lui-même (voir
// ports.go). Cette absence de dépendance est structurelle et doit le
// rester: c'est elle qui laisse le domaine se tester sans base et sans
// serveur.
package memory

import (
	"time"

	"github.com/google/uuid"
)

// Principal est l'appelant résolu depuis son token.
type Principal struct {
	ClientID          uuid.UUID
	Label             string
	WorkspaceID       string
	AllowedIdentities []string
}

// Message est un message tel que stocké, source de vérité du service.
type Message struct {
	MessageID      uuid.UUID
	ConversationID string
	WorkspaceID    string
	SequenceNumber int64
	AuthorKey      string
	Role           string
	Content        string
	CreatedAt      time.Time
	EditedAt       *time.Time
	DeletedAt      *time.Time
}

// Unit est une unité d'indexation vectorielle: un message contextualisé.
type Unit struct {
	MemoryUnitID    uuid.UUID
	WorkspaceID     string
	ConversationID  string
	AnchorMessageID uuid.UUID
	StartSequence   int64
	EndSequence     int64
	EmbeddingText   string
	EmbeddingModel  string
	Strategy        string
	Version         int
	Scope           string
}

// AccessReasonExplicitACL est la raison d'accès rendue par le SQL (voir
// accessReasonExpr) quand une ligne n'a été admise ni par le scope
// 'workspace' ni par la participation, mais par une ligne memory_unit_acl
// sur une unité précise. C'est la seule raison d'accès qui ne donne aucun
// droit sur le reste de la conversation: l'octroi porte sur une unité, pas
// sur la conversation, donc rien de ce qui déborde de la fenêtre de cette
// unité n'est couvert. Nommée ici plutôt qu'écrite en clair aux trois
// endroits qui la testent (expand, MergeAdjacent, toResults), pour qu'une
// faute de frappe dans l'une des trois ne rouvre pas le débordement en
// silence.
const AccessReasonExplicitACL = "explicit_acl"

// Candidate est un résultat brut d'une stratégie, avant fusion.
type Candidate struct {
	Strategy string
	Rank     int
	// RawScore n'a de sens que dans l'échelle de sa propre stratégie (un
	// ts_rank_cd pour lexical, un 1 - cosinus pour dense): la fusion
	// multi-stratégies doit comparer des Rank, jamais des RawScore entre
	// stratégies différentes.
	RawScore        float64
	MemoryUnitID    *uuid.UUID
	AnchorMessageID uuid.UUID
	ConversationID  string
	StartSequence   int64
	EndSequence     int64
	AccessReason    string
	MatchedEntities []string
}

// Fused est un candidat après fusion des stratégies, identifié par son
// message d'ancrage.
type Fused struct {
	AnchorMessageID uuid.UUID
	ConversationID  string
	StartSequence   int64
	EndSequence     int64
	Score           float64
	Ranks           map[string]int
	RawScores       map[string]float64
	AccessReason    string
	MemoryUnitID    *uuid.UUID
	MatchedEntities []string
}

// Excerpt est un Fused dont la fenêtre de messages a été rechargée.
type Excerpt struct {
	Fused
	Messages []Message
	CoreFrom int64
	CoreTo   int64
}

// RecentMessage est un tour de dialogue récent, fourni par l'appelant pour
// enrichir le texte soumis à l'encodeur (section 11 de la spec).
type RecentMessage struct {
	AuthorKey string
	Role      string
	Content   string
}

// SearchRequest porte les paramètres d'une recherche hybride.
type SearchRequest struct {
	WorkspaceID         string
	RequesterKey        string
	ConversationID      string
	Query               string
	RecentMessages      []RecentMessage
	KnownSubjects       []string
	Strategies          []string
	CandidateLimit      int
	ResultLimit         int
	TokenBudget         int
	ExcludeMessageIDs   []uuid.UUID
	IncludeContextBlock bool
}

// Result est un souvenir rendu à l'appelant. SourceMessageIDs n'est jamais
// vide et SourceType vaut toujours "original_messages": le service retourne
// des messages réels, jamais un résumé (critère d'acceptation 7).
type Result struct {
	MemoryID         string
	ConversationID   string
	AnchorMessageID  uuid.UUID
	SourceMessageIDs []uuid.UUID
	OccurredAt       time.Time
	Content          string
	Participants     []string
	Scores           map[string]float64
	MatchedEntities  []string
	AccessReason     string
	SourceType       string
}

// SearchDebug porte des compteurs de diagnostic, absents de la réponse par
// défaut (voir Service.DebugSearch). ACLFiltered atteste que le filtre
// d'accès a été appliqué en SQL; il n'existe volontairement aucun compteur
// de rejets ACL, ce qui exigerait de matérialiser des candidats non
// autorisés pour les compter.
type SearchDebug struct {
	DenseCandidates int
	// DenseDroppedByFloor compte les candidats du dense écartés par
	// retrieval.minimum_dense_score. Distinct de DroppedByBudget: celui-ci
	// mesure une éviction faute de place, celui-là un refus de pertinence.
	DenseDroppedByFloor int

	// DenseBest et DenseMedian décrivent la forme de la distribution des
	// similarités denses de cette requête, avant tout filtrage. Elles
	// servent à calibrer un plancher, et surtout à distinguer une requête
	// qui a une réponse d'une requête qui n'en a pas: dans le premier cas
	// le meilleur candidat se détache, dans le second l'embedder rend un
	// voisinage plat où rien ne ressort.
	DenseBest   float64
	DenseMedian float64

	// DenseNoAnswer dit que la détection de question sans réponse a écarté
	// tous les candidats du dense. Distinct de DenseDroppedByFloor, qui
	// écarte candidat par candidat: ici c'est la requête entière qui est
	// jugée sans réponse.
	DenseNoAnswer bool

	// RerankApplied dit que le réordonnanceur a effectivement changé
	// l'ordre. Faux couvre trois cas qu'il faut pouvoir distinguer: pas
	// configuré, en panne, ou rien à réordonner.
	RerankApplied       bool
	LexicalCandidates   int
	GraphCandidates     int
	Fused               int
	AfterMerge          int
	DiscardedAsExcluded int
	DroppedByBudget     int
	ACLFiltered         bool
	StrategyErrors      map[string]string
}

// GraphFact est une relation du graphe rendue à l'appelant. Elle n'entre
// jamais dans la liste des Result, qui ne porte que des messages réels:
// elle est rendue à part, dans son propre bloc du context_block, pour que
// le modèle lecteur ne confonde jamais un fait dérivé avec une citation.
type GraphFact struct {
	RelationID uuid.UUID
	Subject    string
	Predicate  string
	Object     string
	ObservedAt time.Time
	ValidFrom  *time.Time
	ValidUntil *time.Time
	Confidence float64
	// SourceMessageIDs ne contient que les messages sources que le
	// demandeur a le droit de lire: c'est ce qui a fait ressortir le fait.
	SourceMessageIDs []uuid.UUID
}

// SearchResponse est le résultat d'une recherche hybride.
type SearchResponse struct {
	QueryID      string
	Results      []Result
	GraphFacts   []GraphFact
	ContextBlock string
	Debug        *SearchDebug
}
