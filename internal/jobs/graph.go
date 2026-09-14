package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"log/slog"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

// candidateEntityLimit borne le nombre d'entités connues proposées au
// modèle. Assez pour qu'il retrouve une entité déjà vue, assez peu pour que
// le prompt reste court et que la fenêtre de contexte serve au texte.
const candidateEntityLimit = 30

// GraphExtractHandler extrait le graphe d'un message. Il ne rescanne jamais
// la conversation entière (section 7.1 de la spec): le message neuf, ses
// quelques prédécesseurs, les identités de la conversation et les entités
// du workspace qui ressemblent au texte, rien de plus.
//
// Aucun log de ce handler ne porte de contenu de message ni de réponse du
// modèle.
func GraphExtractHandler(cfg *config.Config, msgs memory.MessageRepo,
	repo memory.GraphRepo, ext memory.GraphExtractor, emb memory.Embedder) Handler {

	return func(ctx context.Context, j *postgres.Job) error {
		var p messagePayload
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}

		anchor, err := msgs.ByID(ctx, p.MessageID)
		if err != nil {
			// Un message disparu entre la pose du job et son traitement
			// n'est pas une erreur: rien à extraire, et faire échouer le
			// job le ferait boucler jusqu'à la lettre morte.
			if errors.Is(err, memory.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("load message: %w", err)
		}
		if anchor.DeletedAt != nil {
			return nil
		}

		contextCount := cfg.Graph.ContextMessages
		if contextCount < 0 {
			contextCount = 0
		}
		from := anchor.SequenceNumber - int64(contextCount)
		if from < 0 {
			from = 0
		}
		window, err := msgs.Around(ctx, anchor.ConversationID, from,
			anchor.SequenceNumber)
		if err != nil {
			return fmt.Errorf("load context: %w", err)
		}

		// Le message ancre est retiré du contexte: il est déjà transmis à
		// part, et le laisser ici le ferait compter deux fois dans le
		// prompt. Les messages supprimés sont écartés au même endroit.
		var before []memory.Message
		for _, m := range window {
			if m.MessageID == anchor.MessageID || m.DeletedAt != nil {
				continue
			}
			before = append(before, m)
		}

		participants, err := msgs.Participants(ctx, anchor.ConversationID)
		if err != nil {
			return fmt.Errorf("load participants: %w", err)
		}
		scope, err := msgs.ConversationScope(ctx, anchor.ConversationID)
		if err != nil {
			if errors.Is(err, memory.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("load scope: %w", err)
		}

		// Les candidates alimentent la résolution d'entités. Une panne de
		// cette lecture n'est pas fatale au sens métier, mais elle
		// signale une base en difficulté: on la propage plutôt que
		// d'extraire sans mémoire des entités connues, ce qui créerait des
		// doublons d'entités que le service ne fusionnera jamais.
		candidates, err := repo.CandidateEntities(ctx, anchor.WorkspaceID,
			anchor.Content, candidateEntityLimit)
		if err != nil {
			return fmt.Errorf("load candidate entities: %w", err)
		}

		out, err := ext.Extract(ctx, memory.GraphExtractorInput{
			WorkspaceID:       anchor.WorkspaceID,
			ConversationID:    anchor.ConversationID,
			Message:           anchor,
			Context:           before,
			Participants:      participants,
			CandidateEntities: candidates,
		})
		if err != nil {
			// L'échec repart en backoff par le circuit générique du
			// runner. Le message reste consultable par le dense et le
			// lexical entre-temps.
			return fmt.Errorf("extract graph: %w", err)
		}

		// Le scope des relations est celui de la conversation au moment de
		// l'extraction, pas le scope par défaut de la configuration:
		// l'extracteur n'a aucun moyen de le connaître.
		for i := range out.Relations {
			out.Relations[i].Scope = scope
		}

		// Le vecteur du fait est calculé ici, entre l'extraction et
		// l'écriture, parce que c'est le seul endroit qui connaît à la fois
		// les relations et l'embedder. Il donne à la recherche par graphe le
		// signal de pertinence qui lui manquait: sans lui, elle rend le
		// voisinage de la graine dans un ordre qui ignore la question.
		//
		// Une panne de l'embedder n'empêche pas l'écriture: les relations
		// partent sans vecteur, restent trouvables, et le rejeu du job les
		// complétera puisque l'upsert ne remplit l'embedding que s'il
		// manque. C'est la même posture que pour l'indexation des messages,
		// où une panne dégrade la recherche sans rendre le service
		// indisponible.
		if emb != nil && len(out.Relations) > 0 {
			textes := make([]string, 0, len(out.Relations))
			for _, r := range out.Relations {
				objet := r.TargetLiteral
				if r.TargetEntityID != nil {
					objet = displayNameOf(out.Entities, *r.TargetEntityID)
				}
				textes = append(textes, memory.FactText(
					displayNameOf(out.Entities, r.SourceEntityID),
					r.RelationType, objet))
			}
			vecteurs, err := emb.Embed(ctx, textes)
			if err != nil {
				slog.WarnContext(ctx, "graph fact embedding failed",
					"job_type", j.JobType, "relations", len(out.Relations),
					"error", err)
			} else if len(vecteurs) == len(out.Relations) {
				for i := range out.Relations {
					out.Relations[i].Embedding = vecteurs[i]
				}
			}
		}

		if err := repo.Apply(ctx, out, cfg.Graph.SingleValuedRelations); err != nil {
			return fmt.Errorf("apply graph: %w", err)
		}
		return nil
	}
}

// GraphReevalHandler traite la disparition ou l'édition d'un message. Toute
// la logique est dans le repo, parce qu'elle est transactionnelle: le
// handler ne fait que décoder le payload et transmettre la même liste
// single_valued_relations que GraphExtractHandler passe à Apply. Une liste
// différente entre les deux figerait les fenêtres de validité sur des dates
// qui ne veulent plus rien dire, sans que rien ne le signale.
func GraphReevalHandler(cfg *config.Config, repo memory.GraphRepo) Handler {
	return func(ctx context.Context, j *postgres.Job) error {
		var p messagePayload
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}
		if err := repo.Reevaluate(ctx, p.MessageID,
			cfg.Graph.SingleValuedRelations); err != nil {
			return fmt.Errorf("reevaluate graph: %w", err)
		}
		return nil
	}
}

// displayNameOf rend le nom d'affichage d'une entité de l'extraction, ou une
// chaîne vide si elle n'y figure pas. Le texte embeddé doit être celui que
// l'appelant lira dans le context_block, donc les noms et non les
// identifiants.
func displayNameOf(ents []memory.GraphEntity, id uuid.UUID) string {
	for _, e := range ents {
		if e.EntityID == id {
			return e.DisplayName
		}
	}
	return ""
}
