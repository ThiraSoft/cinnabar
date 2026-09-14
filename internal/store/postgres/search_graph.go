package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/ThiraSoft/cinnabar/internal/graph"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// maxGraphSourcesPerRelation plafonne le nombre de messages sources rendus
// pour une même relation.
//
// La limite de la requête porte sur les lignes, une ligne étant une relation
// croisée avec un de ses messages sources lisibles. Sans plafond, une seule
// relation très bien sourcée consomme toute la limite: mesuré, une relation à
// dix sources plus deux autres relations avec une limite de 5 rendait un seul
// fait, les deux autres entièrement disparus. graph_top_k voulait donc dire
// "jusqu'à N lignes" et son plancher effectif en nombre de faits était 1.
//
// Trois est assez pour attester un fait auprès du lecteur (la spec rend les
// sources pour qu'il puisse remonter aux messages, pas pour les énumérer
// toutes) et assez petit pour qu'une limite de 5 laisse de la place à
// d'autres relations. La limite globale reste la borne externe: le plafond
// répartit les lignes, il n'en ajoute aucune.
const maxGraphSourcesPerRelation = 3

// graphSQL est la troisième stratégie. Elle part d'entités graines,
// parcourt les relations non invalidées jusqu'à $5 sauts, remonte aux
// messages sources, et applique exactement la même règle d'accès que le
// dense et le lexical.
//
// La dérivation d'accès de la section 8.3 tombe ici naturellement: le join
// des sources passe par messages, donc une relation ne ressort que si au
// moins un de ses messages sources est lisible. Aucune ligne non autorisée
// n'est jamais matérialisée côté Go.
//
// « Au moins une source lisible » ne suffit pourtant pas quand la lecture ne
// vient que d'une ligne memory_unit_acl: l'octroi porte sur une unité et pas
// sur la conversation, donc il faut que toutes les sources de la relation
// tombent dans la fenêtre de cette unité. C'est
// ExplicitACLSourceWindowPredicate, ajouté au même WHERE, et son commentaire
// explique la couture qu'il referme.
//
// La traversée porte sur les entités et non sur les arêtes. Une CTE
// récursive sur les arêtes redécouvre le même chemin par ses deux bouts et
// n'a pas de critère d'arrêt naturel; sur les entités, la profondeur est
// bien définie et DISTINCT ON garde le plus court chemin vers chaque
// relation.
//
// L'étape récursive ne suit que les arêtes entre entités
// (target_entity_id IS NOT NULL): une cible littérale est une feuille, et
// la suivre produirait des NULL qui ne joignent plus rien.
//
// max_hops compte les arêtes, pas les entités. Une relation attachée à une
// graine est donc à un saut, d'où le depth + 1 dans facts, et la récursion
// s'arrête à n.depth < $5 - 1: une entité de profondeur $5 n'apporterait que
// des relations à $5 + 1 sauts. C'est la seule borne de profondeur de la
// requête, et c'est délibéré.
//
// Il y en avait deux, la seconde étant un n.depth < $5 dans facts, présenté
// comme un garde-fou. Elle n'en était pas un: reachable ne produit jamais de
// profondeur au-delà de $5 - 1, donc ce filtre n'écartait rien, et il rendait
// la vraie borne intestable. Desserrer la récursion en n.depth < $5 laissait
// la suite entière verte, y compris un test qui affirme sur une chaîne de
// quatre arêtes que max_hops = k rend exactement hop1 à hopk, parce que le
// filtre mort de facts rattrapait le saut supplémentaire. Le scénario que ça
// ouvrait est un refactor en deux temps, chacun vert: retirer le filtre de
// facts comme code mort, puis desserrer la récursion. Une seule borne, testée
// par TestSearchGraphRendExactementLesSautsDemandes, vaut mieux que deux dont
// une masque l'autre.
//
// La borne est aussi le seul critère d'arrêt de la CTE: il n'y a pas de
// détection de cycle. Le UNION (et non UNION ALL) déduplique les couples
// (entité, profondeur), ce qui borne la CTE à |entités| x max_hops lignes,
// mais sans la borne de profondeur la requête ne terminerait pas sur un
// graphe cyclique.
//
// La troisième source de graines compare la question aux noms et aux alias
// avec word_similarity et non similarity, pour la raison exacte qui a déjà
// fait corriger candidateEntitiesSQL: similarity compare les deux chaînes
// dans leur ensemble, donc un nom court noyé dans une phrase longue ne passe
// jamais le seuil. Mesuré contre la base,
// similarity('Paul', 'Que cultive Paul ?') vaut 0,2941, sous le seuil
// pg_trgm par défaut de 0,3, alors que word_similarity vaut 1 sur la même
// paire. Avec similarity, la troisième source de graines de la section 8.2
// était donc inerte sur toute question réelle.
//
// Le seuil est candidateSimilarityThreshold, partagé avec
// candidateEntitiesSQL exprès: les deux requêtes posent la même question
// dans les deux directions (une entité connue apparaît-elle dans ce texte),
// l'une pour peupler le prompt d'extraction, l'autre pour semer la
// traversée. Deux seuils distincts finiraient par diverger sans que
// personne sache pourquoi le rappel change d'un chemin à l'autre. Il est
// passé en paramètre plutôt que laissé à l'opérateur <%, qui lit un réglage
// de session invisible depuis ici.
//
// valid_until n'est pas rendu tel quel, et c'est le second correctif de la
// revue finale. Pour un type à valeur unique, recomputeChainTx pose le
// valid_until d'une observation sur l'observed_at de la suivante, en
// recalculant sur toutes les observations non invalidées du workspace, sans
// filtre de conversation ni d'accès. C'est correct: c'est de la maintenance.
// Mais l'observation qui supplante est une ligne différente, et quand sa
// seule source est dans une conversation que le demandeur ne peut pas lire,
// le SQL refuse correctement cette ligne tout en laissant passer sa date sur
// le fait lisible. Le demandeur apprenait donc qu'à telle date exacte, dans
// une conversation fermée, quelque chose avait supplanté ce fait.
//
// Ce n'est pas le canal accepté par la section 12 pour observed_at et
// confidence: ces deux-là ne bougent que quand le même triplet est affirmé
// des deux côtés, donc sur la même ligne dédupliquée, et rien de nouveau ne
// franchit.
//
// La dérivation est donc refaite à la lecture, et elle masque les dates que la
// chaîne a posées depuis une observation illisible: valid_until n'est mis à
// NULL que si une observation du couple porte cet instant sans être lisible
// par ce demandeur. readable_chain est la liste des instants lisibles, et elle
// se lit sur source_rows: pas une variante de la règle d'accès, la même,
// composée une seule fois.
//
// Le premier WHEN du CASE est ce qui distingue les deux origines possibles
// d'un valid_until sans qu'aucune colonne ne l'enregistre, et il vaut de
// s'arrêter sur la raison. Une date posée par la chaîne vaut l'observed_at
// d'une autre observation du couple, parce que graph.Chain ferme chaque
// observation sur l'instant de la suivante et jamais sur autre chose. Une date
// posée par l'extracteur, pour un type qui n'est pas à valeur unique et que la
// chaîne ne recalcule donc jamais (section 7.4), n'a aucune raison de
// coïncider avec l'une d'elles. « Aucune observation du couple ne porte cet
// instant » identifie donc la seconde, et elle est rendue: elle est
// affirmée par un message que le demandeur peut lire, et la masquer perdrait
// de l'information sans rien protéger. Le SELECT sur graph_relations ne
// s'embarrasse d'aucune règle d'accès parce qu'il ne rend aucune ligne: il ne
// décide que de montrer ou non une valeur déjà portée par une ligne lisible.
//
// Ce SELECT ne filtre volontairement pas invalidated_at, contrairement au
// reste de la requête, et c'est le point qui a demandé le plus d'attention.
// Une première version le filtrait, et ça ouvrait un trou: la prémisse
// ci-dessus, écrite comme un « toujours », ne tient en réalité que tant que la
// chaîne d'un couple est à jour. Dès qu'une observation est invalidée sans que
// son couple soit recalculé, la ligne fermée garde un valid_until que plus
// aucune observation visible ne justifie, le premier WHEN le laisse passer, et
// la date repart au demandeur.
//
// Or une relation invalidée a toutes ses sources mortes, donc elle n'est
// lisible par personne: rendre la date qu'elle a posée revient à divulguer
// celle d'une observation qui n'existe plus pour ce demandeur. En ne filtrant
// pas, on masque, ce qui est le choix conservateur et le seul cohérent avec le
// motif de la règle.
//
// Le cas est réel et pas théorique: le recalcul de chaîne ne s'applique qu'aux
// types listés dans single_valued_relations, donc retirer un type de cette
// liste gèle les chaînes qu'il avait construites, et une invalidation
// ultérieure ne les rouvre plus. La vraie cause est là, dans la maintenance,
// et pas dans la lecture: ce valid_until périmé ne devrait pas rester sur la
// ligne, un recalcul l'aurait effacé. Le masquage n'en est que le filet, et la
// lacune est écrite en section 12.
//
// Deux prix restants, écrits plutôt que laissés à découvrir.
//
// Le premier est la coïncidence: si une date posée par l'extracteur tombe par
// hasard sur l'observed_at d'une observation illisible du même couple, elle est
// masquée, la lecture ne distinguant pas les deux origines autrement que par
// cette coïncidence. Le doute joue en faveur du secret, ce qui est le bon sens
// pour ce garde.
//
// Le second vient de facts, et contredit ce que ce commentaire affirmait: le
// couple d'un fait rendu n'est pas nécessairement tout entier dans facts.
// facts admet une relation dès que son entité source OU son entité cible est
// atteinte par la traversée, si bien qu'un fait peut y entrer par sa cible
// alors que son entité source n'est pas atteinte. Ses frères de couple à cible
// littérale, eux, n'y entrent alors pas, donc ils ne sont pas dans
// readable_chain, donc un valid_until pourtant entièrement lisible est masqué.
// Conservateur là aussi, et le corriger demanderait une seconde dérivation
// d'accès pour les frères de couple hors traversée.
//
// $1 workspace_id, $2 requester_key, $3 clés graines, $4 texte de la
// requête, $5 profondeur maximale, $6 limite, $7 seuil de similarité,
// $8 plafond de sources par relation.
const graphSQL = `
WITH RECURSIVE ` + ReadableCTE + `,
seeds AS (
	SELECT e.entity_id
	FROM graph_entities e
	WHERE e.workspace_id = $1
	  AND (
		e.canonical_key = ANY($3)
		OR split_part(e.canonical_key, ':', 2) = ANY($3)
		OR lower(e.display_name) = ANY($3)
		OR ($4 <> '' AND word_similarity(e.display_name, $4) >= $7)
		OR ($4 <> '' AND EXISTS (
			SELECT 1 FROM jsonb_array_elements_text(e.aliases) AS t(a)
			WHERE word_similarity(t.a, $4) >= $7
		))
	  )
	-- La coupure est silencieuse par construction: on ne peut pas semer tout
	-- un workspace. Elle doit donc au moins etre previsible, et preferer ce
	-- que l'appelant a nomme lui-meme a ce qu'un trigramme a devine. Sans cet
	-- ordre, un sujet fourni explicitement pouvait perdre sa place au profit
	-- de cinquante homonymes approximatifs, et le jeu retenu dependait du
	-- plan d'execution.
	ORDER BY (
		e.canonical_key = ANY($3)
		OR split_part(e.canonical_key, ':', 2) = ANY($3)
		OR lower(e.display_name) = ANY($3)
	) DESC,
		word_similarity(e.display_name, $4) DESC,
		e.entity_id
	LIMIT $9
),
reachable AS (
	SELECT entity_id, 0 AS depth FROM seeds
	UNION
	SELECT CASE WHEN rel.source_entity_id = n.entity_id
	            THEN rel.target_entity_id ELSE rel.source_entity_id END,
	       n.depth + 1
	FROM reachable n
	JOIN graph_relations rel
	  ON (rel.source_entity_id = n.entity_id OR rel.target_entity_id = n.entity_id)
	WHERE rel.workspace_id = $1
	  AND rel.invalidated_at IS NULL
	  AND rel.target_entity_id IS NOT NULL
	  AND n.depth < $5 - 1
),
facts AS (
	SELECT DISTINCT ON (rel.relation_id)
		rel.relation_id, rel.source_entity_id, rel.target_entity_id,
		rel.relation_type, rel.target_literal, rel.confidence,
		rel.observed_at, rel.valid_from, rel.valid_until, rel.embedding,
		n.depth + 1 AS depth
	FROM reachable n
	JOIN graph_relations rel
	  ON (rel.source_entity_id = n.entity_id OR rel.target_entity_id = n.entity_id)
	WHERE rel.workspace_id = $1
	  AND rel.invalidated_at IS NULL
	ORDER BY rel.relation_id, n.depth
),
source_rows AS (
	SELECT
		f.relation_id, f.depth, f.source_entity_id, f.relation_type,
		f.confidence, f.observed_at, f.valid_from, f.valid_until,
		-- Distance cosinus du fait à la question, quand les deux vecteurs
		-- existent. 2 est au-delà du maximum d'une distance cosinus, donc
		-- une relation sans vecteur passe derrière toutes celles qui en ont
		-- un, sans jamais disparaître.
		CASE
			WHEN $10::vector IS NOT NULL AND f.embedding IS NOT NULL
				THEN f.embedding <=> $10::vector
			ELSE 2
		END AS fact_distance,
		se.display_name AS subject,
		COALESCE(te.display_name, f.target_literal) AS object,
		mu.memory_unit_id,
		m.message_id, m.conversation_id,
		COALESCE(mu.start_sequence, m.sequence_number) AS start_sequence,
		COALESCE(mu.end_sequence, m.sequence_number)   AS end_sequence,
		m.sequence_number,
		ROW_NUMBER() OVER (
			PARTITION BY f.relation_id
			ORDER BY m.sequence_number DESC, m.message_id DESC
		) AS source_rank,
		-- Quota par profondeur, voir perDepthQuota: sans lui le LIMIT
		-- final, qui trie par profondeur croissante, coupe systématiquement
		-- les faits du dernier saut, ceux qui rendent une question à deux
		-- sauts répondable.
		ROW_NUMBER() OVER (
			PARTITION BY f.depth
			ORDER BY f.confidence DESC, f.observed_at DESC,
			         f.relation_id, m.sequence_number DESC
		) AS depth_rank,` +
	accessReasonExpr + ` AS access_reason
	FROM facts f
	JOIN graph_entities se ON se.entity_id = f.source_entity_id
	LEFT JOIN graph_entities te ON te.entity_id = f.target_entity_id
	JOIN graph_relation_sources gs ON gs.relation_id = f.relation_id
	JOIN messages m ON m.message_id = gs.message_id
` + ConversationGuardJoinOnMessages + `
	LEFT JOIN readable r
		ON r.conversation_id = m.conversation_id
	LEFT JOIN LATERAL (
		SELECT mu.memory_unit_id, mu.start_sequence, mu.end_sequence
		FROM memory_units mu
		WHERE mu.anchor_message_id = m.message_id
		  AND mu.active
		ORDER BY mu.updated_at DESC, mu.memory_unit_id DESC
		LIMIT 1
	) mu ON TRUE
	WHERE m.workspace_id = $1
	  AND m.deleted_at IS NULL
	  AND ($11::text[] IS NULL OR m.conversation_id = ANY($11::text[]))
	  AND ($12::jsonb IS NULL OR cinnabar_metadata_match(m.metadata, $12::jsonb))
	  AND (
		r.conversation_id IS NOT NULL
		OR EXISTS (
			SELECT 1 FROM memory_unit_acl a
			WHERE a.memory_unit_id = mu.memory_unit_id
			  AND a.principal_key = $2
			  AND a.permission = 'read'
		)
	  )` + ExplicitACLSourceWindowPredicate + `
),
readable_chain AS (
	SELECT DISTINCT source_entity_id, relation_type, observed_at
	FROM source_rows
)
SELECT
	sr.relation_id, sr.depth, sr.relation_type, sr.confidence,
	sr.observed_at, sr.valid_from,
	CASE
		WHEN NOT EXISTS (
			SELECT 1 FROM graph_relations sib
			WHERE sib.workspace_id = $1
			  AND sib.source_entity_id = sr.source_entity_id
			  AND sib.relation_type = sr.relation_type
			  AND sib.observed_at = sr.valid_until
		) THEN sr.valid_until
		WHEN EXISTS (
			SELECT 1 FROM readable_chain rc
			WHERE rc.source_entity_id = sr.source_entity_id
			  AND rc.relation_type = sr.relation_type
			  AND rc.observed_at = sr.valid_until
		) THEN sr.valid_until
	END AS valid_until,
	sr.subject, sr.object, sr.memory_unit_id,
	sr.message_id, sr.conversation_id,
	sr.start_sequence, sr.end_sequence, sr.access_reason
FROM source_rows sr
WHERE sr.source_rank <= $8
-- L'ordre entrelace les profondeurs au lieu de les empiler: on prend le
-- meilleur fait de chaque profondeur, puis le deuxième de chaque, et ainsi
-- de suite. Trier d'abord par profondeur, comme on le faisait, condamnait
-- les faits du dernier saut dès qu'une entité graine était un peu
-- connectée: mesuré, les treize faits rendus pour une question à deux sauts
-- étaient tous à un saut, et les relations du second saut, présentes en base
-- et lisibles, n'arrivaient jamais. Or ce sont exactement celles qui rendent
-- la question répondable par le modèle lecteur.
--
-- La profondeur reste un critère, en second: à rang égal dans sa propre
-- profondeur, le plus proche de la graine passe devant.
-- La pertinence à la question passe devant tout le reste quand elle est
-- disponible. C'est le signal qui manquait: mesuré, trois questions sans
-- rapport rendaient exactement les mêmes quatorze faits, faute de pouvoir
-- préférer celui qui répond.
--
-- L'entrelacement des profondeurs reste le critère suivant, pour les
-- relations sans vecteur qui partagent toutes la même distance factice.
ORDER BY sr.fact_distance ASC, sr.depth_rank ASC, sr.depth ASC,
         sr.confidence DESC, sr.observed_at DESC,
         sr.sequence_number DESC, sr.message_id DESC
LIMIT $6`

