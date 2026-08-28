package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"net/netip"
	"strings"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// zoneColumns is the read projection, in the order [scanZone] expects. sort_key
// is not among them: it exists for ORDER BY and is derived from the name, so
// reading it back would only be a second copy of something already in hand.
const zoneColumns = `
	id, name, kind, rev_prefix, rev_prefix_len,
	soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl,
	default_ttl, auto_reverse, disabled, comment, created_at, updated_at`

// scannable is a row from either QueryRow or Query.
type scannable interface {
	Scan(dest ...any) error
}

// ZoneByID returns one zone.
func (r reader) ZoneByID(ctx context.Context, zid zone.ZoneID) (*zone.Zone, error) {
	row := r.q.QueryRowContext(ctx, `SELECT`+zoneColumns+` FROM zones WHERE id = ?`, string(zid))
	z, err := scanZone(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("zone with the identifier", zid)
	}
	return z, err
}

// ZoneByName returns the zone whose apex is exactly name.
func (r reader) ZoneByName(ctx context.Context, name zone.Name) (*zone.Zone, error) {
	row := r.q.QueryRowContext(ctx, `SELECT`+zoneColumns+` FROM zones WHERE name = ?`, name.String())
	z, err := scanZone(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("zone named", name)
	}
	return z, err
}

// IterZones streams every zone in canonical name order.
func (r reader) IterZones(ctx context.Context) iter.Seq2[*zone.Zone, error] {
	return streamRows(ctx, r.q, scanZone, `SELECT`+zoneColumns+` FROM zones ORDER BY sort_key, id`)
}

