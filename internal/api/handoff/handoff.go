// Package handoff carries a session from the server to `weg login` through
// the browser, sealed to a key the CLI made for that one sign-in
// (docs/decisions/d52-weg-login-holds-a-session.md).
//
// The server and the CLI both import it, so that the two halves of the seal
// cannot drift apart. It holds the format and the cryptography and nothing
// else: no transport, no cookies.
package handoff

import (
	"crypto/ecdh"
	"crypto/hpke"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// info binds a sealed value to this use, so that nothing sealed for another
// purpose under the same suite opens as a session (RFC 9180 §5.1).
const info = "wegweiser sign-in handoff"

// Session is what a sign-in hands the CLI: the sealed session cookie's value,
// the CSRF value a write has to repeat, and when both stop working.
type Session struct {
	Value   string    `json:"session"`
	CSRF    string    `json:"csrf"`
	Expires time.Time `json:"expires"`
}

// The suite: X25519 for the key, HKDF-SHA256 and AES-256-GCM. A post-quantum
// KEM would put a kilobyte and more into a URL for a value that is worth
// nothing after twelve hours.
func kem() hpke.KEM   { return hpke.DHKEM(ecdh.X25519()) }
func kdf() hpke.KDF   { return hpke.HKDFSHA256() }
func aead() hpke.AEAD { return hpke.AES256GCM() }

// Key is the CLI's half: made for one sign-in and dropped after it.
type Key struct{ priv hpke.PrivateKey }

// NewKey makes a key for one sign-in.
func NewKey() (*Key, error) {
	priv, err := kem().GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("handoff: generate a key: %w", err)
	}
	return &Key{priv: priv}, nil
}

// Public is the key the server seals to, in a form a URL may carry.
func (k *Key) Public() string {
	return base64.RawURLEncoding.EncodeToString(k.priv.PublicKey().Bytes())
}

// Open reads what Seal wrote to this key.
func (k *Key) Open(sealed string) (*Session, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return nil, errors.New("handoff: the sealed session is not base64")
	}
	plain, err := hpke.Open(k.priv, kdf(), aead(), []byte(info), raw)
	if err != nil {
		return nil, errors.New("handoff: the sealed session does not open under this sign-in's key")
	}
	var s Session
	if err := json.Unmarshal(plain, &s); err != nil {
		return nil, fmt.Errorf("handoff: the sealed session is not readable: %w", err)
	}
	return &s, nil
}

// ValidPublic reports whether public is a key Seal can seal to, so that a
// sign-in can be refused at its start rather than after the person has
// signed in.
func ValidPublic(public string) bool {
	_, err := publicKey(public)
	return err == nil
}

// Seal writes s to the public key, in a form a URL may carry.
func Seal(public string, s *Session) (string, error) {
	pub, err := publicKey(public)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("handoff: encode the session: %w", err)
	}
	sealed, err := hpke.Seal(pub, kdf(), aead(), []byte(info), plain)
	if err != nil {
		return "", fmt.Errorf("handoff: seal the session: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func publicKey(public string) (hpke.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(public)
	if err != nil {
		return nil, errors.New("handoff: the key is not base64")
	}
	pub, err := kem().NewPublicKey(raw)
	if err != nil {
		return nil, errors.New("handoff: the key is not an X25519 public key")
	}
	return pub, nil
}
