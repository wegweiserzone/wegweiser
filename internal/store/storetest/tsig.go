package storetest

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// newTSIGKey is one key, ready to store.
func newTSIGKey(name string, alg zone.TSIGAlgorithm) *store.TSIGKey {
	return &store.TSIGKey{
		ID:        newID[store.TSIGKeyID](),
		Name:      zone.MustParseName(name),
		Algorithm: alg,
		Secret:    make([]byte, alg.SecretBytes()),
	}
}

func testTSIGKeys(t *testing.T, open Open) {
	s := open(t)
	ctx := t.Context()

	k := newTSIGKey("secondary.example.com.", zone.HMACSHA256)
	for i := range k.Secret {
		k.Secret[i] = byte(i)
	}
	mustUpdate(t, s, func(tx store.Tx) error { return tx.CreateTSIGKey(ctx, k) })

	t.Run("it comes back by the name a request would carry", func(t *testing.T) {
		got, err := s.TSIGKeyByName(ctx, zone.MustParseName("secondary.example.com."))
		if err != nil {
			t.Fatalf("TSIGKeyByName: %v", err)
		}
		if got.ID != k.ID || got.Algorithm != zone.HMACSHA256 {
			t.Errorf("got %s/%s, want %s/%s", got.ID, got.Algorithm, k.ID, zone.HMACSHA256)
		}
		// The secret is the point: without it nothing can be verified.
		if !bytes.Equal(got.Secret, k.Secret) {
			t.Errorf("the secret came back as %x, want %x", got.Secret, k.Secret)
		}
		if !got.Active() {
			t.Error("a key that was just created is not active")
		}
	})

	t.Run("the name is matched the way RFC 8945 §9 compares one", func(t *testing.T) {
		if _, err := s.TSIGKeyByName(ctx, zone.MustParseName("SECONDARY.Example.COM.")); err != nil {
			t.Errorf("a key named in another case was not found: %v", err)
		}
	})

	t.Run("a second key cannot take a name that already signs", func(t *testing.T) {
		clash := newTSIGKey("secondary.example.com.", zone.HMACSHA512)
		clash.Secret = []byte("another secret entirely")
		err := updateErr(t, s, func(tx store.Tx) error { return tx.CreateTSIGKey(ctx, clash) })
		wantErrIs(t, err, store.ErrConflict, "creating a second key with the same name")
	})

	t.Run("a key with no secret signs nothing and is refused", func(t *testing.T) {
		empty := newTSIGKey("empty.example.com.", zone.HMACSHA256)
		empty.Secret = nil
		err := updateErr(t, s, func(tx store.Tx) error { return tx.CreateTSIGKey(ctx, empty) })
		wantErrIs(t, err, zone.ErrInvalid, "creating a key with no secret")
	})

	revoked := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	t.Run("revoking takes the secret with it", func(t *testing.T) {
		mustUpdate(t, s, func(tx store.Tx) error { return tx.RevokeTSIGKey(ctx, k.ID, revoked) })

		if _, err := s.TSIGKeyByName(ctx, k.Name); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a revoked key is still resolved by name: %v", err)
		}

		got, err := s.TSIGKeyByID(ctx, k.ID)
		if err != nil {
			t.Fatalf("TSIGKeyByID: %v", err)
		}
		if len(got.Secret) != 0 {
			t.Errorf("the secret survived revocation: %x", got.Secret)
		}
		if got.Active() {
			t.Error("a revoked key reports itself as active")
		}
		if !got.RevokedAt.Equal(revoked) {
			t.Errorf("revoked at %s, want %s", got.RevokedAt, revoked)
		}
		// The name and the dates stay, so the record still reads.
		if !got.Name.Equal(k.Name) {
			t.Errorf("the name became %s", got.Name)
		}
	})

	t.Run("revoking twice does not move the moment it stopped", func(t *testing.T) {
		later := revoked.Add(time.Hour)
		mustUpdate(t, s, func(tx store.Tx) error { return tx.RevokeTSIGKey(ctx, k.ID, later) })

		got, err := s.TSIGKeyByID(ctx, k.ID)
		if err != nil {
			t.Fatalf("TSIGKeyByID: %v", err)
		}
		if !got.RevokedAt.Equal(revoked) {
			t.Errorf("revoked at %s, want the first revocation at %s", got.RevokedAt, revoked)
		}
	})

	t.Run("revoking one that is not here says so", func(t *testing.T) {
		err := updateErr(t, s, func(tx store.Tx) error {
			return tx.RevokeTSIGKey(ctx, newID[store.TSIGKeyID](), revoked)
		})
		wantErrIs(t, err, store.ErrNotFound, "revoking a key that does not exist")
	})

	t.Run("the name is free again once the key is revoked", func(t *testing.T) {
		// Rotating a key an operator has already configured on a secondary
		// should not force them to rename it there.
		next := newTSIGKey("secondary.example.com.", zone.HMACSHA256)
		next.Secret = []byte("a fresh secret for the same name")
		mustUpdate(t, s, func(tx store.Tx) error { return tx.CreateTSIGKey(ctx, next) })

		got, err := s.TSIGKeyByName(ctx, next.Name)
		if err != nil {
			t.Fatalf("TSIGKeyByName: %v", err)
		}
		if got.ID != next.ID {
			t.Errorf("the name resolves to %s, want the key that replaced it, %s", got.ID, next.ID)
		}
	})

	t.Run("the listing holds both, and only the live one has a secret", func(t *testing.T) {
		keys, err := s.ListTSIGKeys(ctx)
		if err != nil {
			t.Fatalf("ListTSIGKeys: %v", err)
		}
		if len(keys) != 2 {
			t.Fatalf("the listing holds %d keys, want the revoked one and its replacement", len(keys))
		}
		for _, got := range keys {
			if got.Active() == (len(got.Secret) == 0) {
				t.Errorf("key %s is active=%v with a %d byte secret",
					got.ID, got.Active(), len(got.Secret))
			}
		}
	})

	t.Run("a name nobody has signed with is not found", func(t *testing.T) {
		_, err := s.TSIGKeyByName(ctx, zone.MustParseName("nobody.example.com."))
		wantErrIs(t, err, store.ErrNotFound, "resolving a key that does not exist")
	})
}
