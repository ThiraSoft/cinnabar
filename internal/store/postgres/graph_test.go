package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/graph"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

func TestUpsertEntitiesFusionneLesAliasSansDupliquerLaLigne(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)

	key := "person:paul"
	id := graph.EntityID("ws1", key)

	first := memory.GraphEntity{
		EntityID: id, WorkspaceID: "ws1", CanonicalKey: key,
		EntityType: "person", DisplayName: "Paul Dupont",
		Aliases: []string{"Paul"}, Resolved: true,
	}
	second := memory.GraphEntity{
		EntityID: id, WorkspaceID: "ws1", CanonicalKey: key,
		EntityType: "person", DisplayName: "paul dupont",
		Aliases: []string{"Dupont", "Paul"}, Resolved: false,
	}

	if err := repo.UpsertEntities(ctx, []memory.GraphEntity{first}); err != nil {
		t.Fatalf("premier upsert: %v", err)
	}
	if err := repo.UpsertEntities(ctx, []memory.GraphEntity{second}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var (
		count       int
		displayName string
		aliases     []string
		resolved    bool
	)
	if err := pool.QueryRow(ctx, `
		SELECT count(*) OVER (), display_name,
		       ARRAY(SELECT jsonb_array_elements_text(aliases) ORDER BY 1),
		       resolved
		FROM graph_entities WHERE workspace_id = 'ws1' AND canonical_key = $1`,
		key).Scan(&count, &displayName, &aliases, &resolved); err != nil {
		t.Fatalf("relecture: %v", err)
	}

	if count != 1 {
		t.Errorf("%d lignes, want 1", count)
	}
	if displayName != "Paul Dupont" {
		t.Errorf("display_name = %q, want %q (jamais écrasé)", displayName, "Paul Dupont")
	}
	if len(aliases) != 3 {
		t.Errorf("aliases = %v, want 3 valeurs distinctes dont l'ancienne graphie", aliases)
	}
	if !resolved {
		t.Error("resolved est repassé à faux")
	}
}

