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

func startMember(t *testing.T, id string) *member {
	t.Helper()
	st := newStore(t)
	a := newApplier(t, st)
	tr, _ := newTransport(t, secretOf(7), 0)
	mux := serve(t, tr)

	n, err := Start(NodeConfig{
		ID: id, Advertise: mux.Addr().String(), Dir: t.TempDir(),
		Transport: tr, Mux: mux, Store: st, Applier: a, Loader: &loads{},
		OnError: func(err error) { t.Errorf("member %s: %v", id, err) },
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
