package dns

import (
	"net"
	"sync"
	"testing"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// collector gathers what a server observed. Queries are answered on several
// goroutines, so the events arrive on several too.
type collector struct {
	mu     sync.Mutex
	events []Event
}

func (c *collector) observe(ev Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

// await waits for n events and returns them, rather than reading whatever has
// arrived by the time the response has: the observer runs after the write, so
// the client can hold the answer before the event exists.
func (c *collector) await(t *testing.T, n int) []Event {
	t.Helper()

	deadline := time.Now().Add(testTimeout)
	for {
		c.mu.Lock()
		got := len(c.events)
		if got >= n {
			out := make([]Event, got)
			copy(out, c.events)
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()

		if time.Now().After(deadline) {
			t.Fatalf("observed %d events, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServerObservesQueries(t *testing.T) {
	t.Parallel()

	t.Run("over a datagram", func(t *testing.T) {
		t.Parallel()

		var c collector
		s := startServer(t, resolveFixture(t), Config{Observe: c.observe})
		packed := askUDP(t, s.Addr().String(), packQuery(t, "www.example.com.", zone.TypeA))

		ev := c.await(t, 1)[0]
		if ev.Name != "www.example.com." || ev.Type != zone.TypeA || ev.Class != zone.ClassIN {
			t.Errorf("question = %s %s %s, want www.example.com. IN A", ev.Name, ev.Class, ev.Type)
		}
		if ev.Transport != UDP || ev.Rcode != wire.RcodeSuccess || ev.Dropped {
			t.Errorf("transport=%s rcode=%d dropped=%v, want udp, NOERROR and sent",
				ev.Transport, ev.Rcode, ev.Dropped)
		}
		if ev.Size != len(packed) {
			t.Errorf("size = %d, want the %d octets the client received", ev.Size, len(packed))
		}
		if !ev.Client.Addr().IsLoopback() {
			t.Errorf("client = %s, want the loopback address the query came from", ev.Client)
		}
		if ev.At.IsZero() || ev.Latency <= 0 {
			t.Errorf("at=%v latency=%v, want both to have been measured", ev.At, ev.Latency)
		}
	})

	t.Run("over a connection", func(t *testing.T) {
		t.Parallel()

		var c collector
		s := startServer(t, resolveFixture(t), Config{Observe: c.observe})
		conn := dialTCP(t, s.Addr().String())
		writeFramed(t, conn, packQuery(t, "txt.example.com.", zone.TypeTXT))
		readFramed(t, conn)

		ev := c.await(t, 1)[0]
		if ev.Transport != TCP || ev.Name != "txt.example.com." || ev.Type != zone.TypeTXT {
			t.Errorf("event = %s %s over %s, want txt.example.com. TXT over tcp",
				ev.Name, ev.Type, ev.Transport)
		}
		local, ok := conn.LocalAddr().(*net.TCPAddr)
		if !ok {
			t.Fatalf("the connection's local address is a %T", conn.LocalAddr())
		}
		if ev.Client != local.AddrPort() {
			t.Errorf("client = %s, want the connection's own address %s", ev.Client, local.AddrPort())
		}
	})

	// A name asked for in mixed case is what the client sent, not what the
	// server stores: somebody watching the stream is looking for their own
	// query, and 0x20 encoding means it will not be lowercase.
	t.Run("the name is the one that was asked", func(t *testing.T) {
		t.Parallel()

		var c collector
		s := startServer(t, resolveFixture(t), Config{Observe: c.observe})
		askUDP(t, s.Addr().String(), packQuery(t, "WwW.ExAmPlE.cOm.", zone.TypeA))

		if got := c.await(t, 1)[0].Name; got != "WwW.ExAmPlE.cOm." {
			t.Errorf("name = %q, want the casing the client used", got)
		}
	})

	// A denial is an event like any other. It is also the one an operator
	// looking for a misconfiguration searches the stream for.
	t.Run("a denial carries its response code", func(t *testing.T) {
		t.Parallel()

		var c collector
		s := startServer(t, resolveFixture(t), Config{Observe: c.observe})
		askUDP(t, s.Addr().String(), packQuery(t, "nothing.example.com.", zone.TypeA))

		if got := c.await(t, 1)[0].Rcode; got != wire.RcodeNameError {
			t.Errorf("rcode = %d, want NXDOMAIN", got)
		}
	})
}

// A query that gets silence is exactly what somebody debugging is trying to
// find, so it is an event too, and the one where the size has to say that
// nothing was sent.
func TestServerObservesDroppedQueries(t *testing.T) {
	t.Parallel()

	var c collector
	s := startServer(t, resolveFixture(t), Config{Observe: c.observe})
	addr := s.Addr().String()

	// A message with QR already set is a response, and answering a response is
	// how two servers talk each other into a loop.
	response := packQuery(t, "www.example.com.", zone.TypeA, func(m *wire.Msg) { m.Response = true })
	if got := askUDPWithin(t, addr, response, 200*time.Millisecond); got != nil {
		t.Fatalf("the server answered a response with %d octets", len(got))
	}

	ev := c.await(t, 1)[0]
	if !ev.Dropped || ev.Size != 0 {
		t.Errorf("dropped=%v size=%d, want a dropped event that sent nothing", ev.Dropped, ev.Size)
	}
	if ev.Name != "www.example.com." {
		t.Errorf("name = %q, want the question that was dropped", ev.Name)
	}
}

// A responder is reused for every query its reader handles. An event left over
// from the previous one would describe this one wrongly, and the query with no
// readable question is where that would show.
func TestResponderEventDoesNotOutliveItsQuery(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)
	r := NewResponder(DefaultLimits())

	if _, err := r.Respond(snap, packQuery(t, "www.example.com.", zone.TypeA), UDP, nil); err != nil {
		t.Fatalf("respond: %v", err)
	}
	if got := r.Observed(); got.Name != "www.example.com." || got.Dropped {
		t.Fatalf("first event = %q dropped=%v, want the query that was answered", got.Name, got.Dropped)
	}

	if _, err := r.Respond(snap, []byte{0x2A}, UDP, nil); err == nil {
		t.Fatal("a message too short to hold a header was answered")
	}
	got := r.Observed()
	if got.Name != "" || got.Type != 0 || !got.Dropped {
		t.Errorf("second event = %q %s dropped=%v, want an empty question and a drop",
			got.Name, got.Type, got.Dropped)
	}
}

// Truncation is a fact about the exchange that the response itself no longer
// carries once it has been cut, so the event has to hold it.
func TestServerObservesTruncation(t *testing.T) {
	t.Parallel()

	var c collector
	// One name with more addresses than the 512 octets RFC 1035 §4.2.1 allows
	// an unextended datagram to carry.
	s := startServer(t, bigZone(t), Config{Observe: c.observe})
	got := parse(t, askUDP(t, s.Addr().String(), packQuery(t, "many.example.com.", zone.TypeA)))
	if !got.Truncated {
		t.Fatalf("the response was not truncated, so this test measures nothing")
	}

	if ev := c.await(t, 1)[0]; !ev.Truncated {
		t.Errorf("the event says the response was not truncated")
	}
}

// Nothing watching means nothing is built, and in particular the clock is not
// read twice per query for an event nobody receives.
func TestServerWithoutAnObserver(t *testing.T) {
	t.Parallel()

	s := startServer(t, resolveFixture(t), Config{})
	if got := s.startedAt(); !got.IsZero() {
		t.Errorf("startedAt = %v with no observer, want the zero time", got)
	}
	askUDP(t, s.Addr().String(), packQuery(t, "www.example.com.", zone.TypeA))
}

func TestTransportString(t *testing.T) {
	t.Parallel()
	if got, want := UDP.String(), "udp"; got != want {
		t.Errorf("UDP = %q, want %q", got, want)
	}
	if got, want := TCP.String(), "tcp"; got != want {
		t.Errorf("TCP = %q, want %q", got, want)
	}
}
