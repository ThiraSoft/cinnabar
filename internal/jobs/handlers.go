package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

type messagePayload struct {
	MessageID uuid.UUID `json:"message_id"`
}

// EmbedHandler reconstruit l'unité d'un message et l'indexe. Il est idempotent
// par construction: l'identifiant d'unité est déterministe, donc un rejeu
// réécrit la même ligne.
func EmbedHandler(cfg *config.Config, msgs memory.MessageRepo,
	units memory.UnitRepo, emb memory.Embedder) Handler {

	return func(ctx context.Context, j *postgres.Job) error {
		var p messagePayload
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}

		anchor, err := msgs.ByID(ctx, p.MessageID)
		if err != nil {
			return fmt.Errorf("load message: %w", err)
		}
		// Un message supprimé entre-temps n'a plus à être indexé, et ce n'est
		// pas une erreur.
		if anchor.DeletedAt != nil {
			return nil
		}

		from := anchor.SequenceNumber - int64(cfg.Indexing.PreviousMessages)
		if from < 0 {
			from = 0
		}
		window, err := msgs.Around(ctx, anchor.ConversationID, from,
			anchor.SequenceNumber)
		if err != nil {
			return fmt.Errorf("load context: %w", err)
		}
		previous := make([]memory.Message, 0, len(window))
		for _, m := range window {
			if m.MessageID != anchor.MessageID {
				previous = append(previous, m)
			}
		}

		participants, err := msgs.Participants(ctx, anchor.ConversationID)
		if err != nil {
			return fmt.Errorf("load participants: %w", err)
		}

		// Le scope employé ici doit être celui, réel, de la conversation:
		// utiliser cfg.Access.DefaultScope rendrait la visibilité de l'unité
		// dépendante du mode de cohérence choisi par l'appelant (searchable
		// écrit le scope réel via l'indexation en ligne, eventual écrirait
		// alors le défaut de configuration), ce qui est un bug d'ACL et pas
		// une simple incohérence cosmétique.
		scope, err := msgs.ConversationScope(ctx, anchor.ConversationID)
		if errors.Is(err, memory.ErrNotFound) {
			// Disparition analogue à celle d'un message soft-deleted plus
			// haut: la conversation n'existe plus, il n'y a plus rien à
			// indexer, et retenter n'y changerait rien.
			return nil
		}
		if err != nil {
			return fmt.Errorf("load conversation scope: %w", err)
		}

		unit, indexable := memory.BuildUnit(cfg.Indexing, emb.Model(),
			scope, participants, anchor, previous)
		if !indexable {
			return nil
		}

		vecs, err := emb.Embed(ctx, []string{unit.EmbeddingText})
		if err != nil {
			return fmt.Errorf("embed: %w", err)
		}
		return units.Upsert(ctx, unit, vecs[0])
	}
}
