package graph

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCanonicalKeyNormalise(t *testing.T) {
	cases := []struct{ in, name, want string }{
		{"person", "Paul Dupont", "person:paul-dupont"},
		{"person", "  Paul   Dupont  ", "person:paul-dupont"},
		{"plant", "Tomate cerise", "plant:tomate-cerise"},
		{"person", "Café Müller", "person:café-müller"},
		{"Person", "Paul", "person:paul"},
		{"person", "Jean-Luc", "person:jean-luc"},
		{"person", "!!!", "person:"},
	}
	for _, c := range cases {
		if got := CanonicalKey(c.in, c.name); got != c.want {
			t.Errorf("CanonicalKey(%q, %q) = %q, want %q", c.in, c.name, got, c.want)
		}
	}
}

func TestUnresolvedKeyPorteLaConversation(t *testing.T) {
	got := UnresolvedKey("person", "Paul", "conv_8453")
	want := "unresolved-person:paul-conv_8453"
	if got != want {
		t.Errorf("UnresolvedKey = %q, want %q", got, want)
	}
}

func TestEntityIDDeterministeEtIsoleParWorkspace(t *testing.T) {
	a := EntityID("ws1", "person:paul")
	b := EntityID("ws1", "person:paul")
	c := EntityID("ws2", "person:paul")
	if a != b {
		t.Errorf("EntityID non déterministe: %s != %s", a, b)
	}
	if a == c {
		t.Errorf("EntityID identique entre deux workspaces: %s", a)
	}
}

// Le cas qui condamne le séparateur simple de la spec: deux relations
// différentes dont la concaténation naïve serait identique.
func TestDedupKeyResisteAuSeparateurDansLesDonnees(t *testing.T) {
	src := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	a := DedupKey(src, "likes|very", nil, "green", nil)
	b := DedupKey(src, "likes", nil, "very|green", nil)
	if a == b {
		t.Fatalf("collision entre deux relations distinctes: %q", a)
	}
}

func TestDedupKeyIgnoreLaCasseDuLitteral(t *testing.T) {
	src := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	if DedupKey(src, "has_color", nil, "Green", nil) !=
		DedupKey(src, "has_color", nil, "green", nil) {
		t.Error("la casse du littéral change la dedup_key")
	}
}

func TestDedupKeyDistingueEntiteEtLitteral(t *testing.T) {
	src := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tgt := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	if DedupKey(src, "knows", &tgt, "", nil) ==
		DedupKey(src, "knows", nil, tgt.String(), nil) {
		t.Error("une cible entité et un littéral de même texte partagent la même clé")
	}
}

// Le même valid_from peut arriver avec des fractions de seconde différentes
// selon le chemin qu'il a pris, un timestamptz de Postgres portant des
// microsecondes là où un time.Now en porte des nanosecondes. Tronquer à la
// seconde fait que le même fait garde la même clé.
func TestDedupKeyTronqueLeValidFromALaSeconde(t *testing.T) {
	src := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	t1 := time.Date(2026, 6, 1, 10, 0, 0, 123456000, time.UTC)
	t2 := time.Date(2026, 6, 1, 10, 0, 0, 987654000, time.UTC)
	if DedupKey(src, "x", nil, "y", &t1) != DedupKey(src, "x", nil, "y", &t2) {
		t.Error("les fractions de seconde changent la dedup_key")
	}
}

func TestDedupKeyNormaliseLeFuseau(t *testing.T) {
	src := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	utc := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	paris := utc.In(time.FixedZone("CEST", 2*3600))
	if DedupKey(src, "x", nil, "y", &utc) != DedupKey(src, "x", nil, "y", &paris) {
		t.Error("le fuseau du valid_from change la dedup_key")
	}
}

func TestDedupKeyDistingueValidFromAbsentEtEpoque(t *testing.T) {
	src := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	zero := time.Time{}
	if DedupKey(src, "x", nil, "y", nil) == DedupKey(src, "x", nil, "y", &zero) {
		t.Error("un valid_from absent et un valid_from à zéro partagent la même clé")
	}
}

func TestNormalizeRelationType(t *testing.T) {
	cases := []struct{ in, want string }{
		{"HAS_OBSERVED_STATE", "has_observed_state"},
		{"  has observed state ", "has_observed_state"},
		{"a-pour-état", "a_pour_état"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeRelationType(c.in); got != c.want {
			t.Errorf("NormalizeRelationType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSlugConserveLesAccentsDecomposes couvre le constat de la revue des
// tâches 1 et 3: un "é" écrit en NFD est un "e" suivi d'une marque
// combinante, et une marque combinante n'est ni une lettre ni un chiffre.
// Sans traitement, la forme décomposée perdait son accent et produisait une
// clé canonique différente de sa jumelle composée, sans que rien ne le
// signale.
//
// Les deux formes restent deux clés distinctes, faute de normalisation
// Unicode dans la bibliothèque standard. Ce que ce test garantit, c'est
// qu'aucune des deux ne perd d'information au passage.
func TestSlugConserveLesAccentsDecomposes(t *testing.T) {
	const (
		compose   = "café"  // é précomposé
		decompose = "café" // e + accent aigu combinant
	)
	if got := Slug(compose); got != "café" {
		t.Errorf("Slug(NFC) = %q, want %q", got, "café")
	}
	got := Slug(decompose)
	if got == "cafe" {
		t.Errorf("Slug(NFD) = %q: l'accent a été perdu", got)
	}
	if []rune(got)[len([]rune(got))-1] != '́' {
		t.Errorf("Slug(NFD) = %q, la marque combinante devrait être conservée", got)
	}
}

// TestRelationIDDeterministeEtIsoleParWorkspace: la revue a noté que
// RelationID revendiquait la même propriété qu'EntityID sans qu'aucun test
// ne la vérifie. Elle porte pourtant l'idempotence du rejeu d'un job
// d'extraction: deux calculs de la même relation doivent réécrire la même
// ligne plutôt qu'en créer une seconde.
func TestRelationIDDeterministeEtIsoleParWorkspace(t *testing.T) {
	src := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	dk := DedupKey(src, "likes", nil, "green", nil)

	a := RelationID("ws1", dk)
	if b := RelationID("ws1", dk); a != b {
		t.Errorf("RelationID non déterministe: %s != %s", a, b)
	}
	if c := RelationID("ws2", dk); a == c {
		t.Errorf("RelationID identique entre deux workspaces: %s", a)
	}
	other := DedupKey(src, "likes", nil, "red", nil)
	if d := RelationID("ws1", other); a == d {
		t.Errorf("deux dedup_key distinctes donnent le même RelationID: %s", a)
	}
}
