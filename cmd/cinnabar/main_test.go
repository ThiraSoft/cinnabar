package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

// TestDrainLateErrorReturnsBufferedError couvre le correctif de la revue:
// une erreur écrite sur errCh après le select principal de run() (par
// exemple un Shutdown qui dépasse service.shutdown_grace pendant qu'une
// requête est encore en vol) doit être récupérée plutôt que rester dans un
// canal que plus personne ne lit, ce qui ferait passer un arrêt en échec
// pour un arrêt propre.
func TestDrainLateErrorReturnsBufferedError(t *testing.T) {
	ch := make(chan error, 1)
	want := errors.New("http server: shutdown deadline exceeded")
	ch <- want

	if got := drainLateError(ch); got != want {
		t.Errorf("drainLateError = %v, want %v", got, want)
	}
}

// TestDrainLateErrorReturnsNilWithoutBlocking prouve le second aspect du
// correctif: en l'absence d'erreur, l'appel rend nil immédiatement plutôt
// que de bloquer en attendant une écriture qui n'arrivera jamais.
func TestDrainLateErrorReturnsNilWithoutBlocking(t *testing.T) {
	ch := make(chan error, 1)
	if err := drainLateError(ch); err != nil {
		t.Errorf("drainLateError = %v, want nil", err)
	}
}

// TestRunCLIRejectsUnknownSubcommand couvre le correctif de la revue: une
// sous-commande inconnue ("key" au lieu de "keys", une simple faute de
// frappe) doit échouer avec un message d'usage plutôt que de laisser main()
// démarrer le service à la place. runCLI rejette avant de toucher à la
// configuration, donc ce test n'a besoin ni de fichier ni de base.
func TestRunCLIRejectsUnknownSubcommand(t *testing.T) {
	err := runCLI("unused.yaml", []string{"key", "create"})
	if err == nil {
		t.Fatal("une sous-commande inconnue doit être rejetée")
	}
}

// TestEmbeddingStartupErrorSeparatesUnreachableFromMismatch : une sonde qui
// n'atteint pas le modèle laisse démarrer en mode dégradé (avec un
// avertissement bruyant), un écart de dimension reste fatal. Voir la section
// 4.5 de la spec pour le second, et 5.5 / 8.2 pour la posture sur
// l'embedder.
func TestEmbeddingStartupErrorSeparatesUnreachableFromMismatch(t *testing.T) {
	if err := embeddingStartupError(nil); err != nil {
		t.Errorf("aucune erreur de sonde: %v", err)
	}

	unreachable := fmt.Errorf("%w: %w", postgres.ErrEmbeddingProbeUnavailable,
		errors.New("connection refused"))
	if err := embeddingStartupError(unreachable); err != nil {
		t.Errorf("une sonde injoignable ne doit pas empêcher le démarrage: %v", err)
	}

	mismatch := errors.New("embedding dimension mismatch: ...")
	if err := embeddingStartupError(mismatch); err == nil {
		t.Error("un écart de dimension doit rester fatal")
	}
}

// TestValidateRunModes : -api=false -workers=false ne câble rien et laissait
// le processus bloqué sur un wg.Wait() que rien n'allait débloquer.
func TestValidateRunModes(t *testing.T) {
	if err := validateRunModes(false, false); err == nil {
		t.Error("désactiver les deux composants doit être une faute d'usage")
	}
	for _, c := range [][2]bool{{true, true}, {true, false}, {false, true}} {
		if err := validateRunModes(c[0], c[1]); err != nil {
			t.Errorf("api=%v workers=%v: %v", c[0], c[1], err)
		}
	}
}

// TestWireGraphDisabledLeavesGraphSearcherNil épingle le piège documenté dans
// wireGraph : affecter un *postgres.SearchRepo nil à une variable
// d'interface memory.GraphSearcher sans condition produirait une interface
// non nulle (un pointeur nil typé, pas une interface nil), et
// memory.Searcher.Search finirait par appeler une méthode sur un récepteur
// nil à chaque recherche. graphRepo et extractor sont les témoins faciles
// (leur zéro-valeur est un pointeur nil ordinaire, un mauvais wireGraph les
// laisserait nil aussi) ; graphFinder est celui qui compte vraiment.
func TestWireGraphDisabledLeavesGraphSearcherNil(t *testing.T) {
	cfg := &config.Config{}
	cfg.Graph.Enabled = false

	graphRepo, graphFinder, extractor := wireGraph(cfg, nil, nil)

	if graphRepo != nil {
		t.Error("graph.enabled=false doit laisser graphRepo nil")
	}
	if extractor != nil {
		t.Error("graph.enabled=false doit laisser extractor nil")
	}
	if graphFinder != nil {
		t.Fatal("graph.enabled=false doit laisser un GraphSearcher nil, pas un " +
			"pointeur nil typé glissé dans l'interface: voir le commentaire de wireGraph")
	}
}

// TestWireGraphEnabledWiresGraphSearcher est le contrepoint : activé, avec un
// searchRepo réel, graphFinder doit porter la stratégie de recherche par
// graphe, pas rester nil par excès de prudence.
func TestWireGraphEnabledWiresGraphSearcher(t *testing.T) {
	cfg := &config.Config{}
	cfg.Graph.Enabled = true
	searchRepo := postgres.NewSearchRepo(nil)

	graphRepo, graphFinder, extractor := wireGraph(cfg, nil, searchRepo)

	if graphRepo == nil {
		t.Error("graph.enabled=true doit construire un graphRepo")
	}
	if extractor == nil {
		t.Error("graph.enabled=true doit construire un extractor")
	}
	if graphFinder == nil {
		t.Fatal("graph.enabled=true doit câbler un GraphSearcher")
	}
}

// TestSearchWithDisabledGraphAnswersWithoutError branche le graphFinder rendu
// par wireGraph (graphe désactivé) dans un Searcher réel et vérifie qu'une
// recherche répond sans erreur.
//
// Ce test ne prouve pas ce qu'on pourrait croire, et la revue de la tâche 10
// l'a établi par mutation: il ne rougit pas si on glisse un pointeur nil typé
// dans l'interface. La raison est que search.go teste
// s.cfg.Graph.Enabled avant s.graph, et que la configuration est ici
// désactivée: la goroutine du graphe n'est jamais lancée, donc le récepteur
// nil n'est jamais déréférencé, quelle que soit la valeur de l'interface.
//
// Ce qui attrape vraiment le piège du nil typé, c'est
// TestWireGraphDisabledLeavesGraphSearcherNil, qui inspecte l'interface
// elle-même. Celui-ci garde autre chose, qui compte aussi: qu'un déploiement
// sans graphe réponde normalement de bout en bout.
func TestSearchWithDisabledGraphAnswersWithoutError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Graph.Enabled = false
	cfg.Retrieval.FinalTopK = 5
	cfg.Retrieval.MaxMemoryTokens = 1200
	cfg.Indexing.CharsPerToken = 4

	_, graphFinder, _ := wireGraph(cfg, nil, nil)

	finder := memory.NewSearcher(cfg, nil, nil, nil, nil, graphFinder)

	resp, err := finder.Search(context.Background(), memory.SearchRequest{
		WorkspaceID:  "ws1",
		RequesterKey: "user:paul",
		Query:        "peu importe",
	})
	if err != nil {
		t.Fatalf("Search avec le graphe désactivé ne doit pas échouer: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Errorf("aucun repo câblé: len(Results) = %d, want 0", len(resp.Results))
	}
}
