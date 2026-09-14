package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/graph"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// questionNeutre est le texte de requête des tests dont la graine doit venir
// de Subjects et de rien d'autre. Elle est réaliste, comme une vraie question
// d'appelant, et ne nomme délibérément aucune des entités des fixtures:
// mesurée contre la base, word_similarity de cette question contre "Paul",
// "Jardin", "Tomate", "Paul Dupont", "Paul l'inconnu" et "Tomate cerise" va
// de 0 à 0,143, toutes sous le seuil de candidateSimilarityThreshold (0,4).
//
// C'est ce qui manquait à la livraison de la tâche 8: ses cinq tests
// passaient tous Text: "Paul", exactement le nom d'affichage de l'entité
// graine, donc la branche trigramme trouvait la graine à elle seule et
// masquait les trois branches d'égalité de $3. seedKeys pouvait rendre nil
// sans qu'aucun test ne bronche. Une question qui ne nomme rien force les
// clés d'identité à faire le travail.
const questionNeutre = "Qu'est-ce qui a changé depuis la dernière fois ?"

// addParticipant inscrit un principal comme participant d'une conversation,
// comme le fait l'accumulation de participants de MessageRepo.Append. Les
// tests de graphe en ont besoin pour un demandeur qui n'est pas l'auteur des
// messages semés: ReadableCTE n'ouvre la branche 'participants' que sur une
// ligne conversation_participants dont left_at est nul. Écrit ici plutôt que
// dans graph_test.go, où l'ancien helper du même rôle avait été retiré comme
// code mort.
func addParticipant(t *testing.T, pool *pgxpool.Pool, conv, key string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO conversation_participants (conversation_id, participant_key)
		 VALUES ($1, $2)
		 ON CONFLICT (conversation_id, participant_key)
		 DO UPDATE SET left_at = NULL`, conv, key); err != nil {
		t.Fatalf("add participant: %v", err)
	}
}

func TestSearchGraphNeRendQueDesRelationsDontUneSourceEstLisible(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	// conv_ouverte est en scope participants avec alice dedans.
	// conv_fermee est en scope private: personne ne la lit sans ACL.
	ouverts := seedConversation(t, pool, "ws1", "conv_ouverte", "participants", 1)
	fermes := seedConversation(t, pool, "ws1", "conv_fermee", "private", 1)
	addParticipant(t, pool, "conv_ouverte", "user:alice")

	src := graph.EntityID("ws1", "person:paul")
	ent := memory.GraphEntity{EntityID: src, WorkspaceID: "ws1",
		CanonicalKey: "person:paul", EntityType: "person",
		DisplayName: "Paul", Resolved: true}
	mk := func(literal, conv string, msg uuid.UUID) memory.GraphRelation {
		dk := graph.DedupKey(src, "likes", nil, literal, nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "likes", TargetLiteral: literal,
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: conv, SourceMessageIDs: []uuid.UUID{msg},
		}
	}
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv_ouverte",
		Entities:  []memory.GraphEntity{ent},
		Relations: []memory.GraphRelation{mk("vert", "conv_ouverte", ouverts[0])},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv_fermee",
		Entities:  []memory.GraphEntity{ent},
		Relations: []memory.GraphRelation{mk("secret", "conv_fermee", fermes[0])},
	}, nil); err != nil {
		t.Fatal(err)
	}

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range got.Facts {
		if f.Object == "secret" {
			t.Fatal("un fait dont la seule source est dans une conversation privée est ressorti")
		}
	}
	if len(got.Facts) != 1 {
		t.Errorf("%d faits, want 1", len(got.Facts))
	}
	// Le compte des candidats avant la boucle: c'est un contrôle de
	// confidentialité (« un candidat d'une conversation privée est ressorti »)
	// et il passait tout aussi bien si les candidats disparaissaient
	// entièrement. La relation lisible a une seule source, donc un candidat.
	if len(got.Candidates) != 1 {
		t.Fatalf("%d candidats, want 1: une boucle sur une liste vide ne "+
			"prouve rien, surtout pas une absence de fuite", len(got.Candidates))
	}
	for _, c := range got.Candidates {
		if c.ConversationID == "conv_fermee" {
			t.Fatal("un candidat d'une conversation privée est ressorti")
		}
		if c.Strategy != "graph" {
			t.Errorf("strategy = %q, want graph", c.Strategy)
		}
		if c.AccessReason == "" {
			t.Error("access_reason vide")
		}
	}
}

func TestSearchGraphNeFranchitPasLesWorkspaces(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws2", "conv_ws2", "workspace", 1)
	src := graph.EntityID("ws2", "person:paul")
	dk := graph.DedupKey(src, "likes", nil, "vert", nil)
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws2", ConversationID: "conv_ws2",
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws2",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul", Resolved: true}},
		Relations: []memory.GraphRelation{{
			RelationID: graph.RelationID("ws2", dk), WorkspaceID: "ws2",
			SourceEntityID: src, RelationType: "likes", TargetLiteral: "vert",
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "workspace",
			DedupKey: dk, ConversationID: "conv_ws2",
			SourceMessageIDs: []uuid.UUID{msgs[0]}}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 0 || len(got.Candidates) != 0 {
		t.Errorf("le graphe d'un autre workspace est ressorti: %d faits, %d candidats",
			len(got.Facts), len(got.Candidates))
	}
}

func TestSearchGraphIgnoreLesRelationsInvalidees(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 1)
	addParticipant(t, pool, "conv1", "user:alice")

	src := graph.EntityID("ws1", "person:paul")
	dk := graph.DedupKey(src, "likes", nil, "vert", nil)
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul", Resolved: true}},
		Relations: []memory.GraphRelation{{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "likes", TargetLiteral: "vert",
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msgs[0]}}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	q := memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	}

	before, err := repo.SearchGraph(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Facts) != 1 {
		t.Fatalf("%d faits avant suppression, want 1", len(before.Facts))
	}

	softDelete(t, pool, msgs[0])
	if err := gr.Reevaluate(ctx, msgs[0], nil); err != nil {
		t.Fatal(err)
	}

	after, err := repo.SearchGraph(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Facts) != 0 || len(after.Candidates) != 0 {
		t.Errorf("une relation invalidée ressort encore: %d faits, %d candidats",
			len(after.Facts), len(after.Candidates))
	}
}

func TestSearchGraphAtteintUneEntiteADeuxSauts(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 2)
	addParticipant(t, pool, "conv1", "user:alice")

	paul := graph.EntityID("ws1", "person:paul")
	jardin := graph.EntityID("ws1", "place:jardin")
	tomate := graph.EntityID("ws1", "plant:tomate")
	ents := []memory.GraphEntity{
		{EntityID: paul, WorkspaceID: "ws1", CanonicalKey: "person:paul",
			EntityType: "person", DisplayName: "Paul", Resolved: true},
		{EntityID: jardin, WorkspaceID: "ws1", CanonicalKey: "place:jardin",
			EntityType: "place", DisplayName: "Jardin", Resolved: true},
		{EntityID: tomate, WorkspaceID: "ws1", CanonicalKey: "plant:tomate",
			EntityType: "plant", DisplayName: "Tomate", Resolved: true},
	}
	edge := func(from, to uuid.UUID, rtype string, msg uuid.UUID) memory.GraphRelation {
		target := to
		dk := graph.DedupKey(from, rtype, &target, "", nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: from, RelationType: rtype, TargetEntityID: &target,
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msg},
		}
	}
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1", Entities: ents,
		Relations: []memory.GraphRelation{
			edge(paul, jardin, "owns", msgs[0]),
			edge(jardin, tomate, "contains", msgs[1]),
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	q := func(hops int) memory.GraphQuery {
		return memory.GraphQuery{
			CandidateQuery: memory.CandidateQuery{
				WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
			Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: hops,
		}
	}
	has := func(res memory.GraphResult, predicate string) bool {
		for _, f := range res.Facts {
			if f.Predicate == predicate {
				return true
			}
		}
		return false
	}

	un, err := repo.SearchGraph(ctx, q(1))
	if err != nil {
		t.Fatal(err)
	}
	if !has(un, "owns") {
		t.Error("la relation à un saut manque avec max_hops = 1")
	}
	if has(un, "contains") {
		t.Error("la relation à deux sauts ressort avec max_hops = 1")
	}

	deux, err := repo.SearchGraph(ctx, q(2))
	if err != nil {
		t.Fatal(err)
	}
	if !has(deux, "owns") || !has(deux, "contains") {
		t.Fatalf("les deux relations devraient ressortir avec max_hops = 2: %+v", deux.Facts)
	}
	// La plus proche est classée en premier: c'est l'ordre par profondeur
	// croissante de la section 8.2.
	if deux.Facts[0].Predicate != "owns" {
		t.Errorf("premier fait = %q, want owns (profondeur la plus faible)",
			deux.Facts[0].Predicate)
	}
}

func TestSearchGraphRefuseUnDemandeurVide(t *testing.T) {
	pool := newTestPool(t)
	repo := NewSearchRepo(pool)
	if _, err := repo.SearchGraph(context.Background(), memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{WorkspaceID: "ws1", Limit: 10},
	}); err == nil {
		t.Fatal("want une erreur sur un requester_key vide")
	}
}

// TestSearchGraphSemeParUneQuestionRealiste épingle la troisième source de
// graines de la section 8.2, celle qui détecte une entité en comparant la
// question à son nom d'affichage et à ses alias. Aucun Subjects n'est passé:
// le texte est la seule source de graines possible, donc si la branche
// trigramme ne mord pas, la traversée n'a pas de point de départ et le test
// échoue.
//
// La question est une vraie question, pas un nom nu. C'est tout le sujet:
// mesuré contre la base, similarity('Paul', 'Que cultive Paul ?') vaut
// 0,2941, sous le seuil pg_trgm par défaut de 0,3, alors que
// word_similarity('Paul', ...) vaut 1. Comparer la question entière au nom
// rendait donc la branche inerte sur toute question réelle, et verte sur les
// seuls tests qui passaient un nom nu.
//
// Le second cas isole la branche des alias: "Le voisin" ne ressemble pas à la
// question (word_similarity mesurée à 0,20, sous le seuil de 0,4), c'est
// l'alias "Paul" qui vaut 1 et qui sème.
func TestSearchGraphSemeParUneQuestionRealiste(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 2)
	addParticipant(t, pool, "conv1", "user:alice")

	nomme := graph.EntityID("ws1", "person:paul")
	alias := graph.EntityID("ws1", "person:le-voisin")
	rel := func(src uuid.UUID, rtype, literal string, msg uuid.UUID) memory.GraphRelation {
		dk := graph.DedupKey(src, rtype, nil, literal, nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: rtype, TargetLiteral: literal,
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1", SourceMessageIDs: []uuid.UUID{msg},
		}
	}
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{
			{EntityID: nomme, WorkspaceID: "ws1", CanonicalKey: "person:paul",
				EntityType: "person", DisplayName: "Paul", Resolved: true},
			{EntityID: alias, WorkspaceID: "ws1", CanonicalKey: "person:le-voisin",
				EntityType: "person", DisplayName: "Le voisin",
				Aliases: []string{"Paul"}, Resolved: true},
		},
		Relations: []memory.GraphRelation{
			rel(nomme, "cultive", "tomates", msgs[0]),
			rel(alias, "arrose", "le potager", msgs[1]),
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	question := "Quelles sont les tomates que Paul cultive dans son jardin ?"
	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: nil, Text: question, MaxHops: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	predicats := map[string]bool{}
	for _, f := range got.Facts {
		predicats[f.Predicate] = true
	}
	if !predicats["cultive"] {
		t.Errorf("la question ne sème pas l'entité nommée par son nom d'affichage: %+v",
			got.Facts)
	}
	if !predicats["arrose"] {
		t.Errorf("la question ne sème pas l'entité nommée par un de ses alias: %+v",
			got.Facts)
	}
}

// TestSearchGraphSemeParChacuneDesTroisFormesDeCle épingle séparément les
// trois comparaisons de $3. C'est le troisième piège de la tâche: les sujets
// connus sont des clés d'identité comme "user:paul" alors que les entités
// portent des clés canoniques comme "person:paul", et les deux ne s'égalent
// jamais. seedKeys décompose donc chaque sujet en la clé entière et sa partie
// locale mise en slug, et le SQL confronte les deux formes à canonical_key,
// à sa partie locale et au nom d'affichage.
//
// Trois entités, chacune taillée pour qu'une seule des trois comparaisons
// puisse la trouver, et un sujet par cas. Text vide partout: la branche
// trigramme est fermée par son garde sur un texte non vide, donc rien ne peut
// masquer la branche testée.
//
//   - clé entière: une entité non résolue porte
//     "unresolved-person:paul-conv_1", dont la partie après le premier
//     deux-points ("paul-conv_1") n'est pas stable par slug, puisque Slug
//     réduit le souligné à un tiret. La comparaison sur la partie locale ne
//     peut donc pas la trouver, et son nom d'affichage ne ressemble à rien de
//     ce que seedKeys émet. Seule l'égalité sur canonical_key la sème.
//   - partie locale: le sujet "user:paul" contre l'entité "person:paul". Les
//     clés entières diffèrent par leur préfixe de type, le nom d'affichage
//     est "Paul Dupont", donc seul split_part la sème. C'est le cas nominal
//     de la spec.
//   - nom d'affichage: l'entité "plant:tomate-cerise" s'affiche "Tomate". Le
//     sujet "Tomate" n'a pas de deux-points, donc seedKeys n'émet que
//     "tomate", qui ne vaut ni la clé entière ni sa partie locale
//     ("tomate-cerise"). Seul lower(display_name) la sème.
func TestSearchGraphSemeParChacuneDesTroisFormesDeCle(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv_1", "participants", 3)
	addParticipant(t, pool, "conv_1", "user:alice")

	cleEntiere := graph.UnresolvedKey("person", "Paul", "conv_1")
	if cleEntiere != "unresolved-person:paul-conv_1" {
		t.Fatalf("la forme de la clé non résolue a changé: %q", cleEntiere)
	}
	inconnu := graph.EntityID("ws1", cleEntiere)
	paul := graph.EntityID("ws1", "person:paul")
	tomate := graph.EntityID("ws1", "plant:tomate-cerise")

	rel := func(src uuid.UUID, rtype, literal string, msg uuid.UUID) memory.GraphRelation {
		dk := graph.DedupKey(src, rtype, nil, literal, nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: rtype, TargetLiteral: literal,
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv_1", SourceMessageIDs: []uuid.UUID{msg},
		}
	}
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv_1",
		Entities: []memory.GraphEntity{
			{EntityID: inconnu, WorkspaceID: "ws1", CanonicalKey: cleEntiere,
				EntityType: "person", DisplayName: "Paul l'inconnu"},
			{EntityID: paul, WorkspaceID: "ws1", CanonicalKey: "person:paul",
				EntityType: "person", DisplayName: "Paul Dupont", Resolved: true},
			{EntityID: tomate, WorkspaceID: "ws1", CanonicalKey: "plant:tomate-cerise",
				EntityType: "plant", DisplayName: "Tomate", Resolved: true},
		},
		Relations: []memory.GraphRelation{
			rel(inconnu, "hante", "le grenier", msgs[0]),
			rel(paul, "cultive", "des tomates", msgs[1]),
			rel(tomate, "murit", "en juillet", msgs[2]),
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	cas := []struct {
		nom      string
		branche  string
		sujet    string
		predicat string
	}{
		{"clé entière", "e.canonical_key = ANY($3)", cleEntiere, "hante"},
		{"partie locale", "split_part(e.canonical_key, ':', 2) = ANY($3)",
			"user:paul", "cultive"},
		{"nom d'affichage", "lower(e.display_name) = ANY($3)", "Tomate", "murit"},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			got, err := repo.SearchGraph(ctx, memory.GraphQuery{
				CandidateQuery: memory.CandidateQuery{
					WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
				Subjects: []string{c.sujet}, Text: "", MaxHops: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Facts) != 1 {
				t.Fatalf("le sujet %q rend %d faits, want 1 (%+v): la branche %s "+
					"ne sème plus", c.sujet, len(got.Facts), got.Facts, c.branche)
			}
			if got.Facts[0].Predicate != c.predicat {
				t.Errorf("le sujet %q sème la mauvaise entité: predicate = %q, want %q",
					c.sujet, got.Facts[0].Predicate, c.predicat)
			}
		})
	}
}

// relationInvalidatedAt lit l'invalidated_at d'une relation. Les deux tests
// qui séparent les filtres de vivacité en ont besoin pour affirmer dans quel
// état ils ont mis la base, plutôt que de le supposer.
func relationInvalidatedAt(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) *time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT invalidated_at FROM graph_relations WHERE relation_id = $1`,
		id).Scan(&at); err != nil {
		t.Fatalf("read invalidated_at: %v", err)
	}
	return at
}

