// Package llm parle à un endpoint OpenAI-compatible. Le même client sert pour
// les embeddings et pour les complétions de chat, avec deux
// configurations distinctes.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

// Client parle à un endpoint OpenAI-compatible. Une même instance sert soit
// les embeddings, soit les complétions: elle est construite via NewEmbedder
// ou NewChat, jamais directement.
type Client struct {
	baseURL     string
	model       string
	apiKey      string
	headers     map[string]string
	docPrefix   string
	queryPrefix string
	normalize   bool
	http        *http.Client
	maxTokens   int
}

// NewEmbedder construit un client dédié aux embeddings (Ollama en pratique).
func NewEmbedder(cfg config.Embedding) *Client {
	return &Client{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		model:       cfg.Model,
		docPrefix:   cfg.DocumentPrefix,
		queryPrefix: cfg.QueryPrefix,
		normalize:   cfg.Normalize,
		http:        &http.Client{Timeout: cfg.Timeout},
	}
}

// NewChat construit un client dédié aux complétions de chat (extraction
// et rerank).
func NewChat(cfg config.Extraction) *Client {
	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   cfg.Model,
		apiKey:  cfg.APIKey,
		headers: cfg.Headers,
		http:    &http.Client{Timeout: cfg.Timeout},

		maxTokens: cfg.MaxTokens,
	}
}

// Model renvoie le nom du modèle configuré.
func (c *Client) Model() string { return c.model }

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path,
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("post %s%s: %w", c.baseURL, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("post %s%s: status %d: %s",
			c.baseURL, path, resp.StatusCode, describeErrorBody(resp.Body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s%s response: %w", c.baseURL, path, err)
	}
	return nil
}

// describeErrorBody rend une description d'erreur sûre à journaliser.
//
// Le corps brut ne peut pas y aller. Un fournisseur compatible OpenAI y
// recopie volontiers tout ou partie de la requête, et la requête porte ici
// soit le texte d'un message à encoder, soit le prompt d'extraction qui
// contient plusieurs messages entiers. Cette erreur remonte jusqu'au log du
// runner de jobs, où la contrainte du projet est qu'aucune ligne ne porte de
// contenu de message.
//
// Ce qu'on garde: le type et le code de l'enveloppe d'erreur OpenAI, deux
// valeurs énumérées par le fournisseur, jamais le message qui les
// accompagne. C'est assez pour distinguer un modèle inconnu d'un contexte
// dépassé ou d'une clé refusée. Quand le corps n'a pas cette forme, on ne
// rend que sa taille: un opérateur qui a besoin de plus lit les logs du
// fournisseur, qui est l'endroit où un message d'erreur du fournisseur a sa
// place.
func describeErrorBody(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return "unreadable error body"
	}

	var env struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return fmt.Sprintf("opaque error body of %d bytes", len(raw))
	}
	switch {
	case env.Error.Type != "" && env.Error.Code != "":
		return fmt.Sprintf("%s (%s)", env.Error.Type, env.Error.Code)
	case env.Error.Type != "":
		return env.Error.Type
	case env.Error.Code != "":
		return env.Error.Code
	default:
		return fmt.Sprintf("opaque error body of %d bytes", len(raw))
	}
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed encode des documents. Le préfixe document est ajouté ici et n'est
// jamais persisté. Les vecteurs sont remis dans l'ordre des entrées: le
// serveur peut répondre dans un autre ordre, et une inversion silencieuse
// associerait des embeddings aux mauvais messages.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	return c.embed(ctx, c.docPrefix, inputs)
}

// EmbedQuery encode une requête. Les modèles Nomic attendent un préfixe
// différent de celui des documents, et se tromper de préfixe dégrade
// silencieusement le rappel. C'est pour ça que l'appelant ne compose jamais le
// préfixe lui-même: il choisit la méthode.
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.embed(ctx, c.queryPrefix, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

func (c *Client) embed(ctx context.Context, prefix string,
	inputs []string) ([][]float32, error) {

	if len(inputs) == 0 {
		return nil, nil
	}
	payload := make([]string, len(inputs))
	for i, in := range inputs {
		payload[i] = prefix + in
	}

	var resp embedResponse
	err := c.post(ctx, "/embeddings", map[string]any{
		"model": c.model,
		"input": payload,
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(resp.Data) != len(inputs) {
		return nil, fmt.Errorf("embed: got %d vectors for %d inputs",
			len(resp.Data), len(inputs))
	}

	out := make([][]float32, len(inputs))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(inputs) {
			return nil, fmt.Errorf("embed: index %d out of range", d.Index)
		}
		v := d.Embedding
		if c.normalize {
			v = normalize(v)
		}
		out[d.Index] = v
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("embed: missing vector for input %d", i)
		}
	}
	return out, nil
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// ChatRequest décrit une requête de complétion. JSONSchema est optionnel: il
// n'est utilisé que par l'extracteur de graphe (plan ultérieur), pour forcer
// une sortie structurée via response_format.
type ChatRequest struct {
	System      string
	User        string
	Temperature float64
	JSONSchema  json.RawMessage // optionnel, pour response_format
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Chat envoie une complétion de chat et renvoie le contenu du premier choix.
func (c *Client) Chat(ctx context.Context, r ChatRequest) (string, error) {
	msgs := []map[string]string{}
	if r.System != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": r.System})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": r.User})

	body := map[string]any{
		"model":       c.model,
		"messages":    msgs,
		"temperature": r.Temperature,
	}
	if c.maxTokens > 0 {
		body["max_tokens"] = c.maxTokens
	}
	if len(r.JSONSchema) > 0 {
		body["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": json.RawMessage(r.JSONSchema),
		}
	}

	var resp chatResponse
	if err := c.post(ctx, "/chat/completions", body, &resp); err != nil {
		return "", fmt.Errorf("chat: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("chat: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}
