package apply

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// CookieSecretSetting is the key the DNS cookie secrets are stored under.
//
// A Server Cookie is a hash under this secret (RFC 9018 §4.4), so it decides
// which cookies this server recognises as its own. It is a stored setting
// rather than a file beside the database because every node answering for the
// same addresses has to hold the same one, and settings are what a batch
// already carries between nodes (D32).
const CookieSecretSetting = "cookie_secrets"

// CookieSecretLen is the length of one secret, in octets. SipHash-2-4 takes a
// 128-bit key, and RFC 9018 §6 leaves no second choice of hash.
const CookieSecretLen = 16

// CookieSecret is one key Server Cookies are computed under.
//
// It is minted here and never shown to anybody: unlike a token or a TSIG key,
// no operator ever types it in, and there is nothing on the other end of it to
// configure. That is what makes rotating it a decision this server can take on
// its own.
type CookieSecret [CookieSecretLen]byte

// NewCookieSecret mints a secret from the system's randomness.
//
// D24's rule about minting holds here: the secret is settled on the node that
// decided to rotate and travels with the entry, because two nodes inventing
// two secrets for one rotation would hand out cookies neither recognises.
func NewCookieSecret() (CookieSecret, error) {
	var s CookieSecret
	if _, err := rand.Read(s[:]); err != nil {
		return CookieSecret{}, fmt.Errorf("apply: mint a cookie secret: %w", err)
	}
	return s, nil
}

// IsZero reports whether the secret is unset. A minted one is never zero in
// any run anybody will see.
func (s CookieSecret) IsZero() bool { return s == CookieSecret{} }

// MarshalJSON writes the secret as hex, which is how RFC 9018's own test
// vectors write one and the only form it is ever read in.
func (s CookieSecret) MarshalJSON() ([]byte, error) {
	if s.IsZero() {
		return []byte(`""`), nil
	}
	return json.Marshal(hex.EncodeToString(s[:]))
}

// UnmarshalJSON reads a secret written as hex, and refuses one of the wrong
// length rather than padding it into something that hashes.
func (s *CookieSecret) UnmarshalJSON(b []byte) error {
	var text string
	if err := json.Unmarshal(b, &text); err != nil {
		return err
	}
	if text == "" {
		*s = CookieSecret{}
		return nil
	}

	raw, err := hex.DecodeString(text)
	if err != nil {
		return fmt.Errorf("apply: a cookie secret is not hex: %w", err)
	}
	if len(raw) != CookieSecretLen {
		return fmt.Errorf("%w cookie secret of %d octets, want %d",
			zone.ErrInvalid, len(raw), CookieSecretLen)
	}
	*s = CookieSecret(raw)
	return nil
}

// CookieSecrets are the secrets this server computes and checks Server Cookies
// under.
//
// There are two of them because a rotation cannot be instantaneous. A client
// holds a Server Cookie for up to an hour (RFC 9018 §4.3), and a rotation that
// forgot the secret those were built under would refuse every one of them in
// the same second, which is the round trip D35 is trying to charge once.
type CookieSecrets struct {
	// Current is what a cookie handed out now is computed under.
	Current CookieSecret `json:"current"`

	// Previous is the secret before it, still accepted. It is zero on a server
	// that has never rotated.
	Previous CookieSecret `json:"previous"`

	// RotatedAt is when Current was minted, and is what says whether the next
	// rotation is due.
	RotatedAt time.Time `json:"rotated_at"`
}

// IsZero reports whether no secret has been minted yet, which is true exactly
// once in a server's life.
func (s CookieSecrets) IsZero() bool { return s.Current.IsZero() }

// Rotate returns the pair that follows this one: a freshly minted secret, with
// the one it replaces kept as the predecessor.
//
// Rotating the zero pair is how the first secret comes into existence, and it
// leaves no predecessor because there is none to keep.
func (s CookieSecrets) Rotate(now time.Time) (CookieSecrets, error) {
	next, err := NewCookieSecret()
	if err != nil {
		return CookieSecrets{}, err
	}
	return CookieSecrets{Current: next, Previous: s.Current, RotatedAt: now.UTC()}, nil
}

// CookieRotation is how often the secret changes.
//
// It is the hour a Server Cookie is honoured for (RFC 9018 §4.3) rather than a
// period somebody picked, and the two are the same number for a reason: an
// hour is the shortest rotation that never takes a cookie away from a client
// still entitled to use it. The secret a cookie was issued under is the
// current one for an hour and the predecessor for an hour after that, by which
// time every cookie built under it has expired on its own.
const CookieRotation = time.Hour

// Due reports whether the secrets are old enough to be rotated at now. A pair
// that was never minted is due immediately, which is how the first one comes
// into existence.
func (s CookieSecrets) Due(now time.Time) bool {
	return s.IsZero() || !now.Before(s.RotatedAt.Add(CookieRotation))
}

// RotateCookieSecrets mints the next secret where the stored pair is due, and
// returns the pair in force afterwards along with whether it changed.
//
// In a cluster this is the leader's to do, for the reason D36 gives about
// probing a secondary: every node rotating on its own schedule would publish
// several answers to one fact, and a client would hold a cookie that half the
// nodes recognise.
func (a *Applier) RotateCookieSecrets(ctx context.Context) (CookieSecrets, bool, error) {
	var stored CookieSecrets
	if err := a.store.View(ctx, func(r store.Reader) error {
		var verr error
		stored, verr = StoredCookieSecrets(ctx, r)
		return verr
	}); err != nil {
		return CookieSecrets{}, false, err
	}

	now := a.now()
	if !stored.Due(now) {
		return stored, false, nil
	}

	next, err := stored.Rotate(now)
	if err != nil {
		return CookieSecrets{}, false, err
	}
	change, cerr := CookieSecretsChange(next)
	if cerr != nil {
		return CookieSecrets{}, false, cerr
	}
	if serr := a.SetSettings(ctx, []SettingChange{change}); serr != nil {
		return CookieSecrets{}, false, serr
	}
	return next, true, nil
}

// StoredCookieSecrets returns the secrets the database holds, or the zero pair
// when none have been minted.
func StoredCookieSecrets(ctx context.Context, r store.Reader) (CookieSecrets, error) {
	raw, err := r.Setting(ctx, CookieSecretSetting)
	if errors.Is(err, store.ErrNotFound) {
		return CookieSecrets{}, nil
	}
	if err != nil {
		return CookieSecrets{}, err
	}

	var secrets CookieSecrets
	if uerr := json.Unmarshal(raw, &secrets); uerr != nil {
		return CookieSecrets{}, fmt.Errorf(
			"apply: the stored cookie secrets are not readable: %w", uerr)
	}
	if secrets.Current.IsZero() {
		return CookieSecrets{}, fmt.Errorf(
			"%w cookie secrets in the database: no current secret", zone.ErrInvalid)
	}
	return secrets, nil
}

// CookieSecretsChange is the change that records a mint or a rotation.
func CookieSecretsChange(s CookieSecrets) (SettingChange, error) {
	if s.Current.IsZero() {
		return SettingChange{}, fmt.Errorf("%w cookie secrets: no current secret", zone.ErrInvalid)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return SettingChange{}, fmt.Errorf("apply: encode the cookie secrets: %w", err)
	}
	return SettingChange{Key: CookieSecretSetting, Value: raw}, nil
}
