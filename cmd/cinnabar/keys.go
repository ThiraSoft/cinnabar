package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

func runKeys(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cinnabar keys create|revoke ...")
	}

	pool, err := postgres.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool); err != nil {
		return err
	}
	repo := postgres.NewClientRepo(pool)

	switch args[0] {
	case "create":
		if len(args) != 4 {
			return fmt.Errorf("usage: cinnabar keys create <label> <workspace_id> <identities,comma,separated>")
		}
		rawIdents := strings.Split(args[3], ",")
		idents := make([]string, len(rawIdents))
		for i, ident := range rawIdents {
			idents[i] = strings.TrimSpace(ident)
		}
		token, id, err := repo.Create(ctx, args[1], args[2], idents)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr,
			"client %s créé (%s). Le token ci-dessous ne sera plus affiché.\n",
			args[1], id)
		fmt.Println(token)
		return nil

	case "revoke":
		if len(args) != 2 {
			return fmt.Errorf("usage: cinnabar keys revoke <label>")
		}
		n, err := repo.Revoke(ctx, args[1])
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no live client found for label %q", args[1])
		}
		fmt.Fprintf(os.Stderr, "%d client(s) révoqué(s)\n", n)
		return nil

	default:
		return fmt.Errorf("unknown keys subcommand %q", args[0])
	}
}
