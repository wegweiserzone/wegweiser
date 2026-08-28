package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// testTimeout is the deadline on every socket operation in this file. It is
// generous, because it is there to turn a hang into a failure rather than to
// measure anything.
const testTimeout = 5 * time.Second

// startServer brings a server up on an ephemeral port and takes it down again
// when the test ends.
func startServer(t *testing.T, snap *Snapshot, cfg Config) *Server {
	t.Helper()

	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	if cfg.OnError == nil {
		cfg.OnError = func(err error) { t.Errorf("the server reported a fault: %v", err) }
	}

	s := NewServer(cfg)
	s.SetSnapshot(snap)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return s
}

// askUDP sends one datagram and waits for the answer.
func askUDP(t *testing.T, addr string, query []byte) []byte {
	t.Helper()
	return askUDPWithin(t, addr, query, testTimeout)
}

// askUDPWithin is askUDP with a deadline of its own, so that a test expecting
// silence does not pay the full timeout to find it.
func askUDPWithin(t *testing.T, addr string, query []byte, within time.Duration) []byte {
	t.Helper()

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()

	// One err throughout: shadowing it here would hide the read error the
	// silence check below has to look at.
	if err = conn.SetDeadline(time.Now().Add(within)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err = conn.Write(query); err != nil {
		t.Fatalf("write the query: %v", err)
	}

	buf := make([]byte, wire.MaxMsgSize)
	n, err := conn.Read(buf)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil
		}
		t.Fatalf("read the response: %v", err)
	}
	return buf[:n]
}

// writeFramed sends one query with the two-octet length prefix of RFC 1035
// §4.2.2.
func writeFramed(t *testing.T, conn net.Conn, query []byte) {
	t.Helper()

	framed := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(framed, uint16(len(query)))
	copy(framed[2:], query)
	if _, err := conn.Write(framed); err != nil {
		t.Fatalf("write the query: %v", err)
	}
}

// readFramed reads one length-prefixed response.
func readFramed(t *testing.T, conn net.Conn) []byte {
	t.Helper()

	var length [2]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		t.Fatalf("read the response length: %v", err)
	}
	buf := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read the response: %v", err)
	}
	return buf
}

// dialTCP opens a connection with a deadline covering the whole test.
func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	return conn
}

// parse turns response bytes into a message, failing the test if they are not
// one.
func parse(t *testing.T, packed []byte) *wire.Msg {
	t.Helper()

	m := new(wire.Msg)
	if err := m.Unpack(packed); err != nil {
		t.Fatalf("the response does not parse: %v", err)
	}
	return m
}

func TestServerAnswers(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)
	s := startServer(t, snap, Config{})
	addr := s.Addr().String()
	query := packQuery(t, "www.example.com.", zone.TypeA)

	t.Run("over a datagram", func(t *testing.T) {
		got := parse(t, askUDP(t, addr, query))
		if got.Id != queryID || !got.Response || !got.Authoritative {
			t.Errorf("id=%#04x qr=%v aa=%v, want the query's id with QR and AA set",
				got.Id, got.Response, got.Authoritative)
		}
		if len(got.Answer) != 2 {
			t.Errorf("the answer holds %d records, want 2", len(got.Answer))
		}
	})

	t.Run("over a connection", func(t *testing.T) {
		conn := dialTCP(t, addr)
		writeFramed(t, conn, query)
		got := parse(t, readFramed(t, conn))
		if got.Id != queryID || len(got.Answer) != 2 {
			t.Errorf("id=%#04x with %d answer records, want the query's id and 2",
				got.Id, len(got.Answer))
		}
	})
}

// TestServerOneAddress covers the detail that makes SO_REUSEPORT usable with a
// configured port of 0: every socket has to join the port the first one was
// given, or the server ends up listening on as many addresses as it has
// readers.
func TestServerOneAddress(t *testing.T) {
	t.Parallel()

	s := startServer(t, resolveFixture(t), Config{UDPSockets: 4})

	if len(s.udp) != 4 {
		t.Fatalf("the server opened %d datagram sockets, want 4", len(s.udp))
	}
	want := s.udp[0].LocalAddr().String()
	for i, conn := range s.udp {
		if got := conn.LocalAddr().String(); got != want {
			t.Errorf("socket %d is bound to %s, want %s", i, got, want)
		}
	}
	if got := s.tcp.Addr().String(); got != want {
		t.Errorf("the connection listener is on %s, want %s", got, want)
	}

	// Every socket has to be able to answer, whichever one the kernel picks.
	for range 20 {
		if got := parse(t, askUDP(t, want, packQuery(t, "www.example.com.", zone.TypeA))); len(got.Answer) != 2 {
			t.Fatalf("the answer holds %d records, want 2", len(got.Answer))
		}
	}
}

