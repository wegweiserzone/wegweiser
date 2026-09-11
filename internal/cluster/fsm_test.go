package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func newStore(t *testing.T) store.Store {
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
	return st
}

func newApplier(t *testing.T, st store.Store) *apply.Applier {
	t.Helper()
	a, err := apply.New(st, apply.Options{})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}
	return a
}

// loads counts how often the whole store was copied into the query path.
type loads struct{ n atomic.Int32 }

func (l *loads) Load(context.Context) error {
	l.n.Add(1)
	return nil
}

// reports collects what a member said it could not do.
type reports struct {
	mu  sync.Mutex
	got []error
}

func (r *reports) hear(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, err)
}

func (r *reports) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// mentioning counts the reports that say something.
func (r *reports) mentioning(what string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, err := range r.got {
		if strings.Contains(err.Error(), what) {
			n++
		}
	}
	return n
}

// quickly is the retry policy tests run with: D29's shape, without the minute.
var quickly = retryPolicy{attempts: 3, first: time.Millisecond, ceiling: time.Millisecond}

var errDiskFull = errors.New("no space left on device")

// flakyStore fails its writes while armed, the way a full disk does.
type flakyStore struct {
	store.Store
	failures atomic.Int32
}

func (s *flakyStore) Update(ctx context.Context, fn func(store.Tx) error) error {
	for {
		n := s.failures.Load()
		if n <= 0 {
			return s.Store.Update(ctx, fn)
		}
		if s.failures.CompareAndSwap(n, n-1) {
			return errDiskFull
		}
	}
}

type machine struct {
	*fsm
	loads   *loads
	reports *reports
	stalls  *atomic.Int32
}

func newMachine(t *testing.T) machine {
	t.Helper()
	return newMachineOn(t, newStore(t))
}

// newMachineOn builds a state machine applying to st. Batches for it are
// planned elsewhere, so that a store that fails its writes fails the applying
// and not the planning.
func newMachineOn(t *testing.T, st store.Store) machine {
	t.Helper()
	m := machine{loads: &loads{}, reports: &reports{}, stalls: &atomic.Int32{}}
	m.fsm = &fsm{
		ctx: t.Context(), applier: newApplier(t, st), store: st,
		loader: m.loads, report: m.reports.hear, retry: quickly,
		stalled: func(Stall) { m.stalls.Add(1) },
	}
	return m
}

// zoneBatch plans the creation of a zone, apex NS and all, against a's store.
func zoneBatch(t *testing.T, a *apply.Applier, apex string) *apply.Batch {
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
	b, _, err := a.PlanCreateZone(t.Context(), &z, []zone.Record{ns},
		apply.Meta{Source: journal.SourceAPI, Actor: "test"})
	if err != nil {
		t.Fatalf("PlanCreateZone %s: %v", apex, err)
	}
	return b
}

func entry(t *testing.T, index uint64, b *apply.Batch) *raft.Log {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("encode the batch: %v", err)
	}
	return &raft.Log{Index: index, Type: raft.LogCommand, Data: data}
}

func zonesIn(t *testing.T, st store.Store) int {
	t.Helper()
	n := 0
	for _, err := range st.IterZones(t.Context()) {
		if err != nil {
			t.Fatalf("IterZones: %v", err)
		}
		n++
	}
	return n
}

func appliedIn(t *testing.T, st store.Store) uint64 {
	t.Helper()
	index, err := st.AppliedIndex(t.Context())
	if err != nil {
		t.Fatalf("AppliedIndex: %v", err)
	}
	return index
}

// memorySink is a snapshot sink that keeps what it was given.
type memorySink struct {
	bytes.Buffer
	cancelled bool
}

func (s *memorySink) ID() string            { return "memory" }
func (s *memorySink) Cancel() error         { s.cancelled = true; return nil }
func (s *memorySink) Close() error          { return nil }
func (s *memorySink) reader() io.ReadCloser { return io.NopCloser(bytes.NewReader(s.Bytes())) }

