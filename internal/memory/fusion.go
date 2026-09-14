package memory

import (
	"sort"

	"github.com/google/uuid"
)

// FuseRRF applique la fusion par rang réciproque (Reciprocal Rank Fusion). La
// clé de fusion est le message d'ancrage: les stratégies ne rendent pas le
// même type d'objet (le dense projette son unité vectorielle sur son ancre,
// le lexical et le graphe rendent directement des messages), et c'est cette
// projection qui réalise au passage le regroupement des candidats pointant
// vers les mêmes messages.
//
// La contribution d'un candidat de rang r vaut 1 / (k + r). k <= 0 retombe
// sur 60, la valeur par défaut documentée.
func FuseRRF(k int, lists ...[]Candidate) []Fused {
	if k <= 0 {
		k = 60
	}

	byAnchor := map[uuid.UUID]*Fused{}
	// order retient l'ordre de première apparition des ancres, pour rendre
	// les égalités de score déterministes: sans lui, deux appels sur la même
	// requête pourraient rendre des ordres différents et rien en aval ne
	// serait reproductible.
	var order []uuid.UUID

	for _, list := range lists {
		for _, c := range list {
			f, ok := byAnchor[c.AnchorMessageID]
			if !ok {
				f = &Fused{
					AnchorMessageID: c.AnchorMessageID,
					ConversationID:  c.ConversationID,
					StartSequence:   c.StartSequence,
					EndSequence:     c.EndSequence,
					Ranks:           map[string]int{},
					RawScores:       map[string]float64{},
					AccessReason:    c.AccessReason,
					MemoryUnitID:    c.MemoryUnitID,
				}
				byAnchor[c.AnchorMessageID] = f
				order = append(order, c.AnchorMessageID)
			}

			f.Score += 1 / float64(k+c.Rank)
			f.Ranks[c.Strategy] = c.Rank
			// RawScore n'a de sens que dans l'échelle de sa propre stratégie:
			// on le garde par stratégie, on ne le mélange jamais aux autres.
			f.RawScores[c.Strategy] = c.RawScore

			// Union des intervalles. Tous les candidats regroupés ici
			// partagent la même ancre, et chaque stratégie fait finir sa
			// fenêtre sur le message d'ancrage lui-même: les intervalles se
			// recouvrent donc tous au moins en ce point, et leur union ne
			// peut jamais fabriquer un intervalle qui saute par-dessus des
			// messages absents de toutes les fenêtres sources.
			if c.StartSequence < f.StartSequence {
				f.StartSequence = c.StartSequence
			}
			if c.EndSequence > f.EndSequence {
				f.EndSequence = c.EndSequence
			}
			if f.MemoryUnitID == nil && c.MemoryUnitID != nil {
				f.MemoryUnitID = c.MemoryUnitID
			}
			f.MatchedEntities = mergeEntities(f.MatchedEntities, c.MatchedEntities)
		}
	}

	firstSeen := make(map[uuid.UUID]int, len(order))
	for i, id := range order {
		firstSeen[id] = i
	}

	out := make([]Fused, 0, len(order))
	for _, id := range order {
		out = append(out, *byAnchor[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return firstSeen[out[i].AnchorMessageID] < firstSeen[out[j].AnchorMessageID]
	})
	return out
}

// MergeAdjacent recolle les extraits d'une même conversation dont les
// intervalles se touchent ou se chevauchent. La spécification d'origine
// proposait de jeter les chevauchements: on s'en écarte délibérément, jeter
// un extrait parce qu'il touche son voisin perd du texte exploitable, alors
// que le recollement produit un passage plus long et plus lisible.
//
// Ne jamais recoller au travers d'une conversation différente: deux extraits
// de conversations distinctes, même à séquences adjacentes, sont un texte
// sans rapport, et les recoller fabriquerait un passage qui n'a jamais
// existé.
//
// Ne jamais recoller non plus un extrait admis par une ligne memory_unit_acl
// (AccessReasonExplicitACL) avec un voisin admis autrement: le recollement
// prend l'union des intervalles, ce qui ré-élargirait exactement la fenêtre
// que Searcher.expand borne pour cette raison d'accès, et rendrait au
// bénéficiaire du partage des messages que son octroi ne couvre pas. Deux
// extraits explicit_acl adjacents se recollent en revanche normalement:
// l'union de deux fenêtres qui se touchent ne contient que des messages
// couverts par l'un des deux octrois.
func MergeAdjacent(items []Fused) []Fused {
	if len(items) < 2 {
		out := make([]Fused, len(items))
		copy(out, items)
		return out
	}

	// On travaille sur une copie triée par conversation puis par séquence,
	// pour détecter les voisinages; le tri de sortie (par score) est refait
	// à la fin.
	work := make([]Fused, len(items))
	copy(work, items)
	sort.SliceStable(work, func(i, j int) bool {
		if work[i].ConversationID != work[j].ConversationID {
			return work[i].ConversationID < work[j].ConversationID
		}
		return work[i].StartSequence < work[j].StartSequence
	})

	var merged []Fused
	cur := work[0]
	for _, next := range work[1:] {
		sameConv := next.ConversationID == cur.ConversationID
		touching := next.StartSequence <= cur.EndSequence+1
		// sameGrant empêche de recoller un extrait obtenu par ACL explicite,
		// dont la fenêtre a été bornée à l'unité partagée, avec un voisin
		// obtenu autrement: l'union des intervalles rélargirait exactement
		// ce que le bornage venait de fermer.
		//
		// Aujourd'hui la garde ne se déclenche jamais en production, et
		// c'est voulu plutôt que raté: accessReasonExpr dérive la raison
		// d'accès de la conversation, donc deux candidats de la même
		// conversation portent forcément la même. Elle tient pour une
		// quatrième stratégie, ou pour une raison d'accès qui deviendrait
		// un jour dérivable de l'unité et non de la conversation. Son test
		// construit donc un état que le SQL actuel ne peut pas produire.
		sameGrant := (cur.AccessReason == AccessReasonExplicitACL) ==
			(next.AccessReason == AccessReasonExplicitACL)
		if sameConv && touching && sameGrant {
			if next.EndSequence > cur.EndSequence {
				cur.EndSequence = next.EndSequence
			}
			// Le recollé garde le meilleur score des deux, pas le premier
			// rencontré: c'est ce score qui pilote son rang final.
			if next.Score > cur.Score {
				cur.Score = next.Score
				cur.AnchorMessageID = next.AnchorMessageID
				cur.AccessReason = next.AccessReason
				cur.MemoryUnitID = next.MemoryUnitID
			}
			cur.Ranks = mergeBestRank(cur.Ranks, next.Ranks)
			cur.RawScores = mergeRawScores(cur.RawScores, next.RawScores)
			cur.MatchedEntities = mergeEntities(cur.MatchedEntities, next.MatchedEntities)
			continue
		}
		merged = append(merged, cur)
		cur = next
	}
	merged = append(merged, cur)

	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Score != merged[j].Score {
			return merged[i].Score > merged[j].Score
		}
		if merged[i].ConversationID != merged[j].ConversationID {
			return merged[i].ConversationID < merged[j].ConversationID
		}
		return merged[i].StartSequence < merged[j].StartSequence
	})
	return merged
}

