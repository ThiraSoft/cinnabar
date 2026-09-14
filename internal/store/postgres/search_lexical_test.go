package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// Critère d'acceptation 3: une recherche exacte retrouve les noms et termes rares.
func TestAcceptance3_LexicalFindsRareTerms(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	target := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"le cultivar Rose de Berne pousse bien cette année", unitVec(0))
	seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"il fait beau aujourd'hui", unitVec(1))

	cands, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Text: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) == 0 {
		t.Fatal("un terme rare doit être retrouvé par la recherche lexicale")
	}
	if cands[0].AnchorMessageID != target.MessageID {
		t.Error("le message contenant le terme doit venir en premier")
	}
	if cands[0].Strategy != "lexical" {
		t.Errorf("strategy = %q", cands[0].Strategy)
	}
	if cands[0].Rank != 1 {
		t.Errorf("rank = %d", cands[0].Rank)
	}
}

func TestSearchLexicalStemsFrench(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"mes tomates étaient encore vertes", unitVec(0))

	// La configuration french doit rapprocher le singulier du pluriel.
	cands, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Text: "tomate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Errorf("%d candidats, want 1: la racine française doit apparier", len(cands))
	}
}

func TestSearchLexicalAppliesACL(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	seedIndexedMessage(t, msgs, units, "conv_prive", "user:paul",
		"le cultivar Rose de Berne", unitVec(0))

	cands, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:etranger", Limit: 10,
		},
		Text: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("l'ACL doit s'appliquer aussi au lexical, got %d", len(cands))
	}
}

func TestSearchLexicalFindsMessageWithoutUnit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, search := NewMessageRepo(pool), NewSearchRepo(pool)

	// Message enregistré mais pas encore vectorisé: c'est le repli de la
	// section 15 de la spec, la recherche lexicale doit quand même le voir.
	res, err := msgs.Append(ctx,
		appendInput("conv_1", "user:paul", "user", "le cultivar Rose de Berne", ""), 2, nil)
	if err != nil {
		t.Fatal(err)
	}

	cands, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Text: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("%d candidats, want 1", len(cands))
	}
	if cands[0].AnchorMessageID != res.Message.MessageID {
		t.Error("le message non vectorisé doit être rendu")
	}
	if cands[0].MemoryUnitID != nil {
		t.Error("MemoryUnitID doit être nil quand aucune unité n'existe")
	}
}

func TestSearchLexicalSkipsDeleted(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"le cultivar Rose de Berne", unitVec(0))
	if _, _, err := msgs.SoftDeleteAndDeactivate(ctx, m.MessageID); err != nil {
		t.Fatal(err)
	}

	cands, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Text: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("un message supprimé ne doit pas ressortir, got %d", len(cands))
	}
}

// Les trois tests qui suivent sont les pendants lexicaux de
// TestSearchDenseSoftDeletedConversationWithACLGetsNothing,
// TestSearchDenseWorkspaceMismatchWithACLGetsNothing et
// TestSearchDensePrivateScopeRequiresExplicitACL. La recherche lexicale
// interroge une table différente (messages, avec sa propre jointure de
// garde ConversationGuardJoinOnMessages) de la recherche dense
// (memory_units, avec ConversationGuardJoin): corriger l'une ne protège pas
// l'autre, donc les deux fuites trouvées en revue sur le chemin dense ont
// chacune besoin de leur propre test de non-régression ici plutôt que de
// compter sur celui déjà committé côté dense.

// TestSearchLexicalSoftDeletedConversationWithACLGetsNothing est le pendant
// lexical de TestSearchDenseSoftDeletedConversationWithACLGetsNothing: la
// branche memory_unit_acl ne doit pas pouvoir rendre un message dont la
// conversation est supprimée, même quand le demandeur détient une ACL
// explicite sur l'unité ancrée dessus.
func TestSearchLexicalSoftDeletedConversationWithACLGetsNothing(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"le cultivar Rose de Berne", unitVec(0))
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

	cands, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:invite", Limit: 10,
		},
		Text: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("une conversation supprimée ne doit rien rendre, même via ACL explicite, got %d", len(cands))
	}
}

