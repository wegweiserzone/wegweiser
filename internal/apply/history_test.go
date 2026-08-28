package apply_test

import (
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// since is Since against the fixture's zone, with the failure already checked.
func (f *fixture) since(from, to zone.Serial) ([]*journal.Commit, bool) {
	f.t.Helper()

	cs, ok, err := f.a.Since(f.t.Context(), f.z.Name, from, to)
	if err != nil {
		f.t.Fatalf("Since(%s, %s): %v", from, to, err)
	}
	return cs, ok
}

func TestSinceReturnsTheRangeOldestFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// The fixture's clock does not move, so every one of these is recorded in
	// the same millisecond and the listing orders them by identifier, which is
	// to say at random. The range still has to come back in the order it
	// happened.
	start := f.serial()
	names := []string{"a", "b", "c", "d", "e"}
	for _, name := range names {
		f.add(name)
	}
	end := f.serial()

	cs, ok := f.since(start, end)
	if !ok {
		t.Fatal("the history does not cover a range it just recorded")
	}
	if len(cs) != len(names) {
		t.Fatalf("the range holds %d commits, want %d", len(cs), len(names))
	}

	at := start
	for i, c := range cs {
		if c.SerialFrom != at {
			t.Fatalf("commit %d starts at %s, want %s", i, c.SerialFrom, at)
		}
		// The listing carries no events, so a commit read for a transfer has to
		// be read in full or there would be nothing to send.
		if want := "+" + names[i] + ".example.com."; !strings.HasPrefix(
			strings.Join(eventLines(c), " "), want) {
			t.Errorf("commit %d recorded %v, want it to open with %q", i, eventLines(c), want)
		}
		at = c.SerialTo
	}
	if at != end {
		t.Errorf("the range ends at %s, want %s", at, end)
	}
}

func TestSinceStopsAtTheSerialTheCallerNamed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	start := f.serial()
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("a.example.com.", zone.TypeA, 300, "192.0.2.1"),
	}))
	published := f.serial()
	// A write that has landed in the database but not yet in the snapshot a
	// transfer is served from. Sending it would have a secondary announce a
	// version this server does not answer with.
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("b.example.com.", zone.TypeA, 300, "192.0.2.2"),
	}))

	cs, ok := f.since(start, published)
	if !ok {
		t.Fatal("the history does not cover a range it just recorded")
	}
	if len(cs) != 1 {
		t.Fatalf("the range holds %d commits, want 1", len(cs))
	}
	if cs[0].SerialTo != published {
		t.Errorf("the range ends at %s, want %s", cs[0].SerialTo, published)
	}
}

func TestSinceReportsARangeItCannotCover(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("a.example.com.", zone.TypeA, 300, "192.0.2.1"),
	}))
	now := f.serial()

	tests := []struct {
		name     string
		from, to zone.Serial
	}{
		{"a serial the zone never held", zone.NewSerial(9999), now},
		{"a version nobody has been answered from", f.serial(), zone.NewSerial(9999)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if cs, ok := f.since(tc.from, tc.to); ok {
				t.Errorf("the range is reported as covered, by %d commits", len(cs))
			}
		})
	}
}

func TestSinceOfAZoneThatIsNotHere(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, ok, err := f.a.Since(f.t.Context(), zone.MustParseName("example.org."),
		zone.NewSerial(0), f.serial())
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if ok {
		t.Error("a zone this server does not hold reports a covered range")
	}
}
