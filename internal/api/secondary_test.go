package api

import (
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// secondaryConfig asks for the configuration the other end of a transfer needs.
func (h *harness) secondaryConfig(t *testing.T, q url.Values) gen.SecondaryConfig {
	t.Helper()

	var out gen.SecondaryConfig
	h.decode(h.do(http.MethodGet, "/secondary-config?"+q.Encode(), nil), http.StatusOK, &out)
	return out
}

// refusedConfig asks for one that cannot be written, and returns the refusal.
func (h *harness) refusedConfig(t *testing.T, q url.Values, want int) string {
	t.Helper()

	resp := h.do(http.MethodGet, "/secondary-config?"+q.Encode(), nil)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the refusal: %v", err)
	}
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, want, body)
	}
	return string(body)
}

// query is the smallest request that can be answered.
func query(format string) url.Values {
	return url.Values{"format": {format}, "primary": {"192.0.2.1"}}
}

func TestSecondaryConfigCarriesEveryZoneAndTheKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.createZone("example.com.")
	h.createZone("2.0.192.in-addr.arpa.")
	key := h.createTSIGKey(t, gen.CreateTSIGKey{Name: "ns2.example.com."})
	h.decode(h.do(http.MethodPatch, "/settings", gen.UpdateSettings{
		TransferAllow: &[]string{"key:ns2.example.com."},
		NotifyTargets: &[]string{"198.51.100.53"},
	}), http.StatusOK, nil)

	q := query("bind")
	q.Set("secondary", "198.51.100.53")
	got := h.secondaryConfig(t, q)

	if got.Format != gen.SecondaryFormatBind {
		t.Errorf("format = %q", got.Format)
	}
	// The reverse zone as well as the forward one: it is the one a person
	// setting a secondary up by hand forgets.
	for _, want := range []string{
		`zone "example.com." {`,
		`zone "2.0.192.in-addr.arpa." {`,
		`key "ns2.example.com." {`,
		`secret "` + key.Secret + `";`,
	} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("the configuration is missing %q:\n%s", want, got.Content)
		}
	}
	// The algorithm is stored with the trailing dot RFC 8945 gives it, and
	// neither program takes it that way.
	if !strings.Contains(got.Content, "algorithm hmac-sha256;") {
		t.Errorf("the algorithm still carries its dot:\n%s", got.Content)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("a complete arrangement warns: %v", got.Warnings)
	}
	// The field is required, so nothing having gone wrong is an empty list
	// rather than a null a client has to guard against.
	if got.Warnings == nil {
		t.Error("warnings is null, want a list a client can range over")
	}
}

func TestSecondaryConfigLeavesOutADisabledZone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.createZone("example.com.")
	off := h.createZone("off.example.")
	h.decode(h.do(http.MethodPatch, "/zones/"+off.Id, gen.UpdateZone{Disabled: ptr(true)}),
		http.StatusOK, nil)

	got := h.secondaryConfig(t, query("knot"))

	if !strings.Contains(got.Content, "domain: example.com.") {
		t.Errorf("the zone that answers is missing:\n%s", got.Content)
	}
	// It is not in the snapshot, so a transfer of it is refused and a
	// secondary configured for it would retry for ever.
	if strings.Contains(got.Content, "off.example.") {
		t.Errorf("a switched-off zone is in the configuration:\n%s", got.Content)
	}

	t.Run("and refuses to write one that was asked for", func(t *testing.T) {
		q := query("knot")
		q.Set("zone", "off.example.")
		if body := h.refusedConfig(t, q, http.StatusBadRequest); !strings.Contains(
			body, "switched off") {
			t.Errorf("the refusal does not say why:\n%s", body)
		}
	})
}

func TestSecondaryConfigWritesTheZonesItIsGiven(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.createZone("example.com.")
	h.createZone("other.example.")

	q := query("bind")
	q.Set("zone", "example.com")
	got := h.secondaryConfig(t, q)

	if !strings.Contains(got.Content, `zone "example.com." {`) {
		t.Errorf("the zone that was asked for is missing:\n%s", got.Content)
	}
	if strings.Contains(got.Content, "other.example.") {
		t.Errorf("a zone nobody asked for is in the configuration:\n%s", got.Content)
	}
}

