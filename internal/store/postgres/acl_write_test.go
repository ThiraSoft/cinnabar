package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

func TestACLRepoUnitContextReportsParticipantsAndScope(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, acl := NewMessageRepo(pool), NewUnitRepo(pool), NewACLRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"les tomates", unitVec(0))
	unitID := memory.UnitID(m.MessageID, "m", "s", 1)

	uc, err := acl.UnitContext(ctx, unitID)
	if err != nil {
		t.Fatal(err)
	}
	if uc.WorkspaceID != "ws1" || uc.ConversationID != "conv_1" {
		t.Errorf("contexte = %+v", uc)
	}
	if uc.Scope != "participants" {
		t.Errorf("scope = %q", uc.Scope)
	}
	if len(uc.Participants) != 1 || uc.Participants[0] != "user:paul" {
		t.Errorf("participants = %v", uc.Participants)
	}
}

func TestACLRepoUnitContextUnknownUnit(t *testing.T) {
	pool := newTestPool(t)
	acl := NewACLRepo(pool)
	if _, err := acl.UnitContext(context.Background(), uuid.New()); !errors.Is(err, ErrNoUnit) {
		t.Fatalf("unité inconnue: got %v, want ErrNoUnit", err)
	}
}

// TestACLRepoUnitContextSoftDeletedConversation couvre le premier point du
// self-review de la tâche 18: une conversation supprimée doit rendre
// l'unité comme si elle n'existait plus, jamais un contexte périmé qu'un
// appelant pourrait utiliser pour partager une unité que plus personne ne
// peut lire par ailleurs.
func TestACLRepoUnitContextSoftDeletedConversation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, acl := NewMessageRepo(pool), NewUnitRepo(pool), NewACLRepo(pool)
	convs := NewConversationRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul", "x", unitVec(0))
	unitID := memory.UnitID(m.MessageID, "m", "s", 1)

	if _, err := convs.SoftDelete(ctx, "conv_1", "ws1"); err != nil {
		t.Fatal(err)
	}

	if _, err := acl.UnitContext(ctx, unitID); !errors.Is(err, ErrNoUnit) {
		t.Fatalf("conversation supprimée: got %v, want ErrNoUnit", err)
	}
}

// TestACLRepoUnitContextUsesConversationWorkspace couvre le point de revue:
// UnitContext doit rendre le workspace_id de la conversation, pas celui
// (dénormalisé) de l'unité, pour que canShare décide sur exactement la même
// colonne que ConversationGuardJoin utilise côté recherche. En
// fonctionnement normal les deux valeurs coïncident toujours (memory_units
// n'est jamais écrit avec un workspace_id différent de celui de sa
// conversation), donc ce test force volontairement une divergence pour
// prouver que UnitContext ne se fie pas à la copie de memory_units.
func TestACLRepoUnitContextUsesConversationWorkspace(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, acl := NewMessageRepo(pool), NewUnitRepo(pool), NewACLRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul", "x", unitVec(0))
	unitID := memory.UnitID(m.MessageID, "m", "s", 1)

	if _, err := pool.Exec(ctx,
		`UPDATE memory_units SET workspace_id = 'ws_divergent' WHERE memory_unit_id = $1`,
		unitID); err != nil {
		t.Fatal(err)
	}

	uc, err := acl.UnitContext(ctx, unitID)
	if err != nil {
		t.Fatal(err)
	}
	if uc.WorkspaceID != "ws1" {
		t.Errorf("workspace = %q, want ws1 (celui de la conversation, pas celui de l'unité)", uc.WorkspaceID)
	}
}

