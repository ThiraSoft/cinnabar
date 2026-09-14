// Commande cinnabar: service de mémoire hybride pour agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/api"
	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/graph"
	"github.com/ThiraSoft/cinnabar/internal/jobs"
	"github.com/ThiraSoft/cinnabar/internal/llm"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
	"github.com/ThiraSoft/cinnabar/internal/version"
)

func main() {
	var (
		configPath  = flag.String("config", "config.yaml", "chemin du fichier de configuration")
		runAPI      = flag.Bool("api", true, "servir l'API HTTP")
		runWorkers  = flag.Bool("workers", true, "exécuter les workers de jobs")
		showVersion = flag.Bool("version", false, "afficher la version et sortir")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("cinnabar %s (%s, %s)\n",
			version.Version, version.Commit, version.BuildDate)
		return
	}

	// Une sous-commande (aujourd'hui seule "keys" existe) est un outil
	// d'exploitation lancé à la main par un opérateur dans un terminal: ses
	// erreurs restent du texte simple en français sur stderr, comme avant
	// le câblage complet du service, plutôt que des lignes slog structurées
	// pensées pour un agrégateur de logs. Toute sous-commande inconnue
	// (une simple faute de frappe: "key" au lieu de "keys") est un usage
	// invalide qui doit s'arrêter là, pas un signal silencieux pour
	// démarrer le service à la place.
	if args := flag.Args(); len(args) > 0 {
		if err := runCLI(*configPath, args); err != nil {
			fmt.Fprintf(os.Stderr, "erreur: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Faute d'usage de l'opérateur, comme une sous-commande inconnue: texte
	// simple en français sur stderr et code 2 (celui du paquet flag pour un
	// usage invalide), pas une ligne slog. Sans ce garde-fou, le processus
	// démarrait, ne câblait ni API ni workers, et restait bloqué sur un
	// wg.Wait() que rien n'allait jamais débloquer.
	if err := validateRunModes(*runAPI, *runWorkers); err != nil {
		fmt.Fprintf(os.Stderr, "erreur: %v\n", err)
		os.Exit(2)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := run(*configPath, *runAPI, *runWorkers); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// runCLI route les sous-commandes d'exploitation. Une erreur ici est une
// faute d'usage de l'opérateur (commande inconnue, arguments manquants),
// jamais une panne du service: main() l'affiche en clair sur stderr plutôt
// que de la faire passer par slog.
func runCLI(configPath string, args []string) error {
	if args[0] != "keys" {
		return fmt.Errorf(
			"commande inconnue %q: usage: cinnabar [-config <chemin>] keys create|revoke ...",
			args[0])
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runKeys(ctx, cfg, args[1:])
}

// validateRunModes refuse la combinaison qui ne laisse rien à exécuter.
// Isolée de main() pour être testable sans démarrer quoi que ce soit.
func validateRunModes(runAPI, runWorkers bool) error {
	if !runAPI && !runWorkers {
		return fmt.Errorf("-api=false et -workers=false ensemble ne laissent " +
			"rien à exécuter: activez au moins l'un des deux")
	}
	return nil
}

// run démarre le service (API HTTP, workers de jobs, ou les deux) et bloque
// jusqu'à l'arrêt. Toute erreur d'ici est une panne opérationnelle,
// journalisée en JSON par main() pour un agrégateur de logs plutôt
// qu'affichée pour un humain.
func run(configPath string, runAPI, runWorkers bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := postgres.Migrate(ctx, pool); err != nil {
		return err
	}

	embedder, err := llm.OpenEmbedder(cfg.Embedding)
	if err != nil {
		return err
	}
	defer embedder.Close()
	// Un écart entre le modèle, la configuration et la colonne VECTOR(n)
	// doit empêcher le démarrage: sinon il ne se révèle qu'à la première
	// écriture, sous la forme d'une erreur Postgres obscure. Une sonde qui
	// n'atteint pas le modèle, elle, laisse démarrer: voir
	// embeddingStartupError.
	if err := embeddingStartupError(postgres.VerifyEmbeddingDimension(
		ctx, pool, embedder, cfg.Embedding.Dimensions)); err != nil {
		return err
	}

	msgs := postgres.NewMessageRepo(pool)
	units := postgres.NewUnitRepo(pool)
	jobRepo := postgres.NewJobRepo(pool)
	searchRepo := postgres.NewSearchRepo(pool)
	clients := postgres.NewClientRepo(pool)
	convs := postgres.NewConversationRepo(pool)
	aclRepo := postgres.NewACLRepo(pool)
	ops := postgres.NewOps(pool, jobRepo)

	ingester := memory.NewIngester(cfg, msgs, units, embedder, jobRepo)
	// Le graphe est optionnel. Quand il est désactivé, graphRepo et
	// extractor restent nil (ils ne servent qu'aux handlers de job
	// ci-dessous) et graphFinder est un GraphSearcher nil, pas un pointeur
	// nil typé glissé dans l'interface: voir le commentaire de wireGraph.
	graphRepo, graphFinder, extractor := wireGraph(cfg, pool, searchRepo)
	finder := memory.NewSearcher(cfg, msgs, embedder, searchRepo, searchRepo, graphFinder)
	// Le réordonnanceur est le seul appel de modèle du chemin de recherche,
	// interdit par le critère 9 d'être nécessaire: il n'est câblé que si la
	// configuration le demande, et son absence ne change rien au reste.
	if cfg.Rerank.Enabled {
		finder = finder.WithReranker(llm.NewReranker(cfg.Rerank))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	if runAPI {
		// NewServer construit son *http.Server interne; ListenAndServe(ctx)
		// sert ensuite et se charge lui-même de l'arrêt gracieux quand ctx
		// s'annule, avec le délai configuré. Rien d'autre à orchestrer ici:
		// pas de goroutine séparée pour Shutdown qui entrerait en course
		// avec celle qui sert.
		srv := api.NewServer(cfg, clients, ingester, ingester, finder, convs, ops).
			WithACL(aclRepo, convs)
		wg.Add(1)
		go func() {
			defer wg.Done()
			slog.Info("http server starting",
				"addr", cfg.Service.Listen, "version", version.Version)
			if err := srv.ListenAndServe(ctx); err != nil {
				errCh <- fmt.Errorf("http server: %w", err)
			}
		}()
	}

	if runWorkers {
		// Runner.Run(ctx) laisse chaque worker finir le job qu'il tient
		// avant de rendre la main: un SIGTERM ne l'interrompt jamais en
		// plein traitement.
		handlers := map[string]jobs.Handler{
			"embed": jobs.EmbedHandler(cfg, msgs, units, embedder),
		}
		// Un job graph_extract ou graph_reeval resté en file d'une
		// exécution précédente (graphe activé puis redésactivé) ne trouve
		// alors plus de handler enregistré: il tombe dans le circuit
		// générique de Runner.handle, échoue proprement et part en lettre
		// morte après jobs.retry_limit tentatives, sans bloquer la file ni
		// les jobs embed qui continuent d'être traités normalement.
		if cfg.Graph.Enabled {
			handlers["graph_extract"] = jobs.GraphExtractHandler(cfg, msgs, graphRepo, extractor, embedder)
			handlers["graph_reeval"] = jobs.GraphReevalHandler(cfg, graphRepo)
		}
		runner := jobs.NewRunner(jobRepo, cfg.Jobs, handlers)
		wg.Add(1)
		go func() {
			defer wg.Done()
			slog.Info("job runner starting", "workers", cfg.Jobs.Workers)
			runner.Run(ctx)
		}()
	}

	// L'un ou l'autre composant peut déclencher l'arrêt: un signal (ctx
	// s'annule de lui-même) ou une panne de l'un des deux (errCh). Dans le
	// second cas, stop() annule ctx à la main pour que le composant restant
	// s'arrête proprement lui aussi, au lieu de tourner seul indéfiniment.
	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case runErr = <-errCh:
		slog.Error("component failed, shutting down", "error", runErr)
		stop()
	}

	wg.Wait()

	// Une erreur peut aussi arriver après ce select: ListenAndServe qui
	// dépasse le délai de grâce configuré (Shutdown rend alors une erreur
	// de deadline, précisément quand des requêtes étaient encore en vol)
	// n'écrit dans errCh que juste avant que sa goroutine ne rende la main,
	// donc éventuellement après que wg.Wait() a débloqué. drainLateError
	// l'attrape plutôt que de la laisser dans un canal que plus personne ne
	// lit: sans ça, un arrêt qui a coupé des requêtes en vol se
	// terminerait quand même par "stopped", comme un arrêt propre.
	if runErr == nil {
		runErr = drainLateError(errCh)
	}
	if runErr != nil {
		return runErr
	}
	slog.Info("stopped")
	return nil
}

// wireGraph construit les dépendances optionnelles du graphe de
// connaissances. Isolée de run() pour être testable sans base ni serveur
// HTTP: voir main_test.go pour le piège qu'elle existe pour éviter.
//
// Quand graph.enabled vaut false, les trois valeurs rendues restent nil,
// graphFinder en particulier: c'est une variable d'interface
// (memory.GraphSearcher) simplement jamais affectée, donc réellement nil,
// pas un *postgres.SearchRepo nil glissé dans l'interface. La distinction
// compte parce qu'une interface qui porte un pointeur nil typé n'est pas
// une interface nil: un `if s.graph != nil` dans internal/memory serait
// vrai quand même, et search.go finirait par appeler une méthode sur un
// récepteur nil à chaque recherche. D'où la variable de retour déclarée du
// type de l'interface et laissée à sa valeur zéro sur la branche
// désactivée, plutôt qu'un pointeur nil affecté sans condition.
func wireGraph(cfg *config.Config, pool *pgxpool.Pool, searchRepo *postgres.SearchRepo) (
	graphRepo *postgres.GraphRepo, graphFinder memory.GraphSearcher, extractor memory.GraphExtractor) {

	if !cfg.Graph.Enabled {
		return nil, nil, nil
	}
	graphRepo = postgres.NewGraphRepo(pool)
	graphFinder = searchRepo
	extractor = graph.NewExtractor(llm.NewChat(cfg.Extraction), cfg.Graph)
	return graphRepo, graphFinder, extractor
}

// embeddingStartupError décide ce qu'une sonde d'embedding ratée doit faire
// au démarrage. Un écart de dimension reste fatal, comme le demande la
// section 4.5 de la spec: il ne se révélerait sinon qu'à la première
// écriture, sous une erreur Postgres obscure, et aucune écriture ne peut
// réussir tant qu'il dure.
//
// Une sonde qui n'a pas pu joindre le modèle est une autre histoire: la
// posture du design sur l'embedder (sections 5.5 et 8.2, critère
// d'acceptation 10) est que sa panne dégrade la recherche et l'ingestion
// sans rendre le service indisponible, et /health ne le sonde d'ailleurs pas
// pour cette raison exacte. En faire une erreur fatale voulait dire qu'une
// panne d'Ollama empêchait n'importe quel réplica de redémarrer, donc qu'une
// dépendance explicitement non critique devenait critique au pire moment. On
// démarre donc en mode dégradé, avec un avertissement assez bruyant pour
// qu'un opérateur le voie: la dimension n'a pas pu être vérifiée, et elle ne
// le sera qu'au retour du modèle.
func embeddingStartupError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, postgres.ErrEmbeddingProbeUnavailable) {
		slog.Warn("embedding probe could not reach the model: starting in "+
			"degraded mode, search and ingestion will degrade until it "+
			"answers again, and the embedding dimension stays unverified "+
			"until then",
			"error", err)
		return nil
	}
	return err
}

// drainLateError rend une erreur déjà présente sur errCh sans jamais
// bloquer, ou nil s'il n'y en a pas. Isolée de run() pour être testable
// sans dépendre de Postgres ni d'un vrai serveur HTTP: voir
// main_test.go.
func drainLateError(errCh <-chan error) error {
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}
