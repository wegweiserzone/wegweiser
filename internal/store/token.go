package store

import "time"

// TokenID identifies an API token.
type TokenID string

// Token is an API credential.
//
// It lives in this package rather than beside the authentication code because
// the authentication code lives in internal/api, and nothing may import that
// (architecture invariant 1). The alternative, a package holding one struct,
// buys nothing.
type Token struct {
	ID   TokenID
	Name string

	// Prefix is the leading characters of the secret, kept so the UI and the
	// audit log can tell two tokens apart without holding either.
	Prefix string
	// Hash is the SHA-256 of the secret. Not a slow password hash: a token is
	// 256 bits from crypto/rand and is not brute-forceable whatever the hash
	// costs, so a slow one would only rate-limit our own request path. See
	// data model §4.7.
	Hash []byte

	// Scopes are the permissions granted. The vocabulary belongs to the API,
	// which is the only thing that interprets them.
	Scopes []string

	CreatedAt time.Time
	// LastUsedAt is zero for a token that has never authenticated a request.
	// It is written advisorily and may lag.
	LastUsedAt time.Time
	// ExpiresAt is zero for a token that does not expire.
	ExpiresAt time.Time
	// RevokedAt is zero for a token that is still valid.
	RevokedAt time.Time
}

// Active reports whether the token may authenticate a request at the given
// time. A caller still has to check the scopes.
func (t Token) Active(at time.Time) bool {
	if !t.RevokedAt.IsZero() {
		return false
	}
	// Expiry is exclusive: a token expiring at 12:00 does not work at 12:00.
	return t.ExpiresAt.IsZero() || at.Before(t.ExpiresAt)
}
