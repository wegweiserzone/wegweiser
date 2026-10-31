package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/api/handoff"
)

// testIDP is an OpenID Connect provider small enough to read: discovery, its
// keys, and a token endpoint that checks PKCE and signs an ID token for
// whoever the test says signed in. The authorization endpoint is never
// visited; the test plays the browser and the person both.
type testIDP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu sync.Mutex
	// codes are the codes handed out, each to the challenge and nonce of the
	// sign-in it belongs to.
	codes map[string]issued
	// claims are what the next ID token says beyond who issued it to whom.
	claims map[string]any
	// userinfo, when set, is what the user info endpoint answers.
	userinfo map[string]any
}

type issued struct{ challenge, nonce string }

func newTestIDP(t *testing.T) *testIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the provider's key: %v", err)
	}
	idp := &testIDP{key: key, codes: map[string]issued{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                idp.srv.URL,
			"authorization_endpoint":                idp.srv.URL + "/authorize",
			"token_endpoint":                        idp.srv.URL + "/token",
			"jwks_uri":                              idp.srv.URL + "/keys",
			"userinfo_endpoint":                     idp.srv.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		idp.mu.Lock()
		got, ok := idp.codes[r.Form.Get("code")]
		delete(idp.codes, r.Form.Get("code"))
		claims := idp.claims
		idp.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != got.challenge {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid_grant"})
			return
		}
		writeJSON(w, map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 300,
			"id_token": idp.sign(t, got.nonce, claims),
		})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		info := idp.userinfo
		idp.mu.Unlock()
		if info == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, info)
	})
	idp.srv = httptest.NewTLSServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (idp *testIDP) sign(t *testing.T, nonce string, extra map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: idp.key},
		(&jose.SignerOptions{}).WithHeader("kid", "k"))
	if err != nil {
		t.Fatalf("build the signer: %v", err)
	}
	now := time.Now()
	claims := map[string]any{
		"iss": idp.srv.URL, "sub": "u-1", "aud": "weg", "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("sign the ID token: %v", err)
	}
	return raw
}

// authorize is the provider's half of a sign-in the browser was sent to it
// for: it takes the person as signed in, and hands back the URL the browser
// is sent on to.
func (idp *testIDP) authorize(t *testing.T, location string, claims map[string]any) *url.URL {
	t.Helper()
	to, err := url.Parse(location)
	if err != nil {
		t.Fatalf("the start sent the browser to %q: %v", location, err)
	}
	q := to.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" {
		t.Fatalf("the start asked for %s, want PKCE with S256 and a nonce", to)
	}
	code := "code-" + q.Get("state")[:8]
	idp.mu.Lock()
	idp.codes[code] = issued{challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}
	idp.claims = claims
	idp.mu.Unlock()

	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		t.Fatalf("the redirect URI %q: %v", q.Get("redirect_uri"), err)
	}
	back.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	return back
}

// browser is a client that keeps cookies and does not follow redirects, so
// that each step of a sign-in can be looked at.
type browser struct {
	t      *testing.T
	client *http.Client
	jar    map[string]*http.Cookie
}

func newBrowser(t *testing.T) *browser {
	return &browser{
		t: t, jar: map[string]*http.Cookie{},
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

func (b *browser) get(target string) *http.Response {
	b.t.Helper()
	req, err := http.NewRequestWithContext(b.t.Context(), http.MethodGet, target, http.NoBody)
	if err != nil {
		b.t.Fatalf("build the request: %v", err)
	}
	for _, c := range b.jar {
		req.AddCookie(c)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("GET %s: %v", target, err)
	}
	b.t.Cleanup(func() { resp.Body.Close() })
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(b.jar, c.Name)
		} else {
			b.jar[c.Name] = c
		}
	}
	return resp
}

// signInThroughOIDC adds a provider for idp and has a person sign in through
// it with the claims given, returning where the browser ended up.
func signInThroughOIDC(t *testing.T, h *harness, idp *testIDP, claims map[string]any) (*browser, *url.URL, gen.IdentityProvider) {
	t.Helper()
	p := addOIDCProvider(t, h, idp)
	b := newBrowser(t)
	landed := b.signIn(idp, h.http.URL+basePath+oidcPath+"/"+p.Id+"/start", claims)
	return b, landed, p
}

