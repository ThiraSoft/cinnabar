package postgres

import (
	"context"
	"testing"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

func seedMessage(t *testing.T, repo *MessageRepo, conv, content string) memory.Message {
	t.Helper()
	res, err := repo.Append(context.Background(),
		appendInput(conv, "user:paul", "user", content, ""), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Message
}

func vec768(seed float32) []float32 {
	v := make([]float32, 768)
	for i := range v {
		v[i] = seed
	}
	return v
}

func TestUnitRepoUpsertIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs := NewMessageRepo(pool)
	units := NewUnitRepo(pool)

	m := seedMessage(t, msgs, "conv_1", "les tomates sont vertes")
	u := memory.Unit{
		MemoryUnitID:    memory.UnitID(m.MessageID, "model-a", "contextualized_message", 1),
		WorkspaceID:     "ws1",
		ConversationID:  "conv_1",
		AnchorMessageID: m.MessageID,
		StartSequence:   1, EndSequence: 1,
		EmbeddingText:  "texte",
		EmbeddingModel: "model-a",
		Strategy:       "contextualized_message",
		Version:        1,
		Scope:          "participants",
	}

	if err := units.Upsert(ctx, u, vec768(0.1)); err != nil {
		t.Fatalf("premier Upsert: %v", err)
	}
	u.EmbeddingText = "texte corrigé"
	if err := units.Upsert(ctx, u, vec768(0.2)); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memory_units`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d unités, want 1: l'identifiant déterministe doit dédupliquer", n)
	}
	var text string
	var active bool
	if err := pool.QueryRow(ctx,
		`SELECT embedding_text, active FROM memory_units`).Scan(&text, &active); err != nil {
		t.Fatal(err)
	}
	if text != "texte corrigé" {
		t.Errorf("embedding_text = %q, l'Upsert doit mettre à jour", text)
	}
	if !active {
		t.Error("un Upsert doit réactiver l'unité")
	}
}

// TestDeactivateCoveringTouchesEveryCoveringUnit vise la requête partagée
// par EditAndDeactivate et SoftDeleteAndDeactivate, appelée ici hors
// transaction via le pool (querier accepte les deux): c'est la requête
// elle-même qui est vérifiée, pas le chemin transactionnel, couvert par
// ailleurs dans messages_test.go.
func TestDeactivateCoveringTouchesEveryCoveringUnit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs := NewMessageRepo(pool)
	units := NewUnitRepo(pool)

	var ms []memory.Message
	for _, c := range []string{"un", "deux", "trois"} {
		ms = append(ms, seedMessage(t, msgs, "conv_1", c))
	}
	// Trois unités: [1,1], [1,2], [2,3].
	specs := [][2]int64{{1, 1}, {1, 2}, {2, 3}}
	for i, s := range specs {
		u := memory.Unit{
			MemoryUnitID: memory.UnitID(ms[i].MessageID, "m", "s", 1),
			WorkspaceID:  "ws1", ConversationID: "conv_1",
			AnchorMessageID: ms[i].MessageID,
			StartSequence:   s[0], EndSequence: s[1],
			EmbeddingText: "t", EmbeddingModel: "m", Strategy: "s",
			Version: 1, Scope: "participants",
		}
		if err := units.Upsert(ctx, u, vec768(float32(i))); err != nil {
			t.Fatal(err)
		}
	}

	// Le message 2 est touché: les unités [1,2] et [2,3] le couvrent, pas [1,1].
	touched, err := deactivateCovering(ctx, pool, "conv_1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(touched) != 2 {
		t.Errorf("%d unités désactivées, want 2", len(touched))
	}
	var activeCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_units WHERE active`).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Errorf("%d unités actives, want 1", activeCount)
	}
}
