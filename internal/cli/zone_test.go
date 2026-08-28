package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/metrics"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
	"github.com/wegweiserzone/wegweiser/internal/stream"
)

const zonefileText = `$ORIGIN example.com.
$TTL 3600
@	IN	SOA	ns1.example.com. hostmaster.example.com. (
		2026081801 7200 900 1209600 3600 )
@	IN	NS	ns1.example.com.
ns1	IN	A	192.0.2.53
www	IN	A	192.0.2.10
sub	IN	NS	ns1.sub.example.com.
buried.sub IN	TXT	"never answered"
`

// server brings up a real API on a loopback port, because a command whose only
// test is against a stub proves that the stub matches the command rather than
// that either matches the server.
type server struct {
	addr   string
	token  string
	stream *stream.Hub
}

func newServer(t *testing.T) server {
	t.Helper()

	st, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(t.TempDir(), "weg.db")})
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("close the database: %v", cerr)
		}
	})
	if merr := st.Migrate(t.Context()); merr != nil {
		t.Fatalf("migrate: %v", merr)
	}

	applier, err := apply.New(st, apply.Options{})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}
	token, err := api.EnsureBootstrapToken(t.Context(), st, time.Now())
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}

	var snap *dns.Snapshot
	if verr := st.View(t.Context(), func(r store.Reader) error {
		var berr error
		snap, berr = dns.Rebuild(t.Context(), r)
		return berr
	}); verr != nil {
		t.Fatalf("build the first snapshot: %v", verr)
	}
	holder := &snapshots{current: snap}

	hub := stream.NewHub(stream.Options{})
	apiSrv, handler, err := api.New(api.Config{
		Store: st, Applier: applier, Snapshots: holder,
		Metrics: metrics.New(), Stream: hub,
		OnError: func(err error) { t.Errorf("the server reported a fault: %v", err) },
	})
	if err != nil {
		t.Fatalf("build the API: %v", err)
	}
	t.Cleanup(func() {
		if cerr := apiSrv.Close(); cerr != nil {
			t.Errorf("close the API: %v", cerr)
		}
	})

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	return server{addr: "http://" + l.Addr().String(), token: token, stream: hub}
}

type snapshots struct{ current *dns.Snapshot }

func (s *snapshots) Snapshot() *dns.Snapshot     { return s.current }
func (s *snapshots) SetSnapshot(n *dns.Snapshot) { s.current = n }

func TestZoneImportAndExport(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	path := filepath.Join(t.TempDir(), "db.example.com")
	writeFile(t, path, zonefileText)

	var stdout, stderr syncBuffer
	code := Execute(t.Context(), []string{
		"zone", "import", path, "--server", srv.addr, "--token", srv.token,
	}, &stdout, &stderr)

	if code != ExitOK {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr.String())
	}
	got := stdout.String()

	if !strings.Contains(got, "imported example.com.") || !strings.Contains(got, "serial 2026081801") {
		t.Errorf("output = %q, want the zone and the serial it kept", got)
	}

	t.Run("it says what it left out, in words", func(t *testing.T) {
		// A person running a migration has to see this; a JSON field nobody
		// looked at is not telling them.
		if !strings.Contains(got, "skipped") || !strings.Contains(got, "buried.sub.example.com.") {
			t.Errorf("output = %q, want the record below the delegation named", got)
		}
	})

	t.Run("it offers the reverse zone that would be needed", func(t *testing.T) {
		if !strings.Contains(got, "2.0.192.in-addr.arpa.") {
			t.Errorf("output = %q, want the reverse zone offered (D6)", got)
		}
	})

	t.Run("exporting writes the file to standard output", func(t *testing.T) {
		var out, errOut syncBuffer
		if c := Execute(t.Context(), []string{
			"zone", "export", "example.com", "--server", srv.addr, "--token", srv.token,
		}, &out, &errOut); c != ExitOK {
			t.Fatalf("exit code = %d; stderr: %s", c, errOut.String())
		}
		text := out.String()
		if !strings.HasPrefix(text, "$ORIGIN example.com.") {
			t.Errorf("the export does not start with the origin:\n%s", text)
		}
		if !strings.Contains(text, "www.example.com.\t3600\tIN\tA\t192.0.2.10") {
			t.Errorf("the export is missing a record:\n%s", text)
		}
	})

	t.Run("a trailing dot is optional, as everywhere else", func(t *testing.T) {
		var out, errOut syncBuffer
		if c := Execute(t.Context(), []string{
			"zone", "export", "example.com.", "--server", srv.addr, "--token", srv.token,
		}, &out, &errOut); c != ExitOK {
			t.Fatalf("exit code = %d; stderr: %s", c, errOut.String())
		}
	})

	t.Run("--output json reports the same thing to a script", func(t *testing.T) {
		var out, errOut syncBuffer
		if c := Execute(t.Context(), []string{
			"zone", "export", "example.com", "--server", srv.addr, "--token", srv.token,
			"--output", "json",
		}, &out, &errOut); c != ExitOK {
			t.Fatalf("exit code = %d; stderr: %s", c, errOut.String())
		}
		var got zoneExported
		if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
			t.Fatalf("decode: %v (%s)", err, out.String())
		}
		if got.Zone != "example.com." || !strings.Contains(got.Content, "$ORIGIN") {
			t.Errorf("json = %+v, want the zone and its file", got)
		}
	})
}

