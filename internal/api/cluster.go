package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cluster"
)

// GetCluster says who the members are, as this node knows them, and how far
// each has got (docs/decisions/d42-membership-lives-in-the-log.md). The
// others are asked as it answers, unless another member is the one asking
// (docs/decisions/d47-status-asks-every-member.md).
func (s *Server) GetCluster(
	ctx context.Context, _ gen.GetClusterRequestObject,
) (gen.GetClusterResponseObject, error) {
	if s.cluster == nil {
		return nil, noClusterSection()
	}
	st, err := s.cluster.Status(ctx)
	if err != nil {
		return nil, err
	}
	id, addr := s.cluster.Member()
	out := gen.GetCluster200JSONResponse{
		Self:        gen.ClusterMember{Id: id, Address: addr},
		Replicating: s.cluster.Replicating(),
		Removed:     st.Removed,
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
	if ctx.Value(viaClusterPort{}) == nil {
		s.askMembers(ctx, out.Members, gen.ClusterStatus(out))
		if out.Replicating && !out.Removed && out.Behind == nil {
			out.Quorum = quorumOf(out.Members)
		}
	}
	return out, nil
}

// quorumOf counts the voters, how many make a majority, and how many of them
// answered and still take part. A witness votes like any voter (D39).
func quorumOf(members []gen.ClusterMemberState) *gen.ClusterQuorum {
	var q gen.ClusterQuorum
	for i := range members {
		m := &members[i]
		if m.Role == gen.ClusterMemberStateRoleNonvoter {
			continue
		}
		q.Voters++
		if p := m.Progress; p != nil && !p.Removed && p.Behind == nil {
			q.Answered++
		}
	}
	if q.Voters == 0 {
		return nil
	}
	q.Needed = q.Voters/2 + 1
	return &q
}

// memberWait bounds how long a status waits for another member to answer.
const memberWait = 2 * time.Second

// askMembers fills in how far each member has got: this one from what it
// knows itself, every other one from what it answers over the cluster port,
// all of them at once.
func (s *Server) askMembers(ctx context.Context, members []gen.ClusterMemberState, self gen.ClusterStatus) {
	identity, err := forwardedAs(subjectOf(ctx))
	if err != nil {
		s.report(err)
		return
	}
	client := &http.Client{Transport: s.forwardTransport, Timeout: memberWait}
	var wg sync.WaitGroup
	for i := range members {
		m := &members[i]
		if m.Id == self.Self.Id {
			m.Progress = progressOf(self)
			continue
		}
		wg.Go(func() {
			got, aerr := askMember(ctx, client, m.Address, identity)
			if aerr != nil {
				trouble := aerr.Error()
				m.Trouble = &trouble
				return
			}
			m.Progress = progressOf(got)
		})
	}
	wg.Wait()
}

// askMember asks the member at addr for its own status.
func askMember(ctx context.Context, client *http.Client, addr, identity string) (gen.ClusterStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+basePath+"/cluster", http.NoBody)
	if err != nil {
		return gen.ClusterStatus{}, err
	}
	req.Header.Set(forwardedHeader, identity)
	resp, err := client.Do(req)
	if err != nil {
		return gen.ClusterStatus{}, notReached(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var problem gen.Problem
		if derr := json.NewDecoder(resp.Body).Decode(&problem); derr != nil || problem.Detail == nil {
			return gen.ClusterStatus{}, fmt.Errorf("it answered %s", resp.Status)
		}
		return gen.ClusterStatus{}, errors.New(*problem.Detail)
	}
	var st gen.ClusterStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return gen.ClusterStatus{}, fmt.Errorf("its answer could not be read: %w", err)
	}
	return st, nil
}

// notReached says in a few words why a member could not be asked. The whole
// chain names the URL, the transport and the dial, and the reader wants only
// the last of those.
func notReached(err error) error {
	var timeout net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return errors.New("not reached: connection refused")
	case errors.As(err, &timeout) && timeout.Timeout():
		return fmt.Errorf("not reached: no answer within %s", memberWait)
	}
	for inner := errors.Unwrap(err); inner != nil; inner = errors.Unwrap(err) {
		err = inner
	}
	return fmt.Errorf("not reached: %w", err)
}

func progressOf(st gen.ClusterStatus) *gen.ClusterProgress {
	return &gen.ClusterProgress{
		Applied: st.Applied, Committed: st.Committed, Removed: st.Removed, Behind: st.Behind,
	}
}

// noClusterSection is a cluster endpoint asked of a single server.
func noClusterSection() *apiError {
	return &apiError{
		status: http.StatusNotFound,
		kind:   typeNotFound,
		title:  "Not found",
		detail: "this node has no cluster section in its configuration file, and is a single server",
	}
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

// RemoveClusterMember takes a member out of the cluster. The request reaches
// the leader wherever it was sent (docs/decisions/d44-starting-and-joining.md).
func (s *Server) RemoveClusterMember(
	ctx context.Context, req gen.RemoveClusterMemberRequestObject,
) (gen.RemoveClusterMemberResponseObject, error) {
	if err := requireAdmin(ctx, "removing a cluster member"); err != nil {
		return nil, err
	}
	if s.cluster == nil {
		return nil, noClusterSection()
	}
	if err := s.cluster.Remove(req.MemberId); err != nil {
		return nil, err
	}
	return gen.RemoveClusterMember204Response{}, nil
}
