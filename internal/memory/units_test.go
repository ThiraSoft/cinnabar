package memory

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

func testIndexing() config.Indexing {
	return config.Indexing{
		Strategy: "contextualized_message", PreviousMessages: 2,
		MaxChars: 1600, CharsPerToken: 4,
		IndexUserMessages: true, IndexAgentMessages: true,
		IndexSystemMessages: false, IndexToolMessages: false, Version: 1,
	}
}

func msg(seq int64, author, role, content string) Message {
	return Message{
		MessageID:      uuid.New(),
		ConversationID: "conv_8453",
		WorkspaceID:    "ws1",
		SequenceNumber: seq,
		AuthorKey:      author,
		Role:           role,
		Content:        content,
		CreatedAt:      time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC),
	}
}

func TestShouldIndexFollowsConfig(t *testing.T) {
	cfg := testIndexing()
	cases := map[string]bool{
		"user": true, "assistant": true, "system": false, "tool": false,
	}
	for role, want := range cases {
		if got := ShouldIndex(cfg, role); got != want {
			t.Errorf("ShouldIndex(%q) = %v, want %v", role, got, want)
		}
	}
	cfg.IndexToolMessages = true
	if !ShouldIndex(cfg, "tool") {
		t.Error("tool doit être indexé quand la config l'active")
	}
}

func TestUnitIDIsDeterministicAndDiscriminating(t *testing.T) {
	anchor := uuid.MustParse("94234016-c556-4894-878c-e0cdbfe38220")

	a := UnitID(anchor, "nomic-embed-text-v2-moe", "contextualized_message", 1)
	b := UnitID(anchor, "nomic-embed-text-v2-moe", "contextualized_message", 1)
	if a != b {
		t.Fatal("UnitID doit être déterministe: c'est le second verrou du critère 5")
	}
	if a.Version() != 5 {
		t.Errorf("UnitID doit produire un UUIDv5, got v%d", a.Version())
	}
	// Chaque composante doit discriminer, sinon un changement de modèle ou de
	// version d'indexation écraserait l'ancienne unité.
	if UnitID(anchor, "autre-modele", "contextualized_message", 1) == a {
		t.Error("le modèle doit discriminer")
	}
	if UnitID(anchor, "nomic-embed-text-v2-moe", "autre_strategie", 1) == a {
		t.Error("la stratégie doit discriminer")
	}
	if UnitID(anchor, "nomic-embed-text-v2-moe", "contextualized_message", 2) == a {
		t.Error("la version doit discriminer")
	}
	if UnitID(uuid.New(), "nomic-embed-text-v2-moe", "contextualized_message", 1) == a {
		t.Error("l'ancre doit discriminer")
	}
}

func TestBuildUnitShapesTheText(t *testing.T) {
	cfg := testIndexing()
	anchor := msg(42, "user:paul", "user",
		"Je suis passé près de mes tomates, elles étaient encore vertes.")
	prev := []Message{
		msg(40, "user:paul", "user", "Salut"),
		msg(41, "agent:jardinage", "assistant", "Comment avance ton potager ?"),
	}

	u, ok := BuildUnit(cfg, "nomic-embed-text-v2-moe", "participants",
		[]string{"user:paul", "agent:jardinage"}, anchor, prev)
	if !ok {
		t.Fatal("un message user doit produire une unité")
	}

	if u.StartSequence != 40 || u.EndSequence != 42 {
		t.Errorf("intervalle = [%d,%d], want [40,42]", u.StartSequence, u.EndSequence)
	}
	if u.AnchorMessageID != anchor.MessageID {
		t.Error("l'ancre doit être le message cible")
	}
	if u.Scope != "participants" || u.WorkspaceID != "ws1" {
		t.Errorf("scope=%q workspace=%q", u.Scope, u.WorkspaceID)
	}

	txt := u.EmbeddingText
	// Le préambule nomme les participants, comme la section 9.1 de la spec.
	if !strings.HasPrefix(txt, "Conversation impliquant user:paul et agent:jardinage.") {
		t.Errorf("préambule manquant ou mal formé:\n%s", txt)
	}
	// Les rôles et auteurs restent visibles, pour qu'une hypothèse d'agent ne
	// soit pas confondue avec une affirmation utilisateur.
	if !strings.Contains(txt, "agent:jardinage : Comment avance ton potager ?") {
		t.Errorf("contexte précédent absent:\n%s", txt)
	}
	if !strings.Contains(txt, "user:paul : Je suis passé près de mes tomates") {
		t.Errorf("message principal absent:\n%s", txt)
	}
	// Le préfixe d'encodage n'est jamais stocké.
	if strings.Contains(txt, "search_document:") {
		t.Error("le préfixe search_document ne doit pas être stocké")
	}
}

