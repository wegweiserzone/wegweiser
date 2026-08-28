package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
)

// open returns a migrated store on a fresh database in a temporary directory.
func open(t *testing.T) *sqlite.Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "weg.db"))
}

func openAt(t *testing.T, path string) *sqlite.Store {
	t.Helper()

	s, err := sqlite.Open(t.Context(), sqlite.Options{Path: path})
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	closeOnCleanup(t, s)
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func closeOnCleanup(t *testing.T, s *sqlite.Store) {
	t.Helper()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

func TestOpenRejectsUnusablePaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"in-memory", ":memory:"},
		{"in-memory URI", "file::memory:"},
		{"in-memory shared cache", "file::memory:?cache=shared"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, err := sqlite.Open(t.Context(), sqlite.Options{Path: tc.path})
			if err == nil {
				closeOnCleanup(t, s)
				t.Fatalf("Open(%q) succeeded, want an error", tc.path)
			}
		})
	}
}

// The connection string carries the path, so a directory containing characters
// that mean something in a URI must not reshape it. Go names temporary
// directories after the test, which is how such a path turns up by accident.
func TestOpenHandlesAwkwardPaths(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{"plain", "with space", "with#hash", "with?question", "with%percent"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			base := filepath.Join(t.TempDir(), dir)
			if err := os.MkdirAll(base, 0o750); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			path := filepath.Join(base, "weg.db")

			s := openAt(t, path)
			if s.Path() != path {
				t.Errorf("Path() = %q, want %q", s.Path(), path)
			}
			// The database has to be the file that was named, not one SQLite
			// invented after the string was cut short at the special character.
			if _, err := os.Stat(path); err != nil {
				t.Errorf("no database at %q: %v", path, err)
			}
		})
	}
}

func TestDataSourceName(t *testing.T) {
	t.Parallel()

	writer := sqlite.DataSourceNameForTest("/var/lib/weg.db", 2500*time.Millisecond, true)
	for _, want := range []string{
		// Percent-encoded, because the brackets of a pragma call are not
		// query-string characters.
		"foreign_keys%281%29",
		"busy_timeout%282500%29",
		"synchronous%28NORMAL%29",
		"_txlock=immediate",
	} {
		if !strings.Contains(writer, want) {
			t.Errorf("the write connection string is missing %s:\n  %s", want, writer)
		}
	}

	reader := sqlite.DataSourceNameForTest("/var/lib/weg.db", 2500*time.Millisecond, false)
	if !strings.Contains(reader, "query_only%281%29") {
		t.Errorf("the read connection string does not ask for query_only:\n  %s", reader)
	}
	// Neither pool sets the journal mode: it belongs to the file, Open switches
	// it once, and asking for it per connection takes a lock per connection.
	for role, dsn := range map[string]string{"write": writer, "read": reader} {
		if strings.Contains(dsn, "journal_mode") {
			t.Errorf("the %s connection string tries to set the journal mode:\n  %s", role, dsn)
		}
	}
	if strings.Contains(reader, "_txlock") {
		t.Errorf("the read connection string asks for a write lock:\n  %s", reader)
	}

	// A path with a character that would otherwise end the path or start the
	// query has to survive.
	awkward := sqlite.DataSourceNameForTest("/var/lib/a b?c#d/weg.db", time.Second, true)
	if strings.Contains(awkward, "a b?c#d") {
		t.Errorf("the path was not encoded:\n  %s", awkward)
	}
}

