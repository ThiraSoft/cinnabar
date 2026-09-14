package memory

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// GraphEntity est un nœud du graphe. CanonicalKey porte la déduplication:
// deux extractions qui produisent la même clé dans le même workspace
// écrivent la même ligne.
type GraphEntity struct {
	EntityID     uuid.UUID
	WorkspaceID  string
	CanonicalKey string
	EntityType   string
	DisplayName  string
	Aliases      []string
	Resolved     bool
}

// GraphRelation est une arête bi-temporelle. Exactement un de
// TargetEntityID et TargetLiteral est renseigné, contrainte que Validate
// fait respecter avant que Postgres n'ait à la rejeter.
//
// Les deux axes temporels ne disent pas la même chose. ValidFrom et
// ValidUntil portent la validité du fait dans le monde, un ValidUntil nul
// voulant dire "toujours vrai". IngestedAt et InvalidatedAt portent ce que
// le service croit, un InvalidatedAt renseigné voulant dire que toutes les
// sources ont disparu. ObservedAt reste distinct de ValidFrom: un message
// du 9 septembre peut affirmer un fait vrai depuis juin.
type GraphRelation struct {
	RelationID       uuid.UUID
	WorkspaceID      string
	SourceEntityID   uuid.UUID
	RelationType     string
	TargetEntityID   *uuid.UUID
	TargetLiteral    string
	ObservedAt       time.Time
	ValidFrom        *time.Time
	ValidUntil       *time.Time
	InvalidatedAt    *time.Time
	Confidence       float64
	Scope            string
	DedupKey         string
	ConversationID   string
	SourceMessageIDs []uuid.UUID

	// Embedding porte la représentation vectorielle du fait rendu en
	// phrase, calculée à l'écriture. C'est ce qui permet à la recherche par
	// graphe de préférer les faits qui répondent à la question, au lieu de
	// rendre le voisinage de la graine dans un ordre qui ignore la
	// question. Nil est un état normal: la relation reste écrite et
	// trouvable, simplement classée comme avant.
	Embedding []float32
}

// FactText rend le texte d'un fait tel qu'il est embeddé. Une seule
// définition, parce que l'écriture et la lecture doivent embarquer exactement
// la même forme: un décalage entre les deux dégraderait le classement sans
// lever d'erreur.
func FactText(subject, predicate, object string) string {
	return subject + " " + predicate + " " + object
}

// GraphExtraction est la sortie d'une passe d'extraction, prête à écrire.
type GraphExtraction struct {
	WorkspaceID    string
	ConversationID string
	Entities       []GraphEntity
	Relations      []GraphRelation
}

// Validate écarte les relations que la base refuserait ou qui n'ont pas de
// sens, et borne les valeurs continues. Elle ne renvoie pas d'erreur: une
// extraction partiellement exploitable vaut mieux qu'un job qui échoue et
// repart en backoff, puisque le message reste de toute façon consultable
// par le dense et le lexical.
func (e GraphExtraction) Validate() GraphExtraction {
	// Seules les entités du workspace de l'extraction comptent, et elles
	// seules peuvent servir de source ou de cible à une relation. Une
	// entité d'un autre workspace n'a rien à faire ici: elle serait écrite
	// sous son propre workspace pendant que le reste de l'extraction irait
	// dans un autre, et la recherche ne la retrouverait jamais.
	known := make(map[uuid.UUID]bool, len(e.Entities))
	var entities []GraphEntity
	for _, ent := range e.Entities {
		if ent.WorkspaceID != e.WorkspaceID {
			continue
		}
		known[ent.EntityID] = true
		entities = append(entities, ent)
	}

	// Les entités sont recopiées plutôt que partagées. Sans ça, la valeur
	// rendue et son entrée partagent le même tableau sous-jacent, et un
	// appelant qui modifie l'une modifie l'autre à distance. Ce projet a
	// déjà livré exactement ce défaut une fois, dans MergeAdjacent, et la
	// copie coûte ici le prix d'une extraction, c'est-à-dire rien.
	out := GraphExtraction{
		WorkspaceID:    e.WorkspaceID,
		ConversationID: e.ConversationID,
		Entities:       entities,
	}
	for _, r := range e.Relations {
		// Le workspace d'une relation doit être celui de l'extraction. Le
		// repo écrit la ligne sous le workspace de la relation mais
		// recalcule les chaînes de validité sous celui de l'extraction:
		// deux valeurs différentes écriraient une relation que sa propre
		// maintenance temporelle ne reverrait plus jamais. L'extracteur
		// renseigne les deux depuis la même source, donc ce cas ne vient
		// que d'un bug, et c'est précisément ce que Validate est là pour
		// arrêter avant la base.
		if r.WorkspaceID != e.WorkspaceID {
			continue
		}
		if !known[r.SourceEntityID] {
			continue
		}
		hasEntity := r.TargetEntityID != nil
		hasLiteral := r.TargetLiteral != ""
		if hasEntity == hasLiteral {
			continue
		}
		if hasEntity && !known[*r.TargetEntityID] {
			continue
		}
		if r.RelationType == "" || len(r.SourceMessageIDs) == 0 {
			continue
		}
		if r.ObservedAt.IsZero() {
			continue
		}
		switch {
		case r.Confidence > 1:
			r.Confidence = 1
		case r.Confidence < 0:
			r.Confidence = 0
		}
		out.Relations = append(out.Relations, r)
	}
	return out
}

// GraphExtractorInput porte ce que l'extracteur reçoit: le message neuf,
// son contexte immédiat, les identités de la conversation et les entités
// déjà connues du workspace qui pourraient correspondre. L'extracteur ne
// rescanne jamais la conversation entière (section 7.1 de la spec).
type GraphExtractorInput struct {
	WorkspaceID       string
	ConversationID    string
	Message           Message
	Context           []Message
	Participants      []string
	CandidateEntities []GraphEntity
}

// GraphExtractor est le port du domaine vers le modèle d'extraction. Une
// implémentation nil est un état normal quand graph.enabled vaut false.
type GraphExtractor interface {
	Extract(ctx context.Context, in GraphExtractorInput) (GraphExtraction, error)
}

// GraphRepo est le port d'écriture et de maintenance du graphe.
type GraphRepo interface {
	// Apply écrit entités, relations et sources dans une seule
	// transaction, puis recalcule les chaînes de validité des couples
	// (source, type) touchés qui figurent dans singleValued.
	Apply(ctx context.Context, e GraphExtraction, singleValued []string) error

	// CandidateEntities rend les entités du workspace dont le nom ou un
	// alias ressemble au texte donné, pour alimenter le prompt.
	CandidateEntities(ctx context.Context, workspaceID, text string, limit int) ([]GraphEntity, error)

	// Reevaluate traite la disparition ou l'édition d'un message: les
	// relations dont toutes les sources ont disparu prennent un
	// invalidated_at, puis les chaînes de validité des couples touchés
	// sont recalculées.
	Reevaluate(ctx context.Context, messageID uuid.UUID, singleValued []string) error
}
