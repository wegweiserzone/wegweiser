package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
)

// check asks the server to check a provider, as somebody named or nobody.
func (h *harness) check(t *testing.T, id, username string) gen.IdentityProviderCheck {
	t.Helper()
	body := gen.IdentityProviderCheckRequest{Id: &id}
	if username != "" {
		body.Username = &username
	}
	return h.checkWith(t, body)
}

// checkWith asks for a check as the request says.
func (h *harness) checkWith(t *testing.T, body gen.IdentityProviderCheckRequest) gen.IdentityProviderCheck {
	t.Helper()
	var got gen.IdentityProviderCheck
	h.decode(h.do(http.MethodPost, "/identity-providers/check", body), http.StatusOK, &got)
	return got
}

// statuses is each step as "step: status", for a failure message that shows
// the whole run.
func statuses(c gen.IdentityProviderCheck) string {
	parts := make([]string, 0, len(c.Steps))
	for _, s := range c.Steps {
		parts = append(parts, s.Step+": "+string(s.Status)+" ("+s.Detail+")")
	}
	return strings.Join(parts, "; ")
}

// last is the step a check ended on.
func last(c gen.IdentityProviderCheck) gen.CheckStep {
	if len(c.Steps) == 0 {
		return gen.CheckStep{}
	}
	return c.Steps[len(c.Steps)-1]
}

func TestADirectoryIsChecked(t *testing.T) {
	t.Parallel()

	t.Run("it is reached, bound to, and its bases are there", func(t *testing.T) {
		t.Parallel()
		h, _, p := directoryHarness(t, string(apply.GroupsSearch),
			gen.GroupMapping{Group: "admins", Scope: gen.ScopeAdmin})
		got := h.check(t, p.Id, "")
		if !got.Ok || len(got.Steps) != 4 || got.Person != nil {
			t.Errorf("check = %s, want connect, bind and both bases, and nobody looked up", statuses(got))
		}
	})

	t.Run("somebody named is found, with their groups and scope", func(t *testing.T) {
		t.Parallel()
		h, d, p := directoryHarness(t, string(apply.GroupsMemberOf),
			gen.GroupMapping{Group: "admins", Scope: gen.ScopeAdmin})
		got := h.check(t, p.Id, "alice")
		if !got.Ok || got.Person == nil || got.Person.Scope == nil || *got.Person.Scope != gen.ScopeAdmin ||
			got.Person.Dn != "uid=alice,"+usersBase || len(got.Person.Groups) != 2 {
			t.Fatalf("check = %s, person %+v; want alice, in two groups, with admin", statuses(got), got.Person)
		}
		// Without a password: nobody but the service account was bound as.
		for _, dn := range d.bound() {
			if dn != serviceDN {
				t.Errorf("bound as %s, want the service account alone", dn)
			}
		}
	})

	t.Run("somebody in no mapped group is told which groups they are in", func(t *testing.T) {
		t.Parallel()
		h, _, p := directoryHarness(t, string(apply.GroupsMemberOf),
			gen.GroupMapping{Group: "admins", Scope: gen.ScopeAdmin})
		got := h.check(t, p.Id, "bob")
		if step := last(got); got.Ok || step.Status != gen.CheckStepStatusFailed || !strings.Contains(step.Detail, "staff") {
			t.Errorf("check = %s, want a refusal naming staff", statuses(got))
		}
		if got.Person == nil || got.Person.Scope != nil {
			t.Errorf("person = %+v, want bob without a scope", got.Person)
		}
	})

	t.Run("somebody the filter does not find", func(t *testing.T) {
		t.Parallel()
		h, _, p := directoryHarness(t, string(apply.GroupsMemberOf))
		got := h.check(t, p.Id, "carol")
		if step := last(got); got.Ok || !strings.Contains(step.Detail, "finds nobody") {
			t.Errorf("check = %s, want it said the filter found nobody", statuses(got))
		}
	})

	t.Run("a wrong bind password stops it there", func(t *testing.T) {
		t.Parallel()
		h, _, p := directoryHarness(t, string(apply.GroupsMemberOf))
		in := inputOfProvider(p)
		in.Ldap.BindPassword = ptr("wrong")
		h.decode(h.do(http.MethodPut, "/identity-providers/"+p.Id, in), http.StatusOK, nil)
		got := h.check(t, p.Id, "alice")
		if step := last(got); got.Ok || !strings.HasPrefix(step.Step, "Bind as") || got.Person != nil {
			t.Errorf("check = %s, want it to stop at the bind", statuses(got))
		}
	})

	t.Run("a user base that is not there", func(t *testing.T) {
		t.Parallel()
		h, _, p := directoryHarness(t, string(apply.GroupsMemberOf))
		in := inputOfProvider(p)
		in.Ldap.UserBase = "cn=people,dc=example,dc=com"
		h.decode(h.do(http.MethodPut, "/identity-providers/"+p.Id, in), http.StatusOK, nil)
		got := h.check(t, p.Id, "")
		if step := last(got); got.Ok || !strings.Contains(step.Detail, "does not exist") {
			t.Errorf("check = %s, want the missing base named", statuses(got))
		}
	})
}

