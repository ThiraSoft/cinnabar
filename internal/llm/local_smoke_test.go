package llm

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

// TestLocalEmbedder charge le GGUF nommé par CINNABAR_EMBED_MODEL et encode
// dans le processus. CINNABAR_EMBED_DEVICE=cpu reste sur le processeur.
func TestLocalEmbedder(t *testing.T) {
	path := os.Getenv("CINNABAR_EMBED_MODEL")
	if path == "" {
		t.Skip("set CINNABAR_EMBED_MODEL to a nomic-embed-text-v2-moe GGUF")
	}
	device := os.Getenv("CINNABAR_EMBED_DEVICE")
	if device == "" {
		device = config.EmbeddingDeviceVulkan
	}
	e, err := NewLocalEmbedder(config.Embedding{
		Provider: config.EmbeddingProviderGolem, ModelPath: path, Device: device,
		DocumentPrefix: "search_document: ", QueryPrefix: "search_query: ",
		Normalize: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()
	docs, err := e.Embed(ctx, []string{"mes tomates sont vertes", "le train part à 8h"})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || len(docs[0]) != 768 {
		t.Fatalf("got %d vectors of %d dims, want 2 of 768", len(docs), len(docs[0]))
	}
	q, err := e.EmbedQuery(ctx, "de quelle couleur sont les tomates ?")
	if err != nil {
		t.Fatal(err)
	}
	var norm float64
	for _, x := range q {
		norm += float64(x) * float64(x)
	}
	if math.Abs(norm-1) > 1e-4 {
		t.Errorf("norme au carré %f, want 1", norm)
	}
	if cos(q, docs[0]) <= cos(q, docs[1]) {
		t.Errorf("la requête devrait préférer le document sur les tomates: %f <= %f",
			cos(q, docs[0]), cos(q, docs[1]))
	}
}

func cos(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
