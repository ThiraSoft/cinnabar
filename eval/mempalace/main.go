// Commande mempalace rejoue sur Cinnabar les quatre bancs publiés par
// MemPalace (github.com/MemPalace/mempalace, dossier benchmarks): LoCoMo,
// LongMemEval, ConvoMem et MemBench. Les données sont découpées comme dans
// leurs scripts et notées avec leurs métriques, pour que les chiffres se
// comparent à ceux de leurs propres exécutions.
//
// Chaque question est cherchée dans plusieurs variantes: configuration de
// base (sans plancher dense), avec le réordonnanceur, avec les candidats du
// graphe dans la fusion, et les deux. Les résultats vont en JSONL, une ligne
// par question et par variante, ce qui permet de reprendre un run coupé.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

type variant struct {
	Name     string
	K        int
	Rerank   bool
	Graph    bool
	Floor    bool // configuration livrée: plancher dense et détection de non-réponse
	Distinct bool // longue liste, notée en k sessions distinctes
	Core     bool // sans élargissement: le noyau recollé seul

	// Tune modifie les réglages de recherche de la variante, après Floor
	// et Core. Budget borne les tokens rendus: 0 ne borne rien, -1 garde
	// retrieval.max_memory_tokens.
	Tune   func(r *config.Retrieval)
	Budget int
}

// sweepVariants balaie les réglages de recherche qui ne demandent aucun
// modèle, sur une seule ingestion. La valeur livrée de chaque réglage est
// dans le lot, pour mesurer l'écart à ce qui tourne aujourd'hui.
func sweepVariants() []variant {
	f := func(v float64) *float64 { return &v }
	var vs []variant
	// Plancher dense et détection de non-réponse, à k=5 sans borne.
	for _, floor := range []float64{0, 0.30, 0.35, 0.40, 0.45, 0.52} {
		for _, na := range []bool{false, true} {
			floor, na := floor, na
			vs = append(vs, variant{Name: fmt.Sprintf("plancher%.2f_nonrep%v_k5", floor, na), K: 5,
				Tune: func(r *config.Retrieval) {
					r.MinimumDenseScore, r.NoAnswerBestBelow, r.NoAnswerMarginBelow = nil, nil, nil
					if floor > 0 {
						r.MinimumDenseScore = f(floor)
					}
					if na {
						r.NoAnswerBestBelow, r.NoAnswerMarginBelow = f(0.58), f(0.05)
					}
				}})
		}
	}
	noFloor := func(r *config.Retrieval) {
		r.MinimumDenseScore, r.NoAnswerBestBelow, r.NoAnswerMarginBelow = nil, nil, nil
	}
	// Nombre de résultats et budget de tokens.
	for _, k := range []int{5, 8, 10} {
		for _, b := range []int{1200, 2000, 3000, 0} {
			vs = append(vs, variant{Name: fmt.Sprintf("k%d_budget%d", k, b), K: k, Budget: b, Tune: noFloor})
		}
	}
	// Élargissement autour du noyau.
	for _, k := range []int{5, 10} {
		for _, e := range []int{0, 1, 2, 3} {
			e := e
			vs = append(vs, variant{Name: fmt.Sprintf("k%d_elargi%d", k, e), K: k, Tune: func(r *config.Retrieval) {
				noFloor(r)
				r.ExpandBefore, r.ExpandAfter = e, e
			}})
		}
	}
	// Fusion et profondeur des candidats.
	for _, rrf := range []int{20, 60} {
		for _, top := range []int{20, 40} {
			rrf, top := rrf, top
			vs = append(vs, variant{Name: fmt.Sprintf("k5_rrf%d_top%d", rrf, top), K: 5, Tune: func(r *config.Retrieval) {
				noFloor(r)
				r.RRFK, r.DenseTopK, r.LexicalTopK = rrf, top, top
			}})
		}
	}
	// Stratégies seules, pour savoir ce que chacune apporte.
	for _, only := range []string{"dense", "lexical"} {
		only := only
		vs = append(vs, variant{Name: "k5_seul_" + only, K: 5, Tune: func(r *config.Retrieval) {
			noFloor(r)
			if only == "dense" {
				r.LexicalTopK = 0
			} else {
				r.DenseTopK = 0
			}
		}})
	}
	return vs
}

