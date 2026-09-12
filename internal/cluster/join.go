package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Role is what a node joins as (docs/decisions/d25-cluster-shape.md).
type Role string

const (
	// RoleVoter counts towards quorum and can lead.
	RoleVoter Role = "voter"
	// RoleNonvoter receives the whole log and answers queries like any member,
	// and neither votes nor is waited for.
	RoleNonvoter Role = "nonvoter"
)

func (r Role) valid() bool { return r == RoleVoter || r == RoleNonvoter }

// JoinRequest is what a node sends to be made a member
// (docs/decisions/d44-starting-and-joining.md).
type JoinRequest struct {
	// ID is the identifier the node is a member by (D42).
	ID string `json:"id"`
	// Address is where the other members reach it, as host:port.
	Address string `json:"address"`
	// Role is what it joins as.
	Role Role `json:"role"`
}

func (r JoinRequest) check() error {
	switch {
	case r.ID == "":
		return errors.New("cluster: a node asking to join names no identifier")
	case !r.Role.valid():
		return fmt.Errorf("cluster: %q is not a role a node can join as; it joins as %s or %s",
			r.Role, RoleVoter, RoleNonvoter)
	}
	if _, _, err := net.SplitHostPort(r.Address); err != nil {
		return fmt.Errorf("cluster: a node asking to join has to say where it is reached, as host:port: %w", err)
	}
	return nil
}

// joinReply is the answer: done, ask the leader at this address, or no.
type joinReply struct {
	Done   bool   `json:"done,omitempty"`
	Leader string `json:"leader,omitempty"`
	Error  string `json:"error,omitempty"`
}

// joinHops bounds how often a node follows "ask the leader". Leadership can
// move while a node is asking, so one hop is not always enough, and a bound
// keeps two members that disagree about who leads from sending it in circles.
const joinHops = 5

// joinTimeout bounds one attempt. The member that leads answers once the
// addition is committed, which can take as long as a membership change is
// given.
const joinTimeout = membershipTimeout + 5*time.Second

// ErrNoLeader is a member that could not say who leads.
var ErrNoLeader = errors.New("cluster: the member asked knows of no leader")

// Join asks the member at addr to make this node a member, follows it to the
// leader when it does not lead, and returns once the addition is committed.
func Join(ctx context.Context, t *Transport, addr string, req JoinRequest) error {
	if err := req.check(); err != nil {
		return err
	}
	target := addr
	for range joinHops {
		reply, err := askToJoin(ctx, t, target, req)
		if err != nil {
			return err
		}
		switch {
		case reply.Done:
			return nil
		case reply.Error != "":
			return fmt.Errorf("cluster: %s would not add this node: %s", target, reply.Error)
		case reply.Leader != "":
			target = reply.Leader
		default:
			return fmt.Errorf("%w (%s)", ErrNoLeader, target)
		}
	}
	return fmt.Errorf("cluster: sent on %d times without reaching a member that leads; "+
		"the members disagree about who does", joinHops)
}

// askToJoin puts the request to one member and reads its answer.
func askToJoin(ctx context.Context, t *Transport, addr string, req JoinRequest) (_ joinReply, err error) {
	ctx, cancel := context.WithTimeout(ctx, joinTimeout)
	defer cancel()

	conn, err := t.Dial(ctx, addr, StreamJoin)
	if err != nil {
		return joinReply{}, err
	}
	defer func() { err = errors.Join(err, closeQuietly(conn)) }()
	if deadline, ok := ctx.Deadline(); ok {
		if derr := conn.SetDeadline(deadline); derr != nil {
			return joinReply{}, derr
		}
	}

	if eerr := json.NewEncoder(conn).Encode(req); eerr != nil {
		return joinReply{}, fmt.Errorf("cluster: ask %s to join: %w", addr, eerr)
	}
	var reply joinReply
	if derr := json.NewDecoder(conn).Decode(&reply); derr != nil {
		return joinReply{}, fmt.Errorf("cluster: read %s's answer to the join: %w", addr, derr)
	}
	return reply, nil
}

// serveJoins answers nodes asking to be made members, for as long as this
// member runs.
func (n *Node) serveJoins(l net.Listener) {
	defer n.wg.Done()
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.answerJoin(conn)
		}()
	}
}

func (n *Node) answerJoin(conn net.Conn) {
	defer func() {
		if err := closeQuietly(conn); err != nil {
			n.report(fmt.Errorf("cluster: close a join stream: %w", err))
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(joinTimeout)); err != nil {
		n.report(fmt.Errorf("cluster: a join stream: %w", err))
		return
	}

	var req JoinRequest
	reply := joinReply{}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		reply.Error = "the request could not be read: " + err.Error()
	} else {
		reply = n.admit(req)
	}
	if err := json.NewEncoder(conn).Encode(reply); err != nil {
		n.report(fmt.Errorf("cluster: answer a join from %s: %w", conn.RemoteAddr(), err))
	}
}

// admit decides one request to join. A member that leads proposes the
// addition and answers once it is committed; one that does not names the
// member that does (D44).
func (n *Node) admit(req JoinRequest) joinReply {
	if s, ok := n.Stalled(); ok {
		return joinReply{Error: s.Err().Error()}
	}
	if err := req.check(); err != nil {
		return joinReply{Error: err.Error()}
	}
	if !n.IsLeader() {
		if _, addr := n.Leader(); addr != "" {
			return joinReply{Leader: addr}
		}
		return joinReply{Error: ErrNoLeader.Error()}
	}

	var err error
	switch req.Role {
	case RoleNonvoter:
		err = n.AddNonvoter(req.ID, req.Address)
	default:
		err = n.AddVoter(req.ID, req.Address)
	}
	switch {
	case errors.Is(err, ErrNotLeader):
		// Leadership moved while the change was being proposed.
		if _, addr := n.Leader(); addr != "" {
			return joinReply{Leader: addr}
		}
		return joinReply{Error: ErrNoLeader.Error()}
	case err != nil:
		return joinReply{Error: err.Error()}
	}
	return joinReply{Done: true}
}
