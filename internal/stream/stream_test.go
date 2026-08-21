package stream_test

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/stream"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// at is a fixed moment, so that a test controls which sampling window an
// exchange falls into instead of racing the clock.
var at = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

// event is one exchange, with the fields a test does not care about filled in.
func event(shape ...func(*dns.Event)) dns.Event {
	ev := dns.Event{
		At:        at,
		Latency:   250 * time.Microsecond,
		Client:    netip.MustParseAddrPort("192.0.2.50:41234"),
		Transport: dns.UDP,
		Name:      "www.example.com.",
		Type:      zone.TypeA,
		Class:     zone.ClassIN,
		Size:      100,
	}
	for _, s := range shape {
		s(&ev)
	}
	return ev
}

// next reads one exchange, failing the test rather than hanging if none comes.
func next(t *testing.T, s *stream.Subscription) dns.Event {
	t.Helper()

	select {
	case ev := <-s.Events():
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no exchange arrived")
		return dns.Event{}
	}
}

// empty asserts that nothing is waiting, which is what a filter that excluded
// everything looks like.
func empty(t *testing.T, s *stream.Subscription) {
	t.Helper()

	select {
	case ev := <-s.Events():
		t.Fatalf("an exchange arrived that the filter excludes: %s %s", ev.Name, ev.Type)
	default:
	}
}

func subscribe(t *testing.T, h *stream.Hub, f stream.Filter) *stream.Subscription {
	t.Helper()

	s, err := h.Subscribe(f)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestHubDelivers(t *testing.T) {
	t.Parallel()

	h := stream.NewHub(stream.Options{})
	s := subscribe(t, h, stream.Filter{})

	sent := event()
	h.Observe(sent)

	got := next(t, s)
	if got.Name != sent.Name || got.Type != sent.Type || got.Client != sent.Client {
		t.Errorf("got %s %s from %s, want the exchange that was observed",
			got.Name, got.Type, got.Client)
	}
	if st := s.Stats(); st.Matched != 1 || st.Sent != 1 || st.Ratio != 1 {
		t.Errorf("stats = %+v, want one matched, one sent, no sampling", st)
	}
}

// Nobody watching has to cost nothing, because the query path pays for it
// once per query whether or not anybody ever subscribes.
func TestHubWithoutWatchers(t *testing.T) {
	t.Parallel()

	h := stream.NewHub(stream.Options{})
	h.Observe(event())
	if got := h.Watchers(); got != 0 {
		t.Errorf("watchers = %d, want 0", got)
	}
}

func TestFilters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		filter stream.Filter
		ev     dns.Event
		want   bool
	}{
		{"the zero filter matches everything", stream.Filter{}, event(), true},

		{"a name matches itself",
			stream.Filter{Name: zone.MustParseName("www.example.com.")}, event(), true},
		{"a name matches what is below it",
			stream.Filter{Name: zone.MustParseName("example.com.")}, event(), true},
		{"a name does not match what is beside it",
			stream.Filter{Name: zone.MustParseName("example.net.")}, event(), false},
		// "notexample.com." ends with the octets of "example.com." and is not
		// below it. This is the mistake the API's own zone lookup was fixed
		// for once already.
		{"a name does not match a longer label ending the same way",
			stream.Filter{Name: zone.MustParseName("example.com.")},
			event(func(e *dns.Event) { e.Name = "www.notexample.com." }), false},
		// RFC 4343: names compare case-insensitively, and 0x20 encoding means
		// a real query is rarely all lowercase.
		{"a name matches whatever case it was asked in",
			stream.Filter{Name: zone.MustParseName("example.com.")},
			event(func(e *dns.Event) { e.Name = "WwW.ExAmPlE.cOm." }), true},
		// A dot written \. is inside a label, not between two.
		{"a dot inside a label is not a label boundary",
			stream.Filter{Name: zone.MustParseName("example.com.")},
			event(func(e *dns.Event) { e.Name = `a\.example.com.` }), false},
		{"the root matches everything",
			stream.Filter{Name: zone.Root}, event(), true},

		{"a type matches", stream.Filter{Types: []zone.RRType{zone.TypeA}}, event(), true},
		{"another type does not",
			stream.Filter{Types: []zone.RRType{zone.TypeAAAA}}, event(), false},
		{"one of several types matches",
			stream.Filter{Types: []zone.RRType{zone.TypeAAAA, zone.TypeA}}, event(), true},

		{"a response code matches",
			stream.Filter{Rcodes: []int{0}}, event(), true},
		{"looking for failures skips the successes",
			stream.Filter{Rcodes: []int{3}}, event(), false},
		{"looking for failures finds them",
			stream.Filter{Rcodes: []int{3}},
			event(func(e *dns.Event) { e.Rcode = 3 }), true},

		{"a client network matches",
			stream.Filter{Client: netip.MustParsePrefix("192.0.2.0/24")}, event(), true},
		{"another client network does not",
			stream.Filter{Client: netip.MustParsePrefix("198.51.100.0/24")}, event(), false},

		{"every part has to match",
			stream.Filter{
				Name:  zone.MustParseName("example.com."),
				Types: []zone.RRType{zone.TypeAAAA},
			}, event(), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := stream.NewHub(stream.Options{})
			s := subscribe(t, h, tc.filter)
			h.Observe(tc.ev)

			if tc.want {
				next(t, s)
				return
			}
			empty(t, s)
			if got := s.Stats().Matched; got != 0 {
				t.Errorf("matched = %d, want 0", got)
			}
		})
	}
}