// TestACLRepoUnitContextInactiveUnit couvre le point de revue: une unité
// désactivée ne doit pas pouvoir être partagée. Un Grant sur une unité
// inactive ne changerait rien d'observable (la recherche l'ignore déjà via
// mu.active), donc un 200 y serait trompeur: aussi bien répondre 404 tout de
// suite, comme pour une unité qui n'existe pas.
func TestACLRepoUnitContextInactiveUnit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, acl := NewMessageRepo(pool), NewUnitRepo(pool), NewACLRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul", "x", unitVec(0))
	unitID := memory.UnitID(m.MessageID, "m", "s", 1)

	if _, err := pool.Exec(ctx,
		`UPDATE memory_units SET active = FALSE WHERE memory_unit_id = $1`, unitID); err != nil {
		t.Fatal(err)
	}

	if _, err := acl.UnitContext(ctx, unitID); !errors.Is(err, ErrNoUnit) {
		t.Fatalf("unité inactive: got %v, want ErrNoUnit", err)
	}
}

// TestACLRepoUnitContextDeletedAnchor est le pendant de
// TestACLRepoUnitContextInactiveUnit pour le message ancre plutôt que
// l'unité elle-même: denseSQL exclut aussi une unité dont le message ancre
// est supprimé (m.deleted_at IS NULL), donc UnitContext doit refuser de
// même plutôt que de laisser croire qu'un partage y aurait un effet.
func TestACLRepoUnitContextDeletedAnchor(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, acl := NewMessageRepo(pool), NewUnitRepo(pool), NewACLRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul", "x", unitVec(0))
	unitID := memory.UnitID(m.MessageID, "m", "s", 1)

	if _, err := pool.Exec(ctx,
		`UPDATE messages SET deleted_at = now() WHERE message_id = $1`, m.MessageID); err != nil {
		t.Fatal(err)
	}

	if _, err := acl.UnitContext(ctx, unitID); !errors.Is(err, ErrNoUnit) {
		t.Fatalf("ancre supprimée: got %v, want ErrNoUnit", err)
	}
}

func TestACLRepoGrantThenSearchSeesTheUnit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, acl := NewMessageRepo(pool), NewUnitRepo(pool), NewACLRepo(pool)
	search := NewSearchRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"les tomates", unitVec(0))
	unitID := memory.UnitID(m.MessageID, "m", "s", 1)

	q := memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:invite", Limit: 10,
		},
		Embedding: unitVec(0),
	}

	before, err := search.SearchDense(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("pré-condition: l'invité ne doit rien voir, got %d", len(before))
	}

	if err := acl.Grant(ctx, unitID, "agent:invite", "read"); err != nil {
		t.Fatal(err)
	}

	after, err := search.SearchDense(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("après Grant l'invité doit voir, got %d", len(after))
	}
	if after[0].AccessReason != "explicit_acl" {
		t.Errorf("access_reason = %q, want explicit_acl", after[0].AccessReason)
	}
}

func TestACLRepoGrantIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, acl := NewMessageRepo(pool), NewUnitRepo(pool), NewACLRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul", "x", unitVec(0))
	unitID := memory.UnitID(m.MessageID, "m", "s", 1)

	if err := acl.Grant(ctx, unitID, "agent:invite", "read"); err != nil {
		t.Fatal(err)
	}
	if err := acl.Grant(ctx, unitID, "agent:invite", "read"); err != nil {
		t.Fatalf("un second Grant identique ne doit pas échouer: %v", err)
	}

	entries, err := acl.List(ctx, unitID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("%d entrées, want 1", len(entries))
	}
}

func TestACLRepoRevoke(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, acl := NewMessageRepo(pool), NewUnitRepo(pool), NewACLRepo(pool)
	search := NewSearchRepo(pool)

	m := seedIndexedMessage(t, msgs, units, "conv_1", "user:paul", "x", unitVec(0))
	unitID := memory.UnitID(m.MessageID, "m", "s", 1)

	if err := acl.Grant(ctx, unitID, "agent:invite", "read"); err != nil {
		t.Fatal(err)
	}
	n, err := acl.Revoke(ctx, unitID, "agent:invite")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Revoke a touché %d lignes, want 1", n)
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
		t.Errorf("après Revoke l'accès doit disparaître, got %d", len(cands))
	}
}

