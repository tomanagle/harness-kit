// Package maintenance runs bounded, replica-safe housekeeping. A scheduler can
// invoke cleanup on any instance; all durable coordination remains in Postgres.
package maintenance

import (
	"context"
	"github.com/jackc/pgx/v5"
)

type Database interface {
	Begin(context.Context) (pgx.Tx, error)
}

func Run(ctx context.Context, db Database) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var acquired bool
	if err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(783192422)").Scan(&acquired); err != nil || !acquired {
		return err
	}
	for _, table := range []string{"account_tokens", "access_tokens", "refresh_tokens", "device_requests", "account_challenges"} {
		_, err = tx.Exec(ctx, "DELETE FROM "+table+" WHERE hash IN (SELECT hash FROM "+table+" WHERE expires_at<now() LIMIT 5000)")
		if err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, "DELETE FROM rate_limits WHERE key IN (SELECT key FROM rate_limits WHERE window_start<now()-interval '1 day' LIMIT 5000)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM mail_jobs WHERE id IN (SELECT id FROM mail_jobs WHERE expires_at<now() LIMIT 5000)"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
