package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// The cookies and the header that carry a browser session (docs/decisions/
// D5).
const (
	sessionCookie = "weg_session"
	csrfCookie    = "weg_csrf"
	csrfHeader    = "X-Wegweiser-CSRF"

	// sessionPath is the route a browser opens a session on. It is the one
	// authenticated-looking endpoint that a request with no credential may
	// reach, because a request that could authenticate would not need it.
	sessionPath = "/auth/session"

	// sessionBytes is the length of a session identifier.
	sessionBytes = 32
)

// session is a browser's authenticated state, as its cookie carries it.
//
// Nothing about it is held on the server. The cookie is sealed under the
// cluster's session key, so every member that holds the key opens it, and a
// restart or a failover ends nothing
// (docs/decisions/d51-accounts-come-from-an-identity-provider.md).
type session struct {
	// ID names the session, so that it can be ended before its time. It is
	// minted when the session opens and means nothing on its own.
	ID string `json:"id"`

	// Token is the SHA-256 of the token the session was opened with. The token
	// is looked up by it on every request, so that revoking the token ends its
	// sessions everywhere and a session never holds more than the token does.
	Token []byte `json:"tok"`

	// Expires is when the session ends.
	Expires time.Time `json:"exp"`

	// csrf is the value the page has to repeat, worked out from the key the
	// session was opened under rather than carried.
	csrf string
}

// The labels the session key is stretched under, one per use, so that the
// key sealing a cookie is never also the one a CSRF value is computed with.
const (
	sealLabel = "wegweiser session seal"
	csrfLabel = "wegweiser session csrf"
)

// sealSession writes a session into the value its cookie carries: AES-256-GCM
// under the current key, with the nonce in front.
func sealSession(key apply.SessionKey, sess *session) (string, error) {
	plain, err := json.Marshal(sess)
	if err != nil {
		return "", fmt.Errorf("api: encode the session: %w", err)
	}
	aead, err := sealerFor(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	if _, rerr := rand.Read(nonce); rerr != nil {
		return "", fmt.Errorf("api: generate a session nonce: %w", rerr)
	}
	sealed := aead.Seal(nonce, nonce, plain, []byte(sessionCookie))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// openSession reads a cookie back, under the current key or the one before
// it. A value that opens under neither, or that has run out, is no session.
func openSession(keys apply.SessionKeys, value string, now time.Time) *session {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil
	}
	for _, key := range []apply.SessionKey{keys.Current, keys.Previous} {
		if key.IsZero() {
			continue
		}
		aead, aerr := sealerFor(key)
		if aerr != nil || len(raw) < aead.NonceSize() {
			continue
		}
		nonce, sealed := raw[:aead.NonceSize()], raw[aead.NonceSize():]
		plain, oerr := aead.Open(nil, nonce, sealed, []byte(sessionCookie))
		if oerr != nil {
			continue
		}
		var sess session
		if uerr := json.Unmarshal(plain, &sess); uerr != nil {
			return nil
		}
		if sess.ID == "" || len(sess.Token) != sha256.Size || !now.Before(sess.Expires) {
			return nil
		}
		csrf, cerr := csrfFor(key, sess.ID)
		if cerr != nil {
			return nil
		}
		sess.csrf = csrf
		return &sess
	}
	return nil
}

// sealerFor is the AEAD a session key seals with.
func sealerFor(key apply.SessionKey) (cipher.AEAD, error) {
	sub, err := hkdf.Key(sha256.New, key[:], nil, sealLabel, apply.SessionKeyLen)
	if err != nil {
		return nil, fmt.Errorf("api: derive the session seal: %w", err)
	}
	block, err := aes.NewCipher(sub)
	if err != nil {
		return nil, fmt.Errorf("api: build the session seal: %w", err)
	}
	return cipher.NewGCM(block)
}

// csrfFor is the CSRF value of the session by this identifier, under key.
func csrfFor(key apply.SessionKey, id string) (string, error) {
	sub, err := hkdf.Key(sha256.New, key[:], nil, csrfLabel, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("api: derive the CSRF key: %w", err)
	}
	mac := hmac.New(sha256.New, sub)
	mac.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// endedID is how an ended session is recorded: the hash of its identifier,
// so that the list holds nothing a cookie could be made from.
func endedID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
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

	var keys apply.SessionKeys
	if verr := s.store.View(ctx, func(r store.Reader) error {
		var kerr error
		keys, kerr = apply.StoredSessionKeys(ctx, r)
		return kerr
	}); verr != nil {
		return nil, verr
	}
	if keys.IsZero() {
		// Minted as the server starts, or arriving in the log a member is
		// replaying, so this is the moment before either has happened.
		return nil, &apiError{
			status: http.StatusServiceUnavailable,
			kind:   typeUnavailable,
			title:  "No session can be opened yet",
			detail: "this server holds no key to seal a session under yet; a member that is joining " +
				"receives it from the cluster shortly. A bearer token works meanwhile",
		}
	}

	id, err := randomSecret()
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(req.Body.Token))
	sess := &session{ID: id, Token: hash[:], Expires: s.now().Add(apply.SessionLifetime).UTC()}
	value, err := sealSession(keys.Current, sess)
	if err != nil {
		return nil, err
	}
	if sess.csrf, err = csrfFor(keys.Current, id); err != nil {
		return nil, err
	}
	return &sessionOpened{value: value, sess: sess, sub: sub, secure: secure}, nil
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
		out.ExpiresAt = ptr(sess.Expires)
	}
	return gen.GetSession200JSONResponse(out), nil
}

