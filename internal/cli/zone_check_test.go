package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestZoneCheck(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	mustRun(t, srv, "zone", "create", "example.com")
	// A new zone points at ns1 and has no address for it, which the check
	// reports until somebody adds one.
	mustRun(t, srv, "record", "add", "example.com", "ns1", "A", "192.0.2.53")
	mustRun(t, srv, "record", "add", "example.com", "www", "A", "192.0.2.10")

	t.Run("a sound zone says so, and how much it looked at", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "check", "example.com")
		if !strings.Contains(out, "is sound") {
			t.Errorf("output = %q, want it to say the zone is sound", out)
		}
		if !strings.Contains(out, "records checked") {
			t.Errorf("output = %q, want it to say how many records it read", out)
		}
	})

	t.Run("json carries the list a script reads", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "check", "example.com", "--output", "json")

		var got zoneChecked
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		if got.Zone != "example.com." {
			t.Errorf("zone = %q, want the absolute name", got.Zone)
		}
		if got.Findings == nil {
			t.Error("findings is null, want an empty list a script can range over")
		}
		if got.Records == 0 {
			t.Error("records = 0, want what the check read")
		}
		if got.Errors != 0 || got.Warnings != 0 {
			t.Errorf("a sound zone counted %d errors and %d warnings", got.Errors, got.Warnings)
		}
	})

	t.Run("a zone that is not here says so", func(t *testing.T) {
		code, _, errOut := run(t, srv, "zone", "check", "nosuch.example.")
		if code == ExitOK {
			t.Error("checking a zone that does not exist succeeded")
		}
		if !strings.Contains(errOut, "no zone named") {
			t.Errorf("stderr = %q, want it to name the missing zone", errOut)
		}
	})
}

// Findings are the answer, not a failure, so the exit status stays zero and
// the report is what a caller reads.
func TestZoneCheckReportsFindingsAndStillSucceeds(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	mustRun(t, srv, "zone", "create", "example.com")
	mustRun(t, srv, "record", "add", "example.com", "ns1", "A", "192.0.2.53")
	mustRun(t, srv, "record", "add", "example.com", "sub", "NS", "ns1.other.example.")

	// Past the write path, which refuses exactly this, so that the check has
	// something to find. It is the state an older build or a hand on the
	// database file leaves behind.
	occlude(t, srv, "example.com.", "buried.sub.example.com.")

	code, out, errOut := run(t, srv, "zone", "check", "example.com")
	if code != ExitOK {
		t.Fatalf("exit code %d, want %d; stderr: %s", code, ExitOK, errOut)
	}
	if !strings.Contains(out, "1 error") {
		t.Errorf("output = %q, want it to count the error", out)
	}
	if !strings.Contains(out, "delegation") || !strings.Contains(out, "buried.sub.example.com.") {
		t.Errorf("output = %q, want the scope and the name", out)
	}
	if !strings.Contains(out, "only A and AAAA glue") {
		t.Errorf("output = %q, want the sentence the write path refuses with", out)
	}
}

// occlude writes a TXT record straight into the database at a name the write
// path would refuse it at, which is how a test reaches state that only an
// older build or a hand on the file could leave behind.
func occlude(t *testing.T, srv server, apex, name string) {
	t.Helper()
	storeRecord(t, srv, apex, name, zone.TypeTXT, `"never answered"`)
}

// storeRecord writes a record straight into the database, past the API and so
// past everything the write path does about it.
func storeRecord(t *testing.T, srv server, apex, name string, typ zone.RRType, data string) {
	t.Helper()

	var zid zone.ZoneID
	if verr := srv.store.View(t.Context(), func(r store.Reader) error {
		z, err := r.ZoneByName(t.Context(), zone.MustParseName(apex))
		if err != nil {
			return err
		}
		zid = z.ID
		return nil
	}); verr != nil {
		t.Fatalf("find the zone: %v", verr)
	}

	rec, err := zone.NewRecord(zid, zone.MustParseName(name), zone.ClassIN, typ, 300, data)
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	rec.ID = zone.RecordID(id.New())

	if uerr := srv.store.Update(t.Context(), func(tx store.Tx) error {
		return tx.InsertRecord(t.Context(), &rec)
	}); uerr != nil {
		t.Fatalf("insert behind the API's back: %v", uerr)
	}
}

// The summary line is the one place a warning and an error have to read
// differently, and no server produces a warning yet.
func TestCheckSummaryCountsBySeverity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		errors   int
		warnings int
		want     string
	}{
		{"only errors", 2, 0, "2 errors in"},
		{"one error", 1, 0, "1 error in"},
		{"one warning", 0, 1, "1 warning in"},
		{"both", 1, 2, "1 error and 2 warnings in"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report := zoneChecked{Zone: "example.com.", Records: 9,
				Errors: tc.errors, Warnings: tc.warnings}
			for range tc.errors + tc.warnings {
				report.Findings = append(report.Findings, zoneFinding{})
			}

			var buf strings.Builder
			if err := writeCheck(&buf, report); err != nil {
				t.Fatalf("writeCheck: %v", err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("output = %q, want it to contain %q", buf.String(), tc.want)
			}
		})
	}
}

// A reverse zone created after the addresses it should answer for has nothing
// to react to, so it starts empty. The check says so and reconcile fixes it.
func TestZoneCheckReverseAndReconcile(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	mustRun(t, srv, "zone", "create", "example.com")
	mustRun(t, srv, "record", "add", "example.com", "ns1", "A", "192.0.2.53")
	mustRun(t, srv, "zone", "create", "10.0.0.0/24")

	// Past the API, because everything that goes through it generates the
	// entry as it is written. What is left for the check is what the write
	// path never saw.
	storeRecord(t, srv, "example.com.", "www.example.com.", zone.TypeA, "10.0.0.1")

	t.Run("without the flag the reverse is not looked at", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "check", "0.0.10.in-addr.arpa.", "--output", "json")
		var got zoneChecked
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		for _, f := range got.Findings {
			if f.Scope == string(gen.FindingScopeReverse) {
				t.Errorf("a reverse finding without --reverse: %+v", f)
			}
		}
	})

	t.Run("with the flag it says what is missing", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "check", "0.0.10.in-addr.arpa.", "--reverse", "--output", "json")

		var got zoneChecked
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		if got.Warnings == 0 {
			t.Fatalf("nothing was reported for a reverse zone that is missing a PTR: %+v", got)
		}
		var reverse int
		for _, f := range got.Findings {
			if f.Scope == string(gen.FindingScopeReverse) {
				reverse++
				if f.Severity != "warning" {
					t.Errorf("a missing PTR is %q, want a warning", f.Severity)
				}
			}
		}
		if reverse != 1 {
			t.Errorf("got %d reverse findings, want 1: %+v", reverse, got.Findings)
		}
	})

	t.Run("reconcile writes them, and the check goes quiet", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "reconcile", "0.0.10.in-addr.arpa.")
		if !strings.Contains(out, "1 entry written") {
			t.Errorf("output = %q, want it to say what it wrote", out)
		}

		out = mustRun(t, srv, "zone", "check", "0.0.10.in-addr.arpa.", "--reverse", "--output", "json")
		var got zoneChecked
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		for _, f := range got.Findings {
			if f.Scope == string(gen.FindingScopeReverse) {
				t.Errorf("still missing after reconciling: %+v", f)
			}
		}

		// And again changes nothing.
		if again := mustRun(t, srv, "zone", "reconcile", "0.0.10.in-addr.arpa."); !strings.Contains(again, "needed nothing") {
			t.Errorf("a second reconcile said %q, want it to say the zone needed nothing", again)
		}
	})
}
