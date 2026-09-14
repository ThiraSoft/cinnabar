package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/graph"
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

// Un fait rend chacune de ses sources lisibles avec sa conversation et ses
// metadata: c'est ce qui permet au client de peser un fait selon ce que dit
// chaque message qui l'a fait naître.
func TestSearchGraphRendLesMetadataDeChaqueSource(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	msgs, repo, gr := NewMessageRepo(pool), NewSearchRepo(pool), NewGraphRepo(pool)

	add := func(conv, meta string) memory.Message {
		t.Helper()
		in := appendInput(conv, "agent:village", "assistant", "Ricardo doit dix pièces", "")
		in.Metadata = json.RawMessage(meta)
		res, err := msgs.Append(ctx, in, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.Message
	}
	crue := add("nine|player:ricardo", `{"belief":"oui"}`)
	doute := add("halvig|player:ricardo", `{"belief":"doute"}`)

	src := graph.EntityID("ws1", "person:ricardo")
	ent := memory.GraphEntity{EntityID: src, WorkspaceID: "ws1",
		CanonicalKey: "person:ricardo", EntityType: "person",
		DisplayName: "Ricardo", Resolved: true}
	dk := graph.DedupKey(src, "doit", nil, "dix pièces", nil)
	for _, m := range []memory.Message{crue, doute} {
		if err := gr.Apply(ctx, memory.GraphExtraction{
			WorkspaceID: "ws1", ConversationID: m.ConversationID,
			Entities: []memory.GraphEntity{ent},
			Relations: []memory.GraphRelation{{
				RelationID: graph.RelationID("ws1", dk), WorkspaceID: "ws1",
				SourceEntityID: src, RelationType: "doit", TargetLiteral: "dix pièces",
				ObservedAt: time.Now().UTC(), Confidence: 1, Scope: "participants",
				DedupKey: dk, ConversationID: m.ConversationID,
				SourceMessageIDs: []uuid.UUID{m.MessageID},
			}},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:village", Limit: 20},
		Subjects: []string{"user:ricardo"}, Text: questionNeutre, MaxHops: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 || len(got.Facts[0].Sources) != 2 {
		t.Fatalf("faits = %+v", got.Facts)
	}
	byConv := map[string]string{}
	for _, s := range got.Facts[0].Sources {
		var meta map[string]string
		if err := json.Unmarshal(s.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		byConv[s.ConversationID] = meta["belief"]
		if s.MessageID != crue.MessageID && s.MessageID != doute.MessageID {
			t.Errorf("source inconnue %s", s.MessageID)
		}
	}
	if byConv["nine|player:ricardo"] != "oui" || byConv["halvig|player:ricardo"] != "doute" {
		t.Errorf("sources = %v", byConv)
	}

	// Restreint à une conversation, le fait ne rend que la source qui s'y trouve.
	got, err = repo.SearchGraph(ctx, memory.GraphQuery{
		CandidateQuery: memory.CandidateQuery{
			WorkspaceID: "ws1", RequesterKey: "agent:village", Limit: 20,
			ConversationIDs: []string{"halvig|player:ricardo"}},
		Subjects: []string{"user:ricardo"}, Text: questionNeutre, MaxHops: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 || len(got.Facts[0].Sources) != 1 ||
		got.Facts[0].Sources[0].ConversationID != "halvig|player:ricardo" {
		t.Errorf("fait restreint = %+v", got.Facts)
	}
}
