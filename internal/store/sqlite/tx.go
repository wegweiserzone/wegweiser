package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	sqlite3 "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"

	"github.com/wegweiserzone/wegweiser/internal/store"
)

// querier is the part of database/sql that a pool and a transaction have in
// common. Every query in this package is written against it once and then runs
// in both places, which is what makes [store.Reader] satisfiable inside and
// outside a transaction without a second copy of each statement.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	// PrepareContext is here for the one statement that runs many times: an
	// import commit can carry hundreds of thousands of events, and preparing
	// the insert once rather than per row is the difference between a pause and
	// a wait.
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

// reader implements [store.Reader]. Outside a transaction it is bound to the
// read pool; inside one, to the transaction.
//
// It carries the clock because the store owns two kinds of time: the created
// and updated stamps it writes, and the "now" a read compares against when it
// decides whether a token has expired.
type reader struct {
	q   querier
	now func() time.Time
}

// txn implements [store.Tx] by adding the write side to the read side.
type txn struct {
	reader
}

// Update runs fn inside one write transaction on the single write connection.
func (s *Store) Update(ctx context.Context, fn func(store.Tx) error) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return fn(&txn{reader: reader{q: tx, now: s.now}})
	})
}

// View runs fn inside one read transaction, so that several reads see one
// state. Write-ahead logging means it does not block the writer.
func (s *Store) View(ctx context.Context, fn func(store.Reader) error) error {
	return s.inReadTx(ctx, func(tx *sql.Tx) error {
		return fn(&reader{q: tx, now: s.now})
	})
}

// inReadTx runs fn inside one read transaction.
func (s *Store) inReadTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("sqlite: beginning a read transaction: %w", err)
	}
	defer func() {
		// A read transaction has nothing to commit, so it ends the same way
		// whether fn returned, failed or panicked.
		err = errors.Join(err, ignoreTxDone(tx.Rollback()))
	}()
	return fn(tx)
}

// ignoreTxDone drops the error a rollback reports for a transaction that has
// already ended.
func ignoreTxDone(err error) error {
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}

// translate turns a driver error into one of the store's own, so that nothing
// above the persistence boundary ever has to recognise a SQLite result code.
//
// Only the uniqueness violations become [store.ErrConflict]. A failed CHECK, a
// NULL in a NOT NULL column or a dangling foreign key are not states a caller
// can resolve by trying something else; they mean this package wrote a row it
// should never have built, and they stay as they are so that they are read as
// the bugs they are.
func translate(err error, conflict string) error {
	if err == nil {
		return nil
	}
	var serr *sqlite3.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case sqlitelib.SQLITE_CONSTRAINT_UNIQUE, sqlitelib.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %s", store.ErrConflict, conflict)
		}
	}
	return err
}

// narrow converts the integers a column holds into the sized values DNS
// actually uses, and remembers the first one that did not fit.
type narrow struct {
	table string
	rowID string
	err   error
}

func (n *narrow) u16(column string, v int64) uint16 {
	if v < 0 || v > math.MaxUint16 {
		n.fail(column, v)
		return 0
	}
	return uint16(v)
}

func (n *narrow) u32(column string, v int64) uint32 {
	if v < 0 || v > math.MaxUint32 {
		n.fail(column, v)
		return 0
	}
	return uint32(v)
}

func (n *narrow) fail(column string, v int64) {
	if n.err == nil {
		n.err = corrupt(n.table, n.rowID, column,
			fmt.Errorf("%d lies outside the range the column is constrained to", v))
	}
}

// notFound reports the absence of one object in the store's own terms.
func notFound(what string, which any) error {
	return fmt.Errorf("%w: no %s %v", store.ErrNotFound, what, which)
}

// oneRow reports whether a statement changed exactly one row, and turns "none"
// into [store.ErrNotFound]. Every update and delete in this package addresses a
// single row by primary key, so anything else is a bug rather than a state.
func oneRow(res sql.Result, err error, what string, which any) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	switch {
	case n == 0:
		return notFound(what, which)
	case n > 1:
		return fmt.Errorf("sqlite: %s %v matched %d rows, which cannot happen for a primary key",
			what, which, n)
	default:
		return nil
	}
}

// stamp is the current time at the resolution the database stores.
//
// Timestamps are written back into the caller's struct, and a nanosecond value
// there would never equal the millisecond value any later read produces —
// making "what I stored is what I get back" false in a way that only shows up
// in a comparison.
func (t *txn) stamp() time.Time { return fromMillis(t.now().UnixMilli()) }
