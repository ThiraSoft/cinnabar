package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// Ops implémente api.Ops sur PostgreSQL: les routes d'exploitation
// (/health, /debug/stats) et la vérification de démarrage de la dimension
// d'embedding.
type Ops struct {
	pool *pgxpool.Pool
	jobs *JobRepo
}

func NewOps(pool *pgxpool.Pool, jobs *JobRepo) *Ops {
	return &Ops{pool: pool, jobs: jobs}
}

// Health ne sonde que Postgres. Une panne de l'embedder ou de l'extracteur ne
// rend pas le service indisponible: la recherche dégrade par stratégie et
// l'ingestion continue en mode eventual, donc les sonder ferait redémarrer un
// service par ailleurs parfaitement fonctionnel pour une dépendance dont la
// panne est déjà absorbée plus haut dans la pile.
func (o *Ops) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return o.pool.Ping(ctx)
}

// Stats agrège les compteurs exposés par /debug/stats dans memory.Stats. Le
// compte de lettres mortes est dérivé de la même lecture de la profondeur de
// file que queue_depth, plutôt que d'une seconde requête séparée sur jobs:
// une seule source de vérité pour ces deux nombres.
func (o *Ops) Stats(ctx context.Context) (memory.Stats, error) {
	depth, err := o.jobs.Depth(ctx)
	if err != nil {
		return memory.Stats{}, err
	}
	queueDepth := make(map[string]memory.QueueDepth, len(depth))
	var dead int
	for jobType, d := range depth {
		queueDepth[jobType] = memory.QueueDepth{Pending: d.Pending, Dead: d.Dead}
		dead += d.Dead
	}

	var units, messages, conversations int
	err = o.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM memory_units WHERE active),
			(SELECT count(*) FROM messages WHERE deleted_at IS NULL),
			(SELECT count(*) FROM conversations WHERE deleted_at IS NULL)`,
	).Scan(&units, &messages, &conversations)
	if err != nil {
		return memory.Stats{}, err
	}

	stat := o.pool.Stat()
	return memory.Stats{
		QueueDepth:    queueDepth,
		DeadJobs:      dead,
		MemoryUnits:   units,
		Messages:      messages,
		Conversations: conversations,
		DBPool: memory.DBPoolStats{
			Acquired: stat.AcquiredConns(),
			Idle:     stat.IdleConns(),
			Total:    stat.TotalConns(),
		},
	}, nil
}

// ErrEmbeddingProbeUnavailable distingue une sonde qui n'a pas pu atteindre
// le modèle d'une sonde qui a répondu avec la mauvaise dimension. Les deux
// remontaient jusqu'ici sous une même erreur, et run() les traitait toutes
// les deux comme fatales: une simple panne d'Ollama empêchait donc n'importe
// quel réplica de redémarrer. Or la spec ne demande de refuser le démarrage
// que sur un écart de dimension (section 4.5); la posture retenue sur
// l'embedder (sections 5.5 et 8.2, critère d'acceptation 10) est au
// contraire que sa panne dégrade la recherche et l'ingestion sans rendre le
// service indisponible.
//
// Couvre aussi la réponse malformée (un nombre de vecteurs inattendu): elle
// ne dit rien de la dimension, donc elle ne peut pas prouver un écart, et
// n'a pas à empêcher un démarrage.
var ErrEmbeddingProbeUnavailable = errors.New("embedding probe could not reach the model")

// VerifyEmbeddingDimension compare trois nombres qui doivent s'accorder: la
// dimension réellement rendue par le modèle, celle déclarée par
// embedding.dimensions dans la configuration, et celle de la colonne
// memory_units.embedding en base (VECTOR(n), lue depuis pg_attribute, pas
// recopiée depuis la config). Comparer seulement le modèle à `expected`
// laisserait passer un opérateur qui aligne honnêtement sa configuration sur
// un nouveau modèle sans avoir migré la colonne: la vérification rendrait
// alors vert un service qui échouera à la première écriture avec une erreur
// Postgres obscure, exactement ce qu'elle existe pour empêcher.
func VerifyEmbeddingDimension(ctx context.Context, pool *pgxpool.Pool,
	emb memory.Embedder, expected int) error {

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var schemaDim int
	if err := pool.QueryRow(ctx, `
		SELECT atttypmod FROM pg_attribute
		WHERE attrelid = 'memory_units'::regclass AND attname = 'embedding'`,
	).Scan(&schemaDim); err != nil {
		return fmt.Errorf("read memory_units.embedding column dimension: %w", err)
	}

	vecs, err := emb.Embed(ctx, []string{"dimension probe"})
	if err != nil {
		return fmt.Errorf("%w: model %q: %w", ErrEmbeddingProbeUnavailable,
			emb.Model(), err)
	}
	if len(vecs) != 1 {
		return fmt.Errorf("%w: model %q returned %d vectors instead of 1",
			ErrEmbeddingProbeUnavailable, emb.Model(), len(vecs))
	}
	modelDim := len(vecs[0])

	if modelDim != schemaDim || expected != schemaDim {
		return fmt.Errorf(
			"embedding dimension mismatch: model %q renders %d dimensions, "+
				"config embedding.dimensions is %d, schema column "+
				"memory_units.embedding is VECTOR(%d): all three must agree "+
				"before the service can write",
			emb.Model(), modelDim, expected, schemaDim)
	}
	return nil
}
