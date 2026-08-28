package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wegweiserzone/wegweiser/internal/dns"
)

// BenchmarkObserve is what counting a query costs. It is paid on the reader's
// goroutine, once per query, so it is part of the answer's latency whether or
// not anybody ever looks at the result.
func BenchmarkObserve(b *testing.B) {
	m := New()
	ev := dns.Event{Transport: dns.UDP, Type: 1 /* A */, Rcode: 0, Size: 100, Latency: 250_000}

	b.ReportAllocs()
	for b.Loop() {
		m.Observe(ev)
	}
	b.StopTimer()

	// Without this, an Observe that returned early would benchmark as free.
	if got := testutil.ToFloat64(m.queries.WithLabelValues("udp", "A", "NOERROR")); got == 0 {
		b.Fatal("nothing was counted, so this benchmark measured the wrong thing")
	}
}
