package postgres

import (
	"context"
	"testing"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// seedIndexedMessage écrit un message et son unité vectorielle.
func seedIndexedMessage(t *testing.T, msgs *MessageRepo, units *UnitRepo,
	conv, author, content string, embedding []float32) memory.Message {

	t.Helper()
	ctx := context.Background()
	res, err := msgs.Append(ctx, appendInput(conv, author, "user", content, ""), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := memory.Unit{
		MemoryUnitID: memory.UnitID(res.Message.MessageID, "m", "s", 1),
		WorkspaceID:  "ws1", ConversationID: conv,
		AnchorMessageID: res.Message.MessageID,
		StartSequence:   res.Message.SequenceNumber,
		EndSequence:     res.Message.SequenceNumber,
		EmbeddingText:   content, EmbeddingModel: "m", Strategy: "s",
		Version: 1, Scope: res.Scope,
	}
	if err := units.Upsert(ctx, u, embedding); err != nil {
		t.Fatal(err)
	}
	return res.Message
}

// unitVec produit un vecteur unitaire orienté sur une dimension, ce qui rend
// les distances cosinus prévisibles sans dépendre d'un vrai modèle.
func unitVec(dim int) []float32 {
	v := make([]float32, 768)
	v[dim] = 1
	return v
}

func TestSearchDenseOrdersByCosineDistance(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	near := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"les tomates de mon jardin", unitVec(0))
	_ = seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"la mécanique des fluides", unitVec(5))

	cands, err := search.SearchDense(ctx, memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Embedding: unitVec(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("%d candidats, want 2", len(cands))
	}
	if cands[0].AnchorMessageID != near.MessageID {
		t.Error("le candidat le plus proche doit venir en premier")
	}
	if cands[0].Rank != 1 || cands[1].Rank != 2 {
		t.Errorf("rangs = %d, %d", cands[0].Rank, cands[1].Rank)
	}
	if cands[0].Strategy != "dense" {
		t.Errorf("strategy = %q", cands[0].Strategy)
	}
	if cands[0].MemoryUnitID == nil {
		t.Error("la stratégie dense doit renseigner MemoryUnitID")
	}
	if cands[0].AccessReason != "conversation_participant" {
		t.Errorf("access_reason = %q", cands[0].AccessReason)
	}
}

// Critère d'acceptation 4: un agent non autorisé ne reçoit aucun extrait.
func TestAcceptance4_UnauthorizedAgentGetsNothing(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	// Conversation en scope participants (la valeur par défaut d'Append):
	// paul y participe, l'agent jardinage jamais. Ce test couvre le scope
	// participants, pas private ni explicit malgré son nom de conversation
	// trompeur d'origine: voir TestSearchDensePrivateScopeRequiresExplicitACL
	// pour le scope private.
	seedIndexedMessage(t, msgs, units, "conv_prive", "user:paul",
		"mon code de coffre est 1234", unitVec(0))

	q := memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", Limit: 10,
		},
		Embedding: unitVec(0),
	}

	// Le participant voit.
	q.RequesterKey = "user:paul"
	own, err := search.SearchDense(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 {
		t.Fatalf("le participant doit voir son souvenir, got %d", len(own))
	}

	// Un tiers ne voit rien.
	q.RequesterKey = "agent:jardinage"
	none, err := search.SearchDense(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("un agent non participant ne doit rien voir, got %d candidats", len(none))
	}
}

func TestSearchDenseIsolatesWorkspaces(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	seedIndexedMessage(t, msgs, units, "conv_1", "user:paul", "secret", unitVec(0))

	cands, err := search.SearchDense(ctx, memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws_autre", RequesterKey: "user:paul", Limit: 10,
		},
		Embedding: unitVec(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("un autre workspace ne doit rien voir, got %d", len(cands))
	}
}

func TestSearchDenseHonorsWorkspaceScope(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id, scope)
		VALUES ('conv_pub', 'ws1', 'workspace')`); err != nil {
		t.Fatal(err)
	}
	seedIndexedMessage(t, msgs, units, "conv_pub", "user:paul",
		"note d'équipe", unitVec(0))

	cands, err := search.SearchDense(ctx, memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:nouveau", Limit: 10,
		},
		Embedding: unitVec(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("un scope workspace doit être visible par tout le workspace, got %d", len(cands))
	}
	if cands[0].AccessReason != "workspace_scope" {
		t.Errorf("access_reason = %q, want workspace_scope", cands[0].AccessReason)
	}
}

func TestSearchDenseHonorsExplicitACL(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"partagé explicitement", unitVec(0))
	unitID := memory.UnitID(m.MessageID, "m", "s", 1)
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_unit_acl (memory_unit_id, principal_key)
		VALUES ($1, 'agent:invite')`, unitID); err != nil {
		t.Fatal(err)
	}

	cands, err := search.SearchDense(ctx, memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:invite", Limit: 10,
		},
		Embedding: unitVec(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("une ACL explicite doit donner accès, got %d", len(cands))
	}
	if cands[0].AccessReason != "explicit_acl" {
		t.Errorf("access_reason = %q, want explicit_acl", cands[0].AccessReason)
	}
}

