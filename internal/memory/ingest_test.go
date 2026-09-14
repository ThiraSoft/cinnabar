package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

type fakeMessageRepo struct {
	res AppendResult
	err error
	// gotJobs retient les jobs que le domaine a demandé de poser dans la
	// transaction d'écriture, pour que les tests puissent vérifier qu'ils y
	// entrent bien plutôt que d'être posés après coup.
	gotJobs []AppendJob
}

func (f *fakeMessageRepo) Append(_ context.Context, _ AppendInput, _ int,
	jobs []AppendJob) (AppendResult, error) {
	f.gotJobs = jobs
	return f.res, f.err
}
func (f *fakeMessageRepo) ByID(context.Context, uuid.UUID) (Message, error) {
	return Message{}, nil
}
func (f *fakeMessageRepo) Around(context.Context, string, int64, int64) ([]Message, error) {
	return nil, nil
}
func (f *fakeMessageRepo) EditAndDeactivate(context.Context, uuid.UUID, string) (Message, []uuid.UUID, error) {
	return Message{}, nil, nil
}
func (f *fakeMessageRepo) SoftDeleteAndDeactivate(context.Context, uuid.UUID) (Message, []uuid.UUID, error) {
	return Message{}, nil, nil
}
func (f *fakeMessageRepo) Participants(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *fakeMessageRepo) ConversationScope(context.Context, string) (string, error) {
	return "", nil
}
func (f *fakeMessageRepo) MessageOwner(context.Context, uuid.UUID) (string, string, error) {
	return "", "", nil
}

type fakeUnitRepo struct {
	upserts int
	err     error
}

func (f *fakeUnitRepo) Upsert(context.Context, Unit, []float32) error {
	f.upserts++
	return f.err
}

type fakeEmbedder struct {
	calls int
	err   error
}

func (f *fakeEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}
func (f *fakeEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := f.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}
func (f *fakeEmbedder) Model() string { return "fake-model" }

// fakeQueue enregistre le type de chaque job posé, et son payload au même
// indice (payloads[i] correspond à enqueued[i]): certains tests ont besoin de
// savoir non seulement combien de jobs embed ont été posés, mais lesquels
// (voir TestDeleteMessageRebuildsLiveNeighboursNotDeletedAnchor).
type fakeQueue struct {
	enqueued []string
	payloads []any
}

func (f *fakeQueue) Enqueue(ctx context.Context, jobType, ws, conv string, payload any) error {
	f.enqueued = append(f.enqueued, jobType)
	f.payloads = append(f.payloads, payload)
	return nil
}

func newTestIngester(t *testing.T) (*Ingester, *fakeUnitRepo, *fakeEmbedder, *fakeQueue) {
	t.Helper()
	cfg := &config.Config{
		Indexing: testIndexing(),
		Graph:    config.Graph{Enabled: true},
	}
	anchor := msg(1, "user:paul", "user", "mes tomates sont vertes")
	msgs := &fakeMessageRepo{res: AppendResult{
		Message: anchor, Scope: "participants",
		Participants: []string{"user:paul"},
	}}
	units := &fakeUnitRepo{}
	emb := &fakeEmbedder{}
	q := &fakeQueue{}
	return NewIngester(cfg, msgs, units, emb, q), units, emb, q
}

func newIngesterWith(msgs MessageRepo, units UnitRepo, q JobQueue) *Ingester {
	cfg := &config.Config{
		Indexing: testIndexing(),
		Graph:    config.Graph{Enabled: true},
		Access:   config.Access{DefaultScope: "participants"},
	}
	return NewIngester(cfg, msgs, units, &fakeEmbedder{}, q)
}

func TestIngestEventualDefersEmbedding(t *testing.T) {
	ing, units, emb, q := newTestIngester(t)

	res, err := ing.Ingest(context.Background(), AppendInput{Role: "user"}, "eventual")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "stored" {
		t.Errorf("status = %q, want stored", res.Status)
	}
	if emb.calls != 0 {
		t.Error("le mode eventual ne doit pas appeler l'embedder sur le chemin critique")
	}
	if units.upserts != 0 {
		t.Error("le mode eventual ne doit pas écrire d'unité tout de suite")
	}
	// Les deux jobs entrent dans la transaction d'Append, jamais après elle:
	// c'est ce que la file hors transaction, restée vide, atteste ici.
	jobs := ing.msgs.(*fakeMessageRepo).gotJobs
	if len(jobs) != 2 || jobs[0].Type != "embed" || jobs[1].Type != "graph_extract" {
		t.Errorf("jobs = %v, want [embed graph_extract]", jobs)
	}
	if len(q.enqueued) != 0 {
		t.Errorf("jobs posés hors transaction = %v, want aucun", q.enqueued)
	}
}

