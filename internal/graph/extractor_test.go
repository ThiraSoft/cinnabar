package graph

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/llm"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

type fakeChat struct {
	reply   string
	err     error
	lastReq llm.ChatRequest
	calls   int
}

func (f *fakeChat) Chat(ctx context.Context, r llm.ChatRequest) (string, error) {
	f.calls++
	f.lastReq = r
	return f.reply, f.err
}

func testInput() memory.GraphExtractorInput {
	msgID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	ctxID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")
	return memory.GraphExtractorInput{
		WorkspaceID:    "ws1",
		ConversationID: "conv1",
		Message: memory.Message{
			MessageID: msgID, ConversationID: "conv1", Role: "user",
			AuthorKey: "user:alice", Content: "Les tomates de Paul sont vertes.",
			CreatedAt: time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC),
		},
		Context: []memory.Message{{
			MessageID: ctxID, ConversationID: "conv1", Role: "user",
			AuthorKey: "user:alice", Content: "Paul jardine beaucoup.",
			CreatedAt: time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC),
		}},
		Participants: []string{"user:alice", "agent:cuisine"},
		CandidateEntities: []memory.GraphEntity{{
			EntityID: EntityID("ws1", "person:paul"), WorkspaceID: "ws1",
			CanonicalKey: "person:paul", EntityType: "person",
			DisplayName: "Paul", Resolved: true,
		}},
	}
}

const bonneReponse = `{
  "entities": [
    {"temp_id":"e1","entity_type":"person","display_name":"Paul",
     "canonical_key":"person:paul","aliases":[],"resolved":true},
    {"temp_id":"e2","entity_type":"plant","display_name":"Tomate",
     "canonical_key":null,"aliases":["tomates"],"resolved":true}
  ],
  "relations": [
    {"source_temp_id":"e2","relation_type":"HAS_OBSERVED_STATE",
     "target_temp_id":null,"target_literal":"vertes",
     "observed_at":"2026-09-09T10:00:00Z","valid_from":null,"valid_until":null,
     "confidence":0.9,
     "source_message_ids":["aaaaaaaa-0000-0000-0000-000000000001"]},
    {"source_temp_id":"e1","relation_type":"owns",
     "target_temp_id":"e2","target_literal":null,
     "observed_at":"2026-09-09T10:00:00Z","valid_from":null,"valid_until":null,
     "confidence":0.8,
     "source_message_ids":["aaaaaaaa-0000-0000-0000-000000000001"]}
  ]
}`

func TestExtractResoutLesTempIDsEtNormalise(t *testing.T) {
	chat := &fakeChat{reply: bonneReponse}
	e := NewExtractor(chat, config.Graph{ContextMessages: 4})

	got, err := e.Extract(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got.Entities) != 2 {
		t.Fatalf("%d entités, want 2", len(got.Entities))
	}
	if len(got.Relations) != 2 {
		t.Fatalf("%d relations, want 2", len(got.Relations))
	}

	byKey := map[string]memory.GraphEntity{}
	for _, ent := range got.Entities {
		byKey[ent.CanonicalKey] = ent
		if ent.WorkspaceID != "ws1" {
			t.Errorf("workspace de l'entité = %q, want ws1", ent.WorkspaceID)
		}
		if ent.EntityID != EntityID("ws1", ent.CanonicalKey) {
			t.Errorf("entity_id de %q non déterministe", ent.CanonicalKey)
		}
	}
	if _, ok := byKey["person:paul"]; !ok {
		t.Error("la clé canonique fournie par le modèle n'a pas été reprise")
	}
	if _, ok := byKey["plant:tomate"]; !ok {
		t.Errorf("clé canonique calculée absente, clés = %v", byKey)
	}

	for _, r := range got.Relations {
		if r.RelationType != NormalizeRelationType(r.RelationType) {
			t.Errorf("relation_type non normalisé: %q", r.RelationType)
		}
		if r.DedupKey == "" || r.RelationID == uuid.Nil {
			t.Errorf("relation sans clé de déduplication: %+v", r)
		}
		if r.ConversationID != "conv1" || r.WorkspaceID != "ws1" {
			t.Errorf("relation mal rattachée: %+v", r)
		}
	}
}

