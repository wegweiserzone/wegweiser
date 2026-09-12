package cluster

import (
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
)

// asking is the request a test member makes to join.
func asking(m *member, role Role) JoinRequest {
	return JoinRequest{ID: m.id, Address: m.addr, Role: role}
}

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

	if err := Join(t.Context(), d.tr, a.addr, asking(d, RoleVoter)); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if got, ok := suffrageOf(t, a, "d"); !ok || got != raft.Voter {
		t.Errorf("the leader counts d as %v (listed %v), want a voter", got, ok)
	}
	propose(t, a, "example.com.")
	holds(t, d, 1)
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
	if err := Join(t.Context(), e.tr, b.addr, asking(e, RoleVoter)); err != nil {
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

	if err := Join(t.Context(), n.tr, a.addr, asking(n, RoleNonvoter)); err != nil {
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

	err := Join(t.Context(), w.tr, a.addr, asking(w, Role("witness")))
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

	err := Join(t.Context(), stranger, a.addr, JoinRequest{ID: "x", Address: "192.0.2.9:8054", Role: RoleVoter})
	if !errors.Is(err, ErrRefused) {
		t.Errorf("Join without the secret = %v, want it refused at the port", err)
	}
	if _, ok := suffrageOf(t, a, "x"); ok {
		t.Error("the stranger made it into the configuration")
	}
}
