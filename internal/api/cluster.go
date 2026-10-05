package api

import (
	"context"
	"errors"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cluster"
)

// InitCluster starts a cluster with this node as its first member
// (docs/decisions/d44-starting-and-joining.md).
func (s *Server) InitCluster(
	ctx context.Context, _ gen.InitClusterRequestObject,
) (gen.InitClusterResponseObject, error) {
	if err := requireAdmin(ctx, "starting a cluster"); err != nil {
		return nil, err
	}
	if s.cluster == nil {
		return nil, conflict("this node has no cluster section in its configuration file, " +
			"so it has no address for other members to reach it at and no secret to prove they belong; " +
			"add one and restart it")
	}

	// Everything the store holds goes into the cluster through the log
	// snapshot Init ends with, so no write may land between the two.
	err := s.applier.Exclusive(ctx, s.cluster.Init)
	switch {
	case errors.Is(err, cluster.ErrMember):
		return nil, conflict("this node is a member of a cluster already; a cluster is started once")
	case err != nil:
		return nil, err
	}
	id, addr := s.cluster.Member()
	return gen.InitCluster200JSONResponse{Id: id, Address: addr}, nil
}
