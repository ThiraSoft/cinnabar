package memory

import (
	"testing"

	"github.com/google/uuid"
)

func cand(strategy string, rank int, anchor uuid.UUID, from, to int64) Candidate {
	return Candidate{
		Strategy: strategy, Rank: rank, RawScore: 1 / float64(rank),
		AnchorMessageID: anchor, ConversationID: "conv_1",
		StartSequence: from, EndSequence: to,
		AccessReason: "conversation_participant",
	}
}

func TestFuseRRFCombinesRanksAcrossStrategies(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()

	dense := []Candidate{
		cand("dense", 1, a, 1, 1),
		cand("dense", 2, b, 2, 2),
	}
	lexical := []Candidate{
		cand("lexical", 1, b, 2, 2),
		cand("lexical", 2, c, 3, 3),
	}

	fused := FuseRRF(60, dense, lexical)

	// b est premier en lexical et second en dense, donc il doit battre a qui
	// n'apparaît que dans une seule liste.
	if len(fused) != 3 {
		t.Fatalf("%d résultats, want 3", len(fused))
	}
	if fused[0].AnchorMessageID != b {
		t.Errorf("premier = %v, want b: présent dans les deux listes", fused[0].AnchorMessageID)
	}
	// Vérification de la formule: 1/(60+2) + 1/(60+1).
	want := 1.0/62.0 + 1.0/61.0
	if diff := fused[0].Score - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("score = %v, want %v", fused[0].Score, want)
	}
	if fused[0].Ranks["dense"] != 2 || fused[0].Ranks["lexical"] != 1 {
		t.Errorf("rangs = %v", fused[0].Ranks)
	}
	if len(fused[0].RawScores) != 2 {
		t.Errorf("les scores bruts des deux stratégies doivent être conservés: %v",
			fused[0].RawScores)
	}
}

func TestFuseRRFGroupsByAnchorMessage(t *testing.T) {
	a := uuid.New()
	// Le dense projette son unité sur son ancre, donc les deux stratégies
	// convergent sur la même clé de fusion.
	fused := FuseRRF(60,
		[]Candidate{cand("dense", 1, a, 1, 3)},
		[]Candidate{cand("lexical", 1, a, 3, 3)},
	)
	if len(fused) != 1 {
		t.Fatalf("%d résultats, want 1: la fusion se fait sur le message d'ancrage", len(fused))
	}
	// L'intervalle retenu est l'union, pour ne pas perdre le contexte que
	// l'unité dense apportait.
	if fused[0].StartSequence != 1 || fused[0].EndSequence != 3 {
		t.Errorf("intervalle = [%d,%d], want [1,3]",
			fused[0].StartSequence, fused[0].EndSequence)
	}
}

func TestFuseRRFIgnoresEmptyLists(t *testing.T) {
	a := uuid.New()
	fused := FuseRRF(60, nil, []Candidate{cand("lexical", 1, a, 1, 1)}, nil)
	if len(fused) != 1 {
		t.Fatalf("%d résultats, want 1", len(fused))
	}
	if fused[0].Ranks["dense"] != 0 {
		t.Error("une stratégie absente ne doit pas apparaître dans les rangs")
	}
}

func TestFuseRRFIsDeterministicOnTies(t *testing.T) {
	a, b := uuid.MustParse("00000000-0000-0000-0000-00000000000a"),
		uuid.MustParse("00000000-0000-0000-0000-00000000000b")

	// Deux candidats au même rang dans la même stratégie: l'ordre doit être
	// stable d'un appel à l'autre, sinon la réponse du service devient
	// non reproductible.
	for i := 0; i < 5; i++ {
		fused := FuseRRF(60, []Candidate{
			cand("dense", 1, a, 1, 1),
			cand("dense", 1, b, 2, 2),
		})
		if fused[0].AnchorMessageID != a {
			t.Fatalf("itération %d: ordre instable sur égalité", i)
		}
	}
}

