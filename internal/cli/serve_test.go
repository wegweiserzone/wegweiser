package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/api"
	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// seedDatabase writes one zone with one record and returns the database path,
// so that a server started against it has something to answer with.
func seedDatabase(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "weg.db")
	st, err := sqlite.Open(t.Context(), sqlite.Options{Path: path})
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("close the database: %v", cerr)
		}
	}()

	if merr := st.Migrate(t.Context()); merr != nil {
		t.Fatalf("migrate: %v", merr)
	}

	z, err := zone.NewZone(
		zone.MustParseName("example.com."),
		zone.DefaultSOA(
			zone.MustParseName("ns1.example.com."),
			zone.MustParseName("hostmaster.example.com."),
		),
	)
	if err != nil {
		t.Fatalf("build the zone: %v", err)
	}
	z.ID = zone.ZoneID(id.New())

	rec, err := zone.NewRecord(z.ID, zone.MustParseName("www.example.com."),
		zone.ClassIN, zone.TypeA, 300, "192.0.2.10")
	if err != nil {
		t.Fatalf("build the record: %v", err)
	}
	rec.ID = zone.RecordID(id.New())

	if err := st.Update(t.Context(), func(tx store.Tx) error {
		if cerr := tx.CreateZone(t.Context(), &z); cerr != nil {
			return cerr
		}
		return tx.InsertRecord(t.Context(), &rec)
	}); err != nil {
		t.Fatalf("seed the database: %v", err)
	}

	return path
}