// TestSearchGraphEcarteUneSourceSupprimeeDontLaRelationSurvit isole
// m.deleted_at IS NULL.
//
// L'état monté ici est celui que la section 7.5 décrit: une relation à deux
// sources dont une seule disparaît reste crue, parce que invalidateSQL exige
// un NOT EXISTS sur toutes les sources vivantes. invalidated_at reste donc
// NULL, et le filtre du graphe ne peut rien écarter. Seul
// m.deleted_at IS NULL empêche le message supprimé de ressortir, en candidat
// comme en source du fait: c'est le critère d'acceptation 6 sur le chemin du
// graphe.
//
// Le test affirme l'état de la base avant de conclure, pour que son échec
// dise lequel des deux mécanismes a lâché.
func TestSearchGraphEcarteUneSourceSupprimeeDontLaRelationSurvit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 2)
	addParticipant(t, pool, "conv1", "user:alice")

	src := graph.EntityID("ws1", "person:paul")
	dk := graph.DedupKey(src, "likes", nil, "vert", nil)
	relID := graph.RelationID("ws1", dk)
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul Dupont", Resolved: true}},
		Relations: []memory.GraphRelation{{
			RelationID: relID, WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "likes", TargetLiteral: "vert",
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msgs[0], msgs[1]}}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	q := memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 1,
	}

	before, err := repo.SearchGraph(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Facts) != 1 || len(before.Facts[0].SourceMessageIDs) != 2 {
		t.Fatalf("avant suppression: %d faits, %d sources, want 1 et 2",
			len(before.Facts), len(before.Facts[0].SourceMessageIDs))
	}

	softDelete(t, pool, msgs[0])
	if err := gr.Reevaluate(ctx, msgs[0], nil); err != nil {
		t.Fatal(err)
	}
	if at := relationInvalidatedAt(t, pool, relID); at != nil {
		t.Fatalf("la relation a été invalidée alors qu'une source survit (%v): "+
			"ce test ne sépare plus les deux filtres", at)
	}

	after, err := repo.SearchGraph(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Facts) != 1 {
		t.Fatalf("%d faits après suppression d'une source sur deux, want 1", len(after.Facts))
	}
	if got := after.Facts[0].SourceMessageIDs; len(got) != 1 || got[0] != msgs[1] {
		t.Errorf("sources du fait = %v, want [%v]: un message supprimé est "+
			"resté source d'un fait", got, msgs[1])
	}
	if len(after.Candidates) != 1 || after.Candidates[0].AnchorMessageID != msgs[1] {
		t.Errorf("%d candidats, ancrages %v: un message supprimé est ressorti "+
			"en candidat", len(after.Candidates), after.Candidates)
	}
}