func TestUpsertEntitiesIsoleLesWorkspaces(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)

	key := "person:paul"
	for _, ws := range []string{"ws1", "ws2"} {
		e := memory.GraphEntity{
			EntityID: graph.EntityID(ws, key), WorkspaceID: ws,
			CanonicalKey: key, EntityType: "person", DisplayName: "Paul",
			Resolved: true,
		}
		if err := repo.UpsertEntities(ctx, []memory.GraphEntity{e}); err != nil {
			t.Fatalf("upsert %s: %v", ws, err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM graph_entities WHERE canonical_key = $1`,
		key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d lignes, want 2: deux workspaces ne partagent pas une entité", n)
	}
}

func TestCandidateEntitiesTrouveParAliasEtParNomApproche(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)

	ents := []memory.GraphEntity{
		{EntityID: graph.EntityID("ws1", "person:paul-dupont"), WorkspaceID: "ws1",
			CanonicalKey: "person:paul-dupont", EntityType: "person",
			DisplayName: "Paul Dupont", Aliases: []string{"Dudu"}, Resolved: true},
		{EntityID: graph.EntityID("ws1", "plant:tomate"), WorkspaceID: "ws1",
			CanonicalKey: "plant:tomate", EntityType: "plant",
			DisplayName: "Tomate", Resolved: true},
		{EntityID: graph.EntityID("ws2", "person:paul-dupont"), WorkspaceID: "ws2",
			CanonicalKey: "person:paul-dupont", EntityType: "person",
			DisplayName: "Paul Dupont", Resolved: true},
	}
	if err := repo.UpsertEntities(ctx, ents); err != nil {
		t.Fatal(err)
	}

	got, err := repo.CandidateEntities(ctx, "ws1", "est-ce que Paul Dupon aime les tomates", 10)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range got {
		names[e.DisplayName] = true
		if e.WorkspaceID != "ws1" {
			t.Errorf("entité du workspace %q rendue", e.WorkspaceID)
		}
	}
	if !names["Paul Dupont"] {
		t.Error("le nom approché n'a pas été trouvé par trigramme")
	}
	if !names["Tomate"] {
		t.Error("Tomate n'a pas été trouvée")
	}
}

// seedConversation crée une conversation dans le scope demandé et y écrit
// n messages, puis rend leurs identifiants dans l'ordre des séquences.
// Elle passe par MessageRepo.Append plutôt que par du SQL direct, pour que
// les conversations et les participants soient créés exactement comme en
// production.
func seedConversation(t *testing.T, pool *pgxpool.Pool,
	workspace, conv, scope string, n int) []uuid.UUID {

	t.Helper()
	ctx := context.Background()
	msgs := NewMessageRepo(pool)

	var out []uuid.UUID
	for i := 0; i < n; i++ {
		res, err := msgs.Append(ctx, memory.AppendInput{
			WorkspaceID: workspace, ConversationID: conv, DefaultScope: scope,
			AuthorKey: "user:alice", Role: "user",
			Content: fmt.Sprintf("message %d", i),
		}, 2, nil)
		if err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
		out = append(out, res.Message.MessageID)
	}

	// DefaultScope ne s'applique qu'à la création. Un test qui veut un
	// scope précis sur une conversation déjà existante doit le poser
	// explicitement.
	if _, err := pool.Exec(ctx,
		`UPDATE conversations SET scope = $2 WHERE conversation_id = $1`,
		conv, scope); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	return out
}

// softDelete marque un message comme supprimé, comme le ferait la route de
// suppression.
func softDelete(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE messages SET deleted_at = now() WHERE message_id = $1`, id); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
}

func TestApplyDedupliqueLesRelationsEtCumuleLesSources(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)

	// seedConversation est le helper déjà utilisé par les tests
	// d'intégration du paquet: il crée la conversation, ses participants
	// et n messages, et rend leurs identifiants.
	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 2)

	ext := func(msg uuid.UUID) memory.GraphExtraction {
		src := graph.EntityID("ws1", "person:paul")
		dk := graph.DedupKey(src, "likes", nil, "green", nil)
		return memory.GraphExtraction{
			WorkspaceID: "ws1", ConversationID: "conv1",
			Entities: []memory.GraphEntity{{
				EntityID: src, WorkspaceID: "ws1", CanonicalKey: "person:paul",
				EntityType: "person", DisplayName: "Paul", Resolved: true,
			}},
			Relations: []memory.GraphRelation{{
				RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
				SourceEntityID: src, RelationType: "likes", TargetLiteral: "green",
				ObservedAt: time.Now().UTC(), Confidence: 0.6, Scope: "participants",
				DedupKey: dk, ConversationID: "conv1",
				SourceMessageIDs: []uuid.UUID{msg},
			}},
		}
	}

	if err := repo.Apply(ctx, ext(msgs[0]), nil); err != nil {
		t.Fatalf("premier apply: %v", err)
	}
	second := ext(msgs[1])
	second.Relations[0].Confidence = 0.9
	if err := repo.Apply(ctx, second, nil); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	var relations, sources int
	// float32 et non float64: la colonne confidence est un REAL, donc du
	// simple précision. Relue dans un float64, la valeur 0.9 revient en
	// 0.8999999761581421 et aucune comparaison exacte ne tiendrait. En
	// float32 la constante 0.9 est convertie dans la même précision que
	// celle où Postgres l'a stockée, et l'égalité redevient vraie.
	var confidence float32
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM graph_relations WHERE workspace_id = 'ws1'),
		       (SELECT count(*) FROM graph_relation_sources),
		       (SELECT confidence FROM graph_relations WHERE workspace_id = 'ws1')`).
		Scan(&relations, &sources, &confidence); err != nil {
		t.Fatal(err)
	}
	if relations != 1 {
		t.Errorf("%d relations, want 1", relations)
	}
	if sources != 2 {
		t.Errorf("%d sources, want 2", sources)
	}
	if confidence != 0.9 {
		t.Errorf("confidence = %v, want 0.9 (la plus élevée est gardée)", confidence)
	}
}

func TestApplyFermeLObservationPrecedentePourUnTypeAValeurUnique(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)
	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 2)

	src := graph.EntityID("ws1", "plant:tomate")
	ent := memory.GraphEntity{
		EntityID: src, WorkspaceID: "ws1", CanonicalKey: "plant:tomate",
		EntityType: "plant", DisplayName: "Tomate", Resolved: true,
	}
	rel := func(state string, observed time.Time, msg uuid.UUID) memory.GraphRelation {
		dk := graph.DedupKey(src, "has_observed_state", nil, state, nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "has_observed_state",
			TargetLiteral: state, ObservedAt: observed, Confidence: 1,
			Scope: "participants", DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msg},
		}
	}

	t1 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	single := []string{"has_observed_state"}

	if err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{ent}, Relations: []memory.GraphRelation{rel("green", t1, msgs[0])},
	}, single); err != nil {
		t.Fatal(err)
	}
	if err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{ent}, Relations: []memory.GraphRelation{rel("red", t2, msgs[1])},
	}, single); err != nil {
		t.Fatal(err)
	}

	var greenUntil *time.Time
	var redUntil *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT valid_until FROM graph_relations WHERE target_literal = 'green'),
		       (SELECT valid_until FROM graph_relations WHERE target_literal = 'red')`).
		Scan(&greenUntil, &redUntil); err != nil {
		t.Fatal(err)
	}
	if greenUntil == nil || !greenUntil.Equal(t2) {
		t.Errorf("valid_until de green = %v, want %v", greenUntil, t2)
	}
	if redUntil != nil {
		t.Errorf("valid_until de red = %v, want nil", redUntil)
	}

	// Rien n'est supprimé: les deux observations restent lisibles.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM graph_relations WHERE workspace_id = 'ws1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d relations, want 2: l'historique doit être conservé", n)
	}
}

