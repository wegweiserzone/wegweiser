package cluster

import (
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/id"
)

func TestAnIdentifierIsMintedOnceAndKept(t *testing.T) {
	t.Parallel()
	st := newStore(t)

	first, err := Identity(t.Context(), st, "")
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if !id.Valid(first) {
		t.Errorf("the minted identifier %q is not one", first)
	}
	again, err := Identity(t.Context(), st, "")
	if err != nil {
		t.Fatalf("Identity a second time: %v", err)
	}
	if again != first {
		t.Errorf("the second start is member %q, want the %q minted on the first", again, first)
	}
}

func TestAConfiguredIdentifierIsTakenAndThenHeldTo(t *testing.T) {
	t.Parallel()
	st := newStore(t)

	if got, err := Identity(t.Context(), st, "ns1"); err != nil || got != "ns1" {
		t.Fatalf("Identity with ns1 configured = %q, %v; want ns1", got, err)
	}
	if got, err := Identity(t.Context(), st, "ns1"); err != nil || got != "ns1" {
		t.Errorf("the same file a second time = %q, %v; want ns1 again", got, err)
	}
	if got, err := Identity(t.Context(), st, ""); err != nil || got != "ns1" {
		t.Errorf("a file that names nothing = %q, %v; want the ns1 already held", got, err)
	}

	_, err := Identity(t.Context(), st, "ns2")
	if err == nil || !strings.Contains(err.Error(), `is member "ns1"`) {
		t.Errorf("a file naming ns2 = %v, want a refusal saying the node is ns1", err)
	}
}