func TestMergeAdjacentRecollesOverlappingIntervals(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()

	items := []Fused{
		{AnchorMessageID: a, ConversationID: "conv_1", StartSequence: 1,
			EndSequence: 3, Score: 0.9, AccessReason: "conversation_participant"},
		{AnchorMessageID: b, ConversationID: "conv_1", StartSequence: 3,
			EndSequence: 5, Score: 0.5, AccessReason: "conversation_participant"},
		{AnchorMessageID: c, ConversationID: "conv_2", StartSequence: 1,
			EndSequence: 2, Score: 0.7, AccessReason: "workspace_scope"},
	}

	merged := MergeAdjacent(items)

	if len(merged) != 2 {
		t.Fatalf("%d résultats, want 2: les intervalles qui se touchent sont recollés", len(merged))
	}
	// Le recollement garde le meilleur score et l'union des intervalles.
	if merged[0].StartSequence != 1 || merged[0].EndSequence != 5 {
		t.Errorf("intervalle recollé = [%d,%d], want [1,5]",
			merged[0].StartSequence, merged[0].EndSequence)
	}
	if merged[0].Score != 0.9 {
		t.Errorf("score = %v, want 0.9 (le meilleur)", merged[0].Score)
	}
	if merged[1].ConversationID != "conv_2" {
		t.Error("deux conversations différentes ne doivent jamais être recollées")
	}
}

func TestMergeAdjacentKeepsDistantIntervalsApart(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	items := []Fused{
		{AnchorMessageID: a, ConversationID: "conv_1", StartSequence: 1, EndSequence: 2, Score: 0.9},
		{AnchorMessageID: b, ConversationID: "conv_1", StartSequence: 40, EndSequence: 41, Score: 0.8},
	}
	if got := MergeAdjacent(items); len(got) != 2 {
		t.Errorf("%d résultats, want 2: deux passages distants restent distincts", len(got))
	}
}

func excerpt(score float64, core int64, before, after int) Excerpt {
	e := Excerpt{
		Fused: Fused{
			AnchorMessageID: uuid.New(), ConversationID: "conv_1",
			StartSequence: core, EndSequence: core, Score: score,
		},
		CoreFrom: core, CoreTo: core,
	}
	for i := before; i > 0; i-- {
		e.Messages = append(e.Messages,
			msg(core-int64(i), "user:paul", "user", "contexte avant 0123456789"))
	}
	e.Messages = append(e.Messages, msg(core, "user:paul", "user", "coeur 0123456789"))
	for i := 1; i <= after; i++ {
		e.Messages = append(e.Messages,
			msg(core+int64(i), "user:paul", "user", "contexte après 0123456789"))
	}
	return e
}

func TestFitBudgetKeepsEverythingWhenItFits(t *testing.T) {
	items := []Excerpt{excerpt(0.9, 10, 2, 2)}
	out := FitBudget(items, 10000, 4)
	if len(out) != 1 || len(out[0].Messages) != 5 {
		t.Errorf("rien ne doit être coupé quand le budget suffit: %d extraits, %d messages",
			len(out), len(out[0].Messages))
	}
}

func TestFitBudgetDegradesExpansionBeforeDroppingExcerpts(t *testing.T) {
	items := []Excerpt{excerpt(0.9, 10, 2, 2), excerpt(0.5, 50, 2, 2)}

	total := ExcerptChars(items[0]) + ExcerptChars(items[1])
	// Budget qui force une réduction sans permettre de tout garder.
	budget := (total - 40) / 4

	out := FitBudget(items, budget, 4)

	if len(out) != 2 {
		t.Fatalf("%d extraits, want 2: on dégrade l'expansion avant de jeter", len(out))
	}
	// L'extrait le moins bien classé perd son contexte en premier.
	if len(out[1].Messages) >= len(out[0].Messages) {
		t.Errorf("l'extrait le moins bien classé doit être dégradé en premier: %d vs %d",
			len(out[1].Messages), len(out[0].Messages))
	}
	// Le coeur survit toujours.
	for i, e := range out {
		if len(e.Messages) == 0 {
			t.Errorf("extrait %d vidé de son coeur", i)
		}
	}
}

func TestFitBudgetDropsExcerptsAsLastResort(t *testing.T) {
	items := []Excerpt{excerpt(0.9, 10, 0, 0), excerpt(0.5, 50, 0, 0)}

	// Budget qui ne peut contenir qu'un seul coeur.
	budget := (ExcerptChars(items[0]) + 4) / 4

	out := FitBudget(items, budget, 4)
	if len(out) != 1 {
		t.Fatalf("%d extraits, want 1", len(out))
	}
	if out[0].Score != 0.9 {
		t.Error("c'est l'extrait le moins bien classé qui doit être écarté")
	}
}

func TestFitBudgetNeverTruncatesMidMessage(t *testing.T) {
	items := []Excerpt{excerpt(0.9, 10, 0, 0)}
	// Budget plus petit que le seul message: on rend zéro extrait plutôt
	// qu'un message coupé, qui produirait un souvenir trompeur.
	out := FitBudget(items, 1, 4)
	if len(out) != 0 {
		t.Errorf("%d extraits, want 0", len(out))
	}
}
