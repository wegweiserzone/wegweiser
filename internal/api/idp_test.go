package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
)

func authentikInput() gen.IdentityProviderInput {
	return gen.IdentityProviderInput{
		Name: "Authentik", Kind: gen.IdentityProviderKindOidc,
		Mappings: &[]gen.GroupMapping{{Group: "dns-admins", Scope: gen.ScopeAdmin}},
		Oidc: &gen.OIDCSettings{
			Issuer:       "https://auth.example.com/application/o/wegweiser/",
			ClientId:     "wegweiser",
			ClientSecret: ptr("s3cret"),
			RedirectUris: []string{"https://ns1.example.com/api/v1/auth/oidc/callback"},
		},
	}
}

func freeIPAInput() gen.IdentityProviderInput {
	return gen.IdentityProviderInput{
		Name: "FreeIPA", Kind: gen.IdentityProviderKindLdap,
		Mappings: &[]gen.GroupMapping{{Group: "cn=admins,cn=groups,cn=accounts,dc=example,dc=com", Scope: gen.ScopeAdmin}},
		Ldap: &gen.LDAPSettings{
			Url:          "ldaps://ipa.example.com",
			BindDn:       ptr("uid=wegweiser,cn=sysaccounts,cn=etc,dc=example,dc=com"),
			BindPassword: ptr("hunter2"),
			UserBase:     "cn=users,cn=accounts,dc=example,dc=com",
			UserFilter:   "(uid={username})",
		},
	}
}

func TestAnIdentityProviderIsManagedAndItsSecretNeverShown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	var oidc gen.IdentityProvider
	h.decode(h.do(http.MethodPost, "/identity-providers", authentikInput()), http.StatusCreated, &oidc)
	if !oidc.Enabled || oidc.Oidc == nil || *oidc.Oidc.GroupsClaim != "groups" {
		t.Errorf("made %+v, want it enabled, with the defaults filled in", oidc)
	}
	var ldap gen.IdentityProvider
	h.decode(h.do(http.MethodPost, "/identity-providers", freeIPAInput()), http.StatusCreated, &ldap)

	for _, path := range []string{"/identity-providers", "/identity-providers/" + oidc.Id, "/identity-providers/" + ldap.Id} {
		resp := h.do(http.MethodGet, path, nil)
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d, %v", path, resp.StatusCode, err)
		}
		for _, secret := range []string{"s3cret", "hunter2", "clientSecret", "bindPassword"} {
			if strings.Contains(string(body), secret) {
				t.Errorf("GET %s shows %q: %s", path, secret, body)
			}
		}
	}

	changed := authentikInput()
	changed.Oidc.ClientSecret = nil
	changed.Enabled = ptr(false)
	var got gen.IdentityProvider
	h.decode(h.do(http.MethodPut, "/identity-providers/"+oidc.Id, changed), http.StatusOK, &got)
	if got.Enabled || got.Id != oidc.Id {
		t.Errorf("updated %+v, want the same provider, switched off", got)
	}

	h.decode(h.do(http.MethodDelete, "/identity-providers/"+oidc.Id, nil), http.StatusNoContent, nil)
	h.decode(h.do(http.MethodGet, "/identity-providers/"+oidc.Id, nil), http.StatusNotFound, nil)
	var left []gen.IdentityProvider
	h.decode(h.do(http.MethodGet, "/identity-providers", nil), http.StatusOK, &left)
	if len(left) != 1 || left[0].Id != ldap.Id {
		t.Errorf("listed %+v after the deletion, want the directory alone", left)
	}
}

func TestAnIdentityProviderIsRefusedWhatTheAPIDoesNotSpeak(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	elsewhere := authentikInput()
	elsewhere.Oidc.RedirectUris = []string{"https://ns1.example.com/somewhere/else"}
	h.decode(h.do(http.MethodPost, "/identity-providers", elsewhere), http.StatusBadRequest, nil)

	noScope := authentikInput()
	noScope.Mappings = &[]gen.GroupMapping{{Group: "dns-admins", Scope: "root"}}
	h.decode(h.do(http.MethodPost, "/identity-providers", noScope), http.StatusBadRequest, nil)

	// Understood, and refused for what it would do, as every value the
	// applier turns down is.
	inTheClear := freeIPAInput()
	inTheClear.Ldap.Url = "ldap://ipa.example.com"
	h.decode(h.do(http.MethodPost, "/identity-providers", inTheClear), http.StatusUnprocessableEntity, nil)
}

func TestIdentityProvidersNeedTheAdminScope(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A mapping is how an account becomes an administrator, so whoever may
	// write one may make themselves one.
	var minted gen.TokenCreated
	h.decode(h.do(http.MethodPost, "/tokens", gen.CreateToken{
		Name: "writer", Scopes: []gen.Scope{gen.ScopeWrite},
	}), http.StatusCreated, &minted)
	as := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+minted.Secret) }

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/identity-providers"},
		{http.MethodPost, "/identity-providers"},
		{http.MethodGet, "/identity-providers/01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		{http.MethodPut, "/identity-providers/01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		{http.MethodDelete, "/identity-providers/01ARZ3NDEKTSV4RRFFQ69G5FAV"},
	} {
		resp := h.do(tc.method, tc.path, authentikInput(), as)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s is %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
	}
}