func TestExtractRabotteAuLieuDeRejeter(t *testing.T) {
	// e9 n'existe pas, la seconde relation a deux cibles, la troisième a
	// une confiance hors bornes. Seule la troisième doit survivre.
	reply := `{
	  "entities":[{"temp_id":"e1","entity_type":"person","display_name":"Paul",
	    "canonical_key":"person:paul","aliases":[],"resolved":true}],
	  "relations":[
	    {"source_temp_id":"e9","relation_type":"likes","target_temp_id":null,
	     "target_literal":"green","observed_at":"2026-09-09T10:00:00Z",
	     "valid_from":null,"valid_until":null,"confidence":0.5,
	     "source_message_ids":["aaaaaaaa-0000-0000-0000-000000000001"]},
	    {"source_temp_id":"e1","relation_type":"knows","target_temp_id":"e1",
	     "target_literal":"lui-même","observed_at":"2026-09-09T10:00:00Z",
	     "valid_from":null,"valid_until":null,"confidence":0.5,
	     "source_message_ids":["aaaaaaaa-0000-0000-0000-000000000001"]},
	    {"source_temp_id":"e1","relation_type":"likes","target_temp_id":null,
	     "target_literal":"green","observed_at":"2026-09-09T10:00:00Z",
	     "valid_from":null,"valid_until":null,"confidence":7,
	     "source_message_ids":["aaaaaaaa-0000-0000-0000-000000000001"]}
	  ]}`
	e := NewExtractor(&fakeChat{reply: reply}, config.Graph{})

	got, err := e.Extract(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got.Relations) != 1 {
		t.Fatalf("%d relations conservées, want 1: %+v", len(got.Relations), got.Relations)
	}
	if got.Relations[0].Confidence != 1 {
		t.Errorf("confidence = %v, want 1 (bornée)", got.Relations[0].Confidence)
	}
}

