package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// UnitRepo implémente memory.UnitRepo sur PostgreSQL.
type UnitRepo struct{ pool *pgxpool.Pool }

func NewUnitRepo(pool *pgxpool.Pool) *UnitRepo { return &UnitRepo{pool: pool} }

// Upsert écrit ou réécrit une unité. L'identifiant étant déterministe, un
// rejeu retombe sur la même ligne, ce qui est le second verrou du critère 5.
func (r *UnitRepo) Upsert(ctx context.Context, u memory.Unit, embedding []float32) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO memory_units
			(memory_unit_id, workspace_id, conversation_id, anchor_message_id,
			 start_sequence, end_sequence, embedding_text, embedding,
			 embedding_model, indexing_strategy, indexing_version, scope, active)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,TRUE)
		ON CONFLICT (memory_unit_id) DO UPDATE SET
			start_sequence = EXCLUDED.start_sequence,
			end_sequence   = EXCLUDED.end_sequence,
			embedding_text = EXCLUDED.embedding_text,
			embedding      = EXCLUDED.embedding,
			scope          = EXCLUDED.scope,
			active         = TRUE,
			updated_at     = now()`,
		u.MemoryUnitID, u.WorkspaceID, u.ConversationID, u.AnchorMessageID,
		u.StartSequence, u.EndSequence, u.EmbeddingText,
		pgvector.NewVector(embedding),
		u.EmbeddingModel, u.Strategy, u.Version, u.Scope)
	if err != nil {
		return fmt.Errorf("upsert memory unit: %w", err)
	}
	return nil
}

// deactivateCoveringSQL est partagée par MessageRepo.EditAndDeactivate et
// MessageRepo.SoftDeleteAndDeactivate, chacune dans sa propre transaction:
// les deux ne doivent jamais pouvoir diverger, sous peine qu'une suppression
// et une édition désactivent des unités différentes pour la même situation.
const deactivateCoveringSQL = `
	UPDATE memory_units
	SET active = FALSE, updated_at = now()
	WHERE conversation_id = $1
	  AND active
	  AND $2 BETWEEN start_sequence AND end_sequence
	RETURNING memory_unit_id, anchor_message_id`

// deactivateCovering désactive toute unité dont l'intervalle contient ce
// numéro de séquence. On ne se limite pas aux unités ancrées sur le message:
// une unité voisine qui le citait en contexte deviendrait mensongère.
//
// Malgré son nom, elle rend les identifiants des messages ancres (pas des
// unités): l'appelant en a besoin pour reconstruire les unités désactivées.
// q est le pgx.Tx de l'édition ou de la suppression en cours; querier
// (déclarée dans messages.go) est l'interface minimale que *pgxpool.Pool
// satisfait aussi, ce dont les tests de ce paquet se servent pour vérifier
// la requête toute seule.
func deactivateCovering(ctx context.Context, q querier, conv string,
	sequence int64) ([]uuid.UUID, error) {

	rows, err := q.Query(ctx, deactivateCoveringSQL, conv, sequence)
	if err != nil {
		return nil, fmt.Errorf("deactivate units: %w", err)
	}
	defer rows.Close()

	var anchors []uuid.UUID
	for rows.Next() {
		var unitID, anchorID uuid.UUID
		if err := rows.Scan(&unitID, &anchorID); err != nil {
			return nil, fmt.Errorf("scan deactivated unit: %w", err)
		}
		anchors = append(anchors, anchorID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deactivated units: %w", err)
	}
	return anchors, nil
}
