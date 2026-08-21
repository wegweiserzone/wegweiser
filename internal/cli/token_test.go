package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTokens(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	t.Run("the bootstrap token is there", func(t *testing.T) {
		out := mustRun(t, srv, "token", "list")
		if !strings.HasPrefix(out, "NAME") || !strings.Contains(out, "admin") {
			t.Errorf("output = %q, want the bootstrap token", out)
		}
		if !strings.Contains(out, "active") {
			t.Errorf("output = %q, want its status", out)
		}
		// Whatever else it shows, it cannot show a secret: the server keeps a
		// hash and has nothing else to give.
		if strings.Contains(out, srv.token) {
			t.Error("the listing printed a secret")
		}
	})

	t.Run("minting puts the secret on stdout and the prose on stderr", func(t *testing.T) {
		code, out, errOut := run(t, srv, "token", "create", "ansible", "--scope", "write")
		if code != ExitOK {
			t.Fatalf("exit code = %d; stderr: %s", code, errOut)
		}
		// Capturable on its own: `WEG_TOKEN=$(weg token create ...)` has to
		// give a token and not a paragraph.
		secret := strings.TrimSpace(out)
		if !strings.HasPrefix(secret, "weg_") || strings.Contains(secret, " ") {
			t.Errorf("stdout = %q, want the secret and nothing else", out)
		}
		if !strings.Contains(errOut, "only time") {
			t.Errorf("stderr = %q, want it said that this is the one showing", errOut)
		}

		// And it works.
		other := server{addr: srv.addr, token: secret, stream: srv.stream}
		if got := mustRun(t, other, "zone", "list"); !strings.Contains(got, "no zones") {
			t.Errorf("the minted token did not work: %q", got)
		}
	})

	t.Run("a token needs a scope", func(t *testing.T) {
		code, _, errOut := run(t, srv, "token", "create", "nameless")
		if code != ExitUsage {
			t.Errorf("exit code = %d, want %d", code, ExitUsage)
		}
		if !strings.Contains(errOut, "--scope") {
			t.Errorf("stderr = %q, want it to say what is missing", errOut)
		}
	})

	t.Run("a scope that is not one is the user's mistake", func(t *testing.T) {
		code, _, errOut := run(t, srv, "token", "create", "x", "--scope", "root")
		if code != ExitUsage {
			t.Errorf("exit code = %d, want %d", code, ExitUsage)
		}
		if !strings.Contains(errOut, "read, write and admin") {
			t.Errorf("stderr = %q, want it to list the scopes", errOut)
		}
	})

	t.Run("--expires takes a date a person would type", func(t *testing.T) {
		mustRun(t, srv, "token", "create", "temporary", "--scope", "read",
			"--expires", "2030-01-01")

		var tokens []tokenListed
		if err := json.Unmarshal(
			[]byte(mustRun(t, srv, "token", "list", "--output", "json")), &tokens,
		); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, tok := range tokens {
			if tok.Name != "temporary" {
				continue
			}
			// Read in the local zone, so "2030-01-01" is the midnight the
			// person typing it means. In UTC that may be the evening before,
			// which is right and is why this compares locally.
			if tok.ExpiresAt == nil || tok.ExpiresAt.In(time.Local).Format("2006-01-02") != "2030-01-01" {
				t.Errorf("expiry = %v, want the local start of 2030-01-01", tok.ExpiresAt)
			}
			return
		}
		t.Error("the token was not listed")
	})

	t.Run("revoking stops it working", func(t *testing.T) {
		out := mustRun(t, srv, "token", "create", "doomed", "--scope", "read")
		doomed := strings.TrimSpace(out)

		if got := mustRun(t, srv, "token", "revoke", "doomed", "--yes"); !strings.Contains(got, "revoked") {
			t.Errorf("output = %q, want the revocation", got)
		}

		other := server{addr: srv.addr, token: doomed, stream: srv.stream}
		code, _, errOut := run(t, other, "zone", "list")
		if code == ExitOK {
			t.Error("the revoked token still works")
		}
		if !strings.Contains(errOut, "credential") {
			t.Errorf("stderr = %q, want it to say the credential was refused", errOut)
		}

		// Still listed, because the history it appears in has to name
		// something.
		if listed := mustRun(t, srv, "token", "list"); !strings.Contains(listed, "revoked") {
			t.Errorf("listing = %q, want the revoked token still in it", listed)
		}
	})

	t.Run("the last administrator cannot revoke itself", func(t *testing.T) {
		code, _, errOut := run(t, srv, "token", "revoke", "bootstrap", "--yes")
		if code != ExitError {
			t.Errorf("exit code = %d, want %d", code, ExitError)
		}
		if errOut == "" {
			t.Error("nothing was said about why")
		}
	})

	t.Run("a token that is not there says so", func(t *testing.T) {
		code, _, errOut := run(t, srv, "token", "revoke", "nothere", "--yes")
		if code != ExitError {
			t.Errorf("exit code = %d, want %d", code, ExitError)
		}
		if !strings.Contains(errOut, "weg token list") {
			t.Errorf("stderr = %q, want it to say where to look", errOut)
		}
	})
}

func TestSince(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tests := []struct {
		name string
		in   *time.Time
		want string
	}{
		{"never used", nil, "never"},
		{"seconds ago", ptr(now.Add(-10 * time.Second)), "just now"},
		{"minutes ago", ptr(now.Add(-5 * time.Minute)), "5m ago"},
		{"hours ago", ptr(now.Add(-3 * time.Hour)), "3h ago"},
		{"days ago", ptr(now.Add(-50 * time.Hour)), "2d ago"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := since(tc.in); got != tc.want {
				t.Errorf("since(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
