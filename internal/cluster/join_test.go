package cluster

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
)

// suffrageOf reports how the leader's configuration counts a member, and
// whether it lists it at all.
func suffrageOf(t *testing.T, leader *member, id string) (raft.ServerSuffrage, bool) {
	t.Helper()
	f := leader.node.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	for _, s := range f.Configuration().Servers {
		if string(s.ID) == id {
			return s.Suffrage, true
		}
	}
	return 0, false
}

func TestANewMemberJoinsByAskingTheLeader(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	d := startMember(t, "d")

	propose(t, a, "example.com.")

	if err := d.node.Join(t.Context(), a.addr, RoleVoter); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if got, ok := suffrageOf(t, a, "d"); !ok || got != raft.Voter {
		t.Errorf("the leader counts d as %v (listed %v), want a voter", got, ok)
	}
	// Join returns once the log has reached the node, not merely once it is
	// in the configuration.
	if got := zonesIn(t, d.store); got != 1 {
		t.Errorf("d holds %d zones when Join returns, want the 1 committed before it asked", got)
	}
	propose(t, a, "two.example.")
	holds(t, d, 2)
}

// A voter that nobody can reach must not cost the cluster its quorum. Added
// as a voter straight away it would: two voters, a majority of two, and the
// one leader left could commit nothing, its removal included.
func TestAVoterNobodyCanReachLeavesTheQuorumAlone(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")

	reply, err := askToJoin(t.Context(), a.tr, a.addr,
		JoinRequest{ID: "lost", Address: unreachable(t), Role: RoleVoter})
	if err != nil {
		t.Fatalf("ask to join: %v", err)
	}
	if !reply.Staged {
		t.Fatalf("the answer was %+v, want the voter staged without a vote", reply)
	}
	if got, ok := suffrageOf(t, a, "lost"); !ok || got != raft.Nonvoter {
		t.Errorf("the leader counts the lost node as %v (listed %v), want a non-voter", got, ok)
	}
	propose(t, a, "example.com.")
	holds(t, a, 1)
}

// D44: a member that does not lead names the one that does, and the node
// asks again there.
func TestAskingAFollowerIsSentToTheLeader(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	b := startMember(t, "b")
	if err := a.node.AddVoter("b", b.addr); err != nil {
		t.Fatalf("AddVoter b: %v", err)
	}
	waitFor(t, "b to know who leads", func() bool { id, _ := b.node.Leader(); return id == "a" })

	e := startMember(t, "e")
	if err := e.node.Join(t.Context(), b.addr, RoleVoter); err != nil {
		t.Fatalf("Join by way of the follower b: %v", err)
	}
	if _, ok := suffrageOf(t, a, "e"); !ok {
		t.Error("e is not in the leader's configuration")
	}
}

func TestANodeCanJoinWithoutAVote(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	n := startMember(t, "n")

	if err := n.node.Join(t.Context(), a.addr, RoleNonvoter); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if got, ok := suffrageOf(t, a, "n"); !ok || got != raft.Nonvoter {
		t.Errorf("the leader counts n as %v (listed %v), want a non-voter", got, ok)
	}
	propose(t, a, "example.com.")
	holds(t, n, 1)
}

// The role is where a witness will say it is one (D44). Until the cluster
// can tell which voters are witnesses, it is refused as a role this build
// does not know.
func TestARoleThisBuildDoesNotKnowIsRefused(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	w := startMember(t, "w")

	err := w.node.Join(t.Context(), a.addr, Role("witness"))
	if err == nil || !strings.Contains(err.Error(), "not a role") {
		t.Errorf("Join as a witness = %v, want the role refused", err)
	}
}

// Holding the secret is what entitles a node to join (D43, D44), so a node
// without it is turned away at the port.
func TestANodeWithoutTheSecretCannotJoin(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	stranger, _ := newTransport(t, secretOf(8), 0)

	_, err := askToJoin(t.Context(), stranger, a.addr, JoinRequest{ID: "x", Address: "192.0.2.9:8054", Role: RoleVoter})
	if !errors.Is(err, ErrRefused) {
		t.Errorf("Join without the secret = %v, want it refused at the port", err)
	}
	if _, ok := suffrageOf(t, a, "x"); ok {
		t.Error("the stranger made it into the configuration")
	}
}

// unreachable is an address nothing listens on: a port that was bound, and
// let go of again.
func unreachable(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}
