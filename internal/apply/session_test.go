package apply_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestSessionKeysRotateOnceASessionLifetime(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	start := f.now

	first, rotated, err := f.a.RotateSessionKeys(t.Context())
	if err != nil {
		t.Fatalf("RotateSessionKeys: %v", err)
	}
	if !rotated || first.Current.IsZero() || !first.Previous.IsZero() {
		t.Fatalf("the first call gave %+v, rotated %t, want one key and no predecessor", first, rotated)
	}

	f.now = start.Add(apply.SessionRotation - time.Second)
	again, rotated, err := f.a.RotateSessionKeys(t.Context())
	if err != nil {
		t.Fatalf("RotateSessionKeys: %v", err)
	}
	if rotated || again.Current != first.Current {
		t.Errorf("the key changed after %s", apply.SessionRotation-time.Second)
	}

	// A key is current for one lifetime and the predecessor for one more, so
	// no session sealed under it ends before its time.
	f.now = start.Add(apply.SessionRotation)
	next, rotated, err := f.a.RotateSessionKeys(t.Context())
	if err != nil {
		t.Fatalf("RotateSessionKeys: %v", err)
	}
	if !rotated || next.Current == first.Current || next.Previous != first.Current {
		t.Errorf("after a lifetime the keys are %+v, want a new one with the old one kept", next)
	}
	if apply.SessionRotation < apply.SessionLifetime {
		t.Errorf("keys rotate every %s, and a session lasts %s", apply.SessionRotation, apply.SessionLifetime)
	}
}

func TestEndingASessionKeepsTheListToWhatCanStillBeRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	start := f.now
	ended := func() []apply.EndedSession {
		t.Helper()
		var got []apply.EndedSession
		if err := f.s.View(t.Context(), func(r store.Reader) error {
			var verr error
			got, verr = apply.StoredEndedSessions(t.Context(), r)
			return verr
		}); err != nil {
			t.Fatalf("StoredEndedSessions: %v", err)
		}
		return got
	}
	end := func(id string, until time.Time) {
		t.Helper()
		if err := f.a.EndSession(t.Context(), id, until); err != nil {
			t.Fatalf("EndSession %s: %v", id, err)
		}
	}

	end("one", start.Add(time.Hour))
	end("two", start.Add(3*time.Hour))
	end("two", start.Add(3*time.Hour))
	if got := ended(); len(got) != 2 {
		t.Fatalf("ended = %+v, want each session once", got)
	}

	// A session that has run out needs nothing to refuse it, and is dropped
	// the next time the list is written.
	f.now = start.Add(2 * time.Hour)
	end("three", start.Add(4*time.Hour))
	got := ended()
	if len(got) != 2 || got[0].ID != "two" || got[1].ID != "three" {
		t.Errorf("ended = %+v, want two and three", got)
	}

	// Nor is one written that has run out already.
	end("four", start.Add(time.Hour))
	if got := ended(); len(got) != 2 {
		t.Errorf("ended = %+v, want a session past its end left out", got)
	}
}

// D51: ending a person refuses every session they opened before it, and none
// they open after.
func TestEndingAPersonCoversTheSessionsTheyHadOpen(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	start := f.now
	p, err := f.a.CreateIdentityProvider(t.Context(), freeIPA())
	if err != nil {
		t.Fatalf("CreateIdentityProvider: %v", err)
	}
	if err := f.a.EndPersonSessions(t.Context(), p.ID, "alice"); err != nil {
		t.Fatalf("EndPersonSessions: %v", err)
	}
	f.now = start.Add(time.Minute)
	if err := f.a.EndPersonSessions(t.Context(), p.ID, " Alice "); err != nil {
		t.Fatalf("EndPersonSessions again: %v", err)
	}

	var ended []apply.EndedSession
	if err := f.s.View(t.Context(), func(r store.Reader) error {
		var verr error
		ended, verr = apply.StoredEndedSessions(t.Context(), r)
		return verr
	}); err != nil {
		t.Fatalf("StoredEndedSessions: %v", err)
	}
	if len(ended) != 1 {
		t.Fatalf("ended = %+v, want the person once, the later entry in place of the first", ended)
	}
	e := ended[0]

	opened := f.now.Add(apply.SessionLifetime) // a session opened as they were ended
	for _, c := range []struct {
		what           string
		provider, name string
		expires        time.Time
		want           bool
	}{
		{"one opened before", p.ID, "alice", opened.Add(-time.Hour), true},
		{"one opened as they were ended", p.ID, "ALICE", opened, true},
		{"one opened after", p.ID, "alice", opened.Add(time.Second), false},
		{"somebody else's", p.ID, "bob", opened.Add(-time.Hour), false},
		{"an alice at another provider", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice", opened.Add(-time.Hour), false},
		{"a token's", "", "", opened.Add(-time.Hour), false},
	} {
		if got := e.Ends("x", c.provider, c.name, c.expires); got != c.want {
			t.Errorf("%s: ended = %v, want %v", c.what, got, c.want)
		}
	}

	if err := f.a.EndPersonSessions(t.Context(), "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no such provider gave %v, want not found", err)
	}
	if err := f.a.EndPersonSessions(t.Context(), p.ID, " "); !errors.Is(err, zone.ErrInvalid) {
		t.Errorf("no name gave %v, want it refused", err)
	}
}
