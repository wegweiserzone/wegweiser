package apply_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// fixture is one applier over one store holding one zone.
type fixture struct {
	t   *testing.T
	s   store.Store
	a   *apply.Applier
	z   *zone.Zone
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, apply.Options{})
}

func newFixtureWith(t *testing.T, opts apply.Options) *fixture {
	t.Helper()

	s, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(t.TempDir(), "weg.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	})
	if merr := s.Migrate(t.Context()); merr != nil {
		t.Fatalf("Migrate: %v", merr)
	}

	f := &fixture{t: t, s: s, now: time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)}
	opts.Now = func() time.Time { return f.now }
	f.a = newApplier(t, s, opts)

	z := newZone(t, "example.com.")
	f.z = z
	// A zone is only usable once it names an authoritative server, so the apex
	// NS goes in with it.
	ns, err := zone.NewRecord(z.ID, z.Name, zone.ClassIN, zone.TypeNS, 3600, "ns1.example.com.")
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	if _, err := f.a.CreateZone(t.Context(), z, []zone.Record{ns}, testMeta()); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	return f
}

func newApplier(t *testing.T, s store.Store, opts apply.Options) *apply.Applier {
	t.Helper()

	a, err := apply.New(s, opts)
	if err != nil {
		t.Fatalf("apply.New: %v", err)
	}
	return a
}

func newZone(t *testing.T, apex string) *zone.Zone {
	t.Helper()

	z, err := zone.NewZone(zone.MustParseName(apex),
		zone.DefaultSOA(zone.MustParseName("ns1.example.com."),
			zone.MustParseName("hostmaster.example.com.")))
	if err != nil {
		t.Fatalf("NewZone(%s): %v", apex, err)
	}
	return &z
}

func testMeta() apply.Meta {
	return apply.Meta{Source: journal.SourceAPI, Actor: "deploy-token"}
}

func (f *fixture) record(name string, typ zone.RRType, ttl zone.TTL, rdata string) *zone.Record {
	f.t.Helper()

	r, err := zone.NewRecord(f.z.ID, zone.MustParseName(name), zone.ClassIN, typ, ttl, rdata)
	if err != nil {
		f.t.Fatalf("NewRecord(%s %s %s): %v", name, typ, rdata, err)
	}
	return &r
}

func (f *fixture) command(ops ...apply.RecordOp) apply.Command {
	return apply.Command{
		ZoneID: f.z.ID,
		Ops:    ops,
		Kind:   journal.KindEdit,
		Source: journal.SourceAPI,
		Actor:  "deploy-token",
	}
}

func (f *fixture) mustApply(cmd apply.Command) *journal.Commit {
	f.t.Helper()

	res, err := f.a.Apply(f.t.Context(), cmd)
	if err != nil {
		f.t.Fatalf("Apply: %v", err)
	}
	return res.Commit()
}

func (f *fixture) serial() zone.Serial {
	f.t.Helper()

	z, err := f.s.ZoneByID(f.t.Context(), f.z.ID)
	if err != nil {
		f.t.Fatalf("ZoneByID: %v", err)
	}
	return z.SOA.Serial
}

// add is one commit adding one record, named after the step it belongs to.
func (f *fixture) add(name string) {
	f.t.Helper()
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record(name+".example.com.", zone.TypeA, 300, "192.0.2.1"),
	}))
}

// records renders the zone as sorted "name TYPE data" lines.
func (f *fixture) records() []string {
	f.t.Helper()

	var out []string
	for r, err := range f.s.IterZoneRecords(f.t.Context(), f.z.ID) {
		if err != nil {
			f.t.Fatalf("IterZoneRecords: %v", err)
		}
		out = append(out, r.Name.String()+" "+r.Type.String()+" "+r.RData.String())
	}
	return out
}

func (f *fixture) recordAt(name string, typ zone.RRType, rdata string) *zone.Record {
	f.t.Helper()

	page, err := f.s.ListRecords(f.t.Context(), store.RecordFilter{
		ZoneID: f.z.ID,
		Name:   zone.MustParseName(name),
		Types:  []zone.RRType{typ},
	})
	if err != nil {
		f.t.Fatalf("ListRecords: %v", err)
	}
	for _, r := range page.Items {
		if r.RData.String() == rdata {
			return r
		}
	}
	f.t.Fatalf("no %s %s record at %s; the zone holds %v", typ, rdata, name, f.records())
	return nil
}

