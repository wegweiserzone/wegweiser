package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// republish rebuilds the zones a write touched and swaps the result into the
// data plane (architecture §3.4).
func (s *Server) republish(ctx context.Context, res *apply.Result) {
	if s.snapshots == nil || !res.Changed() {
		return
	}

	next := s.snapshots.Snapshot()
	err := s.store.View(ctx, func(r store.Reader) error {
		for _, c := range res.Commits {
			var berr error
			if next, berr = republishZone(ctx, next, r, c.ZoneID, c.ZoneName); berr != nil {
				return berr
			}
		}
		return nil
	})
	if err != nil {
		// The database is right and the data plane is now behind it. There is
		// no repair to attempt here: recovery is a full rebuild, which is what
		// starting up does, so the honest thing is to say so loudly.
		s.report(fmt.Errorf(
			"the change is committed but the query path still answers from the state "+
				"before it; restart to rebuild the snapshot: %w", err))
		return
	}

	s.snapshots.SetSnapshot(next)
	s.tellSecondaries(next, res)
}

// tellSecondaries starts a notification for each zone the write touched.
//
// After the publish and never before. RFC 1996 §4.2 sends a notification once
// the version it announces is being served, and a secondary that came straight
// back would otherwise be handed a serial this server does not yet answer with.
func (s *Server) tellSecondaries(snap *dns.Snapshot, res *apply.Result) {
	if s.notifier == nil {
		return
	}
	told := make(map[zone.Name]struct{}, len(res.Commits))
	for _, c := range res.Commits {
		if _, done := told[c.ZoneName]; done {
			continue
		}
		told[c.ZoneName] = struct{}{}
		s.notifier.Notify(snap, c.ZoneName)
	}
}

// republishZone returns the snapshot with one zone brought up to date. A zone
// that is no longer in the store was deleted, and is dropped by the name the
// commit carries, which is why a commit carries one at all.
func republishZone(
	ctx context.Context, snap *dns.Snapshot, r store.Reader,
	zid zone.ZoneID, name zone.Name,
) (*dns.Snapshot, error) {
	z, err := r.ZoneByID(ctx, zid)
	if errors.Is(err, store.ErrNotFound) {
		return snap.WithoutZone(name), nil
	}
	if err != nil {
		return nil, err
	}
	return snap.WithZone(ctx, z, r)
}
