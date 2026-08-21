package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/wegweiserzone/wegweiser/internal/store"
)

// migrationFS holds the schema, applied in file-name order.
//
// Migrations only go forward. For a product shipping as one binary the tested
// recovery path is "restore the backup", not a down-migration nobody has ever
// exercised.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationTable is created by the migrator rather than by a migration,
// because the migrator has to read it in order to decide whether the first
// migration has run.
const migrationTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`

// migration is one numbered schema step.
type migration struct {
	version int
	name    string
	sql     string
}

// Migrate brings the schema up to what this build expects.
func (s *Store) Migrate(ctx context.Context) error {
	all, err := loadMigrations(migrationFS)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return errors.New("sqlite: no migrations are embedded in this binary")
	}

	if _, terr := s.write.ExecContext(ctx, migrationTable); terr != nil {
		return fmt.Errorf("sqlite: creating the migration table: %w", terr)
	}

	current, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	latest := all[len(all)-1].version
	if current > latest {
		return fmt.Errorf(
			"%w: the database at %s is at schema version %d and this build knows up to %d; "+
				"run the newer build, or restore a backup taken before the upgrade",
			store.ErrSchemaTooNew, s.path, current, latest)
	}

	for _, m := range all {
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// schemaVersion returns the highest applied migration version, or zero for a
// database no migration has run on.
func (s *Store) schemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.read.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("sqlite: reading the schema version: %w", err)
	}
	return int(v.Int64), nil
}

// applyMigration runs one migration, unless it has already run.
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Inside the transaction, and therefore behind the write lock the
		// connection takes at BEGIN IMMEDIATE: another process that got here
		// first has already committed, and this sees it.
		var applied int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM schema_migrations WHERE version = ?`, m.version,
		).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			return nil
		}

		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, s.now().UnixMilli())
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: applying migration %04d_%s: %w", m.version, m.name, err)
	}
	return nil
}

// loadMigrations reads the embedded migrations and checks that they form an
// unbroken sequence starting at one. A gap means a file was lost, and applying
// what is left would produce a schema that never existed anywhere else.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("sqlite: listing migrations: %w", err)
	}
	// Glob returns sorted names, and the fixed-width version prefix makes name
	// order and version order the same thing.
	out := make([]migration, 0, len(entries))
	for _, file := range entries {
		version, name, err := parseMigrationName(path.Base(file))
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("sqlite: reading %s: %w", file, err)
		}
		if want := len(out) + 1; version != want {
			return nil, fmt.Errorf(
				"sqlite: migration %s is numbered %d where %d was expected; migrations must run "+
					"from 1 upwards without a gap", file, version, want)
		}
		out = append(out, migration{version: version, name: name, sql: string(body)})
	}
	return out, nil
}

// parseMigrationName splits "0001_initial.sql" into its version and its name.
func parseMigrationName(file string) (version int, name string, err error) {
	base := strings.TrimSuffix(file, ".sql")
	digits, name, ok := strings.Cut(base, "_")
	if !ok || len(digits) != 4 || name == "" {
		return 0, "", fmt.Errorf(
			"sqlite: migration %q is not named NNNN_description.sql", file)
	}
	version, err = strconv.Atoi(digits)
	if err != nil || version < 1 {
		return 0, "", fmt.Errorf("sqlite: migration %q does not start with a version number", file)
	}
	return version, name, nil
}
