package postgres

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/graph"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// TestReevaluateConcurrentNInterbloquePas couvre le résiduel de la re-revue
// du tour de correction de la tâche 5.
//
// Le tri des couples (entité source, type) avant le recalcul des chaînes
// n'était gardé par aucun test qui morde. TestApplyConcurrentNInterbloquePas
// le manque pour une raison précise: Apply commence par l'upsert des
// entités, dont le verrou de ligne sérialise déjà les deux transactions
// avant qu'elles n'atteignent la boucle des couples. Retirer le tri des
// couples y laissait donc le test vert.
//
// Reevaluate n'a pas ce garde-fou: elle ne touche aucune entité. Deux
// réévaluations concurrentes qui déplacent les mêmes couples verrouillent
// donc dans l'ordre que Go donne au parcours d'une map, c'est-à-dire un
// ordre volontairement aléatoire, et interbloquent une fois sur deux sans le
// tri.
//
// Le montage évite délibérément de supprimer un message: invalidateSQL ne
// verrouille alors aucune ligne (son NOT EXISTS ne rend rien), ce qui laisse
// la boucle des couples seule responsable de l'ordre de verrouillage. Un
// message supprimé ferait prendre les verrous par l'UPDATE d'invalidation,
// dont le plan est le même dans les deux transactions, et masquerait à
// nouveau ce qu'on veut tester.
func TestReevaluateConcurrentNInterbloquePas(t *testing.T) {
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
	rel := func(src uuid.UUID, state string, observed time.Time,
		sources ...uuid.UUID) memory.GraphRelation {

		dk := graph.DedupKey(src, "has_observed_state", nil, state, nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "has_observed_state",
			TargetLiteral: state, ObservedAt: observed, Confidence: 1,
			Scope: "participants", DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: sources,
		}
	}

	single := []string{"has_observed_state"}
	t1 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// Les deux messages sourcent les mêmes deux couples, chacun avec deux
	// observations pour que la chaîne ait quelque chose à recalculer.
	// Réévaluer l'un ou l'autre déplace donc exactement le même ensemble de
	// couples, dans un ordre que seule la map décide.
	if err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: []memory.GraphEntity{
			ent(tomate, "plant:tomate", "Tomate"),
			ent(basilic, "plant:basilic", "Basilic"),
		},
		Relations: []memory.GraphRelation{
			rel(tomate, "green", t1, msgs[0], msgs[1]),
			rel(basilic, "green", t1, msgs[0], msgs[1]),
			rel(tomate, "red", t2, msgs[0], msgs[1]),
			rel(basilic, "red", t2, msgs[0], msgs[1]),
		},
	}, single); err != nil {
		t.Fatal(err)
	}

	const rounds = 40
	errs := make(chan error, 2*rounds)
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		for _, id := range msgs {
			wg.Add(1)
			go func(msgID uuid.UUID) {
				defer wg.Done()
				if err := repo.Reevaluate(ctx, msgID, single); err != nil {
					errs <- err
				}
			}(id)
		}
		wg.Wait()
	}
	close(errs)
	for err := range errs {
		t.Fatalf("reevaluate concurrent: %v", err)
	}
}

