package storetest

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func testTokens(t *testing.T, open Open) {
	hashOf := func(secret string) []byte {
		sum := sha256.Sum256([]byte(secret))
		return sum[:]
	}

	newToken := func(name, secret string) *store.Token {
		return &store.Token{
			ID:     newID[store.TokenID](),
			Name:   name,
			Prefix: secret[:min(8, len(secret))],
			Hash:   hashOf(secret),
			Scopes: []string{"zones:read", "zones:write"},
		}
	}

	t.Run("a token reads back by the hash of its secret", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		tok := newToken("deploy", "s3cret-token-value")
		mustUpdate(t, s, func(tx store.Tx) error { return tx.CreateToken(ctxOf(t), tok) })

		got, err := s.TokenByHash(ctxOf(t), hashOf("s3cret-token-value"))
		if err != nil {
			t.Fatalf("TokenByHash: %v", err)
		}
		if got.ID != tok.ID || got.Name != "deploy" || got.Prefix != "s3cret-t" {
			t.Errorf("token came back as %+v", got)
		}
		if !bytes.Equal(got.Hash, tok.Hash) {
			t.Error("the stored hash is not the one that was written")
		}
		if len(got.Scopes) != 2 || got.Scopes[0] != "zones:read" {
			t.Errorf("scopes came back as %v", got.Scopes)
		}
		if !got.Active(time.Now()) {
			t.Error("a fresh token reports itself as inactive")
		}
	})

	t.Run("an unknown secret is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		_, err := s.TokenByHash(ctxOf(t), hashOf("never issued"))
		wantErrIs(t, err, store.ErrNotFound, "TokenByHash on an unknown secret")

		_, err = s.TokenByHash(ctxOf(t), nil)
		wantErrIs(t, err, store.ErrNotFound, "TokenByHash on no hash at all")
	})

	// Unknown, revoked and expired all leave by the same door, so that a caller
	// cannot learn which tokens exist by watching which error comes back.
	t.Run("a revoked token is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		tok := newToken("old", "revoked-secret")
		mustUpdate(t, s, func(tx store.Tx) error { return tx.CreateToken(ctxOf(t), tok) })
		mustUpdate(t, s, func(tx store.Tx) error {
			return tx.RevokeToken(ctxOf(t), tok.ID, time.Now())
		})

		_, err := s.TokenByHash(ctxOf(t), hashOf("revoked-secret"))
		wantErrIs(t, err, store.ErrNotFound, "TokenByHash on a revoked token")

		// It is still listed, because the audit log keeps naming it.
		all, err := s.ListTokens(ctxOf(t))
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}
		if len(all) != 1 || all[0].RevokedAt.IsZero() {
			t.Errorf("the revoked token is not in the listing with its revocation: %+v", all)
		}
	})

	t.Run("an expired token is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		tok := newToken("stale", "expired-secret")
		tok.ExpiresAt = truncMillis(time.Now().Add(-time.Minute))
		mustUpdate(t, s, func(tx store.Tx) error { return tx.CreateToken(ctxOf(t), tok) })

		_, err := s.TokenByHash(ctxOf(t), hashOf("expired-secret"))
		wantErrIs(t, err, store.ErrNotFound, "TokenByHash on an expired token")
	})

	t.Run("a token expiring later still works", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		tok := newToken("temporary", "future-secret")
		tok.ExpiresAt = truncMillis(time.Now().Add(time.Hour))
		mustUpdate(t, s, func(tx store.Tx) error { return tx.CreateToken(ctxOf(t), tok) })

		got, err := s.TokenByHash(ctxOf(t), hashOf("future-secret"))
		if err != nil {
			t.Fatalf("TokenByHash: %v", err)
		}
		if !got.ExpiresAt.Equal(tok.ExpiresAt) {
			t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, tok.ExpiresAt)
		}
	})

	t.Run("revoking twice does not move the moment it stopped working", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		tok := newToken("deploy", "revoke-twice")
		mustUpdate(t, s, func(tx store.Tx) error { return tx.CreateToken(ctxOf(t), tok) })

		first := truncMillis(time.Now())
		mustUpdate(t, s, func(tx store.Tx) error { return tx.RevokeToken(ctxOf(t), tok.ID, first) })
		mustUpdate(t, s, func(tx store.Tx) error {
			return tx.RevokeToken(ctxOf(t), tok.ID, first.Add(time.Hour))
		})

		all, err := s.ListTokens(ctxOf(t))
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}
		if !all[0].RevokedAt.Equal(first) {
			t.Errorf("RevokedAt = %v, want the first revocation at %v", all[0].RevokedAt, first)
		}
	})

	t.Run("revoking a token that is not there is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		err := updateErr(t, s, func(tx store.Tx) error {
			return tx.RevokeToken(ctxOf(t), newID[store.TokenID](), time.Now())
		})
		wantErrIs(t, err, store.ErrNotFound, "revoking an unknown token")
	})

	t.Run("use is recorded", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		tok := newToken("deploy", "touched-secret")
		mustUpdate(t, s, func(tx store.Tx) error { return tx.CreateToken(ctxOf(t), tok) })

		used := truncMillis(time.Now())
		mustUpdate(t, s, func(tx store.Tx) error { return tx.TouchToken(ctxOf(t), tok.ID, used) })

		got, err := s.TokenByHash(ctxOf(t), hashOf("touched-secret"))
		if err != nil {
			t.Fatalf("TokenByHash: %v", err)
		}
		if !got.LastUsedAt.Equal(used) {
			t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, used)
		}

		// Advisory: touching a token that has gone is not worth failing a
		// request that has already succeeded.
		if err := updateErr(t, s, func(tx store.Tx) error {
			return tx.TouchToken(ctxOf(t), newID[store.TokenID](), used)
		}); err != nil {
			t.Errorf("touching an unknown token: %v", err)
		}
	})

	t.Run("a token needs an identifier and a full-length hash", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		noID := newToken("x", "some-secret")
		noID.ID = ""
		err := updateErr(t, s, func(tx store.Tx) error { return tx.CreateToken(ctxOf(t), noID) })
		wantErrIs(t, err, zone.ErrInvalid, "creating a token with no identifier")

		shortHash := newToken("x", "some-secret")
		shortHash.Hash = []byte("too short")
		err = updateErr(t, s, func(tx store.Tx) error { return tx.CreateToken(ctxOf(t), shortHash) })
		wantErrIs(t, err, zone.ErrInvalid, "creating a token with a truncated hash")
	})

	t.Run("two tokens cannot share a secret", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		mustUpdate(t, s, func(tx store.Tx) error {
			return tx.CreateToken(ctxOf(t), newToken("one", "shared-secret"))
		})
		err := updateErr(t, s, func(tx store.Tx) error {
			return tx.CreateToken(ctxOf(t), newToken("two", "shared-secret"))
		})
		wantErrIs(t, err, store.ErrConflict, "creating a second token with one secret")
	})
}

