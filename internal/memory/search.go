package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

// Searcher assemble le chemin de lecture: construire le texte de requête,
// lancer les stratégies de recherche, fusionner, étendre les extraits et les
// faire tenir dans le budget de tokens. Il ne reçoit aucun client de
// complétion: c'est la preuve structurelle du critère d'acceptation 9,
// aucun résumé LLM sur le chemin de lecture.
type Searcher struct {
	cfg     *config.Config
	msgs    MessageRepo
	emb     Embedder
	dense   DenseSearcher
	lexical LexicalSearcher
	graph   GraphSearcher

	// reranker est facultatif: le critère 9 interdit de dépendre d'un appel
	// de modèle sur le chemin de recherche, donc nil est un état normal.
	reranker Reranker
}

// NewSearcher construit un Searcher. graph peut être nil: la couche graphe
// appartient à un autre plan, et son absence est un état normal, pas une
// panne.
func NewSearcher(cfg *config.Config, msgs MessageRepo, emb Embedder,
	dense DenseSearcher, lexical LexicalSearcher, graph GraphSearcher) *Searcher {
	return &Searcher{cfg: cfg, msgs: msgs, emb: emb,
		dense: dense, lexical: lexical, graph: graph}
}

// WithReranker câble un réordonnanceur sur un Searcher déjà construit.
// Séparé du constructeur plutôt qu'ajouté en septième paramètre: le
// réordonnanceur est le seul composant que le critère 9 interdit de rendre
// nécessaire, et un constructeur qui le réclame ferait croire l'inverse à
// chaque appelant.
func (s *Searcher) WithReranker(r Reranker) *Searcher {
	s.reranker = r
	return s
}

// BuildQueryText assemble le texte soumis à l'encodeur, selon la section 11
// de la spec: demandeur, sujets connus, contexte récent puis question. Le
// préfixe d'encodage ("search_query: ") est ajouté par l'embedder, jamais
// ici, sous peine de double préfixage.
func BuildQueryText(req SearchRequest) string {
	var b strings.Builder
	if req.RequesterKey != "" {
		fmt.Fprintf(&b, "Demandeur : %s\n", req.RequesterKey)
	}
	for _, s := range req.KnownSubjects {
		fmt.Fprintf(&b, "Sujet mentionné : %s\n", s)
	}
	if len(req.RecentMessages) > 0 {
		b.WriteString("\nContexte récent :\n")
		recent := req.RecentMessages
		if len(recent) > 4 {
			recent = recent[len(recent)-4:]
		}
		for _, m := range recent {
			fmt.Fprintf(&b, "%s : %s\n", m.AuthorKey, m.Content)
		}
	}
	fmt.Fprintf(&b, "\nQuestion actuelle :\n%s", req.Query)
	return b.String()
}

// validStrategyNames énumère les seules valeurs acceptées dans
// SearchRequest.Strategies. Une valeur hors de cette liste est un signal
// qu'un appelant s'est trompé de nom (une faute de frappe typiquement), pas
// une stratégie à ignorer en silence: un "densee" mal orthographié ne doit
// jamais se comporter comme "aucune mémoire trouvée".
var validStrategyNames = map[string]bool{"dense": true, "lexical": true, "graph": true}

// ErrUnknownStrategy accompagne le refus d'un nom de stratégie inconnu.
// Sentinelle typée plutôt qu'une simple erreur formatée: c'est une faute de
// l'appelant, pas une panne du service, et la couche HTTP doit pouvoir la
// traduire en 400 sans comparer des chaînes de message. Sans elle, toutes
// les erreurs de Search tombaient dans le même writeInternal, donc un
// "densee" mal orthographié réveillait l'astreinte avec un 500.
var ErrUnknownStrategy = errors.New("unknown search strategy")

