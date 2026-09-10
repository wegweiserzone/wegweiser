package apply

import (
	"bytes"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// testContent is one of everything a log snapshot carries, including the
// halves a snapshot has and a batch does not: a revoked token, and a revoked
// key with no secret left.
func testContent(t *testing.T) []*store.Replicated {
	t.Helper()

	fwd, err := zone.NewZone(zone.MustParseName("example.com."), zone.DefaultSOA(
		zone.MustParseName("ns1.example.com."), zone.MustParseName("hostmaster.example.com.")))
	if err != nil {
		t.Fatalf("NewZone: %v", err)
	}
	fwd.ID = zone.ZoneID(id.New())
	fwd.CreatedAt, fwd.UpdatedAt = codecTime, codecTime

	rev, err := zone.NewZone(zone.MustParseName("0.0.10.in-addr.arpa."), fwd.SOA)
	if err != nil {
		t.Fatalf("NewZone: %v", err)
	}
	rev.ID = zone.ZoneID(id.New())
	rev.CreatedAt, rev.UpdatedAt = codecTime, codecTime

	rec := func(z zone.Zone, name string, typ zone.RRType, rdata string) *zone.Record {
		t.Helper()
		r, rerr := zone.NewRecord(z.ID, zone.MustParseName(name), zone.ClassIN, typ, 300, rdata)
		if rerr != nil {
			t.Fatalf("NewRecord(%s %s %s): %v", name, typ, rdata, rerr)
		}
		r.ID = zone.RecordID(id.New())
		r.CreatedAt, r.UpdatedAt = codecTime, codecTime
		return &r
	}
	www := rec(fwd, "www.example.com.", zone.TypeA, "10.0.0.1")
	ptr := rec(rev, "1.0.0.10.in-addr.arpa.", zone.TypePTR, "www.example.com.")
	ptr.ManagedBy, ptr.ManagedKind = www.ID, zone.ManagedPTR

	hash := func(b byte) []byte {
		h := make([]byte, 32)
		h[0] = b
		return h
	}

	return []*store.Replicated{
		{Kind: store.ReplicatedSetting, Setting: &store.Setting{
			Key: TransferSetting, Value: []byte(`{"prefixes":["192.0.2.0/24"]}`)}},
		{Kind: store.ReplicatedToken, Token: &store.Token{
			ID: store.TokenID(id.New()), Name: "deploy", Prefix: "wgw_dep", Hash: hash(1),
			Scopes: []string{"zones:write"}, CreatedAt: codecTime}},
		{Kind: store.ReplicatedToken, Token: &store.Token{
			ID: store.TokenID(id.New()), Name: "retired", Prefix: "wgw_ret", Hash: hash(2),
			Scopes: []string{"zones:read"}, CreatedAt: codecTime, RevokedAt: codecTime}},
		{Kind: store.ReplicatedTSIGKey, TSIGKey: &store.TSIGKey{
			ID: store.TSIGKeyID(id.New()), Name: zone.MustParseName("ns2.example.com."),
			Algorithm: zone.HMACSHA256, Secret: []byte("thirty-two octets of key material"),
			CreatedAt: codecTime}},
		{Kind: store.ReplicatedTSIGKey, TSIGKey: &store.TSIGKey{
			ID: store.TSIGKeyID(id.New()), Name: zone.MustParseName("old.example.com."),
			Algorithm: zone.HMACSHA256, CreatedAt: codecTime, RevokedAt: codecTime}},
		{Kind: store.ReplicatedZone, Zone: &fwd},
		{Kind: store.ReplicatedZone, Zone: &rev},
		{Kind: store.ReplicatedRecord, Record: www},
		{Kind: store.ReplicatedRecord, Record: ptr},
		{Kind: store.ReplicatedCommit, Commit: &journal.Commit{
			ID: journal.CommitID(id.New()), ZoneID: fwd.ID, ZoneName: fwd.Name,
			SerialFrom: zone.NewSerial(1), SerialTo: zone.NewSerial(2),
			Kind: journal.KindEdit, Source: journal.SourceAPI, Actor: "deploy",
			Events: []journal.Event{{
				Seq: 0, Op: journal.OpAdd, Name: www.Name, Class: www.Class,
				Type: www.Type, TTL: www.TTL, RData: www.RData,
			}},
			CreatedAt: codecTime,
		}},
	}
}

func writeSnapshot(t *testing.T, h SnapshotHeader, items []*store.Replicated) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := WriteSnapshot(&buf, h, streamOf(items)); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	return buf.Bytes()
}

// streamOf hands a slice over as the stream an export produces.
func streamOf(items []*store.Replicated) iter.Seq2[*store.Replicated, error] {
	return func(yield func(*store.Replicated, error) bool) {
		for _, item := range items {
			if !yield(item, nil) {
				return
			}
		}
	}
}

// readSnapshot reads a whole snapshot, and returns the first error met on the
// way, in the header or in the content.
func readSnapshot(data []byte) (SnapshotHeader, []*store.Replicated, error) {
	h, items, err := ReadSnapshot(bytes.NewReader(data))
	if err != nil {
		return h, nil, err
	}
	var out []*store.Replicated
	for item, ierr := range items {
		if ierr != nil {
			return h, out, ierr
		}
		out = append(out, item)
	}
	return h, out, nil
}

var sealed = SnapshotHeader{Index: 42, Writer: WriterStore}

func TestSnapshotSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	want := writeSnapshot(t, sealed, testContent(t))
	h, items, err := readSnapshot(want)
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if h != sealed {
		t.Errorf("the header came back as %+v, want %+v", h, sealed)
	}
	if got := writeSnapshot(t, h, items); !bytes.Equal(got, want) {
		t.Errorf("rewriting a snapshot that was read back changed it\n got: %s\nwant: %s", got, want)
	}
}

