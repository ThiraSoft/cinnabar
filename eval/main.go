// Commande eval mesure le rappel sémantique contre le vrai modèle
// d'embedding. Elle n'est pas dans la suite de tests: elle exige Ollama et
// prend plusieurs dizaines de secondes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/graph"
	"github.com/ThiraSoft/cinnabar/internal/jobs"
	"github.com/ThiraSoft/cinnabar/internal/llm"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

type datasetMessage struct {
	AuthorKey string `json:"author_key"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	ID        string `json:"id"`

	// CreatedAt date le message, en RFC3339. Optionnel, mais c'est ce qui
	// rend la famille temporelle mesurable: le chaînage des fenêtres de
	// validité ne ferme une observation que sur une observation
	// strictement postérieure, donc un corpus dont tous les messages
	// partagent le même horodatage ne l'exerce jamais.
	CreatedAt string `json:"created_at,omitempty"`
}

type datasetConversation struct {
	ConversationID string           `json:"conversation_id"`
	Participants   []string         `json:"participants"`
	Messages       []datasetMessage `json:"messages"`

	// Scope vaut "participants" quand il est absent. Le renseigner permet à
	// un corpus de contenir des conversations réellement inatteignables
	// (private) ou ouvertes à tout le workspace, donc de mesurer la règle
	// d'accès sur autre chose que la non-participation.
	Scope string `json:"scope,omitempty"`
}

type datasetQuery struct {
	Name         string   `json:"name"`
	RequesterKey string   `json:"requester_key"`
	Query        string   `json:"query"`
	ExpectedIDs  []string `json:"expected_ids"`
	Family       string   `json:"family"`

	// Strategies force les stratégies passées à Search pour cette requête
	// précise, quand la présence est vide (le comportement par défaut du
	// service: dense, lexical et graphe). Une requête "graph" qui veut
	// prouver qu'un fait n'est atteignable QUE par la traversée du graphe
	// se pose ["graph"] ici: sans ça, le dense ou le lexical pourraient
	// retrouver le même message par pur hasard sémantique et masquer un
	// graphe qui, lui, n'aurait rien trouvé.
	Strategies []string `json:"strategies,omitempty"`
}

type dataset struct {
	Conversations []datasetConversation `json:"conversations"`
	Queries       []datasetQuery        `json:"queries"`
}

func main() {
	var (
		datasetPath = flag.String("dataset", "eval/dataset.json", "jeu de données annoté")
		configPath  = flag.String("config", "config.yaml", "configuration du service")
		topK        = flag.Int("k", 5, "profondeur du rappel mesuré")
		audit       = flag.Bool("audit", false,
			"vérifie que les requêtes paraphrase ne sont pas résolubles par le seul lexical")
		keep = flag.Bool("keep", false,
			"conserver le workspace créé pour cette exécution au lieu de le supprimer")
	)
	flag.Parse()

	if err := run(*datasetPath, *configPath, *topK, *keep, *audit); err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		os.Exit(1)
	}
}

func run(datasetPath, configPath string, topK int, keep, audit bool) error {
	raw, err := os.ReadFile(datasetPath)
	if err != nil {
		return err
	}
	var ds dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		return err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	// graph.enabled est respecté tel que fourni par la configuration,
	// contrairement aux premières versions de cette commande: mesurer la
	// catégorie "graph" du jeu de données exige que l'extraction ait
	// vraiment tourné. Le défaut du fichier de configuration reste false,
	// donc une exécution qui ne demande rien de particulier se comporte
	// exactement comme avant.

	ctx := context.Background()
	pool, err := postgres.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool); err != nil {
		return err
	}

	embedder := llm.NewEmbedder(cfg.Embedding)
	if err := postgres.VerifyEmbeddingDimension(ctx, pool, embedder,
		cfg.Embedding.Dimensions); err != nil {
		return err
	}

	msgs := postgres.NewMessageRepo(pool)
	units := postgres.NewUnitRepo(pool)
	jobRepo := postgres.NewJobRepo(pool)
	searchRepo := postgres.NewSearchRepo(pool)
	convs := postgres.NewConversationRepo(pool)

	// Même câblage que cmd/cinnabar/main.go: graphRepo, graphFinder et
	// extractGraph restent nil (respectivement une interface nil, pas un
	// pointeur nil typé) tant que graph.enabled vaut false.
	var (
		graphRepo    *postgres.GraphRepo
		graphFinder  memory.GraphSearcher
		extractGraph memory.GraphExtractor
	)
	if cfg.Graph.Enabled {
		graphRepo = postgres.NewGraphRepo(pool)
		graphFinder = searchRepo
		extractGraph = graph.NewExtractor(llm.NewChat(cfg.Extraction), cfg.Graph)
	}

	ingester := memory.NewIngester(cfg, msgs, units, embedder, jobRepo)
	finder := memory.NewSearcher(cfg, msgs, embedder, searchRepo, searchRepo, graphFinder)
	if cfg.Rerank.Enabled {
		finder = finder.WithReranker(llm.NewReranker(cfg.Rerank))
	}

	// Un workspace unique par exécution, pour ne pas mélanger deux
	// campagnes, et pour que -keep=false puisse le supprimer sans risquer
	// d'emporter les données d'une autre exécution.
	workspace := "eval_" + uuid.NewString()[:8]
	fmt.Printf("workspace : %s\n\n", workspace)

	// Une évaluation qu'on relance vingt fois en réglant la configuration ne
	// doit pas laisser vingt workspaces de débris en base. Le nettoyage est
	// posé en defer dès que le workspace existe, avant même la moindre
	// écriture, pour s'appliquer aussi bien sur un run réussi que sur un
	// échec d'ingestion en cours de route: l'ordre des defer (LIFO) le fait
	// tourner avant la fermeture du pool posée juste au-dessus.
	if !keep {
		defer func() {
			if err := cleanupWorkspace(ctx, pool, workspace); err != nil {
				fmt.Fprintf(os.Stderr,
					"eval: échec du nettoyage du workspace %s: %v\n", workspace, err)
				return
			}
			fmt.Printf("\nworkspace %s supprimé (relancer avec -keep pour le conserver)\n",
				workspace)
		}()
	} else {
		fmt.Printf("\nworkspace %s conservé (option -keep)\n", workspace)
	}

	// protegeParDemandeur porte, pour chaque demandeur cité par une requête
	// d'ACL, l'ensemble des identifiants annotés qu'il n'a pas le droit de
	// lire: ceux des conversations où il n'est pas participant et dont le
	// scope ne l'ouvre pas au workspace.
	//
	// C'est ce qui permet de mesurer la vraie propriété d'une requête d'ACL,
	// à savoir qu'aucun contenu protégé ne ressort, plutôt que la propriété
	// approchée « elle ne rend rien ». Les deux se confondent sur un corpus
	// minuscule et divergent dès qu'il grossit: une question dont la réponse
	// est protégée a souvent des voisins parfaitement lisibles, et les
	// rendre n'est pas une fuite. Confondre les deux fait passer pour un
	// défaut d'accès ce qui n'est qu'un défaut de pertinence.
	protegeParDemandeur := map[string]map[string]bool{}
	for _, q := range ds.Queries {
		if q.Family != "acl" {
			continue
		}
		if _, deja := protegeParDemandeur[q.RequesterKey]; deja {
			continue
		}
		interdits := map[string]bool{}
		for _, c := range ds.Conversations {
			if c.Scope == "workspace" {
				continue
			}
			participe := false
			for _, p := range c.Participants {
				if p == q.RequesterKey {
					participe = true
				}
			}
			if participe {
				continue
			}
			for _, m := range c.Messages {
				if m.ID != "" {
					interdits[m.ID] = true
				}
			}
		}
		protegeParDemandeur[q.RequesterKey] = interdits
	}

	// idByMessage relie l'identifiant annoté du jeu de données au message_id
	// réellement attribué par le service.
	idByMessage := map[uuid.UUID]string{}

	start := time.Now()
	for _, conv := range ds.Conversations {
		convID := workspace + "_" + conv.ConversationID
		scope := conv.Scope
		if scope == "" {
			scope = "participants"
		}
		if err := convs.Declare(ctx, convID, workspace, scope,
			conv.Participants); err != nil {
			return err
		}
		for _, m := range conv.Messages {
			var createdAt time.Time
			if m.CreatedAt != "" {
				parsed, err := time.Parse(time.RFC3339, m.CreatedAt)
				if err != nil {
					return fmt.Errorf("message %s: created_at %q: %w",
						m.ID, m.CreatedAt, err)
				}
				createdAt = parsed
			}
			res, err := ingester.Ingest(ctx, memory.AppendInput{
				WorkspaceID: workspace, ConversationID: convID,
				AuthorKey: m.AuthorKey, Role: m.Role, Content: m.Content,
				CreatedAt: createdAt,
			}, "searchable")
			if err != nil {
				return fmt.Errorf("ingest %s: %w", m.ID, err)
			}
			// Un statut autre que "searchable" ferait mesurer un rappel sur
			// un index en partie vide: mieux vaut échouer bruyamment ici
			// que produire un tableau de résultats qui semble valide.
			if res.Status != "searchable" {
				return fmt.Errorf("ingest %s: status %q, indexing_error %q",
					m.ID, res.Status, res.IndexingError)
			}
			if m.ID != "" {
				idByMessage[res.Message.MessageID] = m.ID
			}
		}
	}
	fmt.Printf("ingestion : %d conversations en %s\n\n",
		len(ds.Conversations), time.Since(start).Round(time.Millisecond))

	if cfg.Graph.Enabled {
		// Cette commande n'a pas de runner de jobs asynchrone: mesurer un
		// rappel déterministe pour la catégorie "graph" exige que
		// l'extraction ait fini avant la moindre recherche, pas qu'elle
		// tourne en course avec elle. drainGraphExtraction traite donc en
		// synchrone tous les jobs graph_extract que l'ingestion vient de
		// poser, avec le même handler que le service en production.
		extractStart := time.Now()
		n, err := drainGraphExtraction(ctx, jobRepo,
			jobs.GraphExtractHandler(cfg, msgs, graphRepo, extractGraph, embedder))
		if err != nil {
			return fmt.Errorf(
				"extraction du graphe: %w (l'extracteur configuré sur %s "+
					"est-il joignable depuis cet environnement ?)",
				err, cfg.Extraction.BaseURL)
		}
		fmt.Printf("extraction graphe : %d messages en %s\n\n",
			n, time.Since(extractStart).Round(time.Millisecond))
	}

	type outcome struct {
		query datasetQuery
		hit   bool
		ok    bool
		found []string
		// bestDense porte la meilleure similarité cosinus brute de la
		// requête. C'est la grandeur sur laquelle un plancher de pertinence
		// au niveau de la requête peut porter, contrairement au score
		// fusionné qui n'encode qu'un rang.
		bestDense float64
	}
	var outcomes []outcome

	// hits et misses portent le score de chaque résultat individuel,
	// classé par sa propre pertinence, jamais par le succès global de la
	// requête qui l'a rendu: une requête qui réussit rend typiquement un
	// vrai résultat entouré de plusieurs voisins sans rapport (voir "Ce que
	// rend une recherche" dans le README, dense n'ayant pas de plancher),
	// et étiqueter les cinq "pertinents" parce que l'un d'eux l'est
	// noierait le signal sous le bruit de remplissage.
	var hits, misses []float64
	var denseHits, denseMisses []float64
	for _, q := range ds.Queries {
		resp, err := finder.Search(ctx, memory.SearchRequest{
			WorkspaceID: workspace, RequesterKey: q.RequesterKey,
			Query: q.Query, ResultLimit: topK, Strategies: q.Strategies,
		})
		if err != nil {
			return fmt.Errorf("search %s: %w", q.Name, err)
		}

		var found []string
		for _, r := range resp.Results {
			relevant := false
			for _, src := range r.SourceMessageIDs {
				name, ok := idByMessage[src]
				if !ok {
					continue
				}
				found = append(found, name)
				for _, want := range q.ExpectedIDs {
					if name == want {
						relevant = true
					}
				}
			}
			// Une requête negative ou acl n'a aucun expected_ids: aucun de
			// ses résultats ne peut donc jamais tomber dans hits, par
			// construction plutôt que par oubli. C'est voulu, ces requêtes
			// ne peuvent par définition rien retrouver de "pertinent".
			if relevant {
				hits = append(hits, r.Scores["final"])
				if d, ok := r.Scores["dense"]; ok {
					denseHits = append(denseHits, d)
				}
			} else {
				misses = append(misses, r.Scores["final"])
				if d, ok := r.Scores["dense"]; ok {
					denseMisses = append(denseMisses, d)
				}
			}
		}

		hit := false
		for _, want := range q.ExpectedIDs {
			for _, got := range found {
				if got == want {
					hit = true
				}
			}
		}
		// Une requête négative réussit en ne rendant strictement rien: elle
		// mesure la pertinence, et rendre quoi que ce soit à une question
		// sans réponse est un défaut.
		//
		// Une requête d'ACL mesure autre chose, et le critère est donc
		// différent: elle réussit quand aucun contenu protégé ne ressort.
		// Rendre un voisin lisible n'est pas une fuite, c'est au pire un
		// manque de pertinence, déjà mesuré par la famille négative.
		ok := hit
		switch q.Family {
		case "negative":
			ok = len(resp.Results) == 0
		case "acl":
			interdits := protegeParDemandeur[q.RequesterKey]
			ok = true
			for _, nom := range found {
				if interdits[nom] {
					ok = false
					fmt.Printf("  FUITE ACL  %s rend %q, interdit à %s\n",
						q.Name, nom, q.RequesterKey)
				}
			}
		}

		// Compteurs de diagnostic quand la configuration les demande: sans
		// eux, une requête qui rend des résultats alors qu'aucune stratégie
		// n'aurait dû en produire est indiagnosticable depuis la sortie.
		if resp.Debug != nil {
			fmt.Printf("  [forme %-30s %-11s best=%.4f med=%.4f marge=%.4f | dense=%d lex=%d graphe=%d]\n",
				q.Name, q.Family,
				resp.Debug.DenseBest, resp.Debug.DenseMedian,
				resp.Debug.DenseBest-resp.Debug.DenseMedian,
				resp.Debug.DenseCandidates, resp.Debug.LexicalCandidates,
				resp.Debug.GraphCandidates)
		}

		// Pour la famille graphe, les faits comptent plus que les messages:
		// répondre à une question à deux sauts demande de raisonner sur la
		// chaîne, ce que le service ne fait pas et ne doit pas faire sur le
		// chemin de recherche. Ce que le graphe doit livrer, c'est la
		// matière de ce raisonnement au modèle lecteur.
		if audit && q.Family == "graph" {
			fmt.Printf("  faits pour %s (%d):\n", q.Name, len(resp.GraphFacts))
			for i, f := range resp.GraphFacts {
				if i >= 30 {
					fmt.Printf("      ... %d autres\n", len(resp.GraphFacts)-i)
					break
				}
				fmt.Printf("      %s %s %s\n", f.Subject, f.Predicate, f.Object)
			}
		}

		if audit && (q.Name == "paraphrase_maturite" || q.Name == "c_temporal_piano_actuel") {
			fmt.Printf("  detail %s:\n", q.Name)
			for i, r := range resp.Results {
				noms := []string{}
				for _, src := range r.SourceMessageIDs {
					if n, ok := idByMessage[src]; ok {
						noms = append(noms, n)
					}
				}
				fmt.Printf("      %d. final=%.4f dense=%.4f lexical=%.4f date=%s %v\n",
					i+1, r.Scores["final"], r.Scores["dense"], r.Scores["lexical"],
					r.OccurredAt.Format("2006-01-02"), noms)
			}
		}

		best := 0.0
		for _, r := range resp.Results {
			if d, ok := r.Scores["dense"]; ok && d > best {
				best = d
			}
		}
		outcomes = append(outcomes, outcome{
			query: q, hit: hit, ok: ok, found: found, bestDense: best,
		})
	}

	byFamily := map[string][2]int{}
	for _, o := range outcomes {
		c := byFamily[o.query.Family]
		c[1]++
		if o.ok {
			c[0]++
		}
		byFamily[o.query.Family] = c
	}

	fmt.Printf("%-26s %-12s %-6s %-9s %s\n", "REQUÊTE", "FAMILLE", "OK", "DENSE_MAX", "TROUVÉ")
	for _, o := range outcomes {
		mark := "non"
		if o.ok {
			mark = "oui"
		}
		fmt.Printf("%-26s %-12s %-6s %-9.4f %v\n",
			o.query.Name, o.query.Family, mark, o.bestDense, o.found)
	}

	fmt.Println()
	families := make([]string, 0, len(byFamily))
	for f := range byFamily {
		families = append(families, f)
	}
	sort.Strings(families)
	total, passed := 0, 0
	for _, f := range families {
		c := byFamily[f]
		fmt.Printf("%-12s %d/%d\n", f, c[0], c[1])
		passed += c[0]
		total += c[1]
	}
	fmt.Printf("\nRecall@%d global : %d/%d (%.0f%%)\n",
		topK, passed, total, 100*float64(passed)/float64(total))

	// L'audit ne mesure pas le service, il mesure le corpus. Une requête de
	// la famille paraphrase est censée exiger de la ressemblance sémantique:
	// si la recherche lexicale seule la résout, c'est que la question a
	// recopié un mot distinctif de son message cible, et la requête ne
	// mesure alors plus ce qu'elle annonce. C'est le défaut systématique des
	// corpus écrits par un modèle, et il ne se voit pas à la lecture d'une
	// question isolée: il faut la confronter à sa cible, ce que fait ce
	// mode.
	if audit {
		fmt.Println("\nAUDIT DU CORPUS — requêtes paraphrase résolues par le seul lexical")
		contaminees := 0
		total := 0
		for _, q := range ds.Queries {
			if q.Family != "paraphrase" {
				continue
			}
			total++
			resp, err := finder.Search(ctx, memory.SearchRequest{
				WorkspaceID: workspace, RequesterKey: q.RequesterKey,
				Query: q.Query, ResultLimit: topK,
				Strategies: []string{"lexical"},
			})
			if err != nil {
				return fmt.Errorf("audit %s: %w", q.Name, err)
			}
			touche := false
			for _, r := range resp.Results {
				for _, src := range r.SourceMessageIDs {
					name := idByMessage[src]
					for _, want := range q.ExpectedIDs {
						if name == want {
							touche = true
						}
					}
				}
			}
			if touche {
				contaminees++
				fmt.Printf("  CONTAMINÉE  %-28s %s\n", q.Name, q.Query)
			}
		}
		fmt.Printf("\n%d requêtes paraphrase sur %d sont résolubles par le seul lexical.\n",
			contaminees, total)
		if contaminees > 0 {
			fmt.Println("Reformuler leur question pour en retirer les mots distinctifs")
			fmt.Println("du message cible, sinon elles mesurent le lexical et non le dense.")
		}

		// Vérification symétrique pour la famille graphe. Une requête qui
		// annonce exiger une traversée doit être hors de portée du dense et
		// du lexical réunis: si ces deux-là la résolvent, elle ne mesure
		// pas le graphe, et son échec ou sa réussite ne dit rien de lui.
		fmt.Println("\nAUDIT DU CORPUS — requêtes graphe résolues sans le graphe")
		inutiles, totalG := 0, 0
		for _, q := range ds.Queries {
			if q.Family != "graph" {
				continue
			}
			totalG++
			resp, err := finder.Search(ctx, memory.SearchRequest{
				WorkspaceID: workspace, RequesterKey: q.RequesterKey,
				Query: q.Query, ResultLimit: topK,
				Strategies: []string{"dense", "lexical"},
			})
			if err != nil {
				return fmt.Errorf("audit graphe %s: %w", q.Name, err)
			}
			touche := false
			for _, r := range resp.Results {
				for _, src := range r.SourceMessageIDs {
					for _, want := range q.ExpectedIDs {
						if idByMessage[src] == want {
							touche = true
						}
					}
				}
			}
			if touche {
				inutiles++
				fmt.Printf("  SANS GRAPHE %-30s %s\n", q.Name, q.Query)
			}
		}
		fmt.Printf("\n%d requêtes graphe sur %d sont résolues par le dense et le lexical seuls.\n",
			inutiles, totalG)
		if inutiles > 0 {
			fmt.Println("Elles ne mesurent pas la traversée: leur reponse est déjà")
			fmt.Println("atteignable sans elle. À reformuler ou à reclasser.")
		}
	}

	// Distribution des scores, pour calibrer retrieval.minimum_score sans
	// l'inventer, comme le demande la section 18 de la spec. hits et misses
	// ont été accumulés plus haut, résultat par résultat, pendant la boucle
	// de recherche.
	sort.Float64s(hits)
	sort.Float64s(misses)
	fmt.Printf("\nscores des résultats pertinents :     %v\n", round(hits))
	fmt.Printf("scores des résultats non pertinents : %v\n", round(misses))
	// Le score dense brut est la seule des deux grandeurs sur laquelle un
	// plancher de pertinence peut porter: c'est une similarité cosinus, elle
	// garde son sens hors de son classement. Le score fusionné, lui, n'encode
	// qu'un rang, ce que la ligne de base de la v1 a établi.
	sort.Float64s(denseHits)
	sort.Float64s(denseMisses)
	fmt.Printf("\nscores denses bruts, pertinents :     %v\n", round(denseHits))
	fmt.Printf("scores denses bruts, non pertinents : %v\n", round(denseMisses))
	fmt.Println("\nLe plancher se pose sur le score dense brut, entre les deux")
	fmt.Println("distributions ci-dessus, et seulement si elles se séparent.")
	return nil
}

// drainGraphExtraction traite en synchrone, dans cette même goroutine, tous
// les jobs graph_extract prêts en file, jusqu'à ce qu'il n'en reste plus.
// Un run réussi de cette commande ne pose jamais que des graph_extract:
// graph_reeval ne naît que d'une édition ou d'une suppression de message, ce
// que cette commande ne fait jamais faire. Un type de job inattendu ici est
// donc un signal que cette hypothèse a cessé d'être vraie ailleurs dans le
// code, pas un cas à ignorer en silence.
//
// Le premier job qui échoue arrête le drainage et rend l'erreur telle
// quelle plutôt que de continuer sur les suivants: un extracteur injoignable
// le sera tout autant pour le prochain message, et laisser la boucle
// continuer ne ferait que répéter la même panne pour chaque message ingéré.
func drainGraphExtraction(ctx context.Context, jobRepo *postgres.JobRepo,
	handler jobs.Handler) (int, error) {

	n := 0
	for {
		j, err := jobRepo.Claim(ctx)
		if err != nil {
			return n, fmt.Errorf("claim: %w", err)
		}
		if j == nil {
			return n, nil
		}
		if j.JobType != "graph_extract" {
			return n, fmt.Errorf("unexpected job type %q, want graph_extract", j.JobType)
		}
		n++
		if err := handler(ctx, j); err != nil {
			_ = jobRepo.Fail(ctx, j.JobID, err, 1)
			return n, fmt.Errorf("job %d: %w", j.JobID, err)
		}
		if err := jobRepo.Complete(ctx, j.JobID); err != nil {
			return n, fmt.Errorf("complete job %d: %w", j.JobID, err)
		}
	}
}

// cleanupWorkspace supprime toutes les traces d'un run: la suppression des
// conversations entraîne, par ON DELETE CASCADE, celle des messages, des
// unités de mémoire et de leurs éventuelles lignes memory_unit_acl. La table
// jobs n'a pas de clé étrangère vers conversations (un job décrit une
// conversation par son identifiant texte, pas par une contrainte), donc elle
// est nettoyée séparément; en pratique un run réussi en mode "searchable" et
// graph.enabled à false n'y pose rien, puisque l'indexation en ligne ne pose
// de job embed que si elle échoue (un cas qu'on fait déjà échouer bruyamment
// plus haut) et qu'aucun job graphe n'est posé quand le graphe est
// désactivé.
//
// graph_entities n'a pas non plus de clé étrangère vers conversations (une
// entité appartient à un workspace, pas à une conversation précise: rien ne
// l'empêche d'être citée depuis plusieurs), donc elle est nettoyée à part
// elle aussi. La cascade fait le reste depuis là: graph_relations référence
// graph_entities ON DELETE CASCADE, et graph_relation_sources référence
// graph_relations ON DELETE CASCADE, donc supprimer les entités du
// workspace suffit à emporter les relations et leurs sources avec elles.
// Un run avec graph.enabled à false n'y pose rien non plus, cette
// suppression est simplement un no-op dans ce cas.
//
// Les trois suppressions passent par la même transaction: la cascade rend
// déjà chacun de ces deux sous-arbres atomique à lui seul, mais sans
// transaction commune un arrêt brutal entre les DELETE laisserait une ligne
// orpheline dans jobs ou dans graph_entities.
func cleanupWorkspace(ctx context.Context, pool *pgxpool.Pool, workspace string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin cleanup tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM conversations WHERE workspace_id = $1`, workspace); err != nil {
		return fmt.Errorf("delete conversations: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM graph_entities WHERE workspace_id = $1`, workspace); err != nil {
		return fmt.Errorf("delete graph entities: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM jobs WHERE workspace_id = $1`, workspace); err != nil {
		return fmt.Errorf("delete jobs: %w", err)
	}
	return tx.Commit(ctx)
}

func round(in []float64) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[i] = float64(int(v*10000)) / 10000
	}
	return out
}
