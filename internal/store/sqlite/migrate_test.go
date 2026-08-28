package sqlite_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
)

func TestLoadMigrations(t *testing.T) {
	t.Parallel()

	file := func(names ...string) fstest.MapFS {
		fsys := make(fstest.MapFS, len(names))
		for _, n := range names {
			fsys["migrations/"+n] = &fstest.MapFile{Data: []byte("SELECT 1;")}
		}
		return fsys
	}

	tests := []struct {
		name    string
		fsys    fstest.MapFS
		want    []string
		wantErr string
	}{
		{
			name: "one step",
			fsys: file("0001_initial.sql"),
			want: []string{"0001_initial"},
		},
		{
			name: "several steps in order",
			fsys: file("0002_add_tokens.sql", "0001_initial.sql", "0003_widen_comment.sql"),
			want: []string{"0001_initial", "0002_add_tokens", "0003_widen_comment"},
		},
		{
			name: "files that are not migrations are not collected",
			fsys: fstest.MapFS{
				"migrations/0001_initial.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
				"migrations/README.md":        &fstest.MapFile{Data: []byte("notes")},
			},
			want: []string{"0001_initial"},
		},
		{
			// A lost file would leave a schema that never existed anywhere
			// else, which is worse than refusing to start.
			name:    "gap in the sequence",
			fsys:    file("0001_initial.sql", "0003_widen_comment.sql"),
			wantErr: "without a gap",
		},
		{
			name:    "not starting at one",
			fsys:    file("0002_add_tokens.sql"),
			wantErr: "without a gap",
		},
		{
			name:    "duplicate version",
			fsys:    file("0001_initial.sql", "0001_other.sql"),
			wantErr: "without a gap",
		},
		{
			name:    "no version prefix",
			fsys:    file("initial.sql"),
			wantErr: "NNNN_description.sql",
		},
		{
			name:    "version too short",
			fsys:    file("001_initial.sql"),
			wantErr: "NNNN_description.sql",
		},
		{
			name:    "no description",
			fsys:    file("0001_.sql"),
			wantErr: "NNNN_description.sql",
		},
		{
			name:    "version is not a number",
			fsys:    file("00x1_initial.sql"),
			wantErr: "does not start with a version number",
		},
		{
			name:    "version zero",
			fsys:    file("0000_initial.sql"),
			wantErr: "does not start with a version number",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := sqlite.LoadMigrationsForTest(tc.fsys)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("loaded %v, want an error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("loaded %v, want %v", got, tc.want)
			}
		})
	}
}

// A failed write must roll back, and a panicking one must roll back too: the
// write pool holds exactly one connection, so a transaction left open would
// block every later write for as long as the process lives.
func TestWriteTransactionAlwaysEnds(t *testing.T) {
	t.Parallel()

	s := open(t)

	insert := func(tx *sql.Tx, key string) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO settings (key, value, updated_at) VALUES (?, '1', 0)`, key)
		return err
	}

	t.Run("an error rolls back", func(t *testing.T) {
		want := errors.New("no")
		err := s.InTxForTest(t.Context(), func(tx *sql.Tx) error {
			if err := insert(tx, "rolled-back"); err != nil {
				return err
			}
			return want
		})
		if !errors.Is(err, want) {
			t.Fatalf("InTx = %v, want %v", err, want)
		}
		if settingExists(t, s, "rolled-back") {
			t.Error("the row survived a rolled-back transaction")
		}
	})

	t.Run("a panic rolls back and does not keep the connection", func(t *testing.T) {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic did not reach the caller")
				}
			}()
			err := s.InTxForTest(t.Context(), func(tx *sql.Tx) error {
				if err := insert(tx, "panicked"); err != nil {
					return err
				}
				panic("boom")
			})
			// Unreachable: the panic passes through rather than being turned
			// into an error, which is what the deferred recover has to preserve.
			t.Errorf("InTx returned %v instead of letting the panic through", err)
		}()

		if settingExists(t, s, "panicked") {
			t.Error("the row survived a panicking transaction")
		}
	})

	t.Run("the write connection still works afterwards", func(t *testing.T) {
		if err := s.InTxForTest(t.Context(), func(tx *sql.Tx) error {
			return insert(tx, "after")
		}); err != nil {
			t.Fatalf("InTx after a panic: %v", err)
		}
		if !settingExists(t, s, "after") {
			t.Error("the committed row is missing")
		}
	})
}

func settingExists(t *testing.T, s *sqlite.Store, key string) bool {
	t.Helper()

	var n int
	if err := s.ReadPoolForTest().QueryRowContext(t.Context(),
		`SELECT count(*) FROM settings WHERE key = ?`, key).Scan(&n); err != nil {
		t.Fatalf("counting settings: %v", err)
	}
	return n > 0
}
