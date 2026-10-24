package apply

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// IdentityProviderSetting is the key the identity providers are stored under
// (docs/decisions/d51-accounts-come-from-an-identity-provider.md).
//
// A setting, like the transfer list, because every member has to offer the
// same providers and map their groups the same way, and settings are what a
// batch carries. The client secret and the bind password are held here as a
// TSIG secret is held in its table (D28): the database and its backups are the
// controls.
const IdentityProviderSetting = "identity_providers"

// idpLock is the key a change to the providers serializes on. The list is
// read and written whole.
const idpLock = "identity providers"

// ProviderKind is the protocol a provider is spoken to in.
type ProviderKind string

const (
	// ProviderOIDC is OpenID Connect, by the authorization code flow with PKCE.
	ProviderOIDC ProviderKind = "oidc"
	// ProviderLDAP is a directory a person binds to as themselves.
	ProviderLDAP ProviderKind = "ldap"
)

// LDAPGroups is where a directory says which groups a person is in.
type LDAPGroups string

const (
	// GroupsMemberOf reads the memberOf attribute of the person's entry, which
	// FreeIPA, Active Directory and OpenLDAP with the memberof overlay keep.
	GroupsMemberOf LDAPGroups = "memberOf"
	// GroupsSearch searches for the groups that name the person.
	GroupsSearch LDAPGroups = "search"
)

// The placeholders a filter is written with. A value put in their place is
// escaped for the filter it lands in (RFC 4515 §3).
const (
	PlaceholderUsername = "{username}"
	PlaceholderDN       = "{dn}"
)

// GroupMapping grants the members of one group a scope.
type GroupMapping struct {
	// Group is the group as the provider names it: a value of the groups
	// claim, or for a directory the group's DN or the value of its first RDN.
	Group string `json:"group"`
	// Scope is a token scope. Which ones exist is the API's vocabulary.
	Scope string `json:"scope"`
}

// OIDCProvider is how an OpenID Connect provider is reached.
type OIDCProvider struct {
	// Issuer is the provider's issuer identifier; its discovery document is
	// below it (OpenID Connect Discovery 1.0 §4).
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	// Scopes are asked for at sign-in. openid is always among them.
	Scopes []string `json:"scopes"`
	// GroupsClaim and NameClaim are the claims a person's groups and name are
	// read from.
	GroupsClaim string `json:"groups_claim"`
	NameClaim   string `json:"name_claim"`
	// RedirectURIs are the callbacks registered with the provider, one for
	// each address a member is reached by. A sign-in returns only to one of
	// them.
	RedirectURIs []string `json:"redirect_uris"`
}

// LDAPProvider is how a directory is reached and searched.
type LDAPProvider struct {
	// URL is ldaps://host[:port], or ldap://host[:port] with StartTLS. A bind
	// in the clear is never made.
	URL      string `json:"url"`
	StartTLS bool   `json:"start_tls"`
	// BindDN and BindPassword are what the person's entry is searched for
	// with. Empty is an anonymous search.
	BindDN       string `json:"bind_dn,omitempty"`
	BindPassword string `json:"bind_password,omitempty"`
	// UserBase and UserFilter find the person's entry, the filter holding
	// {username}. NameAttribute is what they are called in a commit.
	UserBase      string `json:"user_base"`
	UserFilter    string `json:"user_filter"`
	NameAttribute string `json:"name_attribute"`
	// Groups says where the person's groups are read from; a search needs
	// GroupBase and a GroupFilter holding {dn} or {username}.
	Groups      LDAPGroups `json:"groups"`
	GroupBase   string     `json:"group_base,omitempty"`
	GroupFilter string     `json:"group_filter,omitempty"`
}

