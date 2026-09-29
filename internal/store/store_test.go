package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func openTest(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kerge.db")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestOpenAppliesPragmas(t *testing.T) {
	db, path := openTest(t)
	ctx := t.Context()

	for _, pool := range []struct {
		name string
		db   *sql.DB
	}{{"write", db.write}, {"read", db.read}} {
		var mode string
		var busy, fk, syncMode, tempStore int
		q := pool.db.QueryRowContext
		if err := q(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if err := q(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if err := q(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := q(ctx, "PRAGMA synchronous").Scan(&syncMode); err != nil {
			t.Fatal(err)
		}
		// temp_store must stay MEMORY: a deployment that gives the panel no
		// writable temp directory would otherwise lose whole transactions
		// once a statement journal spills to disk.
		if err := q(ctx, "PRAGMA temp_store").Scan(&tempStore); err != nil {
			t.Fatal(err)
		}
		// synchronous: 1 = NORMAL. temp_store: 2 = MEMORY.
		if mode != "wal" || busy != 5000 || fk != 1 || syncMode != 1 || tempStore != 2 {
			t.Errorf("%s pool: journal_mode=%s busy_timeout=%d foreign_keys=%d synchronous=%d temp_store=%d",
				pool.name, mode, busy, fk, syncMode, tempStore)
		}
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("database file mode = %v, want no group/other access", perm)
		}
	}
}

func TestOpenRejectsBadPath(t *testing.T) {
	for _, p := range []string{"", ":memory:", "file:x.db", "/tmp/x.db?mode=ro"} {
		if _, err := Open(t.Context(), p); err == nil {
			t.Errorf("Open(%q) succeeded", p)
		}
	}
}

func TestReadPoolIsReadOnly(t *testing.T) {
	db, _ := openTest(t)
	_, err := db.Read().ExecContext(t.Context(), "INSERT INTO settings (key, value) VALUES (?, ?)", "k", "v")
	if err == nil {
		t.Fatal("write through read pool succeeded")
	}
}

func TestMigrationsAreRepeatable(t *testing.T) {
	db, path := openTest(t)
	ctx := t.Context()
	migs, err := loadMigrations(migrationFiles())
	if err != nil {
		t.Fatal(err)
	}
	want := len(migs)
	if v, _ := schemaVersion(ctx, db.write); v != want {
		t.Fatalf("user_version = %d, want %d", v, want)
	}
	before := schemaSQL(t, db.read)
	db.Close()

	// Reopening must not re-run or fail on already applied migrations.
	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if v, _ := schemaVersion(ctx, db2.write); v != want {
		t.Fatalf("user_version after reopen = %d, want %d", v, want)
	}
	if after := schemaSQL(t, db2.read); after != before {
		t.Errorf("schema changed on reopen:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestInitialSchemaTables(t *testing.T) {
	db, _ := openTest(t)
	rows, err := db.Read().QueryContext(t.Context(),
		"SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	want := "enroll_tokens hosts metrics_1h metrics_1m metrics_raw sessions settings setup_state traffic_daily traffic_state users"
	if strings.Join(got, " ") != want {
		t.Errorf("tables = %v, want %s", got, want)
	}
}

func TestMigrateFailureLeavesPreviousVersion(t *testing.T) {
	db, _ := openTest(t)
	ctx := t.Context()
	base := len(mustLoad(t, migrationFiles()))

	fsys := fstest.MapFS{}
	for _, m := range mustLoad(t, migrationFiles()) {
		fsys[m.name] = &fstest.MapFile{Data: []byte(m.sql)}
	}
	next := base + 1
	name := func(v int, desc string) string { return fmt.Sprintf("%04d_%s.sql", v, desc) }
	fsys[name(next, "good_then_bad")] = &fstest.MapFile{Data: []byte(
		"CREATE TABLE partial (id INTEGER PRIMARY KEY); SELECT * FROM no_such_table;")}

	if err := migrate(ctx, db.write, fsys); err == nil {
		t.Fatal("migrate with a bad migration succeeded")
	}
	if v, _ := schemaVersion(ctx, db.write); v != base {
		t.Errorf("user_version = %d, want %d", v, base)
	}
	var n int
	if err := db.read.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_schema WHERE name = 'partial'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("partial migration was not rolled back")
	}

	// A fixed migration applies cleanly afterwards.
	fsys[name(next, "good_then_bad")] = &fstest.MapFile{Data: []byte("CREATE TABLE partial (id INTEGER PRIMARY KEY);")}
	if err := migrate(ctx, db.write, fsys); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, _ := schemaVersion(ctx, db.write); v != next {
		t.Errorf("user_version = %d, want %d", v, next)
	}
}

func TestMigrateRejectsNewerDatabase(t *testing.T) {
	db, _ := openTest(t)
	ctx := t.Context()
	if _, err := db.write.ExecContext(ctx, "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	err := migrate(ctx, db.write, migrationFiles())
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("migrate on newer schema: err = %v", err)
	}
}

func TestLoadMigrationsValidatesNames(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"gap":        {"0001_a.sql": {}, "0003_c.sql": {}},
		"no prefix":  {"init.sql": {}},
		"short":      {"1_a.sql": {}},
		"start at 2": {"0002_b.sql": {}},
	}
	for name, fsys := range cases {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: loadMigrations succeeded", name)
		}
	}
}

func TestForeignKeysCascade(t *testing.T) {
	db, _ := openTest(t)
	ctx := t.Context()
	err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO hosts (id, name, created_at) VALUES (1, 'a', 0)"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO metrics_raw (host_id, ts, cpu) VALUES (1, 100, 1.5)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO metrics_raw (host_id, ts) VALUES (42, 100)")
		return err
	})
	if err == nil {
		t.Error("insert with unknown host_id succeeded; foreign keys not enforced")
	}
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM hosts WHERE id = 1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "metrics_raw"); n != 0 {
		t.Errorf("metrics_raw rows after host delete = %d, want 0", n)
	}
}