// TestSearchGraphEcarteUneRelationInvalideeDontLaSourceEstVivante isole
// r.invalidated_at IS NULL.
//
// L'état monté est le symétrique du précédent: la relation porte un
// invalidated_at et son message source est vivant, donc m.deleted_at IS NULL
// ne peut rien écarter et seul le filtre d'invalidation le fait.
//
// Pour l'atteindre, la source est supprimée, Reevaluate pose
// l'invalidated_at, puis la suppression douce est annulée. Le service ne
// ressuscite jamais un message de lui-même, et ce n'est pas ce que le test
// prétend: il fabrique l'état minimal qui sépare les deux filtres de lecture.
// La sémantique du chemin d'écriture, elle, est déjà épinglée par les tests
// de Reevaluate.
func TestSearchGraphEcarteUneRelationInvalideeDontLaSourceEstVivante(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 1)
	addParticipant(t, pool, "conv1", "user:alice")

	src := graph.EntityID("ws1", "person:paul")
	dk := graph.DedupKey(src, "likes", nil, "vert", nil)
	relID := graph.RelationID("ws1", dk)
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul Dupont", Resolved: true}},
		Relations: []memory.GraphRelation{{
			RelationID: relID, WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "likes", TargetLiteral: "vert",
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msgs[0]}}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	softDelete(t, pool, msgs[0])
	if err := gr.Reevaluate(ctx, msgs[0], nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET deleted_at = NULL WHERE message_id = $1`,
		msgs[0]); err != nil {
		t.Fatal(err)
	}

	if at := relationInvalidatedAt(t, pool, relID); at == nil {
		t.Fatal("la relation n'a pas été invalidée: le test ne monte plus l'état " +
			"qu'il prétend monter")
	}
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at FROM messages WHERE message_id = $1`,
		msgs[0]).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if deletedAt != nil {
		t.Fatalf("le message est encore supprimé (%v): le test ne sépare plus "+
			"les deux filtres", deletedAt)
	}

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 0 || len(got.Candidates) != 0 {
		t.Errorf("une relation invalidée dont la source est vivante ressort: "+
			"%d faits, %d candidats", len(got.Facts), len(got.Candidates))
	}
}

// TestSearchGraphRendExactementLesSautsDemandes épingle la borne de
// profondeur par son comportement observable, sur une chaîne de quatre
// arêtes: max_hops = k rend exactement hop1 à hopk, ni un de moins ni un de
// plus.
//
// max_hops compte les arêtes et non les entités, donc une relation attachée
// à une graine est à un saut. Le test vérifie les deux sens du bornage: une
// borne desserrée fait apparaître un saut de trop, une borne resserrée en
// fait disparaître un. C'est ce qui manquait: la borne était desserrable
// seule sans qu'aucun test ne bronche, parce qu'un second filtre de
// profondeur redondant absorbait le changement.
//
// Text est vide pour que la traversée n'ait qu'une graine, celle du sujet:
// une seconde graine trouvée par similarité raccourcirait les distances et
// le test ne mesurerait plus la profondeur.
func TestSearchGraphRendExactementLesSautsDemandes(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 4)
	addParticipant(t, pool, "conv1", "user:alice")

	noms := []string{"a", "b", "c", "d", "e"}
	ids := make([]uuid.UUID, len(noms))
	ents := make([]memory.GraphEntity, len(noms))
	for i, n := range noms {
		key := "chain:" + n
		ids[i] = graph.EntityID("ws1", key)
		ents[i] = memory.GraphEntity{EntityID: ids[i], WorkspaceID: "ws1",
			CanonicalKey: key, EntityType: "chain",
			DisplayName: "Maillon " + strings.ToUpper(n), Resolved: true}
	}
	var rels []memory.GraphRelation
	for i := 0; i < 4; i++ {
		target := ids[i+1]
		rtype := fmt.Sprintf("hop%d", i+1)
		dk := graph.DedupKey(ids[i], rtype, &target, "", nil)
		rels = append(rels, memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: ids[i], RelationType: rtype, TargetEntityID: &target,
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msgs[i]},
		})
	}
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: ents, Relations: rels,
	}, nil); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		hops int
		want []string
	}{
		{1, []string{"hop1"}},
		{2, []string{"hop1", "hop2"}},
		{3, []string{"hop1", "hop2", "hop3"}},
		{4, []string{"hop1", "hop2", "hop3", "hop4"}},
		{5, []string{"hop1", "hop2", "hop3", "hop4"}},
	} {
		t.Run(fmt.Sprintf("max_hops=%d", tc.hops), func(t *testing.T) {
			got, err := repo.SearchGraph(ctx, memory.GraphQuery{
				CandidateQuery: memory.CandidateQuery{
					WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 50},
				Subjects: []string{"chain:a"}, Text: "", MaxHops: tc.hops,
			})
			if err != nil {
				t.Fatal(err)
			}
			var predicats []string
			for _, f := range got.Facts {
				predicats = append(predicats, f.Predicate)
			}
			// Les faits arrivent par profondeur croissante, donc l'ordre
			// attendu est celui de la chaîne.
			if strings.Join(predicats, ",") != strings.Join(tc.want, ",") {
				t.Errorf("max_hops = %d rend %v, want %v",
					tc.hops, predicats, tc.want)
			}
		})
	}
}

