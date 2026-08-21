package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// table renders aligned columns.
type table struct {
	w       *tabwriter.Writer
	columns int
}

// newTable starts a table with the given column titles.
func newTable(w io.Writer, titles ...string) *table {
	t := newRows(w, len(titles))
	// The titles are not coloured. They are a row like any other as far as the
	// column widths are concerned, so an escape sequence in one would widen
	// its column by octets that take no space and push every value below it
	// out of line.
	t.row(titles...)
	return t
}

// newRows starts a table with no titles, for a block of name-and-value pairs
// where a header would name nothing.
func newRows(w io.Writer, columns int) *table {
	return &table{
		// Two spaces between columns, which is enough to read as a gap and
		// narrow enough that a long zone name does not push the rest off the
		// terminal.
		w:       tabwriter.NewWriter(w, 0, 0, 2, ' ', 0),
		columns: columns,
	}
}

// row writes one line. A cell count other than the header's is a bug in the
// caller, and one that would silently produce a table with a column missing.
func (t *table) row(cells ...string) {
	if len(cells) != t.columns {
		// A programming error, and one whose other outcome is a table with a
		// column quietly missing. Any test that runs the command finds it.
		panic(fmt.Sprintf("table: %d cells for %d columns", len(cells), t.columns))
	}
	// The write error is sticky and comes back from Flush, so there is one
	// place that reports it rather than one per row.
	fmt.Fprintln(t.w, strings.Join(cells, "\t"))
}

// flush writes the table out, now that every column's width is known.
func (t *table) flush() error {
	if err := t.w.Flush(); err != nil {
		return fmt.Errorf("write the table: %w", err)
	}
	return nil
}
