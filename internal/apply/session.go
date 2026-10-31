package apply

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// SessionKeySetting is the key the browser session keys are stored under.
//
// A session is a cookie sealed under this key rather than an entry in one
// node's memory, so that every member holding the key reads every session
// (docs/decisions/d51-accounts-come-from-an-identity-provider.md). It is a
// stored setting for the reason the cookie secret is one: every member has
// to hold the same key, and settings are what a batch carries between them.
const SessionKeySetting = "session_keys"

// EndedSessionSetting is the key the sessions ended before their time are
// stored under.
const EndedSessionSetting = "ended_sessions"

// SessionLifetime is how long a browser session lasts. Long enough for a
// working day, short enough that a laptop left open overnight is not an open
// door.
const SessionLifetime = 12 * time.Hour

// sessionLock is the key ending a session serializes on. The list is read and
// written whole, so two ending at once must not each write a list without the
// other.
const sessionLock = "sessions"

// SessionKeyLen is the length of one key, in octets: AES-256 takes 32.
const SessionKeyLen = 32

// SessionKey is one key browser sessions are sealed under. Like a cookie
// secret it is minted here and never shown to anybody.
type SessionKey [SessionKeyLen]byte

// NewSessionKey mints a key from the system's randomness, on the node that
// decided to rotate, for D24's reason: two nodes minting two keys for one
// rotation would each seal sessions the other cannot open.
func NewSessionKey() (SessionKey, error) {
	var k SessionKey
	if _, err := rand.Read(k[:]); err != nil {
		return SessionKey{}, fmt.Errorf("apply: mint a session key: %w", err)
	}
	return k, nil
}

// IsZero reports whether the key is unset.
func (k SessionKey) IsZero() bool { return k == SessionKey{} }

// MarshalJSON writes the key as hex.
func (k SessionKey) MarshalJSON() ([]byte, error) {
	if k.IsZero() {
		return []byte(`""`), nil
	}
	return json.Marshal(hex.EncodeToString(k[:]))
}

// UnmarshalJSON reads a key written as hex, and refuses one of the wrong
// length.
func (k *SessionKey) UnmarshalJSON(b []byte) error {
	var text string
	if err := json.Unmarshal(b, &text); err != nil {
		return err
	}
	if text == "" {
		*k = SessionKey{}
		return nil
	}
	raw, err := hex.DecodeString(text)
	if err != nil {
		return fmt.Errorf("apply: a session key is not hex: %w", err)
	}
	if len(raw) != SessionKeyLen {
		return fmt.Errorf("%w session key of %d octets, want %d", zone.ErrInvalid, len(raw), SessionKeyLen)
	}
	*k = SessionKey(raw)
	return nil
}

// SessionKeys are the keys a session is sealed and opened under.
//
// Two, for the reason there are two cookie secrets: a session sealed under the
// key just replaced has hours left to run, and a rotation that forgot that key
// would end it.
type SessionKeys struct {
	// Current is what a session opened now is sealed under.
	Current SessionKey `json:"current"`

	// Previous is the key before it, still opened. Zero until the first
	// rotation.
	Previous SessionKey `json:"previous"`

	// RotatedAt is when Current was minted.
	RotatedAt time.Time `json:"rotated_at"`
}

// IsZero reports whether no key has been minted yet.
func (k SessionKeys) IsZero() bool { return k.Current.IsZero() }

// Rotate returns the pair that follows this one.
func (k SessionKeys) Rotate(now time.Time) (SessionKeys, error) {
	next, err := NewSessionKey()
	if err != nil {
		return SessionKeys{}, err
	}
	return SessionKeys{Current: next, Previous: k.Current, RotatedAt: now.UTC()}, nil
}

// SessionRotation is how often the session key changes. It is the lifetime of
// a session and no shorter: a key is current for one lifetime and the
// predecessor for one more, by which time every session sealed under it has
// ended on its own.
const SessionRotation = SessionLifetime

// Due reports whether the keys are old enough to be rotated at now. Keys never
// minted are due at once, which is how the first comes into existence.
func (k SessionKeys) Due(now time.Time) bool {
	return k.IsZero() || !now.Before(k.RotatedAt.Add(SessionRotation))
}

// RotateSessionKeys mints the next key where the stored pair is due, and
// returns the pair in force afterwards along with whether it changed. In a
// cluster it is the leader's to do, as rotating the cookie secret is.
func (a *Applier) RotateSessionKeys(ctx context.Context) (SessionKeys, bool, error) {
	ctx, done, serr := a.settle(ctx)
	if serr != nil {
		return SessionKeys{}, false, serr
	}
	defer done()
	var stored SessionKeys
	if err := a.store.View(ctx, func(r store.Reader) error {
		var verr error
		stored, verr = StoredSessionKeys(ctx, r)
		return verr
	}); err != nil {
		return SessionKeys{}, false, err
	}

	now := a.now()
	if !stored.Due(now) {
		return stored, false, nil
	}
	next, err := stored.Rotate(now)
	if err != nil {
		return SessionKeys{}, false, err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return SessionKeys{}, false, fmt.Errorf("apply: encode the session keys: %w", err)
	}
	if serr := a.SetSettings(ctx, []SettingChange{{Key: SessionKeySetting, Value: raw}}); serr != nil {
		return SessionKeys{}, false, serr
	}
	return next, true, nil
}

