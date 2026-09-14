package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

type stubConversationLister struct {
	all []string
	got []string // prefix|after|limit de chaque appel
}

func (s *stubConversationLister) List(_ context.Context, ws, requester, prefix, after string,
	limit int) ([]memory.ConversationSummary, error) {
	s.got = append(s.got, fmt.Sprintf("%s|%s|%s|%s|%d", ws, requester, prefix, after, limit))
	var out []memory.ConversationSummary
	for _, id := range s.all {
		if id > after && len(out) < limit {
			out = append(out, memory.ConversationSummary{ConversationID: id, Scope: "workspace",
				UpdatedAt: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)})
		}
	}
	return out, nil
}

func newConversationListServer(t *testing.T, l ConversationLister) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Service: config.Service{MaxRequestBytes: 1 << 20, ConsistencyDefault: "eventual"},
		Access:  config.Access{DefaultScope: "participants"},
	}
	res := &stubResolver{principal: &memory.Principal{
		ClientID: uuid.New(), Label: "village",
		WorkspaceID: "ws1", AllowedIdentities: []string{"agent:*"},
	}}
	srv := NewServer(cfg, res, &stubIngester{}, &stubEditor{}, &stubFinder{},
		&stubConversations{}, &stubOps{})
	if l != nil {
		srv.WithConversationLister(l)
	}
	return srv.Handler()
}

func getConversations(h http.Handler, q url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/conversations?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestListConversationsPages(t *testing.T) {
	l := &stubConversationLister{all: []string{"nine|a", "nine|b", "nine|c"}}
	h := newConversationListServer(t, l)
	q := url.Values{"workspace_id": {"ws1"}, "requester_key": {"agent:village"},
		"prefix": {"nine|"}, "limit": {"2"}}

	rec := getConversations(h, q)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var page struct {
		Conversations []struct {
			ConversationID string `json:"conversation_id"`
			Scope          string `json:"scope"`
		} `json:"conversations"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Conversations) != 2 || page.Conversations[1].ConversationID != "nine|b" ||
		page.Conversations[0].Scope != "workspace" || page.NextCursor == "" {
		t.Fatalf("première page = %s", rec.Body)
	}
	if l.got[0] != "ws1|agent:village|nine|||3" {
		t.Errorf("appel = %s", l.got[0])
	}

	q.Set("cursor", page.NextCursor)
	rec = getConversations(h, q)
	page.NextCursor = ""
	page.Conversations = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Conversations) != 1 || page.Conversations[0].ConversationID != "nine|c" || page.NextCursor != "" {
		t.Errorf("seconde page = %s", rec.Body)
	}
}

func TestListConversationsRefuses(t *testing.T) {
	base := url.Values{"workspace_id": {"ws1"}, "requester_key": {"agent:village"}}
	with := func(k, v string) url.Values {
		q := url.Values{}
		for kk, vv := range base {
			q[kk] = vv
		}
		q.Set(k, v)
		return q
	}
	for name, c := range map[string]struct {
		q    url.Values
		l    ConversationLister
		want int
	}{
		"autre workspace":  {with("workspace_id", "ws2"), &stubConversationLister{}, http.StatusForbidden},
		"identité refusée": {with("requester_key", "user:paul"), &stubConversationLister{}, http.StatusForbidden},
		"sans demandeur":   {with("requester_key", ""), &stubConversationLister{}, http.StatusBadRequest},
		"limite":           {with("limit", "501"), &stubConversationLister{}, http.StatusBadRequest},
		"limite illisible": {with("limit", "deux"), &stubConversationLister{}, http.StatusBadRequest},
		"curseur":          {with("cursor", "!!"), &stubConversationLister{}, http.StatusBadRequest},
		"non câblé":        {base, nil, http.StatusNotImplemented},
	} {
		if rec := getConversations(newConversationListServer(t, c.l), c.q); rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d: %s", name, rec.Code, c.want, rec.Body)
		}
	}
}
