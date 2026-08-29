package apply_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestParseTransferAllow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []string
		// bad is what the refusal has to mention, or empty if it is accepted.
		bad string
	}{
		{name: "nothing at all", in: nil, want: []string{}},
		{name: "blank entries are dropped", in: []string{"", "  "}, want: []string{}},
		{
			name: "prefixes come back as they went in",
			in:   []string{"192.0.2.0/24", "2001:db8::/32"},
			want: []string{"192.0.2.0/24", "2001:db8::/32"},
		},
		{
			// Naming one secondary should not require knowing that a host is a
			// /32.
			name: "a bare address is the host it means",
			in:   []string{"192.0.2.7", "2001:db8::1"},
			want: []string{"192.0.2.7/32", "2001:db8::1/128"},
		},
		{
			name: "a v4 address written as v6 is stored as v4",
			in:   []string{"::ffff:192.0.2.7"},
			want: []string{"192.0.2.7/32"},
		},
		{
			// Masking it would sometimes hand a zone to a whole network
			// nobody meant to name.
			name: "a prefix with host bits set is refused, and says what was meant",
			in:   []string{"192.0.2.7/24"},
			bad:  "192.0.2.0/24",
		},
		{
			// A key grants a transfer from anywhere, which is what it is for
			// (D28).
			name: "a key is named as such",
			in:   []string{"key:secondary.example.com."},
			want: []string{"key:secondary.example.com."},
		},
		{
			name: "addresses come first, whatever order they were sent in",
			in:   []string{"key:secondary.example.com.", "192.0.2.0/24"},
			want: []string{"192.0.2.0/24", "key:secondary.example.com."},
		},
		{name: "a key with no name", in: []string{"key:"}, bad: "does not name a TSIG key"},
		{name: "a name is not an address", in: []string{"ns1.example.com."}, bad: "not an address"},
		{name: "nonsense", in: []string{"192.0.2.0/33"}, bad: "not an address"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := apply.ParseTransferAllow(tt.in)
			if tt.bad != "" {
				if err == nil {
					t.Fatalf("%v was accepted as %v", tt.in, got)
				}
				if !strings.Contains(err.Error(), tt.bad) {
					t.Errorf("error is %q, want it to mention %q", err, tt.bad)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTransferAllow(%v): %v", tt.in, err)
			}
			if text := apply.TransferAllowText(got); !slices.Equal(text, tt.want) {
				t.Errorf("got %v, want %v", text, tt.want)
			}
		})
	}
}

func TestTransferAllowRoundTripsThroughTheStore(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	// Nothing set is nobody, which is where a server starts (D26).
	var got apply.TransferAllow
	read := func() {
		t.Helper()
		if err := f.s.View(t.Context(), func(r store.Reader) error {
			var verr error
			got, verr = apply.StoredTransferAllow(t.Context(), r)
			return verr
		}); err != nil {
			t.Fatalf("StoredTransferAllow: %v", err)
		}
	}

	read()
	if !got.Empty() {
		t.Fatalf("a fresh server allows %v", apply.TransferAllowText(got))
	}

	want, err := apply.ParseTransferAllow(
		[]string{"192.0.2.0/24", "2001:db8::1", "key:secondary.example.com."})
	if err != nil {
		t.Fatalf("ParseTransferAllow: %v", err)
	}
	change, cerr := apply.TransferAllowChange(want)
	if cerr != nil {
		t.Fatalf("TransferAllowChange: %v", cerr)
	}
	setSetting(t, f, change)

	read()
	if !slices.Equal(apply.TransferAllowText(got), apply.TransferAllowText(want)) {
		t.Errorf("read back %v, want %v",
			apply.TransferAllowText(got), apply.TransferAllowText(want))
	}

	// And back to nobody, which has to be expressible or a list can never be
	// taken away again.
	empty, eerr := apply.TransferAllowChange(apply.TransferAllow{})
	if eerr != nil {
		t.Fatalf("TransferAllowChange: %v", eerr)
	}
	setSetting(t, f, empty)
	read()
	if !got.Empty() {
		t.Errorf("after clearing it, %v may still transfer", apply.TransferAllowText(got))
	}
}

func TestParseNotifyTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []string
		// bad is what the refusal has to mention, or empty if it is accepted.
		bad string
	}{
		{name: "nothing at all", in: nil, want: []string{}},
		{name: "blank entries are dropped", in: []string{"", "  "}, want: []string{}},
		{
			// The port every resolver would have assumed is left off again, so
			// what an operator typed is what they read back.
			name: "an address alone means the port RFC 1035 §4.2 assigns",
			in:   []string{"192.0.2.53", "2001:db8::53"},
			want: []string{"192.0.2.53", "2001:db8::53"},
		},
		{
			name: "a secondary somewhere else keeps its port",
			in:   []string{"192.0.2.53:5353", "[2001:db8::53]:5353"},
			want: []string{"192.0.2.53:5353", "[2001:db8::53]:5353"},
		},
		{
			name: "a port that is the assigned one is still written without it",
			in:   []string{"192.0.2.53:53"},
			want: []string{"192.0.2.53"},
		},
		{
			name: "a v4 address written as v6 is stored as v4",
			in:   []string{"::ffff:192.0.2.53"},
			want: []string{"192.0.2.53"},
		},
		{
			// A secondary that insists on TSIG has to be able to trust the
			// news too (D28). Written the way BIND writes an also-notify.
			name: "a target can name the key its notification is signed with",
			in:   []string{"192.0.2.53 key:secondary.example.com."},
			want: []string{"192.0.2.53 key:secondary.example.com."},
		},
		{
			name: "with a port as well",
			in:   []string{"[2001:db8::53]:5353 key:secondary.example.com."},
			want: []string{"[2001:db8::53]:5353 key:secondary.example.com."},
		},
		{
			name: "something after the address that is not a key",
			in:   []string{"192.0.2.53 secondary.example.com."},
			bad:  "is not a key",
		},
		{
			// The transfer list takes prefixes and this one cannot: a
			// notification has to arrive somewhere.
			name: "a prefix is not somewhere a datagram can arrive",
			in:   []string{"192.0.2.0/24"},
			bad:  "a prefix cannot be notified",
		},
		{name: "a name is not an address", in: []string{"ns1.example.com."}, bad: "not an address"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := apply.ParseNotifyTargets(tt.in)
			if tt.bad != "" {
				if err == nil {
					t.Fatalf("%v was accepted as %v", tt.in, got)
				}
				if !strings.Contains(err.Error(), tt.bad) {
					t.Errorf("error is %q, want it to mention %q", err, tt.bad)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNotifyTargets(%v): %v", tt.in, err)
			}
			if text := apply.NotifyTargetsText(got); !slices.Equal(text, tt.want) {
				t.Errorf("got %v, want %v", text, tt.want)
			}
		})
	}
}

func TestNotifyTargetsRoundTripThroughTheStore(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	// Nothing set tells nobody, which is where a server starts (D27).
	var got []apply.NotifyTarget
	read := func() {
		t.Helper()
		if err := f.s.View(t.Context(), func(r store.Reader) error {
			var verr error
			got, verr = apply.StoredNotifyTargets(t.Context(), r)
			return verr
		}); err != nil {
			t.Fatalf("StoredNotifyTargets: %v", err)
		}
	}

	read()
	if len(got) != 0 {
		t.Fatalf("a fresh server tells %v", apply.NotifyTargetsText(got))
	}

	want, err := apply.ParseNotifyTargets(
		[]string{"192.0.2.53", "[2001:db8::53]:5353 key:secondary.example.com."})
	if err != nil {
		t.Fatalf("ParseNotifyTargets: %v", err)
	}
	change, cerr := apply.NotifyTargetsChange(want)
	if cerr != nil {
		t.Fatalf("NotifyTargetsChange: %v", cerr)
	}
	setSetting(t, f, change)

	read()
	if !slices.Equal(apply.NotifyTargetsText(got), apply.NotifyTargetsText(want)) {
		t.Errorf("stored %v, read back %v",
			apply.NotifyTargetsText(want), apply.NotifyTargetsText(got))
	}
}

// setSetting writes a setting the way everything else is written: planned, and
// applied as a batch.
func setSetting(t *testing.T, f *fixture, c apply.SettingChange) {
	t.Helper()

	if err := f.a.SetSettings(t.Context(), []apply.SettingChange{c}); err != nil {
		t.Fatalf("SetSettings(%s): %v", c.Key, err)
	}
}

// A setting is replicated state, so it has to survive the journey a batch
// makes between nodes and land the same on the far side (D32).
func TestASettingTravelsInABatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	change, err := apply.PolicyChange(apply.PolicyLastWins)
	if err != nil {
		t.Fatalf("PolicyChange: %v", err)
	}

	b, err := f.a.PlanSettings([]apply.SettingChange{change})
	if err != nil {
		t.Fatalf("PlanSettings: %v", err)
	}
	// A batch that touches no zone produces no commit, so "has commits" is not
	// the test for whether it does anything.
	if b.Empty() {
		t.Fatal("a batch carrying a setting reports itself as empty")
	}

	encoded, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshalling the batch: %v", err)
	}
	var travelled apply.Batch
	if uerr := json.Unmarshal(encoded, &travelled); uerr != nil {
		t.Fatalf("unmarshalling the batch: %v", uerr)
	}
	if travelled.Empty() {
		t.Fatal("the batch arrived empty")
	}

	if aerr := f.a.ApplyBatch(t.Context(), &travelled); aerr != nil {
		t.Fatalf("ApplyBatch: %v", aerr)
	}

	var got apply.Policy
	if verr := f.s.View(t.Context(), func(r store.Reader) error {
		var rerr error
		got, rerr = apply.StoredPolicy(t.Context(), r)
		return rerr
	}); verr != nil {
		t.Fatalf("StoredPolicy: %v", verr)
	}
	if got != apply.PolicyLastWins {
		t.Errorf("the policy is %q after the batch travelled, want %q", got, apply.PolicyLastWins)
	}
}

