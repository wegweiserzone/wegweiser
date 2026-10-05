package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cluster"
)

// member stands in for a node that starts its cluster once and refuses after.
type member struct{ started bool }

func (m *member) Init(context.Context) error {
	if m.started {
		return cluster.ErrMember
	}
	m.started = true
	return nil
}

func (m *member) Member() (id, addr string) { return "ns1", "192.0.2.1:8054" }
func (m *member) Replicating() bool         { return m.started }

func (m *member) Status(context.Context) (cluster.Status, error) {
	if !m.started {
		return cluster.Status{}, nil
	}
	return cluster.Status{Applied: 3, Committed: 3, Members: []cluster.MemberState{
		{ID: "ns1", Address: "192.0.2.1:8054", Role: cluster.RoleVoter, Leader: true},
		{ID: "ns2", Address: "192.0.2.2:8054", Role: cluster.RoleNonvoter},
	}}, nil
}

func (m *member) IsLeader() bool                 { return m.started }
func (m *member) Leader() (id, addr string)      { return m.Member() }
func (m *member) Stalled() (cluster.Stall, bool) { return cluster.Stall{}, false }
func (m *member) DialForward(context.Context, string) (net.Conn, error) {
	return nil, errors.New("a member that leads forwards nothing")
}

func TestInitCluster(t *testing.T) {
	t.Parallel()
	m := &member{}
	h := newHarness(t, func(cfg *Config) { cfg.Cluster = m })

	var got gen.ClusterMember
	h.decode(h.do(http.MethodPost, "/cluster/init", nil), http.StatusOK, &got)
	if !m.started || got.Id != "ns1" || got.Address != "192.0.2.1:8054" {
		t.Errorf("started = %v, answer = %+v, want this node started as ns1 at its address", m.started, got)
	}

	// D44: a cluster is started once.
	if resp := h.do(http.MethodPost, "/cluster/init", nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("starting it again is %d, want 409", resp.StatusCode)
	}
}

// Without a cluster section there is no address to be reached at and no
// secret to prove membership with, so there is nothing to start.
func TestInitClusterNeedsAClusterSection(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if resp := h.do(http.MethodPost, "/cluster/init", nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("POST /cluster/init without a cluster section is %d, want 409", resp.StatusCode)
	}
}

func TestInitClusterNeedsTheAdminScope(t *testing.T) {
	t.Parallel()
	m := &member{}
	h := newHarness(t, func(cfg *Config) { cfg.Cluster = m })

	var minted gen.TokenCreated
	h.decode(h.do(http.MethodPost, "/tokens", gen.CreateToken{
		Name: "writer", Scopes: []gen.Scope{gen.ScopeWrite},
	}), http.StatusCreated, &minted)
	as := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+minted.Secret) }

	if resp := h.do(http.MethodPost, "/cluster/init", nil, as); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /cluster/init with a write token is %d, want 403", resp.StatusCode)
	}
	if m.started {
		t.Error("a write token started a cluster")
	}
}

func TestGetCluster(t *testing.T) {
	t.Parallel()

	t.Run("a member lists the members", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, func(cfg *Config) { cfg.Cluster = &member{started: true} })
		var got gen.ClusterStatus
		h.decode(h.do(http.MethodGet, "/cluster", nil), http.StatusOK, &got)
		if !got.Replicating || got.Self.Id != "ns1" || got.Applied != 3 || got.Behind != nil {
			t.Errorf("status = %+v, want member ns1, current at entry 3", got)
		}
		if len(got.Members) != 2 || !got.Members[0].Leader || got.Members[1].Role != gen.ClusterMemberStateRoleNonvoter {
			t.Errorf("members = %+v, want ns1 leading and ns2 without a vote", got.Members)
		}
	})

	t.Run("a member that stopped says where", func(t *testing.T) {
		t.Parallel()
		stall := &cluster.Stall{Entry: 7, Reason: errors.New("disk full"), At: time.Now()}
		h := newHarness(t, func(cfg *Config) { cfg.Cluster = &follower{stall: stall} })
		var got gen.ClusterStatus
		h.decode(h.do(http.MethodGet, "/cluster", nil), http.StatusOK, &got)
		if got.Behind == nil || got.Behind.Entry != 7 || got.Behind.Reason != "disk full" {
			t.Errorf("behind = %+v, want entry 7 and why", got.Behind)
		}
	})

	t.Run("a single server has no cluster to show", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		if resp := h.do(http.MethodGet, "/cluster", nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET /cluster without a cluster section is %d, want 404", resp.StatusCode)
		}
	})
}
