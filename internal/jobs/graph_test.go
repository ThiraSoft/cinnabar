package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

// Les doubles réutilisent ceux du fichier handlers_test.go quand ils
// existent déjà (fakeMessages notamment). N'en créer un second que si la
// signature diffère.

type fakeGraphRepo struct {
	applied   []memory.GraphExtraction
	reevaled  []uuid.UUID
	single    []string
	applyErr  error
	candidate []memory.GraphEntity
}

func (f *fakeGraphRepo) Apply(ctx context.Context, e memory.GraphExtraction, sv []string) error {
	f.applied = append(f.applied, e)
	f.single = sv
	return f.applyErr
}
func (f *fakeGraphRepo) CandidateEntities(ctx context.Context, ws, text string, limit int) ([]memory.GraphEntity, error) {
	return f.candidate, nil
}
func (f *fakeGraphRepo) Reevaluate(ctx context.Context, id uuid.UUID, sv []string) error {
	f.reevaled = append(f.reevaled, id)
	f.single = sv
	return nil
}

type fakeExtractor struct {
	in  memory.GraphExtractorInput
	out memory.GraphExtraction
	err error
}

func (f *fakeExtractor) Extract(ctx context.Context, in memory.GraphExtractorInput) (memory.GraphExtraction, error) {
	f.in = in
	return f.out, f.err
}

func jobFor(id uuid.UUID, jobType string) *postgres.Job {
	payload, _ := json.Marshal(messagePayload{MessageID: id})
	return &postgres.Job{JobType: jobType, WorkspaceID: "ws1",
		ConversationID: "conv1", Payload: payload}
}

func TestGraphExtractHandlerPasseLeContexteEtLesCandidats(t *testing.T) {
	anchor := memory.Message{
		MessageID: uuid.New(), WorkspaceID: "ws1", ConversationID: "conv1",
		SequenceNumber: 5, Role: "user", AuthorKey: "user:alice",
		Content: "Les tomates de Paul sont vertes.", CreatedAt: time.Now().UTC(),
	}
	msgs := &fakeMessages{
		byID:   map[uuid.UUID]memory.Message{anchor.MessageID: anchor},
		around: []memory.Message{{MessageID: uuid.New(), SequenceNumber: 3}, anchor},
		parts:  []string{"user:alice", "agent:cuisine"},
		scope:  "participants",
	}
	repo := &fakeGraphRepo{candidate: []memory.GraphEntity{{CanonicalKey: "person:paul"}}}
	// L'extraction rendue porte une relation, et ce n'est pas décoratif: sans
	// elle, la boucle qui vérifie le scope plus bas ne s'exécute jamais et le
	// test reste vert même si le handler cessait de poser le scope. La revue
	// de la tâche 7 l'a démontré en supprimant la boucle de marquage.
	ext := &fakeExtractor{out: memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Relations: []memory.GraphRelation{{
			WorkspaceID: "ws1", RelationType: "has_observed_state",
			TargetLiteral: "vertes", ConversationID: "conv1",
		}},
	}}

	cfg := &config.Config{Graph: config.Graph{Enabled: true, ContextMessages: 4,
		SingleValuedRelations: []string{"has_observed_state"}}}
	h := GraphExtractHandler(cfg, msgs, repo, ext, nil)

	if err := h(context.Background(), jobFor(anchor.MessageID, "graph_extract")); err != nil {
		t.Fatalf("handler: %v", err)
	}

	if ext.in.Message.MessageID != anchor.MessageID {
		t.Error("le message ancre n'a pas été transmis à l'extracteur")
	}
	// Le message ancre ne doit pas figurer aussi dans le contexte: il y
	// serait compté deux fois dans le prompt. Le compte d'abord, pour la
	// raison déjà écrite vingt lignes plus bas à propos des relations: une
	// boucle sur une liste vide ne prouve rien, et celle-ci était restée sans
	// garde. La fixture rend deux messages autour de l'ancre, l'ancre
	// comprise: le handler doit donc en transmettre exactement un, ce qui
	// épingle du même coup qu'il transmet le contexte et qu'il en retire
	// l'ancre.
	if len(ext.in.Context) != 1 {
		t.Fatalf("%d messages de contexte transmis, want 1", len(ext.in.Context))
	}
	for _, m := range ext.in.Context {
		if m.MessageID == anchor.MessageID {
			t.Error("le message ancre figure aussi dans le contexte")
		}
	}
	if len(ext.in.Participants) != 2 {
		t.Errorf("%d participants transmis, want 2", len(ext.in.Participants))
	}
	if len(ext.in.CandidateEntities) != 1 {
		t.Error("les entités candidates n'ont pas été transmises")
	}
	if len(repo.applied) != 1 {
		t.Fatalf("%d extractions écrites, want 1", len(repo.applied))
	}
	if len(repo.single) != 1 || repo.single[0] != "has_observed_state" {
		t.Errorf("single_valued_relations = %v, want [has_observed_state]", repo.single)
	}
	// Le scope de la conversation doit atterrir sur les relations, pas le
	// scope par défaut de la configuration. Le compte est vérifié d'abord:
	// une boucle sur une liste vide ne prouve rien.
	if len(repo.applied[0].Relations) != 1 {
		t.Fatalf("%d relations écrites, want 1", len(repo.applied[0].Relations))
	}
	for _, r := range repo.applied[0].Relations {
		if r.Scope != "participants" {
			t.Errorf("scope de la relation = %q, want participants", r.Scope)
		}
	}
}

