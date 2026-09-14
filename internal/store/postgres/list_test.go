package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

func TestListMessagesPagesNewestFirstUnderTheAccessRule(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, search := NewMessageRepo(pool), NewSearchRepo(pool)

	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	add := func(conv, author, content, meta string, at time.Time) memory.Message {
		t.Helper()
		in := appendInput(conv, author, "user", content, "")
		in.CreatedAt, in.Metadata = at, json.RawMessage(meta)
		res, err := msgs.Append(ctx, in, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.Message
	}
	m1 := add("conv_a", "user:paul", "un", `{"belief":"oui"}`, base)
	m2 := add("conv_a", "user:paul", "deux", `{"belief":"non"}`, base.Add(time.Minute))
	m3 := add("conv_a", "user:paul", "trois", `{}`, base.Add(2*time.Minute))
	add("conv_b", "user:paul", "ailleurs", `{}`, base.Add(3*time.Minute))
	add("conv_marie", "user:marie", "chez Marie", `{}`, base.Add(4*time.Minute))

	q := memory.ListQuery{CandidateQuery: memory.CandidateQuery{
		WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 2,
		ConversationIDs: []string{"conv_a", "conv_marie"},
	}}
	page, err := search.ListMessages(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].MessageID != m3.MessageID || page[1].MessageID != m2.MessageID {
		t.Fatalf("première page = %v", ids(page))
	}
	if string(page[1].Metadata) != `{"belief": "non"}` && string(page[1].Metadata) != `{"belief":"non"}` {
		t.Errorf("metadata = %s", page[1].Metadata)
	}

	last := page[len(page)-1]
	q.BeforeAt, q.BeforeID = &last.CreatedAt, last.MessageID
	page, err = search.ListMessages(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].MessageID != m1.MessageID {
		t.Fatalf("seconde page = %v", ids(page))
	}

	q.BeforeAt = nil
	q.Limit = 10
	q.MetadataFilter = &memory.MetadataFilter{Key: "belief", Op: "ne", Value: json.RawMessage(`"non"`)}
	page, err = search.ListMessages(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].MessageID != m3.MessageID || page[1].MessageID != m1.MessageID {
		t.Errorf("listage filtré = %v", ids(page))
	}
}

func TestListMessagesRejectsEmptyRequester(t *testing.T) {
	pool := newTestPool(t)
	_, err := NewSearchRepo(pool).ListMessages(context.Background(), memory.ListQuery{
		CandidateQuery: memory.CandidateQuery{WorkspaceID: "ws1"},
	})
	if err == nil {
		t.Fatal("un demandeur vide doit être refusé")
	}
}

func ids(ms []memory.Message) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Content)
	}
	return out
}