func TestConversationRepoSoftDeleteHidesEverything(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units := NewMessageRepo(pool), NewUnitRepo(pool)
	convs, search := NewConversationRepo(pool), NewSearchRepo(pool)

	seedIndexedMessage(t, msgs, units, "conv_1", "user:paul",
		"le cultivar Rose de Berne", unitVec(0))

	n, err := convs.SoftDelete(ctx, "conv_1", "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("SoftDelete a touché %d conversations, want 1", n)
	}

	q := memory.DenseQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
		},
		Embedding: unitVec(0),
	}
	dense, err := search.SearchDense(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(dense) != 0 {
		t.Errorf("le dense doit être vide après suppression, got %d", len(dense))
	}

	lex, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: q.CandidateQuery, Text: "Rose de Berne",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lex) != 0 {
		t.Errorf("le lexical doit être vide après suppression, got %d", len(lex))
	}

	// Les unités doivent aussi être désactivées, pas seulement masquées par la
	// CTE readable: une réactivation accidentelle du scope ne doit pas
	// ressusciter le contenu.
	var active int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_units WHERE conversation_id = 'conv_1' AND active`,
	).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Errorf("%d unités encore actives", active)
	}
}

func TestConversationRepoSoftDeleteIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	convs := NewConversationRepo(pool)

	if err := convs.Declare(ctx, "conv_1", "ws1", "participants", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := convs.SoftDelete(ctx, "conv_1", "ws1"); err != nil {
		t.Fatal(err)
	}
	n, err := convs.SoftDelete(ctx, "conv_1", "ws1")
	if err != nil {
		t.Fatalf("un second SoftDelete ne doit pas échouer: %v", err)
	}
	if n != 0 {
		t.Errorf("second SoftDelete a touché %d lignes, want 0", n)
	}
}

func TestConversationRepoSoftDeleteRefusesForeignWorkspace(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	convs := NewConversationRepo(pool)

	if err := convs.Declare(ctx, "conv_1", "ws1", "participants", nil); err != nil {
		t.Fatal(err)
	}
	n, err := convs.SoftDelete(ctx, "conv_1", "ws_du_voisin")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("un workspace étranger ne doit rien supprimer, got %d lignes", n)
	}

	var deleted bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at IS NOT NULL FROM conversations WHERE conversation_id = 'conv_1'`,
	).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("la conversation ne devait pas être marquée supprimée")
	}
}

// TestConversationRepoContextReportsScopeAndParticipants couvre
// ConversationRepo.Context, ajoutée pour que handleDeleteConversation
// applique la même règle d'accès que le partage (participation ou scope
// workspace) avant de supprimer une conversation entière, plutôt que de ne
// vérifier que le workspace.
func TestConversationRepoContextReportsScopeAndParticipants(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units := NewMessageRepo(pool), NewUnitRepo(pool)
	convs := NewConversationRepo(pool)

	seedIndexedMessage(t, msgs, units, "conv_1", "user:paul", "x", unitVec(0))

	cc, err := convs.Context(ctx, "conv_1")
	if err != nil {
		t.Fatal(err)
	}
	if cc.WorkspaceID != "ws1" {
		t.Errorf("workspace = %q, want ws1", cc.WorkspaceID)
	}
	if cc.Scope != "participants" {
		t.Errorf("scope = %q, want participants", cc.Scope)
	}
	if len(cc.Participants) != 1 || cc.Participants[0] != "user:paul" {
		t.Errorf("participants = %v", cc.Participants)
	}
}

func TestConversationRepoContextUnknownConversation(t *testing.T) {
	pool := newTestPool(t)
	convs := NewConversationRepo(pool)
	if _, err := convs.Context(context.Background(), "conv_absente"); !errors.Is(err, ErrNoConversation) {
		t.Fatalf("conversation inconnue: got %v, want ErrNoConversation", err)
	}
}

func TestConversationRepoContextSoftDeletedConversation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	convs := NewConversationRepo(pool)

	if err := convs.Declare(ctx, "conv_1", "ws1", "participants", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := convs.SoftDelete(ctx, "conv_1", "ws1"); err != nil {
		t.Fatal(err)
	}

	if _, err := convs.Context(ctx, "conv_1"); !errors.Is(err, ErrNoConversation) {
		t.Fatalf("conversation supprimée: got %v, want ErrNoConversation", err)
	}
}
