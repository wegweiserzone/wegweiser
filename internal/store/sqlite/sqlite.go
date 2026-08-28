// Package sqlite implements the Wegweiser store on SQLite.
//
// SQLite allows one writer at a time. Go's database/sql knows nothing about
// that and will happily hand two goroutines two connections to the same file,
// where they deadlock against each other: a deadlock the busy handler cannot
// resolve, because backing off does not release the lock the other connection
// is already holding. The result is SQLITE_BUSY under load with a busy timeout
// configured, which is the most common way to end up with a mysteriously flaky
// SQLite backend in Go.
//
//   - the write pool, capped at a single connection, which every [Store.Update]
//     goes through, and which opens its transactions as BEGIN IMMEDIATE so that
//     a lock conflict with another process surfaces at the start rather than
//     halfway in;
//   - the read pool, which several readers share. Write-ahead logging lets them
//     read while a write is in progress, and PRAGMA query_only makes a stray
//     write through this pool an error instead of a violation of the
//     single-writer assumption.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/store"

	// The driver registers itself as "sqlite".
	sqlite3 "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

// Defaults for [Options].
const (
	// DefaultBusyTimeout is how long a connection waits for a lock held by
	// another process before giving up.
	DefaultBusyTimeout = 5 * time.Second
	// MinReaders is the smallest read pool that still lets one reader proceed
	// while another is blocked.
	MinReaders = 2
)

// Options configure a SQLite store.
type Options struct {
	// Path is the database file. It is created if it does not exist.
	Path string

	// MaxReaders is the size of the read pool. Zero picks a default based on
	// the number of CPUs; anything below [MinReaders] is raised to it.
	MaxReaders int

	// BusyTimeout is how long to wait for a lock before failing. Zero picks
	// [DefaultBusyTimeout].
	BusyTimeout time.Duration

	// Now supplies the current time for the timestamps the store owns. Nil
	// picks [time.Now]. Tests set it to keep results reproducible.
	Now func() time.Time
}

// Store is a SQLite-backed [store.Store].
//
// It embeds the read side, so a read outside a transaction runs the same
// statement against the read pool that a read inside one runs against the
// transaction. The write side is deliberately not embedded: it lives on the
// transaction alone, so that no caller can write without one.
type Store struct {
	reader

	write *sql.DB
	read  *sql.DB

	now  func() time.Time
	path string

	closeOnce sync.Once
	closeErr  error
}

// Open opens the database at opts.Path, creating it if necessary, and verifies
// that both connection pools carry the settings they were configured with.
//
// It does not apply migrations: a freshly opened store may be several schema
// versions behind, and whether to change the database on start is the caller's
// decision, not this package's. Call [Store.Migrate] for that.
func Open(ctx context.Context, opts Options) (_ *Store, err error) {
	if opts.Path == "" {
		return nil, errors.New("sqlite: no database path given")
	}
	if strings.Contains(opts.Path, ":memory:") || strings.HasPrefix(opts.Path, "file::memory:") {
		// Worth catching precisely, because the failure is otherwise silent and
		// deeply confusing: every connection would get its own empty database,
		// so a write through one pool would be invisible to the other.
		return nil, errors.New(
			"sqlite: an in-memory database cannot be used, because the read pool and the write " +
				"pool would each get their own copy; use a file, and a temporary directory in tests")
	}

	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = DefaultBusyTimeout
	}
	readers := opts.MaxReaders
	if readers == 0 {
		readers = runtime.NumCPU()
	}
	readers = max(readers, MinReaders)

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	s := &Store{now: now, path: opts.Path}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()

	// The write pool comes first. It is the one allowed to switch the database
	// into write-ahead logging, and on a database that does not exist yet it is
	// the one that creates the file.
	s.write, err = sql.Open("sqlite", dataSourceName(opts.Path, busy, true))
	if err != nil {
		return nil, fmt.Errorf("sqlite: opening %s for writing: %w", opts.Path, err)
	}
	s.write.SetMaxOpenConns(1)
	s.write.SetMaxIdleConns(1)
	s.write.SetConnMaxLifetime(0)

	err = enableWAL(ctx, s.write, opts.Path, busy)
	if err != nil {
		return nil, err
	}
	err = verifyPool(ctx, s.write, 1, writerPragmas(busy))
	if err != nil {
		return nil, err
	}

	s.read, err = sql.Open("sqlite", dataSourceName(opts.Path, busy, false))
	if err != nil {
		return nil, fmt.Errorf("sqlite: opening %s for reading: %w", opts.Path, err)
	}
	s.read.SetMaxOpenConns(readers)
	s.read.SetMaxIdleConns(readers)
	s.read.SetConnMaxLifetime(0)

	err = verifyPool(ctx, s.read, readers, readerPragmas(busy))
	if err != nil {
		return nil, err
	}
	s.reader = reader{q: s.read, now: now}

	return s, nil
}

