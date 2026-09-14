package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/graph"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// TestConversationRepoDeclareRefusesForeignWorkspace couvre le second verrou
// du contournement de lecture: même si la couche HTTP laissait passer un
// Declare sur une conversation d'un autre workspace (une course entre le
// chargement du contexte et l'écriture, par exemple), le DO UPDATE filtré sur
// workspace_id ne doit ni changer le scope ni inscrire de participant.
func TestConversationRepoDeclareRefusesForeignWorkspace(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	convs := NewConversationRepo(pool)

	if err := convs.Declare(ctx, "conv_1", "ws1", "private",
		[]string{"user:alice"}); err != nil {
		t.Fatal(err)
	}

	err := convs.Declare(ctx, "conv_1", "ws_du_voisin", "workspace",
		[]string{"agent:bot"})
	if !errors.Is(err, memory.ErrWorkspaceMismatch) {
		t.Fatalf("err = %v, want memory.ErrWorkspaceMismatch", err)
	}

	var scope, workspace string
	if err := pool.QueryRow(ctx,
		`SELECT scope, workspace_id FROM conversations WHERE conversation_id = 'conv_1'`,
	).Scan(&scope, &workspace); err != nil {
		t.Fatal(err)
	}
	if scope != "private" || workspace != "ws1" {
		t.Errorf("scope = %q, workspace = %q, want private/ws1", scope, workspace)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversation_participants
		 WHERE conversation_id = 'conv_1' AND participant_key = 'agent:bot'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("un participant étranger a été inscrit: %d", n)
	}
}

// TestConversationRepoDeclareUpdatesScopeInSameWorkspace garde la séquence
// d'amorçage du README vivante au niveau SQL: re-déclarer dans le même
// workspace change bien le scope.
func TestConversationRepoDeclareUpdatesScopeInSameWorkspace(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	convs := NewConversationRepo(pool)

	if err := convs.Declare(ctx, "conv_1", "ws1", "participants",
		[]string{"user:paul"}); err != nil {
		t.Fatal(err)
	}
	if err := convs.Declare(ctx, "conv_1", "ws1", "explicit", nil); err != nil {
		t.Fatalf("re-déclaration dans le même workspace: %v", err)
	}

	var scope string
	if err := pool.QueryRow(ctx,
		`SELECT scope FROM conversations WHERE conversation_id = 'conv_1'`,
	).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != "explicit" {
		t.Errorf("scope = %q, want explicit", scope)
	}
}

// TestConversationRepoDeclareRefusesDeletedConversation ferme le trou que la
// re-revue de la vague de correctifs a trouvé: Context, que la couche HTTP
// appelle avant Declare, écarte les conversations supprimées, donc une
// conversation soft-deleted lui rend ErrNotFound et le handler la traite
// comme une création à autoriser librement. Sans le prédicat deleted_at du
// DO UPDATE, n'importe quel token du workspace ressuscitait alors la ligne,
// en réécrivait le scope et s'y inscrivait comme participant.
func TestConversationRepoDeclareRefusesDeletedConversation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	convs := NewConversationRepo(pool)

	if err := convs.Declare(ctx, "conv_1", "ws1", "private",
		[]string{"user:alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := convs.SoftDelete(ctx, "conv_1", "ws1"); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	err := convs.Declare(ctx, "conv_1", "ws1", "workspace",
		[]string{"agent:bot"})
	if !errors.Is(err, memory.ErrWorkspaceMismatch) {
		t.Fatalf("err = %v, want memory.ErrWorkspaceMismatch", err)
	}

	var (
		scope   string
		deleted *time.Time
		n       int
	)
	if err := pool.QueryRow(ctx, `
		SELECT c.scope, c.deleted_at,
		       (SELECT count(*) FROM conversation_participants p
		        WHERE p.conversation_id = c.conversation_id
		          AND p.participant_key = 'agent:bot')
		FROM conversations c WHERE c.conversation_id = 'conv_1'`,
	).Scan(&scope, &deleted, &n); err != nil {
		t.Fatal(err)
	}
	if scope != "private" {
		t.Errorf("scope = %q, want private: une conversation supprimée n'est pas redéclarable", scope)
	}
	if deleted == nil {
		t.Error("la conversation a été ressuscitée")
	}
	if n != 0 {
		t.Errorf("un participant a été inscrit dans une conversation supprimée: %d", n)
	}
}

