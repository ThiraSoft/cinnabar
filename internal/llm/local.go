package llm

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ThiraSoft/golem/nomic"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

// Embedder est ce que le service attend d'un embedder, qu'il passe par HTTP
// ou tourne dans le processus. Close libère le modèle local; pour le client
// HTTP il ne fait rien.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	Model() string
	Close() error
}

// OpenEmbedder construit l'embedder que la configuration demande:
// embedding.provider "http" parle à un endpoint OpenAI-compatible, "golem"
// charge le GGUF en mémoire et encode sans aucune requête.
func OpenEmbedder(cfg config.Embedding) (Embedder, error) {
	if cfg.Provider == config.EmbeddingProviderGolem {
		return NewLocalEmbedder(cfg)
	}
	return NewEmbedder(cfg), nil
}

// Close ne fait rien: le client HTTP ne tient aucune ressource à libérer.
func (c *Client) Close() error { return nil }

// LocalEmbedder encode avec golem dans le processus, sur le CPU ou sur une
// carte Vulkan. Seul nomic-embed-text-v2-moe est pris en charge, c'est le
// modèle que golem sait faire tourner.
type LocalEmbedder struct {
	m           *nomic.Model
	name        string
	docPrefix   string
	queryPrefix string
	normalize   bool
}

// NewLocalEmbedder charge le modèle. Un device vulkan qui ne s'ouvre pas est
// une erreur, pas un repli sur le CPU: une capacité prévue pour la carte ne
// tiendrait pas sur le processeur.
func NewLocalEmbedder(cfg config.Embedding) (*LocalEmbedder, error) {
	m, err := nomic.Open(cfg.ModelPath)
	if err != nil {
		return nil, fmt.Errorf("open embedding model %s: %w", cfg.ModelPath, err)
	}
	if cfg.Device == config.EmbeddingDeviceVulkan {
		if err := m.UseVulkan(); err != nil {
			m.Close()
			return nil, fmt.Errorf("embedding model on vulkan: %w", err)
		}
	}
	name := cfg.Model
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(cfg.ModelPath), ".gguf")
	}
	return &LocalEmbedder{
		m:           m,
		name:        name,
		docPrefix:   cfg.DocumentPrefix,
		queryPrefix: cfg.QueryPrefix,
		normalize:   cfg.Normalize,
	}, nil
}

// Model renvoie le nom configuré, ou le nom du fichier à défaut.
func (e *LocalEmbedder) Model() string { return e.name }

// Close libère le modèle et la carte le cas échéant.
func (e *LocalEmbedder) Close() error { return e.m.Close() }

// Embed encode des documents avec le préfixe document, comme Client.Embed.
func (e *LocalEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	return e.embed(ctx, e.docPrefix, inputs)
}

// EmbedQuery encode une requête avec le préfixe requête.
func (e *LocalEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.embed(ctx, e.queryPrefix, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// embed ne peut pas interrompre une passe en cours: golem n'expose pas
// d'annulation. Le contexte n'est consulté qu'avant de lancer la passe, qui
// se compte en millisecondes pour un lot de messages.
func (e *LocalEmbedder) embed(ctx context.Context, prefix string,
	inputs []string) ([][]float32, error) {

	if len(inputs) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	ids := make([][]int32, len(inputs))
	for i, in := range inputs {
		ids[i] = e.m.Tokenize(prefix + in)
	}
	vecs, err := e.m.Embed(ids)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if e.normalize {
		for _, v := range vecs {
			nomic.Normalize(v)
		}
	}
	return vecs, nil
}