func TestSecondaryConfigReportsAnArrangementThatCannotWork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.createZone("example.com.")
	got := h.secondaryConfig(t, query("bind"))

	// A server nobody has configured for transfers, which is where one starts.
	if len(got.Warnings) != 2 {
		t.Fatalf("got %d warnings, want the transfer list and the notify list: %v",
			len(got.Warnings), got.Warnings)
	}
	if !strings.Contains(got.Warnings[0], "nobody may transfer") {
		t.Errorf("the first warning is %q", got.Warnings[0])
	}
	if !strings.Contains(got.Warnings[1], "nobody is told") {
		t.Errorf("the second warning is %q", got.Warnings[1])
	}
	// It is written anyway. Half an arrangement is what somebody setting one
	// up has, and the file is the half this server can supply.
	if !strings.Contains(got.Content, `zone "example.com." {`) {
		t.Errorf("nothing was written:\n%s", got.Content)
	}
}

func TestSecondaryConfigNeedsTheAdminScope(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	var minted gen.TokenCreated
	h.decode(h.do(http.MethodPost, "/tokens", gen.CreateToken{
		Name: "the deploy pipeline", Scopes: []gen.Scope{gen.ScopeWrite},
	}), http.StatusCreated, &minted)

	resp := h.do(http.MethodGet, "/secondary-config?"+query("bind").Encode(), nil,
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+minted.Secret) })
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want a write token refused a key's secret", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the refusal: %v", err)
	}
	// The refusal names what was refused rather than the first thing the scope
	// happens to guard.
	if !strings.Contains(string(body), "secondary's configuration") {
		t.Errorf("the refusal does not say what it refused:\n%s", body)
	}
}

func TestSecondaryConfigRefusesWhatItCannotWrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.createZone("example.com.")

	cases := []struct {
		name  string
		query func() url.Values
		want  string
	}{
		{
			name: "software nobody writes for",
			query: func() url.Values {
				q := query("djbdns")
				return q
			},
			want: "no configuration is written for",
		},
		{
			name: "a primary that is not an address",
			query: func() url.Values {
				q := query("bind")
				q.Set("primary", "ns1.example.com")
				return q
			},
			want: "is an address with an optional port",
		},
		{
			name: "a secondary given as a network",
			query: func() url.Values {
				q := query("bind")
				q.Set("secondary", "198.51.100.0/24")
				return q
			},
			want: "one address rather than a network",
		},
		{
			name: "a key that signs nothing here",
			query: func() url.Values {
				q := query("bind")
				q.Set("key", "nobody.example.")
				return q
			},
			want: "no key named nobody.example. signs here",
		},
		{
			name: "a zone this server does not hold",
			query: func() url.Values {
				q := query("bind")
				q.Set("zone", "nowhere.example.")
				return q
			},
			want: "no zone named nowhere.example.",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if body := h.refusedConfig(t, c.query(), http.StatusBadRequest); !strings.Contains(
				body, c.want) {
				t.Errorf("the refusal does not say %q:\n%s", c.want, body)
			}
		})
	}
}

func TestSecondaryConfigAsksWhichKeyWhenSeveralMay(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.createZone("example.com.")
	h.createTSIGKey(t, gen.CreateTSIGKey{Name: "ns2.example.com."})
	h.createTSIGKey(t, gen.CreateTSIGKey{Name: "ns3.example.com."})
	h.decode(h.do(http.MethodPatch, "/settings", gen.UpdateSettings{
		TransferAllow: &[]string{"key:ns2.example.com.", "key:ns3.example.com."},
	}), http.StatusOK, nil)

	body := h.refusedConfig(t, query("bind"), http.StatusBadRequest)
	for _, want := range []string{"more than one key", "ns2.example.com.", "ns3.example.com."} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, body)
		}
	}

	t.Run("and writes one once it is named", func(t *testing.T) {
		q := query("bind")
		q.Set("key", "ns3.example.com.")
		got := h.secondaryConfig(t, q)
		if !strings.Contains(got.Content, `key "ns3.example.com." {`) {
			t.Errorf("the key that was named is missing:\n%s", got.Content)
		}
		if strings.Contains(got.Content, "ns2.example.com.") {
			t.Errorf("a key nobody named is in the configuration:\n%s", got.Content)
		}
	})

	t.Run("and writes none at all where the address list is the control", func(t *testing.T) {
		q := query("bind")
		q.Set("signed", "false")
		got := h.secondaryConfig(t, q)
		if strings.Contains(got.Content, "key ") {
			t.Errorf("a configuration that signs nothing carries a key:\n%s", got.Content)
		}
	})
}