type record struct {
	Bench         string             `json:"bench"`
	QID           string             `json:"qid"`
	Category      string             `json:"category"`
	Variant       string             `json:"variant"`
	K             int                `json:"k"`
	Metrics       map[string]float64 `json:"metrics"`
	Results       int                `json:"results"`
	Tokens        int                `json:"tokens"`
	Millis        float64            `json:"ms"`
	RerankApplied bool               `json:"rerank_applied,omitempty"`
	RerankFailed  bool               `json:"rerank_failed,omitempty"`
	GraphCands    int                `json:"graph_candidates,omitempty"`
	GraphFacts    int                `json:"graph_facts,omitempty"`
	DenseBest     float64            `json:"dense_best,omitempty"`
	DenseMedian   float64            `json:"dense_median,omitempty"`
}

func main() {
	var (
		bench      = flag.String("bench", "", "locomo, longmemeval, convomem ou membench")
		dataPath   = flag.String("data", "", "fichier ou répertoire du banc")
		configPath = flag.String("config", "config.yaml", "configuration du service")
		outPath    = flag.String("out", "", "JSONL des résultats (repris s'il existe)")
		withGraph  = flag.Bool("graph", false, "extraire le graphe et ajouter les variantes graphe")
		withRerank = flag.Bool("rerank", true, "ajouter les variantes avec réordonnanceur")
		perCat     = flag.Int("per-category", 0, "questions par catégorie (0: toutes)")
		limit      = flag.Int("limit", 50, "ConvoMem: items par catégorie, comme --limit du script")
		chunk      = flag.Int("chunk", 25, "workspaces ingérés puis purgés ensemble")
		ingestW    = flag.Int("ingest-workers", 4, "workspaces ingérés en parallèle")
		graphW     = flag.Int("graph-workers", 4, "extractions de graphe en parallèle")
		searchW    = flag.Int("search-workers", 4, "questions cherchées en parallèle")
		keep       = flag.Bool("keep", false, "ne pas purger les workspaces")
		report     = flag.Bool("report", false, "résumer le JSONL sans rien exécuter")
		batch      = flag.Int("batch", 0, "graph.extraction_batch (0: celui de la configuration)")
		sweep      = flag.Bool("sweep", false, "balayer les réglages de recherche sans modèle, au lieu des variantes habituelles")
		tag        = flag.String("tag", "", "suffixe des workspaces, pour faire tourner deux runs du même banc côte à côte")
	)
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if *report {
		if err := printReport(*outPath); err != nil {
			fmt.Fprintln(os.Stderr, "mempalace:", err)
			os.Exit(1)
		}
		return
	}
	opts := options{bench: *bench, data: *dataPath, config: *configPath, out: *outPath,
		graph: *withGraph, rerank: *withRerank, perCat: *perCat, limit: *limit, chunk: *chunk,
		ingestW: *ingestW, graphW: *graphW, searchW: *searchW, keep: *keep, tag: *tag, batch: *batch, sweep: *sweep}
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "mempalace:", err)
		os.Exit(1)
	}
}

type options struct {
	bench, data, config, out, tag string
	graph, rerank, keep, sweep    bool
	perCat, limit, chunk          int
	ingestW, graphW, searchW      int
	batch                         int
}

func variantsFor(bench string, withGraph, withRerank bool) []variant {
	ks := map[string][]int{"locomo": {5, 10}, "longmemeval": {5, 10},
		"convomem": {10}, "membench": {5}}[bench]
	var vs []variant
	for _, k := range ks {
		vs = append(vs, variant{Name: fmt.Sprintf("livree_k%d", k), K: k, Floor: true})
		vs = append(vs, variant{Name: fmt.Sprintf("base_k%d", k), K: k})
		vs = append(vs, variant{Name: fmt.Sprintf("noyau_k%d", k), K: k, Core: true})
		// Sur LongMemEval, un vivier de 2k extraits de messages longs à k=10
		// dépasse la fenêtre d'un slot du modèle: le rerank n'y est mesuré
		// qu'à k=5.
		if withRerank && (bench != "longmemeval" || k == 5) {
			vs = append(vs, variant{Name: fmt.Sprintf("rerank_k%d", k), K: k, Rerank: true})
		}
		if withGraph {
			vs = append(vs, variant{Name: fmt.Sprintf("graphe_k%d", k), K: k, Graph: true})
			if withRerank {
				vs = append(vs, variant{Name: fmt.Sprintf("graphe_rerank_k%d", k), K: k, Graph: true, Rerank: true})
			}
		}
	}
	if bench == "locomo" || bench == "longmemeval" {
		vs = append(vs, variant{Name: "base_sessions", K: 50, Distinct: true})
		if withGraph {
			vs = append(vs, variant{Name: "graphe_sessions", K: 50, Distinct: true, Graph: true})
		}
	}
	return vs
}

