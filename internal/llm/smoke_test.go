package llm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

// TestEmbedAgainstRealOllama vérifie le client contre un vrai serveur Ollama
// local. Elle est ignorée par défaut car elle dépend d'un service externe;
// mettre OLLAMA_SMOKE=1 pour l'exécuter réellement.
func TestEmbedAgainstRealOllama(t *testing.T) {
	if os.Getenv("OLLAMA_SMOKE") != "1" {
		t.Skip("set OLLAMA_SMOKE=1 to run against a real Ollama instance")
	}

	c := NewEmbedder(config.Embedding{
		BaseURL:        "http://localhost:11434/v1",
		Model:          "nomic-embed-text-v2-moe",
		DocumentPrefix: "search_document: ",
		Normalize:      true,
		Timeout:        30 * time.Second,
	})

	v, err := c.Embed(context.Background(), []string{"mes tomates sont vertes"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != 1 {
		t.Fatalf("got %d vectors, want 1", len(v))
	}
	got := len(v[0])
	t.Logf("dimensions: %d", got)
	if got != 768 {
		t.Errorf("dimensions: got %d, want 768", got)
	}
}
