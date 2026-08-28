package storetest

import (
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// newCommit builds an unsaved commit stepping a zone's serial by one.
func newCommit(z *zone.Zone, from uint32, kind journal.Kind, events ...journal.Event) *journal.Commit {
	return &journal.Commit{
		ID:         newID[journal.CommitID](),
		ZoneID:     z.ID,
		ZoneName:   z.Name,
		SerialFrom: zone.NewSerial(from),
		SerialTo:   zone.NewSerial(from + 1),
		Kind:       kind,
		Source:     journal.SourceAPI,
		Actor:      "deploy-token",
		Events:     events,
	}
}

func event(seq int, op journal.Op, name string, typ zone.RRType, rdata string) journal.Event {
	return journal.Event{
		Seq:   seq,
		Op:    op,
		Name:  zone.MustParseName(name),
		Class: zone.ClassIN,
		Type:  typ,
		TTL:   3600,
		RData: zone.MustParseRData(typ, zone.ClassIN, rdata),
	}
}

func testJournal(t *testing.T, open Open) {
	t.Run("a commit reads back with its events in order", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		c := newCommit(z, 1, journal.KindEdit,
			event(0, journal.OpDel, "www.example.com.", zone.TypeA, "192.0.2.10"),
			event(1, journal.OpAdd, "www.example.com.", zone.TypeA, "192.0.2.11"),
			event(2, journal.OpAdd, "mail.example.com.", zone.TypeAAAA, "2001:db8::1"),
		)
		c.Comment = "moved the web server"
		mustUpdate(t, s, func(tx store.Tx) error { return tx.AppendCommit(ctxOf(t), c) })

		if c.CreatedAt.IsZero() {
			t.Fatal("the store did not stamp the commit it was handed")
		}

		got, err := s.CommitByID(ctxOf(t), c.ID)
		if err != nil {
			t.Fatalf("CommitByID: %v", err)
		}
		if got.ZoneID != z.ID || got.Kind != journal.KindEdit || got.Source != journal.SourceAPI {
			t.Errorf("commit metadata came back as %+v", got)
		}
		if got.SerialFrom != c.SerialFrom || got.SerialTo != c.SerialTo {
			t.Errorf("serials came back as %s to %s, want %s to %s",
				got.SerialFrom, got.SerialTo, c.SerialFrom, c.SerialTo)
		}
		if got.Actor != "deploy-token" || got.Comment != "moved the web server" {
			t.Errorf("actor and comment came back as %q and %q", got.Actor, got.Comment)
		}
		if !got.CreatedAt.Equal(c.CreatedAt) {
			t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, c.CreatedAt)
		}

		if len(got.Events) != len(c.Events) {
			t.Fatalf("read back %d events, want %d", len(got.Events), len(c.Events))
		}
		for i := range got.Events {
			if got.Events[i] != c.Events[i] {
				t.Errorf("event %d came back as %+v, want %+v", i, got.Events[i], c.Events[i])
			}
		}
		// What comes out of the store must still be a valid commit, or the
		// journal has lost something on the way through.
		if err := got.Validate(); err != nil {
			t.Errorf("the commit read back does not validate: %v", err)
		}
	})

	t.Run("a commit with no events is legal", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		c := newCommit(z, 1, journal.KindZoneUpdate)
		mustUpdate(t, s, func(tx store.Tx) error { return tx.AppendCommit(ctxOf(t), c) })

		got, err := s.CommitByID(ctxOf(t), c.ID)
		if err != nil {
			t.Fatalf("CommitByID: %v", err)
		}
		if len(got.Events) != 0 {
			t.Errorf("read back %d events, want none", len(got.Events))
		}
	})

	t.Run("a rollback keeps the serial it restored", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		target := zone.NewSerial(7)
		c := newCommit(z, 41, journal.KindRollback)
		c.RevertsTo = &target
		mustUpdate(t, s, func(tx store.Tx) error { return tx.AppendCommit(ctxOf(t), c) })

		got, err := s.CommitByID(ctxOf(t), c.ID)
		if err != nil {
			t.Fatalf("CommitByID: %v", err)
		}
		if got.RevertsTo == nil || *got.RevertsTo != target {
			t.Errorf("RevertsTo = %v, want %s", got.RevertsTo, target)
		}
	})

	// One commit per serial is what makes "restore the zone to serial 91" name
	// a single state. The unique index is the last line of defence for it.
	t.Run("two commits producing one serial is a conflict", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		mustUpdate(t, s, func(tx store.Tx) error {
			return tx.AppendCommit(ctxOf(t), newCommit(z, 1, journal.KindEdit))
		})
		err := updateErr(t, s, func(tx store.Tx) error {
			return tx.AppendCommit(ctxOf(t), newCommit(z, 1, journal.KindEdit))
		})
		wantErrIs(t, err, store.ErrConflict, "appending a second commit for one serial")
	})

	t.Run("two zones advance independently", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		one := createZone(t, s, "example.com.")
		two := createZone(t, s, "example.net.")

		mustUpdate(t, s, func(tx store.Tx) error {
			if err := tx.AppendCommit(ctxOf(t), newCommit(one, 1, journal.KindEdit)); err != nil {
				return err
			}
			return tx.AppendCommit(ctxOf(t), newCommit(two, 1, journal.KindEdit))
		})
	})

	t.Run("an invalid commit is refused", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		c := newCommit(z, 1, journal.KindEdit)
		c.SerialTo = zone.NewSerial(99) // more than one step
		err := updateErr(t, s, func(tx store.Tx) error { return tx.AppendCommit(ctxOf(t), c) })
		wantErrIs(t, err, zone.ErrInvalid, "appending a commit that skips serials")
	})

	t.Run("a missing commit is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		_, err := s.CommitByID(ctxOf(t), newID[journal.CommitID]())
		wantErrIs(t, err, store.ErrNotFound, "CommitByID on an unknown identifier")
	})

	// A commit outlives the zone it describes. The last thing that happens to a
	// zone is that someone removes it, and a journal that went with it could
	// never answer who did.
	t.Run("the journal outlives the zone", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		c := newCommit(z, 1, journal.KindEdit,
			event(0, journal.OpAdd, "www.example.com.", zone.TypeA, "192.0.2.10"))
		mustUpdate(t, s, func(tx store.Tx) error { return tx.AppendCommit(ctxOf(t), c) })
		mustUpdate(t, s, func(tx store.Tx) error { return tx.DeleteZone(ctxOf(t), z.ID) })

		got, err := s.CommitByID(ctxOf(t), c.ID)
		if err != nil {
			t.Fatalf("the journal went with the zone: %v", err)
		}
		// Including the name, which there is no longer a zone row to join to.
		if !got.ZoneName.Equal(z.Name) {
			t.Errorf("the commit names the zone %q, want %q", got.ZoneName, z.Name)
		}
		if len(got.Events) != 1 {
			t.Errorf("the commit came back with %d events, want one", len(got.Events))
		}

		// The records did go, because they belong to the zone rather than to
		// its history.
		left := drainRecords(t, s, store.RecordFilter{ZoneID: z.ID})
		if len(left) != 0 {
			t.Errorf("the deleted zone still holds %v", recordKeys(left))
		}
	})

	t.Run("listing", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		one := createZone(t, s, "example.com.")
		two := createZone(t, s, "example.net.")

		base := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
		kinds := []journal.Kind{
			journal.KindZoneCreate, journal.KindEdit, journal.KindImport, journal.KindEdit,
		}
		for i, kind := range kinds {
			c := newCommit(one, uint32(i)+1, kind)
			if kind == journal.KindZoneCreate {
				c.SerialFrom = zone.NewSerial(0)
				c.SerialTo = zone.NewSerial(uint32(i) + 1)
			}
			c.CreatedAt = base.Add(time.Duration(i) * time.Minute)
			c.Actor = "token-" + string(rune('a'+i))
			mustUpdate(t, s, func(tx store.Tx) error { return tx.AppendCommit(ctxOf(t), c) })
		}
		elsewhere := newCommit(two, 1, journal.KindEdit)
		elsewhere.CreatedAt = base.Add(time.Hour)
		mustUpdate(t, s, func(tx store.Tx) error { return tx.AppendCommit(ctxOf(t), elsewhere) })

		t.Run("newest first", func(t *testing.T) {
			page, err := s.ListCommits(ctxOf(t), store.CommitFilter{ZoneID: one.ID})
			if err != nil {
				t.Fatalf("ListCommits: %v", err)
			}
			if len(page.Items) != 4 {
				t.Fatalf("got %d commits, want four", len(page.Items))
			}
			for i := 1; i < len(page.Items); i++ {
				if page.Items[i-1].CreatedAt.Before(page.Items[i].CreatedAt) {
					t.Errorf("commit %d is older than the one before it", i)
				}
			}
			// The audit log renders headings, so the events stay behind.
			for _, c := range page.Items {
				if len(c.Events) != 0 {
					t.Errorf("the listing carried events for %s", c.ID)
				}
			}
		})

		t.Run("by zone", func(t *testing.T) {
			page, err := s.ListCommits(ctxOf(t), store.CommitFilter{ZoneID: two.ID})
			if err != nil {
				t.Fatalf("ListCommits: %v", err)
			}
			if len(page.Items) != 1 || page.Items[0].ID != elsewhere.ID {
				t.Errorf("commits of example.net = %d, want the one", len(page.Items))
			}
		})

		t.Run("by kind", func(t *testing.T) {
			page, err := s.ListCommits(ctxOf(t), store.CommitFilter{
				ZoneID: one.ID,
				Kinds:  []journal.Kind{journal.KindEdit, journal.KindImport},
			})
			if err != nil {
				t.Fatalf("ListCommits: %v", err)
			}
			if len(page.Items) != 3 {
				t.Errorf("edits and imports = %d, want three", len(page.Items))
			}
		})

		t.Run("by actor", func(t *testing.T) {
			page, err := s.ListCommits(ctxOf(t), store.CommitFilter{Actor: "token-b"})
			if err != nil {
				t.Fatalf("ListCommits: %v", err)
			}
			if len(page.Items) != 1 {
				t.Errorf("commits by token-b = %d, want one", len(page.Items))
			}
		})

		t.Run("by time window", func(t *testing.T) {
			page, err := s.ListCommits(ctxOf(t), store.CommitFilter{
				ZoneID: one.ID,
				Since:  base.Add(time.Minute),
				Until:  base.Add(3 * time.Minute),
			})
			if err != nil {
				t.Fatalf("ListCommits: %v", err)
			}
			// Since is inclusive and Until exclusive, so minutes one and two.
			if len(page.Items) != 2 {
				t.Errorf("commits in the window = %d, want two", len(page.Items))
			}
		})

		t.Run("one commit per page reaches the same list", func(t *testing.T) {
			var paged []journal.CommitID
			f := store.CommitFilter{ZoneID: one.ID, Paging: store.Paging{Limit: 1}}
			for {
				page, err := s.ListCommits(ctxOf(t), f)
				if err != nil {
					t.Fatalf("ListCommits: %v", err)
				}
				for _, c := range page.Items {
					paged = append(paged, c.ID)
				}
				if page.NextCursor == "" {
					break
				}
				f.Cursor = page.NextCursor
			}
			if len(paged) != 4 {
				t.Errorf("paging produced %d commits, want four", len(paged))
			}
		})
	})
}
