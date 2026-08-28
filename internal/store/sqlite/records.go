package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"net/netip"
	"strings"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// recordColumns is the read projection, in the order [scanRecord] expects.
// sort_key, rdata_hash and addr are all derived from the columns that are here,
// so they are written and indexed but never read back.
const recordColumns = `
	id, zone_id, name, class, rrtype, ttl, rdata,
	managed_by, managed_kind, comment, disabled, created_at, updated_at`

// recordOrder is the canonical order of RFC 4034 §6.1, then by type, then by
// identity so that the order is total and a cursor can resume in it. It matches
// the records_zone_sort_idx index.
const recordOrder = ` ORDER BY sort_key, rrtype, id`

// recordOrderAtName is the same order for a listing already narrowed to one
// owner name, where every row shares a sort key and it therefore orders
// nothing.
//
// It is not a micro-optimisation but the difference between an index seek and
// a scan of the whole zone. Given both `zone_id = ?` and `name = ?`, SQLite
// still prefers records_zone_sort_idx, because that index alone satisfies
// ORDER BY sort_key and avoids the sort, and then walks every record in the
// zone testing the name row by row. Dropping the column it cannot vary lets it
// take records_rr_uq and seek straight to the name. Measured on a zone of a
// hundred thousand records, that is the difference between milliseconds and
// microseconds, on the path every write and every RRset edit goes down.
const recordOrderAtName = ` ORDER BY rrtype, id`

// RecordByID returns one record.
func (r reader) RecordByID(ctx context.Context, rid zone.RecordID) (*zone.Record, error) {
	row := r.q.QueryRowContext(ctx, `SELECT`+recordColumns+` FROM records WHERE id = ?`, string(rid))
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("record with the identifier", rid)
	}
	return rec, err
}

// ListRecords returns one page of records in canonical order.
func (r reader) ListRecords(ctx context.Context, f store.RecordFilter) (store.Page[*zone.Record], error) {
	var page store.Page[*zone.Record]

	after, err := decodeCursor(f.Cursor, cursorRecords)
	if err != nil {
		return page, err
	}

	w := recordConditions(f, after)

	order := recordOrder
	if !f.Name.IsZero() {
		order = recordOrderAtName
	}

	limit := f.EffectiveLimit()
	rows, err := r.q.QueryContext(ctx,
		`SELECT`+recordColumns+` FROM records`+w.clause()+order+` LIMIT ?`,
		append(w.args, limit+1)...)
	if err != nil {
		return page, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	records := make([]*zone.Record, 0, limit)
	for rows.Next() {
		rec, serr := scanRecord(rows)
		if serr != nil {
			return page, serr
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}

	if len(records) > limit {
		last := records[limit-1]
		records = records[:limit]
		page.NextCursor = cursor{
			Kind: cursorRecords,
			Sort: last.Name.SortKey(),
			Type: uint16(last.Type),
			ID:   string(last.ID),
		}.encode()
	}
	page.Items = records
	return page, nil
}

// recordConditions turns a filter and a cursor position into a WHERE clause.
func recordConditions(f store.RecordFilter, after cursor) where {
	var w where

	if after.ID != "" {
		if f.Name.IsZero() {
			w.add(`(sort_key, rrtype, id) > (?, ?, ?)`, after.Sort, int64(after.Type), after.ID)
		} else {
			// Every row shares one sort key here, so comparing it would only
			// stop the index from being used. See [recordOrderAtName].
			w.add(`(rrtype, id) > (?, ?)`, int64(after.Type), after.ID)
		}
	}
	if f.ZoneID != "" {
		w.add(`zone_id = ?`, string(f.ZoneID))
	}
	if !f.Name.IsZero() {
		w.add(`name = ?`, f.Name.String())
	}
	if !f.Under.IsZero() {
		// A sort key terminates every label with two zero octets, so the key of
		// a name is a byte prefix of the key of everything below it, and one
		// indexed range covers the whole branch.
		low := f.Under.SortKey()
		high, ok := upperBound(low)
		if !ok {
			// The root, below which everything lies. No bound to add.
			return w
		}
		w.add(`sort_key >= ? AND sort_key < ?`, low, high)
	}
	if len(f.Types) > 0 {
		marks := make([]string, len(f.Types))
		args := make([]any, len(f.Types))
		for i, t := range f.Types {
			marks[i] = "?"
			args[i] = int64(t)
		}
		w.add(`rrtype IN (`+strings.Join(marks, ",")+`)`, args...)
	}
	if f.Prefix.IsValid() {
		lo, hi := prefixBounds(f.Prefix)
		// "addr IS NOT NULL" is repeated from the partial index's own condition,
		// without which SQLite will not use it. The length is compared because
		// blob ordering is bytewise before it is by length, so a sixteen-byte
		// address can otherwise fall inside a four-byte range.
		w.add(`addr IS NOT NULL AND length(addr) = ? AND addr BETWEEN ? AND ?`, len(lo), lo, hi)
	}
	if f.Search != "" {
		needle := strings.ToLower(f.Search)
		w.add(`(instr(lower(name), ?) > 0 OR instr(lower(rdata), ?) > 0)`, needle, needle)
	}
	if f.Managed != nil {
		if *f.Managed {
			w.add(`managed_by IS NOT NULL`)
		} else {
			w.add(`managed_by IS NULL`)
		}
	}
	return w
}

// IterZoneRecords streams every record of a zone in canonical order.
func (r reader) IterZoneRecords(ctx context.Context, zid zone.ZoneID) iter.Seq2[*zone.Record, error] {
	return r.stream(ctx, `SELECT`+recordColumns+` FROM records WHERE zone_id = ?`+recordOrder, string(zid))
}

// stream runs a record query whose result may be too large to hold at once.
func (r reader) stream(ctx context.Context, query string, args ...any) iter.Seq2[*zone.Record, error] {
	return streamRows(ctx, r.q, scanRecord, query, args...)
}

// streamRows runs a query and yields its rows one at a time, so that a result
// nobody could hold in memory can still be read.
//
// The close-and-report dance below is the reason this is one function rather
// than one per row type. Closing a result set reports what the iteration itself
// would have, so it is the last chance to say that the stream ended early —
// and once the caller has stopped listening there is nobody left to tell.
func streamRows[T any](
	ctx context.Context, q querier, scan func(scannable) (T, error), query string, args ...any,
) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T

		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			yield(zero, err)
			return
		}

		listening := true
		defer func() {
			if cerr := rows.Close(); cerr != nil && listening {
				yield(zero, cerr)
			}
		}()

		for rows.Next() {
			item, serr := scan(rows)
			if serr != nil {
				listening = false
				yield(zero, serr)
				return
			}
			if !yield(item, nil) {
				listening = false
				return
			}
		}
		if err := rows.Err(); err != nil {
			listening = false
			yield(zero, err)
		}
	}
}

