package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// Scope is a permission a token carries.
type Scope string

// The scopes a token may carry. They are ordered: admin allows everything
// write allows, and write allows everything read allows. Three levels rather
// than a permission per endpoint, because a permission model nobody can hold
// in their head is one that gets granted wholesale.
const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
	ScopeAdmin Scope = "admin"
)

// rank orders the scopes so that a stronger one satisfies a weaker requirement.
func rank(s Scope) int {
	switch s {
	case ScopeRead:
		return 1
	case ScopeWrite:
		return 2
	case ScopeAdmin:
		return 3
	default:
		return 0
	}
}

// TokenPrefix marks a secret as one of ours, so that a token pasted into the
// wrong field is recognisable in a log and a secret scanner has something to
// match on.
const TokenPrefix = "weg_"

// tokenBytes is the length of the secret, in octets. 256 bits from crypto/rand
// is not brute-forceable, which is the whole reason the stored form is a plain
// SHA-256 rather than a slow password hash (docs/decisions.md D5).
const tokenBytes = 32

// prefixLen is how much of the secret is stored in the clear, so that the UI
// and the audit log can tell two tokens apart without holding either. Long
// enough to be distinctive, far too short to be guessed from.
const prefixLen = len(TokenPrefix) + 8

// ErrTokenInvalid is what every failed authentication returns.
//
// Unknown, revoked and expired are one error on purpose: three would let an
// attacker sort guesses into "wrong" and "used to be right" (D5).
var ErrTokenInvalid = errors.New("api: the token is not valid")

// MintToken creates a new token and the record that stores it.
func MintToken(name string, scopes []Scope, now time.Time) (secret string, tok store.Token, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", store.Token{}, fmt.Errorf("api: generate a token: %w", err)
	}
	secret = TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	granted := make([]string, 0, len(scopes))
	for _, s := range scopes {
		granted = append(granted, string(s))
	}

	sum := sha256.Sum256([]byte(secret))
	return secret, store.Token{
		ID:        store.TokenID(id.New()),
		Name:      name,
		Prefix:    secret[:prefixLen],
		Hash:      sum[:],
		Scopes:    granted,
		CreatedAt: now,
	}, nil
}

// subject is the authenticated caller, carried on the request context.
type subject struct {
	tokenID store.TokenID
	name    string
	scopes  []Scope
}

// allows reports whether the subject may do something needing the given scope.
func (s *subject) allows(need Scope) bool {
	for _, held := range s.scopes {
		if rank(held) >= rank(need) {
			return true
		}
	}
	return false
}

// The context keys. Unexported types, so nothing another package stores can
// collide with them.
type (
	subjectKey   struct{}
	sessionKey   struct{}
	sessionIDKey struct{}
	factsKey     struct{}
)

// facts are the things about a request that a strict handler cannot see,
// because it is handed a decoded request object rather than an *http.Request.
type facts struct {
	// from is what the rate limiter counts against.
	from string
	// secure reports whether the request arrived over TLS, which decides
	// whether a cookie may be marked Secure.
	secure bool
}

// withFacts records what the handlers below cannot work out for themselves.
func withFacts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := facts{from: sourceAddress(r), secure: r.TLS != nil}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), factsKey{}, f)))
	})
}

// sourceOf returns the address the request came from.
func sourceOf(ctx context.Context) string {
	f, ok := ctx.Value(factsKey{}).(facts)
	if !ok {
		return ""
	}
	return f.from
}

// secureOf reports whether the request arrived over TLS.
func secureOf(ctx context.Context) bool {
	f, ok := ctx.Value(factsKey{}).(facts)
	return ok && f.secure
}

// sessionOf returns the browser session a request authenticated with, or nil
// when it authenticated with a bearer token.
func sessionOf(ctx context.Context) *session {
	s, ok := ctx.Value(sessionKey{}).(*session)
	if !ok {
		return nil
	}
	return s
}

// sessionIDOf returns the identifier addressing that session, so that ending
// it can find it again.
func sessionIDOf(ctx context.Context) string {
	sid, ok := ctx.Value(sessionIDKey{}).(string)
	if !ok {
		return ""
	}
	return sid
}

// isTokenInvalid reports whether a failure was the credential rather than the
// server.
func isTokenInvalid(err error) bool { return errors.Is(err, ErrTokenInvalid) }

// tooManyAttempts is what an address that has been guessing gets.
func tooManyAttempts() *apiError {
	return &apiError{
		status: http.StatusTooManyRequests,
		kind:   typeRateLimited,
		title:  "Too many failed attempts",
		detail: "too many tokens have been rejected from this address recently",
	}
}

