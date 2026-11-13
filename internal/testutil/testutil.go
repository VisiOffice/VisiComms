// Package testutil gives tests a migrated Postgres database. The schema is
// rebuilt once per test binary; each test starts from empty tables.
package testutil

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

var (
	once    sync.Once
	pool    *pgxpool.Pool
	initErr error
)

// URL is TEST_DATABASE_URL (default: a local visicomms_test database) with
// the test binary's package appended to the database name. `go test ./...`
// runs packages in parallel, and each rebuilds its schema, so each gets its
// own database.
func URL() string {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = "postgres://localhost:5432/visicomms_test?sslmode=disable"
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	pkg := strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")
	u.Path += "_" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, strings.ToLower(pkg))
	return u.String()
}

// DB returns a pool on an empty, fully migrated test database.
func DB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	once.Do(func() { pool, initErr = setup(ctx) })
	if initErr != nil {
		t.Fatalf("test database: %v", initErr)
	}
	if err := db.Truncate(ctx, pool); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func setup(ctx context.Context) (*pgxpool.Pool, error) {
	if err := createDatabase(ctx, URL()); err != nil {
		return nil, err
	}
	p, err := db.Connect(ctx, URL())
	if err != nil {
		return nil, err
	}
	if err := db.Recreate(ctx, p); err != nil {
		return nil, err
	}
	if _, err := db.Migrate(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// createDatabase creates the test database when it does not exist yet.
func createDatabase(ctx context.Context, uri string) error {
	u, err := url.Parse(uri)
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(u.Path, "/")
	u.Path = "/postgres"
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	return err
}
