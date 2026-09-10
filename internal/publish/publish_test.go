package publish_test

import (
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/publish"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// snapshots stands in for the query path's zones.
type snapshots struct {
	current atomic.Pointer[dns.Snapshot]
}

func (s *snapshots) Snapshot() *dns.Snapshot     { return s.current.Load() }
func (s *snapshots) SetSnapshot(n *dns.Snapshot) { s.current.Store(n) }

// sinks stands in for every other copy, and remembers what each was last
// handed and how many times anything was.
type sinks struct {
	mu        sync.Mutex
	transfers dns.Transfers
	keys      dns.Keyring
	targets   []dns.NotifyTarget
	cookies   dns.CookieSecrets
	handed    int
}

func (s *sinks) SetTransfers(t dns.Transfers) { s.hand(func() { s.transfers = t }) }
func (s *sinks) SetKeys(k dns.Keyring)        { s.hand(func() { s.keys = k }) }
func (s *sinks) SetTargets(t []dns.NotifyTarget) {
	s.hand(func() { s.targets = t })
}
func (s *sinks) SetCookieSecrets(c dns.CookieSecrets) { s.hand(func() { s.cookies = c }) }

func (s *sinks) hand(set func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set()
	s.handed++
}

func (s *sinks) times() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handed
}

// fixture is a store, a publisher copying it, and an applier that tells the
// publisher about each batch, which is the arrangement `weg serve` makes.
type fixture struct {
	store   store.Store
	snaps   *snapshots
	sinks   *sinks
	pub     *publish.Publisher
	applier *apply.Applier
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	st, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(t.TempDir(), "weg.db")})
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("close the database: %v", cerr)
		}
	})
	if merr := st.Migrate(t.Context()); merr != nil {
		t.Fatalf("migrate: %v", merr)
	}

	f := &fixture{store: st, snaps: &snapshots{}, sinks: &sinks{}}
	f.pub, err = publish.New(publish.Config{
		Store: st, Snapshots: f.snaps,
		Transfers: f.sinks, Keyring: f.sinks, Notify: f.sinks, Cookies: f.sinks,
		OnError: func(err error) { t.Errorf("the publisher reported a fault: %v", err) },
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	f.applier, err = apply.New(st, apply.Options{OnApplied: f.pub.Applied})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}
	return f
}

