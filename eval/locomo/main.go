// Commande locomo mesure le service sur LoCoMo (snap-research/locomo), le
// jeu public utilisé par Mem0, Zep et Letta pour publier leurs chiffres.
// Chaque question y porte les identifiants des tours de dialogue qui
// contiennent la réponse (champ evidence), ce qui permet de mesurer le rappel
// de la recherche seule, sans modèle juge.
//
// Chaque conversation LoCoMo devient un workspace, chaque session une
// conversation du service, datée comme dans le jeu. La commande mesure aussi
// les latences d'ingestion et de recherche, et écrit les contextes rendus en
// JSONL pour une évaluation de bout en bout par un modèle lecteur.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/llm"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

type turn struct {
	Speaker     string `json:"speaker"`
	DiaID       string `json:"dia_id"`
	Text        string `json:"text"`
	BlipCaption string `json:"blip_caption"`
}

type qa struct {
	Question string          `json:"question"`
	Answer   json.RawMessage `json:"answer"`
	Evidence []string        `json:"evidence"`
	Category int             `json:"category"`
}

type sample struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []qa                       `json:"qa"`
}

type session struct {
	num   int
	date  time.Time
	turns []turn
}

type variant struct {
	name       string
	strategies []string
	k          int
	budget     int
	noFloor    bool
}

type question struct {
	sample   string
	ws       string
	speaker  string
	q        qa
	evidence map[string]bool
}

type contextOut struct {
	Sample   string          `json:"sample_id"`
	Question string          `json:"question"`
	Answer   json.RawMessage `json:"answer"`
	Category int             `json:"category"`
	Variant  string          `json:"variant"`
	Context  string          `json:"context"`
	Tokens   int             `json:"tokens"`
	Millis   float64         `json:"search_ms"`
}

var sessionKey = regexp.MustCompile(`^session_(\d+)$`)

func main() {
	var (
		dataPath   = flag.String("data", "locomo10.json", "fichier locomo10.json")
		configPath = flag.String("config", "config.yaml", "configuration du service")
		outPath    = flag.String("out", "locomo-contexts.jsonl", "contextes rendus, pour l'évaluation de bout en bout")
		samples    = flag.Int("samples", 10, "nombre de conversations LoCoMo à charger")
		workers    = flag.Int("concurrency", 8, "requêtes simultanées pour la mesure de débit")
		keep       = flag.Bool("keep", false, "conserver les workspaces créés")
		reuse      = flag.String("reuse", "", "préfixe d'un run conservé par -keep: saute l'ingestion")
	)
	flag.Parse()
	// Le service journalise chaque recherche: sur des milliers de requêtes,
	// ces lignes noieraient les résultats.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := run(*dataPath, *configPath, *outPath, *samples, *workers, *keep, *reuse); err != nil {
		fmt.Fprintln(os.Stderr, "locomo:", err)
		os.Exit(1)
	}
}

