package store

import (
	"context"
	"iter"
	"net/netip"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Backend names a persistence implementation.
type Backend string

const (
	// BackendSQLite is the embedded default, one file, no server.
	BackendSQLite Backend = "sqlite"
	// BackendPostgres is the optional backend for large deployments.
	BackendPostgres Backend = "postgres"
)

// Capabilities reports what a backend can do beyond the common interface, so a
// caller can adapt without asserting on the concrete type.
type Capabilities struct {
	Backend Backend

	// ConcurrentWriters is false for SQLite, which serializes writers itself.
	// It tells the applier whether its per-zone lock orders writes or merely
	// sits in front of the database's own ordering.
	ConcurrentWriters bool

	// ListenNotify is true when the backend can push change notifications
	// instead of being polled. False for SQLite.
	ListenNotify bool
}

// Reader is the read-only surface of the store, satisfied by both [Store] and
// [Tx] so a helper written against it works inside or outside a transaction.
//
// A lookup that finds nothing returns [ErrNotFound], never a nil result. The
// streaming methods yield each item with a nil error; on failure they yield a
// nil item with the error and stop, so the error must be tested on every step.
type Reader interface {
	// ZoneByID returns one zone.
	ZoneByID(ctx context.Context, id zone.ZoneID) (*zone.Zone, error)

	// ZoneByName returns the zone whose apex is exactly name. It does not walk
	// up looking for an enclosing zone: which zone answers a query is decided
	// against the snapshot, never the database (invariant 2).
	ZoneByName(ctx context.Context, name zone.Name) (*zone.Zone, error)

	// ListZones returns one page of zones in canonical name order.
	ListZones(ctx context.Context, f ZoneFilter) (Page[*zone.Zone], error)

	// IterZones streams every zone, disabled ones included, in canonical name
	// order. It exists for the snapshot rebuild, which wants each zone once and
	// has no reason to carry a cursor.
	IterZones(ctx context.Context) iter.Seq2[*zone.Zone, error]

	// ReverseZoneFor returns the most specific reverse zone covering addr,
	// which is where an address record's PTR belongs. Longest prefix wins, so a
	// classless RFC 2317 child beats the /24 delegating to it.
	ReverseZoneFor(ctx context.Context, addr netip.Addr) (*zone.Zone, error)

	// RecordByID returns one record.
	RecordByID(ctx context.Context, id zone.RecordID) (*zone.Record, error)

	// ListRecords returns one page of records in canonical order.
	ListRecords(ctx context.Context, f RecordFilter) (Page[*zone.Record], error)

	// IterZoneRecords streams every record of a zone, disabled ones included,
	// in canonical order. It must not materialize the zone: a large one holds
	// millions of records.
	IterZoneRecords(ctx context.Context, id zone.ZoneID) iter.Seq2[*zone.Record, error]

	// RecordsByAddress returns every A and AAAA record pointing at addr, across
	// all zones. Reverse automation asks before generating a second PTR.
	RecordsByAddress(ctx context.Context, addr netip.Addr) ([]*zone.Record, error)

	// ManagedBy returns the records generated from the given source record.
	ManagedBy(ctx context.Context, id zone.RecordID) ([]*zone.Record, error)

	// ManagedByZone streams the records generated from any record of a zone and
	// living somewhere else, in canonical order. Deleting a zone takes them
	// with it, written out rather than cascaded, because the removals belong to
	// the journal of the zone they are in.
	ManagedByZone(ctx context.Context, id zone.ZoneID) iter.Seq2[*zone.Record, error]

	// CommitByID returns one journal commit with its events. Not named Commit:
	// this interface is embedded in [Tx], where that would read as the method
	// ending the transaction.
	CommitByID(ctx context.Context, id journal.CommitID) (*journal.Commit, error)

	// ListCommits returns one page of commit metadata, newest first and without
	// events. The events of one commit are fetched by identifier.
	ListCommits(ctx context.Context, f CommitFilter) (Page[*journal.Commit], error)

	// TokenByHash resolves an API token by the SHA-256 of its secret. Unknown,
	// revoked and expired tokens all return [ErrNotFound], so a caller cannot
	// tell which of the three it hit.
	TokenByHash(ctx context.Context, hash []byte) (*Token, error)

	// TSIGKeyByName resolves the key a transfer request names. A revoked key
	// is not found: its secret is gone and nothing can be verified with it.
	TSIGKeyByName(ctx context.Context, name zone.Name) (*TSIGKey, error)

	// TSIGKeyByID returns one key, revoked or not, which is how a client asks
	// to see a secret it has already been shown once.
	TSIGKeyByID(ctx context.Context, id TSIGKeyID) (*TSIGKey, error)

	// ListTSIGKeys returns every key, revoked ones included, so the interface
	// can show what a secondary used to sign with. A revoked key carries no
	// secret.
	ListTSIGKeys(ctx context.Context) ([]*TSIGKey, error)

	// ListTokens returns every token, including revoked and expired ones.
	// Secrets are not stored and cannot be returned.
	ListTokens(ctx context.Context) ([]*Token, error)

	// Setting returns one JSON-encoded setting value.
	Setting(ctx context.Context, key string) ([]byte, error)
}

// Writer is the mutating surface, reachable only through [Store.Update], so
// nothing writes outside a transaction. Methods taking a pointer write back the
// timestamps the store owns.
type Writer interface {
	// CreateZone stores a new zone. It returns [ErrConflict] if the apex is
	// already taken.
	CreateZone(ctx context.Context, z *zone.Zone) error

	// UpdateZone replaces a zone's settings. It does not touch its records.
	UpdateZone(ctx context.Context, z *zone.Zone) error

	// DeleteZone removes a zone together with its records and its journal.
	DeleteZone(ctx context.Context, id zone.ZoneID) error

	// SetZoneSerial advances a zone serial. Separate from UpdateZone because
	// the serial belongs to the journal, not to whoever is editing the zone.
	SetZoneSerial(ctx context.Context, id zone.ZoneID, serial zone.Serial) error

	// InsertRecord stores a new record. It returns [ErrConflict] if an
	// identical resource record is already in the same RRset (RFC 2181 §5).
	InsertRecord(ctx context.Context, r *zone.Record) error

	// UpdateRecord replaces a record in place, keeping its identity so its
	// comment, history and generated PTR stay attached.
	UpdateRecord(ctx context.Context, r *zone.Record) error

	// DeleteRecord removes one record, and with it anything generated from it.
	DeleteRecord(ctx context.Context, id zone.RecordID) error

	// DeleteRRset removes every record of one owner name, class and type. The
	// RRset is the unit DNS operates on (RFC 2181 §5), and enumerating members
	// first would race with whoever adds one in between.
	DeleteRRset(ctx context.Context, id zone.ZoneID, key zone.RRsetKey) error

	// AppendCommit stores a commit and its events. It returns [ErrConflict] if
	// the zone already has a commit producing that serial, which is the last
	// line of defence for one commit per serial step.
	AppendCommit(ctx context.Context, c *journal.Commit) error

	// CreateToken stores a new API token.
	CreateToken(ctx context.Context, t *Token) error

	// CreateTSIGKey stores a new key.
	CreateTSIGKey(ctx context.Context, k *TSIGKey) error

	// RevokeTSIGKey marks a key unusable and clears its secret. The row stays
	// so the name and the dates can still be read; the material does not,
	// because nothing would read it again.
	RevokeTSIGKey(ctx context.Context, id TSIGKeyID, at time.Time) error

	// RevokeToken marks a token unusable. Tokens are not deleted, so the audit
	// log keeps naming the token behind each change.
	RevokeToken(ctx context.Context, id TokenID, at time.Time) error

	// TouchToken records that a token authenticated a request. It is advisory:
	// an implementation may batch or drop these writes, and no caller may
	// depend on the value it reads back.
	TouchToken(ctx context.Context, id TokenID, at time.Time) error

	// PutSetting stores a JSON-encoded setting value, replacing any previous
	// one.
	PutSetting(ctx context.Context, key string, value []byte) error
}

// Tx is a read-write transaction. Reads inside it see its own uncommitted
// writes.
type Tx interface {
	Reader
	Writer
}

// Store is a persistence backend.
type Store interface {
	Reader

	// Update runs fn inside one write transaction, committing if fn returns nil
	// and rolling back otherwise. Writers are serialized: SQLite by
	// construction, Postgres by asking.
	//
	// The Tx must not outlive fn or be used from another goroutine.
	Update(ctx context.Context, fn func(tx Tx) error) error

	// View runs fn inside one read transaction, for a caller that needs several
	// reads to see the same state.
	View(ctx context.Context, fn func(r Reader) error) error

	// Migrate brings the schema up to what this build expects. It returns
	// [ErrSchemaTooNew] if the database was written by a newer build.
	Migrate(ctx context.Context) error

	// Ping checks that the backend is reachable. It is what /healthz asks.
	Ping(ctx context.Context) error

	// Capabilities reports optional backend features.
	Capabilities() Capabilities

	// Close releases the backend. Transactions still running are rolled back.
	Close() error
}
