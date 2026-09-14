package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// lexicalSQL classe les messages par ts_rank_cd, pas les unités: le
// préambule "Conversation impliquant..." qui préfixe le texte embeddé d'une
// unité fausserait le classement, et un message pas encore vectorisé doit
// rester trouvable (le repli de la section 15 de la spec). L'unité
// éventuellement ancrée sur le message est jointe en LEFT JOIN LATERAL,
// donc un message non encore vectorisé ressort avec un MemoryUnitID nul.
//
// ConversationGuardJoinOnMessages est obligatoire ici, comme
// ConversationGuardJoin l'est pour denseSQL: sans lui, la branche
// memory_unit_acl du WHERE ne consulterait jamais conversations et pourrait
// rendre un message dont la conversation est supprimée ou dont le
// workspace diverge de celui du demandeur.
//
// Le LATERAL ne rend au plus qu'une unité active par message, la plus
// récemment mise à jour: un message pourrait avoir plusieurs unités actives
// si plusieurs modèles ou versions d'indexation cohabitaient (un simple
// LEFT JOIN les aurait toutes rendues, dupliquant la ligne du message), et
// rien n'empêche ce cas en écriture (Upsert ne désactive jamais les unités
// d'une version antérieure). Le EXISTS de la branche ACL est délibérément
// gardé sur ce même mu.memory_unit_id plutôt que sur "n'importe quelle
// unité ancrée sur ce message": l'identifiant et la fenêtre de séquence
// rendus doivent être ceux de l'unité que l'ACL couvre réellement, jamais
// ceux d'une autre unité active sur le même message.
//
// Le tri porte donc aussi sur memory_unit_id, pas seulement sur updated_at:
// deux unités mises à jour dans la même transaction partagent le même
// now(), et sans départage l'ordre est celui que le planificateur veut bien
// rendre. Ce n'est pas cosmétique, puisque la valeur choisie est le
// memory_unit_id rendu au client, c'est-à-dire l'identifiant contre lequel
// un partage d'ACL est ensuite écrit: partager d'après un résultat non
// déterministe reviendrait à partager parfois la mauvaise unité.
const lexicalSQL = `
WITH ` + ReadableCTE + `,
q AS (
	-- websearch_to_tsquery assemble les termes par ET, donc cette stratégie
	-- ne se déclenche que si le message contient tous les mots de la
	-- question, mots outils compris. Sur le corpus d'évaluation, seules
	-- quatre requêtes sur quarante-six trouvent ainsi un candidat lexical.
	--
	-- Cette rareté a l'air d'un défaut et n'en est pas un: elle a été
	-- mesurée. Assembler les termes par OU, avec ts_rank_cd pour trier,
	-- fait bien remonter la stratégie sur les questions à terme rare, mais
	-- elle rend alors des candidats faibles pour à peu près toutes les
	-- questions, qui inondent la fusion RRF exactement comme le faisait un
	-- graphe mal extrait. Mesuré: le rappel global tombe de 78 % à 61 %, la
	-- paraphrase perd quatre requêtes, et la détection de question sans
	-- réponse cesse de fonctionner puisqu'elle ne borne que le dense. Un
	-- plancher sur ts_rank_cd n'y change rien, de 0 à 0,10.
	--
	-- Le lexical vaut donc par sa précision, pas par son rappel: il ne dit
	-- quelque chose que quand il a une vraie raison, et son silence est une
	-- information que la détection de non-réponse exploite.
	SELECT websearch_to_tsquery('french', $3) AS tsq
)
SELECT
	mu.memory_unit_id,
	m.message_id,
	m.conversation_id,
	COALESCE(mu.start_sequence, m.sequence_number) AS start_sequence,
	COALESCE(mu.end_sequence, m.sequence_number)   AS end_sequence,
	ts_rank_cd(m.tsv, q.tsq)::float8 AS score,` + accessReasonExpr + ` AS access_reason
FROM messages m
CROSS JOIN q
` + ConversationGuardJoinOnMessages + `
LEFT JOIN readable r
	ON r.conversation_id = m.conversation_id
LEFT JOIN LATERAL (
	SELECT mu.memory_unit_id, mu.start_sequence, mu.end_sequence
	FROM memory_units mu
	WHERE mu.anchor_message_id = m.message_id
	  AND mu.active
	ORDER BY mu.updated_at DESC, mu.memory_unit_id DESC
	LIMIT 1
) mu ON TRUE
WHERE m.workspace_id = $1
  AND m.deleted_at IS NULL
  AND m.tsv @@ q.tsq
  AND (
	r.conversation_id IS NOT NULL
	OR EXISTS (
		SELECT 1 FROM memory_unit_acl a
		WHERE a.memory_unit_id = mu.memory_unit_id
		  AND a.principal_key = $2
		  AND a.permission = 'read'
	)
  )
ORDER BY score DESC, m.sequence_number DESC
LIMIT $4`

// SearchLexical classe les messages par pertinence plein texte française et
// rend les meilleurs candidats que le demandeur est autorisé à voir. La
// règle d'accès est appliquée dans lexicalSQL, pas ici: aucun filtrage côté
// Go après coup, pour ne jamais laisser fuiter en mémoire une ligne non
// autorisée.
//
// RawScore ici porte un ts_rank_cd, pas comparable à 1 - cosinus côté
// SearchDense: la fusion multi-stratégies compare des rangs, jamais ces
// scores bruts entre eux.
func (r *SearchRepo) SearchLexical(ctx context.Context,
	q memory.LexicalQuery) ([]memory.Candidate, error) {

	// ValidateRequester est appelé avant toute autre validation, y compris
	// le retour anticipé sur un texte vide juste en dessous: un refus
	// d'autorisation ne doit jamais être masqué par un retour anticipé sur
	// une entrée vide.
	if err := ValidateRequester(q.WorkspaceID, q.RequesterKey); err != nil {
		return nil, fmt.Errorf("lexical search: %w", err)
	}
	// websearch_to_tsquery peut rendre une requête tsquery vide à partir
	// d'un texte non vide (que de la ponctuation, par exemple): ce n'est pas
	// une erreur non plus, juste zéro résultat via m.tsv @@ q.tsq qui ne
	// matche jamais rien. On ne garde ce filtre en Go que pour le cas
	// évident d'un texte entièrement blanc, qui éviterait sinon un aller-
	// retour Postgres inutile.
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}

	rows, err := r.pool.Query(ctx, lexicalSQL,
		q.WorkspaceID, q.RequesterKey, q.Text, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("lexical search: %w", err)
	}
	defer rows.Close()

	var out []memory.Candidate
	rank := 0
	for rows.Next() {
		var (
			unitID *uuid.UUID
			c      memory.Candidate
		)
		if err := rows.Scan(&unitID, &c.AnchorMessageID, &c.ConversationID,
			&c.StartSequence, &c.EndSequence, &c.RawScore, &c.AccessReason); err != nil {
			return nil, fmt.Errorf("lexical search: scan: %w", err)
		}
		// Les rangs sont 1-based et attribués dans l'ordre où SQL rend les
		// lignes, comme pour SearchDense.
		rank++
		c.Strategy, c.Rank, c.MemoryUnitID = "lexical", rank, unitID
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lexical search: %w", err)
	}
	return out, nil
}

var _ memory.LexicalSearcher = (*SearchRepo)(nil)
