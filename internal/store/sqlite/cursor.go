package sqlite

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wegweiserzone/wegweiser/internal/store"
)

// A cursor carries the ordering key of the last row of a page, so the next page
// resumes with a comparison against an index rather than by counting rows and
// throwing them away. Offset paging over a table being written to skips and
// duplicates rows, which a virtualized table shows the moment two people edit
// at once.
//
// The encoding is opaque on purpose: it is this package's private business, and
// a caller that took it apart would tie itself to the current ordering.
type cursor struct {
	// Kind names the listing the cursor came from, so that a record cursor
	// handed to the zone listing is refused rather than silently misread as a
	// position in a different order.
	Kind string `json:"k"`
	// Sort is the canonical name key, for the listings ordered by name.
	Sort []byte `json:"s,omitempty"`
	// Type is the record type, which orders records sharing an owner name.
	Type uint16 `json:"t,omitempty"`
	// Millis is the creation time, for the listings ordered by time.
	Millis int64 `json:"m,omitempty"`
	// ID breaks any remaining tie, because it is unique.
	ID string `json:"i"`
}

// Cursor kinds. They exist to keep one listing's position from being read as
// another's.
const (
	cursorZones   = "z"
	cursorRecords = "r"
	cursorCommits = "c"
)

// encode renders the cursor for a caller to hand back.
func (c cursor) encode() store.Cursor {
	raw, err := json.Marshal(c)
	if err != nil {
		// The struct holds only strings, integers and a byte slice, none of
		// which json can refuse.
		panic("sqlite: encoding a cursor: " + err.Error())
	}
	return store.Cursor(base64.RawURLEncoding.EncodeToString(raw))
}

// decodeCursor reads a cursor produced by [cursor.encode]. The empty cursor
// means the start of the listing and decodes to the zero value.
func decodeCursor(c store.Cursor, kind string) (cursor, error) {
	if c == "" {
		return cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(string(c))
	if err != nil {
		return cursor{}, invalidCursor(c)
	}
	var out cursor
	// Reject a cursor carrying fields this listing does not use, rather than
	// resuming from a position that happens to decode.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil || out.Kind != kind || out.ID == "" {
		return cursor{}, invalidCursor(c)
	}
	return out, nil
}

func invalidCursor(c store.Cursor) error {
	return fmt.Errorf(
		"sqlite: %q is not a cursor from this listing; pass back the cursor the previous page "+
			"returned, or none at all to start over", string(c))
}

// upperBound returns the smallest byte string greater than every string having
// key as a prefix, so that "this name and everything below it" is one indexed
// range rather than a scan with a LIKE.
func upperBound(key []byte) ([]byte, bool) {
	out := make([]byte, len(key))
	copy(out, key)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xFF {
			out[i]++
			return out[:i+1], true
		}
	}
	// Every octet was 0xFF, so nothing sorts above it. The empty key lands here
	// too, which is right: the root has no upper bound because everything is
	// below it.
	return nil, false
}
