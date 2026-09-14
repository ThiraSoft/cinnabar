package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// ErrNoConversation signale qu'aucune conversation ne correspond à
// l'identifiant demandé. Elle nomme la disparition pour un lecteur de log;
// c'est memory.ErrNotFound, dont elle est toujours accompagnée, qui porte la
// décision côté appelant.
//
// Convention du paquet, sans exception: toute méthode qui ne trouve pas sa
// ligne rend une erreur qui satisfait errors.Is(err, memory.ErrNotFound).
// Une sentinelle locale peut la précéder pour dire laquelle des deux
// disparitions s'est produite (fmt.Errorf("...: %w: %w", ErrNoConversation,
// memory.ErrNotFound)), jamais la remplacer: un appelant hors de ce paquet
// n'a alors qu'une seule sentinelle à connaître, quel que soit le point
// d'entrée. C'est ce qui laisse internal/jobs décider sur memory.ErrNotFound
// plutôt que d'importer ce paquet pour une sentinelle de plus.
var ErrNoConversation = errors.New("conversation not found")

// MessageRepo implémente memory.MessageRepo sur PostgreSQL.
type MessageRepo struct{ pool *pgxpool.Pool }

func NewMessageRepo(pool *pgxpool.Pool) *MessageRepo { return &MessageRepo{pool: pool} }

const messageColumns = `
	message_id, conversation_id, workspace_id, sequence_number,
	author_key, role, content, created_at, edited_at, deleted_at`

func scanMessage(row pgx.Row) (memory.Message, error) {
	var m memory.Message
	err := row.Scan(&m.MessageID, &m.ConversationID, &m.WorkspaceID,
		&m.SequenceNumber, &m.AuthorKey, &m.Role, &m.Content,
		&m.CreatedAt, &m.EditedAt, &m.DeletedAt)
	return m, err
}

// testHookAfterIdempotencyCheck, quand il est non nil, s'exécute juste après
// la vérification d'idempotence et avant l'incrément de séquence. Réservé aux
// tests: il sert à forcer l'entrelacement qui mène au chemin ON CONFLICT de
// l'insertion, autrement presque impossible à provoquer parce que le verrou
// de ligne sur la conversation sérialise les appels. En production il reste
// nil et ne coûte qu'une comparaison.
var testHookAfterIdempotencyCheck func()

