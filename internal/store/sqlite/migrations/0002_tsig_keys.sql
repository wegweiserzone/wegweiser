-- TSIG keys: a shared secret a secondary signs its transfer requests with
-- (RFC 8945). See docs/decisions/d28-tsig.md.
--
-- Unlike api_tokens this holds the secret rather than a hash of it, because
-- verifying a signature means recomputing the MAC. Revoking a key clears the
-- column instead of keeping material nothing will read again.
CREATE TABLE tsig_keys (
    id         TEXT    PRIMARY KEY CHECK (length(id) = 26),
    name       TEXT    NOT NULL,              -- key name in domain name syntax, lower case
    algorithm  TEXT    NOT NULL,
    secret     BLOB,                          -- NULL once the key is revoked
    created_at INTEGER NOT NULL,
    revoked_at INTEGER,

    CHECK ((revoked_at IS NULL) = (secret IS NOT NULL))
);

-- Only among the keys that still sign. A revoked key keeps its name in the
-- record, and rotating one an operator has already configured on a secondary
-- should not force them to rename it there.
CREATE UNIQUE INDEX tsig_keys_active_name_uq ON tsig_keys(name) WHERE revoked_at IS NULL;
