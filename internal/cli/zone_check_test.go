package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestZoneCheck(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	mustRun(t, srv, "zone", "create", "example.com")
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
	mustRun(t, srv, "record", "add", "example.com", "sub", "NS", "ns1.other.example.")

	// Past the write path, which refuses exactly this, so that the check has
	// something to find. It is the state an older build or a hand on the
	// database file leaves behind.
	occlude(t, srv, "example.com.", "buried.sub.example.com.")

	code, out, errOut := run(t, srv, "zone", "check", "example.com")
	if code != ExitOK {
		t.Fatalf("exit code %d, want %d; stderr: %s", code, ExitOK, errOut)
	}
	if !strings.Contains(out, "1 finding") {
		t.Errorf("output = %q, want it to count the finding", out)
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

	rec, err := zone.NewRecord(zid, zone.MustParseName(name),
		zone.ClassIN, zone.TypeTXT, 300, `"never answered"`)
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
