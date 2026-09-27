// Package dbtest provides a Postgres handle for integration tests.
//
// Open skips the test when CPI_TEST_DB_HOST is unset, so `go test ./...` needs
// no database. Otherwise each call gets its own schema in the test database
// (default copilot_insights_test, created on demand) via search_path, so tests
// run in parallel across packages without touching the dev database or each
// other. The schema is dropped in t.Cleanup.
package dbtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.dfds.cloud/copilot-insights/internal/store"
)

// validDBName guards the identifier interpolated into CREATE DATABASE, which
// cannot take parameters.
var validDBName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Options returns the test connection settings from CPI_TEST_DB_*, and false
// when CPI_TEST_DB_HOST is unset.
func Options() (store.Options, bool) {
	host := os.Getenv("CPI_TEST_DB_HOST")
	if host == "" {
		return store.Options{}, false
	}
	port, _ := strconv.Atoi(envOr("CPI_TEST_DB_PORT", "5533"))
	return store.Options{
		Host:         host,
		Port:         port,
		Name:         envOr("CPI_TEST_DB_NAME", "copilot_insights_test"),
		User:         envOr("CPI_TEST_DB_USER", "postgres"),
		Password:     envOr("CPI_TEST_DB_PASSWORD", "secret"),
		SSLMode:      envOr("CPI_TEST_DB_SSLMODE", "disable"),
		MaxOpenConns: 4,
	}, true
}

// Open returns a handle scoped to a fresh, empty schema. Migrations are not
// applied; call store.Migrate or use OpenMigrated.
func Open(t testing.TB) *sql.DB {
	t.Helper()
	opts, ok := Options()
	if !ok {
		t.Skip("CPI_TEST_DB_HOST not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := ensureDatabase(ctx, opts); err != nil {
		t.Fatalf("ensure test database %q: %v", opts.Name, err)
	}

	schema := "t_" + randomHex(6)
	admin, err := store.Open(ctx, opts)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	opts.Params = map[string]string{"search_path": schema}
	db, err := store.Open(ctx, opts)
	if err != nil {
		admin.Close()
		t.Fatalf("open test schema: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
		admin.Close()
	})
	return db
}

// OpenMigrated is Open plus all migrations applied.
func OpenMigrated(t testing.TB) *sql.DB {
	t.Helper()
	db := Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// ensureDatabase creates the test database if absent, connecting through the
// maintenance "postgres" database.
func ensureDatabase(ctx context.Context, opts store.Options) error {
	if !validDBName.MatchString(opts.Name) {
		return errors.New("invalid test database name")
	}
	adminOpts := opts
	adminOpts.Name = "postgres"
	admin, err := store.Open(ctx, adminOpts)
	if err != nil {
		return err
	}
	defer admin.Close()

	var exists bool
	if err := admin.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", opts.Name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{opts.Name}.Sanitize())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P04" { // duplicate_database: a parallel package won the race
		return nil
	}
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
