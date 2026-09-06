package dns

import (
	"testing"
	"time"
)

// TestLoadMeter walks the switch of D37 without a load generator, which is
// half of why the signal is the readers' own idleness rather than a rate: the
// state can be driven directly.
func TestLoadMeter(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := newLoadMeter(2)

	// Nothing has happened at all: both readers are blocked on a receive,
	// which is what a server nobody is querying looks like.
	m.reader(0).waiting(now.Add(-time.Minute))
	m.reader(1).waiting(now.Add(-time.Minute))
	m.window(now)
	if m.underLoad() {
		t.Error("a server waiting for its first query reads as overloaded")
	}

	// Both readers answer without ever waiting: datagrams were queued every
	// time they came back for one.
	m.reader(0).since.Store(0)
	m.reader(1).since.Store(0)
	m.window(now.Add(loadWindow))
	if !m.underLoad() {
		t.Error("a window in which no reader waited is not under load")
	}

	// One reader gets a moment to itself, which is enough: something was idle,
	// so the queries are not arriving faster than they are answered.
	m.reader(1).waiting(now.Add(loadWindow))
	m.reader(1).woke(now.Add(loadWindow + time.Millisecond))
	m.window(now.Add(2 * loadWindow))
	if m.underLoad() {
		t.Error("a window with an idle reader in it is still under load")
	}

	// And back, on the next window that has none.
	m.window(now.Add(3 * loadWindow))
	if !m.underLoad() {
		t.Error("the state did not come back on the next saturated window")
	}
}

// TestLoadMeterCountsTheWaitInProgress covers the reader that is blocked for
// longer than a whole window: it adds nothing to its total, and a meter
// looking only at totals would call that server overloaded.
func TestLoadMeterCountsTheWaitInProgress(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := newLoadMeter(1)

	m.reader(0).waiting(now.Add(-time.Hour))
	for i := range 3 {
		m.window(now.Add(time.Duration(i) * loadWindow))
		if m.underLoad() {
			t.Fatalf("window %d: a reader blocked for an hour reads as busy", i)
		}
	}

	// The datagram finally arrives, and the wait lands in the total.
	m.reader(0).woke(now.Add(3 * loadWindow))
	if waited := m.reader(0).waited.Load(); waited <= 0 {
		t.Errorf("the wait was not counted: %d ns", waited)
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
