package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/stream"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// streamPath is the one route that is meant to occupy a handler for as long as
// the client stays, so the request timeout does not apply to it.
const streamPath = basePath + "/queries/stream"

// streamHeartbeat is how often a stream says something when nothing is being
// asked. It keeps a proxy from deciding the connection is dead, and it is what
// carries the sampling ratio to a watcher whose traffic has not changed.
const streamHeartbeat = 10 * time.Second

// StreamQueries carries a live tail of the exchanges matching the filter, as
// Server-Sent Events.
func (s *Server) StreamQueries(
	ctx context.Context, request gen.StreamQueriesRequestObject,
) (gen.StreamQueriesResponseObject, error) {
	filter, err := streamFilter(request.Params)
	if err != nil {
		return nil, err
	}

	sub, err := s.stream.Subscribe(filter)
	if err != nil {
		if errors.Is(err, stream.ErrTooManyWatchers) {
			// Every watcher costs the query path a filter per query, so this
			// is a bound on the data plane rather than on this endpoint. The
			// client is told to come back rather than told it did something
			// wrong, because it did not.
			return nil, &apiError{
				status: http.StatusServiceUnavailable,
				kind:   typeUnavailable,
				title:  "Too many watchers",
				detail: "the query stream is already being watched by as many clients as it carries; close one and try again",
			}
		}
		return nil, internal(err)
	}
	return &queryStream{ctx: ctx, sub: sub}, nil
}

// streamFilter turns the query parameters into the filter the stream applies.
func streamFilter(p gen.StreamQueriesParams) (stream.Filter, error) {
	var f stream.Filter

	if p.Name != nil && *p.Name != "" {
		name, err := zone.ParseName(*p.Name)
		if err != nil {
			return f, badRequest("name %q: %v", *p.Name, err)
		}
		f.Name = name
	}

	if p.Type != nil {
		for _, t := range *p.Type {
			typ, err := zone.ParseRRType(t)
			if err != nil {
				return f, badRequest("type %q: %v", t, err)
			}
			f.Types = append(f.Types, typ)
		}
	}

	if p.Rcode != nil {
		for _, r := range *p.Rcode {
			rcode, err := dns.ParseRcode(r)
			if err != nil {
				return f, badRequest("rcode %q: %v", r, err)
			}
			f.Rcodes = append(f.Rcodes, rcode)
		}
	}

	if p.Client != nil && *p.Client != "" {
		prefix, err := clientPrefix(*p.Client)
		if err != nil {
			return f, badRequest("client %q: %v", *p.Client, err)
		}
		f.Client = prefix
	}

	return f, nil
}

// clientPrefix reads an address or a network. A bare address is a single host,
// because "watch this resolver" is what somebody types far more often than
// "watch this /32".
func clientPrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// statusInterval is the fastest a stream repeats what it is leaving out while
// it is leaving something out.
//
// Under load the ratio moves with every few queries (it is worked out from
// how full the current second already is) so a message per change would be a
// flood of its own on the wire and on the screen. Measured against a real one:
// three hundred status lines for a three-second flood. Starting and stopping
// are still said at once, because those are the transitions a person acts on.
const statusInterval = time.Second

// queryStream writes the events out until the client goes away.
type queryStream struct {
	ctx context.Context
	sub *stream.Subscription
}

func (q *queryStream) VisitStreamQueriesResponse(w http.ResponseWriter) error {
	defer q.sub.Close()

	// A stream that a proxy is free to buffer is not a live one, and one a
	// browser is free to cache is worse.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Nothing in this server's own wiring produces one, but a middleware
		// that wraps the writer might, and a stream nobody can see arriving is
		// worse than a refusal.
		return errors.New("api: the query stream needs a response writer that can flush")
	}

	// The first message is the status, so a watcher knows from the outset
	// whether it is being shown everything: rather than after the first
	// exchange, which on a quiet server may be minutes away.
	last := q.sub.Stats()
	said := time.Now()
	if err := writeStatus(w, last); err != nil {
		return err
	}
	flusher.Flush()

	beat := time.NewTicker(streamHeartbeat)
	defer beat.Stop()

	for {
		select {
		case <-q.ctx.Done():
			return nil
		case <-q.sub.Done():
			return nil

		case ev := <-q.sub.Events():
			if err := writeEvent(w, "query", queryEvent(ev)); err != nil {
				return err
			}
			if now := q.sub.Stats(); statusDue(last, now, said) {
				if err := writeStatus(w, now); err != nil {
					return err
				}
				last, said = now, time.Now()
			}
			flusher.Flush()

		case <-beat.C:
			last, said = q.sub.Stats(), time.Now()
			if err := writeStatus(w, last); err != nil {
				return err
			}
			flusher.Flush()
		}
	}
}

// statusDue reports whether what the stream is leaving out is worth saying
// again.
func statusDue(last, now stream.Stats, said time.Time) bool {
	if (now.Ratio > 1) != (last.Ratio > 1) {
		return true
	}
	if now == last {
		return false
	}
	return time.Since(said) >= statusInterval
}

// queryEvent is one exchange in the shape the spec describes.
func queryEvent(ev dns.Event) gen.QueryEvent {
	out := gen.QueryEvent{
		At:        ev.At,
		LatencyUs: int(ev.Latency.Microseconds()),
		Transport: gen.QueryEventTransport(ev.Transport.String()),
		Name:      ev.Name,
		Type:      ev.Type.String(),
		Class:     ev.Class.String(),
		Rcode:     dns.RcodeName(ev.Rcode),
		Size:      ev.Size,
		Truncated: ev.Truncated,
		Dropped:   ev.Dropped,
	}
	if ev.Client.IsValid() {
		out.Client = ev.Client.Addr().String()
		out.Port = ptr(int(ev.Client.Port()))
	}
	return out
}

// writeStatus says what the stream is leaving out.
func writeStatus(w http.ResponseWriter, st stream.Stats) error {
	return writeEvent(w, "status", gen.StreamStatus{
		Matched: counted(st.Matched),
		Sent:    counted(st.Sent),
		Sampled: counted(st.Sampled),
		Dropped: counted(st.Dropped),
		Ratio:   st.Ratio,
	})
}

// counted narrows a counter for the document, which says integer.
func counted(n uint64) int {
	if n > math.MaxInt {
		return math.MaxInt
	}
	return int(n)
}

// writeEvent writes one Server-Sent Event.
func writeEvent(w http.ResponseWriter, name string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("api: encode a %s event: %w", name, err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, body); err != nil {
		return err
	}
	return nil
}
