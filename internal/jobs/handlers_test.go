package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

// newTestPool crée une base Postgres vierge et migrée, dédiée à ce test.
// Réplique volontairement le helper interne du paquet postgres (non exporté,
// donc inaccessible d'ici): ce paquet a besoin d'un pool réel pour prouver
// que EmbedHandler écrit le scope de la conversation, pas celui de la
// configuration, ce qu'aucun repo en mémoire ne peut trancher. Ignoré sans
// POSTGRES_TEST_DSN, comme les tests d'intégration du paquet postgres.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("POSTGRES_TEST_DSN")
	if base == "" {
		t.Skip("POSTGRES_TEST_DSN absent, test d'intégration ignoré")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("connexion admin: %v", err)
	}
	defer admin.Close()

	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	dbName := "cinnabar_test_" + hex.EncodeToString(buf)
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", dbName)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		a, err := pgxpool.New(cctx, base)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(cctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", dbName))
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName

	pool, err := postgres.Open(ctx, config.Database{DSN: u.String(), MaxConns: 4})
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// fakeEmbedder rend un vecteur constant, seule la trace des appels compte
// dans ce test.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, in []string) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = make([]float32, 768)
	}
	return out, nil
}
func (f fakeEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := f.Embed(ctx, []string{text})
	return vecs[0], err
}
func (fakeEmbedder) Model() string { return "fake-model" }

func testIndexingConfig() *config.Config {
	return &config.Config{
		Indexing: config.Indexing{
			Strategy: "contextualized_message", PreviousMessages: 2,
			MaxChars: 1600, CharsPerToken: 4,
			IndexUserMessages: true, IndexAgentMessages: true, Version: 1,
		},
		// DefaultScope diffère volontairement de "workspace", le scope réel
		// déclaré pour la conversation du test: si EmbedHandler retombait
		// sur cfg.Access.DefaultScope, l'unité écrite porterait
		// "participants" au lieu de "workspace" et le test échouerait.
		Access: config.Access{DefaultScope: "participants"},
	}
}

