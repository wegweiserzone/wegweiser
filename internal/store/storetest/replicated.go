package storetest

import (
	"errors"
	"iter"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// fill puts one of everything a cluster replicates into a store, so that an
// export has something of each kind to carry.
func fill(t *testing.T, s store.Store) {
	t.Helper()

	forward := createZone(t, s, "example.com.")
	reverse := createZone(t, s, "2.0.192.in-addr.arpa.")

	source := createRecord(t, s, forward.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
	generated := newRecord(t, reverse.ID, "10.2.0.192.in-addr.arpa.", zone.TypePTR, "www.example.com.")
	generated.ManagedBy = source.ID
	generated.ManagedKind = zone.ManagedPTR

	active := newTSIGKey("transfer.example.com.", zone.HMACSHA256)
	retired := newTSIGKey("old.example.com.", zone.HMACSHA256)

	kept := &store.Token{
		ID: newID[store.TokenID](), Name: "deploy", Prefix: "wgw_dep",
		Hash: make([]byte, 32), Scopes: []string{"zones:write"},
	}
	kept.Hash[0] = 1
	withdrawn := &store.Token{
		ID: newID[store.TokenID](), Name: "retired", Prefix: "wgw_ret",
		Hash: make([]byte, 32), Scopes: []string{"zones:read"},
	}
	withdrawn.Hash[0] = 2

	at := time.Now().UTC().Truncate(time.Millisecond)
	mustUpdate(t, s, func(tx store.Tx) error {
		return errors.Join(
			tx.InsertRecord(ctxOf(t), generated),
			tx.CreateTSIGKey(ctxOf(t), active),
			tx.CreateTSIGKey(ctxOf(t), retired),
			tx.RevokeTSIGKey(ctxOf(t), retired.ID, at),
			tx.CreateToken(ctxOf(t), kept),
			tx.CreateToken(ctxOf(t), withdrawn),
			tx.RevokeToken(ctxOf(t), withdrawn.ID, at),
			// Node-local, and the export has to drop it (D24).
			tx.TouchToken(ctxOf(t), kept.ID, at),
			tx.PutSetting(ctxOf(t), "reverse.conflicts", []byte(`"first-wins"`)),
			tx.PutSetting(ctxOf(t), "transfer.allow", []byte(`["192.0.2.53"]`)),
			tx.AppendCommit(ctxOf(t), newCommit(forward, 1, journal.KindEdit,
				event(0, journal.OpAdd, "www.example.com.", zone.TypeA, "192.0.2.10"))),
			tx.AppendCommit(ctxOf(t), newCommit(reverse, 1, journal.KindEdit,
				event(0, journal.OpAdd, "10.2.0.192.in-addr.arpa.", zone.TypePTR, "www.example.com."))),
		)
	})
}

// exportAll drains an export inside one read transaction, which is where it is
// meant to run: outside one the stream would describe several states.
func exportAll(t *testing.T, s store.Store) []*store.Replicated {
	t.Helper()

	var out []*store.Replicated
	err := s.View(ctxOf(t), func(r store.Reader) error {
		for item, ierr := range r.ExportReplicated(ctxOf(t)) {
			if ierr != nil {
				return ierr
			}
			out = append(out, item)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExportReplicated: %v", err)
	}
	return out
}

// streamOf hands a slice back as the stream a restore takes.
func streamOf(items []*store.Replicated) iter.Seq2[*store.Replicated, error] {
	return func(yield func(*store.Replicated, error) bool) {
		for _, item := range items {
			if !yield(item, nil) {
				return
			}
		}
	}
}

// kindsOf renders a stream for a failure message.
func kindsOf(items []*store.Replicated) []store.ReplicatedKind {
	out := make([]store.ReplicatedKind, len(items))
	for i, item := range items {
		out[i] = item.Kind
	}
	return out
}

// sameExport fails unless two exports describe the same content item for item.
func sameExport(t *testing.T, got, want []*store.Replicated) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("the export holds %d items, want %d\n got: %v\nwant: %v",
			len(got), len(want), kindsOf(got), kindsOf(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("item %d of the export is a %s that came back changed:\n got: %+v\nwant: %+v",
				i, want[i].Kind, got[i], want[i])
		}
	}
}

func testReplicatedExport(t *testing.T, open Open) {
	t.Run("an empty store exports nothing", func(t *testing.T) {
		t.Parallel()

		if items := exportAll(t, open(t)); len(items) != 0 {
			t.Errorf("an untouched store exported %v, want nothing", kindsOf(items))
		}
	})

	t.Run("everything replicated is carried, grouped by kind", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		fill(t, s)

		var seen []store.ReplicatedKind
		for _, item := range exportAll(t, s) {
			if len(seen) == 0 || seen[len(seen)-1] != item.Kind {
				seen = append(seen, item.Kind)
			}
		}

		want := []store.ReplicatedKind{
			store.ReplicatedSetting, store.ReplicatedToken, store.ReplicatedTSIGKey,
			store.ReplicatedZone, store.ReplicatedRecord, store.ReplicatedCommit,
		}
		if !slices.Equal(seen, want) {
			t.Errorf("the export runs %v, want %v", seen, want)
		}
	})

	t.Run("a commit carries its events", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		fill(t, s)

		for _, item := range exportAll(t, s) {
			if item.Kind != store.ReplicatedCommit {
				continue
			}
			if len(item.Commit.Events) != 1 {
				t.Errorf("commit %s exported %d events, want 1",
					item.Commit.ID, len(item.Commit.Events))
			}
		}
	})

	// A token's last use is the one node-local field living on a replicated
	// row. It says when the token was used *here*, which is nothing to tell
	// another node (D24).
	t.Run("when a token was last used stays at home", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		fill(t, s)

		var tokens int
		for _, item := range exportAll(t, s) {
			if item.Kind != store.ReplicatedToken {
				continue
			}
			tokens++
			if !item.Token.LastUsedAt.IsZero() {
				t.Errorf("token %s travels with a last use of %s, want none",
					item.Token.Name, item.Token.LastUsedAt)
			}
		}
		if tokens != 2 {
			t.Errorf("the export carried %d tokens, want 2", tokens)
		}
	})

	// The revoked half of each pair is the one that would be lost by an export
	// written around the methods that create them: one refuses a key with no
	// secret, and a revoked key is exactly that.
	t.Run("a revoked token and a revoked key are carried too", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		fill(t, s)

		var revokedTokens, revokedKeys int
		for _, item := range exportAll(t, s) {
			if item.Kind == store.ReplicatedToken && !item.Token.RevokedAt.IsZero() {
				revokedTokens++
			}
			if item.Kind == store.ReplicatedTSIGKey && !item.TSIGKey.RevokedAt.IsZero() {
				revokedKeys++
			}
		}
		if revokedTokens != 1 || revokedKeys != 1 {
			t.Errorf("the export carried %d revoked tokens and %d revoked keys, want 1 of each",
				revokedTokens, revokedKeys)
		}
	})
}

