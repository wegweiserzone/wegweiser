package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/api/handoff"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// The routes a person signs in through, below the API's base path.
const (
	signInProvidersPath = "/auth/providers"
	oidcPath            = "/auth/oidc"
)

const (
	// flowCookie carries what the callback needs to check a provider's
	// answer, from the start of a sign-in to its end.
	flowCookie = "weg_oidc"

	// flowLifetime is how long a person has to sign in at the provider.
	flowLifetime = 10 * time.Minute

	// signInError is the query parameter the sign-in page reads a failure
	// from.
	signInError = "signin_error"

	// discoveryTimeout bounds every call to a provider: discovery, the code
	// exchange and the user info. A provider that does not answer is one
	// nobody can sign in through, and saying so beats a browser left hanging.
	discoveryTimeout = 10 * time.Second
)

// oidcFlow is a sign-in under way, sealed into its cookie under the session
// key, so that whichever member the callback reaches can check it and none
// has to have remembered anything.
type oidcFlow struct {
	Provider    string    `json:"idp"`
	State       string    `json:"state"`
	Nonce       string    `json:"nonce"`
	Verifier    string    `json:"verifier"`
	RedirectURI string    `json:"redirect_uri"`
	Expires     time.Time `json:"exp"`

	// Loopback and Key are where `weg login` waits for the session and the
	// key it is sealed to, for a sign-in the CLI started (D52).
	Loopback int    `json:"loopback,omitempty"`
	Key      string `json:"key,omitempty"`

	// Test is a sign-in an administrator started to see what it gives,
	// which opens no session and ends on the provider's page.
	Test bool `json:"test,omitempty"`
}

// ListSignInProviders names the providers that are switched on.
func (s *Server) ListSignInProviders(
	ctx context.Context, _ gen.ListSignInProvidersRequestObject,
) (gen.ListSignInProvidersResponseObject, error) {
	list, err := s.identityProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gen.SignInProvider, 0, len(list))
	for i := range list {
		if list[i].Enabled {
			out = append(out, gen.SignInProvider{
				Id: list[i].ID, Name: list[i].Name, Kind: gen.IdentityProviderKind(list[i].Kind),
			})
		}
	}
	return gen.ListSignInProviders200JSONResponse(out), nil
}

// StartOIDCSignIn sends the browser to the provider.
func (s *Server) StartOIDCSignIn(
	ctx context.Context, req gen.StartOIDCSignInRequestObject,
) (gen.StartOIDCSignInResponseObject, error) {
	secure := secureOf(ctx)
	back, rerr := returnFor(req.Params.Loopback, req.Params.Key)
	fail := func(err error) (gen.StartOIDCSignInResponseObject, error) {
		return &signInRedirect{to: back.failed(err), secure: secure}, nil
	}
	if rerr != nil {
		return fail(rerr)
	}
	if err := checkTransport(secure, sourceOf(ctx)); err != nil {
		return fail(err)
	}

	p, keys, err := s.oidcProvider(ctx, req.ProviderId, false)
	if err != nil {
		return fail(err)
	}
	r, err := s.startOIDC(ctx, p, keys, oidcFlow{Loopback: back.port, Key: back.key})
	if err != nil {
		return fail(err)
	}
	return r, nil
}

// TryOIDCSignIn sends an administrator's browser through a sign-in that opens
// no session, so that what a sign-in gives can be seen before anybody relies
// on it, through a provider that may not be switched on yet.
func (s *Server) TryOIDCSignIn(
	ctx context.Context, req gen.TryOIDCSignInRequestObject,
) (gen.TryOIDCSignInResponseObject, error) {
	if err := requireAdmin(ctx, "trying a sign-in through an identity provider"); err != nil {
		return nil, err
	}
	secure := secureOf(ctx)
	p, keys, err := s.oidcProvider(ctx, req.ProviderId, true)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrNotFound, err)
	}
	r, err := s.startOIDC(ctx, p, keys, oidcFlow{Test: true})
	if err != nil {
		var c checker
		c.fail("Start a sign-in", "%s", failureDetail(err))
		return s.tried(keys, p.ID, &c, secure)
	}
	return r, nil
}