// TestSearchLexicalWorkspaceMismatchWithACLGetsNothing est le pendant
// lexical de TestSearchDenseWorkspaceMismatchWithACLGetsNothing. Le message
// lui-même porte un workspace_id ("ws_attacker") qui diverge de celui de sa
// vraie conversation ("ws_real"), forme de corruption qu'Append refuse mais
// qui peut déjà exister dans une base ancienne (comme pour memory_units en
// tâche 6). Il est inséré en SQL brut pour cette raison, pas via Append. Une
// ligne memory_unit_acl sur l'unité ancrée ne doit pas suffire à faire
// franchir la frontière de workspace à un demandeur qui interroge
// "ws_attacker".
func TestSearchLexicalWorkspaceMismatchWithACLGetsNothing(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	units, search := NewUnitRepo(pool), NewSearchRepo(pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id, scope)
		VALUES ('conv_real', 'ws_real', 'participants')`); err != nil {
		t.Fatal(err)
	}
	messageID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO messages (message_id, conversation_id, workspace_id,
			sequence_number, author_key, role, content, created_at)
		VALUES ($1, 'conv_real', 'ws_attacker', 1, 'user:paul', 'user',
			'le cultivar Rose de Berne', $2)`,
		messageID, time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	u := memory.Unit{
		MemoryUnitID: memory.UnitID(messageID, "m", "s", 1),
		WorkspaceID:  "ws_attacker", ConversationID: "conv_real",
		AnchorMessageID: messageID,
		StartSequence:   1, EndSequence: 1,
		EmbeddingText: "le cultivar Rose de Berne", EmbeddingModel: "m", Strategy: "s",
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

	cands, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws_attacker", RequesterKey: "agent:invite", Limit: 10,
		},
		Text: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("un message dont le workspace diverge de sa conversation ne doit rien rendre via ACL, got %d", len(cands))
	}
}

// TestSearchLexicalPrivateScopeRequiresExplicitACL est le pendant lexical de
// TestSearchDensePrivateScopeRequiresExplicitACL: le scope private (comme
// explicit) ne doit se résoudre que par memory_unit_acl, jamais par la seule
// participation à la conversation.
func TestSearchLexicalPrivateScopeRequiresExplicitACL(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"le cultivar Rose de Berne", unitVec(0))
	if _, err := pool.Exec(ctx, `
		UPDATE conversations SET scope = 'private' WHERE conversation_id = 'conv_1'`); err != nil {
		t.Fatal(err)
	}

	cands, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Text: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("un scope private ne doit pas s'ouvrir par simple participation, got %d", len(cands))
	}
}

func TestSearchLexicalEmptyQueryIsNotAnError(t *testing.T) {
	pool := newTestPool(t)
	search := NewSearchRepo(pool)

	// websearch_to_tsquery sur du bruit peut rendre une requête vide. Ce n'est
	// pas une erreur, juste zéro résultat.
	cands, err := search.SearchLexical(context.Background(), memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Text: "   ",
	})
	if err != nil {
		t.Fatalf("une requête vide ne doit pas être une erreur: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("%d candidats pour une requête vide", len(cands))
	}
}

// TestSearchLexicalPunctuationOnlyQueryReachesPostgresAndIsNotAnError couvre
// le Mineur 4 de la revue: TestSearchLexicalEmptyQueryIsNotAnError s'arrête
// avant Postgres parce que le garde Go sur une chaîne blanche l'intercepte
// en premier, donc la propriété que son propre commentaire décrit
// (websearch_to_tsquery peut rendre une requête vide à partir d'un texte non
// vide) n'était jamais exercée. "!!!" n'est pas blanc mais ne contient aucun
// terme lexical, donc websearch_to_tsquery le réduit à une requête vide côté
// serveur; Postgres peut émettre une notice pour un mot-outil filtré, sans
// gravité.
func TestSearchLexicalPunctuationOnlyQueryReachesPostgresAndIsNotAnError(t *testing.T) {
	pool := newTestPool(t)
	search := NewSearchRepo(pool)

	cands, err := search.SearchLexical(context.Background(), memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Text: "!!!",
	})
	if err != nil {
		t.Fatalf("une requête sans terme lexical ne doit pas être une erreur: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("%d candidats pour une requête sans terme lexical", len(cands))
	}
}

