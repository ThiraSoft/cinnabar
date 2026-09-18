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

// fakeProgress sert les messages en attente par tranches de limit, comme
// MessageRepo.GraphPending, et retient les positions avancées.
type fakeProgress struct {
	msgs     []memory.Message
	through  int64
	advanced []int64
	err      error
	limits   []int
}

func (f *fakeProgress) GraphPending(_ context.Context, _ string, limit int) ([]memory.Message, bool, error) {
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return nil, false, f.err
	}
	var out []memory.Message
	for _, m := range f.msgs {
		if m.SequenceNumber > f.through {
			out = append(out, m)
		}
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

func (f *fakeProgress) AdvanceGraphProgress(_ context.Context, _ string, through int64) error {
	f.advanced = append(f.advanced, through)
	if through > f.through {
		f.through = through
	}
	return nil
}

type fakeScheduler struct{ calls int }

func (f *fakeScheduler) ScheduleGraphExtract(context.Context, string, string) error {
	f.calls++
	return nil
}

func msgSeq(seq int64, content string) memory.Message {
	return memory.Message{
		MessageID: uuid.New(), WorkspaceID: "ws1", ConversationID: "conv1",
		SequenceNumber: seq, Role: "user", AuthorKey: "user:alice",
		Content: content, CreatedAt: time.Now().UTC(),
	}
}

func convJob() *postgres.Job {
	return &postgres.Job{JobType: "graph_extract", WorkspaceID: "ws1",
		ConversationID: "conv1", Payload: []byte(`{}`)}
}

func TestGraphExtractHandlerSoumetUneFenetreEtAvance(t *testing.T) {
	pending := []memory.Message{
		msgSeq(5, "Les tomates de Paul sont vertes."),
		msgSeq(6, "Claire a repeint son vélo."),
	}
	prog := &fakeProgress{msgs: pending, through: 4}
	msgs := &fakeMessages{
		around: []memory.Message{msgSeq(3, "Paul jardine."), msgSeq(4, "Oui.")},
		parts:  []string{"user:alice", "agent:cuisine"},
		scope:  "participants",
	}
	repo := &fakeGraphRepo{candidate: []memory.GraphEntity{{CanonicalKey: "person:paul"}}}
	// L'extraction rendue porte une relation, et ce n'est pas décoratif: sans
	// elle, la boucle qui vérifie le scope plus bas ne s'exécute jamais et le
	// test reste vert même si le handler cessait de poser le scope.
	ext := &fakeExtractor{out: memory.GraphExtraction{
		WorkspaceID: "ws1", ConversationID: "conv1",
		Relations: []memory.GraphRelation{{
			WorkspaceID: "ws1", RelationType: "has_observed_state",
			TargetLiteral: "vertes", ConversationID: "conv1",
		}},
	}}
	sched := &fakeScheduler{}
	cfg := &config.Config{Graph: config.Graph{Enabled: true, ContextMessages: 4,
		ExtractionBatch: 12, SingleValuedRelations: []string{"has_observed_state"}}}

	h := GraphExtractHandler(cfg, msgs, prog, sched, repo, ext, nil)
	if err := h(context.Background(), convJob()); err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(ext.in.Messages) != 2 || ext.in.Messages[0].MessageID != pending[0].MessageID ||
		ext.in.Messages[1].MessageID != pending[1].MessageID {
		t.Fatalf("messages transmis = %+v, want la fenêtre en attente dans l'ordre", ext.in.Messages)
	}
	if len(ext.in.Context) != 2 {
		t.Errorf("%d messages de contexte, want 2", len(ext.in.Context))
	}
	if len(ext.in.Participants) != 2 || len(ext.in.CandidateEntities) != 1 {
		t.Error("participants ou entités candidates non transmis")
	}
	if len(prog.limits) != 1 || prog.limits[0] != 12 {
		t.Errorf("limites demandées = %v, want [12]", prog.limits)
	}
	if len(repo.applied) != 1 {
		t.Fatalf("%d extractions écrites, want 1", len(repo.applied))
	}
	if len(repo.applied[0].Relations) != 1 || repo.applied[0].Relations[0].Scope != "participants" {
		t.Errorf("relations = %+v, want une relation au scope de la conversation", repo.applied[0].Relations)
	}
	if len(prog.advanced) != 1 || prog.advanced[0] != 6 {
		t.Errorf("positions avancées = %v, want [6]", prog.advanced)
	}
	if sched.calls != 0 {
		t.Error("un job suivant a été posé alors que rien ne restait")
	}
}

// TestGraphExtractHandlerPoseLaSuite: au-delà d'une fenêtre, le reste part
// dans un job à lui, et la position n'avance que jusqu'à la fin de la
// fenêtre traitée.
func TestGraphExtractHandlerPoseLaSuite(t *testing.T) {
	var pending []memory.Message
	for i := int64(1); i <= 5; i++ {
		pending = append(pending, msgSeq(i, "message"))
	}
	prog := &fakeProgress{msgs: pending}
	sched := &fakeScheduler{}
	ext := &fakeExtractor{}
	cfg := &config.Config{Graph: config.Graph{ExtractionBatch: 2}}

	h := GraphExtractHandler(cfg, &fakeMessages{scope: "participants"}, prog, sched, &fakeGraphRepo{}, ext, nil)
	if err := h(context.Background(), convJob()); err != nil {
		t.Fatal(err)
	}
	if len(ext.in.Messages) != 2 {
		t.Errorf("%d messages soumis, want 2", len(ext.in.Messages))
	}
	if len(prog.advanced) != 1 || prog.advanced[0] != 2 {
		t.Errorf("positions avancées = %v, want [2]", prog.advanced)
	}
	if sched.calls != 1 {
		t.Errorf("%d jobs suivants posés, want 1", sched.calls)
	}
}

// TestGraphExtractHandlerSauteLesMessagesSupprimes: un message supprimé
// n'est pas soumis, mais la position le dépasse, sans quoi il resterait en
// tête de file pour toujours.
func TestGraphExtractHandlerSauteLesMessagesSupprimes(t *testing.T) {
	deleted := time.Now().UTC()
	vivant := msgSeq(1, "Paul jardine.")
	supprime := msgSeq(2, "message supprimé")
	supprime.DeletedAt = &deleted
	prog := &fakeProgress{msgs: []memory.Message{vivant, supprime}}
	ext := &fakeExtractor{}

	h := GraphExtractHandler(&config.Config{Graph: config.Graph{ExtractionBatch: 12}},
		&fakeMessages{scope: "participants"}, prog, &fakeScheduler{}, &fakeGraphRepo{}, ext, nil)
	if err := h(context.Background(), convJob()); err != nil {
		t.Fatal(err)
	}
	if len(ext.in.Messages) != 1 || ext.in.Messages[0].MessageID != vivant.MessageID {
		t.Errorf("messages soumis = %+v, want le seul vivant", ext.in.Messages)
	}
	if len(prog.advanced) != 1 || prog.advanced[0] != 2 {
		t.Errorf("positions avancées = %v, want [2]", prog.advanced)
	}
}

func TestGraphExtractHandlerNAppellePasLeModeleSurUneFenetreTouteSupprimee(t *testing.T) {
	deleted := time.Now().UTC()
	m := msgSeq(1, "supprimé")
	m.DeletedAt = &deleted
	prog := &fakeProgress{msgs: []memory.Message{m}}
	chat := &fakeExtractor{}
	repo := &fakeGraphRepo{}

	h := GraphExtractHandler(&config.Config{}, &fakeMessages{}, prog, &fakeScheduler{}, repo, chat, nil)
	if err := h(context.Background(), convJob()); err != nil {
		t.Fatal(err)
	}
	if chat.in.Messages != nil || len(repo.applied) != 0 {
		t.Error("l'extracteur a été appelé sur une fenêtre sans message vivant")
	}
	if len(prog.advanced) != 1 || prog.advanced[0] != 1 {
		t.Errorf("positions avancées = %v, want [1]", prog.advanced)
	}
}

// TestGraphExtractHandlerSansRienAFaire couvre le doublon laissé par une
// reprise: le job trouve la position à jour et ne fait rien.
func TestGraphExtractHandlerSansRienAFaire(t *testing.T) {
	prog := &fakeProgress{msgs: []memory.Message{msgSeq(1, "déjà extrait")}, through: 1}
	ext := &fakeExtractor{}
	h := GraphExtractHandler(&config.Config{}, &fakeMessages{}, prog, &fakeScheduler{}, &fakeGraphRepo{}, ext, nil)
	if err := h(context.Background(), convJob()); err != nil {
		t.Fatal(err)
	}
	if ext.in.Messages != nil || len(prog.advanced) != 0 {
		t.Error("un job sans message en attente a appelé le modèle ou bougé la position")
	}
}

func TestGraphExtractHandlerIgnoreUneConversationDisparue(t *testing.T) {
	for _, tc := range []struct {
		name string
		prog *fakeProgress
		msgs *fakeMessages
	}{
		{"avant lecture", &fakeProgress{err: memory.ErrNotFound}, &fakeMessages{}},
		{"pendant l'extraction", &fakeProgress{msgs: []memory.Message{msgSeq(1, "x")}},
			&fakeMessages{scopeErr: memory.ErrNotFound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeGraphRepo{}
			h := GraphExtractHandler(&config.Config{}, tc.msgs, tc.prog, &fakeScheduler{}, repo, &fakeExtractor{}, nil)
			if err := h(context.Background(), convJob()); err != nil {
				t.Fatalf("une conversation disparue ne doit pas faire échouer le job: %v", err)
			}
			if len(repo.applied) != 0 {
				t.Error("une extraction a été écrite pour une conversation disparue")
			}
		})
	}
}

// TestGraphExtractHandlerPropageLEchecDeLExtracteur: le job repart en
// backoff, et la position ne bouge pas, pour que la fenêtre soit resoumise.
func TestGraphExtractHandlerPropageLEchecDeLExtracteur(t *testing.T) {
	prog := &fakeProgress{msgs: []memory.Message{msgSeq(1, "x")}}
	boom := errors.New("modèle indisponible")
	h := GraphExtractHandler(&config.Config{}, &fakeMessages{scope: "participants"}, prog,
		&fakeScheduler{}, &fakeGraphRepo{}, &fakeExtractor{err: boom}, nil)
	if err := h(context.Background(), convJob()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want une enveloppe de %v: le job doit repartir en backoff", err, boom)
	}
	if len(prog.advanced) != 0 {
		t.Errorf("position avancée malgré l'échec: %v", prog.advanced)
	}
}

// TestGraphExtractHandlerEcarteLesMessagesSupprimesDuContexte: la protection
// réelle vient de MessageRepo.Around, qui filtre deleted_at en SQL, mais le
// handler refiltre pour ne pas dépendre d'une garantie qui vit dans un autre
// paquet.
func TestGraphExtractHandlerEcarteLesMessagesSupprimesDuContexte(t *testing.T) {
	deleted := time.Now().UTC()
	vivant := msgSeq(3, "Paul jardine.")
	supprime := msgSeq(4, "message supprimé")
	supprime.DeletedAt = &deleted
	prog := &fakeProgress{msgs: []memory.Message{msgSeq(5, "Les tomates sont vertes.")}, through: 4}
	msgs := &fakeMessages{around: []memory.Message{vivant, supprime}, scope: "participants"}
	ext := &fakeExtractor{out: memory.GraphExtraction{WorkspaceID: "ws1"}}

	cfg := &config.Config{Graph: config.Graph{ContextMessages: 4}}
	h := GraphExtractHandler(cfg, msgs, prog, &fakeScheduler{}, &fakeGraphRepo{}, ext, nil)
	if err := h(context.Background(), convJob()); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(ext.in.Context) != 1 || ext.in.Context[0].MessageID != vivant.MessageID {
		t.Errorf("contexte = %+v, want le seul message vivant", ext.in.Context)
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

	extractRepo := &fakeGraphRepo{}
	prog := &fakeProgress{msgs: []memory.Message{msgSeq(1, "x")}}
	extractHandler := GraphExtractHandler(cfg, &fakeMessages{scope: "participants"}, prog,
		&fakeScheduler{}, extractRepo, &fakeExtractor{}, nil)
	if err := extractHandler(context.Background(), convJob()); err != nil {
		t.Fatalf("GraphExtractHandler: %v", err)
	}

	reevalRepo := &fakeGraphRepo{}
	reevalHandler := GraphReevalHandler(cfg, reevalRepo)
	if err := reevalHandler(context.Background(), jobFor(uuid.New(), "graph_reeval")); err != nil {
		t.Fatalf("GraphReevalHandler: %v", err)
	}

	if len(extractRepo.single) == 0 || len(reevalRepo.single) == 0 {
		t.Fatal("un des handlers n'a transmis aucune liste single_valued_relations")
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

// TestGraphExtractHandlerSauteLaFenetreALaDerniereTentative: une fenêtre
// qui échoue jusqu'à la lettre morte ne doit pas bloquer le reste de la
// conversation.
func TestGraphExtractHandlerSauteLaFenetreALaDerniereTentative(t *testing.T) {
	boom := errors.New("réponse illisible")
	cfg := &config.Config{Graph: config.Graph{ExtractionBatch: 1}, Jobs: config.Jobs{RetryLimit: 3}}
	for _, tc := range []struct {
		attempts     int
		wantAdvanced bool
	}{{0, false}, {1, false}, {2, true}} {
		prog := &fakeProgress{msgs: []memory.Message{msgSeq(1, "x"), msgSeq(2, "y")}}
		sched := &fakeScheduler{}
		h := GraphExtractHandler(cfg, &fakeMessages{scope: "participants"}, prog, sched,
			&fakeGraphRepo{}, &fakeExtractor{err: boom}, nil)
		j := convJob()
		j.Attempts = tc.attempts
		if err := h(context.Background(), j); !errors.Is(err, boom) {
			t.Fatalf("tentative %d: err = %v, want %v pour que la cause reste en lettre morte", tc.attempts, err, boom)
		}
		if advanced := len(prog.advanced) == 1 && prog.advanced[0] == 1; advanced != tc.wantAdvanced {
			t.Errorf("tentative %d: positions = %v, want avancée=%v", tc.attempts, prog.advanced, tc.wantAdvanced)
		}
		if tc.wantAdvanced && sched.calls != 1 {
			t.Errorf("tentative %d: la suite de la conversation n'a pas été posée", tc.attempts)
		}
	}
}