func (f *fixture) load(t *testing.T) {
	t.Helper()
	if err := f.pub.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// storeZone writes a zone straight into the store, past the applier and so
// past the hook.
func (f *fixture) storeZone(t *testing.T, apex string) *zone.Zone {
	t.Helper()

	z, err := zone.NewZone(zone.MustParseName(apex), zone.DefaultSOA(
		zone.MustParseName("ns1.example.com."), zone.MustParseName("hostmaster.example.com.")))
	if err != nil {
		t.Fatalf("build the zone %s: %v", apex, err)
	}
	z.ID = zone.ZoneID(id.New())
	f.update(t, func(tx store.Tx) error { return tx.CreateZone(t.Context(), &z) })
	return &z
}

func (f *fixture) update(t *testing.T, fn func(store.Tx) error) {
	t.Helper()
	if err := f.store.Update(t.Context(), fn); err != nil {
		t.Fatalf("write transaction: %v", err)
	}
}

// changed is what the applier would have said about a batch touching z.
func changed(z *zone.Zone) apply.Applied {
	return apply.Applied{Zones: []apply.AppliedZone{{ID: z.ID, Name: z.Name}}}
}

// settings returns a function that takes what a setting constructor returns
// and fails the test on an error, so that a constructor call reads as one value.
func settings(t *testing.T) func(apply.SettingChange, error) apply.SettingChange {
	return func(change apply.SettingChange, err error) apply.SettingChange {
		t.Helper()
		if err != nil {
			t.Fatalf("build the setting: %v", err)
		}
		return change
	}
}

func TestLoadCopiesEverythingTheStoreHolds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	must := settings(t)

	f.storeZone(t, "example.com.")
	key := &store.TSIGKey{
		ID: store.TSIGKeyID(id.New()), Name: zone.MustParseName("ns2.example.com."),
		Algorithm: zone.HMACSHA256, Secret: make([]byte, zone.HMACSHA256.SecretBytes()),
	}
	secrets, err := apply.CookieSecrets{}.Rotate(time.Now())
	if err != nil {
		t.Fatalf("mint the cookie secrets: %v", err)
	}
	allow := must(apply.TransferAllowChange(apply.TransferAllow{
		Prefixes: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
	}))
	notify := must(apply.NotifyTargetsChange([]apply.NotifyTarget{
		{Addr: netip.MustParseAddrPort("198.51.100.53:53")},
	}))
	cookies := must(apply.CookieSecretsChange(secrets))
	f.update(t, func(tx store.Tx) error {
		for _, c := range []apply.SettingChange{allow, notify, cookies} {
			if perr := tx.PutSetting(t.Context(), c.Key, c.Value); perr != nil {
				return perr
			}
		}
		return tx.CreateTSIGKey(t.Context(), key)
	})

	f.load(t)

	if got := f.snaps.Snapshot().Zones(); got != 1 {
		t.Errorf("the snapshot answers for %d zones, want 1", got)
	}
	f.sinks.mu.Lock()
	defer f.sinks.mu.Unlock()
	if _, ok := f.sinks.keys[key.Name]; !ok {
		t.Errorf("the keyring holds %v, want %s in it", f.sinks.keys, key.Name)
	}
	if f.sinks.transfers == nil || !f.sinks.transfers.MayTransfer(
		netip.MustParseAddr("192.0.2.7"), zone.Name{}, zone.MustParseName("example.com.")) {
		t.Error("the transfer list does not let 192.0.2.7 in")
	}
	if len(f.sinks.targets) != 1 {
		t.Errorf("the notify list holds %v, want one secondary", f.sinks.targets)
	}
	if f.sinks.cookies.Current != dns.CookieSecret(secrets.Current) {
		t.Error("the cookie secret in force is not the one the store holds")
	}
}

// Through the applier and its hook, which is the path every change takes: the
// test is that nothing else has to be called for a change to arrive.
func TestASettingReachesTheCopyItIsFor(t *testing.T) {
	t.Parallel()

	t.Run("who may transfer", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.load(t)

		must := settings(t)
		change := must(apply.TransferAllowChange(apply.TransferAllow{
			Prefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")},
		}))
		if err := f.applier.SetSettings(t.Context(), []apply.SettingChange{change}); err != nil {
			t.Fatalf("SetSettings: %v", err)
		}

		f.sinks.mu.Lock()
		defer f.sinks.mu.Unlock()
		if !f.sinks.transfers.MayTransfer(
			netip.MustParseAddr("2001:db8::1"), zone.Name{}, zone.MustParseName("example.com.")) {
			t.Error("the query path was not told about the change")
		}
	})

	t.Run("who is told of a change", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.load(t)

		must := settings(t)
		change := must(apply.NotifyTargetsChange([]apply.NotifyTarget{
			{Addr: netip.MustParseAddrPort("198.51.100.53:53")},
			{Addr: netip.MustParseAddrPort("198.51.100.54:53")},
		}))
		if err := f.applier.SetSettings(t.Context(), []apply.SettingChange{change}); err != nil {
			t.Fatalf("SetSettings: %v", err)
		}

		f.sinks.mu.Lock()
		defer f.sinks.mu.Unlock()
		if len(f.sinks.targets) != 2 {
			t.Errorf("the notify list holds %v, want both secondaries", f.sinks.targets)
		}
	})

	// A rotation is a setting like the others. On a node that did not rotate
	// it, applying the setting is the only way the secret arrives at all.
	t.Run("the cookie secret", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.load(t)

		secrets, rotated, err := f.applier.RotateCookieSecrets(t.Context())
		if err != nil || !rotated {
			t.Fatalf("RotateCookieSecrets = rotated %v, %v; want a first rotation", rotated, err)
		}

		f.sinks.mu.Lock()
		defer f.sinks.mu.Unlock()
		if f.sinks.cookies.Current != dns.CookieSecret(secrets.Current) {
			t.Error("the query path computes cookies under a secret other than the one just minted")
		}
	})

	t.Run("the reverse policy, which nothing copies", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.load(t)
		before := f.sinks.times()

		must := settings(t)
		change := must(apply.PolicyChange(apply.PolicyLastWins))
		if err := f.applier.SetSettings(t.Context(), []apply.SettingChange{change}); err != nil {
			t.Fatalf("SetSettings: %v", err)
		}
		if after := f.sinks.times(); after != before {
			t.Errorf("changing the policy handed the query path %d copies, want none", after-before)
		}
	})
}

