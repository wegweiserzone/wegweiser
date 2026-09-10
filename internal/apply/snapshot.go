package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// snapshotFormat names what a log snapshot is, so that anything else handed to
// a restore is refused for what it is rather than for its first odd byte.
const snapshotFormat = "wegweiser-log-snapshot"

// snapshotVersion changes only when an older reader would restore a snapshot
// wrongly. Adding a field is not such a change: unknown fields are ignored, as
// they are in a batch (D19).
const snapshotVersion = 1

// snapshotEnd marks the end of the content. Without it a snapshot cut off
// between two items would read as a complete one describing a smaller state,
// and restoring it would throw away everything after the cut.
const snapshotEnd = "end"

// SnapshotWriter says what kind of member wrote a log snapshot.
type SnapshotWriter string

const (
	// WriterStore is a member that holds a store, which is every member this
	// server can be.
	WriterStore SnapshotWriter = "store"

	// WriterWitness is a member that keeps the log and applies none of it
	// (docs/decisions/d39-the-witness.md). Its snapshot has no content, and a
	// node that restored one would empty itself.
	WriterWitness SnapshotWriter = "witness"
)

func (w SnapshotWriter) valid() bool { return w == WriterStore || w == WriterWitness }

// SnapshotHeader is what a log snapshot says about itself ahead of its content
// (docs/decisions/d30-what-a-log-snapshot-contains.md).
type SnapshotHeader struct {
	// Index is the position in the replicated log the content is current as
	// of. It travels inside the snapshot because a restore is handed the bytes
	// and nothing else.
	Index uint64

	// Writer is what kind of member wrote it.
	Writer SnapshotWriter
}

// wireSnapshotHeader is the header as it travels. Like the batch, it is a
// promise to every other member and is kept apart from the types above.
type wireSnapshotHeader struct {
	Format  string         `json:"format"`
	Version int            `json:"version"`
	Index   uint64         `json:"index"`
	Writer  SnapshotWriter `json:"writer"`
}

// wireItem is one item of the content, or the mark that there is no more.
type wireItem struct {
	Kind    string         `json:"kind"`
	Setting *wireSetting   `json:"setting,omitempty"`
	Token   *wireToken     `json:"token,omitempty"`
	Key     *wireKey       `json:"key,omitempty"`
	Zone    *wireZoneValue `json:"zone,omitempty"`
	Record  *wireRecord    `json:"record,omitempty"`
	Commit  *wireCommit    `json:"commit,omitempty"`

	// Count is carried by the end mark alone: how many items came before it.
	Count uint64 `json:"count,omitempty"`
}

// WriteSnapshot writes a log snapshot: the header, one line per item, and a
// mark saying the content is complete. Items may be nil, which is what a
// witness writes.
func WriteSnapshot(w io.Writer, h SnapshotHeader, items iter.Seq2[*store.Replicated, error]) error {
	if !h.Writer.valid() {
		return fmt.Errorf("%w: %q is not a kind of member that writes log snapshots",
			zone.ErrInvalid, h.Writer)
	}

	enc := json.NewEncoder(w)
	if err := enc.Encode(wireSnapshotHeader{
		Format: snapshotFormat, Version: snapshotVersion, Index: h.Index, Writer: h.Writer,
	}); err != nil {
		return fmt.Errorf("write the log snapshot header: %w", err)
	}

	var count uint64
	if items != nil {
		for item, err := range items {
			if err != nil {
				return err
			}
			wi, werr := toWireItem(item)
			if werr != nil {
				return werr
			}
			if eerr := enc.Encode(wi); eerr != nil {
				return fmt.Errorf("write item %d of the log snapshot: %w", count+1, eerr)
			}
			count++
		}
	}
	if err := enc.Encode(wireItem{Kind: snapshotEnd, Count: count}); err != nil {
		return fmt.Errorf("write the end of the log snapshot: %w", err)
	}
	return nil
}

