// Package db owns the Postgres pool and the schema. Application migrations
// are embedded SQL files applied in name order; River's own tables are
// migrated alongside them so one `migrate` brings a database fully current.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx, so model code can
// run inside or outside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func Connect(ctx context.Context, uri string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(uri)
	if err != nil {
		return nil, errors.New("invalid database url")
	}
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database unreachable: %w", err)
	}
	return pool, nil
}

// Versions lists every application migration in apply order.
func Versions() []string {
	entries, _ := fs.ReadDir(migrations, "migrations")
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".sql"))
	}
	sort.Strings(out)
	return out
}

// migrateLock keeps two processes (a deploy's new container and a manual
// run, say) from migrating at the same time.
const migrateLock = 7_202_610_080

// Migrate applies River's migrations, then every pending application
// migration, each in its own transaction, holding an advisory lock throughout.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLock); err != nil {
		return nil, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLock)

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return nil, err
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return nil, fmt.Errorf("river migrations: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	applied, err := Applied(ctx, pool)
	if err != nil {
		return nil, err
	}
	var ran []string
	for _, version := range Versions() {
		if applied[version] {
			continue
		}
		sql, err := migrations.ReadFile("migrations/" + version + ".sql")
		if err != nil {
			return ran, err
		}
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return ran, fmt.Errorf("migration %s: %w", version, err)
		}
		ran = append(ran, version)
	}
	return ran, nil
}

func Applied(ctx context.Context, q Querier) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, v := range versions {
		out[v] = true
	}
	return out, nil
}

// Pending lists application migrations not yet applied.
func Pending(ctx context.Context, q Querier) ([]string, error) {
	applied, err := Applied(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range Versions() {
		if !applied[v] {
			out = append(out, v)
		}
	}
	return out, nil
}

// RequireMigrated refuses a schema with pending application or River
// migrations. The server calls it before starting: a container that refuses
// to start never reports healthy, so a deploy that skipped its migration
// aborts and the previous release keeps serving.
func RequireMigrated(ctx context.Context, pool *pgxpool.Pool) error {
	pending, err := Pending(ctx, pool)
	if err != nil {
		return fmt.Errorf("checking migrations: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d pending migration(s), first %s: run `visicomms migrate` before starting", len(pending), pending[0])
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	res, err := migrator.Validate(ctx, nil)
	if err != nil {
		return fmt.Errorf("checking river migrations: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("river schema not current: %s: run `visicomms migrate` before starting", strings.Join(res.Messages, "; "))
	}
	return nil
}

// Recreate drops and recreates the public schema. Test databases only.
func Recreate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	return err
}

// Truncate empties every application table, keeping the schema and River's
// tables. TRUNCATE bypasses the events table's append-only row trigger, which
// is why it is for tests only.
func Truncate(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, `SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations' AND tablename NOT LIKE 'river\_%'`)
	if err != nil {
		return err
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) == 0 {
		return err
	}
	for i, t := range tables {
		tables[i] = pgx.Identifier{t}.Sanitize()
	}
	_, err = q.Exec(ctx, "TRUNCATE "+strings.Join(tables, ", ")+" RESTART IDENTITY CASCADE")
	return err
}

// IsUniqueViolation reports a unique or exclusion constraint failure.
func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// ConstraintName returns the violated constraint, if any.
func ConstraintName(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.ConstraintName
	}
	return ""
}

// NotFound reports pgx's no-rows error.
func NotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// Tx runs fn in a transaction, committing only if fn returns nil.
func Tx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, fn)
}
