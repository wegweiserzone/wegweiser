package api

import (
	"context"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// ListRecords returns one page of a zone's records.
func (s *Server) ListRecords(
	ctx context.Context, req gen.ListRecordsRequestObject,
) (gen.ListRecordsResponseObject, error) {
	f := store.RecordFilter{
		Paging: store.Paging{
			Cursor: store.Cursor(deref(req.Params.Cursor, "")),
			Limit:  deref(req.Params.Limit, 0),
		},
		ZoneID: zone.ZoneID(req.ZoneId),
	}
	if req.Params.Search != nil {
		f.Search = *req.Params.Search
	}
	if req.Params.Name != nil {
		name, err := parseName("the owner name", *req.Params.Name)
		if err != nil {
			return nil, err
		}
		f.Name = name
	}
	if req.Params.Type != nil {
		typ, err := zone.ParseRRType(*req.Params.Type)
		if err != nil {
			return nil, badRequest("%q is not a record type: %v", *req.Params.Type, err)
		}
		f.Types = []zone.RRType{typ}
	}

	var page store.Page[*zone.Record]
	if err := s.store.View(ctx, func(r store.Reader) error {
		// The zone is read first so that listing the records of a zone that is
		// not there is a 404 rather than an empty page, which would say that
		// the zone exists and happens to be empty.
		if _, verr := r.ZoneByID(ctx, zone.ZoneID(req.ZoneId)); verr != nil {
			return verr
		}
		var verr error
		page, verr = r.ListRecords(ctx, f)
		return verr
	}); err != nil {
		return nil, err
	}

	out := gen.ListRecords200JSONResponse{Items: recordsToAPI(page.Items)}
	if page.NextCursor != "" {
		out.NextCursor = ptr(string(page.NextCursor))
	}
	return out, nil
}

// GetRecord returns one record.
func (s *Server) GetRecord(
	ctx context.Context, req gen.GetRecordRequestObject,
) (gen.GetRecordResponseObject, error) {
	rec, err := s.recordByID(ctx, zone.RecordID(req.RecordId))
	if err != nil {
		return nil, err
	}
	return gen.GetRecord200JSONResponse(recordToAPI(rec)), nil
}

// CreateRecord adds a record to a zone.
//
// What adding it caused comes back with it. An address record may generate a
// PTR, may find that the address already answers with another name, or may find
// no reverse zone to put anything in, and the last two are not errors. They are
// decisions for a person, so they travel as data (docs/decisions/ D3 and D6).
func (s *Server) CreateRecord(
	ctx context.Context, req gen.CreateRecordRequestObject,
) (gen.CreateRecordResponseObject, error) {
	z, err := s.zoneByID(ctx, zone.ZoneID(req.ZoneId))
	if err != nil {
		return nil, err
	}

	rec, err := recordFrom(z, *req.Body)
	if err != nil {
		return nil, err
	}

	meta := s.meta(ctx, "")
	res, err := s.applier.Apply(ctx, apply.Command{
		ZoneID:  z.ID,
		Ops:     []apply.RecordOp{{Action: apply.ActionAdd, Record: &rec}},
		Kind:    journal.KindEdit,
		Source:  meta.Source,
		Actor:   meta.Actor,
		Comment: deref(req.Body.Comment, ""),
	})
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)

	written, err := s.recordWritten(ctx, rec.ID, res)
	if err != nil {
		return nil, err
	}
	return gen.CreateRecord201JSONResponse{
		Body: written,
		Headers: gen.CreateRecord201ResponseHeaders{
			Location: ptr(basePath + "/records/" + string(rec.ID)),
		},
	}, nil
}

