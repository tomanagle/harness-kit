// Package migrations embeds the versioned database schema. Run it with a
// separate schema-owner credential, never the runtime application credential.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
)

//go:embed *.sql
var files embed.FS

type Database interface {
	Begin(context.Context) (pgx.Tx, error)
}

func Apply(ctx context.Context, pool Database) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(783192421)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY)"); err != nil {
		return err
	}
	names, _ := fs.Glob(files, "*.sql")
	for _, name := range names {
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)", name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		b, e := files.ReadFile(name)
		if e != nil {
			return e
		}
		if _, err = tx.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(name) VALUES($1)", name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
