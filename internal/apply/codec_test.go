package apply

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

var codecTime = time.Date(2026, time.August, 22, 9, 30, 0, 0, time.UTC)

// testBatch holds one of everything that travels: a record removed, replaced
// and added, a start of authority, a second zone, and a generated record.
func testBatch(t *testing.T) *Batch {
	t.Helper()

	fwd := zone.ZoneID(id.New())
	rev := zone.ZoneID(id.New())
	fwdName := zone.MustParseName("example.com.")
	revName := zone.MustParseName("0.0.10.in-addr.arpa.")

	rec := func(zid zone.ZoneID, name string, typ zone.RRType, ttl zone.TTL, rdata string) zone.Record {
		t.Helper()
		r, err := zone.NewRecord(zid, zone.MustParseName(name), zone.ClassIN, typ, ttl, rdata)
		if err != nil {
			t.Fatalf("NewRecord(%s %s %s): %v", name, typ, rdata, err)
		}
		r.ID = zone.RecordID(id.New())
		r.CreatedAt, r.UpdatedAt = codecTime, codecTime
		return r
	}

	gone := rec(fwd, "old.example.com.", zone.TypeA, 300, "10.0.0.9")
	was := rec(fwd, "www.example.com.", zone.TypeA, 300, "10.0.0.1")
	now := was
	now.RData = zone.MustParseRData(zone.TypeA, zone.ClassIN, "10.0.0.2")
	now.Comment = "moved"
	added := rec(fwd, "mail.example.com.", zone.TypeMX, 3600, "10 mx1.example.com.")

	ptr := rec(rev, "2.0.0.10.in-addr.arpa.", zone.TypePTR, 300, "www.example.com.")
	ptr.ManagedBy = now.ID
	ptr.ManagedKind = zone.ManagedPTR

	before := zone.DefaultSOA(zone.MustParseName("ns1.example.com."),
		zone.MustParseName("hostmaster.example.com."))
	before.Serial = zone.NewSerial(7)
	after := before
	after.Minimum = 900

	settings, err := zone.NewZone(fwdName, after)
	if err != nil {
		t.Fatalf("NewZone: %v", err)
	}
	settings.ID = fwd
	settings.Comment = "the office"
	settings.CreatedAt, settings.UpdatedAt = codecTime, codecTime

	set := &changeSet{
		byZone: map[zone.ZoneID]*changes{},
		zones:  map[zone.ZoneID]*zone.Zone{},
		order:  []zone.ZoneID{fwd, rev},
	}
	set.byZone[fwd] = &changes{
		deletes: []zone.Record{gone},
		updates: []recordUpdate{{before: was, after: now}},
		inserts: []zone.Record{added},
		soa:     &soaChange{before: before, after: after},
	}
	set.byZone[fwd].touch(gone.Name)
	set.byZone[fwd].touch(now.Name)
	set.byZone[fwd].touch(added.Name)

	set.byZone[rev] = &changes{inserts: []zone.Record{ptr}}
	set.byZone[rev].touch(ptr.Name)

	commit := func(zid zone.ZoneID, name zone.Name, from uint32, events []journal.Event) *journal.Commit {
		f := zone.NewSerial(from)
		return &journal.Commit{
			ID:         journal.CommitID(id.New()),
			ZoneID:     zid,
			ZoneName:   name,
			SerialFrom: f,
			SerialTo:   f.Next(),
			Kind:       journal.KindEdit,
			Source:     journal.SourceAPI,
			Actor:      "deploy-token",
			Events:     events,
			CreatedAt:  codecTime,
		}
	}

	event := func(seq int, op journal.Op, r zone.Record) journal.Event {
		return journal.Event{
			Seq: seq, Op: op,
			Name: r.Name, Class: r.Class, Type: r.Type, TTL: r.TTL, RData: r.RData,
		}
	}

	return &Batch{
		set:   set,
		Zones: []ZoneOp{{Kind: ZoneUpdate, ZoneID: fwd, Zone: &settings}},
		Commits: []*journal.Commit{
			commit(fwd, fwdName, 7, []journal.Event{
				event(0, journal.OpDel, gone),
				event(1, journal.OpDel, was),
				event(2, journal.OpAdd, now),
				event(3, journal.OpAdd, added),
			}),
			commit(rev, revName, 3, []journal.Event{
				event(0, journal.OpAdd, ptr),
			}),
		},
	}
}

func encode(t *testing.T, b *Batch) []byte {
	t.Helper()

	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return data
}

func decode(t *testing.T, data []byte) *Batch {
	t.Helper()

	var b Batch
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return &b
}

func TestBatchSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	want := encode(t, testBatch(t))
	got := encode(t, decode(t, want))

	if !bytes.Equal(got, want) {
		t.Errorf("re-encoding a decoded batch changed it\n got: %s\nwant: %s", got, want)
	}
}

