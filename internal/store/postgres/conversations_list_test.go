package postgres

import (
	"context"
	"testing"
)

func TestConversationListByPrefixUnderTheAccessRule(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	convs := NewConversationRepo(pool)

	for _, c := range []struct{ id, scope, ws string }{
		{"nine|player:a", "workspace", "ws1"},
		{"nine|public", "workspace", "ws1"},
		{"halvig|public", "workspace", "ws1"},
		{"nine|secret", "participants", "ws1"},
		{"nine|ailleurs", "workspace", "ws2"},
		{"nine_x", "workspace", "ws1"},
	} {
		if err := convs.Declare(ctx, c.id, c.ws, c.scope, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := convs.SoftDelete(ctx, "nine|public", "ws1"); err != nil {
		t.Fatal(err)
	}
	if err := convs.Declare(ctx, "nine|public2", "ws1", "workspace", nil); err != nil {
		t.Fatal(err)
	}

	got, err := convs.List(ctx, "ws1", "agent:village", "nine|", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"nine|player:a", "nine|public2"}
	if len(got) != len(want) {
		t.Fatalf("conversations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].ConversationID != want[i] || got[i].Scope != "workspace" {
			t.Errorf("conversation %d = %+v, want %s", i, got[i], want[i])
		}
	}

	page, err := convs.List(ctx, "ws1", "agent:village", "nine|", "nine|player:a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].ConversationID != "nine|public2" {
		t.Errorf("après nine|player:a = %+v", page)
	}

	// Le préfixe est littéral: un _ ne vaut pas n'importe quel caractère.
	lit, err := convs.List(ctx, "ws1", "agent:village", "nine_", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lit) != 1 || lit[0].ConversationID != "nine_x" {
		t.Errorf("préfixe littéral = %+v", lit)
	}

	all, err := convs.List(ctx, "ws1", "agent:village", "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ConversationID != "halvig|public" {
		t.Errorf("sans préfixe = %+v", all)
	}

	if _, err := convs.List(ctx, "ws1", "", "", "", 10); err == nil {
		t.Error("un demandeur vide doit être refusé")
	}
}
