package apply_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func authentik() apply.IdentityProvider {
	return apply.IdentityProvider{
		Name: "Authentik", Kind: apply.ProviderOIDC, Enabled: true,
		Mappings: []apply.GroupMapping{{Group: "dns-admins", Scope: "admin"}},
		OIDC: &apply.OIDCProvider{
			Issuer:       "https://auth.example.com/application/o/wegweiser/",
			ClientID:     "wegweiser",
			ClientSecret: "s3cret",
			RedirectURIs: []string{"https://ns1.example.com/api/v1/auth/oidc/callback"},
		},
	}
}

func freeIPA() apply.IdentityProvider {
	return apply.IdentityProvider{
		Name: "FreeIPA", Kind: apply.ProviderLDAP, Enabled: true,
		LDAP: &apply.LDAPProvider{
			URL:          "ldaps://ipa.example.com",
			BindDN:       "uid=wegweiser,cn=sysaccounts,cn=etc,dc=example,dc=com",
			BindPassword: "hunter2",
			UserBase:     "cn=users,cn=accounts,dc=example,dc=com",
			UserFilter:   "(uid={username})",
		},
	}
}

func storedProviders(t *testing.T, f *fixture) []apply.IdentityProvider {
	t.Helper()
	var got []apply.IdentityProvider
	if err := f.s.View(t.Context(), func(r store.Reader) error {
		var verr error
		got, verr = apply.StoredIdentityProviders(t.Context(), r)
		return verr
	}); err != nil {
		t.Fatalf("StoredIdentityProviders: %v", err)
	}
	return got
}

func TestAnIdentityProviderIsMadeWithItsDefaults(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	made, err := f.a.CreateIdentityProvider(t.Context(), authentik())
	if err != nil {
		t.Fatalf("CreateIdentityProvider: %v", err)
	}
	if made.ID == "" || made.CreatedAt.IsZero() {
		t.Errorf("made %+v, want an identifier and a time", made)
	}
	o := made.OIDC
	if o.GroupsClaim != "groups" || o.NameClaim != "preferred_username" || len(o.Scopes) == 0 || o.Scopes[0] != "openid" {
		t.Errorf("oidc = %+v, want the defaults filled in", o)
	}

	ldap, err := f.a.CreateIdentityProvider(t.Context(), freeIPA())
	if err != nil {
		t.Fatalf("CreateIdentityProvider: %v", err)
	}
	if ldap.LDAP.Groups != apply.GroupsMemberOf || ldap.LDAP.NameAttribute != "uid" {
		t.Errorf("ldap = %+v, want memberOf and uid", ldap.LDAP)
	}
	if got := storedProviders(t, f); len(got) != 2 || got[0].Name != "Authentik" || got[1].Name != "FreeIPA" {
		t.Errorf("stored %+v, want both, in the order they were made", got)
	}

	other := freeIPA()
	other.Name = "freeipa"
	if _, err := f.a.CreateIdentityProvider(t.Context(), other); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a second provider by the same name gave %v, want a conflict", err)
	}
}

