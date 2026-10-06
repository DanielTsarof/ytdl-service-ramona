// Package db is the PostgreSQL layer: a pgx pool, sqlc-generated queries
// (package dbgen, generated from queries/*.sql; never edit by hand) and
// goose migrations embedded from migrations/*.sql.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
)

// ErrNotFound is returned by the helpers in this package for missing rows.
// Generated queries return pgx.ErrNoRows; IsNotFound accepts both.
var ErrNotFound = errors.New("db: not found")

// IsNotFound reports whether err means "no such row".
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, pgx.ErrNoRows)
}

// DB embeds the generated queries, so callers use d.GetVideoBySourceID(...)
// etc. directly; Pool is exposed for anything sqlc does not cover.
type DB struct {
	Pool *pgxpool.Pool
	*dbgen.Queries
	log *slog.Logger
}

// Open connects and verifies the connection. It does not migrate; call
// Migrate explicitly (at startup, or in tests).
func Open(ctx context.Context, dsn string, logger *slog.Logger) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &DB{Pool: pool, Queries: dbgen.New(pool), log: logger}, nil
}

func (d *DB) Close() {
	d.Pool.Close()
}

// Migrate applies all pending migrations. goose serialises concurrent runs
// with a Postgres advisory lock, so several instances may call it at startup.
func (d *DB) Migrate(ctx context.Context) error {
	p, err := d.provider()
	if err != nil {
		return err
	}
	defer p.Close() // closes the database/sql wrapper only, not the pool

	results, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("db: migrate: %w", err)
	}
	for _, r := range results {
		d.log.Info("migration applied",
			slog.Int64("version", r.Source.Version),
			slog.String("file", r.Source.Path),
			slog.Duration("elapsed", r.Duration))
	}
	version, err := p.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("db: migrate: read version: %w", err)
	}
	d.log.Debug("database schema up to date", slog.Int64("version", version), slog.Int("applied", len(results)))
	return nil
}

// MigrateDownTo rolls back to version (0 = empty schema). Meant for tests
// and manual recovery, not for application startup.
func (d *DB) MigrateDownTo(ctx context.Context, version int64) error {
	p, err := d.provider()
	if err != nil {
		return err
	}
	defer p.Close()
	if _, err := p.DownTo(ctx, version); err != nil {
		return fmt.Errorf("db: migrate down to %d: %w", version, err)
	}
	return nil
}

func (d *DB) provider() (*goose.Provider, error) {
	p, err := goose.NewProvider(goose.DialectPostgres, stdlib.OpenDBFromPool(d.Pool), Migrations())
	if err != nil {
		return nil, fmt.Errorf("db: goose: %w", err)
	}
	return p, nil
}

// InTx runs fn in a transaction: committed if fn returns nil, rolled back
// otherwise (including on panic).
func (d *DB) InTx(ctx context.Context, fn func(q *dbgen.Queries) error) error {
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		return fn(d.Queries.WithTx(tx))
	})
}
