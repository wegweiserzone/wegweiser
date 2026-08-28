package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
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

// The case the check exists for. Each write is checked against the names it
// touches, and neither of these two touches the record the pair occludes.
func TestCheckZoneFindsWhatNoSingleWriteCould(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	z := h.createZone("example.com.")

	h.createRecord(z.Id, gen.CreateRecord{
		Name: "host.sub.example.com.", Type: "TXT", Data: `"hello"`,
	})
	h.createRecord(z.Id, gen.CreateRecord{
		Name: "sub.example.com.", Type: "NS", Data: "ns1.other.example.",
	})

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
