package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

func TestEmbedSendsPrefixedInputsAndParsesVectors(t *testing.T) {
	var gotPath, gotAuth, gotHeader string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotHeader = r.Header.Get("x-custom-header")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"index":1,"embedding":[0.5,0.5]},
			{"index":0,"embedding":[1,0]}
		]}`))
	}))
	defer srv.Close()

	c := NewEmbedder(config.Embedding{
		BaseURL: srv.URL, Model: "nomic-embed-text-v2-moe",
		DocumentPrefix: "search_document: ", Normalize: false,
		Timeout: 5 * time.Second,
	})

	vecs, err := c.Embed(context.Background(), []string{"un", "deux"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/embeddings" {
		t.Errorf("path = %q, want /embeddings", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("pas de clé configurée donc pas d'en-tête Authorization, got %q", gotAuth)
	}
	if gotHeader != "" {
		t.Errorf("pas d'en-tête custom attendu, got %q", gotHeader)
	}
	inputs, _ := gotBody["input"].([]any)
	if len(inputs) != 2 || inputs[0] != "search_document: un" {
		t.Errorf("input = %v, le préfixe document doit être ajouté", inputs)
	}
	// Les résultats doivent être remis dans l'ordre des entrées, pas dans
	// l'ordre de la réponse.
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][0] != 0.5 {
		t.Errorf("vecteurs mal réordonnés: %v", vecs)
	}
}

func TestEmbedNormalizes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[3,4]}]}`))
	}))
	defer srv.Close()

	c := NewEmbedder(config.Embedding{
		BaseURL: srv.URL, Normalize: true, Timeout: 5 * time.Second,
	})
	vecs, err := c.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := vecs[0][0]; got < 0.59 || got > 0.61 {
		t.Errorf("normalisation: got %v, want ~0.6", got)
	}
}

func TestEmbedQueryUsesTheQueryPrefix(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()

	c := NewEmbedder(config.Embedding{
		BaseURL: srv.URL, DocumentPrefix: "search_document: ",
		QueryPrefix: "search_query: ", Timeout: 5 * time.Second,
	})

	if _, err := c.EmbedQuery(context.Background(), "les tomates de Paul"); err != nil {
		t.Fatal(err)
	}
	inputs, _ := gotBody["input"].([]any)
	if len(inputs) != 1 || inputs[0] != "search_query: les tomates de Paul" {
		t.Errorf("input = %v, le préfixe de requête doit être utilisé seul", inputs)
	}
}

func TestChatSendsCustomHeadersAndReturnsContent(t *testing.T) {
	var gotAuth, gotHeader, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotHeader = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-custom-header")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer srv.Close()

	c := NewChat(config.Extraction{
		BaseURL: srv.URL, Model: "google/gemma-4-31B-it", APIKey: "tok",
		Headers: map[string]string{"x-custom-header": "qualif"}, Timeout: 5 * time.Second,
	})

	out, err := c.Chat(context.Background(), ChatRequest{
		System: "s", User: "u", Temperature: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotHeader != "qualif" {
		t.Errorf("x-custom-header = %q", gotHeader)
	}
	if out != `{"ok":true}` {
		t.Errorf("content = %q", out)
	}
}

func TestHTTPErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	c := NewEmbedder(config.Embedding{BaseURL: srv.URL, Timeout: 5 * time.Second})
	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("un 500 doit produire une erreur")
	}
}

func TestEmbedCountMismatchIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer srv.Close()

	c := NewEmbedder(config.Embedding{BaseURL: srv.URL, Timeout: 5 * time.Second})
	if _, err := c.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("un nombre de vecteurs différent du nombre d'entrées doit être une erreur")
	}
}

// TestHTTPErrorNeRecopiePasLeCorps couvre le constat de la revue de la tâche
// 6: l'erreur d'un appel HTTP remonte jusqu'au log du runner de jobs, et un
// fournisseur compatible OpenAI recopie volontiers la requête dans son corps
// d'erreur. La requête porte ici soit un message à encoder, soit un prompt
// d'extraction fait de plusieurs messages entiers. Aucune ligne de log ne
// doit porter de contenu de message.
func TestHTTPErrorNeRecopiePasLeCorps(t *testing.T) {
	const secret = "les tomates de Paul sont vertes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"cannot embed ` + secret +
			`","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	}))
	defer srv.Close()

	c := NewEmbedder(config.Embedding{BaseURL: srv.URL, Timeout: 5 * time.Second})
	_, err := c.Embed(context.Background(), []string{secret})
	if err == nil {
		t.Fatal("un 400 doit produire une erreur")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("l'erreur recopie le contenu du message: %v", err)
	}
	// Ce qui reste doit suffire à diagnostiquer.
	for _, want := range []string{"400", "invalid_request_error", "context_length_exceeded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("l'erreur ne porte pas %q: %v", want, err)
		}
	}
}

// Un corps qui n'a pas la forme d'une enveloppe d'erreur OpenAI ne doit pas
// non plus être recopié: c'est le cas d'un proxy ou d'un load balancer qui
// répond du HTML, et rien ne garantit ce qu'il y met.
func TestHTTPErrorSurUnCorpsOpaqueNeRendQueSaTaille(t *testing.T) {
	const secret = "les tomates de Paul sont vertes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>upstream refused: " + secret + "</body></html>"))
	}))
	defer srv.Close()

	c := NewEmbedder(config.Embedding{BaseURL: srv.URL, Timeout: 5 * time.Second})
	_, err := c.Embed(context.Background(), []string{secret})
	if err == nil {
		t.Fatal("un 502 doit produire une erreur")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("l'erreur recopie un corps opaque: %v", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("l'erreur ne porte pas le code HTTP: %v", err)
	}
}