func persist(t *testing.T, snap raft.FSMSnapshot) *memorySink {
	t.Helper()
	sink := &memorySink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	return sink
}

func TestAnEntryIsAppliedOnceAtItsPosition(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	e := entry(t, 5, zoneBatch(t, m.applier, "example.com."))

	for range 2 {
		if resp := m.Apply(e); resp != nil {
			t.Fatalf("Apply: %v", resp)
		}
	}
	if got := zonesIn(t, m.store); got != 1 {
		t.Errorf("the store holds %d zones after the entry was applied twice, want 1", got)
	}
	if got := appliedIn(t, m.store); got != 5 {
		t.Errorf("the applied index is %d, want the entry's 5", got)
	}
}

func TestAConfigurationEntryMovesThePosition(t *testing.T) {
	t.Parallel()
	m := newMachine(t)

	m.StoreConfiguration(7, raft.Configuration{})
	if got := appliedIn(t, m.store); got != 7 {
		t.Errorf("the applied index is %d after configuration entry 7, want 7", got)
	}
}

func TestASnapshotRestoresOnAnotherMember(t *testing.T) {
	t.Parallel()
	from, into := newMachine(t), newMachine(t)
	if resp := from.Apply(entry(t, 3, zoneBatch(t, from.applier, "example.com."))); resp != nil {
		t.Fatalf("Apply: %v", resp)
	}
	snap, err := from.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if rerr := into.Restore(persist(t, snap).reader()); rerr != nil {
		t.Fatalf("Restore: %v", rerr)
	}
	if got := zonesIn(t, into.store); got != 1 {
		t.Errorf("the restored store holds %d zones, want 1", got)
	}
	if got := appliedIn(t, into.store); got != 3 {
		t.Errorf("the restored store is at %d, want 3", got)
	}
	if got := into.loads.n.Load(); got != 1 {
		t.Errorf("the query path was loaded %d times after the restore, want once", got)
	}
}

// Raft files a snapshot under the position it had when it asked for one, and
// entries go on being applied while the snapshot is written. The content can
// therefore be ahead of that position, and the snapshot is only safe because
// it names its own.
func TestASnapshotAheadOfItsPositionIsStillSafe(t *testing.T) {
	t.Parallel()
	from, into := newMachine(t), newMachine(t)
	if resp := from.Apply(entry(t, 3, zoneBatch(t, from.applier, "one.example."))); resp != nil {
		t.Fatalf("Apply 3: %v", resp)
	}
	snap, err := from.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	later := entry(t, 4, zoneBatch(t, from.applier, "two.example."))
	if resp := from.Apply(later); resp != nil {
		t.Fatalf("Apply 4: %v", resp)
	}

	if rerr := into.Restore(persist(t, snap).reader()); rerr != nil {
		t.Fatalf("Restore: %v", rerr)
	}
	if got := appliedIn(t, into.store); got != 4 {
		t.Fatalf("the restored store is at %d, want the 4 its content is current as of", got)
	}
	// What Raft does next: replay everything after the position it filed the
	// snapshot under, which includes an entry the content already holds.
	if resp := into.Apply(later); resp != nil {
		t.Fatalf("replaying entry 4: %v", resp)
	}
	if got := zonesIn(t, into.store); got != 2 {
		t.Errorf("after the replay the store holds %d zones, want 2", got)
	}
}

func TestAWitnessSnapshotIsNotRestored(t *testing.T) {
	t.Parallel()
	into := newMachine(t)
	if resp := into.Apply(entry(t, 2, zoneBatch(t, into.applier, "example.com."))); resp != nil {
		t.Fatalf("Apply: %v", resp)
	}

	sink := &memorySink{}
	if err := apply.WriteSnapshot(sink, apply.SnapshotHeader{Index: 9, Writer: apply.WriterWitness}, nil); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	err := into.Restore(sink.reader())
	if !errors.Is(err, ErrBehind) || !strings.Contains(err.Error(), "witness") {
		t.Fatalf("Restore of a witness's snapshot = %v, want this member stopped over the witness", err)
	}
	if s, ok := into.stalledAt(); !ok || s.Entry != 9 {
		t.Errorf("the member stalled at %+v, want at the snapshot's entry 9", s)
	}
	if got := zonesIn(t, into.store); got != 1 {
		t.Errorf("the refused restore left %d zones, want the 1 that was there", got)
	}
	if got := into.loads.n.Load(); got != 0 {
		t.Errorf("the query path was reloaded %d times after a refused restore, want never", got)
	}
}

