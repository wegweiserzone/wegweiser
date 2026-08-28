package store

import (
	"time"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// TSIGKeyID identifies a TSIG key.
type TSIGKeyID string

// TSIGKey is a secret shared with a secondary, which signs its transfer
// requests with it and verifies what comes back (RFC 8945).
//
// It lives here for the same reason [Token] does: what interprets it is spread
// across packages that may not import one another, and a package holding one
// struct buys nothing.
type TSIGKey struct {
	ID TSIGKeyID

	// Name is the key name both ends agree on, in domain name syntax
	// (RFC 8945 §4.2). It is compared case-insensitively (§9), which is what
	// [zone.Name] does anyway.
	Name zone.Name

	Algorithm zone.TSIGAlgorithm

	// Secret is the shared secret, held as it is used rather than as a hash of
	// itself. Verifying a signature means recomputing the MAC, so there is no
	// arrangement in which this database holds a usable key and cannot hand it
	// over; docs/decisions/d28-tsig.md says what follows for the file.
	//
	// It is empty for a revoked key, whose secret is cleared rather than kept.
	Secret []byte

	CreatedAt time.Time
	// RevokedAt is zero for a key that still signs.
	RevokedAt time.Time
}

// Active reports whether the key may still sign or verify.
func (k TSIGKey) Active() bool { return k.RevokedAt.IsZero() && len(k.Secret) > 0 }
