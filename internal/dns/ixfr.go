package dns

import (
	"context"
	"fmt"
	"strings"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// History is where an incremental transfer reads what a zone has done.
//
// The query path may not reach the database (invariant 2), so this is the seam
// docs/decisions/ D26 leaves for the one thing a transfer needs that no snapshot
// holds: the record changes between two serials.
type History interface {
	// Since returns the commits that took the zone from one serial to another,
	// oldest first, with the first starting at from and the last ending at to.
	//
	// The upper serial is the one the snapshot holds, not the newest the
	// journal has. The two differ for as long as a write takes to publish, and
	// a transfer that announced the newer one would have a secondary serve a
	// state this server does not.
	//
	// A range the history does not cover is false rather than an error: a full
	// transfer answers an incremental request correctly (RFC 1995 §2), so not
	// having the difference is a fallback, not a fault.
	Since(
		ctx context.Context, apex zone.Name, from, to zone.Serial,
	) ([]*journal.Commit, bool, error)
}

// clientSerial is the version the client says it holds, which RFC 1995 §3 puts
// in a single start of authority in the request's authority section.
func clientSerial(req *wire.Msg) (zone.Serial, bool) {
	if len(req.Ns) != 1 {
		return zone.Serial{}, false
	}
	soa, ok := req.Ns[0].(*wire.SOA)
	if !ok {
		return zone.Serial{}, false
	}
	return zone.NewSerial(soa.Serial), true
}

// ixfr answers a request for what has changed since the client's version.
//
// Every reason not to send a difference is answered with the whole zone, which
// RFC 1995 §2 allows and which is never wrong, only larger.
func (s *Server) ixfr(t *zoneTree, since zone.Serial, w *transferWriter) error {
	to := t.soa.Serial

	// RFC 1982 §3.2 leaves serials half the space apart unordered, and guessing
	// which side the client is on could send it backwards.
	if !since.Comparable(to) {
		return t.axfr(w)
	}
	if !since.Before(to) {
		// RFC 1995 §2: a client already at this version, or ahead of it, is
		// told the version and nothing else.
		soa, err := t.apexSOA()
		if err != nil {
			return err
		}
		if aerr := w.add(soa); aerr != nil {
			return aerr
		}
		return w.flush()
	}

	if s.cfg.History == nil {
		return t.axfr(w)
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.TCPIdleTimeout)
	defer cancel()

	commits, ok, err := s.cfg.History.Since(ctx, t.name, since, to)
	if err != nil {
		// The client still gets a correct zone, only a whole one. The operator
		// hears about it, because a history that cannot be read is a fault even
		// where the transfer it was wanted for succeeds anyway.
		s.report(fmt.Errorf("read the history of %s for an incremental transfer: %w", t.name, err))
		return t.axfr(w)
	}
	if !ok || !worthSending(commits, t.count) {
		return t.axfr(w)
	}

	steps, ok, err := t.steps(commits)
	if err != nil {
		return err
	}
	if !ok {
		return t.axfr(w)
	}
	return t.differences(steps, w)
}

// worthSending reports whether the difference is smaller than the zone it
// describes. RFC 1995 §4 asks for the whole zone where it is not, counted here
// in records, which is what both answers are made of.
func worthSending(commits []*journal.Commit, records int) bool {
	n := 0
	for _, c := range commits {
		// Two for the pair of start-of-authority records that frames the step.
		n += len(c.Events) + 2
	}
	// A whole zone is its records with the start of authority sent a second
	// time to close it (RFC 5936 §2.2).
	whole := records + 1
	return n < whole
}

// step is one difference sequence: the zone's start of authority before a
// commit and after it, and the records the commit removed and added.
type step struct {
	before, after wire.RR
	del, add      []wire.RR
}

// steps turns a range of commits into the difference sequences RFC 1995 §4
// sends.
//
// It walks backwards from the start of authority the snapshot holds, because
// that is the only one this server knows to be true: an ordinary edit steps the
// serial without writing an SOA to the journal, so the version a step begins at
// is reconstructed rather than read. A range whose commits do not join up is
// reported as false, which sends the whole zone instead of a difference nobody
// can trust.
func (t *zoneTree) steps(commits []*journal.Commit) ([]step, bool, error) {
	out := make([]step, len(commits))
	after := t.soa
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		if c.SerialTo != after.Serial {
			return nil, false, nil
		}

		before, err := soaBefore(c, t.name, after)
		if err != nil {
			return nil, false, err
		}
		del, add, cerr := changed(c, t.name)
		if cerr != nil {
			return nil, false, cerr
		}
		beforeRR, berr := soaRRFor(t.name, before.TTL, before)
		if berr != nil {
			return nil, false, berr
		}
		afterRR, aerr := soaRRFor(t.name, after.TTL, after)
		if aerr != nil {
			return nil, false, aerr
		}

		out[i] = step{before: beforeRR, after: afterRR, del: del, add: add}
		after = before
	}
	return out, true, nil
}