// Append fait, dans une seule transaction, l'auto-création de la
// conversation, l'accumulation du participant, la vérification
// d'idempotence, l'attribution de la séquence puis l'insertion. L'ordre est
// contraint: l'idempotence est vérifiée avant l'incrément de séquence, sinon
// un rejeu consommerait un numéro pour rien et laisserait un trou permanent
// dans l'ordonnancement de la conversation.
func (r *MessageRepo) Append(ctx context.Context, in memory.AppendInput,
	previousCount int, jobs []memory.AppendJob) (memory.AppendResult, error) {

	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now().UTC()
	}
	if len(in.Metadata) == 0 {
		in.Metadata = []byte(`{}`)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return memory.AppendResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Conversation. ON CONFLICT DO NOTHING préserve un scope déjà déclaré
	// par un appel explicite à POST /v1/conversations: on n'écrase jamais le
	// scope ici.
	if _, err := tx.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id, scope)
		VALUES ($1, $2, $3)
		ON CONFLICT (conversation_id) DO NOTHING`,
		in.ConversationID, in.WorkspaceID, in.DefaultScope); err != nil {
		return memory.AppendResult{}, fmt.Errorf("ensure conversation: %w", err)
	}

	var scope, convWorkspace string
	if err := tx.QueryRow(ctx, `
		SELECT scope, workspace_id FROM conversations WHERE conversation_id = $1`,
		in.ConversationID).Scan(&scope, &convWorkspace); err != nil {
		return memory.AppendResult{}, fmt.Errorf("read scope: %w", err)
	}
	// conversation_id est un TEXT libre, indexé seul: une conversation déjà
	// créée sous un autre workspace doit bloquer ici, avant tout effet de
	// bord (participant, séquence, insertion) et avant de rendre quoi que ce
	// soit à l'appelant, pour ne jamais lui exposer les participants ou les
	// messages précédents d'un workspace qui n'est pas le sien.
	if convWorkspace != in.WorkspaceID {
		return memory.AppendResult{}, fmt.Errorf("%w: conversation %q belongs to workspace %q",
			memory.ErrWorkspaceMismatch, in.ConversationID, convWorkspace)
	}

	// 2. Participant.
	if _, err := tx.Exec(ctx, `
		INSERT INTO conversation_participants (conversation_id, participant_key)
		VALUES ($1, $2)
		ON CONFLICT (conversation_id, participant_key) DO NOTHING`,
		in.ConversationID, in.AuthorKey); err != nil {
		return memory.AppendResult{}, fmt.Errorf("ensure participant: %w", err)
	}

	// 3. Idempotence, avant l'incrément de séquence.
	if in.RequestID != "" {
		m, err := scanMessage(tx.QueryRow(ctx,
			`SELECT `+messageColumns+` FROM messages
			 WHERE conversation_id = $1 AND request_id = $2`,
			in.ConversationID, in.RequestID))
		if err == nil {
			return r.finishReplay(ctx, tx, m, scope, previousCount)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return memory.AppendResult{}, fmt.Errorf("idempotency lookup: %w", err)
		}
	}

	if testHookAfterIdempotencyCheck != nil {
		testHookAfterIdempotencyCheck()
	}

	// 4 et 5 se font sous un point de sauvegarde (une savepoint Postgres,
	// exposée par pgx comme une transaction imbriquée: tx.Begin dessus donne
	// un pseudo-Tx dont Commit fait RELEASE SAVEPOINT et Rollback fait
	// ROLLBACK TO SAVEPOINT). Une transaction concurrente qui a gagné la
	// course sur request_id ne se révèle qu'à l'étape 5, après que l'étape 4
	// a déjà incrémenté next_sequence: sans ce point de sauvegarde, ce
	// numéro de séquence resterait consommé pour rien et laisserait un trou
	// permanent, exactement ce que l'idempotence est censée éviter. Le
	// rollback ici annule uniquement l'UPDATE de l'étape 4, pas la
	// transaction entière: la vérification d'idempotence et l'auto-création
	// de la conversation faites plus haut restent acquises.
	seqTx, err := tx.Begin(ctx)
	if err != nil {
		return memory.AppendResult{}, fmt.Errorf("begin sequence savepoint: %w", err)
	}

	// 4. Séquence. Le verrou de ligne sérialise cette conversation
	// seulement: deux Append concurrents sur des conversations différentes
	// ne se bloquent pas entre eux.
	var seq int64
	if err := seqTx.QueryRow(ctx, `
		UPDATE conversations
		SET next_sequence = next_sequence + 1, updated_at = now()
		WHERE conversation_id = $1
		RETURNING next_sequence`, in.ConversationID).Scan(&seq); err != nil {
		return memory.AppendResult{}, fmt.Errorf("next sequence: %w", err)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return memory.AppendResult{}, fmt.Errorf("generate message id: %w", err)
	}

	// 5. Insertion. Une clause SELECT préalable ne peut pas gagner la course
	// sur la clé d'idempotence: deux transactions concurrentes avec le même
	// request_id ne voient pas encore l'insertion non validée l'une de
	// l'autre, et passent donc toutes les deux la vérification de l'étape 3.
	// L'insertion est donc en ON CONFLICT DO NOTHING; un conflit veut dire
	// qu'une transaction concurrente a gagné la course, et on relit alors
	// son message.
	//
	// Ce chemin est rarement emprunté: le verrou de ligne pris par l'étape 4
	// sur la ligne conversations sérialise déjà, dans la quasi-totalité des
	// cas, les appels concurrents sur une même conversation (voir le rapport
	// de la tâche 6 pour l'investigation complète). Il reste néanmoins
	// atteignable, précisément parce que la vérification d'idempotence de
	// l'étape 3 précède ce verrou: une deuxième transaction peut passer cette
	// vérification avant que la première n'ait committé, puis se retrouver
	// bloquée sur le verrou, puis arriver ici après que la première a
	// committé et inséré la ligne. TestAppendConflictPathResolvesToSameMessage
	// force cet entrelacement via testHookAfterIdempotencyCheck et couvre
	// exactement ce cas: ne pas retirer cette clause en la croyant morte.
	var requestID any
	if in.RequestID != "" {
		requestID = in.RequestID
	}
	tag, err := seqTx.Exec(ctx, `
		INSERT INTO messages
			(message_id, conversation_id, workspace_id, sequence_number,
			 author_key, role, content, request_id, created_at, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (conversation_id, request_id) DO NOTHING`,
		id, in.ConversationID, in.WorkspaceID, seq, in.AuthorKey, in.Role,
		in.Content, requestID, in.CreatedAt, in.Metadata)
	if err != nil {
		return memory.AppendResult{}, fmt.Errorf("insert message: %w", err)
	}

	if tag.RowsAffected() == 0 {
		// Une transaction concurrente a gagné la course: on annule
		// l'incrément de séquence de cette tentative (ROLLBACK TO SAVEPOINT)
		// pour ne pas laisser de trou, puis on relit son message.
		if err := seqTx.Rollback(ctx); err != nil {
			return memory.AppendResult{}, fmt.Errorf("rollback sequence savepoint: %w", err)
		}
		// Invariant: on n'atteint ce RowsAffected() == 0 que si
		// in.RequestID != "", puisqu'un request_id NULL ne peut jamais
		// entrer en conflit sur l'index inféré (NULL <> NULL en SQL). Si ce
		// branchement était atteint autrement, cette relecture échouerait
		// avec un "no rows in result set" incompréhensible.
		m, err := scanMessage(tx.QueryRow(ctx,
			`SELECT `+messageColumns+` FROM messages
			 WHERE conversation_id = $1 AND request_id = $2`,
			in.ConversationID, in.RequestID))
		if err != nil {
			return memory.AppendResult{}, fmt.Errorf("reread after conflict: %w", err)
		}
		return r.finishReplay(ctx, tx, m, scope, previousCount)
	}

	if err := seqTx.Commit(ctx); err != nil {
		return memory.AppendResult{}, fmt.Errorf("commit sequence savepoint: %w", err)
	}

	written := memory.Message{
		MessageID: id, ConversationID: in.ConversationID,
		WorkspaceID: in.WorkspaceID, SequenceNumber: seq,
		AuthorKey: in.AuthorKey, Role: in.Role, Content: in.Content,
		CreatedAt: in.CreatedAt,
	}
	prev, err := previousInTx(ctx, tx, in.ConversationID, seq, previousCount)
	if err != nil {
		// previousInTx nomme déjà l'étape dans son propre message d'erreur.
		return memory.AppendResult{}, err
	}
	parts, err := participants(ctx, tx, in.ConversationID)
	if err != nil {
		// participants nomme déjà l'étape dans son propre message d'erreur.
		return memory.AppendResult{}, err
	}

	// 6. Jobs, dans cette même transaction (section 6.1 de la spec). Les
	// poser après le commit, sur une autre connexion, laissait une fenêtre
	// où une panne rendait le message durable sans qu'aucun job ne vienne
	// jamais l'indexer: rien ne balaie les messages sans unité active, et le
	// reaper de la file ne récupère que les jobs déjà arrivés en table.
	// Rien n'est posé sur les deux chemins de rejeu (finishReplay), où
	// aucun message neuf n'a été écrit.
	if err := enqueueInTx(ctx, tx, jobs, written); err != nil {
		return memory.AppendResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return memory.AppendResult{}, fmt.Errorf("commit: %w", err)
	}

	return memory.AppendResult{
		Message: written, Scope: scope,
		Participants: parts, Previous: prev,
	}, nil
}

// enqueueInTx insère les jobs décrits par le domaine dans la transaction en
// cours. Le payload est composé ici et pas dans le domaine: l'identifiant du
// message n'existe qu'une fois la transaction ouverte, et memory.AppendJob ne
// porte donc que le type (voir son commentaire).
func enqueueInTx(ctx context.Context, tx pgx.Tx, jobs []memory.AppendJob,
	written memory.Message) error {

	if len(jobs) == 0 {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"message_id": written.MessageID})
	if err != nil {
		return fmt.Errorf("encode job payload: %w", err)
	}
	for _, j := range jobs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO jobs (job_type, workspace_id, conversation_id, payload)
			VALUES ($1, $2, $3, $4)`,
			j.Type, written.WorkspaceID, written.ConversationID, payload); err != nil {
			return fmt.Errorf("enqueue %s: %w", j.Type, err)
		}
	}
	return nil
}