// subjectOf returns the caller a request was authenticated as, or nil.
func subjectOf(ctx context.Context) *subject {
	s, ok := ctx.Value(subjectKey{}).(*subject)
	if !ok {
		return nil
	}
	return s
}

// authenticate resolves a bearer secret to the caller holding it.
func (s *Server) authenticate(ctx context.Context, secret string) (*subject, error) {
	sum := sha256.Sum256([]byte(secret))

	var tok *store.Token
	err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		tok, verr = r.TokenByHash(ctx, sum[:])
		return verr
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrTokenInvalid
		}
		return nil, err
	}

	// The lookup already matched on the hash, so this compares two values that
	// are equal or the database is lying. It is here because D5 says token
	// comparison is constant-time, and a comparison that only happens to be
	// safe is one refactor away from not being.
	if subtle.ConstantTimeCompare(tok.Hash, sum[:]) != 1 {
		return nil, ErrTokenInvalid
	}
	if !tok.Active(s.now()) {
		return nil, ErrTokenInvalid
	}

	held := make([]Scope, 0, len(tok.Scopes))
	for _, sc := range tok.Scopes {
		held = append(held, Scope(sc))
	}
	// Noted rather than written: the update happens off the request path, in
	// one transaction for every token seen since the last one (lastused.go).
	s.tokenUse.record(tok.ID, s.now())

	return &subject{tokenID: tok.ID, name: tok.Name, scopes: held}, nil
}

// authenticator is the middleware every request but the health check passes
// through.
func (s *Server) authenticator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.unauthenticated(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Nothing to verify is not the same as something that failed to
		// verify. The web interface asks who it is on every load, and with no
		// session that request carries no credential at all: counting it as a
		// rejected token let a browser lock its own user out by being opened
		// eleven times. A request with nothing in it is also nothing to guess
		// with, which is what the limiter exists to slow down.
		if !presented(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wegweiser"`)
			writeProblem(w, r, unauthorized(missingOrInvalid(r)))
			return
		}

		from := sourceOf(r.Context())
		if !s.limiter.allow(from, s.now()) {
			writeProblem(w, r, tooManyAttempts())
			return
		}

		ctx, sub, err := s.credential(r)
		if err != nil {
			if !isTokenInvalid(err) {
				writeProblem(w, r, err)
				return
			}
			s.limiter.spend(from, s.now())
			w.Header().Set("WWW-Authenticate", `Bearer realm="wegweiser"`)
			writeProblem(w, r, unauthorized(missingOrInvalid(r)))
			return
		}

		if need := scopeNeeded(r); !sub.allows(need) {
			writeProblem(w, r, &apiError{
				status: http.StatusForbidden,
				kind:   typeForbidden,
				title:  "Not allowed",
				detail: fmt.Sprintf("this token does not carry the %q scope", need),
			})
			return
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// unauthenticated reports whether a route may be reached without a credential.
func (s *Server) unauthenticated(r *http.Request) bool {
	if strings.HasSuffix(r.URL.Path, healthPath) {
		return true
	}
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, sessionPath)
}

// presented reports whether the request carried a credential at all.
func presented(r *http.Request) bool {
	if _, ok := bearer(r); ok {
		return true
	}
	_, err := r.Cookie(sessionCookie)
	return err == nil
}

// credential resolves whichever of the two credentials the request carried,
// and returns the context the handlers below should see.
func (s *Server) credential(r *http.Request) (context.Context, *subject, error) {
	ctx := r.Context()

	if secret, ok := bearer(r); ok {
		sub, err := s.authenticate(ctx, secret)
		if err != nil {
			if !isTokenInvalid(err) {
				return nil, nil, internal(err)
			}
			return nil, nil, err
		}
		return context.WithValue(ctx, subjectKey{}, sub), sub, nil
	}

	c, cerr := r.Cookie(sessionCookie)
	if cerr != nil {
		return nil, nil, ErrTokenInvalid
	}
	sess := s.sessions.lookup(c.Value, s.now())
	if sess == nil {
		return nil, nil, ErrTokenInvalid
	}

	// Only here. A bearer token is not ambient (a cross-site request cannot
	// make the browser attach one) so requiring the header of a program that
	// sends its own credential would protect nothing and break every client.
	if changesState(r.Method) {
		if err := checkCSRF(r, sess); err != nil {
			return nil, nil, err
		}
	}

	sub := &subject{tokenID: sess.tokenID, name: sess.name, scopes: sess.scopes}
	ctx = context.WithValue(ctx, subjectKey{}, sub)
	ctx = context.WithValue(ctx, sessionKey{}, sess)
	ctx = context.WithValue(ctx, sessionIDKey{}, c.Value)
	return ctx, sub, nil
}

// unauthorized reports a request that carried no usable credential.
func unauthorized(detail string) *apiError {
	return &apiError{
		status: http.StatusUnauthorized,
		kind:   typeUnauthorized,
		title:  "Not authenticated",
		detail: detail,
	}
}

// scopeNeeded returns the scope a request needs.
//
// From the method, with one exception: a caller managing its own session needs
// no permission beyond being authenticated at all. Ending a session takes
// access away rather than using it, and a read-only token that could not log
// out would be a read-only token that cannot stop being logged in.
func scopeNeeded(r *http.Request) Scope {
	if strings.HasSuffix(r.URL.Path, sessionPath) {
		return ScopeRead
	}
	return scopeFor(r.Method)
}

// scopeFor returns the scope a method needs, from what it intends to do.
func scopeFor(method string) Scope {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ScopeRead
	default:
		return ScopeWrite
	}
}

