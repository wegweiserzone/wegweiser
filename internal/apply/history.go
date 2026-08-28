package apply

import (
	"context"
	"errors"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Since returns the commits that took a zone from one serial to another, oldest
// first, so that an incremental transfer can replay them (RFC 1995 §4).
//
// The upper end is the caller's rather than the newest the journal holds. A
// transfer is served from the snapshot, which lags the database for as long as
// a write takes to publish, and a commit past it has not been answered from
// yet.
//
// It reports false where the range is not covered, which is the ordinary answer
// to a client that has been away longer than the retention of docs/decisions/
// D8, or that names a serial the zone never had.
func (a *Applier) Since(
	ctx context.Context, apex zone.Name, from, to zone.Serial,
) ([]*journal.Commit, bool, error) {
	var out []*journal.Commit
	err := a.store.View(ctx, func(r store.Reader) error {
		z, zerr := r.ZoneByName(ctx, apex)
		if zerr != nil {
			return zerr
		}
		var cerr error
		out, cerr = commitsBetween(ctx, r, z, from, to)
		return cerr
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}