// finishReplay termine un Append qui retombe sur un message déjà écrit,
// qu'il s'agisse d'un rejeu détecté à l'étape 3 (lecture avant l'incrément
// de séquence) ou d'une course perdue à l'insertion (étape 5). Dans les deux
// cas, aucun numéro de séquence n'a été consommé pour ce message.
func (r *MessageRepo) finishReplay(ctx context.Context, tx pgx.Tx,
	m memory.Message, scope string, previousCount int) (memory.AppendResult, error) {

	prev, err := previousInTx(ctx, tx, m.ConversationID, m.SequenceNumber, previousCount)
	if err != nil {
		// previousInTx nomme déjà l'étape dans son propre message d'erreur.
		return memory.AppendResult{}, err
	}
	parts, err := participants(ctx, tx, m.ConversationID)
	if err != nil {
		// participants nomme déjà l'étape dans son propre message d'erreur.
		return memory.AppendResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.AppendResult{}, fmt.Errorf("commit: %w", err)
	}
	return memory.AppendResult{
		Message: m, Replayed: true, Scope: scope,
		Participants: parts, Previous: prev,
	}, nil
}

// previousInTx rend les messages précédents par séquence croissante, ce dont
// dépend memory.BuildUnit pour ne pas construire un contexte inversé.
//
// Filtre deleted_at IS NULL comme Around, pour que les deux chemins qui
// alimentent l'expansion de contexte soient d'accord. memory.BuildUnit (tâche
// 5) filtre déjà DeletedAt != nil de son côté: la redondance est volontaire,
// pour qu'un message supprimé ne puisse jamais atteindre le texte
// d'embedding même si ce filtre aval venait à disparaître dans un futur
// refactor.
func previousInTx(ctx context.Context, tx pgx.Tx, conv string,
	before int64, limit int) ([]memory.Message, error) {

	if limit <= 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT `+messageColumns+` FROM messages
		WHERE conversation_id = $1 AND sequence_number < $2 AND deleted_at IS NULL
		ORDER BY sequence_number DESC
		LIMIT $3`, conv, before, limit)
	if err != nil {
		return nil, fmt.Errorf("query previous: %w", err)
	}
	defer rows.Close()

	var desc []memory.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan previous: %w", err)
		}
		desc = append(desc, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
		desc[i], desc[j] = desc[j], desc[i]
	}
	return desc, nil
}

// querier est l'interface minimale commune à pgx.Tx et *pgxpool.Pool pour les
// lectures partagées entre code transactionnel et hors transaction: les deux
// types la satisfont déjà, sans adaptation. Elle évite de dupliquer la même
// requête et la même boucle de scan une fois par contexte d'appel.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// participants rend les participants actifs d'une conversation. Partagée
// entre l'intérieur d'une transaction (via un pgx.Tx) et l'appel hors
// transaction de MessageRepo.Participants (via le *pgxpool.Pool), pour que
// les deux chemins ne puissent pas diverger le jour où ce filtre change.
func participants(ctx context.Context, q querier, conv string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT participant_key FROM conversation_participants
		WHERE conversation_id = $1 AND left_at IS NULL
		ORDER BY joined_at, participant_key`, conv)
	if err != nil {
		return nil, fmt.Errorf("query participants: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Participants rend les participants actifs d'une conversation, hors
// transaction.
func (r *MessageRepo) Participants(ctx context.Context, conv string) ([]string, error) {
	return participants(ctx, r.pool, conv)
}

// ConversationScope rend le scope déclaré d'une conversation. C'est ce que
// le worker d'embedding utilise pour construire l'unité avec le scope réel
// de la conversation plutôt qu'avec le scope par défaut de la
// configuration, cohérent avec ce que fait déjà Append pour l'indexation en
// ligne.
func (r *MessageRepo) ConversationScope(ctx context.Context, conv string) (string, error) {
	var scope string
	err := r.pool.QueryRow(ctx,
		`SELECT scope FROM conversations WHERE conversation_id = $1`, conv).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("conversation scope: %w: %w",
			ErrNoConversation, memory.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("select conversation scope: %w", err)
	}
	return scope, nil
}

// MessageOwner rend le workspace et l'auteur d'un message, sans en charger
// le contenu ni ses métadonnées. C'est ce que les handlers PATCH/DELETE
// appellent avant toute modification, pour comparer le workspace et
// l'auteur du message à ce que le principal a le droit de toucher avant
// d'écrire quoi que ce soit.
//
// Rend memory.ErrNotFound sur une ligne absente: c'est la sentinelle que le
// port memory.MessageRepo documente, la même que celle rendue par toutes les
// autres méthodes de ce paquet, pour que la couche HTTP n'ait qu'une seule
// sentinelle à vérifier quel que soit le point d'entrée.
func (r *MessageRepo) MessageOwner(ctx context.Context, id uuid.UUID) (string, string, error) {
	var workspaceID, authorKey string
	err := r.pool.QueryRow(ctx,
		`SELECT workspace_id, author_key FROM messages WHERE message_id = $1`,
		id).Scan(&workspaceID, &authorKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", memory.ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("select message owner: %w", err)
	}
	return workspaceID, authorKey, nil
}

// ByID rend un message par son identifiant. Une ligne absente rend
// memory.ErrNotFound, comme partout ailleurs dans ce paquet (voir la
// convention décrite sur ErrNoConversation).
func (r *MessageRepo) ByID(ctx context.Context, id uuid.UUID) (memory.Message, error) {
	m, err := scanMessage(r.pool.QueryRow(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE message_id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.Message{}, fmt.Errorf("select message: %w", memory.ErrNotFound)
	}
	if err != nil {
		return memory.Message{}, fmt.Errorf("select message: %w", err)
	}
	return m, nil
}

// Around rend les messages non supprimés d'un intervalle de séquence, pour
// l'expansion de contexte au moment de la recherche.
func (r *MessageRepo) Around(ctx context.Context, conv string,
	from, to int64) ([]memory.Message, error) {

	rows, err := r.pool.Query(ctx, `
		SELECT `+messageColumns+` FROM messages
		WHERE conversation_id = $1
		  AND sequence_number BETWEEN $2 AND $3
		  AND deleted_at IS NULL
		ORDER BY sequence_number`, conv, from, to)
	if err != nil {
		return nil, fmt.Errorf("query around: %w", err)
	}
	defer rows.Close()
	var out []memory.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan around: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate around: %w", err)
	}
	return out, nil
}

// testHookAfterEditBeforeDeactivate, quand il est non nil, s'exécute juste
// après la mise à jour du contenu et avant la désactivation des unités, à
// l'intérieur de la transaction d'EditAndDeactivate. Réservé aux tests: en
// rendant une erreur, il force le rollback de la transaction entière, ce qui
// permet de vérifier que les deux effets (contenu modifié, unités
// désactivées) sont bien atomiques plutôt que déjà commit séparément. En
// production il reste nil et ne coûte qu'une comparaison. Voir
// TestEditAndDeactivateRollsBackContentOnDeactivateFailure.
var testHookAfterEditBeforeDeactivate func() error

// EditAndDeactivate et SoftDeleteAndDeactivate existent ensemble et pour la
// même raison, chacune dans sa propre transaction: modifier le contenu (ou
// supprimer) puis désactiver les unités couvrantes en deux appels séparés
// laisserait, sur une panne entre les deux, une unité active citer un texte
// qui ne correspond plus au message (memory_units, via deactivateCovering,
// la même requête que UnitRepo.DeactivateCovering). Pour l'édition c'est
// même pire que pour la suppression: le message affiche déjà le nouveau
// texte pendant qu'une unité continue d'en citer un autre, une contradiction
// du dossier plutôt qu'une simple péremption. Ne les séparez pas à nouveau
// en deux appels pour "simplifier": c'est cette atomicité qui est le
// correctif. EditMessage et DeleteMessage s'en servent respectivement,
// plutôt que d'enchaîner Edit/SoftDelete puis UnitRepo.DeactivateCovering.
func (r *MessageRepo) EditAndDeactivate(ctx context.Context, id uuid.UUID,
	content string) (memory.Message, []uuid.UUID, error) {

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return memory.Message{}, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	m, err := scanMessage(tx.QueryRow(ctx, `
		UPDATE messages SET content = $2, edited_at = now()
		WHERE message_id = $1 AND deleted_at IS NULL
		RETURNING `+messageColumns, id, content))
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.Message{}, nil, memory.ErrNotFound
	}
	if err != nil {
		return memory.Message{}, nil, fmt.Errorf("update message: %w", err)
	}

	if testHookAfterEditBeforeDeactivate != nil {
		if err := testHookAfterEditBeforeDeactivate(); err != nil {
			return memory.Message{}, nil, err
		}
	}

	anchors, err := deactivateCovering(ctx, tx, m.ConversationID, m.SequenceNumber)
	if err != nil {
		return memory.Message{}, nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return memory.Message{}, nil, fmt.Errorf("commit: %w", err)
	}
	return m, anchors, nil
}

func (r *MessageRepo) SoftDeleteAndDeactivate(ctx context.Context,
	id uuid.UUID) (memory.Message, []uuid.UUID, error) {

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return memory.Message{}, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	m, err := scanMessage(tx.QueryRow(ctx, `
		UPDATE messages SET deleted_at = now()
		WHERE message_id = $1 AND deleted_at IS NULL
		RETURNING `+messageColumns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.Message{}, nil, memory.ErrNotFound
	}
	if err != nil {
		return memory.Message{}, nil, fmt.Errorf("soft delete message: %w", err)
	}

	anchors, err := deactivateCovering(ctx, tx, m.ConversationID, m.SequenceNumber)
	if err != nil {
		return memory.Message{}, nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return memory.Message{}, nil, fmt.Errorf("commit: %w", err)
	}
	return m, anchors, nil
}