// TestApplyEtReevaluateConcurrentsNInterbloquentPas croise les deux chemins
// d'écriture du graphe. C'est le couple que personne n'avait testé, et c'est
// celui qui cassait: la revue finale l'a mesuré à 47 interblocages sur
// 60 rondes.
//
// Les deux tests de concurrence existants gardent chacun un chemin contre
// lui-même. Apply x Apply est sérialisé par le verrou de ligne que l'upsert
// des entités prend en tout premier; Reevaluate x Reevaluate est sérialisé par
// le tri des couples. Mais Reevaluate ne touche aucune ligne d'entité, donc le
// tri des entités ne sert à rien face à elle, et les deux chemins prenaient
// des verrous sur les mêmes lignes de graph_relations dans deux ordres
// différents: Apply dans l'ordre du slice e.Relations, qui est l'ordre de
// sortie du modèle et n'est trié par rien, Reevaluate dans l'ordre des couples
// puis celui de chainSelectSQL.
//
// Le montage est celui de la revue: deux entités sources distinctes, donc deux
// couples de chaîne, et Apply qui écrit ses relations dans l'ordre inverse de
// celui des couples. Aucun message n'est supprimé, pour que invalidateSQL ne
// verrouille rien (son NOT EXISTS ne rend aucune ligne) et que l'ordre de
// verrouillage soit celui des seules boucles qu'on veut tester.
//
// Apply ne réaffirme qu'une observation par couple alors que Reevaluate
// verrouille les deux de chaque couple: c'est ce qui rend l'entrelacement
// possible même quand chaque chemin est trié pour son propre compte, et c'est
// pour ça que le tri doit porter sur le couple d'abord et sur relation_id
// ensuite, exactement l'ordre de sortedChainPairs puis de chainSelectSQL.
func TestApplyEtReevaluateConcurrentsNInterbloquentPas(t *testing.T) {
	pool := newTestPoolWithConns(t, 8)
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
	rel := func(src uuid.UUID, state string, observed time.Time,
		sources ...uuid.UUID) memory.GraphRelation {

		dk := graph.DedupKey(src, "has_observed_state", nil, state, nil)
		return memory.GraphRelation{
			RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
			SourceEntityID: src, RelationType: "has_observed_state",
			TargetLiteral: state, ObservedAt: observed, Confidence: 1,
			Scope: "participants", DedupKey: dk, ConversationID: "conv1",
			SourceMessageIDs: sources,
		}
	}

	single := []string{"has_observed_state"}
	t1 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	entites := []memory.GraphEntity{
		ent(tomate, "plant:tomate", "Tomate"),
		ent(basilic, "plant:basilic", "Basilic"),
	}
	if err := repo.Apply(ctx, memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1", Entities: entites,
		Relations: []memory.GraphRelation{
			rel(tomate, "green", t1, msgs[0], msgs[1]),
			rel(basilic, "green", t1, msgs[0], msgs[1]),
			rel(tomate, "red", t2, msgs[0], msgs[1]),
			rel(basilic, "red", t2, msgs[0], msgs[1]),
		},
	}, single); err != nil {
		t.Fatal(err)
	}

	// L'ordre adverse est calculé, pas devine: c'est l'inverse strict de
	// celui de sortedChainPairs, qui trie les couples par les octets de
	// l'entite source. Un ordre ecrit en dur dependrait de la valeur d'un
	// hachage et le test cesserait de mordre le jour ou une cle change.
	sources := []uuid.UUID{tomate, basilic}
	slices.SortFunc(sources, func(a, b uuid.UUID) int {
		return bytes.Compare(b[:], a[:])
	})
	inverse := memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1", Entities: entites,
		Relations: []memory.GraphRelation{
			rel(sources[0], "red", t2, msgs[1]),
			rel(sources[1], "red", t2, msgs[1]),
		},
	}

	const rounds = 60
	errs := make(chan error, 2*rounds)
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := repo.Apply(ctx, inverse, single); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if err := repo.Reevaluate(ctx, msgs[0], single); err != nil {
				errs <- err
			}
		}()
		wg.Wait()
	}
	close(errs)

	n := 0
	for err := range errs {
		n++
		if n == 1 {
			t.Errorf("apply x reevaluate concurrents: %v", err)
		}
	}
	if n > 0 {
		t.Fatalf("%d erreurs sur %d rondes de deux goroutines", n, rounds)
	}
}