// addOIDCProvider adds idp as a provider, with a redirect URI on the harness.
func addOIDCProvider(t *testing.T, h *harness, idp *testIDP) gen.IdentityProvider {
	t.Helper()
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.srv.Certificate().Raw}))
	var p gen.IdentityProvider
	h.decode(h.do(http.MethodPost, "/identity-providers", gen.IdentityProviderInput{
		Name: "Test IDP", Kind: gen.IdentityProviderKindOidc, CaCertificate: &certPEM,
		Mappings: &[]gen.GroupMapping{{Group: "dns-ops", Scope: gen.ScopeWrite}, {Group: "dns-readers", Scope: gen.ScopeRead}},
		Oidc: &gen.OIDCSettings{
			Issuer: idp.srv.URL, ClientId: "weg", ClientSecret: ptr("s3cret"),
			RedirectUris: []string{"https://elsewhere.example/api/v1/auth/oidc/callback", h.http.URL + basePath + oidcCallbackPath},
		},
	}), http.StatusCreated, &p)
	return p
}

// signIn goes from the start to the end of a sign-in, with the person the
// claims describe, and returns where the callback sent the browser.
func (b *browser) signIn(idp *testIDP, startURL string, claims map[string]any) *url.URL {
	t := b.t
	t.Helper()
	start := b.get(startURL)
	if start.StatusCode != http.StatusFound || b.jar[flowCookie] == nil {
		t.Fatalf("the start answered %d with cookies %v, want a redirect carrying the flow", start.StatusCode, b.jar)
	}
	back := idp.authorize(t, start.Header.Get("Location"), claims)
	done := b.get(back.String())
	if done.StatusCode != http.StatusFound {
		t.Fatalf("the callback answered %d, want a redirect", done.StatusCode)
	}
	landed, err := url.Parse(done.Header.Get("Location"))
	if err != nil {
		t.Fatalf("the callback sent the browser to %q: %v", done.Header.Get("Location"), err)
	}
	if b.jar[flowCookie] != nil {
		t.Error("the flow cookie outlived the callback")
	}
	return landed
}

func (b *browser) session(h *harness) (int, gen.Session) {
	b.t.Helper()
	var who gen.Session
	resp := h.do(http.MethodGet, sessionPath, nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		for _, c := range b.jar {
			r.AddCookie(c)
		}
	})
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
			b.t.Fatalf("decode the session: %v", err)
		}
	}
	return resp.StatusCode, who
}

// D51: a person signs in through an OpenID Connect provider and holds the
// strongest scope their groups map to, for as long as the provider is there.
func TestAPersonSignsInThroughOpenIDConnect(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idp := newTestIDP(t)

	b, landed, p := signInThroughOIDC(t, h, idp, map[string]any{
		"preferred_username": "alice", "groups": []string{"dns-readers", "dns-ops", "staff"},
	})
	if landed.Path != "/" || landed.Query().Get(signInError) != "" {
		t.Fatalf("the browser landed at %s, want the interface", landed)
	}
	status, who := b.session(h)
	if status != http.StatusOK || who.Name != "alice (Test IDP)" ||
		len(who.Scopes) != 1 || who.Scopes[0] != gen.ScopeWrite || who.ExpiresAt == nil {
		t.Errorf("session = %d %+v, want alice through the provider, with write", status, who)
	}

	// Switching the provider off ends what was opened through it.
	in := gen.IdentityProviderInput{Name: p.Name, Kind: p.Kind, Enabled: ptr(false), Mappings: &p.Mappings, CaCertificate: p.CaCertificate, Oidc: p.Oidc}
	h.decode(h.do(http.MethodPut, "/identity-providers/"+p.Id, in), http.StatusOK, nil)
	if status, _ := b.session(h); status != http.StatusUnauthorized {
		t.Errorf("status = %d once the provider is off, want 401", status)
	}
}

