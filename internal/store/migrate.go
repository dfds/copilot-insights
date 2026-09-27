package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func newProvider(db *sql.DB) (*goose.Provider, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, db, sub)
}

// Migrate applies all pending migrations and returns the versions applied.
// Provider.Close is never called: it closes the *sql.DB it was given.
func Migrate(ctx context.Context, db *sql.DB) ([]int64, error) {
	p, err := newProvider(db)
	if err != nil {
		return nil, fmt.Errorf("init migrations: %w", err)
	}
	results, err := p.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	applied := make([]int64, 0, len(results))
	for _, r := range results {
		applied = append(applied, r.Source.Version)
	}
	return applied, nil
}

// MigrateDownAll rolls back every migration. Used by tests to check the Down
// sections.
func MigrateDownAll(ctx context.Context, db *sql.DB) error {
	p, err := newProvider(db)
	if err != nil {
		return err
	}
	_, err = p.DownTo(ctx, 0)
	return err
}