// ExcerptChars mesure le coût en caractères d'un extrait tel qu'il sera
// effectivement rendu: le préfixe d'auteur et le saut de ligne que l'étape
// de rendu ajoutera comptent, pas seulement le contenu brut.
func ExcerptChars(e Excerpt) int {
	n := 0
	for _, m := range e.Messages {
		n += len(m.AuthorKey) + len(" : ") + len(m.Content) + len("\n")
	}
	return n
}

// FitBudget fait tenir les extraits dans le budget de tokens. La dégradation
// retire d'abord le contexte étendu des extraits les moins bien classés (en
// partant de la fin de la liste triée et de l'extrémité la plus éloignée du
// coeur), et n'écarte un extrait entier qu'une fois qu'il n'a plus de
// contexte à céder. Le coeur (l'intervalle fusionné) n'est jamais entamé: un
// message n'est jamais tronqué en son milieu, une phrase coupée produirait
// un souvenir trompeur. Si le coeur d'un extrait dépasse seul le budget, cet
// extrait est écarté en entier plutôt que rendu tronqué.
//
// items est trié par score décroissant en interne avant toute dégradation:
// tout le contrat de cette fonction (quel extrait perd quoi) dépend de cet
// ordre, donc on ne le suppose pas déjà respecté par l'appelant, on
// l'impose.
func FitBudget(items []Excerpt, maxTokens, charsPerToken int) []Excerpt {
	budget := maxTokens * charsPerToken

	out := make([]Excerpt, len(items))
	copy(out, items)
	// Tri défensif: tout le contrat de cette fonction (qui perd quoi) repose
	// sur un ordre par score décroissant. On ne fait pas confiance à
	// l'appelant pour l'avoir déjà trié, une entrée non triée dégraderait le
	// mauvais extrait sans qu'aucune erreur ne le signale nulle part. Le tri
	// est stable pour que deux scores égaux gardent l'ordre d'arrivée.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})

	total := func() int {
		n := 0
		for _, e := range out {
			n += ExcerptChars(e)
		}
		return n
	}

	// Phase 1: retirer le contexte étendu, du moins bien classé au mieux, en
	// s'arrêtant net dès que le budget est respecté. Chaque itération retire
	// au plus un message, et le nombre total de messages de contexte est
	// fini: la boucle termine forcément, que le budget soit atteint ou que
	// plus aucun extrait ne puisse céder de contexte.
	for total() > budget {
		trimmed := false
		for i := len(out) - 1; i >= 0; i-- {
			if dropped, ok := dropFarthestContext(out[i]); ok {
				out[i] = dropped
				trimmed = true
				break
			}
		}
		if !trimmed {
			break
		}
	}

	// Phase 2: dernier recours, écarter des extraits entiers en partant du
	// moins bien classé (la fin de la liste). Termine en au plus len(out)
	// itérations.
	for total() > budget && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out
}