// D9: the filter runs before anything is buffered, so a watcher looking at one
// zone gets a complete stream however busy the rest of the server is. Without
// that, a narrow filter would compete for buffer space with every query the
// watcher did not ask about.
func TestANarrowFilterStaysComplete(t *testing.T) {
	t.Parallel()

	const buffer = 8
	h := stream.NewHub(stream.Options{Buffer: buffer, MaxRate: 1_000_000})
	s := subscribe(t, h, stream.Filter{Name: zone.MustParseName("wanted.example.com.")})

	// Far more traffic than the buffer holds, of which a handful is the
	// watcher's.
	const wanted = buffer / 2
	for i := range 500 {
		if i%(500/wanted) == 0 {
			h.Observe(event(func(e *dns.Event) { e.Name = "wanted.example.com." }))
			continue
		}
		h.Observe(event(func(e *dns.Event) { e.Name = "noise.example.com." }))
	}

	st := s.Stats()
	if st.Matched != wanted || st.Sent != wanted || st.Dropped != 0 {
		t.Errorf("stats = %+v, want all %d matching exchanges sent and none dropped",
			st, wanted)
	}
}

// A watcher that is not reading loses its oldest exchanges, not its newest:
// this is a live view, and the answer to what is happening now is not the
// queries from a minute ago.
func TestASlowWatcherLosesTheOldest(t *testing.T) {
	t.Parallel()

	const buffer = 4
	h := stream.NewHub(stream.Options{Buffer: buffer, MaxRate: 1_000_000})
	s := subscribe(t, h, stream.Filter{})

	const sent = 20
	for i := range sent {
		h.Observe(event(func(e *dns.Event) { e.Size = i }))
	}

	// What is left is the tail, in order.
	var got []int
	for range buffer {
		got = append(got, next(t, s).Size)
	}
	for i, size := range got {
		if want := sent - buffer + i; size != want {
			t.Errorf("event %d is #%d, want #%d — the buffer kept the wrong end", i, size, want)
		}
	}

	if st := s.Stats(); st.Dropped != sent-buffer {
		t.Errorf("dropped = %d, want %d", st.Dropped, sent-buffer)
	}
}