// missingOrInvalid tells a client that forgot a credential apart from one
// whose credential was refused.
//
// Which credential was refused, and why, is deliberately not said: unknown,
// revoked and expired are one answer, so that an attacker cannot sort guesses
// into "wrong" and "used to be right" (docs/decisions.md D5).
func missingOrInvalid(r *http.Request) string {
	if _, ok := bearer(r); ok {
		return "the credential is not valid"
	}
	if _, err := r.Cookie(sessionCookie); err == nil {
		return "the session is not valid; open a new one"
	}
	return "this endpoint needs a token or a session"
}

// bearer extracts the token from an Authorization header.
func bearer(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// sourceAddress is what the rate limiter counts against.
func sourceAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Rate limiting of failed authentication (D5).
const (
	// authBurst is how many rejections one address may collect before it has
	// to wait. Enough for a person pasting the wrong token a few times.
	authBurst = 10
	// authRefill is how long it takes to earn one attempt back.
	authRefill = 6 * time.Second
	// authForget is how long an address with a full bucket is remembered. It
	// bounds what an attacker can make the server hold.
	authForget = 10 * time.Minute
)

// authLimiter caps failed authentication per source address.
type authLimiter struct {
	mu      sync.Mutex
	buckets map[string]*authBucket
}

type authBucket struct {
	// tokens is in units of authRefill, tracked as a time: the bucket is full
	// when its deadline is in the past, and every failure pushes the deadline
	// one refill further out. One timestamp is the whole state.
	full time.Time
	seen time.Time
}

func newAuthLimiter() *authLimiter {
	return &authLimiter{buckets: make(map[string]*authBucket)}
}

// allow reports whether an address may try again.
func (l *authLimiter) allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[addr]
	if b == nil {
		return true
	}
	b.seen = now
	return !b.full.After(now.Add(authBurst * authRefill))
}

// spend records a rejection.
func (l *authLimiter) spend(addr string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.forget(now)

	b := l.buckets[addr]
	if b == nil {
		b = &authBucket{full: now}
		l.buckets[addr] = b
	}
	if b.full.Before(now) {
		b.full = now
	}
	b.full = b.full.Add(authRefill)
	b.seen = now
}

// forget drops addresses that have not been heard from in a while, so that the
// table is bounded by recent traffic rather than by every address that has ever
// guessed wrong.
func (l *authLimiter) forget(now time.Time) {
	for addr, b := range l.buckets {
		if now.Sub(b.seen) > authForget {
			delete(l.buckets, addr)
		}
	}
}

// BootstrapName is the name the first token carries, so that an operator
// listing tokens can see which one came from the installer rather than from a
// person.
const BootstrapName = "bootstrap"

// EnsureBootstrapToken creates the first admin token if the database has none,
// and returns the secret exactly once.
//
// An empty secret means tokens already existed and nothing was created. There
// is no way to ask for the secret again: what is stored is a hash, so a lost
// bootstrap token is replaced rather than recovered (docs/decisions.md D5).
func EnsureBootstrapToken(
	ctx context.Context, s store.Store, now time.Time,
) (secret string, err error) {
	var existing []*store.Token
	if verr := s.View(ctx, func(r store.Reader) error {
		var lerr error
		existing, lerr = r.ListTokens(ctx)
		return lerr
	}); verr != nil {
		return "", fmt.Errorf("api: look for existing tokens: %w", verr)
	}
	if len(existing) > 0 {
		return "", nil
	}

	secret, tok, err := MintToken(BootstrapName, []Scope{ScopeAdmin}, now)
	if err != nil {
		return "", err
	}
	if err := s.Update(ctx, func(tx store.Tx) error {
		return tx.CreateToken(ctx, &tok)
	}); err != nil {
		return "", fmt.Errorf("api: store the bootstrap token: %w", err)
	}
	return secret, nil
}
