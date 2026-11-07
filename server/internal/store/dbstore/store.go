// Package dbstore implements Store on Postgres. A Pool and password service are
// supplied by the composition root; handlers never open their own connections.
package dbstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tomanagle/harness-kit/server/internal/auth"
	"github.com/tomanagle/harness-kit/server/internal/store"
)

// Database is the Postgres functionality this repository consumes. The pool is
// created and closed by main; the repository cannot open or replace it.
type Database interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Ping(context.Context) error
}

type Options struct {
	RequireVerifiedEmail bool
	MailEnabled          bool
	PublicURL            string
	Database             Database
	Passwords            auth.Passwords
	Tokens               auth.Tokens
	Now                  func() time.Time
}

type Store struct {
	requireVerifiedEmail bool
	mailEnabled          bool
	publicURL            string
	db                   Database
	passwords            auth.Passwords
	tokens               auth.Tokens
	now                  func() time.Time
}

func New(opts Options) (*Store, error) {
	if opts.Database == nil || opts.Passwords == nil || opts.Tokens == nil || opts.Now == nil {
		return nil, errors.New("dbstore: database, passwords, tokens, and clock are required")
	}
	return &Store{requireVerifiedEmail: opts.RequireVerifiedEmail, mailEnabled: opts.MailEnabled, publicURL: opts.PublicURL, db: opts.Database, passwords: opts.Passwords, tokens: opts.Tokens, now: opts.Now}, nil
}

// MustNew is the fail-fast startup variant of New. Call it only from main.
func MustNew(opts Options) *Store {
	repository, err := New(opts)
	if err != nil {
		panic(err)
	}
	return repository
}

var _ store.Store = (*Store)(nil)

func (s *Store) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

func dbError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return store.ErrConflict
	}
	return err
}

func transaction[T any](ctx context.Context, s *Store, fn func(pgx.Tx) (T, error)) (T, error) {
	var zero T
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	result, err := fn(tx)
	if err != nil {
		return zero, dbError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return zero, err
	}
	return result, nil
}

func (s *Store) scope(ctx context.Context, tx pgx.Tx, p store.Principal) error {
	if p.DeviceID == "" {
		return store.ErrForbidden
	}
	// Recheck authorization in the transaction and hold the grant and membership
	// until commit, so a completed revocation fences subsequent acknowledgements.
	var role string
	err := tx.QueryRow(ctx, `SELECT m.role FROM devices d JOIN memberships m ON m.user_id=d.user_id AND m.tenant_id=d.tenant_id WHERE d.id=$1 AND d.user_id=$2 AND d.tenant_id=$3 AND d.application_id=$4 AND NOT d.revoked FOR SHARE OF d,m`, p.DeviceID, p.AccountID, p.TenantID, p.ApplicationID).Scan(&role)
	if err != nil {
		return store.ErrUnauthorized
	}
	if role != p.Role {
		return store.ErrUnauthorized
	}
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.application_id',$2,true),set_config('app.account_id',$3,true),set_config('app.device_id',$4,true)`, p.TenantID, p.ApplicationID, p.AccountID, p.DeviceID)
	return err
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func validID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func requireAccount(p store.Principal) error {
	if p.AccountID == "" || p.DeviceID != "" {
		return store.ErrForbidden
	}
	return nil
}

func invalid(message string) error { return fmt.Errorf("%w: %s", store.ErrInvalid, message) }