// TestSearchGraphUneRelationTresSourceeNAffamePasLesAutres épingle le
// plafond de sources par relation.
//
// La limite de la requête porte sur les lignes, une ligne étant une relation
// croisée avec un de ses messages sources lisibles. Sans plafond, une seule
// relation très bien sourcée consomme toute la limite et les autres faits
// disparaissent entièrement: mesuré avant correction, une relation à dix
// sources plus deux autres relations, avec Limit à 5, rendait un seul fait.
// graph_top_k voulait donc dire "jusqu'à N lignes" et pas du tout "jusqu'à N
// faits", et son plancher effectif en nombre de faits était 1.
func TestSearchGraphUneRelationTresSourceeNAffamePasLesAutres(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 12)
	addParticipant(t, pool, "conv1", "user:alice")

	src := graph.EntityID("ws1", "person:paul")
	rel := func(rtype, literal string, conf float64, sources []uuid.UUID) memory.GraphRelation {
		dk := graph.DedupKey(src, rtype, nil, literal, nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: rtype, TargetLiteral: literal,
			ObservedAt: time.Now().UTC(), Confidence: conf, Scope: "participants",
			DedupKey: dk, ConversationID: "conv1", SourceMessageIDs: sources,
		}
	}
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul Dupont", Resolved: true}},
		Relations: []memory.GraphRelation{
			rel("repete", "partout", 1, msgs[:10]),
			rel("discret", "parfois", 0.5, []uuid.UUID{msgs[10]}),
			rel("rare", "une fois", 0.4, []uuid.UUID{msgs[11]}),
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 5},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	sources := map[string]int{}
	for _, f := range got.Facts {
		sources[f.Predicate] = len(f.SourceMessageIDs)
	}
	for _, predicat := range []string{"repete", "discret", "rare"} {
		if _, ok := sources[predicat]; !ok {
			t.Errorf("le fait %q a été affamé par la relation très sourcée: "+
				"faits rendus %v", predicat, sources)
		}
	}
	if n := sources["repete"]; n > maxGraphSourcesPerRelation {
		t.Errorf("la relation très sourcée rend %d sources, plafond attendu %d",
			n, maxGraphSourcesPerRelation)
	}
	// La limite reste la borne externe: elle porte sur les lignes, donc le
	// nombre de candidats ne peut pas la dépasser.
	if len(got.Candidates) > 5 {
		t.Errorf("%d candidats pour une limite de 5", len(got.Candidates))
	}
}

// TestSearchGraphPrefereUnSujetNommeAUnHomonymeApproche couvre le constat L4
// de la revue de la tâche 8: la coupure des graines n'avait pas d'ordre, donc
// un sujet fourni explicitement par l'appelant pouvait perdre sa place au
// profit d'homonymes trouvés par trigramme, et le jeu retenu dépendait du plan
// d'exécution. Le nombre de graines n'est pas ce qui compte, l'ordre l'est.
//
// C'est la troisième version de ce montage, et les deux premières
// n'épinglaient rien. La première passait avec et sans l'ORDER BY (jugement
// 40). La seconde passait avec l'ORDER BY privé de son terme de priorité, ce
// qui est justement le correctif qu'elle prétendait garder: elle nommait ses
// homonymes "Paulette 0" à "Paulette 59" et pariait sur la monotonie de
// word_similarity, que la revue finale a mesurée fausse. Contre la base, pour
// la question de ce test:
//
//	Paul          0,8
//	Paulette 0    0,8181818
//	Paulette 59   0,75
//
// Seuls les dix homonymes à un chiffre devançaient Paul; les cinquante à deux
// chiffres étaient derrière, et avec LIMIT 50 Paul arrivait au rang 11 et
// survivait tout seul.
//
// Pour que le test morde, il faut que tous les homonymes devancent Paul sur
// word_similarity. Ils s'appellent donc tous "Paulette" tout court, ce qui
// vaut 1 contre 0,8 pour "Paul", les deux mesurés contre la base sur la
// question de ce test. Le 0,8181818 cité plus haut est celui de
// "Paulette 0", pas celui de "Paulette": ne pas confondre les deux, c'est
// l'écart entre un montage qui mord et un qui ne mord pas. Ils sont assez
// nombreux pour saturer la coupure à eux seuls, si bien que sans le terme de
// priorité Paul tombe au rang 61 sur 60 graines retenues et disparaît.
func TestSearchGraphPrefereUnSujetNommeAUnHomonymeApproche(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "workspace", 1)

	ent := func(key, name string) memory.GraphEntity {
		return memory.GraphEntity{
			EntityID: graph.EntityID("ws1", key), WorkspaceID: "ws1",
			CanonicalKey: key, EntityType: "person", DisplayName: name,
			Resolved: true,
		}
	}
	rel := func(e memory.GraphEntity) memory.GraphRelation {
		dk := graph.DedupKey(e.EntityID, "likes", nil, "vert", nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: e.EntityID, RelationType: "likes",
			TargetLiteral: "vert", ObservedAt: time.Now().UTC(), Confidence: 1,
			Scope: "workspace", DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msgs[0]},
		}
	}
	apply := func(ents []memory.GraphEntity) {
		t.Helper()
		var rels []memory.GraphRelation
		for _, e := range ents {
			rels = append(rels, rel(e))
		}
		if err := gr.Apply(ctx, memory.GraphExtraction{
			WorkspaceID: "ws1", ConversationID: "conv1",
			Entities: ents, Relations: rels,
		}, nil); err != nil {
			t.Fatal(err)
		}
	}

	// Tous les homonymes portent exactement le même nom d'affichage, celui
	// qui devance "Paul" sur word_similarity pour la question posée. Seule la
	// clé canonique les distingue, ce qui suffit: c'est la clé qui déduplique
	// les entités, pas le nom.
	//
	// Ils sont écrits d'abord et le sujet nommé ensuite, dans une transaction
	// séparée. L'ordre d'écriture fixe l'ordre physique des lignes, donc
	// celui qu'un parcours séquentiel rendrait sans ORDER BY, et il rend le
	// test rouge aussi pour la mutation qui retire l'ORDER BY en entier.
	var homonymes []memory.GraphEntity
	for i := 0; i < maxSeedEntities+10; i++ {
		homonymes = append(homonymes, ent(
			fmt.Sprintf("person:paulette-%d", i), "Paulette"))
	}
	apply(homonymes)
	apply([]memory.GraphEntity{ent("person:paul", "Paul")})

	// "Paul" est nommé par sa clé d'identité, les Paulette ne ressortent que
	// par similarité de mot sur le texte de la question.
	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 200},
		Subjects: []string{"user:paul"},
		Text:     "Est-ce que Paulette aime le vert ?",
		MaxHops:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) == 0 {
		t.Fatal("aucun fait rendu: le montage ne prouve rien")
	}

	// Les homonymes saturent la coupure: si le montage n'en rend pas au moins
	// autant que la borne, c'est qu'ils ne sont pas assez nombreux ou pas
	// assez ressemblants, et l'absence de Paul ne prouverait plus rien.
	if len(got.Facts) < maxSeedEntities {
		t.Fatalf("%d faits rendus pour %d graines au plus: la coupure n'est pas "+
			"saturée, le montage ne prouve rien", len(got.Facts), maxSeedEntities)
	}

	trouve := false
	for _, f := range got.Facts {
		if f.Subject == "Paul" {
			trouve = true
			break
		}
	}
	if !trouve {
		t.Errorf("le sujet nommé explicitement a été évincé par ses homonymes, "+
			"%d faits rendus", len(got.Facts))
	}
}

