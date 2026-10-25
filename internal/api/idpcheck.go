package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// checkTimeout bounds a whole check. Each call inside it is bounded as a
// sign-in bounds it; this keeps a provider that answers slowly at every step
// from holding the request for all of them added up.
const checkTimeout = 30 * time.Second

// CheckIdentityProvider tries what a sign-in through a provider needs, short
// of a person signing in, and says what does not work.
func (s *Server) CheckIdentityProvider(
	ctx context.Context, req gen.CheckIdentityProviderRequestObject,
) (gen.CheckIdentityProviderResponseObject, error) {
	if err := requireAdmin(ctx, "checking an identity provider"); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, badRequest("say which provider to check: the id of a saved one, a draft, or both")
	}
	p, err := s.providerToCheck(ctx, req.Body)
	if err != nil {
		return nil, err
	}
	username := strings.TrimSpace(deref(req.Body.Username, ""))
	if username != "" && p.Kind != apply.ProviderLDAP {
		return nil, badRequest("only a directory can say who somebody is without them signing in; " +
			"an OpenID Connect provider says so at the end of a sign-in")
	}

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var c checker
	switch p.Kind {
	case apply.ProviderOIDC:
		checkOIDC(ctx, p, &c)
	case apply.ProviderLDAP:
		s.checkDirectory(ctx, p, username, &c)
	default:
		c.fail("Know the provider", "%s is a kind of provider this server does not know: %s", p.Name, p.Kind)
	}
	return gen.CheckIdentityProvider200JSONResponse(c.result()), nil
}

// providerToCheck is the provider a check is asked about: a saved one, or a
// draft, over a saved one when both are given.
func (s *Server) providerToCheck(ctx context.Context, in *gen.IdentityProviderCheckRequest) (*apply.IdentityProvider, error) {
	list, err := s.identityProviders(ctx)
	if err != nil {
		return nil, err
	}
	pid := deref(in.Id, "")
	if in.Provider != nil {
		draft, derr := providerFromAPI(in.Provider)
		if derr != nil {
			return nil, derr
		}
		return apply.DraftIdentityProvider(list, pid, draft)
	}
	if pid == "" {
		return nil, badRequest("say which provider to check: the id of a saved one, a draft, or both")
	}
	i := slices.IndexFunc(list, func(p apply.IdentityProvider) bool { return p.ID == pid })
	if i < 0 {
		return nil, fmt.Errorf("%w: there is no identity provider %s", store.ErrNotFound, pid)
	}
	return &list[i], nil
}

// checker collects the steps of a check.
type checker struct {
	steps  []gen.CheckStep
	person *gen.CheckedPerson
}

func (c *checker) add(status gen.CheckStepStatus, step, format string, args ...any) {
	c.steps = append(c.steps, gen.CheckStep{Step: step, Status: status, Detail: fmt.Sprintf(format, args...)})
}

func (c *checker) pass(step, format string, args ...any) {
	c.add(gen.CheckStepStatusOk, step, format, args...)
}
func (c *checker) fail(step, format string, args ...any) {
	c.add(gen.CheckStepStatusFailed, step, format, args...)
}
func (c *checker) unknown(step, format string, args ...any) {
	c.add(gen.CheckStepStatusUnknown, step, format, args...)
}

func (c *checker) result() gen.IdentityProviderCheck {
	ok := !slices.ContainsFunc(c.steps, func(s gen.CheckStep) bool { return s.Status == gen.CheckStepStatusFailed })
	return gen.IdentityProviderCheck{Ok: ok, Steps: c.steps, Person: c.person}
}

// discovery is the part of an OpenID Connect discovery document a sign-in
// relies on (OpenID Connect Discovery 1.0 §3).
type discovery struct {
	Issuer        string   `json:"issuer"`
	Authorization string   `json:"authorization_endpoint"`
	Token         string   `json:"token_endpoint"`
	Keys          string   `json:"jwks_uri"`
	Challenges    []string `json:"code_challenge_methods_supported"`
	Claims        []string `json:"claims_supported"`
}