func TestReevaluateNInvalidePasTantQuUneSourceSurvit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)
	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 2)

	src := graph.EntityID("ws1", "person:paul")
	dk := graph.DedupKey(src, "likes", nil, "green", nil)
	if err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul", Resolved: true}},
		Relations: []memory.GraphRelation{{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "likes", TargetLiteral: "green",
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msgs[0], msgs[1]},
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	softDelete(t, pool, msgs[0])
	if err := repo.Reevaluate(ctx, msgs[0], nil); err != nil {
		t.Fatal(err)
	}
	var invalidated *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT invalidated_at FROM graph_relations`).Scan(&invalidated); err != nil {
		t.Fatal(err)
	}
	if invalidated != nil {
		t.Fatalf("relation invalidée alors qu'une source vit encore: %v", invalidated)
	}

	softDelete(t, pool, msgs[1])
	if err := repo.Reevaluate(ctx, msgs[1], nil); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT invalidated_at FROM graph_relations`).Scan(&invalidated); err != nil {
		t.Fatal(err)
	}
	if invalidated == nil {
		t.Error("relation non invalidée alors que toutes ses sources ont disparu")
	}
}

// Le cas que la section 7.5 vise nommément: c'est la plus récente qui
// disparaît, et la précédente doit se rouvrir plutôt que de rester fermée
// sur une observation qui n'existe plus.
func TestReevaluateRouvreLaFenetreQuandLaPlusRecenteDisparait(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)
	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 2)

	src := graph.EntityID("ws1", "plant:tomate")
	ent := memory.GraphEntity{EntityID: src, WorkspaceID: "ws1",
		CanonicalKey: "plant:tomate", EntityType: "plant",
		DisplayName: "Tomate", Resolved: true}
	rel := func(state string, observed time.Time, msg uuid.UUID) memory.GraphRelation {
		dk := graph.DedupKey(src, "has_observed_state", nil, state, nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "has_observed_state",
			TargetLiteral: state, ObservedAt: observed, Confidence: 1,
			Scope: "participants", DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msg},
		}
	}
	t1 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	single := []string{"has_observed_state"}

	for i, r := range []memory.GraphRelation{rel("green", t1, msgs[0]), rel("red", t2, msgs[1])} {
		if err := repo.Apply(ctx, memory.GraphExtraction{
			WorkspaceID: "ws1", ConversationID: "conv1",
			Entities: []memory.GraphEntity{ent}, Relations: []memory.GraphRelation{r},
		}, single); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}

	softDelete(t, pool, msgs[1])
	if err := repo.Reevaluate(ctx, msgs[1], single); err != nil {
		t.Fatal(err)
	}

	var greenUntil *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT valid_until FROM graph_relations WHERE target_literal = 'green'`).
		Scan(&greenUntil); err != nil {
		t.Fatal(err)
	}
	if greenUntil != nil {
		t.Errorf("valid_until de green = %v, want nil: la fenêtre doit se rouvrir", greenUntil)
	}
}

// TestCandidateEntitiesTrouveLesVariantesDeGraphieFrancaises couvre le
// constat de la revue de la tâche 4: le seuil par défaut de l'opérateur <%,
// 0,6, laissait tomber en silence les variantes de graphie les plus
// courantes du français. Une entité manquée dans le prompt n'est pas une
// erreur visible: le modèle recrée simplement une entité de plus, que le
// service ne fusionnera jamais de lui-même.
func TestCandidateEntitiesTrouveLesVariantesDeGraphieFrancaises(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)

	noms := []string{"Müller", "Élodie", "François"}
	var ents []memory.GraphEntity
	for _, n := range noms {
		key := graph.CanonicalKey("person", n)
		ents = append(ents, memory.GraphEntity{
			EntityID: graph.EntityID("ws1", key), WorkspaceID: "ws1",
			CanonicalKey: key, EntityType: "person", DisplayName: n,
			Resolved: true,
		})
	}
	if err := repo.UpsertEntities(ctx, ents); err != nil {
		t.Fatal(err)
	}

	cas := []struct{ texte, veut string }{
		{"est-ce que Muller a répondu hier", "Müller"},
		{"elodie a répondu hier soir", "Élodie"},
		{"Francois a parlé des tomates", "François"},
	}
	for _, c := range cas {
		got, err := repo.CandidateEntities(ctx, "ws1", c.texte, 10)
		if err != nil {
			t.Fatalf("%q: %v", c.texte, err)
		}
		trouve := false
		for _, e := range got {
			if e.DisplayName == c.veut {
				trouve = true
			}
		}
		if !trouve {
			t.Errorf("%q ne retrouve pas %q, candidats = %v", c.texte, c.veut, got)
		}
	}
}

// TestCandidateEntitiesEcarteUnNomSansRapport: le seuil descendu à 0,4 ne
// doit pas se mettre à tout laisser passer. Les vrais négatifs mesurent
// autour de 0,13, donc la marge est large, mais un test vaut mieux qu'une
// mesure faite une fois.
func TestCandidateEntitiesEcarteUnNomSansRapport(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)

	key := graph.CanonicalKey("person", "Marie Lefebvre")
	if err := repo.UpsertEntities(ctx, []memory.GraphEntity{{
		EntityID: graph.EntityID("ws1", key), WorkspaceID: "ws1",
		CanonicalKey: key, EntityType: "person", DisplayName: "Marie Lefebvre",
		Resolved: true,
	}}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.CandidateEntities(ctx, "ws1",
		"les tomates de Paul sont vertes ce matin", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("candidats = %v, want aucun", got)
	}
}

// TestApplyEstToutOuRien garde l'invariant que Apply existe pour porter:
// entités, relations et sources tombent ensemble ou pas du tout. Une revue a
// montré qu'un Apply découpé en deux commits laissait la suite entièrement
// verte, donc que rien ne retenait quelqu'un de défaire l'atomicité sans s'en
// apercevoir.
//
// Le levier est une source qui pointe sur un message inexistant: la clé
// étrangère de graph_relation_sources la refuse, et elle la refuse après que
// les entités ont été écrites. Une relation dont l'entité source manque ne
// ferait pas l'affaire, puisque Validate l'écarterait avant la base.
func TestApplyEstToutOuRien(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)
	seedConversation(t, pool, "ws1", "conv1", "participants", 1)

	src := graph.EntityID("ws1", "person:paul")
	dk := graph.DedupKey(src, "likes", nil, "green", nil)
	ext := memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{{
			EntityID: src, WorkspaceID: "ws1", CanonicalKey: "person:paul",
			EntityType: "person", DisplayName: "Paul", Resolved: true,
		}},
		Relations: []memory.GraphRelation{{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "likes", TargetLiteral: "green",
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{uuid.New()},
		}},
	}

	if err := repo.Apply(ctx, ext, nil); err == nil {
		t.Fatal("Apply a réussi malgré une source qui pointe sur un message inexistant")
	}

	var ents, rels, sources int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM graph_entities),
		       (SELECT count(*) FROM graph_relations),
		       (SELECT count(*) FROM graph_relation_sources)`).
		Scan(&ents, &rels, &sources); err != nil {
		t.Fatal(err)
	}
	if ents != 0 || rels != 0 || sources != 0 {
		t.Errorf("après échec: %d entités, %d relations, %d sources, want 0 partout: "+
			"l'entité écrite avant l'échec doit repartir avec la transaction",
			ents, rels, sources)
	}
}

