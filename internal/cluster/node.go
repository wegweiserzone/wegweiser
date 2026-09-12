package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// A node's settings. Raft's own timeouts are its defaults for a local network,
// and like these they are fixed rather than configured, for the reason D29
// gives its retry count: an operator has nothing to choose them by, and a
// cluster spread over a wide area wants zone transfer instead
// (docs/decisions/d25-cluster-shape.md).
const (
	snapshotsKept     = 2
	streamPool        = 3
	streamTimeout     = 10 * time.Second
	membershipTimeout = 10 * time.Second
	proposeTimeout    = 10 * time.Second
)

// ErrNotLeader is a proposal made to a member that is not leading. It is the
// cue to forward the write (docs/decisions/d40-a-write-reaches-the-leader.md).
var ErrNotLeader = errors.New("cluster: this member is not the leader")

// NodeConfig is what a [Node] needs.
type NodeConfig struct {
	// ID identifies this member for as long as it is one
	// (docs/decisions/d42-membership-lives-in-the-log.md).
	ID string

	// Advertise is where the other members reach this one, as host:port.
	Advertise string

	// Dir is where Raft keeps its log and its snapshots. The store is kept
	// elsewhere; D24 says why Raft's log is not in it.
	Dir string

	Transport *Transport
	Mux       *Mux

	Store   store.Store
	Applier *apply.Applier
	Loader  Loader

	// Logger is where Raft's lines go. Nil discards them.
	Logger *slog.Logger

	// OnError hears about an entry this member could not carry out, and about
	// it leaving the cluster over one. May be nil.
	OnError func(error)

	// retry is how patiently an entry is tried before the member gives up on
	// it. The zero value is the fixed policy D29 calls for; tests shorten it.
	retry retryPolicy

	// trailingLogs is how many entries a snapshot leaves behind it. Zero is
	// Raft's default; a test lowers it to make a late member start from a
	// snapshot rather than from the log.
	trailingLogs uint64
}

// Node is this server's member of a cluster.
type Node struct {
	raft    *raft.Raft
	logs    *raftboltdb.BoltStore
	trans   *raft.NetworkTransport
	machine *fsm
	joins   net.Listener
	id      raft.ServerID
	addr    raft.ServerAddress
	report  func(error)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start brings the member up. A member that has never been part of a cluster
// holds no Raft state, and waits to be added by one that is, or to be told to
// start one with [Node.Bootstrap].
func Start(cfg NodeConfig) (_ *Node, err error) {
	switch {
	case cfg.ID == "":
		return nil, errors.New("cluster: a member needs an identifier (docs/decisions/d42-membership-lives-in-the-log.md)")
	case cfg.Advertise == "":
		return nil, errors.New("cluster: a member needs an address the others can reach it at")
	case cfg.Dir == "":
		return nil, errors.New("cluster: a member needs a directory for Raft's log")
	case cfg.Transport == nil, cfg.Mux == nil:
		return nil, errors.New("cluster: a member needs a transport and the port it serves")
	case cfg.Store == nil, cfg.Applier == nil, cfg.Loader == nil:
		return nil, errors.New("cluster: a member needs a store, an applier and a loader")
	}

	advertise, err := net.ResolveTCPAddr("tcp", cfg.Advertise)
	if err != nil {
		return nil, fmt.Errorf("cluster: the address to advertise: %w", err)
	}
	if merr := os.MkdirAll(cfg.Dir, 0o700); merr != nil {
		return nil, fmt.Errorf("cluster: the directory for Raft's log: %w", merr)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	report := cfg.OnError
	if report == nil {
		report = func(error) {}
	}
	rlog := newRaftLogger(logger)

	logs, err := raftboltdb.NewBoltStore(filepath.Join(cfg.Dir, "raft.db"))
	if err != nil {
		return nil, fmt.Errorf("cluster: open Raft's log: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, logs.Close())
		}
	}()
	snaps, err := raft.NewFileSnapshotStoreWithLogger(cfg.Dir, snapshotsKept, rlog)
	if err != nil {
		return nil, fmt.Errorf("cluster: open the log snapshots: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			cancel()
		}
	}()
	// Buffered, so that a member which stalls before anybody is watching still
	// leaves once somebody is.
	leaving := make(chan struct{}, 1)
	machine := &fsm{
		ctx: ctx, applier: cfg.Applier, store: cfg.Store, loader: cfg.Loader,
		report: report, retry: cfg.retry,
		stalled: func(Stall) {
			select {
			case leaving <- struct{}{}:
			default:
			}
		},
	}
	if cerr := catchUp(ctx, machine, cfg.Store, snaps); cerr != nil {
		return nil, cerr
	}

	trans := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream:  &raftStream{l: cfg.Mux.Listener(StreamRaft), t: cfg.Transport, addr: advertise},
		MaxPool: streamPool,
		Timeout: streamTimeout,
		Logger:  rlog,
	})
	defer func() {
		if err != nil {
			err = errors.Join(err, trans.Close())
		}
	}()

	conf := raft.DefaultConfig()
	conf.LocalID = raft.ServerID(cfg.ID)
	conf.Logger = rlog
	// The store is on disk and knows how far it has got, so Raft replays from
	// its last snapshot onward over it instead of restoring the snapshot first,
	// which would empty the store only to fill it with what it held.
	conf.NoSnapshotRestoreOnStart = true
	if cfg.trailingLogs > 0 {
		conf.TrailingLogs = cfg.trailingLogs
	}

	r, err := raft.NewRaft(conf, machine, logs, logs, snaps, trans)
	if err != nil {
		return nil, fmt.Errorf("cluster: start Raft: %w", err)
	}
	n := &Node{
		raft: r, logs: logs, trans: trans, machine: machine, joins: cfg.Mux.Listener(StreamJoin),
		id: conf.LocalID, addr: raft.ServerAddress(cfg.Advertise), report: report,
		ctx: ctx, cancel: cancel,
	}
	n.wg.Add(2)
	go n.watch(leaving)
	go n.serveJoins(n.joins)
	return n, nil
}