func TestSecondaryConfigNamesAPortOnlyWhenItIsNotTheOneMeant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.createZone("example.com.")

	plain := h.secondaryConfig(t, query("knot"))
	if strings.Contains(plain.Content, "@") {
		t.Errorf("an address on port 53 carries a port:\n%s", plain.Content)
	}

	q := query("knot")
	q.Set("primary", "[2001:db8::1]:5353")
	moved := h.secondaryConfig(t, q)
	if !strings.Contains(moved.Content, "address: 2001:db8::1@5353") {
		t.Errorf("the port is missing:\n%s", moved.Content)
	}
}

// standing is a fixed answer from the prober, so that the mapping onto the wire
// is what the test is about rather than the asking.
type standing []dns.ProbeStanding

func (s standing) Standing() []dns.ProbeStanding { return s }

// secondaryStatus asks where the secondaries stand.
func (h *harness) secondaryStatus(t *testing.T) []gen.SecondaryStanding {
	t.Helper()

	var out []gen.SecondaryStanding
	h.decode(h.do(http.MethodGet, "/secondary-status", nil), http.StatusOK, &out)
	return out
}

func TestSecondaryStatusSaysWhereEachOneStands(t *testing.T) {
	t.Parallel()

	target := netip.MustParseAddrPort("192.0.2.53:53")
	asked := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	h := newHarness(t, func(c *Config) {
		c.Secondaries = standing{
			{
				Zone: zone.MustParseName("behind.example."), Target: target,
				Outcome: dns.ProbeBehind, Serial: zone.NewSerial(9), Known: true,
				Lag: 3, At: asked,
			},
			{
				Zone: zone.MustParseName("current.example."), Target: target,
				Outcome: dns.ProbeInStep, Serial: zone.NewSerial(12), Known: true,
				At: asked,
			},
			{
				Zone: zone.MustParseName("unasked.example."), Target: target,
			},
		}
	})

	got := h.secondaryStatus(t)
	if len(got) != 3 {
		t.Fatalf("the status holds %d entries, want three", len(got))
	}

	behind := got[0]
	if behind.State != gen.SecondaryStandingStateBehind {
		t.Errorf("the first is %s, want behind", behind.State)
	}
	if behind.Lag == nil || *behind.Lag != 3 {
		t.Errorf("it is %v commits behind, want 3", behind.Lag)
	}
	if behind.Serial == nil || *behind.Serial != 9 {
		t.Errorf("it holds serial %v, want 9", behind.Serial)
	}
	if behind.AskedAt == nil || !behind.AskedAt.Equal(asked) {
		t.Errorf("it was asked at %v, want %v", behind.AskedAt, asked)
	}
	if behind.Target != target.String() {
		t.Errorf("the target is %s, want %s", behind.Target, target)
	}

	// A lag belongs to a secondary that is behind, and to no other state.
	if current := got[1]; current.State != gen.SecondaryStandingStateInStep || current.Lag != nil {
		t.Errorf("the second is %s carrying lag %v, want inStep carrying none", current.State, current.Lag)
	}

	// Nothing has come back for the third, and saying it is in step would be
	// the one thing this endpoint exists to stop.
	unasked := got[2]
	if unasked.State != gen.SecondaryStandingStateUnasked {
		t.Errorf("the third is %s, want unasked", unasked.State)
	}
	if unasked.Serial != nil || unasked.AskedAt != nil {
		t.Errorf("it carries serial %v asked at %v, want neither", unasked.Serial, unasked.AskedAt)
	}
}

// A server running without a prober knows nothing about any secondary, which
// is the same answer one with an empty notify list gives.
func TestSecondaryStatusIsEmptyWithoutAProber(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if got := h.secondaryStatus(t); len(got) != 0 {
		t.Errorf("the status holds %d entries, want none", len(got))
	}
}