// TestApplyEcritUneCibleEntite explore la moitié du modèle de la section 4.8
// qu'aucun test ne touchait: une arête d'entité à entité, dont le littéral est
// vide. C'est le seul cas où nullIfEmpty porte quelque chose, puisque le CHECK
// de graph_relations exige qu'exactement une des deux cibles soit renseignée
// et qu'une chaîne vide compte comme renseignée. Sans la conversion en NULL,
// ce test échoue sur la contrainte.
func TestApplyEcritUneCibleEntite(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)
	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 1)

	paul := graph.EntityID("ws1", "person:paul")
	marie := graph.EntityID("ws1", "person:marie")
	dk := graph.DedupKey(paul, "knows", &marie, "", nil)

	if err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{
			{EntityID: paul, WorkspaceID: "ws1", CanonicalKey: "person:paul",
				EntityType: "person", DisplayName: "Paul", Resolved: true},
			{EntityID: marie, WorkspaceID: "ws1", CanonicalKey: "person:marie",
				EntityType: "person", DisplayName: "Marie", Resolved: true},
		},
		Relations: []memory.GraphRelation{{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: paul, RelationType: "knows", TargetEntityID: &marie,
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msgs[0]},
		}},
	}, nil); err != nil {
		t.Fatalf("apply cible entité: %v", err)
	}

	var target *uuid.UUID
	var literal *string
	if err := pool.QueryRow(ctx,
		`SELECT target_entity_id, target_literal FROM graph_relations`).
		Scan(&target, &literal); err != nil {
		t.Fatal(err)
	}
	if literal != nil {
		t.Errorf("target_literal = %q, want NULL: un littéral vide doit devenir NULL", *literal)
	}
	if target == nil || *target != marie {
		t.Errorf("target_entity_id = %v, want %v", target, marie)
	}
}

