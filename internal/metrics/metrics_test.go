package metrics

import (
	"bytes"
	"context"
	"iter"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// event is one exchange, with the fields a test does not care about filled in.
func event(shape ...func(*dns.Event)) dns.Event {
	ev := dns.Event{
		At:        time.Now(),
		Latency:   250 * time.Microsecond,
		Transport: dns.UDP,
		Name:      "www.example.com.",
		Type:      zone.TypeA,
		Class:     zone.ClassIN,
		Rcode:     0, // NOERROR
		Size:      100,
	}
	for _, s := range shape {
		s(&ev)
	}
	return ev
}

func TestObserveCountsAQuery(t *testing.T) {
	t.Parallel()

	m := New()
	m.Observe(event())
	m.Observe(event(func(e *dns.Event) { e.Transport = dns.TCP; e.Rcode = 3 }))

	if got := testutil.ToFloat64(m.queries.WithLabelValues("udp", "A", "NOERROR")); got != 1 {
		t.Errorf("udp/A/NOERROR = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.queries.WithLabelValues("tcp", "A", "NXDOMAIN")); got != 1 {
		t.Errorf("tcp/A/NXDOMAIN = %v, want 1", got)
	}
	if got := samples(t, m, "weg_dns_query_duration_seconds", "udp"); got != 1 {
		t.Errorf("the udp duration histogram holds %d observations, want 1", got)
	}
	if got := samples(t, m, "weg_dns_query_duration_seconds", "tcp"); got != 1 {
		t.Errorf("the tcp duration histogram holds %d observations, want 1", got)
	}
}

// A QTYPE is sixteen bits. Without the fold, a client walking all of them
// would decide how many time series this server has.
func TestObserveBoundsTheTypeLabel(t *testing.T) {
	t.Parallel()

	m := New()
	for _, typ := range []zone.RRType{zone.RRType(64000), zone.RRType(64001), zone.RRType(65000)} {
		if typ.HasMnemonic() {
			t.Fatalf("%s has a mnemonic, so it is the wrong type to test the fold with", typ)
		}
		m.Observe(event(func(e *dns.Event) { e.Type = typ }))
	}
	m.Observe(event(func(e *dns.Event) { e.Type = zone.TypeAAAA }))

	if got := testutil.ToFloat64(m.queries.WithLabelValues("udp", otherType, "NOERROR")); got != 3 {
		t.Errorf("the folded series counted %v, want all 3 unassigned types", got)
	}
	if got := testutil.CollectAndCount(m.queries); got != 2 {
		t.Errorf("three unassigned types and one assigned made %d series, want 2", got)
	}
}

// A dropped query has no response code and no size. Counting it as an answer
// would put messages nobody answered into the same series as the answers.
func TestObserveSeparatesDrops(t *testing.T) {
	t.Parallel()

	m := New()
	m.Observe(event(func(e *dns.Event) { e.Dropped = true; e.Rcode = 0; e.Size = 0 }))

	if got := testutil.ToFloat64(m.dropped.WithLabelValues("udp")); got != 1 {
		t.Errorf("dropped = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(m.queries); got != 0 {
		t.Errorf("a dropped query produced %d answered series, want none", got)
	}
	if got := samples(t, m, "weg_dns_response_size_bytes", "udp"); got != 0 {
		t.Errorf("a dropped query was measured as a response of some size (%d observations)", got)
	}

	// The same series after a query that was answered, so that the zero above
	// is a fact about the drop rather than about the assertion.
	m.Observe(event())
	if got := samples(t, m, "weg_dns_response_size_bytes", "udp"); got != 1 {
		t.Errorf("an answered query was measured %d times, want 1", got)
	}
}

func TestObserveCountsTruncation(t *testing.T) {
	t.Parallel()

	m := New()
	m.Observe(event(func(e *dns.Event) { e.Truncated = true }))
	m.Observe(event())

	if got := testutil.ToFloat64(m.truncated.WithLabelValues("udp")); got != 1 {
		t.Errorf("truncated = %v, want 1 of the 2 responses", got)
	}
}

func TestSetSnapshot(t *testing.T) {
	t.Parallel()

	m := New()
	snap := dnsSnapshot(t)
	published := time.Unix(1_700_000_000, 0)
	m.SetSnapshot(snap, published)

	if got := testutil.ToFloat64(m.zones); got != float64(snap.Zones()) {
		t.Errorf("zones = %v, want %d", got, snap.Zones())
	}
	if got := testutil.ToFloat64(m.records); got != float64(snap.Records()) {
		t.Errorf("records = %v, want %d", got, snap.Records())
	}
	if got := testutil.ToFloat64(m.built); got != float64(published.Unix()) {
		t.Errorf("published = %v, want %v", got, published.Unix())
	}

	// A publish that never happened must not overwrite what did.
	m.SetSnapshot(nil, published.Add(time.Hour))
	if got := testutil.ToFloat64(m.zones); got != float64(snap.Zones()) {
		t.Errorf("a nil snapshot changed the gauge to %v", got)
	}
}

func TestWriteTo(t *testing.T) {
	t.Parallel()

	m := New()
	m.Observe(event())

	var buf bytes.Buffer
	n, err := m.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != int64(buf.Len()) {
		t.Errorf("WriteTo reported %d octets and wrote %d", n, buf.Len())
	}

	out := buf.String()
	for _, want := range []string{
		"# HELP weg_dns_queries_total",
		"# TYPE weg_dns_queries_total counter",
		`weg_dns_queries_total{rcode="NOERROR",transport="udp",type="A"} 1`,
		"weg_dns_query_duration_seconds_bucket",
		"weg_build_info{",
		// The runtime and process collectors are registered so that an
		// operator does not need a second exporter beside this one.
		"go_goroutines",
		"process_resident_memory_bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the exposition output does not mention %q", want)
		}
	}
}

// The query path hands events over from every reader goroutine at once, so the
// race detector has something to find here if the collectors were not shared
// safely.
func TestObserveIsConcurrent(t *testing.T) {
	t.Parallel()

	m := New()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				m.Observe(event(func(e *dns.Event) {
					if i%2 == 0 {
						e.Transport = dns.TCP
					}
				}))
			}
		}()
	}
	wg.Wait()

	udp := testutil.ToFloat64(m.queries.WithLabelValues("udp", "A", "NOERROR"))
	tcp := testutil.ToFloat64(m.queries.WithLabelValues("tcp", "A", "NOERROR"))
	if udp+tcp != 800 {
		t.Errorf("counted %v queries, want 800", udp+tcp)
	}
}

