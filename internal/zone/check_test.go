package zone_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// checkZone runs a check over records given in any order, sorting them the way
// the store already keeps them so that a test reads as a zone rather than as a
// sequence. Every name server counts as addressed, so that the table below is
// about the rules; the diagnosis has tests of its own.
func checkZone(t *testing.T, records []zone.Record) zone.Report {
	t.Helper()

	return runCheck(t, sortRecords(records), func(zone.Name) (bool, error) { return true, nil })
}

// sortRecords puts records into the order the store keeps them in.
func sortRecords(records []zone.Record) []zone.Record {
	sorted := slices.Clone(records)
	slices.SortStableFunc(sorted, func(a, b zone.Record) int {
		if c := a.Name.Compare(b.Name); c != 0 {
			return c
		}
		return int(a.Type) - int(b.Type)
	})
	return sorted
}

// runCheck feeds records that are already in order, with the answer the check
// cannot work out on its own.
func runCheck(t *testing.T, sorted []zone.Record, addressed zone.Addressed) zone.Report {
	t.Helper()

	c := zone.NewCheck(newTestZone(t, "example.com."))
	for i := range sorted {
		c.Add(&sorted[i])
	}
	rep, err := c.Done(addressed)
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	return rep
}

// addressedFrom answers from the records themselves, which is what the store
// answers from for real.
func addressedFrom(records []zone.Record) zone.Addressed {
	return func(name zone.Name) (bool, error) {
		for i := range records {
			r := &records[i]
			if r.Name.Equal(name) && (r.Type == zone.TypeA || r.Type == zone.TypeAAAA) {
				return true, nil
			}
		}
		return false, nil
	}
}

func TestCheckReportsWhatIsWrong(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records func(*testing.T) []zone.Record
		want    []zone.FindingScope
		phrase  string // a phrase the first finding must contain
	}{
		{
			name: "a sound zone has nothing to say",
			records: func(t *testing.T) []zone.Record {
				return append(apexNS(t),
					rec(t, "www.example.com.", 300, zone.TypeA, "192.0.2.1"),
					rec(t, "www.example.com.", 300, zone.TypeA, "192.0.2.2"))
			},
		},
		{
			name: "an apex with no NS",
			records: func(t *testing.T) []zone.Record {
				return []zone.Record{rec(t, "www.example.com.", 300, zone.TypeA, "192.0.2.1")}
			},
			want:   []zone.FindingScope{zone.ScopeZone},
			phrase: "no NS record at its apex",
		},
		{
			name: "one RRset with two TTLs",
			records: func(t *testing.T) []zone.Record {
				return append(apexNS(t),
					rec(t, "www.example.com.", 300, zone.TypeA, "192.0.2.1"),
					rec(t, "www.example.com.", 600, zone.TypeA, "192.0.2.2"))
			},
			want:   []zone.FindingScope{zone.ScopeOwner},
			phrase: "different TTLs",
		},
		{
			name: "a CNAME beside other data",
			records: func(t *testing.T) []zone.Record {
				return append(apexNS(t),
					rec(t, "www.example.com.", 300, zone.TypeCNAME, "host.example.com."),
					rec(t, "www.example.com.", 300, zone.TypeA, "192.0.2.1"))
			},
			want:   []zone.FindingScope{zone.ScopeOwner},
			phrase: "CNAME alongside other records",
		},
		{
			name: "something other than NS at a delegation point",
			records: func(t *testing.T) []zone.Record {
				return append(apexNS(t),
					rec(t, "sub.example.com.", 3600, zone.TypeNS, "ns1.other.example."),
					rec(t, "sub.example.com.", 300, zone.TypeTXT, `"hello"`))
			},
			want:   []zone.FindingScope{zone.ScopeDelegation},
			phrase: "referred to the child",
		},
		{
			name: "something other than glue below a delegation",
			records: func(t *testing.T) []zone.Record {
				return append(apexNS(t),
					rec(t, "sub.example.com.", 3600, zone.TypeNS, "ns1.other.example."),
					rec(t, "host.sub.example.com.", 300, zone.TypeTXT, `"hello"`))
			},
			want:   []zone.FindingScope{zone.ScopeDelegation},
			phrase: "only A and AAAA glue",
		},
		{
			name: "glue below a delegation is what belongs there",
			records: func(t *testing.T) []zone.Record {
				return append(apexNS(t),
					rec(t, "sub.example.com.", 3600, zone.TypeNS, "ns1.sub.example.com."),
					rec(t, "ns1.sub.example.com.", 300, zone.TypeA, "192.0.2.53"))
			},
		},
		{
			// A delegation stops applying at the end of its subtree, and
			// canonical order is what makes that a pop rather than a search.
			name: "a name after a delegation is not judged by it",
			records: func(t *testing.T) []zone.Record {
				return append(apexNS(t),
					rec(t, "sub.example.com.", 3600, zone.TypeNS, "ns1.other.example."),
					rec(t, "www.example.com.", 300, zone.TypeTXT, `"hello"`))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rep := checkZone(t, tc.records(t))

			got := make([]zone.FindingScope, len(rep.Findings))
			for i, f := range rep.Findings {
				got[i] = f.Scope
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("scopes = %v, want %v\nfindings: %+v", got, tc.want, rep.Findings)
			}
			if rep.Sound() != (len(tc.want) == 0) {
				t.Errorf("Sound() = %v with %d findings", rep.Sound(), len(rep.Findings))
			}
			if tc.phrase != "" && !strings.Contains(rep.Findings[0].Detail, tc.phrase) {
				t.Errorf("detail is %q, want it to contain %q", rep.Findings[0].Detail, tc.phrase)
			}
			// Everything a check knows about today is refused by the write
			// path, so everything it reports is an error (D31).
			for _, f := range rep.Findings {
				if f.Severity != zone.SeverityError {
					t.Errorf("%q is %q, want %q", f.Name, f.Severity, zone.SeverityError)
				}
			}
			if rep.Errors() != len(rep.Findings) {
				t.Errorf("Errors() = %d with %d findings", rep.Errors(), len(rep.Findings))
			}
		})
	}
}

