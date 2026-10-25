package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api/handoff"
)

// runWith runs weg with options of the test's own: a directory to keep
// sessions in, something typed into it, a browser.
func runWith(t *testing.T, opts *options, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut syncBuffer
	opts.stdout, opts.stderr, opts.printer = &out, &errOut, nil
	root := newRootCommand(opts)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	root.SetContext(t.Context())
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// tokenSession opens a real session with the token, standing in for one a
// provider opened: the server seals both alike, and a test has no provider.
func tokenSession(t *testing.T, srv server) handoff.Session {
	t.Helper()
	body, err := json.Marshal(map[string]string{"token": srv.token})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.addr+basePath+"/auth/session", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open a session: %v", err)
	}
	defer resp.Body.Close()
	var who struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("open a session: %d %v", resp.StatusCode, err)
	}
	s := handoff.Session{Expires: who.ExpiresAt}
	for _, c := range resp.Cookies() {
		switch c.Name {
		case sessionCookie:
			s.Value = c.Value
		case csrfCookie:
			s.CSRF = c.Value
		}
	}
	return s
}

// addAuthentik adds an OpenID Connect provider nobody is ever sent to: the
// tests play the browser and the server's callback both.
func addAuthentik(t *testing.T, srv server) {
	t.Helper()
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatalf("write the secret: %v", err)
	}
	mustRun(t, srv, "idp", "add", "oidc", "Authentik",
		"--issuer", "https://auth.example.com/application/o/weg/", "--client-id", "weg",
		"--client-secret-file", secretFile,
		"--redirect-uri", "https://ns1.example.com/api/v1/auth/oidc/callback", "--map", "dns=admin")
}

// D52: weg login keeps the session the browser brings back, the commands
// after it use it, writes included, and weg logout ends it.
func TestWegLoginThroughOpenIDConnect(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	addAuthentik(t, srv)
	opened := tokenSession(t, srv)
	dir := t.TempDir()

	// The browser, and the server's callback in it: what it does is seal the
	// session to the key in the address and go to the loopback.
	browser := func(ctx context.Context, address string) error {
		u, err := url.Parse(address)
		if err != nil || !strings.HasPrefix(u.Path, basePath+"/auth/oidc/") || !strings.HasSuffix(u.Path, "/start") {
			t.Errorf("the browser was sent to %s, want the server's start", address)
		}
		sealed, err := handoff.Seal(u.Query().Get("key"), &opened)
		if err != nil {
			return err
		}
		back := "http://127.0.0.1:" + u.Query().Get("loopback") + "/?" + url.Values{"session": {sealed}}.Encode()
		go func() {
			req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, back, http.NoBody)
			if rerr != nil {
				return
			}
			if resp, gerr := http.DefaultClient.Do(req); gerr == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}

	out, errOut, err := runWith(t, &options{configDir: dir, openBrowser: browser}, "login", "--server", srv.addr)
	if err != nil {
		t.Fatalf("login: %v; stderr: %s", err, errOut)
	}
	if !strings.Contains(out, "signed in at "+srv.addr+" as bootstrap, with admin") {
		t.Errorf("stdout = %q, want who signed in and with what", out)
	}
	info, err := os.Stat(filepath.Join(dir, sessionsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the session file: %v %v, want it readable by its owner alone", info, err)
	}

	// No token from here on.
	if _, werrOut, werr := runWith(t, &options{configDir: dir}, "zone", "create", "example.com", "--server", srv.addr); werr != nil {
		t.Fatalf("a write with the session: %v; stderr: %s", werr, werrOut)
	}

	out, _, err = runWith(t, &options{configDir: dir}, "logout", "--server", srv.addr)
	if err != nil || !strings.Contains(out, "signed bootstrap out") {
		t.Errorf("logout = %q %v, want it said whom it signed out", out, err)
	}
	_, _, err = runWith(t, &options{configDir: dir}, "zone", "list", "--server", srv.addr)
	if err == nil || !strings.Contains(err.Error(), "weg login") {
		t.Errorf("after logout: %v, want it said how to sign in", err)
	}

	// And the server was told: the session it handed over no longer works.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.addr+basePath+"/auth/session", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	(&heldSession{Value: opened.Value, CSRF: opened.CSRF}).attach(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the session answered %d after logout, want 401", resp.StatusCode)
	}
}

