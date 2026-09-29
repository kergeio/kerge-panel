// Package store owns the panel's SQLite database: opening it with the
// required pragmas, applying embedded schema migrations, and serializing all
// writes through a single writer goroutine.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// readPoolSize is the maximum number of concurrent read connections.
const readPoolSize = 4

// connPragmas are applied to every connection in both pools.
//
// temp_store keeps SQLite's scratch data in memory. SQLite otherwise writes
// it to a file in the first writable directory of SQLITE_TMPDIR, TMPDIR,
// /var/tmp, /usr/tmp, /tmp and the working directory, and a hardened
// deployment has none of those: a statement journal that outgrows its
// in-memory limit then fails with a disk I/O error, which makes SQLite
// discard the whole transaction. A cascading DELETE of a host with a few
// hundred rows of history is already large enough to hit it.
var connPragmas = []string{
	"busy_timeout(5000)",
	"foreign_keys(1)",
	"synchronous(NORMAL)",
	"temp_store(MEMORY)",
}

// DB is an open panel database. Reads go through Read; all writes must go
// through Write.
type DB struct {
	read   *sql.DB
	write  *sql.DB
	writer *Writer
}

// Open opens (creating if needed) the database at path, enables WAL mode,
// applies pending migrations and starts the writer goroutine.
func Open(ctx context.Context, path string) (*DB, error) {
	if path == "" || strings.ContainsRune(path, '?') || strings.HasPrefix(path, "file:") || path == ":memory:" {
		return nil, fmt.Errorf("store: invalid database path %q", path)
	}
	// Create the file up front so it is not world-readable. SQLite gives the
	// -wal and -shm files the same permissions as the database file.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("store: create database file: %w", err)
	}

	write, err := sql.Open("sqlite", dsn(path, "_txlock=immediate", "_pragma=journal_mode(WAL)"))
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)
	write.SetConnMaxIdleTime(0)

	if err := checkJournalMode(ctx, write); err != nil {
		write.Close()
		return nil, err
	}
	if err := migrate(ctx, write, migrationFiles()); err != nil {
		write.Close()
		return nil, err
	}

	read, err := sql.Open("sqlite", dsn(path, "_pragma=query_only(1)"))
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	read.SetMaxOpenConns(readPoolSize)
	read.SetMaxIdleConns(readPoolSize)
	if err := read.PingContext(ctx); err != nil {
		read.Close()
		write.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}

	return &DB{read: read, write: write, writer: newWriter(write)}, nil
}

// Read returns the read-only connection pool. Any attempt to write through it
// fails.
func (d *DB) Read() *sql.DB { return d.read }

// Write runs fn inside a write transaction on the writer goroutine and waits
// for the result. See Writer.Do.
func (d *DB) Write(ctx context.Context, fn TxFunc) error {
	return d.writer.Do(ctx, fn)
}

// Close flushes pending writes and closes both pools.
func (d *DB) Close() error {
	d.writer.Close()
	return errors.Join(d.read.Close(), d.write.Close())
}

func dsn(path string, extra ...string) string {
	params := make([]string, 0, len(connPragmas)+len(extra))
	for _, p := range connPragmas {
		params = append(params, "_pragma="+p)
	}
	params = append(params, extra...)
	return path + "?" + strings.Join(params, "&")
}

func checkJournalMode(ctx context.Context, db *sql.DB) error {
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("store: read journal mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("store: journal mode is %q, want wal", mode)
	}
	return nil
}
