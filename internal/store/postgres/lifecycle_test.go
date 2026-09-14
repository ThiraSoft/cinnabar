package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// hashEmbedder produit un vecteur reproductible à partir du texte. Il ne teste
// pas la sémantique, mais il valide toute la plomberie sans réseau.
type hashEmbedder struct{}

func (h *hashEmbedder) Model() string { return "hash-test-model" }

func (h *hashEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := h.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

func (h *hashEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		sum := sha256.Sum256([]byte(in))
		v := make([]float32, 768)
		for j := 0; j < 768; j++ {
			word := binary.BigEndian.Uint32(sum[(j*4)%28 : (j*4)%28+4])
			v[j] = float32(word%1000) / 1000
		}
		out[i] = v
	}
	return out, nil
}

// lifecycleConfig est partagée par lifecycleFixture et par le test de
// reconstruction, qui a besoin de la partie Indexing pour rejouer à la main
// ce que ferait le worker embed.
func lifecycleConfig() *config.Config {
	return &config.Config{
		Service:   config.Service{ConsistencyDefault: "searchable"},
		Embedding: config.Embedding{QueryPrefix: "search_query: ", Dimensions: 768},
		Indexing: config.Indexing{
			Strategy: "contextualized_message", PreviousMessages: 2,
			MaxChars: 1600, CharsPerToken: 4,
			IndexUserMessages: true, IndexAgentMessages: true, Version: 1,
		},
		Retrieval: config.Retrieval{
			DenseTopK: 20, LexicalTopK: 20, FinalTopK: 5,
			MaxMemoryTokens: 1200, ExpandBefore: 1, ExpandAfter: 1, RRFK: 60,
		},
		Graph:  config.Graph{Enabled: false},
		Access: config.Access{DefaultScope: "participants"},
	}
}

func lifecycleFixture(t *testing.T) (*memory.Ingester, *memory.Searcher, *MessageRepo) {
	t.Helper()
	pool := newTestPool(t)

	msgs := NewMessageRepo(pool)
	units := NewUnitRepo(pool)
	jobsRepo := NewJobRepo(pool)
	search := NewSearchRepo(pool)
	emb := &hashEmbedder{}

	cfg := lifecycleConfig()

	ing := memory.NewIngester(cfg, msgs, units, emb, jobsRepo)
	finder := memory.NewSearcher(cfg, msgs, emb, search, search, nil)
	return ing, finder, msgs
}

