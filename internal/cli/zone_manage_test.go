package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// run executes one command against srv and returns what it printed.
func run(t *testing.T, srv server, args ...string) (code int, out, errOut string) {
	t.Helper()

	var stdout, stderr syncBuffer
	code = Execute(t.Context(), append(args, "--server", srv.addr, "--token", srv.token),
		&stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// mustRun executes a command and fails the test if it did not succeed.
func mustRun(t *testing.T, srv server, args ...string) string {
	t.Helper()

	code, out, errOut := run(t, srv, args...)
	if code != ExitOK {
		t.Fatalf("%v: exit code %d; stderr: %s", args, code, errOut)
	}
	return out
}

func TestZoneCreateListShowDelete(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	t.Run("an empty server says what to do about it", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "list")
		if !strings.Contains(out, "no zones") || !strings.Contains(out, "weg zone create") {
			t.Errorf("output = %q, want an empty state that says what comes next", out)
		}
	})

	// The short form: a name and nothing else. Everything the zone needs comes
	// from the server's defaults, which is the whole point of the API change
	// this command was built on.
	out := mustRun(t, srv, "zone", "create", "example.com")
	if !strings.Contains(out, "created example.com. (forward)") || !strings.Contains(out, "serial 1") {
		t.Fatalf("output = %q, want the zone, its kind and its serial", out)
	}
	if !strings.Contains(out, "ns1.example.com.") {
		t.Errorf("output = %q, want the name server it was given", out)
	}

	t.Run("a reverse zone is recognised by its name", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "create", "2.0.192.in-addr.arpa")
		if !strings.Contains(out, "(reverse)") {
			t.Errorf("output = %q, want it recognised as a reverse zone", out)
		}
	})

	t.Run("the listing lines up and holds both", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "list")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 3 {
			t.Fatalf("listing has %d lines, want a header and two zones:\n%s", len(lines), out)
		}
		if !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "STATUS") {
			t.Errorf("header = %q, want the column titles", lines[0])
		}
		// The columns are aligned, so the name column has the same width in
		// every row. A misaligned table is what a coloured cell in the middle
		// would produce.
		for _, l := range lines[1:] {
			if !strings.Contains(l, "enabled") {
				t.Errorf("row = %q, want a status", l)
			}
		}
		if col := strings.Index(lines[1], "forward"); col != strings.Index(lines[2], "reverse") {
			t.Errorf("the kind column starts in a different place on each row:\n%s", out)
		}
	})

	t.Run("--kind narrows it", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "list", "--kind", "reverse")
		if strings.Contains(out, "example.com.") {
			t.Errorf("output = %q, want only the reverse zone", out)
		}
		if !strings.Contains(out, "in-addr.arpa.") {
			t.Errorf("output = %q, want the reverse zone", out)
		}
	})

	t.Run("a kind that is not one is the user's mistake", func(t *testing.T) {
		code, _, errOut := run(t, srv, "zone", "list", "--kind", "sideways")
		if code != ExitUsage {
			t.Errorf("exit code = %d, want %d", code, ExitUsage)
		}
		if !strings.Contains(errOut, "forward or reverse") {
			t.Errorf("stderr = %q, want it to say what a kind is", errOut)
		}
	})

	t.Run("show prints the zone itself", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "show", "example.com")
		for _, want := range []string{
			"name", "example.com.", "primary ns", "ns1.example.com.",
			"mailbox", "hostmaster.example.com.", "reverse automation", "follows the server",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output does not mention %q:\n%s", want, out)
			}
		}
	})

	t.Run("--output json is the same thing for a script", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "list", "--output", "json")
		var zones []struct {
			Name   string `json:"name"`
			Kind   string `json:"kind"`
			Serial int64  `json:"serial"`
		}
		if err := json.Unmarshal([]byte(out), &zones); err != nil {
			t.Fatalf("decode: %v\n%s", err, out)
		}
		if len(zones) != 2 {
			t.Fatalf("decoded %d zones, want 2", len(zones))
		}
		if zones[0].Serial == 0 {
			t.Errorf("zone = %+v, want a serial", zones[0])
		}
	})

	t.Run("deleting asks first", func(t *testing.T) {
		var stdout, stderr syncBuffer
		code := Execute(t.Context(), []string{
			"zone", "delete", "example.com", "--server", srv.addr, "--token", srv.token,
		}, &stdout, &stderr)

		// No terminal and no answer: the command says how to mean it rather
		// than doing it.
		if code != ExitUsage {
			t.Errorf("exit code = %d, want %d", code, ExitUsage)
		}
		if !strings.Contains(stderr.String(), "--yes") {
			t.Errorf("stderr = %q, want it to say how to confirm", stderr.String())
		}
		if out := mustRun(t, srv, "zone", "list"); !strings.Contains(out, "example.com.") {
			t.Error("the zone was deleted without an answer")
		}
	})

	t.Run("--yes means it", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "delete", "example.com", "--yes")
		if !strings.Contains(out, "deleted example.com.") || !strings.Contains(out, "journal") {
			t.Errorf("output = %q, want the deletion and what survives it", out)
		}
		if got := mustRun(t, srv, "zone", "list"); strings.Contains(got, "example.com.") {
			t.Errorf("the zone is still listed:\n%s", got)
		}
	})

	t.Run("a zone that is not there says so", func(t *testing.T) {
		code, _, errOut := run(t, srv, "zone", "show", "nothere.example.")
		if code != ExitError {
			t.Errorf("exit code = %d, want %d", code, ExitError)
		}
		if !strings.Contains(errOut, "nothere.example.") {
			t.Errorf("stderr = %q, want the name that was not found", errOut)
		}
	})
}