func TestWegLoginThroughAnOpenIDConnectRefusal(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	addAuthentik(t, srv)

	browser := func(ctx context.Context, address string) error {
		u, err := url.Parse(address)
		if err != nil {
			return err
		}
		back := "http://127.0.0.1:" + u.Query().Get("loopback") + "/?" +
			url.Values{"error": {"Authentik signed mallory in, and none of their groups is given a scope here"}}.Encode()
		go func() {
			req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, back, http.NoBody)
			if rerr != nil {
				return
			}
			if resp, gerr := http.DefaultClient.Do(req); gerr == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	dir := t.TempDir()
	_, _, err := runWith(t, &options{configDir: dir, openBrowser: browser}, "login", "authentik", "--server", srv.addr)
	if err == nil || !strings.Contains(err.Error(), "none of their groups") {
		t.Errorf("login: %v, want the reason the provider gave", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, sessionsFile)); serr == nil {
		t.Error("a refused sign-in left a session file")
	}
}

// directoryInFront is the server with a directory in front of its sign-in:
// alice with the password "correct" is let in, as a session the token opens.
// Everything else reaches the server unchanged.
func directoryInFront(t *testing.T, srv server) string {
	t.Helper()
	target, err := url.Parse(srv.addr)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, basePath+"/auth/ldap/") {
			proxy.ServeHTTP(w, r)
			return
		}
		var in struct{ Username, Password string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Username != "alice" || in.Password != "correct" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			if _, werr := io.WriteString(w, `{"title":"Not authenticated","status":401,"detail":"the name and password do not sign anybody in"}`); werr != nil {
				t.Error(werr)
			}
			return
		}
		body, merr := json.Marshal(map[string]string{"token": srv.token})
		if merr != nil {
			t.Error(merr)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = basePath + "/auth/session"
		r2.Body = io.NopCloser(bytes.NewReader(body))
		r2.ContentLength = int64(len(body))
		proxy.ServeHTTP(w, r2)
	}))
	t.Cleanup(front.Close)
	return front.URL
}

func TestWegLoginToADirectory(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	mustRun(t, srv, "idp", "add", "ldap", "FreeIPA",
		"--url", "ldaps://ipa.example.com", "--user-base", "cn=users,cn=accounts,dc=example,dc=com",
		"--user-filter", "(uid={username})", "--map", "admins=admin")
	front := directoryInFront(t, srv)

	t.Run("the name and password are asked for", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		opts := &options{configDir: dir, stdin: strings.NewReader("alice\ncorrect\n")}
		out, errOut, err := runWith(t, opts, "login", "--server", front)
		if err != nil {
			t.Fatalf("login: %v; stderr: %s", err, errOut)
		}
		if !strings.Contains(errOut, "Username at FreeIPA") || !strings.Contains(errOut, "Password") {
			t.Errorf("stderr = %q, want both asked for", errOut)
		}
		if strings.Contains(out+errOut, "correct") {
			t.Error("the password was printed")
		}
		if _, errOut, err := runWith(t, &options{configDir: dir}, "zone", "list", "--server", front); err != nil {
			t.Errorf("a read with the session: %v; stderr: %s", err, errOut)
		}
	})

	t.Run("a wrong password is the directory's answer", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		opts := &options{configDir: dir, stdin: strings.NewReader("alice\nwrong\n")}
		_, _, err := runWith(t, opts, "login", "FreeIPA", "--server", front)
		if err == nil || !strings.Contains(err.Error(), "do not sign anybody in") {
			t.Errorf("login: %v, want the server's refusal", err)
		}
	})

	t.Run("a provider nobody named is suggested", func(t *testing.T) {
		t.Parallel()
		_, _, err := runWith(t, &options{configDir: t.TempDir()}, "login", "FreeIP", "--server", front)
		if err == nil || !strings.Contains(err.Error(), "did you mean FreeIPA") {
			t.Errorf("login: %v, want a suggestion", err)
		}
	})
}

func TestAPasswordStaysOffAPlainNetwork(t *testing.T) {
	t.Parallel()
	for server, want := range map[string]bool{
		"https://ns1.example.com:8053": true,
		"http://127.0.0.1:8053":        true,
		"http://[::1]:8053":            true,
		"http://localhost:8053":        true,
		"http://ns1.example.com:8053":  false,
		"http://192.0.2.1:8053":        false,
	} {
		if got := checkPasswordTransport(server) == nil; got != want {
			t.Errorf("%s: allowed = %v, want %v", server, got, want)
		}
	}
}

func TestATokenBeatsAHeldSession(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, sessionsFile)
	held := heldSessions{Servers: map[string]heldSession{
		srv.addr: {Value: "not a session", CSRF: "x", Name: "nobody", Expires: time.Now().Add(time.Hour)},
	}}
	if err := writeSessions(path, held, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, errOut, err := runWith(t, &options{configDir: dir}, "zone", "list", "--server", srv.addr, "--token", srv.token); err != nil {
		t.Errorf("with a token: %v; stderr: %s", err, errOut)
	}
	if _, _, err := runWith(t, &options{configDir: dir}, "zone", "list", "--server", srv.addr); err == nil {
		t.Error("the broken session was accepted")
	}
}
