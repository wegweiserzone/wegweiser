// Package metrics is what this server tells a monitoring system.
//
// It is a consumer of the query path rather than a part of it: the server
// offers every exchange to an observer (architecture §2.9) and this is
// one of the two things that listens. Nothing here is on the path to an answer,
// and nothing here may block.
package metrics

import (
	"fmt"
	"io"
	"net/netip"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/common/expfmt"

	"github.com/wegweiserzone/wegweiser/internal/buildinfo"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// namespace prefixes every metric this server exports, so that a dashboard can
// tell ours from everything else scraped from the same host.
const namespace = "weg"

// ContentType is what the exposition format this package writes calls itself.
// A scraper reads the version out of it, so it travels with the bytes.
var ContentType = string(expfmt.NewFormat(expfmt.TypeTextPlain))

// otherType is where every record type without an assigned mnemonic is
// counted. A QTYPE is sixteen bits and a scanner walks all of them; without
// this, one host could mint sixty-five thousand time series by asking.
const otherType = "OTHER"

// durationBuckets bracket the target of docs/decisions/ D12, p99 under a
// millisecond for a snapshot hit, rather than the defaults, which start at
// 5 ms and would put every answer this server gives in the first bucket.
var durationBuckets = []float64{
	0.000_01, 0.000_025, 0.000_05, 0.000_1, 0.000_25, 0.000_5,
	0.001, 0.0025, 0.005, 0.01, 0.1,
}

// sizeBuckets are the sizes that mean something to DNS rather than round
// numbers: 512 is what an unextended datagram carries (RFC 1035 §4.2.1), 1232
// is what the DNS Flag Day advice settles on for EDNS, 4096 is what a resolver
// commonly advertises, and 65535 is the largest message a stream can frame.
var sizeBuckets = []float64{64, 128, 256, 512, 1232, 2048, 4096, 16384, 65535}

// Metrics is every collector this server exports, and the registry they are on.
//
// It is safe for concurrent use: the query path hands it events from every
// reader goroutine at once.
type Metrics struct {
	reg *prometheus.Registry

	queries   *prometheus.CounterVec
	dropped   *prometheus.CounterVec
	truncated *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	size      *prometheus.HistogramVec

	notify *prometheus.CounterVec

	probes     *prometheus.CounterVec
	lag        *prometheus.GaugeVec
	behind     *prometheus.GaugeVec
	unanswered *prometheus.GaugeVec

	// pairs is what is known about each zone on each secondary, keyed by the
	// pair D36 probes. It is held here rather than exported as a series per
	// zone, for the reason [Metrics.ObserveNotify] gives: the zone count is
	// what makes a label expensive, and the gauges above are what an operator
	// alerts on. Which zone it is belongs in an answer somebody asked for.
	pairMu sync.Mutex
	pairs  map[lagKey]lagState

	zones   prometheus.Gauge
	records prometheus.Gauge
	built   prometheus.Gauge

	byTransport [2]transportMetrics
}

// The two slots of a [Metrics.byTransport], named rather than indexed by the
// transport itself: a value from outside the two this server has would be an
// out-of-range index on the hot path.
const (
	slotUDP = iota
	slotTCP
)

// transportMetrics are the collectors whose only label is the transport,
// resolved once instead of looked up by label value on every query.
type transportMetrics struct {
	dropped   prometheus.Counter
	truncated prometheus.Counter
	duration  prometheus.Observer
	size      prometheus.Observer
}

// New returns the metrics of one server, with the Go runtime and process
// collectors already registered: an operator asking why answers got slow wants
// goroutines, heap and resident memory beside the query counters, not from a
// second exporter.
func New() *Metrics {
	m := &Metrics{
		reg:   prometheus.NewRegistry(),
		pairs: make(map[lagKey]lagState),

		queries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "dns", Name: "queries_total",
			Help: "Queries answered, by transport, question type and response code.",
		}, []string{"transport", "type", "rcode"}),

		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "dns", Name: "queries_dropped_total",
			Help: "Queries that got no response at all, by transport.",
		}, []string{"transport"}),

		truncated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "dns", Name: "responses_truncated_total",
			Help: "Responses cut to fit the transport and marked TC, by transport.",
		}, []string{"transport"}),

		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "dns", Name: "query_duration_seconds",
			Help:    "Time from reading a query to having written its response.",
			Buckets: durationBuckets,
		}, []string{"transport"}),

		size: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "dns", Name: "response_size_bytes",
			Help:    "Response sizes, in octets on the wire.",
			Buckets: sizeBuckets,
		}, []string{"transport"}),

		notify: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "dns", Name: "notifications_total",
			Help: "Notifications and what became of them: one sent per datagram written, " +
				"including retransmissions, and one answered or abandoned per secondary told.",
		}, []string{"outcome"}),

		probes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "secondary", Name: "probes_total",
			Help: "Serial probes and what they found, one per zone asked about per secondary.",
		}, []string{"outcome"}),

		lag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "secondary", Name: "serial_lag",
			Help: "Commits the furthest behind zone on this secondary has yet to see. " +
				"Zero once every zone it is asked about is in step.",
		}, []string{"target"}),

		behind: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "secondary", Name: "zones_behind",
			Help: "Zones this secondary is known to hold an older serial for.",
		}, []string{"target"}),

		unanswered: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "secondary", Name: "zones_unanswered",
			Help: "Zones this secondary was asked about and said nothing useful for. " +
				"A secondary that has gone quiet reads zero behind, and this is where it shows.",
		}, []string{"target"}),

		zones: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "snapshot", Name: "zones",
			Help: "Zones in the snapshot queries are being answered from.",
		}),

		records: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "snapshot", Name: "records",
			Help: "Records in the snapshot queries are being answered from.",
		}),

		built: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "snapshot", Name: "published_timestamp_seconds",
			Help: "When the snapshot being answered from was published.",
		}),
	}

	m.reg.MustRegister(
		m.queries, m.dropped, m.truncated, m.duration, m.size, m.notify,
		m.probes, m.lag, m.behind, m.unanswered,
		m.zones, m.records, m.built,
		buildInfo(),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	for slot, tr := range map[int]dns.Transport{slotUDP: dns.UDP, slotTCP: dns.TCP} {
		m.byTransport[slot] = transportMetrics{
			dropped:   m.dropped.WithLabelValues(tr.String()),
			truncated: m.truncated.WithLabelValues(tr.String()),
			duration:  m.duration.WithLabelValues(tr.String()),
			size:      m.size.WithLabelValues(tr.String()),
		}
	}
	return m
}

