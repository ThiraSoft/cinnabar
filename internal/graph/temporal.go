package graph

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// Observation est une relation vue sous son seul angle temporel: quand elle
// a été observée, et jusqu'à quand le fait qu'elle porte est tenu pour
// valide. Chain ne manipule que ça, ce qui la garde testable sans base.
type Observation struct {
	RelationID uuid.UUID
	ObservedAt time.Time
	ValidUntil *time.Time
}

// Chain recalcule les fenêtres de validité d'une suite d'observations d'un
// même couple (entité source, type de relation) à valeur unique. Chaque
// observation se ferme sur l'observed_at de la suivante, la dernière reste
// ouverte.
//
// Le recalcul est complet et non incrémental, et c'est délibéré. Fermer
// seulement la précédente à l'arrivée d'une nouvelle marche à l'écriture,
// mais laisse un trou quand c'est l'observation la plus récente qui
// disparaît: la précédente resterait fermée sur une date qui ne correspond
// plus à rien. Recalculer toute la chaîne rend aussi l'opération
// idempotente, donc rejouable par un worker qui reprend un job.
//
// L'entrée n'est pas modifiée; la sortie est une copie triée par
// observed_at croissant.
func Chain(obs []Observation) []Observation {
	if len(obs) == 0 {
		return nil
	}

	out := make([]Observation, len(obs))
	copy(out, obs)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ObservedAt.Before(out[j].ObservedAt)
	})

	for i := range out {
		out[i].ValidUntil = nil
		// Deux observations exactement simultanées ne se ferment pas l'une
		// sur l'autre: la fenêtre serait vide et le fait deviendrait faux
		// à l'instant même où il a été observé. On cherche donc la
		// première observation strictement postérieure.
		for j := i + 1; j < len(out); j++ {
			if out[j].ObservedAt.After(out[i].ObservedAt) {
				until := out[j].ObservedAt
				out[i].ValidUntil = &until
				break
			}
		}
	}
	return out
}

// IsSingleValued teste un type de relation contre la liste configurée, en
// normalisant les deux côtés: la liste est écrite à la main dans un YAML,
// le type sort d'un modèle de langage, et ni l'un ni l'autre ne garantit
// une casse ou une ponctuation stable.
func IsSingleValued(singleValued []string, relationType string) bool {
	want := NormalizeRelationType(relationType)
	if want == "" {
		return false
	}
	for _, s := range singleValued {
		if NormalizeRelationType(s) == want {
			return true
		}
	}
	return false
}
