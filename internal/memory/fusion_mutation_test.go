package memory

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// Ajouté suite à la revue: MergeAdjacent copiait les Fused d'entrée
// superficiellement (copy() sur un []Fused ne clone ni les map ni les
// slices), puis mergeBestRank/mergeRawScores réutilisaient la map du
// premier extrait comme cible d'écriture. Résultat: fusionner deux extraits
// adjacents insérait les clés du second directement dans les map de
// l'appelant, alors que MergeAdjacent est censé être un calcul pur qui
// rend un résultat neuf sans toucher à son entrée.
func TestMergeAdjacentDoesNotMutateCallerInputs(t *testing.T) {
	a, b := uuid.New(), uuid.New()

	// entities0 a de la capacité de réserve (cap=4, len=1): un append qui
	// réutiliserait cette capacité écrirait dans la mémoire de l'appelant
	// sans même changer la longueur ni l'adresse de la slice, invisible à
	// une simple comparaison de contenu.
	entities0 := make([]string, 1, 4)
	entities0[0] = "tomate"

	items := []Fused{
		{
			AnchorMessageID: a, ConversationID: "conv_1",
			StartSequence: 1, EndSequence: 3, Score: 0.9,
			Ranks:           map[string]int{"dense": 1},
			RawScores:       map[string]float64{"dense": 0.1},
			MatchedEntities: entities0,
		},
		{
			AnchorMessageID: b, ConversationID: "conv_1",
			StartSequence: 3, EndSequence: 5, Score: 0.5,
			Ranks:           map[string]int{"lexical": 2},
			RawScores:       map[string]float64{"lexical": 0.2},
			MatchedEntities: []string{"potager"},
		},
	}

	// Instantané profond de l'entrée avant l'appel.
	wantRanks := map[string]int{"dense": 1}
	wantRawScores := map[string]float64{"dense": 0.1}
	wantEntities := []string{"tomate"}

	merged := MergeAdjacent(items)

	if len(merged) != 1 {
		t.Fatalf("%d extraits recollés, want 1 (préambule du test)", len(merged))
	}
	// Le résultat doit bien contenir la fusion (sinon le test ne prouve rien).
	if merged[0].Ranks["lexical"] != 2 || merged[0].RawScores["lexical"] != 0.2 {
		t.Fatalf("le recollement n'a pas eu lieu comme attendu: %+v", merged[0])
	}

	if !reflect.DeepEqual(items[0].Ranks, wantRanks) {
		t.Errorf("Ranks de l'entrée modifié par effet de bord: %v, want %v",
			items[0].Ranks, wantRanks)
	}
	if !reflect.DeepEqual(items[0].RawScores, wantRawScores) {
		t.Errorf("RawScores de l'entrée modifié par effet de bord: %v, want %v",
			items[0].RawScores, wantRawScores)
	}
	if !reflect.DeepEqual(items[0].MatchedEntities, wantEntities) {
		t.Errorf("MatchedEntities de l'entrée modifié par effet de bord: %v, want %v",
			items[0].MatchedEntities, wantEntities)
	}
	// Sonde au-delà de la longueur visible: si l'append a réutilisé la
	// capacité de réserve d'entities0, l'index 1 du tableau sous-jacent
	// contiendra "potager" même si items[0].MatchedEntities semble intact.
	probe := entities0[:cap(entities0)]
	if probe[1] != "" {
		t.Errorf("la capacité de réserve de la slice d'entrée a été écrasée: %q", probe[1])
	}
}