// TestServe is the whole binary doing its job: a database on disk becomes a
// server on a socket that answers a real query, and a cancelled context brings
// it down again with a success code.
func TestServe(t *testing.T) {
	t.Parallel()

	path := seedDatabase(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// A pipe rather than a buffer, so the status can be read while the command
	// is still running, which is the whole point of a server command.
	pr, pw := io.Pipe()
	defer pr.Close()

	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		code := Execute(ctx, []string{
			"serve", "--listen", "127.0.0.1:0", "--api-listen", "127.0.0.1:0",
			"--db", path, "--output", "json",
		}, pw, &stderr)
		pw.Close()
		done <- code
	}()

	var status serveStatus
	if err := json.NewDecoder(pr).Decode(&status); err != nil {
		t.Fatalf("read the status the server reports: %v (stderr: %s)", err, stderr.String())
	}

	if status.Zones != 1 || status.Records != 2 {
		t.Errorf("reported %d zones and %d records, want 1 and 2 (the record and its SOA)",
			status.Zones, status.Records)
	}
	if status.Database != path {
		t.Errorf("reported the database as %q, want %q", status.Database, path)
	}
	if _, port, err := net.SplitHostPort(status.Address); err != nil || port == "0" {
		t.Fatalf("reported the address as %q, which is not one that was bound", status.Address)
	}

	t.Run("it answers a query", func(t *testing.T) {
		got := ask(t, status.Address, "www.example.com.", zone.TypeA)
		if got.Rcode != wire.RcodeSuccess || !got.Authoritative {
			t.Errorf("rcode = %s, AA = %v, want NOERROR with AA set",
				wire.RcodeToString[got.Rcode], got.Authoritative)
		}
		if len(got.Answer) != 1 || !strings.Contains(got.Answer[0].String(), "192.0.2.10") {
			t.Errorf("answer = %v, want the seeded address", got.Answer)
		}
	})

	t.Run("the API is up and says it is serving", func(t *testing.T) {
		resp, err := http.Get("http://" + status.APIAddress + "/api/v1/healthz")
		if err != nil {
			t.Fatalf("get the health endpoint: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		var health struct {
			Status  string `json:"status"`
			Zones   int    `json:"zones"`
			Records int    `json:"records"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
			t.Fatalf("decode the health document: %v", err)
		}
		if health.Status != "serving" || health.Zones != 1 {
			t.Errorf("health = %+v, want it serving one zone", health)
		}
	})

	t.Run("the first start shows an administrator token once", func(t *testing.T) {
		if got := stderr.String(); !strings.Contains(got, "weg_") {
			t.Errorf("stderr = %q, want the bootstrap token in it", got)
		}
	})

	t.Run("it denies a name the zone does not hold", func(t *testing.T) {
		if got := ask(t, status.Address, "nothere.example.com.", zone.TypeA); got.Rcode != wire.RcodeNameError {
			t.Errorf("rcode = %s, want NXDOMAIN", wire.RcodeToString[got.Rcode])
		}
	})

	// The query path, the metrics and the API are three packages that know
	// nothing about each other; what connects them is the wiring above, and
	// this is where a wire left unattached shows. It runs after the queries
	// above, so both an answer and a denial have been counted by now.
	t.Run("it exports what it has been doing", func(t *testing.T) {
		out := scrape(t, status.APIAddress, bootstrapToken(t, stderr.String()))

		for _, want := range []string{
			`weg_dns_queries_total{rcode="NOERROR",transport="udp",type="A"} 1`,
			`weg_dns_queries_total{rcode="NXDOMAIN",transport="udp",type="A"} 1`,
			// The snapshot gauges follow every publish, including the first
			// one at startup, which no write is responsible for.
			"weg_snapshot_zones 1",
			"weg_snapshot_records 2",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the exposition does not say %q", want)
			}
		}
	})

	cancel()

	select {
	case code := <-done:
		// A signal is how a server is meant to be stopped, so stopping is a
		// success. Anything else makes every restart look like a crash to
		// whatever supervises the process.
		if code != ExitOK {
			t.Errorf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop after its context was cancelled")
	}

	if got := stderr.String(); !strings.Contains(got, "draining") {
		t.Errorf("stderr = %q, want it to say that it is stopping", got)
	}
}

// TestServeReportsAnUnusableDatabase checks that a server which cannot start
// says why and fails, rather than coming up with nothing to answer.
func TestServeReportsAnUnusableDatabase(t *testing.T) {
	t.Parallel()

	var stdout, stderr syncBuffer
	code := Execute(t.Context(), []string{
		"serve", "--listen", "127.0.0.1:0", "--api-listen", "127.0.0.1:0",
		"--db", filepath.Join(t.TempDir(), "no", "such", "weg.db"),
	}, &stdout, &stderr)

	if code != ExitError {
		t.Errorf("exit code = %d, want %d", code, ExitError)
	}
	if stdout.String() != "" {
		t.Errorf("a server that never started reported %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "weg:") {
		t.Errorf("stderr = %q, want it to name the failure", stderr.String())
	}
}

// TestServeRejectsAnUnusableAddress checks that a listen address which cannot
// be bound is a failure and not a server that quietly answers nothing.
func TestServeRejectsAnUnusableAddress(t *testing.T) {
	t.Parallel()

	var stdout, stderr syncBuffer
	code := Execute(t.Context(), []string{
		"serve", "--listen", "127.0.0.1:not-a-port", "--api-listen", "127.0.0.1:0",
		"--db", seedDatabase(t),
	}, &stdout, &stderr)

	if code != ExitError {
		t.Errorf("exit code = %d, want %d", code, ExitError)
	}
	if stdout.String() != "" {
		t.Errorf("a server that never started reported %q", stdout.String())
	}
}

// ask sends one query over UDP and returns the response.
func ask(t *testing.T, addr, name string, qtype zone.RRType) *wire.Msg {
	t.Helper()

	m := new(wire.Msg)
	m.SetQuestion(name, uint16(qtype))
	query, err := m.Pack()
	if err != nil {
		t.Fatalf("pack the query: %v", err)
	}

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err = conn.Write(query); err != nil {
		t.Fatalf("write the query: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}

	got := new(wire.Msg)
	if err := got.Unpack(buf[:n]); err != nil {
		t.Fatalf("the response does not parse: %v", err)
	}
	return got
}

// syncBuffer collects output that is written from more than one goroutine. The
// server reports faults from every reader it has, so an ordinary buffer here
// would be a data race rather than a convenience.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestServeLogsAreStructured checks that the fault stream follows --output.
// A flag that asks for something a script can read should not be contradicted
// by the stream the faults come out on.
func TestServeLogsAreStructured(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		format string
		check  func(t *testing.T, line string)
	}{
		{
			format: "text",
			check: func(t *testing.T, line string) {
				// logfmt: what journald and every log shipper reads unaided.
				if !strings.Contains(line, "level=INFO") || !strings.Contains(line, "msg=") {
					t.Errorf("line = %q, want logfmt with a level and a message", line)
				}
			},
		},
		{
			format: "json",
			check: func(t *testing.T, line string) {
				var event map[string]any
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatalf("line = %q, want JSON: %v", line, err)
				}
				if event["level"] != "INFO" || event["msg"] == "" {
					t.Errorf("event = %v, want a level and a message", event)
				}
				if _, ok := event["time"]; !ok {
					t.Error("the event carries no timestamp")
				}
			},
		},
	} {
		t.Run(tc.format, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			// A pipe, so that the run can be stopped once it is up rather than
			// after a timeout.
			pr, pw := io.Pipe()
			defer pr.Close()

			var stderr syncBuffer
			done := make(chan int, 1)
			go func() {
				code := Execute(ctx, []string{
					"serve", "--listen", "127.0.0.1:0", "--api-listen", "127.0.0.1:0",
					"--db", seedDatabase(t), "--output", tc.format,
				}, pw, &stderr)
				pw.Close()
				done <- code
			}()

			if _, err := bufio.NewReader(pr).ReadString('\n'); err != nil {
				t.Fatalf("the server never reported that it was up: %v (stderr: %s)",
					err, stderr.String())
			}
			cancel()
			if code := <-done; code != ExitOK {
				t.Fatalf("exit code = %d; stderr: %s", code, stderr.String())
			}

			// The shutdown notice is the one event every run produces.
			var line string
			for _, l := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
				if strings.Contains(l, "draining") {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("stderr = %q, want the shutdown event in it", stderr.String())
			}
			tc.check(t, line)
		})
	}
}

// bootstrapToken picks the administrator token out of what the first start
// printed. It is shown once and never again (docs/decisions/ D5), so this is
// the only place a test of a real process can get one.
func bootstrapToken(t *testing.T, printed string) string {
	t.Helper()

	i := strings.Index(printed, api.TokenPrefix)
	if i < 0 {
		t.Fatalf("no token in %q", printed)
	}
	tok := printed[i:]
	if j := strings.IndexFunc(tok, func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' }); j >= 0 {
		tok = tok[:j]
	}
	return tok
}

// scrape reads the metrics endpoint the way a monitoring system would.
func scrape(t *testing.T, apiAddr, token string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://"+apiAddr+"/api/v1/metrics", http.NoBody)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the exposition: %v", err)
	}
	return string(body)
}
