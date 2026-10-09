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
	// RoleWitness votes and keeps the log, and applies none of it
	// (docs/decisions/d39-the-witness.md). Only wegwitness joins as one.
	RoleWitness Role = "witness"
)

func (r Role) valid() bool { return r == RoleVoter || r == RoleNonvoter || r == RoleWitness }

// JoinRequest is what a node sends to be made a member
// (docs/decisions/d44-starting-and-joining.md).
type JoinRequest struct {
	// ID is the identifier the node is a member by (D42).
	ID string `json:"id"`
	// Address is where the other members reach it, as host:port.
	Address string `json:"address"`
	// Role is what it joins as.
	Role Role `json:"role"`
	// HoldsLog says the node has been sent the log and applied it. A voter
	// asks once without it and is added without a vote, and asks again with
	// it to be given one.
	HoldsLog bool `json:"holdsLog,omitempty"`
}

func (r JoinRequest) check() error {
	switch {
	case r.ID == "":
		return errors.New("cluster: a node asking to join names no identifier")
	case !r.Role.valid():
		return fmt.Errorf("cluster: %q is not a role a node can join as; it joins as %s, %s or %s",
			r.Role, RoleVoter, RoleNonvoter, RoleWitness)
	case IsWitness(r.ID) != (r.Role == RoleWitness):
		return fmt.Errorf("cluster: %s asks to join as a %s; a witness joins as one, under an identifier "+
			"beginning %q, and nothing else does (docs/decisions/d48-a-witness-is-known-by-its-identifier.md)",
			r.ID, r.Role, WitnessPrefix)
	}
	if _, _, err := net.SplitHostPort(r.Address); err != nil {
		return fmt.Errorf("cluster: a node asking to join has to say where it is reached, as host:port: %w", err)
	}
	return nil
}

// joinReply is the answer: done, added without a vote for now, ask the leader
// at this address, or no.
type joinReply struct {
	Done   bool   `json:"done,omitempty"`
	Staged bool   `json:"staged,omitempty"`
	Leader string `json:"leader,omitempty"`
	Error  string `json:"error,omitempty"`
	// Held is how far the leader's store had got when it answered. The node
	// waits to get as far before Join returns: its own commit index can trail
	// the leader's, and a snapshot older than the last write satisfies it.
	// Zero from a leader that does not send it, and then nothing is waited for.
	Held uint64 `json:"held,omitempty"`
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

// logWait bounds how long a node that has been added waits for the log to
// reach it. What arrives first is a log snapshot of the whole store, so this
// is a bound on a member that cannot be reached rather than on a large one.
const logWait = time.Minute

// Join asks the member at addr to make this node a member, follows it to the
// leader when it does not lead, and returns once this node is one and has
// applied the log it was sent.
//
// A voter is added without a vote first, and asks for one once the log has
// reached it, so that a node the leader cannot reach never counts towards
// quorum (docs/decisions/d44-starting-and-joining.md, where it stands).
func (n *Node) Join(ctx context.Context, addr string, role Role) error {
	req := JoinRequest{ID: string(n.id), Address: string(n.addr), Role: role}
	if err := req.check(); err != nil {
		return err
	}
	target := addr
	for range joinHops {
		reply, err := askToJoin(ctx, n.tr, target, req)
		if err != nil {
			return err
		}
		switch {
		case reply.Error != "":
			return fmt.Errorf("cluster: %s would not add this node: %s", target, reply.Error)
		case reply.Leader != "":
			target = reply.Leader
		case reply.Staged || reply.Done:
			if werr := n.awaitLog(ctx, reply.Held); werr != nil {
				return werr
			}
			if reply.Done {
				return nil
			}
			req.HoldsLog = true
		default:
			return fmt.Errorf("%w (%s)", ErrNoLeader, target)
		}
	}
	return fmt.Errorf("cluster: sent on %d times without reaching a member that leads; "+
		"the members disagree about who does", joinHops)
}

// awaitLog returns once this member has applied everything it knows to be
// committed, has been sent something to apply at all, and its store holds
// at least what the leader's held when it answered.
//
// The store's own index is asked as well as Raft's, because Raft counts an
// entry as applied once it is handed over rather than once it is written, and
// a restored log snapshot is what moves the store's.
func (n *Node) awaitLog(ctx context.Context, want uint64) error {
	ctx, cancel := context.WithTimeout(ctx, logWait)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if s, ok := n.Stalled(); ok {
			return s.Err()
		}
		held, err := n.machine.store.AppliedIndex(ctx)
		if err != nil && ctx.Err() == nil {
			return err
		}
		committed := n.raft.CommitIndex()
		if held > 0 && held >= want && committed > 0 && n.raft.AppliedIndex() >= committed {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cluster: this node was added to the cluster and the log has not reached it "+
				"in %s; check that the members can reach it at %s: %w", logWait, n.addr, ctx.Err())
		case <-tick.C:
		}
	}
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
// member that does (D44). A voter is staged first, as [Node.Join] describes.
func (n *Node) admit(req JoinRequest) joinReply {
	if err := n.Left(); err != nil {
		return joinReply{Error: err.Error()}
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

	// A witness that could never be given its vote is not staged either.
	if req.Role == RoleWitness {
		if err := n.keepsWitnessesFew(req.ID, true); err != nil {
			return joinReply{Error: err.Error()}
		}
	}

	var err error
	reply := joinReply{Done: true}
	switch {
	case req.Role == RoleNonvoter:
		err = n.AddNonvoter(req.ID, req.Address)
	case !req.HoldsLog:
		// The first of a voter's two steps (see [Node.Join]). A member that is
		// a voter already keeps its vote: Raft only moves its address.
		err = n.AddNonvoter(req.ID, req.Address)
		reply = joinReply{Staged: true}
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
	// Asked after the change was committed, so it covers every write the
	// node was meant to find when it got in.
	if held, herr := n.machine.store.AppliedIndex(n.ctx); herr == nil {
		reply.Held = held
	}
	return reply
}