func TestASignInThroughOpenIDConnectIsRefused(t *testing.T) {
	t.Parallel()

	t.Run("a person in no mapped group", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		b, landed, _ := signInThroughOIDC(t, h, newTestIDP(t), map[string]any{
			"preferred_username": "mallory", "groups": []string{"staff"},
		})
		if msg := landed.Query().Get(signInError); !strings.Contains(msg, "none of their groups") {
			t.Errorf("landed at %s, want the sign-in page saying why", landed)
		}
		if status, _ := b.session(h); status != http.StatusUnauthorized {
			t.Errorf("status = %d, want no session", status)
		}
	})

	t.Run("a callback this browser did not start", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		got := newBrowser(t).get(h.http.URL + basePath + oidcCallbackPath + "?code=x&state=y")
		to, err := url.Parse(got.Header.Get("Location"))
		if err != nil || got.StatusCode != http.StatusFound || to.Query().Get(signInError) == "" {
			t.Errorf("answered %d to %s, want the sign-in page saying why", got.StatusCode, got.Header.Get("Location"))
		}
	})

	t.Run("a provider that does not exist", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		got := newBrowser(t).get(h.http.URL + basePath + oidcPath + "/01ARZ3NDEKTSV4RRFFQ69G5FAV/start")
		if to := got.Header.Get("Location"); got.StatusCode != http.StatusFound || !strings.Contains(to, signInError) {
			t.Errorf("answered %d to %s, want the sign-in page saying why", got.StatusCode, to)
		}
	})
}

// D52: a sign-in weg login started ends at its loopback port, with the
// session sealed to its key, and the browser holds nothing.
func TestWegLoginSignsInThroughOpenIDConnect(t *testing.T) {
	t.Parallel()

	start := func(h *harness, p gen.IdentityProvider, key string) string {
		return h.http.URL + basePath + oidcPath + "/" + p.Id + "/start?" +
			url.Values{"loopback": {"45678"}, "key": {key}}.Encode()
	}
	atLoopback := func(t *testing.T, landed *url.URL) {
		t.Helper()
		if landed.Scheme != "http" || landed.Host != "127.0.0.1:45678" || landed.Path != "/" {
			t.Fatalf("the browser was sent to %s, want weg login's loopback", landed)
		}
	}

	t.Run("the session reaches the CLI", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		idp := newTestIDP(t)
		p := addOIDCProvider(t, h, idp)
		key, err := handoff.NewKey()
		if err != nil {
			t.Fatal(err)
		}

		b := newBrowser(t)
		landed := b.signIn(idp, start(h, p, key.Public()), map[string]any{
			"preferred_username": "alice", "groups": []string{"dns-ops"},
		})
		atLoopback(t, landed)
		if b.jar[sessionCookie] != nil || b.jar[csrfCookie] != nil {
			t.Error("the browser was given the session the CLI asked for")
		}
		got, err := key.Open(landed.Query().Get("session"))
		if err != nil {
			t.Fatalf("the session does not open under the CLI's key: %v", err)
		}

		// It is the session a browser would have held, CSRF value and all.
		cli := &browser{t: t, jar: map[string]*http.Cookie{sessionCookie: {Name: sessionCookie, Value: got.Value}}}
		if status, who := cli.session(h); status != http.StatusOK || who.Name != "alice (Test IDP)" ||
			who.ExpiresAt == nil || !who.ExpiresAt.Equal(got.Expires) {
			t.Errorf("session = %d %+v, want alice until %s", status, who, got.Expires)
		}
		resp := h.do(http.MethodDelete, sessionPath, nil, func(r *http.Request) {
			r.Header.Del("Authorization")
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: got.Value})
			r.Header.Set(csrfHeader, got.CSRF)
		})
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("signing out with the CSRF value answered %d, want 204", resp.StatusCode)
		}
	})

	t.Run("a refusal reaches the CLI", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		idp := newTestIDP(t)
		key, err := handoff.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		landed := newBrowser(t).signIn(idp, start(h, addOIDCProvider(t, h, idp), key.Public()), map[string]any{
			"preferred_username": "mallory", "groups": []string{"staff"},
		})
		atLoopback(t, landed)
		if msg := landed.Query().Get("error"); !strings.Contains(msg, "none of their groups") || landed.Query().Has("session") {
			t.Errorf("landed at %s, want the reason and no session", landed)
		}
	})

	t.Run("a key that is not one is refused at the start", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		p := addOIDCProvider(t, h, newTestIDP(t))
		got := newBrowser(t).get(start(h, p, "AAAA"))
		landed, err := url.Parse(got.Header.Get("Location"))
		if err != nil || got.StatusCode != http.StatusFound {
			t.Fatalf("answered %d to %s, want a redirect", got.StatusCode, got.Header.Get("Location"))
		}
		atLoopback(t, landed)
		if landed.Query().Get("error") == "" {
			t.Errorf("landed at %s, want the reason", landed)
		}
	})
}

