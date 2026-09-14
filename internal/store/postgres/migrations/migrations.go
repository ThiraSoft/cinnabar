// Package migrations embarque et applique les migrations SQL.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var files embed.FS

const bootstrap = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    name       TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

// migrationLockKey est la clé du verrou consultatif Postgres qui sérialise
// Apply entre plusieurs processus migrant la même base au même moment (par
// exemple plusieurs réplicas qui démarrent ensemble). Valeur arbitraire,
// dérivée des octets ASCII de "potato" (ancien nom du projet), sans autre
// signification que d'être peu susceptible d'entrer en collision avec un autre verrou consultatif de
// la même base. Elle ne doit plus jamais changer une fois déployée: la
// changer romprait la mutuelle exclusion avec un processus qui tournerait
// encore l'ancienne valeur.
const migrationLockKey int64 = 123623996224623

// Apply applique les migrations non encore appliquées, par ordre de nom, et
// indique si au moins une migration a été jouée. Chaque migration tourne
// dans sa propre transaction avec la ligne de suivi, donc une migration qui
// échoue ne laisse pas le schéma à moitié appliqué.
//
// L'ensemble de la fonction tourne sous un verrou consultatif Postgres pris
// sur une connexion dédiée acquise du pool, pour que deux processus qui
// appellent Apply en même temps sur la même base ne se marchent pas dessus:
// sans ça, CREATE TABLE IF NOT EXISTS n'est pas à l'abri d'une course entre
// sessions concurrentes, et la vérification "déjà appliquée" tourne hors de
// la transaction qui applique réellement la migration, donc les deux
// processus peuvent la lire à faux avant que l'un des deux ne committe.
//
// Toutes les requêtes qui suivent, y compris le bootstrap et les migrations
// elles-mêmes, passent par cette même connexion plutôt que par le pool: un
// pool ouvert avec une seule connexion (les tests d'écriture de vecteurs en
// ont besoin ailleurs dans ce paquet) se bloquerait sinon indéfiniment, la
// connexion unique restant accaparée par le verrou pendant qu'une requête
// via le pool en attendrait une autre.
func Apply(ctx context.Context, pool *pgxpool.Pool) (applied bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer conn.Release()

	if _, lockErr := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); lockErr != nil {
		return false, fmt.Errorf("acquire migration lock: %w", lockErr)
	}
	defer func() {
		// Libération sur la même connexion que celle qui a pris le verrou:
		// un pg_advisory_unlock sur une autre connexion ne libère rien
		// (verrou consultatif de session) et le laisserait fuir jusqu'à la
		// fermeture de cette connexion.
		if _, unlockErr := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("release migration lock: %w", unlockErr)
		}
	}()

	if _, bootErr := conn.Exec(ctx, bootstrap); bootErr != nil {
		return false, fmt.Errorf("bootstrap schema_migrations: %w", bootErr)
	}

	entries, err := files.ReadDir(".")
	if err != nil {
		return false, fmt.Errorf("read embedded migrations directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var exists bool
		scanErr := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name,
		).Scan(&exists)
		if scanErr != nil {
			return applied, fmt.Errorf("check migration %s already applied: %w", name, scanErr)
		}
		if exists {
			continue
		}

		body, readErr := files.ReadFile(name)
		if readErr != nil {
			return applied, fmt.Errorf("read migration %s: %w", name, readErr)
		}
		tx, beginErr := conn.Begin(ctx)
		if beginErr != nil {
			return applied, fmt.Errorf("begin migration %s: %w", name, beginErr)
		}
		if _, execErr := tx.Exec(ctx, string(body)); execErr != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("migration %s: %w", name, execErr)
		}
		if _, insertErr := tx.Exec(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`, name); insertErr != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("record migration %s: %w", name, insertErr)
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return applied, fmt.Errorf("migration %s commit: %w", name, commitErr)
		}
		applied = true
	}
	return applied, nil
}
