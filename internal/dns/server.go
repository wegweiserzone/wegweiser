package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Defaults for the knobs a [Config] leaves unset.
const (
	// defaultAddr is the port RFC 1035 §4.2 assigns, on every address the host
	// has. Reaching it without root is what CAP_NET_BIND_SERVICE is for
	// (architecture invariant 7).
	defaultAddr = ":53"

	// defaultTCPIdle is how long a connection may sit without a query.
	// RFC 7766 §6.2.3 asks for an idle period "of the order of seconds": long
	// enough that a client which fell back from a truncated datagram can ask
	// its follow-up questions, short enough that idle connections are not a
	// way to hold the server's file descriptors hostage.
	defaultTCPIdle = 10 * time.Second

	// defaultTCPClients is how many connections may be open at once.
	//
	// Each costs a goroutine, a Responder and buffers that grow to the largest
	// message the client has sent, up to the 64 KiB the length prefix allows.
	// TCP is where a datagram client is told to go when an answer does not fit,
	// so an unbounded accept loop is a cheap way to take the server's memory.
	// BIND, NSD and Knot all default to this number.
	defaultTCPClients = 150

	// maxUDPQuery is the largest datagram read. A question plus an OPT record
	// with options is far below this; anything above it is not a query that
	// this server has an answer for, and reading only the first part of it
	// produces a message that fails to parse, which is the right outcome.
	maxUDPQuery = 4096
)

// Config is what a [Server] needs to start. The zero value is usable and takes
// every default below.
type Config struct {
	// Addr is the address to listen on, as host:port. Empty means [defaultAddr].
	Addr string

	// Limits bound the messages the server sends. See [Limits].
	Limits Limits

	// UDPSockets is how many datagram sockets to open on Addr, each with a
	// reader of its own. Zero means one per available processor.
	UDPSockets int

	// TCPIdleTimeout is how long a connection may sit without a query before it
	// is closed. Zero means [defaultTCPIdle].
	TCPIdleTimeout time.Duration

	// MaxTCPClients is how many connections may be open at once. A connection
	// arriving when they all are is closed straight away rather than queued,
	// so that the accept loop never stalls behind a client that has stopped
	// talking. Zero means [defaultTCPClients]; a negative value means no bound
	// at all, which is a decision an operator makes on purpose.
	MaxTCPClients int

	// OnError is called for faults the server cannot act on itself: a socket
	// that fails to read, a connection that fails to write, a response that
	// fails to pack. Never for a merely malformed query, which is ordinary
	// traffic that an attacker could otherwise use to generate work.
	//
	// A hook rather than a logger: how the process logs is the wiring's
	// decision, not this package's. May be nil.
	OnError func(error)

	// Transfers decides who may pull a whole zone off this server. Nil allows
	// nobody, which is what an unconfigured server does.
	Transfers Transfers

	// Keys are the TSIG keys this server verifies and signs with. Empty holds
	// none, and then a signed request is refused with BADKEY.
	Keys Keyring

	// History supplies the record changes between two serials, which is the one
	// thing an incremental transfer needs and no snapshot holds. Nil answers
	// every incremental request with a whole zone, which RFC 1995 §2 allows.
	History History

	// Observe is called once per query, after the response has been written.
	// Both the metrics and the live query stream are fed from it; composing
	// them is the wiring's job.
	//
	// It runs on the goroutine that read the query and must not block: an
	// observer that waits stops a reader. Nil means nothing is watching, and
	// then not even the clock is read.
	Observe func(Event)
}

// Server answers queries from a snapshot over UDP and TCP.
//
// The snapshot sits in an [atomic.Pointer] and is the only coupling between the
// control and data planes (invariant 2). A commit publishes a new one with
// [Server.SetSnapshot]; a query in flight keeps the one it started with, so
// neither side ever waits for the other.
type Server struct {
	cfg Config

	current atomic.Pointer[Snapshot]

	// transferring is who may pull a whole zone, held the same way and for the
	// same reason: the setting lives in the database and changes while the
	// server runs.
	transferring atomic.Pointer[transferHolder]

	// keys is where a TSIG key is resolved, held the same way: a key is created
	// and revoked while the server runs.
	keys atomic.Pointer[keyHolder]

	started atomic.Bool
	closing atomic.Bool

	udp []*net.UDPConn
	tcp *net.TCPListener

	// slots bounds how many connections may be open at once. A send takes a
	// slot and a receive gives it back; it is nil when the operator asked for
	// no bound.
	slots chan struct{}
	// saturated is whether the last connection to arrive was turned away. It
	// makes the report edge-triggered: an operator hears once that the bound
	// is being reached, rather than once per refused connection, which would
	// be a way to make the server generate work.
	saturated atomic.Bool

	// conns are the open connections, kept so that a shutdown can wake their
	// readers instead of waiting out the idle timeout on each.
	mu    sync.Mutex
	conns map[*net.TCPConn]struct{}

	wg sync.WaitGroup
}

