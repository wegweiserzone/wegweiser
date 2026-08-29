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

// ListZones returns one page of zones.
func (s *Server) ListZones(
	ctx context.Context, req gen.ListZonesRequestObject,
) (gen.ListZonesResponseObject, error) {
	f := store.ZoneFilter{
		Paging: store.Paging{
			Cursor: store.Cursor(deref(req.Params.Cursor, "")),
			Limit:  deref(req.Params.Limit, 0),
		},
		Search:   deref(req.Params.Search, ""),
		Disabled: req.Params.Disabled,
	}
	if req.Params.Kind != nil {
		f.Kind = zone.Kind(*req.Params.Kind)
	}
	if req.Params.Name != nil {
		name, err := parseName("the zone name", *req.Params.Name)
		if err != nil {
			return nil, err
		}
		f.Name = name
	}

	var page store.Page[*zone.Zone]
	if err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		page, verr = r.ListZones(ctx, f)
		return verr
	}); err != nil {
		return nil, err
	}

	items := make([]gen.Zone, 0, len(page.Items))
	for _, z := range page.Items {
		items = append(items, zoneToAPI(z))
	}
	out := gen.ListZones200JSONResponse{Items: items}
	if page.NextCursor != "" {
		out.NextCursor = ptr(string(page.NextCursor))
	}
	return out, nil
}

// GetZone returns one zone.
func (s *Server) GetZone(
	ctx context.Context, req gen.GetZoneRequestObject,
) (gen.GetZoneResponseObject, error) {
	z, err := s.zoneByID(ctx, zone.ZoneID(req.ZoneId))
	if err != nil {
		return nil, err
	}

	lame, err := s.lameNameServers(ctx, z)
	if err != nil {
		return nil, err
	}
	return gen.GetZone200JSONResponse(zoneDetailToAPI(z, lame)), nil
}

// lameNameServers finds the name servers this zone points at and has no
// address for.
//
// It reads the zone's NS records and asks about each distinct target, rather
// than walking the zone the way a check does, which is what makes it cheap
// enough to answer on every read of a zone (D31).
func (s *Server) lameNameServers(
	ctx context.Context, z *zone.Zone,
) ([]zone.NameServer, error) {
	var lame []zone.NameServer
	err := s.store.View(ctx, func(r store.Reader) error {
		lame = lame[:0]

		ns, lerr := allOfType(ctx, r, z.ID, zone.TypeNS)
		if lerr != nil {
			return lerr
		}
		for _, server := range zone.NameServersInside(*z, ns) {
			addressed, aerr := hasAddress(ctx, r, z.ID, server.Target)
			if aerr != nil {
				return aerr
			}
			if !addressed {
				lame = append(lame, server)
			}
		}
		return nil
	})
	return lame, err
}

// allOfType reads every record of one type in a zone, following the cursor to
// the end.
func allOfType(
	ctx context.Context, r store.Reader, zid zone.ZoneID, typ zone.RRType,
) ([]zone.Record, error) {
	f := store.RecordFilter{
		ZoneID: zid,
		Types:  []zone.RRType{typ},
		Paging: store.Paging{Limit: store.MaxLimit},
	}
	var out []zone.Record
	for {
		page, err := r.ListRecords(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, rec := range page.Items {
			out = append(out, *rec)
		}
		if page.NextCursor == "" {
			return out, nil
		}
		f.Cursor = page.NextCursor
	}
}

// CreateZone brings a zone into existence.
func (s *Server) CreateZone(
	ctx context.Context, req gen.CreateZoneRequestObject,
) (gen.CreateZoneResponseObject, error) {
	name, err := zoneApex(req.Body.Name)
	if err != nil {
		return nil, err
	}

	soa, err := soaFor(name, req.Body.Soa)
	if err != nil {
		return nil, err
	}

	z, err := zone.NewZone(name, soa)
	if err != nil {
		return nil, err
	}
	if req.Body.DefaultTtl != nil {
		if z.DefaultTTL, err = ttlFrom("the default TTL", *req.Body.DefaultTtl); err != nil {
			return nil, err
		}
	}
	z.Comment = deref(req.Body.Comment, "")

	apexNS, err := zone.NewRecord(z.ID, name, zone.ClassIN, zone.TypeNS, z.DefaultTTL, soa.NS.String())
	if err != nil {
		return nil, fmt.Errorf("%w: the zone's own name server record: %w", zone.ErrInvalid, err)
	}

	res, err := s.applier.CreateZone(ctx, &z, []zone.Record{apexNS}, s.meta(ctx, "create zone"))
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)

	return gen.CreateZone201JSONResponse{
		Body:    zoneToAPI(&z),
		Headers: gen.CreateZone201ResponseHeaders{Location: ptr(basePath + "/zones/" + string(z.ID))},
	}, nil
}

