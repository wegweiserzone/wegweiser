-- Initial schema: zones, records and the journal.
--
-- No PRAGMA statements belong here. journal_mode and foreign_keys cannot be set
-- inside a transaction, and foreign_keys is per connection rather than per
-- database, so both are set in the connection string and verified at startup.
--
-- schema_migrations is not created here either: the migrator needs it in order
-- to decide whether this file has run.

-- --------------------------------------------------------------------- zones
CREATE TABLE zones (
    id             TEXT    PRIMARY KEY CHECK (length(id) = 26),
    name           TEXT    NOT NULL,              -- lowercase FQDN, trailing dot
    sort_key       BLOB    NOT NULL,              -- canonical order, RFC 4034 6.1
    kind           TEXT    NOT NULL
                   CHECK (kind IN ('forward','reverse')),

    -- Reverse zones only: the network this zone answers for, derived from the
    -- zone name when the zone is created.
    rev_prefix     BLOB    CHECK (rev_prefix IS NULL OR length(rev_prefix) IN (4,16)),
    rev_prefix_len INTEGER CHECK (rev_prefix_len IS NULL OR rev_prefix_len BETWEEN 0 AND 128),

    -- The SOA is stored as typed columns rather than as a record: its serial
    -- belongs to the journal, not to whoever is editing the zone.
    soa_ns         TEXT    NOT NULL,
    soa_mbox       TEXT    NOT NULL,
    soa_serial     INTEGER NOT NULL CHECK (soa_serial  BETWEEN 0 AND 4294967295),
    soa_refresh    INTEGER NOT NULL CHECK (soa_refresh BETWEEN 0 AND 2147483647),
    soa_retry      INTEGER NOT NULL CHECK (soa_retry   BETWEEN 0 AND 2147483647),
    soa_expire     INTEGER NOT NULL CHECK (soa_expire  BETWEEN 0 AND 2147483647),
    soa_minimum    INTEGER NOT NULL CHECK (soa_minimum BETWEEN 0 AND 2147483647),
    soa_ttl        INTEGER NOT NULL CHECK (soa_ttl     BETWEEN 0 AND 2147483647),

    default_ttl    INTEGER NOT NULL CHECK (default_ttl BETWEEN 0 AND 2147483647),
    auto_reverse   INTEGER CHECK (auto_reverse IN (0,1)),   -- NULL inherits the global setting
    disabled       INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0,1)),
    comment        TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,                        -- unix milliseconds
    updated_at     INTEGER NOT NULL,

    -- A reverse zone has a network and a forward zone does not. Both halves of
    -- the prefix travel together.
    CHECK ((kind = 'reverse') = (rev_prefix IS NOT NULL)),
    CHECK ((rev_prefix IS NULL) = (rev_prefix_len IS NULL))
);

CREATE UNIQUE INDEX zones_name_uq  ON zones(name);
CREATE INDEX        zones_sort_idx ON zones(sort_key);

-- Finding the reverse zone responsible for an address is a longest-prefix
-- match, run once for every address record written.
CREATE INDEX zones_rev_idx ON zones(rev_prefix_len, rev_prefix)
                           WHERE rev_prefix IS NOT NULL;

-- ------------------------------------------------------------------- records
CREATE TABLE records (
    id           TEXT    PRIMARY KEY CHECK (length(id) = 26),
    zone_id      TEXT    NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    sort_key     BLOB    NOT NULL,
    class        INTEGER NOT NULL CHECK (class  BETWEEN 0 AND 65535),
    rrtype       INTEGER NOT NULL CHECK (rrtype BETWEEN 0 AND 65535),
    ttl          INTEGER NOT NULL CHECK (ttl    BETWEEN 0 AND 2147483647),
    rdata        TEXT    NOT NULL,                -- canonical presentation form
    -- Truncated SHA-256 of rdata. The uniqueness constraint has to cover the
    -- data, and a TXT record holds up to 64 KB, which is not a B-tree key.
    rdata_hash   BLOB    NOT NULL CHECK (length(rdata_hash) = 16),

    -- Set for A and AAAA, so reverse automation can ask which names point at an
    -- address without scanning.
    addr         BLOB    CHECK (addr IS NULL OR length(addr) IN (4,16)),

    -- A generated record points at the record it was derived from, and goes
    -- when that one goes.
    managed_by   TEXT    REFERENCES records(id) ON DELETE CASCADE,
    managed_kind TEXT    CHECK (managed_kind IS NULL OR managed_kind IN ('ptr','rfc2317-cname')),

    comment      TEXT    NOT NULL DEFAULT '',
    disabled     INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0,1)),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,

    CHECK ((managed_by IS NULL) = (managed_kind IS NULL))
);