func run(dataPath, configPath, outPath string, nSamples, workers int, keep bool, reuse string) error {
	raw, err := os.ReadFile(dataPath)
	if err != nil {
		return err
	}
	var data []sample
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	if nSamples < len(data) {
		data = data[:nSamples]
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	cfg.Graph.Enabled = false
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
	ingester := memory.NewIngester(cfg, msgs, units, embedder, jobRepo)

	run := "lc_" + uuid.NewString()[:6]
	if reuse != "" {
		run = reuse
		keep = true
	}
	fmt.Printf("run : %s\n", run)
	diaByMsg := map[uuid.UUID]string{}
	var workspaces []string
	var questions []question
	var ingestLat []float64
	var ingestChars int

	if !keep {
		defer func() {
			for _, ws := range workspaces {
				if err := cleanupWorkspace(ctx, pool, ws); err != nil {
					fmt.Fprintf(os.Stderr, "nettoyage %s: %v\n", ws, err)
				}
			}
		}()
	}

	start := time.Now()
	for _, s := range data {
		ws := run + "_" + s.SampleID
		workspaces = append(workspaces, ws)
		var speakerA, speakerB string
		_ = json.Unmarshal(s.Conversation["speaker_a"], &speakerA)
		_ = json.Unmarshal(s.Conversation["speaker_b"], &speakerB)
		participants := []string{authorKey(speakerA), authorKey(speakerB)}

		sessions, err := parseSessions(s.Conversation)
		if err != nil {
			return fmt.Errorf("%s: %w", s.SampleID, err)
		}
		for _, sess := range sessions {
			convID := fmt.Sprintf("%s_s%d", ws, sess.num)
			if reuse != "" {
				rows, err := pool.Query(ctx, `SELECT message_id FROM messages
					WHERE conversation_id = $1 ORDER BY sequence_number`, convID)
				if err != nil {
					return err
				}
				i := 0
				for rows.Next() {
					var id uuid.UUID
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						return err
					}
					if i < len(sess.turns) {
						diaByMsg[id] = s.SampleID + "/" + sess.turns[i].DiaID
					}
					i++
				}
				rows.Close()
				if i != len(sess.turns) {
					return fmt.Errorf("%s: %d messages en base, %d tours attendus", convID, i, len(sess.turns))
				}
				continue
			}
			if err := convs.Declare(ctx, convID, ws, "participants", participants); err != nil {
				return err
			}
			for i, t := range sess.turns {
				content := t.Text
				if t.BlipCaption != "" {
					content += " [partage une image : " + t.BlipCaption + "]"
				}
				t0 := time.Now()
				res, err := ingester.Ingest(ctx, memory.AppendInput{
					WorkspaceID: ws, ConversationID: convID,
					AuthorKey: authorKey(t.Speaker), Role: "user", Content: content,
					CreatedAt: sess.date.Add(time.Duration(i) * time.Minute),
				}, "searchable")
				if err != nil {
					return fmt.Errorf("ingest %s %s: %w", s.SampleID, t.DiaID, err)
				}
				if res.Status != "searchable" {
					return fmt.Errorf("ingest %s %s: status %q", s.SampleID, t.DiaID, res.Status)
				}
				ingestLat = append(ingestLat, ms(time.Since(t0)))
				ingestChars += len(content)
				diaByMsg[res.Message.MessageID] = s.SampleID + "/" + t.DiaID
			}
		}
		for _, q := range s.QA {
			// La catégorie 5 (questions piège sans réponse) est exclue par
			// tous les résultats publiés sur LoCoMo, et n'a pas d'evidence
			// exploitable.
			if q.Category == 5 || len(q.Evidence) == 0 {
				continue
			}
			ev := map[string]bool{}
			for _, e := range q.Evidence {
				for _, part := range strings.Split(e, ";") {
					part = strings.TrimSpace(part)
					if part != "" {
						ev[s.SampleID+"/"+part] = true
					}
				}
			}
			questions = append(questions, question{
				sample: s.SampleID, ws: ws, speaker: authorKey(speakerA), q: q, evidence: ev,
			})
		}
		fmt.Printf("ingéré %s (%d messages cumulés)\n", s.SampleID, len(ingestLat))
	}
	ingestDur := time.Since(start)
	fmt.Printf("\nINGESTION : %d messages, %d caractères en %s, %.1f msg/s\n",
		len(ingestLat), ingestChars, ingestDur.Round(time.Second),
		float64(len(ingestLat))/ingestDur.Seconds())
	printLat("latence d'ingestion (searchable, embedding compris)", ingestLat)

	// Chaque question est embeddée une seule fois, ici, et mesurée à part.
	// Les recherches qui suivent réutilisent ce vecteur: leur latence est
	// celle du service seul, et les cinq variantes ne repaient pas cinq fois
	// un embedder qui, sur ce poste, domine tout le reste.
	cache := &cachedEmbedder{inner: embedder, m: map[string][]float32{}}
	var embLat []float64
	for _, q := range questions {
		// La clé doit être le texte que le service embedde réellement, qui
		// ajoute le demandeur à la question: sans ça le cache ne sert jamais.
		text := memory.BuildQueryText(memory.SearchRequest{
			RequesterKey: q.speaker, Query: q.q.Question,
		})
		t0 := time.Now()
		if _, err := cache.EmbedQuery(ctx, text); err != nil {
			return err
		}
		embLat = append(embLat, ms(time.Since(t0)))
	}
	printLat("embedding de la question seul (Ollama)", embLat)

	variants := []variant{
		{name: "hybride_k5", k: 5},
		{name: "hybride_k5_sans_plancher", k: 5, noFloor: true},
		{name: "hybride_k10_sans_plancher", k: 10, budget: 4000, noFloor: true},
		{name: "dense_k5_sans_plancher", strategies: []string{"dense"}, k: 5, noFloor: true},
		{name: "lexical_k5", strategies: []string{"lexical"}, k: 5},
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	enc := json.NewEncoder(out)

	for _, v := range variants {
		vcfg := *cfg
		vcfg.Retrieval = cfg.Retrieval
		if v.noFloor {
			vcfg.Retrieval.MinimumDenseScore = nil
			vcfg.Retrieval.NoAnswerBestBelow = nil
			vcfg.Retrieval.NoAnswerMarginBelow = nil
		}
		finder := memory.NewSearcher(&vcfg, msgs, cache, searchRepo, searchRepo, nil)

		type catStat struct {
			n, hitAny, hitAll int
			frac              float64
		}
		cats := map[int]*catStat{}
		var anchorFrac, ctxFrac float64
		var anyHit, empty, tokens int
		var lat []float64

		for _, q := range questions {
			t0 := time.Now()
			resp, err := finder.Search(ctx, memory.SearchRequest{
				WorkspaceID: q.ws, RequesterKey: q.speaker, Query: q.q.Question,
				Strategies: v.strategies, ResultLimit: v.k, TokenBudget: v.budget,
			})
			if err != nil {
				return fmt.Errorf("search %s: %w", v.name, err)
			}
			d := time.Since(t0)
			lat = append(lat, ms(d))

			anchors, seen := map[string]bool{}, map[string]bool{}
			var sb strings.Builder
			for _, r := range resp.Results {
				anchors[diaByMsg[r.AnchorMessageID]] = true
				for _, src := range r.SourceMessageIDs {
					seen[diaByMsg[src]] = true
				}
				fmt.Fprintf(&sb, "[%s]\n%s\n\n", r.OccurredAt.Format("2 January 2006"), r.Content)
			}
			if len(resp.Results) == 0 {
				empty++
			}
			ctxText := sb.String()
			tok := len(ctxText) / 4
			tokens += tok

			nA, nC := 0, 0
			for e := range q.evidence {
				if anchors[e] {
					nA++
				}
				if seen[e] {
					nC++
				}
			}
			fa := float64(nA) / float64(len(q.evidence))
			fc := float64(nC) / float64(len(q.evidence))
			anchorFrac += fa
			ctxFrac += fc
			if nC > 0 {
				anyHit++
			}
			c := cats[q.q.Category]
			if c == nil {
				c = &catStat{}
				cats[q.q.Category] = c
			}
			c.n++
			c.frac += fc
			if nC > 0 {
				c.hitAny++
			}
			if nC == len(q.evidence) {
				c.hitAll++
			}

			if err := enc.Encode(contextOut{
				Sample: q.sample, Question: q.q.Question, Answer: q.q.Answer,
				Category: q.q.Category, Variant: v.name, Context: ctxText,
				Tokens: tok, Millis: ms(d),
			}); err != nil {
				return err
			}
		}

		n := float64(len(questions))
		fmt.Printf("\n=== %s (%d questions) ===\n", v.name, len(questions))
		fmt.Printf("rappel des preuves, message ancre     : %.1f %%\n", 100*anchorFrac/n)
		fmt.Printf("rappel des preuves, contexte rendu    : %.1f %%\n", 100*ctxFrac/n)
		fmt.Printf("au moins une preuve dans le contexte  : %.1f %%\n", 100*float64(anyHit)/n)
		fmt.Printf("réponses vides                        : %d\n", empty)
		fmt.Printf("tokens de contexte moyens             : %.0f\n", float64(tokens)/n)
		keys := make([]int, 0, len(cats))
		for k := range cats {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			c := cats[k]
			fmt.Printf("  catégorie %d %-12s n=%-4d rappel=%.1f %%  toutes preuves=%.1f %%\n",
				k, catName(k), c.n, 100*c.frac/float64(c.n), 100*float64(c.hitAll)/float64(c.n))
		}
		printLat("latence de recherche hors embedding, séquentielle", lat)
	}

	// Débit sous charge: la configuration livrée, toutes les questions
	// réparties sur plusieurs goroutines.
	finder := memory.NewSearcher(cfg, msgs, cache, searchRepo, searchRepo, nil)
	var mu sync.Mutex
	var loadLat []float64
	next := make(chan question)
	var wg sync.WaitGroup
	var firstErr error
	t0 := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for q := range next {
				s := time.Now()
				_, err := finder.Search(ctx, memory.SearchRequest{
					WorkspaceID: q.ws, RequesterKey: q.speaker, Query: q.q.Question,
				})
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				loadLat = append(loadLat, ms(time.Since(s)))
				mu.Unlock()
			}
		}()
	}
	for _, q := range questions {
		next <- q
	}
	close(next)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	total := time.Since(t0)
	fmt.Printf("\n=== débit, %d requêtes simultanées ===\n", workers)
	fmt.Printf("%d recherches en %s : %.1f requêtes/s\n", len(loadLat),
		total.Round(time.Millisecond), float64(len(loadLat))/total.Seconds())
	printLat("latence sous charge, hors embedding", loadLat)
	return nil
}