// StoredSessionKeys returns the keys the database holds, or the zero pair when
// none have been minted.
func StoredSessionKeys(ctx context.Context, r store.Reader) (SessionKeys, error) {
	raw, err := r.Setting(ctx, SessionKeySetting)
	if errors.Is(err, store.ErrNotFound) {
		return SessionKeys{}, nil
	}
	if err != nil {
		return SessionKeys{}, err
	}
	var keys SessionKeys
	if uerr := json.Unmarshal(raw, &keys); uerr != nil {
		return SessionKeys{}, fmt.Errorf("apply: the stored session keys are not readable: %w", uerr)
	}
	if keys.Current.IsZero() {
		return SessionKeys{}, fmt.Errorf("%w session keys in the database: no current key", zone.ErrInvalid)
	}
	return keys, nil
}

// EndedSession is one session ended before its time, or every session one
// person had open when an administrator ended them.
type EndedSession struct {
	// ID is the SHA-256 of the session's identifier, in hex. What is stored
	// is not what a cookie carries, for the reason D5 stores a token's hash.
	ID string `json:"id,omitempty"`

	// Provider and Name, given in place of ID, are a person as their sessions
	// carry them: the provider they signed in through and what they are
	// called in a commit, compared without case.
	Provider string `json:"idp,omitempty"`
	Name     string `json:"name,omitempty"`

	// Until is when the session would have ended anyway. After it there is
	// nothing left to refuse, and the entry is dropped. For a person it is
	// one lifetime after they were ended, so that it covers every session
	// opened before and none opened after.
	Until time.Time `json:"until"`
}

// Ends reports whether the entry refuses a session: the one by this hashed
// identifier, or one of this person's that ends no later than the entry.
//
// shortcut: this trusts the members' clocks to agree with the leader's, as
// every session's expiry already does. A member whose clock is behind refuses
// a person signing in again for the difference; one whose clock is ahead lets
// a session it opened that much before the end survive it. Worth a margin if
// members are ever expected to run without time sync.
func (e EndedSession) Ends(id, provider, name string, expires time.Time) bool {
	if e.ID != "" {
		return e.ID == id
	}
	return e.Provider != "" && e.Provider == provider && strings.EqualFold(e.Name, name) &&
		!expires.After(e.Until)
}

// replaces reports whether e and o name the same session or the same person,
// so that one written second takes the place of the first.
func (e EndedSession) replaces(o EndedSession) bool {
	return e.ID == o.ID && e.Provider == o.Provider && strings.EqualFold(e.Name, o.Name)
}

// StoredEndedSessions returns the sessions ended before their time, including
// any whose time has since run out.
func StoredEndedSessions(ctx context.Context, r store.Reader) ([]EndedSession, error) {
	raw, err := r.Setting(ctx, EndedSessionSetting)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ended []EndedSession
	if uerr := json.Unmarshal(raw, &ended); uerr != nil {
		return nil, fmt.Errorf("apply: the stored ended sessions are not readable: %w", uerr)
	}
	return ended, nil
}

// EndSession records a session as ended, so that no member opens it again.
func (a *Applier) EndSession(ctx context.Context, id string, until time.Time) error {
	if id == "" {
		return fmt.Errorf("%w: no session named to end", zone.ErrInvalid)
	}
	return a.endSessions(ctx, func(_ store.Reader, _ time.Time) (EndedSession, error) {
		return EndedSession{ID: id, Until: until.UTC()}, nil
	})
}

// EndPersonSessions ends every session the person by this name holds through
// the provider, so that they sign in again and get what their groups give
// them now (D51).
func (a *Applier) EndPersonSessions(ctx context.Context, provider, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: name the person whose sessions are to end", zone.ErrInvalid)
	}
	return a.endSessions(ctx, func(r store.Reader, now time.Time) (EndedSession, error) {
		providers, err := StoredIdentityProviders(ctx, r)
		if err != nil {
			return EndedSession{}, err
		}
		if !slices.ContainsFunc(providers, func(p IdentityProvider) bool { return p.ID == provider }) {
			return EndedSession{}, fmt.Errorf("%w: no identity provider %s", store.ErrNotFound, provider)
		}
		return EndedSession{Provider: provider, Name: name, Until: now.Add(SessionLifetime).UTC()}, nil
	})
}

// endSessions adds the entry entry works out to the list of ended sessions.
//
// The list is written whole, and the entries whose sessions would have ended
// by now are dropped while it is planned, so that every node is handed the
// same list rather than each pruning by its own clock (D24). It never holds
// more than one lifetime's worth.
func (a *Applier) endSessions(
	ctx context.Context, entry func(store.Reader, time.Time) (EndedSession, error),
) error {
	ctx, done, serr := a.settle(ctx)
	if serr != nil {
		return serr
	}
	defer done()
	unlock := a.locks.lock(sessionLock)
	defer unlock()

	now := a.now()
	var (
		ended []EndedSession
		next  EndedSession
	)
	if err := a.store.View(ctx, func(r store.Reader) error {
		var verr error
		if next, verr = entry(r, now); verr != nil {
			return verr
		}
		ended, verr = StoredEndedSessions(ctx, r)
		return verr
	}); err != nil {
		return err
	}
	if !now.Before(next.Until) {
		return nil
	}
	ended = slices.DeleteFunc(ended, func(e EndedSession) bool {
		return e.replaces(next) || !now.Before(e.Until)
	})
	ended = append(ended, next)

	raw, err := json.Marshal(ended)
	if err != nil {
		return fmt.Errorf("apply: encode the ended sessions: %w", err)
	}
	b, err := a.PlanSettings([]SettingChange{{Key: EndedSessionSetting, Value: raw}})
	if err != nil {
		return err
	}
	return a.submit(ctx, b)
}