// startOIDC sends the browser to p, with flow, which says where the sign-in
// ends, sealed into its cookie with what the callback checks the answer by.
func (s *Server) startOIDC(
	ctx context.Context, p *apply.IdentityProvider, keys apply.SessionKeys, flow oidcFlow,
) (*signInRedirect, error) {
	redirect, err := redirectFor(p.OIDC.RedirectURIs, hostOf(ctx))
	if err != nil {
		return nil, err
	}
	client, err := s.oidc.client(ctx, p)
	if err != nil {
		return nil, err
	}
	if flow.State, err = randomSecret(); err != nil {
		return nil, err
	}
	if flow.Nonce, err = randomSecret(); err != nil {
		return nil, err
	}
	flow.Provider, flow.Verifier, flow.RedirectURI = p.ID, oauth2.GenerateVerifier(), redirect
	flow.Expires = s.now().Add(flowLifetime).UTC()
	sealed, err := seal(keys.Current, flowCookie, &flow)
	if err != nil {
		return nil, err
	}
	to := client.config(p, redirect).AuthCodeURL(flow.State,
		oidc.Nonce(flow.Nonce), oauth2.S256ChallengeOption(flow.Verifier))
	return &signInRedirect{to: to, flow: sealed, flowExpires: flow.Expires, secure: secureOf(ctx)}, nil
}

// FinishOIDCSignIn takes the browser back from the provider.
func (s *Server) FinishOIDCSignIn(
	ctx context.Context, req gen.FinishOIDCSignInRequestObject,
) (gen.FinishOIDCSignInResponseObject, error) {
	from, secure := sourceOf(ctx), secureOf(ctx)
	keys, kerr := s.sessionKeys(ctx)
	// Opened before anything can fail, because it says where a failure is
	// reported: on the sign-in page, or to the `weg login` waiting for it.
	// One that has run out still says so.
	var (
		flow *oidcFlow
		back signInReturn
	)
	if kerr == nil {
		if flow = openFlow(keys, flowOf(ctx)); flow != nil {
			back = signInReturn{port: flow.Loopback, key: flow.Key}
		}
	}
	fail := func(err error) (gen.FinishOIDCSignInResponseObject, error) {
		s.limiter.spend(from, s.now())
		if flow != nil && flow.Test {
			var c checker
			c.fail(signInStep, "%s", failureDetail(err))
			return s.tried(keys, flow.Provider, &c, secure)
		}
		return &signInRedirect{to: back.failed(err), clearFlow: true, secure: secure}, nil
	}
	if !s.limiter.allow(from, s.now()) {
		return &signInRedirect{to: back.failed(tooManyAttempts()), clearFlow: true, secure: secure}, nil
	}
	if kerr != nil {
		return fail(kerr)
	}
	if err := checkTransport(secure, from); err != nil {
		return fail(err)
	}
	if e := req.Params.Error; e != nil {
		return fail(fmt.Errorf("the provider refused the sign-in: %s %s", *e, valueOr(req.Params.ErrorDescription)))
	}
	if flow != nil && flow.Test {
		return s.tried(keys, flow.Provider, s.finishTest(ctx, valueOr(req.Params.Code), valueOr(req.Params.State), flow), secure)
	}

	sess, err := s.finishOIDC(ctx, valueOr(req.Params.Code), valueOr(req.Params.State), flow)
	if err != nil {
		return fail(err)
	}
	value, err := sealSession(keys.Current, sess)
	if err != nil {
		return nil, err
	}
	if sess.csrf, err = csrfFor(keys.Current, sess.ID); err != nil {
		return nil, err
	}
	if back.cli() {
		sealed, serr := handoff.Seal(back.key, &handoff.Session{Value: value, CSRF: sess.csrf, Expires: sess.Expires})
		if serr != nil {
			return nil, serr
		}
		return &signInRedirect{to: back.loopback(url.Values{"session": {sealed}}), clearFlow: true, secure: secure}, nil
	}
	return &signInRedirect{to: "/", session: value, sess: sess, clearFlow: true, secure: secure}, nil
}