// D9: sampling starts only when the filtered stream is faster than a watcher
// is meant to be sent, and what it is sampling at is part of the interface.
func TestSamplingStartsAtTheCapAndSaysSo(t *testing.T) {
	t.Parallel()

	const maxRate = 10
	h := stream.NewHub(stream.Options{Buffer: 4096, MaxRate: maxRate})
	s := subscribe(t, h, stream.Filter{})

	// One second at exactly the cap: nothing is sampled, and the stream says
	// it is showing everything.
	for range maxRate {
		h.Observe(event())
	}
	if st := s.Stats(); st.Ratio != 1 || st.Sampled != 0 || st.Sent != maxRate {
		t.Fatalf("stats = %+v at the cap, want everything sent and no sampling", st)
	}

	// One past it, in the same second, and the stream is already sampling.
	// Waiting for the second to end before deciding would send a burst whole
	// and start sampling after the traffic that needed it was over.
	h.Observe(event())
	if st := s.Stats(); st.Ratio != 2 {
		t.Fatalf("ratio = %d one exchange past the cap, want the stream to have "+
			"reacted inside its own second", st.Ratio)
	}

	// The overshoot that leaves is bounded, and bounded logarithmically: the
	// ratio rises as the second fills, so a second holding a hundred times the
	// cap sends about five times it rather than a hundred. Measured at a cap
	// of 200: 1.5x for a 2x second, 2.9x for 10x, 5.2x for 100x, 7.5x for
	// 1000x. The second after any of them is held at the cap exactly.
	second := at.Add(time.Second)
	for range maxRate * 100 {
		h.Observe(event(func(e *dns.Event) { e.At = second }))
	}
	sent := s.Stats().Sent - uint64(maxRate)
	if sent > uint64(maxRate)*8 {
		t.Errorf("%d of %d exchanges were sent in one second at a cap of %d",
			sent, maxRate*100, maxRate)
	}
	if sent < uint64(maxRate) {
		t.Errorf("only %d exchanges were sent in a second at a cap of %d, "+
			"which is sampling the stream away", sent, maxRate)
	}

	// The second after a steady flood is held at the cap from its first
	// exchange, because the ratio the flood needed is carried into it.
	third := at.Add(2 * time.Second)
	h.Observe(event(func(e *dns.Event) { e.At = third }))
	st := s.Stats()
	if st.Ratio != 100 {
		t.Fatalf("ratio = %d after %d exchanges in a second at a cap of %d, want 100",
			st.Ratio, maxRate*100, maxRate)
	}

	// At one in a hundred, a thousand more exchanges are ten more sent.
	before := st.Sent
	for range 1000 {
		h.Observe(event(func(e *dns.Event) { e.At = third }))
	}
	if got := s.Stats().Sent - before; got != 10 {
		t.Errorf("%d of 1000 exchanges were sent at a ratio of 100, want 10", got)
	}
	if s.Stats().Sampled == 0 {
		t.Error("nothing was counted as sampled, so the stream cannot say what it left out")
	}

	// And it stops again when the traffic does: the fourth second is quiet,
	// and the fifth is worked out from it.
	fourth := at.Add(3 * time.Second)
	h.Observe(event(func(e *dns.Event) { e.At = fourth }))
	fifth := at.Add(4 * time.Second)
	h.Observe(event(func(e *dns.Event) { e.At = fifth }))
	if got := s.Stats().Ratio; got != 1 {
		t.Errorf("ratio = %d after a quiet second, want sampling to have stopped", got)
	}
}

// Every watcher costs a filter evaluation on the query path, so how many there
// may be is a bound on what somebody with a credential can make the data plane
// do.
func TestWatchersAreBounded(t *testing.T) {
	t.Parallel()

	h := stream.NewHub(stream.Options{MaxWatchers: 2})
	first := subscribe(t, h, stream.Filter{})
	subscribe(t, h, stream.Filter{})

	if _, err := h.Subscribe(stream.Filter{}); err == nil {
		t.Fatal("a third watcher was allowed past a bound of two")
	}

	// Leaving makes room again.
	first.Close()
	if h.Watchers() != 1 {
		t.Errorf("watchers = %d after one left, want 1", h.Watchers())
	}
	if _, err := h.Subscribe(stream.Filter{}); err != nil {
		t.Errorf("subscribe after one left: %v", err)
	}
}

func TestCloseEndsTheStream(t *testing.T) {
	t.Parallel()

	h := stream.NewHub(stream.Options{})
	s := subscribe(t, h, stream.Filter{})

	s.Close()
	select {
	case <-s.Done():
	default:
		t.Error("Done is not closed after Close")
	}
	if h.Watchers() != 0 {
		t.Errorf("watchers = %d, want the closed one gone", h.Watchers())
	}

	// Closing twice, and observing afterwards, are both things a real caller
	// does: a handler closes on the way out while a query is still in flight.
	s.Close()
	h.Observe(event())
	empty(t, s)
}

// The query path offers exchanges from every reader goroutine at once, and a
// watcher may leave in the middle of it. Under the race detector this is the
// test that has something to find.
func TestConcurrentObserveAndClose(t *testing.T) {
	t.Parallel()

	h := stream.NewHub(stream.Options{Buffer: 16, MaxWatchers: 32})
	for range 8 {
		if _, err := h.Subscribe(stream.Filter{}); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					h.Observe(event())
				}
			}
		}()
	}

	// Watchers come and go while the queries keep arriving.
	for range 50 {
		s, err := h.Subscribe(stream.Filter{})
		if err != nil {
			continue // the bound was reached, which is not what this tests
		}
		<-s.Events()
		s.Close()
	}
	close(stop)
	wg.Wait()
}
