package stream_test

import (
	"fmt"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/stream"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// BenchmarkObserve is what the stream costs the query path. It is paid on the
// reader's goroutine, once per query, once per watcher, which is why there is
// a bound on how many watchers there may be.
func BenchmarkObserve(b *testing.B) {
	ev := event()

	b.Run("nobody watching", func(b *testing.B) {
		h := stream.NewHub(stream.Options{})
		b.ReportAllocs()
		for b.Loop() {
			h.Observe(ev)
		}
	})

	// A watcher that reads nothing, so what is measured is the offer and the
	// drop rather than a channel handing over to a goroutine.
	for _, tt := range []struct {
		name   string
		filter stream.Filter
	}{
		{"one watcher, everything", stream.Filter{}},
		{"one watcher, filtered out", stream.Filter{Name: zone.MustParseName("elsewhere.invalid.")}},
		{"one watcher, name matched", stream.Filter{Name: zone.MustParseName("example.com.")}},
	} {
		b.Run(tt.name, func(b *testing.B) {
			h := stream.NewHub(stream.Options{MaxRate: 1 << 30})
			s, err := h.Subscribe(tt.filter)
			if err != nil {
				b.Fatalf("subscribe: %v", err)
			}
			defer s.Close()

			b.ReportAllocs()
			for b.Loop() {
				h.Observe(ev)
			}
			b.StopTimer()

			// Without this, a filter that excluded everything by accident
			// would benchmark as the fastest option there is.
			if st := s.Stats(); (st.Matched == 0) != (tt.name == "one watcher, filtered out") {
				b.Fatalf("stats = %+v for %q, so this measured the wrong thing", st, tt.name)
			}
		})
	}
}

// BenchmarkObserveConcurrent is the number that matters under load. A datagram
// socket has one reader per processor and they all offer to the same watcher,
// so the counters a watcher keeps are a cache line every reader writes to.
//
// It is measured with sampling in force, because that is the case a flood
// actually reaches: a watcher being sent everything at this rate is one whose
// buffer is being emptied into a socket, which is a different bottleneck.
func BenchmarkObserveConcurrent(b *testing.B) {
	for _, watchers := range []int{0, 1, 4, 16} {
		b.Run(fmt.Sprintf("watchers=%d", watchers), func(b *testing.B) {
			h := stream.NewHub(stream.Options{Buffer: 64, MaxWatchers: 16, MaxRate: 200})
			subs := make([]*stream.Subscription, 0, watchers)
			for range watchers {
				s, err := h.Subscribe(stream.Filter{})
				if err != nil {
					b.Fatalf("subscribe: %v", err)
				}
				subs = append(subs, s)
			}
			defer func() {
				for _, s := range subs {
					s.Close()
				}
			}()

			ev := event()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					h.Observe(ev)
				}
			})
		})
	}
}