func eventLines(c *journal.Commit) []string {
	out := make([]string, len(c.Events))
	for i, e := range c.Events {
		out[i] = e.String()
	}
	return out
}

func TestApplyAddsRecords(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	before := f.serial()
	c := f.mustApply(f.command(
		apply.RecordOp{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")},
		apply.RecordOp{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.11")},
	))

	if c == nil {
		t.Fatal("adding two records produced no commit")
	}
	if c.SerialFrom != before || c.SerialTo != before.Next() {
		t.Errorf("commit went from %s to %s, want %s to %s", c.SerialFrom, c.SerialTo, before, before.Next())
	}
	if got := f.serial(); got != before.Next() {
		t.Errorf("the zone is at serial %s, want %s", got, before.Next())
	}
	if !c.CreatedAt.Equal(f.now) {
		t.Errorf("CreatedAt = %v, want the injected clock at %v", c.CreatedAt, f.now)
	}
	if c.Actor != "deploy-token" {
		t.Errorf("Actor = %q", c.Actor)
	}

	want := []string{
		"+www.example.com.\t3600\tIN\tA\t192.0.2.10",
		"+www.example.com.\t3600\tIN\tA\t192.0.2.11",
	}
	if got := eventLines(c); !slices.Equal(got, want) {
		t.Errorf("events\n  got  %v\n  want %v", got, want)
	}
	// A commit read back out of the journal has to be the one that went in.
	if err := c.Validate(); err != nil {
		t.Errorf("the commit does not validate: %v", err)
	}
}

// Identifiers are minted before anything is written, because the command is
// what a cluster will replicate: two nodes each inventing one would diverge.
func TestApplyMintsIdentifiersUpFront(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")
	if rec.ID != "" {
		t.Fatal("the test record already carries an identifier")
	}
	cmd := f.command(apply.RecordOp{Action: apply.ActionAdd, Record: rec})
	f.mustApply(cmd)

	if !id.Valid(string(rec.ID)) {
		t.Errorf("the record was stored with the identifier %q", rec.ID)
	}
	if _, err := f.s.RecordByID(t.Context(), rec.ID); err != nil {
		t.Errorf("the minted identifier does not address the stored record: %v", err)
	}
}

func TestApplyUpdatesAndDeletes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.mustApply(f.command(
		apply.RecordOp{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")},
	))
	rec := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")

	t.Run("an update keeps the identity", func(t *testing.T) {
		next := f.record("www.example.com.", zone.TypeA, 60, "192.0.2.99")
		c := f.mustApply(f.command(apply.RecordOp{
			Action: apply.ActionUpdate, RecordID: rec.ID, Record: next,
		}))

		want := []string{
			"-www.example.com.\t3600\tIN\tA\t192.0.2.10",
			"+www.example.com.\t60\tIN\tA\t192.0.2.99",
		}
		if got := eventLines(c); !slices.Equal(got, want) {
			t.Errorf("events\n  got  %v\n  want %v", got, want)
		}

		got, err := f.s.RecordByID(t.Context(), rec.ID)
		if err != nil {
			t.Fatalf("RecordByID: %v", err)
		}
		if got.RData.String() != "192.0.2.99" || got.TTL != 60 {
			t.Errorf("the record reads back as %s %d", got.RData, got.TTL)
		}
		if !got.CreatedAt.Equal(rec.CreatedAt) {
			t.Errorf("CreatedAt moved from %v to %v", rec.CreatedAt, got.CreatedAt)
		}
	})

	t.Run("an update that changes nothing produces no commit", func(t *testing.T) {
		before := f.serial()
		same := f.record("www.example.com.", zone.TypeA, 60, "192.0.2.99")

		res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionUpdate, RecordID: rec.ID, Record: same,
		}))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if c := res.Commit(); c != nil {
			t.Errorf("a change that changed nothing produced the commit %s", c.ID)
		}
		// A serial step tells every secondary in the world to fetch a copy.
		if got := f.serial(); got != before {
			t.Errorf("the serial moved from %s to %s for no change", before, got)
		}
	})

	t.Run("a delete removes the record", func(t *testing.T) {
		c := f.mustApply(f.command(apply.RecordOp{Action: apply.ActionDelete, RecordID: rec.ID}))

		want := []string{"-www.example.com.\t60\tIN\tA\t192.0.2.99"}
		if got := eventLines(c); !slices.Equal(got, want) {
			t.Errorf("events\n  got  %v\n  want %v", got, want)
		}
		if _, err := f.s.RecordByID(t.Context(), rec.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the record survived its deletion: %v", err)
		}
	})
}

