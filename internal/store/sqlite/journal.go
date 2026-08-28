package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

const commitColumns = `
	id, zone_id, zone_name, serial_from, serial_to, kind, source, actor, comment,
	reverts_to, created_at`

// CommitByID returns one commit with its events.
func (r reader) CommitByID(ctx context.Context, cid journal.CommitID) (*journal.Commit, error) {
	row := r.q.QueryRowContext(ctx,
		`SELECT`+commitColumns+` FROM journal_commits WHERE id = ?`, string(cid))

	c, err := scanCommit(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("commit with the identifier", cid)
	}
	if err != nil {
		return nil, err
	}
	if c.Events, err = r.events(ctx, cid); err != nil {
		return nil, err
	}
	return c, nil
}

// ListCommits returns one page of commit metadata, newest first.
func (r reader) ListCommits(ctx context.Context, f store.CommitFilter) (store.Page[*journal.Commit], error) {
	var page store.Page[*journal.Commit]

	after, err := decodeCursor(f.Cursor, cursorCommits)
	if err != nil {
		return page, err
	}

	var w where
	if after.ID != "" {
		w.add(`(created_at, id) < (?, ?)`, after.Millis, after.ID)
	}
	if f.ZoneID != "" {
		w.add(`zone_id = ?`, string(f.ZoneID))
	}
	if len(f.Kinds) > 0 {
		marks := make([]string, len(f.Kinds))
		args := make([]any, len(f.Kinds))
		for i, k := range f.Kinds {
			marks[i] = "?"
			args[i] = string(k)
		}
		w.add(`kind IN (`+strings.Join(marks, ",")+`)`, args...)
	}
	if f.Actor != "" {
		w.add(`actor = ?`, f.Actor)
	}
	if !f.Since.IsZero() {
		w.add(`created_at >= ?`, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		w.add(`created_at < ?`, f.Until.UnixMilli())
	}

	limit := f.EffectiveLimit()
	rows, err := r.q.QueryContext(ctx,
		`SELECT`+commitColumns+` FROM journal_commits`+w.clause()+
			` ORDER BY created_at DESC, id DESC LIMIT ?`,
		append(w.args, limit+1)...)
	if err != nil {
		return page, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	commits := make([]*journal.Commit, 0, limit)
	for rows.Next() {
		c, serr := scanCommit(rows)
		if serr != nil {
			return page, serr
		}
		commits = append(commits, c)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}

	if len(commits) > limit {
		last := commits[limit-1]
		commits = commits[:limit]
		page.NextCursor = cursor{
			Kind:   cursorCommits,
			Millis: last.CreatedAt.UnixMilli(),
			ID:     string(last.ID),
		}.encode()
	}
	page.Items = commits
	return page, nil
}

// events reads one commit's events in their recorded order.
func (r reader) events(ctx context.Context, cid journal.CommitID) (_ []journal.Event, err error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT seq, op, name, class, rrtype, ttl, rdata FROM journal_events
		 WHERE commit_id = ? ORDER BY seq`, string(cid))
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	var out []journal.Event
	for rows.Next() {
		var (
			e          journal.Event
			op         string
			name       string
			class, typ int64
			ttl        int64
			rdata      string
		)
		if err := rows.Scan(&e.Seq, &op, &name, &class, &typ, &ttl, &rdata); err != nil {
			return nil, err
		}

		var perr error
		e.Op = journal.Op(op)
		if e.Name, perr = zone.ParseName(name); perr != nil {
			return nil, corrupt("journal_events", string(cid), "name", perr)
		}
		if e.RData, perr = zone.RDataFromCanonical(rdata); perr != nil {
			return nil, corrupt("journal_events", string(cid), "rdata", perr)
		}
		n := narrow{table: "journal_events", rowID: string(cid)}
		e.Class = zone.Class(n.u16("class", class))
		e.Type = zone.RRType(n.u16("rrtype", typ))
		e.TTL = zone.TTL(n.u32("ttl", ttl))
		if n.err != nil {
			return nil, n.err
		}

		out = append(out, e)
	}
	return out, rows.Err()
}

// AppendCommit stores a commit and its events.
func (t *txn) AppendCommit(ctx context.Context, c *journal.Commit) error {
	if c == nil {
		return errors.New("sqlite: no commit given")
	}
	if err := c.Validate(); err != nil {
		return err
	}

	if c.CreatedAt.IsZero() {
		c.CreatedAt = t.stamp()
	} else {
		c.CreatedAt = fromMillis(c.CreatedAt.UnixMilli())
	}

	_, err := t.q.ExecContext(ctx, `
		INSERT INTO journal_commits (
			id, zone_id, zone_name, serial_from, serial_to, kind, source, actor, comment,
			reverts_to, event_count, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(c.ID), string(c.ZoneID), c.ZoneName.String(),
		int64(c.SerialFrom.Uint32()), int64(c.SerialTo.Uint32()),
		string(c.Kind), string(c.Source), c.Actor, c.Comment,
		revertsToColumn(c.RevertsTo), len(c.Events), c.CreatedAt.UnixMilli())
	if err != nil {
		return translate(err, fmt.Sprintf(
			"zone %s already has a commit producing serial %s; a serial names one state, so it "+
				"can only be reached once", c.ZoneID, c.SerialTo))
	}

	return t.appendEvents(ctx, c)
}

// appendEvents writes a commit's events through one prepared statement. An
// import can carry hundreds of thousands of them.
func (t *txn) appendEvents(ctx context.Context, c *journal.Commit) (err error) {
	if len(c.Events) == 0 {
		return nil
	}

	stmt, err := t.q.PrepareContext(ctx, `
		INSERT INTO journal_events (commit_id, seq, op, name, class, rrtype, ttl, rdata)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, stmt.Close()) }()

	for i := range c.Events {
		e := &c.Events[i]
		if _, err := stmt.ExecContext(ctx,
			string(c.ID), e.Seq, string(e.Op), e.Name.String(),
			int64(e.Class), int64(e.Type), int64(e.TTL), e.RData.String(),
		); err != nil {
			return err
		}
	}
	return nil
}

// scanCommit reads one row of [commitColumns], without its events.
func scanCommit(row scannable) (*journal.Commit, error) {
	var (
		c         journal.Commit
		cid, zid  string
		zoneName  string
		from, to  int64
		kind      string
		source    string
		revertsTo sql.NullInt64
		created   int64
	)

	if err := row.Scan(&cid, &zid, &zoneName, &from, &to, &kind, &source,
		&c.Actor, &c.Comment, &revertsTo, &created); err != nil {
		return nil, err
	}

	c.ID = journal.CommitID(cid)
	c.ZoneID = zone.ZoneID(zid)
	var nerr error
	if c.ZoneName, nerr = zone.ParseName(zoneName); nerr != nil {
		return nil, corrupt("journal_commits", cid, "zone_name", nerr)
	}
	n := narrow{table: "journal_commits", rowID: cid}
	c.SerialFrom = zone.NewSerial(n.u32("serial_from", from))
	c.SerialTo = zone.NewSerial(n.u32("serial_to", to))
	c.Kind = journal.Kind(kind)
	c.Source = journal.Source(source)
	if revertsTo.Valid {
		s := zone.NewSerial(n.u32("reverts_to", revertsTo.Int64))
		c.RevertsTo = &s
	}
	if n.err != nil {
		return nil, n.err
	}
	c.CreatedAt = fromMillis(created)

	return &c, nil
}

func revertsToColumn(s *zone.Serial) any {
	if s == nil {
		return nil
	}
	return int64(s.Uint32())
}
