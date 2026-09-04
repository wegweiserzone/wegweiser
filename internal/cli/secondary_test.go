package cli

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestSecondaryConfig(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	mustRun(t, srv, "zone", "create", "example.com")
	mustRun(t, srv, "zone", "create", "192.0.2.0/24")
	mustRun(t, srv, "tsig", "create", "ns2.example.com.")
	mustRun(t, srv, "settings", "set",
		"--transfer-allow", "key:ns2.example.com.",
		"--notify", "198.51.100.53")

	t.Run("text is the file and nothing else", func(t *testing.T) {
		code, out, errOut := run(t, srv, "secondary", "config", "bind",
			"--primary", "192.0.2.1", "--secondary", "198.51.100.53")
		if code != ExitOK {
			t.Fatalf("code = %d, stderr: %s", code, errOut)
		}
		// The reverse zone as much as the forward one: it is the one somebody
		// setting a secondary up by hand leaves out.
		for _, want := range []string{
			`zone "example.com." {`,
			`zone "2.0.192.in-addr.arpa." {`,
			`key "ns2.example.com." {`,
			"primaries { 192.0.2.1; };",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the file is missing %q:\n%s", want, out)
			}
		}
		if errOut != "" {
			t.Errorf("a complete arrangement wrote to standard error:\n%s", errOut)
		}
	})

	t.Run("json carries the file and the warnings together", func(t *testing.T) {
		out := mustRun(t, srv, "secondary", "config", "knot",
			"--primary", "192.0.2.1", "--output", "json")

		var got secondaryConfigured
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		if got.Format != "knot" {
			t.Errorf("format = %q", got.Format)
		}
		if !strings.Contains(got.Content, "domain: example.com.") {
			t.Errorf("the file is missing a zone:\n%s", got.Content)
		}
		if got.Warnings == nil {
			t.Error("warnings is null, want a list a script can range over")
		}
	})

	t.Run("the zones it is given are the zones it writes", func(t *testing.T) {
		out := mustRun(t, srv, "secondary", "config", "bind",
			"--primary", "192.0.2.1", "example.com")
		if strings.Contains(out, "in-addr.arpa") {
			t.Errorf("a zone nobody asked for is in the file:\n%s", out)
		}
	})

	t.Run("--unsigned writes a file that signs nothing", func(t *testing.T) {
		out := mustRun(t, srv, "secondary", "config", "bind",
			"--primary", "192.0.2.1", "--unsigned")
		if strings.Contains(out, "ns2.example.com.") {
			t.Errorf("a key is in a configuration that signs nothing:\n%s", out)
		}
	})
}

func TestSecondaryConfigWarnsWithoutSpoilingTheFile(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	// A server nobody has configured for transfers, which is where one starts.
	mustRun(t, srv, "zone", "create", "example.com")

	code, out, errOut := run(t, srv, "secondary", "config", "bind", "--primary", "192.0.2.1")
	if code != ExitOK {
		t.Fatalf("code = %d: the file was written, so this is not a failure; stderr: %s",
			code, errOut)
	}
	if !strings.Contains(out, `zone "example.com." {`) {
		t.Errorf("nothing was written:\n%s", out)
	}
	if strings.Contains(out, "warning") {
		t.Errorf("a warning is in the file, where it would have to be deleted by hand:\n%s", out)
	}
	for _, want := range []string{"nobody may transfer", "nobody is told"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("standard error does not say %q:\n%s", want, errOut)
		}
	}
}

func TestSecondaryConfigWithoutAPrimary(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	code, _, errOut := run(t, srv, "secondary", "config", "bind")
	if code != ExitUsage {
		t.Errorf("code = %d, want a usage error", code)
	}
	if !strings.Contains(errOut, "does not know which of its addresses") {
		t.Errorf("the refusal does not say why it cannot guess:\n%s", errOut)
	}
	// The API here is on loopback, which says nothing about where a secondary
	// reaches this server, so the refusal offers nothing rather than an
	// address that cannot work.
	if strings.Contains(errOut, "Did you mean") {
		t.Errorf("it offered a loopback address as the primary:\n%s", errOut)
	}
}

