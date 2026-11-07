// Package database owns Postgres connection setup and runtime-role validation.
// Repositories receive their own narrow database interfaces, not connection URLs.
package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Role uint8

const (
	Runtime Role = iota
	Migrator
)

type Config struct {
	URL            string
	MaxConnections int32
	ConnectTimeout time.Duration
}

func Defaults() Config { return Config{MaxConnections: 20, ConnectTimeout: 10 * time.Second} }

// Open eagerly checks connectivity and role permissions. Failed setup closes the
// pool before returning; the caller owns Close on a successful result.
func Open(ctx context.Context, cfg Config, role Role) (*pgxpool.Pool, error) {
	if cfg.URL == "" || cfg.MaxConnections < 1 || cfg.ConnectTimeout <= 0 {
		return nil, errors.New("database: URL and positive connection limits are required")
	}
	if role != Runtime && role != Migrator {
		return nil, errors.New("database: invalid role")
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, errors.New("database: invalid connection URL")
	}
	poolCfg.MaxConns = cfg.MaxConnections
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	startup, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(startup, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}
	if err = pool.Ping(startup); err == nil && role == Runtime {
		err = CheckRuntimeRole(startup, pool)
	}
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: startup: %w", err)
	}
	return pool, nil
}

// MustOpen is the fail-fast startup variant of Open. Call it only from main.
func MustOpen(ctx context.Context, cfg Config, role Role) *pgxpool.Pool {
	pool, err := Open(ctx, cfg, role)
	if err != nil {
		panic(err)
	}
	return pool
}

type RoleQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func CheckRuntimeRole(ctx context.Context, db RoleQuerier) error {
	var unsafe bool
	err := db.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls OR EXISTS(SELECT 1 FROM pg_tables WHERE schemaname='public' AND tableowner=current_user) FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe)
	if err != nil {
		return err
	}
	if unsafe {
		return errors.New("runtime database role must not own tables, be superuser, or bypass RLS")
	}
	return nil
}
