package apply_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// everything renders every zone and record in the store, so that a check for
// "nothing changed" cannot miss the zone it was not looking at.
func (f *fixture) everything() []string {
	f.t.Helper()

	var out []string
	for z, err := range f.s.IterZones(f.t.Context()) {
		if err != nil {
			f.t.Fatalf("IterZones: %v", err)
		}
		out = append(out, z.Name.String()+" serial "+z.SOA.Serial.String())
		for r, rerr := range f.s.IterZoneRecords(f.t.Context(), z.ID) {
			if rerr != nil {
				f.t.Fatalf("IterZoneRecords: %v", rerr)
			}
			out = append(out, z.Name.String()+" | "+r.Name.String()+" "+
				r.Type.String()+" "+r.RData.String())
		}
	}
	slices.Sort(out)
	return out
}

func TestPlanningLeavesTheStoreAsItFoundIt(t *testing.T) {
	t.Parallel()

	type planner func() (*apply.Batch, *apply.Result, error)

	tests := []struct {
		name    string
		prepare func(*fixture) planner
	}{
		{
			name: "a command",
			prepare: func(f *fixture) planner {
				f.reverseZone("0.0.10.in-addr.arpa.")
				return func() (*apply.Batch, *apply.Result, error) {
					return f.a.Plan(f.t.Context(), f.command(apply.RecordOp{
						Action: apply.ActionAdd,
						Record: f.record("www.example.com.", zone.TypeA, 300, "10.0.0.1"),
					}))
				}
			},
		},
		{
			name: "a rollback",
			prepare: func(f *fixture) planner {
				was := f.serial()
				f.addA("www.example.com.", "10.0.0.1")
				return func() (*apply.Batch, *apply.Result, error) {
					return f.a.PlanRollback(f.t.Context(), f.z.ID, was, testMeta())
				}
			},
		},
		{
			name: "creating a zone",
			prepare: func(f *fixture) planner {
				f.reverseZone("2.0.192.in-addr.arpa.")
				z := newZone(f.t, "example.net.")
				www, err := zone.NewRecord(z.ID, zone.MustParseName("www.example.net."),
					zone.ClassIN, zone.TypeA, 3600, "192.0.2.50")
				if err != nil {
					f.t.Fatalf("NewRecord: %v", err)
				}
				records := []zone.Record{apexNS(f.t, z, "ns1.example.net."), www}
				return func() (*apply.Batch, *apply.Result, error) {
					return f.a.PlanCreateZone(f.t.Context(), z, records, testMeta())
				}
			},
		},
		{
			name: "deleting a zone",
			prepare: func(f *fixture) planner {
				f.reverseZone("2.0.192.in-addr.arpa.")
				f.addA("www.example.com.", "192.0.2.10")
				return func() (*apply.Batch, *apply.Result, error) {
					return f.a.PlanDeleteZone(f.t.Context(), f.z.ID, testMeta())
				}
			},
		},
		{
			name: "changing a zone's settings",
			prepare: func(f *fixture) planner {
				next := *f.z
				next.Comment = "the office"
				next.SOA.Refresh = 7200
				return func() (*apply.Batch, *apply.Result, error) {
					return f.a.PlanUpdateZone(f.t.Context(), &next, testMeta())
				}
			},
		},
		{
			name: "reconciling a reverse zone",
			prepare: func(f *fixture) planner {
				f.addA("www.example.com.", "10.0.0.1")
				rev := f.reverseZone("0.0.10.in-addr.arpa.")
				return func() (*apply.Batch, *apply.Result, error) {
					return f.a.PlanReconcile(f.t.Context(), rev.ID, testMeta())
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			plan := tt.prepare(f)
			before := f.everything()

			b, _, err := plan()
			if err != nil {
				t.Fatalf("planning: %v", err)
			}
			if b.Empty() {
				t.Fatal("the plan came back with nothing to do")
			}
			if got := f.everything(); !slices.Equal(got, before) {
				t.Errorf("planning changed the store\n got: %v\nwant: %v", got, before)
			}

			// The other half: a plan that changes nothing when applied would
			// pass the check above for the wrong reason.
			if err = f.a.ApplyBatch(t.Context(), b); err != nil {
				t.Fatalf("ApplyBatch: %v", err)
			}
			if got := f.everything(); slices.Equal(got, before) {
				t.Errorf("applying the plan changed nothing: %v", got)
			}
		})
	}
}

func TestABatchStillMeansTheSameAfterATripThroughJSON(t *testing.T) {
	t.Parallel()

	// The control: the same command, applied the ordinary way.
	control := newFixture(t)
	controlRev := control.reverseZone("0.0.10.in-addr.arpa.")
	control.mustApply(control.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: control.record("www.example.com.", zone.TypeA, 300, "10.0.0.1"),
	}))

	f := newFixture(t)
	rev := f.reverseZone("0.0.10.in-addr.arpa.")

	b, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "10.0.0.1"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var travelled apply.Batch
	if err = json.Unmarshal(data, &travelled); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if err = f.a.ApplyBatch(t.Context(), &travelled); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}

	if got, want := f.records(), control.records(); !slices.Equal(got, want) {
		t.Errorf("the zone came out different\n got: %v\nwant: %v", got, want)
	}
	if got, want := f.ptrs(rev), control.ptrs(controlRev); !slices.Equal(got, want) {
		t.Errorf("the reverse zone came out different\n got: %v\nwant: %v", got, want)
	}
	if got, want := f.serial(), control.serial(); got != want {
		t.Errorf("the serial is %s, want %s", got, want)
	}
}

