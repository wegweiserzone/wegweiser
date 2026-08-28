-- Where in the replicated log this node has got to, written in the same
-- transaction as the batch it belongs to. Why it has to be the same
-- transaction: docs/decisions/d24-what-the-cluster-replicates.md.
--
-- One row at most. There is none until the first replicated batch arrives, and
-- on a single node there never is one.
CREATE TABLE applied_index (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    log_index  INTEGER NOT NULL CHECK (log_index > 0),
    updated_at INTEGER NOT NULL
);
