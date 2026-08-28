package apply

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// batchVersion changes only when an older decoder would get the batch wrong.
// Adding a field is not such a change: unknown fields are ignored, which is
// what a rolling upgrade needs (D19).
const batchVersion = 1

// wireBatch is the batch as it travels. Kept apart from the applier's own
// types: those may be refactored, this one is a promise to every other node.
type wireBatch struct {
	Version int          `json:"version"`
	Zones   []wireZone   `json:"zones,omitempty"`
	ZoneOps []wireZoneOp `json:"zoneOps,omitempty"`
	Commits []wireCommit `json:"commits,omitempty"`
}

type wireZoneOp struct {
	Kind   ZoneOpKind     `json:"kind"`
	ZoneID zone.ZoneID    `json:"zoneId"`
	Zone   *wireZoneValue `json:"zone,omitempty"`
}

// wireZoneValue leaves out Kind and Prefix. Both are worked out from the name
// by [zone.NewZone], and a zone rebuilt any other way arrives as a forward one
// however its name reads.
type wireZoneValue struct {
	ID          zone.ZoneID  `json:"id"`
	Name        zone.Name    `json:"name"`
	SOA         wireSOAValue `json:"soa"`
	DefaultTTL  zone.TTL     `json:"defaultTtl"`
	AutoReverse *bool        `json:"autoReverse,omitempty"`
	Disabled    bool         `json:"disabled,omitempty"`
	Comment     string       `json:"comment,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt"`
}

func toWireZoneValue(z *zone.Zone) *wireZoneValue {
	if z == nil {
		return nil
	}
	return &wireZoneValue{
		ID: z.ID, Name: z.Name, SOA: toWireSOA(z.SOA),
		DefaultTTL: z.DefaultTTL, AutoReverse: z.AutoReverse,
		Disabled: z.Disabled, Comment: z.Comment,
		CreatedAt: z.CreatedAt, UpdatedAt: z.UpdatedAt,
	}
}

func (w *wireZoneValue) zone() (*zone.Zone, error) {
	if w == nil {
		return nil, nil
	}
	z, err := zone.NewZone(w.Name, w.SOA.soa())
	if err != nil {
		return nil, err
	}
	z.ID = w.ID
	z.DefaultTTL = w.DefaultTTL
	z.AutoReverse = w.AutoReverse
	z.Disabled = w.Disabled
	z.Comment = w.Comment
	z.CreatedAt, z.UpdatedAt = w.CreatedAt, w.UpdatedAt
	if err := z.Validate(); err != nil {
		return nil, err
	}
	return &z, nil
}

// wireZone is one zone's share of the work.
type wireZone struct {
	ZoneID  zone.ZoneID  `json:"zoneId"`
	Deletes []wireRecord `json:"deletes,omitempty"`
	Updates []wireUpdate `json:"updates,omitempty"`
	Inserts []wireRecord `json:"inserts,omitempty"`

	// Touched are the owner names to re-check, sorted so that encoding the same
	// batch twice gives the same bytes.
	Touched []zone.Name `json:"touched,omitempty"`

	SOA *wireSOA `json:"soa,omitempty"`
}

type wireUpdate struct {
	Before wireRecord `json:"before"`
	After  wireRecord `json:"after"`
}

type wireSOA struct {
	Before wireSOAValue `json:"before"`
	After  wireSOAValue `json:"after"`
}

// wireSOAValue spells the fields out: encoding [zone.SOA] directly would tie
// the wire format to Go field names this package does not own.
type wireSOAValue struct {
	NS      zone.Name   `json:"ns"`
	Mbox    zone.Name   `json:"mbox"`
	Serial  zone.Serial `json:"serial"`
	Refresh zone.TTL    `json:"refresh"`
	Retry   zone.TTL    `json:"retry"`
	Expire  zone.TTL    `json:"expire"`
	Minimum zone.TTL    `json:"minimum"`
	TTL     zone.TTL    `json:"ttl"`
}

func toWireSOA(s zone.SOA) wireSOAValue {
	return wireSOAValue{
		NS: s.NS, Mbox: s.Mbox, Serial: s.Serial,
		Refresh: s.Refresh, Retry: s.Retry, Expire: s.Expire,
		Minimum: s.Minimum, TTL: s.TTL,
	}
}