func TestBatchDecodesToValuesAndNotToText(t *testing.T) {
	t.Parallel()

	// Byte equality would also hold if every field came back as the string it
	// was written as, so the parts that are not strings are checked by hand.
	got := decode(t, encode(t, testBatch(t)))

	if len(got.set.order) != 2 {
		t.Fatalf("the batch reaches %d zones, want 2", len(got.set.order))
	}
	fwd := got.set.byZone[got.set.order[0]]
	rev := got.set.byZone[got.set.order[1]]

	if fwd.updates[0].after.RData.String() != "10.0.0.2" {
		t.Errorf("rdata is %q, want the replacement", fwd.updates[0].after.RData)
	}
	if _, ok := fwd.updates[0].after.RData.Address(zone.TypeA); !ok {
		t.Error("the rdata came back as text rather than as an address")
	}
	if fwd.soa == nil || fwd.soa.after.Minimum != 900 {
		t.Errorf("the start of authority did not survive: %+v", fwd.soa)
	}
	if len(fwd.touched) != 3 {
		t.Errorf("%d touched names, want 3", len(fwd.touched))
	}

	ptr := rev.inserts[0]
	if ptr.ManagedBy != fwd.updates[0].after.ID {
		t.Errorf("provenance points at %q, want %q", ptr.ManagedBy, fwd.updates[0].after.ID)
	}
	if ptr.ManagedKind != zone.ManagedPTR {
		t.Errorf("managed kind is %q, want %q", ptr.ManagedKind, zone.ManagedPTR)
	}
}

func TestBatchEncodesTheSameBytesEveryTime(t *testing.T) {
	t.Parallel()

	// The touched names come out of a map, and a map has no order to preserve.
	b := testBatch(t)
	first := encode(t, b)
	for range 20 {
		if got := encode(t, b); !bytes.Equal(got, first) {
			t.Fatalf("encoding is not stable\n got: %s\nwant: %s", got, first)
		}
	}
}

func TestBatchIgnoresAFieldItDoesNotKnow(t *testing.T) {
	t.Parallel()

	// A rolling upgrade has a newer node replicating to an older one, so a
	// field the receiver has never heard of has to be survivable (D19).
	data := encode(t, testBatch(t))
	grown := strings.Replace(string(data), `{"version":1,`,
		`{"version":1,"somethingLater":{"a":[1,2]},`, 1)
	if grown == string(data) {
		t.Fatal("the test did not manage to add a field")
	}

	if got := encode(t, decode(t, []byte(grown))); !bytes.Equal(got, data) {
		t.Errorf("an unknown field disturbed the rest\n got: %s\nwant: %s", got, data)
	}
}

func TestBatchRefusesWhatItShould(t *testing.T) {
	t.Parallel()

	sound := string(encode(t, testBatch(t)))

	tests := []struct {
		name   string
		mangle func(string) string
		want   string
	}{
		{
			name:   "a version this build does not speak",
			mangle: func(s string) string { return strings.Replace(s, `"version":1`, `"version":2`, 1) },
			want:   "version 2 batch",
		},
		{
			name:   "rdata that does not parse",
			mangle: func(s string) string { return strings.Replace(s, `"10.0.0.2"`, `"010.0.0.2"`, 1) },
			want:   "invalid record data",
		},
		{
			// It parses; it is just not the form D18 stores.
			name: "rdata that parses but is not canonical",
			mangle: func(s string) string {
				return strings.Replace(s, `"10 mx1.example.com."`, `"10 MX1.EXAMPLE.COM."`, 1)
			},
			want: "canonical",
		},
		{
			name:   "a serial that steps by more than one",
			mangle: func(s string) string { return strings.Replace(s, `"serialTo":8`, `"serialTo":9`, 1) },
			want:   "exactly one step",
		},
		{
			name:   "an addition ahead of a deletion",
			mangle: func(s string) string { return strings.Replace(s, `"op":"del"`, `"op":"add"`, 1) },
			want:   "difference sequence",
		},
		{
			name: "a delete that carries a zone anyway",
			mangle: func(s string) string {
				return strings.Replace(s, `"kind":"update"`, `"kind":"delete"`, 1)
			},
			want: "only a delete does not",
		},
		{
			// The zone value leads with its identifier, so this swaps the one
			// the operation carries while leaving the one it names.
			name: "an operation naming one zone and carrying another",
			mangle: func(s string) string {
				const lead = `"zone":{"id":"`
				i := strings.Index(s, lead)
				if i < 0 {
					return s
				}
				j := i + len(lead)
				return s[:j] + "01ARZ3NDEKTSV4RRFFQ69G5FAV" + s[j+26:]
			},
			want: "carries zone",
		},
		{
			name:   "a record with no identity",
			mangle: func(s string) string { return strings.Replace(s, `"id":"`, `"id":"","was":"`, 1) },
			want:   "identity",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			broken := tt.mangle(sound)
			if broken == sound {
				t.Fatal("the test did not manage to break anything")
			}
			var b Batch
			err := json.Unmarshal([]byte(broken), &b)
			if err == nil {
				t.Fatal("decoded without complaint")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error is %q, want it to mention %q", err, tt.want)
			}
		})
	}
}