-- No duplicate resource record within an RRset (RFC 2181 section 5).
CREATE UNIQUE INDEX records_rr_uq ON records(zone_id, name, class, rrtype, rdata_hash);

-- Snapshot build and zone export read a zone in canonical order.
CREATE INDEX records_zone_sort_idx ON records(zone_id, sort_key, rrtype);

CREATE INDEX records_addr_idx    ON records(addr)       WHERE addr IS NOT NULL;
CREATE INDEX records_managed_idx ON records(managed_by) WHERE managed_by IS NOT NULL;

-- ----------------------------------------------------------- journal commits
CREATE TABLE journal_commits (
    id          TEXT    PRIMARY KEY CHECK (length(id) = 26),

    -- Deliberately no foreign key to zones. A commit outlives the zone it
    -- describes: the last thing that happens to a zone is that someone deletes
    -- it, and a cascade would erase that record along with the answer to "who
    -- deleted example.com, and when". The name is carried here for the same
    -- reason: once the zone row is gone there is nothing left to join to.
    zone_id     TEXT    NOT NULL CHECK (length(zone_id) = 26),
    zone_name   TEXT    NOT NULL,

    serial_from INTEGER NOT NULL CHECK (serial_from BETWEEN 0 AND 4294967295),
    serial_to   INTEGER NOT NULL CHECK (serial_to   BETWEEN 0 AND 4294967295),
    kind        TEXT    NOT NULL
                CHECK (kind IN ('zone_create','zone_update','zone_delete',
                                'edit','import','rollback')),
    source      TEXT    NOT NULL
                CHECK (source IN ('api','cli','import','system')),
    actor       TEXT    NOT NULL DEFAULT '',
    comment     TEXT    NOT NULL DEFAULT '',
    reverts_to  INTEGER CHECK (reverts_to IS NULL OR reverts_to BETWEEN 0 AND 4294967295),
    event_count INTEGER NOT NULL CHECK (event_count >= 0),
    created_at  INTEGER NOT NULL,

    -- Only a rollback names a serial it restores.
    CHECK ((kind = 'rollback') = (reverts_to IS NOT NULL))
);

-- Exactly one commit per serial step. Restoring a zone to a serial has to name
-- one state, and an incremental transfer has to replay commits one for one.
CREATE UNIQUE INDEX journal_commits_serial_uq ON journal_commits(zone_id, serial_to);
CREATE INDEX journal_commits_zone_time_idx    ON journal_commits(zone_id, created_at);
CREATE INDEX journal_commits_time_idx         ON journal_commits(created_at);

-- ------------------------------------------------------------ journal events
CREATE TABLE journal_events (
    commit_id TEXT    NOT NULL REFERENCES journal_commits(id) ON DELETE CASCADE,
    seq       INTEGER NOT NULL CHECK (seq >= 0),
    op        TEXT    NOT NULL CHECK (op IN ('add','del')),
    name      TEXT    NOT NULL,
    class     INTEGER NOT NULL CHECK (class  BETWEEN 0 AND 65535),
    rrtype    INTEGER NOT NULL CHECK (rrtype BETWEEN 0 AND 65535),
    ttl       INTEGER NOT NULL CHECK (ttl    BETWEEN 0 AND 2147483647),
    rdata     TEXT    NOT NULL,

    PRIMARY KEY (commit_id, seq)
) WITHOUT ROWID;

-- ---------------------------------------------------------------- api tokens
CREATE TABLE api_tokens (
    id           TEXT    PRIMARY KEY CHECK (length(id) = 26),
    name         TEXT    NOT NULL,
    prefix       TEXT    NOT NULL,                -- display only, never the secret
    hash         BLOB    NOT NULL CHECK (length(hash) = 32),
    scopes       TEXT    NOT NULL DEFAULT '[]',   -- JSON array
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    expires_at   INTEGER,
    revoked_at   INTEGER
);
CREATE UNIQUE INDEX api_tokens_hash_uq ON api_tokens(hash);

-- ------------------------------------------------------------------ settings
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,                     -- JSON
    updated_at INTEGER NOT NULL
) WITHOUT ROWID;
