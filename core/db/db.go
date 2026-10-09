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
	"time"

	"github.com/jackc/pgx/v5"
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

// dynamicMaxConnLifetime recycles pooled connections well inside the shortest lease TTL (1h), so no
// connection outlives the validity of the credential it presented — the invariant that makes lease
// rotation invisible to the pool's users.
const dynamicMaxConnLifetime = 15 * time.Minute

// ConnectDynamic opens a pool whose CREDENTIAL is supplied per-connection instead of embedded in the DSN
// (TG-422 slice 2: dynamic Postgres credentials). dsnTemplate carries the connection coordinates and NO
// userinfo; cred returns the current leased username/password and is consulted by pgx's BeforeConnect on
// EVERY new connection, so the pool keeps working across lease rotation: each new connection dials with the
// current lease. A connection opened under an OLD lease is NOT self-healing — a dropped lease role's live
// session survives the DROP but goes UNPRIVILEGED (permission-denied on every table; TG-553), so the pool
// must be Reset when a lease rotates (Provider.OnRotate → pool.Reset, wired at the worker composition root),
// not trusted to keep working. A cred error fails that connection closed; there is no static fallback.
func ConnectDynamic(ctx context.Context, dsnTemplate string, cred func(context.Context) (string, string, error)) (*Pool, error) {
	if cred == nil {
		return nil, fmt.Errorf("db: dynamic connect: no credential source (fail closed)")
	}
	cfg, err := pgxpool.ParseConfig(dsnTemplate)
	if err != nil {
		return nil, fmt.Errorf("db: dynamic connect: parse template: %w", err)
	}
	cfg.BeforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
		u, p, cerr := cred(ctx)
		if cerr != nil {
			return fmt.Errorf("db: dynamic credential: %w", cerr)
		}
		cc.User, cc.Password = u, p
		return nil
	}
	cfg.MaxConnLifetime = dynamicMaxConnLifetime
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: dynamic connect: %w", err)
	}
	// Ping forces one real connection through BeforeConnect NOW, so a dead credential source is a boot-time
	// failure with a named cause, not a first-query surprise.
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("db: dynamic ping: %w", err)
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

	// Own every object this run creates as the PERSISTENT tg_migration role, not the EPHEMERAL dyndb lease
	// the migration DSN resolves to at boot (`dyn:tg_migration`). A table CREATEd under the lease is owned by
	// it, and because ALTER DEFAULT PRIVILEGES keys grants on the CREATING role, that table also misses the
	// tg_runtime base grants — so when the lease expires the table is orphan-owned AND the triage/actuation
	// planes silently lose access (TG-546, observed live: actuation_target_state was owned by an expired lease,
	// tg_actuate could not write it, and the actuation plane executed nothing). SET ROLE fixes both at the
	// source: new objects are born owned by tg_migration with the correct default-privilege grants, which
	// ApplyPlaneGrants then mirrors to the plane roles. Adopted only when the migration identity is a MEMBER
	// of a DISTINCT tg_migration role (the production lease is); a static tg_migration DSN already IS the role
	// (nothing to adopt), and a bare superuser/test fixture has no tg_migration role (skip) — so this changes
	// only the leased-credential production path and never the test fixtures.
	var adoptMigrationRole bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_roles r
			WHERE r.rolname = 'tg_migration'
			  AND r.rolname <> current_user
			  AND pg_has_role(current_user, r.oid, 'MEMBER')
		)`).Scan(&adoptMigrationRole); err != nil {
		return fmt.Errorf("db: migrate check tg_migration membership: %w", err)
	}
	if adoptMigrationRole {
		if _, err := conn.Exec(ctx, "SET ROLE tg_migration"); err != nil {
			return fmt.Errorf("db: migrate adopt tg_migration role (own DDL as the persistent role, TG-546): %w", err)
		}
	}

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