// Nothing reaches the store that the write path has not resolved first.
func TestASettingThatIsNotJSONIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	err := f.a.SetSettings(t.Context(), []apply.SettingChange{
		{Key: "reverse_policy", Value: []byte(`{not json`)},
	})
	if err == nil {
		t.Fatal("a value that is not JSON was accepted")
	}
	if !errors.Is(err, zone.ErrInvalid) {
		t.Errorf("error = %v, want one wrapping zone.ErrInvalid", err)
	}
}

// A transfer key travels whole, secret included: a node without the material
// cannot recompute a MAC and so cannot answer a signed request at all (D28).
func TestATransferKeyTravelsInABatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	secret := make([]byte, zone.HMACSHA256.SecretBytes())
	for i := range secret {
		secret[i] = byte(i)
	}

	b, key, err := f.a.PlanCreateKey(
		zone.MustParseName("secondary.example.com."), zone.HMACSHA256, secret)
	if err != nil {
		t.Fatalf("PlanCreateKey: %v", err)
	}
	if b.Empty() {
		t.Fatal("a batch carrying a key reports itself as empty")
	}
	if key.ID == "" || key.CreatedAt.IsZero() {
		t.Error("the plan left the identifier or the moment for whoever writes it to invent")
	}

	encoded, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshalling the batch: %v", err)
	}
	var travelled apply.Batch
	if uerr := json.Unmarshal(encoded, &travelled); uerr != nil {
		t.Fatalf("unmarshalling the batch: %v", uerr)
	}
	if aerr := f.a.ApplyBatch(t.Context(), &travelled); aerr != nil {
		t.Fatalf("ApplyBatch: %v", aerr)
	}

	got, err := f.s.TSIGKeyByName(t.Context(), zone.MustParseName("secondary.example.com."))
	if err != nil {
		t.Fatalf("TSIGKeyByName: %v", err)
	}
	if got.ID != key.ID {
		t.Errorf("the key arrived as %s, want %s: one key, one identity", got.ID, key.ID)
	}
	if !bytes.Equal(got.Secret, secret) {
		t.Error("the secret did not survive the journey, so this node cannot verify with it")
	}
	if !got.CreatedAt.Equal(key.CreatedAt) {
		t.Errorf("the key is dated %s here and %s where it was planned",
			got.CreatedAt, key.CreatedAt)
	}

	// And revoking it takes the secret with it, on whichever node applies it.
	rb, err := f.a.PlanRevokeKey(key.ID)
	if err != nil {
		t.Fatalf("PlanRevokeKey: %v", err)
	}
	if aerr := f.a.ApplyBatch(t.Context(), rb); aerr != nil {
		t.Fatalf("ApplyBatch: %v", aerr)
	}
	revoked, err := f.s.TSIGKeyByID(t.Context(), key.ID)
	if err != nil {
		t.Fatalf("TSIGKeyByID: %v", err)
	}
	if len(revoked.Secret) != 0 {
		t.Error("a revoked key still carries its secret")
	}
}

// A token created on one node has to authenticate on every node, so what is
// stored about it travels. The secret does not: this server never held one.
func TestATokenTravelsInABatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = byte(i)
	}
	tok := &store.Token{
		ID:     store.TokenID(id.New()),
		Name:   "deploy",
		Prefix: "weg_abcd",
		Hash:   hash,
		Scopes: []string{"write"},
	}

	b, err := f.a.PlanCreateToken(tok)
	if err != nil {
		t.Fatalf("PlanCreateToken: %v", err)
	}
	if tok.CreatedAt.IsZero() {
		t.Error("the plan left the moment for whoever writes it to invent")
	}

	encoded, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshalling the batch: %v", err)
	}
	if bytes.Contains(encoded, []byte("weg_abcd")) == false {
		t.Error("the prefix did not travel, and the interface tells tokens apart by it")
	}
	var travelled apply.Batch
	if uerr := json.Unmarshal(encoded, &travelled); uerr != nil {
		t.Fatalf("unmarshalling the batch: %v", uerr)
	}
	if aerr := f.a.ApplyBatch(t.Context(), &travelled); aerr != nil {
		t.Fatalf("ApplyBatch: %v", aerr)
	}

	got, err := f.s.TokenByHash(t.Context(), tok.Hash)
	if err != nil {
		t.Fatalf("TokenByHash: %v", err)
	}
	if got.ID != tok.ID || got.Name != "deploy" {
		t.Errorf("the token arrived as %s/%s, want %s/deploy", got.ID, got.Name, tok.ID)
	}
}

// A revocation is refused while it is planned, not while it is written: a
// follower must not be in a position to disagree with the node that decided.
func TestARevocationIsRefusedInThePlan(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	refused := errors.New("the last administrator")
	err := f.a.RevokeToken(t.Context(), store.TokenID(id.New()),
		func([]*store.Token) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("error = %v, want the guard's own", err)
	}
}