func TestTheSignInPageIsToldWhichProvidersThereAre(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.decode(h.do(http.MethodPost, "/identity-providers", authentikInput()), http.StatusCreated, nil)
	off := freeIPAInput()
	off.Enabled = ptr(false)
	h.decode(h.do(http.MethodPost, "/identity-providers", off), http.StatusCreated, nil)

	var listed []gen.SignInProvider
	h.decode(h.do(http.MethodGet, signInProvidersPath, nil, func(r *http.Request) { r.Header.Del("Authorization") }),
		http.StatusOK, &listed)
	if len(listed) != 1 || listed[0].Name != "Authentik" || listed[0].Kind != gen.IdentityProviderKindOidc {
		t.Errorf("listed %+v, want the one switched on, asked without a credential", listed)
	}
}

// An administrator tries a sign-in through a provider that is switched off,
// and sees what it gives without anybody being signed in by it.
func TestAnAdministratorTriesASignInThroughOpenIDConnect(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idp := newTestIDP(t)
	p := addOIDCProvider(t, h, idp)
	in := gen.IdentityProviderInput{Name: p.Name, Kind: p.Kind, Enabled: ptr(false), Mappings: &p.Mappings, CaCertificate: p.CaCertificate, Oidc: p.Oidc}
	h.decode(h.do(http.MethodPut, "/identity-providers/"+p.Id, in), http.StatusOK, nil)

	ticket := func(t *testing.T, body gen.TryOIDCSignInJSONRequestBody) string {
		t.Helper()
		var got gen.TryOIDCSignIn200JSONResponse
		h.decode(h.do(http.MethodPost, "/identity-providers/"+p.Id+"/try-sign-in", body), http.StatusOK, &got)
		return h.http.URL + got.Url
	}
	check := func(id, sealed string) *http.Response {
		return h.do(http.MethodPost, "/identity-providers/check", gen.IdentityProviderCheckRequest{Id: &id, SignIn: &sealed})
	}
	// The browser that redeems the ticket brings no credential of its own.
	try := func(t *testing.T, start string, claims map[string]any) (*browser, *url.URL) {
		t.Helper()
		b := newBrowser(t)
		landed := b.signIn(idp, start, claims)
		if b.jar[sessionCookie] != nil {
			t.Errorf("a tried sign-in left a session: %v", b.jar)
		}
		return b, landed
	}
	tried := func(t *testing.T, claims map[string]any) (gen.IdentityProviderCheck, string) {
		t.Helper()
		_, landed := try(t, ticket(t, gen.TryOIDCSignInJSONRequestBody{}), claims)
		if landed.Path != "/providers/"+p.Id || landed.RawQuery != "" {
			t.Fatalf("the callback sent the browser to %s, want the provider's page", landed)
		}
		frag, err := url.ParseQuery(landed.Fragment)
		if err != nil {
			t.Fatalf("the fragment %q: %v", landed.Fragment, err)
		}
		var got gen.IdentityProviderCheck
		h.decode(check(p.Id, frag.Get("tried")), http.StatusOK, &got)
		return got, frag.Get("tried")
	}

	got, sealed := tried(t, map[string]any{"preferred_username": "alice", "groups": []string{"dns-ops", "staff"}})
	if who := got.Person; !got.Ok || who == nil || who.Name != "alice" || who.Subject != "u-1" ||
		len(who.Groups) != 2 || who.Scope == nil || *who.Scope != gen.ScopeWrite {
		t.Errorf("tried = %+v, want alice, her groups and write", got)
	}

	// Somebody it would refuse is what the sign-in came to, not a failure to try.
	if got, _ := tried(t, map[string]any{"preferred_username": "mallory", "groups": []string{"staff"}}); got.Ok ||
		got.Person == nil || got.Person.Scope != nil {
		t.Errorf("tried = %+v, want mallory, refused", got)
	}
	if got, _ := tried(t, map[string]any{"preferred_username": "bob"}); got.Ok ||
		!slices.ContainsFunc(got.Steps, func(s gen.CheckStep) bool {
			return s.Step == "The groups claim" && s.Status == gen.CheckStepStatusFailed
		}) {
		t.Errorf("tried = %+v, want the missing groups claim said", got)
	}

	// What came back is good for the provider it was tried through, and is
	// nothing anybody can write.
	h.decode(check("01ARZ3NDEKTSV4RRFFQ69G5FAV", sealed), http.StatusBadRequest, nil)
	h.decode(check(p.Id, sealed[:len(sealed)-2]), http.StatusBadRequest, nil)

	// Started by weg idp check --sign-in, it ends at the CLI's loopback.
	key, err := handoff.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	_, landed := try(t, ticket(t, gen.TryOIDCSignInJSONRequestBody{Loopback: ptr(45678), Key: ptr(key.Public())}),
		map[string]any{"preferred_username": "alice", "groups": []string{"dns-ops"}})
	if landed.Host != "127.0.0.1:45678" || landed.Query().Get("tried") == "" || landed.Query().Has("session") {
		t.Errorf("the CLI's sign-in landed at %s, want its loopback with what it came to", landed)
	}

	// Without a ticket it is a sign-in, which a provider switched off refuses;
	// a ticket is good for its provider alone.
	if got := newBrowser(t).get(h.http.URL + basePath + oidcPath + "/" + p.Id + "/start"); !strings.Contains(got.Header.Get("Location"), signInError) {
		t.Errorf("a start without a ticket went to %s, want it refused", got.Header.Get("Location"))
	}
	var other gen.IdentityProvider
	h.decode(h.do(http.MethodPost, "/identity-providers", authentikInput()), http.StatusCreated, &other)
	forged := strings.Replace(ticket(t, gen.TryOIDCSignInJSONRequestBody{}), p.Id, other.Id, 1)
	if got := newBrowser(t).get(forged); !strings.Contains(got.Header.Get("Location"), signInError) {
		t.Errorf("a ticket for another provider went to %s, want it refused", got.Header.Get("Location"))
	}
}