// soaBefore is the start of authority the zone held going into the commit: the
// one the commit recorded if it changed it, and otherwise the one it came out
// with at the serial it came in at.
func soaBefore(c *journal.Commit, apex zone.Name, after zone.SOA) (zone.SOA, error) {
	for i := range c.Events {
		e := &c.Events[i]
		if e.Op != journal.OpDel || e.Type != zone.TypeSOA || !e.Name.Equal(apex) {
			continue
		}
		soa, err := zone.ParseSOAData(e.RData.String())
		if err != nil {
			return zone.SOA{}, fmt.Errorf(
				"the recorded start of authority of %s at serial %s: %w", apex, c.SerialFrom, err)
		}
		soa.TTL = e.TTL
		return soa, nil
	}
	before := after
	before.Serial = c.SerialFrom
	return before, nil
}

// changed renders a commit's record events, leaving out the start of authority:
// the SOA frames a difference sequence rather than sitting inside one.
func changed(c *journal.Commit, apex zone.Name) (del, add []wire.RR, err error) {
	for i := range c.Events {
		e := &c.Events[i]
		if e.Type == zone.TypeSOA && e.Name.Equal(apex) {
			continue
		}
		rr, rerr := eventRR(e)
		if rerr != nil {
			return nil, nil, rerr
		}
		if e.Op == journal.OpDel {
			del = append(del, rr)
		} else {
			add = append(add, rr)
		}
	}
	return del, add, nil
}

// differences streams the answer RFC 1995 §4 lays out: the current version,
// then each step opening with the version it removes and closing with the one
// it adds, then the current version again.
func (t *zoneTree) differences(steps []step, w *transferWriter) error {
	current, err := t.apexSOA()
	if err != nil {
		return err
	}
	if aerr := w.add(current); aerr != nil {
		return aerr
	}
	for i := range steps {
		s := &steps[i]
		if aerr := w.add(s.before); aerr != nil {
			return aerr
		}
		for _, rr := range s.del {
			if aerr := w.add(rr); aerr != nil {
				return aerr
			}
		}
		if aerr := w.add(s.after); aerr != nil {
			return aerr
		}
		for _, rr := range s.add {
			if aerr := w.add(rr); aerr != nil {
				return aerr
			}
		}
	}
	if aerr := w.add(current); aerr != nil {
		return aerr
	}
	return w.flush()
}

// eventRR renders a journal event's record for the wire, the way [recordRR]
// renders a stored one and for the same reason: the text came out of this
// library on the way in, so it parses back to what it was (D18).
func eventRR(e *journal.Event) (wire.RR, error) {
	var b strings.Builder
	b.WriteString(e.Name.String())
	b.WriteByte('\t')
	b.WriteString(e.TTL.String())
	b.WriteByte('\t')
	b.WriteString(e.Class.String())
	b.WriteByte('\t')
	b.WriteString(e.Type.String())
	b.WriteByte('\t')
	b.WriteString(e.RData.String())

	rr, err := wire.NewRR(b.String())
	if err != nil {
		return nil, fmt.Errorf("journal event %q: %w", b.String(), err)
	}
	if rr == nil {
		return nil, fmt.Errorf("journal event %q: parsed to nothing", b.String())
	}
	return rr, nil
}
