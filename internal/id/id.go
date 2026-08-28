// Package id generates the identifiers Wegweiser uses as primary keys.
//
//   - it can be assigned before the transaction that stores it, so a caller can
//     build a whole object graph and write it in one batch;
//   - it stays unique when nodes merge their state once the cluster arrives;
//   - it is safe to put in a URL, because it leaks no row count.
package id

import (
	"crypto/rand"

	"github.com/oklog/ulid/v2"
)

// Size is the length of an identifier in its canonical text form.
const Size = ulid.EncodedSize

// New returns a fresh identifier.
//
// New panics if the operating system's entropy source fails, which is not a
// condition a server can sensibly continue past.
func New() string {
	return ulid.MustNew(ulid.Now(), rand.Reader).String()
}

// Valid reports whether s is an identifier in canonical form.
func Valid(s string) bool {
	parsed, err := ulid.ParseStrict(s)
	return err == nil && parsed.String() == s
}