// The point of a check is the list. One that stopped at the first problem
// would be the write path with extra steps.
func TestCheckDoesNotStopAtTheFirstProblem(t *testing.T) {
	t.Parallel()

	rep := checkZone(t, append(apexNS(t),
		rec(t, "a.example.com.", 300, zone.TypeA, "192.0.2.1"),
		rec(t, "a.example.com.", 600, zone.TypeA, "192.0.2.2"),
		rec(t, "b.example.com.", 300, zone.TypeCNAME, "a.example.com."),
		rec(t, "b.example.com.", 300, zone.TypeMX, "10 a.example.com.")))

	if len(rep.Findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(rep.Findings), rep.Findings)
	}
	for i, want := range []string{"a.example.com.", "b.example.com."} {
		if got := rep.Findings[i].Name.String(); got != want {
			t.Errorf("finding %d is about %q, want %q", i, got, want)
		}
	}
	if rep.Records != 5 {
		t.Errorf("Records = %d, want 5", rep.Records)
	}
}

// A finding has to say what the write path would say, or repairing a zone
// until the check is quiet would not make it writable.
func TestAFindingSaysWhatTheWritePathWouldSay(t *testing.T) {
	t.Parallel()

	owned := []zone.Record{
		rec(t, "www.example.com.", 300, zone.TypeCNAME, "host.example.com."),
		rec(t, "www.example.com.", 300, zone.TypeA, "192.0.2.1"),
	}

	refused := zone.ValidateOwner(newTestZone(t, "example.com."),
		zone.MustParseName("www.example.com."), owned)
	if refused == nil {
		t.Fatal("the write path accepted a CNAME beside an A record")
	}

	rep := checkZone(t, append(owned, apexNS(t)...))
	if len(rep.Findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(rep.Findings), rep.Findings)
	}
	if !strings.Contains(refused.Error(), rep.Findings[0].Detail) {
		t.Errorf("the check says %q\nthe write path says %q", rep.Findings[0].Detail, refused)
	}
}

