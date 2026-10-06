package cluster

import (
	"errors"
	"testing"
)

// threeVoters starts a cluster of a, b and c, all voting, with one zone in it.
func threeVoters(t *testing.T) (a, b, c *member) {
	t.Helper()
	a = startAlone(t, "a")
	b, c = startMember(t, "b"), startMember(t, "c")
	for _, m := range []*member{b, c} {
		if err := a.node.AddVoter(m.id, m.addr); err != nil {
			t.Fatalf("AddVoter %s: %v", m.id, err)
		}
	}
	propose(t, a, "one.example.")
	holds(t, b, 1)
	holds(t, c, 1)
	return a, b, c
}

// D46: a member taken out keeps what it held, refuses writes, and says it has
// left, including after a restart.
func TestARemovedFollowerKnowsItHasLeft(t *testing.T) {
	t.Parallel()
	a, b, c := threeVoters(t)

	if err := a.node.Remove("b"); err != nil {
		t.Fatalf("Remove b: %v", err)
	}
	waitFor(t, "b to know it has left", func() bool { return errors.Is(b.node.Left(), ErrRemoved) })

	propose(t, a, "two.example.")
	holds(t, c, 2)
	if got := zonesIn(t, b.store); got != 1 {
		t.Errorf("b holds %d zones, want the 1 it held when it was removed", got)
	}
	if err := b.node.Propose(t.Context(), zoneBatch(t, b.applier, "three.example.")); !errors.Is(err, ErrRemoved) {
		t.Errorf("a write on b fails with %v, want it refused as made on a member that has left", err)
	}
	st, err := b.node.Status(t.Context())
	if err != nil || !st.Removed {
		t.Errorf("b's status = %+v, %v, want it removed", st, err)
	}

	b = b.restart(t, b.store)
	if err := b.node.Left(); !errors.Is(err, ErrRemoved) {
		t.Errorf("after a restart b says %v, want that it has left", err)
	}
}

// A leader may take itself out. It stops taking part, and the others elect
// one of their own and go on.
func TestALeaderCanLeave(t *testing.T) {
	t.Parallel()
	a, b, c := threeVoters(t)

	if err := a.node.Remove("a"); err != nil {
		t.Fatalf("a removing itself: %v", err)
	}
	if err := a.node.Left(); !errors.Is(err, ErrRemoved) {
		t.Errorf("a says %v once it has left, want ErrRemoved", err)
	}
	st, err := a.node.Status(t.Context())
	if err != nil || !st.Removed {
		t.Errorf("a's status = %+v, %v, want it removed", st, err)
	}

	next := leaderOf(t, b, c)
	propose(t, next, "two.example.")
	holds(t, b, 2)
	holds(t, c, 2)
	if got := zonesIn(t, a.store); got != 1 {
		t.Errorf("a holds %d zones, want the 1 it held when it left", got)
	}
}

func TestARemovalNeedsAMemberToRemoveAndLeavesAVoter(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	n := startMember(t, "n")
	if err := n.node.Join(t.Context(), a.addr, RoleNonvoter); err != nil {
		t.Fatalf("Join: %v", err)
	}

	if err := a.node.Remove("nobody"); !errors.Is(err, ErrNoMember) {
		t.Errorf("removing a member the cluster lacks: %v, want ErrNoMember", err)
	}
	if err := a.node.Remove("a"); !errors.Is(err, ErrLastVoter) {
		t.Errorf("removing the only voter: %v, want ErrLastVoter", err)
	}
	if err := a.node.Left(); err != nil {
		t.Errorf("a refused removal took a out: %v", err)
	}
	if err := a.node.Remove("n"); err != nil {
		t.Errorf("removing the non-voter: %v", err)
	}
}