func TestBuildUnitSkipsNonIndexedRoles(t *testing.T) {
	cfg := testIndexing()
	anchor := msg(1, "system", "system", "Tu es un assistant.")
	if _, ok := BuildUnit(cfg, "m", "participants", nil, anchor, nil); ok {
		t.Error("un message system ne doit pas produire d'unité")
	}
}

func TestBuildUnitDropsDeletedContext(t *testing.T) {
	cfg := testIndexing()
	deleted := msg(41, "user:paul", "user", "à oublier")
	now := time.Now()
	deleted.DeletedAt = &now

	anchor := msg(42, "user:paul", "user", "message courant")
	u, ok := BuildUnit(cfg, "m", "participants", nil, anchor, []Message{deleted})
	if !ok {
		t.Fatal("l'unité doit être produite")
	}
	if strings.Contains(u.EmbeddingText, "à oublier") {
		t.Error("un message supprimé ne doit pas servir de contexte")
	}
	if u.StartSequence != 42 {
		t.Errorf("start = %d, want 42 quand tout le contexte est supprimé", u.StartSequence)
	}
}

func TestBuildUnitTruncatesContextFirst(t *testing.T) {
	cfg := testIndexing()
	cfg.MaxChars = 200

	anchor := msg(42, "user:paul", "user", strings.Repeat("A", 150))
	prev := []Message{msg(41, "agent:x", "assistant", strings.Repeat("B", 500))}

	u, ok := BuildUnit(cfg, "m", "participants", nil, anchor, prev)
	if !ok {
		t.Fatal("l'unité doit être produite")
	}
	if len(u.EmbeddingText) > cfg.MaxChars {
		t.Errorf("longueur = %d, doit rester sous %d", len(u.EmbeddingText), cfg.MaxChars)
	}
	// Le message principal survit, le contexte est sacrifié.
	if !strings.Contains(u.EmbeddingText, strings.Repeat("A", 150)) {
		t.Error("le message principal ne doit pas être tronqué avant le contexte")
	}
	if strings.Count(u.EmbeddingText, "B") > 100 {
		t.Error("le contexte doit être tronqué en premier")
	}
}

func TestBuildUnitTruncatesAnchorAsLastResort(t *testing.T) {
	cfg := testIndexing()
	cfg.MaxChars = 120

	anchor := msg(42, "user:paul", "user", strings.Repeat("A", 1000))
	u, ok := BuildUnit(cfg, "m", "participants", nil, anchor, nil)
	if !ok {
		t.Fatal("l'unité doit être produite")
	}
	if len(u.EmbeddingText) > cfg.MaxChars {
		t.Errorf("longueur = %d, doit rester sous %d", len(u.EmbeddingText), cfg.MaxChars)
	}
}

func TestBuildUnitKeepsOnlyConfiguredNumberOfPreviousMessages(t *testing.T) {
	cfg := testIndexing()
	cfg.PreviousMessages = 2

	prev := []Message{
		msg(38, "user:paul", "user", "trop-vieux"),
		msg(39, "user:paul", "user", "avant-avant"),
		msg(40, "user:paul", "user", "avant"),
	}
	anchor := msg(41, "user:paul", "user", "courant")

	u, ok := BuildUnit(cfg, "m", "participants", nil, anchor, prev)
	if !ok {
		t.Fatal("l'unité doit être produite")
	}
	if strings.Contains(u.EmbeddingText, "trop-vieux") {
		t.Error("seuls les 2 derniers messages précédents doivent être gardés")
	}
	if u.StartSequence != 39 {
		t.Errorf("start = %d, want 39", u.StartSequence)
	}
}