// TestEmbedHandlerUsesConversationScope prouve que le worker d'embedding
// indexe avec le scope réel de la conversation et non avec le scope par
// défaut de la configuration: une conversation déclarée en scope
// "workspace" doit produire une unité "workspace", même si la conversation
// est indexée de façon différée (mode eventual, via un job). Sans cela, une
// même conversation produirait des unités visibles à des audiences
// différentes selon le mode de cohérence choisi par l'appelant.
func TestEmbedHandlerUsesConversationScope(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	msgs := postgres.NewMessageRepo(pool)
	units := postgres.NewUnitRepo(pool)
	jobRepo := postgres.NewJobRepo(pool)

	// Conversation déclarée d'avance avec un scope non par défaut.
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id, scope)
		VALUES ('conv_1', 'ws1', 'workspace')`); err != nil {
		t.Fatal(err)
	}

	res, err := msgs.Append(ctx, memory.AppendInput{
		WorkspaceID: "ws1", ConversationID: "conv_1",
		// DefaultScope n'est utilisé que si la conversation n'existe pas
		// encore, ce qui n'est pas le cas ici (elle est créée juste au-dessus
		// avec le scope "workspace"): Append ne l'écrase pas, voir
		// TestAppendRespectsExplicitConversationScope. On lui donne quand
		// même une valeur valide pour satisfaire la contrainte CHECK que
		// Postgres évalue avant de résoudre le conflit ON CONFLICT DO
		// NOTHING.
		DefaultScope: "participants",
		AuthorKey:    "user:paul", Role: "user", Content: "mes tomates sont vertes",
		CreatedAt: time.Now().UTC(),
	}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := jobRepo.Enqueue(ctx, "embed", "ws1", "conv_1",
		map[string]any{"message_id": res.Message.MessageID}); err != nil {
		t.Fatal(err)
	}

	j, err := jobRepo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("le job embed posé doit être réclamable")
	}

	handler := EmbedHandler(testIndexingConfig(), msgs, units, fakeEmbedder{})
	if err := handler(ctx, j); err != nil {
		t.Fatalf("EmbedHandler: %v", err)
	}

	var scope string
	if err := pool.QueryRow(ctx,
		`SELECT scope FROM memory_units WHERE anchor_message_id = $1`,
		res.Message.MessageID).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != "workspace" {
		t.Errorf("scope = %q, want workspace (le scope réel de la conversation, pas le défaut de configuration)", scope)
	}
}

// fakeMsgRepoForVanishedConversation simule un ByID qui réussit mais un
// ConversationScope qui rapporte une conversation disparue: ça n'arrive pas
// aujourd'hui contre un vrai Postgres (la clé étrangère de messages vers
// conversations est ON DELETE CASCADE, donc supprimer la conversation
// supprimerait le message avec, et ByID échouerait avant ConversationScope),
// mais EmbedHandler doit quand même traiter cette disparition proprement si
// elle devient un jour atteignable.
type fakeMsgRepoForVanishedConversation struct{ anchor memory.Message }

func (f *fakeMsgRepoForVanishedConversation) Append(context.Context, memory.AppendInput, int, []memory.AppendJob) (memory.AppendResult, error) {
	return memory.AppendResult{}, nil
}
func (f *fakeMsgRepoForVanishedConversation) ByID(context.Context, uuid.UUID) (memory.Message, error) {
	return f.anchor, nil
}
func (f *fakeMsgRepoForVanishedConversation) Around(context.Context, string, int64, int64) ([]memory.Message, error) {
	return nil, nil
}
func (f *fakeMsgRepoForVanishedConversation) EditAndDeactivate(context.Context, uuid.UUID, string) (memory.Message, []uuid.UUID, error) {
	return memory.Message{}, nil, nil
}
func (f *fakeMsgRepoForVanishedConversation) SoftDeleteAndDeactivate(context.Context, uuid.UUID) (memory.Message, []uuid.UUID, error) {
	return memory.Message{}, nil, nil
}
func (f *fakeMsgRepoForVanishedConversation) Participants(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *fakeMsgRepoForVanishedConversation) ConversationScope(context.Context, string) (string, error) {
	return "", fmt.Errorf("conversation scope: %w", memory.ErrNotFound)
}
func (f *fakeMsgRepoForVanishedConversation) MessageOwner(context.Context, uuid.UUID) (string, string, error) {
	return f.anchor.WorkspaceID, f.anchor.AuthorKey, nil
}

// fakeMessages est le double de memory.MessageRepo partagé par les tests de
// ce paquet qui n'ont pas besoin d'un vrai Postgres (contrairement à
// TestEmbedHandlerUsesConversationScope plus haut). byID et byIDErr pilotent
// ByID, around Around, parts Participants, et scope/scopeErr
// ConversationScope. Les méthodes non utilisées par les tests de graphe
// rendent des valeurs neutres.
type fakeMessages struct {
	byID     map[uuid.UUID]memory.Message
	byIDErr  error
	around   []memory.Message
	parts    []string
	scope    string
	scopeErr error
}

func (f *fakeMessages) Append(context.Context, memory.AppendInput, int, []memory.AppendJob) (memory.AppendResult, error) {
	return memory.AppendResult{}, nil
}
func (f *fakeMessages) ByID(_ context.Context, id uuid.UUID) (memory.Message, error) {
	if f.byIDErr != nil {
		return memory.Message{}, f.byIDErr
	}
	return f.byID[id], nil
}
func (f *fakeMessages) Around(context.Context, string, int64, int64) ([]memory.Message, error) {
	return f.around, nil
}
func (f *fakeMessages) EditAndDeactivate(context.Context, uuid.UUID, string) (memory.Message, []uuid.UUID, error) {
	return memory.Message{}, nil, nil
}
func (f *fakeMessages) SoftDeleteAndDeactivate(context.Context, uuid.UUID) (memory.Message, []uuid.UUID, error) {
	return memory.Message{}, nil, nil
}
func (f *fakeMessages) Participants(context.Context, string) ([]string, error) {
	return f.parts, nil
}
func (f *fakeMessages) ConversationScope(context.Context, string) (string, error) {
	if f.scopeErr != nil {
		return "", f.scopeErr
	}
	return f.scope, nil
}
func (f *fakeMessages) MessageOwner(context.Context, uuid.UUID) (string, string, error) {
	return "", "", nil
}

type recordingUnitRepo struct{ upserts int }

func (r *recordingUnitRepo) Upsert(context.Context, memory.Unit, []float32) error {
	r.upserts++
	return nil
}

// TestEmbedHandlerTreatsVanishedConversationAsSuccess couvre la
// cohérence des deux disparitions que le handler doit traiter de la même
// façon: un message soft-deleted (déjà couvert plus haut, anchor.DeletedAt)
// et une conversation absente ne sont pas des raisons de retenter le job,
// puisqu'il n'y a de toute façon plus rien à indexer dans les deux cas.
func TestEmbedHandlerTreatsVanishedConversationAsSuccess(t *testing.T) {
	anchor := memory.Message{
		MessageID: uuid.New(), ConversationID: "conv_gone", WorkspaceID: "ws1",
		SequenceNumber: 1, AuthorKey: "user:paul", Role: "user",
		Content: "mes tomates sont vertes", CreatedAt: time.Now().UTC(),
	}
	msgs := &fakeMsgRepoForVanishedConversation{anchor: anchor}
	units := &recordingUnitRepo{}
	handler := EmbedHandler(testIndexingConfig(), msgs, units, fakeEmbedder{})

	payload, err := json.Marshal(map[string]any{"message_id": anchor.MessageID})
	if err != nil {
		t.Fatal(err)
	}
	j := &postgres.Job{JobID: 1, JobType: "embed", Payload: payload}

	if err := handler(context.Background(), j); err != nil {
		t.Fatalf("EmbedHandler = %v, want nil: une conversation disparue n'a plus rien à indexer", err)
	}
	if units.upserts != 0 {
		t.Error("aucune unité ne doit être écrite pour une conversation disparue")
	}
}
