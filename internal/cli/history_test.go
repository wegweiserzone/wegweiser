package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHistoryAndRollback(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	mustRun(t, srv, "zone", "create", "example.com", "--ttl", "300")
	mustRun(t, srv, "record", "add", "example.com", "www", "A", "192.0.2.10")
	mustRun(t, srv, "record", "add", "example.com", "www", "A", "192.0.2.11")

	t.Run("the listing holds every write", func(t *testing.T) {
		out := mustRun(t, srv, "history", "list", "example.com")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 4 {
			t.Fatalf("history has %d lines, want a header and three commits:\n%s", len(lines), out)
		}
		if !strings.HasPrefix(lines[0], "WHEN") || !strings.Contains(lines[0], "SERIAL") {
			t.Errorf("header = %q, want the column titles", lines[0])
		}
		if !strings.Contains(out, "zone_create") || !strings.Contains(out, "edit") {
			t.Errorf("output = %q, want the kinds of commit", out)
		}
		// D2: one commit is exactly one serial.
		for _, want := range []string{"0→1", "1→2", "2→3"} {
			if !strings.Contains(out, want) {
				t.Errorf("output does not show %q; a commit moves the serial by one:\n%s", want, out)
			}
		}
	})

	t.Run("--kind narrows it", func(t *testing.T) {
		out := mustRun(t, srv, "history", "list", "example.com", "--kind", "zone_create")
		if strings.Contains(out, "edit") {
			t.Errorf("output = %q, want only the creation", out)
		}
	})

	t.Run("a kind that is not one is the user's mistake", func(t *testing.T) {
		code, _, errOut := run(t, srv, "history", "list", "--kind", "sideways")
		if code != ExitUsage {
			t.Errorf("exit code = %d, want %d", code, ExitUsage)
		}
		if !strings.Contains(errOut, "rollback") {
			t.Errorf("stderr = %q, want it to list the kinds", errOut)
		}
	})

	t.Run("--since takes a date a person would type", func(t *testing.T) {
		today := time.Now().Format("2006-01-02")
		if out := mustRun(t, srv, "history", "list", "--since", today); !strings.Contains(out, "example.com.") {
			t.Errorf("output = %q, want today's commits", out)
		}
		tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
		if out := mustRun(t, srv, "history", "list", "--since", tomorrow); !strings.Contains(out, "no commits") {
			t.Errorf("output = %q, want nothing after today", out)
		}
	})

	// The commit identifiers come out of the JSON, which is how a script
	// would get one too.
	var commits []commitListed
	if err := json.Unmarshal(
		[]byte(mustRun(t, srv, "history", "list", "example.com", "--output", "json")),
		&commits,
	); err != nil {
		t.Fatalf("decode the history: %v", err)
	}
	if len(commits) != 3 {
		t.Fatalf("decoded %d commits, want 3", len(commits))
	}

	t.Run("show prints the change as a diff", func(t *testing.T) {
		// The newest commit is the second address being added.
		out := mustRun(t, srv, "history", "show", commits[0].ID)
		if !strings.Contains(out, commits[0].ID) || !strings.Contains(out, "edit") {
			t.Errorf("output = %q, want the commit and its kind", out)
		}
		if !strings.Contains(out, "+www.example.com. 300 IN A 192.0.2.11") {
			t.Errorf("output = %q, want the addition as a diff line", out)
		}
	})

	// Serial 2 is the state after the first address was added and before the
	// second: creating the zone was 0→1, and each edit one step on.
	t.Run("rolling back moves forward to an older state", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "rollback", "example.com", "2", "--yes")
		if !strings.Contains(out, "back at the state it had at serial 2") {
			t.Errorf("output = %q, want the state it was restored to", out)
		}
		// Forwards, not backwards: the zone lands on a new, higher serial,
		// because RFC 1982 makes the older number the older one and RFC 1995
		// has no way to say "go back".
		if !strings.Contains(out, "now serial 4") {
			t.Errorf("output = %q, want the new serial, one step on", out)
		}

		left := mustRun(t, srv, "record", "list", "example.com", "--name", "www")
		if strings.Contains(left, "192.0.2.11") || !strings.Contains(left, "192.0.2.10") {
			t.Errorf("the zone was not restored:\n%s", left)
		}
	})

	t.Run("rolling back to where it already is writes nothing", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "rollback", "example.com", "4", "--yes")
		if !strings.Contains(out, "already at serial 4") || !strings.Contains(out, "nothing was written") {
			t.Errorf("output = %q, want it to say nothing happened", out)
		}
	})

	t.Run("a rollback is in the history, and says where to", func(t *testing.T) {
		out := mustRun(t, srv, "history", "list", "example.com", "--kind", "rollback")
		if !strings.Contains(out, "back to serial 2") {
			t.Errorf("output = %q, want the rollback and its target", out)
		}
	})

	t.Run("rolling back asks first", func(t *testing.T) {
		code, _, errOut := run(t, srv, "zone", "rollback", "example.com", "2")
		if code != ExitUsage {
			t.Errorf("exit code = %d, want %d", code, ExitUsage)
		}
		if !strings.Contains(errOut, "--yes") {
			t.Errorf("stderr = %q, want it to say how to confirm", errOut)
		}
	})
}