// TestSearchLexicalRejectsEmptyRequesterKey est le pendant lexical de
// TestSearchDenseRejectsEmptyRequesterKey: le Critique 1 de la revue de la
// tâche 10 s'appliquait ici sans aucune protection avant le correctif,
// puisque SearchLexical ne validait que Text et Limit, jamais l'identité.
func TestSearchLexicalRejectsEmptyRequesterKey(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id, scope)
		VALUES ('conv_pub', 'ws1', 'workspace')`); err != nil {
		t.Fatal(err)
	}
	seedIndexedMessage(t, msgs, units, "conv_pub", "user:paul",
		"note d'équipe rose de berne", unitVec(0))

	_, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "", Limit: 10,
		},
		Text: "Rose de Berne",
	})
	if err == nil {
		t.Fatal("une clé demandeur vide doit être rejetée, pas rendre le scope workspace")
	}
}

// TestSearchLexicalDedupesMultipleActiveUnitsOnSameAnchor couvre l'Important
// 2 de la revue: rien n'empêche en écriture deux unités actives sur la même
// ancre (Upsert ne désactive jamais une version d'indexation antérieure
// quand une nouvelle est écrite), et un simple LEFT JOIN les aurait toutes
// jointes, dupliquant la ligne du message. Le LATERAL de lexicalSQL ne rend
// que la plus récemment mise à jour, et le EXISTS de la branche ACL est
// gardé sur cette même unité: une ACL sur l'unité la plus ancienne ne doit
// pas donner accès via l'identité de la plus récente.
func TestSearchLexicalDedupesMultipleActiveUnitsOnSameAnchor(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	res, err := msgs.Append(ctx,
		appendInput("conv_1", "user:paul", "user", "le cultivar Rose de Berne", ""), 2, nil)
	if err != nil {
		t.Fatal(err)
	}

	older := memory.Unit{
		MemoryUnitID:    memory.UnitID(res.Message.MessageID, "m", "s", 1),
		WorkspaceID:     "ws1",
		ConversationID:  "conv_1",
		AnchorMessageID: res.Message.MessageID,
		StartSequence:   res.Message.SequenceNumber,
		EndSequence:     res.Message.SequenceNumber,
		EmbeddingText:   "le cultivar Rose de Berne",
		EmbeddingModel:  "m", Strategy: "s", Version: 1, Scope: "participants",
	}
	if err := units.Upsert(ctx, older, unitVec(0)); err != nil {
		t.Fatal(err)
	}

	newer := older
	newer.MemoryUnitID = memory.UnitID(res.Message.MessageID, "m", "s", 2)
	newer.Version = 2
	if err := units.Upsert(ctx, newer, unitVec(0)); err != nil {
		t.Fatal(err)
	}
	// Force un ordre déterministe entre les deux updated_at, plutôt que de
	// compter sur l'écart naturel entre deux Upsert consécutifs.
	if _, err := pool.Exec(ctx, `
		UPDATE memory_units SET updated_at = updated_at + interval '1 second'
		WHERE memory_unit_id = $1`, newer.MemoryUnitID); err != nil {
		t.Fatal(err)
	}

	// Une ACL uniquement sur l'unité la plus ancienne.
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_unit_acl (memory_unit_id, principal_key)
		VALUES ($1, 'agent:invite')`, older.MemoryUnitID); err != nil {
		t.Fatal(err)
	}

	cands, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Text: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("%d candidats, want 1: deux unités actives sur la même ancre ne doivent pas dupliquer la ligne", len(cands))
	}
	if cands[0].MemoryUnitID == nil || *cands[0].MemoryUnitID != newer.MemoryUnitID {
		t.Error("MemoryUnitID doit être celui de l'unité la plus récemment mise à jour")
	}

	// Un tiers non participant, dont l'ACL ne couvre que l'unité ancienne,
	// ne doit pas obtenir accès via l'identité de l'unité récente.
	cands, err = search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:invite", Limit: 10,
		},
		Text: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("une ACL sur l'unité la plus ancienne ne doit pas donner accès via l'unité la plus récente, got %d", len(cands))
	}
}
