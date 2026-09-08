package dns

import (
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestLoadMeterSeesTheKernelDropping is the switch of D38 closed and opened
// again, without a load generator: a socket with a small receive buffer that
// nobody reads from loses datagrams in milliseconds, and losing them is the
// whole condition.
func TestLoadMeterSeesTheKernelDropping(t *testing.T) {
	t.Parallel()

	conn := smallSocket(t)
	m := newLoadMeter([]*net.UDPConn{conn}, nil)

	// Nothing has been sent, so nothing has been dropped.
	m.window()
	if m.underLoad() {
		t.Fatal("a socket nobody has sent to reads as overloaded")
	}

	flood(t, conn)
	m.window()
	if !m.underLoad() {
		t.Error("the kernel dropped datagrams on this socket and the meter did not notice")
	}

	// The flood is over and the counter stops moving, which is the state
	// coming back on the next window rather than on a restart.
	m.window()
	if m.underLoad() {
		t.Error("the state stayed after the drops stopped")
	}
}

// TestLoadMeterSaysNothingWhenTheKernelWillNot covers a kernel that does not
// answer: the server goes on answering everybody, and hears about it once.
func TestLoadMeterSaysNothingWhenTheKernelWillNot(t *testing.T) {
	t.Parallel()

	conn := smallSocket(t)
	var faults int
	m := newLoadMeter([]*net.UDPConn{conn}, func(error) { faults++ })

	// A closed socket is the same shape of failure as an option a kernel does
	// not implement: the counter cannot be read.
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for range 3 {
		m.window()
		if m.underLoad() {
			t.Fatal("a meter that cannot read the counter refuses clients anyway")
		}
	}
	if faults != 1 {
		t.Errorf("the fault was reported %d times, want once", faults)
	}
}

// TestServerUnderLoad covers the state a server reports before it starts and
// after it stops, neither of which is a server that cannot keep up.
func TestServerUnderLoad(t *testing.T) {
	t.Parallel()

	s := NewServer(Config{Addr: "127.0.0.1:0"})
	if s.UnderLoad() {
		t.Error("a server that has not started is under load")
	}

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if s.UnderLoad() {
		t.Error("a server that has stopped is under load")
	}
}

// smallSocket returns a socket with the smallest receive buffer the kernel
// will give, so that filling it is a matter of a few hundred datagrams.
func smallSocket(t *testing.T) *net.UDPConn {
	t.Helper()

	cfg := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var setErr error
			if err := c.Control(func(fd uintptr) {
				setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 1024)
			}); err != nil {
				return err
			}
			return setErr
		},
	}

	packet, err := cfg.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	conn, ok := packet.(*net.UDPConn)
	if !ok {
		t.Fatalf("listening gave a %T", packet)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// flood sends more at a socket than its receive buffer holds, and nothing
// reads it, which is what makes the kernel discard the rest.
func flood(t *testing.T, conn *net.UDPConn) {
	t.Helper()

	sender, err := net.Dial("udp", conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer sender.Close()

	msg := make([]byte, 512)
	for range 500 {
		if _, werr := sender.Write(msg); werr != nil {
			t.Fatalf("write: %v", werr)
		}
	}
}