func TestParseWhen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"2026-08-19", "2026-08-19 00:00:00"},
		{"2026-08-19 14:30", "2026-08-19 14:30:00"},
		{"2026-08-19 14:30:05", "2026-08-19 14:30:05"},
		{"2026-08-19T14:30:05+02:00", ""},
		{"the day before yesterday", ""},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()

			got, err := parseWhen(tc.in)
			switch {
			case tc.want == "" && tc.in == "the day before yesterday":
				if err == nil {
					t.Errorf("parseWhen(%q) = %v, want an error naming the forms", tc.in, got)
				}
				return
			case err != nil:
				t.Fatalf("parseWhen(%q): %v", tc.in, err)
			case tc.want == "":
				return // an RFC 3339 stamp carries its own zone
			}
			// A date a person types is the day they are having, not a day in
			// UTC: --since 2026-08-19 must not start eight hours early.
			if got.Location() != time.Local {
				t.Errorf("parseWhen(%q) is in %v, want the local zone", tc.in, got.Location())
			}
			if s := got.Format("2006-01-02 15:04:05"); s != tc.want {
				t.Errorf("parseWhen(%q) = %s, want %s", tc.in, s, tc.want)
			}
		})
	}
}

// Invariant 1: the filter the interface uses is a filter the command has.
func TestHistoryListFiltersByCause(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	mustRun(t, srv, "zone", "create", "example.com")
	mustRun(t, srv, "zone", "create", "192.0.2.0/24")
	mustRun(t, srv, "record", "add", "example.com", "www", "A", "192.0.2.10")

	// The reverse zone's commit is the server's own doing, so asking for what
	// people did leaves it out.
	people := mustRun(t, srv, "history", "list", "--source", "cli")
	if strings.Contains(people, "2.0.192.in-addr.arpa.") {
		t.Errorf("a change the server made on its own is listed as something a person did:\n%s", people)
	}

	system := mustRun(t, srv, "history", "list", "--source", "system")
	if !strings.Contains(system, "2.0.192.in-addr.arpa.") {
		t.Errorf("the reverse entry the server wrote is not listed:\n%s", system)
	}
	if strings.Contains(system, "example.com.  ") {
		t.Errorf("a change a person made is listed as the server's own:\n%s", system)
	}

	code, _, errOut := run(t, srv, "history", "list", "--source", "nonsense")
	if code == ExitOK || !strings.Contains(errOut, "is not a cause") {
		t.Errorf("an unknown cause was accepted: code %d, stderr: %s", code, errOut)
	}
}
