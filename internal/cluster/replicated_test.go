package cluster

import (
	"errors"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// createZoneVia creates a zone through m's applier, the way the API does.
func createZoneVia(t *testing.T, m *member, apex string) error {
	t.Helper()
	z, err := zone.NewZone(zone.MustParseName(apex), zone.DefaultSOA(
		zone.MustParseName("ns1.example.com."), zone.MustParseName("hostmaster.example.com.")))
	if err != nil {
		t.Fatalf("NewZone: %v", err)
	}
	z.ID = zone.ZoneID(id.New())
	ns, err := zone.NewRecord(z.ID, z.Name, zone.ClassIN, zone.TypeNS, 3600, "ns1.example.com.")
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	_, err = m.applier.CreateZone(t.Context(), &z, []zone.Record{ns},
		apply.Meta{Source: journal.SourceAPI, Actor: "test"})
	return err
}

func TestAWriteThroughTheLeadersApplierReachesEveryMember(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	b := startMember(t, "b")
	if err := a.node.AddVoter("b", b.addr); err != nil {
		t.Fatalf("AddVoter b: %v", err)
	}

	if err := createZoneVia(t, a, "example.com."); err != nil {
		t.Fatalf("CreateZone on the leader: %v", err)
	}
	holds(t, a, 1)
	holds(t, b, 1)
}

func TestAWriteOnAFollowerIsRefusedBeforeItIsPlanned(t *testing.T) {
	t.Parallel()
	a := startAlone(t, "a")
	b := startMember(t, "b")
	if err := a.node.AddVoter("b", b.addr); err != nil {
		t.Fatalf("AddVoter b: %v", err)
	}
	waitFor(t, "b to know who leads", func() bool { leader, _ := b.node.Leader(); return leader == "a" })

	if err := createZoneVia(t, b, "example.com."); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("CreateZone on the follower = %v, want it told it is not the leader", err)
	}
	if got := zonesIn(t, b.store) + zonesIn(t, a.store); got != 0 {
		t.Errorf("the refused write left %d zones behind, want none anywhere", got)
	}
}

// D44: a single node that has been running for a year becomes the first member
// of a cluster with everything it holds. None of it is in the log, so a member
// that joins later can only have it from the snapshot Init ends with.
func TestAFounderBringsWhatItHeldBeforeInit(t *testing.T) {
	t.Parallel()
	f := startMember(t, "f")
	for _, apex := range []string{"one.example.", "two.example."} {
		if err := createZoneVia(t, f, apex); err != nil {
			t.Fatalf("CreateZone %s before the cluster exists: %v", apex, err)
		}
	}
	if f.node.Replicating() {
		t.Fatal("a member that has started no cluster already replicates")
	}

	if err := f.applier.Exclusive(t.Context(), f.node.Init); err != nil {
		t.Fatalf("Init: %v", err)
	}
	j := startMember(t, "j")
	if err := Join(t.Context(), j.tr, f.addr, asking(j, RoleVoter)); err != nil {
		t.Fatalf("Join: %v", err)
	}
	holds(t, j, 2)
	if got := j.loads.n.Load(); got != 1 {
		t.Errorf("j loaded the query path %d times, want the once a restored snapshot takes", got)
	}

	if err := createZoneVia(t, f, "three.example."); err != nil {
		t.Fatalf("CreateZone after Init: %v", err)
	}
	holds(t, j, 3)
}