// TestSearchGraphRawScoreSuitLeNombreDeSauts: RawScore n'était vérifié par
// rien, et un garde mort absorbait la mutation qui remplace n.depth + 1 par
// n.depth dans le comptage des sauts. Ce test regarde la valeur, ce qui rend
// cette mutation visible.
//
// Sa première version n'affirmait pourtant que « tout score est dans ]0,1] »
// et « le meilleur vaut 1 », deux propriétés qu'un score constant à 1
// satisfait: la mutation RawScore = 1 la laissait verte, alors que son nom
// promet que le score suit le nombre de sauts. La relation est donc épinglée
// candidat par candidat: la relation attachée à la graine est à un saut et vaut
// 1, celle qui part de l'entité voisine est à deux sauts et vaut 0,5.
func TestSearchGraphRawScoreSuitLeNombreDeSauts(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "workspace", 2)

	paul := graph.EntityID("ws1", "person:paul")
	jardin := graph.EntityID("ws1", "place:jardin")
	tomate := graph.EntityID("ws1", "plant:tomate")
	ents := []memory.GraphEntity{
		{EntityID: paul, WorkspaceID: "ws1", CanonicalKey: "person:paul",
			EntityType: "person", DisplayName: "Paul", Resolved: true},
		{EntityID: jardin, WorkspaceID: "ws1", CanonicalKey: "place:jardin",
			EntityType: "place", DisplayName: "Jardin", Resolved: true},
		{EntityID: tomate, WorkspaceID: "ws1", CanonicalKey: "plant:tomate",
			EntityType: "plant", DisplayName: "Tomate", Resolved: true},
	}
	edge := func(from, to uuid.UUID, rtype string, msg uuid.UUID) memory.GraphRelation {
		target := to
		dk := graph.DedupKey(from, rtype, &target, "", nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: from, RelationType: rtype, TargetEntityID: &target,
			ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "workspace",
			DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: []uuid.UUID{msg},
		}
	}
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1", Entities: ents,
		Relations: []memory.GraphRelation{
			edge(paul, jardin, "owns", msgs[0]),
			edge(jardin, tomate, "contains", msgs[1]),
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 50},
		Subjects: []string{"user:paul"}, Text: "", MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Un candidat par message d'ancrage: msgs[0] porte l'arête Paul -> Jardin,
	// attachée à la graine donc à un saut, et msgs[1] porte
	// Jardin -> Tomate, attachée à une entité atteinte en un saut donc à
	// deux. Le compte d'abord, comme partout ailleurs.
	if len(got.Candidates) != 2 {
		t.Fatalf("%d candidats rendus, want 2", len(got.Candidates))
	}
	scores := map[uuid.UUID]float64{}
	for _, c := range got.Candidates {
		scores[c.AnchorMessageID] = c.RawScore
		// Un score infini signalerait une profondeur nulle, donc un comptage
		// des sauts décalé de un.
		if c.RawScore <= 0 || c.RawScore > 1 {
			t.Errorf("RawScore = %v, attendu dans ]0, 1]", c.RawScore)
		}
	}
	if scores[msgs[0]] != 1 {
		t.Errorf("RawScore à un saut = %v, want 1", scores[msgs[0]])
	}
	if scores[msgs[1]] != 0.5 {
		t.Errorf("RawScore à deux sauts = %v, want 0,5: le score doit suivre le "+
			"nombre de sauts et pas seulement rester borné", scores[msgs[1]])
	}
}

// seedUnitWindow écrit une unité de mémoire active ancrée sur un message et
// couvrant exactement la fenêtre [start, end]. Écrite en SQL direct plutôt
// que par UnitRepo.Upsert parce que ces tests ont besoin de fixer la fenêtre
// au message près, indépendamment de indexing.previous_messages: c'est
// justement l'écart entre cette fenêtre et celle de l'extraction du graphe
// qui est l'objet du test.
func seedUnitWindow(t *testing.T, pool *pgxpool.Pool, workspace, conv string,
	anchor, first uuid.UUID) uuid.UUID {

	t.Helper()
	start, end := messageSequence(t, pool, first), messageSequence(t, pool, anchor)
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO memory_units
			(memory_unit_id, workspace_id, conversation_id, anchor_message_id,
			 start_sequence, end_sequence, embedding_text, embedding_model,
			 indexing_strategy, indexing_version, scope, active)
		VALUES ($1, $2, $3, $4, $5, $6, 'texte', 'test-model',
		        'message_window', 1, 'private', TRUE)`,
		id, workspace, conv, anchor, start, end); err != nil {
		t.Fatalf("seed memory unit: %v", err)
	}
	return id
}

// messageSequence lit le numéro de séquence d'un message. Les séquences
// commencent à 1 et non à 0: une fenêtre écrite en dur se décalerait d'un
// message et le test ne prouverait plus ce qu'il annonce.
func messageSequence(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(context.Background(),
		`SELECT sequence_number FROM messages WHERE message_id = $1`,
		id).Scan(&seq); err != nil {
		t.Fatalf("lecture de la séquence: %v", err)
	}
	return seq
}

// grantRead pose la seule ligne memory_unit_acl qui ouvre l'accès à une
// unité, comme le fait la route POST /v1/memories/{id}/acl.
func grantRead(t *testing.T, pool *pgxpool.Pool, unit uuid.UUID, principal string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO memory_unit_acl (memory_unit_id, principal_key, permission)
		VALUES ($1, $2, 'read')`, unit, principal); err != nil {
		t.Fatalf("grant read: %v", err)
	}
}

// applyPaulSalaire écrit la relation (Paul, salaire_annuel, "95000 euros")
// avec les messages sources donnés, et rend l'identifiant de l'entité source.
// Le littéral est un fragment que le modèle a levé du texte d'un message: ce
// n'est ni un nom d'entité ni une distance, donc rien de ce que la
// section 12 accepte de laisser franchir la frontière.
func applyPaulSalaire(t *testing.T, gr *GraphRepo, conv string,
	sources ...uuid.UUID) uuid.UUID {

	t.Helper()
	src := graph.EntityID("ws1", "person:paul")
	dk := graph.DedupKey(src, "salaire_annuel", nil, "95000 euros", nil)
	if err := gr.Apply(context.Background(), memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: conv,
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul", Resolved: true}},
		Relations: []memory.GraphRelation{{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "salaire_annuel",
			TargetLiteral: "95000 euros", ObservedAt: time.Now().UTC(),
			Confidence: 1, Scope: "private", DedupKey: dk,
			ConversationID: conv, SourceMessageIDs: sources}},
	}, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return src
}

// TestSearchGraphNeRendPasUnFaitSourceHorsDeLUniteOctroyee couvre le constat
// F1 de la revue finale de branche, une couture entre deux tâches correctes
// séparément.
//
// graph.context_messages vaut 4 et indexing.previous_messages vaut 2: la
// fenêtre que voit l'extracteur est deux fois plus large que celle qu'une
// unité de mémoire couvre, et filterSources autorise délibérément toute la
// fenêtre d'extraction comme source. Une relation extraite en traitant M4
// peut donc être sourcée par M0 alors que l'unité ancrée sur M4 ne couvre que
// [M2, M4].
//
// Tout le mécanisme d'accès se comporte correctement: la ligne de M0 est
// écartée par le SQL, la fenêtre du candidat est bornée à [2,4], la raison
// d'accès est bien explicit_acl, et l'extrait rendu ne contient pas M0. Et
// pourtant le littéral « 95000 euros », énoncé dans M0 seulement, arrivait au
// demandeur dans graph_facts et dans le context_block. C'est la forme exacte
// du second Critique de la branche parente, transposée du texte cité au fait
// dérivé.
//
// La règle épinglée ici: un fait dont la raison d'accès est explicit_acl ne
// ressort que si toutes ses sources tombent dans la fenêtre de l'unité
// octroyée. Pas « au moins une source lisible », qui reste la règle juste
// pour un accès par le scope, où toute la conversation est lisible.
func TestSearchGraphNeRendPasUnFaitSourceHorsDeLUniteOctroyee(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	// Conversation privée: aucun message n'est lisible par le scope, et
	// agent:invite n'en est pas participant. La seule porte est l'ACL.
	msgs := seedConversation(t, pool, "ws1", "conv_privee", "private", 5)
	unit := seedUnitWindow(t, pool, "ws1", "conv_privee", msgs[4], msgs[2])
	grantRead(t, pool, unit, "agent:invite")

	applyPaulSalaire(t, gr, "conv_privee", msgs[0], msgs[4])

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:invite", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range got.Facts {
		t.Errorf("un fait sourcé hors de la fenêtre octroyée est ressorti: "+
			"%s %s %q, sources=%v", f.Subject, f.Predicate, f.Object,
			f.SourceMessageIDs)
	}
	if len(got.Facts) != 0 {
		t.Errorf("%d faits rendus, want 0", len(got.Facts))
	}
	if len(got.Candidates) != 0 {
		t.Errorf("%d candidats rendus, want 0", len(got.Candidates))
	}
}