// watch waits for the state machine to give up on an entry, and then leaves.
func (n *Node) watch(leaving <-chan struct{}) {
	defer n.wg.Done()
	select {
	case <-leaving:
		n.leave()
	case <-n.ctx.Done():
	}
}

// leave is what a member does once it cannot go on applying the log
// (docs/decisions/d29-a-node-that-cannot-apply.md). It stops taking part in
// Raft and turns Raft streams away at its port, so that the others see it gone
// rather than slow. The query path is not touched, and nothing brings the
// member back by itself.
func (n *Node) leave() {
	if err := errors.Join(n.raft.Shutdown().Error(), n.trans.Close()); err != nil {
		n.report(fmt.Errorf("cluster: leave the cluster: %w", err))
	}
}

// Stalled reports whether this member has stopped applying the log, and where.
func (n *Node) Stalled() (Stall, bool) { return n.machine.stalledAt() }

// electionWait bounds how long [Node.Init] waits for a member alone in its
// new cluster to elect itself.
const electionWait = 30 * time.Second

// Replicating reports whether this member holds Raft state: it has started a
// cluster or been added to one. Until then it is an ordinary server (D44).
//
// A member that has stalled counts as replicating, so that a write made there
// is refused as one made on a member that is behind, rather than landing in
// its store unseen by any other.
func (n *Node) Replicating() bool {
	if _, ok := n.Stalled(); ok {
		return true
	}
	return n.raft.LastIndex() > 0
}

// Settle returns once this member may plan a write: it leads, and everything
// committed before now is applied here. A follower fails with [ErrNotLeader]
// before anything is planned
// (docs/decisions/d24-what-the-cluster-replicates.md).
func (n *Node) Settle(ctx context.Context) error {
	if s, ok := n.Stalled(); ok {
		return s.Err()
	}
	timeout := proposeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	if err := n.raft.Barrier(timeout).Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return ErrNotLeader
		}
		return fmt.Errorf("cluster: wait for the log to be applied here: %w", err)
	}
	return nil
}

// Init makes this member the first of a new cluster, with everything its store
// holds (docs/decisions/d44-starting-and-joining.md).
//
// What the store held until now is in no entry of the log, so a member that
// joins later could not replay it. Init therefore ends with a log snapshot and
// nothing of the log left in front of it, and every member that joins starts
// from that snapshot. Run it through [apply.Applier.Exclusive], so that no
// write lands between the log beginning and the snapshot being taken.
func (n *Node) Init(ctx context.Context) error {
	if err := n.Bootstrap(); err != nil {
		return fmt.Errorf("cluster: start a cluster: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, electionWait)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !n.IsLeader() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cluster: this member has not come to lead its new cluster: %w", ctx.Err())
		case <-tick.C:
		}
	}
	return n.snapshotEverything()
}

// snapshotEverything writes a log snapshot and leaves nothing of the log
// before it, so that the next member to join has to start from the snapshot.
func (n *Node) snapshotEverything() error {
	if err := n.raft.Barrier(proposeTimeout).Error(); err != nil {
		return fmt.Errorf("cluster: wait for the log to be applied here: %w", err)
	}
	was := n.raft.ReloadableConfig()
	none := was
	none.TrailingLogs = 0
	if err := n.raft.ReloadConfig(none); err != nil {
		return fmt.Errorf("cluster: keep no log behind the first snapshot: %w", err)
	}
	serr := n.raft.Snapshot().Error()
	if serr != nil {
		serr = fmt.Errorf("cluster: write the first log snapshot: %w", serr)
	}
	return errors.Join(serr, n.raft.ReloadConfig(was))
}

