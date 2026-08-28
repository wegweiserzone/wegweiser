package api

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// createTSIGKey makes one and returns it with its secret.
func (h *harness) createTSIGKey(t *testing.T, in gen.CreateTSIGKey) gen.TSIGKeySecret {
	t.Helper()

	var out gen.TSIGKeySecret
	h.decode(h.do(http.MethodPost, "/tsig-keys", in), http.StatusCreated, &out)
	return out
}

func TestTSIGKeyIsCreatedWithASecretOfItsOwn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	got := h.createTSIGKey(t, gen.CreateTSIGKey{Name: "secondary.example.com."})
	if got.Key.Name != "secondary.example.com." {
		t.Errorf("the key is named %q", got.Key.Name)
	}
	// The default, because RFC 8945 §6 calls the other two MUST-implement
	// algorithms NOT RECOMMENDED and MUST NOT use (D28).
	if got.Key.Algorithm != "hmac-sha256." {
		t.Errorf("it signs with %q, want hmac-sha256.", got.Key.Algorithm)
	}
	if got.Key.RevokedAt != nil {
		t.Error("a key that was just created is already withdrawn")
	}

	secret, err := base64.StdEncoding.DecodeString(got.Secret)
	if err != nil {
		t.Fatalf("the secret is not base64: %v", err)
	}
	// At least as long as the hash output (RFC 8945 §8).
	if len(secret) != zone.HMACSHA256.SecretBytes() {
		t.Errorf("the secret is %d bytes, want %d", len(secret), zone.HMACSHA256.SecretBytes())
	}
}

func TestTSIGSecretCanBeReadBackButIsNotInTheListing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	made := h.createTSIGKey(t, gen.CreateTSIGKey{Name: "secondary.example.com."})

	// Unlike a token: verifying a signature means recomputing the MAC, so this
	// server has it and pretending otherwise would be theatre (D28).
	var read gen.TSIGKeySecret
	h.decode(h.do(http.MethodGet, "/tsig-keys/"+made.Key.Id+"/secret", nil), http.StatusOK, &read)
	if read.Secret != made.Secret {
		t.Errorf("the secret read back as %q, want %q", read.Secret, made.Secret)
	}

	// The listing is where somebody is looking at something else, so it holds
	// no secret at all.
	resp := h.do(http.MethodGet, "/tsig-keys", nil)
	var listed []gen.TSIGKey
	h.decode(resp, http.StatusOK, &listed)
	if len(listed) != 1 {
		t.Fatalf("the listing holds %d keys, want one", len(listed))
	}
	if listed[0].Id != made.Key.Id {
		t.Errorf("the listing holds %s, want %s", listed[0].Id, made.Key.Id)
	}
}

func TestTSIGKeyTakesASecretThatAlreadyExists(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// The other end is often not ours to choose.
	want := base64.StdEncoding.EncodeToString([]byte("a secret the secondary already has"))
	got := h.createTSIGKey(t, gen.CreateTSIGKey{
		Name:      "secondary.example.com.",
		Algorithm: ptr(gen.TSIGAlgorithm("hmac-sha512.")),
		Secret:    &want,
	})
	if got.Secret != want {
		t.Errorf("the secret came back as %q, want %q", got.Secret, want)
	}
	if got.Key.Algorithm != "hmac-sha512." {
		t.Errorf("it signs with %q, want hmac-sha512.", got.Key.Algorithm)
	}
}

func TestTSIGKeyRefusesWhatItDoesNotSignWith(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	tests := []struct {
		name string
		in   gen.CreateTSIGKey
	}{
		{
			// The departure D28 argues, refused rather than quietly
			// downgraded.
			name: "an algorithm this server does not offer",
			in: gen.CreateTSIGKey{
				Name: "secondary.example.com.", Algorithm: ptr(gen.TSIGAlgorithm("hmac-sha1")),
			},
		},
		// A space is a legal label octet, so the refusals here are names that
		// really are not: nothing at all, and a label past the limit of
		// RFC 1035 §2.3.4.
		{name: "no name at all", in: gen.CreateTSIGKey{Name: ""}},
		{
			name: "a label longer than 63 octets",
			in:   gen.CreateTSIGKey{Name: strings.Repeat("a", 64) + ".example.com."},
		},
		{
			name: "a secret that is not base64",
			in: gen.CreateTSIGKey{
				Name: "secondary.example.com.", Secret: ptr("this is not base64!!"),
			},
		},
		{
			name: "an empty secret",
			in:   gen.CreateTSIGKey{Name: "secondary.example.com.", Secret: ptr("")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, "/tsig-keys", tc.in)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status is %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestTSIGKeyWithdrawalTakesTheSecret(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	made := h.createTSIGKey(t, gen.CreateTSIGKey{Name: "secondary.example.com."})

	if resp := h.do(http.MethodDelete, "/tsig-keys/"+made.Key.Id, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("withdrawing it is %d, want 204", resp.StatusCode)
	}

	// A token survives revocation because only a hash survives. Here the
	// material would be all that was left, so it goes (D28).
	if resp := h.do(http.MethodGet, "/tsig-keys/"+made.Key.Id+"/secret", nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("reading the secret back is %d, want 409", resp.StatusCode)
	}

	// The row stays, so a name a secondary still has configured looks up.
	var listed []gen.TSIGKey
	h.decode(h.do(http.MethodGet, "/tsig-keys", nil), http.StatusOK, &listed)
	if len(listed) != 1 || listed[0].RevokedAt == nil {
		t.Fatalf("the listing holds %v", listed)
	}

	// And withdrawing twice is not an error.
	if resp := h.do(http.MethodDelete, "/tsig-keys/"+made.Key.Id, nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("withdrawing it again is %d, want 204", resp.StatusCode)
	}

	// The name is free, so a rotation does not mean renaming it on the other
	// end.
	next := h.createTSIGKey(t, gen.CreateTSIGKey{Name: "secondary.example.com."})
	if next.Key.Id == made.Key.Id {
		t.Error("the replacement reused the withdrawn key")
	}
}

func TestTSIGKeysNeedTheAdminScope(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A key that may transfer may take every zone, so it is a credential like
	// a token and not a setting like the others.
	var minted gen.TokenCreated
	h.decode(h.do(http.MethodPost, "/tokens", gen.CreateToken{
		Name: "writer", Scopes: []gen.Scope{gen.Write},
	}), http.StatusCreated, &minted)
	as := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+minted.Secret) }

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/tsig-keys"},
		{http.MethodPost, "/tsig-keys"},
	} {
		resp := h.do(tc.method, tc.path, gen.CreateTSIGKey{Name: "x.example.com."}, as)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s is %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
	}
}
