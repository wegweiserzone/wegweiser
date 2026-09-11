package cluster

import (
	"fmt"
	"testing"
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
