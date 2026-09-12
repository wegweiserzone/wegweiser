package cluster

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/wegweiserzone/wegweiser/internal/apply"
)

// Replication is the applier's way to this member ([apply.Replication]), bound
// once the member is up. The applier is built before the member, because the
// member applies the log through it, so the two meet here.
//
// Until it is bound, and while the member holds no Raft state, the node is an
// ordinary server and writes to its own store
// (docs/decisions/d44-starting-and-joining.md).
type Replication struct {
	node atomic.Pointer[Node]
}

var _ apply.Replication = (*Replication)(nil)

// Bind connects the member the applier's writes go through from now on.
func (r *Replication) Bind(n *Node) { r.node.Store(n) }

// Replicating implements [apply.Replication].
func (r *Replication) Replicating() bool {
	n := r.node.Load()
	return n != nil && n.Replicating()
}

// Settle implements [apply.Replication].
func (r *Replication) Settle(ctx context.Context) error {
	n := r.node.Load()
	if n == nil {
		return nil
	}
	return n.Settle(ctx)
}

// Propose implements [apply.Replication].
func (r *Replication) Propose(ctx context.Context, b *apply.Batch) error {
	n := r.node.Load()
	if n == nil {
		return errors.New("cluster: there is no member to propose through yet")
	}
	return n.Propose(ctx, b)
}
