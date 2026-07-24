// Package db owns the single PostgreSQL connection and the startup migration runner.
//
// Provenance: [O] INV-03 (parameterized queries only, no string-built SQL), INV-16 (one DB, one DSN,
// deploy-time migrations under an advisory lock, DML-only runtime role), M-03, P0-3 · [R]
// paradigm-rule 1 (single-organization schema; the DML-only runtime role is the privilege boundary,
// ADR-0010 — no tenant_id, no cross-tenant RLS).
//
// Two roles, two DSNs (see deploy/.env.example):
//   - MIGRATION role: owns DDL, used only by Migrate() at startup under an advisory lock.
//   - RUNTIME role:   DML only, no DDL — an attempted CREATE TABLE at request time fails at the
//     privilege level. The app pool uses this DSN.
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey serializes migrations across replicas (arbitrary constant).
const advisoryLockKey int64 = 0x7467_0001 // "tg" 0001

//go:embed migrations/*.up.sql
var migrationsFS embed.FS

// Pool is the runtime connection pool (DML-only role).
type Pool struct{ *pgxpool.Pool }

// Connect opens a pgx pool against dsn. All queries through this pool must be parameterized; there is
// no string-built-SQL path in this codebase (enforced by the P0-8 CI lint gate).
func Connect(ctx context.Context, dsn string) (*Pool, error) {
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &Pool{p}, nil
}

// Migrate applies pending ordered migrations under a Postgres advisory lock, each in its own
// transaction. It must be run with the MIGRATION (DDL-capable) DSN, never inside request handling.
// A migration that has already been applied is skipped by version.
func Migrate(ctx context.Context, migrationDSN string) error {
	p, err := pgxpool.New(ctx, migrationDSN)
	if err != nil {
		return fmt.Errorf("db: migrate connect: %w", err)
	}
	defer p.Close()

	conn, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("db: advisory lock: %w", err)
	}
	defer conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("db: ensure schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".up.sql")
		var applied bool
		if err := conn.QueryRow(ctx, "SELECT true FROM schema_migrations WHERE version=$1", version).Scan(&applied); err == nil && applied {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("db: migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", version); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