// SearchGraph rend les candidats et les faits d'une même traversée. La
// limite porte sur les lignes (une relation croisée avec un de ses messages
// sources lisibles), pas sur les faits ni sur les candidats: c'est ce qui
// borne le travail de Postgres, et le nombre de faits comme de candidats en
// découle et lui reste inférieur ou égal.
//
// Ce qui compte, et que le commentaire d'origine omettait: sans le plafond de
// maxGraphSourcesPerRelation, le plancher du nombre de faits était 1, parce
// qu'une seule relation très bien sourcée pouvait consommer toute la limite.
// Le plafond répartit les lignes entre relations, il n'en ajoute aucune, donc
// la limite reste la borne externe. Le prix est explicite: un fait rend au
// plus trois de ses messages sources, même s'il en a dix.
func (r *SearchRepo) SearchGraph(ctx context.Context,
	q memory.GraphQuery) (memory.GraphResult, error) {

	// Avant tout autre chose, y compris le retour anticipé sur une absence
	// de graine: un refus d'autorisation ne doit jamais être masqué par un
	// retour anticipé sur une entrée vide.
	if err := ValidateRequester(q.WorkspaceID, q.RequesterKey); err != nil {
		return memory.GraphResult{}, fmt.Errorf("graph search: %w", err)
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.MaxHops <= 0 {
		q.MaxHops = 1
	}

	seeds := seedKeys(q.Subjects)
	text := strings.TrimSpace(q.Text)
	// Sans graine explicite ni texte, la traversée n'a pas de point de
	// départ: pas d'erreur, juste zéro résultat, comme le lexical sur une
	// requête vide.
	if len(seeds) == 0 && text == "" {
		return memory.GraphResult{}, nil
	}

	convs, filter, err := restriction(q.CandidateQuery)
	if err != nil {
		return memory.GraphResult{}, fmt.Errorf("graph search: %w", err)
	}

	rows, err := r.pool.Query(ctx, graphSQL,
		q.WorkspaceID, q.RequesterKey, seeds, text, q.MaxHops, q.Limit,
		candidateSimilarityThreshold, maxGraphSourcesPerRelation, maxSeedEntities,
		queryVector(q.Embedding), convs, filter)
	if err != nil {
		return memory.GraphResult{}, fmt.Errorf("graph search: %w", err)
	}
	defer rows.Close()

	var (
		out       memory.GraphResult
		factIndex = map[uuid.UUID]int{}
		candIndex = map[uuid.UUID]int{}
		rank      int
	)
	for rows.Next() {
		var (
			relationID uuid.UUID
			depth      int
			fact       memory.GraphFact
			unitID     *uuid.UUID
			cand       memory.Candidate
		)
		if err := rows.Scan(&relationID, &depth, &fact.Predicate, &fact.Confidence,
			&fact.ObservedAt, &fact.ValidFrom, &fact.ValidUntil,
			&fact.Subject, &fact.Object, &unitID,
			&cand.AnchorMessageID, &cand.ConversationID,
			&cand.StartSequence, &cand.EndSequence, &cand.AccessReason); err != nil {
			return memory.GraphResult{}, fmt.Errorf("graph search: scan: %w", err)
		}

		// Un fait par relation, avec la liste de ses sources lisibles. Une
		// source non lisible n'est jamais arrivée jusqu'ici: le SQL l'a
		// écartée.
		if i, ok := factIndex[relationID]; ok {
			out.Facts[i].SourceMessageIDs = append(
				out.Facts[i].SourceMessageIDs, cand.AnchorMessageID)
		} else {
			fact.RelationID = relationID
			fact.SourceMessageIDs = []uuid.UUID{cand.AnchorMessageID}
			factIndex[relationID] = len(out.Facts)
			out.Facts = append(out.Facts, fact)
		}

		// Un candidat par message d'ancrage. Le premier rencontré porte le
		// meilleur rang, puisque le SQL rend déjà les lignes dans l'ordre
		// de pertinence; les suivants n'ajoutent que leurs entités.
		if i, ok := candIndex[cand.AnchorMessageID]; ok {
			out.Candidates[i].MatchedEntities = appendEntity(
				out.Candidates[i].MatchedEntities, fact.Subject, fact.Object)
			continue
		}
		rank++
		cand.Strategy = "graph"
		cand.Rank = rank
		cand.MemoryUnitID = unitID
		// RawScore porte l'inverse du nombre de sauts, pas une distance
		// comparable à celle du dense ni à un ts_rank_cd. La fusion
		// compare des rangs, jamais ces scores entre stratégies.
		//
		// Aucun garde ne ramène depth à 1 ici, volontairement: facts rend
		// n.depth + 1 et sème à zéro, donc une relation attachée à une
		// graine est déjà à un saut. Un tel garde avait existé, et il
		// absorbait la mutation qui remplace n.depth + 1 par n.depth,
		// rendant invisible une erreur de comptage des sauts. Mieux vaut
		// qu'une profondeur nulle produise un score aberrant qu'un test ne
		// peut pas manquer.
		cand.RawScore = 1 / float64(depth)
		cand.MatchedEntities = appendEntity(nil, fact.Subject, fact.Object)
		candIndex[cand.AnchorMessageID] = len(out.Candidates)
		out.Candidates = append(out.Candidates, cand)
	}
	if err := rows.Err(); err != nil {
		return memory.GraphResult{}, fmt.Errorf("graph search: %w", err)
	}
	return out, nil
}

// seedKeys décompose les sujets connus en formes comparables aux clés
// canoniques. Un sujet est une clé d'identité comme "user:paul" alors
// qu'une entité porte une clé canonique comme "person:paul": comparer les
// deux telles quelles ne trouverait jamais rien. On propose donc au SQL la
// clé entière et sa partie locale mise en slug, et le SQL compare les deux
// à canonical_key, à sa partie locale et au nom d'affichage.
func seedKeys(subjects []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range subjects {
		add(s)
		if i := strings.IndexByte(s, ':'); i >= 0 {
			add(graph.Slug(s[i+1:]))
		}
	}
	return out
}

// appendEntity ajoute des noms d'entités sans doublon et sans valeur vide.
func appendEntity(list []string, names ...string) []string {
	for _, n := range names {
		if n == "" {
			continue
		}
		found := false
		for _, e := range list {
			if e == n {
				found = true
				break
			}
		}
		if !found {
			list = append(list, n)
		}
	}
	return list
}

// maxSeedEntities borne le nombre d'entites graines. Le depassement est
// silencieux, et il ne peut pas en etre autrement: rendre une erreur parce
// qu'un workspace contient beaucoup d'homonymes ferait echouer une recherche
// legitime. Ce qui est garanti, c'est l'ordre (voir l'ORDER BY de seeds), donc
// qu'un sujet nomme explicitement par l'appelant ne soit jamais evince par un
// homonyme approximatif.
const maxSeedEntities = 50

// queryVector convertit le vecteur de la question en valeur pgvector, ou en
// NULL quand il est absent. Absent est un état normal et non une erreur: une
// panne de l'embedder doit dégrader le classement du graphe, pas empêcher la
// traversée, ce qui est la lettre du critère 10.
func queryVector(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	return pgvector.NewVector(v)
}

var _ memory.GraphSearcher = (*SearchRepo)(nil)