// Search exécute une recherche hybride. Les étapes:
//
//  1. Rejeter tout nom de stratégie inconnu.
//  2. Compléter la requête avec les valeurs par défaut de configuration.
//  3. Construire le texte soumis à l'encodeur.
//  4. Déterminer l'ensemble des stratégies demandées et la limite de
//     candidats propre à chacune.
//  5. Lancer dense, lexical et graphe en parallèle, chacune dans sa propre
//     goroutine, sans errgroup: une stratégie en panne ne doit jamais
//     empêcher les autres de répondre (critère d'acceptation 10).
//  6. Attendre leur fin.
//  7. N'échouer que si toutes les stratégies tentées ont échoué.
//  8. Fusionner les candidats par rang réciproque, puis appliquer le seuil
//     minimal optionnel s'il est configuré.
//  9. Écarter les candidats dont le message d'ancrage est explicitement
//     exclu par l'appelant.
//  10. Recoller les extraits adjacents d'une même conversation.
//  11. Tronquer au nombre de résultats demandé avant d'étendre quoi que ce
//     soit: étendre un extrait qu'on va jeter gaspillerait des
//     aller-retours en base.
//  12. Étendre chaque extrait retenu autour de son coeur, puis le faire
//     tenir dans le budget de tokens et construire les résultats renvoyés
//     à l'appelant.
func (s *Searcher) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	for _, name := range req.Strategies {
		if !validStrategyNames[name] {
			return SearchResponse{}, fmt.Errorf("%w %q", ErrUnknownStrategy, name)
		}
	}

	s.applyDefaults(&req)

	queryText := BuildQueryText(req)
	wanted := strategySet(req.Strategies)

	// Chaque stratégie a sa propre limite configurée (dense_top_k,
	// lexical_top_k, graph_top_k): les trois existent précisément parce que
	// dense, lexical et graphe n'ont pas le même rappel à la même limite.
	// req.CandidateLimit, quand l'appelant le renseigne, l'emporte pour les
	// trois: une demande explicite prime sur la configuration. Calculées une
	// seule fois ici, avant tout lancement de goroutine, ces valeurs ne sont
	// plus jamais écrites: aucune synchronisation n'est nécessaire pour les
	// lire depuis les goroutines de stratégie.
	denseLimit := candidateLimit(req.CandidateLimit, s.cfg.Retrieval.DenseTopK)
	lexicalLimit := candidateLimit(req.CandidateLimit, s.cfg.Retrieval.LexicalTopK)
	graphLimit := candidateLimit(req.CandidateLimit, s.cfg.Retrieval.GraphTopK)
	// Un graph_top_k à zéro ou négatif dans un fichier de configuration ne
	// doit ni tout laisser passer ni tout couper. Le repli est explicite ici
	// plutôt qu'à moitié dans le SQL (qui applique déjà son propre défaut) et
	// à moitié dans le bornage des faits plus bas: les deux doivent voir la
	// même valeur, sans quoi la traversée rendrait vingt candidats et zéro
	// fait, pour la même requête.
	if graphLimit <= 0 {
		graphLimit = defaultGraphLimit
	}

	var (
		mu         sync.Mutex
		wg         sync.WaitGroup
		denseOut   []Candidate
		lexOut     []Candidate
		graphOut   []Candidate
		graphFacts []GraphFact
		errs       = map[string]string{}
		attempted  int
	)

	record := func(name string, err error) {
		mu.Lock()
		defer mu.Unlock()
		errs[name] = err.Error()
		// Une stratégie en panne doit être observable dans les logs, pas
		// seulement dans le bloc debug de la réponse (qui n'existe que si
		// debug_search est activé): sinon une dégradation gracieuse du
		// critère 10 est indiscernable d'un service qui se dégrade en
		// silence. Aucun contenu de message ni de requête dans le log.
		slog.WarnContext(ctx, "search strategy failed",
			"strategy", name, "workspace_id", req.WorkspaceID, "error", err)
	}

	// Les stratégies partent en parallèle. Pas d'errgroup: il annulerait les
	// goroutines survivantes au premier échec, exactement l'inverse de ce
	// que demande le critère 10. Une recherche n'échoue que si toutes les
	// stratégies tentées ont échoué, ce qui est vérifié plus bas en
	// comparant le nombre de tentatives au nombre d'erreurs, pas la taille
	// des résultats: une stratégie qui répond avec zéro candidat a réussi.
	// Le vecteur de la question est calculé une fois, avant la mise en
	// parallèle, parce que deux stratégies s'en servent désormais: le dense
	// pour trouver ses candidats, le graphe pour classer ses faits par
	// pertinence à la question. Le calculer dans chaque goroutine ferait
	// deux appels à l'embedder pour un seul besoin.
	//
	// L'échec est porté jusqu'aux stratégies plutôt que remonté ici: le
	// dense ne peut rien faire sans vecteur et déclare son échec, le graphe
	// s'en passe et retombe sur son ordre historique. C'est le critère 10,
	// une panne de l'embedder dégrade sans rendre indisponible.
	var (
		queryVec    []float32
		queryVecErr error
	)
	if (wanted["dense"] && s.dense != nil) ||
		(wanted["graph"] && s.graph != nil && s.cfg.Graph.Enabled) {
		queryVec, queryVecErr = s.embedQuery(ctx, queryText)
		if queryVecErr != nil {
			queryVec = nil
		}
	}

	if wanted["dense"] && s.dense != nil {
		attempted++
		wg.Add(1)
		go func() {
			defer wg.Done()
			vec, err := queryVec, queryVecErr
			if err != nil {
				record("dense", err)
				return
			}
			out, err := s.dense.SearchDense(ctx, DenseQuery{
				CandidateQuery: CandidateQuery{
					WorkspaceID: req.WorkspaceID, RequesterKey: req.RequesterKey,
					Limit: denseLimit,
				},
				Embedding: vec,
			})
			if err != nil {
				record("dense", err)
				return
			}
			mu.Lock()
			denseOut = out
			mu.Unlock()
		}()
	}

	if wanted["lexical"] && s.lexical != nil {
		attempted++
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := s.lexical.SearchLexical(ctx, LexicalQuery{
				CandidateQuery: CandidateQuery{
					WorkspaceID: req.WorkspaceID, RequesterKey: req.RequesterKey,
					Limit: lexicalLimit,
				},
				Text: req.Query,
			})
			if err != nil {
				record("lexical", err)
				return
			}
			mu.Lock()
			lexOut = out
			mu.Unlock()
		}()
	}

	if wanted["graph"] && s.graph != nil && s.cfg.Graph.Enabled {
		attempted++
		wg.Add(1)
		go func() {
			defer wg.Done()
			subjects := append([]string{}, req.KnownSubjects...)
			if req.ConversationID != "" {
				if parts, err := s.msgs.Participants(ctx, req.ConversationID); err == nil {
					subjects = append(subjects, parts...)
				}
			}
			out, err := s.graph.SearchGraph(ctx, GraphQuery{
				CandidateQuery: CandidateQuery{
					WorkspaceID: req.WorkspaceID, RequesterKey: req.RequesterKey,
					Limit: graphLimit,
				},
				Subjects:  subjects,
				Text:      req.Query,
				MaxHops:   s.cfg.Graph.MaxHops,
				Embedding: queryVec,
			})
			if err != nil {
				record("graph", err)
				return
			}
			mu.Lock()
			// Les faits remontent toujours; les candidats seulement si la
			// configuration le demande. Voir config.Graph.FuseCandidates
			// pour la mesure qui justifie de pouvoir les couper.
			if s.cfg.Graph.FuseCandidates {
				graphOut = out.Candidates
			}
			graphFacts = out.Facts
			mu.Unlock()
		}()
	}

	wg.Wait()

	// Une recherche n'échoue que si toutes les stratégies tentées ont
	// échoué. Une stratégie jamais demandée (attempted ne la compte pas) ne
	// peut jamais faire échouer la recherche, et une stratégie qui a répondu
	// avec zéro candidat n'est pas une stratégie en échec.
	if attempted > 0 && len(errs) == attempted {
		return SearchResponse{}, fmt.Errorf("all attempted search strategies failed: %v", errs)
	}
	if attempted == 0 {
		// Toutes les stratégies demandées existent (sinon Search aurait déjà
		// échoué plus haut) mais aucune n'a de searcher câblé ou n'est
		// activée par la configuration: ce n'est pas une erreur (le graphe,
		// par exemple, est légitimement absent tant que son plan n'est pas
		// livré), mais ça doit rester visible plutôt que de se confondre
		// silencieusement avec "aucun souvenir pertinent".
		slog.WarnContext(ctx, "search attempted no strategy",
			"workspace_id", req.WorkspaceID, "requested_strategies", req.Strategies)
	}

	// Plancher de pertinence sur le score dense brut, avant la fusion. Le
	// score dense est une similarité cosinus: il garde son sens hors de son
	// classement, contrairement au score fusionné qui n'encode qu'un rang.
	// Seuls les candidats du dense sont filtrés; une correspondance lexicale
	// et un fait du graphe restent des preuves de pertinence par eux-mêmes.
	// La forme de la distribution est calculée avant tout filtrage: c'est
	// elle qui dit si le meilleur candidat se détache ou si l'embedder a
	// rendu un voisinage plat, et un plancher appliqué d'abord la
	// tronquerait.
	denseBest, denseMedian := denseShape(denseOut)

	// Détection d'une question sans réponse: le voisinage dense est à la
	// fois lointain et plat, donc l'embedder n'a rien trouvé de proche et
	// rien qui se détache. Les candidats du dense partent alors en entier,
	// et la réponse ne tient plus qu'aux stratégies qui, elles, savent dire
	// qu'elles n'ont rien trouvé: le lexical ne rend rien quand la requête
	// ne matche aucun terme, le graphe rien quand aucune graine ne prend.
	//
	// La conjonction est le coeur de la règle. Écarter sur la seule
	// similarité absolue coûte les questions dont la réponse est
	// sémantiquement lointaine; écarter sur la seule marge coûte celles
	// dont plusieurs messages sont également pertinents. Les deux ensemble
	// ne visent que le cas où il n'y a ni l'un ni l'autre.
	noAnswer := false
	if a, b := s.cfg.Retrieval.NoAnswerBestBelow, s.cfg.Retrieval.NoAnswerMarginBelow; a != nil && b != nil {
		if len(denseOut) > 0 && denseBest < *a && denseBest-denseMedian < *b {
			noAnswer = true
		}
	}

	droppedByFloor := 0
	if noAnswer {
		droppedByFloor = len(denseOut)
		denseOut = nil
	} else if floor := s.cfg.Retrieval.MinimumDenseScore; floor != nil {
		kept := denseOut[:0]
		for _, c := range denseOut {
			if c.RawScore >= *floor {
				kept = append(kept, c)
			}
		}
		droppedByFloor = len(denseOut) - len(kept)
		denseOut = kept
	}

	fused := FuseRRF(s.cfg.Retrieval.RRFK, denseOut, lexOut, graphOut)
	// fusedCount est capturé avant le seuil minimal: Debug.Fused doit
	// rendre compte de la fusion elle-même, pas de ce qu'il en reste après
	// un filtre optionnel, sous peine de rendre les rejets par seuil
	// invisibles.
	fusedCount := len(fused)
	if threshold := s.cfg.Retrieval.MinimumScore; threshold != nil {
		kept := fused[:0]
		for _, f := range fused {
			if f.Score >= *threshold {
				kept = append(kept, f)
			}
		}
		fused = kept
	}

	excluded := map[uuid.UUID]bool{}
	for _, id := range req.ExcludeMessageIDs {
		excluded[id] = true
	}
	filtered := make([]Fused, 0, len(fused))
	discardedExcluded := 0
	for _, f := range fused {
		if excluded[f.AnchorMessageID] {
			discardedExcluded++
			continue
		}
		filtered = append(filtered, f)
	}

	merged := MergeAdjacent(filtered)
	// afterMergeCount est capturé avant la troncature au ResultLimit: sinon
	// Debug.AfterMerge rendrait le compte après troncature sous un nom qui
	// promet le compte après recollement, et le nombre d'extraits jetés par
	// la limite deviendrait invisible.
	afterMergeCount := len(merged)

	// Quand le réordonnanceur est câblé, on garde un vivier plus large que
	// la limite demandée: sans marge il n'y a rien à réordonner. Le vivier
	// est tronqué après le réordonnancement.
	keep := req.ResultLimit
	reranking := s.reranker != nil && s.cfg.Rerank.Enabled &&
		s.cfg.Rerank.Pool > req.ResultLimit
	if reranking {
		keep = s.cfg.Rerank.Pool
	}
	if len(merged) > keep {
		merged = merged[:keep]
	}

	excerpts, err := s.expand(ctx, merged, excluded)
	if err != nil {
		return SearchResponse{}, err
	}

	// Le réordonnancement a besoin du texte des extraits, donc il vient
	// après l'expansion et avant le budget de tokens: réordonner d'abord
	// ferait dégrader l'expansion des extraits que le modèle vient de
	// classer en tête.
	//
	// Un échec ne remonte pas: on garde l'ordre de la fusion et on
	// journalise, comme pour une stratégie en panne. C'est la même posture
	// que le critère 10 appliquée à un composant que le critère 9 interdit
	// de rendre nécessaire.
	rerankApplied := false
	if reranking && len(excerpts) > req.ResultLimit {
		cands := make([]RerankCandidate, 0, len(excerpts))
		for _, e := range excerpts {
			cands = append(cands, RerankCandidate{Text: excerptText(e)})
		}
		order, rerr := s.reranker.Rerank(ctx, req.Query, cands)
		if rerr != nil {
			slog.WarnContext(ctx, "rerank failed, keeping fusion order",
				"workspace_id", req.WorkspaceID, "error", rerr)
		} else if len(order) > 0 {
			reordered := make([]Excerpt, 0, len(excerpts))
			for _, i := range order {
				if i >= 0 && i < len(excerpts) {
					reordered = append(reordered, excerpts[i])
				}
			}
			if len(reordered) == len(excerpts) {
				// Le score porte l'ordre, parce que tout l'aval s'y fie:
				// FitBudget retrie défensivement par score décroissant pour
				// savoir quel extrait dégrader en premier, et rendrait donc
				// l'ordre de la fusion si on ne réécrivait pas le score.
				// Le score de fusion n'est pas perdu, il passe dans
				// RawScores sous la clé "fusion" et reste rendu à
				// l'appelant.
				for i := range reordered {
					if reordered[i].RawScores == nil {
						reordered[i].RawScores = map[string]float64{}
					}
					reordered[i].RawScores["fusion"] = reordered[i].Score
					reordered[i].Score = float64(len(reordered) - i)
				}
				excerpts = reordered
				rerankApplied = true
			}
		}
	}
	if len(excerpts) > req.ResultLimit {
		excerpts = excerpts[:req.ResultLimit]
	}

	beforeBudget := len(excerpts)
	excerpts = FitBudget(excerpts, req.TokenBudget, s.cfg.Indexing.CharsPerToken)

	results, err := s.toResults(ctx, excerpts)
	if err != nil {
		return SearchResponse{}, err
	}

	// Les faits du graphe ne passent ni par la fusion RRF ni par le budget
	// de tokens des extraits: ils sont bornés ici, à graph_top_k faits au
	// plus, ce qui les empêche de gonfler indéfiniment sans les faire
	// concurrencer les extraits pour la place dans le context_block.
	// graphLimit, et non s.cfg.Retrieval.GraphTopK: c'est la même valeur que
	// celle passée à la traversée, repli compris. Tester la valeur brute
	// avec un "limit > 0" désactivait silencieusement le bornage quand
	// graph_top_k valait zéro.
	if len(graphFacts) > graphLimit {
		graphFacts = graphFacts[:graphLimit]
	}

	resp := SearchResponse{QueryID: uuid.NewString(), Results: results, GraphFacts: graphFacts}
	if req.IncludeContextBlock {
		resp.ContextBlock = RenderContextBlock(graphFacts, results)
	}
	if s.cfg.Service.DebugSearch {
		resp.Debug = &SearchDebug{
			DenseCandidates:     len(denseOut),
			DenseDroppedByFloor: droppedByFloor,
			RerankApplied:       rerankApplied,
			DenseNoAnswer:       noAnswer,
			DenseBest:           denseBest,
			DenseMedian:         denseMedian,
			LexicalCandidates:   len(lexOut),
			GraphCandidates:     len(graphOut),
			Fused:               fusedCount,
			AfterMerge:          afterMergeCount,
			DiscardedAsExcluded: discardedExcluded,
			DroppedByBudget:     beforeBudget - len(excerpts),
			// La spec d'origine prévoyait un compteur discarded_by_acl. Le
			// compter exigerait de rejouer chaque requête sans le prédicat
			// d'accès, donc de matérialiser des candidats non autorisés, ce
			// que la section 5 de la spec interdit. ACLFiltered atteste
			// seulement que le filtre a été appliqué en SQL.
			ACLFiltered:    true,
			StrategyErrors: errs,
		}
	}

	// Journalisation des accès aux souvenirs, section 16 de la spec. Aucun
	// contenu de message ni de requête dans les logs.
	slog.InfoContext(ctx, "memory search",
		"query_id", resp.QueryID,
		"workspace_id", req.WorkspaceID,
		"requester_key", req.RequesterKey,
		"results", len(results),
		"strategy_errors", len(errs))

	return resp, nil
}

