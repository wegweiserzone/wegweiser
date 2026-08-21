// Package journal records every change to a zone as an ordered sequence of
// commits.
//
// The journal is not a log kept alongside the data; it is the only way data
// changes (architecture invariant 4). Four of the product's features read from
// this single structure rather than from four mechanisms: the audit log, the
// diff view, rollback, and incremental zone transfer (RFC 1995). A fifth
// arrives with the cluster, where a commit is what Raft replicates: see
// docs/adr/0002-journal-as-command-log.md.
//
// Rejections wrap [zone.ErrInvalid] rather than introducing a second sentinel,
// so a caller that maps a validation failure onto an HTTP 400 has one rule
// instead of one per package.
package journal