func TestExtractRejetteUneSourceHorsFenetre(t *testing.T) {
	// Un message_id que le modèle a inventé, ou pris ailleurs: il ne doit
	// jamais devenir une source, sinon une relation se rattacherait à une
	// conversation que le demandeur n'a pas le droit de lire, et la
	// dérivation d'accès par les sources laisserait fuiter.
	reply := strings.Replace(bonneReponse,
		`"aaaaaaaa-0000-0000-0000-000000000001"`,
		`"bbbbbbbb-0000-0000-0000-000000000009"`, -1)
	e := NewExtractor(&fakeChat{reply: reply}, config.Graph{})

	got, err := e.Extract(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Sans cette garde, le test passe à vide: une extraction qui ne rend
	// aucune relation satisfait trivialement toutes les assertions de la
	// boucle qui suit, et la frontière de sécurité n'est plus verifiée du
	// tout. C'est exactement ce que la revue de la tâche 6 a démontré en
	// supprimant l'affectation du WorkspaceID, qui vidait l'extraction sans
	// faire rougir ce test.
	if len(got.Relations) != 2 {
		t.Fatalf("%d relations, want 2: le repli sur le message ancre doit les "+
			"conserver, pas les faire disparaître", len(got.Relations))
	}
	for _, r := range got.Relations {
		for _, id := range r.SourceMessageIDs {
			if id.String() == "bbbbbbbb-0000-0000-0000-000000000009" {
				t.Fatal("un message_id hors fenêtre est devenu une source")
			}
		}
		if len(r.SourceMessageIDs) != 1 ||
			r.SourceMessageIDs[0] != testInput().Message.MessageID {
			t.Errorf("sources = %v, want [le message ancre]", r.SourceMessageIDs)
		}
	}
}

func TestExtractSurUneReponseIllisibleEchoue(t *testing.T) {
	e := NewExtractor(&fakeChat{reply: "je ne sais pas faire ça"}, config.Graph{})
	if _, err := e.Extract(context.Background(), testInput()); err == nil {
		t.Fatal("want une erreur sur une réponse non JSON")
	}
}

func TestExtractTolereLesBalisesMarkdown(t *testing.T) {
	e := NewExtractor(&fakeChat{reply: "```json\n" + bonneReponse + "\n```"}, config.Graph{})
	got, err := e.Extract(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got.Relations) == 0 {
		t.Error("la réponse entourée de balises de code n'a pas été comprise")
	}
}

func TestExtractPropageLErreurDuClient(t *testing.T) {
	want := errors.New("boom")
	e := NewExtractor(&fakeChat{err: want}, config.Graph{})
	if _, err := e.Extract(context.Background(), testInput()); !errors.Is(err, want) {
		t.Fatalf("err = %v, want une enveloppe de %v", err, want)
	}
}

func TestExtractEnvoieUnSchemaEtUneTemperatureNulle(t *testing.T) {
	chat := &fakeChat{reply: bonneReponse}
	e := NewExtractor(chat, config.Graph{})
	if _, err := e.Extract(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	if len(chat.lastReq.JSONSchema) == 0 {
		t.Error("aucun json_schema envoyé: le modèle est libre de répondre en prose")
	}
	if chat.lastReq.Temperature != 0 {
		t.Errorf("temperature = %v, want 0", chat.lastReq.Temperature)
	}
	if !strings.Contains(chat.lastReq.User, "person:paul") {
		t.Error("les entités candidates n'ont pas été proposées au modèle")
	}
	if !strings.Contains(chat.lastReq.User, "Les tomates de Paul sont vertes.") {
		t.Error("le message à traiter n'est pas dans le prompt")
	}
}

// TestExtractNeReprendUneCleCanoniqueQueSiElleEstConnue couvre le second
// constat de la revue de la tâche 6: aucun test ne distinguait "la clé
// canonique du modèle est reprise parce qu'elle figure parmi les candidates"
// de "elle est reprise inconditionnellement". Supprimer la garde laissait les
// sept tests verts.
//
// L'enjeu est la règle 7.3 de la spec: le service ne fusionne jamais deux
// entités de sa propre initiative. Un modèle qui invente "person:paul" pour
// un Paul dont il n'a jamais entendu parler rattacherait sa relation à
// l'entité d'un autre Paul, ce qui est exactement une fusion décidée par le
// modèle et non par un humain.
func TestExtractNeReprendUneCleCanoniqueQueSiElleEstConnue(t *testing.T) {
	const reponse = `{
	  "entities":[{"temp_id":"e1","entity_type":"person","display_name":"Marie",
	    "canonical_key":"person:une-cle-que-personne-n-a-proposee","aliases":[],
	    "resolved":true}],
	  "relations":[]}`

	e := NewExtractor(&fakeChat{reply: reponse}, config.Graph{})
	got, err := e.Extract(context.Background(), testInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entities) != 1 {
		t.Fatalf("%d entités, want 1", len(got.Entities))
	}

	// La clé inventée est ignorée au profit de celle que le service calcule
	// depuis le type et le nom.
	if got.Entities[0].CanonicalKey == "person:une-cle-que-personne-n-a-proposee" {
		t.Error("une clé canonique inventée par le modèle a été reprise telle quelle")
	}
	if want := CanonicalKey("person", "Marie"); got.Entities[0].CanonicalKey != want {
		t.Errorf("clé = %q, want %q", got.Entities[0].CanonicalKey, want)
	}

	// Et la clé d'une entité réellement proposée, elle, est bien reprise:
	// sans ce second cas, le test passerait aussi si le service ignorait
	// toujours ce que le modèle propose.
	const reprise = `{
	  "entities":[{"temp_id":"e1","entity_type":"person","display_name":"Paulo",
	    "canonical_key":"person:paul","aliases":[],"resolved":true}],
	  "relations":[]}`
	e2 := NewExtractor(&fakeChat{reply: reprise}, config.Graph{})
	got2, err := e2.Extract(context.Background(), testInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(got2.Entities) != 1 || got2.Entities[0].CanonicalKey != "person:paul" {
		t.Errorf("clé = %+v, want person:paul reprise depuis les candidates",
			got2.Entities)
	}
}

// TestExtractRetombeSurLaDateDuMessage: sans observed_at exploitable, la date
// du message fait foi. Un observed_at à zéro atteindrait Validate, qui
// jetterait la relation en silence.
func TestExtractRetombeSurLaDateDuMessage(t *testing.T) {
	const reponse = `{
	  "entities":[{"temp_id":"e1","entity_type":"person","display_name":"Paul",
	    "canonical_key":"person:paul","aliases":[],"resolved":true}],
	  "relations":[
	    {"source_temp_id":"e1","relation_type":"likes","target_temp_id":null,
	     "target_literal":"vert","observed_at":null,"valid_from":null,
	     "valid_until":null,"confidence":0.5,
	     "source_message_ids":["aaaaaaaa-0000-0000-0000-000000000001"]},
	    {"source_temp_id":"e1","relation_type":"aime","target_temp_id":null,
	     "target_literal":"bleu","observed_at":"pas une date","valid_from":null,
	     "valid_until":null,"confidence":0.5,
	     "source_message_ids":["aaaaaaaa-0000-0000-0000-000000000001"]}
	  ]}`

	in := testInput()
	e := NewExtractor(&fakeChat{reply: reponse}, config.Graph{})
	got, err := e.Extract(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Relations) != 2 {
		t.Fatalf("%d relations, want 2: une date absente ou illisible ne doit "+
			"pas faire perdre la relation", len(got.Relations))
	}
	for _, r := range got.Relations {
		if !r.ObservedAt.Equal(in.Message.CreatedAt) {
			t.Errorf("observed_at = %v, want la date du message %v",
				r.ObservedAt, in.Message.CreatedAt)
		}
	}
}

// TestCleanAliasesEcarteLesPronoms couvre un défaut trouvé par l'évaluation
// et pas par les tests: le modèle avait attribué l'alias « elles » à l'entité
// « tomates », et word_similarity('elles', 'Quelle est la capitale de la
// Mongolie ?') vaut 0,429, au-dessus du seuil de détection de graines. Une
// question sans aucun rapport semait donc le graphe, qui rendait douze
// candidats là où rien ne devait répondre.
func TestCleanAliasesEcarteLesPronoms(t *testing.T) {
	got := cleanAliases([]string{
		"elles", "Elles", "il", "la", "ce", "x", "",
		"Tomates cerises", "PP2X", "RH", "Tomates cerises",
	})

	want := []string{"Tomates cerises", "PP2X", "RH"}
	if len(got) != len(want) {
		t.Fatalf("alias = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("alias[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Les sigles courts sont de vrais alias et doivent survivre: le filtre écarte
// les pronoms, pas la brièveté.
func TestCleanAliasesGardeLesSigles(t *testing.T) {
	for _, a := range []string{"RH", "PP2X", "GSR", "SNCF"} {
		if got := cleanAliases([]string{a}); len(got) != 1 {
			t.Errorf("%q écarté à tort: %v", a, got)
		}
	}
}

// TestExtractNettoieLesAliasDuModele: le filtre doit être branché dans
// convert, pas seulement exister.
func TestExtractNettoieLesAliasDuModele(t *testing.T) {
	const reponse = `{
	  "entities":[{"temp_id":"e1","entity_type":"plant","display_name":"tomates",
	    "canonical_key":null,"aliases":["elles","Tomates cerises"],"resolved":true}],
	  "relations":[]}`

	e := NewExtractor(&fakeChat{reply: reponse}, config.Graph{})
	got, err := e.Extract(context.Background(), testInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entities) != 1 {
		t.Fatalf("%d entités, want 1", len(got.Entities))
	}
	for _, a := range got.Entities[0].Aliases {
		if strings.EqualFold(a, "elles") {
			t.Errorf("le pronom est resté dans les alias: %v", got.Entities[0].Aliases)
		}
	}
	if len(got.Entities[0].Aliases) != 1 {
		t.Errorf("alias = %v, want le seul vrai alias", got.Entities[0].Aliases)
	}
}