// TestServerPipelining covers RFC 7766 §6.2.1.1 from the side that matters to a
// client: several queries may be in the connection at once, and every one of
// them is answered.
func TestServerPipelining(t *testing.T) {
	t.Parallel()

	s := startServer(t, resolveFixture(t), Config{})
	conn := dialTCP(t, s.Addr().String())

	// The last one repeats the first, which is also what makes the connection
	// reuse its frame buffer instead of growing it every time.
	names := []string{
		"www.example.com.", "nothere.example.com.", "alias.example.com.", "www.example.com.",
	}
	for _, name := range names {
		writeFramed(t, conn, packQuery(t, name, zone.TypeA))
	}

	want := []int{
		wire.RcodeSuccess, wire.RcodeNameError, wire.RcodeSuccess, wire.RcodeSuccess,
	}
	for i, name := range names {
		got := parse(t, readFramed(t, conn))
		if got.Rcode != want[i] {
			t.Errorf("%s: rcode = %s, want %s", name,
				wire.RcodeToString[got.Rcode], wire.RcodeToString[want[i]])
		}
		if len(got.Question) != 1 || got.Question[0].Name != name {
			t.Errorf("response %d answers %+v, want %s in the order it was asked",
				i, got.Question, name)
		}
	}
}

// TestServerSnapshotSwap is the RCU property from the outside: a query before
// the swap and one after see two different worlds, and neither waits for the
// other.
func TestServerSnapshotSwap(t *testing.T) {
	t.Parallel()

	s := startServer(t, nil, Config{})
	addr := s.Addr().String()
	query := packQuery(t, "www.example.com.", zone.TypeA)

	if got := parse(t, askUDP(t, addr, query)); got.Rcode != wire.RcodeRefused {
		t.Errorf("rcode = %s before a snapshot is published, want REFUSED",
			wire.RcodeToString[got.Rcode])
	}

	s.SetSnapshot(resolveFixture(t))

	got := parse(t, askUDP(t, addr, query))
	if got.Rcode != wire.RcodeSuccess || len(got.Answer) != 2 {
		t.Errorf("rcode = %s with %d records after the swap, want NOERROR with 2",
			wire.RcodeToString[got.Rcode], len(got.Answer))
	}
}

// TestServerSurvivesGarbage checks that a packet the server refuses to answer
// costs it nothing but the packet: it stays silent, and it keeps serving.
func TestServerSurvivesGarbage(t *testing.T) {
	t.Parallel()

	s := startServer(t, resolveFixture(t), Config{})
	addr := s.Addr().String()

	// All four are packets §2.2 answers with silence: three are too short to
	// hold a header, and the fourth has QR set, which makes it a response.
	// Answering a response is how two servers talk each other into a loop.
	for _, junk := range [][]byte{
		{},
		{0x01},
		make([]byte, headerLen-1),
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	} {
		if got := askUDPWithin(t, addr, junk, 250*time.Millisecond); got != nil {
			t.Errorf("a %d-octet packet was answered with %d octets, want silence",
				len(junk), len(got))
		}
	}

	if got := parse(t, askUDP(t, addr, packQuery(t, "www.example.com.", zone.TypeA))); len(got.Answer) != 2 {
		t.Errorf("the server answers %d records after the garbage, want 2", len(got.Answer))
	}
}

// TestServerShutdownDrains checks that closing down does not wait out the idle
// period of every connection that happens to be open.
func TestServerShutdownDrains(t *testing.T) {
	t.Parallel()

	s := NewServer(Config{Addr: "127.0.0.1:0", TCPIdleTimeout: time.Hour})
	s.SetSnapshot(resolveFixture(t))
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// An idle connection, parked in a read that will never return on its own.
	conn := dialTCP(t, s.Addr().String())
	writeFramed(t, conn, packQuery(t, "www.example.com.", zone.TypeA))
	readFramed(t, conn)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	start := time.Now()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > testTimeout/2 {
		t.Errorf("shutdown took %v, so it waited for the idle period rather than "+
			"waking the connection", elapsed)
	}

	// A second shutdown is not an error, and the sockets are gone.
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("the second shutdown: %v", err)
	}
	if _, err := net.Dial("tcp", s.Addr().String()); err == nil {
		t.Error("the listener still accepts connections after shutdown")
	}
}

