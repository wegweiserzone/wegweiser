package cluster

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// electionPatience is how long a test waits for Raft to settle. An election
// takes a second or two on Raft's defaults, which are the ones a node runs.
const electionPatience = 15 * time.Second

type member struct {
	id      string
	node    *Node
	tr      *Transport
	store   store.Store
	applier *apply.Applier
	addr    string
	dir     string
	loads   *loads
	closed  bool
}

// memberOptions change how a test member is built.
type memberOptions struct {
	// store is what the member applies to; nil is a plain one.
	store store.Store
	// onError hears what the member reports; nil fails the test on anything.
	onError func(error)
	// dir is Raft's directory; empty is a fresh one. A restart passes the old.
	dir string
	// trailingLogs is how many entries a snapshot leaves; zero is Raft's own.
	trailingLogs uint64
}

func withTrailingLogs(n uint64) func(*memberOptions) {
	return func(o *memberOptions) { o.trailingLogs = n }
}

// stop closes the member before the test ends.
func (m *member) stop(t *testing.T) {
	t.Helper()
	if err := m.node.Close(); err != nil {
		t.Errorf("close member %s: %v", m.id, err)
	}
	m.closed = true
}

// restart stops m and starts it again on the same Raft directory, applying to
// st: the same store for an ordinary restart, another one to put an older
// database back underneath the log. It listens on a new port, which only a
// member alone in its cluster gets away with.
func (m *member) restart(t *testing.T, st store.Store) *member {
	t.Helper()
	m.stop(t)
	return startMember(t, m.id, func(o *memberOptions) {
		o.store = st
		o.dir = m.dir
	})
}

func startMember(t *testing.T, id string, opts ...func(*memberOptions)) *member {
	t.Helper()
	var o memberOptions
	for _, opt := range opts {
		opt(&o)
	}
	st := o.store
	if st == nil {
		st = newStore(t)
	}
	onError := o.onError
	if onError == nil {
		onError = func(err error) { t.Errorf("member %s: %v", id, err) }
	}
	dir := o.dir
	if dir == "" {
		dir = t.TempDir()
	}
	// Writes made through this member's applier go through its node once the
	// node is in a cluster, the way `weg serve` wires them.
	repl := &Replication{}
	a, err := apply.New(st, apply.Options{Replication: repl})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}
	tr, _ := newTransport(t, secretOf(7), 0)
	mux := serve(t, tr)
	ld := &loads{}

	n, err := Start(NodeConfig{
		ID: id, Advertise: mux.Addr().String(), Dir: dir,
		Transport: tr, Mux: mux, Store: st, Applier: a, Loader: ld,
		OnError: onError, retry: quickly, trailingLogs: o.trailingLogs,
	})
	if err != nil {
		t.Fatalf("Start %s: %v", id, err)
	}
	repl.Bind(n)
	m := &member{
		id: id, node: n, tr: tr, store: st, applier: a, addr: mux.Addr().String(), dir: dir, loads: ld,
	}
	t.Cleanup(func() {
		if !m.closed {
			m.stop(t)
		}
	})
	return m
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(electionPatience)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("still waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAMemberThatStartsAClusterLeadsIt(t *testing.T) {
	t.Parallel()
	a := startMember(t, "a")

	if err := a.node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	waitFor(t, "a to lead", a.node.IsLeader)

	if err := a.node.Propose(t.Context(), zoneBatch(t, a.applier, "example.com.")); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got := zonesIn(t, a.store); got != 1 {
		t.Errorf("the leader holds %d zones after the proposal, want 1", got)
	}

	// Once, by one member (D42).
	if err := a.node.Bootstrap(); !errors.Is(err, raft.ErrCantBootstrap) {
		t.Errorf("a second Bootstrap = %v, want it refused", err)
	}
}

func TestAChangeReachesEveryMember(t *testing.T) {
	t.Parallel()
	a, b, c := startMember(t, "a"), startMember(t, "b"), startMember(t, "c")

	if err := a.node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	waitFor(t, "a to lead", a.node.IsLeader)
	for _, m := range []*member{b, c} {
		if err := a.node.AddVoter(m.id, m.addr); err != nil {
			t.Fatalf("AddVoter %s: %v", m.id, err)
		}
	}

	if err := a.node.Propose(t.Context(), zoneBatch(t, a.applier, "example.com.")); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	for _, m := range []*member{b, c} {
		waitFor(t, fmt.Sprintf("the zone to reach %s", m.id), func() bool { return zonesIn(t, m.store) == 1 })
		waitFor(t, fmt.Sprintf("%s to know who leads", m.id), func() bool {
			id, _ := m.node.Leader()
			return id == "a"
		})
	}

	if err := b.node.Propose(t.Context(), zoneBatch(t, b.applier, "other.example.")); !errors.Is(err, ErrNotLeader) {
		t.Errorf("a proposal to a follower = %v, want it told it is not the leader", err)
	}
}

// D29 from the outside: a member that cannot apply an entry leaves, says where
// it stopped, refuses writes as the one that is behind, and the others carry on.
func TestAMemberThatCannotApplyLeavesAndTheRestGoOn(t *testing.T) {
	t.Parallel()
	flaky := &flakyStore{Store: newStore(t)}
	heard := &reports{}
	a, b := startMember(t, "a"), startMember(t, "b")
	c := startMember(t, "c", func(o *memberOptions) {
		o.store = flaky
		o.onError = heard.hear
	})

	if err := a.node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	waitFor(t, "a to lead", a.node.IsLeader)
	for _, m := range []*member{b, c} {
		if err := a.node.AddVoter(m.id, m.addr); err != nil {
			t.Fatalf("AddVoter %s: %v", m.id, err)
		}
	}
	// Every configuration entry through c before its disk fills, so that what
	// it stalls on is the zone and nothing earlier.
	waitFor(t, "c to catch up", func() bool { return appliedIn(t, c.store) >= appliedIn(t, a.store) })
	flaky.failures.Store(1_000_000)

	if err := a.node.Propose(t.Context(), zoneBatch(t, a.applier, "one.example.")); err != nil {
		t.Fatalf("Propose while c is failing: %v", err)
	}
	waitFor(t, "c to stall", func() bool { _, ok := c.node.Stalled(); return ok })

	// Leaving is Raft stopping on c, not merely c refusing to apply.
	waitFor(t, "c to leave Raft", func() bool { return c.node.raft.State() == raft.Shutdown })

	s, _ := c.node.Stalled()
	if !errors.Is(s.Reason, errDiskFull) {
		t.Errorf("c stalled over %v, want the full disk", s.Reason)
	}
	if heard.mentioning("leaving the cluster") != 1 {
		t.Errorf("c did not say it was leaving; it reported %d things", heard.count())
	}
	// Planned where the disk is fine: what is under test is that c refuses it.
	if err := c.node.Propose(t.Context(), zoneBatch(t, a.applier, "mine.example.")); !errors.Is(err, ErrBehind) {
		t.Errorf("a write to c = %v, want it refused as the member that is behind", err)
	}

	// Two of three is still a quorum, and the cluster does not wait for c.
	if err := a.node.Propose(t.Context(), zoneBatch(t, a.applier, "two.example.")); err != nil {
		t.Fatalf("Propose after c left: %v", err)
	}
	waitFor(t, "both zones to reach b", func() bool { return zonesIn(t, b.store) == 2 })
	if got := zonesIn(t, c.store); got != 0 {
		t.Errorf("c holds %d zones, want none past the entry it stopped at", got)
	}
}
