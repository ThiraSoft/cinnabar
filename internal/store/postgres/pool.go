// Package postgres implémente les repos du domaine sur PostgreSQL.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres/migrations"
)

// Open ouvre un pool de connexions vers cfg.DSN. Chaque connexion enregistre
// le type vector de pgvector via AfterConnect, ce qui le rend décodable dès
// la première requête sur cette connexion.
func Open(ctx context.Context, cfg config.Database) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MaxConnLifetime > 0 {
		pc.MaxConnLifetime = cfg.MaxConnLifetime
	}
	pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// Sur une base toute neuve, l'extension vector n'existe pas encore tant
		// que Migrate n'a pas tourné. Dans ce cas on n'enregistre rien: il n'y a
		// de toute façon aucune donnée vector à décoder avant la migration, et
		// les connexions ouvertes après coup l'enregistreront normalement.
		var installed bool
		if err := conn.QueryRow(ctx, `SELECT to_regtype('vector') IS NOT NULL`).Scan(&installed); err != nil {
			return fmt.Errorf("check vector extension: %w", err)
		}
		if !installed {
			return nil
		}
		return pgxvec.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Migrate applique les migrations SQL embarquées sur pool.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	applied, err := migrations.Apply(ctx, pool)
	if err != nil {
		return err
	}
	if applied {
		// Au moins une migration vient de tourner, ce qui a pu créer
		// l'extension vector. Les connexions déjà ouvertes dans le pool
		// (typiquement celle du Ping fait par Open, sur une base toute
		// neuve) l'ont manquée et n'ont donc pas le type enregistré: Reset
		// ferme les connexions inactives sans invalider le pool lui-même, si
		// bien que la prochaine acquisition en ouvre une neuve qui rejoue
		// AfterConnect avec l'extension désormais présente. Si rien n'a été
		// appliqué (redémarrage normal contre une base déjà à jour), il n'y
		// a rien de neuf à recycler.
		pool.Reset()
	}
	return nil
}