// cachedEmbedder mémorise l'embedding de chaque question: le banc interroge
// cinq variantes avec les mêmes questions.
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

func parseSessions(conv map[string]json.RawMessage) ([]session, error) {
	var out []session
	for k, v := range conv {
		m := sessionKey.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		num, _ := strconv.Atoi(m[1])
		var turns []turn
		if err := json.Unmarshal(v, &turns); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		var dateStr string
		_ = json.Unmarshal(conv[k+"_date_time"], &dateStr)
		date, err := time.Parse("3:04 pm on 2 January, 2006", strings.ToLower(dateStr))
		if err != nil {
			date, err = time.Parse("3:04 pm on 2 january, 2006", strings.ToLower(dateStr))
			if err != nil {
				return nil, fmt.Errorf("%s: date %q: %w", k, dateStr, err)
			}
		}
		out = append(out, session{num: num, date: date, turns: turns})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].num < out[j].num })
	return out, nil
}

func authorKey(name string) string {
	return "user:" + strings.ToLower(strings.ReplaceAll(name, " ", "_"))
}

func catName(c int) string {
	switch c {
	case 1:
		return "multi-saut"
	case 2:
		return "temporel"
	case 3:
		return "ouvert"
	case 4:
		return "simple"
	}
	return "?"
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func printLat(label string, v []float64) {
	if len(v) == 0 {
		return
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	p := func(q float64) float64 { return s[int(q*float64(len(s)-1))] }
	var sum float64
	for _, x := range s {
		sum += x
	}
	fmt.Printf("%s : moy %.1f ms, p50 %.1f, p95 %.1f, p99 %.1f, max %.1f (n=%d)\n",
		label, sum/float64(len(s)), p(0.5), p(0.95), p(0.99), s[len(s)-1], len(s))
}

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