// TestServerStartTwice covers the guard rather than the case: a server started
// twice would bind a second set of sockets and leak the first.
func TestServerStartTwice(t *testing.T) {
	t.Parallel()

	s := startServer(t, resolveFixture(t), Config{})
	if err := s.Start(); err == nil {
		t.Error("the server started a second time")
	}
}

// TestServerTCPIdleTimeout checks that a connection which asks nothing is not
// kept for ever (RFC 7766 §6.2.3).
func TestServerTCPIdleTimeout(t *testing.T) {
	t.Parallel()

	s := startServer(t, resolveFixture(t), Config{TCPIdleTimeout: 50 * time.Millisecond})
	conn := dialTCP(t, s.Addr().String())

	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Error("the connection is still open after the idle period")
	}
}

// TestServerDefaults pins what an empty Config means, since every one of these
// is a decision rather than an accident.
func TestServerDefaults(t *testing.T) {
	t.Parallel()

	s := NewServer(Config{})
	if s.cfg.Addr != defaultAddr {
		t.Errorf("Addr = %q, want %q", s.cfg.Addr, defaultAddr)
	}
	if s.cfg.UDPSockets != runtime.GOMAXPROCS(0) {
		t.Errorf("UDPSockets = %d, want one per processor (%d)",
			s.cfg.UDPSockets, runtime.GOMAXPROCS(0))
	}
	if s.cfg.TCPIdleTimeout != defaultTCPIdle {
		t.Errorf("TCPIdleTimeout = %v, want %v", s.cfg.TCPIdleTimeout, defaultTCPIdle)
	}
	if s.cfg.MaxTCPClients != defaultTCPClients || cap(s.slots) != defaultTCPClients {
		t.Errorf("MaxTCPClients = %d with %d slots, want %d",
			s.cfg.MaxTCPClients, cap(s.slots), defaultTCPClients)
	}
	if s.Snapshot() != nil || s.Addr() != nil {
		t.Error("a server that has not started already has a snapshot or an address")
	}
}

// TestServerStartFails checks that a server which cannot bind says so and
// leaves nothing behind. A half-bound server would hold the sockets it did get
// while reporting that it never started.
func TestServerStartFails(t *testing.T) {
	t.Parallel()

	s := NewServer(Config{Addr: "127.0.0.1:not-a-port"})
	if err := s.Start(); err == nil {
		t.Fatal("the server started on an address that cannot be parsed")
	}
	if s.Addr() != nil {
		t.Errorf("the server holds %s after failing to start", s.Addr())
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("shutting down a server that never started: %v", err)
	}
}

// TestServerReport pins what the error hook is for, and what it is not for.
func TestServerReport(t *testing.T) {
	t.Parallel()

	var got []string
	s := NewServer(Config{OnError: func(err error) { got = append(got, err.Error()) }})

	s.report(errors.New("while serving"))
	s.closing.Store(true)
	s.report(errors.New("while shutting down"))

	if want := []string{"while serving"}; !slices.Equal(got, want) {
		t.Errorf("the hook saw %q, want %q", got, want)
	}

	// A server without a hook has nowhere to put a fault and must not mind.
	NewServer(Config{}).report(errors.New("nobody is listening"))
}

