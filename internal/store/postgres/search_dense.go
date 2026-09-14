package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// SearchRepo implémente les stratégies de recherche de candidats sur
// PostgreSQL: dense ici, lexicale et graphe dans les tâches suivantes.
type SearchRepo struct{ pool *pgxpool.Pool }

func NewSearchRepo(pool *pgxpool.Pool) *SearchRepo { return &SearchRepo{pool: pool} }

var _ memory.DenseSearcher = (*SearchRepo)(nil)

// embeddingDimension est la dimension attendue des vecteurs d'embedding sur
// tout le service. Une requête d'une autre dimension doit produire une
// erreur de domaine claire plutôt que remonter telle quelle une erreur
// Postgres opaque au niveau de l'opérateur vectoriel.
const embeddingDimension = 768

// denseSQL classe les unités par distance cosinus. Le join sur messages avec
// deleted_at IS NULL est le filet de sécurité du critère 6: même si une unité
// n'a pas encore été désactivée, son message supprimé ne ressort pas.
//
// ConversationGuardJoin est obligatoire ici, en plus de ReadableCTE: sans
// lui, la branche memory_unit_acl du WHERE ne consulterait jamais
// conversations et pourrait rendre une unité dont la conversation est
// supprimée ou dont le workspace diverge de celui du demandeur.
//
// ReadableCTE, ConversationGuardJoin et accessReasonExpr sont des constantes
// de type chaîne: la concaténation est donc elle-même une constante, ce qui
// permet à denseSQL d'être un const plutôt qu'un var package-level
// réassignable par erreur ailleurs dans le paquet.
const denseSQL = `
WITH ` + ReadableCTE + `
SELECT
	mu.memory_unit_id,
	mu.anchor_message_id,
	mu.conversation_id,
	mu.start_sequence,
	mu.end_sequence,
	1 - (mu.embedding <=> $3) AS score,` + accessReasonExpr + ` AS access_reason
FROM memory_units mu
JOIN messages m
	ON m.message_id = mu.anchor_message_id
	AND m.deleted_at IS NULL
` + ConversationGuardJoin + `
LEFT JOIN readable r
	ON r.conversation_id = mu.conversation_id
WHERE mu.workspace_id = $1
  AND mu.active
  AND mu.embedding IS NOT NULL
  AND (mu.embedding <=> $3) <> 'NaN'
  AND (
	r.conversation_id IS NOT NULL
	OR EXISTS (
		SELECT 1 FROM memory_unit_acl a
		WHERE a.memory_unit_id = mu.memory_unit_id
		  AND a.principal_key = $2
		  AND a.permission = 'read'
	)
  )
ORDER BY mu.embedding <=> $3
LIMIT $4`

// SearchDense classe les unités par distance cosinus et rend les meilleurs
// candidats que le demandeur est autorisé à voir. La règle d'accès est
// appliquée dans denseSQL, pas ici: aucun filtrage côté Go après coup, pour
// ne jamais laisser fuiter en mémoire une ligne non autorisée.
func (r *SearchRepo) SearchDense(ctx context.Context,
	q memory.DenseQuery) ([]memory.Candidate, error) {

	// ValidateRequester est appelé avant toute autre validation: un refus
	// d'autorisation ne doit jamais être masqué par un retour anticipé sur
	// une entrée vide (voir son commentaire pour la raison précise).
	if err := ValidateRequester(q.WorkspaceID, q.RequesterKey); err != nil {
		return nil, fmt.Errorf("dense search: %w", err)
	}
	if len(q.Embedding) != embeddingDimension {
		return nil, fmt.Errorf("dense search: embedding dimension must be %d, got %d",
			embeddingDimension, len(q.Embedding))
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}

	rows, err := r.pool.Query(ctx, denseSQL,
		q.WorkspaceID, q.RequesterKey, pgvector.NewVector(q.Embedding), q.Limit)
	if err != nil {
		return nil, fmt.Errorf("dense search: %w", err)
	}
	defer rows.Close()

	var out []memory.Candidate
	rank := 0
	for rows.Next() {
		var (
			unitID uuid.UUID
			c      memory.Candidate
		)
		if err := rows.Scan(&unitID, &c.AnchorMessageID, &c.ConversationID,
			&c.StartSequence, &c.EndSequence, &c.RawScore, &c.AccessReason); err != nil {
			return nil, fmt.Errorf("dense search: scan: %w", err)
		}
		// Les rangs sont 1-based et attribués dans l'ordre où SQL rend les
		// lignes: la fusion multi-stratégies en dépend, donc on ne réordonne
		// jamais ici.
		rank++
		id := unitID
		c.Strategy, c.Rank, c.MemoryUnitID = "dense", rank, &id
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dense search: %w", err)
	}
	return out, nil
}