func TestAZoneIsRebuiltAndADeletedOneDropped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.load(t)

	z := f.storeZone(t, "example.com.")
	f.pub.Applied(t.Context(), changed(z))
	if got := f.snaps.Snapshot().Zones(); got != 1 {
		t.Fatalf("after the zone was created the snapshot answers for %d zones, want 1", got)
	}

	f.update(t, func(tx store.Tx) error { return tx.DeleteZone(t.Context(), z.ID) })
	f.pub.Applied(t.Context(), changed(z))
	if got := f.snaps.Snapshot().Zones(); got != 0 {
		t.Errorf("after the zone was deleted the snapshot answers for %d zones, want 0", got)
	}
}

func TestAKeyThatNoLongerSignsIsLeftOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.load(t)

	newKey := func(name string) *store.TSIGKey {
		return &store.TSIGKey{
			ID: store.TSIGKeyID(id.New()), Name: zone.MustParseName(name),
			Algorithm: zone.HMACSHA256, Secret: make([]byte, zone.HMACSHA256.SecretBytes()),
		}
	}
	signing, withdrawn := newKey("ns2.example.com."), newKey("old.example.com.")
	f.update(t, func(tx store.Tx) error {
		if err := tx.CreateTSIGKey(t.Context(), signing); err != nil {
			return err
		}
		if err := tx.CreateTSIGKey(t.Context(), withdrawn); err != nil {
			return err
		}
		return tx.RevokeTSIGKey(t.Context(), withdrawn.ID, time.Now())
	})
	f.pub.Applied(t.Context(), apply.Applied{Keys: true})

	f.sinks.mu.Lock()
	defer f.sinks.mu.Unlock()
	if _, ok := f.sinks.keys[signing.Name]; !ok || len(f.sinks.keys) != 1 {
		t.Errorf("the keyring holds %v, want %s and nothing else", f.sinks.keys, signing.Name)
	}
}

// Rebuilding a zone reads the snapshot in force and installs one with the zone
// replaced. Two of those running into each other would each install their own,
// and the one installed second would carry the other's zone as it was before.
func TestChangesAtTheSameMomentAreAllKept(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.load(t)

	const zones = 64
	created := make([]*zone.Zone, zones)
	for i := range created {
		created[i] = f.storeZone(t, "z"+string(rune('a'+i%26))+string(rune('a'+i/26))+".example.")
	}

	var wg sync.WaitGroup
	for _, z := range created {
		wg.Go(func() { f.pub.Applied(t.Context(), changed(z)) })
	}
	wg.Wait()

	if got := f.snaps.Snapshot().Zones(); got != zones {
		t.Errorf("after %d changes at once the snapshot answers for %d zones", zones, got)
	}
}

// A batch that arrives before the first load has no copy to keep in step. The
// load reads everything the store holds by then, that batch included.
func TestNothingIsBuiltBeforeTheFirstLoad(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	z := f.storeZone(t, "example.com.")
	f.pub.Applied(t.Context(), changed(z))
	if f.snaps.Snapshot() != nil {
		t.Fatal("a batch before the first load built a snapshot out of one zone")
	}

	f.load(t)
	if got := f.snaps.Snapshot().Zones(); got != 1 {
		t.Errorf("the first load built a snapshot of %d zones, want the one the store holds", got)
	}
}
