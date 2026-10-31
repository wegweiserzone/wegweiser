package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
)

// fakeDirectory stands in for FreeIPA: people under cn=users, groups under
// cn=groups, memberOf kept on every entry. It answers the two searches a
// sign-in makes by comparing the filter with the one the provider's template
// should have produced, which is also what shows a name was escaped.
type fakeDirectory struct {
	mu sync.Mutex
	// people are by uid; groups by DN, each to the DNs of their members.
	people map[string]fakePerson
	groups map[string][]string
	// binds are the DNs bound as, in order, for a test to look at.
	binds []string
}

type fakePerson struct {
	dn, password string
}

const (
	usersBase  = "cn=users,cn=accounts,dc=example,dc=com"
	groupsBase = "cn=groups,cn=accounts,dc=example,dc=com"
	serviceDN  = "uid=weg,cn=sysaccounts,cn=etc,dc=example,dc=com"
)

func newFakeDirectory() *fakeDirectory {
	alice := "uid=alice," + usersBase
	bob := "uid=bob," + usersBase
	return &fakeDirectory{
		people: map[string]fakePerson{
			"alice": {dn: alice, password: "correct horse"},
			"bob":   {dn: bob, password: "battery staple"},
		},
		groups: map[string][]string{
			"cn=admins," + groupsBase: {alice},
			"cn=staff," + groupsBase:  {alice, bob},
		},
	}
}

func (d *fakeDirectory) dial(context.Context, *apply.IdentityProvider) (directory, error) {
	return d, nil
}

func (d *fakeDirectory) Bind(dn, password string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.binds = append(d.binds, dn)
	if dn == serviceDN && password == "service" {
		return nil
	}
	for _, p := range d.people {
		if p.dn == dn && p.password == password && password != "" {
			return nil
		}
	}
	return ldap.NewError(ldap.LDAPResultInvalidCredentials, errors.New("invalid credentials"))
}

func (d *fakeDirectory) Search(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := &ldap.SearchResult{}
	switch req.BaseDN {
	case usersBase:
		for uid, p := range d.people {
			if req.Filter != "(uid="+ldap.EscapeFilter(uid)+")" {
				continue
			}
			var memberOf []string
			for g, members := range d.groups {
				for _, m := range members {
					if m == p.dn {
						memberOf = append(memberOf, g)
					}
				}
			}
			out.Entries = append(out.Entries, ldap.NewEntry(p.dn, map[string][]string{
				"uid": {uid}, "memberOf": memberOf,
			}))
		}
	case groupsBase:
		for g, members := range d.groups {
			for _, m := range members {
				if req.Filter == "(member="+ldap.EscapeFilter(m)+")" {
					out.Entries = append(out.Entries, ldap.NewEntry(g, nil))
				}
			}
		}
	default:
		// As a directory answers a search below a base it does not hold.
		return nil, ldap.NewError(ldap.LDAPResultNoSuchObject, errors.New("no such object"))
	}
	return out, nil
}

func (d *fakeDirectory) Close() error { return nil }

// bound is who has been bound as so far.
func (d *fakeDirectory) bound() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.binds)
}

// directoryHarness is a server with the fake directory as a provider, mapped
// as the given groups say.
func directoryHarness(t *testing.T, groups string, mappings ...gen.GroupMapping) (*harness, *fakeDirectory, gen.IdentityProvider) {
	t.Helper()
	d := newFakeDirectory()
	h := newHarness(t, func(cfg *Config) { cfg.dialLDAP = d.dial })
	in := gen.IdentityProviderInput{
		Name: "FreeIPA", Kind: gen.IdentityProviderKindLdap, Mappings: &mappings,
		Ldap: &gen.LDAPSettings{
			Url: "ldaps://ipa.example.com", BindDn: ptr(serviceDN), BindPassword: ptr("service"),
			UserBase: usersBase, UserFilter: "(uid={username})",
			Groups: ptr(gen.LDAPSettingsGroups(groups)),
		},
	}
	if groups == string(apply.GroupsSearch) {
		in.Ldap.GroupBase, in.Ldap.GroupFilter = ptr(groupsBase), ptr("(member={dn})")
	}
	var p gen.IdentityProvider
	h.decode(h.do(http.MethodPost, "/identity-providers", in), http.StatusCreated, &p)
	return h, d, p
}

