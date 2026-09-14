package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// ErrNoConversation est déclarée dans messages.go (utilisée aussi par
// ConversationScope). Context, ci-dessous, l'enveloppe avec
// memory.ErrNotFound, comme ErrNoUnit pour ACLRepo.UnitContext et comme
// toutes les autres lectures de ce paquet, pour qu'internal/api et
// internal/jobs la reconnaissent avec errors.Is sans importer ce paquet
// dans un fichier non-test.

type ConversationRepo struct{ pool *pgxpool.Pool }

func NewConversationRepo(pool *pgxpool.Pool) *ConversationRepo {
	return &ConversationRepo{pool: pool}
}

// activeParticipants rend les participant_key actifs d'une conversation,
// triés par ordre d'arrivée. Partagée par ConversationRepo.Context et
// ACLRepo.UnitContext: les deux ont besoin exactement de la même requête
// pour appliquer la même règle d'accès (participation ou scope workspace).
func activeParticipants(ctx context.Context, pool *pgxpool.Pool,
	conversationID string) ([]string, error) {

	rows, err := pool.Query(ctx, `
		SELECT participant_key FROM conversation_participants
		WHERE conversation_id = $1 AND left_at IS NULL
		ORDER BY joined_at, participant_key`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Declare crée ou met à jour une conversation et ses participants. C'est
// l'appel optionnel qui permet de fixer un scope non par défaut ou d'inscrire
// des participants passifs, qui liront sans avoir écrit.
//
// Le DO UPDATE filtre sur workspace_id ET sur deleted_at, dans le même esprit
// que SoftDelete:
// conversation_id est un TEXT libre choisi par le client et indexé seul, donc
// devinable depuis un autre workspace, et ce DO UPDATE écrase un scope, ce
// qui suffit à ouvrir ou à fermer la lecture de tous les souvenirs de la
// conversation. La couche HTTP charge déjà le contexte de la conversation
// avant d'appeler ici (voir handlePostConversation); ce filtre est le second
// verrou, celui qui tient même sur la course entre les deux appels. Un
// conflit sur une conversation d'un autre workspace ne touche alors aucune
// ligne, et rend memory.ErrWorkspaceMismatch plutôt qu'un succès silencieux
// qui aurait quand même inscrit les participants: même sentinelle que
// MessageRepo.Append pour la même situation.
//
// Le filtre sur deleted_at ferme le trou symétrique. Context, que la couche
// HTTP appelle avant, écarte les conversations supprimées: une conversation
// soft-deleted lui rend ErrNotFound, donc le handler la traite comme une
// création et n'autorise plus rien. Sans ce second prédicat, le DO UPDATE
// ressuscitait quand même la ligne existante: n'importe quel token du
// workspace en réécrivait le scope et s'y inscrivait comme participant, sur
// le seul chemin que cette autorisation existe pour garder. La conversation
// restait supprimée pour la recherche, donc aucune fuite en lecture, mais
// tout message écrit ensuite s'y accumulait sans jamais pouvoir en ressortir.
// Les deux prédicats donnent la même sentinelle, et la couche HTTP rend 404
// dans les deux cas: une conversation supprimée est introuvable, exactement
// comme une conversation d'autrui.
func (r *ConversationRepo) Declare(ctx context.Context, conversationID,
	workspaceID, scope string, participants []string) error {

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id, scope)
		VALUES ($1, $2, $3)
		ON CONFLICT (conversation_id) DO UPDATE
			SET scope = EXCLUDED.scope, updated_at = now()
			WHERE conversations.workspace_id = $2
			  AND conversations.deleted_at IS NULL`,
		conversationID, workspaceID, scope)
	if err != nil {
		return fmt.Errorf("declare conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: conversation %q is deleted or belongs to another workspace than %q",
			memory.ErrWorkspaceMismatch, conversationID, workspaceID)
	}

	for _, p := range participants {
		_, err := tx.Exec(ctx, `
			INSERT INTO conversation_participants (conversation_id, participant_key)
			VALUES ($1, $2)
			ON CONFLICT (conversation_id, participant_key) DO NOTHING`,
			conversationID, p)
		if err != nil {
			return fmt.Errorf("declare participant: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// Context rend le workspace, le scope et les participants actifs d'une
// conversation entière. Utilisée par handleDeleteConversation pour
// appliquer, avant toute suppression, la même règle d'accès que le partage
// d'une unité (voir canShare/canDeleteConversation côté HTTP): incarner un
// participant, ou bénéficier d'un scope 'workspace'. Une conversation déjà
// supprimée rend ErrNoConversation, jamais un contexte périmé.
func (r *ConversationRepo) Context(ctx context.Context,
	conversationID string) (memory.ConversationContext, error) {

	var cc memory.ConversationContext
	err := r.pool.QueryRow(ctx, `
		SELECT workspace_id, scope FROM conversations
		WHERE conversation_id = $1 AND deleted_at IS NULL`, conversationID,
	).Scan(&cc.WorkspaceID, &cc.Scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.ConversationContext{},
			fmt.Errorf("conversation context: %w: %w", ErrNoConversation, memory.ErrNotFound)
	}
	if err != nil {
		return memory.ConversationContext{}, fmt.Errorf("conversation context: %w", err)
	}

	participants, err := activeParticipants(ctx, r.pool, conversationID)
	if err != nil {
		return memory.ConversationContext{}, err
	}
	cc.Participants = participants
	return cc, nil
}

// enqueueConversationReevalSQL met en file un graph_reeval par message de la
// conversation qui source réellement une relation.
//
// Sans ça, la suppression d'une conversation entière ne posait aucun job:
// seules l'édition et la suppression d'un message individuel en posaient. Les
// relations sourcées uniquement par les messages de cette conversation ne
// prenaient donc jamais d'invalidated_at, et aucun balayage ne repassait,
// Reevaluate n'étant appelée que par un job. L'invariant « rien n'est jamais
// supprimé du graphe, mais une source disparue pose invalidated_at » était
// violé pour tout ce chemin, et un valid_until posé par une de ces relations
// restait en place: la réponse rendue était fausse, pas seulement sale.
//
// Le DISTINCT et la jointure sur graph_relation_sources sont ce qui borne la
// mise en file: un message par relation sourcée, jamais un job par message de
// la conversation. Une conversation qui n'a alimenté aucune relation ne pose
// donc rien du tout, ce qui est le cas courant quand le graphe est désactivé,
// puisqu'il n'y a alors aucune relation à sourcer et donc aucun job orphelin.
//
// Le job est posé dans la transaction de la suppression, comme le fait
// enqueueInTx pour MessageRepo.Append: une suppression commitée sans son job
// laisserait exactement l'état que ce job existe pour réparer.
//
// L'ordre importe: cette requête vient après l'UPDATE des messages, donc le
// worker qui traitera le job verra bien les deleted_at, quel que soit le
// moment où il le réclame. Il ne peut de toute façon pas le voir avant le
// commit.
const enqueueConversationReevalSQL = `
INSERT INTO jobs (job_type, workspace_id, conversation_id, payload)
SELECT DISTINCT 'graph_reeval', $2, $1,
       jsonb_build_object('message_id', m.message_id)
FROM messages m
JOIN graph_relation_sources s ON s.message_id = m.message_id
WHERE m.conversation_id = $1`

// SoftDelete masque une conversation et désactive toutes ses unités dans la
// même transaction. Les messages restent en base comme source de vérité et
// pour l'audit, mais rien ne peut plus ressortir de la recherche.
//
// workspaceID doit être celui de l'appelant: conversation_id est un TEXT
// libre choisi par le client, indexé seul (sans workspace_id), donc devinable
// depuis un autre workspace. Filtrer dessus aussi rend une conversation
// étrangère indiscernable, pour l'appelant, d'une conversation inexistante
// (0 ligne touchée), plutôt que de la supprimer parce qu'il en connaissait
// l'identifiant.
//
// Rend 0 sans erreur si la conversation était déjà supprimée ou n'appartient
// pas à ce workspace, ce qui rend l'appel rejouable.
func (r *ConversationRepo) SoftDelete(ctx context.Context,
	conversationID, workspaceID string) (int64, error) {

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE conversations SET deleted_at = now(), updated_at = now()
		WHERE conversation_id = $1 AND workspace_id = $2 AND deleted_at IS NULL`,
		conversationID, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("soft delete conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE messages SET deleted_at = now()
		WHERE conversation_id = $1 AND deleted_at IS NULL`, conversationID); err != nil {
		return 0, fmt.Errorf("soft delete messages: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_units SET active = FALSE, updated_at = now()
		WHERE conversation_id = $1 AND active`, conversationID); err != nil {
		return 0, fmt.Errorf("deactivate units: %w", err)
	}
	if _, err := tx.Exec(ctx, enqueueConversationReevalSQL,
		conversationID, workspaceID); err != nil {
		return 0, fmt.Errorf("enqueue graph reeval: %w", err)
	}

	return tag.RowsAffected(), tx.Commit(ctx)
}
