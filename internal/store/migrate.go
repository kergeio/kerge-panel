package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

func migrationFiles() fs.FS {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		panic(err) // the embedded directory is fixed at compile time
	}
	return sub
}

// migration is one schema step. Version n moves the schema from n-1 to n.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads NNNN_description.sql files from fsys. Versions must
// start at 1 and be contiguous.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	// fs.Glob returns names in lexical order, which matches version order
	// because the prefix is zero-padded and contiguity is enforced below.
	out := make([]migration, 0, len(names))
	for i, name := range names {
		prefix, _, ok := strings.Cut(path.Base(name), "_")
		if !ok || len(prefix) != 4 {
			return nil, fmt.Errorf("store: migration %q: name must be NNNN_description.sql", name)
		}
		v, err := strconv.Atoi(prefix)
		if err != nil || v != i+1 {
			return nil, fmt.Errorf("store: migration %q: expected version %d", name, i+1)
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(body)})
	}
	return out, nil
}

// migrate applies every migration newer than the database's user_version.
// Each migration and its version bump run in one transaction, so a failed
// migration leaves the schema at the previous version.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	migs, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	current, err := schemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if current > len(migs) {
		return fmt.Errorf("store: database schema version %d is newer than this build supports (%d)", current, len(migs))
	}
	for _, m := range migs[current:] {
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	// PRAGMA statements cannot take bound parameters. The value is an integer
	// derived from embedded file names, never from external input.
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.version)); err != nil {
		return fmt.Errorf("store: migration %s: set version: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migration %s: commit: %w", m.name, err)
	}
	return nil
}

func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	return v, nil
}