// openFlow reads the sealed flow the browser brought back, or nil when it
// brought none this server sealed.
func openFlow(keys apply.SessionKeys, sealed string) *oidcFlow {
	var flow oidcFlow
	if _, ok := unseal(keys, flowCookie, sealed, &flow); !ok {
		return nil
	}
	return &flow
}

// finishOIDC checks the provider's answer against the flow the start sealed,
// and opens a session for the person it names.
func (s *Server) finishOIDC(ctx context.Context, code, state string, flow *oidcFlow) (*session, error) {
	p, who, err := s.oidcIdentity(ctx, code, state, flow)
	if err != nil {
		return nil, err
	}
	scope, ok := scopeOf(p, who.groups)
	if !ok {
		return nil, fmt.Errorf("%s signed %s in, and none of their groups is given a scope here", p.Name, who.name)
	}
	id, err := randomSecret()
	if err != nil {
		return nil, err
	}
	return &session{
		ID: id, Provider: p.ID, Subject: who.subject, Name: who.name, Scope: scope,
		Expires: s.now().Add(apply.SessionLifetime).UTC(),
	}, nil
}

// signInStep is the step a test sign-in reports the provider's part under.
const signInStep = "Sign in at the provider"

// finishTest checks the provider's answer as finishOIDC does, and says what
// it came to instead of opening a session.
func (s *Server) finishTest(ctx context.Context, code, state string, flow *oidcFlow) *checker {
	var c checker
	p, who, err := s.oidcIdentity(ctx, code, state, flow)
	if err != nil {
		c.fail(signInStep, "%s", failureDetail(err))
		return &c
	}
	c.pass(signInStep, "%s took the client secret and the redirect URI, and signed %s in", p.Name, who.name)

	claim := "The " + p.OIDC.GroupsClaim + " claim"
	if who.hasGroups {
		c.pass(claim, "%d groups", len(who.groups))
	} else {
		c.fail(claim, "%s sent none, so whoever it signs in is refused. Many send groups only when a "+
			"scope or a mapper asks for them", p.Name)
	}
	c.person = &gen.CheckedPerson{Subject: who.subject, Name: who.name, Groups: who.groups}
	if scope, ok := scopeOf(p, who.groups); ok {
		c.person.Scope = ptr(gen.Scope(scope))
		c.pass("Their scope", "%s would sign in with %s", who.name, scope)
	} else {
		c.fail("Their scope", "none of their groups is given a scope here, so a sign-in is refused")
	}
	return &c
}

// triedSeal is what a test sign-in's outcome is sealed as, so that nothing
// sealed for another use can be passed off as one.
const triedSeal = "weg_oidc_tried"

// triedSignIn is what a test sign-in came to, sealed for the page it started
// from to hand to the check, which alone opens it.
type triedSignIn struct {
	Provider string                    `json:"idp"`
	Expires  time.Time                 `json:"exp"`
	Result   gen.IdentityProviderCheck `json:"result"`
}

// tried sends the browser back to the provider's page with what a test
// sign-in came to. It goes in the fragment, which the browser sends to no
// server, so that it stays out of every log on the way.
func (s *Server) tried(keys apply.SessionKeys, pid string, c *checker, secure bool) (*signInRedirect, error) {
	sealed, err := seal(keys.Current, triedSeal, &triedSignIn{
		Provider: pid, Expires: s.now().Add(flowLifetime).UTC(), Result: c.result(),
	})
	if err != nil {
		return nil, err
	}
	to := "/providers/" + url.PathEscape(pid) + "#" + url.Values{"tried": {sealed}}.Encode()
	return &signInRedirect{to: to, clearFlow: true, secure: secure}, nil
}

