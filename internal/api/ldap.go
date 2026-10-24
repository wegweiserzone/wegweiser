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
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// ldapPath is where a person signs in through a directory, below the API's
// base path.
const ldapPath = "/auth/ldap"

// directoryTimeout bounds a sign-in's whole conversation with a directory.
const directoryTimeout = 10 * time.Second

// errNotSignedIn is every way a sign-in through a directory fails that the
// person could be told apart by: no such name, more than one, a wrong
// password. They are said the same way, so that the answer does not tell
// whoever is guessing which names exist.
var errNotSignedIn = errors.New("the name and password do not sign anybody in")

// directory is the part of an LDAP connection a sign-in uses. A
// *ldap.Conn is one; a test stands one in.
type directory interface {
	Bind(username, password string) error
	Search(req *ldap.SearchRequest) (*ldap.SearchResult, error)
	Close() error
}

// dialDirectory connects to a directory over TLS, as ldaps:// or by StartTLS
// on ldap://. The provider was refused when it was saved if it would be
// neither, and this refuses again rather than trusting that.
func dialDirectory(ctx context.Context, p *apply.IdentityProvider) (directory, error) {
	l := p.LDAP
	u, err := url.Parse(l.URL)
	if err != nil {
		return nil, fmt.Errorf("the directory's URL: %w", err)
	}
	cfg := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if p.CACertificate != "" {
		pool, perr := x509.SystemCertPool()
		if perr != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(p.CACertificate)) {
			return nil, errors.New("the directory's CA certificate holds no certificate in PEM")
		}
		cfg.RootCAs = pool
	}

	dialer := &net.Dialer{Timeout: directoryTimeout}
	if deadline, ok := ctx.Deadline(); ok {
		dialer.Deadline = deadline
	}
	switch {
	case u.Scheme == "ldaps":
		conn, derr := ldap.DialURL(l.URL, ldap.DialWithDialer(dialer), ldap.DialWithTLSConfig(cfg))
		if derr != nil {
			return nil, derr
		}
		conn.SetTimeout(directoryTimeout)
		return conn, nil
	case u.Scheme == "ldap" && l.StartTLS:
		conn, derr := ldap.DialURL(l.URL, ldap.DialWithDialer(dialer))
		if derr != nil {
			return nil, derr
		}
		conn.SetTimeout(directoryTimeout)
		if terr := conn.StartTLS(cfg); terr != nil {
			return nil, errors.Join(fmt.Errorf("StartTLS: %w", terr), conn.Close())
		}
		return conn, nil
	default:
		return nil, errors.New("a password would cross the network in the clear")
	}
}

// CreateDirectorySession signs a person in through a directory.
func (s *Server) CreateDirectorySession(
	ctx context.Context, req gen.CreateDirectorySessionRequestObject,
) (gen.CreateDirectorySessionResponseObject, error) {
	from, secure := sourceOf(ctx), secureOf(ctx)
	if err := checkTransport(secure, from); err != nil {
		return nil, err
	}
	if !s.limiter.allow(from, s.now()) {
		return nil, tooManyAttempts()
	}
	if req.Body == nil {
		return nil, badRequest("a sign-in sends a username and a password")
	}

	p, keys, err := s.directoryProvider(ctx, req.ProviderId)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, directoryTimeout)
	defer cancel()
	person, err := s.signInToDirectory(ctx, p, req.Body.Username, req.Body.Password)
	if err != nil {
		s.limiter.spend(from, s.now())
		var refused *apiError
		if errors.As(err, &refused) {
			return nil, refused
		}
		if errors.Is(err, errNotSignedIn) {
			return nil, unauthorized(err.Error())
		}
		return nil, &apiError{
			status: http.StatusBadGateway, kind: typeUnavailable, title: "The directory could not be asked",
			detail: fmt.Sprintf("%s could not be asked who this is: %v", p.Name, err),
		}
	}

	id, err := randomSecret()
	if err != nil {
		return nil, err
	}
	sess := &session{
		ID: id, Provider: p.ID, Subject: person.dn, Name: person.name, Scope: person.scope,
		Expires: s.now().Add(apply.SessionLifetime).UTC(),
	}
	value, err := sealSession(keys.Current, sess)
	if err != nil {
		return nil, err
	}
	if sess.csrf, err = csrfFor(keys.Current, id); err != nil {
		return nil, err
	}
	sub := &subject{name: personName(person.name, p.Name), scopes: []Scope{person.scope}}
	return &sessionOpened{value: value, sess: sess, sub: sub, secure: secure}, nil
}

// VisitCreateDirectorySessionResponse writes the session the way a token's
// is written.
func (r *sessionOpened) VisitCreateDirectorySessionResponse(w http.ResponseWriter) error {
	return r.VisitCreateSessionResponse(w)
}

