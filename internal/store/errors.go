package store

import "errors"

// The errors every implementation reports, so that no caller ever has to
// recognise a driver error. An implementation wraps these with what was being
// looked for; a caller tests with [errors.Is].
var (
	// ErrNotFound means the requested object does not exist. It is also what a
	// lookup of a revoked or expired token returns, so that a caller cannot
	// tell those apart from an unknown one.
	ErrNotFound = errors.New("store: not found")

	// ErrConflict means the write collided with something already stored: a
	// duplicate resource record within an RRset (RFC 2181 §5), a zone name
	// already in use, or a serial the journal already holds a commit for.
	ErrConflict = errors.New("store: conflict")

	// ErrSchemaTooNew means the database was written by a newer build than this
	// one. Continuing would let an older binary write rows the newer schema
	// expects to be shaped differently, so the store refuses to open instead.
	ErrSchemaTooNew = errors.New("store: schema newer than this build")

	// ErrClosed means the store has been closed.
	ErrClosed = errors.New("store: closed")
)
