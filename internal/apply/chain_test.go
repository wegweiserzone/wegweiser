package apply

import (
	"slices"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// ends indexes commits by the serial each produced, the way a range is built up
// as pages of the listing arrive.
func ends(pairs [][2]uint32) map[zone.Serial]*journal.Commit {
	out := make(map[zone.Serial]*journal.Commit, len(pairs))
	for _, p := range pairs {
		c := &journal.Commit{SerialFrom: zone.NewSerial(p[0]), SerialTo: zone.NewSerial(p[1])}
		out[c.SerialTo] = c
	}
	return out
}

// The listing a range is built from orders by the millisecond a commit was
// recorded and then by identifier, so two commits inside one millisecond arrive
// in no order at all. What places them is the chain of serials (D2), and this
// is the walk along it.
func TestChainBack(t *testing.T) {
	t.Parallel()

	// Six steps, one of them past the end of every range asked for below.
	all := [][2]uint32{{1, 2}, {2, 3}, {3, 4}, {4, 5}, {5, 6}, {6, 7}}

	tests := []struct {
		name     string
		have     [][2]uint32
		from, to uint32
		// want is the serial each commit of the chain ends at, and nil is a
		// range the walk cannot cover.
		want []uint32
	}{
		{"the whole range", all, 1, 6, []uint32{2, 3, 4, 5, 6}},
		{"it stops where the caller said, not where the journal ends", all, 1, 3, []uint32{2, 3}},
		{"a single step", all, 5, 6, []uint32{6}},
		{"a range with nothing in it", all, 4, 4, []uint32{}},
		{"a step is missing", [][2]uint32{{1, 2}, {3, 4}}, 1, 4, nil},
		{"nothing has been read yet", nil, 1, 6, nil},
		{"a serial that has come round to itself", [][2]uint32{{4, 4}}, 1, 4, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := chainBack(ends(tc.have), zone.NewSerial(tc.from), zone.NewSerial(tc.to))
			if ok != (tc.want != nil) {
				t.Fatalf("covered = %v, want %v", ok, tc.want != nil)
			}
			if !ok {
				return
			}
			reached := make([]uint32, len(got))
			for i, c := range got {
				reached[i] = c.SerialTo.Uint32()
			}
			if !slices.Equal(reached, tc.want) {
				t.Errorf("the chain ends at %v, want %v", reached, tc.want)
			}
		})
	}
}