func signInToDirectory(h *harness, p gen.IdentityProvider, username, password string) *http.Response {
	h.t.Helper()
	return h.do(http.MethodPost, ldapPath+"/"+p.Id, gen.CreateDirectorySession{Username: username, Password: password},
		func(r *http.Request) { r.Header.Del("Authorization") })
}

// D51: a person signs in through a directory by binding as their own entry,
// and holds the strongest scope their groups map to.
func TestAPersonSignsInThroughADirectory(t *testing.T) {
	t.Parallel()

	for _, groups := range []string{string(apply.GroupsMemberOf), string(apply.GroupsSearch)} {
		t.Run("groups from "+groups, func(t *testing.T) {
			t.Parallel()
			// One mapping names the group by its DN, the other by its cn.
			h, d, p := directoryHarness(t, groups,
				gen.GroupMapping{Group: "cn=staff," + groupsBase, Scope: gen.ScopeRead},
				gen.GroupMapping{Group: "admins", Scope: gen.ScopeAdmin})

			resp := signInToDirectory(h, p, "alice", "correct horse")
			var opened gen.Session
			h.decode(resp, http.StatusCreated, &opened)
			if opened.Name != "alice (FreeIPA)" || len(opened.Scopes) != 1 || opened.Scopes[0] != gen.ScopeAdmin {
				t.Errorf("session = %+v, want alice through FreeIPA, with admin", opened)
			}
			value, csrf := cookiesOf(t, resp)
			made := h.do(http.MethodPost, "/zones", gen.CreateZone{Name: "alice.example."}, func(r *http.Request) {
				r.Header.Del("Authorization")
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
				r.Header.Set(csrfHeader, csrf)
			})
			if made.StatusCode != http.StatusCreated {
				t.Errorf("a write in alice's session: status %d, want 201", made.StatusCode)
			}

			var bobs gen.Session
			h.decode(signInToDirectory(h, p, "bob", "battery staple"), http.StatusCreated, &bobs)
			if len(bobs.Scopes) != 1 || bobs.Scopes[0] != gen.ScopeRead {
				t.Errorf("bob's session = %+v, want read", bobs)
			}
			if b := d.bound(); len(b) < 2 || b[0] != serviceDN || b[1] != "uid=alice,"+usersBase {
				t.Errorf("bound as %v, want the service account and then alice's entry", b)
			}
		})
	}
}