// Trying again gives the same answer for an entry this build cannot read, so
// the member stops at once rather than after a minute of asking.
func TestAnEntryThatCannotBeReadStopsTheMemberAtOnce(t *testing.T) {
	t.Parallel()
	m := newMachine(t)

	resp := m.Apply(&raft.Log{Index: 2, Type: raft.LogCommand, Data: []byte("not a batch")})
	if err, ok := resp.(error); !ok || !errors.Is(err, ErrBehind) {
		t.Fatalf("Apply of an unreadable entry returned %v, want the member stopped", resp)
	}
	if got := m.reports.mentioning("trying again"); got != 0 {
		t.Errorf("the member tried an unreadable entry again %d times, want never", got)
	}
	if s, ok := m.stalledAt(); !ok || s.Entry != 2 {
		t.Errorf("the member stalled at %+v, want at entry 2", s)
	}
}

// A full disk that somebody empties is the failure D29 retries for.
func TestAWriteThatFailsForAWhileIsTriedAgain(t *testing.T) {
	t.Parallel()
	planner := newApplier(t, newStore(t))
	flaky := &flakyStore{Store: newStore(t)}
	m := newMachineOn(t, flaky)
	flaky.failures.Store(2)

	if resp := m.Apply(entry(t, 5, zoneBatch(t, planner, "example.com."))); resp != nil {
		t.Fatalf("Apply: %v", resp)
	}
	if got := zonesIn(t, m.store); got != 1 {
		t.Errorf("the store holds %d zones once the disk cleared, want 1", got)
	}
	if got := m.reports.mentioning("trying again"); got != 2 {
		t.Errorf("the member reported %d retries, want 2", got)
	}
	if _, ok := m.stalledAt(); ok {
		t.Error("the member stalled over a failure that cleared")
	}
}

func TestAWriteThatNeverSucceedsStopsTheMember(t *testing.T) {
	t.Parallel()
	planner := newApplier(t, newStore(t))
	flaky := &flakyStore{Store: newStore(t)}
	m := newMachineOn(t, flaky)
	flaky.failures.Store(1_000)

	resp := m.Apply(entry(t, 5, zoneBatch(t, planner, "one.example.")))
	if err, ok := resp.(error); !ok || !errors.Is(err, ErrBehind) || !errors.Is(err, errDiskFull) {
		t.Fatalf("Apply returned %v, want the member stopped over the full disk", resp)
	}
	if got := m.stalls.Load(); got != 1 {
		t.Errorf("the member was set leaving %d times, want once", got)
	}
	if got := m.reports.mentioning("leaving the cluster"); got != 1 {
		t.Errorf("the member said it was leaving %d times, want once", got)
	}

	// The disk clears, and it makes no difference: nothing after the gap is
	// applied, and the position stays where the member stopped.
	flaky.failures.Store(0)
	resp = m.Apply(entry(t, 6, zoneBatch(t, planner, "two.example.")))
	if err, ok := resp.(error); !ok || !errors.Is(err, ErrBehind) {
		t.Errorf("an entry after the stall returned %v, want it refused", resp)
	}
	m.StoreConfiguration(7, raft.Configuration{})
	if got := zonesIn(t, m.store); got != 0 {
		t.Errorf("the stalled member holds %d zones, want none of the entries after its stall", got)
	}
	if got := appliedIn(t, m.store); got != 0 {
		t.Errorf("the stalled member's position moved to %d, want it left where it stopped", got)
	}
	if s, ok := m.stalledAt(); !ok || s.Entry != 5 {
		t.Errorf("the member stalled at %+v, want at entry 5", s)
	}
}