// Critère d'acceptation 1: un message en mode searchable est retrouvable au
// tour suivant.
func TestAcceptance1_SearchableIsImmediatelyRetrievable(t *testing.T) {
	ing, finder, _ := lifecycleFixture(t)
	ctx := context.Background()

	res, err := ing.Ingest(ctx, memory.AppendInput{
		WorkspaceID: "ws1", ConversationID: "conv_1",
		AuthorKey: "user:paul", Role: "user",
		Content: "le cultivar Rose de Berne pousse bien",
	}, "searchable")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "searchable" {
		t.Fatalf("status = %q, want searchable", res.Status)
	}

	found, err := finder.Search(ctx, memory.SearchRequest{
		WorkspaceID: "ws1", RequesterKey: "user:paul",
		Query: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Results) == 0 {
		t.Fatal("le message doit être retrouvable au tour suivant")
	}
	if len(found.Results[0].SourceMessageIDs) == 0 {
		t.Error("le résultat doit porter ses messages sources")
	}
}

// Critère d'acceptation 6: la suppression retire immédiatement le contenu.
func TestAcceptance6_DeletionRemovesImmediately(t *testing.T) {
	ing, finder, _ := lifecycleFixture(t)
	ctx := context.Background()

	res, err := ing.Ingest(ctx, memory.AppendInput{
		WorkspaceID: "ws1", ConversationID: "conv_1",
		AuthorKey: "user:paul", Role: "user",
		Content: "le cultivar Rose de Berne pousse bien",
	}, "searchable")
	if err != nil {
		t.Fatal(err)
	}

	before, err := finder.Search(ctx, memory.SearchRequest{
		WorkspaceID: "ws1", RequesterKey: "user:paul", Query: "Rose de Berne",
	})
	if err != nil || len(before.Results) == 0 {
		t.Fatalf("pré-condition: le souvenir doit être trouvable, %v", err)
	}

	if _, err := ing.DeleteMessage(ctx, res.Message.MessageID); err != nil {
		t.Fatal(err)
	}

	after, err := finder.Search(ctx, memory.SearchRequest{
		WorkspaceID: "ws1", RequesterKey: "user:paul", Query: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Results) != 0 {
		t.Errorf("%d résultats après suppression, want 0", len(after.Results))
	}
}

// TestAcceptance6_DeletionRemovesImmediatelyFromBothStrategies couvre le même
// critère mais force chaque stratégie séparément: la suppression doit
// retirer le message aussi bien du dense (via la désactivation immédiate de
// l'unité) que du lexical (qui interroge messages directement, où le soft
// delete pose deleted_at).
func TestAcceptance6_DeletionRemovesImmediatelyFromBothStrategies(t *testing.T) {
	ing, finder, _ := lifecycleFixture(t)
	ctx := context.Background()

	res, err := ing.Ingest(ctx, memory.AppendInput{
		WorkspaceID: "ws1", ConversationID: "conv_1",
		AuthorKey: "user:paul", Role: "user",
		Content: "j'ai planté un cornichon Vert de Massy",
	}, "searchable")
	if err != nil {
		t.Fatal(err)
	}

	for _, strategy := range []string{"dense", "lexical"} {
		found, err := finder.Search(ctx, memory.SearchRequest{
			WorkspaceID: "ws1", RequesterKey: "user:paul",
			Query: "Vert de Massy", Strategies: []string{strategy},
		})
		if err != nil || len(found.Results) == 0 {
			t.Fatalf("pré-condition (%s): le souvenir doit être trouvable, %v", strategy, err)
		}
	}

	if _, err := ing.DeleteMessage(ctx, res.Message.MessageID); err != nil {
		t.Fatal(err)
	}

	for _, strategy := range []string{"dense", "lexical"} {
		found, err := finder.Search(ctx, memory.SearchRequest{
			WorkspaceID: "ws1", RequesterKey: "user:paul",
			Query: "Vert de Massy", Strategies: []string{strategy},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(found.Results) != 0 {
			t.Errorf("stratégie %s: %d résultats après suppression, want 0",
				strategy, len(found.Results))
		}
	}
}

// Critère d'acceptation 8: le contexte injecté respecte le budget de tokens.
//
// Le test tenait ce budget à vide, et c'est la revue finale qui l'a montré:
// avec les huit messages ci-dessous, les extraits se recollent en un seul de
// 639 caractères, soit environ 160 tokens. Sous ce seuil FitBudget écarte
// l'extrait entier plutôt que de le couper, donc la recherche rendait zéro
// résultat et la somme des caractères valait zéro, quoi que fasse le budget.
// La seule mutation qui le faisait rougir était celle qui gonfle FitBudget;
// une récupération devenue muette le laissait vert.
//
// Les deux bornes sont donc épinglées ensemble, de part et d'autre du seuil
// mesuré: au-dessus, un extrait est rendu et il tient dans le budget;
// au-dessous, il est écarté et non tronqué, ce qui est la lettre du critère 7.
func TestAcceptance8_TokenBudgetIsRespected(t *testing.T) {
	// Le contenu et le nombre de messages fixent la taille de l'extrait
	// recollé, donc les deux budgets ci-dessous. Les changer demande de
	// remesurer.
	const (
		messages     = 8
		content      = "le cultivar Rose de Berne pousse bien cette année dans mon potager"
		excerptChars = 639 // mesuré: les huit messages se recollent en un extrait
		large        = 200 // tokens, soit 800 caractères: l'extrait passe
		serre        = 150 // tokens, soit 600 caractères: il ne passe pas
	)

	rechercher := func(t *testing.T, budget int) memory.SearchResponse {
		t.Helper()
		ing, finder, _ := lifecycleFixture(t)
		ctx := context.Background()
		for i := 0; i < messages; i++ {
			if _, err := ing.Ingest(ctx, memory.AppendInput{
				WorkspaceID: "ws1", ConversationID: "conv_1",
				AuthorKey: "user:paul", Role: "user", Content: content,
			}, "searchable"); err != nil {
				t.Fatal(err)
			}
		}
		found, err := finder.Search(ctx, memory.SearchRequest{
			WorkspaceID: "ws1", RequesterKey: "user:paul",
			Query: "Rose de Berne", TokenBudget: budget,
			IncludeContextBlock: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return found
	}

	t.Run("budget suffisant", func(t *testing.T) {
		found := rechercher(t, large)
		// Le compte d'abord: un budget tenu par une récupération vide n'est
		// pas un budget tenu.
		if len(found.Results) != 1 {
			t.Fatalf("%d résultats, want 1: la somme des caractères ne prouve "+
				"rien sur une liste vide", len(found.Results))
		}
		chars := 0
		for _, r := range found.Results {
			chars += len(r.Content)
		}
		if chars != excerptChars {
			t.Errorf("%d caractères rendus, %d mesurés: le montage a changé et "+
				"les deux budgets sont à remesurer", chars, excerptChars)
		}
		if chars > large*4 {
			t.Errorf("%d caractères rendus, budget = %d tokens soit %d caractères",
				chars, large, large*4)
		}
		if found.ContextBlock == "" {
			t.Error("context_block vide alors qu'un extrait est rendu")
		}
	})

	t.Run("budget trop serré", func(t *testing.T) {
		found := rechercher(t, serre)
		// L'extrait ne tient pas: il est écarté en entier, jamais coupé.
		// C'est ce que la mutation qui gonfle FitBudget casse, avec ses
		// 639 caractères rendus pour un budget de 600.
		if len(found.Results) != 0 {
			chars := 0
			for _, r := range found.Results {
				chars += len(r.Content)
			}
			t.Errorf("%d résultats et %d caractères rendus pour un budget de "+
				"%d tokens soit %d caractères: un extrait qui ne tient pas doit "+
				"être écarté, pas coupé", len(found.Results), chars, serre, serre*4)
		}
	})
}

// Critère d'acceptation 2 et 3 en version plomberie: la même requête doit
// trouver par le lexical même quand le dense est inutile. La qualité
// sémantique réelle est mesurée par la commande d'évaluation, tâche 17.
func TestLexicalStillWorksWithUselessEmbeddings(t *testing.T) {
	ing, finder, _ := lifecycleFixture(t)
	ctx := context.Background()

	if _, err := ing.Ingest(ctx, memory.AppendInput{
		WorkspaceID: "ws1", ConversationID: "conv_1",
		AuthorKey: "user:paul", Role: "user",
		Content: "j'ai acheté un motoculteur Staub PP2X",
	}, "searchable"); err != nil {
		t.Fatal(err)
	}

	found, err := finder.Search(ctx, memory.SearchRequest{
		WorkspaceID: "ws1", RequesterKey: "user:paul",
		Query: "Staub PP2X", Strategies: []string{"lexical"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Results) == 0 {
		t.Error("un terme rare doit être retrouvé par le lexical seul")
	}
}

// TestEditThenRebuildMakesOldTextUnfindableAndNewTextFindable couvre le
// cycle complet noté en tâche 7: index, invalidation, reconstruction. Sans
// lui, la réactivation d'une unité désactivée (par opposition à son maintien
// actif) ne serait jamais prouvée par les tests existants.
func TestEditThenRebuildMakesOldTextUnfindableAndNewTextFindable(t *testing.T) {
	ing, finder, msgs := lifecycleFixture(t)
	ctx := context.Background()

	res, err := ing.Ingest(ctx, memory.AppendInput{
		WorkspaceID: "ws1", ConversationID: "conv_1",
		AuthorKey: "user:paul", Role: "user",
		Content: "le cultivar Rose de Berne pousse bien",
	}, "searchable")
	if err != nil {
		t.Fatal(err)
	}

	// Les vérifications de contenu ci-dessous se font par le lexical seul.
	// hashEmbedder ne rend qu'un vecteur reproductible, pas sémantique
	// (voir son commentaire): en dense, sans minimum_score configuré, la
	// meilleure unité active de la conversation ressort toujours comme
	// candidate, même quand son texte n'a plus rien à voir avec la requête.
	// Le lexical, lui, s'appuie sur le tsvector du contenu réel et reste
	// donc le seul juge fiable de "ce texte est présent ou non" ici.
	lexicalQuery := func(q string) int {
		found, err := finder.Search(ctx, memory.SearchRequest{
			WorkspaceID: "ws1", RequesterKey: "user:paul",
			Query: q, Strategies: []string{"lexical"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return len(found.Results)
	}

	if lexicalQuery("Rose de Berne") == 0 {
		t.Fatal("pré-condition: le souvenir doit être trouvable")
	}

	// Capturé avant l'édition: c'est cet identifiant précis, pas seulement
	// "une" unité active, que la reconstruction doit faire réapparaître
	// (voir la vérification finale).
	var originalUnitID uuid.UUID
	if err := msgs.pool.QueryRow(ctx,
		`SELECT memory_unit_id FROM memory_units WHERE conversation_id = 'conv_1'`,
	).Scan(&originalUnitID); err != nil {
		t.Fatal(err)
	}

	editRes, err := ing.EditMessage(ctx, res.Message.MessageID, "le cultivar Noire de Crimée pousse bien")
	if err != nil {
		t.Fatal(err)
	}
	if len(editRes.DeactivatedAnchors) == 0 {
		t.Fatal("l'édition doit désactiver au moins une unité")
	}

	// Le contenu du message est déjà mis à jour en base par Edit, donc le
	// lexical (qui interroge messages directement) ne trouve plus l'ancien
	// texte tout de suite, sans attendre la reconstruction de l'unité.
	if n := lexicalQuery("Rose de Berne"); n != 0 {
		t.Errorf("%d résultats pour l'ancien texte après édition, want 0", n)
	}

	// La reconstruction est ce que ferait le worker embed (jobs.EmbedHandler):
	// recharger le message, recharger sa fenêtre de contexte, reconstruire
	// l'unité et la upserter. On le rejoue ici à la main avec les mêmes
	// briques (memory.BuildUnit, UnitRepo.Upsert) plutôt que d'importer le
	// paquet jobs, qui importerait ce paquet-ci en retour. La fenêtre est
	// rechargée via Around exactement comme EmbedHandler le fait, pas passée
	// à nil: sans ça, le seul endroit où la citation d'un voisin pourrait
	// apparaître dans le texte reconstruit ne serait jamais exercé.
	cfg := lifecycleConfig()
	anchor, err := msgs.ByID(ctx, res.Message.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	from := anchor.SequenceNumber - int64(cfg.Indexing.PreviousMessages)
	if from < 0 {
		from = 0
	}
	window, err := msgs.Around(ctx, anchor.ConversationID, from, anchor.SequenceNumber)
	if err != nil {
		t.Fatal(err)
	}
	previous := make([]memory.Message, 0, len(window))
	for _, m := range window {
		if m.MessageID != anchor.MessageID {
			previous = append(previous, m)
		}
	}
	scope, err := msgs.ConversationScope(ctx, anchor.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	participants, err := msgs.Participants(ctx, anchor.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	emb := &hashEmbedder{}
	unit, indexable := memory.BuildUnit(cfg.Indexing, emb.Model(),
		scope, participants, anchor, previous)
	if !indexable {
		t.Fatal("le message édité doit rester indexable")
	}
	units := NewUnitRepo(msgs.pool)
	vecs, err := emb.Embed(ctx, []string{unit.EmbeddingText})
	if err != nil {
		t.Fatal(err)
	}
	if err := units.Upsert(ctx, unit, vecs[0]); err != nil {
		t.Fatal(err)
	}

	if n := lexicalQuery("Rose de Berne"); n != 0 {
		t.Errorf("%d résultats pour l'ancien texte après reconstruction, want 0", n)
	}
	if lexicalQuery("Noire de Crimée") == 0 {
		t.Error("le nouveau texte doit être trouvable une fois la reconstruction faite")
	}

	// La reconstruction doit avoir réactivé la MÊME unité que celle
	// désactivée par l'édition, pas seulement en avoir créé une neuve à côté
	// (auquel cas l'ancienne resterait inactive et ce compte serait déjà
	// vrai sans prouver la réactivation). L'identifiant est déterministe:
	// c'est cette égalité qui distingue "réactivée" de "remplacée par une
	// nouvelle ligne".
	var reactivatedID uuid.UUID
	var activeCount int
	rows, err := msgs.pool.Query(ctx,
		`SELECT memory_unit_id FROM memory_units WHERE conversation_id = 'conv_1' AND active`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		if err := rows.Scan(&reactivatedID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		activeCount++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("%d unités actives après reconstruction, want 1", activeCount)
	}
	if reactivatedID != originalUnitID {
		t.Errorf("unité active après reconstruction = %s, want %s (la même, réactivée)",
			reactivatedID, originalUnitID)
	}
}