// checkOIDC reads the provider's discovery document and holds it against what
// a sign-in here needs.
func checkOIDC(ctx context.Context, p *apply.IdentityProvider, c *checker) {
	o := p.OIDC
	hc, err := httpClientFor(p.CACertificate)
	if err != nil {
		c.fail("Read the CA certificate", "%v", err)
		return
	}

	const reach = "Reach the issuer"
	wellKnown := strings.TrimSuffix(o.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, http.NoBody)
	if err != nil {
		c.fail(reach, "%s is not an address: %v", wellKnown, err)
		return
	}
	resp, err := hc.Do(req)
	if err != nil {
		c.fail(reach, "%s: %s", wellKnown, unreachable(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.fail(reach, "%s answered %d. Is the issuer right? Authentik's ends in "+
			"/application/o/<slug>/, Keycloak's in /realms/<realm>, Entra's in /v2.0", wellKnown, resp.StatusCode)
		return
	}
	var doc discovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		c.fail(reach, "%s answered with something that is not a discovery document: %v", wellKnown, err)
		return
	}
	c.pass(reach, "%s answered with a discovery document", wellKnown)

	// Compared exactly, as the ID token's iss claim is (OpenID Connect Core
	// 1.0 §3.1.3.7): a trailing slash is a different issuer.
	if doc.Issuer != o.Issuer {
		c.fail("The issuer is the one configured", "the provider calls itself %s, and the ID tokens it "+
			"signs will say so; set that as the issuer", doc.Issuer)
		return
	}
	c.pass("The issuer is the one configured", "the provider calls itself %s", doc.Issuer)

	var missing []string
	for _, e := range []struct{ name, value string }{
		{"authorization endpoint", doc.Authorization}, {"token endpoint", doc.Token}, {"key set", doc.Keys},
	} {
		if e.value == "" {
			missing = append(missing, e.name)
		}
	}
	if len(missing) > 0 {
		c.fail("It offers what a sign-in uses", "the discovery document names no %s", strings.Join(missing, ", "))
		return
	}
	c.pass("It offers what a sign-in uses", "an authorization endpoint, a token endpoint and a key set")

	switch {
	case len(doc.Challenges) == 0:
		c.unknown("PKCE with S256", "the provider does not say which methods it supports; current ones support S256")
	case slices.Contains(doc.Challenges, "S256"):
		c.pass("PKCE with S256", "offered")
	default:
		c.fail("PKCE with S256", "the provider offers %s and not S256, which a sign-in here uses",
			strings.Join(doc.Challenges, ", "))
	}

	claim := "The " + o.GroupsClaim + " claim"
	switch {
	case len(doc.Claims) == 0:
		c.unknown(claim, "the provider does not list its claims; if a sign-in is refused for want of "+
			"groups, it does not send them in %s", o.GroupsClaim)
	case slices.Contains(doc.Claims, o.GroupsClaim):
		c.pass(claim, "listed among the claims the provider sends")
	default:
		c.unknown(claim, "the provider does not list it among its claims. Some send groups only when "+
			"a mapper or scope asks for them; if a sign-in is refused for want of groups, this is why")
	}

	if len(o.RedirectURIs) == 0 {
		c.fail("Redirect URIs", "none are listed, so no sign-in can come back")
	} else {
		c.pass("Redirect URIs", "%d listed. Each has to be registered with the provider as well, "+
			"which only a sign-in shows", len(o.RedirectURIs))
	}

	c.unknown("The client secret", "is only tried when somebody signs in; a wrong one shows there as "+
		"the provider not exchanging the code")
}

// checkDirectory connects to a directory as a sign-in would, and finds the
// person named, if one is.
func (s *Server) checkDirectory(ctx context.Context, p *apply.IdentityProvider, username string, c *checker) {
	l := p.LDAP
	const connect = "Connect over TLS"
	conn, err := s.dialLDAP(ctx, p)
	if err != nil {
		c.fail(connect, "%s: %s", l.URL, unreachable(err))
		return
	}
	defer conn.Close()
	how := "LDAPS"
	if l.StartTLS {
		how = "StartTLS"
	}
	c.pass(connect, "%s, by %s", l.URL, how)

	if l.BindDN != "" {
		bind := "Bind as " + l.BindDN
		if berr := conn.Bind(l.BindDN, l.BindPassword); berr != nil {
			if ldap.IsErrorWithCode(berr, ldap.LDAPResultInvalidCredentials) {
				c.fail(bind, "the directory refused the bind DN or its password")
			} else {
				c.fail(bind, "%v", berr)
			}
			return
		}
		c.pass(bind, "accepted")
	}

	exists := func(step, base string) bool {
		_, serr := conn.Search(ldap.NewSearchRequest(
			base, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, int(directoryTimeout.Seconds()), false,
			"(objectClass=*)", []string{"dn"}, nil))
		switch {
		case serr == nil:
			c.pass(step, "%s is there", base)
			return true
		case ldap.IsErrorWithCode(serr, ldap.LDAPResultNoSuchObject):
			c.fail(step, "%s does not exist", base)
		case ldap.IsErrorWithCode(serr, ldap.LDAPResultInsufficientAccessRights):
			c.fail(step, "%s may not be read by %s", base, searcher(l))
		default:
			c.fail(step, "%s: %v", base, serr)
		}
		return false
	}
	if !exists("The user base", l.UserBase) {
		return
	}
	if l.Groups == apply.GroupsSearch && !exists("The group base", l.GroupBase) {
		return
	}
	if username == "" {
		return
	}

	find := "Find " + username
	found, err := searchPerson(conn, l, username)
	switch {
	case err != nil:
		c.fail(find, "%v", err)
		return
	case len(found) == 0:
		c.fail(find, "the user filter finds nobody by that name under %s, searching as %s", l.UserBase, searcher(l))
		return
	case len(found) > 1:
		c.fail(find, "the user filter finds more than one entry, and a sign-in needs exactly one; "+
			"make the filter narrower")
		return
	}
	entry := found[0]
	c.pass(find, "%s", entry.DN)

	groups, err := personGroups(conn, l, entry, username)
	if err != nil {
		c.fail("Read their groups", "%v", err)
		return
	}
	name := entry.GetAttributeValue(l.NameAttribute)
	if name == "" {
		name = username
	}
	c.person = &gen.CheckedPerson{Dn: entry.DN, Name: name, Groups: groups}

	scope, ok := scopeOf(p, groupNames(groups))
	switch {
	case ok:
		c.person.Scope = ptr(gen.Scope(scope))
		c.pass("Their scope", "%s would sign in with %s", name, scope)
	case len(groups) == 0:
		c.fail("Their scope", "the directory says %s is in no group, so a sign-in is refused", name)
	default:
		c.fail("Their scope", "none of their groups is given a scope here, so a sign-in is refused. "+
			"They are in %s", strings.Join(shortNames(groups), ", "))
	}
}

// searcher is who a directory's searches run as, for a sentence.
func searcher(l *apply.LDAPProvider) string {
	if l.BindDN == "" {
		return "nobody (an anonymous search)"
	}
	return l.BindDN
}

// shortNames is each group by the value of its first RDN, which is what a
// mapping usually names it by.
func shortNames(dns []string) []string {
	out := make([]string, 0, len(dns))
	for _, raw := range dns {
		name := raw
		if dn, err := ldap.ParseDN(raw); err == nil && len(dn.RDNs) > 0 && len(dn.RDNs[0].Attributes) > 0 {
			name = dn.RDNs[0].Attributes[0].Value
		}
		out = append(out, name)
	}
	return out
}

// unreachable is a failure to reach a provider, said so that the usual cause
// of each is named.
func unreachable(err error) string {
	var unknownCA x509.UnknownAuthorityError
	var verify *tls.CertificateVerificationError
	var hostname x509.HostnameError
	switch {
	case errors.As(err, &hostname):
		return "the certificate is not for this name; use the name it was issued for: " + err.Error()
	case errors.As(err, &unknownCA), errors.As(err, &verify):
		return "the certificate is not one this server trusts; put the CA that signed it under Trust: " + err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer in time"
	}
	return err.Error()
}
