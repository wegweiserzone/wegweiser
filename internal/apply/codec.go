package apply

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// batchVersion changes only when an older decoder would get the batch wrong.
// Adding a field is not such a change: unknown fields are ignored, which is
// what a rolling upgrade needs (D19).
const batchVersion = 1

// wireBatch is the batch as it travels. Kept apart from the applier's own
// types: those may be refactored, this one is a promise to every other node.
type wireBatch struct {
	Version  int           `json:"version"`
	Zones    []wireZone    `json:"zones,omitempty"`
	ZoneOps  []wireZoneOp  `json:"zoneOps,omitempty"`
	Commits  []wireCommit  `json:"commits,omitempty"`
	Settings []wireSetting `json:"settings,omitempty"`
	Keys     []wireKeyOp   `json:"keys,omitempty"`
	Tokens   []wireTokenOp `json:"tokens,omitempty"`
}

// wireTokenOp is one change to an API token as it travels.
type wireTokenOp struct {
	Kind    TokenOpKind   `json:"kind"`
	Token   *wireToken    `json:"token,omitempty"`
	TokenID store.TokenID `json:"tokenId,omitempty"`
	At      *time.Time    `json:"at,omitempty"`
}

// wireToken is a token as it travels. The hash rather than the secret, which
// this server never held (D5). LastUsedAt is left out: it is node-local and
// named as such by D24.
type wireToken struct {
	ID        store.TokenID `json:"id"`
	Name      string        `json:"name"`
	Prefix    string        `json:"prefix"`
	Hash      []byte        `json:"hash"`
	Scopes    []string      `json:"scopes"`
	CreatedAt time.Time     `json:"createdAt"`
	ExpiresAt time.Time     `json:"expiresAt,omitzero"`
	RevokedAt time.Time     `json:"revokedAt,omitzero"`
}

// wireKeyOp is one change to a transfer key as it travels.
type wireKeyOp struct {
	Kind  KeyOpKind       `json:"kind"`
	Key   *wireKey        `json:"key,omitempty"`
	KeyID store.TSIGKeyID `json:"keyId,omitempty"`
	At    *time.Time      `json:"at,omitempty"`
}

// wireKey is a transfer key as it travels. The secret is base64 in JSON, which
// is what encoding/json does with octets and what an operator pastes anyway.
type wireKey struct {
	ID        store.TSIGKeyID    `json:"id"`
	Name      zone.Name          `json:"name"`
	Algorithm zone.TSIGAlgorithm `json:"algorithm"`
	Secret    []byte             `json:"secret"`
	CreatedAt time.Time          `json:"createdAt"`
}

// wireSetting is one server setting as it travels. The value is carried as the
// JSON it already is rather than re-encoded, so what a node stores is the bytes
// the node that planned it stored.
type wireSetting struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
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

	for _, c := range b.Settings {
		w.Settings = append(w.Settings, wireSetting{Key: c.Key, Value: c.Value})
	}

	for _, op := range b.Keys {
		out := wireKeyOp{Kind: op.Kind, KeyID: op.KeyID}
		if op.Key != nil {
			out.Key = &wireKey{
				ID: op.Key.ID, Name: op.Key.Name, Algorithm: op.Key.Algorithm,
				Secret: op.Key.Secret, CreatedAt: op.Key.CreatedAt,
			}
		}
		if !op.At.IsZero() {
			at := op.At
			out.At = &at
		}
		w.Keys = append(w.Keys, out)
	}

	for _, op := range b.Tokens {
		out := wireTokenOp{Kind: op.Kind, TokenID: op.TokenID}
		if op.Token != nil {
			out.Token = &wireToken{
				ID: op.Token.ID, Name: op.Token.Name, Prefix: op.Token.Prefix,
				Hash: op.Token.Hash, Scopes: op.Token.Scopes,
				CreatedAt: op.Token.CreatedAt, ExpiresAt: op.Token.ExpiresAt,
				RevokedAt: op.Token.RevokedAt,
			}
		}
		if !op.At.IsZero() {
			at := op.At
			out.At = &at
		}
		w.Tokens = append(w.Tokens, out)
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

	settings := make([]SettingChange, 0, len(w.Settings))
	for _, ws := range w.Settings {
		if ws.Key == "" {
			return fmt.Errorf("%w: a setting in the batch names no setting", zone.ErrInvalid)
		}
		if !json.Valid(ws.Value) {
			return fmt.Errorf("%w: the value for the setting %q is not JSON",
				zone.ErrInvalid, ws.Key)
		}
		settings = append(settings, SettingChange{Key: ws.Key, Value: ws.Value})
	}

	keys := make([]KeyOp, 0, len(w.Keys))
	for _, wo := range w.Keys {
		op := KeyOp{Kind: wo.Kind, KeyID: wo.KeyID}
		if wo.Key != nil {
			op.Key = &store.TSIGKey{
				ID: wo.Key.ID, Name: wo.Key.Name, Algorithm: wo.Key.Algorithm,
				Secret: wo.Key.Secret, CreatedAt: wo.Key.CreatedAt,
			}
		}
		if wo.At != nil {
			op.At = *wo.At
		}
		keys = append(keys, op)
	}

	b.set = set
	b.Zones = ops
	b.Commits = commits
	tokens := make([]TokenOp, 0, len(w.Tokens))
	for _, wo := range w.Tokens {
		op := TokenOp{Kind: wo.Kind, TokenID: wo.TokenID}
		if wo.Token != nil {
			op.Token = &store.Token{
				ID: wo.Token.ID, Name: wo.Token.Name, Prefix: wo.Token.Prefix,
				Hash: wo.Token.Hash, Scopes: wo.Token.Scopes,
				CreatedAt: wo.Token.CreatedAt, ExpiresAt: wo.Token.ExpiresAt,
				RevokedAt: wo.Token.RevokedAt,
			}
		}
		if wo.At != nil {
			op.At = *wo.At
		}
		tokens = append(tokens, op)
	}

	b.Settings = settings
	b.Keys = keys
	b.Tokens = tokens
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