// NewServer returns a server configured but not yet listening. It answers for
// nothing until a snapshot is published.
func NewServer(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = defaultAddr
	}
	if cfg.UDPSockets <= 0 {
		cfg.UDPSockets = runtime.GOMAXPROCS(0)
	}
	if cfg.TCPIdleTimeout <= 0 {
		cfg.TCPIdleTimeout = defaultTCPIdle
	}
	if cfg.MaxTCPClients == 0 {
		cfg.MaxTCPClients = defaultTCPClients
	}

	s := &Server{cfg: cfg, conns: make(map[*net.TCPConn]struct{})}
	if cfg.MaxTCPClients > 0 {
		s.slots = make(chan struct{}, cfg.MaxTCPClients)
	}
	s.SetTransfers(cfg.Transfers)
	s.SetKeys(cfg.Keys)
	return s
}

// SetSnapshot publishes a snapshot for every query from here on.
//
// It is one atomic store and never blocks. Queries already running finish
// against the snapshot they started with, which is collected once the last of
// them lets go.
func (s *Server) SetSnapshot(snap *Snapshot) { s.current.Store(snap) }

// Snapshot returns the snapshot queries are currently answered from.
func (s *Server) Snapshot() *Snapshot { return s.current.Load() }

// Addr returns the address the server actually bound, which is what a
// configured port of 0 has to be read back from. It returns nil before
// [Server.Start].
func (s *Server) Addr() net.Addr {
	if len(s.udp) == 0 {
		return nil
	}
	return s.udp[0].LocalAddr()
}

// Start binds the sockets and begins answering, returning as soon as the
// listeners are up.
func (s *Server) Start() error {
	if s.started.Swap(true) {
		return errors.New("dns: the server is already started")
	}

	if err := s.listen(); err != nil {
		s.closeListeners()
		return err
	}

	for _, conn := range s.udp {
		s.wg.Add(1)
		go s.readUDP(conn)
	}
	s.wg.Add(1)
	go s.acceptTCP()

	return nil
}

// listenAttempts is how many ephemeral ports are tried before giving up.
const listenAttempts = 10

// listen binds every socket the server answers on.
func (s *Server) listen() error {
	attempts := 1
	if ephemeralPort(s.cfg.Addr) {
		attempts = listenAttempts
	}

	var err error
	for range attempts {
		if err = s.bind(); err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return err
		}
		s.closeListeners()
		// Cleared as well as closed, so the next attempt does not append to a
		// set of sockets that has been given up on. Safe only here: nothing
		// reads these until Start has seen this function succeed.
		s.udp, s.tcp = nil, nil
	}
	return err
}

// ephemeralPort reports whether the address asks the kernel to choose.
func ephemeralPort(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && (port == "0" || port == "")
}

// bind opens one full set of sockets.
//
// The datagram sockets carry SO_REUSEPORT and bind the same address, so the
// kernel spreads packets across them itself. That keeps a single queue from
// being the bottleneck at the rate D12 asks for, and gives each reader its own
// socket buffer.
func (s *Server) bind() error {
	lc := net.ListenConfig{Control: reusePort}
	addr := s.cfg.Addr

	for range s.cfg.UDPSockets {
		pc, err := lc.ListenPacket(context.Background(), "udp", addr)
		if err != nil {
			return fmt.Errorf("listen on %s for datagrams: %w", addr, err)
		}
		conn, ok := pc.(*net.UDPConn)
		if !ok {
			pc.Close()
			return fmt.Errorf("listen on %s for datagrams: got a %T", addr, pc)
		}
		s.udp = append(s.udp, conn)
		addr = conn.LocalAddr().String()
	}

	// No SO_REUSEPORT on the stream socket. There is one accept loop and
	// nothing to spread, and the option would let any process running as this
	// user bind the same port and take connections away from us.
	l, err := new(net.ListenConfig).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s for connections: %w", addr, err)
	}
	tcp, ok := l.(*net.TCPListener)
	if !ok {
		l.Close()
		return fmt.Errorf("listen on %s for connections: got a %T", addr, l)
	}
	s.tcp = tcp

	return nil
}

