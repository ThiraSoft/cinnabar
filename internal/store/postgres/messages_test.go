package postgres

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/google/uuid"
)

func appendInput(conv, author, role, content, requestID string) memory.AppendInput {
	return memory.AppendInput{
		WorkspaceID:    "ws1",
		ConversationID: conv,
		DefaultScope:   "participants",
		AuthorKey:      author,
		Role:           role,
		Content:        content,
		CreatedAt:      time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC),
		RequestID:      requestID,
	}
}

func TestAppendCreatesConversationAndParticipant(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	res, err := repo.Append(ctx, appendInput("conv_1", "user:paul", "user", "salut", ""), 2, nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if res.Message.SequenceNumber != 1 {
		t.Errorf("sequence = %d, want 1", res.Message.SequenceNumber)
	}
	if res.Message.MessageID.Version() != 7 {
		t.Errorf("message_id doit être un UUIDv7, got v%d", res.Message.MessageID.Version())
	}
	if res.Scope != "participants" {
		t.Errorf("scope = %q", res.Scope)
	}
	if len(res.Participants) != 1 || res.Participants[0] != "user:paul" {
		t.Errorf("participants = %v", res.Participants)
	}
	if res.Replayed {
		t.Error("premier envoi, Replayed doit être faux")
	}
}

func TestAppendAccumulatesParticipants(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	if _, err := repo.Append(ctx, appendInput("conv_1", "user:paul", "user", "a", ""), 2, nil); err != nil {
		t.Fatal(err)
	}
	res, err := repo.Append(ctx,
		appendInput("conv_1", "agent:cuisine", "assistant", "b", ""), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Participants) != 2 {
		t.Errorf("participants = %v, want 2 entrées", res.Participants)
	}
}

func TestAppendAssignsMonotonicSequence(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	for want := int64(1); want <= 5; want++ {
		res, err := repo.Append(ctx, appendInput("conv_1", "user:paul", "user", "x", ""), 2, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Message.SequenceNumber != want {
			t.Fatalf("sequence = %d, want %d", res.Message.SequenceNumber, want)
		}
	}
}

// Critère d'acceptation 5: le rejeu du même événement ne crée pas de doublon.
func TestAcceptance5_ReplayDoesNotDuplicate(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	first, err := repo.Append(ctx,
		appendInput("conv_1", "user:paul", "user", "les tomates", "evt-123"), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Append(ctx,
		appendInput("conv_1", "user:paul", "user", "les tomates", "evt-123"), 2, nil)
	if err != nil {
		t.Fatalf("le rejeu doit réussir, pas échouer: %v", err)
	}
	if !second.Replayed {
		t.Error("Replayed doit être vrai au rejeu")
	}
	if second.Message.MessageID != first.Message.MessageID {
		t.Error("le rejeu doit rendre le même message_id")
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d messages en base, want 1", n)
	}
	// Un rejeu ne doit pas consommer un numéro de séquence.
	var next int64
	if err := pool.QueryRow(ctx,
		`SELECT next_sequence FROM conversations WHERE conversation_id = 'conv_1'`,
	).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != 1 {
		t.Errorf("next_sequence = %d, want 1: un rejeu ne consomme pas de séquence", next)
	}
}

func TestAppendConcurrentSameIdempotencyKey(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := repo.Append(ctx,
				appendInput("conv_1", "user:paul", "user", "concurrent", "evt-same"), 2, nil)
			errs[i] = err
			if err == nil {
				ids[i] = res.Message.MessageID.String()
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("appel %d: %v, la course sur la clé d'idempotence doit être résolue en interne", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("message_id divergents: %q vs %q", ids[i], ids[0])
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d messages, want 1", count)
	}
}

func TestAppendConcurrentDistinctKeysKeepsSequenceUnique(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = repo.Append(ctx,
				appendInput("conv_1", "user:paul", "user", "x", ""), 2, nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("appel %d: %v", i, err)
		}
	}
	var distinct int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT sequence_number) FROM messages`).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != n {
		t.Errorf("%d séquences distinctes pour %d messages", distinct, n)
	}
}

func TestAppendReturnsPreviousMessagesInOrder(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	for _, c := range []string{"un", "deux", "trois"} {
		if _, err := repo.Append(ctx,
			appendInput("conv_1", "user:paul", "user", c, ""), 2, nil); err != nil {
			t.Fatal(err)
		}
	}
	res, err := repo.Append(ctx, appendInput("conv_1", "user:paul", "user", "quatre", ""), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Previous) != 2 {
		t.Fatalf("Previous = %d messages, want 2", len(res.Previous))
	}
	if res.Previous[0].Content != "deux" || res.Previous[1].Content != "trois" {
		t.Errorf("Previous doit être croissant: %q, %q",
			res.Previous[0].Content, res.Previous[1].Content)
	}
}

// TestAppendPreviousExcludesSoftDeletedMessages couvre previousInTx: il doit
// filtrer deleted_at IS NULL comme Around, pour que les deux chemins qui
// alimentent l'expansion de contexte soient d'accord (voir le commentaire
// sur previousInTx).
func TestAppendPreviousExcludesSoftDeletedMessages(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	var toDelete uuid.UUID
	for _, c := range []string{"un", "deux", "trois"} {
		res, err := repo.Append(ctx,
			appendInput("conv_1", "user:paul", "user", c, ""), 3, nil)
		if err != nil {
			t.Fatal(err)
		}
		if c == "deux" {
			toDelete = res.Message.MessageID
		}
	}
	if _, _, err := repo.SoftDeleteAndDeactivate(ctx, toDelete); err != nil {
		t.Fatal(err)
	}

	res, err := repo.Append(ctx, appendInput("conv_1", "user:paul", "user", "quatre", ""), 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Previous) != 2 {
		t.Fatalf("Previous = %d messages, want 2: le message supprimé doit être exclu", len(res.Previous))
	}
	if res.Previous[0].Content != "un" || res.Previous[1].Content != "trois" {
		t.Errorf("Previous = %q, %q, want \"un\", \"trois\" (\"deux\" est supprimé)",
			res.Previous[0].Content, res.Previous[1].Content)
	}
}

func TestAppendRespectsExplicitConversationScope(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	// Conversation créée d'avance avec un scope non par défaut: Append ne doit
	// pas l'écraser.
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id, scope)
		VALUES ('conv_1', 'ws1', 'workspace')`); err != nil {
		t.Fatal(err)
	}
	res, err := repo.Append(ctx, appendInput("conv_1", "user:paul", "user", "x", ""), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scope != "workspace" {
		t.Errorf("scope = %q, want workspace: Append ne doit pas écraser un scope déclaré", res.Scope)
	}
}

// TestAppendConflictPathResolvesToSameMessage force, via
// testHookAfterIdempotencyCheck, l'entrelacement que la sérialisation
// naturelle sur la ligne conversations rend presque impossible à obtenir par
// le seul hasard des goroutines (voir TestAppendConcurrentSameIdempotencyKey
// et le rapport de la tâche 6): un premier appel passe sa vérification
// d'idempotence, se retrouve bloqué avant l'incrément de séquence, pendant
// qu'un second appel avec la même clé va jusqu'au bout et committe. Le
// premier, une fois débloqué, doit alors tomber sur la clause ON CONFLICT de
// l'insertion des messages plutôt que d'y échapper. Supprimer la gestion de
// ON CONFLICT sur l'insertion doit faire échouer ce test.
func TestAppendConflictPathResolvesToSameMessage(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	// La conversation et son participant sont créés d'avance et committés:
	// sinon la propre étape 1 (INSERT ... ON CONFLICT DO NOTHING) du premier
	// appel, elle-même non committée tant qu'il est bloqué sur le hook,
	// ferait attendre l'étape 1 du second appel jusqu'à la fin de la
	// transaction du premier, un blocage différent de celui qu'on veut
	// provoquer ici et qui interbloquerait le test (le second n'avancerait
	// jamais, donc ne débloquerait jamais le premier).
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id, scope)
		VALUES ('conv_1', 'ws1', 'participants')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversation_participants (conversation_id, participant_key)
		VALUES ('conv_1', 'user:paul')`); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	// Si ce test échoue avant l'appel explicite ci-dessous (par exemple si
	// t.Fatalf coupe la goroutine du test sur l'erreur du second appel), le
	// premier appel resterait sinon bloqué pour toujours sur <-release,
	// tenant une transaction et une connexion du pool ouvertes. pgxpool.Pool
	// Close (posé en t.Cleanup par newTestPool) attend que chaque connexion
	// revienne, donc un échec d'assertion lisible se transformerait en
	// blocage de tout le paquet à la fermeture. defer avec un sync.Once
	// garantit que le premier appel est toujours débloqué exactement une
	// fois, y compris sur ce chemin d'échec.
	defer releaseFirst()

	firstReached := make(chan struct{})
	var once sync.Once
	testHookAfterIdempotencyCheck = func() {
		isFirst := false
		once.Do(func() { isFirst = true })
		if isFirst {
			close(firstReached)
			<-release
		}
	}
	t.Cleanup(func() { testHookAfterIdempotencyCheck = nil })

	type outcome struct {
		res memory.AppendResult
		err error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		res, err := repo.Append(ctx,
			appendInput("conv_1", "user:paul", "user", "premier", "evt-race"), 2, nil)
		firstDone <- outcome{res, err}
	}()

	<-firstReached // le premier appel est bloqué juste après sa vérification d'idempotence

	second, err := repo.Append(ctx,
		appendInput("conv_1", "user:paul", "user", "second", "evt-race"), 2, nil)
	if err != nil {
		t.Fatalf("second Append: %v", err)
	}

	releaseFirst() // débloque le premier, qui doit maintenant rencontrer le conflit

	first := <-firstDone
	if first.err != nil {
		t.Fatalf("first Append: %v, le conflit doit être résolu en interne, pas remonté en erreur", first.err)
	}

	if first.res.Message.MessageID != second.Message.MessageID {
		t.Fatalf("message_id divergents: %q vs %q",
			first.res.Message.MessageID, second.Message.MessageID)
	}
	if !first.res.Replayed {
		t.Error("le premier appel doit avoir emprunté le chemin ON CONFLICT et rendre Replayed=true")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d messages en base, want 1", count)
	}

	var next int64
	if err := pool.QueryRow(ctx,
		`SELECT next_sequence FROM conversations WHERE conversation_id = 'conv_1'`,
	).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != 1 {
		t.Errorf("next_sequence = %d, want 1: un seul numéro de séquence doit être consommé", next)
	}
}

// TestAppendRejectsConversationFromAnotherWorkspace couvre une fuite
// cross-workspace: conversation_id est un TEXT libre choisi par le client, et
// conversations est indexée sur conversation_id seul. Un appelant légitime de
// ws2 qui devine ou réutilise un conversation_id déjà pris par ws1 ne doit ni
// pouvoir écrire dans cette conversation, ni recevoir en retour les
// participants ou les messages précédents de ws1.
func TestAppendRejectsConversationFromAnotherWorkspace(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	if _, err := repo.Append(ctx,
		appendInput("conv_1", "user:paul", "user", "secret ws1", ""), 2, nil); err != nil {
		t.Fatal(err)
	}

	in := appendInput("conv_1", "user:eve", "user", "intrusion", "")
	in.WorkspaceID = "ws2"
	res, err := repo.Append(ctx, in, 2, nil)
	if !errors.Is(err, memory.ErrWorkspaceMismatch) {
		t.Fatalf("err = %v, want memory.ErrWorkspaceMismatch", err)
	}
	if !reflect.DeepEqual(res, memory.AppendResult{}) {
		t.Errorf("AppendResult = %+v, want zero value: la fuite serait de rendre les participants ou les messages précédents de ws1", res)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d messages en base, want 1: aucune ligne ne doit avoir été ajoutée", count)
	}

	var next int64
	if err := pool.QueryRow(ctx,
		`SELECT next_sequence FROM conversations WHERE conversation_id = 'conv_1'`,
	).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != 1 {
		t.Errorf("next_sequence = %d, want 1: la tentative refusée ne doit pas consommer de séquence", next)
	}
}

// TestEditAndDeactivateRollsBackContentOnDeactivateFailure couvre
// l'atomicité corrigée par la revue de la tâche 15: EditAndDeactivate fait
// la mise à jour du contenu et la désactivation des unités dans une seule
// transaction, précisément pour qu'une panne entre les deux ne puisse pas
// laisser le contenu déjà modifié pendant qu'une unité reste active à citer
// l'ancien texte. testHookAfterEditBeforeDeactivate force cette panne juste
// après l'UPDATE et avant la désactivation, à l'intérieur de la
// transaction: si l'atomicité n'était qu'apparente (deux appels séparés,
// par exemple), le contenu resterait modifié malgré l'erreur rendue par
// EditAndDeactivate. C'est la deuxième ligne de défense: la première est la
// contrainte d'unicité de memory_units, qui empêche par ailleurs qu'Upsert
// crée une ligne neuve à côté d'une existante (voir le commentaire du test
// de réactivation dans lifecycle_test.go) — deux mécanismes indépendants
// valent mieux qu'un seul, et celle-ci resterait la seule à détecter une
// régression si la contrainte SQL venait à être assouplie.
func TestEditAndDeactivateRollsBackContentOnDeactivateFailure(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	m := seedMessage(t, repo, "conv_1", "contenu original")

	testHookAfterEditBeforeDeactivate = func() error {
		return errors.New("panne simulée entre la mise à jour du contenu et la désactivation")
	}
	t.Cleanup(func() { testHookAfterEditBeforeDeactivate = nil })

	if _, _, err := repo.EditAndDeactivate(ctx, m.MessageID, "contenu modifié"); err == nil {
		t.Fatal("EditAndDeactivate doit remonter l'erreur du hook")
	}

	// Le rollback doit avoir annulé l'UPDATE de contenu: relire le message
	// (hors de toute transaction, avec le hook désarmé par le Cleanup à ce
	// stade n'a pas d'importance ici puisque ByID ne le déclenche pas) doit
	// rendre le contenu original, pas le contenu modifié.
	reread, err := repo.ByID(ctx, m.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if reread.Content != "contenu original" {
		t.Errorf("content = %q après l'échec de la désactivation, want %q (rollback attendu)",
			reread.Content, "contenu original")
	}
	if reread.EditedAt != nil {
		t.Error("edited_at ne doit pas non plus être resté modifié après le rollback")
	}
}

// TestAppendPostsJobsInTheSameTransaction couvre l'étape 6 de la section 6.1
// de la spec: les jobs entrent dans la transaction qui écrit le message. Tant
// qu'ils étaient posés après le commit, sur une autre connexion, une panne
// entre les deux laissait un message durable que plus rien ne viendrait
// jamais indexer.
func TestAppendPostsJobsInTheSameTransaction(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	res, err := repo.Append(ctx,
		appendInput("conv_1", "user:paul", "user", "salut", ""), 2,
		[]memory.AppendJob{{Type: "embed"}, {Type: "graph_extract"}})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx, `
		SELECT job_type, workspace_id, conversation_id, payload->>'message_id'
		FROM jobs ORDER BY job_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var jobType, ws, conv, msgID string
		if err := rows.Scan(&jobType, &ws, &conv, &msgID); err != nil {
			t.Fatal(err)
		}
		if msgID != res.Message.MessageID.String() {
			t.Errorf("payload message_id = %q, want %q", msgID,
				res.Message.MessageID)
		}
		if ws != res.Message.WorkspaceID || conv != "conv_1" {
			t.Errorf("job = %s/%s, want %s/conv_1", ws, conv, res.Message.WorkspaceID)
		}
		got = append(got, jobType)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "embed" || got[1] != "graph_extract" {
		t.Errorf("jobs = %v, want [embed graph_extract]", got)
	}
}

// TestAppendRollsBackTheMessageWhenAJobCannotBePosted est le test qui pince
// vraiment l'atomicité de la section 6.1, là où
// TestAppendPostsJobsInTheSameTransaction ne pince que la présence des jobs:
// celui-ci reste vert que l'insertion des jobs soit dans la transaction du
// message ou juste après son commit, puisqu'il ne regarde l'état qu'une fois
// tout terminé.
//
// La propriété qui distingue les deux montages est celle-ci: si la pose d'un
// job échoue, le message ne doit pas exister. On la provoque avec un type de
// job que le CHECK de la table jobs refuse. Avec l'insertion après le commit,
// le message serait durable et n'aurait plus jamais de job pour l'indexer,
// ce qui est exactement le défaut que la vague de correctifs a fermé.
func TestAppendRollsBackTheMessageWhenAJobCannotBePosted(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	_, err := repo.Append(ctx,
		appendInput("conv_1", "user:paul", "user", "salut", ""), 2,
		[]memory.AppendJob{{Type: "embed"}, {Type: "type_refuse_par_le_check"}})
	if err == nil {
		t.Fatal("want une erreur: le CHECK de jobs refuse ce type")
	}

	var messages, jobs int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM messages WHERE conversation_id = 'conv_1'),
		       (SELECT count(*) FROM jobs)`).Scan(&messages, &jobs); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Errorf("%d messages en base, want 0: le message doit disparaître avec ses jobs", messages)
	}
	if jobs != 0 {
		t.Errorf("%d jobs en base, want 0", jobs)
	}
}

// TestAppendPostsNoJobOnReplay: un rejeu n'écrit aucun message neuf, donc il
// ne doit poser aucun job. L'unité existe déjà et son identifiant est
// déterministe.
func TestAppendPostsNoJobOnReplay(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	in := appendInput("conv_1", "user:paul", "user", "salut", "evt-1")
	if _, err := repo.Append(ctx, in, 2,
		[]memory.AppendJob{{Type: "embed"}}); err != nil {
		t.Fatal(err)
	}
	res, err := repo.Append(ctx, in, 2, []memory.AppendJob{{Type: "embed"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed {
		t.Fatal("le second Append doit être un rejeu")
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d jobs en base, want 1: un rejeu n'en pose aucun", n)
	}
}