// A draft is tried before it is saved: on its own, or over a saved provider
// whose secrets it keeps where it leaves them out.
func TestADraftIsChecked(t *testing.T) {
	t.Parallel()

	t.Run("a new one, not yet named", func(t *testing.T) {
		t.Parallel()
		h, _, p := directoryHarness(t, string(apply.GroupsMemberOf),
			gen.GroupMapping{Group: "admins", Scope: gen.ScopeAdmin})
		draft := inputOfProvider(p)
		draft.Name = ""
		draft.Ldap.BindPassword = ptr("service")
		got := h.checkWith(t, gen.IdentityProviderCheckRequest{Provider: &draft, Username: ptr("alice")})
		if !got.Ok || got.Person == nil || got.Person.Scope == nil || *got.Person.Scope != gen.ScopeAdmin {
			t.Errorf("check = %s, want alice found with admin", statuses(got))
		}
	})

	t.Run("over a saved one, keeping its bind password", func(t *testing.T) {
		t.Parallel()
		h, _, p := directoryHarness(t, string(apply.GroupsMemberOf))
		draft := inputOfProvider(p)
		draft.Ldap.UserFilter = "(cn={username})"
		got := h.checkWith(t, gen.IdentityProviderCheckRequest{Id: &p.Id, Provider: &draft, Username: ptr("alice")})
		// The bind works, because the saved password was used, and the
		// filter the draft changed is the one that finds nobody.
		if step := last(got); got.Ok || !strings.Contains(step.Detail, "finds nobody") {
			t.Errorf("check = %s, want the draft's filter to find nobody", statuses(got))
		}

		// And nothing was saved.
		var stored gen.IdentityProvider
		h.decode(h.do(http.MethodGet, "/identity-providers/"+p.Id, nil), http.StatusOK, &stored)
		if stored.Ldap.UserFilter != "(uid={username})" {
			t.Errorf("filter = %q after a check, want the saved one untouched", stored.Ldap.UserFilter)
		}
	})

	t.Run("a draft that could never work is refused as it would be saved", func(t *testing.T) {
		t.Parallel()
		h, _, p := directoryHarness(t, string(apply.GroupsMemberOf))
		draft := inputOfProvider(p)
		draft.Ldap.Url = "ldap://ipa.example.com"
		resp := h.do(http.MethodPost, "/identity-providers/check", gen.IdentityProviderCheckRequest{Id: &p.Id, Provider: &draft})
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want the refusal saving it would meet", resp.StatusCode)
		}
	})

	t.Run("neither an id nor a draft", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		resp := h.do(http.MethodPost, "/identity-providers/check", gen.IdentityProviderCheckRequest{})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestAnOpenIDConnectProviderIsChecked(t *testing.T) {
	t.Parallel()

	t.Run("its discovery document is what a sign-in needs", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		p := addOIDCProvider(t, h, newTestIDP(t))
		got := h.check(t, p.Id, "")
		if !got.Ok {
			t.Fatalf("check = %s, want it to pass", statuses(got))
		}
		// The secret cannot be tried without a person, and it says so.
		if step := last(got); step.Status != gen.CheckStepStatusUnknown || !strings.Contains(step.Step, "secret") {
			t.Errorf("last step = %+v, want the client secret, unknown", step)
		}
	})

	t.Run("an issuer that is not quite the one it names", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		idp := newTestIDP(t)
		p := addOIDCProvider(t, h, idp)
		in := inputOfProvider(p)
		in.Oidc.Issuer = idp.srv.URL + "/"
		h.decode(h.do(http.MethodPut, "/identity-providers/"+p.Id, in), http.StatusOK, nil)
		got := h.check(t, p.Id, "")
		if step := last(got); got.Ok || !strings.Contains(step.Detail, "calls itself "+idp.srv.URL) {
			t.Errorf("check = %s, want the issuer the provider gives", statuses(got))
		}
	})

	t.Run("a certificate nobody said to trust", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		p := addOIDCProvider(t, h, newTestIDP(t))
		in := inputOfProvider(p)
		in.CaCertificate = nil
		h.decode(h.do(http.MethodPut, "/identity-providers/"+p.Id, in), http.StatusOK, nil)
		got := h.check(t, p.Id, "")
		if step := last(got); got.Ok || !strings.Contains(step.Detail, "under Trust") {
			t.Errorf("check = %s, want it told to add the CA", statuses(got))
		}
	})

	t.Run("a username is refused", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		p := addOIDCProvider(t, h, newTestIDP(t))
		resp := h.do(http.MethodPost, "/identity-providers/check",
			gen.IdentityProviderCheckRequest{Id: &p.Id, Username: ptr("alice")})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

// inputOfProvider is a provider as it would be sent back unchanged, its
// secrets left out so that they are kept.
func inputOfProvider(p gen.IdentityProvider) gen.IdentityProviderInput {
	return gen.IdentityProviderInput{
		Name: p.Name, Kind: p.Kind, Enabled: &p.Enabled, Mappings: &p.Mappings,
		CaCertificate: p.CaCertificate, Oidc: p.Oidc, Ldap: p.Ldap,
	}
}