// An address is what a person has in their head; the RNAME of RFC 1035 §3.3.13
// is what the zone holds.
func TestMailbox(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "hostmaster@example.com", want: "hostmaster.example.com"},
		// A dot in the local part is part of the label, not a separator, or
		// this would claim a mailbox "last" in a host "first".
		{in: "first.last@example.com", want: `first\.last.example.com`},
		// Already a DNS name: left alone, because rewriting it would be
		// guessing at what somebody typed on purpose.
		{in: "hostmaster.example.com.", want: "hostmaster.example.com."},
		{in: "@example.com", wantErr: true},
		{in: "hostmaster@", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()

			got, err := mailbox(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mailbox(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mailbox(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("mailbox(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The prompt itself, which Execute cannot reach because a command line has no
// way to hand it an answer.
func TestConfirm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		answer string
		want   error
	}{
		{answer: "y\n"},
		{answer: "yes\n"},
		{answer: "Y\n"},
		{answer: "  y  \n"},
		{answer: "n\n", want: errCancelled},
		{answer: "\n", want: errCancelled},
		// Anything that is not a yes is a no. A destructive command is not the
		// place to guess at what somebody meant.
		{answer: "sure\n", want: errCancelled},
		{answer: "", want: errCancelled},
	}

	for _, tc := range tests {
		t.Run(strings.TrimSpace(tc.answer), func(t *testing.T) {
			t.Parallel()

			var stderr syncBuffer
			opts := &options{stderr: &stderr, stdin: strings.NewReader(tc.answer)}
			if got := confirm(opts, "delete everything"); !errors.Is(got, tc.want) {
				t.Errorf("confirm(%q) = %v, want %v", tc.answer, got, tc.want)
			}
			if !strings.Contains(stderr.String(), "delete everything? [y/N]") {
				t.Errorf("the question was not asked: %q", stderr.String())
			}
		})
	}
}

// A name server inside the zone it serves needs an address in that zone.
//
// Without one this server answers NXDOMAIN, authoritatively, for its own name
// server: the delegation is lame (RFC 1912 §2.8) and nothing else about the
// zone looks wrong. Nothing is invented for it (this server does not know
// which of its addresses the world reaches it on) so it is said instead.
func TestZoneNameServerAddress(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	t.Run("without one, creating says so where it can be fixed", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "create", "lame.example")
		if !strings.Contains(out, "no address yet") {
			t.Errorf("output = %q, want it to say the name server has no address", out)
		}
		if !strings.Contains(out, "weg record add") {
			t.Errorf("output = %q, want it to say what fixes it", out)
		}
	})

	t.Run("and showing the zone keeps saying so", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "show", "lame.example")
		if !strings.Contains(out, "warning:") || !strings.Contains(out, "ns1.lame.example.") {
			t.Errorf("output = %q, want a warning naming the name server", out)
		}
	})

	t.Run("with one, the record is written and nothing warns", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "create", "good.example",
			"--ns-address", "192.0.2.10", "--ns-address", "2001:db8::10")
		if !strings.Contains(out, "ns1.good.example. A 192.0.2.10") ||
			!strings.Contains(out, "ns1.good.example. AAAA 2001:db8::10") {
			t.Errorf("output = %q, want both address records", out)
		}
		if strings.Contains(out, "no address yet") {
			t.Errorf("output = %q, want no warning", out)
		}

		shown := mustRun(t, srv, "zone", "show", "good.example")
		if strings.Contains(shown, "warning:") {
			t.Errorf("zone show still warns:\n%s", shown)
		}
	})

	// A name server outside the zone needs nothing from us: it is resolved the
	// ordinary way, which is what an off-site secondary is for.
	t.Run("an off-site name server is not a defect", func(t *testing.T) {
		mustRun(t, srv, "zone", "create", "offsite.example", "--ns", "ns1.elsewhere.example.")

		shown := mustRun(t, srv, "zone", "show", "offsite.example")
		if strings.Contains(shown, "warning:") {
			t.Errorf("an off-site name server was reported as a defect:\n%s", shown)
		}
	})
}