// UpdateRecord changes a record, keeping its identity.
func (s *Server) UpdateRecord(
	ctx context.Context, req gen.UpdateRecordRequestObject,
) (gen.UpdateRecordResponseObject, error) {
	before, err := s.recordByID(ctx, zone.RecordID(req.RecordId))
	if err != nil {
		return nil, err
	}
	z, err := s.zoneByID(ctx, before.ZoneID)
	if err != nil {
		return nil, err
	}

	after, err := patchRecord(z, before, *req.Body)
	if err != nil {
		return nil, err
	}

	meta := s.meta(ctx, "")
	res, err := s.applier.Apply(ctx, apply.Command{
		ZoneID: z.ID,
		Ops: []apply.RecordOp{{
			Action:   apply.ActionUpdate,
			RecordID: before.ID,
			Record:   &after,
		}},
		Kind:    journal.KindEdit,
		Source:  meta.Source,
		Actor:   meta.Actor,
		Comment: "update record",
	})
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)

	written, err := s.recordWritten(ctx, before.ID, res)
	if err != nil {
		return nil, err
	}
	return gen.UpdateRecord200JSONResponse(written), nil
}

// DetachRecord turns a generated record into an authored one.
//
// This is the way out of "a generated record cannot be edited" (D4). A record
// that is already authored comes back unchanged rather than as an error, which
// makes calling this twice harmless.
func (s *Server) DetachRecord(
	ctx context.Context, req gen.DetachRecordRequestObject,
) (gen.DetachRecordResponseObject, error) {
	rec, err := s.recordByID(ctx, zone.RecordID(req.RecordId))
	if err != nil {
		return nil, err
	}

	meta := s.meta(ctx, "")
	res, err := s.applier.Apply(ctx, apply.Command{
		ZoneID:  rec.ZoneID,
		Ops:     []apply.RecordOp{{Action: apply.ActionDetach, RecordID: rec.ID}},
		Kind:    journal.KindEdit,
		Source:  meta.Source,
		Actor:   meta.Actor,
		Comment: "detach record",
	})
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)

	written, err := s.recordWritten(ctx, rec.ID, res)
	if err != nil {
		return nil, err
	}
	return gen.DetachRecord200JSONResponse(written), nil
}

// DeleteRecord removes a record, and whatever was generated from it.
func (s *Server) DeleteRecord(
	ctx context.Context, req gen.DeleteRecordRequestObject,
) (gen.DeleteRecordResponseObject, error) {
	rec, err := s.recordByID(ctx, zone.RecordID(req.RecordId))
	if err != nil {
		return nil, err
	}

	meta := s.meta(ctx, "")
	res, err := s.applier.Apply(ctx, apply.Command{
		ZoneID:  rec.ZoneID,
		Ops:     []apply.RecordOp{{Action: apply.ActionDelete, RecordID: rec.ID}},
		Kind:    journal.KindEdit,
		Source:  meta.Source,
		Actor:   meta.Actor,
		Comment: "delete record",
	})
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)

	return gen.DeleteRecord204Response{}, nil
}

// recordByID reads one record.
func (s *Server) recordByID(ctx context.Context, rid zone.RecordID) (*zone.Record, error) {
	var rec *zone.Record
	err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		rec, verr = r.RecordByID(ctx, rid)
		return verr
	})
	return rec, err
}

// recordWritten renders a record as it now stands, together with what writing
// it caused.
func (s *Server) recordWritten(
	ctx context.Context, rid zone.RecordID, res *apply.Result,
) (gen.RecordWritten, error) {
	stored, err := s.recordByID(ctx, rid)
	if err != nil {
		return gen.RecordWritten{}, err
	}
	return gen.RecordWritten{
		Record:       recordToAPI(stored),
		Conflicts:    conflictsToAPI(res.Conflicts),
		MissingZones: missingZonesToAPI(res.MissingZones),
		Generated:    s.generatedBy(ctx, rid),
	}, nil
}

// generatedBy returns what the automation wrote because of a record, so that
// the response shows the PTR the client did not ask for but did cause.
func (s *Server) generatedBy(ctx context.Context, rid zone.RecordID) *[]gen.Record {
	var recs []*zone.Record
	if err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		recs, verr = r.ManagedBy(ctx, rid)
		return verr
	}); err != nil || len(recs) == 0 {
		// A response that cannot list what it generated is still a correct
		// response about what it was asked to do; the record is written either
		// way and the listing will show it.
		s.report(err)
		return nil
	}
	return ptr(recordsToAPI(recs))
}

