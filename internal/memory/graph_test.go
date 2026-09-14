package memory

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGraphExtractionValidateRejetteUneRelationOrpheline(t *testing.T) {
	e := GraphExtraction{
		Entities: []GraphEntity{{
			EntityID: uuid.New(), CanonicalKey: "person:paul",
			EntityType: "person", DisplayName: "Paul",
		}},
		Relations: []GraphRelation{{
			SourceEntityID:   uuid.New(), // inconnue de Entities
			RelationType:     "likes",
			TargetLiteral:    "green",
			ObservedAt:       time.Now(),
			SourceMessageIDs: []uuid.UUID{uuid.New()},
		}},
	}
	got := e.Validate()
	if len(got.Relations) != 0 {
		t.Errorf("%d relations conservées, want 0", len(got.Relations))
	}
	if len(got.Entities) != 1 {
		t.Errorf("%d entités conservées, want 1", len(got.Entities))
	}
}

func TestGraphExtractionValidateRejetteUneCibleDoubleOuVide(t *testing.T) {
	src := uuid.New()
	tgt := uuid.New()
	base := []GraphEntity{
		{EntityID: src, CanonicalKey: "person:paul", EntityType: "person", DisplayName: "Paul"},
		{EntityID: tgt, CanonicalKey: "person:marie", EntityType: "person", DisplayName: "Marie"},
	}
	// Les deux cibles renseignées: le CHECK de graph_relations rejetterait
	// la ligne, donc on l'écarte avant d'atteindre Postgres.
	double := GraphExtraction{Entities: base, Relations: []GraphRelation{{
		SourceEntityID: src, RelationType: "knows",
		TargetEntityID: &tgt, TargetLiteral: "marie",
		ObservedAt:       time.Now(),
		SourceMessageIDs: []uuid.UUID{uuid.New()},
	}}}
	if len(double.Validate().Relations) != 0 {
		t.Error("une relation à double cible a été conservée")
	}
	// Aucune des deux.
	vide := GraphExtraction{Entities: base, Relations: []GraphRelation{{
		SourceEntityID: src, RelationType: "knows", ObservedAt: time.Now(),
		SourceMessageIDs: []uuid.UUID{uuid.New()},
	}}}
	if len(vide.Validate().Relations) != 0 {
		t.Error("une relation sans cible a été conservée")
	}
}

func TestGraphExtractionValidateBorneLaConfiance(t *testing.T) {
	src := uuid.New()
	e := GraphExtraction{
		Entities: []GraphEntity{{EntityID: src, CanonicalKey: "person:paul",
			EntityType: "person", DisplayName: "Paul"}},
		Relations: []GraphRelation{
			{SourceEntityID: src, RelationType: "a", TargetLiteral: "x",
				ObservedAt: time.Now(), Confidence: 4.2,
				SourceMessageIDs: []uuid.UUID{uuid.New()}},
			{SourceEntityID: src, RelationType: "b", TargetLiteral: "x",
				ObservedAt: time.Now(), Confidence: -1,
				SourceMessageIDs: []uuid.UUID{uuid.New()}},
		},
	}
	got := e.Validate()
	if len(got.Relations) != 2 {
		t.Fatalf("%d relations, want 2", len(got.Relations))
	}
	if got.Relations[0].Confidence != 1 || got.Relations[1].Confidence != 0 {
		t.Errorf("confiances = %v, %v; want 1, 0",
			got.Relations[0].Confidence, got.Relations[1].Confidence)
	}
}

func TestGraphExtractionValidateRejetteUneRelationSansSource(t *testing.T) {
	src := uuid.New()
	e := GraphExtraction{
		Entities: []GraphEntity{{EntityID: src, CanonicalKey: "person:paul",
			EntityType: "person", DisplayName: "Paul"}},
		Relations: []GraphRelation{{
			SourceEntityID: src, RelationType: "likes", TargetLiteral: "green",
			ObservedAt: time.Now(), SourceMessageIDs: nil,
		}},
	}
	if len(e.Validate().Relations) != 0 {
		t.Error("une relation sans message source a été conservée")
	}
}

// TestGraphExtractionValidateRejetteUneCibleEntiteInconnue: branche de
// Validate qu'aucun des tests du plan n'exerçait, relevée par la revue de la
// tâche 2. Une relation dont la cible est une entité que l'extraction ne
// déclare pas violerait la clé étrangère de graph_relations, donc ferait
// échouer le job entier plutôt que la seule relation fautive.
func TestGraphExtractionValidateRejetteUneCibleEntiteInconnue(t *testing.T) {
	src := uuid.New()
	inconnue := uuid.New()
	e := GraphExtraction{
		Entities: []GraphEntity{{
			EntityID: src, CanonicalKey: "person:paul",
			EntityType: "person", DisplayName: "Paul",
		}},
		Relations: []GraphRelation{{
			SourceEntityID:   src,
			RelationType:     "knows",
			TargetEntityID:   &inconnue,
			ObservedAt:       time.Now(),
			SourceMessageIDs: []uuid.UUID{uuid.New()},
		}},
	}
	if got := e.Validate(); len(got.Relations) != 0 {
		t.Errorf("%d relations conservées, want 0: la cible n'est déclarée nulle part",
			len(got.Relations))
	}
}

