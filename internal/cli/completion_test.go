package cli

import (
	"strings"
	"testing"
)

// complete asks the command tree for suggestions the way a shell does: through
// the hidden __complete command cobra installs for exactly this.
func complete(t *testing.T, srv server, args ...string) []string {
	t.Helper()

	// The connection flags go after the command path and before anything the
	// shell is completing. Appending them would make the token the word being
	// completed; putting them last-but-one would feed them to a flag that is
	// waiting for its own value.
	at := len(args) - 1
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			at = i
			break
		}
	}
	full := []string{"__complete"}
	full = append(full, args[:at]...)
	full = append(full, "--server", srv.addr, "--token", srv.token)
	full = append(full, args[at:]...)

	var stdout, stderr syncBuffer
	if code := Execute(t.Context(), full, &stdout, &stderr); code != ExitOK {
		t.Fatalf("%v: exit code %d; stderr: %s", args, code, stderr.String())
	}

	var out []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		// The last lines are cobra's directive and its own commentary.
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "Completion ended") {
			continue
		}
		out = append(out, strings.SplitN(line, "\t", 2)[0])
	}
	return out
}

func has(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestCompletion(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	mustRun(t, srv, "zone", "create", "example.com", "--ttl", "300")
	mustRun(t, srv, "zone", "create", "2.0.192.in-addr.arpa")
	mustRun(t, srv, "record", "add", "example.com", "www", "A", "192.0.2.10")
	mustRun(t, srv, "record", "add", "example.com", "www", "AAAA", "2001:db8::10")
	mustRun(t, srv, "record", "add", "example.com", "mail", "A", "192.0.2.20")

	t.Run("zone names come from the server", func(t *testing.T) {
		got := complete(t, srv, "zone", "show", "")
		if !has(got, "example.com.") || !has(got, "2.0.192.in-addr.arpa.") {
			t.Errorf("suggestions = %v, want both zones", got)
		}
	})

	t.Run("a prefix narrows them", func(t *testing.T) {
		got := complete(t, srv, "zone", "delete", "exam")
		if !has(got, "example.com.") {
			t.Errorf("suggestions = %v, want the zone that matches", got)
		}
		if has(got, "2.0.192.in-addr.arpa.") {
			t.Errorf("suggestions = %v, want only what the prefix matches", got)
		}
	})

	t.Run("record names come from the zone", func(t *testing.T) {
		got := complete(t, srv, "record", "delete", "example.com", "")
		for _, want := range []string{"www.example.com.", "mail.example.com.", "example.com."} {
			if !has(got, want) {
				t.Errorf("suggestions = %v, want %q", got, want)
			}
		}
		// One suggestion per name, not one per record: www has two.
		var www int
		for _, g := range got {
			if g == "www.example.com." {
				www++
			}
		}
		if www != 1 {
			t.Errorf("www.example.com. was suggested %d times, want once", www)
		}
	})

	// For a record that has to exist, the types offered are the ones the name
	// actually has. Offering AAAA where there is none is offering a command
	// that will fail.
	t.Run("types come from the name for a record that must exist", func(t *testing.T) {
		got := complete(t, srv, "record", "delete", "example.com", "mail.example.com.", "")
		if !has(got, "A") {
			t.Errorf("suggestions = %v, want the type it has", got)
		}
		if has(got, "AAAA") || has(got, "TXT") {
			t.Errorf("suggestions = %v, want only the types this name has", got)
		}
	})

	t.Run("adding a record gets the whole vocabulary", func(t *testing.T) {
		got := complete(t, srv, "record", "add", "example.com", "new.example.com.", "")
		if !has(got, "AAAA") || !has(got, "TXT") || !has(got, "SRV") {
			t.Errorf("suggestions = %v, want the types a person types", got)
		}
	})

	t.Run("flags with a closed vocabulary suggest it", func(t *testing.T) {
		if got := complete(t, srv, "zone", "list", "--kind", ""); !has(got, "forward") || !has(got, "reverse") {
			t.Errorf("suggestions = %v, want the kinds", got)
		}
		if got := complete(t, srv, "token", "create", "x", "--scope", ""); !has(got, "admin") {
			t.Errorf("suggestions = %v, want the scopes", got)
		}
		if got := complete(t, srv, "history", "list", "--kind", ""); !has(got, "rollback") {
			t.Errorf("suggestions = %v, want the commit kinds", got)
		}
	})

	t.Run("tokens that can still be revoked", func(t *testing.T) {
		mustRun(t, srv, "token", "create", "doomed", "--scope", "read")
		if got := complete(t, srv, "token", "revoke", ""); !has(got, "doomed") {
			t.Errorf("suggestions = %v, want the token", got)
		}

		mustRun(t, srv, "token", "revoke", "doomed", "--yes")
		if got := complete(t, srv, "token", "revoke", ""); has(got, "doomed") {
			t.Errorf("suggestions = %v, want the revoked token left out", got)
		}
	})

	// A shell asks on a keystroke. A server that is not there has to become
	// "no suggestions" rather than a hung terminal or a wall of error text.
	t.Run("a server that is not there suggests nothing", func(t *testing.T) {
		var stdout, stderr syncBuffer
		code := Execute(t.Context(), []string{
			"__complete", "zone", "show", "",
			"--server", "http://127.0.0.1:1", "--token", "weg_nothing",
		}, &stdout, &stderr)

		if code != ExitOK {
			t.Errorf("exit code = %d, want %d — a shell is not a place to report errors", code, ExitOK)
		}
		for _, line := range strings.Split(stdout.String(), "\n") {
			if line != "" && !strings.HasPrefix(line, ":") {
				t.Errorf("suggested %q with no server to ask", line)
			}
		}
	})
}