func TestProvenanceSurvivesTheWire(t *testing.T) {
	t.Parallel()

	// The property a journal event cannot carry. Without it the PTR would sit
	// in the reverse zone with nothing linking it back (D24).
	f := newFixture(t)
	rev := f.reverseZone("0.0.10.in-addr.arpa.")

	b, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "10.0.0.1"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var travelled apply.Batch
	if err = json.Unmarshal(data, &travelled); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err = f.a.ApplyBatch(t.Context(), &travelled); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}

	addr := f.recordAt("www.example.com.", zone.TypeA, "10.0.0.1")
	derived, err := f.s.ManagedBy(t.Context(), addr.ID)
	if err != nil {
		t.Fatalf("ManagedBy: %v", err)
	}
	if len(derived) != 1 {
		t.Fatalf("%d records hang off the address record, want the one PTR; the reverse zone holds %v",
			len(derived), f.ptrs(rev))
	}
	if derived[0].Type != zone.TypePTR {
		t.Errorf("a %s hangs off the address record, want a PTR", derived[0].Type)
	}
	if derived[0].ManagedKind != zone.ManagedPTR {
		t.Errorf("managed kind is %q, want %q", derived[0].ManagedKind, zone.ManagedPTR)
	}
}

func TestApplyingABatchTwiceChangesThingsOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rev := f.reverseZone("0.0.10.in-addr.arpa.")

	b, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "10.0.0.1"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err = f.a.ApplyBatch(t.Context(), b); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}

	records, ptrs, serial := f.records(), f.ptrs(rev), f.serial()

	// A restarting node replays its log from the last snapshot, so a batch it
	// already carried out comes round again (D24).
	if err = f.a.ApplyBatch(t.Context(), b); err != nil {
		t.Fatalf("ApplyBatch a second time: %v", err)
	}

	if got := f.records(); !slices.Equal(got, records) {
		t.Errorf("the zone changed on the replay\n got: %v\nwant: %v", got, records)
	}
	if got := f.ptrs(rev); !slices.Equal(got, ptrs) {
		t.Errorf("the reverse zone changed on the replay\n got: %v\nwant: %v", got, ptrs)
	}
	if got := f.serial(); got != serial {
		t.Errorf("the serial moved to %s on the replay, want it left at %s", got, serial)
	}
}

func TestHalfABatchIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	first, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "10.0.0.1"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err = f.a.ApplyBatch(t.Context(), first); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}

	second, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("mail.example.com.", zone.TypeA, 300, "10.0.0.2"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// One commit already in the journal, one not. Neither "already done" nor
	// "still to do" is true of it.
	second.Commits = append(first.Commits, second.Commits...)

	err = f.a.ApplyBatch(t.Context(), second)
	if err == nil {
		t.Fatal("the batch was accepted")
	}
	if !strings.Contains(err.Error(), "whole or not at all") {
		t.Errorf("error is %q, want it to say the batch lands whole", err)
	}
}

