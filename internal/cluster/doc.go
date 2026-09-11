// Package cluster is the part of Wegweiser that several servers share: the
// transport members talk over (docs/decisions/d43-the-cluster-transport.md),
// and the Raft node that carries the replicated log across it and applies each
// entry to the store (docs/decisions/d24-what-the-cluster-replicates.md).
//
// None of it runs unless a cluster is configured. A single node never listens
// on the cluster port and never dials anybody.
package cluster
