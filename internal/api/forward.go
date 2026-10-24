package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wegweiserzone/wegweiser/internal/cluster"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// writes are the routes a member that is not leading forwards to the one that
// is (docs/decisions/d40-a-write-reaches-the-leader.md). Whether a route
// writes is a property of the route rather than of its method: logging in is
// a POST, and writes nothing (D51). Logging out is answered where it lands
// too, because the browser's cookies are cleared there whatever the leader
// says, and the member hands the leader the session to record itself. A
// test holds this list to the specification, so that a route added there is
// sorted into one side or the other on purpose.
var writes = map[string]bool{
	"POST /zones":                        true,
	"PATCH /zones/{zoneId}":              true,
	"DELETE /zones/{zoneId}":             true,
	"POST /zones/{zoneId}/records":       true,
	"POST /zones/import":                 true,
	"POST /zones/{zoneId}/reconcile":     true,
	"POST /zones/{zoneId}/rollback":      true,
	"PUT /zones/{zoneId}/rrsets":         true,
	"PATCH /records/{recordId}":          true,
	"DELETE /records/{recordId}":         true,
	"POST /records/{recordId}/detach":    true,
	"POST /records/{recordId}/canonical": true,
	"PATCH /settings":                    true,
	"DELETE /cluster/members/{memberId}": true,
	"POST /tokens":                       true,
	"DELETE /tokens/{tokenId}":           true,
	"POST /tsig-keys":                    true,
	"DELETE /tsig-keys/{keyId}":          true,
}

// forwardedHeader carries who a forwarded write is from. It is read on the
// cluster port only, where every connection has proved it holds the
// cluster's secret; the API's own listener never looks at it.
const forwardedHeader = "Weg-Forwarded-Subject"

// forwardedSubject is the caller as the member that authenticated it
// describes it. The credential stays behind: what the leader needs is who the
// caller is, which the member has established already (D40).
type forwardedSubject struct {
	TokenID string  `json:"tokenId"`
	Name    string  `json:"name"`
	Scopes  []Scope `json:"scopes"`

	// Ends is the session a member hands over to be recorded as ended.
	Ends *ending `json:"ends,omitempty"`
}

// forwardedAs is the header value that tells another member who a request is
// made for.
func forwardedAs(sub *subject) (string, error) {
	identity, err := json.Marshal(forwardedSubject{
		TokenID: string(sub.tokenID), Name: sub.name, Scopes: sub.scopes,
	})
	return string(identity), err
}

// viaClusterPort marks a request that another member made, on behalf of its
// caller. A status asked that way answers for this member only (D47).
type viaClusterPort struct{}

// route names the route a request matched, the way [writes] lists it.
func route(r *http.Request) string {
	pattern := ""
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		pattern = strings.TrimPrefix(rctx.RoutePattern(), basePath)
	}
	return r.Method + " " + pattern
}

// forwarding sends a write that arrived at a member that is not leading to the
// member that is, and answers with what that member answered. It runs once
// the route is known and the caller authenticated, and before the body is
// read, so that the body travels as it came.
func (s *Server) forwarding(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := s.cluster
		if c == nil || !writes[route(r)] || !c.Replicating() || c.IsLeader() {
			next.ServeHTTP(w, r)
			return
		}
		// A member that has left the cluster refuses rather than forwards:
		// what it would answer afterwards would not show the change
		// (docs/decisions/d29-a-node-that-cannot-apply.md).
		if err := c.Left(); err != nil {
			writeProblem(w, r, err)
			return
		}
		leaderID, leader := c.Leader()
		switch {
		case leader == "":
			writeProblem(w, r, noLeader())
			return
		case cluster.IsWitness(leaderID):
			// A witness holds no data to plan a write against, and is about
			// to hand leadership on (D39, D48).
			writeProblem(w, r, witnessLeads())
			return
		}
		// The authenticator ran before this, and every route that writes
		// needs a caller.
		identity, err := forwardedAs(subjectOf(r.Context()))
		if err != nil {
			writeProblem(w, r, internal(err))
			return
		}

		proxy := &httputil.ReverseProxy{
			Transport: s.forwardTransport,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "http"
				pr.Out.URL.Host = leader
				pr.Out.Header.Del("Authorization")
				pr.Out.Header.Del("Cookie")
				pr.Out.Header.Set(forwardedHeader, identity)
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				writeProblem(w, r, &apiError{
					status: http.StatusServiceUnavailable,
					kind:   typeUnavailable,
					title:  "The cluster's leader could not be reached",
					detail: fmt.Sprintf("this member forwards writes to the leader at %s, "+
						"and could not reach it: %v", leader, err),
				})
			},
		}
		proxy.ServeHTTP(w, r)
	})
}