// TestSearchGraphRendUnFaitEntierementDansLUniteOctroyee est la
// contre-épreuve du test ci-dessus: le bornage ne doit pas se contenter de
// tout refuser. Une relation dont toutes les sources tombent dans la fenêtre
// de l'unité octroyée reste rendue, avec explicit_acl pour raison.
func TestSearchGraphRendUnFaitEntierementDansLUniteOctroyee(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv_privee", "private", 5)
	unit := seedUnitWindow(t, pool, "ws1", "conv_privee", msgs[4], msgs[2])
	grantRead(t, pool, unit, "agent:invite")

	// M2 et M4 sont tous deux dans [2,4].
	applyPaulSalaire(t, gr, "conv_privee", msgs[2], msgs[4])

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:invite", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 {
		t.Fatalf("%d faits rendus, want 1: le bornage ne doit pas refuser un "+
			"fait entièrement couvert par l'unité octroyée", len(got.Facts))
	}
	if got.Facts[0].Object != "95000 euros" {
		t.Errorf("objet = %q, want %q", got.Facts[0].Object, "95000 euros")
	}
	if len(got.Candidates) != 1 {
		t.Fatalf("%d candidats rendus, want 1", len(got.Candidates))
	}
	if got.Candidates[0].AccessReason != "explicit_acl" {
		t.Errorf("access_reason = %q, want explicit_acl",
			got.Candidates[0].AccessReason)
	}
	wantStart := messageSequence(t, pool, msgs[2])
	wantEnd := messageSequence(t, pool, msgs[4])
	if got.Candidates[0].StartSequence != wantStart ||
		got.Candidates[0].EndSequence != wantEnd {
		t.Errorf("fenêtre = [%d,%d], want [%d,%d]",
			got.Candidates[0].StartSequence, got.Candidates[0].EndSequence,
			wantStart, wantEnd)
	}
}

// applyEtat écrit une observation (Paul, has_observed_state, état) sourcée par
// un message, dans le scope donné. Le type est à valeur unique par défaut,
// donc le recalcul de chaîne ferme l'observation précédente sur l'observed_at
// de celle-ci.
func applyEtat(t *testing.T, gr *GraphRepo, conv, scope, etat string,
	observed time.Time, msg uuid.UUID) {

	t.Helper()
	src := graph.EntityID("ws1", "person:paul")
	dk := graph.DedupKey(src, "has_observed_state", nil, etat, nil)
	if err := gr.Apply(context.Background(), memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: conv,
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul", Resolved: true}},
		Relations: []memory.GraphRelation{{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "has_observed_state",
			TargetLiteral: etat, ObservedAt: observed, Confidence: 1,
			Scope: scope, DedupKey: dk, ConversationID: conv,
			SourceMessageIDs: []uuid.UUID{msg}}},
	}, []string{"has_observed_state"}); err != nil {
		t.Fatalf("apply %q: %v", etat, err)
	}
}

// TestSearchGraphNeRendPasUnValidUntilPoseParUneObservationIllisible couvre le
// constat F2 de la revue finale de branche.
//
// recomputeChainTx recalcule la chaîne d'un couple sur toutes les
// observations non invalidées du workspace, sans filtre de conversation ni
// d'accès. C'est correct: c'est de la maintenance. graphSQL rend un fait dès
// qu'une de ses sources est lisible et rendait rel.valid_until tel quel. Ça
// aussi était correct en soi. Ensemble, les deux faisaient traverser une
// frontière de scope à une information que la relation privée, elle, ne
// traversait pas: le demandeur apprenait qu'à telle date exacte, dans une
// conversation qu'il ne peut pas lire, quelque chose avait supplanté ce fait.
//
// Ce n'est pas le canal accepté par le jugement 37. observed_at et confidence
// ne bougent que quand le même triplet est affirmé des deux côtés, donc sur la
// même ligne dédupliquée: rien de nouveau ne franchit, seulement
// l'horodatage d'une assertion identique. valid_until est posé par une
// relation différente, une ligne distincte que le SQL refuse correctement de
// rendre.
func TestSearchGraphNeRendPasUnValidUntilPoseParUneObservationIllisible(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	ouverts := seedConversation(t, pool, "ws1", "conv_ouverte", "participants", 1)
	fermes := seedConversation(t, pool, "ws1", "conv_fermee", "private", 1)
	addParticipant(t, pool, "conv_ouverte", "user:alice")

	janvier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	juin := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	applyEtat(t, gr, "conv_ouverte", "participants", "travaille chez acme",
		janvier, ouverts[0])
	applyEtat(t, gr, "conv_fermee", "private", "travaille chez globex",
		juin, fermes[0])

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 {
		t.Fatalf("%d faits rendus à alice, want 1", len(got.Facts))
	}
	if got.Facts[0].Object != "travaille chez acme" {
		t.Fatalf("objet = %q, want %q: la relation privée ne doit pas ressortir",
			got.Facts[0].Object, "travaille chez acme")
	}
	if got.Facts[0].ValidUntil != nil {
		t.Errorf("valid_until = %v, want nil: cette date est celle d'une "+
			"observation dont la seule source est dans une conversation que le "+
			"demandeur ne peut pas lire", got.Facts[0].ValidUntil.UTC())
	}
}

// TestSearchGraphRendUnValidUntilPoseParUneObservationLisible est la
// contre-épreuve: quand l'observation qui supplante est elle-même lisible par
// la même dérivation, sa date reste rendue. Sans ce test, remplacer le
// valid_until par un NULL inconditionnel passerait.
func TestSearchGraphRendUnValidUntilPoseParUneObservationLisible(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv_ouverte", "participants", 2)
	addParticipant(t, pool, "conv_ouverte", "user:alice")

	janvier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	juin := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	applyEtat(t, gr, "conv_ouverte", "participants", "travaille chez acme",
		janvier, msgs[0])
	applyEtat(t, gr, "conv_ouverte", "participants", "travaille chez globex",
		juin, msgs[1])

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 2 {
		t.Fatalf("%d faits rendus, want 2", len(got.Facts))
	}
	var ferme *memory.GraphFact
	for i := range got.Facts {
		if got.Facts[i].Object == "travaille chez acme" {
			ferme = &got.Facts[i]
		}
	}
	if ferme == nil {
		t.Fatal("le fait fermé n'est pas ressorti: le montage ne prouve rien")
	}
	if ferme.ValidUntil == nil {
		t.Fatal("valid_until = nil: la date posée par une observation lisible " +
			"doit rester rendue")
	}
	if !ferme.ValidUntil.UTC().Equal(juin) {
		t.Errorf("valid_until = %v, want %v", ferme.ValidUntil.UTC(), juin)
	}
}

