package postgres

import (
	"context"
	"fmt"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// listSQL rend les messages lisibles par le demandeur, du plus récent au plus
// ancien. La règle d'accès est celle des stratégies de recherche, mot pour
// mot: ReadableCTE pour le scope, ConversationGuardJoinOnMessages pour le
// workspace et la suppression, et la branche memory_unit_acl sur l'unité
// ancrée au message, comme le lexical.
//
// Les colonnes sont préfixées, contrairement à messageColumns: conversations
// porte elle aussi conversation_id, workspace_id, created_at et deleted_at.
//
// Le curseur compare le couple (created_at, message_id): deux messages d'un
// même instant restent départagés, et une page ne rend jamais deux fois le
// même message.
const listSQL = `
WITH ` + ReadableCTE + `
SELECT
	m.message_id, m.conversation_id, m.workspace_id, m.sequence_number,
	m.author_key, m.role, m.content, m.created_at, m.edited_at, m.deleted_at,
	m.metadata
FROM messages m
` + ConversationGuardJoinOnMessages + `
LEFT JOIN readable r
	ON r.conversation_id = m.conversation_id
LEFT JOIN LATERAL (
	SELECT mu.memory_unit_id
	FROM memory_units mu
	WHERE mu.anchor_message_id = m.message_id
	  AND mu.active
	ORDER BY mu.updated_at DESC, mu.memory_unit_id DESC
	LIMIT 1
) mu ON TRUE
WHERE m.workspace_id = $1
  AND m.deleted_at IS NULL
  AND ($3::text[] IS NULL OR m.conversation_id = ANY($3::text[]))
  AND ($4::jsonb IS NULL OR cinnabar_metadata_match(m.metadata, $4::jsonb))
  AND ($5::timestamptz IS NULL OR (m.created_at, m.message_id) < ($5::timestamptz, $6::uuid))
  AND (
	r.conversation_id IS NOT NULL
	OR EXISTS (
		SELECT 1 FROM memory_unit_acl a
		WHERE a.memory_unit_id = mu.memory_unit_id
		  AND a.principal_key = $2
		  AND a.permission = 'read'
	)
  )
ORDER BY m.created_at DESC, m.message_id DESC
LIMIT $7`

// ListMessages rend une page de messages lisibles, sans score. Comme pour
// les stratégies de recherche, la règle d'accès est dans le SQL et rien
// n'est filtré en Go après coup.
func (r *SearchRepo) ListMessages(ctx context.Context, q memory.ListQuery) ([]memory.Message, error) {
	if err := ValidateRequester(q.WorkspaceID, q.RequesterKey); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	convs, filter, err := restriction(q.CandidateQuery)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}

	rows, err := r.pool.Query(ctx, listSQL, q.WorkspaceID, q.RequesterKey,
		convs, filter, q.BeforeAt, q.BeforeID, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var out []memory.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("list messages: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	return out, nil
}
