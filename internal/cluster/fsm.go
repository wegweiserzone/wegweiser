package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/hashicorp/raft"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

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
}

var _ raft.ConfigurationStore = (*fsm)(nil)

// Apply carries out one committed entry. Raft hands over commands only;
// configuration entries arrive through [fsm.StoreConfiguration].
//
// What it returns reaches the leader that proposed the entry, through that
// leader's ApplyFuture, and is dropped on every other member.
func (f *fsm) Apply(entry *raft.Log) any {
	var b apply.Batch
	if err := json.Unmarshal(entry.Data, &b); err != nil {
		return f.failed(entry.Index, err)
	}
	if err := f.applier.ApplyBatchAt(f.ctx, &b, apply.Index(entry.Index)); err != nil {
		return f.failed(entry.Index, err)
	}
	return nil
}

// failed says that an entry could not be carried out.
//
// TODO(D29): a member that cannot apply a committed entry retries a bounded
// number of times, then leaves the cluster and goes on answering queries.
// Until that is built the failure is reported on this member and returned to
// the leader, and a follower that met it is behind without Raft knowing.
func (f *fsm) failed(index uint64, err error) error {
	err = fmt.Errorf("cluster: entry %d could not be applied: %w", index, err)
	f.report(err)
	return err
}

// StoreConfiguration moves the applied index past a configuration entry, so
// that it names the last entry this member has been through, whatever kind.
// A log snapshot taken before any command would otherwise claim position zero,
// which no restore accepts.
func (f *fsm) StoreConfiguration(index uint64, _ raft.Configuration) {
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
func (f *fsm) Restore(rc io.ReadCloser) (err error) {
	defer func() { err = errors.Join(err, rc.Close()) }()

	h, items, err := apply.ReadSnapshot(rc)
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	if h.Writer != apply.WriterStore {
		// TODO(D29): leave the cluster and keep answering, as for an entry
		// that cannot be applied.
		return fmt.Errorf("cluster: the log snapshot was written by a %s, which holds nothing; "+
			"restoring it would empty this member (docs/decisions/d39-the-witness.md)", h.Writer)
	}
	if rerr := f.store.ReplaceReplicated(f.ctx, h.Index, items); rerr != nil {
		return fmt.Errorf("cluster: restore the log snapshot: %w", rerr)
	}
	return f.loader.Load(f.ctx)
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