// Path returns the database file the store was opened on.
func (s *Store) Path() string { return s.path }

// Close releases both pools. It is safe to call more than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		var errs []error
		// Readers first: closing the writer while a read transaction is open
		// would leave the write-ahead log unable to check itself back in.
		for _, db := range []*sql.DB{s.read, s.write} {
			if db != nil {
				errs = append(errs, db.Close())
			}
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// Ping reports whether the database is reachable. It checks both pools, since
// a reader failing while the writer works is exactly the asymmetry a health
// check exists to catch.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.read.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite: read pool: %w", err)
	}
	if err := s.write.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite: write pool: %w", err)
	}
	return nil
}

// dataSourceName builds the connection string for one pool.
func dataSourceName(path string, busy time.Duration, writer bool) string {
	q := make(url.Values)
	for _, p := range pragmas(busy, writer) {
		if p.set != "" {
			q.Add("_pragma", p.set)
		}
	}
	if writer {
		// Take the write lock when the transaction begins rather than when it
		// first writes. A deferred transaction that has already read cannot be
		// upgraded while another writer holds the lock, and SQLite reports that
		// as busy immediately, without consulting the busy timeout.
		q.Set("_txlock", "immediate")
	}

	// Percent-encoding the path so that a directory containing a space, a
	// question mark or a hash does not truncate or reshape the string. SQLite
	// is opened with SQLITE_OPEN_URI and decodes it again.
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath(), RawQuery: q.Encode()}
	return u.String()
}

// pragma is a connection setting together with the value that reading it back
// must produce. An empty set means the setting is only verified, not asked for.
type pragma struct {
	name string
	set  string
	want string
	// why explains, in the error, what silently breaks when the setting did not
	// take effect.
	why string
}

func pragmas(busy time.Duration, writer bool) []pragma {
	ms := busy.Milliseconds()
	ps := []pragma{{
		name: "foreign_keys",
		set:  "foreign_keys(1)",
		want: "1",
		why: "foreign keys are enforced per connection and default to off, so the ON DELETE " +
			"CASCADE that removes a generated record along with the record it came from " +
			"would do nothing at all",
	}, {
		name: "busy_timeout",
		set:  fmt.Sprintf("busy_timeout(%d)", ms),
		want: fmt.Sprint(ms),
		why: "without it a lock held by another process fails the query immediately " +
			"instead of being waited out",
	}}

	// The journal mode is verified everywhere and set nowhere: it belongs to the
	// database file rather than to a connection, so [enableWAL] switches it once
	// at startup. Asking for it on every connection would take a lock on every
	// connection, for a setting that is already what it should be.
	ps = append(ps, pragma{
		name: "journal_mode",
		want: "wal",
		why: "write-ahead logging is what lets reads proceed during a write; without it " +
			"every reader blocks behind every writer",
	})

	if writer {
		return append(ps, pragma{
			name: "synchronous",
			set:  "synchronous(NORMAL)",
			want: "1", // NORMAL
			why: "with write-ahead logging this survives a process crash intact and loses at " +
				"most the last transaction on power loss; FULL would cost an fsync per commit",
		})
	}
	return append(ps, pragma{
		name: "query_only",
		set:  "query_only(1)",
		want: "1",
		why: "the read pool must refuse writes, so that a write reaching the database outside " +
			"the single write connection fails loudly instead of racing",
	})
}