// recordFrom builds a record from what the client sent, filling in what the
// zone decides: the class nothing else answers in, and the zone's own default
// TTL for a record that came without one.
func recordFrom(z *zone.Zone, in gen.CreateRecord) (zone.Record, error) {
	name, err := parseName("the owner name", in.Name)
	if err != nil {
		return zone.Record{}, err
	}
	if !z.Contains(name) {
		return zone.Record{}, outsideZone(z, name)
	}

	typ, err := zone.ParseRRType(in.Type)
	if err != nil {
		return zone.Record{}, badRequest("%q is not a record type: %v", in.Type, err)
	}

	class := zone.ClassIN
	if in.Class != nil {
		class, err = zone.ParseClass(*in.Class)
		if err != nil {
			return zone.Record{}, badRequest("%q is not a class: %v", *in.Class, err)
		}
	}

	ttl := z.DefaultTTL
	if in.Ttl != nil {
		if ttl, err = ttlFrom("the TTL", *in.Ttl); err != nil {
			return zone.Record{}, err
		}
	}

	rec, err := zone.NewRecord(z.ID, name, class, typ, ttl, in.Data)
	if err != nil {
		return zone.Record{}, err
	}
	rec.Comment = deref(in.Comment, "")
	return rec, nil
}

// patchRecord applies what the client sent onto the record as it stands.
func patchRecord(z *zone.Zone, before *zone.Record, in gen.UpdateRecord) (zone.Record, error) {
	name := before.Name
	if in.Name != nil {
		n, err := parseName("the owner name", *in.Name)
		if err != nil {
			return zone.Record{}, err
		}
		if !z.Contains(n) {
			return zone.Record{}, outsideZone(z, n)
		}
		name = n
	}

	typ := before.Type
	if in.Type != nil {
		t, err := zone.ParseRRType(*in.Type)
		if err != nil {
			return zone.Record{}, badRequest("%q is not a record type: %v", *in.Type, err)
		}
		typ = t
	}

	ttl := before.TTL
	if in.Ttl != nil {
		t, err := ttlFrom("the TTL", *in.Ttl)
		if err != nil {
			return zone.Record{}, err
		}
		ttl = t
	}

	after, err := zone.NewRecord(z.ID, name, before.Class, typ, ttl, deref(in.Data, before.RData.String()))
	if err != nil {
		return zone.Record{}, err
	}
	after.ID = before.ID
	after.Comment = deref(in.Comment, before.Comment)
	after.Disabled = deref(in.Disabled, before.Disabled)
	return after, nil
}

// ReplaceRRsets makes the named RRsets exactly what the client sent.
func (s *Server) ReplaceRRsets(
	ctx context.Context, req gen.ReplaceRRsetsRequestObject,
) (gen.ReplaceRRsetsResponseObject, error) {
	z, err := s.zoneByID(ctx, zone.ZoneID(req.ZoneId))
	if err != nil {
		return nil, err
	}
	if len(req.Body.Rrsets) == 0 {
		return nil, badRequest("no RRsets were given, so there is nothing to replace")
	}

	ops := make([]apply.RecordOp, 0, len(req.Body.Rrsets))
	keys := make([]zone.RRsetKey, 0, len(req.Body.Rrsets))
	for i := range req.Body.Rrsets {
		op, oerr := rrsetOp(z, req.Body.Rrsets[i])
		if oerr != nil {
			return nil, fmt.Errorf("rrsets[%d]: %w", i, oerr)
		}
		ops = append(ops, op)
		keys = append(keys, op.Key)
	}

	meta := s.meta(ctx, "")
	res, err := s.applier.Apply(ctx, apply.Command{
		ZoneID:  z.ID,
		Ops:     ops,
		Kind:    journal.KindEdit,
		Source:  meta.Source,
		Actor:   meta.Actor,
		Comment: deref(req.Body.Comment, "replace RRsets"),
	})
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)

	records, err := s.readRRsets(ctx, z.ID, keys)
	if err != nil {
		return nil, err
	}
	return gen.ReplaceRRsets200JSONResponse{
		Records:      recordsToAPI(records),
		Generated:    s.generatedByAll(ctx, records),
		Conflicts:    conflictsToAPI(res.Conflicts),
		MissingZones: missingZonesToAPI(res.MissingZones),
	}, nil
}

