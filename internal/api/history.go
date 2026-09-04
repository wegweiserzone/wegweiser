package api

import (
	"context"
	"math"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// ListCommits returns one page of history, newest first.
func (s *Server) ListCommits(
	ctx context.Context, req gen.ListCommitsRequestObject,
) (gen.ListCommitsResponseObject, error) {
	f := store.CommitFilter{
		Paging: store.Paging{
			Cursor: store.Cursor(deref(req.Params.Cursor, "")),
			Limit:  deref(req.Params.Limit, 0),
		},
		ZoneID: zone.ZoneID(deref(req.Params.ZoneId, "")),
		Actor:  deref(req.Params.Actor, ""),
		Since:  deref(req.Params.Since, time0),
		Until:  deref(req.Params.Until, time0),
	}
	if req.Params.Kind != nil {
		f.Kinds = make([]journal.Kind, 0, len(*req.Params.Kind))
		for _, k := range *req.Params.Kind {
			f.Kinds = append(f.Kinds, journal.Kind(k))
		}
	}
	if req.Params.Source != nil {
		f.Sources = make([]journal.Source, 0, len(*req.Params.Source))
		for _, src := range *req.Params.Source {
			f.Sources = append(f.Sources, journal.Source(src))
		}
	}

	var page store.Page[*journal.Commit]
	if err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		page, verr = r.ListCommits(ctx, f)
		return verr
	}); err != nil {
		return nil, err
	}

	items := make([]gen.Commit, 0, len(page.Items))
	for _, c := range page.Items {
		items = append(items, commitToAPI(c))
	}
	out := gen.ListCommits200JSONResponse{Items: items}
	if page.NextCursor != "" {
		out.NextCursor = ptr(string(page.NextCursor))
	}
	return out, nil
}

// GetCommit returns one commit with everything it changed.
func (s *Server) GetCommit(
	ctx context.Context, req gen.GetCommitRequestObject,
) (gen.GetCommitResponseObject, error) {
	var c *journal.Commit
	if err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		c, verr = r.CommitByID(ctx, journal.CommitID(req.CommitId))
		return verr
	}); err != nil {
		return nil, err
	}
	return gen.GetCommit200JSONResponse(commitToAPI(c)), nil
}

// RollbackZone restores a zone to the state it had at a serial.
func (s *Server) RollbackZone(
	ctx context.Context, req gen.RollbackZoneRequestObject,
) (gen.RollbackZoneResponseObject, error) {
	target, err := serialFrom(req.Body.Serial)
	if err != nil {
		return nil, err
	}

	res, err := s.applier.Rollback(ctx, zone.ZoneID(req.ZoneId), target,
		s.meta(ctx, deref(req.Body.Comment, "roll back to serial "+target.String())))
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)

	out := gen.RollbackResult{
		Conflicts:    conflictsToAPI(res.Conflicts),
		MissingZones: missingZonesToAPI(res.MissingZones),
	}
	if c := res.Commit(); c != nil {
		out.Commit = ptr(commitToAPI(c))
	}
	return gen.RollbackZone200JSONResponse(out), nil
}

// serialFrom reads a serial a client sent. It is a 32-bit number that wraps
// (RFC 1982), so anything outside that range is a mistake rather than a serial
// this server has ever written.
func serialFrom(v int64) (zone.Serial, error) {
	if v < 0 || v > math.MaxUint32 {
		return zone.Serial{}, badRequest(
			"%d is not a serial; the range is 0 to %d (RFC 1982)", v, uint32(math.MaxUint32))
	}
	return zone.NewSerial(uint32(v)), nil
}