// TestServerTCPClientLimit checks the bound that keeps a connection flood from
// being a memory flood. A connection costs a goroutine, a Responder and buffers
// that grow to the largest message the client sent, so an unbounded accept loop
// is a cheap way to take the server's memory.
func TestServerTCPClientLimit(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var reports []string
	s := startServer(t, resolveFixture(t), Config{
		MaxTCPClients:  2,
		TCPIdleTimeout: testTimeout,
		OnError: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			reports = append(reports, err.Error())
		},
	})
	addr := s.Addr().String()

	// Two connections that stay open, each with a query answered on it, so the
	// slots are provably taken rather than merely accepted.
	held := make([]net.Conn, 0, 2)
	for range 2 {
		conn := dialTCP(t, addr)
		writeFramed(t, conn, packQuery(t, "www.example.com.", zone.TypeA))
		if m := parse(t, readFramed(t, conn)); m.Rcode != wire.RcodeSuccess {
			t.Fatalf("rcode = %s on a connection within the limit", wire.RcodeToString[m.Rcode])
		}
		held = append(held, conn)
	}

	t.Run("the next one is closed rather than left waiting", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			// Refused at the kernel is the same answer by a shorter route.
			return
		}
		defer conn.Close()
		if derr := conn.SetDeadline(time.Now().Add(testTimeout)); derr != nil {
			t.Fatalf("set deadline: %v", derr)
		}

		// The write may well succeed (the connection was accepted before it
		// was closed) so what says the slot was refused is the read.
		if _, werr := conn.Write(framed(packQuery(t, "www.example.com.", zone.TypeA))); werr != nil {
			return // already gone, which is the same answer
		}
		if _, rerr := io.ReadFull(conn, make([]byte, 2)); rerr == nil {
			t.Error("a connection past the limit was answered")
		}
	})

	t.Run("the operator is told once, not once per connection", func(t *testing.T) {
		// Reporting every refused connection would let a flood generate work
		// of its own, which is the same reason a malformed query is not
		// reported at all.
		for range 5 {
			if conn, err := net.Dial("tcp", addr); err == nil {
				conn.Close()
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if len(reports) != 1 {
			t.Errorf("the server reported %d times: %v", len(reports), reports)
		}
		if len(reports) == 1 && !strings.Contains(reports[0], "connection slots") {
			t.Errorf("report = %q, want it to name what ran out", reports[0])
		}
	})

	t.Run("a slot freed lets the next one in", func(t *testing.T) {
		held[0].Close()

		// The reader has to notice the close before the slot comes back, so
		// this retries rather than assuming the handover is instant.
		deadline := time.Now().Add(testTimeout)
		for {
			conn, err := net.Dial("tcp", addr)
			if err == nil {
				if derr := conn.SetDeadline(time.Now().Add(testTimeout)); derr != nil {
					t.Fatalf("set deadline: %v", derr)
				}
				if _, werr := conn.Write(framed(packQuery(t, "www.example.com.", zone.TypeA))); werr == nil {
					if _, rerr := io.ReadFull(conn, make([]byte, 2)); rerr == nil {
						conn.Close()
						return
					}
				}
				conn.Close()
			}
			if time.Now().After(deadline) {
				t.Fatal("no connection got in after a slot was freed")
			}
		}
	})
}

// framed wraps a query in the two-octet length prefix of RFC 1035 §4.2.2.
func framed(q []byte) []byte {
	out := make([]byte, 2+len(q))
	binary.BigEndian.PutUint16(out, uint16(len(q)))
	copy(out[2:], q)
	return out
}

// A port of zero asks the kernel to choose, and the two protocols choose from
// separate tables, which is why choosing has to be allowed to be retried.
func TestEphemeralPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		want bool
	}{
		{":0", true},
		{"127.0.0.1:0", true},
		{"[::1]:0", true},
		{":", true},
		{"127.0.0.1:", true},
		{":53", false},
		{"127.0.0.1:53", false},
		{"[::1]:5353", false},
		{"127.0.0.1", false}, // no port at all: bind's to reject, not ours
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			if got := ephemeralPort(tc.addr); got != tc.want {
				t.Errorf("ephemeralPort(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// An address somebody chose is not retried. Quietly moving to another port
// would be worse than failing: the server would be answering somewhere nobody
// configured and nobody is querying.
func TestListenFailsOnATakenPort(t *testing.T) {
	t.Parallel()

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer held.Close()

	s := NewServer(Config{Addr: held.Addr().String(), UDPSockets: 1})
	s.SetSnapshot(resolveFixture(t))

	if err := s.Start(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if serr := s.Shutdown(ctx); serr != nil {
			t.Errorf("shutdown: %v", serr)
		}
		t.Fatalf("started on %s, which is already taken for connections", held.Addr())
	} else if !strings.Contains(err.Error(), held.Addr().String()) {
		t.Errorf("error = %v, want it to name %s", err, held.Addr())
	}
}

// Starting on an ephemeral port has to work while the machine is busy taking
// ephemeral ports, which is the situation the whole retry exists for.
func TestEphemeralPortsUnderContention(t *testing.T) {
	t.Parallel()

	// Held open for the whole test, so the numbers stay unavailable.
	var held []net.Listener
	t.Cleanup(func() {
		for _, l := range held {
			l.Close()
		}
	})
	for range 256 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("cannot hold enough ports on this machine: %v", err)
		}
		held = append(held, l)
	}

	snap := resolveFixture(t)
	for i := range 40 {
		s := NewServer(Config{Addr: "127.0.0.1:0", UDPSockets: 1})
		s.SetSnapshot(snap)
		if err := s.Start(); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		err := s.Shutdown(ctx)
		cancel()
		if err != nil {
			t.Fatalf("shutdown %d: %v", i, err)
		}
	}
}
