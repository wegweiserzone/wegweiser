-- Who this node is as a member of a cluster. Node-local: the identifier is a
-- statement about this machine rather than about the zones, so no log snapshot
-- carries it and restoring one does not replace it. Why it is minted once and
-- never changes: docs/decisions/d42-membership-lives-in-the-log.md.
--
-- One row at most. There is none until the node first starts in a cluster, and
-- on a single node there never is one.
CREATE TABLE node_identity (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    member_id  TEXT    NOT NULL CHECK (length(member_id) > 0),
    created_at INTEGER NOT NULL
);
