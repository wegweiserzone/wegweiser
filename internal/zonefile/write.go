package zonefile

import (
	"bufio"
	"fmt"
	"io"
	"slices"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Write renders a zone as an RFC 1035 §5 file.
//
// The SOA comes first, as RFC 1035 §5.2 requires of the zone's first record,
// with the apex records after it and everything else in the canonical order of
// RFC 4034 §6.1: the same order the database lists them in, so exporting the
// same zone twice produces the same bytes.
func Write(w io.Writer, c *Content) error {
	if c == nil {
		return fmt.Errorf("%w: nothing to write", zone.ErrInvalid)
	}
	if c.Origin.IsZero() {
		return fmt.Errorf("%w: a zonefile names the zone it describes", zone.ErrInvalid)
	}
	if err := c.SOA.Validate(); err != nil {
		return fmt.Errorf("the start of authority of %q: %w", c.Origin, err)
	}

	// A bufio.Writer keeps the first error it meets and refuses every write
	// after it, so Flush at the end reports whatever went wrong. Checking each
	// write as well would be the same answer four times over.
	bw := bufio.NewWriter(w)

	fmt.Fprintf(bw, "$ORIGIN %s\n", c.Origin)
	fmt.Fprintf(bw, "$TTL %s\n\n", c.SOA.TTL)

	// Broken over lines in parentheses with the field names in comments. It is
	// the one record where the numbers are otherwise unreadable, and it is the
	// record somebody is most likely to have to edit by hand.
	fmt.Fprintf(bw, "%s\t%s\tIN\tSOA\t%s %s (\n",
		c.Origin, c.SOA.TTL, c.SOA.NS, c.SOA.Mbox)
	// The serial is printed from its own type rather than borrowed into a TTL:
	// a serial runs to 2^32-1 and a TTL stops at 2^31-1 (RFC 2181 §8), so the
	// two are only interchangeable until somebody's zone is old enough.
	fmt.Fprintf(bw, "\t\t\t%-12s ; serial\n", c.SOA.Serial)
	for _, f := range []struct {
		value zone.TTL
		what  string
	}{
		{c.SOA.Refresh, "refresh"},
		{c.SOA.Retry, "retry"},
		{c.SOA.Expire, "expire"},
		{c.SOA.Minimum, "negative caching"},
	} {
		fmt.Fprintf(bw, "\t\t\t%-12s ; %s\n", f.value, f.what)
	}
	fmt.Fprint(bw, "\t\t\t)\n\n")

	sorted := slices.Clone(c.Records)
	slices.SortStableFunc(sorted, compareRecords)

	for i := range sorted {
		r := &sorted[i]
		// A disabled record is not part of the zone as it answers, and a file
		// has nowhere to say "present but switched off". Writing it would hand
		// another server records this one does not serve.
		if r.Disabled {
			continue
		}
		fmt.Fprintln(bw, r.String())
	}
	return bw.Flush()
}

// compareRecords puts records in the canonical order of RFC 4034 §6.1: by
// owner name, then by type, then by data.
func compareRecords(a, b zone.Record) int {
	if c := a.Name.Compare(b.Name); c != 0 {
		return c
	}
	if a.Type != b.Type {
		return int(a.Type) - int(b.Type)
	}
	if a.RData.String() < b.RData.String() {
		return -1
	}
	if a.RData.String() > b.RData.String() {
		return 1
	}
	return 0
}