func TestWriterCommits(t *testing.T) {
	db, _ := openTest(t)
	if err := db.Write(t.Context(), insertSetting("a", "1")); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := db.Read().QueryRowContext(t.Context(), "SELECT value FROM settings WHERE key = ?", "a").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "1" {
		t.Errorf("value = %q", v)
	}
}

// blockWriter occupies the writer goroutine until the returned release
// function is called.
func blockWriter(t *testing.T, db *DB) (release func()) {
	t.Helper()
	started := make(chan struct{})
	gate := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.Write(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
			close(started)
			<-gate
			return nil
		})
	}()
	<-started
	return func() {
		close(gate)
		if err := <-done; err != nil {
			t.Errorf("blocking job: %v", err)
		}
	}
}

func TestWriterBatchesQueuedJobs(t *testing.T) {
	db, _ := openTest(t)
	var mu sync.Mutex
	var sizes []int
	db.writer.batchHook = func(n int) {
		mu.Lock()
		sizes = append(sizes, n)
		mu.Unlock()
	}

	release := blockWriter(t, db)
	const n = 50
	errs := make(chan error, n)
	for i := range n {
		go func() { errs <- db.Write(context.Background(), insertSetting("k"+itoa(i), "v")) }()
	}
	waitQueued(t, db, n)
	release()
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sizes) != 2 || sizes[0] != 1 || sizes[1] != n {
		t.Errorf("batch sizes = %v, want [1 %d]", sizes, n)
	}
	if got := count(t, db, "settings"); got != n {
		t.Errorf("settings rows = %d, want %d", got, n)
	}
}

func TestWriterIsolatesFailingJob(t *testing.T) {
	db, _ := openTest(t)
	release := blockWriter(t, db)

	boom := errors.New("boom")
	type result struct {
		key string
		err error
	}
	jobs := map[string]TxFunc{
		"ok1": insertSetting("ok1", "v"),
		"bad": func(ctx context.Context, tx *sql.Tx) error {
			if err := insertSetting("bad", "v")(ctx, tx); err != nil {
				return err
			}
			return boom
		},
		"panic": func(ctx context.Context, tx *sql.Tx) error {
			if err := insertSetting("panic", "v")(ctx, tx); err != nil {
				return err
			}
			panic("job panic")
		},
		"ok2": insertSetting("ok2", "v"),
	}
	results := make(chan result, len(jobs))
	for k, fn := range jobs {
		go func() { results <- result{k, db.Write(context.Background(), fn)} }()
	}
	waitQueued(t, db, len(jobs))
	release()

	for range jobs {
		r := <-results
		switch r.key {
		case "ok1", "ok2":
			if r.err != nil {
				t.Errorf("%s: %v", r.key, r.err)
			}
		case "bad":
			if !errors.Is(r.err, boom) {
				t.Errorf("bad: err = %v, want boom", r.err)
			}
		case "panic":
			if r.err == nil || !strings.Contains(r.err.Error(), "panicked") {
				t.Errorf("panic: err = %v", r.err)
			}
		}
	}
	keys := settingKeys(t, db)
	if keys != "ok1 ok2" {
		t.Errorf("committed keys = %q, want \"ok1 ok2\"", keys)
	}
}

