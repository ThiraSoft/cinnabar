package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// seedWithMetadata écrit un message indexé portant des metadata.
func seedWithMetadata(t *testing.T, msgs *MessageRepo, units *UnitRepo,
	conv, author, content, meta string, embedding []float32) memory.Message {

	t.Helper()
	ctx := context.Background()
	in := appendInput(conv, author, "user", content, "")
	in.Metadata = json.RawMessage(meta)
	res, err := msgs.Append(ctx, in, 0, nil)
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

func filterOf(t *testing.T, raw string) *memory.MetadataFilter {
	t.Helper()
	var f memory.MetadataFilter
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	return &f
}

func anchors(cands []memory.Candidate) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, c := range cands {
		out[c.AnchorMessageID] = true
	}
	return out
}

func TestSearchDenseRestrictsToConversationsAndMetadata(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	a := seedWithMetadata(t, msgs, units, "nine|player:ricardo", "agent:village",
		"Ricardo a promis de revenir", `{"belief":"oui","min_affinity":0}`, unitVec(0))
	b := seedWithMetadata(t, msgs, units, "nine|player:astaroth", "agent:village",
		"Astaroth a volé une pomme", `{"min_affinity":0}`, unitVec(0))
	c := seedWithMetadata(t, msgs, units, "nine|lore", "agent:village",
		"Nine a perdu son frère", `{"min_affinity":60,"lore_id":"nine-1"}`, unitVec(0))
	d := seedWithMetadata(t, msgs, units, "nine|player:ricardo", "agent:village",
		"Ricardo a menti", `{"belief":"non"}`, unitVec(0))

	query := func(convs []string, filter string) map[uuid.UUID]bool {
		t.Helper()
		q := memory.DenseQuery{
			CandidateQuery: memory.CandidateQuery{
				WorkspaceID: "ws1", RequesterKey: "agent:village", Limit: 10,
				ConversationIDs: convs,
			},
			Embedding: unitVec(0),
		}
		if filter != "" {
			q.MetadataFilter = filterOf(t, filter)
		}
		cands, err := search.SearchDense(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range cands {
			if c.Rank != i+1 {
				t.Errorf("rang %d à la position %d", c.Rank, i)
			}
		}
		return anchors(cands)
	}

	got := query([]string{"nine|player:ricardo", "nine|lore"}, "")
	if !got[a.MessageID] || got[b.MessageID] || !got[c.MessageID] || !got[d.MessageID] {
		t.Errorf("filtre de conversations: %v", got)
	}

	got = query([]string{"nine|player:ricardo", "nine|lore"},
		`{"all":[{"key":"belief","op":"ne","value":"non"},
		  {"any":[{"key":"min_affinity","op":"lte","value":40},
		          {"key":"lore_id","op":"in","value":["nine-1"]}]}]}`)
	if !got[a.MessageID] || got[b.MessageID] || !got[c.MessageID] || got[d.MessageID] {
		t.Errorf("filtre de metadata avec lore débloqué: %v", got)
	}

	got = query(nil, `{"key":"min_affinity","op":"lte","value":40}`)
	if !got[a.MessageID] || !got[b.MessageID] || got[c.MessageID] || got[d.MessageID] {
		t.Errorf("filtre de metadata seul: %v", got)
	}
}

func TestSearchFilterNeverOpensAnUnreadableConversation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	seedWithMetadata(t, msgs, units, "conv_paul", "user:paul",
		"les tomates de Paul", `{"k":1}`, unitVec(0))

	base := memory.CandidateQuery{
		WorkspaceID: "ws1", RequesterKey: "user:marie", Limit: 10,
		ConversationIDs: []string{"conv_paul"},
		MetadataFilter:  filterOf(t, `{"key":"k","op":"eq","value":1}`),
	}
	dense, err := search.SearchDense(ctx, memory.DenseQuery{CandidateQuery: base, Embedding: unitVec(0)})
	if err != nil {
		t.Fatal(err)
	}
	lex, err := search.SearchLexical(ctx, memory.LexicalQuery{CandidateQuery: base, Text: "tomates"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dense) != 0 || len(lex) != 0 {
		t.Errorf("une conversation illisible a fui: dense %d, lexical %d", len(dense), len(lex))
	}
}

func TestSearchLexicalRestrictsToConversationsAndMetadata(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, units, search := NewMessageRepo(pool), NewUnitRepo(pool), NewSearchRepo(pool)

	a := seedWithMetadata(t, msgs, units, "conv_a", "user:paul",
		"les tomates vertes", `{"importance":8}`, unitVec(0))
	b := seedWithMetadata(t, msgs, units, "conv_b", "user:paul",
		"les tomates rouges", `{"importance":8}`, unitVec(0))
	c := seedWithMetadata(t, msgs, units, "conv_a", "user:paul",
		"les tomates mûres", `{"importance":2}`, unitVec(0))

	cands, err := search.SearchLexical(ctx, memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
			ConversationIDs: []string{"conv_a"},
			MetadataFilter:  filterOf(t, `{"key":"importance","op":"gt","value":5}`),
		},
		Text: "tomates",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := anchors(cands)
	if !got[a.MessageID] || got[b.MessageID] || got[c.MessageID] {
		t.Errorf("lexical filtré: %v", got)
	}
}

func TestSearchRejectsAnInvalidFilter(t *testing.T) {
	pool := newTestPool(t)
	search := NewSearchRepo(pool)
	_, err := search.SearchLexical(context.Background(), memory.LexicalQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "user:paul", Limit: 10,
			MetadataFilter: &memory.MetadataFilter{Key: "a", Op: "like"},
		},
		Text: "tomates",
	})
	if err == nil {
		t.Fatal("un filtre invalide doit être refusé avant la base")
	}
}

func TestMessageMetadataRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	msgs, units := NewMessageRepo(pool), NewUnitRepo(pool)
	m := seedWithMetadata(t, msgs, units, "conv_a", "user:paul",
		"bonjour", `{"importance":8}`, unitVec(0))
	got, err := msgs.ByID(context.Background(), m.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]int
	if err := json.Unmarshal(got.Metadata, &meta); err != nil || meta["importance"] != 8 {
		t.Errorf("metadata = %s, %v", got.Metadata, err)
	}
}