// IdentityProvider is one place people sign in through.
type IdentityProvider struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Kind    ProviderKind `json:"kind"`
	Enabled bool         `json:"enabled"`

	Mappings []GroupMapping `json:"mappings"`

	// CACertificate is PEM, for a provider whose certificate an internal CA
	// signed. Empty trusts what the system trusts.
	CACertificate string `json:"ca_certificate,omitempty"`

	// Exactly one of these is set, the one Kind names.
	OIDC *OIDCProvider `json:"oidc,omitempty"`
	LDAP *LDAPProvider `json:"ldap,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StoredIdentityProviders returns the providers the database holds, in the
// order they were made.
func StoredIdentityProviders(ctx context.Context, r store.Reader) ([]IdentityProvider, error) {
	raw, err := r.Setting(ctx, IdentityProviderSetting)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []IdentityProvider
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return nil, fmt.Errorf("apply: the stored identity providers are not readable: %w", uerr)
	}
	return out, nil
}

// CreateIdentityProvider adds a provider, minting its identifier and filling
// in the defaults it left out.
func (a *Applier) CreateIdentityProvider(ctx context.Context, p IdentityProvider) (*IdentityProvider, error) {
	var made IdentityProvider
	err := a.changeProviders(ctx, func(list []IdentityProvider, now time.Time) ([]IdentityProvider, error) {
		p.ID = id.New()
		p.CreatedAt, p.UpdatedAt = now, now
		p.withDefaults()
		if err := p.validate(list); err != nil {
			return nil, err
		}
		made = p
		return append(list, p), nil
	})
	if err != nil {
		return nil, err
	}
	return &made, nil
}

// UpdateIdentityProvider replaces what a provider says about itself. A secret
// left empty is kept, so that changing a mapping does not mean typing the
// client secret again; the kind cannot change.
func (a *Applier) UpdateIdentityProvider(
	ctx context.Context, pid string, p IdentityProvider,
) (*IdentityProvider, error) {
	var made IdentityProvider
	err := a.changeProviders(ctx, func(list []IdentityProvider, now time.Time) ([]IdentityProvider, error) {
		i := slices.IndexFunc(list, func(q IdentityProvider) bool { return q.ID == pid })
		if i < 0 {
			return nil, fmt.Errorf("%w: no identity provider %s", store.ErrNotFound, pid)
		}
		old := list[i]
		if p.Kind != old.Kind {
			return nil, fmt.Errorf("%w: a provider stays the kind it was made as; "+
				"remove it and make another", zone.ErrInvalid)
		}
		p.ID, p.CreatedAt, p.UpdatedAt = old.ID, old.CreatedAt, now
		if p.OIDC != nil && old.OIDC != nil && p.OIDC.ClientSecret == "" {
			p.OIDC.ClientSecret = old.OIDC.ClientSecret
		}
		if p.LDAP != nil && old.LDAP != nil && p.LDAP.BindPassword == "" && p.LDAP.BindDN == old.LDAP.BindDN {
			p.LDAP.BindPassword = old.LDAP.BindPassword
		}
		p.withDefaults()
		others := slices.Delete(slices.Clone(list), i, i+1)
		if err := p.validate(others); err != nil {
			return nil, err
		}
		made = p
		list[i] = p
		return list, nil
	})
	if err != nil {
		return nil, err
	}
	return &made, nil
}

// DeleteIdentityProvider removes a provider. Nobody signs in through it
// afterwards.
func (a *Applier) DeleteIdentityProvider(ctx context.Context, pid string) error {
	return a.changeProviders(ctx, func(list []IdentityProvider, _ time.Time) ([]IdentityProvider, error) {
		i := slices.IndexFunc(list, func(q IdentityProvider) bool { return q.ID == pid })
		if i < 0 {
			return nil, fmt.Errorf("%w: no identity provider %s", store.ErrNotFound, pid)
		}
		return slices.Delete(list, i, i+1), nil
	})
}

// changeProviders reads the list, has change work out the next one, and writes
// that. What reaches every node is the finished list, decided here (D24).
func (a *Applier) changeProviders(
	ctx context.Context, change func([]IdentityProvider, time.Time) ([]IdentityProvider, error),
) error {
	ctx, done, serr := a.settle(ctx)
	if serr != nil {
		return serr
	}
	defer done()
	unlock := a.locks.lock(idpLock)
	defer unlock()

	var list []IdentityProvider
	if err := a.store.View(ctx, func(r store.Reader) error {
		var verr error
		list, verr = StoredIdentityProviders(ctx, r)
		return verr
	}); err != nil {
		return err
	}
	next, err := change(list, a.now().UTC())
	if err != nil {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("apply: encode the identity providers: %w", err)
	}
	b, err := a.PlanSettings([]SettingChange{{Key: IdentityProviderSetting, Value: raw}})
	if err != nil {
		return err
	}
	return a.submit(ctx, b)
}

// withDefaults fills in what a provider may leave out.
func (p *IdentityProvider) withDefaults() {
	if p.Mappings == nil {
		p.Mappings = []GroupMapping{}
	}
	if o := p.OIDC; o != nil {
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"openid", "profile", "email", "groups"}
		}
		if !slices.Contains(o.Scopes, "openid") {
			o.Scopes = append([]string{"openid"}, o.Scopes...)
		}
		if o.GroupsClaim == "" {
			o.GroupsClaim = "groups"
		}
		if o.NameClaim == "" {
			o.NameClaim = "preferred_username"
		}
	}
	if l := p.LDAP; l != nil {
		if l.NameAttribute == "" {
			l.NameAttribute = "uid"
		}
		if l.Groups == "" {
			l.Groups = GroupsMemberOf
		}
	}
}

// validate refuses a provider that could never sign anybody in, or one that
// would sign them in insecurely. others are the providers beside it.
func (p *IdentityProvider) validate(others []IdentityProvider) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{zone.ErrInvalid}, args...)...)
	}
	if strings.TrimSpace(p.Name) == "" {
		return invalid("a provider needs a name, which is what the sign-in page offers it as")
	}
	for i := range others {
		if strings.EqualFold(others[i].Name, p.Name) {
			return fmt.Errorf("%w: there is a provider called %q already", store.ErrConflict, others[i].Name)
		}
	}
	for _, m := range p.Mappings {
		if strings.TrimSpace(m.Group) == "" {
			return invalid("a mapping names the group it is for")
		}
	}
	if p.CACertificate != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(p.CACertificate)) {
		return invalid("the CA certificate holds no certificate in PEM")
	}

	switch p.Kind {
	case ProviderOIDC:
		if p.OIDC == nil || p.LDAP != nil {
			return invalid("an OpenID Connect provider carries its oidc settings and no others")
		}
		return p.OIDC.validate(invalid)
	case ProviderLDAP:
		if p.LDAP == nil || p.OIDC != nil {
			return invalid("a directory carries its ldap settings and no others")
		}
		return p.LDAP.validate(invalid)
	default:
		return invalid("%q is not a kind of provider; oidc and ldap are", p.Kind)
	}
}

func (o *OIDCProvider) validate(invalid func(string, ...any) error) error {
	if err := secureURL(o.Issuer, "https"); err != nil {
		return invalid("the issuer: %v", err)
	}
	if o.ClientID == "" {
		return invalid("an OpenID Connect provider needs the client ID it was registered under")
	}
	if o.ClientSecret == "" {
		return invalid("an OpenID Connect provider needs the client secret it was registered with")
	}
	if len(o.RedirectURIs) == 0 {
		return invalid("name the redirect URIs registered with the provider, one for each address " +
			"a member is reached by, or nobody can return from signing in")
	}
	for _, u := range o.RedirectURIs {
		if err := secureURL(u, "https"); err != nil {
			return invalid("the redirect URI %q: %v", u, err)
		}
	}
	return nil
}

func (l *LDAPProvider) validate(invalid func(string, ...any) error) error {
	u, err := url.Parse(l.URL)
	if err != nil || u.Host == "" {
		return invalid("the directory is named by a URL such as ldaps://ipa.example.com, and %q is not one", l.URL)
	}
	switch {
	case u.Scheme == "ldaps" && l.StartTLS:
		return invalid("ldaps:// is TLS already; StartTLS is for ldap://")
	case u.Scheme == "ldap" && !l.StartTLS:
		return invalid("a password would cross the network in the clear; " +
			"use ldaps://, or ldap:// with StartTLS")
	case u.Scheme != "ldaps" && u.Scheme != "ldap":
		return invalid("%q is not a directory URL; it starts ldaps:// or ldap://", l.URL)
	}
	if l.BindDN != "" && l.BindPassword == "" {
		return invalid("a bind DN needs its password, which a changed bind DN does not keep; " +
			"leave both out to search anonymously")
	}
	if l.UserBase == "" {
		return invalid("name the base people are searched for under")
	}
	if !strings.Contains(l.UserFilter, PlaceholderUsername) {
		return invalid("the user filter has to hold %s, such as (uid=%s)", PlaceholderUsername, PlaceholderUsername)
	}
	switch l.Groups {
	case GroupsMemberOf:
	case GroupsSearch:
		if l.GroupBase == "" {
			return invalid("a group search needs the base groups are searched for under")
		}
		if !strings.Contains(l.GroupFilter, PlaceholderDN) && !strings.Contains(l.GroupFilter, PlaceholderUsername) {
			return invalid("the group filter has to hold %s or %s, such as (member=%s)",
				PlaceholderDN, PlaceholderUsername, PlaceholderDN)
		}
	default:
		return invalid("groups are read from %s or found by a %s, and %q is neither",
			GroupsMemberOf, GroupsSearch, l.Groups)
	}
	return nil
}

// secureURL accepts an absolute URL of the scheme given, and plain http for a
// host on loopback, where nothing crosses the network.
func secureURL(raw, scheme string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("%q is not an absolute URL", raw)
	}
	if u.Scheme == scheme {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("%q is not %s", raw, scheme)
}
