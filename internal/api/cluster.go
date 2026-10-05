package api

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cluster"
)

// GetCluster says who the members are, as this node knows them, and how far
// it has got itself (docs/decisions/d42-membership-lives-in-the-log.md).
func (s *Server) GetCluster(
	ctx context.Context, _ gen.GetClusterRequestObject,
) (gen.GetClusterResponseObject, error) {
	if s.cluster == nil {
		return nil, &apiError{
			status: http.StatusNotFound,
			kind:   typeNotFound,
			title:  "Not found",
			detail: "this node has no cluster section in its configuration file, and is a single server",
		}
	}
	st, err := s.cluster.Status(ctx)
	if err != nil {
		return nil, err
	}
	id, addr := s.cluster.Member()
	out := gen.GetCluster200JSONResponse{
		Self:        gen.ClusterMember{Id: id, Address: addr},
		Replicating: s.cluster.Replicating(),
		Applied:     logIndex(st.Applied),
		Committed:   logIndex(st.Committed),
		Members:     make([]gen.ClusterMemberState, 0, len(st.Members)),
	}
	for _, m := range st.Members {
		out.Members = append(out.Members, gen.ClusterMemberState{
			Id: m.ID, Address: m.Address, Role: gen.ClusterMemberStateRole(m.Role), Leader: m.Leader,
		})
	}
	if st.Stall != nil {
		out.Behind = &gen.ClusterStall{
			Entry: logIndex(st.Stall.Entry), Reason: st.Stall.Reason.Error(), Since: st.Stall.At,
		}
	}
	return out, nil
}

// logIndex puts a log position in the API's integer. A log that reached 2^63
// entries would have taken longer than anybody will run this server.
func logIndex(i uint64) int64 {
	return int64(min(i, math.MaxInt64)) // #nosec G115 -- bounded on the line itself
}

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