// enableWAL switches the database into write-ahead logging unless it is already
// there.
//
// The wait is measured against the real clock, not the injected one, because a
// test clock that does not advance would turn this into a hang.
func enableWAL(ctx context.Context, db *sql.DB, path string, timeout time.Duration) error {
	const pause = 20 * time.Millisecond

	start := time.Now()
	for {
		var mode string
		err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode)
		if err == nil && strings.EqualFold(mode, "wal") {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("the database stayed in %q mode", mode)
		}

		// Only contention is worth another attempt. A file that cannot be
		// opened answers the same way however long anyone waits for it, and
		// waiting the whole busy timeout to say so leaves an operator with a
		// typo in the path staring at nothing for five seconds.
		if !isBusy(err) {
			return fmt.Errorf("sqlite: cannot use %s as a database: %w.%s", path, err, hint(err))
		}
		if time.Since(start) >= timeout {
			return fmt.Errorf(
				"sqlite: could not switch %s into write-ahead logging within %s: %w. Another "+
					"process may be holding the database; check for a second weg on the same file",
				path, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
	}
}

// isBusy reports whether an error means the database is held by something else
// right now, which is the only kind of failure that trying again can resolve.
func isBusy(err error) bool {
	var serr *sqlite3.Error
	if !errors.As(err, &serr) {
		return true
	}
	switch serr.Code() & 0xFF {
	case sqlitelib.SQLITE_BUSY, sqlitelib.SQLITE_LOCKED:
		return true
	default:
		return false
	}
}

// hint adds what to do about a failure the operator can actually fix. SQLite
// says "unable to open database file" for a missing directory, an unwritable
// one and a bad path alike, which is true and useless on its own.
func hint(err error) string {
	var serr *sqlite3.Error
	if errors.As(err, &serr) && serr.Code()&0xFF == sqlitelib.SQLITE_CANTOPEN {
		return " Check that the directory exists and that this user may write to it"
	}
	return ""
}

func writerPragmas(busy time.Duration) []pragma { return pragmas(busy, true) }
func readerPragmas(busy time.Duration) []pragma { return pragmas(busy, false) }

// verifyPool checks the settings on n connections held at the same time, which
// both proves the connection string applies to every connection the pool opens
// and leaves the pool warm.
func verifyPool(ctx context.Context, db *sql.DB, n int, want []pragma) (err error) {
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			err = errors.Join(err, c.Close())
		}
	}()

	for range n {
		c, cerr := db.Conn(ctx)
		if cerr != nil {
			return fmt.Errorf("sqlite: opening a connection: %w", cerr)
		}
		conns = append(conns, c)

		for _, p := range want {
			var got string
			// The pragma name is ours, never a caller's, so interpolating it is
			// not a query built from input.
			if serr := c.QueryRowContext(ctx, "PRAGMA "+p.name).Scan(&got); serr != nil {
				return fmt.Errorf("sqlite: reading PRAGMA %s: %w", p.name, serr)
			}
			if !strings.EqualFold(got, p.want) {
				return fmt.Errorf(
					"sqlite: PRAGMA %s is %q but should be %q — %s. SQLite accepts a misspelled "+
						"pragma name and an out-of-range value without complaining, so check the "+
						"_pragma parameters in the connection string",
					p.name, got, p.want, p.why)
			}
		}
	}
	return nil
}

// inTx runs fn inside one write transaction on the single write connection.
//
// The transaction is committed if fn returns nil and rolled back otherwise,
// including when fn panics: a panic that left the transaction open would hold
// the only write connection there is, and every later write would block behind
// it forever.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: beginning a write transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			err = errors.Join(err, tx.Rollback())
			panic(p)
		}
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()

	err = fn(tx)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: committing: %w", err)
	}
	return nil
}

// Capabilities reports what this backend can do.
func (s *Store) Capabilities() store.Capabilities {
	return store.Capabilities{
		Backend: store.BackendSQLite,
		// SQLite serializes writers itself, and this package narrows that to
		// one connection so the serialization is ours to reason about.
		ConcurrentWriters: false,
		// No channel to push a change down; a second node would have to poll.
		ListenNotify: false,
	}
}