func testSettings(t *testing.T, open Open) {
	t.Run("a setting reads back and can be replaced", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		mustUpdate(t, s, func(tx store.Tx) error {
			return tx.PutSetting(ctxOf(t), "auto_reverse", []byte(`true`))
		})
		got, err := s.Setting(ctxOf(t), "auto_reverse")
		if err != nil {
			t.Fatalf("Setting: %v", err)
		}
		if string(got) != "true" {
			t.Errorf("Setting = %q, want %q", got, "true")
		}

		mustUpdate(t, s, func(tx store.Tx) error {
			return tx.PutSetting(ctxOf(t), "auto_reverse", []byte(`{"enabled":false}`))
		})
		got, err = s.Setting(ctxOf(t), "auto_reverse")
		if err != nil {
			t.Fatalf("Setting: %v", err)
		}
		if string(got) != `{"enabled":false}` {
			t.Errorf("Setting = %q after replacing it", got)
		}
	})

	t.Run("an unset setting is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		_, err := s.Setting(ctxOf(t), "never_set")
		wantErrIs(t, err, store.ErrNotFound, "reading an unset setting")
	})

	// The column holds JSON by contract and every reader unmarshals it, so a
	// value that is not JSON has to be refused on the way in rather than
	// discovered on the way out.
	t.Run("a value that is not JSON is refused", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		err := updateErr(t, s, func(tx store.Tx) error {
			return tx.PutSetting(ctxOf(t), "broken", []byte(`{not json`))
		})
		if err == nil {
			t.Fatal("a value that is not JSON was stored")
		}

		err = updateErr(t, s, func(tx store.Tx) error {
			return tx.PutSetting(ctxOf(t), "", []byte(`1`))
		})
		if err == nil {
			t.Fatal("a setting with no key was stored")
		}
	})
}