// D51: an administrator ends every session one person holds, and nobody
// else's; the person can sign in again afterwards.
func TestAnAdministratorEndsEverySessionOfOnePerson(t *testing.T) {
	t.Parallel()
	h, _, p := directoryHarness(t, string(apply.GroupsMemberOf),
		gen.GroupMapping{Group: "admins", Scope: gen.ScopeAdmin}, gen.GroupMapping{Group: "staff", Scope: gen.ScopeRead})

	session := func(name, password string) func(*http.Request) {
		value, csrf := cookiesOf(t, signInToDirectory(h, p, name, password))
		return func(r *http.Request) {
			r.Header.Del("Authorization")
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
			r.Header.Set(csrfHeader, csrf)
		}
	}
	status := func(as func(*http.Request)) int {
		resp := h.do(http.MethodGet, "/auth/session", nil, as)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	alice, aliceAgain, bob := session("alice", "correct horse"), session("alice", "correct horse"), session("bob", "battery staple")

	end := func(name string) *http.Response {
		return h.do(http.MethodPost, "/identity-providers/"+p.Id+"/end-sessions",
			gen.EndIdentityProviderSessionsJSONRequestBody{Name: name})
	}
	h.decode(end("Alice"), http.StatusNoContent, nil)
	if status(alice) != http.StatusUnauthorized || status(aliceAgain) != http.StatusUnauthorized {
		t.Error("alice's sessions still work after they were ended")
	}
	if got := status(bob); got != http.StatusOK {
		t.Errorf("bob's session: status %d, want it untouched", got)
	}
	if got := status(session("alice", "correct horse")); got != http.StatusOK {
		t.Errorf("alice signing in again: status %d, want 200", got)
	}

	h.decode(end(" "), http.StatusUnprocessableEntity, nil)
	if resp := h.do(http.MethodPost, "/identity-providers/01ARZ3NDEKTSV4RRFFQ69G5FAV/end-sessions",
		gen.EndIdentityProviderSessionsJSONRequestBody{Name: "alice"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("no such provider: status %d, want 404", resp.StatusCode)
	}
	if got := h.do(http.MethodPost, "/identity-providers/"+p.Id+"/end-sessions",
		gen.EndIdentityProviderSessionsJSONRequestBody{Name: "alice"}, bob); got.StatusCode != http.StatusForbidden {
		t.Errorf("bob, with read, ending alice's sessions: status %d, want 403", got.StatusCode)
	}
}

func TestASignInThroughADirectoryIsRefused(t *testing.T) {
	t.Parallel()
	h, d, p := directoryHarness(t, string(apply.GroupsMemberOf), gen.GroupMapping{Group: "admins", Scope: gen.ScopeAdmin})

	// A wrong password and a name nobody has read the same, so that the
	// answer does not say which names exist.
	wrong := signInToDirectory(h, p, "alice", "wrong")
	nobody := signInToDirectory(h, p, "carol", "anything")
	for _, resp := range []*http.Response{wrong, nobody} {
		if resp.StatusCode != http.StatusUnauthorized || len(resp.Cookies()) != 0 {
			t.Errorf("status = %d with %d cookies, want 401 and none", resp.StatusCode, len(resp.Cookies()))
		}
	}

	// An empty password is an unauthenticated bind, which a directory may
	// well call a success. It is never even tried.
	before := len(d.bound())
	if got := signInToDirectory(h, p, "alice", ""); got.StatusCode != http.StatusUnauthorized {
		t.Errorf("an empty password: status %d, want 401", got.StatusCode)
	}
	if after := d.bound(); len(after) != before {
		t.Errorf("an empty password reached the directory: %v", after[before:])
	}

	// What somebody types goes into the filter escaped, so it cannot widen it.
	if got := signInToDirectory(h, p, "*)(uid=*", "correct horse"); got.StatusCode != http.StatusUnauthorized {
		t.Errorf("a name written to match everybody: status %d, want 401", got.StatusCode)
	}

	// Bob signs in well, and is in no group that maps to anything.
	if got := signInToDirectory(h, p, "bob", "battery staple"); got.StatusCode != http.StatusForbidden {
		t.Errorf("a person in no mapped group: status %d, want 403", got.StatusCode)
	} else if body := problemDetail(t, got); !strings.Contains(body, "none of their groups") {
		t.Errorf("detail = %q, want it said why", body)
	}

	in := gen.IdentityProviderInput{Name: p.Name, Kind: p.Kind, Enabled: ptr(false), Mappings: &p.Mappings, Ldap: p.Ldap}
	h.decode(h.do(http.MethodPut, "/identity-providers/"+p.Id, in), http.StatusOK, nil)
	if got := signInToDirectory(h, p, "alice", "correct horse"); got.StatusCode != http.StatusNotFound {
		t.Errorf("a directory switched off: status %d, want 404", got.StatusCode)
	}
}

func problemDetail(t *testing.T, resp *http.Response) string {
	t.Helper()
	var problem gen.Problem
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode the problem: %v", err)
	}
	if problem.Detail == nil {
		return ""
	}
	return *problem.Detail
}

// A form on another site can post JSON as text/plain. A sign-in sent that way
// is refused before it is tried, so that no site signs a visitor's browser in
// as somebody else.
func TestASignInMustSayItIsJSON(t *testing.T) {
	t.Parallel()
	h, d, p := directoryHarness(t, string(apply.GroupsMemberOf), gen.GroupMapping{Group: "admins", Scope: gen.ScopeAdmin})

	asForm := func(r *http.Request) {
		r.Header.Del("Authorization")
		r.Header.Set("Content-Type", "text/plain")
	}
	before := len(d.bound())
	for _, tc := range []struct {
		path string
		body any
	}{
		{ldapPath + "/" + p.Id, gen.CreateDirectorySession{Username: "alice", Password: "correct horse"}},
		{sessionPath, gen.CreateSession{Token: h.token}},
	} {
		resp := h.do(http.MethodPost, tc.path, tc.body, asForm)
		if resp.StatusCode != http.StatusUnsupportedMediaType || len(resp.Cookies()) != 0 {
			t.Errorf("%s as text/plain: status %d with %d cookies, want 415 and none", tc.path, resp.StatusCode, len(resp.Cookies()))
		}
	}
	if after := d.bound(); len(after) != before {
		t.Errorf("a sign-in sent as a form reached the directory: %v", after[before:])
	}
	if got := signInToDirectory(h, p, "alice", "correct horse"); got.StatusCode != http.StatusCreated {
		t.Errorf("a sign-in sent as JSON: status %d, want 201", got.StatusCode)
	}
}
