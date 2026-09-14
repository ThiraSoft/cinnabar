package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

type stubLister struct {
	got []memory.ListQuery
	out []memory.Message
	err error
}

func (s *stubLister) ListMessages(_ context.Context, q memory.ListQuery) ([]memory.Message, error) {
	s.got = append(s.got, q)
	if len(s.out) > q.Limit {
		return s.out[:q.Limit], s.err
	}
	return s.out, s.err
}

func newListServer(t *testing.T, l Lister) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Service: config.Service{MaxRequestBytes: 1 << 20, ConsistencyDefault: "eventual"},
		Access:  config.Access{DefaultScope: "participants"},
	}
	res := &stubResolver{principal: &memory.Principal{
		ClientID: uuid.New(), Label: "orchestrateur",
		WorkspaceID: "ws1", AllowedIdentities: []string{"agent:*", "user:paul"},
	}}
	srv := NewServer(cfg, res, &stubIngester{}, &stubEditor{}, &stubFinder{},
		&stubConversations{}, &stubOps{})
	if l != nil {
		srv.WithLister(l)
	}
	return srv.Handler()
}

func listedMessages(n int) []memory.Message {
	base := time.Date(2026, 9, 14, 10, 0, 0, 123456000, time.UTC)
	out := make([]memory.Message, 0, n)
	for i := range n {
		out = append(out, memory.Message{
			MessageID: uuid.New(), ConversationID: "nine|public", SequenceNumber: int64(n - i),
			AuthorKey: "agent:village", Role: "assistant", Content: fmt.Sprintf("souvenir %d", i),
			CreatedAt: base.Add(-time.Duration(i) * time.Minute),
			Metadata:  json.RawMessage(`{"importance":5}`),
		})
	}
	return out
}

func TestListMessagesPagesWithACursor(t *testing.T) {
	l := &stubLister{out: listedMessages(3)}
	h := newListServer(t, l)

	rec := post(h, "/v1/messages/list", "tok", `{
		"workspace_id":"ws1","requester_key":"agent:village",
		"conversation_ids":["nine|public"],
		"metadata_filter":{"key":"belief","op":"ne","value":"non"},
		"limit":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp listMessagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Messages) != 2 || resp.NextCursor == "" {
		t.Fatalf("page = %d messages, curseur %q", len(resp.Messages), resp.NextCursor)
	}
	if string(resp.Messages[0].Metadata) != `{"importance":5}` {
		t.Errorf("metadata = %s", resp.Messages[0].Metadata)
	}
	q := l.got[0]
	if q.Limit != 3 || len(q.ConversationIDs) != 1 || q.MetadataFilter == nil || q.BeforeAt != nil {
		t.Errorf("requête transmise = %+v", q)
	}

	rec = post(h, "/v1/messages/list", "tok", fmt.Sprintf(`{
		"workspace_id":"ws1","requester_key":"agent:village","cursor":%q}`, resp.NextCursor))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	q = l.got[1]
	second := l.out[1]
	if q.BeforeAt == nil || !q.BeforeAt.Equal(second.CreatedAt) || q.BeforeID != second.MessageID {
		t.Errorf("curseur relu = %v %v, want %v %v", q.BeforeAt, q.BeforeID, second.CreatedAt, second.MessageID)
	}
	if q.Limit != defaultListLimit+1 {
		t.Errorf("limite par défaut = %d", q.Limit)
	}
}

func TestListMessagesRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		want int
		l    Lister
	}{
		"sans lister":        {`{"workspace_id":"ws1","requester_key":"agent:village"}`, http.StatusNotImplemented, nil},
		"identité étrangère": {`{"workspace_id":"ws1","requester_key":"user:marie"}`, http.StatusForbidden, &stubLister{}},
		"autre workspace":    {`{"workspace_id":"ws2","requester_key":"agent:village"}`, http.StatusForbidden, &stubLister{}},
		"limite trop haute":  {`{"workspace_id":"ws1","requester_key":"agent:village","limit":501}`, http.StatusBadRequest, &stubLister{}},
		"limite négative":    {`{"workspace_id":"ws1","requester_key":"agent:village","limit":-1}`, http.StatusBadRequest, &stubLister{}},
		"curseur illisible":  {`{"workspace_id":"ws1","requester_key":"agent:village","cursor":"%%%"}`, http.StatusBadRequest, &stubLister{}},
		"filtre invalide": {`{"workspace_id":"ws1","requester_key":"agent:village",
			"metadata_filter":{"key":"a","op":"like","value":1}}`, http.StatusBadRequest, &stubLister{}},
		"conversation vide": {`{"workspace_id":"ws1","requester_key":"agent:village","conversation_ids":[""]}`, http.StatusBadRequest, &stubLister{}},
	} {
		rec := post(newListServer(t, c.l), "/v1/messages/list", "tok", c.body)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d: %s", name, rec.Code, c.want, rec.Body)
		}
	}
}

func TestSearchForwardsRestrictionsAndReturnsMetadata(t *testing.T) {
	h, _, _, find := newTestServer(t)
	anchor := uuid.New()
	find.res = memory.SearchResponse{Results: []memory.Result{{
		MemoryID: anchor.String(), ConversationID: "nine|public", AnchorMessageID: anchor,
		SourceMessageIDs: []uuid.UUID{anchor}, Content: "x", Scores: map[string]float64{"dense": 0.6},
		SourceType: "original_messages", Metadata: json.RawMessage(`{"importance":5}`),
	}}}

	rec := post(h, "/v1/memories/search", "tok", `{
		"workspace_id":"ws1","requester_key":"agent:village","query":"pomme",
		"conversation_ids":["nine|public","nine|lore"],
		"metadata_filter":{"any":[{"key":"min_affinity","op":"lte","value":40},
		                          {"key":"lore_id","op":"in","value":["nine-1"]}]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(find.got.ConversationIDs) != 2 || find.got.MetadataFilter == nil || len(find.got.MetadataFilter.Any) != 2 {
		t.Errorf("restrictions transmises = %v %+v", find.got.ConversationIDs, find.got.MetadataFilter)
	}
	if !strings.Contains(rec.Body.String(), `"metadata":{"importance":5}`) {
		t.Errorf("metadata absentes de la réponse: %s", rec.Body)
	}
}

func TestSearchRefusesBadRestrictions(t *testing.T) {
	h, _, _, _ := newTestServer(t)
	for name, extra := range map[string]string{
		"filtre invalide": `"metadata_filter":{"all":[]}`,
		"champ inconnu":   `"metadata_filter":{"key":"a","op":"eq","vlaue":1}`,
	} {
		rec := post(h, "/v1/memories/search", "tok",
			`{"workspace_id":"ws1","requester_key":"agent:village","query":"pomme",`+extra+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d: %s", name, rec.Code, rec.Body)
		}
	}
}

// Un habitant qui connaît beaucoup de joueurs a autant de conversations: la
// liste n'a pas d'autre borne que la taille de la requête.
func TestSearchAcceptsManyConversations(t *testing.T) {
	h, _, _, find := newTestServer(t)
	convs := make([]string, 200)
	for i := range convs {
		convs[i] = fmt.Sprintf("nine|player:%d", i)
	}
	many, _ := json.Marshal(convs)
	rec := post(h, "/v1/memories/search", "tok",
		`{"workspace_id":"ws1","requester_key":"agent:village","query":"pomme","conversation_ids":`+string(many)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(find.got.ConversationIDs) != 200 {
		t.Errorf("conversations transmises = %d", len(find.got.ConversationIDs))
	}
}
