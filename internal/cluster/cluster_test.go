package cluster

import (
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// The tests here run whole members over real sockets, and are about what a
// cluster does over time: losing its leader, taking on a member late, and a
// member coming back from a restart.

func propose(t *testing.T, m *member, apex string) {
	t.Helper()
	if err := m.node.Propose(t.Context(), zoneBatch(t, m.applier, apex)); err != nil {
		t.Fatalf("Propose %s on %s: %v", apex, m.id, err)
	}
}

func snapshot(t *testing.T, m *member) {
	t.Helper()
	if err := m.node.raft.Snapshot().Error(); err != nil {
		t.Fatalf("snapshot on %s: %v", m.id, err)
	}
}

func holds(t *testing.T, m *member, zones int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%s to hold %d zones", m.id, zones), func() bool {
		return zonesIn(t, m.store) == zones
	})
}

func leaderOf(t *testing.T, members ...*member) *member {
	t.Helper()
	var leader *member
	waitFor(t, "a leader", func() bool {
		for _, m := range members {
			if m.node.IsLeader() {
				leader = m
				return true
			}
		}
		return false
	})
	return leader
}

// startAlone brings up a member and has it start a cluster of its own.
func startAlone(t *testing.T, id string, opts ...func(*memberOptions)) *member {
	t.Helper()
	m := startMember(t, id, opts...)
	if err := m.node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap %s: %v", id, err)
	}
	waitFor(t, id+" to lead", m.node.IsLeader)
	return m
}

func TestTheClusterOutlivesItsLeader(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	b, c := startMember(t, "b"), startMember(t, "c")
	for _, m := range []*member{b, c} {
		if err := a.node.AddVoter(m.id, m.addr); err != nil {
			t.Fatalf("AddVoter %s: %v", m.id, err)
		}
	}
	propose(t, a, "one.example.")
	holds(t, b, 1)
	holds(t, c, 1)

	a.stop(t)
	next := leaderOf(t, b, c)
	propose(t, next, "two.example.")
	holds(t, b, 2)
	holds(t, c, 2)
}

// handedOver is how soon after its leader stops another member has to lead.
// Without a hand-over nobody stands for election before a heartbeat timeout
// has passed, so half of one tells the two apart.
var handedOver = raft.DefaultConfig().HeartbeatTimeout / 2

// leadsAtOnce is the one of members leading within handedOver, and fails the
// test when none of them does.
func leadsAtOnce(t *testing.T, members ...*member) *member {
	t.Helper()
	deadline := time.Now().Add(handedOver)
	for time.Now().Before(deadline) {
		for _, m := range members {
			if m.node.IsLeader() {
				return m
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("nobody leads %v after the leader stopped", handedOver)
	return nil
}

// A leader that stops hands leadership on, so writes are taken again without
// waiting for the others to notice it has gone.
func TestALeaderThatStopsHandsLeadershipOn(t *testing.T) {
	t.Parallel()
	a, b, c := threeVoters(t)

	a.stop(t)
	propose(t, leadsAtOnce(t, b, c), "two.example.")
}

// D48: a witness would only hand leadership on again, so it is passed over.
func TestALeaderThatStopsPassesOverAWitness(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	b, w := startMember(t, "b"), startMember(t, WitnessPrefix+"w")
	for _, m := range []*member{b, w} {
		if err := a.node.AddVoter(m.id, m.addr); err != nil {
			t.Fatalf("AddVoter %s: %v", m.id, err)
		}
	}
	propose(t, a, "one.example.")
	holds(t, b, 1)
	holds(t, w, 1)

	a.stop(t)
	if got := leadsAtOnce(t, b, w); got != b {
		t.Errorf("%s leads, want b: a witness hands leadership on again", got.id)
	}
}

// A member added after the log it would need is compacted away starts from a
// log snapshot, which is the path D30 exists for.
func TestALateMemberStartsFromASnapshot(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a", withTrailingLogs(1))
	for _, apex := range []string{"one.example.", "two.example.", "three.example."} {
		propose(t, a, apex)
	}
	snapshot(t, a)

	d := startMember(t, "d")
	if err := a.node.AddVoter("d", d.addr); err != nil {
		t.Fatalf("AddVoter d: %v", err)
	}
	holds(t, d, 3)
	if got := d.loads.n.Load(); got != 1 {
		t.Errorf("d loaded the query path %d times, want the once a restored snapshot takes", got)
	}

	propose(t, a, "four.example.")
	holds(t, d, 4)
}

// The store is on disk and knows how far it has got, so a restart replays the
// log over it instead of emptying it for a snapshot of what it already holds.
func TestARestartedMemberKeepsItsStore(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	propose(t, a, "one.example.")
	propose(t, a, "two.example.")
	snapshot(t, a)
	propose(t, a, "three.example.")

	a = a.restart(t, a.store)
	waitFor(t, "a to lead again", a.node.IsLeader)
	holds(t, a, 3)
	if got := a.loads.n.Load(); got != 0 {
		t.Errorf("the restarted member restored a snapshot %d times, want never: its store was current", got)
	}

	propose(t, a, "four.example.")
	holds(t, a, 4)
}

// An earlier database put back underneath a newer log is behind the snapshot,
// and the entries between the two may be gone from the log. Catching up on
// start is what fills that gap.
func TestAStoreBehindTheSnapshotCatchesUpOnStart(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	propose(t, a, "one.example.")
	propose(t, a, "two.example.")
	snapshot(t, a)
	propose(t, a, "three.example.")

	older := newStore(t)
	a = a.restart(t, older)
	holds(t, a, 3)
	if got := a.loads.n.Load(); got != 1 {
		t.Errorf("the member loaded the query path %d times, want the once catching up takes", got)
	}

	waitFor(t, "a to lead again", a.node.IsLeader)
	propose(t, a, "four.example.")
	holds(t, a, 4)
}