// RecordsByAddress returns every A and AAAA record pointing at addr, across all
// zones.
func (r reader) RecordsByAddress(ctx context.Context, addr netip.Addr) ([]*zone.Record, error) {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return nil, errors.New("sqlite: no address given to look records up by")
	}
	return r.collect(ctx,
		`SELECT`+recordColumns+` FROM records WHERE addr = ?`+recordOrder, addr.AsSlice())
}

// ManagedBy returns the records generated from the given source record.
func (r reader) ManagedBy(ctx context.Context, rid zone.RecordID) ([]*zone.Record, error) {
	return r.collect(ctx,
		`SELECT`+recordColumns+` FROM records WHERE managed_by = ?`+recordOrder, string(rid))
}

// ManagedByZone streams the records generated from any record of a zone and
// living somewhere else.
func (r reader) ManagedByZone(ctx context.Context, zid zone.ZoneID) iter.Seq2[*zone.Record, error] {
	return r.stream(ctx,
		`SELECT`+recordColumns+` FROM records
		 WHERE zone_id <> ? AND managed_by IN (SELECT id FROM records WHERE zone_id = ?)`+recordOrder,
		string(zid), string(zid))
}

// collect runs a record query whose result is small enough to hold at once.
func (r reader) collect(ctx context.Context, query string, args ...any) (_ []*zone.Record, err error) {
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	var out []*zone.Record
	for rows.Next() {
		rec, serr := scanRecord(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// InsertRecord stores a new record.
func (t *txn) InsertRecord(ctx context.Context, rec *zone.Record) error {
	if err := checkRecord(rec); err != nil {
		return err
	}

	now := t.stamp()
	rec.CreatedAt, rec.UpdatedAt = now, now

	_, err := t.q.ExecContext(ctx, `
		INSERT INTO records (
			id, zone_id, name, sort_key, class, rrtype, ttl, rdata, rdata_hash, addr,
			managed_by, managed_kind, comment, disabled, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?)`,
		string(rec.ID), string(rec.ZoneID), rec.Name.String(), rec.Name.SortKey(),
		int64(rec.Class), int64(rec.Type), int64(rec.TTL),
		rec.RData.String(), rdataHash(rec.RData), addrColumn(rec),
		nullString(string(rec.ManagedBy)), nullString(string(rec.ManagedKind)),
		rec.Comment, boolToInt(rec.Disabled), now.UnixMilli(), now.UnixMilli())

	return translate(err, duplicateRecord(rec))
}

// UpdateRecord replaces a record in place, keeping its identity.
func (t *txn) UpdateRecord(ctx context.Context, rec *zone.Record) error {
	if err := checkRecord(rec); err != nil {
		return err
	}

	now := t.stamp()
	rec.UpdatedAt = now

	res, err := t.q.ExecContext(ctx, `
		UPDATE records SET
			zone_id = ?, name = ?, sort_key = ?, class = ?, rrtype = ?, ttl = ?,
			rdata = ?, rdata_hash = ?, addr = ?,
			managed_by = ?, managed_kind = ?, comment = ?, disabled = ?, updated_at = ?
		WHERE id = ?`,
		string(rec.ZoneID), rec.Name.String(), rec.Name.SortKey(),
		int64(rec.Class), int64(rec.Type), int64(rec.TTL),
		rec.RData.String(), rdataHash(rec.RData), addrColumn(rec),
		nullString(string(rec.ManagedBy)), nullString(string(rec.ManagedKind)),
		rec.Comment, boolToInt(rec.Disabled), now.UnixMilli(), string(rec.ID))

	if err != nil {
		return translate(err, duplicateRecord(rec))
	}
	return oneRow(res, nil, "record with the identifier", rec.ID)
}

// DeleteRecord removes one record, and with it anything generated from it.
func (t *txn) DeleteRecord(ctx context.Context, rid zone.RecordID) error {
	res, err := t.q.ExecContext(ctx, `DELETE FROM records WHERE id = ?`, string(rid))
	return oneRow(res, err, "record with the identifier", rid)
}

// DeleteRRset removes every record of one owner name, class and type.
//
// Unlike the single-record deletes this does not complain about an empty set: a
// caller asking for an RRset to be gone has got what it asked for.
func (t *txn) DeleteRRset(ctx context.Context, zid zone.ZoneID, key zone.RRsetKey) error {
	_, err := t.q.ExecContext(ctx,
		`DELETE FROM records WHERE zone_id = ? AND name = ? AND class = ? AND rrtype = ?`,
		string(zid), key.Name.String(), int64(key.Class), int64(key.Type))
	return err
}

// checkRecord is the last gate before a record reaches the database.
func checkRecord(rec *zone.Record) error {
	if rec == nil {
		return errors.New("sqlite: no record given")
	}
	if !id.Valid(string(rec.ID)) {
		return fmt.Errorf("%w: a record needs an identifier assigned before it is stored, and %q is not one",
			zone.ErrInvalid, rec.ID)
	}
	if !id.Valid(string(rec.ZoneID)) {
		return fmt.Errorf("%w: a record belongs to a zone, and %q is not a zone identifier",
			zone.ErrInvalid, rec.ZoneID)
	}
	return rec.Validate()
}

func duplicateRecord(rec *zone.Record) string {
	return fmt.Sprintf("%s already has a %s record %q (RFC 2181 §5)",
		rec.Name, rec.Type, rec.RData)
}

// scanRecord reads one row of [recordColumns].
func scanRecord(row scannable) (*zone.Record, error) {
	var (
		rec         zone.Record
		rid, zid    string
		name        string
		class, typ  int64
		ttl         int64
		rdata       string
		managedBy   sql.NullString
		managedKind sql.NullString
		disabled    int64
		created     int64
		updated     int64
	)

	if err := row.Scan(
		&rid, &zid, &name, &class, &typ, &ttl, &rdata,
		&managedBy, &managedKind, &rec.Comment, &disabled, &created, &updated,
	); err != nil {
		return nil, err
	}

	var err error
	rec.ID = zone.RecordID(rid)
	rec.ZoneID = zone.ZoneID(zid)
	if rec.Name, err = zone.ParseName(name); err != nil {
		return nil, corrupt("records", rid, "name", err)
	}
	// Not ParseRData: this is data this package canonicalised on the way in, and
	// re-deriving that on every read would cost a zone with half a million
	// records a second of parsing on every snapshot rebuild.
	if rec.RData, err = zone.RDataFromCanonical(rdata); err != nil {
		return nil, corrupt("records", rid, "rdata", err)
	}

	n := narrow{table: "records", rowID: rid}
	rec.Class = zone.Class(n.u16("class", class))
	rec.Type = zone.RRType(n.u16("rrtype", typ))
	rec.TTL = zone.TTL(n.u32("ttl", ttl))
	if n.err != nil {
		return nil, n.err
	}
	rec.ManagedBy = zone.RecordID(managedBy.String)
	rec.ManagedKind = zone.ManagedKind(managedKind.String)
	rec.Disabled = disabled != 0
	rec.CreatedAt = fromMillis(created)
	rec.UpdatedAt = fromMillis(updated)

	return &rec, nil
}

// prefixBounds returns the first and last address of a network, as the bytes
// the addr column holds.
func prefixBounds(p netip.Prefix) (lo, hi []byte) {
	p = p.Masked()
	lo = p.Addr().AsSlice()
	hi = append([]byte(nil), lo...)

	for i := range hi {
		switch covered := i * 8; {
		case covered >= p.Bits():
			hi[i] = 0xFF
		case covered+8 > p.Bits():
			// The one byte the prefix ends inside: everything below the
			// boundary is host, and the last address has all of it set.
			hi[i] |= byte(1<<(covered+8-p.Bits()) - 1)
		}
	}
	return lo, hi
}

// rdataHash is what the uniqueness index compares instead of the data itself,
// because a TXT record holds up to 64 KB and that does not belong in a B-tree
// key. Half a SHA-256 is ample: this guards a uniqueness constraint on data the
// server itself canonicalised, not a signature.
func rdataHash(d zone.RData) []byte {
	sum := sha256.Sum256([]byte(d.String()))
	return sum[:16]
}

// addrColumn is the indexed address of an A or AAAA record, and nothing for
// every other type. It answers "which names point at this address?" without a
// scan.
func addrColumn(rec *zone.Record) any {
	addr, ok := rec.Address()
	if !ok {
		return nil
	}
	return addr.Unmap().AsSlice()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