func testTransactions(t *testing.T, open Open) {
	t.Run("a failing transaction leaves nothing behind", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := newZone(t, "example.com.")
		boom := errors.New("changed my mind")

		err := s.Update(ctxOf(t), func(tx store.Tx) error {
			if err := tx.CreateZone(ctxOf(t), z); err != nil {
				return err
			}
			// Visible to the transaction that made it, and to nobody else.
			if _, err := tx.ZoneByID(ctxOf(t), z.ID); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("Update = %v, want %v", err, boom)
		}

		_, err = s.ZoneByID(ctxOf(t), z.ID)
		wantErrIs(t, err, store.ErrNotFound, "reading a zone from a rolled-back transaction")
	})

	t.Run("a transaction sees its own writes", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := newZone(t, "example.com.")
		mustUpdate(t, s, func(tx store.Tx) error {
			if err := tx.CreateZone(ctxOf(t), z); err != nil {
				return err
			}
			rec := newRecord(t, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
			if err := tx.InsertRecord(ctxOf(t), rec); err != nil {
				return err
			}
			// The record can only be inserted at all if the zone it references
			// is already visible inside the transaction.
			got, err := tx.RecordByID(ctxOf(t), rec.ID)
			if err != nil {
				return err
			}
			if got.RData.String() != "192.0.2.10" {
				t.Errorf("the record read inside the transaction is %s", got.RData)
			}
			return nil
		})
	})

	t.Run("a read transaction sees one state", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		createZone(t, s, "example.com.")

		if err := s.View(ctxOf(t), func(r store.Reader) error {
			first, err := r.ListZones(ctxOf(t), store.ZoneFilter{})
			if err != nil {
				return err
			}
			second, err := r.ListZones(ctxOf(t), store.ZoneFilter{})
			if err != nil {
				return err
			}
			if len(first.Items) != len(second.Items) {
				t.Errorf("two reads in one view saw %d and %d zones",
					len(first.Items), len(second.Items))
			}
			return nil
		}); err != nil {
			t.Fatalf("View: %v", err)
		}
	})

	t.Run("a failing view reports why", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		boom := errors.New("no")
		if err := s.View(ctxOf(t), func(store.Reader) error { return boom }); !errors.Is(err, boom) {
			t.Errorf("View = %v, want %v", err, boom)
		}
	})

	t.Run("the store reports what it is", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		if err := s.Ping(ctxOf(t)); err != nil {
			t.Errorf("Ping: %v", err)
		}
		if s.Capabilities().Backend == "" {
			t.Error("the store does not name its backend")
		}
		// Migrating an already-migrated store is what every restart does.
		if err := s.Migrate(ctxOf(t)); err != nil {
			t.Errorf("Migrate on an up-to-date store: %v", err)
		}
	})
}

