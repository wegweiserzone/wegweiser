package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cluster"
)

// follower stands in for a member that does not lead. It reaches the leader
// over plain TCP, which is all a test needs of the cluster port.
type follower struct {
	leader string
	stall  *cluster.Stall
}

func (f *follower) Init(context.Context) error { return cluster.ErrMember }
func (f *follower) Member() (id, addr string)  { return "ns2", "192.0.2.2:8054" }
func (f *follower) Replicating() bool          { return true }
func (f *follower) IsLeader() bool             { return false }
func (f *follower) Leader() (id, addr string)  { return "ns1", f.leader }
func (f *follower) Stalled() (cluster.Stall, bool) {
	if f.stall == nil {
		return cluster.Stall{}, false
	}
	return *f.stall, true
}

func (f *follower) DialForward(ctx context.Context, addr string) (net.Conn, error) {
	return new(net.Dialer).DialContext(ctx, "tcp", addr)
}

// leading is a harness that leads, with the cluster port served on a socket
// of its own, and what the last write forwarded to it carried.
type leading struct {
	*harness
	port    string
	carried atomic.Pointer[http.Header]
}

func newLeading(t *testing.T) *leading {
	t.Helper()
	l := &leading{}
	l.harness = newHarness(t, func(cfg *Config) { cfg.Cluster = &member{started: true} })
	forwarded := l.api.Forwarded()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		l.carried.Store(&h)
		forwarded.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	l.port = strings.TrimPrefix(srv.URL, "http://")
	return l
}

func zoneNames(h *harness) []string {
	h.t.Helper()
	var page gen.ZonePage
	h.decode(h.do(http.MethodGet, "/zones", nil), http.StatusOK, &page)
	names := make([]string, 0, len(page.Items))
	for i := range page.Items {
		names = append(names, page.Items[i].Name)
	}
	return names
}

// D40: a write that lands on a member that does not lead is carried out by the
// one that does, and the caller is answered with what that member answered.
func TestAFollowerForwardsAWrite(t *testing.T) {
	t.Parallel()
	leader := newLeading(t)
	f := newHarness(t, func(cfg *Config) { cfg.Cluster = &follower{leader: leader.port} })

	var made gen.Zone
	f.decode(f.do(http.MethodPost, "/zones", gen.CreateZone{Name: "example.com."}),
		http.StatusCreated, &made)
	if made.Name != "example.com." {
		t.Errorf("the answer names %q, want the zone the leader created", made.Name)
	}

	if got := zoneNames(leader.harness); len(got) != 1 || got[0] != "example.com." {
		t.Errorf("the leader holds %v, want the zone the follower was asked for", got)
	}
	// Reads are answered where they land. In a cluster the log would bring
	// the zone here a moment later; these two stores share no log.
	if got := zoneNames(f); len(got) != 0 {
		t.Errorf("the follower holds %v, want the write to have gone to the leader alone", got)
	}

	t.Run("the identity travels, the credential stays behind", func(t *testing.T) {
		h := leader.carried.Load()
		if h == nil {
			t.Fatal("nothing reached the leader")
		}
		if h.Get("Authorization") != "" || h.Get("Cookie") != "" {
			t.Errorf("the forwarded write carried a credential: %v", *h)
		}
		var who forwardedSubject
		if err := json.Unmarshal([]byte(h.Get(forwardedHeader)), &who); err != nil {
			t.Fatalf("the forwarded write says nothing readable about who it is from: %v", err)
		}
		if who.Name != BootstrapName || len(who.Scopes) == 0 {
			t.Errorf("forwarded as %+v, want the follower's caller", who)
		}
	})
}

// Logging in creates a session on the node that was asked, which is where the
// browser will present it, so it is never forwarded (D40).
func TestALoginStaysWhereItLands(t *testing.T) {
	t.Parallel()
	f := newHarness(t, func(cfg *Config) { cfg.Cluster = &follower{leader: unreachableAddr(t)} })

	resp := f.do(http.MethodPost, "/auth/session", map[string]string{"token": f.token},
		func(r *http.Request) { r.Header.Del("Authorization") })
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Errorf("logging in on a follower is %d, want it answered here", resp.StatusCode)
	}
}

func TestAWriteWithNoLeaderToTakeIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		member *follower
		want   string
	}{
		{"no member leads", &follower{}, "no leader"},
		{"the leader cannot be reached", &follower{leader: unreachableAddr(t)}, "could not be reached"},
		{"this member has left the cluster", &follower{leader: unreachableAddr(t), stall: &cluster.Stall{
			Entry: 7, Reason: errors.New("disk full"), At: time.Now(),
		}}, "behind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newHarness(t, func(cfg *Config) { cfg.Cluster = tc.member })

			var problem gen.Problem
			f.decode(f.do(http.MethodPost, "/zones", gen.CreateZone{Name: "example.com."}),
				http.StatusServiceUnavailable, &problem)
			if problem.Type != typeUnavailable || !strings.Contains(strings.ToLower(problem.Title), tc.want) {
				t.Errorf("problem = %+v, want it unavailable and saying %q", problem, tc.want)
			}
		})
	}
}

// Only a member forwards writes, and only to a member: a node that has not
// joined would write a forwarded change to its own store and nowhere else.
func TestOnlyAMemberTakesAForwardedWrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(cfg *Config) { cfg.Cluster = &member{} })
	srv := httptest.NewServer(h.api.Forwarded())
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+basePath+"/zones",
		strings.NewReader(`{"name":"example.com."}`))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(forwardedHeader, `{"tokenId":"x","name":"someone","scopes":["admin"]}`)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a node in no cluster took a forwarded write: status %d", resp.StatusCode)
	}
	if got := zoneNames(h); len(got) != 0 {
		t.Errorf("it holds %v afterwards, want nothing", got)
	}
}

// Every route in the specification is sorted into one that writes, and is
// forwarded, or one that is answered where it lands. Reading is the second
// kind; a route that changes something and is still answered locally is
// named here, so that adding one is a decision rather than an accident.
func TestEveryRouteIsSortedForForwarding(t *testing.T) {
	t.Parallel()
	local := map[string]bool{
		"POST /auth/session":   true, // a session is node-local (D5, D32)
		"DELETE /auth/session": true,
		"POST /cluster/init":   true, // this node, and no other, starts a cluster (D44)
	}

	spec, err := gen.GetSpec()
	if err != nil {
		t.Fatalf("read the specification: %v", err)
	}
	seen := map[string]bool{}
	for path, item := range spec.Paths.Map() {
		for method := range item.Operations() {
			key := method + " " + path
			seen[key] = true
			switch {
			case writes[key] && local[key]:
				t.Errorf("%s is listed as both forwarded and local", key)
			case method == http.MethodGet && writes[key]:
				t.Errorf("%s is a read and is listed as forwarded", key)
			case method != http.MethodGet && !writes[key] && !local[key]:
				t.Errorf("%s is neither forwarded nor named as local; sort it in forward.go", key)
			}
		}
	}
	for key := range writes {
		if !seen[key] {
			t.Errorf("%s is listed as forwarded and is not in the specification", key)
		}
	}
}

// unreachableAddr is a loopback address nothing listens on.
func unreachableAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}