func TestApplyReplaceRRset(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.mustApply(f.command(
		apply.RecordOp{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")},
		apply.RecordOp{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.11")},
	))
	kept := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")
	dropped := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.11")

	key := zone.RRsetKey{
		Name:  zone.MustParseName("www.example.com."),
		Class: zone.ClassIN,
		Type:  zone.TypeA,
	}

	t.Run("only the difference is written", func(t *testing.T) {
		c := f.mustApply(f.command(apply.RecordOp{
			Action: apply.ActionReplaceRRset,
			Key:    key,
			Records: []zone.Record{
				*f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10"),
				*f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.12"),
			},
		}))

		want := []string{
			"-www.example.com.\t3600\tIN\tA\t192.0.2.11",
			"+www.example.com.\t3600\tIN\tA\t192.0.2.12",
		}
		if got := eventLines(c); !slices.Equal(got, want) {
			t.Errorf("events\n  got  %v\n  want %v", got, want)
		}

		// The unchanged member keeps its identity, and with it its comment, its
		// provenance and the diff line pointing at it.
		if _, err := f.s.RecordByID(t.Context(), kept.ID); err != nil {
			t.Errorf("the unchanged record lost its identity: %v", err)
		}
		if _, err := f.s.RecordByID(t.Context(), dropped.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the dropped record survived: %v", err)
		}
	})

	t.Run("replacing with what is already there changes nothing", func(t *testing.T) {
		before := f.serial()
		res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionReplaceRRset,
			Key:    key,
			Records: []zone.Record{
				*f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.12"),
				*f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10"),
			},
		}))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if c := res.Commit(); c != nil {
			t.Errorf("replacing a set with itself produced the commit %s", c.ID)
		}
		if got := f.serial(); got != before {
			t.Errorf("the serial moved from %s to %s", before, got)
		}
	})

	t.Run("a TTL change updates every member in place", func(t *testing.T) {
		c := f.mustApply(f.command(apply.RecordOp{
			Action: apply.ActionReplaceRRset,
			Key:    key,
			Records: []zone.Record{
				*f.record("www.example.com.", zone.TypeA, 300, "192.0.2.10"),
				*f.record("www.example.com.", zone.TypeA, 300, "192.0.2.12"),
			},
		}))
		if len(c.Events) != 4 {
			t.Errorf("a TTL change on two records produced %d events, want four", len(c.Events))
		}
		if _, err := f.s.RecordByID(t.Context(), kept.ID); err != nil {
			t.Errorf("a TTL change threw away the record's identity: %v", err)
		}
	})

	t.Run("an empty set removes the RRset", func(t *testing.T) {
		f.mustApply(f.command(apply.RecordOp{Action: apply.ActionReplaceRRset, Key: key}))

		if got := f.records(); !slices.Equal(got, []string{"example.com. NS ns1.example.com."}) {
			t.Errorf("the zone still holds %v", got)
		}
	})
}

// Deletions are written before additions, or the index forbidding a duplicate
// resource record refuses the new one while the old one is still there.
func TestApplyReusesDataUnderANewIdentity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.mustApply(f.command(
		apply.RecordOp{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")},
	))
	old := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")

	replacement := f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")
	replacement.ID = zone.RecordID(id.New())
	replacement.Comment = "same address, deliberately a different row"

	f.mustApply(f.command(
		apply.RecordOp{Action: apply.ActionDelete, RecordID: old.ID},
		apply.RecordOp{Action: apply.ActionAdd, Record: replacement},
	))

	got, err := f.s.RecordByID(t.Context(), replacement.ID)
	if err != nil {
		t.Fatalf("the replacement was not stored: %v", err)
	}
	if got.Comment != replacement.Comment {
		t.Errorf("comment = %q", got.Comment)
	}
	if _, err := f.s.RecordByID(t.Context(), old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old row survived: %v", err)
	}
}

func TestApplyRefusesBrokenZoneStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ops  func(f *fixture) []apply.RecordOp
	}{
		{
			// RFC 2181 section 10.1: a CNAME means "look up this other name",
			// so nothing else can live at that name.
			name: "a CNAME beside other data",
			ops: func(f *fixture) []apply.RecordOp {
				return []apply.RecordOp{
					{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")},
					{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeCNAME, 3600, "other.example.com.")},
				}
			},
		},
		{
			name: "a CNAME at the apex",
			ops: func(f *fixture) []apply.RecordOp {
				return []apply.RecordOp{
					{Action: apply.ActionAdd, Record: f.record("example.com.", zone.TypeCNAME, 3600, "other.example.com.")},
				}
			},
		},
		{
			// RFC 2181 section 5.2: a resolver caches an RRset as one thing.
			name: "divergent TTLs within an RRset",
			ops: func(f *fixture) []apply.RecordOp {
				return []apply.RecordOp{
					{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")},
					{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 60, "192.0.2.11")},
				}
			},
		},
		{
			name: "a record outside the zone",
			ops: func(f *fixture) []apply.RecordOp {
				return []apply.RecordOp{
					{Action: apply.ActionAdd, Record: f.record("www.example.net.", zone.TypeA, 3600, "192.0.2.10")},
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)

			before := f.serial()
			_, err := f.a.Apply(t.Context(), f.command(tc.ops(f)...))
			if !errors.Is(err, zone.ErrInvalid) {
				t.Fatalf("Apply = %v, want an error wrapping zone.ErrInvalid", err)
			}
			// The whole command is one transaction, so a refusal leaves nothing
			// half done.
			if got := f.records(); !slices.Equal(got, []string{"example.com. NS ns1.example.com."}) {
				t.Errorf("a refused command left %v behind", got)
			}
			if got := f.serial(); got != before {
				t.Errorf("a refused command moved the serial from %s to %s", before, got)
			}
		})
	}
}

// RFC 1034 section 4.2.1: a zone names its own authoritative servers. No single
// record can notice that the last one is going.
func TestApplyRefusesRemovingTheLastApexNS(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	ns := f.recordAt("example.com.", zone.TypeNS, "ns1.example.com.")
	_, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionDelete, RecordID: ns.ID,
	}))
	if !errors.Is(err, zone.ErrInvalid) {
		t.Fatalf("Apply = %v, want an error wrapping zone.ErrInvalid", err)
	}
	if _, err := f.s.RecordByID(t.Context(), ns.ID); err != nil {
		t.Errorf("the NS record went anyway: %v", err)
	}

	// Removing one of two is fine.
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("example.com.", zone.TypeNS, 3600, "ns2.example.com."),
	}))
	f.mustApply(f.command(apply.RecordOp{Action: apply.ActionDelete, RecordID: ns.ID}))
}

func TestApplyRefusesEditingAGeneratedRecord(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	source := f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")
	f.mustApply(f.command(apply.RecordOp{Action: apply.ActionAdd, Record: source}))

	// Written directly, because nothing generates records yet.
	generated := f.record("other.example.com.", zone.TypePTR, 3600, "www.example.com.")
	generated.ID = zone.RecordID(id.New())
	generated.ManagedBy = source.ID
	generated.ManagedKind = zone.ManagedPTR
	if err := f.s.Update(t.Context(), func(tx store.Tx) error {
		return tx.InsertRecord(t.Context(), generated)
	}); err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	for name, op := range map[string]apply.RecordOp{
		"delete": {Action: apply.ActionDelete, RecordID: generated.ID},
		"update": {
			Action:   apply.ActionUpdate,
			RecordID: generated.ID,
			Record:   f.record("other.example.com.", zone.TypePTR, 60, "elsewhere.example.com."),
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.a.Apply(t.Context(), f.command(op))
			if !errors.Is(err, zone.ErrInvalid) {
				t.Fatalf("Apply = %v, want an error wrapping zone.ErrInvalid", err)
			}
		})
	}
}

func TestApplyOptimisticConcurrency(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	stale := f.serial()
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("a.example.com.", zone.TypeA, 3600, "192.0.2.1"),
	}))

	cmd := f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("b.example.com.", zone.TypeA, 3600, "192.0.2.2"),
	})
	cmd.ExpectedSerial = &stale

	_, err := f.a.Apply(t.Context(), cmd)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Apply = %v, want an error wrapping store.ErrConflict", err)
	}

	current := f.serial()
	cmd.ExpectedSerial = &current
	f.mustApply(cmd)
}