// catchUp restores the newest log snapshot when the store is behind it.
//
// Raft does not restore on start here (see [Start]), and what that would get
// wrong is a store older than the snapshot: an earlier database put back under
// a newer log. The entries between the two may already be compacted away, and
// this is what fills the gap.
func catchUp(ctx context.Context, f *fsm, st store.Store, snaps raft.SnapshotStore) error {
	metas, err := snaps.List()
	if err != nil {
		return fmt.Errorf("cluster: list the log snapshots: %w", err)
	}
	if len(metas) == 0 {
		return nil
	}
	applied, err := st.AppliedIndex(ctx)
	if err != nil {
		return err
	}
	if applied >= metas[0].Index {
		return nil
	}
	_, rc, err := snaps.Open(metas[0].ID)
	if err != nil {
		return fmt.Errorf("cluster: open log snapshot %s: %w", metas[0].ID, err)
	}
	return f.Restore(rc)
}

// Bootstrap starts a cluster with this member as its only voter. It is done
// once, by one member, and refused by one that already holds Raft state
// (docs/decisions/d42-membership-lives-in-the-log.md).
func (n *Node) Bootstrap() error {
	return n.raft.BootstrapCluster(raft.Configuration{Servers: []raft.Server{
		{Suffrage: raft.Voter, ID: n.id, Address: n.addr},
	}}).Error()
}

// AddVoter makes the member at addr a voter.
func (n *Node) AddVoter(id, addr string) error {
	return n.membership(n.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(addr), 0, membershipTimeout))
}

// AddNonvoter makes the member at addr one that receives the log and does not
// vote (docs/decisions/d25-cluster-shape.md).
func (n *Node) AddNonvoter(id, addr string) error {
	return n.membership(n.raft.AddNonvoter(raft.ServerID(id), raft.ServerAddress(addr), 0, membershipTimeout))
}

// Remove takes a member out of the cluster. Leaving is an act rather than an
// absence, and this is the act.
func (n *Node) Remove(id string) error {
	return n.membership(n.raft.RemoveServer(raft.ServerID(id), 0, membershipTimeout))
}

func (n *Node) membership(f raft.IndexFuture) error {
	if s, ok := n.Stalled(); ok {
		return s.Err()
	}
	if err := f.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return ErrNotLeader
		}
		return fmt.Errorf("cluster: change the membership: %w", err)
	}
	return nil
}

// Propose puts a batch into the log and waits until this member has applied
// it. Only the leader takes one; anywhere else it fails with [ErrNotLeader].
//
// A member that has stalled refuses it with an error saying that this member
// is behind, rather than that the cluster is.
func (n *Node) Propose(ctx context.Context, b *apply.Batch) error {
	if s, ok := n.Stalled(); ok {
		return s.Err()
	}
	if b.Empty() {
		return nil
	}
	data, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("cluster: encode the batch: %w", err)
	}
	timeout := proposeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}

	f := n.raft.Apply(data, timeout)
	if err := f.Error(); err != nil {
		if s, ok := n.Stalled(); ok {
			return s.Err()
		}
		if errors.Is(err, raft.ErrNotLeader) {
			return ErrNotLeader
		}
		return fmt.Errorf("cluster: propose the batch: %w", err)
	}
	if applyErr, ok := f.Response().(error); ok && applyErr != nil {
		return applyErr
	}
	return nil
}

// IsLeader reports whether this member is leading.
func (n *Node) IsLeader() bool { return n.raft.State() == raft.Leader }

// Leader names the member leading, as far as this one knows. Both are empty
// while there is none.
func (n *Node) Leader() (id, addr string) {
	a, i := n.raft.LeaderWithID()
	return string(i), string(a)
}

// Close stops taking part in the cluster. The query path is not touched: a
// member that stops is one that stops voting, not one that stops answering.
func (n *Node) Close() error {
	// First, so that an entry being retried gives up now rather than after a
	// minute, and so that the watch stops waiting for a stall.
	n.cancel()
	err := errors.Join(n.raft.Shutdown().Error(), n.joins.Close())
	n.wg.Wait()
	return errors.Join(err, n.trans.Close(), n.logs.Close())
}

// raftStream is the cluster port's Raft streams, as the stream layer Raft's
// network transport runs on.
type raftStream struct {
	l    net.Listener
	t    *Transport
	addr net.Addr
}

var _ raft.StreamLayer = (*raftStream)(nil)

func (s *raftStream) Accept() (net.Conn, error) { return s.l.Accept() }
func (s *raftStream) Close() error              { return s.l.Close() }

// Addr is the address this member advertises. Raft hands it to the others, so
// it is the one they can reach rather than the one the port is bound to.
func (s *raftStream) Addr() net.Addr { return s.addr }

func (s *raftStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.t.Dial(ctx, string(address), StreamRaft)
}
