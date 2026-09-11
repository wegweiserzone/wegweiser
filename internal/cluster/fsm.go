package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// How a member retries an entry it could not apply before it gives up on it
// (docs/decisions/d29-a-node-that-cannot-apply.md). Fixed rather than
// configured, because an operator has nothing to choose them by. Six attempts
// over about a minute is long enough for a full disk to be noticed and
// emptied, and short enough that a member which will never get there says so
// soon.
const (
	applyAttempts     = 6
	applyFirstBackoff = 2 * time.Second
	applyBackoffCap   = 30 * time.Second
)

// retryPolicy is how often and how patiently an entry is tried. The zero value
// is the one a member runs with; tests shorten it.
type retryPolicy struct {
	attempts int
	first    time.Duration
	ceiling  time.Duration
}

func (p retryPolicy) orDefault() retryPolicy {
	if p.attempts <= 0 {
		p.attempts = applyAttempts
	}
	if p.first <= 0 {
		p.first = applyFirstBackoff
	}
	if p.ceiling <= 0 {
		p.ceiling = applyBackoffCap
	}
	return p
}

// ErrBehind is a member that has stopped applying the log and left the
// cluster. What it holds is correct as of where it stopped, and the cluster
// may well be fine without it.
var ErrBehind = errors.New("cluster: this member has left the cluster and is behind it")

// Stall is where a member stopped applying the log, and why.
type Stall struct {
	// Entry is the log position it could not get past. Zero is a log
	// snapshot that could not be read far enough to say where it stood.
	Entry uint64

	// Reason is what went wrong there.
	Reason error

	// At is when this member gave up. It is this member's clock, and says
	// nothing about the cluster, which is why it may be read here.
	At time.Time
}

// Err is what a write arriving at a stalled member is refused with. It says
// that this member is behind, not that the cluster is.
func (s Stall) Err() error {
	where := fmt.Sprintf("entry %d", s.Entry)
	if s.Entry == 0 {
		where = "a log snapshot"
	}
	// Both are wrapped, so that whoever is refused can ask why as well as what.
	return fmt.Errorf("%w: it stopped at %s (%w); send writes to another member, and repair this "+
		"one by removing it from the cluster, discarding its store and its Raft directory, and "+
		"joining it again (docs/decisions/d29-a-node-that-cannot-apply.md)", ErrBehind, where, s.Reason)
}

// Loader copies the whole store into the query path. A restored log snapshot
// replaces the store wholesale, so what follows it is a load rather than a
// rebuild of the zones one batch named
// (docs/decisions/d41-what-follows-applying-a-batch.md). A *publish.Publisher
// is one.
type Loader interface {
	Load(ctx context.Context) error
}

// fsm is the state machine Raft drives: the applier, fed batches in log order.
type fsm struct {
	ctx     context.Context
	applier *apply.Applier
	store   store.Store
	loader  Loader
	report  func(error)
	retry   retryPolicy

	// stalled hears once, when this member stops. May be nil.
	stalled func(Stall)

	mu    sync.Mutex
	stall *Stall
}

var _ raft.ConfigurationStore = (*fsm)(nil)

// Apply carries out one committed entry. Raft hands over commands only;
// configuration entries arrive through [fsm.StoreConfiguration].
//
// What it returns reaches the leader that proposed the entry, through that
// leader's ApplyFuture, and is dropped on every other member. That is why a
// member that cannot apply an entry stops rather than returning an error and
// carrying on: Raft would advance past the entry either way.
func (f *fsm) Apply(entry *raft.Log) any {
	if s, ok := f.stalledAt(); ok {
		// Nothing after a gap is applied. A member that skipped an entry and
		// carried on would hold a state no other member has ever had.
		return s.Err()
	}

	var b apply.Batch
	if err := json.Unmarshal(entry.Data, &b); err != nil {
		// The same bytes read by the same build give the same answer however
		// often they are tried. This is a rolling upgrade done in the wrong
		// order, or something worse, and waiting changes neither.
		return f.stop(entry.Index, fmt.Errorf("this build cannot read the entry: %w", err))
	}
	if err := f.tryApply(entry.Index, &b); err != nil {
		if f.ctx.Err() != nil {
			// The member is shutting down, which is not a stall. Raft replays
			// the entry the next time it starts.
			return err
		}
		return f.stop(entry.Index, err)
	}
	return nil
}

// tryApply carries an entry out, and tries again for a while when that fails.
// Each attempt is a transaction of its own, so one that fails leaves nothing
// behind for the next to trip over.
func (f *fsm) tryApply(index uint64, b *apply.Batch) error {
	p := f.retry.orDefault()
	wait := p.first
	for attempt := 1; ; attempt++ {
		err := f.applier.ApplyBatchAt(f.ctx, b, apply.Index(index))
		if err == nil || attempt == p.attempts || f.ctx.Err() != nil {
			return err
		}
		f.report(fmt.Errorf("cluster: entry %d could not be applied (attempt %d of %d), trying again in %s: %w",
			index, attempt, p.attempts, wait, err))
		select {
		case <-f.ctx.Done():
			return errors.Join(err, f.ctx.Err())
		case <-time.After(wait):
		}
		wait = min(wait*2, p.ceiling)
	}
}

