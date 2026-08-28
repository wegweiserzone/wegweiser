package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// checkZone asks what is wrong with a zone.
func (h *harness) checkZone(zid string) gen.ZoneCheck {
	h.t.Helper()

	var out gen.ZoneCheck
	h.decode(h.do(http.MethodGet, "/zones/"+zid+"/check", nil), http.StatusOK, &out)
	return out
}

func TestCheckZoneOnASoundZone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	z := h.createZone("example.com.")
	// A new zone names ns1 as its own name server and has no address for it,
	// which is a lame delegation until somebody adds one.
	h.createRecord(z.Id, gen.CreateRecord{Name: "ns1.example.com.", Type: "A", Data: "192.0.2.53"})
	h.createRecord(z.Id, gen.CreateRecord{Name: "www.example.com.", Type: "A", Data: "192.0.2.1"})

	got := h.checkZone(z.Id)

	if len(got.Findings) != 0 {
		t.Errorf("a zone built through the API has findings: %+v", got.Findings)
	}
	if got.Truncated {
		t.Error("the report says it was truncated")
	}
	if got.Records == 0 {
		t.Error("the check read no records")
	}
}

// The case the check exists for: state the write path never saw. Written
// straight into the database, because that is now the only way to reach it.
func TestCheckZoneFindsWhatTheWritePathNeverSaw(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	z := h.createZone("example.com.")
	h.createRecord(z.Id, gen.CreateRecord{Name: "ns1.example.com.", Type: "A", Data: "192.0.2.53"})
	h.createRecord(z.Id, gen.CreateRecord{
		Name: "sub.example.com.", Type: "NS", Data: "ns1.other.example.",
	})

	occluded, err := zone.NewRecord(zone.ZoneID(z.Id),
		zone.MustParseName("host.sub.example.com."), zone.ClassIN, zone.TypeTXT, 300, `"hello"`)
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	occluded.ID = zone.RecordID(id.New())
	if uerr := h.store.Update(t.Context(), func(tx store.Tx) error {
		return tx.InsertRecord(t.Context(), &occluded)
	}); uerr != nil {
		t.Fatalf("insert the record behind the API's back: %v", uerr)
	}

	got := h.checkZone(z.Id)

	if len(got.Findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got.Findings), got.Findings)
	}
	f := got.Findings[0]
	if f.Scope != gen.FindingScopeDelegation {
		t.Errorf("scope is %q, want %q", f.Scope, gen.FindingScopeDelegation)
	}
	if f.Name != "host.sub.example.com." {
		t.Errorf("the finding is about %q, want the occluded name", f.Name)
	}
	if !strings.Contains(f.Detail, "only A and AAAA glue") {
		t.Errorf("detail is %q, want it to say what may remain below a delegation", f.Detail)
	}
}

func TestCheckZoneNeedsAZoneThatExists(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	resp := h.do(http.MethodGet, "/zones/01ARZ3NDEKTSV4RRFFQ69G5FAV/check", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	_ = resp.Body.Close()
}

// A zone nobody has finished setting up says so without being asked twice: its
// own name server has no address here, so a resolver sent to it is told the
// name does not exist (RFC 1912 §2.8).
func TestCheckZoneWarnsAboutAZoneNobodyFinished(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	z := h.createZone("example.com.")

	got := h.checkZone(z.Id)

	if len(got.Findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got.Findings), got.Findings)
	}
	f := got.Findings[0]
	if f.Severity != gen.Warning {
		t.Errorf("severity is %q, want a warning: this is correct DNS, just unfinished", f.Severity)
	}
	if f.Scope != gen.FindingScopeNameserver {
		t.Errorf("scope is %q, want %q", f.Scope, gen.FindingScopeNameserver)
	}
	if !strings.Contains(f.Detail, "ns1.example.com.") {
		t.Errorf("detail is %q, want it to name the server with no address", f.Detail)
	}
}