func run(o options) error {
	prefix := "mp_" + o.bench + o.tag + "_"
	var (
		wss []wsSpec
		err error
	)
	switch o.bench {
	case "locomo":
		wss, err = loadLoCoMo(o.data, prefix)
	case "longmemeval":
		wss, err = loadLongMemEval(o.data, prefix, o.perCat)
	case "convomem":
		wss, err = loadConvoMem(o.data, prefix, o.limit)
	case "membench":
		wss, err = loadMemBench(o.data, prefix, o.perCat)
	default:
		return fmt.Errorf("banc inconnu %q", o.bench)
	}
	if err != nil {
		return err
	}
	if o.bench == "locomo" && o.perCat > 0 {
		// Sous-échantillon LoCoMo: les N premières questions de chaque
		// catégorie par conversation, pour un essai rapide.
		for i := range wss {
			n := map[string]int{}
			var qs []questionSpec
			for _, q := range wss[i].Qs {
				if n[q.Category] < o.perCat {
					n[q.Category]++
					qs = append(qs, q)
				}
			}
			wss[i].Qs = qs
		}
	}
	nq, nm := 0, 0
	for _, w := range wss {
		nq += len(w.Qs)
		for _, c := range w.Convs {
			nm += len(c.Msgs)
		}
	}
	variants := variantsFor(o.bench, o.graph, o.rerank)
	if o.sweep {
		variants = sweepVariants()
	}
	fmt.Printf("%s : %d workspaces, %d messages, %d questions, %d variantes\n",
		o.bench, len(wss), nm, nq, len(variants))

	done, err := loadDone(o.out)
	if err != nil {
		return err
	}

	cfg, err := config.Load(o.config)
	if err != nil {
		return err
	}
	cfg.Service.DebugSearch = true
	if cfg.Jobs.RetryLimit < 1 {
		cfg.Jobs.RetryLimit = 3
	}
	cfg.Graph.Enabled = o.graph
	cfg.Graph.FuseCandidates = true
	// Le banc draine la file lui-même, juste après l'ingestion: attendre que
	// les conversations se taisent ne ferait que retarder chaque job.
	cfg.Graph.ExtractionIdle = 0
	if o.batch > 0 {
		cfg.Graph.ExtractionBatch = o.batch
	}
	cfg.Rerank.Enabled = false

	ctx := context.Background()
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
	if err := postgres.VerifyEmbeddingDimension(ctx, pool, embedder, cfg.Embedding.Dimensions); err != nil {
		return err
	}

	msgs := postgres.NewMessageRepo(pool)
	units := postgres.NewUnitRepo(pool)
	jobRepo := postgres.NewJobRepo(pool)
	searchRepo := postgres.NewSearchRepo(pool)
	convs := postgres.NewConversationRepo(pool)
	graphRepo := postgres.NewGraphRepo(pool)
	cache := &cachedEmbedder{inner: embedder, m: map[string][]float32{}}
	ingester := memory.NewIngester(cfg, msgs, units, embedder, jobRepo)
	reranker := &trackedReranker{inner: llm.NewReranker(cfg.Rerank)}

	searchers := map[string]*memory.Searcher{}
	for _, v := range variants {
		vcfg := *cfg
		if !v.Floor {
			vcfg.Retrieval.MinimumDenseScore = nil
			vcfg.Retrieval.NoAnswerBestBelow = nil
			vcfg.Retrieval.NoAnswerMarginBelow = nil
		}
		if v.Core {
			vcfg.Retrieval.ExpandBefore = 0
			vcfg.Retrieval.ExpandAfter = 0
		}
		if v.Tune != nil {
			v.Tune(&vcfg.Retrieval)
		}
		vcfg.Graph.Enabled = v.Graph
		vcfg.Rerank.Enabled = v.Rerank
		vcfg.Rerank.Pool = 2 * v.K
		var gs memory.GraphSearcher
		if v.Graph {
			gs = searchRepo
		}
		s := memory.NewSearcher(&vcfg, msgs, cache, searchRepo, searchRepo, gs)
		if v.Rerank {
			s = s.WithReranker(reranker)
		}
		searchers[v.Name] = s
	}

	out, err := os.OpenFile(o.out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	var outMu sync.Mutex
	enc := json.NewEncoder(out)

	var extractGraph memory.GraphExtractor
	if o.graph {
		extractGraph = graph.NewExtractor(llm.NewChat(cfg.Extraction), cfg.Graph)
	}

	start := time.Now()
	var doneQ atomic.Int64
	for c0 := 0; c0 < len(wss); c0 += o.chunk {
		c1 := min(c0+o.chunk, len(wss))
		var todo []wsSpec
		for _, w := range wss[c0:c1] {
			pending := false
			for _, q := range w.Qs {
				for _, v := range variants {
					if !done[q.ID+"|"+v.Name] {
						pending = true
					}
				}
			}
			if pending {
				todo = append(todo, w)
			} else {
				doneQ.Add(int64(len(w.Qs)))
			}
		}
		if len(todo) == 0 {
			continue
		}

		// Ingestion, précédée d'une purge: un run repris ne doit pas
		// doubler les messages d'un paquet déjà en partie écrit.
		t0 := time.Now()
		byID := &sync.Map{}
		var nIngested atomic.Int64
		if err := forEach(len(todo), o.ingestW, func(i int) error {
			w := todo[i]
			if err := cleanupWorkspace(ctx, pool, w.ID); err != nil {
				return err
			}
			for ci := range w.Convs {
				c := &w.Convs[ci]
				if err := convs.Declare(ctx, c.ID, w.ID, "participants", c.Participants); err != nil {
					return err
				}
				for mi := range c.Msgs {
					m := &c.Msgs[mi]
					res, err := ingester.Ingest(ctx, memory.AppendInput{
						WorkspaceID: w.ID, ConversationID: c.ID, AuthorKey: m.Author,
						Role: m.Role, Content: m.Content, CreatedAt: m.At,
					}, "searchable")
					if err != nil {
						return fmt.Errorf("ingest %s: %w", c.ID, err)
					}
					if res.Status != "searchable" {
						return fmt.Errorf("ingest %s: status %q", c.ID, res.Status)
					}
					byID.Store(res.Message.MessageID, m)
					nIngested.Add(1)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		fmt.Printf("[%s] paquet %d-%d : %d messages ingérés en %s\n", since(start), c0, c1,
			nIngested.Load(), time.Since(t0).Round(time.Second))

		if o.graph {
			t0 := time.Now()
			n, err := drainGraph(ctx, pool, jobRepo, jobs.GraphExtractHandler(cfg, msgs, msgs, jobRepo, graphRepo, extractGraph, embedder), o.graphW, cfg.Jobs.RetryLimit)
			if err != nil {
				return err
			}
			fmt.Printf("[%s] graphe : %d extractions en %s\n", since(start), n, time.Since(t0).Round(time.Second))
		}

		type job struct {
			w *wsSpec
			q questionSpec
		}
		var qjobs []job
		for i := range todo {
			for _, q := range todo[i].Qs {
				qjobs = append(qjobs, job{&todo[i], q})
			}
		}
		t0 = time.Now()
		if err := forEach(len(qjobs), o.searchW, func(i int) error {
			jb := qjobs[i]
			for _, v := range variants {
				if done[jb.q.ID+"|"+v.Name] {
					continue
				}
				rec, err := searchOne(ctx, searchers[v.Name], o.bench, jb.w.ID, jb.q, v, byID)
				if err != nil {
					return err
				}
				outMu.Lock()
				err = enc.Encode(rec)
				outMu.Unlock()
				if err != nil {
					return err
				}
			}
			if n := doneQ.Add(1); n%50 == 0 {
				fmt.Printf("[%s] %d/%d questions\n", since(start), n, nq)
			}
			return nil
		}); err != nil {
			return err
		}
		fmt.Printf("[%s] paquet %d-%d : %d questions cherchées en %s\n", since(start), c0, c1,
			len(qjobs), time.Since(t0).Round(time.Second))

		if !o.keep {
			for _, w := range todo {
				if err := cleanupWorkspace(ctx, pool, w.ID); err != nil {
					return err
				}
			}
		}
	}
	out.Close()
	return printReport(o.out)
}

func searchOne(ctx context.Context, s *memory.Searcher, bench, ws string, q questionSpec,
	v variant, byID *sync.Map) (record, error) {

	budget := 1_000_000
	switch {
	case v.Budget > 0:
		budget = v.Budget
	case v.Budget < 0:
		budget = 0
	}
	req := memory.SearchRequest{WorkspaceID: ws, RequesterKey: q.Requester, Query: q.Text,
		ResultLimit: v.K, TokenBudget: budget}
	if v.Distinct {
		req.CandidateLimit = 100
	}
	var failed bool
	rctx := context.WithValue(ctx, rerankFailKey{}, &failed)
	t0 := time.Now()
	resp, err := s.Search(rctx, req)
	if err != nil {
		return record{}, fmt.Errorf("search %s %s: %w", q.ID, v.Name, err)
	}
	ms := float64(time.Since(t0).Microseconds()) / 1000

	lookup := func(id uuid.UUID) *msgSpec {
		if m, ok := byID.Load(id); ok {
			return m.(*msgSpec)
		}
		return &msgSpec{}
	}
	var hits []hit
	tokens := 0
	for _, r := range resp.Results {
		h := hit{Anchor: lookup(r.AnchorMessageID)}
		for _, id := range r.SourceMessageIDs {
			h.Sources = append(h.Sources, lookup(id))
		}
		hits = append(hits, h)
		tokens += len(r.Content) / 4
	}
	rec := record{Bench: bench, QID: q.ID, Category: q.Category, Variant: v.Name, K: v.K,
		Results: len(resp.Results), Tokens: tokens, Millis: ms, RerankFailed: failed}
	if v.Distinct {
		rec.Metrics = map[string]float64{
			"session@5":  scoreDistinctSessions(bench, q, hits, 5),
			"session@10": scoreDistinctSessions(bench, q, hits, 10),
		}
	} else {
		rec.Metrics = scoreQuestion(bench, q, hits)
	}
	if resp.Debug != nil {
		rec.RerankApplied = resp.Debug.RerankApplied
		rec.GraphCands = resp.Debug.GraphCandidates
		rec.DenseBest = resp.Debug.DenseBest
		rec.DenseMedian = resp.Debug.DenseMedian
	}
	rec.GraphFacts = len(resp.GraphFacts)
	if v.Graph && len(resp.GraphFacts) > 0 && !v.Distinct {
		// Lecture complémentaire: les messages sources des faits rendus,
		// ajoutés au contexte. Le context_block les transmet au lecteur.
		var fh []hit
		fh = append(fh, hits...)
		for _, f := range resp.GraphFacts {
			for _, id := range f.SourceMessageIDs {
				m := lookup(id)
				fh = append(fh, hit{Anchor: m, Sources: []*msgSpec{m}})
			}
		}
		for k, val := range scoreQuestion(bench, q, fh) {
			rec.Metrics[k+"+faits"] = val
		}
	}
	return rec, nil
}

type rerankFailKey struct{}

// trackedReranker signale un échec du réordonnanceur à la question qui l'a
// provoqué: le service retombe silencieusement sur l'ordre de la fusion, ce
// qui est voulu en production mais fausserait la mesure sans être vu.
type trackedReranker struct{ inner memory.Reranker }

func (t *trackedReranker) Rerank(ctx context.Context, q string, c []memory.RerankCandidate) ([]int, error) {
	order, err := t.inner.Rerank(ctx, q, c)
	if err != nil {
		if p, ok := ctx.Value(rerankFailKey{}).(*bool); ok {
			*p = true
		}
	}
	return order, err
}

func drainGraph(ctx context.Context, pool *pgxpool.Pool, repo *postgres.JobRepo, h jobs.Handler, workers, retryLimit int) (int, error) {
	var n atomic.Int64
	var firstErr error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := repo.Claim(ctx)
				if err != nil {
					mu.Lock()
					firstErr = err
					mu.Unlock()
					return
				}
				if j == nil {
					// Rien de prêt ne veut pas dire fini: un job peut attendre
					// son backoff, ou la fin de l'extraction de sa
					// conversation par un autre worker, qui posera la suite.
					var left int
					if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs
						WHERE job_type = 'graph_extract' AND status IN ('pending', 'running')`).Scan(&left); err != nil || left == 0 {
						return
					}
					time.Sleep(500 * time.Millisecond)
					continue
				}
				// Une extraction ratée (JSON coupé, timeout) repart en backoff
				// comme en production, et finit en lettre morte au bout de
				// jobs.retry_limit, sans arrêter le banc.
				if err := h(ctx, j); err != nil {
					_ = repo.Fail(ctx, j.JobID, err, retryLimit)
				} else if err := repo.Complete(ctx, j.JobID); err != nil {
					mu.Lock()
					firstErr = err
					mu.Unlock()
					return
				}
				if c := n.Add(1); c%500 == 0 {
					fmt.Printf("  graphe : %d extractions\n", c)
				}
			}
		}()
	}
	wg.Wait()
	var failed int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE job_type = 'graph_extract' AND status = 'dead'`).Scan(&failed)
	var covered, total int64
	_ = pool.QueryRow(ctx, `SELECT coalesce(sum(graph_extracted_seq), 0), coalesce(sum(next_sequence), 0) FROM conversations`).Scan(&covered, &total)
	fmt.Printf("  graphe : %d fenêtres en lettre morte, %d/%d messages couverts\n", failed, covered, total)
	return int(n.Load()), firstErr
}

func forEach(n, workers int, f func(i int) error) error {
	next := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if err := f(i); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	return firstErr
}

func loadDone(path string) (map[string]bool, error) {
	done := map[string]bool{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			done[r.QID+"|"+r.Variant] = true
		}
	}
	return done, sc.Err()
}

func printReport(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	type agg struct {
		n                           int
		sum                         map[string]float64
		cnt                         map[string]int
		tokens, ms                  float64
		rerankApplied, rerankFailed int
		byCat                       map[string]map[string][2]float64
	}
	aggs := map[string]*agg{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	bench := ""
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		bench = r.Bench
		a := aggs[r.Variant]
		if a == nil {
			a = &agg{sum: map[string]float64{}, cnt: map[string]int{}, byCat: map[string]map[string][2]float64{}}
			aggs[r.Variant] = a
		}
		a.n++
		a.tokens += float64(r.Tokens)
		a.ms += r.Millis
		if r.RerankApplied {
			a.rerankApplied++
		}
		if r.RerankFailed {
			a.rerankFailed++
		}
		for k, v := range r.Metrics {
			a.sum[k] += v
			a.cnt[k]++
			c := a.byCat[r.Category]
			if c == nil {
				c = map[string][2]float64{}
				a.byCat[r.Category] = c
			}
			x := c[k]
			c[k] = [2]float64{x[0] + v, x[1] + 1}
		}
	}
	names := make([]string, 0, len(aggs))
	for k := range aggs {
		names = append(names, k)
	}
	sort.Strings(names)
	fmt.Printf("\n=== %s ===\n", bench)
	for _, name := range names {
		a := aggs[name]
		var mk []string
		for k := range a.sum {
			mk = append(mk, k)
		}
		sort.Strings(mk)
		var parts []string
		for _, k := range mk {
			parts = append(parts, fmt.Sprintf("%s=%.1f%%", k, 100*a.sum[k]/float64(a.cnt[k])))
		}
		fmt.Printf("%-22s n=%-5d %s | tokens=%.0f ms=%.0f", name, a.n, strings.Join(parts, " "),
			a.tokens/float64(a.n), a.ms/float64(a.n))
		if a.rerankApplied+a.rerankFailed > 0 {
			fmt.Printf(" rerank_appliqué=%d échecs=%d", a.rerankApplied, a.rerankFailed)
		}
		fmt.Println()
		cats := make([]string, 0, len(a.byCat))
		for c := range a.byCat {
			cats = append(cats, c)
		}
		sort.Strings(cats)
		for _, c := range cats {
			var cp []string
			for _, k := range mk {
				if x, ok := a.byCat[c][k]; ok && !strings.Contains(k, "+faits") {
					cp = append(cp, fmt.Sprintf("%s=%.1f%%", k, 100*x[0]/x[1]))
				}
			}
			fmt.Printf("    %-28s n=%-4.0f %s\n", c, a.byCat[c][mk[0]][1], strings.Join(cp, " "))
		}
	}
	return nil
}

// cachedEmbedder mémorise l'embedding de chaque question: toutes les
// variantes cherchent la même.
type cachedEmbedder struct {
	inner memory.Embedder
	mu    sync.Mutex
	m     map[string][]float32
}

func (c *cachedEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	return c.inner.Embed(ctx, in)
}

func (c *cachedEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	c.mu.Lock()
	v, ok := c.m[text]
	c.mu.Unlock()
	if ok {
		return v, nil
	}
	v, err := c.inner.EmbedQuery(ctx, text)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.m[text] = v
	c.mu.Unlock()
	return v, nil
}

func (c *cachedEmbedder) Model() string { return c.inner.Model() }

func cleanupWorkspace(ctx context.Context, pool *pgxpool.Pool, workspace string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		`DELETE FROM conversations WHERE workspace_id = $1`,
		`DELETE FROM graph_entities WHERE workspace_id = $1`,
		`DELETE FROM jobs WHERE workspace_id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, workspace); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func since(t time.Time) string { return time.Since(t).Round(time.Second).String() }