// TestConversationRepoSoftDeletePoseUnGraphReevalParMessageSourcant couvre le
// constat F4 de la revue finale de branche.
//
// SoftDelete marque la conversation, marque tous ses messages et désactive
// toutes ses unités, dans une transaction, et ne posait aucun job. Les
// relations sourcées uniquement par les messages de cette conversation ne
// prenaient donc jamais d'invalidated_at, et un valid_until posé par l'une
// d'elles restait en place: l'invariant « une relation dont plus aucune source
// n'est vivante est invalidée » était violé pour tout le chemin de la
// suppression de conversation, et aucun balayage ne repassait, Reevaluate
// n'étant appelée que par un job.
//
// La mise en file est bornée et précise: un job par message de la conversation
// qui source réellement une relation, et aucun pour une conversation qui n'a
// rien alimenté. C'est ce que les deux moitiés de ce test vérifient.
func TestConversationRepoSoftDeletePoseUnGraphReevalParMessageSourcant(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	convs := NewConversationRepo(pool)
	gr := NewGraphRepo(pool)

	// conv_source a trois messages, dont deux seulement sourcent une
	// relation. conv_muette n'alimente rien.
	sources := seedConversation(t, pool, "ws1", "conv_source", "participants", 3)
	seedConversation(t, pool, "ws1", "conv_muette", "participants", 2)

	src := graph.EntityID("ws1", "person:paul")
	rel := func(literal string, msgs ...uuid.UUID) memory.GraphRelation {
		dk := graph.DedupKey(src, "likes", nil, literal, nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "likes", TargetLiteral: literal,
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv_source", SourceMessageIDs: msgs,
		}
	}
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv_source",
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul", Resolved: true}},
		Relations: []memory.GraphRelation{
			rel("vert", sources[0]),
			rel("rouge", sources[1]),
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := convs.SoftDelete(ctx, "conv_muette", "ws1"); err != nil {
		t.Fatal(err)
	}
	if n := graphReevalJobs(t, pool, "conv_muette"); n != 0 {
		t.Errorf("%d jobs graph_reeval pour une conversation qui n'a sourcé "+
			"aucune relation, want 0", n)
	}

	if _, err := convs.SoftDelete(ctx, "conv_source", "ws1"); err != nil {
		t.Fatal(err)
	}
	posted := graphReevalMessages(t, pool, "conv_source")
	if len(posted) != 2 {
		t.Fatalf("%d jobs graph_reeval, want 2 (un par message sourçant)",
			len(posted))
	}
	for _, want := range []uuid.UUID{sources[0], sources[1]} {
		if !posted[want] {
			t.Errorf("aucun graph_reeval pour le message %s, qui source une "+
				"relation", want)
		}
	}
	if posted[sources[2]] {
		t.Error("un graph_reeval a été posé pour un message qui ne source rien")
	}

	// Un second appel ne doit rien reposer: la conversation est déjà
	// supprimée, donc SoftDelete rend 0 sans rien toucher.
	if _, err := convs.SoftDelete(ctx, "conv_source", "ws1"); err != nil {
		t.Fatal(err)
	}
	if n := graphReevalJobs(t, pool, "conv_source"); n != 2 {
		t.Errorf("%d jobs après un second appel, want 2: la suppression est "+
			"rejouable sans reposer de job", n)
	}
}

// graphReevalJobs compte les jobs graph_reeval en file pour une conversation.
func graphReevalJobs(t *testing.T, pool *pgxpool.Pool, conv string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs
		 WHERE job_type = 'graph_reeval' AND conversation_id = $1`,
		conv).Scan(&n); err != nil {
		t.Fatalf("compte des jobs: %v", err)
	}
	return n
}

// graphReevalMessages rend l'ensemble des message_id portés par les jobs
// graph_reeval d'une conversation.
func graphReevalMessages(t *testing.T, pool *pgxpool.Pool, conv string) map[uuid.UUID]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT payload->>'message_id' FROM jobs
		 WHERE job_type = 'graph_reeval' AND conversation_id = $1`, conv)
	if err != nil {
		t.Fatalf("lecture des jobs: %v", err)
	}
	defer rows.Close()

	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan du payload: %v", err)
		}
		id, err := uuid.Parse(s)
		if err != nil {
			t.Fatalf("payload sans message_id exploitable (%q): %v", s, err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("lecture des jobs: %v", err)
	}
	return out
}