// A provider that gives the groups only to whoever asks for the user info is
// asked, and what it answers counts only for the person the ID token names.
func TestTheUserInfoIsOnlyTakenForTheSamePerson(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		subject string
		want    int
	}{
		{"the same person", "u-1", http.StatusOK},
		{"somebody else", "u-2", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			idp := newTestIDP(t)
			idp.userinfo = map[string]any{"sub": tc.subject, "groups": []string{"dns-ops"}}
			b, landed, _ := signInThroughOIDC(t, h, idp, map[string]any{"preferred_username": "alice"})
			if status, _ := b.session(h); status != tc.want {
				t.Errorf("landed at %s with a session answering %d, want %d", landed, status, tc.want)
			}
		})
	}
}

// A sign-in this server started and the provider then refused is not a guess:
// PKCE binds it to its flow. However often it fails, the next one goes through.
func TestAFailingProviderDoesNotLockAnAddressOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idp := newTestIDP(t)
	p := addOIDCProvider(t, h, idp)
	start := h.http.URL + basePath + oidcPath + "/" + p.Id + "/start"
	claims := map[string]any{"preferred_username": "alice", "groups": []string{"dns-ops"}}

	for range authBurst + 1 {
		b := newBrowser(t)
		back := idp.authorize(t, b.get(start).Header.Get("Location"), claims)
		q := back.Query()
		q.Set("code", "not-one-it-issued")
		back.RawQuery = q.Encode()
		landed, err := url.Parse(b.get(back.String()).Header.Get("Location"))
		if err != nil || landed.Query().Get(signInError) == "" {
			t.Fatalf("a refused code landed at %v, want the sign-in page saying why", landed)
		}
	}
	b := newBrowser(t)
	if landed := b.signIn(idp, start, claims); landed.Query().Get(signInError) != "" {
		t.Errorf("the sign-in after them landed at %s, want it through", landed)
	}
}