func TestSnapshotDecodesToValuesAndNotToText(t *testing.T) {
	t.Parallel()

	_, items, err := readSnapshot(writeSnapshot(t, sealed, testContent(t)))
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}

	var revokedKey, managed, address bool
	for _, item := range items {
		switch item.Kind {
		case store.ReplicatedTSIGKey:
			if !item.TSIGKey.RevokedAt.IsZero() {
				revokedKey = len(item.TSIGKey.Secret) == 0 && !item.TSIGKey.Name.IsZero()
			}
		case store.ReplicatedRecord:
			if item.Record.IsManaged() {
				managed = item.Record.ManagedKind == zone.ManagedPTR
			}
			if _, ok := item.Record.RData.Address(zone.TypeA); ok {
				address = true
			}
		default:
		}
	}
	if !revokedKey {
		t.Error("the revoked key did not come back as a name without a secret")
	}
	if !managed {
		t.Error("the generated record lost its provenance")
	}
	if !address {
		t.Error("the address record's data came back as text rather than as an address")
	}
}

// A witness snapshots an empty state machine (D39). It still says where in the
// log it stands, and it says what wrote it, which is what a restore checks.
func TestAWitnessSnapshotSaysSo(t *testing.T) {
	t.Parallel()

	witness := SnapshotHeader{Index: 7, Writer: WriterWitness}
	var buf bytes.Buffer
	if err := WriteSnapshot(&buf, witness, nil); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	h, items, err := readSnapshot(buf.Bytes())
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if h != witness || len(items) != 0 {
		t.Errorf("read %+v with %d items, want %+v with none", h, len(items), witness)
	}
}

func TestSnapshotIgnoresAFieldItDoesNotKnow(t *testing.T) {
	t.Parallel()

	data := string(writeSnapshot(t, sealed, testContent(t)))
	grown := strings.Replace(data, `"version":1,`, `"version":1,"somethingLater":[1,2],`, 1)
	grown = strings.Replace(grown, `{"kind":"zone",`, `{"kind":"zone","alsoLater":{"a":1},`, 1)
	if grown == data {
		t.Fatal("the test did not manage to add a field")
	}

	h, items, err := readSnapshot([]byte(grown))
	if err != nil {
		t.Fatalf("an unknown field was refused: %v", err)
	}
	if got := writeSnapshot(t, h, items); !bytes.Equal(got, []byte(data)) {
		t.Errorf("an unknown field disturbed the rest\n got: %s\nwant: %s", got, data)
	}
}

func TestSnapshotRefusesWhatItShould(t *testing.T) {
	t.Parallel()

	content := testContent(t)
	sound := string(writeSnapshot(t, sealed, content))
	end := fmt.Sprintf(`{"kind":"end","count":%d}`, len(content))
	if !strings.Contains(sound, end) {
		t.Fatalf("the end mark is not %s:\n%s", end, sound)
	}

	tests := []struct {
		name   string
		mangle func(string) string
		want   string
	}{
		{
			name:   "something that is not a log snapshot",
			mangle: func(s string) string { return strings.Replace(s, snapshotFormat, "a-shopping-list", 1) },
			want:   "not a log snapshot",
		},
		{
			name:   "a version this build does not read",
			mangle: func(s string) string { return strings.Replace(s, `"version":1`, `"version":2`, 1) },
			want:   "version 2 log snapshot",
		},
		{
			name:   "a writer this build does not know",
			mangle: func(s string) string { return strings.Replace(s, `"writer":"store"`, `"writer":"arbiter"`, 1) },
			want:   `written by a "arbiter"`,
		},
		{
			// Content is not additive the way a field is (D30).
			name:   "a kind of content this build does not know",
			mangle: func(s string) string { return strings.Replace(s, `{"kind":"zone",`, `{"kind":"view",`, 1) },
			want:   `kind "view"`,
		},
		{
			name: "an item carrying two things",
			mangle: func(s string) string {
				return strings.Replace(s, `{"kind":"zone",`, `{"kind":"zone","setting":{"key":"k","value":1},`, 1)
			},
			want: "carries 2 things",
		},
		{
			// Cut exactly between two items: every line before the cut is sound.
			name:   "a snapshot that stops before its end",
			mangle: func(s string) string { return strings.Replace(s, end+"\n", "", 1) },
			want:   "without saying it is complete",
		},
		{
			name: "an end that miscounts",
			mangle: func(s string) string {
				return strings.Replace(s, end, fmt.Sprintf(`{"kind":"end","count":%d}`, len(content)+1), 1)
			},
			want: "holds 11 items and held 10",
		},
		{
			name:   "something after the end",
			mangle: func(s string) string { return s + `{"kind":"setting"}` + "\n" },
			want:   "after its end",
		},
		{
			// D28 clears the secret of a revoked key. One that still carries
			// it is a key two ends disagree about.
			name:   "a revoked key that still has a secret",
			mangle: func(s string) string { return strings.Replace(s, `"secret":null`, `"secret":"c2VjcmV0"`, 1) },
			want:   "both or neither",
		},
		{
			name:   "rdata that parses but is not canonical",
			mangle: func(s string) string { return strings.Replace(s, `"rdata":"10.0.0.1"`, `"rdata":"10.000.0.1"`, 1) },
			want:   "invalid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mangled := tt.mangle(sound)
			if mangled == sound {
				t.Fatal("the mangling changed nothing, so the test tests nothing")
			}
			_, _, err := readSnapshot([]byte(mangled))
			if err == nil {
				t.Fatal("the snapshot was read without complaint")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
}
