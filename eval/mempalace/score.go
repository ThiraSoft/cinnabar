package main

import "strings"

// hit est un résultat de recherche ramené aux données du banc: le message
// ancre et les messages que l'extrait rend autour de lui.
type hit struct {
	Anchor  *msgSpec
	Sources []*msgSpec
}

// scoreQuestion calcule, pour une liste de résultats déjà tronquée, les
// métriques du script MemPalace du banc. Le suffixe _ctx compte aussi les
// messages voisins que l'extrait rend: MemPalace ne rend que son document,
// Cinnabar rend l'ancre élargie, les deux lectures sont gardées.
func scoreQuestion(bench string, q questionSpec, hits []hit) map[string]float64 {
	// Tout est noté sur les messages que l'extrait rend: le noyau recollé
	// et ses voisins. L'ancre seule ne veut rien dire ici, puisque des
	// candidats contigus sont recollés en un extrait qui ne garde qu'une
	// ancre.
	var ms []*msgSpec
	for _, h := range hits {
		ms = append(ms, h.Sources...)
	}
	out := map[string]float64{}
	switch bench {
	case "locomo":
		// compute_retrieval_recall: fraction des preuves retrouvées, 1.0
		// quand la question n'en a pas.
		sess, dia := map[string]bool{}, map[string]bool{}
		for _, m := range ms {
			sess[m.Session] = true
			dia[m.Unit] = true
		}
		out["session"] = frac(q.EvidenceSessions, sess)
		out["dialog"] = frac(q.EvidenceDia, dia)
	case "longmemeval":
		// recall_any au niveau session: une session réponse suffit.
		sess := map[string]bool{}
		for _, m := range ms {
			sess[m.Session] = true
		}
		any := 0.0
		for _, e := range q.EvidenceSessions {
			if sess[e] {
				any = 1
			}
		}
		out["recall_any"] = any
	case "convomem":
		// Correspondance par inclusion de texte, dans un sens ou dans
		// l'autre, sur des textes en minuscules sans espaces de bord.
		var texts []string
		for _, m := range ms {
			texts = append(texts, norm(m.Content))
		}
		out["recall"] = textRecall(q.EvidenceTexts, texts)
	case "membench":
		// Touché si une cible est le sid ou l'index global d'un tour rendu.
		// Un tour y est la paire utilisateur et assistant: les deux messages
		// de Cinnabar pointent le même tour.
		out["hit"] = 0
		for _, v := range q.Targets {
			for _, m := range ms {
				if m.TurnSID == v || m.TurnGlobal == v {
					out["hit"] = 1
				}
			}
		}
	}
	return out
}

// scoreDistinctSessions note les k premières sessions distinctes d'une
// longue liste de résultats: la granularité de MemPalace en mode session,
// où un document est une session entière.
func scoreDistinctSessions(bench string, q questionSpec, hits []hit, k int) float64 {
	var order []string
	seen := map[string]bool{}
	for _, h := range hits {
		if s := h.Anchor.Session; !seen[s] {
			seen[s] = true
			order = append(order, s)
		}
		if len(order) == k {
			break
		}
	}
	top := map[string]bool{}
	for _, s := range order {
		top[s] = true
	}
	if bench == "locomo" {
		return frac(q.EvidenceSessions, top)
	}
	for _, e := range q.EvidenceSessions {
		if top[e] {
			return 1
		}
	}
	return 0
}

func frac(gold []string, got map[string]bool) float64 {
	if len(gold) == 0 {
		return 1
	}
	n := 0
	for _, g := range gold {
		if got[g] {
			n++
		}
	}
	return float64(n) / float64(len(gold))
}

func textRecall(gold, got []string) float64 {
	if len(gold) == 0 {
		return 1
	}
	n := 0
	for _, g := range gold {
		for _, r := range got {
			if strings.Contains(g, r) || strings.Contains(r, g) {
				n++
				break
			}
		}
	}
	return float64(n) / float64(len(gold))
}

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
