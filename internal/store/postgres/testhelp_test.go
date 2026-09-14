package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

// newTestDatabaseDSN crée une base vierge sur le serveur POSTGRES_TEST_DSN et
// programme sa suppression en fin de test. Elle ne migre rien: c'est aux
// appelants de le faire s'ils en ont besoin. Le test est ignoré si
// POSTGRES_TEST_DSN est absent, ce qui garde `go test ./...` utilisable sans
// Docker.
func newTestDatabaseDSN(t *testing.T) string {
	t.Helper()
	base := os.Getenv("POSTGRES_TEST_DSN")
	if base == "" {
		t.Skip("POSTGRES_TEST_DSN absent, test d'intégration ignoré")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("connexion admin: %v", err)
	}
	defer admin.Close()

	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	dbName := "cinnabar_test_" + hex.EncodeToString(buf)

	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", dbName)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	// Enregistré tout de suite après la création, avant tout chemin d'échec
	// fatal des appelants (Open, Migrate...): sinon un t.Fatalf plus loin
	// laisserait la base orpheline pour toujours, faute de Cleanup programmé
	// pour la détruire.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		a, err := pgxpool.New(cctx, base)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(cctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", dbName))
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName
	return u.String()
}

// newTestPoolWithConns crée une base vierge, l'ouvre avec au plus maxConns
// connexions et applique les migrations dessus.
func newTestPoolWithConns(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	dsn := newTestDatabaseDSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := Open(ctx, config.Database{DSN: dsn, MaxConns: maxConns})
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return pool
}

// newTestPool crée une base vierge migrée et rend un pool à 4 connexions
// dessus, suffisant pour l'essentiel des tests d'intégration.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return newTestPoolWithConns(t, 4)
}