// UpdateZone changes a zone's own settings, leaving its records alone.
func (s *Server) UpdateZone(
	ctx context.Context, req gen.UpdateZoneRequestObject,
) (gen.UpdateZoneResponseObject, error) {
	z, err := s.zoneByID(ctx, zone.ZoneID(req.ZoneId))
	if err != nil {
		return nil, err
	}
	if perr := patchZone(z, *req.Body); perr != nil {
		return nil, perr
	}

	res, err := s.applier.UpdateZone(ctx, z, s.meta(ctx, "update zone"))
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)

	return gen.UpdateZone200JSONResponse(zoneToAPI(z)), nil
}

// patchZone applies what the client sent onto the zone as it stands.
func patchZone(z *zone.Zone, in gen.UpdateZone) error {
	if in.Soa != nil {
		if err := patchSOA(&z.SOA, *in.Soa); err != nil {
			return err
		}
	}
	if in.DefaultTtl != nil {
		ttl, err := ttlFrom("the default TTL", *in.DefaultTtl)
		if err != nil {
			return err
		}
		z.DefaultTTL = ttl
	}
	// Three states, which is why this field is nullable and the others are not:
	// absent leaves the zone as it is, null puts it back on the server-wide
	// setting, and a value overrides it.
	if in.AutoReverse.IsSpecified() {
		if in.AutoReverse.IsNull() {
			z.AutoReverse = nil
		} else {
			z.AutoReverse = ptr(in.AutoReverse.MustGet())
		}
	}
	if in.Disabled != nil {
		z.Disabled = *in.Disabled
	}
	if in.Comment != nil {
		z.Comment = *in.Comment
	}
	return nil
}

// patchSOA applies the start-of-authority fields the client sent. The serial is
// not among them; it belongs to the journal (docs/decisions/ D2).
func patchSOA(soa *zone.SOA, in gen.SOAInput) error {
	if in.PrimaryNs != nil {
		ns, err := parseName("the primary name server", *in.PrimaryNs)
		if err != nil {
			return err
		}
		soa.NS = ns
	}
	if in.Mailbox != nil {
		mbox, err := parseName("the mailbox", *in.Mailbox)
		if err != nil {
			return err
		}
		soa.Mbox = mbox
	}
	for _, f := range []struct {
		what string
		in   *int64
		out  *zone.TTL
	}{
		{"the refresh interval", in.Refresh, &soa.Refresh},
		{"the retry interval", in.Retry, &soa.Retry},
		{"the expiry", in.Expire, &soa.Expire},
		{"the negative-caching TTL", in.Minimum, &soa.Minimum},
		{"the SOA TTL", in.Ttl, &soa.TTL},
	} {
		if f.in == nil {
			continue
		}
		v, err := ttlFrom(f.what, *f.in)
		if err != nil {
			return err
		}
		*f.out = v
	}
	return nil
}

// DeleteZone removes a zone and everything in it.
func (s *Server) DeleteZone(
	ctx context.Context, req gen.DeleteZoneRequestObject,
) (gen.DeleteZoneResponseObject, error) {
	res, err := s.applier.DeleteZone(ctx, zone.ZoneID(req.ZoneId), s.meta(ctx, "delete zone"))
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)
	return gen.DeleteZone204Response{}, nil
}

// zoneByID reads one zone, or says which one was not there.
func (s *Server) zoneByID(ctx context.Context, zid zone.ZoneID) (*zone.Zone, error) {
	var z *zone.Zone
	err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		z, verr = r.ZoneByID(ctx, zid)
		return verr
	})
	return z, err
}

// soaFor builds the start-of-authority parameters for a new zone, from what the
// client supplied or from the conventional defaults.
func soaFor(name zone.Name, in *gen.SOAInput) (zone.SOA, error) {
	soa := defaultSOAFor(name)
	if in != nil {
		if err := patchSOA(&soa, *in); err != nil {
			return zone.SOA{}, err
		}
	}

	// The two that cannot always be derived. Validation would refuse them
	// anyway, but it cannot say that the fix is to send one, because by then
	// nothing remembers that this zone was being created rather than edited.
	if soa.NS.IsZero() {
		return zone.SOA{}, badRequest(
			"no name server can be derived for %q, so soa.primaryNs has to be given", name)
	}
	if soa.Mbox.IsZero() {
		return zone.SOA{}, badRequest(
			"no mailbox can be derived for %q, so soa.mailbox has to be given", name)
	}
	return soa, nil
}