func testReplicatedRestore(t *testing.T, open Open) {
	t.Run("a round trip puts the same content back", func(t *testing.T) {
		t.Parallel()

		from := open(t)
		fill(t, from)
		want := exportAll(t, from)

		into := open(t)
		if err := into.ReplaceReplicated(ctxOf(t), 42, streamOf(want)); err != nil {
			t.Fatalf("ReplaceReplicated: %v", err)
		}
		sameExport(t, exportAll(t, into), want)
	})

	t.Run("the log index arrives with the content", func(t *testing.T) {
		t.Parallel()

		from := open(t)
		fill(t, from)

		into := open(t)
		if err := into.ReplaceReplicated(ctxOf(t), 9001, streamOf(exportAll(t, from))); err != nil {
			t.Fatalf("ReplaceReplicated: %v", err)
		}

		got, err := into.AppliedIndex(ctxOf(t))
		if err != nil {
			t.Fatalf("AppliedIndex: %v", err)
		}
		if got != 9001 {
			t.Errorf("AppliedIndex = %d after a restore from entry 9001, want 9001", got)
		}
	})

	// Restoring replaces rather than merges. What a node held before is not
	// part of the state the snapshot describes, so it goes.
	t.Run("what the store held before is gone", func(t *testing.T) {
		t.Parallel()

		from := open(t)
		fill(t, from)

		into := open(t)
		stale := createZone(t, into, "left-over.example.")
		if err := into.ReplaceReplicated(ctxOf(t), 1, streamOf(exportAll(t, from))); err != nil {
			t.Fatalf("ReplaceReplicated: %v", err)
		}

		_, err := into.ZoneByID(ctxOf(t), stale.ID)
		wantErrIs(t, err, store.ErrNotFound, "the zone the node held before the restore")
	})

	// The export groups by kind and promises nothing about the order within
	// the records, because a generated record and the record it was derived
	// from are both records. A restore has to take them either way round.
	t.Run("a generated record may arrive before the record it comes from", func(t *testing.T) {
		t.Parallel()

		from := open(t)
		fill(t, from)
		items := exportAll(t, from)

		records := make([]*store.Replicated, 0, 2)
		rest := make([]*store.Replicated, 0, len(items))
		for _, item := range items {
			if item.Kind == store.ReplicatedRecord {
				records = append(records, item)
				continue
			}
			rest = append(rest, item)
		}
		slices.SortStableFunc(records, func(a, b *store.Replicated) int {
			return boolOrder(b.Record.IsManaged()) - boolOrder(a.Record.IsManaged())
		})

		into := open(t)
		if err := into.ReplaceReplicated(ctxOf(t), 1, streamOf(append(rest, records...))); err != nil {
			t.Fatalf("ReplaceReplicated with the generated record first: %v", err)
		}
		sameExport(t, exportAll(t, into), items)
	})

	t.Run("a restore that fails part way leaves the store as it was", func(t *testing.T) {
		t.Parallel()

		from := open(t)
		fill(t, from)
		items := exportAll(t, from)

		into := open(t)
		fill(t, into)
		before := exportAll(t, into)

		boom := errors.New("the snapshot stopped arriving")
		err := into.ReplaceReplicated(ctxOf(t), 1, func(yield func(*store.Replicated, error) bool) {
			for _, item := range items[:2] {
				if !yield(item, nil) {
					return
				}
			}
			yield(nil, boom)
		})
		if !errors.Is(err, boom) {
			t.Fatalf("ReplaceReplicated error = %v, want the one the stream reported", err)
		}
		sameExport(t, exportAll(t, into), before)
	})

	t.Run("an item that is not what its kind says is refused", func(t *testing.T) {
		t.Parallel()

		s := open(t)
		err := s.ReplaceReplicated(ctxOf(t), 1, streamOf([]*store.Replicated{{
			Kind:  store.ReplicatedZone,
			Token: &store.Token{ID: newID[store.TokenID]()},
		}}))
		wantErrIs(t, err, zone.ErrInvalid, "a zone-shaped item carrying a token")
	})

	t.Run("a restore from no position in the log is refused", func(t *testing.T) {
		t.Parallel()

		s := open(t)
		if err := s.ReplaceReplicated(ctxOf(t), 0, streamOf(nil)); err == nil {
			t.Error("ReplaceReplicated accepted entry 0, which is no entry")
		}
	})
}

// boolOrder sorts false before true.
func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}