// ReadSnapshot reads a log snapshot's header, and returns it with the content
// still to come. The header is read at once so that a caller can refuse the
// snapshot before touching anything; the content can be ranged over once.
//
// Everything is checked again rather than trusted, as a batch is: what comes
// out of this ends up as a DNS answer.
func ReadSnapshot(r io.Reader) (SnapshotHeader, iter.Seq2[*store.Replicated, error], error) {
	dec := json.NewDecoder(r)

	var wh wireSnapshotHeader
	if err := dec.Decode(&wh); err != nil {
		return SnapshotHeader{}, nil, fmt.Errorf("%w: the log snapshot has no header that can be read: %w",
			zone.ErrInvalid, err)
	}
	switch {
	case wh.Format != snapshotFormat:
		return SnapshotHeader{}, nil, fmt.Errorf("%w: this is not a log snapshot", zone.ErrInvalid)
	case wh.Version != snapshotVersion:
		return SnapshotHeader{}, nil, fmt.Errorf(
			"%w: this is a version %d log snapshot and this build reads version %d",
			zone.ErrInvalid, wh.Version, snapshotVersion)
	case !wh.Writer.valid():
		return SnapshotHeader{}, nil, fmt.Errorf(
			"%w: the log snapshot was written by a %q, which this build does not know",
			zone.ErrInvalid, wh.Writer)
	}

	h := SnapshotHeader{Index: wh.Index, Writer: wh.Writer}
	return h, func(yield func(*store.Replicated, error) bool) {
		var seen uint64
		for {
			var wi wireItem
			if err := dec.Decode(&wi); err != nil {
				if errors.Is(err, io.EOF) {
					err = fmt.Errorf("%w: the log snapshot stops after %d items without saying it is complete",
						zone.ErrInvalid, seen)
				} else {
					err = fmt.Errorf("read item %d of the log snapshot: %w", seen+1, err)
				}
				yield(nil, err)
				return
			}

			if wi.Kind == snapshotEnd {
				if wi.Count != seen {
					yield(nil, fmt.Errorf("%w: the log snapshot says it holds %d items and held %d",
						zone.ErrInvalid, wi.Count, seen))
					return
				}
				var after json.RawMessage
				if err := dec.Decode(&after); !errors.Is(err, io.EOF) {
					yield(nil, fmt.Errorf("%w: the log snapshot carries something after its end",
						zone.ErrInvalid))
				}
				return
			}

			item, err := wi.replicated()
			if err != nil {
				yield(nil, fmt.Errorf("item %d of the log snapshot: %w", seen+1, err))
				return
			}
			seen++
			if !yield(item, nil) {
				return
			}
		}
	}, nil
}

func toWireItem(item *store.Replicated) (wireItem, error) {
	if err := item.Validate(); err != nil {
		return wireItem{}, err
	}

	out := wireItem{Kind: string(item.Kind)}
	switch item.Kind {
	case store.ReplicatedSetting:
		out.Setting = &wireSetting{Key: item.Setting.Key, Value: json.RawMessage(item.Setting.Value)}
	case store.ReplicatedToken:
		out.Token = toWireToken(item.Token)
	case store.ReplicatedTSIGKey:
		out.Key = toWireKey(item.TSIGKey)
	case store.ReplicatedZone:
		out.Zone = toWireZoneValue(item.Zone)
	case store.ReplicatedRecord:
		r := toWireRecord(item.Record)
		out.Record = &r
	case store.ReplicatedCommit:
		c := toWireCommit(item.Commit)
		out.Commit = &c
	default:
		return wireItem{}, fmt.Errorf("%w: %q is not something a log snapshot carries",
			zone.ErrInvalid, item.Kind)
	}
	return out, nil
}

// replicated turns an item back into one.
//
// A kind this build does not know is refused rather than skipped. A field is
// additive and an older reader may pass over it, but a whole kind of content
// is data a restored node would silently not have (D30).
func (w wireItem) replicated() (*store.Replicated, error) {
	carried := 0
	for _, set := range []bool{
		w.Setting != nil, w.Token != nil, w.Key != nil,
		w.Zone != nil, w.Record != nil, w.Commit != nil,
	} {
		if set {
			carried++
		}
	}
	if carried != 1 {
		return nil, fmt.Errorf("%w: an item of kind %q carries %d things, and an item carries one",
			zone.ErrInvalid, w.Kind, carried)
	}

	kind := store.ReplicatedKind(w.Kind)
	out := &store.Replicated{Kind: kind}
	var err error
	switch kind {
	case store.ReplicatedSetting:
		if w.Setting != nil {
			if err = w.Setting.check(); err == nil {
				out.Setting = &store.Setting{Key: w.Setting.Key, Value: []byte(w.Setting.Value)}
			}
		}
	case store.ReplicatedToken:
		out.Token = w.Token.token()
	case store.ReplicatedTSIGKey:
		out.TSIGKey, err = w.Key.key()
	case store.ReplicatedZone:
		out.Zone, err = w.Zone.zone()
	case store.ReplicatedRecord:
		if w.Record != nil {
			var r zone.Record
			if r, err = w.Record.record(); err == nil {
				out.Record = &r
			}
		}
	case store.ReplicatedCommit:
		if w.Commit != nil {
			out.Commit, err = w.Commit.commit()
		}
	default:
		return nil, fmt.Errorf("%w: an item of kind %q, which this build does not know",
			zone.ErrInvalid, w.Kind)
	}
	if err != nil {
		return nil, err
	}
	if verr := out.Validate(); verr != nil {
		return nil, verr
	}
	return out, nil
}