// buildInfo is the version of the running binary, as the gauge-set-to-one that
// every Go exporter uses for the purpose: a value nobody reads, carrying labels
// a dashboard joins on.
func buildInfo() prometheus.Collector {
	info := buildinfo.Get()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "build_info",
		Help: "Always 1, labelled with what this binary was built from.",
	}, []string{"version", "commit", "go_version", "platform"})
	g.WithLabelValues(info.Version, info.Commit, info.GoVersion, info.Platform).Set(1)
	return g
}

// Observe records one exchange. It is what a [dns.Server] is given as its
// observer, and it never blocks.
func (m *Metrics) Observe(ev dns.Event) {
	by := &m.byTransport[slotUDP]
	if ev.Transport == dns.TCP {
		by = &m.byTransport[slotTCP]
	}

	if ev.Dropped {
		// A dropped query has no response code and no size, and counting it
		// under one would put messages nobody answered into the same series as
		// the answers.
		by.dropped.Inc()
		return
	}

	m.queries.WithLabelValues(ev.Transport.String(), typeLabel(ev.Type), dns.RcodeName(ev.Rcode)).Inc()
	by.duration.Observe(ev.Latency.Seconds())
	by.size.Observe(float64(ev.Size))
	if ev.Truncated {
		by.truncated.Inc()
	}
}

// ObserveNotify counts one step of telling a secondary that a zone changed.
//
// Without a zone label: a server holds thousands of zones and a series for each
// would cost more than the answer is worth. What an operator asks here is
// whether the secondaries are hearing at all.
func (m *Metrics) ObserveNotify(ev dns.NotifyEvent) {
	m.notify.WithLabelValues(string(ev.Outcome)).Inc()
}