func testCursors(t *testing.T, open Open) {
	s := open(t)

	z := createZone(t, s, "example.com.")
	createZone(t, s, "example.net.")
	createRecord(t, s, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
	createRecord(t, s, z.ID, "www.example.com.", zone.TypeAAAA, "2001:db8::1")

	zonePage, err := s.ListZones(ctxOf(t), store.ZoneFilter{Paging: store.Paging{Limit: 1}})
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	recordPage, err := s.ListRecords(ctxOf(t), store.RecordFilter{Paging: store.Paging{Limit: 1}})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if zonePage.NextCursor == "" || recordPage.NextCursor == "" {
		t.Fatal("a page that is not the last one returned no cursor")
	}

	// A cursor is a position in one listing's order. Read as a position in
	// another's it would silently resume from the wrong place, so it is refused
	// instead.
	t.Run("a cursor from another listing is refused", func(t *testing.T) {
		_, err := s.ListRecords(ctxOf(t), store.RecordFilter{
			Paging: store.Paging{Cursor: zonePage.NextCursor},
		})
		if err == nil {
			t.Error("the record listing accepted a zone cursor")
		}
		_, err = s.ListZones(ctxOf(t), store.ZoneFilter{
			Paging: store.Paging{Cursor: recordPage.NextCursor},
		})
		if err == nil {
			t.Error("the zone listing accepted a record cursor")
		}
	})

	t.Run("an invented cursor is refused", func(t *testing.T) {
		for _, c := range []store.Cursor{"nonsense", "!!!!", "eyJrIjoieiJ9"} {
			if _, err := s.ListZones(ctxOf(t), store.ZoneFilter{
				Paging: store.Paging{Cursor: c},
			}); err == nil {
				t.Errorf("the zone listing accepted the cursor %q", c)
			}
		}
	})
}

// The index is what keeps a node from carrying out a batch it has already
// carried out, so it has to survive on its own and read back exactly.
func testAppliedIndex(t *testing.T, open Open) {
	t.Run("a node that has replayed nothing is at zero", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		got, err := s.AppliedIndex(ctxOf(t))
		if err != nil {
			t.Fatalf("AppliedIndex: %v", err)
		}
		if got != 0 {
			t.Errorf("AppliedIndex = %d on an untouched store, want 0", got)
		}
	})

	t.Run("it reads back and moves on", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		for _, want := range []uint64{1, 7, 1 << 40} {
			mustUpdate(t, s, func(tx store.Tx) error {
				return tx.SetAppliedIndex(ctxOf(t), want)
			})
			got, err := s.AppliedIndex(ctxOf(t))
			if err != nil {
				t.Fatalf("AppliedIndex: %v", err)
			}
			if got != want {
				t.Errorf("AppliedIndex = %d, want %d", got, want)
			}
		}
	})

	// It is written beside the batch it belongs to, so a transaction that rolls
	// back has to take the index with it. Otherwise a node would claim to have
	// applied an entry whose writes it threw away.
	t.Run("a rolled back transaction leaves it where it was", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		mustUpdate(t, s, func(tx store.Tx) error { return tx.SetAppliedIndex(ctxOf(t), 4) })

		wanted := errors.New("no")
		if err := s.Update(ctxOf(t), func(tx store.Tx) error {
			if err := tx.SetAppliedIndex(ctxOf(t), 5); err != nil {
				return err
			}
			return wanted
		}); !errors.Is(err, wanted) {
			t.Fatalf("Update = %v, want the error the function returned", err)
		}

		got, err := s.AppliedIndex(ctxOf(t))
		if err != nil {
			t.Fatalf("AppliedIndex: %v", err)
		}
		if got != 4 {
			t.Errorf("AppliedIndex = %d after a rollback, want 4", got)
		}
	})

	t.Run("zero is not a position and is refused", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		if err := updateErr(t, s, func(tx store.Tx) error {
			return tx.SetAppliedIndex(ctxOf(t), 0)
		}); err == nil {
			t.Error("SetAppliedIndex(0) succeeded, want an error")
		}
	})
}
