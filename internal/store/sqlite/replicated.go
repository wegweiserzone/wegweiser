package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// exportCommitPage is how many commit headers are read at a time. Their events
// are fetched per commit afterwards, which needs the rows carrying the headers
// closed: one connection carries one statement at a time, so a query made while
// another is still streaming would wait for a turn that is not coming.
const exportCommitPage = 512

// ExportReplicated streams everything a cluster replicates, grouped by kind.
func (r reader) ExportReplicated(ctx context.Context) iter.Seq2[*store.Replicated, error] {
	return func(yield func(*store.Replicated, error) bool) {
		for _, stream := range []iter.Seq2[*store.Replicated, error]{
			r.exportSettings(ctx),
			r.exportTokens(ctx),
			r.exportKeys(ctx),
			r.exportZones(ctx),
			r.exportRecords(ctx),
			r.exportCommits(ctx),
		} {
			for item, err := range stream {
				if !yield(item, err) || err != nil {
					return
				}
			}
		}
	}
}

func (r reader) exportSettings(ctx context.Context) iter.Seq2[*store.Replicated, error] {
	return streamRows(ctx, r.q, func(row scannable) (*store.Replicated, error) {
		var (
			s     store.Setting
			value string
		)
		if err := row.Scan(&s.Key, &value); err != nil {
			return nil, err
		}
		s.Value = []byte(value)
		return &store.Replicated{Kind: store.ReplicatedSetting, Setting: &s}, nil
	}, `SELECT key, value FROM settings ORDER BY key`)
}

func (r reader) exportTokens(ctx context.Context) iter.Seq2[*store.Replicated, error] {
	return streamRows(ctx, r.q, func(row scannable) (*store.Replicated, error) {
		tok, err := scanToken(row)
		if err != nil {
			return nil, err
		}
		// When a token was last used is a statement about this node and
		// travels nowhere (D24).
		tok.LastUsedAt = time.Time{}
		return &store.Replicated{Kind: store.ReplicatedToken, Token: tok}, nil
	}, `SELECT`+tokenColumns+` FROM api_tokens ORDER BY created_at, id`)
}

func (r reader) exportKeys(ctx context.Context) iter.Seq2[*store.Replicated, error] {
	return streamRows(ctx, r.q, func(row scannable) (*store.Replicated, error) {
		k, err := scanTSIGKey(row)
		if err != nil {
			return nil, err
		}
		return &store.Replicated{Kind: store.ReplicatedTSIGKey, TSIGKey: k}, nil
	}, `SELECT`+tsigColumns+` FROM tsig_keys ORDER BY created_at, id`)
}

func (r reader) exportZones(ctx context.Context) iter.Seq2[*store.Replicated, error] {
	return streamRows(ctx, r.q, func(row scannable) (*store.Replicated, error) {
		z, err := scanZone(row)
		if err != nil {
			return nil, err
		}
		return &store.Replicated{Kind: store.ReplicatedZone, Zone: z}, nil
	}, `SELECT`+zoneColumns+` FROM zones ORDER BY sort_key, id`)
}

func (r reader) exportRecords(ctx context.Context) iter.Seq2[*store.Replicated, error] {
	return streamRows(ctx, r.q, func(row scannable) (*store.Replicated, error) {
		rec, err := scanRecord(row)
		if err != nil {
			return nil, err
		}
		return &store.Replicated{Kind: store.ReplicatedRecord, Record: rec}, nil
	}, `SELECT`+recordColumns+` FROM records ORDER BY zone_id, sort_key, rrtype, id`)
}

// exportCommits streams the journal oldest first, each commit carrying its
// events. Identifiers are ULIDs, so ordering by one is ordering by time.
func (r reader) exportCommits(ctx context.Context) iter.Seq2[*store.Replicated, error] {
	return func(yield func(*store.Replicated, error) bool) {
		after := ""
		for {
			page, err := r.commitPage(ctx, after)
			if err != nil {
				yield(nil, err)
				return
			}

			for _, c := range page {
				events, eerr := r.events(ctx, c.ID)
				if eerr != nil {
					yield(nil, eerr)
					return
				}
				c.Events = events
				if !yield(&store.Replicated{Kind: store.ReplicatedCommit, Commit: c}, nil) {
					return
				}
			}

			if len(page) < exportCommitPage {
				return
			}
			after = string(page[len(page)-1].ID)
		}
	}
}