// reusePort sets SO_REUSEPORT before the socket is bound, which is the only
// moment the option can be set.
func reusePort(_, _ string, c syscall.RawConn) error {
	var setErr error
	err := c.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	if err != nil {
		return err
	}
	return setErr
}

// Shutdown stops the server and waits for the queries in flight, or for ctx to
// expire. Listeners close first so nothing new arrives, and open connections
// have their read deadline moved into the past, waking a reader that would
// otherwise wait out its idle period. A query being answered is answered.
func (s *Server) Shutdown(ctx context.Context) error {
	if !s.started.Load() || s.closing.Swap(true) {
		return nil
	}

	s.closeListeners()

	s.mu.Lock()
	for conn := range s.conns {
		// A connection that will not take a deadline is one that has already
		// gone; closing it reaches the same place by the shorter route.
		if err := conn.SetReadDeadline(time.Now()); err != nil {
			conn.Close()
		}
	}
	s.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("dns: the server did not drain: %w", ctx.Err())
	}
}

// closeListeners shuts the sockets, which is what unblocks the readers.
func (s *Server) closeListeners() {
	for _, conn := range s.udp {
		conn.Close()
	}
	if s.tcp != nil {
		s.tcp.Close()
	}
}

// report hands a fault to the configured hook, if there is one.
func (s *Server) report(err error) {
	if s.cfg.OnError != nil && !s.closing.Load() {
		s.cfg.OnError(err)
	}
}

// readUDP answers every datagram arriving on one socket, in this goroutine.
//
// Nothing is handed off. Resolving and packing takes well under a microsecond
// and waits on nothing, so a goroutine per packet would buy no parallelism and
// cost a scheduling round trip per query. The parallelism comes from
// SO_REUSEPORT giving each reader its own queue.
//
// This goroutine owns the socket for its whole life, and with it the buffers
// and the [Responder]. Nothing is pooled or shared.
func (s *Server) readUDP(conn *net.UDPConn) {
	defer s.wg.Done()

	r := NewResponder(s.cfg.Limits)
	r.keys = &s.keys
	in := make([]byte, maxUDPQuery)
	out := make([]byte, r.limits.MaxUDPResponse)

	for {
		n, from, err := conn.ReadFromUDPAddrPort(in)
		if err != nil {
			if s.closing.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			s.report(fmt.Errorf("read a datagram on %s: %w", conn.LocalAddr(), err))
			return
		}

		s.answerDatagram(conn, r, in[:n], out, from)
	}
}

// answerDatagram answers one query, or drops it where §2.2 says to drop it.
func (s *Server) answerDatagram(
	conn *net.UDPConn, r *Responder, query, out []byte, from netip.AddrPort,
) {
	start := s.startedAt()

	packed, err := r.Respond(s.current.Load(), query, UDP, out)
	if err != nil {
		// A malformed query is traffic, not a fault. Silence is the answer
		// RFC 1035 leaves for a message there is nothing safe to reply to, and
		// reporting each one would let a flood generate work of its own.
		if !errors.Is(err, ErrUnanswerable) {
			s.report(fmt.Errorf("answer a datagram from %s: %w", from, err))
		}
		s.observe(r, UDP, from, start, 0)
		return
	}
	sent := len(packed)
	if _, err := conn.WriteToUDPAddrPort(packed, from); err != nil {
		if !s.closing.Load() {
			s.report(fmt.Errorf("write a response to %s: %w", from, err))
		}
		sent = 0
	}
	s.observe(r, UDP, from, start, sent)
}