// TestApplyRefuseUneRelationSansScope: la colonne scope est NOT NULL, mais une
// chaîne vide n'est pas NULL et passerait donc la contrainte. Or l'accès se
// dérive du scope et des sources, si bien qu'une relation entrée avec scope=”
// serait un trou du modèle d'accès. L'extracteur ne renseigne pas ce champ:
// c'est au handler de le poser depuis la conversation, et son oubli est un
// défaut de câblage, pas du bruit de modèle. Apply doit donc refuser bruyamment
// plutôt que d'écrire ou de laisser tomber en silence.
func TestApplyRefuseUneRelationSansScope(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)
	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 1)

	src := graph.EntityID("ws1", "person:paul")
	dk := graph.DedupKey(src, "likes", nil, "green", nil)
	relID := graph.RelationID("ws1", dk)
	err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{{
			EntityID: src, WorkspaceID: "ws1", CanonicalKey: "person:paul",
			EntityType: "person", DisplayName: "Paul", Resolved: true,
		}},
		Relations: []memory.GraphRelation{{
			RelationID: relID, WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "likes", TargetLiteral: "green",
			ObservedAt: time.Now().UTC(), Confidence: 1,
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msgs[0]},
		}},
	}, nil)
	if err == nil {
		t.Fatal("Apply a accepté une relation sans scope")
	}
	// L'erreur doit localiser la ligne en cause, et par son identifiant: le
	// type de relation sort du modèle d'extraction, donc du contenu des
	// messages, et cette erreur finit dans un log et dans jobs.last_error.
	if !strings.Contains(err.Error(), relID.String()) {
		t.Errorf("erreur = %q, elle devrait nommer le relation_id en cause", err)
	}
	if strings.Contains(err.Error(), "likes") {
		t.Errorf("erreur = %q: elle porte le type de relation, qui est du "+
			"contenu dérivé des messages", err)
	}

	var rels int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM graph_relations`).Scan(&rels); err != nil {
		t.Fatal(err)
	}
	if rels != 0 {
		t.Errorf("%d relations écrites, want 0", rels)
	}
}

// TestApplyConcurrentNInterbloquePas: deux extractions simultanées qui parlent
// des deux mêmes sujets prennent les mêmes verrous de ligne. Si elles les
// prennent dans des ordres différents, Postgres détecte un cycle et avorte
// l'une des deux avec 40P01. Deux ordres étaient laissés au hasard: celui du
// slice d'entités, qui vient du modèle de langage et n'est trié par rien, et
// celui des couples à recalculer, parcourus depuis une map dont Go randomise
// délibérément l'itération.
//
// Le test monte le cas nommé par la revue: un message parle de la tomate puis
// du basilic, l'autre du basilic puis de la tomate. Les deux types sont à
// valeur unique, donc les deux chaînes se verrouillent aussi.
func TestApplyConcurrentNInterbloquePas(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)
	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 2)

	tomate := graph.EntityID("ws1", "plant:tomate")
	basilic := graph.EntityID("ws1", "plant:basilic")
	ent := func(id uuid.UUID, key, name string) memory.GraphEntity {
		return memory.GraphEntity{EntityID: id, WorkspaceID: "ws1",
			CanonicalKey: key, EntityType: "plant", DisplayName: name,
			Resolved: true}
	}
	rel := func(src uuid.UUID, state string, msg uuid.UUID) memory.GraphRelation {
		dk := graph.DedupKey(src, "has_observed_state", nil, state, nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "has_observed_state",
			TargetLiteral: state, ObservedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
			Confidence: 1, Scope: "participants", DedupKey: dk,
			ConversationID: "conv1", SourceMessageIDs: []uuid.UUID{msg},
		}
	}

	single := []string{"has_observed_state"}
	// Les deux extractions listent les mêmes entités dans l'ordre inverse.
	ext := []memory.GraphExtraction{
		{WorkspaceID: "ws1", ConversationID: "conv1",
			Entities: []memory.GraphEntity{
				ent(tomate, "plant:tomate", "Tomate"),
				ent(basilic, "plant:basilic", "Basilic"),
			},
			Relations: []memory.GraphRelation{
				rel(tomate, "green", msgs[0]), rel(basilic, "green", msgs[0]),
			}},
		{WorkspaceID: "ws1", ConversationID: "conv1",
			Entities: []memory.GraphEntity{
				ent(basilic, "plant:basilic", "Basilic"),
				ent(tomate, "plant:tomate", "Tomate"),
			},
			Relations: []memory.GraphRelation{
				rel(basilic, "red", msgs[1]), rel(tomate, "red", msgs[1]),
			}},
	}

	const rounds = 40
	errs := make(chan error, 2*rounds)
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		for g := range ext {
			wg.Add(1)
			go func(e memory.GraphExtraction) {
				defer wg.Done()
				if err := repo.Apply(ctx, e, single); err != nil {
					errs <- err
				}
			}(ext[g])
		}
		wg.Wait()
	}
	close(errs)
	for err := range errs {
		t.Fatalf("apply concurrent: %v", err)
	}
}

// TestUpsertEntitiesRefuseUneCleVideSansNommerLeContenu couvre le second
// message d'erreur défensif du constat F10 de la revue finale.
//
// Le garde est aujourd'hui inatteignable par le chemin normal: convert produit
// toujours une clé contenant un ':' et écarte celles qui finissent par ':'.
// C'est un filet pour un câblage futur, et c'est exactement le jour où un
// nouveau câblage l'atteindra que l'invariant de journalisation tomberait: le
// display_name sort du modèle d'extraction, donc du contenu des messages, et
// cette erreur remonte jusqu'à un slog.Warn du runner et jusqu'à
// jobs.last_error, une colonne durable. Le test passe donc par UpsertEntities,
// qui expose le garde directement.
func TestUpsertEntitiesRefuseUneCleVideSansNommerLeContenu(t *testing.T) {
	pool := newTestPool(t)
	repo := NewGraphRepo(pool)

	const contenu = "Paul Dupont gagne 95000 euros"
	id := graph.EntityID("ws1", "person:sans-cle")
	err := repo.UpsertEntities(context.Background(), []memory.GraphEntity{{
		EntityID: id, WorkspaceID: "ws1", CanonicalKey: "",
		EntityType: "person", DisplayName: contenu,
	}})
	if err == nil {
		t.Fatal("UpsertEntities a accepté une entité sans clé canonique")
	}
	if !strings.Contains(err.Error(), id.String()) {
		t.Errorf("erreur = %q, elle devrait nommer l'entity_id en cause", err)
	}
	if strings.Contains(err.Error(), contenu) {
		t.Errorf("erreur = %q: elle porte le nom d'affichage, qui est du "+
			"contenu dérivé des messages", err)
	}
}

// TestApplyEcritLEmbeddingDuFait: le vecteur du fait est ce qui donne à la
// recherche par graphe un signal de pertinence par rapport à la question.
// Sans lui, elle rendait le voisinage de l'entité graine dans un ordre qui
// ignorait la question, et trois questions sans rapport recevaient les mêmes
// quatorze faits.
func TestApplyEcritLEmbeddingDuFait(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)
	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 1)

	src := graph.EntityID("ws1", "person:paul")
	vecteur := make([]float32, 768)
	for i := range vecteur {
		vecteur[i] = 0.01
	}
	dk := graph.DedupKey(src, "aime", nil, "vert", nil)
	if err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul", Resolved: true}},
		Relations: []memory.GraphRelation{{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "aime", TargetLiteral: "vert",
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msgs[0]},
			Embedding:        vecteur,
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT embedding IS NOT NULL FROM graph_relations WHERE workspace_id = 'ws1'`,
	).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Error("le vecteur du fait n'a pas été écrit")
	}
}

