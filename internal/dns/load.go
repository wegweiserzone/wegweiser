package dns

import (
	"sync/atomic"
	"time"
)

// loadWindow is how often the load state is decided.
//
// It is a cadence rather than a policy: it says how quickly the switch follows
// the traffic, not who is affected by it, and no deployment has a reason to
// prefer another one (D37).
const loadWindow = time.Second

// cacheLine is the padding one reader's counters get, so that two readers on
// two cores do not fight over the line the other is writing.
const cacheLine = 64

// loadMeter reports whether the datagram readers are keeping up.
//
// Each reader records the time it spends waiting for a datagram to arrive.
// Once a window the meter asks whether any of them waited at all: a window in
// which none did is the arrival rate having overtaken what this machine
// serves, which is what D37 defines as under load. Nothing here is
// configurable, because the capacity it measures against is the machine's own
// and nobody has to guess at it.
type loadMeter struct {
	readers []readerLoad

	// waited is what each reader's counter read at the end of the last
	// window. Only the goroutine running the windows touches it.
	waited []int64

	under atomic.Bool
	stop  chan struct{}
	done  chan struct{}
}

// readerLoad is one reader's share of the meter.
type readerLoad struct {
	// waited is how long this reader has spent waiting for a datagram since
	// the server started, in nanoseconds.
	waited atomic.Int64

	// since is when the wait it is in now began, and zero while it is
	// answering. Without it a reader blocked across an entire window would add
	// nothing to waited and a quiet server would read as an overloaded one,
	// which is the wrong answer in the most common case there is.
	since atomic.Int64

	_ [cacheLine - 16]byte
}

// waiting records that the reader is about to block for a datagram.
func (r *readerLoad) waiting(at time.Time) { r.since.Store(at.UnixNano()) }

// woke records that a datagram arrived, and adds the wait to the total.
func (r *readerLoad) woke(at time.Time) {
	if since := r.since.Swap(0); since != 0 {
		r.waited.Add(at.UnixNano() - since)
	}
}

// newLoadMeter returns a meter for readers datagram readers.
func newLoadMeter(readers int) *loadMeter {
	return &loadMeter{
		readers: make([]readerLoad, readers),
		waited:  make([]int64, readers),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// reader returns the share of the meter one reader writes to.
func (m *loadMeter) reader(i int) *readerLoad { return &m.readers[i] }

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
		case now := <-tick.C:
			m.window(now)
		}
	}
}

// close stops the windows and waits for the goroutine running them.
func (m *loadMeter) close() {
	close(m.stop)
	<-m.done
}

// window decides the state for the window ending at now.
func (m *loadMeter) window(now time.Time) {
	if len(m.readers) == 0 {
		return
	}

	idled := false
	for i := range m.readers {
		waited := m.readers[i].waited.Load()
		if waited != m.waited[i] {
			idled = true
		}
		m.waited[i] = waited

		// A reader still blocked has waited for part of this window without
		// having added it to the total yet.
		if since := m.readers[i].since.Load(); since != 0 && since < now.UnixNano() {
			idled = true
		}
	}
	m.under.Store(!idled)
}

// UnderLoad reports whether the datagram readers have stopped idling, which is
// what D37 settles as the meaning of load on this server.
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