// defaultGraphLimit est le repli quand ni la requête ni la configuration ne
// donnent de limite exploitable pour le graphe. Même valeur que le défaut de
// graph_top_k dans config.
const defaultGraphLimit = 20

// denseShape rend la meilleure similarité dense et la médiane des
// similarités, sur les candidats tels que le SQL les a rendus. Les deux
// ensemble décrivent la forme du voisinage: un écart large veut dire qu'un
// candidat se détache, un écart étroit que l'embedder n'a trouvé qu'un
// plateau de voisins équivalents, ce qui est la signature d'une question à
// laquelle rien ne répond.
//
// La médiane plutôt que la moyenne par robustesse de principe, un candidat
// aberrant ne la déplaçant pas. Mais il faut être honnête sur la portée du
// choix: au seuil en usage les deux grandeurs ne se croisent pratiquement
// jamais, puisque best est le maximum et que rapprocher la moyenne de best
// rapproche aussi la médiane. Aucun test ne distingue donc les deux, et
// remplacer la médiane par la moyenne ne fait rien rougir. C'est écrit ici
// pour que personne ne croie le contraire en lisant le mot médiane.
func denseShape(cands []Candidate) (best, median float64) {
	if len(cands) == 0 {
		return 0, 0
	}
	scores := make([]float64, 0, len(cands))
	for _, c := range cands {
		scores = append(scores, c.RawScore)
	}
	sort.Float64s(scores)
	best = scores[len(scores)-1]
	median = scores[len(scores)/2]
	return best, median
}

