package zonefile_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
	"github.com/wegweiserzone/wegweiser/internal/zonefile"
)

// TestRoundTrip is the property the whole package exists for: what this server
// writes, this server reads back unchanged. Anything less means an export that
// cannot be re-imported, which is a migration path in one direction only.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	first, err := zonefile.Parse(strings.NewReader(realistic), zonefile.Options{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var out bytes.Buffer
	if werr := zonefile.Write(&out, first); werr != nil {
		t.Fatalf("write: %v", werr)
	}

	second, err := zonefile.Parse(strings.NewReader(out.String()), zonefile.Options{})
	if err != nil {
		t.Fatalf("re-read what was written: %v\n%s", err, out.String())
	}

	if !second.Origin.Equal(first.Origin) {
		t.Errorf("origin %q became %q", first.Origin, second.Origin)
	}
	if second.SOA != first.SOA {
		t.Errorf("SOA changed\n  before %+v\n  after  %+v", first.SOA, second.SOA)
	}
	// Compared as a set: an export is the same zone in canonical order (RFC
	// 4034 §6.1), not the same file in the order its author happened to type
	// it. Reordering is the point of a canonical order, so requiring the
	// original order here would be testing against the wrong property.
	if got, want := renderAll(second.Records), renderAll(first.Records); got != want {
		t.Errorf("records changed\n  before\n%s  after\n%s", want, got)
	}

	t.Run("writing it again produces the same bytes", func(t *testing.T) {
		// Exporting the same zone twice has to be the same file, or every
		// export looks like a change to whatever is watching the output.
		var again bytes.Buffer
		if werr := zonefile.Write(&again, second); werr != nil {
			t.Fatalf("write: %v", werr)
		}
		if again.String() != out.String() {
			t.Errorf("the second write differs\n%s", again.String())
		}
	})
}

func TestWrite(t *testing.T) {
	t.Parallel()

	c := &zonefile.Content{
		Origin: zone.MustParseName("example.com."),
		SOA: zone.SOA{
			NS: zone.MustParseName("ns1.example.com."), Mbox: zone.MustParseName("hostmaster.example.com."),
			Serial: zone.NewSerial(4294967295), Refresh: 7200, Retry: 900,
			Expire: 1209600, Minimum: 3600, TTL: 3600,
		},
		Records: []zone.Record{
			rec(t, "www.example.com.", zone.TypeA, 300, "192.0.2.10"),
			rec(t, "example.com.", zone.TypeNS, 3600, "ns1.example.com."),
			rec(t, "b.example.com.", zone.TypeA, 300, "192.0.2.2"),
			rec(t, "a.example.com.", zone.TypeA, 300, "192.0.2.1"),
		},
	}

	var out bytes.Buffer
	if err := zonefile.Write(&out, c); err != nil {
		t.Fatalf("write: %v", err)
	}
	text := out.String()

	t.Run("it says what zone it is before anything else", func(t *testing.T) {
		if !strings.HasPrefix(text, "$ORIGIN example.com.\n$TTL 3600\n") {
			t.Errorf("the file starts\n%q", text[:min(len(text), 60)])
		}
	})

	t.Run("the SOA is the first record (RFC 1035 §5.2)", func(t *testing.T) {
		soa := strings.Index(text, "SOA")
		other := strings.Index(text, "\tNS\t")
		if soa < 0 || other < 0 || soa > other {
			t.Errorf("the SOA is not first:\n%s", text)
		}
	})

	t.Run("a serial past the TTL ceiling survives", func(t *testing.T) {
		// A serial runs to 2^32-1 and a TTL stops at 2^31-1, so borrowing one
		// type for the other breaks on a zone old enough to have wrapped.
		if !strings.Contains(text, "4294967295") {
			t.Errorf("the serial was not written:\n%s", text)
		}
	})

	t.Run("records come out in canonical order", func(t *testing.T) {
		want := []string{"example.com.\t3600\tIN\tNS", "a.example.com.", "b.example.com.", "www.example.com."}
		at := 0
		for _, w := range want {
			i := strings.Index(text[at:], w)
			if i < 0 {
				t.Fatalf("%q is missing or out of order in\n%s", w, text)
			}
			at += i
		}
	})

	t.Run("a disabled record is not exported", func(t *testing.T) {
		// It is not part of the zone as it answers, and a file has nowhere to
		// say "present but switched off"; writing it would hand another server
		// records this one does not serve.
		off := rec(t, "off.example.com.", zone.TypeA, 300, "192.0.2.99")
		off.Disabled = true
		c.Records = append(c.Records, off)

		var with bytes.Buffer
		if err := zonefile.Write(&with, c); err != nil {
			t.Fatalf("write: %v", err)
		}
		if strings.Contains(with.String(), "off.example.com.") {
			t.Errorf("a disabled record was exported:\n%s", with.String())
		}
	})

	t.Run("a zone with no start of authority is refused", func(t *testing.T) {
		if err := zonefile.Write(&bytes.Buffer{}, &zonefile.Content{
			Origin: zone.MustParseName("example.com."),
		}); err == nil {
			t.Error("wrote a file whose SOA would not have parsed back")
		}
	})
}

func rec(t *testing.T, name string, typ zone.RRType, ttl zone.TTL, data string) zone.Record {
	t.Helper()
	r, err := zone.NewRecord("", zone.MustParseName(name), zone.ClassIN, typ, ttl, data)
	if err != nil {
		t.Fatalf("NewRecord(%s %s %s): %v", name, typ, data, err)
	}
	return r
}

// renderAll renders records as sorted lines, so that two sets can be compared
// without their order mattering.
func renderAll(recs []zone.Record) string {
	lines := make([]string, 0, len(recs))
	for i := range recs {
		lines = append(lines, "    "+recs[i].String())
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n") + "\n"
}