// openTried reads back what tried sealed, for the provider by pid, while it
// is still good.
func (s *Server) openTried(ctx context.Context, pid, sealed string) (gen.IdentityProviderCheck, error) {
	keys, err := s.sessionKeys(ctx)
	if err != nil {
		return gen.IdentityProviderCheck{}, err
	}
	var t triedSignIn
	if _, ok := unseal(keys, triedSeal, sealed, &t); !ok || t.Provider != pid || !s.now().Before(t.Expires) {
		return gen.IdentityProviderCheck{}, badRequest("that is not a sign-in tried through this provider in the " +
			"last ten minutes; try it again")
	}
	return t.Result, nil
}

// oidcPerson is who a provider says signed in.
type oidcPerson struct {
	subject, name string
	groups        []string
	// hasGroups is whether the provider sent the groups claim at all, which
	// an empty list of groups does not tell apart.
	hasGroups bool
}

// oidcIdentity checks the provider's answer against the flow the start sealed,
// and returns the provider and who it says signed in.
func (s *Server) oidcIdentity(
	ctx context.Context, code, state string, flow *oidcFlow,
) (*apply.IdentityProvider, *oidcPerson, error) {
	now := s.now()
	if flow == nil || !now.Before(flow.Expires) {
		return nil, nil, errors.New("the sign-in took too long, or was not started in this browser; try again")
	}
	if code == "" || state == "" || state != flow.State {
		return nil, nil, errors.New("the provider's answer is not the one this sign-in asked for; try again")
	}

	p, _, err := s.oidcProvider(ctx, flow.Provider, flow.Test)
	if err != nil {
		return nil, nil, err
	}
	client, err := s.oidc.client(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, client.http), discoveryTimeout)
	defer cancel()

	cfg := client.config(p, flow.RedirectURI)
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		return nil, nil, fmt.Errorf("%s did not exchange the code: %w", p.Name, err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, nil, fmt.Errorf("%s answered without an ID token", p.Name)
	}
	idt, err := client.provider.Verifier(&oidc.Config{ClientID: p.OIDC.ClientID}).Verify(ctx, raw)
	if err != nil {
		return nil, nil, fmt.Errorf("the ID token from %s does not verify: %w", p.Name, err)
	}
	if idt.Nonce != flow.Nonce {
		return nil, nil, fmt.Errorf("the ID token from %s was not issued for this sign-in", p.Name)
	}

	var claims map[string]any
	if cerr := idt.Claims(&claims); cerr != nil {
		return nil, nil, fmt.Errorf("the ID token from %s is not readable: %w", p.Name, cerr)
	}
	// Some providers keep the groups out of the ID token and give them only
	// to whoever asks for the user info, Keycloak among them unless told
	// otherwise.
	if _, present := claims[p.OIDC.GroupsClaim]; !present {
		if info, ierr := client.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); ierr == nil {
			var more map[string]any
			if info.Claims(&more) == nil {
				for k, v := range more {
					if _, have := claims[k]; !have {
						claims[k] = v
					}
				}
			}
		}
	}

	name := firstString(claims, p.OIDC.NameClaim, "preferred_username", "email")
	if name == "" {
		name = idt.Subject
	}
	_, hasGroups := claims[p.OIDC.GroupsClaim]
	return p, &oidcPerson{
		subject: idt.Subject, name: name, groups: stringsOf(claims[p.OIDC.GroupsClaim]), hasGroups: hasGroups,
	}, nil
}