// Every setting Open promises is per connection, and SQLite ignores a
// misspelled pragma without a word, so the promise is worth exactly as much as
// this check.
func TestOpenConfiguresEveryConnection(t *testing.T) {
	t.Parallel()

	const readers = 4
	s, err := sqlite.Open(t.Context(), sqlite.Options{
		Path:        filepath.Join(t.TempDir(), "weg.db"),
		MaxReaders:  readers,
		BusyTimeout: 2500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	closeOnCleanup(t, s)

	want := map[string]map[string]string{
		"read": {
			"journal_mode": "wal",
			"foreign_keys": "1",
			"busy_timeout": "2500",
			"query_only":   "1",
		},
		"write": {
			"journal_mode": "wal",
			"foreign_keys": "1",
			"busy_timeout": "2500",
			"synchronous":  "1",
			"query_only":   "0",
		},
	}
	pools := map[string]struct {
		db *sql.DB
		n  int
	}{
		"read":  {s.ReadPoolForTest(), readers},
		"write": {s.WritePoolForTest(), 1},
	}

	for role, pool := range pools {
		// Hold every connection at once, so this asks each one rather than the
		// same one over and over.
		conns := make([]*sql.Conn, 0, pool.n)
		for range pool.n {
			c, err := pool.db.Conn(t.Context())
			if err != nil {
				t.Fatalf("%s pool: Conn: %v", role, err)
			}
			conns = append(conns, c)
		}

		for i, c := range conns {
			for pragma, expected := range want[role] {
				var got string
				if err := c.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&got); err != nil {
					t.Fatalf("%s connection %d: PRAGMA %s: %v", role, i, pragma, err)
				}
				if !strings.EqualFold(got, expected) {
					t.Errorf("%s connection %d: PRAGMA %s = %q, want %q", role, i, pragma, got, expected)
				}
			}
			if err := c.Close(); err != nil {
				t.Errorf("%s connection %d: Close: %v", role, i, err)
			}
		}
	}
}

// The read pool must refuse a write outright. Without that, a write reaching
// the database outside the single write connection races the writer instead of
// failing.
func TestReadPoolRefusesWrites(t *testing.T) {
	t.Parallel()

	s := open(t)

	_, err := s.ReadPoolForTest().ExecContext(t.Context(),
		`INSERT INTO settings (key, value, updated_at) VALUES ('x', '1', 0)`)
	if err == nil {
		t.Fatal("the read pool accepted a write")
	}
	if !strings.Contains(err.Error(), "readonly") {
		t.Errorf("write refused, but not as read-only: %v", err)
	}
}

// The check at startup only means something if it fails when the settings are
// missing.
func TestVerifyPoolRejectsAnUnconfiguredConnection(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "weg.db")
	openAt(t, path) // create the file, and put it into write-ahead logging

	bare, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("opening a bare connection: %v", err)
	}
	defer func() {
		if cerr := bare.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()

	err = sqlite.VerifyPoolForTest(t.Context(), bare, 1, true)
	if err == nil {
		t.Fatal("a connection with no settings at all passed verification")
	}
	// Foreign keys are the setting whose absence is silent and whose
	// consequence is worst, so the error has to name it.
	if !strings.Contains(err.Error(), "foreign_keys") {
		t.Errorf("the error does not name the missing setting: %v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()

	s := open(t)
	before := schemaVersion(t, s)
	if before < 1 {
		t.Fatalf("schema version is %d, want at least 1", before)
	}

	for range 3 {
		if err := s.Migrate(t.Context()); err != nil {
			t.Fatalf("Migrate again: %v", err)
		}
	}
	if after := schemaVersion(t, s); after != before {
		t.Errorf("schema version moved from %d to %d without a new migration", before, after)
	}
}

func TestMigrateCreatesTheSchema(t *testing.T) {
	t.Parallel()

	s := open(t)

	for _, table := range []string{
		"schema_migrations", "zones", "records",
		"journal_commits", "journal_events", "api_tokens", "settings",
	} {
		var n int
		err := s.ReadPoolForTest().QueryRowContext(t.Context(),
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
		if err != nil {
			t.Fatalf("looking for table %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s was not created", table)
		}
	}
}

// A database written by a newer build must not be written by this one: the
// older binary would insert rows shaped the way the older schema expects, and
// nothing would notice until the newer binary came back.
func TestMigrateRefusesANewerSchema(t *testing.T) {
	t.Parallel()

	s := open(t)

	_, err := s.WritePoolForTest().ExecContext(t.Context(),
		`INSERT INTO schema_migrations (version, applied_at) VALUES (9999, 0)`)
	if err != nil {
		t.Fatalf("faking a newer schema: %v", err)
	}

	err = s.Migrate(t.Context())
	if !errors.Is(err, store.ErrSchemaTooNew) {
		t.Fatalf("Migrate = %v, want an error wrapping store.ErrSchemaTooNew", err)
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Errorf("the error does not name the version it found: %v", err)
	}
}

// Two processes starting together must not both run the same migration. The
// decision is made inside the transaction that applies it, so the second one
// sees the first.
func TestMigrateSurvivesConcurrentStarts(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "weg.db")

	const starters = 4
	var wg sync.WaitGroup
	errs := make([]error, starters)
	for i := range starters {
		wg.Add(1)
		go func() {
			defer wg.Done()

			s, err := sqlite.Open(context.Background(), sqlite.Options{Path: path})
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { errs[i] = errors.Join(errs[i], s.Close()) }()
			errs[i] = s.Migrate(context.Background())
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("starter %d: %v", i, err)
		}
	}

	s := openAt(t, path)
	var applied int
	if err := s.ReadPoolForTest().QueryRowContext(t.Context(),
		`SELECT count(*) FROM schema_migrations WHERE version = 1`).Scan(&applied); err != nil {
		t.Fatalf("counting migrations: %v", err)
	}
	if applied != 1 {
		t.Errorf("migration 1 is recorded %d times, want once", applied)
	}
}

// The whole point of the split pool: a reader keeps working while a write
// transaction is open. Without write-ahead logging this blocks until the busy
// timeout runs out.
func TestReadsProceedDuringAWrite(t *testing.T) {
	t.Parallel()

	s := open(t)

	tx, err := s.WritePoolForTest().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(t.Context(),
		`INSERT INTO settings (key, value, updated_at) VALUES ('pending', '1', 0)`); err != nil {
		t.Fatalf("write inside the transaction: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		var n int
		done <- s.ReadPoolForTest().QueryRowContext(t.Context(),
			`SELECT count(*) FROM settings`).Scan(&n)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("reading while a write was open: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("a read blocked behind an open write transaction")
	}

	if err := tx.Rollback(); err != nil {
		t.Errorf("Rollback: %v", err)
	}
}

func TestPingAndCapabilities(t *testing.T) {
	t.Parallel()

	s := open(t)

	if err := s.Ping(t.Context()); err != nil {
		t.Errorf("Ping: %v", err)
	}

	caps := s.Capabilities()
	if caps.Backend != store.BackendSQLite {
		t.Errorf("Backend = %q, want %q", caps.Backend, store.BackendSQLite)
	}
	if caps.ConcurrentWriters {
		t.Error("ConcurrentWriters is true, but SQLite has one writer")
	}
	if caps.ListenNotify {
		t.Error("ListenNotify is true, but SQLite cannot push a notification")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	s, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(t.TempDir(), "weg.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := range 3 {
		if err := s.Close(); err != nil {
			t.Errorf("Close %d: %v", i+1, err)
		}
	}
}

func schemaVersion(t *testing.T, s *sqlite.Store) int {
	t.Helper()

	var v sql.NullInt64
	if err := s.ReadPoolForTest().QueryRowContext(t.Context(),
		`SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatalf("reading the schema version: %v", err)
	}
	return int(v.Int64)
}

// TestOpenUnusableFileFailsAtOnce covers the difference between a database
// somebody else is holding and one that cannot be opened at all.
func TestOpenUnusableFileFailsAtOnce(t *testing.T) {
	t.Parallel()

	const busy = 3 * time.Second
	start := time.Now()
	_, err := sqlite.Open(t.Context(), sqlite.Options{
		Path:        filepath.Join(t.TempDir(), "no", "such", "weg.db"),
		BusyTimeout: busy,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a database in a directory that does not exist opened")
	}
	if elapsed > busy/3 {
		t.Errorf("failing took %v of a %v busy timeout, so it waited out a lock that "+
			"nobody was holding", elapsed, busy)
	}
	if strings.Contains(err.Error(), "Another process") {
		t.Errorf("error %q blames another process for a path that cannot be opened", err)
	}
	if !strings.Contains(err.Error(), "directory exists") {
		t.Errorf("error %q does not say what to check", err)
	}
}