// ListZones returns one page of zones in canonical name order.
func (r reader) ListZones(ctx context.Context, f store.ZoneFilter) (store.Page[*zone.Zone], error) {
	var page store.Page[*zone.Zone]

	after, err := decodeCursor(f.Cursor, cursorZones)
	if err != nil {
		return page, err
	}

	var w where
	if after.ID != "" {
		w.add(`(sort_key, id) > (?, ?)`, after.Sort, after.ID)
	}
	if f.Kind != "" {
		w.add(`kind = ?`, string(f.Kind))
	}
	if !f.Name.IsZero() {
		w.add(`name = ?`, f.Name.String())
	}
	if f.Search != "" {
		// instr on a lowered column rather than LIKE, so that a percent sign or
		// an underscore in the needle is a character and not a wildcard.
		w.add(`instr(lower(name), ?) > 0`, strings.ToLower(f.Search))
	}
	if f.Disabled != nil {
		w.add(`disabled = ?`, boolToInt(*f.Disabled))
	}

	limit := f.EffectiveLimit()
	rows, err := r.q.QueryContext(ctx,
		`SELECT`+zoneColumns+` FROM zones`+w.clause()+` ORDER BY sort_key, id LIMIT ?`,
		append(w.args, limit+1)...)
	if err != nil {
		return page, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	zones := make([]*zone.Zone, 0, limit)
	for rows.Next() {
		z, serr := scanZone(rows)
		if serr != nil {
			return page, serr
		}
		zones = append(zones, z)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}

	// One row more than the page was asked for is how the listing knows there
	// is a next page without counting the whole table.
	if len(zones) > limit {
		last := zones[limit-1]
		zones = zones[:limit]
		page.NextCursor = cursor{
			Kind: cursorZones,
			Sort: last.Name.SortKey(),
			ID:   string(last.ID),
		}.encode()
	}
	page.Items = zones
	return page, nil
}

// ReverseZoneFor returns the most specific reverse zone covering addr.
//
// The obvious formulation (ask for all 33 possible IPv4 networks, or all 129
// IPv6 ones, in a single row-value IN list) measured badly twice over. SQLite
// answers it by scanning every zone rather than by using the index, and the
// query text itself grows to 129 branches that have to be parsed on every call.
// At 2000 zones that was around 0.5 ms for one lookup.
//
// So it asks a smaller question first: which prefix lengths exist at all? A
// deployment has a handful, not 129, and the answer comes from a covering index
// without touching the table. Only those lengths are then looked up, in one
// indexed query, and it no longer matters whether the address is IPv4 or IPv6 —
// see BenchmarkReverseZoneFor, which measures roughly 45 microseconds at ten
// zones, 55 at a hundred and 240 at two thousand.
func (r reader) ReverseZoneFor(ctx context.Context, addr netip.Addr) (*zone.Zone, error) {
	// An IPv4 address wrapped in IPv6 is the same address, but its 16 bytes
	// would match no 4-byte prefix.
	addr = addr.Unmap()
	if !addr.IsValid() {
		return nil, errors.New("sqlite: no address given to look up a reverse zone for")
	}

	lengths, err := r.reversePrefixLengths(ctx, addr.BitLen())
	if err != nil {
		return nil, err
	}
	if len(lengths) == 0 {
		return nil, notFound("reverse zone covering", addr)
	}

	branches := make([]string, len(lengths))
	args := make([]any, 0, 2*len(lengths))
	for i, bits := range lengths {
		prefix, perr := addr.Prefix(bits)
		if perr != nil {
			return nil, fmt.Errorf("sqlite: %v/%d: %w", addr, bits, perr)
		}
		branches[i] = `SELECT` + zoneColumns + ` FROM zones WHERE rev_prefix_len = ? AND rev_prefix = ?`
		args = append(args, bits, prefix.Addr().AsSlice())
	}

	row := r.q.QueryRowContext(ctx,
		`SELECT`+zoneColumns+` FROM (`+strings.Join(branches, " UNION ALL ")+
			`) ORDER BY rev_prefix_len DESC LIMIT 1`, args...)

	z, err := scanZone(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("reverse zone covering", addr)
	}
	return z, err
}

// reversePrefixLengths returns the prefix lengths reverse zones actually use,
// longest first, for networks an address of the given width could belong to.
func (r reader) reversePrefixLengths(ctx context.Context, bits int) (_ []int, err error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT DISTINCT rev_prefix_len FROM zones
		 WHERE rev_prefix IS NOT NULL AND rev_prefix_len <= ?
		 ORDER BY rev_prefix_len DESC`, bits)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CreateZone stores a new zone.
func (t *txn) CreateZone(ctx context.Context, z *zone.Zone) error {
	if err := checkZone(z); err != nil {
		return err
	}

	// Truncated to what the column holds, so that the value handed back is the
	// value stored rather than a nanosecond-precise one that no read reproduces.
	now := t.stamp()
	z.CreatedAt, z.UpdatedAt = now, now

	prefix, prefixLen := prefixColumns(z.Prefix)
	_, err := t.q.ExecContext(ctx, `
		INSERT INTO zones (
			id, name, sort_key, kind, rev_prefix, rev_prefix_len,
			soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl,
			default_ttl, auto_reverse, disabled, comment, created_at, updated_at)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?,?,?,?)`,
		string(z.ID), z.Name.String(), z.Name.SortKey(), string(z.Kind), prefix, prefixLen,
		z.SOA.NS.String(), z.SOA.Mbox.String(), int64(z.SOA.Serial.Uint32()),
		int64(z.SOA.Refresh), int64(z.SOA.Retry), int64(z.SOA.Expire),
		int64(z.SOA.Minimum), int64(z.SOA.TTL),
		int64(z.DefaultTTL), nullBool(z.AutoReverse), boolToInt(z.Disabled), z.Comment,
		now.UnixMilli(), now.UnixMilli())

	return translate(err, fmt.Sprintf("a zone named %s already exists", z.Name))
}

// UpdateZone replaces a zone's settings, leaving its records alone.
func (t *txn) UpdateZone(ctx context.Context, z *zone.Zone) error {
	if err := checkZone(z); err != nil {
		return err
	}

	now := t.stamp()
	z.UpdatedAt = now

	prefix, prefixLen := prefixColumns(z.Prefix)
	res, err := t.q.ExecContext(ctx, `
		UPDATE zones SET
			name = ?, sort_key = ?, kind = ?, rev_prefix = ?, rev_prefix_len = ?,
			soa_ns = ?, soa_mbox = ?, soa_serial = ?, soa_refresh = ?, soa_retry = ?,
			soa_expire = ?, soa_minimum = ?, soa_ttl = ?,
			default_ttl = ?, auto_reverse = ?, disabled = ?, comment = ?, updated_at = ?
		WHERE id = ?`,
		z.Name.String(), z.Name.SortKey(), string(z.Kind), prefix, prefixLen,
		z.SOA.NS.String(), z.SOA.Mbox.String(), int64(z.SOA.Serial.Uint32()),
		int64(z.SOA.Refresh), int64(z.SOA.Retry), int64(z.SOA.Expire),
		int64(z.SOA.Minimum), int64(z.SOA.TTL),
		int64(z.DefaultTTL), nullBool(z.AutoReverse), boolToInt(z.Disabled), z.Comment,
		now.UnixMilli(), string(z.ID))

	if err != nil {
		return translate(err, fmt.Sprintf("a zone named %s already exists", z.Name))
	}
	return oneRow(res, nil, "zone with the identifier", z.ID)
}

// DeleteZone removes a zone, and with it every record and journal entry that
// hangs off it.
func (t *txn) DeleteZone(ctx context.Context, zid zone.ZoneID) error {
	res, err := t.q.ExecContext(ctx, `DELETE FROM zones WHERE id = ?`, string(zid))
	return oneRow(res, err, "zone with the identifier", zid)
}

// SetZoneSerial advances a zone serial.
func (t *txn) SetZoneSerial(ctx context.Context, zid zone.ZoneID, serial zone.Serial) error {
	res, err := t.q.ExecContext(ctx,
		`UPDATE zones SET soa_serial = ?, updated_at = ? WHERE id = ?`,
		int64(serial.Uint32()), t.stamp().UnixMilli(), string(zid))
	return oneRow(res, err, "zone with the identifier", zid)
}

// checkZone is the last gate before a zone reaches the database. The applier
// has already validated it; this catches the case where something reached the
// store another way, and turns it into a rejection rather than a row that no
// reader can make sense of.
func checkZone(z *zone.Zone) error {
	if z == nil {
		return errors.New("sqlite: no zone given")
	}
	if !id.Valid(string(z.ID)) {
		// Deliberately not minted here. Once nodes replicate, the identifier
		// has to be part of the command every node applies, or two nodes would
		// invent two identifiers for one change.
		return fmt.Errorf("%w: a zone needs an identifier assigned before it is stored, and %q is not one",
			zone.ErrInvalid, z.ID)
	}
	return z.Validate()
}

// scanZone reads one row of [zoneColumns].
func scanZone(row scannable) (*zone.Zone, error) {
	var (
		z                                                   zone.Zone
		zid                                                 string
		name                                                string
		kind                                                string
		prefix                                              []byte
		prefixLen                                           sql.NullInt64
		soaNS                                               string
		soaMbox                                             string
		soaSerial                                           int64
		auto                                                sql.NullBool
		disabled                                            int64
		created                                             int64
		updated                                             int64
		refresh, retry, expire, minimum, soaTTL, defaultTTL int64
	)

	if err := row.Scan(
		&zid, &name, &kind, &prefix, &prefixLen,
		&soaNS, &soaMbox, &soaSerial, &refresh, &retry, &expire, &minimum, &soaTTL,
		&defaultTTL, &auto, &disabled, &z.Comment, &created, &updated,
	); err != nil {
		return nil, err
	}

	var err error
	z.ID = zone.ZoneID(zid)
	z.Kind = zone.Kind(kind)
	if z.Name, err = zone.ParseName(name); err != nil {
		return nil, corrupt("zones", zid, "name", err)
	}
	if z.SOA.NS, err = zone.ParseName(soaNS); err != nil {
		return nil, corrupt("zones", zid, "soa_ns", err)
	}
	if z.SOA.Mbox, err = zone.ParseName(soaMbox); err != nil {
		return nil, corrupt("zones", zid, "soa_mbox", err)
	}
	if z.Prefix, err = prefixFromColumns(prefix, prefixLen); err != nil {
		return nil, corrupt("zones", zid, "rev_prefix", err)
	}

	n := narrow{table: "zones", rowID: zid}
	z.SOA.Serial = zone.NewSerial(n.u32("soa_serial", soaSerial))
	z.SOA.Refresh = zone.TTL(n.u32("soa_refresh", refresh))
	z.SOA.Retry = zone.TTL(n.u32("soa_retry", retry))
	z.SOA.Expire = zone.TTL(n.u32("soa_expire", expire))
	z.SOA.Minimum = zone.TTL(n.u32("soa_minimum", minimum))
	z.SOA.TTL = zone.TTL(n.u32("soa_ttl", soaTTL))
	z.DefaultTTL = zone.TTL(n.u32("default_ttl", defaultTTL))
	if n.err != nil {
		return nil, n.err
	}
	if auto.Valid {
		z.AutoReverse = &auto.Bool
	}
	z.Disabled = disabled != 0
	z.CreatedAt = fromMillis(created)
	z.UpdatedAt = fromMillis(updated)

	return &z, nil
}

// prefixColumns splits a network into the two columns that hold it. A forward
// zone has neither.
func prefixColumns(p netip.Prefix) (addr, bits any) {
	if !p.IsValid() {
		return nil, nil
	}
	return p.Addr().AsSlice(), p.Bits()
}

// prefixFromColumns reassembles a network from its columns.
func prefixFromColumns(addrBytes []byte, bits sql.NullInt64) (netip.Prefix, error) {
	if addrBytes == nil && !bits.Valid {
		return netip.Prefix{}, nil
	}
	if addrBytes == nil || !bits.Valid {
		return netip.Prefix{}, errors.New("only one half of the network was stored")
	}
	addr, ok := netip.AddrFromSlice(addrBytes)
	if !ok {
		return netip.Prefix{}, fmt.Errorf("%d bytes are not an address", len(addrBytes))
	}
	prefix := netip.PrefixFrom(addr, int(bits.Int64))
	if !prefix.IsValid() {
		return netip.Prefix{}, fmt.Errorf("%v is not a network of %d bits", addr, bits.Int64)
	}
	return prefix, nil
}

// where accumulates the conditions of a listing.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

func (w *where) clause() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// corrupt reports a stored value this build cannot read back. It means the row
// was written by something other than this package, so it names the row.
func corrupt(table, rowID, column string, err error) error {
	return fmt.Errorf("sqlite: %s.%s of row %s cannot be read back: %w", table, column, rowID, err)
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func nullBool(p *bool) any {
	if p == nil {
		return nil
	}
	return boolToInt(*p)
}

// fromMillis turns a stored timestamp back into a time in UTC, so that a value
// read back compares equal to itself regardless of the reader's zone.
func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
