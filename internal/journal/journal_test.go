package journal_test

import (
	"errors"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func testEvent(seq int, op journal.Op) journal.Event {
	return journal.Event{
		Seq:   seq,
		Op:    op,
		Name:  zone.MustParseName("www.example.com."),
		Class: zone.ClassIN,
		Type:  zone.TypeA,
		TTL:   3600,
		RData: zone.MustParseRData(zone.TypeA, zone.ClassIN, "192.0.2.10"),
	}
}

func testCommit() journal.Commit {
	return journal.Commit{
		ID:         journal.CommitID(id.New()),
		ZoneID:     zone.ZoneID(id.New()),
		ZoneName:   zone.MustParseName("example.com."),
		SerialFrom: zone.NewSerial(41),
		SerialTo:   zone.NewSerial(42),
		Kind:       journal.KindEdit,
		Source:     journal.SourceAPI,
		Actor:      "deploy-token",
		Events:     []journal.Event{testEvent(0, journal.OpDel), testEvent(1, journal.OpAdd)},
	}
}

func TestEventValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*journal.Event)
		wantErr bool
	}{
		{"deletion", func(*journal.Event) {}, false},
		{"addition", func(e *journal.Event) { e.Op = journal.OpAdd }, false},
		// The SOA is zone metadata rather than a record, but its history still
		// belongs in the journal, so an event may carry one where a record may
		// not.
		{"start of authority", func(e *journal.Event) {
			e.Name = zone.MustParseName("example.com.")
			e.Type = zone.TypeSOA
			e.RData = zone.MustParseRData(zone.TypeSOA, zone.ClassIN,
				"ns1.example.com. hostmaster.example.com. 42 3600 900 1209600 3600")
		}, false},

		{"unknown operation", func(e *journal.Event) { e.Op = "modify" }, true},
		{"empty operation", func(e *journal.Event) { e.Op = "" }, true},
		{"negative sequence", func(e *journal.Event) { e.Seq = -1 }, true},
		{"no owner name", func(e *journal.Event) { e.Name = zone.Name{} }, true},
		{"query-only type", func(e *journal.Event) { e.Type = zone.TypeANY }, true},
		{"meta class", func(e *journal.Event) { e.Class = zone.ClassANY }, true},
		{"ttl above the maximum", func(e *journal.Event) { e.TTL = zone.MaxTTL + 1 }, true},
		{"no data", func(e *journal.Event) { e.RData = zone.RData{} }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e := testEvent(0, journal.OpDel)
			tc.mutate(&e)

			err := e.Validate()
			if tc.wantErr && !errors.Is(err, zone.ErrInvalid) {
				t.Fatalf("Validate() = %v, want an error wrapping zone.ErrInvalid", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestEventString(t *testing.T) {
	t.Parallel()

	del := testEvent(0, journal.OpDel)
	add := testEvent(1, journal.OpAdd)

	if got, want := del.String(), "-www.example.com.\t3600\tIN\tA\t192.0.2.10"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := add.String(), "+www.example.com.\t3600\tIN\tA\t192.0.2.10"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestKindAndSourceValid(t *testing.T) {
	t.Parallel()

	for _, k := range []journal.Kind{
		journal.KindZoneCreate, journal.KindZoneUpdate, journal.KindZoneDelete,
		journal.KindEdit, journal.KindImport, journal.KindRollback,
	} {
		if !k.Valid() {
			t.Errorf("Kind(%q).Valid() = false", k)
		}
	}
	for _, k := range []journal.Kind{"", "delete", "ZONE_CREATE", "rollforward"} {
		if k.Valid() {
			t.Errorf("Kind(%q).Valid() = true", k)
		}
	}

	for _, s := range []journal.Source{
		journal.SourceAPI, journal.SourceCLI, journal.SourceImport, journal.SourceSystem,
	} {
		if !s.Valid() {
			t.Errorf("Source(%q).Valid() = false", s)
		}
	}
	for _, s := range []journal.Source{"", "gui", "API", "raft"} {
		if s.Valid() {
			t.Errorf("Source(%q).Valid() = true", s)
		}
	}
}

func TestCommitValidate(t *testing.T) {
	t.Parallel()

	serial := func(v uint32) *zone.Serial {
		s := zone.NewSerial(v)
		return &s
	}

	tests := []struct {
		name    string
		mutate  func(*journal.Commit)
		wantErr bool
	}{
		{"ordinary edit", func(*journal.Commit) {}, false},
		{"no events", func(c *journal.Commit) {
			// Changing only the zone's own settings moves no records.
			c.Kind = journal.KindZoneUpdate
			c.Events = nil
		}, false},
		{"zone creation starts the serial", func(c *journal.Commit) {
			c.Kind = journal.KindZoneCreate
			c.SerialFrom = zone.NewSerial(0)
			c.SerialTo = zone.NewSerial(2026081601)
		}, false},
		{"rollback", func(c *journal.Commit) {
			c.Kind = journal.KindRollback
			c.RevertsTo = serial(17)
		}, false},
		{"serial wrapping around zero", func(c *journal.Commit) {
			c.SerialFrom = zone.NewSerial(^uint32(0))
			c.SerialTo = zone.NewSerial(0)
		}, false},

		{"no identifier", func(c *journal.Commit) { c.ID = "" }, true},
		{"lowercase identifier", func(c *journal.Commit) {
			c.ID = journal.CommitID("01jabcdefghjkmnpqrstvwxyz0")
		}, true},
		{"no zone", func(c *journal.Commit) { c.ZoneID = "" }, true},
		// The name is what still reads once the zone row is gone.
		{"no zone name", func(c *journal.Commit) { c.ZoneName = zone.Name{} }, true},
		{"unknown kind", func(c *journal.Commit) { c.Kind = "amend" }, true},
		{"unknown source", func(c *journal.Commit) { c.Source = "" }, true},

		{"serial skipping a step", func(c *journal.Commit) { c.SerialTo = zone.NewSerial(43) }, true},
		{"serial standing still", func(c *journal.Commit) { c.SerialTo = c.SerialFrom }, true},
		{"serial going backwards", func(c *journal.Commit) { c.SerialTo = zone.NewSerial(40) }, true},
		{"zone creation from a non-zero serial", func(c *journal.Commit) {
			c.Kind = journal.KindZoneCreate
			c.SerialFrom = zone.NewSerial(1)
			c.SerialTo = zone.NewSerial(2)
		}, true},
		{"zone creation at serial zero", func(c *journal.Commit) {
			c.Kind = journal.KindZoneCreate
			c.SerialFrom = zone.NewSerial(0)
			c.SerialTo = zone.NewSerial(0)
		}, true},

		{"rollback without a target", func(c *journal.Commit) { c.Kind = journal.KindRollback }, true},
		{"target without a rollback", func(c *journal.Commit) { c.RevertsTo = serial(17) }, true},
		{"rollback to the present", func(c *journal.Commit) {
			c.Kind = journal.KindRollback
			c.RevertsTo = serial(41)
		}, true},
		{"rollback to the future", func(c *journal.Commit) {
			c.Kind = journal.KindRollback
			c.RevertsTo = serial(4711)
		}, true},
		{"rollback to an incomparable serial", func(c *journal.Commit) {
			// Exactly half the serial space away, where RFC 1982 §3.2 leaves
			// the ordering undefined.
			c.Kind = journal.KindRollback
			c.SerialFrom = zone.NewSerial(0)
			c.SerialTo = zone.NewSerial(1)
			c.RevertsTo = serial(1 << 31)
		}, true},

		{"broken event", func(c *journal.Commit) { c.Events[1].RData = zone.RData{} }, true},
		{"sequence with a gap", func(c *journal.Commit) { c.Events[1].Seq = 2 }, true},
		{"sequence not starting at zero", func(c *journal.Commit) {
			c.Events[0].Seq = 1
			c.Events[1].Seq = 2
		}, true},
		{"deletion after an addition", func(c *journal.Commit) {
			c.Events[0].Op = journal.OpAdd
			c.Events[1].Op = journal.OpDel
		}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := testCommit()
			tc.mutate(&c)

			err := c.Validate()
			if tc.wantErr && !errors.Is(err, zone.ErrInvalid) {
				t.Fatalf("Validate() = %v, want an error wrapping zone.ErrInvalid", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}