// lagKey is one secondary and one zone, which is the unit a probe answers for.
type lagKey struct {
	target string
	zone   string
}

// lagState is the last thing a probe established about one pair. A pair that
// was asked about and did not come back with a serial is held as not known
// rather than as in step: the difference is a secondary that has gone quiet,
// which is exactly what an operator needs to see.
type lagState struct {
	lag   uint32
	known bool
}

// ObserveProbe records what asking one secondary about one zone found.
func (m *Metrics) ObserveProbe(ev dns.ProbeEvent) {
	m.probes.WithLabelValues(string(ev.Outcome)).Inc()

	st := lagState{}
	switch ev.Outcome {
	case dns.ProbeBehind:
		st = lagState{lag: ev.Lag, known: true}
	case dns.ProbeInStep:
		st = lagState{known: true}
	case dns.ProbeAhead, dns.ProbeUnordered, dns.ProbeSilent, dns.ProbeNoSerial:
		// Nothing was established. A secondary in front of us, one whose
		// serial RFC 1982 cannot order against ours, and one that said nothing
		// are alike to a lag figure: there is not one to report.
	}

	key := lagKey{target: ev.Target.String(), zone: ev.Zone.String()}
	m.pairMu.Lock()
	defer m.pairMu.Unlock()
	m.pairs[key] = st
	m.republish(key.target)
}

// ForgetProbe drops a pair that has stopped existing, because the secondary
// left the notify list or the zone was deleted. A target with nothing left to
// say about it loses its series rather than keeping the last value it had.
func (m *Metrics) ForgetProbe(z zone.Name, target netip.AddrPort) {
	label := target.String()

	m.pairMu.Lock()
	defer m.pairMu.Unlock()
	delete(m.pairs, lagKey{target: label, zone: z.String()})
	m.republish(label)
}

// republish recomputes one secondary's gauges from what is known about its
// zones. Called with the lock held.
func (m *Metrics) republish(target string) {
	var worst, behind, unanswered, pairs uint32
	for key, st := range m.pairs {
		if key.target != target {
			continue
		}
		pairs++
		switch {
		case !st.known:
			unanswered++
		case st.lag > 0:
			behind++
			worst = max(worst, st.lag)
		}
	}
	if pairs == 0 {
		// The secondary is gone, or holds no zones this server serves. Zero
		// would say it is in step, which is a different thing from absent.
		m.lag.DeleteLabelValues(target)
		m.behind.DeleteLabelValues(target)
		m.unanswered.DeleteLabelValues(target)
		return
	}
	m.lag.WithLabelValues(target).Set(float64(worst))
	m.behind.WithLabelValues(target).Set(float64(behind))
	m.unanswered.WithLabelValues(target).Set(float64(unanswered))
}

// SetSnapshot records what is being answered from. The wiring calls it after
// every publish, because the snapshot is what a query actually sees and the
// database it was built from may already be ahead of it.
func (m *Metrics) SetSnapshot(snap *dns.Snapshot, publishedAt time.Time) {
	if snap == nil {
		return
	}
	m.zones.Set(float64(snap.Zones()))
	m.records.Set(float64(snap.Records()))
	m.built.Set(float64(publishedAt.UnixNano()) / float64(time.Second))
}

// WriteTo writes the current values in the Prometheus text exposition format.
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	families, err := m.reg.Gather()
	if err != nil {
		return 0, fmt.Errorf("metrics: gather: %w", err)
	}

	counter := &countingWriter{w: w}
	enc := expfmt.NewEncoder(counter, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range families {
		if err := enc.Encode(mf); err != nil {
			return counter.n, fmt.Errorf("metrics: encode %s: %w", mf.GetName(), err)
		}
	}
	return counter.n, nil
}

// typeLabel folds the record types with no assigned mnemonic together, so that
// what a client asks for cannot decide how many time series exist.
func typeLabel(t zone.RRType) string {
	if !t.HasMnemonic() {
		return otherType
	}
	return t.String()
}

// countingWriter reports how much went through it, which is what [io.WriterTo]
// promises and the encoder does not tell us.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