// snapshotSource is what a snapshot is built from, held in memory. The metrics
// do not care what a snapshot holds, only that a gauge reading zero cannot
// pass for one that was set.
type snapshotSource struct{ records []*zone.Record }

func (s snapshotSource) IterZoneRecords(
	_ context.Context, _ zone.ZoneID,
) iter.Seq2[*zone.Record, error] {
	return func(yield func(*zone.Record, error) bool) {
		for _, r := range s.records {
			if !yield(r, nil) {
				return
			}
		}
	}
}

func dnsSnapshot(t *testing.T) *dns.Snapshot {
	t.Helper()

	z, err := zone.NewZone(zone.MustParseName("example.com."), zone.SOA{
		NS:     zone.MustParseName("ns1.example.com."),
		Mbox:   zone.MustParseName("hostmaster.example.com."),
		Serial: zone.NewSerial(1), Refresh: 7200, Retry: 900,
		Expire: 1209600, Minimum: 3600, TTL: 3600,
	})
	if err != nil {
		t.Fatalf("NewZone: %v", err)
	}

	var src snapshotSource
	for _, r := range [][3]string{
		{"example.com.", "NS", "ns1.example.com."},
		{"ns1.example.com.", "A", "192.0.2.1"},
		{"www.example.com.", "A", "192.0.2.10"},
	} {
		typ, terr := zone.ParseRRType(r[1])
		if terr != nil {
			t.Fatalf("type %q: %v", r[1], terr)
		}
		rec, rerr := zone.NewRecord(z.ID, zone.MustParseName(r[0]), zone.ClassIN, typ, 3600, r[2])
		if rerr != nil {
			t.Fatalf("record %v: %v", r, rerr)
		}
		src.records = append(src.records, &rec)
	}

	snap, err := dns.Build(t.Context(), []*zone.Zone{&z}, src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if snap.Zones() == 0 || snap.Records() == 0 {
		t.Fatalf("the snapshot holds %d zones and %d records, so the gauges below prove nothing",
			snap.Zones(), snap.Records())
	}
	return snap
}

// samples is how many observations a histogram series holds. The series exist
// from the start, so counting them says nothing; what they hold does.
func samples(t *testing.T, m *Metrics, name, transport string) uint64 {
	t.Helper()

	families, err := m.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, series := range mf.GetMetric() {
			for _, l := range series.GetLabel() {
				if l.GetName() == "transport" && l.GetValue() == transport {
					return series.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	t.Fatalf("no series %s{transport=%q}", name, transport)
	return 0
}

func TestObserveNotifyCountsWhatBecameOfIt(t *testing.T) {
	t.Parallel()

	m := New()
	for _, outcome := range []dns.NotifyOutcome{
		dns.NotifySent, dns.NotifySent, dns.NotifyAnswered, dns.NotifyAbandoned,
	} {
		m.ObserveNotify(dns.NotifyEvent{
			Zone:    zone.MustParseName("example.com."),
			Target:  netip.MustParseAddrPort("192.0.2.53:53"),
			Outcome: outcome,
		})
	}

	for _, tc := range []struct {
		outcome string
		want    float64
	}{{"sent", 2}, {"answered", 1}, {"abandoned", 1}} {
		if got := testutil.ToFloat64(m.notify.WithLabelValues(tc.outcome)); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.outcome, got, tc.want)
		}
	}

	// The zone is deliberately not a label: a server holds thousands of them.
	var buf bytes.Buffer
	if _, err := m.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `weg_dns_notifications_total{outcome="abandoned"}`) {
		t.Errorf("the exposition has no abandoned series:\n%s", body)
	}
	if strings.Contains(body, "example.com.") {
		t.Error("the zone reached the exposition as a label value")
	}
}

// probe is one answered question, with the fields a test does not care about
// filled in.
func probe(z string, outcome dns.ProbeOutcome, lag uint32) dns.ProbeEvent {
	return dns.ProbeEvent{
		Zone:    zone.MustParseName(z),
		Target:  netip.MustParseAddrPort("192.0.2.53:53"),
		Outcome: outcome,
		Lag:     lag,
	}
}

func TestObserveProbeSummarisesASecondary(t *testing.T) {
	t.Parallel()

	m := New()
	m.ObserveProbe(probe("a.example.", dns.ProbeBehind, 3))
	m.ObserveProbe(probe("b.example.", dns.ProbeBehind, 7))
	m.ObserveProbe(probe("c.example.", dns.ProbeInStep, 0))

	target := "192.0.2.53:53"
	if got := testutil.ToFloat64(m.lag.WithLabelValues(target)); got != 7 {
		t.Errorf("the lag is %v, want the furthest behind zone, 7", got)
	}
	if got := testutil.ToFloat64(m.behind.WithLabelValues(target)); got != 2 {
		t.Errorf("%v zones behind, want 2", got)
	}
	if got := testutil.ToFloat64(m.unanswered.WithLabelValues(target)); got != 0 {
		t.Errorf("%v zones unanswered, want 0", got)
	}

	// The zone is deliberately not a label here either, for the reason the
	// notification counter gives.
	var buf bytes.Buffer
	if _, err := m.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if body := buf.String(); strings.Contains(body, "b.example.") {
		t.Error("the zone reached the exposition as a label value")
	}
}

func TestObserveProbeCatchingUpClearsTheLag(t *testing.T) {
	t.Parallel()

	m := New()
	m.ObserveProbe(probe("a.example.", dns.ProbeBehind, 4))
	m.ObserveProbe(probe("a.example.", dns.ProbeInStep, 0))

	target := "192.0.2.53:53"
	if got := testutil.ToFloat64(m.lag.WithLabelValues(target)); got != 0 {
		t.Errorf("the lag is %v after the secondary caught up, want 0", got)
	}
	if got := testutil.ToFloat64(m.behind.WithLabelValues(target)); got != 0 {
		t.Errorf("%v zones behind after the secondary caught up, want 0", got)
	}
}

// A secondary that has gone quiet reads zero behind, because nothing is known.
// The gauge that says so is what stops that looking like health.
func TestObserveProbeSeparatesSilenceFromBeingInStep(t *testing.T) {
	t.Parallel()

	m := New()
	m.ObserveProbe(probe("a.example.", dns.ProbeBehind, 4))
	m.ObserveProbe(probe("a.example.", dns.ProbeSilent, 0))

	target := "192.0.2.53:53"
	if got := testutil.ToFloat64(m.behind.WithLabelValues(target)); got != 0 {
		t.Errorf("%v zones behind, want 0: nothing is known any more", got)
	}
	if got := testutil.ToFloat64(m.unanswered.WithLabelValues(target)); got != 1 {
		t.Errorf("%v zones unanswered, want 1", got)
	}
	// The stale distance is gone rather than left standing.
	if got := testutil.ToFloat64(m.lag.WithLabelValues(target)); got != 0 {
		t.Errorf("the lag is %v, want 0: the last figure is not evidence", got)
	}
}

// A gauge outlives what it describes unless somebody says so, which is what
// D36 asks the wiring to do when a target leaves the notify list.
func TestForgetProbeDropsTheSeries(t *testing.T) {
	t.Parallel()

	m := New()
	m.ObserveProbe(probe("a.example.", dns.ProbeBehind, 4))
	m.ForgetProbe(zone.MustParseName("a.example."), netip.MustParseAddrPort("192.0.2.53:53"))

	var buf bytes.Buffer
	if _, err := m.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if body := buf.String(); strings.Contains(body, "weg_secondary_serial_lag{") {
		t.Errorf("the exposition still carries a lag for a secondary that is gone:\n%s", body)
	}
}

func TestObserveProbeCountsWhatItFound(t *testing.T) {
	t.Parallel()

	m := New()
	for _, outcome := range []dns.ProbeOutcome{
		dns.ProbeInStep, dns.ProbeInStep, dns.ProbeBehind, dns.ProbeSilent, dns.ProbeUnordered,
	} {
		m.ObserveProbe(probe("a.example.", outcome, 1))
	}

	for _, tc := range []struct {
		outcome string
		want    float64
	}{{"in_step", 2}, {"behind", 1}, {"silent", 1}, {"unordered", 1}} {
		if got := testutil.ToFloat64(m.probes.WithLabelValues(tc.outcome)); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.outcome, got, tc.want)
		}
	}
}