func TestApplyRejectsMalformedCommands(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tests := map[string]apply.Command{
		"no zone":       {Ops: []apply.RecordOp{{Action: apply.ActionDelete, RecordID: "x"}}, Kind: journal.KindEdit, Source: journal.SourceAPI},
		"no operations": {ZoneID: f.z.ID, Kind: journal.KindEdit, Source: journal.SourceAPI},
		"unknown source": {
			ZoneID: f.z.ID, Kind: journal.KindEdit, Source: "telepathy",
			Ops: []apply.RecordOp{{Action: apply.ActionDelete, RecordID: "x"}},
		},
		"a zone lifecycle kind": {
			ZoneID: f.z.ID, Kind: journal.KindZoneCreate, Source: journal.SourceAPI,
			Ops: []apply.RecordOp{{Action: apply.ActionDelete, RecordID: "x"}},
		},
		"unknown action": {
			ZoneID: f.z.ID, Kind: journal.KindEdit, Source: journal.SourceAPI,
			Ops: []apply.RecordOp{{Action: "rename"}},
		},
		"an update with no content": {
			ZoneID: f.z.ID, Kind: journal.KindEdit, Source: journal.SourceAPI,
			Ops: []apply.RecordOp{{Action: apply.ActionUpdate, RecordID: "x"}},
		},
		"a replacement containing a foreign record": {
			ZoneID: f.z.ID, Kind: journal.KindEdit, Source: journal.SourceAPI,
			Ops: []apply.RecordOp{{
				Action:  apply.ActionReplaceRRset,
				Key:     zone.RRsetKey{Name: zone.MustParseName("www.example.com."), Class: zone.ClassIN, Type: zone.TypeA},
				Records: []zone.Record{*f.record("other.example.com.", zone.TypeA, 3600, "192.0.2.1")},
			}},
		},
	}

	for name, cmd := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := f.a.Apply(t.Context(), cmd); !errors.Is(err, zone.ErrInvalid) {
				t.Fatalf("Apply = %v, want an error wrapping zone.ErrInvalid", err)
			}
		})
	}
}

func TestApplyToAMissingZone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	cmd := f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10"),
	})
	cmd.ZoneID = zone.ZoneID(id.New())

	if _, err := f.a.Apply(t.Context(), cmd); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Apply = %v, want an error wrapping store.ErrNotFound", err)
	}
}

// One commit per serial is the invariant rollback and incremental transfer both
// rest on. Concurrent commands must queue rather than race for the same step.
func TestApplyIsSerializedPerZone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	const writers = 8
	before := f.serial()
	errs := make(chan error, writers)
	for i := range writers {
		go func() {
			rec := f.record("host.example.com.", zone.TypeTXT, 3600, `"writer `+string(rune('a'+i))+`"`)
			_, err := f.a.Apply(context.Background(), f.command(apply.RecordOp{
				Action: apply.ActionAdd, Record: rec,
			}))
			errs <- err
		}()
	}
	for range writers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Apply: %v", err)
		}
	}

	// Eight commands, eight serial steps, and a journal with no gaps.
	page, err := f.s.ListCommits(t.Context(), store.CommitFilter{ZoneID: f.z.ID})
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(page.Items) != writers+1 { // plus the apex NS from the fixture
		t.Fatalf("the journal holds %d commits, want %d", len(page.Items), writers+1)
	}

	seen := make(map[uint32]bool, len(page.Items))
	for _, c := range page.Items {
		if seen[c.SerialTo.Uint32()] {
			t.Errorf("two commits produced serial %s", c.SerialTo)
		}
		seen[c.SerialTo.Uint32()] = true
	}
	// Eight commands, so exactly eight steps: no command was lost and none
	// counted twice.
	if got, want := f.serial(), zone.NewSerial(before.Uint32()+writers); got != want {
		t.Errorf("the zone went from serial %s to %s after %d commands, want %s",
			before, got, writers, want)
	}
}

