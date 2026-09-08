package dns

import (
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// loadWindow is how often the load state is decided.
//
// It is a cadence rather than a policy: it says how quickly the switch follows
// the traffic, not who is affected by it (D37).
const loadWindow = time.Second

// SO_MEMINFO answers with the memory counters of one socket. The last of them
// is sk_drops, the datagrams the kernel discarded because nobody took them in
// time, and it is the only one this server asks about.
const (
	memInfoVars  = 9
	memInfoDrops = 8
)

// loadMeter reports whether the kernel is throwing queries away.
//
// A window in which a socket's drop counter moved is a window this machine
// could not serve, which is what D38 settles as the meaning of load here. It
// is a fact the kernel keeps rather than a number anybody picked, and reading
// it costs one getsockopt per socket per window. Nothing here runs on the path
// of a query.
type loadMeter struct {
	sockets []*net.UDPConn

	// drops is what each socket's counter read at the end of the last window.
	// Only the goroutine running the windows touches it.
	drops []uint32

	under atomic.Bool

	// report is how the one fault this has gets out: a kernel that will not
	// answer. Said once and then never again, because a server repeating it
	// every second is a server whose log nobody reads.
	report func(error)
	silent bool

	stop chan struct{}
	done chan struct{}
}

// newLoadMeter returns a meter watching the sockets a server bound.
func newLoadMeter(sockets []*net.UDPConn, report func(error)) *loadMeter {
	m := &loadMeter{
		sockets: sockets,
		drops:   make([]uint32, len(sockets)),
		report:  report,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}

	// Seeded here, so that a socket which lost datagrams before anybody was
	// watching does not read as a socket losing them now.
	for i, conn := range sockets {
		if drops, err := socketDrops(conn); err == nil {
			m.drops[i] = drops
		}
	}
	return m
}

// underLoad reports the state as the last window left it.
func (m *loadMeter) underLoad() bool { return m.under.Load() }

// run decides the state once a window until the meter is stopped.
func (m *loadMeter) run() {
	defer close(m.done)

	tick := time.NewTicker(loadWindow)
	defer tick.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-tick.C:
			m.window()
		}
	}
}

// close stops the windows and waits for the goroutine running them.
func (m *loadMeter) close() {
	close(m.stop)
	<-m.done
}

// window decides the state for the window that has just ended.
func (m *loadMeter) window() {
	if m.silent || len(m.sockets) == 0 {
		return
	}

	dropping := false
	for i, conn := range m.sockets {
		drops, err := socketDrops(conn)
		if err != nil {
			// A kernel that will not say leaves this server answering
			// everybody, which is what it did before it had cookies at all.
			m.silent = true
			m.under.Store(false)
			if m.report != nil {
				m.report(fmt.Errorf(
					"dns: the kernel will not say how many queries it dropped, so a client "+
						"without a cookie is never refused: %w", err))
			}
			return
		}

		// Any change at all, rather than an increase: the counter is 32 bits
		// and wraps, and either way what moved it is a query that never
		// reached this server.
		if drops != m.drops[i] {
			dropping = true
		}
		m.drops[i] = drops
	}
	m.under.Store(dropping)
}

// socketDrops returns how many datagrams the kernel has discarded on a socket
// since it was opened.
//
// SO_MEMINFO arrived in Linux 4.14 and x/sys/unix has no typed accessor for
// it, so the option is read as the array of counters it is. An older kernel
// answers ENOPROTOOPT, which [loadMeter.window] treats as the kernel declining
// to say.
func socketDrops(conn *net.UDPConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}

	var (
		info  [memInfoVars]uint32
		errno syscall.Errno
	)
	if cerr := raw.Control(func(fd uintptr) {
		size := uint32(unsafe.Sizeof(info))
		_, _, errno = unix.Syscall6(unix.SYS_GETSOCKOPT, fd,
			uintptr(unix.SOL_SOCKET), uintptr(unix.SO_MEMINFO),
			uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&size)), 0)
	}); cerr != nil {
		return 0, cerr
	}
	if errno != 0 {
		return 0, errno
	}
	return info[memInfoDrops], nil
}

// UnderLoad reports whether the kernel is dropping queries this server did not
// get to in time, which is what D38 settles as the meaning of load.
//
// It is the state the last window left, so it is at most a window out of date
// and never a judgement made per query. A server that is not running is not
// under load.
func (s *Server) UnderLoad() bool {
	if meter := s.load.Load(); meter != nil {
		return meter.underLoad()
	}
	return false
}
