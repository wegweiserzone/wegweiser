package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A provider nothing answers for: the check says where it stopped and why, and
// the command succeeds, because a failed step is its answer.
func TestIdentityProviderCheck(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	// Loopback with nothing listening, so that the check fails at once.
	mustRun(t, srv, "idp", "add", "ldap", "Nowhere",
		"--url", "ldaps://127.0.0.1:9", "--user-base", "dc=example,dc=com",
		"--user-filter", "(uid={username})", "--map", "admins=admin")

	t.Run("text says what failed", func(t *testing.T) {
		out := mustRun(t, srv, "idp", "check", "nowhere")
		if !strings.Contains(out, "Nowhere does not work as it stands") ||
			!strings.Contains(out, "failed") || !strings.Contains(out, "Connect over TLS") {
			t.Errorf("output = %q, want the verdict and the step that failed", out)
		}
	})

	t.Run("json carries ok for a script", func(t *testing.T) {
		var got idpChecked
		if err := json.Unmarshal([]byte(mustRun(t, srv, "idp", "check", "Nowhere", "--output", "json")), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.OK || len(got.Steps) != 1 || got.Steps[0].Status != "failed" {
			t.Errorf("check = %+v, want one failed step", got)
		}
	})

	t.Run("a username for a provider that cannot look one up is refused", func(t *testing.T) {
		secretFile := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(secretFile, []byte("s3cret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mustRun(t, srv, "idp", "add", "oidc", "Elsewhere",
			"--issuer", "https://127.0.0.1:9/", "--client-id", "weg", "--client-secret-file", secretFile,
			"--redirect-uri", "https://ns1.example.com/api/v1/auth/oidc/callback")
		code, _, errOut := run(t, srv, "idp", "check", "Elsewhere", "--username", "alice")
		if code == ExitOK || !strings.Contains(errOut, "only a directory") {
			t.Errorf("exit %d, stderr %q; want the server's refusal", code, errOut)
		}
	})
}
