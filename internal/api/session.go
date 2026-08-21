package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// The cookies and the header that carry a browser session (docs/decisions.md
// D5).
const (
	sessionCookie = "weg_session"
	csrfCookie    = "weg_csrf"
	csrfHeader    = "X-Wegweiser-CSRF"

	// sessionPath is the route a browser opens a session on. It is the one
	// authenticated-looking endpoint that a request with no credential may
	// reach, because a request that could authenticate would not need it.
	sessionPath = "/auth/session"

	// sessionTTL is how long a session lasts. Long enough for a working day,
	// short enough that a laptop left open overnight is not an open door.
	sessionTTL = 12 * time.Hour

	// sessionBytes is the length of a session identifier and of a CSRF value.
	sessionBytes = 32
)

// session is a browser's authenticated state.
type session struct {
	tokenID store.TokenID
	name    string
	scopes  []Scope
	csrf    string
	expires time.Time
}

// sessionStore holds the open sessions.
//
// TODO: in memory, so a restart ends every session and the interface asks for
// the token again. That is a real cost and a deliberate one for v0.1, which is
// single-node by scope: the alternative is a table, four store methods and a
// migration, and a session is not zone data; it does not belong in the
// journal, and it is not what the store exists to hold. The seam is this type;
// a store-backed one replaces it without the rest of the package noticing.
type sessionStore struct {
	mu sync.Mutex
	// Keyed by the SHA-256 of the identifier rather than by the identifier, so
	// that what is held in memory is not itself a usable credential: the same
	// reason the database holds a token's hash and not the token.
	open map[[sha256.Size]byte]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{open: make(map[[sha256.Size]byte]*session)}
}

// start opens a session and returns the identifier that addresses it.
func (s *sessionStore) start(sub *subject, now time.Time) (string, *session, error) {
	id, err := randomSecret()
	if err != nil {
		return "", nil, err
	}
	csrf, err := randomSecret()
	if err != nil {
		return "", nil, err
	}

	sess := &session{
		tokenID: sub.tokenID,
		name:    sub.name,
		scopes:  sub.scopes,
		csrf:    csrf,
		expires: now.Add(sessionTTL),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	s.open[sha256.Sum256([]byte(id))] = sess
	return id, sess, nil
}

// lookup resolves an identifier to a session that has not expired.
func (s *sessionStore) lookup(id string, now time.Time) *session {
	sum := sha256.Sum256([]byte(id))

	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.open[sum]
	if !ok {
		return nil
	}
	if !now.Before(sess.expires) {
		delete(s.open, sum)
		return nil
	}
	return sess
}

// end forgets a session, so that a copy of the cookie taken beforehand stops
// working as well as the browser that had it.
func (s *sessionStore) end(id string) {
	sum := sha256.Sum256([]byte(id))

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, sum)
}

// sweepLocked drops what has expired. It runs when a session is opened, which
// is often enough: the map grows only there.
func (s *sessionStore) sweepLocked(now time.Time) {
	for k, sess := range s.open {
		if !now.Before(sess.expires) {
			delete(s.open, k)
		}
	}
}