// TestGraphExtractionValidateNePartagePasSonTableauDEntites: Validate rend une
// valeur, et sans copie son tableau d'entités reste celui de son entrée. Un
// appelant qui trie ou modifie l'un modifierait l'autre à distance. Ce projet
// a déjà livré ce défaut une fois, dans MergeAdjacent.
func TestGraphExtractionValidateNePartagePasSonTableauDEntites(t *testing.T) {
	in := GraphExtraction{
		Entities: []GraphEntity{{
			EntityID: uuid.New(), CanonicalKey: "person:paul",
			EntityType: "person", DisplayName: "Paul",
		}},
	}
	out := in.Validate()
	out.Entities[0].DisplayName = "modifié après coup"

	if in.Entities[0].DisplayName != "Paul" {
		t.Errorf("l'entrée a été modifiée à distance: %q", in.Entities[0].DisplayName)
	}
}

// TestGraphExtractionValidateRejetteUnWorkspaceIncoherent couvre la réserve
// que l'implémenteur de la tâche 5 a signalée: le repo écrit une relation
// sous le workspace de la relation, mais recalcule les chaînes de validité
// sous celui de l'extraction. Deux valeurs différentes produiraient une
// relation que sa propre maintenance temporelle ne reverrait plus jamais.
// L'extracteur renseigne les deux depuis la même source, donc l'écart ne
// vient que d'un bug: Validate est l'endroit où il s'arrête.
func TestGraphExtractionValidateRejetteUnWorkspaceIncoherent(t *testing.T) {
	src := uuid.New()
	etranger := uuid.New()
	e := GraphExtraction{
		WorkspaceID: "ws1",
		Entities: []GraphEntity{
			{EntityID: src, WorkspaceID: "ws1", CanonicalKey: "person:paul",
				EntityType: "person", DisplayName: "Paul"},
			{EntityID: etranger, WorkspaceID: "ws2", CanonicalKey: "person:marie",
				EntityType: "person", DisplayName: "Marie"},
		},
		Relations: []GraphRelation{
			{WorkspaceID: "ws1", SourceEntityID: src, RelationType: "likes",
				TargetLiteral: "green", ObservedAt: time.Now(),
				SourceMessageIDs: []uuid.UUID{uuid.New()}},
			{WorkspaceID: "ws2", SourceEntityID: src, RelationType: "likes",
				TargetLiteral: "red", ObservedAt: time.Now(),
				SourceMessageIDs: []uuid.UUID{uuid.New()}},
		},
	}

	got := e.Validate()
	if len(got.Entities) != 1 || got.Entities[0].WorkspaceID != "ws1" {
		t.Errorf("entités = %+v, want la seule de ws1", got.Entities)
	}
	if len(got.Relations) != 1 {
		t.Fatalf("%d relations conservées, want 1", len(got.Relations))
	}
	if got.Relations[0].TargetLiteral != "green" {
		t.Errorf("relation conservée = %q, want celle de ws1",
			got.Relations[0].TargetLiteral)
	}
}

// Une entité d'un autre workspace ne doit pas non plus pouvoir servir de
// cible: la relation deviendrait une arête vers une ligne que la recherche
// du workspace demandeur ne peut pas voir.
func TestGraphExtractionValidateRejetteUneCibleDUnAutreWorkspace(t *testing.T) {
	src := uuid.New()
	etranger := uuid.New()
	e := GraphExtraction{
		WorkspaceID: "ws1",
		Entities: []GraphEntity{
			{EntityID: src, WorkspaceID: "ws1", CanonicalKey: "person:paul",
				EntityType: "person", DisplayName: "Paul"},
			{EntityID: etranger, WorkspaceID: "ws2", CanonicalKey: "person:marie",
				EntityType: "person", DisplayName: "Marie"},
		},
		Relations: []GraphRelation{{
			WorkspaceID: "ws1", SourceEntityID: src, RelationType: "knows",
			TargetEntityID: &etranger, ObservedAt: time.Now(),
			SourceMessageIDs: []uuid.UUID{uuid.New()},
		}},
	}
	if got := e.Validate(); len(got.Relations) != 0 {
		t.Errorf("%d relations conservées, want 0: la cible est dans un autre workspace",
			len(got.Relations))
	}
}
