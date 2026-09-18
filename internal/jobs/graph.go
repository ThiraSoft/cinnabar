package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

// candidateEntityLimit borne le nombre d'entités connues proposées au
// modèle. Assez pour qu'il retrouve une entité déjà vue, assez peu pour que
// le prompt reste court et que la fenêtre de contexte serve au texte.
const candidateEntityLimit = 30

// GraphExtractHandler extrait le graphe d'une conversation, par fenêtres.
// Le job porte la conversation, pas un message: il soumet au modèle les
// messages pas encore extraits, au plus graph.extraction_batch à la fois,
// avec les quelques messages qui les précèdent, les identités de la
// conversation et les entités du workspace qui ressemblent au texte. Il ne
// rescanne jamais la conversation entière (section 7.1 de la spec).
//
// Rejouer le job est sans effet de bord: la position n'avance qu'après
// l'écriture, et les relations ont des identifiants déterministes, donc une
// fenêtre extraite deux fois écrit les mêmes lignes.
//
// Aucun log de ce handler ne porte de contenu de message ni de réponse du
// modèle.
func GraphExtractHandler(cfg *config.Config, msgs memory.MessageRepo,
	progress memory.GraphProgress, sched memory.GraphScheduler,
	repo memory.GraphRepo, ext memory.GraphExtractor, emb memory.Embedder) Handler {

	return func(ctx context.Context, j *postgres.Job) error {
		conv := j.ConversationID
		batch := cfg.Graph.ExtractionBatch
		if batch < 1 {
			batch = 1
		}

		pending, more, err := progress.GraphPending(ctx, conv, batch)
		if err != nil {
			if errors.Is(err, memory.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("load pending messages: %w", err)
		}
		if len(pending) == 0 {
			// Un doublon laissé par une reprise, ou une fenêtre déjà traitée
			// par un job précédent: rien à faire.
			return nil
		}
		through := pending[len(pending)-1].SequenceNumber

		var window []memory.Message
		for _, m := range pending {
			if m.DeletedAt == nil {
				window = append(window, m)
			}
		}

		// La suite de la conversation part dans un job à elle plutôt que
		// dans une boucle ici: chaque appel au modèle reste borné par le
		// timeout d'un job, et une panne au milieu ne reprend pas depuis le
		// début.
		moveOn := func() error {
			if err := progress.AdvanceGraphProgress(ctx, conv, through); err != nil {
				return fmt.Errorf("advance graph progress: %w", err)
			}
			if more {
				if err := sched.ScheduleGraphExtract(ctx, j.WorkspaceID, conv); err != nil {
					return fmt.Errorf("schedule next window: %w", err)
				}
			}
			return nil
		}

		if len(window) > 0 {
			if err := extractWindow(ctx, cfg, msgs, repo, ext, emb, j, window); err != nil {
				if errors.Is(err, errConversationGone) {
					return nil
				}
				// À la dernière tentative, la fenêtre est sautée avant que le
				// job parte en lettre morte. Sans ça, une fenêtre que le
				// modèle ne sait pas traiter bloquerait pour toujours
				// l'extraction du reste de la conversation, alors qu'avec un
				// job par message elle ne coûtait que ses propres messages.
				// La lettre morte garde la cause.
				if cfg.Jobs.RetryLimit > 0 && j.Attempts+1 >= cfg.Jobs.RetryLimit {
					slog.WarnContext(ctx, "graph window skipped after last attempt",
						"conversation_id", conv, "through", through, "messages", len(window))
					if merr := moveOn(); merr != nil {
						return errors.Join(err, merr)
					}
				}
				return err
			}
		}
		return moveOn()
	}
}

var errConversationGone = errors.New("conversation gone")

// candidateTextLimit borne le texte comparé aux noms d'entités connues. La
// ressemblance par trigrammes ne gagne rien à lire une fenêtre entière, et
// la requête coûte à proportion du texte.
const candidateTextLimit = 4000

func extractWindow(ctx context.Context, cfg *config.Config, msgs memory.MessageRepo,
	repo memory.GraphRepo, ext memory.GraphExtractor, emb memory.Embedder,
	j *postgres.Job, window []memory.Message) error {

	first := window[0]
	contextCount := cfg.Graph.ContextMessages
	if contextCount < 0 {
		contextCount = 0
	}
	var before []memory.Message
	if contextCount > 0 && first.SequenceNumber > 1 {
		from := first.SequenceNumber - int64(contextCount)
		if from < 0 {
			from = 0
		}
		around, err := msgs.Around(ctx, first.ConversationID, from, first.SequenceNumber-1)
		if err != nil {
			return fmt.Errorf("load context: %w", err)
		}
		// Refiltré ici plutôt que confié à Around: la garantie vit dans un
		// autre paquet.
		for _, m := range around {
			if m.DeletedAt == nil && m.SequenceNumber < first.SequenceNumber {
				before = append(before, m)
			}
		}
	}

	participants, err := msgs.Participants(ctx, first.ConversationID)
	if err != nil {
		return fmt.Errorf("load participants: %w", err)
	}
	scope, err := msgs.ConversationScope(ctx, first.ConversationID)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			return errConversationGone
		}
		return fmt.Errorf("load scope: %w", err)
	}

	// Les candidates alimentent la résolution d'entités. Une panne de cette
	// lecture n'est pas fatale au sens métier, mais elle signale une base en
	// difficulté: on la propage plutôt que d'extraire sans mémoire des
	// entités connues, ce qui créerait des doublons d'entités que le service
	// ne fusionnera jamais.
	var text strings.Builder
	for _, m := range window {
		text.WriteString(m.Content)
		text.WriteByte('\n')
	}
	candidates, err := repo.CandidateEntities(ctx, first.WorkspaceID,
		memory.TruncateRunes(text.String(), candidateTextLimit), candidateEntityLimit)
	if err != nil {
		return fmt.Errorf("load candidate entities: %w", err)
	}

	out, err := ext.Extract(ctx, memory.GraphExtractorInput{
		WorkspaceID:       first.WorkspaceID,
		ConversationID:    first.ConversationID,
		Messages:          window,
		Context:           before,
		Participants:      participants,
		CandidateEntities: candidates,
	})
	if err != nil {
		// L'échec repart en backoff par le circuit générique du runner. Les
		// messages restent consultables par le dense et le lexical
		// entre-temps, et la position n'a pas bougé.
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