// commitPage reads the next page of commit headers, without their events.
func (r reader) commitPage(ctx context.Context, after string) (_ []*journal.Commit, err error) {
	var w where
	if after != "" {
		w.add(`id > ?`, after)
	}

	rows, err := r.q.QueryContext(ctx,
		`SELECT`+commitColumns+` FROM journal_commits`+w.clause()+` ORDER BY id LIMIT ?`,
		append(w.args, exportCommitPage)...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	out := make([]*journal.Commit, 0, exportCommitPage)
	for rows.Next() {
		c, serr := scanCommit(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReplaceReplicated empties this store of everything a cluster replicates and
// rebuilds it from in.
func (s *Store) ReplaceReplicated(
	ctx context.Context, index uint64, in iter.Seq2[*store.Replicated, error],
) error {
	if index == 0 {
		return errors.New("sqlite: zero is no position in a log, and a restore arrives from one")
	}
	if in == nil {
		return errors.New("sqlite: no content given to restore")
	}

	return s.inTx(ctx, func(tx *sql.Tx) (err error) {
		t := &txn{reader: reader{q: tx, now: s.now}}

		// A generated record points at the record it was derived from, and the
		// stream says nothing about which of the two comes first. SQLite can
		// hold every foreign key check until the transaction commits, which is
		// what this asks for; it goes back to immediate checking on its own.
		if _, perr := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); perr != nil {
			return fmt.Errorf("sqlite: holding the foreign key checks until the restore is in: %w", perr)
		}
		if derr := emptyReplicated(ctx, tx); derr != nil {
			return derr
		}

		zones, zerr := tx.PrepareContext(ctx, insertZone)
		if zerr != nil {
			return zerr
		}
		defer func() { err = errors.Join(err, zones.Close()) }()

		records, rerr := tx.PrepareContext(ctx, insertRecord)
		if rerr != nil {
			return rerr
		}
		defer func() { err = errors.Join(err, records.Close()) }()

		w := restore{t: t, zones: zones, records: records}

		for item, ierr := range in {
			if ierr != nil {
				return ierr
			}
			if verr := item.Validate(); verr != nil {
				return verr
			}
			if werr := w.write(ctx, item); werr != nil {
				return werr
			}
		}

		return t.SetAppliedIndex(ctx, index)
	})
}

// emptyReplicated removes every replicated row, leaving the node-local ones.
//
// The order is the one that would work without deferred foreign keys anyway,
// so that this reads as what it is rather than as something relying on the
// pragma above.
func emptyReplicated(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`DELETE FROM records`,
		`DELETE FROM zones`,
		`DELETE FROM journal_events`,
		`DELETE FROM journal_commits`,
		`DELETE FROM api_tokens`,
		`DELETE FROM tsig_keys`,
		`DELETE FROM settings`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: emptying the store before the restore (%s): %w", stmt, err)
		}
	}
	return nil
}

// The two inserts a restore runs in bulk. They name every column rather than
// leaning on the column order of the table, so that a migration adding one
// fails here loudly instead of writing a row that is quietly short.
const (
	insertZone = `
		INSERT INTO zones (
			id, name, sort_key, kind, rev_prefix, rev_prefix_len,
			soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl,
			default_ttl, auto_reverse, disabled, comment, created_at, updated_at)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?,?,?,?)`

	insertRecord = `
		INSERT INTO records (
			id, zone_id, name, sort_key, class, rrtype, ttl, rdata, rdata_hash, addr,
			managed_by, managed_kind, comment, disabled, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?)`
)

// restore writes the items of a log snapshot back.
//
// Zones, records and keys are written here rather than through the [store.Tx]
// methods that create them: those stamp a zone and a record with the time they
// were called, which is right for a change somebody is making and wrong for a
// row being put back as it was, and the one that creates a key refuses a
// revoked one, which has no secret left to refuse it for.
//
// Zones and records arrive in bulk, so their inserts are prepared once for the
// whole restore rather than compiled per row.
type restore struct {
	t *txn

	zones   *sql.Stmt
	records *sql.Stmt
}

func (w *restore) write(ctx context.Context, item *store.Replicated) error {
	switch item.Kind {
	case store.ReplicatedSetting:
		return w.t.PutSetting(ctx, item.Setting.Key, item.Setting.Value)
	case store.ReplicatedToken:
		return w.t.CreateToken(ctx, item.Token)
	case store.ReplicatedTSIGKey:
		return w.key(ctx, item.TSIGKey)
	case store.ReplicatedZone:
		return w.zone(ctx, item.Zone)
	case store.ReplicatedRecord:
		return w.record(ctx, item.Record)
	case store.ReplicatedCommit:
		return w.t.AppendCommit(ctx, item.Commit)
	default:
		return fmt.Errorf("%w: %q is not something a cluster replicates", zone.ErrInvalid, item.Kind)
	}
}

func (w *restore) zone(ctx context.Context, z *zone.Zone) error {
	if err := checkZone(z); err != nil {
		return err
	}
	if z.CreatedAt.IsZero() {
		return fmt.Errorf("%w: zone %s arrived without the time it was created", zone.ErrInvalid, z.Name)
	}

	prefix, prefixLen := prefixColumns(z.Prefix)
	_, err := w.zones.ExecContext(ctx,
		string(z.ID), z.Name.String(), z.Name.SortKey(), string(z.Kind), prefix, prefixLen,
		z.SOA.NS.String(), z.SOA.Mbox.String(), int64(z.SOA.Serial.Uint32()),
		int64(z.SOA.Refresh), int64(z.SOA.Retry), int64(z.SOA.Expire),
		int64(z.SOA.Minimum), int64(z.SOA.TTL),
		int64(z.DefaultTTL), nullBool(z.AutoReverse), boolToInt(z.Disabled), z.Comment,
		z.CreatedAt.UnixMilli(), z.UpdatedAt.UnixMilli())

	return translate(err, fmt.Sprintf("the snapshot holds zone %s twice", z.Name))
}

func (w *restore) record(ctx context.Context, rec *zone.Record) error {
	if err := checkRecord(rec); err != nil {
		return err
	}
	if rec.CreatedAt.IsZero() {
		return fmt.Errorf("%w: record %s arrived without the time it was created",
			zone.ErrInvalid, rec.ID)
	}

	_, err := w.records.ExecContext(ctx,
		string(rec.ID), string(rec.ZoneID), rec.Name.String(), rec.Name.SortKey(),
		int64(rec.Class), int64(rec.Type), int64(rec.TTL),
		rec.RData.String(), rdataHash(rec.RData), addrColumn(rec),
		nullString(string(rec.ManagedBy)), nullString(string(rec.ManagedKind)),
		rec.Comment, boolToInt(rec.Disabled),
		rec.CreatedAt.UnixMilli(), rec.UpdatedAt.UnixMilli())

	return translate(err, duplicateRecord(rec))
}

// key writes a transfer key back, revoked or not. A revoked one kept its name
// and lost its secret (D28), and the column pair says so together.
func (w *restore) key(ctx context.Context, k *store.TSIGKey) error {
	if k == nil {
		return errors.New("sqlite: no TSIG key given")
	}
	if k.CreatedAt.IsZero() {
		return fmt.Errorf("%w: key %s arrived without the time it was created", zone.ErrInvalid, k.Name)
	}

	_, err := w.t.q.ExecContext(ctx, `
		INSERT INTO tsig_keys (id, name, algorithm, secret, created_at, revoked_at)
		VALUES (?,?,?,?,?,?)`,
		string(k.ID), k.Name.String(), string(k.Algorithm), k.Secret,
		k.CreatedAt.UnixMilli(), nullMillis(k.RevokedAt))

	return translate(err, fmt.Sprintf("the snapshot holds key %s twice", k.Name))
}