// oidcProvider returns the OpenID Connect provider by this identifier, if it
// is switched on or off is allowed, with the session keys read in the same
// transaction.
func (s *Server) oidcProvider(ctx context.Context, pid string, off bool) (*apply.IdentityProvider, apply.SessionKeys, error) {
	var (
		list []apply.IdentityProvider
		keys apply.SessionKeys
	)
	err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		if list, verr = apply.StoredIdentityProviders(ctx, r); verr != nil {
			return verr
		}
		keys, verr = apply.StoredSessionKeys(ctx, r)
		return verr
	})
	if err != nil {
		return nil, keys, err
	}
	if keys.IsZero() {
		return nil, keys, errors.New("this server holds no key to seal a session under yet; try again shortly")
	}
	i := slices.IndexFunc(list, func(p apply.IdentityProvider) bool { return p.ID == pid })
	if i < 0 || list[i].Kind != apply.ProviderOIDC || !list[i].Enabled && !off {
		return nil, keys, errors.New("that provider is not one this server signs anybody in through")
	}
	return &list[i], keys, nil
}

// sessionKeys reads the keys a session is sealed under.
func (s *Server) sessionKeys(ctx context.Context) (apply.SessionKeys, error) {
	var keys apply.SessionKeys
	err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		keys, verr = apply.StoredSessionKeys(ctx, r)
		return verr
	})
	return keys, err
}

// redirectFor picks the registered redirect URI on the address the browser
// used. Only one of the list is ever chosen, so a forged Host header picks
// another registered one or none (D51).
func redirectFor(registered []string, host string) (string, error) {
	for _, raw := range registered {
		if u, err := url.Parse(raw); err == nil && strings.EqualFold(u.Host, host) {
			return raw, nil
		}
	}
	return "", fmt.Errorf("%s is not the address of any redirect URI the provider lists; "+
		"register one for it with the provider and add it here", host)
}

// scopeOf is the strongest scope any of the groups maps to.
func scopeOf(p *apply.IdentityProvider, groups []string) (Scope, bool) {
	var best Scope
	for _, m := range p.Mappings {
		for _, g := range groups {
			if strings.EqualFold(g, m.Group) && rank(Scope(m.Scope)) > rank(best) {
				best = Scope(m.Scope)
			}
		}
	}
	return best, best != ""
}

// stringsOf reads a claim that is a list of strings, or a single one.
func stringsOf(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// firstString is the first of the claims that holds a string.
func firstString(claims map[string]any, names ...string) string {
	for _, n := range names {
		if s, ok := claims[n].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func valueOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// signInFailed is the sign-in page, saying what went wrong.
func signInFailed(err error) string {
	return "/?" + url.Values{signInError: {failureDetail(err)}}.Encode()
}

// failureDetail is what a person is told about a sign-in that failed.
func failureDetail(err error) string {
	var problem *apiError
	if errors.As(err, &problem) {
		return problem.detail
	}
	return err.Error()
}

// signInReturn is where a sign-in ends: the interface, or the port of
// 127.0.0.1 that `weg login` waits on, with the key it wants the session
// sealed to (D52).
type signInReturn struct {
	port int
	key  string
}

// returnFor reads where a sign-in is to end from what its start was given.
func returnFor(port *int, key *string) (signInReturn, error) {
	if port == nil {
		return signInReturn{}, nil
	}
	if *port < 1 || *port > 65535 {
		return signInReturn{}, fmt.Errorf("%d is not a port weg login could be listening on", *port)
	}
	back := signInReturn{port: *port, key: valueOr(key)}
	if !handoff.ValidPublic(back.key) {
		return back, errors.New("the key weg login sent is not one a session can be sealed to")
	}
	return back, nil
}

// cli reports whether `weg login` started the sign-in.
func (b signInReturn) cli() bool { return b.port != 0 }

// failed is where the browser is sent when the sign-in fails.
func (b signInReturn) failed(err error) string {
	if b.cli() {
		return b.loopback(url.Values{"error": {failureDetail(err)}})
	}
	return signInFailed(err)
}

// loopback is the address `weg login` waits on. It is built from the port and
// nothing else, so that no sign-in can be made to send a browser anywhere but
// the machine it runs on.
func (b signInReturn) loopback(q url.Values) string {
	u := url.URL{
		Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(b.port)),
		Path: "/", RawQuery: q.Encode(),
	}
	return u.String()
}

// oidcClients holds what a provider's discovery document says, so that it is
// read once rather than on every sign-in. A change to the provider replaces
// it, which is what UpdatedAt is compared for.
type oidcClients struct {
	mu   sync.Mutex
	byID map[string]*oidcClient
}

type oidcClient struct {
	updated  time.Time
	http     *http.Client
	provider *oidc.Provider
}

func (c *oidcClients) client(ctx context.Context, p *apply.IdentityProvider) (*oidcClient, error) {
	c.mu.Lock()
	held := c.byID[p.ID]
	c.mu.Unlock()
	if held != nil && held.updated.Equal(p.UpdatedAt) {
		return held, nil
	}

	hc, err := httpClientFor(p.CACertificate)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, hc), discoveryTimeout)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, p.OIDC.Issuer)
	if err != nil {
		return nil, fmt.Errorf("%s could not be reached at %s: %w", p.Name, p.OIDC.Issuer, err)
	}
	made := &oidcClient{updated: p.UpdatedAt, http: hc, provider: provider}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = make(map[string]*oidcClient)
	}
	c.byID[p.ID] = made
	return made, nil
}