// randomSecret returns an unguessable value in a form a cookie may carry.
func randomSecret() (string, error) {
	raw := make([]byte, sessionBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("api: generate a session value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// CreateSession exchanges a token for a browser session.
func (s *Server) CreateSession(
	ctx context.Context, req gen.CreateSessionRequestObject,
) (gen.CreateSessionResponseObject, error) {
	from := sourceOf(ctx)
	secure := secureOf(ctx)
	if err := checkTransport(secure, from); err != nil {
		return nil, err
	}
	if !s.limiter.allow(from, s.now()) {
		return nil, tooManyAttempts()
	}

	sub, err := s.authenticate(ctx, req.Body.Token)
	if err != nil {
		if !isTokenInvalid(err) {
			return nil, err
		}
		s.limiter.spend(from, s.now())
		return nil, unauthorized("the token is not valid")
	}

	id, sess, err := s.sessions.start(sub, s.now())
	if err != nil {
		return nil, err
	}
	return &sessionOpened{id: id, sess: sess, secure: secure}, nil
}

// checkTransport refuses to put a credential on a cookie that would travel the
// network in the clear.
func checkTransport(secure bool, from string) error {
	if secure {
		return nil
	}
	if addr, err := netip.ParseAddr(from); err == nil && addr.IsLoopback() {
		return nil
	}
	return &apiError{
		status: http.StatusForbidden,
		kind:   typeForbidden,
		title:  "Not allowed",
		detail: "a session may not be opened over plain HTTP from another machine; " +
			"reach this server over HTTPS, or put a TLS-terminating proxy in front of it. " +
			"A bearer token works over any transport and is what a program should use.",
	}
}

// GetSession reports who the request is authenticated as.
func (s *Server) GetSession(
	ctx context.Context, _ gen.GetSessionRequestObject,
) (gen.GetSessionResponseObject, error) {
	sub := subjectOf(ctx)
	if sub == nil {
		return nil, unauthorized("this endpoint needs a token")
	}

	out := gen.Session{Name: sub.name, Scopes: scopesToAPI(sub.scopes)}
	// Absent for a bearer token: it has an expiry of its own, and reporting
	// the token's here would say the request is about to stop working when it
	// is the credential that is.
	if sess := sessionOf(ctx); sess != nil {
		out.ExpiresAt = ptr(sess.expires)
	}
	return gen.GetSession200JSONResponse(out), nil
}

// DeleteSession ends the session, if the request came with one.
func (s *Server) DeleteSession(
	ctx context.Context, _ gen.DeleteSessionRequestObject,
) (gen.DeleteSessionResponseObject, error) {
	if id := sessionIDOf(ctx); id != "" {
		s.sessions.end(id)
	}
	return &sessionClosed{secure: secureOf(ctx)}, nil
}

// sessionOpened writes the two cookies and the body.
//
// It is written out rather than taken from the generated response type,
// because that one sets a single Set-Cookie header and a session needs two:
// the credential, which the page may not read, and the CSRF value, which it
// must.
type sessionOpened struct {
	id     string
	sess   *session
	secure bool
}

func (r *sessionOpened) VisitCreateSessionResponse(w http.ResponseWriter) error {
	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookie,
		Value: r.id,
		// Scoped to the API: it is the only thing that reads it, and a cookie
		// sent where it is not needed is a cookie in more logs than necessary.
		Path:     basePath,
		Expires:  r.sess.expires,
		HttpOnly: true,
		Secure:   r.secure,
		SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:  csrfCookie,
		Value: r.sess.csrf,
		// Readable by the page, and at the root, because the interface is
		// served from there and has to put this value in a header.
		Path:     "/",
		Expires:  r.sess.expires,
		HttpOnly: false,
		Secure:   r.secure,
		SameSite: http.SameSiteStrictMode,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	return json.NewEncoder(w).Encode(gen.Session{
		Name:      r.sess.name,
		Scopes:    scopesToAPI(r.sess.scopes),
		ExpiresAt: ptr(r.sess.expires),
	})
}

// sessionClosed clears both cookies.
type sessionClosed struct{ secure bool }

func (r *sessionClosed) VisitDeleteSessionResponse(w http.ResponseWriter) error {
	for _, c := range []struct {
		name string
		path string
		only bool
	}{
		{sessionCookie, basePath, true},
		{csrfCookie, "/", false},
	} {
		http.SetCookie(w, &http.Cookie{
			Name:     c.name,
			Value:    "",
			Path:     c.path,
			MaxAge:   -1,
			HttpOnly: c.only,
			Secure:   r.secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// scopesToAPI renders the scopes a caller holds.
func scopesToAPI(scopes []Scope) []gen.Scope {
	out := make([]gen.Scope, 0, len(scopes))
	for _, sc := range scopes {
		out = append(out, gen.Scope(sc))
	}
	return out
}

// checkCSRF refuses a state-changing request that did not repeat the session's
// CSRF value.
func checkCSRF(r *http.Request, sess *session) error {
	sent := r.Header.Get(csrfHeader)
	if sent == "" {
		return &apiError{
			status: http.StatusForbidden,
			kind:   typeForbidden,
			title:  "Not allowed",
			detail: "a session-authenticated write needs the " + csrfHeader + " header",
		}
	}
	if subtle.ConstantTimeCompare([]byte(sent), []byte(sess.csrf)) != 1 {
		return &apiError{
			status: http.StatusForbidden,
			kind:   typeForbidden,
			title:  "Not allowed",
			detail: "the " + csrfHeader + " header does not match this session",
		}
	}
	return nil
}

// changesState reports whether a method needs the CSRF check. GET, HEAD and
// OPTIONS are safe methods (RFC 9110 §9.2.1) and are not supposed to change
// anything; every endpoint here holds to that.
func changesState(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