// TestSearchGraphNeTraversePasUneAreteInvalidee couvre F5a de la revue finale.
//
// graphSQL porte deux invalidated_at IS NULL, pas un: celui de la CTE facts et
// celui de l'étape récursive de reachable. La revue de la tâche 8 avait
// signalé « les deux filtres de vivacité », le correcteur en a fermé un, et
// celui de reachable restait retirable seul avec la suite entière verte.
//
// L'effet fonctionnel réel de son absence: la traversée suivrait des arêtes
// que le service ne croit plus, donc une entité atteignable seulement par une
// arête retirée redeviendrait un nœud de la traversée, et ses autres
// relations encore valides ressortiraient. Ce n'est pas une fuite d'accès,
// ces relations ont toujours besoin d'une source lisible; c'est un
// élargissement du rappel par une preuve retirée.
//
// Le montage isole exactement ça: Paul -> Jardin est la seule arête vers
// Jardin et elle est invalidée, sa source restant vivante pour que le filtre
// de suppression des messages ne puisse rien écarter. Jardin porte par
// ailleurs une relation littérale parfaitement valide, sourcée par un message
// vivant et lisible. Avec la garde de reachable, Jardin n'est pas atteint et
// cette relation ne ressort pas. Sans elle, elle ressort.
//
// La mutation qui retire la garde de facts fait aussi rougir ce test, par
// l'arête invalidée elle-même; celle qui retire la garde de reachable ne fait
// rougir que lui, et c'est ce qui manquait.
func TestSearchGraphNeTraversePasUneAreteInvalidee(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 2)
	addParticipant(t, pool, "conv1", "user:alice")

	paul := graph.EntityID("ws1", "person:paul")
	jardin := graph.EntityID("ws1", "place:jardin")
	ents := []memory.GraphEntity{
		{EntityID: paul, WorkspaceID: "ws1", CanonicalKey: "person:paul",
			EntityType: "person", DisplayName: "Paul", Resolved: true},
		{EntityID: jardin, WorkspaceID: "ws1", CanonicalKey: "place:jardin",
			EntityType: "place", DisplayName: "Jardin", Resolved: true},
	}

	// L'arête Paul -> Jardin, seul chemin vers Jardin.
	cible := jardin
	arete := graph.DedupKey(paul, "owns", &cible, "", nil)
	areteID := graph.RelationID("ws1", arete)
	// Une relation littérale portée par Jardin, valide et lisible.
	feuille := graph.DedupKey(jardin, "has_observed_state", nil, "arrosé", nil)

	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1", Entities: ents,
		Relations: []memory.GraphRelation{
			{RelationID: areteID, WorkspaceID: "ws1", SourceEntityID: paul,
				RelationType: "owns", TargetEntityID: &cible,
				ObservedAt: time.Now().UTC(), Confidence: 1,
				Scope: "participants", DedupKey: arete, ConversationID: "conv1",
				SourceMessageIDs: []uuid.UUID{msgs[0]}},
			{RelationID: graph.RelationID("ws1", feuille), WorkspaceID: "ws1",
				SourceEntityID: jardin, RelationType: "has_observed_state",
				TargetLiteral: "arrosé", ObservedAt: time.Now().UTC(),
				Confidence: 1, Scope: "participants", DedupKey: feuille,
				ConversationID: "conv1", SourceMessageIDs: []uuid.UUID{msgs[1]}},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	// L'arête est invalidée par le vrai chemin d'écriture, puis la
	// suppression douce de sa source est annulée: le filtre de suppression
	// des messages ne peut alors plus rien écarter, et seule la garde
	// d'invalidation compte. Même procédé que
	// TestSearchGraphEcarteUneRelationInvalideeDontLaSourceEstVivante.
	softDelete(t, pool, msgs[0])
	if err := gr.Reevaluate(ctx, msgs[0], nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET deleted_at = NULL WHERE message_id = $1`,
		msgs[0]); err != nil {
		t.Fatal(err)
	}
	if at := relationInvalidatedAt(t, pool, areteID); at == nil {
		t.Fatal("l'arête n'a pas été invalidée: le test ne monte plus l'état " +
			"qu'il prétend monter")
	}

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: "", MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got.Facts {
		t.Errorf("un fait ressort par une arête invalidée: %s %s %q",
			f.Subject, f.Predicate, f.Object)
	}
	if len(got.Facts) != 0 || len(got.Candidates) != 0 {
		t.Errorf("%d faits, %d candidats, want 0 et 0: la traversée a suivi une "+
			"arête que le service ne croit plus", len(got.Facts), len(got.Candidates))
	}
}

// TestSearchGraphEtLExtractionPartagentLeMemeSeuilDeSimilarite couvre F5b.
//
// Les deux requêtes qui posent la même question dans les deux directions
// (une entité connue apparaît-elle dans ce texte) partagent
// candidateSimilarityThreshold exprès, l'une pour peupler le prompt
// d'extraction, l'autre pour semer la traversée. C'est l'argument explicite du
// commentaire de graphSQL, et rien ne l'épinglait: remplacer >= $7 par
// >= 0.9 en dur laissait la suite entière verte, parce que le seul test qui
// sème par trigramme le fait sur une paire très au-dessus de 0,9.
//
// Le fixture est donc calibré entre les deux valeurs. « Müller » contre une
// question qui écrit « Muller » sans tréma vaut 0,43 en word_similarity,
// mesuré contre la base: au-dessus du seuil partagé (0,4), très en dessous de
// 0,9. Les deux moitiés du test posent la même question aux deux requêtes,
// donc un seuil qui monte dans l'une ou dans l'autre le fait rougir.
func TestSearchGraphEtLExtractionPartagentLeMemeSeuilDeSimilarite(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv1", "workspace", 1)

	src := graph.EntityID("ws1", "person:muller")
	dk := graph.DedupKey(src, "likes", nil, "les tomates", nil)
	if err := gr.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:muller", EntityType: "person",
			DisplayName: "Müller", Resolved: true}},
		Relations: []memory.GraphRelation{{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "likes",
			TargetLiteral: "les tomates", ObservedAt: time.Now().UTC(),
			Confidence: 1, Scope: "workspace", DedupKey: dk,
			ConversationID: "conv1", SourceMessageIDs: []uuid.UUID{msgs[0]}}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	// La question ne nomme aucune clé d'identité: seule la branche trigramme
	// peut semer, et seulement si le seuil reste celui qui est partagé.
	const question = "Est-ce que Muller aime les tomates ?"

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Text: question, MaxHops: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 {
		t.Errorf("%d faits semés par trigramme, want 1: le seuil de graphSQL "+
			"n'est plus celui de candidateSimilarityThreshold", len(got.Facts))
	}

	cands, err := gr.CandidateEntities(ctx, "ws1", question, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Errorf("%d entités candidates, want 1: le seuil de "+
			"candidateEntitiesSQL a changé, et les deux requêtes doivent voir "+
			"la même valeur", len(cands))
	}
}

// applyLikesAvecFenetre écrit une relation (Paul, likes, objet) avec un
// valid_until posé à l'écriture, comme le fait l'extracteur pour un type qui
// n'est pas à valeur unique: la section 7.4 dit qu'un tel type voit son
// valid_until posé une fois par l'extracteur et jamais recalculé ensuite.
// singleValued est laissé vide exprès, donc aucun recalcul de chaîne ne
// touche cette ligne.
func applyLikesAvecFenetre(t *testing.T, gr *GraphRepo, conv, scope, objet string,
	observed time.Time, until *time.Time, msg uuid.UUID) {

	t.Helper()
	src := graph.EntityID("ws1", "person:paul")
	dk := graph.DedupKey(src, "likes", nil, objet, nil)
	if err := gr.Apply(context.Background(), memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: conv,
		Entities: []memory.GraphEntity{{EntityID: src, WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul", Resolved: true}},
		Relations: []memory.GraphRelation{{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "likes", TargetLiteral: objet,
			ObservedAt: observed, ValidUntil: until, Confidence: 1,
			Scope: scope, DedupKey: dk, ConversationID: conv,
			SourceMessageIDs: []uuid.UUID{msg}}},
	}, nil); err != nil {
		t.Fatalf("apply %q: %v", objet, err)
	}
}

// TestSearchGraphRendUnValidUntilPoseParLExtracteur est l'arbitrage rendu sur
// le prix du correctif F2.
//
// La règle de F2 masque un valid_until que le demandeur ne pourrait pas
// déduire des messages qu'il lit. Prise à la lettre, elle masquait aussi la
// date qu'un extracteur pose sur un type qui n'est pas à valeur unique
// (section 7.4), c'est-à-dire une date qu'un message parfaitement lisible
// affirme explicitement. La lettre de la règle contredisait donc son propre
// motif, qui est de rendre « exactement ce que le demandeur croirait des
// messages qu'il peut lire ».
//
// La distinction se fait sans colonne d'origine, et voici pourquoi elle
// fonctionne: un valid_until calculé par la chaîne vaut toujours, par
// construction, l'observed_at d'une autre observation du couple, puisque
// graph.Chain ferme chaque observation sur l'instant de la suivante. Une date
// posée par l'extracteur, elle, n'a aucune raison de coïncider avec l'une
// d'elles. « Aucune observation du couple ne porte cet instant » identifie
// donc la seconde sans avoir à l'enregistrer.
//
// Le cas de coïncidence est écrit, parce qu'il vaut mieux l'écrire que de
// laisser quelqu'un le découvrir: si une date posée par l'extracteur tombe par
// hasard sur l'observed_at d'une observation illisible du même couple, elle
// sera masquée. Voir
// TestSearchGraphMasqueUnValidUntilQuiCoincideAvecUneObservationIllisible.
func TestSearchGraphRendUnValidUntilPoseParLExtracteur(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv_ouverte", "participants", 1)
	addParticipant(t, pool, "conv_ouverte", "user:alice")

	// La date de fin ne coïncide avec l'observed_at d'aucune observation du
	// couple, et pour cause: il n'y en a qu'une.
	janvier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mars := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	applyLikesAvecFenetre(t, gr, "conv_ouverte", "participants",
		"le potager de printemps", janvier, &mars, msgs[0])

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 {
		t.Fatalf("%d faits rendus, want 1", len(got.Facts))
	}
	if got.Facts[0].ValidUntil == nil {
		t.Fatal("valid_until = nil: la date posée par l'extracteur est affirmée " +
			"par un message que le demandeur peut lire, et rien dans le couple " +
			"ne la contredit; la masquer perd de l'information sans rien protéger")
	}
	if !got.Facts[0].ValidUntil.UTC().Equal(mars) {
		t.Errorf("valid_until = %v, want %v", got.Facts[0].ValidUntil.UTC(), mars)
	}
}

// TestSearchGraphMasqueUnValidUntilQuiCoincideAvecUneObservationIllisible
// épingle le cas de coïncidence, qui est le prix restant de l'arbitrage et
// qu'il vaut mieux voir écrit que découvrir.
//
// La lecture ne distingue pas un valid_until posé par la chaîne d'un
// valid_until posé par l'extracteur: elle ne regarde que si une observation du
// couple porte cet instant. Une date d'extracteur qui tombe par hasard sur
// l'observed_at d'une observation illisible du même couple est donc masquée.
// C'est conservateur dans le bon sens: le doute joue en faveur du secret, et
// la seule chose perdue est une date que le demandeur peut de toute façon lire
// dans son propre message.
func TestSearchGraphMasqueUnValidUntilQuiCoincideAvecUneObservationIllisible(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	ouverts := seedConversation(t, pool, "ws1", "conv_ouverte", "participants", 1)
	fermes := seedConversation(t, pool, "ws1", "conv_fermee", "private", 1)
	addParticipant(t, pool, "conv_ouverte", "user:alice")

	janvier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mars := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	// Le fait lisible, avec sa fenêtre posée par l'extracteur.
	applyLikesAvecFenetre(t, gr, "conv_ouverte", "participants",
		"le potager de printemps", janvier, &mars, ouverts[0])
	// Et une autre observation du même couple (Paul, likes), illisible, dont
	// l'observed_at tombe exactement sur cette date de fin.
	applyLikesAvecFenetre(t, gr, "conv_fermee", "private",
		"les orties", mars, nil, fermes[0])

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 {
		t.Fatalf("%d faits rendus à alice, want 1: la relation privée ne doit "+
			"pas ressortir", len(got.Facts))
	}
	if got.Facts[0].Object != "le potager de printemps" {
		t.Fatalf("objet = %q, want %q", got.Facts[0].Object,
			"le potager de printemps")
	}
	if got.Facts[0].ValidUntil != nil {
		t.Errorf("valid_until = %v, want nil: la lecture ne distingue pas une "+
			"date d'extracteur d'une date de chaîne, et une observation "+
			"illisible du couple porte cet instant",
			got.Facts[0].ValidUntil.UTC())
	}
}

// TestSearchGraphMasqueUnValidUntilPoseParUneObservationInvalidee couvre le
// constat R3 de la re-revue, et il ferme la seule condition sous laquelle la
// prémisse de l'arbitrage F2 cessait de tenir.
//
// Cette prémisse est qu'un valid_until posé par la chaîne vaut l'observed_at
// d'une autre observation du couple. Elle tient tant que la chaîne est à jour,
// et elle cesse de tenir dès qu'une observation est invalidée sans que son
// couple soit recalculé. Ça arrive: le recalcul ne s'applique qu'aux types
// listés dans single_valued_relations, donc retirer un type de cette liste
// gèle les chaînes qu'il avait construites, et une invalidation ultérieure ne
// les rouvre plus.
//
// La ligne fermée garde alors un valid_until qui désigne une observation
// devenue invalidée. Une relation invalidée a toutes ses sources mortes, donc
// elle n'est lisible par personne, et cette date n'a plus aucune
// justification dans une observation que le demandeur puisse lire: la rendre
// revient à divulguer la date d'une observation qui n'existe plus pour lui.
// C'est pourquoi le test d'existence des frères de couple ne filtre plus les
// invalidées.
//
// La vraie cause n'est pas dans la lecture: ce valid_until périmé ne devrait
// pas rester sur la ligne, un recalcul de chaîne l'aurait effacé. Le masquage
// à la lecture n'est que le filet, et la lacune de maintenance est écrite en
// section 12.
func TestSearchGraphMasqueUnValidUntilPoseParUneObservationInvalidee(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	msgs := seedConversation(t, pool, "ws1", "conv_ouverte", "participants", 2)
	addParticipant(t, pool, "conv_ouverte", "user:alice")

	janvier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	juin := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Les deux observations sont écrites tant que le type est à valeur
	// unique, donc la chaîne ferme la première sur l'observed_at de la
	// seconde. Les deux sont dans une conversation qu'alice lit.
	applyEtat(t, gr, "conv_ouverte", "participants", "travaille chez acme",
		janvier, msgs[0])
	applyEtat(t, gr, "conv_ouverte", "participants", "travaille chez globex",
		juin, msgs[1])

	// La source de la seconde disparaît, et la réévaluation passe avec une
	// liste de types à valeur unique qui ne contient plus rien: c'est
	// exactement l'effet d'un has_observed_state retiré de la configuration
	// entre l'écriture et la réévaluation. invalidateSQL invalide la seconde
	// observation, et aucun recalcul ne rouvre la fenêtre de la première.
	softDelete(t, pool, msgs[1])
	if err := gr.Reevaluate(ctx, msgs[1], nil); err != nil {
		t.Fatal(err)
	}

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:alice", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 {
		t.Fatalf("%d faits rendus, want 1: l'observation invalidée ne doit pas "+
			"ressortir et la première doit rester", len(got.Facts))
	}
	if got.Facts[0].Object != "travaille chez acme" {
		t.Fatalf("objet = %q, want %q", got.Facts[0].Object, "travaille chez acme")
	}
	if got.Facts[0].ValidUntil != nil {
		t.Errorf("valid_until = %v, want nil: cette date a été posée par une "+
			"observation désormais invalidée, dont toutes les sources sont "+
			"mortes, donc lisible par personne",
			got.Facts[0].ValidUntil.UTC())
	}
}

// TestSearchGraphRetientUnFaitDontLaSourceHorsFenetreEstSupprimee épingle une
// asymétrie voulue du bornage explicit_acl, relevée par la re-revue comme
// constat R6 et jugée à conserver.
//
// Le prédicat ne filtre pas mw.deleted_at, alors que source_rows porte un
// filtre sur m.deleted_at juste à côté. Une source hors fenêtre qui a été
// supprimée continue donc de retenir le fait, définitivement, puisque rien ne
// retire jamais de ligne de graph_relation_sources.
//
// C'est le bon choix: le littéral que le modèle a levé de ce message survit
// dans graph_relations après sa suppression, donc le refuser à un demandeur
// qui n'a jamais eu accès qu'à la fenêtre octroyée est exactement ce que ce
// prédicat existe pour faire. Ce test est là pour que le prochain lecteur qui
// prendra l'asymétrie pour un oubli, et il le fera, voie rouge au lieu de
// livrer sa « correction ».
func TestSearchGraphRetientUnFaitDontLaSourceHorsFenetreEstSupprimee(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewSearchRepo(pool)
	gr := NewGraphRepo(pool)

	// Le montage de F1: la source M0 est hors de la fenêtre [M2, M4] de
	// l'unité octroyée, et le littéral n'est énoncé que là.
	msgs := seedConversation(t, pool, "ws1", "conv_privee", "private", 5)
	unit := seedUnitWindow(t, pool, "ws1", "conv_privee", msgs[4], msgs[2])
	grantRead(t, pool, unit, "agent:invite")
	applyPaulSalaire(t, gr, "conv_privee", msgs[0], msgs[4])

	// Puis M0 est supprimé. La relation survit: M4 est toujours vivant, donc
	// le NOT EXISTS d'invalidateSQL ne rend rien.
	softDelete(t, pool, msgs[0])
	if err := gr.Reevaluate(ctx, msgs[0], nil); err != nil {
		t.Fatal(err)
	}

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:invite", Limit: 20},
		Subjects: []string{"user:paul"}, Text: questionNeutre, MaxHops: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got.Facts {
		t.Errorf("le fait ressort après la suppression de sa source hors "+
			"fenêtre: %s %s %q. Le littéral vient de ce message et survit dans "+
			"graph_relations; sa suppression ne donne pas accès à un demandeur "+
			"qui n'a jamais eu que la fenêtre octroyée",
			f.Subject, f.Predicate, f.Object)
	}
	if len(got.Facts) != 0 || len(got.Candidates) != 0 {
		t.Errorf("%d faits, %d candidats, want 0 et 0",
			len(got.Facts), len(got.Candidates))
	}
}