// stop records that this member cannot get past an entry, and sets it leaving
// the cluster. Only the first stall counts: every entry after it is refused
// on that one's account.
func (f *fsm) stop(index uint64, why error) error {
	f.mu.Lock()
	first := f.stall == nil
	if first {
		f.stall = &Stall{Entry: index, Reason: why, At: time.Now()}
	}
	s := *f.stall
	f.mu.Unlock()

	if first {
		f.report(fmt.Errorf("cluster: this member cannot get past entry %d and is leaving the cluster; "+
			"it goes on answering queries with what it holds: %w", index, why))
		if f.stalled != nil {
			f.stalled(s)
		}
	}
	return s.Err()
}

// stalledAt reports where this member stopped, if it has.
func (f *fsm) stalledAt() (Stall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stall == nil {
		return Stall{}, false
	}
	return *f.stall, true
}

// StoreConfiguration moves the applied index past a configuration entry, so
// that it names the last entry this member has been through, whatever kind.
// A log snapshot taken before any command would otherwise claim position zero,
// which no restore accepts.
//
// A stalled member leaves the index where it stopped. Moving it past a gap
// would claim entries this member never applied.
func (f *fsm) StoreConfiguration(index uint64, _ raft.Configuration) {
	if _, ok := f.stalledAt(); ok {
		return
	}
	if err := f.applier.ApplyBatchAt(f.ctx, &apply.Batch{}, apply.Index(index)); err != nil {
		f.report(fmt.Errorf("cluster: record configuration entry %d as applied: %w", index, err))
	}
}

// Snapshot hands Raft something to write the store out with. The work happens
// in [fsmSnapshot.Persist], which Raft runs while later entries are applied.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	return &fsmSnapshot{ctx: f.ctx, store: f.store}, nil
}

// Restore replaces the store with a log snapshot and loads the query path from
// what it now holds (docs/decisions/d30-what-a-log-snapshot-contains.md).
//
// A snapshot this member cannot restore is a stall like an entry it cannot
// apply. That includes one a witness wrote, which holds nothing and would
// empty this member if it were taken (docs/decisions/d39-the-witness.md).
func (f *fsm) Restore(rc io.ReadCloser) (err error) {
	defer func() { err = errors.Join(err, rc.Close()) }()
	if s, ok := f.stalledAt(); ok {
		return s.Err()
	}

	h, items, err := apply.ReadSnapshot(rc)
	if err != nil {
		return f.stop(0, fmt.Errorf("the log snapshot cannot be read: %w", err))
	}
	if h.Writer != apply.WriterStore {
		return f.stop(h.Index, fmt.Errorf("the log snapshot was written by a %s, which holds nothing; "+
			"restoring it would empty this member", h.Writer))
	}
	if rerr := f.store.ReplaceReplicated(f.ctx, h.Index, items); rerr != nil {
		if f.ctx.Err() != nil {
			return rerr
		}
		return f.stop(h.Index, fmt.Errorf("restore the log snapshot: %w", rerr))
	}

	// The store is right from here on. A query path that could not be loaded
	// from it is stale rather than wrong, and that is reported the way a
	// failed publish after a batch is, not treated as a restore that failed.
	if lerr := f.loader.Load(f.ctx); lerr != nil {
		f.report(fmt.Errorf("cluster: the log snapshot is restored but the query path still answers "+
			"from before it; restart to rebuild it: %w", lerr))
	}
	return nil
}

// fsmSnapshot writes the store out as a log snapshot.
type fsmSnapshot struct {
	ctx   context.Context
	store store.Store
}

// Persist writes the store out in one read transaction.
//
// It runs while later entries are being applied, so what it reads can be ahead
// of the position Raft files the snapshot under. That is safe because the
// snapshot names its own position, read in the same transaction as its
// content, and a member restoring it skips every entry up to there when Raft
// replays the ones after its own index.
func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	err := s.store.View(s.ctx, func(r store.Reader) error {
		index, ierr := r.AppliedIndex(s.ctx)
		if ierr != nil {
			return ierr
		}
		return apply.WriteSnapshot(sink,
			apply.SnapshotHeader{Index: index, Writer: apply.WriterStore}, r.ExportReplicated(s.ctx))
	})
	if err != nil {
		return errors.Join(fmt.Errorf("cluster: write a log snapshot: %w", err), sink.Cancel())
	}
	return sink.Close()
}

// Release has nothing to let go of: the snapshot holds no transaction open
// between calls.
func (s *fsmSnapshot) Release() {}