// rrsetOp turns one submitted RRset into the operation that makes it so.
//
// The TTL is read once for the set rather than once per member, because RFC
// 2181 §5.2 requires the members to share one and a field that can be set
// inconsistently is one that will be.
func rrsetOp(z *zone.Zone, in gen.RRset) (apply.RecordOp, error) {
	name, err := parseName("the owner name", in.Name)
	if err != nil {
		return apply.RecordOp{}, err
	}
	if !z.Contains(name) {
		return apply.RecordOp{}, outsideZone(z, name)
	}

	typ, err := zone.ParseRRType(in.Type)
	if err != nil {
		return apply.RecordOp{}, badRequest("%q is not a record type: %v", in.Type, err)
	}

	class := zone.ClassIN
	if in.Class != nil {
		if class, err = zone.ParseClass(*in.Class); err != nil {
			return apply.RecordOp{}, badRequest("%q is not a class: %v", *in.Class, err)
		}
	}

	ttl := z.DefaultTTL
	if in.Ttl != nil {
		if ttl, err = ttlFrom("the TTL", *in.Ttl); err != nil {
			return apply.RecordOp{}, err
		}
	}

	recs := make([]zone.Record, 0, len(in.Records))
	for i, m := range in.Records {
		rec, rerr := zone.NewRecord(z.ID, name, class, typ, ttl, m.Data)
		if rerr != nil {
			return apply.RecordOp{}, fmt.Errorf("records[%d]: %w", i, rerr)
		}
		rec.Comment = deref(m.Comment, "")
		rec.Disabled = deref(m.Disabled, false)
		recs = append(recs, rec)
	}

	return apply.RecordOp{
		Action:  apply.ActionReplaceRRset,
		Key:     zone.RRsetKey{Name: name, Class: class, Type: typ},
		Records: recs,
	}, nil
}

// readRRsets reads the named sets back, in the order they were given.
func (s *Server) readRRsets(
	ctx context.Context, zid zone.ZoneID, keys []zone.RRsetKey,
) ([]*zone.Record, error) {
	var out []*zone.Record
	err := s.store.View(ctx, func(r store.Reader) error {
		out = out[:0]
		for _, k := range keys {
			// Followed to the end rather than read as one page: an RRset is
			// not bounded by the page size, and answering with the first
			// hundred members of a set that has more would be reporting a set
			// the zone does not hold.
			f := store.RecordFilter{
				ZoneID: zid,
				Name:   k.Name,
				Types:  []zone.RRType{k.Type},
				Paging: store.Paging{Limit: store.MaxLimit},
			}
			for {
				page, perr := r.ListRecords(ctx, f)
				if perr != nil {
					return perr
				}
				for _, rec := range page.Items {
					// The filter selects by name and type; the class is
					// narrowed here, as it is in the write path.
					if rec.Class == k.Class {
						out = append(out, rec)
					}
				}
				if page.NextCursor == "" {
					break
				}
				f.Cursor = page.NextCursor
			}
		}
		return nil
	})
	return out, err
}

// generatedByAll returns what the automation wrote because of these records.
func (s *Server) generatedByAll(ctx context.Context, recs []*zone.Record) *[]gen.Record {
	var out []gen.Record
	for _, rec := range recs {
		if g := s.generatedBy(ctx, rec.ID); g != nil {
			out = append(out, *g...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return &out
}