// candidateLimit rend la limite de candidats à utiliser pour une stratégie:
// la valeur explicite de la requête si elle est renseignée, sinon la valeur
// par défaut propre à cette stratégie.
func candidateLimit(explicit, strategyDefault int) int {
	if explicit > 0 {
		return explicit
	}
	return strategyDefault
}

func (s *Searcher) applyDefaults(req *SearchRequest) {
	if req.ResultLimit <= 0 {
		req.ResultLimit = s.cfg.Retrieval.FinalTopK
	}
	if req.TokenBudget <= 0 {
		req.TokenBudget = s.cfg.Retrieval.MaxMemoryTokens
	}
	if len(req.Strategies) == 0 {
		req.Strategies = []string{"dense", "lexical", "graph"}
	}
}

// embedQuery encode le texte de requête. EmbedQuery applique elle-même le
// préfixe search_query: composer ce préfixe ici double-préfixerait le texte
// et dégraderait le rappel en silence.
func (s *Searcher) embedQuery(ctx context.Context, text string) ([]float32, error) {
	return s.emb.EmbedQuery(ctx, text)
}

// expand recharge, pour chaque extrait fusionné, la fenêtre de messages
// originaux qui l'entoure. Les messages exclus par l'appelant ou déjà
// supprimés sont écartés ici: c'est le filet du critère d'acceptation 6,
// jamais rendre un message supprimé. Un extrait dont la fenêtre ne contient
// plus aucun message exploitable est abandonné plutôt que rendu vide.
//
// MessageRepo.Around ne filtre que sur la conversation, l'intervalle de
// séquence et deleted_at: il n'a aucun prédicat d'accès, et n'en aura
// jamais, puisque la règle d'accès est appliquée par les stratégies au
// moment de produire les candidats. C'est donc ici, et seulement ici, que se
// décide ce que la fenêtre a le droit de contenir. Pour un candidat admis
// par une ligne memory_unit_acl (AccessReasonExplicitACL), l'octroi porte
// sur une unité et pas sur la conversation: la fenêtre est bornée à
// exactement [StartSequence, EndSequence], sans aucune expansion. L'élargir
// rendrait des messages que rien ne couvre, dont certains strictement
// postérieurs à l'unité partagée, et transformerait un partage d'unité en
// accès à toute la conversation pour les scopes 'explicit' et 'private',
// que ReadableCTE tient délibérément hors de la règle au niveau
// conversation. Pour 'workspace' et 'participants' l'expansion reste sans
// effet sur l'accès, la conversation entière étant lisible.
func (s *Searcher) expand(ctx context.Context, items []Fused,
	excluded map[uuid.UUID]bool) ([]Excerpt, error) {

	out := make([]Excerpt, 0, len(items))
	for _, f := range items {
		from, to := f.StartSequence, f.EndSequence
		if f.AccessReason != AccessReasonExplicitACL {
			from = f.StartSequence - int64(s.cfg.Retrieval.ExpandBefore)
			if from < 0 {
				from = 0
			}
			to = f.EndSequence + int64(s.cfg.Retrieval.ExpandAfter)
		}

		window, err := s.msgs.Around(ctx, f.ConversationID, from, to)
		if err != nil {
			return nil, fmt.Errorf("expand excerpt: %w", err)
		}
		kept := make([]Message, 0, len(window))
		for _, m := range window {
			if excluded[m.MessageID] || m.DeletedAt != nil {
				continue
			}
			kept = append(kept, m)
		}
		if len(kept) == 0 {
			continue
		}
		out = append(out, Excerpt{
			Fused:    f,
			Messages: kept,
			CoreFrom: f.StartSequence,
			CoreTo:   f.EndSequence,
		})
	}
	return out, nil
}

