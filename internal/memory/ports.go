package memory

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// AppendInput décrit un message à enregistrer. RequestID vide veut dire pas
// d'idempotence demandée.
type AppendInput struct {
	WorkspaceID    string
	ConversationID string
	DefaultScope   string
	AuthorKey      string
	Role           string
	Content        string
	CreatedAt      time.Time
	Metadata       json.RawMessage
	RequestID      string
}

// AppendJob décrit un job à poser dans la transaction qui écrit le message,
// et non après elle (section 6.1 de la spec, étape 6). Seul le type voyage
// par ce port: le domaine ne connaît pas encore l'identifiant du message au
// moment où il décrit ces jobs, puisque cet identifiant est attribué dans la
// transaction elle-même, donc c'est au repo de composer le payload
// {"message_id": ...} du message qu'il vient d'écrire. C'est aussi ce qui
// garde le domaine ignorant du SQL: il passe une description, pas une
// requête.
type AppendJob struct {
	Type string
}

// AppendResult porte le message écrit et ce qu'il faut pour construire son
// unité d'indexation sans second aller-retour.
type AppendResult struct {
	Message      Message
	Replayed     bool
	Scope        string
	Participants []string
	Previous     []Message
}

// MessageRepo est le port du domaine vers le stockage des messages. Append
// fait, en une seule transaction, l'auto-création de la conversation,
// l'accumulation du participant, la vérification d'idempotence,
// l'attribution du numéro de séquence, l'insertion, puis la pose des jobs
// décrits par jobs.
//
// Les jobs entrent dans cette même transaction, comme le demande la section
// 6.1 de la spec: les poser après le commit, sur une autre connexion,
// laissait une fenêtre où une panne rendait un message durable que plus rien
// ne viendrait jamais indexer (le reaper ne récupère que les jobs déjà
// arrivés en table, et rien ne balaie les messages sans unité active). Un
// rejeu n'en pose aucun: l'unité existe déjà et son identifiant est
// déterministe.
type MessageRepo interface {
	Append(ctx context.Context, in AppendInput, previousCount int, jobs []AppendJob) (AppendResult, error)
	ByID(ctx context.Context, id uuid.UUID) (Message, error)
	Around(ctx context.Context, conversationID string, from, to int64) ([]Message, error)

	// EditAndDeactivate et SoftDeleteAndDeactivate existent ensemble et pour
	// la même raison: modifier le contenu (ou le supprimer) puis désactiver
	// les unités couvrantes en deux appels séparés laisserait, sur une panne
	// entre les deux, une unité active citer un texte qui ne correspond
	// plus au message. Pour l'édition c'est même pire que pour la
	// suppression: le message affiche déjà le nouveau texte pendant qu'une
	// unité continue d'en citer un autre, une contradiction du dossier
	// plutôt qu'une simple péremption. Ne les séparez pas à nouveau en deux
	// appels pour "simplifier": c'est cette atomicité qui est le correctif.
	// EditMessage et DeleteMessage s'en servent respectivement. Les
	// variantes non transactionnelles qu'elles ont remplacées (Edit,
	// SoftDelete, UnitRepo.DeactivateCovering) ont été retirées du port
	// plutôt que laissées là: sans appelant, elles n'étaient plus qu'une
	// invitation à refaire l'enchaînement en deux temps, et une méthode de
	// plus à écrire dans chaque double de test des trois paquets qui en
	// déclarent.

	EditAndDeactivate(ctx context.Context, id uuid.UUID, content string) (Message, []uuid.UUID, error)
	SoftDeleteAndDeactivate(ctx context.Context, id uuid.UUID) (Message, []uuid.UUID, error)

	Participants(ctx context.Context, conversationID string) ([]string, error)

	// ConversationScope rend le scope déclaré d'une conversation. Le worker
	// d'embedding en a besoin pour construire l'unité avec le même scope que
	// l'indexation en ligne, plutôt qu'avec le scope par défaut de la
	// configuration.
	ConversationScope(ctx context.Context, conversationID string) (string, error)

	// MessageOwner rend le workspace et l'auteur d'un message, sans charger
	// son contenu. La couche HTTP s'en sert pour vérifier l'appartenance au
	// workspace ET le périmètre d'identité avant toute édition ou
	// suppression, plutôt qu'après coup: modifier le message d'un voisin, ou
	// le message d'un auteur que le token ne peut pas usurper, laisserait une
	// écriture non autorisée si la vérification ne venait qu'ensuite.
	MessageOwner(ctx context.Context, messageID uuid.UUID) (workspaceID, authorKey string, err error)
}

// Embedder distingue documents et requêtes, parce que les modèles Nomic
// attendent deux préfixes différents et qu'une confusion dégraderait le rappel
// sans lever d'erreur.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	Model() string
}

// UnitRepo est le port du domaine vers le stockage des unités
// d'indexation vectorielle. La désactivation des unités couvrantes n'y
// figure pas: elle se fait dans la transaction de l'édition ou de la
// suppression (MessageRepo.EditAndDeactivate et SoftDeleteAndDeactivate),
// jamais par un appel séparé.
type UnitRepo interface {
	Upsert(ctx context.Context, u Unit, embedding []float32) error
}

// JobQueue est le port du domaine vers la file de jobs asynchrones (embed,
// graph_extract, ...).
type JobQueue interface {
	Enqueue(ctx context.Context, jobType, workspaceID, conversationID string, payload any) error
}