func TestDeletingAZoneIsOneBatch(t *testing.T) {
	t.Parallel()

	// A zone's departure and the removal of what its records generated
	// elsewhere are one change. Two entries could be applied by halves, and a
	// reverse zone would keep pointing at a zone that no longer exists.
	f := newFixture(t)
	rev := f.reverseZone("2.0.192.in-addr.arpa.")
	f.addA("www.example.com.", "192.0.2.10")
	if got := f.ptrs(rev); len(got) != 1 {
		t.Fatalf("the reverse zone holds %v, want the one entry", got)
	}

	b, _, err := f.a.PlanDeleteZone(t.Context(), f.z.ID, testMeta())
	if err != nil {
		t.Fatalf("PlanDeleteZone: %v", err)
	}
	if len(b.Commits) != 2 {
		t.Fatalf("the batch carries %d commits, want the zone's own and the reverse zone's",
			len(b.Commits))
	}
	if b.Commits[0].Kind != journal.KindZoneDelete {
		t.Errorf("the batch leads with a %q, want the deletion itself", b.Commits[0].Kind)
	}
	if b.Commits[1].ZoneID != rev.ID {
		t.Errorf("the second commit belongs to zone %s, want the reverse zone", b.Commits[1].ZoneID)
	}

	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var travelled apply.Batch
	if err = json.Unmarshal(data, &travelled); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err = f.a.ApplyBatch(t.Context(), &travelled); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}

	for _, line := range f.everything() {
		if strings.HasPrefix(line, "example.com.") {
			t.Errorf("the zone is still there: %s", line)
		}
	}
	if got := f.ptrs(rev); len(got) != 0 {
		t.Errorf("the reverse zone still holds %v", got)
	}
}

// index is how far into the replicated log this node has got.
func (f *fixture) index() uint64 {
	f.t.Helper()

	got, err := f.s.AppliedIndex(f.t.Context())
	if err != nil {
		f.t.Fatalf("AppliedIndex: %v", err)
	}
	return got
}

func TestAReplicatedBatchRecordsWhereItCameFrom(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	if got := f.index(); got != 0 {
		t.Fatalf("a store nothing has reached is at index %d, want 0", got)
	}

	b, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "10.0.0.1"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err = f.a.ApplyBatchAt(t.Context(), b, 7); err != nil {
		t.Fatalf("ApplyBatchAt: %v", err)
	}
	if got := f.index(); got != 7 {
		t.Errorf("index = %d, want 7", got)
	}

	// A batch this node planned for itself travelled nowhere, so there is no
	// position to remember and the one already recorded stays where it is.
	local, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("mail.example.com.", zone.TypeA, 300, "10.0.0.2"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err = f.a.ApplyBatch(t.Context(), local); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	if got := f.index(); got != 7 {
		t.Errorf("index = %d after a batch that was never replicated, want 7", got)
	}
}

func TestTheIndexDecidesWhetherAnEntryHasBeenSeen(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	first, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "10.0.0.1"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err = f.a.ApplyBatchAt(t.Context(), first, 5); err != nil {
		t.Fatalf("ApplyBatchAt: %v", err)
	}

	second, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("mail.example.com.", zone.TypeA, 300, "10.0.0.2"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	before := f.everything()

	// Nothing in the journal says this batch has been carried out, and it is
	// still not carried out: the index says the log is already past position
	// five, and the index is what a replaying node has to be able to trust.
	if err = f.a.ApplyBatchAt(t.Context(), second, 5); err != nil {
		t.Fatalf("ApplyBatchAt: %v", err)
	}
	if got := f.everything(); !slices.Equal(got, before) {
		t.Errorf("an entry behind the index was carried out\n got: %v\nwant: %v", got, before)
	}
	if got := f.index(); got != 5 {
		t.Errorf("index = %d, want it left at 5", got)
	}
}

func TestARefusedBatchLeavesTheIndexWhereItWas(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	first, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "10.0.0.1"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err = f.a.ApplyBatchAt(t.Context(), first, 2); err != nil {
		t.Fatalf("ApplyBatchAt: %v", err)
	}

	second, _, err := f.a.Plan(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("mail.example.com.", zone.TypeA, 300, "10.0.0.2"),
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	second.Commits = append(first.Commits, second.Commits...)

	// The index and the batch are one write. A batch that does not land must
	// not leave a node claiming to have applied the entry that carried it.
	if err = f.a.ApplyBatchAt(t.Context(), second, 3); err == nil {
		t.Fatal("the batch was accepted")
	}
	if got := f.index(); got != 2 {
		t.Errorf("index = %d after a batch that was refused, want 2", got)
	}
}
