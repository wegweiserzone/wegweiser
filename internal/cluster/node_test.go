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
	store   store.Store
	applier *apply.Applier
	addr    string
}

// memberOptions change how a test member is built.
type memberOptions struct {
	// store is what the member applies to; nil is a plain one.
	store store.Store
	// onError hears what the member reports; nil fails the test on anything.
	onError func(error)
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
	a := newApplier(t, st)
	tr, _ := newTransport(t, secretOf(7), 0)
	mux := serve(t, tr)

	n, err := Start(NodeConfig{
		ID: id, Advertise: mux.Addr().String(), Dir: t.TempDir(),
		Transport: tr, Mux: mux, Store: st, Applier: a, Loader: &loads{},
		OnError: onError, retry: quickly,
	})
	if err != nil {
		t.Fatalf("Start %s: %v", id, err)
	}
	t.Cleanup(func() {
		if cerr := n.Close(); cerr != nil {
			t.Errorf("close member %s: %v", id, cerr)
		}
	})
	return &member{id: id, node: n, store: st, applier: a, addr: mux.Addr().String()}
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