// toResults transforme les extraits étendus en résultats exposés à
// l'appelant. Un extrait sans message n'atteint jamais cette fonction
// (expand l'a déjà écarté), donc Content et SourceMessageIDs ne sont jamais
// vides ici.
func (s *Searcher) toResults(ctx context.Context, excerpts []Excerpt) ([]Result, error) {
	participantsCache := map[string][]string{}

	out := make([]Result, 0, len(excerpts))
	for _, e := range excerpts {
		// Un extrait admis par une seule ligne memory_unit_acl ne rend
		// jamais la liste des participants: l'octroi porte sur une unité,
		// pas sur la conversation, et un bénéficiaire qui n'a pas le droit
		// de lire la conversation n'a pas non plus à en apprendre la
		// composition. Même raison que la fenêtre bornée dans expand.
		var parts []string
		if e.AccessReason != AccessReasonExplicitACL {
			cached, ok := participantsCache[e.ConversationID]
			if !ok {
				p, err := s.msgs.Participants(ctx, e.ConversationID)
				if err != nil {
					return nil, fmt.Errorf("load participants: %w", err)
				}
				cached = p
				participantsCache[e.ConversationID] = p
			}
			parts = cached
		}

		var (
			sb       strings.Builder
			sources  = make([]uuid.UUID, 0, len(e.Messages))
			occurred time.Time
		)
		for i, m := range e.Messages {
			if i > 0 {
				sb.WriteString("\n")
			}
			fmt.Fprintf(&sb, "%s : %s", m.AuthorKey, m.Content)
			sources = append(sources, m.MessageID)
			if m.MessageID == e.AnchorMessageID || occurred.IsZero() {
				occurred = m.CreatedAt
			}
		}

		scores := map[string]float64{"final": e.Score}
		for k, v := range e.RawScores {
			scores[k] = v
		}

		memoryID := e.AnchorMessageID.String()
		if e.MemoryUnitID != nil {
			memoryID = e.MemoryUnitID.String()
		}

		out = append(out, Result{
			MemoryID:         memoryID,
			ConversationID:   e.ConversationID,
			AnchorMessageID:  e.AnchorMessageID,
			SourceMessageIDs: sources,
			OccurredAt:       occurred,
			Content:          sb.String(),
			Participants:     parts,
			Scores:           scores,
			MatchedEntities:  e.MatchedEntities,
			AccessReason:     e.AccessReason,
			SourceType:       "original_messages",
		})
	}
	return out, nil
}

func strategySet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}
