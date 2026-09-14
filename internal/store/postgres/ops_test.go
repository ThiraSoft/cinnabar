package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestOpsHealthOnLivePool(t *testing.T) {
	pool := newTestPool(t)
	ops := NewOps(pool, NewJobRepo(pool))
	if err := ops.Health(context.Background()); err != nil {
		t.Errorf("Health sur un pool vivant: %v", err)
	}
}

// TestOpsStatsExposesQueueDepth vérifie la forme de memory.Stats plutôt que
// celle d'un map[string]any: Ops.Stats rend un type nommé (voir le
// commentaire de memory.Stats), donc l'assertion porte sur ses champs, avec
// un aller-retour JSON pour verrouiller les noms exposés à /debug/stats.
func TestOpsStatsExposesQueueDepth(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	jobs := NewJobRepo(pool)
	ops := NewOps(pool, jobs)

	if err := jobs.Enqueue(ctx, "embed", "ws1", "c", map[string]any{}); err != nil {
		t.Fatal(err)
	}

	stats, err := ops.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	depth, ok := stats.QueueDepth["embed"]
	if !ok {
		t.Fatalf("queue_depth[embed] absent: %#v", stats.QueueDepth)
	}
	if depth.Pending != 1 {
		t.Errorf("queue_depth[embed].Pending = %d, want 1", depth.Pending)
	}

	raw, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"queue_depth", "dead_jobs", "memory_units", "messages",
		"conversations", "db_pool",
	} {
		if _, ok := asMap[field]; !ok {
			t.Errorf("%s doit être exposé", field)
		}
	}
}

func TestVerifyEmbeddingDimensionRejectsMismatch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// Un embedder qui rend 3 dimensions face à une colonne VECTOR(768) doit
	// empêcher le démarrage plutôt que produire des erreurs à chaque écriture.
	err := VerifyEmbeddingDimension(ctx, pool, &shortEmbedder{dim: 3}, 768)
	if err == nil {
		t.Fatal("une dimension incohérente doit empêcher le démarrage")
	}
	if !strings.Contains(err.Error(), "768") {
		t.Errorf("le message doit nommer la dimension de la colonne: %v", err)
	}
}

// TestVerifyEmbeddingDimensionRejectsConfigModelAgreementOnWrongColumn
// couvre le cas où la config et le modèle s'accordent honnêtement entre eux
// (un opérateur qui migre vers un nouveau modèle a mis à jour
// embedding.dimensions en conséquence) mais ignorent la colonne réelle: ce
// n'est pas parce que la config ne se contredit pas elle-même qu'elle est
// juste, et comparer seulement le modèle à la config laisserait démarrer un
// service qui échouera à la première écriture.
func TestVerifyEmbeddingDimensionRejectsConfigModelAgreementOnWrongColumn(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// memory_units.embedding est VECTOR(768) (voir 001_init.sql). Le modèle
	// et la config s'accordent tous deux sur 512: rien ne se contredit du
	// côté de la configuration, et pourtant la colonne, elle, n'a pas suivi.
	err := VerifyEmbeddingDimension(ctx, pool, &shortEmbedder{dim: 512}, 512)
	if err == nil {
		t.Fatal("un accord config/modèle qui ignore la colonne réelle doit aussi empêcher le démarrage")
	}
	if !strings.Contains(err.Error(), "768") {
		t.Errorf("le message doit nommer la dimension réelle de la colonne: %v", err)
	}
}

type shortEmbedder struct{ dim int }

func (s *shortEmbedder) Model() string { return "short" }
func (s *shortEmbedder) EmbedQuery(ctx context.Context, t string) ([]float32, error) {
	v, err := s.Embed(ctx, []string{t})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}
func (s *shortEmbedder) Embed(_ context.Context, in []string) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = make([]float32, s.dim)
	}
	return out, nil
}

func TestConversationRepoDeclareSetsScopeAndParticipants(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewConversationRepo(pool)

	err := repo.Declare(ctx, "conv_1", "ws1", "workspace",
		[]string{"user:paul", "agent:cuisine"})
	if err != nil {
		t.Fatal(err)
	}

	var scope string
	if err := pool.QueryRow(ctx,
		`SELECT scope FROM conversations WHERE conversation_id = 'conv_1'`,
	).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != "workspace" {
		t.Errorf("scope = %q", scope)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversation_participants WHERE conversation_id = 'conv_1'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d participants, want 2", n)
	}

	// Un second appel doit être idempotent et pouvoir changer le scope.
	if err := repo.Declare(ctx, "conv_1", "ws1", "participants", nil); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT scope FROM conversations WHERE conversation_id = 'conv_1'`,
	).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != "participants" {
		t.Errorf("scope après second Declare = %q, want participants", scope)
	}
}

// unreachableEmbedder simule un embedder joignable par le réseau mais en
// panne (Ollama arrêté, DNS qui ne résout plus, timeout).
type unreachableEmbedder struct{}

func (unreachableEmbedder) Model() string { return "unreachable" }
func (unreachableEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return nil, errors.New("dial tcp 127.0.0.1:11434: connection refused")
}
func (unreachableEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("dial tcp 127.0.0.1:11434: connection refused")
}

// TestVerifyEmbeddingDimensionSeparatesUnreachableFromMismatch : la spec
// (4.5) demande de refuser le démarrage sur un écart de dimension, pas sur
// une panne de l'embedder, dont la posture (5.5, 8.2, critère 10) est de
// dégrader la recherche et l'ingestion sans rendre le service indisponible.
// Confondre les deux voulait dire qu'une panne d'Ollama empêchait tout
// réplica de redémarrer.
func TestVerifyEmbeddingDimensionSeparatesUnreachableFromMismatch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	unreachable := VerifyEmbeddingDimension(ctx, pool, unreachableEmbedder{}, 768)
	if !errors.Is(unreachable, ErrEmbeddingProbeUnavailable) {
		t.Errorf("sonde injoignable: err = %v, want ErrEmbeddingProbeUnavailable",
			unreachable)
	}

	mismatch := VerifyEmbeddingDimension(ctx, pool, &shortEmbedder{dim: 3}, 768)
	if mismatch == nil {
		t.Fatal("une dimension incohérente doit toujours empêcher le démarrage")
	}
	if errors.Is(mismatch, ErrEmbeddingProbeUnavailable) {
		t.Errorf("un écart de dimension n'est pas une panne de sonde: %v", mismatch)
	}
}