// A statement that fails with an I/O error, a full disk or an interrupt makes
// SQLite discard the transaction's savepoints, so the rollback that follows
// the job fails as well. The job's own error is the one that says why, and it
// must survive into the error the caller gets. Dropping a savepoint by hand
// stands in for the statement that dropped it in the field.
func TestWriterKeepsJobErrorWhenTheSavepointIsGone(t *testing.T) {
	db, _ := openTest(t)

	boom := errors.New("boom")
	err := db.Write(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "RELEASE job"); err != nil {
			return fmt.Errorf("releasing the savepoint: %w", err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to report %v", err, boom)
	}
	var txErr errTx
	if !errors.As(err, &txErr) {
		t.Errorf("err = %v, want it to be an errTx so the batch is abandoned", err)
	}
	if !strings.Contains(fmt.Sprint(err), "no such savepoint") {
		t.Errorf("err = %v, want it to report the failed rollback too", err)
	}
}

func TestWriterSkipsCanceledJob(t *testing.T) {
	db, _ := openTest(t)
	release := blockWriter(t, db)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- db.Write(ctx, insertSetting("canceled", "v")) }()
	waitQueued(t, db, 1)
	cancel()
	release()

	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := count(t, db, "settings"); n != 0 {
		t.Errorf("settings rows = %d, want 0", n)
	}
}

func TestWriterCloseFlushesAndRejects(t *testing.T) {
	db, _ := openTest(t)
	release := blockWriter(t, db)

	errc := make(chan error, 1)
	go func() { errc <- db.Write(context.Background(), insertSetting("pending", "v")) }()
	waitQueued(t, db, 1)

	closed := make(chan struct{})
	go func() { db.writer.Close(); close(closed) }()
	release()
	<-closed

	if err := <-errc; err != nil {
		t.Errorf("queued job before Close: %v", err)
	}
	if err := db.Write(context.Background(), insertSetting("late", "v")); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close: err = %v, want ErrClosed", err)
	}
	if keys := settingKeys(t, db); keys != "pending" {
		t.Errorf("committed keys = %q, want \"pending\"", keys)
	}
}

func TestWriterDoRespectsContextWhenQueueFull(t *testing.T) {
	db, _ := openTest(t)
	release := blockWriter(t, db)
	defer release()

	errs := make(chan error, queueSize)
	for range queueSize {
		go func() { errs <- db.Write(context.Background(), insertNothing) }()
	}
	waitQueued(t, db, queueSize)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := db.Write(ctx, insertNothing); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

// helpers

func insertSetting(k, v string) TxFunc {
	return func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES (?, ?)", k, v)
		return err
	}
}

func insertNothing(context.Context, *sql.Tx) error { return nil }

func waitQueued(t *testing.T, db *DB, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(db.writer.jobs) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d queued jobs (have %d)", n, len(db.writer.jobs))
		}
		time.Sleep(time.Millisecond)
	}
}

func count(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	// table is a constant from the test code, never external input.
	if err := db.Read().QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func settingKeys(t *testing.T, db *DB) string {
	t.Helper()
	rows, err := db.Read().QueryContext(t.Context(), "SELECT key FROM settings ORDER BY key")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	return strings.Join(keys, " ")
}

func schemaSQL(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "SELECT sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		b.WriteString(s)
		b.WriteString(";\n")
	}
	return b.String()
}

func mustLoad(t *testing.T, fsys fs.FS) []migration {
	t.Helper()
	migs, err := loadMigrations(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return migs
}

func itoa(i int) string { return strconv.Itoa(i) }