// directoryPerson is who a directory says signed in.
type directoryPerson struct {
	dn    string
	name  string
	scope Scope
}

// signInToDirectory finds the person's entry, binds as it with their
// password, and maps their groups to a scope.
func (s *Server) signInToDirectory(
	ctx context.Context, p *apply.IdentityProvider, username, password string,
) (*directoryPerson, error) {
	// An empty password is an unauthenticated bind (RFC 4513 §5.1.2), which
	// many directories answer with success. It signs nobody in here.
	if strings.TrimSpace(username) == "" || password == "" {
		return nil, errNotSignedIn
	}
	l := p.LDAP
	conn, err := s.dialLDAP(ctx, p)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	asService := func() error {
		if l.BindDN == "" {
			return nil
		}
		if berr := conn.Bind(l.BindDN, l.BindPassword); berr != nil {
			return fmt.Errorf("the bind DN %s was refused: %w", l.BindDN, berr)
		}
		return nil
	}
	if berr := asService(); berr != nil {
		return nil, berr
	}

	attrs := []string{l.NameAttribute}
	if l.Groups == apply.GroupsMemberOf {
		attrs = append(attrs, "memberOf")
	}
	found, err := conn.Search(ldap.NewSearchRequest(
		l.UserBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, int(directoryTimeout.Seconds()), false,
		strings.ReplaceAll(l.UserFilter, apply.PlaceholderUsername, ldap.EscapeFilter(username)),
		attrs, nil))
	if err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		return nil, fmt.Errorf("the search for the person: %w", err)
	}
	if found == nil || len(found.Entries) != 1 {
		return nil, errNotSignedIn
	}
	entry := found.Entries[0]

	if berr := conn.Bind(entry.DN, password); berr != nil {
		if ldap.IsErrorWithCode(berr, ldap.LDAPResultInvalidCredentials) {
			return nil, errNotSignedIn
		}
		return nil, fmt.Errorf("binding as the person: %w", berr)
	}

	groups := entry.GetAttributeValues("memberOf")
	if l.Groups == apply.GroupsSearch {
		// Back to the service account, if there is one: what a person may
		// read of the directory is theirs to decide, not this server's.
		if berr := asService(); berr != nil {
			return nil, berr
		}
		filter := strings.ReplaceAll(l.GroupFilter, apply.PlaceholderDN, ldap.EscapeFilter(entry.DN))
		filter = strings.ReplaceAll(filter, apply.PlaceholderUsername, ldap.EscapeFilter(username))
		in, serr := conn.Search(ldap.NewSearchRequest(
			l.GroupBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, int(directoryTimeout.Seconds()), false,
			filter, []string{"dn"}, nil))
		if serr != nil {
			return nil, fmt.Errorf("the search for the person's groups: %w", serr)
		}
		for _, g := range in.Entries {
			groups = append(groups, g.DN)
		}
	}

	name := entry.GetAttributeValue(l.NameAttribute)
	if name == "" {
		name = username
	}
	scope, ok := scopeOf(p, groupNames(groups))
	if !ok {
		return nil, &apiError{
			status: http.StatusForbidden, kind: typeForbidden, title: "Not allowed",
			detail: fmt.Sprintf("%s signed %s in, and none of their groups is given a scope here", p.Name, name),
		}
	}
	return &directoryPerson{dn: entry.DN, name: name, scope: scope}, nil
}

// groupNames is each group as a DN and as the value of its first RDN, so that
// a mapping may name cn=admins,cn=groups,cn=accounts,dc=example,dc=com or
// just admins.
func groupNames(dns []string) []string {
	out := make([]string, 0, 2*len(dns))
	for _, raw := range dns {
		out = append(out, raw)
		if dn, err := ldap.ParseDN(raw); err == nil && len(dn.RDNs) > 0 && len(dn.RDNs[0].Attributes) > 0 {
			out = append(out, dn.RDNs[0].Attributes[0].Value)
		}
	}
	return out
}

// directoryProvider returns the directory by this identifier, if it is
// switched on, with the session keys read in the same transaction.
func (s *Server) directoryProvider(ctx context.Context, pid string) (*apply.IdentityProvider, apply.SessionKeys, error) {
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
		return nil, keys, &apiError{
			status: http.StatusServiceUnavailable, kind: typeUnavailable, title: "No session can be opened yet",
			detail: "this server holds no key to seal a session under yet; try again shortly",
		}
	}
	i := slices.IndexFunc(list, func(p apply.IdentityProvider) bool { return p.ID == pid })
	if i < 0 || list[i].Kind != apply.ProviderLDAP || !list[i].Enabled {
		return nil, keys, fmt.Errorf("%w: there is no directory %s to sign in through", store.ErrNotFound, pid)
	}
	return &list[i], keys, nil
}
