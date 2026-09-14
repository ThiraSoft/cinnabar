package postgres

import (
	"context"
	"fmt"
	"sort"

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
  AND ($5::text[] IS NULL OR mu.conversation_id = ANY($5::text[]))
  AND ($6::jsonb IS NULL OR cinnabar_metadata_match(m.metadata, $6::jsonb))
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

	convs, filter, err := restriction(q.CandidateQuery)
	if err != nil {
		return nil, fmt.Errorf("dense search: %w", err)
	}

	// Un filtre ajouté à un parcours HNSW peut rendre moins de lignes que
	// la limite: l'index ne visite que ef_search voisins, et le WHERE les
	// élimine ensuite. Le parcours itératif de pgvector 0.8 relance la
	// visite jusqu'à remplir la limite. Il ne vaut que pour la transaction,
	// d'où la transaction, et seulement quand un filtre est posé: sans
	// filtre, rien ne change au chemin historique.
	var db querier = r.pool
	if convs != nil || filter != nil {
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("dense search: begin: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = relaxed_order`); err != nil {
			return nil, fmt.Errorf("dense search: iterative scan: %w", err)
		}
		db = tx
	}

	rows, err := db.Query(ctx, denseSQL,
		q.WorkspaceID, q.RequesterKey, pgvector.NewVector(q.Embedding), q.Limit,
		convs, filter)
	if err != nil {
		return nil, fmt.Errorf("dense search: %w", err)
	}
	defer rows.Close()

	var out []memory.Candidate
	for rows.Next() {
		var (
			unitID uuid.UUID
			c      memory.Candidate
		)
		if err := rows.Scan(&unitID, &c.AnchorMessageID, &c.ConversationID,
			&c.StartSequence, &c.EndSequence, &c.RawScore, &c.AccessReason); err != nil {
			return nil, fmt.Errorf("dense search: scan: %w", err)
		}
		id := unitID
		c.Strategy, c.MemoryUnitID = "dense", &id
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dense search: %w", err)
	}

	// relaxed_order peut rendre des voisins légèrement désordonnés. Le tri
	// stable ne change rien quand l'ordre est déjà strict, et il rend à la
	// fusion le classement par similarité qu'elle attend.
	sort.SliceStable(out, func(i, j int) bool { return out[i].RawScore > out[j].RawScore })
	// Les rangs sont 1-based et attribués après ce tri: la fusion
	// multi-stratégies en dépend.
	for i := range out {
		out[i].Rank = i + 1
	}
	return out, nil
}