// Ajoutés suite à la revue: les tests de troncature de la spec initiale ne
// portaient que sur des répétitions ASCII ("A"), ce qui ne pouvait jamais
// débusquer une troncature qui jette une rune multi-octets complète alors
// qu'elle tenait dans le budget. Le contenu indexé est français, donc les
// runes de 2 octets (accents) sont le cas courant, pas un cas rare.
func TestTruncateRunesKeepsCompleteMultibyteRunesAndDropsIncompleteOnes(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{
			name: "coupe pile après une rune de 2 octets (é) : elle est gardée",
			s:    "café supplémentaire",
			max:  5, // "café" = c(1)+a(1)+f(1)+é(2) = 5 octets, tient exactement
			want: "café",
		},
		{
			name: "coupe à l'intérieur d'une rune de 2 octets : elle est retirée proprement",
			s:    "café supplémentaire",
			max:  4, // ne garde que le premier des deux octets de é
			want: "caf",
		},
		{
			name: "coupe pile après une rune de 3 octets (€) : elle est gardée",
			s:    "prix : 3€ le kilo",
			max:  11, // "prix : 3" (8 octets) + € (3 octets) = 11, tient exactement
			want: "prix : 3€",
		},
		{
			name: "coupe à l'intérieur d'une rune de 3 octets : elle est retirée proprement",
			s:    "prix : 3€ le kilo",
			max:  9, // ne garde que le premier des trois octets de €
			want: "prix : 3",
		},
		{
			name: "coupe pile après une rune de 4 octets (émoji) : elle est gardée",
			s:    "Les tomates poussent bien 🍅 vraiment",
			max:  30, // préfixe ASCII (26 octets) + émoji (4 octets) = 30, tient exactement
			want: "Les tomates poussent bien 🍅",
		},
		{
			name: "coupe à l'intérieur d'une rune de 4 octets : elle est retirée proprement",
			s:    "Les tomates poussent bien 🍅 vraiment",
			max:  28, // ne garde que deux des quatre octets de l'émoji
			want: "Les tomates poussent bien ",
		},
		{
			name: "max plus petit que la toute première rune : chaîne vide, pas d'octet cassé",
			s:    "économie du jardin",
			max:  1, // ne garde que le premier des deux octets du é initial
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateRunes(c.s, c.max)
			if got != c.want {
				t.Errorf("TruncateRunes(%q, %d) = %q, want %q", c.s, c.max, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateRunes(%q, %d) = %q n'est pas de l'UTF-8 valide", c.s, c.max, got)
			}
		})
	}
}

// Ajouté suite à la revue: la clé de UnitID était assemblée avec "|" comme
// séparateur non échappé. Deux tuples différents, dont l'un contient "|"
// dans une de ses valeurs, produisaient la même clé et donc le même
// identifiant: exactement la collision que UnitID existe pour empêcher.
func TestUnitIDKeyFormatIsUnambiguous(t *testing.T) {
	anchor := uuid.MustParse("94234016-c556-4894-878c-e0cdbfe38220")

	a := UnitID(anchor, "m|s", "extra", 1)
	b := UnitID(anchor, "m", "s|extra", 1)
	if a == b {
		t.Error(`UnitID(anchor, "m|s", "extra", 1) et UnitID(anchor, "m", "s|extra", 1) ` +
			"ne doivent pas coïncider: la frontière entre modèle et stratégie doit être " +
			"non ambiguë quel que soit leur contenu")
	}
}

// Ajouté suite à la revue: un préambule (liste de participants) plus long
// que MaxChars ne doit jamais faire dépasser la limite annoncée, même quand
// le message principal est déjà réduit à rien.
func TestBuildUnitNeverExceedsMaxCharsEvenWithAnOversizedPreamble(t *testing.T) {
	cfg := testIndexing()
	cfg.MaxChars = 50

	hugeParticipants := []string{strings.Repeat("participant-tres-long-", 10)}
	anchor := msg(1, "user:paul", "user", "message")

	u, ok := BuildUnit(cfg, "m", "participants", hugeParticipants, anchor, nil)
	if !ok {
		t.Fatal("l'unité doit être produite")
	}
	if len(u.EmbeddingText) > cfg.MaxChars {
		t.Errorf("longueur = %d, doit rester sous %d même avec un préambule surdimensionné",
			len(u.EmbeddingText), cfg.MaxChars)
	}
}