// Une relation écrite sans vecteur, ce qui arrive pendant une panne de
// l'embedder, doit rester écrite et trouvable: la panne dégrade le classement,
// elle ne perd pas la relation. Et le rejeu doit pouvoir la compléter, d'où le
// COALESCE de l'upsert.
func TestApplySansEmbeddingPuisAvecLeComplete(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewGraphRepo(pool)
	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 2)

	src := graph.EntityID("ws1", "person:paul")
	ent := memory.GraphEntity{EntityID: src, WorkspaceID: "ws1",
		CanonicalKey: "person:paul", EntityType: "person",
		DisplayName: "Paul", Resolved: true}
	dk := graph.DedupKey(src, "aime", nil, "vert", nil)
	rel := func(vec []float32, msg uuid.UUID) memory.GraphRelation {
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "aime", TargetLiteral: "vert",
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msg}, Embedding: vec,
		}
	}

	if err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities:  []memory.GraphEntity{ent},
		Relations: []memory.GraphRelation{rel(nil, msgs[0])},
	}, nil); err != nil {
		t.Fatal(err)
	}
	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT embedding IS NOT NULL FROM graph_relations WHERE workspace_id = 'ws1'`,
	).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("un vecteur a été écrit alors qu'aucun n'était fourni")
	}

	vecteur := make([]float32, 768)
	for i := range vecteur {
		vecteur[i] = 0.02
	}
	if err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities:  []memory.GraphEntity{ent},
		Relations: []memory.GraphRelation{rel(vecteur, msgs[1])},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT embedding IS NOT NULL FROM graph_relations WHERE workspace_id = 'ws1'`,
	).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Error("le rejeu n'a pas complété le vecteur manquant")
	}

	// Le sens inverse compte davantage: un rejeu pendant une panne de
	// l'embedder ne doit pas effacer un vecteur déjà écrit. C'est ce que le
	// COALESCE de l'upsert garantit, et remplacer ce COALESCE par
	// EXCLUDED.embedding passait le cas précédent sans faire rougir quoi que
	// ce soit.
	if err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities:  []memory.GraphEntity{ent},
		Relations: []memory.GraphRelation{rel(nil, msgs[0])},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT embedding IS NOT NULL FROM graph_relations WHERE workspace_id = 'ws1'`,
	).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Error("un rejeu sans vecteur a effacé celui qui était déjà écrit")
	}
}
