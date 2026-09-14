package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// ErrNoUnit signale une unité de mémoire inconnue. Enveloppée avec
// memory.ErrNotFound (voir UnitContext) pour qu'internal/api puisse la
// reconnaître avec errors.Is sans importer ce paquet dans un fichier
// non-test: c'est le même arrangement que memory.ErrWorkspaceMismatch pour
// MessageRepo.Append.
var ErrNoUnit = errors.New("memory unit not found")

// ACLRepo écrit et lit les ACL explicites d'une unité de mémoire
// (memory_unit_acl), et rend le contexte qu'il faut pour décider si un
// appelant peut y ajouter un principal.
type ACLRepo struct{ pool *pgxpool.Pool }

func NewACLRepo(pool *pgxpool.Pool) *ACLRepo { return &ACLRepo{pool: pool} }

// UnitContext rend le workspace de la conversation source, la conversation
// elle-même, son scope et ses participants actifs pour une unité.
//
// Le workspace rendu est celui de la conversation (c.workspace_id), pas
// celui, dénormalisé, de l'unité (mu.workspace_id): canShare doit décider
// sur exactement la même colonne que ConversationGuardJoin utilise côté
// recherche, pour qu'une éventuelle divergence entre les deux ne puisse
// jamais rendre une décision différente de celle que la recherche
// appliquerait.
//
// La requête exclut aussi une conversation supprimée (c.deleted_at IS NOT
// NULL), une unité désactivée (mu.active = FALSE) et une unité dont le
// message ancre est supprimé (m.deleted_at IS NOT NULL): dans chacun de ces
// trois cas, la recherche ne rendrait plus jamais cette unité par ailleurs
// (denseSQL applique les trois mêmes exclusions), donc un Grant dessus ne
// changerait rien d'observable. ErrNoUnit, pas un contexte périmé ou
// trompeur, pour qu'un partage ne puisse jamais s'appuyer sur une unité que
// plus personne ne peut lire.
func (r *ACLRepo) UnitContext(ctx context.Context,
	unitID uuid.UUID) (memory.UnitContext, error) {

	var uc memory.UnitContext
	err := r.pool.QueryRow(ctx, `
		SELECT c.workspace_id, mu.conversation_id, c.scope
		FROM memory_units mu
		JOIN conversations c ON c.conversation_id = mu.conversation_id
		JOIN messages m
			ON m.message_id = mu.anchor_message_id
			AND m.deleted_at IS NULL
		WHERE mu.memory_unit_id = $1
		  AND c.deleted_at IS NULL
		  AND mu.active`, unitID,
	).Scan(&uc.WorkspaceID, &uc.ConversationID, &uc.Scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.UnitContext{}, fmt.Errorf("unit context: %w: %w", ErrNoUnit, memory.ErrNotFound)
	}
	if err != nil {
		return memory.UnitContext{}, fmt.Errorf("unit context: %w", err)
	}

	participants, err := activeParticipants(ctx, r.pool, uc.ConversationID)
	if err != nil {
		return memory.UnitContext{}, err
	}
	uc.Participants = participants
	return uc, nil
}

// Grant ajoute ou met à jour une entrée d'ACL. Idempotent: réémettre le même
// partage n'est pas une erreur, ce qui rend l'appel rejouable.
func (r *ACLRepo) Grant(ctx context.Context, unitID uuid.UUID,
	principalKey, permission string) error {

	if permission == "" {
		permission = "read"
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO memory_unit_acl (memory_unit_id, principal_key, permission)
		VALUES ($1, $2, $3)
		ON CONFLICT (memory_unit_id, principal_key) DO UPDATE
			SET permission = EXCLUDED.permission`,
		unitID, principalKey, permission)
	if err != nil {
		return fmt.Errorf("grant acl: %w", err)
	}
	return nil
}

// Revoke retire une entrée d'ACL. Rend 0 sans erreur si elle n'existait pas
// déjà, ce qui rend l'appel rejouable.
func (r *ACLRepo) Revoke(ctx context.Context, unitID uuid.UUID,
	principalKey string) (int64, error) {

	tag, err := r.pool.Exec(ctx, `
		DELETE FROM memory_unit_acl
		WHERE memory_unit_id = $1 AND principal_key = $2`, unitID, principalKey)
	if err != nil {
		return 0, fmt.Errorf("revoke acl: %w", err)
	}
	return tag.RowsAffected(), nil
}

// List rend les entrées d'ACL d'une unité, triées par principal.
func (r *ACLRepo) List(ctx context.Context, unitID uuid.UUID) ([]memory.ACLEntry, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT principal_key, permission FROM memory_unit_acl
		WHERE memory_unit_id = $1 ORDER BY principal_key`, unitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []memory.ACLEntry
	for rows.Next() {
		var e memory.ACLEntry
		if err := rows.Scan(&e.PrincipalKey, &e.Permission); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
