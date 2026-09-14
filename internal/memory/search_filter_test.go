package memory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestSearchForwardsRestrictionsToEveryStrategy(t *testing.T) {
	s, _, dense, lexical, graph, _ := newSearchFixture(t)
	filter := &MetadataFilter{Key: "belief", Op: "ne", Value: json.RawMessage(`"non"`)}
	convs := []string{"conv_1", "conv_2"}

	if _, err := s.Search(context.Background(), SearchRequest{
		WorkspaceID: "ws1", RequesterKey: "user:paul", Query: "tomates",
		ConversationIDs: convs, MetadataFilter: filter,
	}); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]CandidateQuery{
		"dense": dense.got.CandidateQuery, "lexical": lexical.got.CandidateQuery,
		"graph": graph.got.CandidateQuery,
	} {
		if len(q.ConversationIDs) != 2 || q.MetadataFilter != filter {
			t.Errorf("%s: restrictions non transmises: %+v", name, q)
		}
	}
}

func TestSearchRejectsInvalidFilterBeforeAnyStrategy(t *testing.T) {
	s, _, dense, _, _, _ := newSearchFixture(t)
	_, err := s.Search(context.Background(), SearchRequest{
		WorkspaceID: "ws1", RequesterKey: "user:paul", Query: "tomates",
		MetadataFilter: &MetadataFilter{Key: "a", Op: "like", Value: json.RawMessage(`1`)},
	})
	if !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("err = %v, want ErrInvalidFilter", err)
	}
	if dense.seen.Load() != 0 {
		t.Error("aucune stratégie ne doit partir sur un filtre invalide")
	}
}

func TestSearchReturnsAnchorMetadata(t *testing.T) {
	s, repo, _, _, _, _ := newSearchFixture(t)
	window := repo.byConv["conv_1"]
	window[3].Metadata = json.RawMessage(`{"voisin":true}`)
	window[4].Metadata = json.RawMessage(`{"importance":7}`)

	resp, err := s.Search(context.Background(), SearchRequest{
		WorkspaceID: "ws1", RequesterKey: "user:paul", Query: "tomates",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("%d résultats", len(resp.Results))
	}
	if got := string(resp.Results[0].Metadata); got != `{"importance":7}` {
		t.Errorf("metadata = %s, want celles de l'ancre", got)
	}
}
