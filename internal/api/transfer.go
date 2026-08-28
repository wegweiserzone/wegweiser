package api

import (
	"bytes"
	"context"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
	"github.com/wegweiserzone/wegweiser/internal/zonefile"
)

// ImportZone brings a whole zone in from a file in the format of RFC 1035 §5.
func (s *Server) ImportZone(
	ctx context.Context, req gen.ImportZoneRequestObject,
) (gen.ImportZoneResponseObject, error) {
	var opts zonefile.Options
	if req.Params.Origin != nil {
		origin, err := parseName("the origin", *req.Params.Origin)
		if err != nil {
			return nil, err
		}
		opts.Origin = origin
	}

	content, err := zonefile.Parse(req.Body, opts)
	if err != nil {
		return nil, err
	}

	res, err := s.applier.Import(ctx, apply.Import{
		Name:    content.Origin,
		SOA:     content.SOA,
		Records: content.Records,
	}, s.meta(ctx, "import "+content.Origin.String()))
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)

	z, err := s.zoneNamed(ctx, content.Origin)
	if err != nil {
		return nil, err
	}
	return gen.ImportZone201JSONResponse{
		Body: gen.ZoneImported{
			Zone:         zoneToAPI(z),
			Records:      len(content.Records) - len(res.Skipped),
			Skipped:      skippedToAPI(res.Skipped),
			Conflicts:    conflictsToAPI(res.Conflicts),
			MissingZones: missingZonesToAPI(res.MissingZones),
		},
		Headers: gen.ImportZone201ResponseHeaders{
			Location: ptr(basePath + "/zones/" + string(z.ID)),
		},
	}, nil
}

// ExportZone writes a zone out in the format every other authoritative server
// reads.
func (s *Server) ExportZone(
	ctx context.Context, req gen.ExportZoneRequestObject,
) (gen.ExportZoneResponseObject, error) {
	z, err := s.zoneByID(ctx, zone.ZoneID(req.ZoneId))
	if err != nil {
		return nil, err
	}

	records, err := s.allRecords(ctx, z.ID)
	if err != nil {
		return nil, err
	}

	// Assembled whole rather than streamed. A zone is bounded by what this
	// server holds in memory to answer from anyway (docs/decisions/ D12), so
	// buffering it costs nothing that is not already spent, and it means a
	// failure halfway through becomes a problem document rather than a
	// truncated file that looks like a complete one.
	var buf bytes.Buffer
	if werr := zonefile.Write(&buf, &zonefile.Content{
		Origin: z.Name, SOA: z.SOA, Records: records,
	}); werr != nil {
		return nil, werr
	}

	return gen.ExportZone200TextdnsResponse{
		Body:          &buf,
		ContentLength: int64(buf.Len()),
	}, nil
}

// allRecords reads a whole zone, following the cursor to the end.
func (s *Server) allRecords(ctx context.Context, zid zone.ZoneID) ([]zone.Record, error) {
	var out []zone.Record
	err := s.store.View(ctx, func(r store.Reader) error {
		out = out[:0]
		for rec, ierr := range r.IterZoneRecords(ctx, zid) {
			if ierr != nil {
				return ierr
			}
			out = append(out, *rec)
		}
		return nil
	})
	return out, err
}

// zoneNamed reads a zone by its apex.
func (s *Server) zoneNamed(ctx context.Context, name zone.Name) (*zone.Zone, error) {
	var z *zone.Zone
	err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		z, verr = r.ZoneByName(ctx, name)
		return verr
	})
	return z, err
}

// skippedToAPI renders what an import left out.
func skippedToAPI(ss []apply.Skipped) *[]gen.SkippedRecord {
	if len(ss) == 0 {
		return nil
	}
	out := make([]gen.SkippedRecord, 0, len(ss))
	for i := range ss {
		out = append(out, gen.SkippedRecord{
			Record: ss[i].Record.String(),
			Reason: ss[i].Reason,
		})
	}
	return &out
}

// importPath is the one route that takes a file rather than a request.
var importPath = fmt.Sprintf("%s/zones/import", basePath)
