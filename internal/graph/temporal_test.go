package graph

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func at(day int) time.Time {
	return time.Date(2026, 6, day, 12, 0, 0, 0, time.UTC)
}

func obs(day int) Observation {
	return Observation{RelationID: uuid.New(), ObservedAt: at(day)}
}

func TestChainFermeChaqueObservationSurLaSuivante(t *testing.T) {
	in := []Observation{obs(1), obs(5), obs(9)}
	got := Chain(in)

	if got[0].ValidUntil == nil || !got[0].ValidUntil.Equal(at(5)) {
		t.Errorf("valid_until[0] = %v, want %v", got[0].ValidUntil, at(5))
	}
	if got[1].ValidUntil == nil || !got[1].ValidUntil.Equal(at(9)) {
		t.Errorf("valid_until[1] = %v, want %v", got[1].ValidUntil, at(9))
	}
	if got[2].ValidUntil != nil {
		t.Errorf("valid_until[2] = %v, want nil (toujours vrai)", got[2].ValidUntil)
	}
}

func TestChainTrieParObservedAt(t *testing.T) {
	in := []Observation{obs(9), obs(1), obs(5)}
	got := Chain(in)
	for i := 1; i < len(got); i++ {
		if got[i].ObservedAt.Before(got[i-1].ObservedAt) {
			t.Fatalf("sortie non triée: %v avant %v",
				got[i-1].ObservedAt, got[i].ObservedAt)
		}
	}
	if got[2].ValidUntil != nil {
		t.Error("la dernière observation devrait rester ouverte")
	}
}

// C'est le cas que la section 7.5 vise: la plus récente disparaît, et la
// précédente doit se rouvrir au lieu de rester fermée sur une observation
// qui n'existe plus.
func TestChainRouvreLaDerniereSurvivante(t *testing.T) {
	a, b := obs(1), obs(5)
	fermee := at(5)
	a.ValidUntil = &fermee

	got := Chain([]Observation{a})
	if len(got) != 1 {
		t.Fatalf("%d observations, want 1", len(got))
	}
	if got[0].ValidUntil != nil {
		t.Errorf("valid_until = %v, want nil après disparition de la suivante",
			got[0].ValidUntil)
	}
	_ = b
}

func TestChainSurUneListeVideNePaniquePas(t *testing.T) {
	if got := Chain(nil); len(got) != 0 {
		t.Errorf("%d observations, want 0", len(got))
	}
}

// Deux observations au même instant: la chaîne ne doit ni en fermer une sur
// elle-même, ni produire un valid_until antérieur à son observed_at.
func TestChainAvecDeuxObservationsSimultanees(t *testing.T) {
	a, b := obs(3), obs(3)
	got := Chain([]Observation{a, b})
	// Le compte d'abord: les deux contrôles ci-dessous vivent dans la boucle,
	// donc une Chain qui rendrait nil les passait tous les deux.
	if len(got) != 2 {
		t.Fatalf("%d observations rendues, want 2", len(got))
	}
	for i, o := range got {
		if o.ValidUntil != nil && o.ValidUntil.Before(o.ObservedAt) {
			t.Errorf("observation %d: valid_until %v avant observed_at %v",
				i, o.ValidUntil, o.ObservedAt)
		}
		if o.ValidUntil != nil && o.ValidUntil.Equal(o.ObservedAt) {
			t.Errorf("observation %d fermée sur son propre instant", i)
		}
	}
}

func TestIsSingleValuedNormaliseLesDeuxCotes(t *testing.T) {
	list := []string{"HAS_OBSERVED_STATE", "a pour état"}
	if !IsSingleValued(list, "has_observed_state") {
		t.Error("la liste en majuscules ne reconnaît pas la forme normalisée")
	}
	if !IsSingleValued(list, "A_POUR_ÉTAT") {
		t.Error("le type en majuscules n'est pas reconnu")
	}
	if IsSingleValued(list, "likes") {
		t.Error("un type absent de la liste est reconnu")
	}
}