func TestGraphExtractHandlerIgnoreUnMessageSupprime(t *testing.T) {
	deleted := time.Now().UTC()
	id := uuid.New()
	msgs := &fakeMessages{byID: map[uuid.UUID]memory.Message{
		id: {MessageID: id, DeletedAt: &deleted},
	}}
	ext := &fakeExtractor{}
	repo := &fakeGraphRepo{}

	h := GraphExtractHandler(&config.Config{}, msgs, repo, ext, nil)
	if err := h(context.Background(), jobFor(id, "graph_extract")); err != nil {
		t.Fatalf("un message supprimé ne doit pas faire échouer le job: %v", err)
	}
	if ext.in.Message.MessageID != uuid.Nil {
		t.Error("l'extracteur a été appelé sur un message supprimé")
	}
	if len(repo.applied) != 0 {
		t.Error("une extraction a été écrite pour un message supprimé")
	}
}

func TestGraphExtractHandlerIgnoreUnMessageDisparu(t *testing.T) {
	msgs := &fakeMessages{byIDErr: memory.ErrNotFound}
	h := GraphExtractHandler(&config.Config{}, msgs, &fakeGraphRepo{}, &fakeExtractor{}, nil)
	if err := h(context.Background(), jobFor(uuid.New(), "graph_extract")); err != nil {
		t.Fatalf("un message disparu ne doit pas faire échouer le job: %v", err)
	}
}

func TestGraphExtractHandlerIgnoreUneConversationDisparue(t *testing.T) {
	id := uuid.New()
	msgs := &fakeMessages{
		byID: map[uuid.UUID]memory.Message{
			id: {MessageID: id, ConversationID: "conv1", SequenceNumber: 1},
		},
		scopeErr: memory.ErrNotFound,
	}
	ext := &fakeExtractor{}
	repo := &fakeGraphRepo{}

	h := GraphExtractHandler(&config.Config{}, msgs, repo, ext, nil)
	if err := h(context.Background(), jobFor(id, "graph_extract")); err != nil {
		t.Fatalf("une conversation disparue ne doit pas faire échouer le job: %v", err)
	}
	if len(repo.applied) != 0 {
		t.Error("une extraction a été écrite pour une conversation disparue")
	}
}

func TestGraphExtractHandlerPropageLEchecDeLExtracteur(t *testing.T) {
	id := uuid.New()
	msgs := &fakeMessages{byID: map[uuid.UUID]memory.Message{
		id: {MessageID: id, ConversationID: "conv1", SequenceNumber: 1},
	}}
	boom := errors.New("modèle indisponible")
	h := GraphExtractHandler(&config.Config{}, msgs, &fakeGraphRepo{},
		&fakeExtractor{err: boom}, nil)
	if err := h(context.Background(), jobFor(id, "graph_extract")); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want une enveloppe de %v: le job doit repartir en backoff", err, boom)
	}
}

func TestGraphReevalHandlerAppelleLeRepo(t *testing.T) {
	id := uuid.New()
	repo := &fakeGraphRepo{}
	cfg := &config.Config{Graph: config.Graph{
		SingleValuedRelations: []string{"has_observed_state"}}}

	h := GraphReevalHandler(cfg, repo)
	if err := h(context.Background(), jobFor(id, "graph_reeval")); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(repo.reevaled) != 1 || repo.reevaled[0] != id {
		t.Errorf("réévalués = %v, want [%s]", repo.reevaled, id)
	}
	if len(repo.single) != 1 {
		t.Errorf("single_valued_relations = %v, want la liste configurée", repo.single)
	}
}