// acceptTCP takes connections until the listener is closed.
func (s *Server) acceptTCP() {
	defer s.wg.Done()

	for {
		conn, err := s.tcp.AcceptTCP()
		if err != nil {
			if s.closing.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			s.report(fmt.Errorf("accept a connection on %s: %w", s.tcp.Addr(), err))
			return
		}

		if !s.takeSlot() {
			// Closed rather than queued: a connection we are not going to read
			// from is one the client should find out about now, so it can fall
			// back or retry, instead of waiting on a server that will never
			// answer. Blocking the accept loop instead would let one stalled
			// client hold up every new one for a whole idle period.
			conn.Close()
			continue
		}

		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

// takeSlot claims one of the connection slots, and reports whether there was
// one to claim.
func (s *Server) takeSlot() bool {
	if s.slots == nil {
		return true
	}
	select {
	case s.slots <- struct{}{}:
		s.saturated.Store(false)
		return true
	default:
		if !s.saturated.Swap(true) {
			s.report(fmt.Errorf(
				"all %d connection slots are in use, so further connections are being closed; "+
					"raise the limit if this is ordinary load rather than a flood",
				s.cfg.MaxTCPClients))
		}
		return false
	}
}

// freeSlot gives a connection slot back.
func (s *Server) freeSlot() {
	if s.slots != nil {
		<-s.slots
	}
}

// serveConn answers the queries arriving on one connection, one after the next.
//
// RFC 7766 §6.2.1.1 permits answering pipelined queries concurrently and out of
// order; this does not. Every answer is an in-memory lookup of a few hundred
// nanoseconds, so nothing is long enough to be worth overtaking, and a
// goroutine per question would let one connection spawn an unbounded number of
// them. Throughput across connections is unaffected: each has its own reader.
func (s *Server) serveConn(conn *net.TCPConn) {
	defer s.wg.Done()
	defer s.freeSlot()
	defer conn.Close()

	if !s.trackConn(conn) {
		return
	}
	defer s.forgetConn(conn)

	r := NewResponder(s.cfg.Limits)
	r.keys = &s.keys
	from := remoteAddrPort(conn)
	var (
		length [2]byte
		query  []byte
		frame  []byte
	)

	for !s.closing.Load() {
		if err := conn.SetReadDeadline(time.Now().Add(s.cfg.TCPIdleTimeout)); err != nil {
			return
		}
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			// A client that goes away, or one that has nothing more to ask, is
			// the ordinary end of a connection rather than a fault.
			return
		}

		// The length prefix of RFC 1035 §4.2.2 is two octets, so the size of a
		// message is bounded by the framing itself and needs no limit of ours.
		n := int(binary.BigEndian.Uint16(length[:]))
		if cap(query) < n {
			query = make([]byte, n)
		}
		if _, err := io.ReadFull(conn, query[:n]); err != nil {
			return
		}

		start := s.startedAt()

		// A transfer is one question and many messages, so it leaves the loop
		// that writes one response per query read.
		if apex, qtype, ok := transferQuery(query[:n]); ok {
			if err := s.transfer(conn, query[:n], from, apex, qtype, start); err != nil {
				if !s.closing.Load() {
					s.report(fmt.Errorf("%s of %s to %s: %w",
						qtype, apex, conn.RemoteAddr(), err))
				}
				return
			}
			continue
		}

		packed, err := r.Respond(s.current.Load(), query[:n], TCP, frame[:0])
		if err != nil {
			// On a stream there is no answering a later query without having
			// answered this one, and leaving the client waiting says less than
			// hanging up does.
			if !errors.Is(err, ErrUnanswerable) {
				s.report(fmt.Errorf("answer a query from %s: %w", conn.RemoteAddr(), err))
			}
			s.observe(r, TCP, from, start, 0)
			return
		}

		// The length prefix of RFC 1035 §4.2.2 is two octets, and Respond has
		// already cut a stream response to what fits in it. The guard is here
		// because sending a length that means something other than the message
		// behind it would be worse than hanging up.
		size := len(packed)
		if size > math.MaxUint16 {
			s.report(fmt.Errorf("a response of %d octets cannot be framed", size))
			s.observe(r, TCP, from, start, 0)
			return
		}

		frame = growTo(frame, size+2)
		binary.BigEndian.PutUint16(frame, uint16(size))
		copy(frame[2:], packed)

		if err := conn.SetWriteDeadline(time.Now().Add(s.cfg.TCPIdleTimeout)); err != nil {
			s.observe(r, TCP, from, start, 0)
			return
		}
		if _, err := conn.Write(frame[:size+2]); err != nil {
			if !s.closing.Load() {
				s.report(fmt.Errorf("write a response to %s: %w", conn.RemoteAddr(), err))
			}
			s.observe(r, TCP, from, start, 0)
			return
		}
		s.observe(r, TCP, from, start, size)
	}
}

// remoteAddrPort is who a connection is with, in the form an event carries.
func remoteAddrPort(conn *net.TCPConn) netip.AddrPort {
	if a, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		return a.AddrPort()
	}
	return netip.AddrPort{}
}

// growTo returns a slice of at least n octets, reusing b when it is big enough.
// A connection therefore settles at the size of the largest response it has
// sent instead of allocating per query.
func growTo(b []byte, n int) []byte {
	if cap(b) < n {
		return make([]byte, n)
	}
	return b[:n]
}

// trackConn registers a connection so a shutdown can wake it, and reports
// whether the server is still open for business.
func (s *Server) trackConn(conn *net.TCPConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing.Load() {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

// forgetConn removes a connection that has closed.
func (s *Server) forgetConn(conn *net.TCPConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}
