package storetest

import (
	"errors"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func memberIDOf(t *testing.T, s store.Store) string {
	t.Helper()
	id, err := s.MemberID(ctxOf(t))
	if err != nil {
		t.Fatalf("MemberID: %v", err)
	}
	return id
}

func testMemberID(t *testing.T, open Open) {
	t.Run("a node that never joined a cluster has none", func(t *testing.T) {
		t.Parallel()
		if got := memberIDOf(t, open(t)); got != "" {
			t.Errorf("MemberID = %q on an untouched store, want none", got)
		}
	})

	t.Run("it reads back, and setting it again changes nothing", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		for range 2 {
			mustUpdate(t, s, func(tx store.Tx) error { return tx.SetMemberID(ctxOf(t), "ns1") })
		}
		if got := memberIDOf(t, s); got != "ns1" {
			t.Errorf("MemberID = %q, want ns1", got)
		}
	})

	// D42: a member whose identifier changed would graft one member's history
	// onto another's.
	t.Run("another identifier is refused", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		mustUpdate(t, s, func(tx store.Tx) error { return tx.SetMemberID(ctxOf(t), "ns1") })

		err := updateErr(t, s, func(tx store.Tx) error { return tx.SetMemberID(ctxOf(t), "ns2") })
		wantErrIs(t, err, store.ErrConflict, "becoming another member")
		if got := memberIDOf(t, s); got != "ns1" {
			t.Errorf("MemberID = %q after the refusal, want ns1 still", got)
		}
	})

	t.Run("an empty identifier is refused", func(t *testing.T) {
		t.Parallel()
		err := updateErr(t, open(t), func(tx store.Tx) error { return tx.SetMemberID(ctxOf(t), "") })
		wantErrIs(t, err, zone.ErrInvalid, "an empty identifier")
	})

	t.Run("a rolled back transaction leaves none", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		boom := errors.New("changed my mind")
		err := updateErr(t, s, func(tx store.Tx) error {
			if serr := tx.SetMemberID(ctxOf(t), "ns1"); serr != nil {
				return serr
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("Update = %v, want the transaction's own error", err)
		}
		if got := memberIDOf(t, s); got != "" {
			t.Errorf("MemberID = %q after a rollback, want none", got)
		}
	})

	// Node-local, like the applied index: the identifier says who this machine
	// is, and a snapshot from another member says nothing about that.
	t.Run("no snapshot carries it and no restore replaces it", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		mustUpdate(t, s, func(tx store.Tx) error { return tx.SetMemberID(ctxOf(t), "ns1") })

		if items := exportAll(t, s); len(items) != 0 {
			t.Errorf("a store holding only its identifier exported %v, want nothing", kindsOf(items))
		}
		if err := s.ReplaceReplicated(ctxOf(t), 1, streamOf(nil)); err != nil {
			t.Fatalf("ReplaceReplicated: %v", err)
		}
		if got := memberIDOf(t, s); got != "ns1" {
			t.Errorf("MemberID = %q after a restore, want ns1 still", got)
		}
	})
}
