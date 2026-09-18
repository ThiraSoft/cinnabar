package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

func graphJob(debounce time.Duration, flush int) []memory.AppendJob {
	return []memory.AppendJob{{Type: "graph_extract", Debounce: debounce, FlushAfter: flush}}
}

// pendingGraphJobs rend le nombre de jobs graph_extract en attente de la
// conversation et, s'il y en a, si le plus ancien est déjà éligible.
func pendingGraphJobs(t *testing.T, ctx context.Context, repo *MessageRepo, conv string) (int, bool) {
	t.Helper()
	var n int
	var due bool
	if err := repo.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(bool_and(run_after <= now()), false) FROM jobs
		WHERE job_type = 'graph_extract' AND status = 'pending' AND conversation_id = $1`,
		conv).Scan(&n, &due); err != nil {
		t.Fatal(err)
	}
	return n, due
}

// TestAppendCoalesceLeJobDExtraction: une conversation n'a qu'un job
// d'extraction en attente, repoussé à chaque message, et il devient
// éligible tout de suite dès que FlushAfter messages attendent.
func TestAppendCoalesceLeJobDExtraction(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	for i := 0; i < 2; i++ {
		if _, err := repo.Append(ctx, appendInput("conv_g", "user:paul", "user", "salut", ""),
			2, graphJob(time.Hour, 3)); err != nil {
			t.Fatal(err)
		}
	}
	if n, due := pendingGraphJobs(t, ctx, repo, "conv_g"); n != 1 || due {
		t.Fatalf("après 2 messages: %d jobs, éligible=%v, want 1 job repoussé", n, due)
	}
	if _, err := repo.Append(ctx, appendInput("conv_g", "user:paul", "user", "encore", ""),
		2, graphJob(time.Hour, 3)); err != nil {
		t.Fatal(err)
	}
	if n, due := pendingGraphJobs(t, ctx, repo, "conv_g"); n != 1 || !due {
		t.Fatalf("après 3 messages: %d jobs, éligible=%v, want 1 job éligible", n, due)
	}
}

func TestGraphPendingEtAvancement(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewMessageRepo(pool)

	var ids []memory.Message
	for i := 0; i < 5; i++ {
		res, err := repo.Append(ctx, appendInput("conv_p", "user:paul", "user", "m", ""), 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Message)
	}
	if _, _, err := repo.SoftDeleteAndDeactivate(ctx, ids[1].MessageID); err != nil {
		t.Fatal(err)
	}

	got, more, err := repo.GraphPending(ctx, "conv_p", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !more || got[0].SequenceNumber != 1 || got[2].SequenceNumber != 3 {
		t.Fatalf("fenêtre = %d messages, more=%v, want les séquences 1 à 3 et une suite", len(got), more)
	}
	if got[1].DeletedAt == nil {
		t.Error("le message supprimé doit être rendu, marqué, pour que la position le dépasse")
	}

	if err := repo.AdvanceGraphProgress(ctx, "conv_p", 3); err != nil {
		t.Fatal(err)
	}
	// Un job en retard ne fait jamais reculer la position.
	if err := repo.AdvanceGraphProgress(ctx, "conv_p", 1); err != nil {
		t.Fatal(err)
	}
	got, more, err = repo.GraphPending(ctx, "conv_p", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || more || got[0].SequenceNumber != 4 {
		t.Fatalf("après avancement: %d messages, more=%v, want les séquences 4 et 5", len(got), more)
	}

	if _, _, err := repo.GraphPending(ctx, "inconnue", 3); err == nil {
		t.Error("une conversation inconnue doit rendre une erreur ErrNotFound")
	}
}

// TestClaimNeLancePasDeuxExtractionsDeLaMemeConversation: tant qu'une
// extraction tourne, un second job de la même conversation attend, mais une
// autre conversation avance.
func TestClaimNeLancePasDeuxExtractionsDeLaMemeConversation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	jobs := NewJobRepo(pool)

	for _, conv := range []string{"conv_a", "conv_a", "conv_b"} {
		if err := jobs.Enqueue(ctx, "graph_extract", "ws1", conv, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := jobs.Claim(ctx)
	if err != nil || first == nil || first.ConversationID != "conv_a" {
		t.Fatalf("premier claim = %+v, %v, want conv_a", first, err)
	}
	second, err := jobs.Claim(ctx)
	if err != nil || second == nil || second.ConversationID != "conv_b" {
		t.Fatalf("second claim = %+v, %v, want conv_b: conv_a tourne déjà", second, err)
	}
	if third, err := jobs.Claim(ctx); err != nil || third != nil {
		t.Fatalf("troisième claim = %+v, %v, want rien tant que conv_a tourne", third, err)
	}
	if err := jobs.Complete(ctx, first.JobID); err != nil {
		t.Fatal(err)
	}
	if again, err := jobs.Claim(ctx); err != nil || again == nil || again.ConversationID != "conv_a" {
		t.Fatalf("après la fin de conv_a: %+v, %v, want le second job de conv_a", again, err)
	}
}

func TestScheduleGraphExtractRendLeJobEligible(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs := NewMessageRepo(pool)
	jobs := NewJobRepo(pool)

	if _, err := msgs.Append(ctx, appendInput("conv_s", "user:paul", "user", "salut", ""),
		0, graphJob(time.Hour, 100)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.ScheduleGraphExtract(ctx, "ws1", "conv_s"); err != nil {
		t.Fatal(err)
	}
	if n, due := pendingGraphJobs(t, ctx, msgs, "conv_s"); n != 1 || !due {
		t.Fatalf("%d jobs, éligible=%v, want le job existant rendu éligible", n, due)
	}
	// Sans job en attente, il en pose un.
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	if err := jobs.ScheduleGraphExtract(ctx, "ws1", "conv_s"); err != nil {
		t.Fatal(err)
	}
	if n, due := pendingGraphJobs(t, ctx, msgs, "conv_s"); n != 1 || !due {
		t.Fatalf("%d jobs, éligible=%v, want un job posé et éligible", n, due)
	}
}