// DeleteSession ends the session, if the request came with one.
//
// The browser's cookies are cleared whatever happens. Recording the session as
// ended is a write, and it is one the cluster may not be taking: a member that
// does not lead hands it to the one that does, and without a leader the cookie
// a browser no longer holds stays valid until it runs out (D51).
func (s *Server) DeleteSession(
	ctx context.Context, _ gen.DeleteSessionRequestObject,
) (gen.DeleteSessionResponseObject, error) {
	if e := endingOf(ctx); e != nil {
		if err := s.endSession(ctx, e); err != nil {
			s.report(fmt.Errorf("the session is closed in the browser, and not recorded as ended: %w", err))
		}
	}
	return &sessionClosed{secure: secureOf(ctx)}, nil
}

// ending is a session being ended, as the member that holds the cookie
// describes it to the one that leads.
type ending struct {
	ID    string    `json:"id"`
	Until time.Time `json:"until"`
}

// endSession records a session as ended, here or on the leader. It decides
// the way a write is forwarded, and one handed over by another member is
// never handed on.
func (s *Server) endSession(ctx context.Context, e *ending) error {
	c := s.cluster
	if c != nil && c.Replicating() && !c.IsLeader() && ctx.Value(viaClusterPort{}) == nil {
		if err := c.Left(); err != nil {
			return err
		}
		return s.forwardEnding(ctx, e)
	}
	return s.applier.EndSession(ctx, e.ID, e.Until)
}

// sessionOpened writes the two cookies and the body.
//
// It is written out rather than taken from the generated response type,
// because that one sets a single Set-Cookie header and a session needs two:
// the credential, which the page may not read, and the CSRF value, which it
// must.
type sessionOpened struct {
	value  string
	sess   *session
	sub    *subject
	secure bool
}

func (r *sessionOpened) VisitCreateSessionResponse(w http.ResponseWriter) error {
	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookie,
		Value: r.value,
		// Scoped to the API: it is the only thing that reads it, and a cookie
		// sent where it is not needed is a cookie in more logs than necessary.
		Path:     basePath,
		Expires:  r.sess.Expires,
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
		Expires:  r.sess.Expires,
		HttpOnly: false,
		Secure:   r.secure,
		SameSite: http.SameSiteStrictMode,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	return json.NewEncoder(w).Encode(gen.Session{
		Name:      r.sub.name,
		Scopes:    scopesToAPI(r.sub.scopes),
		ExpiresAt: ptr(r.sess.Expires),
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
