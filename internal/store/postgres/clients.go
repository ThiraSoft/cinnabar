package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// ErrNoClient signale un token inconnu ou révoqué. Les deux cas rendent la
// même erreur, pour ne pas dire à un appelant si son token a existé.
var ErrNoClient = errors.New("unknown or revoked client")

type ClientRepo struct {
	pool *pgxpool.Pool
	// mu protège lastUsed, lue et écrite depuis chaque appel HTTP concurrent
	// à Resolve.
	mu sync.Mutex
	// lastUsed limite les écritures de last_used_at à une par minute et par
	// client, pour ne pas transformer chaque requête en écriture.
	lastUsed map[uuid.UUID]time.Time
}

func NewClientRepo(pool *pgxpool.Pool) *ClientRepo {
	return &ClientRepo{pool: pool, lastUsed: map[uuid.UUID]time.Time{}}
}

func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func (r *ClientRepo) Create(ctx context.Context, label, workspaceID string,
	identities []string) (string, uuid.UUID, error) {

	if label == "" || workspaceID == "" {
		return "", uuid.Nil, fmt.Errorf("label and workspace_id are required")
	}
	if len(identities) == 0 {
		return "", uuid.Nil, fmt.Errorf("at least one allowed identity is required")
	}
	for _, ident := range identities {
		if ident == "" {
			return "", uuid.Nil, fmt.Errorf("allowed identities must not contain an empty entry")
		}
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", uuid.Nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	idents, err := json.Marshal(identities)
	if err != nil {
		return "", uuid.Nil, err
	}

	id := uuid.New()
	_, err = r.pool.Exec(ctx, `
		INSERT INTO api_clients
			(client_id, label, token_sha256, workspace_id, allowed_identities)
		VALUES ($1, $2, $3, $4, $5)`,
		id, label, HashToken(token), workspaceID, idents)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("insert client: %w", err)
	}
	return token, id, nil
}

func (r *ClientRepo) Revoke(ctx context.Context, label string) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE api_clients SET revoked_at = now()
		WHERE label = $1 AND revoked_at IS NULL`, label)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *ClientRepo) Resolve(ctx context.Context, token string) (*memory.Principal, error) {
	if token == "" {
		return nil, ErrNoClient
	}
	hash := HashToken(token)

	var (
		p      memory.Principal
		stored []byte
		idents []byte
	)
	err := r.pool.QueryRow(ctx, `
		SELECT client_id, label, workspace_id, allowed_identities, token_sha256
		FROM api_clients
		WHERE token_sha256 = $1 AND revoked_at IS NULL`, hash,
	).Scan(&p.ClientID, &p.Label, &p.WorkspaceID, &idents, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoClient
	}
	if err != nil {
		return nil, fmt.Errorf("resolve client: %w", err)
	}
	if subtle.ConstantTimeCompare(hash, stored) != 1 {
		return nil, ErrNoClient
	}
	if err := json.Unmarshal(idents, &p.AllowedIdentities); err != nil {
		return nil, fmt.Errorf("decode allowed_identities: %w", err)
	}

	r.touch(ctx, p.ClientID)
	return &p, nil
}

func (r *ClientRepo) touch(ctx context.Context, id uuid.UUID) {
	now := time.Now()

	r.mu.Lock()
	last, ok := r.lastUsed[id]
	r.mu.Unlock()
	if ok && now.Sub(last) < time.Minute {
		return
	}

	// L'écriture en base est best-effort: si elle échoue, on ne pose pas le
	// marqueur, pour que le prochain Resolve retente plutôt que de laisser
	// last_used_at figé sur une écriture qui n'a jamais eu lieu.
	if _, err := r.pool.Exec(ctx,
		`UPDATE api_clients SET last_used_at = now() WHERE client_id = $1`, id); err != nil {
		return
	}

	r.mu.Lock()
	r.lastUsed[id] = now
	r.mu.Unlock()
}
