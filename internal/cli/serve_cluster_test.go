package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/api"
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

// serving starts `weg serve` with args in the background, and returns what it
// reported once it was answering and what it wrote to standard error. The
// server is stopped when the test ends, and has to stop cleanly.
func serving(t *testing.T, args ...string) (serveStatus, *syncBuffer) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	pr, pw := io.Pipe()
	stderr := &syncBuffer{}
	done := make(chan int, 1)
	go func() {
		code := Execute(ctx, append([]string{
			"serve", "--listen", "127.0.0.1:0", "--api-listen", "127.0.0.1:0", "--output", "json",
		}, args...), pw, stderr)
		pw.Close()
		done <- code
	}()
	t.Cleanup(func() {
		cancel()
		pr.Close()
		select {
		case code := <-done:
			if code != ExitOK {
				t.Errorf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Error("the server did not stop after its context was cancelled")
		}
	})

	var status serveStatus
	if err := json.NewDecoder(pr).Decode(&status); err != nil {
		t.Fatalf("read the status: %v (stderr: %s)", err, stderr.String())
	}
	return status, stderr
}

// Two servers become a cluster the way an operator makes one: the first is
// started as a cluster with `weg cluster init`, and the second joins it from
// its first start (docs/decisions/d44-starting-and-joining.md).
func TestTwoServersBecomeACluster(t *testing.T) {
	t.Parallel()

	confA, advertiseA := clusterConfig(t)
	a, stderrA := serving(t, "--config", confA, "--db", seedDatabase(t))
	token := bootstrapToken(t, awaitStderr(t, stderrA, api.TokenPrefix))

	var stdout, stderr syncBuffer
	if code := Execute(t.Context(), []string{
		"cluster", "init", "--server", a.APIAddress, "--token", token, "--output", "json",
	}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("cluster init: exit code %d; stderr: %s", code, stderr.String())
	}
	var started clusterStarted
	if err := json.Unmarshal([]byte(stdout.String()), &started); err != nil {
		t.Fatalf("read what cluster init reported: %v (%q)", err, stdout.String())
	}
	if started.Address != advertiseA || started.Member == "" {
		t.Errorf("cluster init reported %+v, want the first member at %s", started, advertiseA)
	}

	confB, advertiseB := clusterConfig(t)
	b, stderrB := serving(t, "--config", confB, "--db", filepath.Join(t.TempDir(), "weg.db"),
		"--join", started.Address)

	if b.Cluster == nil || !b.Cluster.Replicating || b.Cluster.Advertise != advertiseB {
		t.Errorf("cluster status = %+v, want a member advertising %s", b.Cluster, advertiseB)
	}
	t.Run("the new member answers from the cluster's data", func(t *testing.T) {
		if b.Zones != 1 {
			t.Errorf("it reported %d zones at start, want the founder's 1", b.Zones)
		}
		got := ask(t, b.Address, "www.example.com.", zone.TypeA)
		if got.Rcode != wire.RcodeSuccess || len(got.Answer) != 1 {
			t.Errorf("rcode = %s, answer = %v, want the founder's record",
				wire.RcodeToString[got.Rcode], got.Answer)
		}
	})
	// D32: credentials are the cluster's, so the founder's token works on the
	// new member, and the new member mints none of its own.
	t.Run("the founder's token works there, and it mints none", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			"http://"+b.APIAddress+"/api/v1/zones", http.NoBody)
		if err != nil {
			t.Fatalf("build the request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("list the zones on the new member: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("the founder's token on the new member: status %d, want 200", resp.StatusCode)
		}
		if strings.Contains(stderrB.String(), api.TokenPrefix) {
			t.Errorf("the new member showed a bootstrap token of its own:\n%s", stderrB.String())
		}
	})

	t.Run("a cluster is started once", func(t *testing.T) {
		var stdout, stderr syncBuffer
		if code := Execute(t.Context(), []string{
			"cluster", "init", "--server", a.APIAddress, "--token", token,
		}, &stdout, &stderr); code == ExitOK {
			t.Error("starting the cluster a second time succeeded")
		}
		if !strings.Contains(stderr.String(), "already") {
			t.Errorf("stderr = %q, want it to say this node is a member already", stderr.String())
		}
	})
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

	status, stderr := serving(t, "--config", conf, "--db", seedDatabase(t))
	if status.Cluster == nil || status.Cluster.Replicating || status.Cluster.Member == "" {
		t.Errorf("cluster status = %+v, want a node with an identifier and no cluster yet", status.Cluster)
	}
	awaitStderr(t, stderr, api.TokenPrefix)
}

// Without a cluster section, starting one is refused, and says what is missing.
func TestClusterInitNeedsAClusterSection(t *testing.T) {
	t.Parallel()
	status, stderr := serving(t, "--db", seedDatabase(t))
	token := bootstrapToken(t, awaitStderr(t, stderr, api.TokenPrefix))

	var stdout, errOut syncBuffer
	if code := Execute(t.Context(), []string{
		"cluster", "init", "--server", status.APIAddress, "--token", token,
	}, &stdout, &errOut); code == ExitOK {
		t.Fatal("a node without a cluster section started a cluster")
	}
	if !strings.Contains(errOut.String(), "cluster section") {
		t.Errorf("stderr = %q, want it to name the missing cluster section", errOut.String())
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
