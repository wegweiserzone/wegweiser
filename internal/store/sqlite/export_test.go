package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"time"
)

// This file is compiled only into the test binary, so nothing here widens the
// package's real surface. It exists so that tests can inspect the connection
// pools the store actually opened, rather than rebuilding a connection string
// of their own and then proving only that the copy matches itself.

// ReadPoolForTest returns the pool reads go through.
func (s *Store) ReadPoolForTest() *sql.DB { return s.read }

// WritePoolForTest returns the single-connection pool writes go through.
func (s *Store) WritePoolForTest() *sql.DB { return s.write }

// DataSourceNameForTest builds the connection string for one pool.
func DataSourceNameForTest(path string, busy time.Duration, writer bool) string {
	return dataSourceName(path, busy, writer)
}

// VerifyPoolForTest checks that n connections of db carry the settings a pool
// of the given role is supposed to have.
func VerifyPoolForTest(ctx context.Context, db *sql.DB, n int, writer bool) error {
	return verifyPool(ctx, db, n, pragmas(DefaultBusyTimeout, writer))
}

// LoadMigrationsForTest reads and checks a migration set from any file system.
func LoadMigrationsForTest(fsys fs.FS) ([]string, error) {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ms))
	for _, m := range ms {
		names = append(names, fmt.Sprintf("%04d_%s", m.version, m.name))
	}
	return names, nil
}

// InTxForTest runs fn inside a write transaction.
func (s *Store) InTxForTest(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.inTx(ctx, fn)
}