func TestAnIdentityProviderThatCouldNotSignAnybodyInIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*apply.IdentityProvider)
	}{
		{"no name", func(p *apply.IdentityProvider) { p.Name = " " }},
		{"no such kind", func(p *apply.IdentityProvider) { p.Kind = "saml" }},
		{"the other kind's settings", func(p *apply.IdentityProvider) { p.LDAP = freeIPA().LDAP }},
		{"an issuer over plain HTTP", func(p *apply.IdentityProvider) { p.OIDC.Issuer = "http://auth.example.com/" }},
		{"no client secret", func(p *apply.IdentityProvider) { p.OIDC.ClientSecret = "" }},
		{"no redirect URI", func(p *apply.IdentityProvider) { p.OIDC.RedirectURIs = nil }},
		{"a mapping for no group", func(p *apply.IdentityProvider) { p.Mappings = []apply.GroupMapping{{Scope: "read"}} }},
		{"a CA certificate that is not one", func(p *apply.IdentityProvider) { p.CACertificate = "nope" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			p := authentik()
			tc.change(&p)
			if _, err := f.a.CreateIdentityProvider(t.Context(), p); !errors.Is(err, zone.ErrInvalid) {
				t.Errorf("gave %v, want an invalid-value error", err)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		change func(*apply.LDAPProvider)
	}{
		{"a bind in the clear", func(l *apply.LDAPProvider) { l.URL = "ldap://ipa.example.com" }},
		{"StartTLS over TLS", func(l *apply.LDAPProvider) { l.StartTLS = true }},
		{"a bind DN without its password", func(l *apply.LDAPProvider) { l.BindPassword = "" }},
		{"a filter that cannot find anybody", func(l *apply.LDAPProvider) { l.UserFilter = "(uid=alice)" }},
		{"a group search with nowhere to look", func(l *apply.LDAPProvider) {
			l.Groups, l.GroupFilter = apply.GroupsSearch, "(member={dn})"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			p := freeIPA()
			tc.change(p.LDAP)
			if _, err := f.a.CreateIdentityProvider(t.Context(), p); !errors.Is(err, zone.ErrInvalid) {
				t.Errorf("gave %v, want an invalid-value error", err)
			}
		})
	}

	t.Run("StartTLS on ldap:// is not in the clear", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		p := freeIPA()
		p.LDAP.URL, p.LDAP.StartTLS = "ldap://ipa.example.com", true
		if _, err := f.a.CreateIdentityProvider(t.Context(), p); err != nil {
			t.Errorf("CreateIdentityProvider: %v", err)
		}
	})
}

func TestUpdatingAnIdentityProviderKeepsItsSecret(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	oidc, err := f.a.CreateIdentityProvider(t.Context(), authentik())
	if err != nil {
		t.Fatalf("CreateIdentityProvider: %v", err)
	}
	ldap, err := f.a.CreateIdentityProvider(t.Context(), freeIPA())
	if err != nil {
		t.Fatalf("CreateIdentityProvider: %v", err)
	}

	changed := authentik()
	changed.OIDC.ClientSecret = ""
	changed.Mappings = append(changed.Mappings, apply.GroupMapping{Group: "dns-readers", Scope: "read"})
	got, err := f.a.UpdateIdentityProvider(t.Context(), oidc.ID, changed)
	if err != nil {
		t.Fatalf("UpdateIdentityProvider: %v", err)
	}
	if got.OIDC.ClientSecret != "s3cret" || len(got.Mappings) != 2 || got.ID != oidc.ID || !got.CreatedAt.Equal(oidc.CreatedAt) {
		t.Errorf("updated %+v, want the secret, identifier and creation time kept", got)
	}

	sameDN := freeIPA()
	sameDN.LDAP.BindPassword = ""
	if got, err := f.a.UpdateIdentityProvider(t.Context(), ldap.ID, sameDN); err != nil || got.LDAP.BindPassword != "hunter2" {
		t.Errorf("an update leaving the password out gave %+v, %v; want it kept", got, err)
	}
	otherDN := freeIPA()
	otherDN.LDAP.BindDN, otherDN.LDAP.BindPassword = "uid=someone-else,dc=example,dc=com", ""
	if _, err := f.a.UpdateIdentityProvider(t.Context(), ldap.ID, otherDN); !errors.Is(err, zone.ErrInvalid) {
		t.Errorf("a new bind DN without its password gave %v, want it refused", err)
	}

	if _, err := f.a.UpdateIdentityProvider(t.Context(), oidc.ID, freeIPA()); err == nil ||
		!strings.Contains(err.Error(), "kind") {
		t.Errorf("changing the kind gave %v, want it refused", err)
	}
	if _, err := f.a.UpdateIdentityProvider(t.Context(), "01ARZ3NDEKTSV4RRFFQ69G5FAV", authentik()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("updating no provider gave %v, want not found", err)
	}

	if err := f.a.DeleteIdentityProvider(t.Context(), oidc.ID); err != nil {
		t.Fatalf("DeleteIdentityProvider: %v", err)
	}
	if got := storedProviders(t, f); len(got) != 1 || got[0].ID != ldap.ID {
		t.Errorf("stored %+v after the deletion, want the directory alone", got)
	}
	if err := f.a.DeleteIdentityProvider(t.Context(), oidc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting it twice gave %v, want not found", err)
	}
}
