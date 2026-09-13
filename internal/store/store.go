// Package store owns database connectivity and schema migration.
//
// It is deliberately thin: the domain packages hold the SQL for their own
// tables. What lives here is the wiring every caller needs and nobody should
// reimplement — pool construction and bringing the schema up to date.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, for goose only
	"github.com/pressly/goose/v3"

	"github.com/HoseaCodes/arcade-api/migrations"
)

// Connect opens a pool and verifies it can actually reach the database.
//
// pgxpool connects lazily, so without the ping a misconfigured DATABASE_URL
// would not surface until the first request. Better to fail at boot.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// Modest ceiling: this runs on small single-machine deployments, and Neon
	// bills compute by connection-time. Far more than this would queue at the
	// database rather than help.
	cfg.MaxConns = 10
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate applies any outstanding migrations.
//
// goose needs a database/sql handle, which is the only reason the stdlib driver
// is imported at all; everything else in the service uses pgx directly. The
// handle is opened and closed here so it never outlives the migration.
func Migrate(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.Up(db, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
