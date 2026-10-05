package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/api"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/cluster"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// clusterSecret is the secret every member in these tests holds.
var clusterSecret = []byte("0123456789abcdef0123456789abcdef")

// freeAddr is a loopback address nothing listens on, for a member that has to
// advertise its port before it binds it.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// clusterConfig writes a configuration file with a cluster section, and
// returns its path and the address the node advertises.
func clusterConfig(t *testing.T) (path, advertise string) {
	t.Helper()
	advertise = freeAddr(t)
	body := "cluster:\n" +
		"  listen: \"" + advertise + "\"\n" +
		"  advertise: \"" + advertise + "\"\n" +
		"  secret: \"" + base64.StdEncoding.EncodeToString(clusterSecret) + "\"\n"
	path = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	return path, advertise
}

type noLoad struct{}

func (noLoad) Load(context.Context) error { return nil }

// startFounder makes a cluster of one out of the seeded database, the way
// `weg cluster init` will, and returns the address of its cluster port.
func startFounder(t *testing.T) string {
	t.Helper()
	ctx := t.Context()

	st, err := sqlite.Open(ctx, sqlite.Options{Path: seedDatabase(t)})
	if err != nil {
		t.Fatalf("open the founder's database: %v", err)
	}
	repl := &cluster.Replication{}
	a, err := apply.New(st, apply.Options{Replication: repl})
	if err != nil {
		t.Fatalf("build the founder's applier: %v", err)
	}
	tr, err := cluster.New(cluster.Config{Secret: clusterSecret})
	if err != nil {
		t.Fatalf("build the founder's transport: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := tr.Serve(l)
	node, err := cluster.Start(cluster.NodeConfig{
		ID: "founder", Advertise: mux.Addr().String(), Dir: t.TempDir(),
		Transport: tr, Mux: mux, Store: st, Applier: a, Loader: noLoad{},
		OnError: func(err error) { t.Errorf("founder: %v", err) },
	})
	if err != nil {
		t.Fatalf("start the founder: %v", err)
	}
	repl.Bind(node)
	t.Cleanup(func() {
		if cerr := node.Close(); cerr != nil {
			t.Errorf("close the founder: %v", cerr)
		}
		if cerr := mux.Close(); cerr != nil {
			t.Errorf("close the founder's port: %v", cerr)
		}
		if cerr := st.Close(); cerr != nil {
			t.Errorf("close the founder's database: %v", cerr)
		}
	})
	if err := a.Exclusive(ctx, node.Init); err != nil {
		t.Fatalf("start the cluster: %v", err)
	}
	return mux.Addr().String()
}

// A node started with --join holds the cluster's data before it answers a
// single query, and mints nothing of its own on the way in
// (docs/decisions/d44-starting-and-joining.md).
func TestServeJoinsACluster(t *testing.T) {
	t.Parallel()
	founder := startFounder(t)
	conf, advertise := clusterConfig(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	defer pr.Close()
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		code := Execute(ctx, []string{
			"serve", "--config", conf, "--listen", "127.0.0.1:0", "--api-listen", "127.0.0.1:0",
			"--db", filepath.Join(t.TempDir(), "weg.db"), "--output", "json",
			"--join", founder,
		}, pw, &stderr)
		pw.Close()
		done <- code
	}()

	var status serveStatus
	if err := json.NewDecoder(pr).Decode(&status); err != nil {
		t.Fatalf("read the status: %v (stderr: %s)", err, stderr.String())
	}
	if status.Cluster == nil || !status.Cluster.Replicating || status.Cluster.Advertise != advertise {
		t.Errorf("cluster status = %+v, want a member advertising %s", status.Cluster, advertise)
	}
	if status.Zones != 1 {
		t.Errorf("the node reported %d zones at start, want the founder's 1", status.Zones)
	}
	got := ask(t, status.Address, "www.example.com.", zone.TypeA)
	if got.Rcode != wire.RcodeSuccess || len(got.Answer) != 1 {
		t.Errorf("rcode = %s, answer = %v, want the founder's record",
			wire.RcodeToString[got.Rcode], got.Answer)
	}

	cancel()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Errorf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the member did not stop after its context was cancelled")
	}
	if strings.Contains(stderr.String(), api.TokenPrefix) {
		t.Errorf("a joining node showed a bootstrap token of its own:\n%s", stderr.String())
	}
}

// What a node holds before it joins is in no entry of the cluster's log, so
// it joins with an empty database or not at all (D32).
func TestServeWillNotJoinWithData(t *testing.T) {
	t.Parallel()
	conf, _ := clusterConfig(t)

	var stdout, stderr syncBuffer
	code := Execute(t.Context(), []string{
		"serve", "--config", conf, "--listen", "127.0.0.1:0", "--api-listen", "127.0.0.1:0",
		"--db", seedDatabase(t), "--join", freeAddr(t),
	}, &stdout, &stderr)

	if code != ExitError {
		t.Errorf("exit code = %d, want %d", code, ExitError)
	}
	if got := stderr.String(); !strings.Contains(got, "empty database") || !strings.Contains(got, "1 zone") {
		t.Errorf("stderr = %q, want it to say the database has to be empty and what it holds", got)
	}
}

// A cluster section alone makes nobody a member: until a cluster is started
// or joined, the node is an ordinary server, bootstrap token and all (D44).
func TestServeWithAClusterSectionIsAServerUntilItJoins(t *testing.T) {
	t.Parallel()
	conf, _ := clusterConfig(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	defer pr.Close()
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		code := Execute(ctx, []string{
			"serve", "--config", conf, "--listen", "127.0.0.1:0", "--api-listen", "127.0.0.1:0",
			"--db", seedDatabase(t), "--output", "json",
		}, pw, &stderr)
		pw.Close()
		done <- code
	}()

	var status serveStatus
	if err := json.NewDecoder(pr).Decode(&status); err != nil {
		t.Fatalf("read the status: %v (stderr: %s)", err, stderr.String())
	}
	if status.Cluster == nil || status.Cluster.Replicating || status.Cluster.Member == "" {
		t.Errorf("cluster status = %+v, want a node with an identifier and no cluster yet", status.Cluster)
	}
	awaitStderr(t, &stderr, api.TokenPrefix)

	cancel()
	if code := <-done; code != ExitOK {
		t.Errorf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
	}
}

func TestServeRefusesJoinFlagsThatMeanNothing(t *testing.T) {
	t.Parallel()
	conf, _ := clusterConfig(t)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"--join without a cluster section", []string{"--join", "192.0.2.1:8054"}, "cluster section"},
		{"--role without --join", []string{"--config", conf, "--role", "nonvoter"}, "needs --join"},
		{"a role there is not", []string{"--config", conf, "--join", "192.0.2.1:8054", "--role", "witness"},
			"not a role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr syncBuffer
			args := append([]string{"serve", "--listen", "127.0.0.1:0", "--api-listen", "127.0.0.1:0",
				"--db", filepath.Join(t.TempDir(), "weg.db")}, tc.args...)
			if code := Execute(t.Context(), args, &stdout, &stderr); code == ExitOK {
				t.Fatalf("exit code = %d, want a failure", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to say %q", stderr.String(), tc.want)
			}
		})
	}
}

// The secret is the cluster: whoever reads it can speak as any member (D43).
// So showing the configuration says that there is one, and not what it is.
func TestConfigShowKeepsTheClusterSecret(t *testing.T) {
	t.Parallel()
	conf, _ := clusterConfig(t)

	var stdout, stderr syncBuffer
	if code := Execute(t.Context(), []string{"config", "show", "--config", conf}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, base64.StdEncoding.EncodeToString(clusterSecret)) || strings.Contains(out, string(clusterSecret)) {
		t.Errorf("the secret is in what was shown:\n%s", out)
	}
	if !strings.Contains(out, "(256 bits)") {
		t.Errorf("the output does not say there is a secret, and how long:\n%s", out)
	}
}