// TestSecondaryConfigOffersTheHostItWasReachedAt covers the other half: an API
// on an address of its own is a plausible guess, and worth making rather than
// leaving the reader to find it. Nothing is dialled, so no server is needed.
func TestSecondaryConfigOffersTheHostItWasReachedAt(t *testing.T) {
	t.Parallel()

	var stdout, stderr syncBuffer
	code := Execute(t.Context(), []string{
		"secondary", "config", "bind", "--server", "192.0.2.1:8053", "--token", "weg_x",
	}, &stdout, &stderr)

	if code != ExitUsage {
		t.Errorf("code = %d, want a usage error", code)
	}
	if !strings.Contains(stderr.String(), "Did you mean --primary 192.0.2.1?") {
		t.Errorf("it did not offer the host it was pointed at:\n%s", stderr.String())
	}
}

func TestSecondaryConfigForSoftwareNobodyWritesFor(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	code, _, errOut := run(t, srv, "secondary", "config", "djbdns", "--primary", "192.0.2.1")
	if code == ExitOK {
		t.Error("it wrote a configuration for software it does not know")
	}
	if !strings.Contains(errOut, "no configuration is written for") {
		t.Errorf("the refusal does not say what is offered instead:\n%s", errOut)
	}
}

// standing is a fixed answer from the prober, so that the command is what the
// test is about rather than the asking.
type standing []dns.ProbeStanding

func (s standing) Standing() []dns.ProbeStanding { return s }

func TestSecondaryStatus(t *testing.T) {
	t.Parallel()

	target := netip.MustParseAddrPort("198.51.100.53:53")
	asked := time.Now().Add(-90 * time.Second)
	srv := newServer(t, func(c *api.Config) {
		c.Secondaries = standing{
			{
				Zone: zone.MustParseName("example.com."), Target: target,
				Outcome: dns.ProbeBehind, Serial: zone.NewSerial(9), Known: true,
				Lag: 3, At: asked,
			},
			{
				Zone: zone.MustParseName("other.example."), Target: target,
				Outcome: dns.ProbeInStep, Serial: zone.NewSerial(12), Known: true,
				At: asked,
			},
			{Zone: zone.MustParseName("fresh.example."), Target: target},
		}
	})

	t.Run("the table says how far behind each one is", func(t *testing.T) {
		out := mustRun(t, srv, "secondary", "status")
		for _, want := range []string{
			"SECONDARY", "ZONE", "STATE", "SERIAL", "BEHIND", "ASKED",
			"198.51.100.53:53", "example.com.", "in step", "unasked", "1m ago",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the table is missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("a zone nothing came back for is not reported as up to date", func(t *testing.T) {
		out := mustRun(t, srv, "secondary", "status", "--output", "json")

		var got []secondaryStanding
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, out)
		}
		if len(got) != 3 {
			t.Fatalf("%d entries, want three", len(got))
		}

		var fresh *secondaryStanding
		for i := range got {
			if got[i].Zone == "fresh.example." {
				fresh = &got[i]
			}
		}
		if fresh == nil {
			t.Fatalf("the unasked zone is missing:\n%s", out)
		}
		if fresh.State != "unasked" {
			t.Errorf("it reads %q, want unasked", fresh.State)
		}
		if fresh.Serial != nil || fresh.Lag != nil || fresh.AskedAt != nil {
			t.Errorf("it carries serial %v, lag %v, asked %v, want none of them",
				fresh.Serial, fresh.Lag, fresh.AskedAt)
		}
	})
}

// An empty notify list has nothing to ask, and saying so beats an empty table.
func TestSecondaryStatusWithNobodyToAsk(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	out := mustRun(t, srv, "secondary", "status")
	if !strings.Contains(out, "weg settings") {
		t.Errorf("the empty report does not point at where the list is:\n%s", out)
	}
}