func TestSearchDenseSkipsInactiveAndDeleted(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	inactive := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"unité désactivée", unitVec(0))
	if _, err := deactivateCovering(ctx, pool, "conv_1", inactive.SequenceNumber); err != nil {
		t.Fatal(err)
	}

	deleted := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"message supprimé", unitVec(1))
	if _, _, err := msgs.SoftDeleteAndDeactivate(ctx, deleted.MessageID); err != nil {
		t.Fatal(err)
	}

	q := memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Embedding: unitVec(0),
	}
	cands, err := search.SearchDense(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	// Ni l'unité désactivée ni le message supprimé ne doivent ressortir. Le
	// second est le filet de sécurité du critère 6.
	if len(cands) != 0 {
		t.Errorf("%d candidats, want 0: %+v", len(cands), cands)
	}
}

// TestSearchDenseSoftDeletedConversationWithACLGetsNothing couvre la première
// forme du Critique 1 de la revue: la branche memory_unit_acl ne doit pas
// pouvoir rendre une unité dont la conversation est supprimée, même quand le
// demandeur détient une ACL explicite dessus.
func TestSearchDenseSoftDeletedConversationWithACLGetsNothing(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"partagé explicitement", unitVec(0))
	unitID := memory.UnitID(m.MessageID, "m", "s", 1)
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_unit_acl (memory_unit_id, principal_key)
		VALUES ($1, 'agent:invite')`, unitID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE conversations SET deleted_at = now() WHERE conversation_id = 'conv_1'`); err != nil {
		t.Fatal(err)
	}

	cands, err := search.SearchDense(ctx, memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:invite", Limit: 10,
		},
		Embedding: unitVec(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("une conversation supprimée ne doit rien rendre, même via ACL explicite, got %d", len(cands))
	}
}

// TestSearchDenseWorkspaceMismatchWithACLGetsNothing couvre la seconde forme
// du Critique 1: une unité dont workspace_id diverge de celui de sa
// conversation (la forme du bug fermé en tâche 6, mais qui peut déjà exister
// dans une base ancienne) ne doit pas franchir la frontière de workspace via
// une ACL explicite.
func TestSearchDenseWorkspaceMismatchWithACLGetsNothing(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	// conv_b vit dans ws_b.
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id, scope)
		VALUES ('conv_b', 'ws_b', 'participants')`); err != nil {
		t.Fatal(err)
	}
	res, err := msgs.Append(ctx, memory.AppendInput{
		WorkspaceID: "ws_b", ConversationID: "conv_b", DefaultScope: "participants",
		AuthorKey: "user:paul", Role: "user", Content: "secret ws_b",
	}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	// L'unité est taguée ws_a, en désaccord avec sa conversation ws_b.
	u := memory.Unit{
		MemoryUnitID: memory.UnitID(res.Message.MessageID, "m", "s", 1),
		WorkspaceID:  "ws_a", ConversationID: "conv_b",
		AnchorMessageID: res.Message.MessageID,
		StartSequence:   res.Message.SequenceNumber, EndSequence: res.Message.SequenceNumber,
		EmbeddingText: "secret ws_b", EmbeddingModel: "m", Strategy: "s",
		Version: 1, Scope: "participants",
	}
	if err := units.Upsert(ctx, u, unitVec(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_unit_acl (memory_unit_id, principal_key)
		VALUES ($1, 'agent:invite')`, u.MemoryUnitID); err != nil {
		t.Fatal(err)
	}

	cands, err := search.SearchDense(ctx, memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws_a", RequesterKey: "agent:invite", Limit: 10,
		},
		Embedding: unitVec(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("une unité dont le workspace diverge de sa conversation ne doit rien rendre via ACL, got %d", len(cands))
	}
}

// TestSearchDensePrivateScopeRequiresExplicitACL couvre l'Important 2: le
// scope private (comme explicit) ne doit se résoudre que par
// memory_unit_acl, jamais par la seule participation à la conversation.
func TestSearchDensePrivateScopeRequiresExplicitACL(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"secret privé", unitVec(0))
	if _, err := pool.Exec(ctx, `
		UPDATE conversations SET scope = 'private' WHERE conversation_id = 'conv_1'`); err != nil {
		t.Fatal(err)
	}

	cands, err := search.SearchDense(ctx, memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Embedding: unitVec(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("un scope private ne doit pas s'ouvrir par simple participation, got %d", len(cands))
	}
}

// TestSearchDenseRejectsEmptyRequesterKey couvre le Critique 1 de la revue
// de la tâche 10: ReadableCTE ne référence jamais $2 dans sa branche
// 'workspace', donc une requester_key vide continuerait de rendre tout le
// scope workspace du workspace demandé si elle n'était pas rejetée avant
// d'atteindre le SQL (voir ValidateRequester dans acl.go).
func TestSearchDenseRejectsEmptyRequesterKey(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id, scope)
		VALUES ('conv_pub', 'ws1', 'workspace')`); err != nil {
		t.Fatal(err)
	}
	seedIndexedMessage(t, msgs, units, "conv_pub", "user:paul", "note d'équipe", unitVec(0))

	_, err := search.SearchDense(ctx, memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "", Limit: 10,
		},
		Embedding: unitVec(0),
	})
	if err == nil {
		t.Fatal("une clé demandeur vide doit être rejetée, pas rendre le scope workspace")
	}
}
