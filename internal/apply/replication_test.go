package apply

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// fakeLog stands in for a cluster's log. What it is handed it applies here,
// the way the leader's own state machine would, so that the next plan reads it.
type fakeLog struct {
	mu          sync.Mutex
	a           *Applier
	replicating bool
	settleErr   error
	index       Index
	settles     int
	proposals   int

	// hold, when set, keeps the next proposal waiting until it is closed, and
	// holding is closed once that proposal is waiting.
	hold    chan struct{}
	holding chan struct{}
}

func (f *fakeLog) Replicating() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replicating
}

func (f *fakeLog) Settle(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settles++
	return f.settleErr
}

func (f *fakeLog) Propose(ctx context.Context, b *Batch) error {
	f.mu.Lock()
	hold, holding := f.hold, f.holding
	f.hold, f.holding = nil, nil
	f.mu.Unlock()
	if hold != nil {
		close(holding)
		<-hold
	}

	f.mu.Lock()
	f.index++
	at := f.index
	f.proposals++
	f.mu.Unlock()
	return f.a.ApplyBatchAt(ctx, b, at)
}

func (f *fakeLog) counts() (settles, proposals int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settles, f.proposals
}

func newReplicatedApplier(t *testing.T, f *fakeLog) *Applier {
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
	a, err := New(st, Options{Replication: f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.a = a
	return a
}

// replMeta is who the writes in these tests are made by.
var replMeta = Meta{Source: journal.SourceAPI, Actor: "test"}

// replZone is a zone ready to create, apex NS and all.
func replZone(t *testing.T, apex string) (*zone.Zone, []zone.Record) {
	t.Helper()
	z, err := zone.NewZone(zone.MustParseName(apex), zone.DefaultSOA(
		zone.MustParseName("ns1.example.com."), zone.MustParseName("hostmaster.example.com.")))
	if err != nil {
		t.Fatalf("NewZone: %v", err)
	}
	z.ID = zone.ZoneID(id.New())
	ns, err := zone.NewRecord(z.ID, z.Name, zone.ClassIN, zone.TypeNS, 3600, "ns1.example.com.")
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	return &z, []zone.Record{ns}
}

func zonesHeld(t *testing.T, a *Applier) int {
	t.Helper()
	n := 0
	for _, err := range a.store.IterZones(t.Context()) {
		if err != nil {
			t.Fatalf("IterZones: %v", err)
		}
		n++
	}
	return n
}

func within(t *testing.T, what string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish", what)
	}
}

func TestANodeThatDoesNotReplicateWritesItsOwnStore(t *testing.T) {
	t.Parallel()
	f := &fakeLog{}
	a := newReplicatedApplier(t, f)

	z, rr := replZone(t, "example.com.")
	if _, err := a.CreateZone(t.Context(), z, rr, replMeta); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if settles, proposals := f.counts(); settles != 0 || proposals != 0 {
		t.Errorf("a node outside any cluster settled %d times and proposed %d, want neither", settles, proposals)
	}
	if got := zonesHeld(t, a); got != 1 {
		t.Errorf("the store holds %d zones, want the one written to it", got)
	}
}

func TestAReplicatingNodeProposesWhatItPlans(t *testing.T) {
	t.Parallel()
	f := &fakeLog{replicating: true}
	a := newReplicatedApplier(t, f)

	z, rr := replZone(t, "example.com.")
	if _, err := a.CreateZone(t.Context(), z, rr, replMeta); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if settles, proposals := f.counts(); settles != 1 || proposals != 1 {
		t.Errorf("the write settled %d times and proposed %d, want once each", settles, proposals)
	}
	if got := zonesHeld(t, a); got != 1 {
		t.Errorf("the store holds %d zones once the log applied the proposal, want 1", got)
	}
}

// A follower cannot settle, and a write that cannot settle is refused before
// it has read anything: a plan made there would be a plan made against a
// state the leader may already have moved past.
func TestAWriteThatCannotSettleIsNeverPlanned(t *testing.T) {
	t.Parallel()
	errFollower := errors.New("this member is not the leader")
	f := &fakeLog{replicating: true, settleErr: errFollower}
	a := newReplicatedApplier(t, f)

	z, rr := replZone(t, "example.com.")
	if _, err := a.CreateZone(t.Context(), z, rr, replMeta); !errors.Is(err, errFollower) {
		t.Fatalf("CreateZone = %v, want the settling error", err)
	}
	if _, proposals := f.counts(); proposals != 0 {
		t.Errorf("a write that could not settle proposed %d batches, want none", proposals)
	}
	if got := zonesHeld(t, a); got != 0 {
		t.Errorf("the store holds %d zones, want none", got)
	}
}

// A reverse zone's creation ends with a reconcile, and a cookie rotation is a
// settings write. Each runs a write on behalf of another, which would wait
// forever for the lock its caller holds if it took it again.
func TestAWriteMadeOnBehalfOfAnotherDoesNotWaitForIt(t *testing.T) {
	t.Parallel()
	f := &fakeLog{replicating: true}
	a := newReplicatedApplier(t, f)
	rev, rr := replZone(t, "2.0.192.in-addr.arpa.")

	done := make(chan error, 1)
	go func() {
		_, cerr := a.CreateZone(t.Context(), rev, rr, replMeta)
		_, _, rerr := a.RotateCookieSecrets(t.Context())
		done <- errors.Join(cerr, rerr)
	}()
	within(t, "a reverse zone's creation and a cookie rotation", done)

	if settles, _ := f.counts(); settles != 2 {
		t.Errorf("the two writes settled %d times, want once each and not again for what they ran", settles)
	}
}

func TestWritesWithAClusterPlanOneAtATime(t *testing.T) {
	t.Parallel()
	hold, holding := make(chan struct{}), make(chan struct{})
	f := &fakeLog{replicating: true, hold: hold, holding: holding}
	a := newReplicatedApplier(t, f)
	one, oneNS := replZone(t, "one.example.")
	two, twoNS := replZone(t, "two.example.")

	first := make(chan error, 1)
	go func() {
		_, err := a.CreateZone(t.Context(), one, oneNS, replMeta)
		first <- err
	}()
	<-holding

	second := make(chan error, 1)
	go func() {
		_, err := a.CreateZone(t.Context(), two, twoNS, replMeta)
		second <- err
	}()
	// Nothing to wait on but time: what is under test is that something does
	// not happen while the first write is still being proposed.
	time.Sleep(100 * time.Millisecond)
	if settles, _ := f.counts(); settles != 1 {
		t.Errorf("%d writes settled while the first was still being proposed, want only the first", settles)
	}

	close(hold)
	within(t, "the first write", first)
	within(t, "the second write", second)
	if got := zonesHeld(t, a); got != 2 {
		t.Errorf("the store holds %d zones, want both", got)
	}
}

func TestNothingIsWrittenWhileExclusiveRuns(t *testing.T) {
	t.Parallel()
	f := &fakeLog{}
	a := newReplicatedApplier(t, f)
	z, rr := replZone(t, "outside.example.")
	in, inNS := replZone(t, "inside.example.")

	inside, release := make(chan struct{}), make(chan struct{})
	exclusive := make(chan error, 1)
	go func() {
		exclusive <- a.Exclusive(t.Context(), func(ctx context.Context) error {
			// A write made from inside is made on its behalf, and goes through.
			if _, err := a.CreateZone(ctx, in, inNS, replMeta); err != nil {
				return err
			}
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside

	outside := make(chan error, 1)
	go func() {
		_, err := a.CreateZone(t.Context(), z, rr, replMeta)
		outside <- err
	}()
	select {
	case <-outside:
		t.Fatal("a write went through while Exclusive held the lock")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	within(t, "Exclusive", exclusive)
	within(t, "the write that waited for it", outside)
	if got := zonesHeld(t, a); got != 2 {
		t.Errorf("the store holds %d zones, want both", got)
	}
}
