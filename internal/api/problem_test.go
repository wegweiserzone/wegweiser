package api

import (
	"fmt"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// A problem's detail is the sentence about the request. The kind of error is
// the problem's type and title already, and repeating it in front of the
// sentence reads as "not found: store: not found: ...".
func TestProblemDetailLeavesOutTheKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: no commit with the identifier 01XYZ", store.ErrNotFound),
			"no commit with the identifier 01XYZ"},
		{fmt.Errorf("%w: the zone example.com. is at serial 15", zone.ErrInvalid),
			"the zone example.com. is at serial 15"},
		{fmt.Errorf("%w: %s", zone.ErrInvalidRData, `"1.2.3" is not an IPv4 address`),
			`"1.2.3" is not an IPv4 address`},
		{fmt.Errorf("%w: a zone named example.com. exists", store.ErrConflict),
			"a zone named example.com. exists"},
	}
	for _, tt := range tests {
		if got := asProblem(tt.err).detail; got != tt.want {
			t.Errorf("detail of %q = %q, want %q", tt.err, got, tt.want)
		}
	}
}