// dropFarthestContext retire, hors du coeur, le message le plus éloigné du
// coeur (avant ou après, selon lequel s'étend le plus loin). Rend faux
// quand il ne reste plus que le coeur: c'est le signal que cet extrait n'a
// plus rien à céder.
func dropFarthestContext(e Excerpt) (Excerpt, bool) {
	if len(e.Messages) == 0 {
		return e, false
	}
	first, last := e.Messages[0], e.Messages[len(e.Messages)-1]

	beforeGap := e.CoreFrom - first.SequenceNumber
	afterGap := last.SequenceNumber - e.CoreTo

	switch {
	case beforeGap <= 0 && afterGap <= 0:
		// Plus rien avant ni après le coeur.
		return e, false
	case beforeGap >= afterGap:
		e.Messages = e.Messages[1:]
	default:
		e.Messages = e.Messages[:len(e.Messages)-1]
	}
	return e, true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// mergeBestRank combine deux tables de rangs en gardant, par stratégie, le
// meilleur (le plus petit) des deux rangs. Rend toujours une map neuve: ni a
// ni b ne sont réutilisées comme cible d'écriture, pour ne jamais modifier
// par effet de bord une map dont l'appelant garde une référence (a et b
// viennent typiquement de deux Fused distincts eux-mêmes copiés
// superficiellement par MergeAdjacent, donc réutiliser a écrirait dans la
// map de l'appelant).
func mergeBestRank(a, b map[string]int) map[string]int {
	merged := make(map[string]int, len(a)+len(b))
	for strat, rank := range a {
		merged[strat] = rank
	}
	for strat, rank := range b {
		if cur, ok := merged[strat]; !ok || rank < cur {
			merged[strat] = rank
		}
	}
	return merged
}

// mergeRawScores combine deux tables de scores bruts en gardant, par
// stratégie, la plus grande des deux valeurs. Les valeurs restent séparées
// par stratégie: aucune arithmétique n'est faite entre stratégies
// différentes. Comme mergeBestRank, rend toujours une map neuve.
func mergeRawScores(a, b map[string]float64) map[string]float64 {
	merged := make(map[string]float64, len(a)+len(b))
	for strat, score := range a {
		merged[strat] = score
	}
	for strat, score := range b {
		if cur, ok := merged[strat]; !ok || score > cur {
			merged[strat] = score
		}
	}
	return merged
}

// mergeEntities combine deux listes d'entités en une slice neuve, sans
// jamais réutiliser la capacité de réserve de a comme cible d'écriture: un
// append qui réutiliserait cette capacité écrirait dans la mémoire de
// l'appelant sans même changer la longueur ni l'adresse de la slice
// observée par ce dernier, une corruption silencieuse et invisible aux
// comparaisons de contenu habituelles.
func mergeEntities(a, b []string) []string {
	merged := make([]string, len(a), len(a)+len(b))
	copy(merged, a)
	for _, e := range b {
		if !containsString(merged, e) {
			merged = append(merged, e)
		}
	}
	return merged
}
