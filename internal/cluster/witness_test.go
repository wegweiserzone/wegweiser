package cluster

import (
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
)

// The witnesses here are members with a store standing in for wegwitness:
// what is under test is how the cluster counts them, which goes by the
// identifier alone (docs/decisions/d48-a-witness-is-known-by-its-identifier.md).

func TestAWitnessJoinsAndIsCountedAsOne(t *testing.T) {
	t.Parallel()
	a, b, _ := threeVoters(t)
	w := startMember(t, WitnessPrefix+"w")

	if err := w.node.Join(t.Context(), a.addr, RoleWitness); err != nil {
		t.Fatalf("Join as a witness: %v", err)
	}
	if got, ok := suffrageOf(t, a, w.id); !ok || got != raft.Voter {
		t.Errorf("the leader counts the witness as %v (listed %v), want a voter", got, ok)
	}
	st, err := b.node.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, m := range st.Members {
		if (m.ID == w.id) != (m.Role == RoleWitness) {
			t.Errorf("member %s is listed as a %s", m.ID, m.Role)
		}
	}
}

// D39: witnesses stay fewer than half of the voters, on the way in and on
// the way out.
func TestWitnessesStayFewerThanHalfOfTheVoters(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	lone := startMember(t, WitnessPrefix+"lone")

	err := lone.node.Join(t.Context(), a.addr, RoleWitness)
	if err == nil || !strings.Contains(err.Error(), ErrWitnessMajority.Error()) {
		t.Errorf("a witness beside a single voter: %v, want it refused", err)
	}
	if _, ok := suffrageOf(t, a, lone.id); ok {
		t.Error("the refused witness was added without a vote all the same")
	}

	b := startMember(t, "b")
	if err := b.node.Join(t.Context(), a.addr, RoleVoter); err != nil {
		t.Fatalf("Join b: %v", err)
	}
	if err := lone.node.Join(t.Context(), a.addr, RoleWitness); err != nil {
		t.Fatalf("a witness beside two voters: %v", err)
	}
	if err := a.node.Remove("b"); !errors.Is(err, ErrWitnessMajority) {
		t.Errorf("removing a voter that leaves the witness half of the rest: %v, want ErrWitnessMajority", err)
	}
	if err := a.node.Remove(lone.id); err != nil {
		t.Errorf("removing the witness: %v", err)
	}
}

// D48: the role asked for and the identifier have to agree.
func TestARoleAndAnIdentifierThatDisagreeAreRefused(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")

	for _, req := range []JoinRequest{
		{ID: WitnessPrefix + "x", Address: unreachable(t), Role: RoleVoter},
		{ID: "x", Address: unreachable(t), Role: RoleWitness},
	} {
		reply, err := askToJoin(t.Context(), a.tr, a.addr, req)
		if err != nil {
			t.Fatalf("ask to join: %v", err)
		}
		if !strings.Contains(reply.Error, "nothing else does") {
			t.Errorf("%s as a %s: %+v, want it refused", req.ID, req.Role, reply)
		}
	}
}

func TestANodeWithAStoreIsNotNamedAsAWitness(t *testing.T) {
	t.Parallel()
	if _, err := Identity(t.Context(), newStore(t), WitnessPrefix+"ns1"); err == nil {
		t.Error("a node with a store took a witness's identifier")
	}
}