// newForwardTransport carries forwarded writes over the cluster port. Each
// connection proves the secret before anything is sent on it, so it is kept
// for the next write rather than made again.
func newForwardTransport(c Cluster) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return c.DialForward(ctx, addr)
		},
		IdleConnTimeout: time.Minute,
	}
}

// forwardedIdentity is the authenticator of the cluster port: the member that
// forwarded the write has authenticated its caller, and says who that was.
//
// A node that is no member yet takes nothing this way. Its writes go to its
// own store, and one sent here would land there and nowhere else.
func (s *Server) forwardedIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cluster == nil || !s.cluster.Replicating() {
			writeProblem(w, r, noLeader())
			return
		}
		var in forwardedSubject
		if err := json.Unmarshal([]byte(r.Header.Get(forwardedHeader)), &in); err != nil {
			writeProblem(w, r, badRequest("a forwarded write has to say who it is from: %v", err))
			return
		}
		sub := &subject{tokenID: store.TokenID(in.TokenID), name: in.Name, scopes: in.Scopes}
		if need := scopeNeeded(r); !sub.allows(need) {
			writeProblem(w, r, notAllowed(need))
			return
		}
		ctx := context.WithValue(r.Context(), subjectKey{}, sub)
		if in.Ends != nil {
			ctx = context.WithValue(ctx, endingKey{}, in.Ends)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, viaClusterPort{}, struct{}{})))
	})
}

// forwardEnding hands the leader a session to record as ended, the way a
// write is forwarded, but from here rather than from the browser: the request
// that ended it has been answered by the time the leader is asked.
func (s *Server) forwardEnding(ctx context.Context, e *ending) error {
	leaderID, leader := s.cluster.Leader()
	switch {
	case leader == "":
		return noLeader()
	case cluster.IsWitness(leaderID):
		return witnessLeads()
	}
	sub := subjectOf(ctx)
	if sub == nil {
		return errors.New("api: a session is being ended with no caller")
	}
	identity, err := json.Marshal(forwardedSubject{
		TokenID: string(sub.tokenID), Name: sub.name, Scopes: sub.scopes, Ends: e,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://"+leader+basePath+sessionPath, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set(forwardedHeader, string(identity))
	resp, err := (&http.Client{Transport: s.forwardTransport}).Do(req)
	if err != nil {
		return fmt.Errorf("hand the ended session to the leader at %s: %w", leader, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the leader at %s answered %s to an ended session", leader, resp.Status)
	}
	return nil
}

// witnessLeads is a write while a witness leads, for the moment it takes to
// hand leadership to a member that holds data
// (docs/decisions/d39-the-witness.md).
func witnessLeads() *apiError {
	return &apiError{
		status: http.StatusServiceUnavailable,
		kind:   typeUnavailable,
		title:  "No member able to take a write is leading yet",
		detail: "a witness is leading the cluster for the moment, and is handing leadership to a member " +
			"that holds data; try the write again shortly. Every member goes on answering queries",
	}
}

// noLeader is a write while no member leads. Queries are answered as before,
// and the write can be tried again once one is elected
// (docs/decisions/d10-quorum-loss-is-read-only.md).
func noLeader() *apiError {
	return &apiError{
		status: http.StatusServiceUnavailable,
		kind:   typeUnavailable,
		title:  "The cluster has no leader",
		detail: "no member is leading the cluster, so it takes no writes until one is elected; " +
			"every member goes on answering queries",
	}
}
