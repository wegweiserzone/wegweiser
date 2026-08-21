package cli

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// tailUntil runs weg query tail against srv, feeding it exchanges until the
// output holds want, and then stops it the way a person would.
func tailUntil(
	t *testing.T, srv server, args []string, feed []dns.Event, want string,
) (out, errOut string) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, append(args, "--server", srv.addr, "--token", srv.token),
			&stdout, &stderr)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stdout.String(), want) {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("waited for %q; stdout:\n%s\nstderr:\n%s", want, stdout.String(), stderr.String())
		}
		for _, ev := range feed {
			srv.stream.Observe(ev)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Ctrl-C is how a tail ends, and ending it is not a failure: the same
	// rule weg serve follows for a signal.
	cancel()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Errorf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tail did not stop when its context was cancelled")
	}
	return stdout.String(), stderr.String()
}

// exchange is one query for the stream to carry.
func exchange(name string, typ zone.RRType, rcode int) dns.Event {
	return dns.Event{
		At:        time.Now(),
		Latency:   250 * time.Microsecond,
		Client:    netip.MustParseAddrPort("192.0.2.50:41234"),
		Transport: dns.UDP,
		Name:      name,
		Type:      typ,
		Class:     zone.ClassIN,
		Rcode:     rcode,
		Size:      100,
	}
}

func TestQueryTail(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	feed := []dns.Event{
		exchange("www.example.com.", zone.TypeA, 0),
		exchange("www.example.com.", zone.TypeAAAA, 0), // wrong type
		exchange("www.example.net.", zone.TypeA, 0),    // wrong zone
		exchange("gone.example.com.", zone.TypeA, 3),   // NXDOMAIN, same filter
	}
	args := []string{"query", "tail", "--name", "example.com.", "--type", "A"}

	stdout, _ := tailUntil(t, srv, args, feed, "www.example.com.")

	t.Run("it names the columns", func(t *testing.T) {
		if !strings.HasPrefix(stdout, "TIME") || !strings.Contains(stdout, "RCODE") {
			t.Errorf("output does not start with column titles:\n%s", first(stdout))
		}
	})

	t.Run("it shows what a person came to see", func(t *testing.T) {
		for _, want := range []string{"192.0.2.50:41234", "udp", "NOERROR", "250µs", "A"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("output does not mention %q:\n%s", want, first(stdout))
			}
		}
	})

	t.Run("the filter is the server's, and it holds", func(t *testing.T) {
		// Both of these were offered to the hub and neither may appear: the
		// filter runs before the stream, not in this command.
		for _, unwanted := range []string{"example.net.", "AAAA"} {
			if strings.Contains(stdout, unwanted) {
				t.Errorf("output holds %q, which the filter excludes:\n%s", unwanted, first(stdout))
			}
		}
		// A denial matches the same filter and is exactly what somebody
		// watching is usually looking for.
		if !strings.Contains(stdout, "NXDOMAIN") {
			t.Errorf("output has no denial in it:\n%s", first(stdout))
		}
	})
}

// A stream is read by scripts as often as by people, so it is one JSON object
// per exchange and nothing else on standard output.
func TestQueryTailJSON(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	stdout, _ := tailUntil(t, srv,
		[]string{"query", "tail", "--output", "json"},
		[]dns.Event{exchange("www.example.com.", zone.TypeA, 0)},
		"www.example.com.")

	dec := json.NewDecoder(strings.NewReader(stdout))
	var got struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		Rcode     string `json:"rcode"`
		Client    string `json:"client"`
		Port      int    `json:"port"`
		LatencyUs int    `json:"latencyUs"`
		Transport string `json:"transport"`
	}
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode the first exchange: %v\n%s", err, first(stdout))
	}
	if got.Name != "www.example.com." || got.Type != "A" || got.Rcode != "NOERROR" {
		t.Errorf("event = %+v, want the exchange that was observed", got)
	}
	if got.Client != "192.0.2.50" || got.Port != 41234 || got.LatencyUs != 250 || got.Transport != "udp" {
		t.Errorf("event = %+v, want the client, port, latency and transport carried through", got)
	}
}

// A filter the server cannot read is the user's mistake, and saying so beats
// opening a stream that will never match anything.
func TestQueryTailRejectsABadFilter(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	var stdout, stderr syncBuffer
	code := Execute(t.Context(), []string{
		"query", "tail", "--type", "NOTATYPE", "--server", srv.addr, "--token", srv.token,
	}, &stdout, &stderr)

	if code != ExitError {
		t.Errorf("exit code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "NOTATYPE") {
		t.Errorf("stderr = %q, want the type that could not be read", stderr.String())
	}
}

// first is the opening lines of some output, for a failure message that has to
// stay readable when the stream produced hundreds.
func first(s string) string {
	lines := strings.SplitN(s, "\n", 6)
	if len(lines) > 5 {
		lines = lines[:5]
	}
	return strings.Join(lines, "\n")
}