// TestSondeDeChargeApplyEtReevaluateSurUneInvalidation n'est pas un
// garde-fou, et son nom le dit maintenant. C'est la sonde qui a mesuré le
// défaut du chemin d'invalidation et sa correction, gardée pour ça et pour
// rien d'autre.
//
// Ce qu'elle a mesuré, et qui valait la peine: quand un message est réellement
// supprimé, l'UPDATE d'invalidation met à jour des lignes, donc les
// verrouille, et il le fait dans l'ordre que son plan produit, c'est-à-dire un
// ordre que personne n'a choisi. Apply, elle, verrouille les mêmes lignes dans
// l'ordre de sortedRelations. Deux ordres, mêmes lignes, 40P01. Mesuré à 29
// interblocages sur 60 rondes avant correctif, 0 après.
//
// Pourquoi ce n'est pas un garde-fou, et il faut être net là-dessus. La
// re-revue a repris cette sonde et montré qu'elle est sur une pente et pas sur
// un plateau: correctif retiré, elle donne 0 interblocage sur 60 rondes sous
// huit goroutines de charge CPU, deux fois de suite, et 0 à 20 ms comme à
// 160 ms de décalage, 1 à 120 ms. La durée d'un Apply de cette taille est de
// 64 à 72 ms selon la mesure, et non les 110 ms qu'un commentaire précédent
// affirmait: la fenêtre de 80 ms balayée ci-dessous dépasse donc déjà la
// cible. Autrement dit, ce test peut cesser de mordre sans cesser de passer,
// dès qu'une machine, une charge ou une version de Postgres décale les
// durées. Un test de concurrence qui rassure sans protéger est plus dangereux
// qu'un test absent, parce qu'on lui fait confiance.
//
// La garantie est ailleurs, dans graph_lockorder_test.go, et elle est
// déterministe: l'ordre de verrouillage est décidé en Go par des fonctions de
// tri, qui se testent en unitaire sans base, et le fait que les deux chemins
// prennent leurs verrous dans cet ordre-là s'épingle par inspection des
// sources. Les six mutations correspondantes sont rouges. C'est cela qui
// protège; ceci ne fait que documenter la mesure.
//
// La distinction avec TestApplyEtReevaluateConcurrentsNInterbloquentPas, qui
// reste un garde-fou, est mesurée elle aussi: celui-là ne dépend d'aucun
// calibrage temporel et il mord franchement même sous huit goroutines de
// charge CPU (19 et 23 interblocages sur 60 rondes, correctif retiré).
//
// Le montage, pour qui voudrait refaire la mesure. Le type de relation n'est
// pas à valeur unique exprès, pour qu'aucune boucle de chaîne n'entre en jeu
// et qu'il ne reste que le chemin d'invalidation. Les relations sont
// réaffirmées depuis le message déjà supprimé, jamais depuis un message
// vivant: ajouter une source vivante ferait retomber le NOT EXISTS et
// invalidateSQL cesserait de verrouiller quoi que ce soit. L'invalidated_at
// est remis à NULL au début de chaque ronde, le filtre invalidated_at IS NULL
// rendant l'opération idempotente. Les lignes sont écrites une par une dans
// l'ordre inverse de celui des couples, pour que leur ordre physique ne
// coïncide pas avec celui d'Apply. Et Reevaluate est décalée par pas de 4 ms
// sur 80 ms, parce que lancées ensemble les deux transactions ne se croisent
// jamais: Reevaluate fait deux requêtes et a commité pendant qu'Apply est
// encore dans ses upserts d'entités, qui viennent en premier par construction.
// Sans décalage: 0 interblocage sur 180 rondes, même en retournant
// délibérément l'ordre de verrouillage d'Apply.
func TestSondeDeChargeApplyEtReevaluateSurUneInvalidation(t *testing.T) {
	pool := newTestPoolWithConns(t, 8)
	ctx := context.Background()
	repo := NewGraphRepo(pool)
	msgs := seedConversation(t, pool, "ws1", "conv1", "participants", 1)

	var noms []string
	for i := 0; i < 120; i++ {
		noms = append(noms, fmt.Sprintf("plante-%03d", i))
	}
	var entites []memory.GraphEntity
	var relations []memory.GraphRelation
	var ids []uuid.UUID
	for _, nom := range noms {
		id := graph.EntityID("ws1", "plant:"+nom)
		entites = append(entites, memory.GraphEntity{EntityID: id,
			WorkspaceID: "ws1", CanonicalKey: "plant:" + nom,
			EntityType: "plant", DisplayName: nom, Resolved: true})
		dk := graph.DedupKey(id, "likes", nil, "l'eau", nil)
		relID := graph.RelationID("ws1", dk)
		ids = append(ids, relID)
		relations = append(relations, memory.GraphRelation{
			RelationID: relID, WorkspaceID: "ws1", SourceEntityID: id,
			RelationType: "likes", TargetLiteral: "l'eau",
			ObservedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
			Confidence: 1, Scope: "participants", DedupKey: dk,
			ConversationID: "conv1", SourceMessageIDs: []uuid.UUID{msgs[0]},
		})
	}

	extraction := memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Entities: entites, Relations: relations,
	}

	// Les lignes sont écrites une par une, dans l'ordre inverse de celui des
	// couples, pour que leur ordre physique dans la table soit l'inverse de
	// l'ordre de verrouillage d'Apply. C'est ce qui rend le montage adverse,
	// et ce n'est pas un artifice: l'ordre physique de graph_relations n'est
	// choisi par personne. Il vient de l'ordre d'arrivée des extractions,
	// étalées dans le temps, et il change encore avec la réutilisation des
	// pages. Un montage qui écrit tout d'un bloc aligne au contraire l'ordre
	// physique sur celui d'Apply et ne prouve rien: mesuré, 0 interblocage sur
	// 60 rondes avant correctif.
	inverse := make([]memory.GraphRelation, len(relations))
	copy(inverse, relations)
	slices.SortFunc(inverse, func(a, b memory.GraphRelation) int {
		return bytes.Compare(b.SourceEntityID[:], a.SourceEntityID[:])
	})
	for _, rel := range inverse {
		if err := repo.Apply(ctx, memory.GraphExtraction{
			WorkspaceID: "ws1", ConversationID: "conv1",
			Entities: entites, Relations: []memory.GraphRelation{rel},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}

	// La seule source des cent vingt relations disparaît: invalidateSQL a
	// désormais cent vingt lignes à mettre à jour, donc à verrouiller. Ce
	// commentaire a dit « quatre » pendant deux versions, reste d'un montage
	// antérieur, à trente lignes d'une explication de pourquoi il en faut cent
	// vingt et pas quatre. Sur un test dont toute la validité repose sur son
	// calibrage, sa documentation est tout ce qu'on a.
	softDelete(t, pool, msgs[0])

	const rounds = 60
	errs := make(chan error, 2*rounds)
	for i := 0; i < rounds; i++ {
		if _, err := pool.Exec(ctx,
			`UPDATE graph_relations SET invalidated_at = NULL
			 WHERE relation_id = ANY($1)`, ids); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := repo.Apply(ctx, extraction, nil); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i%20) * 4 * time.Millisecond)
			if err := repo.Reevaluate(ctx, msgs[0], nil); err != nil {
				errs <- err
			}
		}()
		wg.Wait()
	}
	close(errs)

	n := 0
	for err := range errs {
		n++
		if n == 1 {
			t.Errorf("apply x reevaluate sur une invalidation: %v", err)
		}
	}
	if n > 0 {
		t.Fatalf("%d erreurs sur %d rondes de deux goroutines", n, rounds)
	}
}