// TestApplyRefusesDataBelowADelegation checks that an incremental write and a
// whole-zone check agree about what may live at and below a delegation point.
func TestApplyRefusesDataBelowADelegation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("sub.example.com.", zone.TypeNS, 3600, "ns1.sub.example.com."),
	}))

	t.Run("a record below it is refused", func(t *testing.T) {
		_, err := f.a.Apply(f.t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionAdd,
			Record: f.record("www.sub.example.com.", zone.TypeTXT, 300, `"hello"`),
		}))
		if err == nil {
			t.Fatal("stored a record that a query for it would never reach")
		}
		if !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want it to wrap ErrInvalid", err)
		}
		if !strings.Contains(err.Error(), "sub.example.com.") {
			t.Errorf("error = %v, want it to name the delegation", err)
		}
	})

	t.Run("glue below it is allowed", func(t *testing.T) {
		// Without it the child's servers cannot be reached: a resolver would
		// have to ask the zone it is trying to find where that zone lives.
		f.mustApply(f.command(apply.RecordOp{
			Action: apply.ActionAdd,
			Record: f.record("ns1.sub.example.com.", zone.TypeA, 3600, "192.0.2.53"),
		}))
		if got := f.recordAt("ns1.sub.example.com.", zone.TypeA, "192.0.2.53"); got == nil {
			t.Error("the glue was not stored")
		}
	})

	t.Run("anything but NS at the point itself is refused", func(t *testing.T) {
		_, err := f.a.Apply(f.t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionAdd,
			Record: f.record("sub.example.com.", zone.TypeA, 300, "192.0.2.1"),
		}))
		if err == nil {
			t.Fatal("stored a record at a delegation point that is referred away")
		}
	})

	t.Run("the whole-zone check says the same thing", func(t *testing.T) {
		// The two rules are one function now, so this is a guard against them
		// drifting apart again rather than a second implementation's test.
		z := newZone(t, "other.example.")
		records := []zone.Record{
			*mustRecord(t, z.ID, "other.example.", zone.TypeNS, 3600, "ns1.other.example."),
			*mustRecord(t, z.ID, "sub.other.example.", zone.TypeNS, 3600, "ns1.sub.other.example."),
			*mustRecord(t, z.ID, "www.sub.other.example.", zone.TypeTXT, 300, `"hello"`),
		}
		if _, err := f.a.CreateZone(f.t.Context(), z, records, testMeta()); err == nil {
			t.Fatal("created a zone holding a record that would never be answered")
		}
	})
}

// mustRecord builds a record in an arbitrary zone, for the cases the fixture's
// own zone does not cover.
func mustRecord(
	t *testing.T, zid zone.ZoneID, name string, typ zone.RRType, ttl zone.TTL, rdata string,
) *zone.Record {
	t.Helper()

	r, err := zone.NewRecord(zid, zone.MustParseName(name), zone.ClassIN, typ, ttl, rdata)
	if err != nil {
		t.Fatalf("NewRecord(%s %s %s): %v", name, typ, rdata, err)
	}
	return &r
}

// ref takes the address of a value, for options that distinguish "unset" from
// the zero value.
func ref[T any](v T) *T { return &v }

// D5a's rule has to hold whichever order the end state is reached in. The
// record below was written first and is not touched by the write that
// delegates over it, so nothing would have looked at it.
func TestDelegatingOverExistingRecordsIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("host.sub.example.com.", zone.TypeTXT, 300, `"hello"`),
	}))

	_, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("sub.example.com.", zone.TypeNS, 3600, "ns1.other.example."),
	}))
	if err == nil {
		t.Fatal("the delegation was accepted and the TXT below it is now unanswerable")
	}
	if !errors.Is(err, zone.ErrInvalid) {
		t.Errorf("error = %v, want one wrapping zone.ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "only A and AAAA glue") {
		t.Errorf("error is %q, want it to say what may remain below a delegation", err)
	}
}

// Glue is what may remain, so delegating over it is the ordinary case and has
// to keep working.
func TestDelegatingOverGlueIsAllowed(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("ns1.sub.example.com.", zone.TypeA, 300, "192.0.2.53"),
	}))

	if _, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("sub.example.com.", zone.TypeNS, 3600, "ns1.sub.example.com."),
	})); err != nil {
		t.Fatalf("delegating over glue was refused: %v", err)
	}
}