// TestGraphHandlersPartagentLaMemeListe couvre la contrainte de câblage que
// le compilateur ne peut pas voir: Apply (via GraphExtractHandler) et
// Reevaluate (via GraphReevalHandler) doivent recevoir exactement la même
// liste single_valued_relations. Si l'une recevait une liste différente de
// l'autre, les fenêtres de validité resteraient figées sur des dates qui ne
// veulent plus rien dire, sans que rien ne le signale.
func TestGraphHandlersPartagentLaMemeListe(t *testing.T) {
	cfg := &config.Config{Graph: config.Graph{
		SingleValuedRelations: []string{"has_observed_state", "lives_in"}}}

	anchor := memory.Message{
		MessageID: uuid.New(), WorkspaceID: "ws1", ConversationID: "conv1",
		SequenceNumber: 1, CreatedAt: time.Now().UTC(),
	}
	extractRepo := &fakeGraphRepo{}
	msgs := &fakeMessages{byID: map[uuid.UUID]memory.Message{anchor.MessageID: anchor}}
	extractHandler := GraphExtractHandler(cfg, msgs, extractRepo, &fakeExtractor{}, nil)
	if err := extractHandler(context.Background(), jobFor(anchor.MessageID, "graph_extract")); err != nil {
		t.Fatalf("GraphExtractHandler: %v", err)
	}

	reevalRepo := &fakeGraphRepo{}
	reevalHandler := GraphReevalHandler(cfg, reevalRepo)
	if err := reevalHandler(context.Background(), jobFor(uuid.New(), "graph_reeval")); err != nil {
		t.Fatalf("GraphReevalHandler: %v", err)
	}

	if len(extractRepo.single) == 0 {
		t.Fatal("GraphExtractHandler n'a transmis aucune liste single_valued_relations à Apply")
	}
	if len(reevalRepo.single) == 0 {
		t.Fatal("GraphReevalHandler n'a transmis aucune liste single_valued_relations à Reevaluate")
	}
	if len(extractRepo.single) != len(reevalRepo.single) {
		t.Fatalf("listes de longueurs différentes: Apply=%v Reevaluate=%v",
			extractRepo.single, reevalRepo.single)
	}
	for i := range extractRepo.single {
		if extractRepo.single[i] != reevalRepo.single[i] {
			t.Fatalf("listes différentes: Apply=%v Reevaluate=%v",
				extractRepo.single, reevalRepo.single)
		}
	}
}

// TestGraphExtractHandlerEcarteLesMessagesSupprimesDuContexte couvre le
// constat mineur de la revue de la tâche 7: rien ne prouvait que le contexte
// transmis à l'extracteur écarte les messages supprimés. La protection réelle
// vient de MessageRepo.Around, qui filtre deleted_at en SQL, mais le handler
// refiltre pour ne pas dépendre d'une garantie qui vit dans un autre paquet,
// et ce refiltrage n'était couvert par rien.
func TestGraphExtractHandlerEcarteLesMessagesSupprimesDuContexte(t *testing.T) {
	deleted := time.Now().UTC()
	anchor := memory.Message{
		MessageID: uuid.New(), WorkspaceID: "ws1", ConversationID: "conv1",
		SequenceNumber: 5, Role: "user", AuthorKey: "user:alice",
		Content: "Les tomates de Paul sont vertes.", CreatedAt: time.Now().UTC(),
	}
	vivant := memory.Message{MessageID: uuid.New(), SequenceNumber: 3,
		ConversationID: "conv1", Content: "Paul jardine."}
	supprime := memory.Message{MessageID: uuid.New(), SequenceNumber: 4,
		ConversationID: "conv1", Content: "message supprimé", DeletedAt: &deleted}

	msgs := &fakeMessages{
		byID:   map[uuid.UUID]memory.Message{anchor.MessageID: anchor},
		around: []memory.Message{vivant, supprime, anchor},
		parts:  []string{"user:alice"},
		scope:  "participants",
	}
	ext := &fakeExtractor{out: memory.GraphExtraction{WorkspaceID: "ws1"}}

	cfg := &config.Config{Graph: config.Graph{Enabled: true, ContextMessages: 4}}
	h := GraphExtractHandler(cfg, msgs, &fakeGraphRepo{}, ext, nil)
	if err := h(context.Background(), jobFor(anchor.MessageID, "graph_extract")); err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(ext.in.Context) != 1 {
		t.Fatalf("%d messages de contexte, want 1: le supprimé et l'ancre sont "+
			"tous deux écartés, contexte = %+v", len(ext.in.Context), ext.in.Context)
	}
	if ext.in.Context[0].MessageID != vivant.MessageID {
		t.Errorf("contexte = %s, want le message vivant %s",
			ext.in.Context[0].MessageID, vivant.MessageID)
	}
}