// defaultSOAFor is the start of authority a zone gets before the client's own
// fields are laid over it: this server's timers, and a name server and mailbox
// under the zone's own apex, which is where every other server puts them.
func defaultSOAFor(name zone.Name) zone.SOA {
	soa := zone.DefaultSOA(zone.Name{}, zone.Name{})
	if primary, err := name.Child("ns1"); err == nil {
		soa.NS = primary
	}
	if mbox, err := name.Child("hostmaster"); err == nil {
		soa.Mbox = mbox
	}
	return soa
}

// meta names who caused a change and why, for the journal.
func (s *Server) meta(ctx context.Context, comment string) apply.Meta {
	m := apply.Meta{Source: journal.SourceAPI, Comment: comment}
	if sub := subjectOf(ctx); sub != nil {
		m.Actor = sub.name
	}
	return m
}

// CheckZone reports what is wrong with a zone as it stands.
func (s *Server) CheckZone(
	ctx context.Context, req gen.CheckZoneRequestObject,
) (gen.CheckZoneResponseObject, error) {
	z, err := s.zoneByID(ctx, zone.ZoneID(req.ZoneId))
	if err != nil {
		return nil, err
	}

	// Streamed rather than read whole, which is the difference between this and
	// the export beside it: a check is the one read that has to work on a zone
	// too large to hold, because that is where hand-repaired data lives.
	var report zone.Report
	if verr := s.store.View(ctx, func(r store.Reader) error {
		check := zone.NewCheck(*z)
		for rec, ierr := range r.IterZoneRecords(ctx, z.ID) {
			if ierr != nil {
				return ierr
			}
			check.Add(rec)
		}
		// After the walk, not during it: the iterator above holds a cursor on
		// this same transaction.
		var derr error
		report, derr = check.Done(func(name zone.Name) (bool, error) {
			return hasAddress(ctx, r, z.ID, name)
		})
		return derr
	}); verr != nil {
		return nil, verr
	}

	findings := report.Findings
	if deref(req.Params.Reverse, false) {
		// After the walk and its own transaction: this one plans a write, so
		// it takes the zone's lock rather than sharing the read above.
		reverse, rerr := s.applier.CheckReverse(ctx, z.ID)
		if rerr != nil {
			return nil, rerr
		}
		findings = append(findings, reverse...)
	}

	out := gen.ZoneCheck{
		Records:   report.Records,
		Truncated: report.Truncated,
		Findings:  make([]gen.Finding, len(findings)),
	}
	for i, f := range findings {
		out.Findings[i] = gen.Finding{
			Severity: gen.FindingSeverity(f.Severity),
			Scope:    gen.FindingScope(f.Scope),
			Name:     f.Name.String(),
			Detail:   f.Detail,
		}
		if f.Record != "" {
			out.Findings[i].Record = ptr(string(f.Record))
		}
	}
	return gen.CheckZone200JSONResponse(out), nil
}

// ReconcileZone writes the reverse entries a zone's records imply and it does
// not have.
func (s *Server) ReconcileZone(
	ctx context.Context, req gen.ReconcileZoneRequestObject,
) (gen.ReconcileZoneResponseObject, error) {
	res, err := s.applier.Reconcile(ctx, zone.ZoneID(req.ZoneId),
		s.meta(ctx, "fill in the reverse entries this zone was missing"))
	if err != nil {
		return nil, err
	}
	s.republish(ctx, res)

	out := gen.ReconcileResult{
		Conflicts:    conflictsToAPI(res.Conflicts),
		MissingZones: missingZonesToAPI(res.MissingZones),
	}
	if c := res.Commit(); c != nil {
		out.Commit = ptr(commitToAPI(c))
	}
	return gen.ReconcileZone200JSONResponse(out), nil
}

// hasAddress reports whether a zone answers for a name with an address.
func hasAddress(
	ctx context.Context, r store.Reader, zid zone.ZoneID, name zone.Name,
) (bool, error) {
	page, err := r.ListRecords(ctx, store.RecordFilter{
		ZoneID: zid,
		Name:   name,
		Types:  []zone.RRType{zone.TypeA, zone.TypeAAAA},
		Paging: store.Paging{Limit: 1},
	})
	if err != nil {
		return false, err
	}
	return len(page.Items) > 0, nil
}