func TestZoneImportFromAPipe(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	var stdout, stderr syncBuffer
	opts := &options{stdout: &stdout, stderr: &stderr, stdin: strings.NewReader(zonefileText)}
	root := newRootCommand(opts)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"zone", "import", "--server", srv.addr, "--token", srv.token})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v; stderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported example.com.") {
		t.Errorf("output = %q", stdout.String())
	}
}

func TestZoneCommandFailures(t *testing.T) {
	t.Parallel()

	t.Run("no server there", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr syncBuffer
		code := Execute(t.Context(), []string{
			"zone", "export", "example.com", "--server", "127.0.0.1:1", "--token", "weg_x",
		}, &stdout, &stderr)
		if code != ExitError {
			t.Errorf("exit code = %d, want %d", code, ExitError)
		}
		if !strings.Contains(stderr.String(), "is it running") {
			t.Errorf("stderr = %q, want advice rather than a dial error", stderr.String())
		}
	})

	t.Run("a zone that is not there", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t)

		var stdout, stderr syncBuffer
		code := Execute(t.Context(), []string{
			"zone", "export", "nowhere.example", "--server", srv.addr, "--token", srv.token,
		}, &stdout, &stderr)
		if code != ExitError {
			t.Errorf("exit code = %d, want %d", code, ExitError)
		}
		if !strings.Contains(stderr.String(), "nowhere.example.") {
			t.Errorf("stderr = %q, want it to name the zone", stderr.String())
		}
	})

	t.Run("a file that is not a zone", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t)

		path := filepath.Join(t.TempDir(), "fragment")
		writeFile(t, path, "www.example.com. IN A 192.0.2.1\n")

		var stdout, stderr syncBuffer
		code := Execute(t.Context(), []string{
			"zone", "import", path, "--server", srv.addr, "--token", srv.token,
		}, &stdout, &stderr)
		if code != ExitError {
			t.Errorf("exit code = %d, want %d", code, ExitError)
		}
		if !strings.Contains(stderr.String(), "fragment") {
			t.Errorf("stderr = %q, want the server's reason", stderr.String())
		}
	})

	t.Run("a file that is not there", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr syncBuffer
		code := Execute(t.Context(), []string{
			"zone", "import", "/nonexistent/db.example.com",
			"--server", "127.0.0.1:1", "--token", "weg_x",
		}, &stdout, &stderr)
		if code != ExitError {
			t.Errorf("exit code = %d, want %d", code, ExitError)
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestZoneWithoutAToken has no t.Parallel anywhere in its chain: it clears the
// environment variable a token would otherwise come from, and that is
// process-wide.
func TestZoneWithoutAToken(t *testing.T) {
	t.Setenv(tokenEnv, "")

	var stdout, stderr syncBuffer
	code := Execute(t.Context(), []string{"zone", "export", "example.com"}, &stdout, &stderr)

	if code != ExitUsage {
		t.Errorf("exit code = %d, want %d", code, ExitUsage)
	}
	// The message has to say where a token comes from. "unauthorized" would
	// leave somebody who has never run this before with nowhere to go.
	if !strings.Contains(stderr.String(), tokenEnv) {
		t.Errorf("stderr = %q, want it to name the environment variable", stderr.String())
	}
}
