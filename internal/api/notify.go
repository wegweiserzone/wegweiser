package api

import (
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// tellSecondaries starts a notification for each zone the write touched.
//
// This node accepted the write, which makes it the one to tell anybody
// (docs/decisions/d41-what-follows-applying-a-batch.md). By now the version it
// announces is the one being served: the applier hands each batch to whatever
// copies the store into the query path before it returns. That is the order
// RFC 1996 §4.2 asks for, so that a secondary coming straight back is answered
// with the serial it was told about.
func (s *Server) tellSecondaries(res *apply.Result) {
	if s.notifier == nil || s.snapshots == nil || !res.Changed() {
		return
	}
	snap := s.snapshots.Snapshot()
	if snap == nil {
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