// config is the OAuth 2.0 client this server is at the provider.
func (c *oidcClient) config(p *apply.IdentityProvider, redirect string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.OIDC.ClientID,
		ClientSecret: p.OIDC.ClientSecret,
		Endpoint:     c.provider.Endpoint(),
		RedirectURL:  redirect,
		Scopes:       p.OIDC.Scopes,
	}
}

// httpClientFor is the client a provider is reached with: the system's trust
// and, where one is given, the provider's own CA beside it.
func httpClientFor(caPEM string) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("api: the default HTTP transport is not one that can be given a CA")
	}
	transport := base.Clone()
	if caPEM != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, errors.New("the provider's CA certificate holds no certificate in PEM")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport, Timeout: discoveryTimeout}, nil
}

// signInRedirect sends the browser on, setting or clearing the cookies the
// sign-in moves through.
type signInRedirect struct {
	to     string
	secure bool

	// flow is the sealed flow a start hands the browser, and clearFlow
	// takes it back once the callback is done with it.
	flow        string
	flowExpires time.Time
	clearFlow   bool

	// session and sess are what a finished sign-in opened.
	session string
	sess    *session
}

func (r *signInRedirect) visit(w http.ResponseWriter) error {
	flowPath := basePath + oidcPath
	switch {
	case r.flow != "":
		// Lax, not Strict: the callback is a navigation from the provider's
		// site, and a Strict cookie would not come with it.
		http.SetCookie(w, &http.Cookie{
			Name: flowCookie, Value: r.flow, Path: flowPath, Expires: r.flowExpires,
			HttpOnly: true, Secure: r.secure, SameSite: http.SameSiteLaxMode,
		})
	case r.clearFlow:
		http.SetCookie(w, &http.Cookie{
			Name: flowCookie, Path: flowPath, MaxAge: -1,
			HttpOnly: true, Secure: r.secure, SameSite: http.SameSiteLaxMode,
		})
	}
	if r.sess != nil {
		setSessionCookies(w, r.session, r.sess, r.secure)
	}
	w.Header().Set("Location", r.to)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
	return nil
}

func (r *signInRedirect) VisitStartOIDCSignInResponse(w http.ResponseWriter) error { return r.visit(w) }
func (r *signInRedirect) VisitTryOIDCSignInResponse(w http.ResponseWriter) error   { return r.visit(w) }
func (r *signInRedirect) VisitFinishOIDCSignInResponse(w http.ResponseWriter) error {
	return r.visit(w)
}

// flowOf is the sealed flow the browser brought back, read before the strict
// handler, which sees no cookies.
func flowOf(ctx context.Context) string {
	v, ok := ctx.Value(flowKey{}).(string)
	if !ok {
		return ""
	}
	return v
}

type flowKey struct{}