// A deeper delegation is what the closest-delegation rule is for: the name
// below carries NS, and its own delegation is the one that judges it. The
// whole-zone check accepts this, so the write path has to as well.
func TestDelegatingOverADeeperDelegationIsAllowed(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("deep.sub.example.com.", zone.TypeNS, 3600, "ns1.other.example."),
	}))

	if _, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("sub.example.com.", zone.TypeNS, 3600, "ns1.other.example."),
	})); err != nil {
		t.Fatalf("delegating above another delegation was refused: %v", err)
	}
}

// The check has to see exactly what reconcile would write, or the two answers
// to one question drift apart.
func TestCheckReverseSeesWhatReconcileWouldWrite(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rev := f.reverseZone("0.0.10.in-addr.arpa.")

	// Past the applier, so nothing generated an entry for them. Creating the
	// zone fills it from what the applier does know about, so this is the one
	// way left to a zone that is missing entries: records the write path never
	// saw.
	f.storeA("www.example.com.", "10.0.0.1")
	f.storeA("mail.example.com.", "10.0.0.2")

	found, err := f.a.CheckReverse(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("CheckReverse: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(found), found)
	}
	for _, got := range found {
		if got.Severity != zone.SeverityWarning {
			t.Errorf("%q is %q, want a warning: nothing here is invalid", got.Name, got.Severity)
		}
		if got.Scope != zone.ScopeReverse {
			t.Errorf("scope is %q, want %q", got.Scope, zone.ScopeReverse)
		}
	}

	// Reconciling writes exactly those, and the check then has nothing to say.
	if _, rerr := f.a.Reconcile(t.Context(), rev.ID, testMeta()); rerr != nil {
		t.Fatalf("Reconcile: %v", rerr)
	}
	after, err := f.a.CheckReverse(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("CheckReverse: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("still %d findings after reconciling: %+v", len(after), after)
	}
}

// A check writes nothing, which is the whole difference between it and the
// reconcile it plans.
func TestCheckReverseWritesNothing(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rev := f.reverseZone("0.0.10.in-addr.arpa.")
	f.storeA("www.example.com.", "10.0.0.1")

	before, serial := f.everything(), f.serial()
	if _, err := f.a.CheckReverse(t.Context(), rev.ID); err != nil {
		t.Fatalf("CheckReverse: %v", err)
	}
	if got := f.everything(); !slices.Equal(got, before) {
		t.Errorf("the store changed\n got: %v\nwant: %v", got, before)
	}
	if got := f.serial(); got != serial {
		t.Errorf("the serial moved to %s, want it left at %s", got, serial)
	}
}

// A conflict is a state two records are in, not an event that was recorded, so
// the check derives it rather than reading it back from anywhere (D33).
func TestCheckReverseReportsAConflict(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rev := f.reverseZone("2.0.192.in-addr.arpa.")
	f.addA("www.example.com.", "192.0.2.10")
	// The second name loses under first-wins, and gets no entry of its own.
	f.addA("mail.example.com.", "192.0.2.10")

	found, err := f.a.CheckReverse(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("CheckReverse: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(found), found)
	}

	got := found[0]
	if got.Severity != zone.SeverityWarning {
		t.Errorf("severity is %q, want a warning: the write path accepted this", got.Severity)
	}
	for _, want := range []string{"mail.example.com.", "192.0.2.10", "www.example.com."} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail is %q, want it to name %q", got.Detail, want)
		}
	}

	// And it stops being reported when it stops being true, without anything
	// having to clear it: the loser is now the only claim on the address.
	f.mustApply(f.command(apply.RecordOp{
		Action:   apply.ActionDelete,
		RecordID: f.recordID("www.example.com.", zone.TypeA),
	}))
	after, err := f.a.CheckReverse(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("CheckReverse: %v", err)
	}
	for _, finding := range after {
		if strings.Contains(finding.Detail, "already names") {
			t.Errorf("the conflict is still reported after the winner went: %+v", finding)
		}
	}
}

// recordID finds the identifier of one record, for a test that has to address
// it rather than describe it.
func (f *fixture) recordID(name string, typ zone.RRType) zone.RecordID {
	f.t.Helper()

	for rec, err := range f.s.IterZoneRecords(f.t.Context(), f.z.ID) {
		if err != nil {
			f.t.Fatalf("IterZoneRecords: %v", err)
		}
		if rec.Name.String() == name && rec.Type == typ {
			return rec.ID
		}
	}
	f.t.Fatalf("no %s record at %s", typ, name)
	return ""
}
