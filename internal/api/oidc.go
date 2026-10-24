package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
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
	fail := func(err error) (gen.StartOIDCSignInResponseObject, error) {
		return &signInRedirect{to: signInFailed(err), secure: secure}, nil
	}
	if err := checkTransport(secure, sourceOf(ctx)); err != nil {
		return fail(err)
	}

	p, keys, err := s.oidcProvider(ctx, req.ProviderId)
	if err != nil {
		return fail(err)
	}
	redirect, err := redirectFor(p.OIDC.RedirectURIs, hostOf(ctx))
	if err != nil {
		return fail(err)
	}
	client, err := s.oidc.client(ctx, p)
	if err != nil {
		return fail(err)
	}

	state, err := randomSecret()
	if err != nil {
		return nil, err
	}
	nonce, err := randomSecret()
	if err != nil {
		return nil, err
	}
	flow := oidcFlow{
		Provider: p.ID, State: state, Nonce: nonce, Verifier: oauth2.GenerateVerifier(),
		RedirectURI: redirect, Expires: s.now().Add(flowLifetime).UTC(),
	}
	sealed, err := seal(keys.Current, flowCookie, &flow)
	if err != nil {
		return nil, err
	}
	to := client.config(p, redirect).AuthCodeURL(state,
		oidc.Nonce(nonce), oauth2.S256ChallengeOption(flow.Verifier))
	return &signInRedirect{to: to, flow: sealed, flowExpires: flow.Expires, secure: secure}, nil
}

// FinishOIDCSignIn takes the browser back from the provider.
func (s *Server) FinishOIDCSignIn(
	ctx context.Context, req gen.FinishOIDCSignInRequestObject,
) (gen.FinishOIDCSignInResponseObject, error) {
	from, secure := sourceOf(ctx), secureOf(ctx)
	fail := func(err error) (gen.FinishOIDCSignInResponseObject, error) {
		s.limiter.spend(from, s.now())
		return &signInRedirect{to: signInFailed(err), clearFlow: true, secure: secure}, nil
	}
	if !s.limiter.allow(from, s.now()) {
		return &signInRedirect{to: signInFailed(tooManyAttempts()), clearFlow: true, secure: secure}, nil
	}
	if err := checkTransport(secure, from); err != nil {
		return fail(err)
	}
	if e := req.Params.Error; e != nil {
		return fail(fmt.Errorf("the provider refused the sign-in: %s %s", *e, valueOr(req.Params.ErrorDescription)))
	}

	sess, keys, err := s.finishOIDC(ctx, valueOr(req.Params.Code), valueOr(req.Params.State), flowOf(ctx))
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
	return &signInRedirect{to: "/", session: value, sess: sess, clearFlow: true, secure: secure}, nil
}

// finishOIDC checks the provider's answer against the flow the start sealed,
// and opens a session for the person it names.
func (s *Server) finishOIDC(ctx context.Context, code, state, sealedFlow string) (*session, apply.SessionKeys, error) {
	now := s.now()
	keys, err := s.sessionKeys(ctx)
	if err != nil {
		return nil, keys, err
	}
	var flow oidcFlow
	if _, ok := unseal(keys, flowCookie, sealedFlow, &flow); !ok || !now.Before(flow.Expires) {
		return nil, keys, errors.New("the sign-in took too long, or was not started in this browser; try again")
	}
	if code == "" || state == "" || state != flow.State {
		return nil, keys, errors.New("the provider's answer is not the one this sign-in asked for; try again")
	}

	p, _, err := s.oidcProvider(ctx, flow.Provider)
	if err != nil {
		return nil, keys, err
	}
	client, err := s.oidc.client(ctx, p)
	if err != nil {
		return nil, keys, err
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, client.http), discoveryTimeout)
	defer cancel()

	cfg := client.config(p, flow.RedirectURI)
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		return nil, keys, fmt.Errorf("%s did not exchange the code: %w", p.Name, err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, keys, fmt.Errorf("%s answered without an ID token", p.Name)
	}
	idt, err := client.provider.Verifier(&oidc.Config{ClientID: p.OIDC.ClientID}).Verify(ctx, raw)
	if err != nil {
		return nil, keys, fmt.Errorf("the ID token from %s does not verify: %w", p.Name, err)
	}
	if idt.Nonce != flow.Nonce {
		return nil, keys, fmt.Errorf("the ID token from %s was not issued for this sign-in", p.Name)
	}

	var claims map[string]any
	if cerr := idt.Claims(&claims); cerr != nil {
		return nil, keys, fmt.Errorf("the ID token from %s is not readable: %w", p.Name, cerr)
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
	scope, ok := scopeOf(p, stringsOf(claims[p.OIDC.GroupsClaim]))
	if !ok {
		return nil, keys, fmt.Errorf("%s signed %s in, and none of their groups is given a scope here", p.Name, name)
	}

	id, err := randomSecret()
	if err != nil {
		return nil, keys, err
	}
	return &session{
		ID: id, Provider: p.ID, Subject: idt.Subject, Name: name, Scope: scope,
		Expires: now.Add(apply.SessionLifetime).UTC(),
	}, keys, nil
}

// oidcProvider returns the OpenID Connect provider by this identifier, if it
// is switched on, with the session keys read in the same transaction.
func (s *Server) oidcProvider(ctx context.Context, pid string) (*apply.IdentityProvider, apply.SessionKeys, error) {
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
	if i < 0 || list[i].Kind != apply.ProviderOIDC || !list[i].Enabled {
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
	detail := err.Error()
	var problem *apiError
	if errors.As(err, &problem) {
		detail = problem.detail
	}
	return "/?" + url.Values{signInError: {detail}}.Encode()
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