// CandidateQuery porte les paramètres communs à toutes les stratégies de
// recherche: le workspace du demandeur et sa clé d'identité, dont dépend la
// clause d'ACL partagée, plus le nombre de candidats voulu.
type CandidateQuery struct {
	WorkspaceID  string
	RequesterKey string
	Limit        int
}

// DenseQuery est une CandidateQuery accompagnée du vecteur de la requête déjà
// embeddé.
type DenseQuery struct {
	CandidateQuery
	Embedding []float32
}

// DenseSearcher est le port du domaine vers la recherche dense: elle classe
// les unités par proximité vectorielle et applique la même règle d'accès que
// les autres stratégies.
type DenseSearcher interface {
	SearchDense(ctx context.Context, q DenseQuery) ([]Candidate, error)
}

// LexicalQuery est une CandidateQuery accompagnée du texte à chercher en
// plein texte.
type LexicalQuery struct {
	CandidateQuery
	Text string
}

// LexicalSearcher est le port du domaine vers la recherche lexicale: elle
// classe les messages par pertinence plein texte française et applique la
// même règle d'accès que les autres stratégies.
type LexicalSearcher interface {
	SearchLexical(ctx context.Context, q LexicalQuery) ([]Candidate, error)
}

// GraphQuery est une CandidateQuery accompagnée des sujets connus et de la
// profondeur de traversée. La couche graphe appartient à un autre plan: un
// GraphSearcher nil est un état normal, pas une erreur de câblage.
type GraphQuery struct {
	CandidateQuery
	Subjects []string
	// Text est la question de l'appelant, utilisée pour détecter des
	// entités graines par similarité trigramme quand Subjects ne suffit
	// pas. C'est la troisième source de graines de la section 8.2.
	Text    string
	MaxHops int

	// Embedding est le vecteur de la question, le même que celui passé à la
	// stratégie dense. Il donne au classement du graphe un signal de
	// pertinence par rapport à la question, que ni le nombre de sauts ni la
	// confiance ne portent. Nil est un état normal: le classement retombe
	// alors sur l'ordre historique.
	Embedding []float32
}

// GraphResult porte les deux sorties d'une même traversée: les candidats,
// qui entrent dans la fusion RRF au même titre que ceux du dense et du
// lexical, et les faits, qui vont dans le context_block sans passer par la
// fusion. Une seule traversée pour les deux, plutôt qu'un second
// aller-retour SQL sur les mêmes relations.
type GraphResult struct {
	Candidates []Candidate
	Facts      []GraphFact
}

// RerankCandidate est un extrait soumis au réordonnanceur: son texte, et
// rien d'autre. L'identité reste du côté du domaine, le réordonnanceur ne
// rend que des positions.
type RerankCandidate struct {
	Text string
}

// Reranker est le port du domaine vers un modèle qui lit la question et les
// extraits ensemble, et rend les positions des plus pertinents, du meilleur
// au moins bon.
//
// C'est le seul appel de modèle du chemin de recherche, et il est facultatif:
// le critère 9 de la spec interdit d'en dépendre, donc un Reranker nil est un
// état normal et un échec doit dégrader vers l'ordre de la fusion, jamais
// faire échouer la recherche. La mesure qui justifie son existence est dans
// docs/evals/: deux requêtes du corpus ont leur réponse entre le
// rang six et le rang dix, et aucun signal déjà disponible ne les remonte,
// puisque l'embedder préfère réellement les distracteurs.
type Reranker interface {
	Rerank(ctx context.Context, question string, cands []RerankCandidate) ([]int, error)
}

// GraphSearcher est le port du domaine vers la recherche par graphe de
// connaissances. Un GraphSearcher nil reste un état normal: c'est ce que
// voit le service quand graph.enabled vaut false.
type GraphSearcher interface {
	SearchGraph(ctx context.Context, q GraphQuery) (GraphResult, error)
}

// UnitContext porte ce qu'il faut pour décider si un appelant a le droit de
// partager une unité de mémoire. Déclaré ici, dans le domaine, plutôt que
// dans internal/store/postgres: internal/api en a besoin dans la signature
// de son port ACLStore (pour décider avec canShare), et internal/api ne
// doit jamais importer internal/store/postgres dans un fichier non-test
// (voir le commentaire de ErrWorkspaceMismatch pour la même raison
// appliquée à un type d'erreur plutôt qu'à une donnée). La règle elle-même
// est appliquée dans la couche HTTP, qui seule connaît les motifs
// d'identité du token.
type UnitContext struct {
	WorkspaceID    string
	ConversationID string
	Scope          string
	Participants   []string
}

// ACLEntry est une ligne de memory_unit_acl telle que rendue par List: un
// principal et la permission qui lui a été accordée sur une unité.
type ACLEntry struct {
	PrincipalKey string
	Permission   string
}

// ConversationContext porte ce qu'il faut pour décider si un appelant a le
// droit d'agir sur une conversation entière (la supprimer, notamment), avec
// la même règle que le partage d'une unité: incarner un participant, ou
// bénéficier d'un scope 'workspace'. Même rôle que UnitContext, mais sans
// unité: ici la ressource est la conversation elle-même, pas un souvenir
// qui en dérive.
type ConversationContext struct {
	WorkspaceID  string
	Scope        string
	Participants []string
}