func TestCheckStopsCollectingAtTheLimit(t *testing.T) {
	t.Parallel()

	records := apexNS(t)
	for i := range zone.MaxFindings + 10 {
		name := fmt.Sprintf("h%d.example.com.", i)
		records = append(records,
			rec(t, name, 300, zone.TypeA, "192.0.2.1"),
			rec(t, name, 600, zone.TypeA, "192.0.2.2"))
	}

	rep := checkZone(t, records)

	if len(rep.Findings) != zone.MaxFindings {
		t.Errorf("got %d findings, want %d", len(rep.Findings), zone.MaxFindings)
	}
	if !rep.Truncated {
		t.Error("the report does not say it was truncated")
	}
	if rep.Records != len(records) {
		t.Errorf("Records = %d, want %d", rep.Records, len(records))
	}
}

// A name server inside the zone with no address there is a lame delegation
// (RFC 1912 §2.8). It is correct DNS, so it is a warning rather than an error.
func TestCheckReportsALameDelegation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records func(*testing.T) []zone.Record
		want    int
	}{
		{
			// The apex NS points at ns1.example.com., and nothing addresses it.
			name:    "a name server here with no address here",
			records: apexNS,
			want:    1,
		},
		{
			name: "the same name server, addressed",
			records: func(t *testing.T) []zone.Record {
				return append(apexNS(t),
					rec(t, "ns1.example.com.", 300, zone.TypeA, "192.0.2.53"))
			},
		},
		{
			name: "a name server somebody else answers for",
			records: func(t *testing.T) []zone.Record {
				return []zone.Record{
					rec(t, "example.com.", 3600, zone.TypeNS, "ns1.other.example."),
				}
			},
		},
		{
			// Two records naming one server is one question, so one answer.
			name: "one name server named twice",
			records: func(t *testing.T) []zone.Record {
				return append(apexNS(t),
					rec(t, "sub.example.com.", 3600, zone.TypeNS, "ns1.example.com."))
			},
			want: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			records := tc.records(t)
			rep := runCheck(t, sortRecords(records), addressedFrom(records))

			if len(rep.Findings) != tc.want {
				t.Fatalf("got %d findings, want %d: %+v", len(rep.Findings), tc.want, rep.Findings)
			}
			for _, f := range rep.Findings {
				if f.Severity != zone.SeverityWarning {
					t.Errorf("%q is %q, want a warning", f.Name, f.Severity)
				}
				if f.Scope != zone.ScopeNameServer {
					t.Errorf("scope is %q, want %q", f.Scope, zone.ScopeNameServer)
				}
			}
			if rep.Errors() != 0 {
				t.Errorf("Errors() = %d, want 0: a lame delegation is not a refusal", rep.Errors())
			}
		})
	}
}

// The two kinds sit in one report and stay countable apart.
func TestCheckSeparatesRefusalsFromDiagnoses(t *testing.T) {
	t.Parallel()

	records := append(apexNS(t),
		rec(t, "www.example.com.", 300, zone.TypeA, "192.0.2.1"),
		rec(t, "www.example.com.", 600, zone.TypeA, "192.0.2.2"))

	rep := runCheck(t, sortRecords(records), addressedFrom(records))

	if len(rep.Findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(rep.Findings), rep.Findings)
	}
	if rep.Errors() != 1 {
		t.Errorf("Errors() = %d, want 1", rep.Errors())
	}
	// The refusal first: it is read in the order the names were walked, and the
	// diagnosis needs an answer the walk could not give.
	if rep.Findings[0].Severity != zone.SeverityError {
		t.Errorf("the first finding is %q, want the error", rep.Findings[0].Severity)
	}
	if rep.Findings[1].Severity != zone.SeverityWarning {
		t.Errorf("the second finding is %q, want the warning", rep.Findings[1].Severity)
	}
}