func TestIngestSearchableIndexesInline(t *testing.T) {
	ing, units, emb, q := newTestIngester(t)

	res, err := ing.Ingest(context.Background(), AppendInput{}, "searchable")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "searchable" {
		t.Errorf("status = %q, want searchable", res.Status)
	}
	if emb.calls != 1 {
		t.Errorf("embedder appelé %d fois, want 1", emb.calls)
	}
	if units.upserts != 1 {
		t.Errorf("%d unités écrites, want 1", units.upserts)
	}
	if len(res.UnitIDs) != 1 {
		t.Errorf("UnitIDs = %v, want 1 entrée", res.UnitIDs)
	}
	// Le graphe reste asynchrone même en mode searchable, et son job entre
	// lui aussi dans la transaction d'écriture.
	jobs := ing.msgs.(*fakeMessageRepo).gotJobs
	if len(jobs) != 1 || jobs[0].Type != "graph_extract" {
		t.Errorf("jobs = %v, want [graph_extract]", jobs)
	}
	if len(q.enqueued) != 0 {
		t.Errorf("jobs posés hors transaction = %v, want aucun", q.enqueued)
	}
	if res.EmbedScheduled {
		t.Error("une unité écrite en ligne n'a aucun rattrapage à programmer")
	}
	if res.GraphStatus != "pending" {
		t.Errorf("graph_status = %q", res.GraphStatus)
	}
}

func TestIngestSearchableDegradesOnEmbedderFailure(t *testing.T) {
	ing, units, emb, q := newTestIngester(t)
	emb.err = errors.New("ollama down")

	res, err := ing.Ingest(context.Background(), AppendInput{}, "searchable")
	if err != nil {
		t.Fatalf("le message est durable, Ingest ne doit pas échouer: %v", err)
	}
	if res.Status != "stored" {
		t.Errorf("status = %q, want stored", res.Status)
	}
	if res.IndexingError == "" {
		t.Error("IndexingError doit expliquer que l'unité n'est pas indexée")
	}
	if units.upserts != 0 {
		t.Error("aucune unité ne doit être écrite sans embedding")
	}
	// Un job de rattrapage doit avoir été posé.
	var hasEmbed bool
	for _, j := range q.enqueued {
		if j == "embed" {
			hasEmbed = true
		}
	}
	if !hasEmbed {
		t.Error("un job embed de rattrapage doit être posé")
	}
	if !res.EmbedScheduled {
		t.Error("EmbedScheduled doit dire que le rattrapage est programmé")
	}
}

func TestIngestReplayedMessageDoesNotReindex(t *testing.T) {
	ing, units, emb, _ := newTestIngester(t)
	ing.msgs.(*fakeMessageRepo).res.Replayed = true

	res, err := ing.Ingest(context.Background(), AppendInput{}, "searchable")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed {
		t.Error("Replayed doit être remonté")
	}
	if emb.calls != 0 || units.upserts != 0 {
		t.Error("un rejeu ne doit ni réencoder ni réécrire")
	}
}

func TestIngestSkipsNonIndexedRole(t *testing.T) {
	ing, units, emb, q := newTestIngester(t)
	ing.msgs.(*fakeMessageRepo).res.Message = msg(1, "system", "system", "consigne")

	res, err := ing.Ingest(context.Background(), AppendInput{}, "searchable")
	if err != nil {
		t.Fatal(err)
	}
	if emb.calls != 0 || units.upserts != 0 {
		t.Error("un message system ne doit pas être indexé")
	}
	if len(res.UnitIDs) != 0 {
		t.Errorf("UnitIDs = %v, want vide", res.UnitIDs)
	}
	// Le graphe est quand même sollicité: un message system peut porter des
	// faits, c'est l'indexation vectorielle qui l'exclut.
	jobs := ing.msgs.(*fakeMessageRepo).gotJobs
	if len(jobs) != 1 || jobs[0].Type != "graph_extract" {
		t.Errorf("jobs = %v", jobs)
	}
	if len(q.enqueued) != 0 {
		t.Errorf("jobs posés hors transaction = %v, want aucun", q.enqueued)
	}
}

// TestIngestEventualPostsTheEmbedJobInTheAppendTransaction couvre la perte
// d'indexation définitive que la section 6.1 de la spec ferme en posant les
// jobs à l'étape 6 de la transaction unique: tant que l'enqueue se faisait
// après le commit, sur une autre connexion, une panne entre les deux
// laissait un message durable que plus rien ne viendrait jamais embedder.
// Ni le reaper (qui ne récupère que les jobs déjà en table) ni la réponse
// (qui annonce "stored" comme d'habitude) ne le signalaient.
func TestIngestEventualPostsTheEmbedJobInTheAppendTransaction(t *testing.T) {
	ing, _, _, q := newTestIngester(t)

	res, err := ing.Ingest(context.Background(),
		AppendInput{Role: "user"}, "eventual")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 0 {
		t.Errorf("jobs posés hors transaction = %v, want aucun: ils doivent "+
			"entrer dans la transaction d'Append", q.enqueued)
	}
	got := ing.msgs.(*fakeMessageRepo).gotJobs
	if len(got) != 2 || got[0].Type != "embed" || got[1].Type != "graph_extract" {
		t.Errorf("jobs passés à Append = %v, want [embed graph_extract]", got)
	}
	if !res.EmbedScheduled {
		t.Error("EmbedScheduled doit dire au client que l'indexation est " +
			"seulement différée, pas perdue")
	}
}