func (w wireSOAValue) soa() zone.SOA {
	return zone.SOA{
		NS: w.NS, Mbox: w.Mbox, Serial: w.Serial,
		Refresh: w.Refresh, Retry: w.Retry, Expire: w.Expire,
		Minimum: w.Minimum, TTL: w.TTL,
	}
}

// wireRecord carries identity and provenance as well, which is what an event
// cannot (D24).
type wireRecord struct {
	ID     zone.RecordID `json:"id"`
	ZoneID zone.ZoneID   `json:"zoneId"`

	Name  zone.Name   `json:"name"`
	Class zone.Class  `json:"class"`
	Type  zone.RRType `json:"type"`
	TTL   zone.TTL    `json:"ttl"`

	// RData travels as its canonical presentation text and is parsed again on
	// the way in. See [wireRecord.record].
	RData string `json:"rdata"`

	ManagedBy   zone.RecordID    `json:"managedBy,omitempty"`
	ManagedKind zone.ManagedKind `json:"managedKind,omitempty"`

	Comment  string `json:"comment,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type wireCommit struct {
	ID       journal.CommitID `json:"id"`
	ZoneID   zone.ZoneID      `json:"zoneId"`
	ZoneName zone.Name        `json:"zoneName"`

	SerialFrom zone.Serial `json:"serialFrom"`
	SerialTo   zone.Serial `json:"serialTo"`

	Kind      journal.Kind   `json:"kind"`
	Source    journal.Source `json:"source"`
	Actor     string         `json:"actor,omitempty"`
	Comment   string         `json:"comment,omitempty"`
	RevertsTo *zone.Serial   `json:"revertsTo,omitempty"`

	Events    []wireEvent `json:"events,omitempty"`
	CreatedAt time.Time   `json:"createdAt"`
}

type wireEvent struct {
	Seq   int         `json:"seq"`
	Op    journal.Op  `json:"op"`
	Name  zone.Name   `json:"name"`
	Class zone.Class  `json:"class"`
	Type  zone.RRType `json:"type"`
	TTL   zone.TTL    `json:"ttl"`
	RData string      `json:"rdata"`
}

// MarshalJSON implements [json.Marshaler].
func (b *Batch) MarshalJSON() ([]byte, error) {
	w := wireBatch{Version: batchVersion}

	if b.set != nil {
		for _, zid := range b.set.order {
			ch := b.set.byZone[zid]
			if ch.empty() {
				continue
			}
			w.Zones = append(w.Zones, wireZone{
				ZoneID:  zid,
				Deletes: wireRecords(ch.deletes),
				Updates: wireUpdates(ch.updates),
				Inserts: wireRecords(ch.inserts),
				Touched: sortedNames(ch.touched),
				SOA:     wireSOAOf(ch.soa),
			})
		}
	}

	for _, op := range b.Zones {
		w.ZoneOps = append(w.ZoneOps, wireZoneOp{
			Kind: op.Kind, ZoneID: op.ZoneID, Zone: toWireZoneValue(op.Zone),
		})
	}

	for _, c := range b.Commits {
		w.Commits = append(w.Commits, toWireCommit(c))
	}
	return json.Marshal(w)
}

// UnmarshalJSON implements [json.Unmarshaler]. Everything is checked again
// rather than trusted: what comes out the far end of this is a DNS answer.
func (b *Batch) UnmarshalJSON(data []byte) error {
	var w wireBatch
	if err := json.Unmarshal(data, &w); err != nil {
		return fmt.Errorf("%w batch: %w", zone.ErrInvalid, err)
	}
	if w.Version != batchVersion {
		return fmt.Errorf("%w: this is a version %d batch and this build speaks version %d",
			zone.ErrInvalid, w.Version, batchVersion)
	}

	set := &changeSet{}
	for _, wz := range w.Zones {
		if wz.ZoneID == "" {
			return fmt.Errorf("%w: a batch entry names the zone it changes", zone.ErrInvalid)
		}
		ch := &changes{}
		var err error
		if ch.deletes, err = records(wz.Deletes); err != nil {
			return err
		}
		if ch.inserts, err = records(wz.Inserts); err != nil {
			return err
		}
		if ch.updates, err = updates(wz.Updates); err != nil {
			return err
		}
		for _, n := range wz.Touched {
			ch.touch(n)
		}
		if wz.SOA != nil {
			ch.soa = &soaChange{before: wz.SOA.Before.soa(), after: wz.SOA.After.soa()}
		}

		if set.byZone == nil {
			set.byZone = make(map[zone.ZoneID]*changes, len(w.Zones))
			set.zones = make(map[zone.ZoneID]*zone.Zone, len(w.Zones))
		}
		if _, dup := set.byZone[wz.ZoneID]; dup {
			return fmt.Errorf("%w: zone %s appears twice in one batch", zone.ErrInvalid, wz.ZoneID)
		}
		set.byZone[wz.ZoneID] = ch
		set.order = append(set.order, wz.ZoneID)
	}

	ops := make([]ZoneOp, 0, len(w.ZoneOps))
	for _, wo := range w.ZoneOps {
		z, zerr := wo.Zone.zone()
		if zerr != nil {
			return fmt.Errorf("zone %s: %w", wo.ZoneID, zerr)
		}
		if (z == nil) != (wo.Kind == ZoneDelete) {
			return fmt.Errorf("%w: a %s carries the zone it means, and only a delete does not",
				zone.ErrInvalid, wo.Kind)
		}
		if z != nil && z.ID != wo.ZoneID {
			return fmt.Errorf("%w: the operation names zone %s and carries zone %s",
				zone.ErrInvalid, wo.ZoneID, z.ID)
		}
		ops = append(ops, ZoneOp{Kind: wo.Kind, ZoneID: wo.ZoneID, Zone: z})
	}

	commits := make([]*journal.Commit, 0, len(w.Commits))
	for i := range w.Commits {
		c, err := w.Commits[i].commit()
		if err != nil {
			return err
		}
		commits = append(commits, c)
	}

	b.set = set
	b.Zones = ops
	b.Commits = commits
	return nil
}

func wireRecords(rs []zone.Record) []wireRecord {
	if len(rs) == 0 {
		return nil
	}
	out := make([]wireRecord, len(rs))
	for i := range rs {
		out[i] = toWireRecord(&rs[i])
	}
	return out
}

func wireUpdates(us []recordUpdate) []wireUpdate {
	if len(us) == 0 {
		return nil
	}
	out := make([]wireUpdate, len(us))
	for i := range us {
		out[i] = wireUpdate{
			Before: toWireRecord(&us[i].before),
			After:  toWireRecord(&us[i].after),
		}
	}
	return out
}

func wireSOAOf(s *soaChange) *wireSOA {
	if s == nil {
		return nil
	}
	return &wireSOA{Before: toWireSOA(s.before), After: toWireSOA(s.after)}
}

// sortedNames turns the touched set into a stable list.
func sortedNames(set map[zone.Name]struct{}) []zone.Name {
	if len(set) == 0 {
		return nil
	}
	out := make([]zone.Name, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	slices.SortFunc(out, func(a, b zone.Name) int { return a.Compare(b) })
	return out
}

func toWireRecord(r *zone.Record) wireRecord {
	return wireRecord{
		ID:          r.ID,
		ZoneID:      r.ZoneID,
		Name:        r.Name,
		Class:       r.Class,
		Type:        r.Type,
		TTL:         r.TTL,
		RData:       r.RData.String(),
		ManagedBy:   r.ManagedBy,
		ManagedKind: r.ManagedKind,
		Comment:     r.Comment,
		Disabled:    r.Disabled,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// record turns a record back into one, re-parsing the data rather than
// wrapping the text it was handed.
func (w wireRecord) record() (zone.Record, error) {
	rdata, err := rdataFromWire(w.Type, w.Class, w.RData, "record "+string(w.ID))
	if err != nil {
		return zone.Record{}, err
	}
	r := zone.Record{
		ID:          w.ID,
		ZoneID:      w.ZoneID,
		Name:        w.Name,
		Class:       w.Class,
		Type:        w.Type,
		TTL:         w.TTL,
		RData:       rdata,
		ManagedBy:   w.ManagedBy,
		ManagedKind: w.ManagedKind,
		Comment:     w.Comment,
		Disabled:    w.Disabled,
		CreatedAt:   w.CreatedAt,
		UpdatedAt:   w.UpdatedAt,
	}
	if err := r.Validate(); err != nil {
		return zone.Record{}, fmt.Errorf("record %s: %w", w.ID, err)
	}
	if r.ID == "" {
		return zone.Record{}, fmt.Errorf(
			"%w: a replicated record carries its identity, so that every node holds it under the same one",
			zone.ErrInvalid)
	}
	return r, nil
}

// rdataFromWire parses the text and insists it was canonical already (D18). Anything else means the sender and this node disagree about what the
// record says.
func rdataFromWire(t zone.RRType, c zone.Class, text, what string) (zone.RData, error) {
	rdata, err := zone.ParseRData(t, c, text)
	if err != nil {
		return zone.RData{}, fmt.Errorf("%s: %w", what, err)
	}
	if rdata.String() != text {
		return zone.RData{}, fmt.Errorf(
			"%w: %s carries %q, which is not the canonical form %q",
			zone.ErrInvalid, what, text, rdata.String())
	}
	return rdata, nil
}

func records(ws []wireRecord) ([]zone.Record, error) {
	if len(ws) == 0 {
		return nil, nil
	}
	out := make([]zone.Record, len(ws))
	for i := range ws {
		r, err := ws[i].record()
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

func updates(ws []wireUpdate) ([]recordUpdate, error) {
	if len(ws) == 0 {
		return nil, nil
	}
	out := make([]recordUpdate, len(ws))
	for i := range ws {
		before, err := ws[i].Before.record()
		if err != nil {
			return nil, err
		}
		after, err := ws[i].After.record()
		if err != nil {
			return nil, err
		}
		out[i] = recordUpdate{before: before, after: after}
	}
	return out, nil
}

func toWireCommit(c *journal.Commit) wireCommit {
	w := wireCommit{
		ID:         c.ID,
		ZoneID:     c.ZoneID,
		ZoneName:   c.ZoneName,
		SerialFrom: c.SerialFrom,
		SerialTo:   c.SerialTo,
		Kind:       c.Kind,
		Source:     c.Source,
		Actor:      c.Actor,
		Comment:    c.Comment,
		RevertsTo:  c.RevertsTo,
		CreatedAt:  c.CreatedAt,
	}
	if len(c.Events) > 0 {
		w.Events = make([]wireEvent, len(c.Events))
		for i, e := range c.Events {
			w.Events[i] = wireEvent{
				Seq:   e.Seq,
				Op:    e.Op,
				Name:  e.Name,
				Class: e.Class,
				Type:  e.Type,
				TTL:   e.TTL,
				RData: e.RData.String(),
			}
		}
	}
	return w
}

// commit turns a commit back into one. [journal.Commit] already knows what a
// well-formed one is, so the decoder asks it rather than repeating it.
func (w wireCommit) commit() (*journal.Commit, error) {
	c := &journal.Commit{
		ID:         w.ID,
		ZoneID:     w.ZoneID,
		ZoneName:   w.ZoneName,
		SerialFrom: w.SerialFrom,
		SerialTo:   w.SerialTo,
		Kind:       w.Kind,
		Source:     w.Source,
		Actor:      w.Actor,
		Comment:    w.Comment,
		RevertsTo:  w.RevertsTo,
		CreatedAt:  w.CreatedAt,
	}
	if len(w.Events) > 0 {
		c.Events = make([]journal.Event, len(w.Events))
		for i, e := range w.Events {
			rdata, err := rdataFromWire(e.Type, e.Class, e.RData,
				fmt.Sprintf("event %d of commit %s", e.Seq, w.ID))
			if err != nil {
				return nil, err
			}
			c.Events[i] = journal.Event{
				Seq:   e.Seq,
				Op:    e.Op,
				Name:  e.Name,
				Class: e.Class,
				Type:  e.Type,
				TTL:   e.TTL,
				RData: rdata,
			}
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}
