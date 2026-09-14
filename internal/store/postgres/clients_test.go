package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestClientRepoCreateAndResolve(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewClientRepo(pool)

	token, id, err := repo.Create(ctx, "orchestrateur", "ws1", []string{"agent:*"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(token) < 32 {
		t.Errorf("token trop court: %d caractères", len(token))
	}

	p, err := repo.Resolve(ctx, token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.ClientID != id {
		t.Errorf("client_id = %v, want %v", p.ClientID, id)
	}
	if p.WorkspaceID != "ws1" {
		t.Errorf("workspace = %q", p.WorkspaceID)
	}
	if len(p.AllowedIdentities) != 1 || p.AllowedIdentities[0] != "agent:*" {
		t.Errorf("identities = %v", p.AllowedIdentities)
	}
}

// TestClientRepoTokenIsNotStoredInClear vérifie deux choses. D'abord que la
// ligne existe bien sous son hash (sinon le test suivant serait vide de
// sens): exactement une ligne a pour token_sha256 le hash du token en clair.
// Ensuite, et c'est le point qui compte, que le token en clair n'apparaît nulle
// part dans la ligne, colonne par colonne confondues: on relit toute la ligne
// castée en texte et on cherche le token en clair dedans comme sous-chaîne. Une
// comparaison directe sur token_sha256 ne peut détecter qu'une seule erreur
// d'implémentation précise (stocker le token en clair dans cette colonne);
// celle-ci détecte une fuite dans n'importe quelle colonne, présente ou future.
func TestClientRepoTokenIsNotStoredInClear(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewClientRepo(pool)

	token, id, err := repo.Create(ctx, "c", "ws1", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM api_clients WHERE token_sha256 = $1::bytea`,
		HashToken(token)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("lignes avec token_sha256 = HashToken(token): got %d, want 1", n)
	}

	var row string
	if err := pool.QueryRow(ctx,
		`SELECT api_clients::text FROM api_clients WHERE client_id = $1`,
		id).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, token) {
		t.Error("le token en clair ne doit jamais apparaître en base, dans aucune colonne")
	}
}

// TestClientRepoResolveIsolatesWorkspaces est l'histoire de l'isolation côté
// stockage: deux clients de deux workspaces différents, chacun résolu avec
// son propre workspace, jamais celui de l'autre.
func TestClientRepoResolveIsolatesWorkspaces(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewClientRepo(pool)

	tokenA, _, err := repo.Create(ctx, "client-a", "ws-a", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	tokenB, _, err := repo.Create(ctx, "client-b", "ws-b", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}

	pA, err := repo.Resolve(ctx, tokenA)
	if err != nil {
		t.Fatalf("Resolve client A: %v", err)
	}
	if pA.WorkspaceID != "ws-a" {
		t.Errorf("client A: workspace = %q, want ws-a", pA.WorkspaceID)
	}

	pB, err := repo.Resolve(ctx, tokenB)
	if err != nil {
		t.Fatalf("Resolve client B: %v", err)
	}
	if pB.WorkspaceID != "ws-b" {
		t.Errorf("client B: workspace = %q, want ws-b", pB.WorkspaceID)
	}
}

func TestClientRepoResolveUnknownToken(t *testing.T) {
	pool := newTestPool(t)
	repo := NewClientRepo(pool)
	if _, err := repo.Resolve(context.Background(), "nope"); !errors.Is(err, ErrNoClient) {
		t.Fatalf("token inconnu: got %v, want ErrNoClient", err)
	}
}

func TestClientRepoRevokedTokenIsRejected(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewClientRepo(pool)

	token, _, err := repo.Create(ctx, "temporaire", "ws1", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := repo.Revoke(ctx, "temporaire")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Revoke a touché %d lignes, want 1", n)
	}
	if _, err := repo.Resolve(ctx, token); !errors.Is(err, ErrNoClient) {
		t.Fatalf("token révoqué: got %v, want ErrNoClient", err)
	}
}

// TestClientRepoResolveConcurrent lance de nombreux appels concurrents à
// Resolve sur le même client. touch() lit et écrit la map lastUsed depuis
// chaque appel: sans mutex sur cette map, -race le détecte comme accès
// concurrent non protégé. La fenêtre de course réelle est étroite (seule la
// toute première écriture par client est disputée, les lectures suivantes
// dans la même minute ne s'accompagnent plus d'écriture), donc le test répète
// l'expérience sur plusieurs clients fraîchement créés pour multiplier les
// chances de heurter cette fenêtre au moins une fois. Ce test échoue sous
// -race si le mutex est retiré de ClientRepo, ce qui est le but: prouver que
// la protection est nécessaire, pas seulement présente.
func TestClientRepoResolveConcurrent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewClientRepo(pool)

	const rounds = 20
	const goroutines = 50

	for round := 0; round < rounds; round++ {
		token, _, err := repo.Create(ctx, fmt.Sprintf("concurrent-%d", round), "ws1", []string{"*"})
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		errs := make([]error, goroutines)
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = repo.Resolve(ctx, token)
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d, goroutine %d: Resolve a échoué: %v", round, i, err)
			}
		}
	}
}
