package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityProviders(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatalf("write the secret: %v", err)
	}

	t.Run("none says how to add one", func(t *testing.T) {
		if out := mustRun(t, srv, "idp", "list"); !strings.Contains(out, "weg idp add") {
			t.Errorf("output = %q, want it said how to add one", out)
		}
	})

	t.Run("an OpenID Connect provider is added, and says nobody is mapped", func(t *testing.T) {
		code, out, errOut := run(t, srv, "idp", "add", "oidc", "Authentik",
			"--issuer", "https://auth.example.com/application/o/weg/",
			"--client-id", "weg", "--client-secret-file", secretFile,
			"--redirect-uri", "https://ns1.example.com/api/v1/auth/oidc/callback")
		if code != ExitOK {
			t.Fatalf("exit code = %d; stderr: %s", code, errOut)
		}
		if !strings.Contains(out, "added Authentik") || !strings.Contains(errOut, "--map") {
			t.Errorf("stdout = %q, stderr = %q; want it added and told it lets nobody in", out, errOut)
		}
	})

	t.Run("a directory is added with its mappings", func(t *testing.T) {
		mustRun(t, srv, "idp", "add", "ldap", "FreeIPA",
			"--url", "ldaps://ipa.example.com",
			"--user-base", "cn=users,cn=accounts,dc=example,dc=com",
			"--user-filter", "(uid={username})",
			"--map", "admins=admin", "--map", "dns=read")
	})

	t.Run("the listing names both and shows no secret", func(t *testing.T) {
		out := mustRun(t, srv, "idp", "list")
		if !strings.Contains(out, "Authentik") || !strings.Contains(out, "admins=admin, dns=read") {
			t.Errorf("output = %q, want both providers and the mappings", out)
		}
		if shown := mustRun(t, srv, "idp", "show", "authentik"); strings.Contains(shown, "s3cret") ||
			!strings.Contains(shown, "preferred_username") {
			t.Errorf("show = %q, want the settings with their defaults and no secret", shown)
		}
	})

	t.Run("set changes what it is given and keeps the rest", func(t *testing.T) {
		mustRun(t, srv, "idp", "set", "Authentik", "--map", "dns-admins=admin", "--disabled")

		var listed []idpListed
		if err := json.Unmarshal([]byte(mustRun(t, srv, "idp", "list", "--output", "json")), &listed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(listed) != 2 || listed[0].Status != "disabled" || len(listed[0].Mappings) != 1 {
			t.Errorf("listed %+v, want Authentik switched off with one mapping", listed)
		}
		if shown := mustRun(t, srv, "idp", "show", "Authentik"); !strings.Contains(shown, "https://auth.example.com/") {
			t.Errorf("show = %q, want the issuer kept", shown)
		}
	})

	t.Run("a mapping is written GROUP=SCOPE", func(t *testing.T) {
		for _, m := range []string{"admins", "admins=root", "=admin"} {
			if code, _, _ := run(t, srv, "idp", "set", "FreeIPA", "--map", m); code != ExitUsage {
				t.Errorf("--map %q: exit code = %d, want %d", m, code, ExitUsage)
			}
		}
	})

	t.Run("a name that is not one suggests the one meant", func(t *testing.T) {
		if code, _, errOut := run(t, srv, "idp", "show", "FreeIPB"); code == ExitOK || !strings.Contains(errOut, "FreeIPA") {
			t.Errorf("exit code = %d, stderr = %q; want FreeIPA suggested", code, errOut)
		}
	})

	t.Run("removing one leaves the other", func(t *testing.T) {
		mustRun(t, srv, "idp", "remove", "Authentik", "--yes")
		if out := mustRun(t, srv, "idp", "list"); strings.Contains(out, "Authentik") || !strings.Contains(out, "FreeIPA") {
			t.Errorf("output = %q, want FreeIPA alone", out)
		}
	})
}
