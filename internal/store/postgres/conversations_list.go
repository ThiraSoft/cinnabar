package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// conversationListSQL rend les conversations lisibles par le demandeur dont
// l'identifiant commence par un préfixe, dans l'ordre des identifiants. La
// règle d'accès est ReadableCTE, la même que la recherche: une conversation
// que le demandeur ne pourrait pas lire n'est pas non plus nommée.
//
// $3 est le préfixe déjà échappé pour LIKE, $4 le dernier identifiant de la
// page précédente, vide pour la première.
const conversationListSQL = `
WITH ` + ReadableCTE + `
SELECT c.conversation_id, c.scope, c.updated_at
FROM conversations c
JOIN readable r ON r.conversation_id = c.conversation_id
WHERE c.conversation_id LIKE $3 || '%' ESCAPE '\'
  AND c.conversation_id > $4
ORDER BY c.conversation_id
LIMIT $5`

// List rend une page de conversations lisibles de préfixe prefix, après
// l'identifiant after.
func (r *ConversationRepo) List(ctx context.Context, workspaceID, requesterKey,
	prefix, after string, limit int) ([]memory.ConversationSummary, error) {

	if err := ValidateRequester(workspaceID, requesterKey); err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, conversationListSQL,
		workspaceID, requesterKey, escapeLike(prefix), after, limit)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	var out []memory.ConversationSummary
	for rows.Next() {
		var c memory.ConversationSummary
		if err := rows.Scan(&c.ConversationID, &c.Scope, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list conversations: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	return out, nil
}

// escapeLike rend un texte littéral pour LIKE ... ESCAPE '\'.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
